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
	"hash/fnv"
	"log"
	"time"

	"github.com/go-rod/rod"

	"gscolar-scraper/internal/browser"
	"gscolar-scraper/internal/config"
	"gscolar-scraper/internal/db"
	"gscolar-scraper/internal/hash"
	"gscolar-scraper/internal/mine"
	"gscolar-scraper/internal/model"
	"gscolar-scraper/internal/parse"
	"gscolar-scraper/internal/partition"
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
//
// Splitting uses double leverage from the scraped corpus:
//
//   - Mined keyword candidates from the conference's known papers seed the year
//     chain (so the count-probe fallback subtracts discriminative words rather
//     than generic config terms).
//   - A single year that still exceeds the cap is first resolved offline: its
//     known papers are partitioned by the Balance-ranked keywords into buckets
//     of at most floor(limit*headroom), and each bucket query is verified with a
//     single count probe. A bucket the offline estimate under-counted is
//     re-split online via split.ResolveOverCap. Only what cannot be brought
//     under the cap stays a needs_split TODO.
// probeBudget is a shared cap on real Scholar count searches (browser
// round-trips) for one conference plan. Cache hits are free; only an actual
// browser search costs budget. When the budget is exhausted, the count reports
// ok=false so both split.Plan and resolveYear stop probing further and the plan
// still terminates (writing whatever tasks it has) instead of exploding the
// CAPTCHA budget on a refractory year.
type probeBudget struct {
	confID int64
	left   int
}

// exhausted reports whether the per-conference probe budget is spent.
func (b *probeBudget) exhausted() bool { return b == nil || b.left <= 0 }

func (c *Crawler) PlanConference(ctx context.Context, conf db.Conference) (int, error) {
	budget := &probeBudget{confID: conf.ID, left: c.cfg.MaxProbesPerConf}
	live := func(query string, yearFrom, yearTo int) (int, bool) {
		return c.budgetCount(ctx, budget, query, yearFrom, yearTo)
	}

	pl := c.planKeywords(ctx, conf)
	leaves, err := split.Plan(conf.Query, c.cfg.StartYear, currentYear(), c.cfg.MaxResults, pl, live, c.cfg.MaxKeywords)
	if err != nil {
		return 0, err
	}

	// Resolve any single-year over-cap leaf into finer pending tasks. The probe
	// budget is shared so no single year can consume the conference's whole
	// CAPTCHA allowance.
	var resolved []split.Leaf
	for _, l := range leaves {
		if !l.NeedsSplit || l.YearFrom != l.YearTo {
			resolved = append(resolved, l)
			continue
		}
		sub, ok := c.resolveYear(ctx, conf, l.YearFrom, l.Count, pl, live, budget)
		if ok {
			resolved = append(resolved, sub...)
		} else {
			resolved = append(resolved, l)
		}
	}

	return c.emitTasks(ctx, conf, resolved)
}

// emitTasks upserts every leaf as a task (pending, or needs_split with a reason)
// and prunes obsolete tasks, returning how many leaves remained needs_split.
func (c *Crawler) emitTasks(ctx context.Context, conf db.Conference, resolved []split.Leaf) (int, error) {
	keepKeys := make([]string, 0, len(resolved))
	needsSplit := 0
	for _, l := range resolved {
		// An over-cap leaf is a re-split TODO, not crawlable work: mark it
		// needs_split at plan time so the crawler skips it (UpsertTask would
		// otherwise create it 'pending' and the crawl would burn its page budget
		// discovering it exceeds max_results).
		t := &db.Task{
			ConferenceID:  conf.ID,
			Query:         l.Query,
			YearFrom:      l.YearFrom,
			YearTo:        l.YearTo,
			Keywords:      l.Keywords,
			TotalEstimate: l.Count,
		}
		if l.NeedsSplit {
			t.Status = db.StatusNeedsSplit
		} else {
			t.Status = db.StatusPending
		}
		id, err := c.db.UpsertTask(ctx, t)
		if err != nil {
			return 0, err
		}
		if l.NeedsSplit {
			// Record why this leaf is a TODO (over max_results), self-documenting
			// the flag the same way the crawl-time page-cap backstop does.
			_ = c.db.SetTaskNeedsSplit(ctx, id,
				fmt.Sprintf("estimate %d exceeds the %d-result cap", l.Count, c.cfg.MaxResults))
		}
		keepKeys = append(keepKeys, t.Key())
		if l.NeedsSplit {
			needsSplit++
		}
	}

	if err := c.db.ReplaceConferencePlan(ctx, conf.ID, keepKeys); err != nil {
		return 0, err
	}
	c.log.Printf("planned %s: %d tasks (%d need re-split)", conf.Name, len(resolved), needsSplit)
	return needsSplit, nil
}

