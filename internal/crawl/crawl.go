// Package crawl orchestrates the plan → crawl → replan lifecycle.
//
// PlanConference turns one conference's broad query into a set of disjoint
// leaf tasks (year ranges, then sequential keyword subtraction), loading each
// leaf's "About N results" count through the browser and caching it in the DB
// so re-planning never re-queries Scholar. CrawlConference then walks every
// pending task page by page, committing each page atomically (raw HTML +
// dedup'd papers + task pointer) so a crash or a CAPTCHA break mid-task
// resumes exactly where it stopped.
package crawl

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/go-rod/rod"

	"gscolar-scraper/internal/browser"
	"gscolar-scraper/internal/config"
	"gscolar-scraper/internal/db"
	"gscolar-scraper/internal/hash"
	"gscolar-scraper/internal/model"
	"gscolar-scraper/internal/parse"
	"gscolar-scraper/internal/split"
)

// sparseRetries is how many consecutive empty-but-paginated pages a task may
// show before it is flagged needs_split (Scholar occasionally "vanishes" after
// page ~6-7; we retry a few times before treating it as a TODO).
const sparseRetries = 3

// Browserer is the slice of the browser package the crawler needs, so the
// crawl logic can be tested offline against recorded HTML with a fake.
type Browserer interface {
	Search(query string, yearFrom, yearTo int) error
	Content() (string, error)
	ClickNext() error
	IsBlocked() bool
	WaitForCaptchaResolved() error
	IncreaseThrottle()
	PauseBetweenPages()
	PauseBetweenSearches()
	PausePlanning()
	MaybeReadingScroll() error
}

// Crawler ties the config, database and browser together.
type Crawler struct {
	cfg *config.Config
	db  *db.DB
	br  Browserer
	log *log.Logger
}

// New builds a Crawler. The browser must already be connected.
func New(cfg *config.Config, d *db.DB, br Browserer, l *log.Logger) *Crawler {
	return &Crawler{cfg: cfg, db: d, br: br, log: l}
}

// ---------------------------------------------------------------------------
// Planning
// ---------------------------------------------------------------------------

// PlanConference computes the leaf plan for one conference, upserts every leaf
// as a task and prunes obsolete needs_split TODOs. Returns how many leaves are
// still flagged for re-splitting (>=0 even on a fully clean plan).
func (c *Crawler) PlanConference(ctx context.Context, conf db.Conference) (int, error) {
	countFunc := func(query string, yearFrom, yearTo int) (int, bool) {
		return c.countForPlan(ctx, query, yearFrom, yearTo)
	}

	leaves, err := split.Plan(conf.Query, c.cfg.StartYear, currentYear(), c.cfg.MaxResults, c.cfg.Keywords, countFunc)
	if err != nil {
		return 0, err
	}

	keepKeys := make([]string, 0, len(leaves))
	needsSplit := 0
	for _, l := range leaves {
		t := &db.Task{
			ConferenceID:  conf.ID,
			Query:         l.Query,
			YearFrom:      l.YearFrom,
			YearTo:        l.YearTo,
			Keywords:      l.Keywords,
			TotalEstimate: l.Count,
		}
		if _, err := c.db.UpsertTask(ctx, t); err != nil {
			return 0, err
		}
		keepKeys = append(keepKeys, t.Key())
		if l.NeedsSplit {
			needsSplit++
		}
	}

	if err := c.db.ReplaceConferencePlan(ctx, conf.ID, keepKeys); err != nil {
		return 0, err
	}
	c.log.Printf("planned %s: %d tasks (%d need re-split)", conf.Name, len(leaves), needsSplit)
	return needsSplit, nil
}