// planKeywords returns the keyword candidates fed to the planner: the
// conference's mined splitter words merged with the configured defaults, minus
// degenerate terms that are substrings of the venue query. Mined keywords come
// first (higher Balance first) so the partition and the count chain see the
// best splitters first.
func (c *Crawler) planKeywords(ctx context.Context, conf db.Conference) []string {
	papers, err := c.db.PapersForConference(ctx, conf.ID, 0, 0)
	if err != nil {
		c.log.Printf("load papers for %s keyword mining: %v", conf.Name, err)
		papers = nil
	}
	opts := mine.DefaultOptions()
	opts.MinBalance = c.cfg.MinBalance
	opts.Bigrams = c.cfg.MineBigrams
	opts.MaxCandidates = c.cfg.MaxMinedKeywords
	mp := make([]mine.Paper, 0, len(papers))
	for _, p := range papers {
		mp = append(mp, mine.Paper{Hash: p.Hash, Title: p.Title, Snippet: p.Snippet, Year: p.Year})
	}
	cands := mine.Mine(mp, opts)

	mined := make([]string, 0, len(cands))
	for _, cd := range cands {
		mined = append(mined, cd.Keyword)
	}
	seen := map[string]bool{}
	var kws []string
	for _, w := range append(mined, c.cfg.Keywords...) {
		if seen[w] || w == "" {
			continue
		}
		seen[w] = true
		kws = append(kws, w)
	}
	return split.DropDegenerateKeywords(conf.Query, kws)
}

// resolveYear brings a single over-cap year under the cap using offline
// partitioning of the conference's known papers, verified with one count probe
// per bucket and re-split online where the estimate under-counts. It returns
// pending leaves on success and ok=false when the corpus is too thin, too
// under-covered, the shared budget is exhausted, or some atom cannot be brought
// under the cap (caller keeps the year's needs_split TODO untouched).
//
// trueCount is the year's verified total (the over-cap leaf's Count). The
// coverage gate is the paper's Feasibility guard in operational form: offline
// partitioning is only as good as the corpus it saw. If the known papers are a
// small fraction of the true total, every bucket over-verifies and the probes
// explode (each count() is an expensive Scholar search), so instead of trusting
// a biased sample we bail to the bounded count-probe chain.
func (c *Crawler) resolveYear(ctx context.Context, conf db.Conference, year, trueCount int, keywords []string, count split.CountFunc, budget *probeBudget) ([]split.Leaf, bool) {
	papers, err := c.db.PapersForConference(ctx, conf.ID, year, year)
	if err != nil {
		return nil, false
	}
	coverage := 1.0
	if trueCount > len(papers) {
		coverage = float64(len(papers)) / float64(trueCount)
	}
	// A thin or under-covered corpus cannot yield a trustworthy offline split:
	// fall back to the online count-probe chain rather than over-verifying every
	// bucket against a much larger real population. budget.exhausted() also short
	// circuits before we spend the conference's allowance here.
	if len(papers) < c.cfg.MinPapersToTrust || coverage < c.cfg.MinCoverage || budget.exhausted() {
		return nil, false
	}

	byHash := make(map[string]db.Paper, len(papers))
	docs := make([]partition.Doc, 0, len(papers))
	for _, p := range papers {
		byHash[p.Hash] = p
		docs = append(docs, partition.Doc{Hash: p.Hash, Year: p.Year})
	}
	text := func(d partition.Doc) string {
		p := byHash[d.Hash]
		return p.Title + " " + p.Snippet
	}
	limit := int(float64(c.cfg.MaxResults) * c.cfg.Headroom)
	if limit < 1 {
		limit = 1
	}

	out := partition.Partition(docs, keywords, text, limit)
	var sub []split.Leaf
	for _, b := range out.Buckets {
		q := b.Query(conf.Query)
		n, okC := count(q, year, year)
		if !okC || budget.exhausted() {
			return nil, false
		}
		leaf := split.Leaf{Query: q, YearFrom: year, YearTo: year, Keywords: append(append([]string{}, b.Includes...), b.Excludes...), Count: n, HasCount: true}
		if n <= c.cfg.MaxResults {
			leaf.NeedsSplit = false
			sub = append(sub, leaf)
			continue
		}
		// Offline underestimate (a bucket that "fit" offline is really bigger):
		// re-split it online, still under the shared probe budget.
		leaf.NeedsSplit = true
		rs, ok := split.ResolveOverCap(leaf, keywords, liveOnly(count, budget), c.cfg.MaxProbes, c.cfg.MinBalance, c.cfg.MaxResults)
		if !ok || budget.exhausted() {
			return nil, false
		}
		sub = append(sub, rs...)
	}
	// An unresolved atom floor (a bucket no keyword divides) is a feasibility
	// ceiling: keep the year needs_split rather than emit an over-cap task.
	if len(out.Unresolved) > 0 {
		return nil, false
	}
	return sub, true
}

// liveOnly wraps count so that once the shared budget is spent the wrapper
// reports ok=false, halting ResolveOverCap's recursion instead of probing more.
func liveOnly(count split.CountFunc, budget *probeBudget) split.CountFunc {
	return func(q string, yFrom, yTo int) (int, bool) {
		if budget.exhausted() {
			return 0, false
		}
		return count(q, yFrom, yTo)
	}
}

// OfflineGenTasks builds a conference's leaf plan purely from what is already
// in the DB — count_cache (no browser, no CAPTCHA) plus the mined keywords —
// and upserts the resulting tasks exactly like a live PlanConference. It is the
// "generate tasks with what is available" command: a cache miss reports ok=false,
// so that window/year falls back to a needs_split TODO rather than driving the
// browser. Use it to materialize a crawl plan from previously-seen counts and
// papers without spending any search budget.
func (c *Crawler) OfflineGenTasks(ctx context.Context, conf db.Conference) (int, error) {
	cacheOnly := func(query string, yearFrom, yearTo int) (int, bool) {
		n, ok, err := c.db.GetCachedCount(ctx, query, yearFrom, yearTo)
		if err != nil {
			return 0, false
		}
		return n, ok
	}
	pl := c.planKeywords(ctx, conf)
	leaves, err := split.Plan(conf.Query, c.cfg.StartYear, currentYear(), c.cfg.MaxResults, pl, cacheOnly, c.cfg.MaxKeywords)
	if err != nil {
		return 0, err
	}
	// The offline budget is effectively unlimited: cache hits are free and
	// resolveYear's verify/re-split probes also read count_cache only.
	budget := &probeBudget{confID: conf.ID, left: int(^uint(0) >> 1)} // max int

	var resolved []split.Leaf
	for _, l := range leaves {
		if !l.NeedsSplit || l.YearFrom != l.YearTo {
			resolved = append(resolved, l)
			continue
		}
		sub, ok := c.resolveYear(ctx, conf, l.YearFrom, l.Count, pl, cacheOnly, budget)
		if ok {
			resolved = append(resolved, sub...)
		} else {
			resolved = append(resolved, l)
		}
	}
	return c.emitTasks(ctx, conf, resolved)
}