// countForPlan is the CountFunc split.Plan uses. It checks the DB count cache
// first, then drives a browser search, records the count and pauses like a
// human between planning queries. A CAPTCHA mid-plan is resolved and the
// throttle bumped before retrying.
func (c *Crawler) countForPlan(ctx context.Context, query string, yearFrom, yearTo int) (int, bool) {
	if n, ok, err := c.db.GetCachedCount(ctx, query, yearFrom, yearTo); err == nil && ok {
		return n, true
	}

	var n int
	var ok bool
	for {
		err := c.br.Search(query, yearFrom, yearTo)
		if err == browser.ErrBlocked {
			if werr := c.br.WaitForCaptchaResolved(); werr != nil {
				c.log.Printf("plan count captcha not resolved: %v", werr)
				return 0, false
			}
			c.br.IncreaseThrottle()
			continue
		}
		if err != nil {
			c.log.Printf("plan count search failed (%q %d-%d): %v", query, yearFrom, yearTo, err)
			return 0, false
		}
		break
	}

	html, err := c.br.Content()
	if err != nil {
		return 0, false
	}
	p, err := parse.Parse(html)
	if err != nil {
		return 0, false
	}
	if p.Blocked {
		if werr := c.br.WaitForCaptchaResolved(); werr != nil {
			return 0, false
		}
		c.br.IncreaseThrottle()
		return 0, false
	}

	n, ok = p.Count, p.HasCount
	if ok {
		_ = c.db.SetCachedCount(ctx, query, yearFrom, yearTo, n)
	}
	c.br.PausePlanning()
	return n, ok
}

// ---------------------------------------------------------------------------
// Crawling
// ---------------------------------------------------------------------------

// CrawlConference processes every pending task of a conference. Failures on a
// single task (even a CAPTCHA that cannot be resolved) do not abort the
// conference; the task is marked error/needs_split and the next one runs.
func (c *Crawler) CrawlConference(ctx context.Context, confID int64) error {
	// Any task left 'running' by a crashed process must be re-queued so the
	// checkpoint (its page pointer) actually resumes it.
	if err := c.db.ResetStaleRunning(ctx, confID); err != nil {
		return err
	}
	tasks, err := c.db.PendingTasks(ctx, confID)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		c.log.Printf("conference %d: no pending tasks", confID)
		return nil
	}
	for _, t := range tasks {
		c.br.PauseBetweenSearches()
		if err := c.crawlTask(ctx, t); err != nil {
			c.log.Printf("task %d failed: %v", t.ID, err)
		}
	}
	return nil
}

// crawlTask runs one task to completion: repeated searches (resuming at the
// task's page pointer), per-page atomic commits, CAPTCHA resolution, sparse
// page retries and the page-cap needs_split backstop.
func (c *Crawler) crawlTask(ctx context.Context, task db.Task) error {
	if err := c.db.MarkTaskRunning(ctx, task.ID); err != nil {
		return err
	}
	c.log.Printf("task %d: %q [%d-%d] from page %d", task.ID, task.Query, task.YearFrom, task.YearTo, task.Page)

	page := task.Page
	if page < 1 {
		page = 1
	}
	if err := c.issueSearch(ctx, task); err != nil {
		_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError, err.Error())
		return err
	}

	// Resume: a fresh search always lands on page 1, so skip forward to the
	// task's page pointer. If the results actually end before that page (a
	// previous run advanced the pointer past the end), the task is done.
	var notFound *rod.ElementNotFoundError
	for i := 1; i < page; i++ {
		err := c.br.ClickNext()
		if err != nil {
			if errors.As(err, &notFound) {
				_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusCompleted, "")
				c.log.Printf("task %d: resume pointer %d past end -> completed", task.ID, page)
				return nil
			}
			_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError, err.Error())
			return err
		}
		_ = c.br.MaybeReadingScroll()
		c.br.PauseBetweenPages()
	}

	maxPages := (c.cfg.MaxResults + 9) / 10 // 10 results per Scholar page
	emptyRuns := 0

	for {
		if c.br.IsBlocked() {
			if err := c.br.WaitForCaptchaResolved(); err != nil {
				_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError, "captcha not resolved")
				return err
			}
			c.br.IncreaseThrottle()
			continue
		}

		html, err := c.br.Content()
		if err != nil {
			_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError, err.Error())
			return err
		}
		p, err := parse.Parse(html)
		if err != nil {
			_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError, err.Error())
			return err
		}

		pc := db.PageCommit{
			TaskID:     task.ID,
			PageNumber: page,
			HTML:       html,
			Papers:     papersFromItems(p.Items, task.ID),
		}

		switch {
		case len(p.Items) == 0 && p.HasNext:
			// The "vanish" case: a results page with zero rows but a live Next
			// link. Retry a few pages; if it persists, flag for re-split.
			pc.NextPage = page // do not advance the resume pointer
			if err := c.db.CommitPage(ctx, pc); err != nil {
				return err
			}
			emptyRuns++
			c.log.Printf("task %d: sparse page %d (%d/%d)", task.ID, page, emptyRuns, sparseRetries)
			if emptyRuns >= sparseRetries {
				_ = c.db.SetTaskNeedsSplit(ctx, task.ID,
					fmt.Sprintf("empty results for %d consecutive pages from page %d", emptyRuns, page))
				c.log.Printf("task %d: sparse results -> needs_split", task.ID)
				return nil
			}
			// fall through to paginate and try the next page

		case len(p.Items) == 0:
			// No results and no Next: natural end of the result set.
			pc.NextPage = page
			pc.Final = true
			if err := c.db.CommitPage(ctx, pc); err != nil {
				return err
			}
			c.log.Printf("task %d: completed at page %d", task.ID, page)
			return nil

		default:
			pc.NextPage = page + 1
			if err := c.db.CommitPage(ctx, pc); err != nil {
				return err
			}
			emptyRuns = 0
			if !p.HasNext {
				_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusCompleted, "")
				c.log.Printf("task %d: completed (no next page) at page %d", task.ID, page)
				return nil
			}
		}

		if page >= maxPages {
			_ = c.db.SetTaskNeedsSplit(ctx, task.ID,
				fmt.Sprintf("hit %d-page cap (estimate %d)", maxPages, task.TotalEstimate))
			c.log.Printf("task %d: page cap %d reached -> needs_split", task.ID, maxPages)
			return nil
		}

		// Paginate: occasional reading scroll, human pause, then click Next.
		_ = c.br.MaybeReadingScroll()
		c.br.PauseBetweenPages()
		if err := c.br.ClickNext(); err != nil {
			if err == browser.ErrBlocked {
				continue // resolved at the top of the loop
			}
			_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError, err.Error())
			return err
		}
		page++
	}
}

// issueSearch runs a task's search, resolving CAPTCHAs as they appear.
func (c *Crawler) issueSearch(ctx context.Context, task db.Task) error {
	for {
		err := c.br.Search(task.Query, task.YearFrom, task.YearTo)
		if err == browser.ErrBlocked {
			if werr := c.br.WaitForCaptchaResolved(); werr != nil {
				return werr
			}
			c.br.IncreaseThrottle()
			continue
		}
		return err
	}
}

// papersFromItems converts parsed results into db.Paper rows keyed by the
// content hash (normalized title + year + first-author last name).
func papersFromItems(items []model.ResultItem, taskID int64) []db.Paper {
	out := make([]db.Paper, 0, len(items))
	for _, it := range items {
		p := db.Paper{
			Hash:         hash.PaperHash(it.Title, it.Year, it.Authors),
			Title:        it.Title,
			Authors:      it.Authors,
			Year:         it.Year,
			Snippet:      it.Snippet,
			Citations:    it.Citations,
			HasCitations: it.HasCitations,
			SourceURL:    it.URL,
			ScholarID:    it.ScholarID,
			RawHTML:      it.RawHTML,
			TaskID:       taskID,
		}
		out = append(out, p)
	}
	return out
}

func currentYear() int {
	return time.Now().Year()
}