// budgetCount checks the cache and, only on a miss, spends one unit of the
// shared probe budget before driving an actual browser search. When the budget
// is spent it reports ok=false (no search) so planning can end gracefully.
func (c *Crawler) budgetCount(ctx context.Context, budget *probeBudget, query string, yearFrom, yearTo int) (int, bool) {
	if budget.exhausted() {
		return 0, false
	}
	if n, ok, err := c.db.GetCachedCount(ctx, query, yearFrom, yearTo); err == nil && ok {
		return n, true
	}
	if budget.exhausted() {
		return 0, false
	}
	budget.left--
	return c.countForPlan(ctx, budget.confID, query, yearFrom, yearTo)
}
func (c *Crawler) countForPlan(ctx context.Context, confID int64, query string, yearFrom, yearTo int) (int, bool) {
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

	// Scholar streams a results page: the result rows can appear a moment before
	// the "About N results" header is populated. Reading the page in that window
	// loses the count, and since only readable counts are cached, the query would
	// be re-searched on every future plan run. Re-read the page while rows exist
	// before giving up (a settled empty page — NoResults — already reports
	// count 0, and a page with no rows and no header won't improve on re-reads).
	var p parse.Page
	for read := 0; ; read++ {
		html, err := c.br.Content()
		if err != nil {
			return 0, false
		}
		p, err = parse.Parse(html)
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
		if p.HasCount || len(p.Items) == 0 || read >= 2 {
			break
		}
		// Rows present but the count header is still streaming in.
		time.Sleep(1500 * time.Millisecond)
	}

	n, ok = p.Count, p.HasCount
	if ok {
		_ = c.db.SetCachedCount(ctx, query, yearFrom, yearTo, n)
	}
	// Harvest the result rows immediately rather than discarding them: each
	// planning search that lands a page is a round-trip, so the papers it
	// returns (conference-linked, sourced_from='plan', no task yet) feed the
	// next round's keyword mining. A failure here must not fail the count (the
	// count is what drives the plan); it only skips that harvest.
	if len(p.Items) > 0 {
		if _, herr := c.db.SavePapers(ctx, papersFromItemsForPlan(p.Items, confID)); herr != nil {
			c.log.Printf("plan harvest (%q %d-%d): %v", query, yearFrom, yearTo, herr)
		}
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

	// maxPages bounds how many pages a task may consume before it is flagged for
	// re-split. Scholar currently serves 20 results per page, so derive the bound
	// from that and max_results with a little slack: a task right at its cap
	// finishes naturally, a runaway task is stopped.
	const scholarPageSize = 20
	maxPages := (c.cfg.MaxResults + scholarPageSize - 1) / scholarPageSize + 2
	emptyRuns := 0
	prevKey := "" // fingerprint of the previous page's results, for stall detection

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

		// Stall guard: if two consecutive pages parse to the same result set, the
		// pagination is not advancing (the old off-viewport Next click silently
		// re-committed the same page until the cap flagged the task — the crawl
		// looked "stuck"). Flag it instead of committing a duplicate. Empty pages
		// return "" from itemsKey, so the sparse/end handlers below own those.
		curKey := itemsKey(p.Items)
		if curKey != "" && curKey == prevKey {
			_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError,
				"pagination stalled: identical results on consecutive pages")
			c.log.Printf("task %d: STALL at page %d (same %d results as previous page) -> error", task.ID, page, len(p.Items))
			return nil
		}
		prevKey = curKey

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
			c.log.Printf("task %d: page %d committed (%d papers)", task.ID, page, len(p.Items))
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

// itemsKey fingerprints a page's parsed results so the crawl loop can detect a
// pagination stall (identical results on consecutive pages). Empty pages return
// "", which deliberately never matches, leaving the zero-results cases to the
// sparse/end-of-results handlers.
func itemsKey(items []model.ResultItem) string {
	if len(items) == 0 {
		return ""
	}
	h := fnv.New64a()
	for _, it := range items {
		fmt.Fprintf(h, "%x,", hash.PaperHash(it.Title, it.Year, it.Authors))
	}
	return fmt.Sprintf("%x", h.Sum64())
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

// papersFromItemsForPlan converts parsed results harvested during planning into
// db.Paper rows carrying the conference association but no task (a planning
// search is not a crawl). Such rows are sourced_from='plan' and later healed to
// 'crawl' when a real crawl saves the same hash.
func papersFromItemsForPlan(items []model.ResultItem, confID int64) []db.Paper {
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
			ConferenceID: confID,
			SourcedFrom:  "plan",
		}
		out = append(out, p)
	}
	return out
}

func currentYear() int {
	return time.Now().Year()
}
