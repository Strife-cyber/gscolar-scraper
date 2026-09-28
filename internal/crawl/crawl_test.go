package crawl

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-rod/rod"

	"gscolar-scraper/internal/browser"
	"gscolar-scraper/internal/config"
	"gscolar-scraper/internal/db"
	"gscolar-scraper/internal/hash"
	"gscolar-scraper/internal/partition"
	"gscolar-scraper/internal/split"
)

// fakeBrowser serves recorded pages in order. A fresh Search rewinds to the
// first page; ClickNext advances. It satisfies the Browserer interface so the
// whole crawl loop runs offline. The err* fields let tests inject a failure
// from a specific method to exercise crawlTask's error paths.
type fakeBrowser struct {
	contents []string // page HTML in the order ClickNext would reach them
	idx      int

	searchErr      error // returned by Search, once, then cleared
	contentErr     error // returned by Content on every call
	clickNextErr   error // returned by ClickNext instead of the normal advance/end behavior
	clickBlockOnce bool  // first ClickNext lands on a CAPTCHA: returns ErrBlocked and blocks
	blocked        bool  // IsBlocked() return value (cleared by a successful CAPTCHA wait)
	captchaErr     error // returned by WaitForCaptchaResolved
	captchaWaits   int   // how many times WaitForCaptchaResolved was called
	nextCalls      int   // how many times ClickNext was called (resume-walk cost)

	// cancel, if set, is invoked from PauseBetweenPages (right after a page has
	// been committed, mirroring where crawlTask checks ctx.Err()) to simulate a
	// Ctrl+C landing between pages.
	cancel context.CancelFunc

	searchCalls int // how many times Search was actually invoked
}

func (f *fakeBrowser) Search(_ string, _, _ int) error {
	f.searchCalls++
	f.idx = 0
	if err := f.searchErr; err != nil {
		f.searchErr = nil
		return err
	}
	return nil
}
func (f *fakeBrowser) Content() (string, error) {
	if f.contentErr != nil {
		return "", f.contentErr
	}
	if f.idx >= len(f.contents) {
		return "", nil
	}
	return f.contents[f.idx], nil
}
func (f *fakeBrowser) ClickNext() error {
	f.nextCalls++
	if f.clickNextErr != nil {
		return f.clickNextErr
	}
	if f.idx+1 >= len(f.contents) {
		return &rod.ElementNotFoundError{}
	}
	f.idx++
	if f.clickBlockOnce {
		// The click did navigate to the next page, but that page is a CAPTCHA:
		// the click itself succeeds while IsBlocked() reports the block until
		// WaitForCaptchaResolved clears it, then Content() serves the page the
		// click reached.
		f.clickBlockOnce = false
		f.blocked = true
	}
	return nil
}
func (f *fakeBrowser) IsBlocked() bool { return f.blocked }
func (f *fakeBrowser) WaitForCaptchaResolved() error {
	f.captchaWaits++
	if f.captchaErr != nil {
		return f.captchaErr
	}
	f.blocked = false // a solved CAPTCHA unblocks the crawl
	return nil
}
func (f *fakeBrowser) IncreaseThrottle() {}
func (f *fakeBrowser) PauseBetweenPages() {
	if f.cancel != nil {
		f.cancel()
	}
}
func (f *fakeBrowser) PauseBetweenSearches()     {}
func (f *fakeBrowser) PausePlanning()            {}
func (f *fakeBrowser) MaybeReadingScroll() error { return nil }

// readSample loads one of the recorded Scholar pages used by the parser tests.
func readSample(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "parse", "testdata", name+".html"))
	if err != nil {
		t.Fatalf("read sample %s: %v", name, err)
	}
	return string(b)
}

// setup builds a temp DB with one seeded task and a crawler over a fake browser.
func setup(t *testing.T, page int, fb *fakeBrowser) (*db.DB, *Crawler) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	confID, err := d.UpsertConference(ctx, "TEST", `"test conf"`)
	if err != nil {
		t.Fatalf("upsert conference: %v", err)
	}
	_, err = d.UpsertTask(ctx, &db.Task{
		ConferenceID:  confID,
		Query:         `"test conf" AND "learning"`,
		YearFrom:      2000,
		YearTo:        2026,
		TotalEstimate: 1000,
	})
	if err != nil {
		t.Fatalf("upsert task: %v", err)
	}
	if page > 1 {
		tasks, err := d.PendingTasks(ctx, confID)
		if err != nil {
			t.Fatalf("pending tasks: %v", err)
		}
		// Seed page_html for the pages the resume pointer skips: CrawlConference
		// reconciles tasks.page against MAX(page_number)+1, so a bare pointer
		// without committed pages would be reset to 1 before the task runs.
		for p := 1; p < page; p++ {
			if _, err := d.CommitPage(ctx, db.PageCommit{
				TaskID: tasks[0].ID, PageNumber: p, HTML: "seed", NextPage: p + 1,
			}); err != nil {
				t.Fatalf("seed page %d: %v", p, err)
			}
		}
	}

	cfg := &config.Config{MaxResults: 1000, StartYear: 2000, Keywords: []string{"learning"}}
	c := New(cfg, d, fb, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return d, c
}

func firstTask(t *testing.T, d *db.DB) db.Task {
	t.Helper()
	tasks, err := d.AllTasks(context.Background(), 1)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("expected one task, got %d (err=%v)", len(tasks), err)
	}
	return tasks[0]
}

func statusCounts(t *testing.T, d *db.DB) db.Stats {
	t.Helper()
	s, err := d.GetStats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	return s
}

// TestCrawlTaskCompletes: normal results page then an end-of-results page.
func TestCrawlTaskCompletes(t *testing.T) {
	normal := readSample(t, "normal")
	empty := readSample(t, "empty")
	fb := &fakeBrowser{contents: []string{normal, empty}}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}

	s := statusCounts(t, d)
	if s.Completed != 1 || s.Pending != 0 {
		t.Fatalf("expected 1 completed task, got stats %+v", s)
	}
	if s.Pages != 2 {
		t.Fatalf("expected 2 raw pages stored, got %d", s.Pages)
	}
	// normal.html has 20 items per the parser tests.
	if s.Papers != 20 {
		t.Fatalf("expected 20 unique papers, got %d", s.Papers)
	}
	if task := firstTask(t, d); task.Page != 2 {
		t.Fatalf("expected resume pointer page 2, got %d", task.Page)
	}
}

// TestCrawlTaskTwoPageNextThenEnd: a first page with a live Next link followed
// by a second page whose results have no Next — the crawl commits both pages
// and completes via the "no next page" branch.
func TestCrawlTaskTwoPageNextThenEnd(t *testing.T) {
	pageA := mkScholarPage([]string{"AAA", "BBB", "CCC"}, true)
	pageB := mkScholarPage([]string{"DDD", "EEE", "FFF"}, false) // ends here
	fb := &fakeBrowser{contents: []string{pageA, pageB}}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}

	s := statusCounts(t, d)
	if s.Completed != 1 {
		t.Fatalf("expected completed task, got %+v", s)
	}
	if s.Pages != 2 {
		t.Fatalf("expected 2 pages committed, got %d", s.Pages)
	}
	if s.Papers != 6 {
		t.Fatalf("expected 6 unique papers across the two pages, got %d", s.Papers)
	}
	if task := firstTask(t, d); task.Page != 3 {
		t.Fatalf("expected resume pointer page 3, got %d", task.Page)
	}
}

// TestCrawlTaskCancelAfterPageCommit: a shutdown signal (ctx cancelled) lands
// between page 1 and page 2, mirroring a Ctrl+C mid-crawl. Page 1's
// already-scraped results must be durably committed and the task left
// resumable — never lost, and never marked error/needs_split just because a
// shutdown was requested. This guards against a regression back to the old
// os.Exit(130)-on-first-signal behavior, which could kill the process before
// a fully-scraped page's CommitPage ever ran.
func TestCrawlTaskCancelAfterPageCommit(t *testing.T) {
	pageA := mkScholarPage([]string{"AAA", "BBB", "CCC"}, true)
	pageB := mkScholarPage([]string{"DDD", "EEE", "FFF"}, false)
	fb := &fakeBrowser{contents: []string{pageA, pageB}}
	d, c := setup(t, 1, fb)

	ctx, cancel := context.WithCancel(context.Background())
	fb.cancel = cancel // fires from PauseBetweenPages, right after page 1 commits

	err := c.CrawlConference(ctx, 1)
	if err == nil {
		t.Fatal("expected CrawlConference to return the cancellation error, got nil")
	}

	s := statusCounts(t, d)
	if s.Pages != 1 {
		t.Fatalf("expected page 1 to be committed before cancellation, got %d pages", s.Pages)
	}
	if s.Papers != 3 {
		t.Fatalf("expected page 1's 3 papers to be saved, got %d", s.Papers)
	}
	if s.Completed != 0 {
		t.Fatalf("task must not be marked completed on a cancelled crawl, got %+v", s)
	}
	task := firstTask(t, d)
	if task.Status == db.StatusError || task.Status == db.StatusNeedsSplit {
		t.Fatalf("cancellation must not error or needs_split the task, got status %q", task.Status)
	}
	if task.Page != 2 {
		t.Fatalf("expected resume pointer left at page 2 (after page 1), got %d", task.Page)
	}
}

// TestCrawlTaskCaptchaResolvedMidCrawl: clicking Next onto page 2 hits a
// CAPTCHA (ClickNext returns ErrBlocked, IsBlocked reports true). The crawl
// waits, the block clears, and pagination resumes on the page the click
// actually reached — the task completes with both pages committed.
func TestCrawlTaskCaptchaResolvedMidCrawl(t *testing.T) {
	pageA := mkScholarPage([]string{"AAA", "BBB", "CCC"}, true)
	pageB := mkScholarPage([]string{"DDD", "EEE", "FFF"}, false)
	fb := &fakeBrowser{contents: []string{pageA, pageB}, clickBlockOnce: true}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}

	if fb.captchaWaits != 1 {
		t.Fatalf("expected exactly 1 CAPTCHA wait, got %d", fb.captchaWaits)
	}
	s := statusCounts(t, d)
	if s.Completed != 1 {
		t.Fatalf("expected completed task after unblock, got %+v", s)
	}
	if s.Pages != 2 {
		t.Fatalf("expected 2 pages committed after resume, got %d", s.Pages)
	}
	if s.Papers != 6 {
		t.Fatalf("expected 6 unique papers after resume, got %d", s.Papers)
	}
}

// TestCrawlTaskCaptchaMidCrawlUnresolvable: the same mid-crawl CAPTCHA, but the
// wait fails — the task is flagged error with the first page still committed.
func TestCrawlTaskCaptchaMidCrawlUnresolvable(t *testing.T) {
	pageA := mkScholarPage([]string{"AAA", "BBB", "CCC"}, true)
	pageB := mkScholarPage([]string{"DDD", "EEE", "FFF"}, false)
	fb := &fakeBrowser{
		contents:       []string{pageA, pageB},
		clickBlockOnce: true,
		captchaErr:     fmt.Errorf("captcha timed out"),
	}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}

	task := firstTask(t, d)
	if task.Status != db.StatusError {
		t.Fatalf("expected status error on unresolved mid-crawl captcha, got %+v", task)
	}
	s := statusCounts(t, d)
	if s.Pages != 1 || s.Papers != 3 {
		t.Fatalf("expected only page 1 committed before the block, got %+v", s)
	}
}

// mkScholarPage renders a minimal results page with the given titles. Items are
// keyed by (title, year, author), so pages sharing titles share papers.
// mkScholarPage builds a fake results page announcing 6 results.
func mkScholarPage(items []string, withNext bool) string {
	return mkScholarPageCount(items, withNext, 6)
}

// mkScholarPageCount is mkScholarPage with an explicit "About N results"
// header. The crawl now reads that header back and overwrites the task's
// planned estimate with it (refreshEstimate), so a test about under-delivery
// has to be able to say what Scholar claims, not just what it serves.
func mkScholarPageCount(items []string, withNext bool, count int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, `<html><body><div id="gs_ab_md">Page 1 of about %d results</div>`, count)
	for i, title := range items {
		fmt.Fprintf(&sb,
			`<div class="gs_r gs_or" data-cid="c%d"><div class="gs_ri">`+
				`<h3 class="gs_rt"><a href="/scholar?q=test">%s</a></h3>`+
				`<div class="gs_a">Author %d - Venue, 2005</div>`+
				`<div class="gs_rs">snippet</div></div></div>`, i, title, i)
	}
	if withNext {
		sb.WriteString(`<div id="gs_n"><a href="/scholar?start=20"><span class="gs_ico_nav_next"></span></a></div>`)
	}
	sb.WriteString(`</body></html>`)
	return sb.String()
}

// TestCrawlTaskStallDetected: two consecutive pages that parse to the SAME
// result set mean pagination is not advancing (the old off-viewport Next click
// re-committed page 1 thirty times). The task must be flagged instead of
// silently committing a duplicate page.
func TestCrawlTaskStallDetected(t *testing.T) {
	pageA := mkScholarPage([]string{"AAA", "BBB", "CCC"}, true)
	fb := &fakeBrowser{contents: []string{pageA, pageA, readSample(t, "empty")}}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}

	task := firstTask(t, d)
	if task.Status != db.StatusError {
		t.Fatalf("stalled task status = %q, want %q (reason %q)", task.Status, db.StatusError, task.Error)
	}
	s := statusCounts(t, d)
	if s.Papers != 3 {
		t.Fatalf("expected only the first page's 3 papers, got %d", s.Papers)
	}
	if s.Pages != 1 {
		t.Fatalf("expected exactly 1 page committed, got %d", s.Pages)
	}
}

// TestCrawlTaskDedupsOverlapAcrossPages: pages that share SOME papers (a later
// page repeats earlier results) must not double-count the shared ones, while
// genuinely different pages must both be committed.
func TestCrawlTaskDedupsOverlapAcrossPages(t *testing.T) {
	pageA := mkScholarPage([]string{"AAA", "BBB", "CCC"}, true)
	// BBB/CCC must sit at the same positions so they carry the same author line
	// and dedup to the same papers.
	pageB := mkScholarPage([]string{"XXX", "BBB", "CCC"}, true)
	fb := &fakeBrowser{contents: []string{pageA, pageB, readSample(t, "empty")}}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}

	s := statusCounts(t, d)
	if s.Completed != 1 {
		t.Fatalf("expected completed task, got %+v", s)
	}
	if s.Pages != 3 { // page 1, page 2, end-of-results page
		t.Fatalf("expected 3 pages crawled, got %d", s.Pages)
	}
	if s.Papers != 4 { // AAA BBB CCC + XXX, with BBB/CCC dedup'd
		t.Fatalf("expected 4 unique papers despite overlap, got %d", s.Papers)
	}
}

// TestCrawlTaskSparseFlagsNeedsSplit: several consecutive empty-but-paginated
// pages (Scholar's "vanish" after page 6-7) flag the task for re-splitting.
func TestCrawlTaskSparseFlagsNeedsSplit(t *testing.T) {
	sparse := `<html><body>
<div id="gs_ab_md">Page 2 of about 25 results</div>
<div id="gs_n"><a href="/scholar?start=10"><span class="gs_ico_nav_next"></span></a></div>
</body></html>`
	fb := &fakeBrowser{contents: []string{sparse, sparse, sparse}}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}

	s := statusCounts(t, d)
	if s.NeedsSplit != 1 {
		t.Fatalf("expected task flagged needs_split, got %+v", s)
	}
	if s.Papers != 0 {
		t.Fatalf("expected 0 papers from sparse pages, got %d", s.Papers)
	}
}

// TestCrawlTaskResumeAtPage: a task whose pointer is page 3 (with pages 1-2
// already committed, which is what keeps the pointer there under page
// reconciliation) must skip straight to page 3 after a fresh search and only
// add pages 3+.
func TestCrawlTaskResumeAtPage(t *testing.T) {
	normal := readSample(t, "normal")
	fb := &fakeBrowser{contents: []string{normal, normal, normal, readSample(t, "empty")}}
	d, c := setup(t, 3, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}

	s := statusCounts(t, d)
	if s.Completed != 1 {
		t.Fatalf("expected completed task, got %+v", s)
	}
	if s.Pages != 4 { // 2 seeded + pages 3 and 4 (the end page)
		t.Fatalf("expected 4 pages stored after resume, got %d", s.Pages)
	}
	if s.Papers != 20 {
		t.Fatalf("expected 20 unique papers, got %d", s.Papers)
	}
	if task := firstTask(t, d); task.Page != 4 {
		t.Fatalf("expected resume pointer page 4, got %d", task.Page)
	}
}

// TestCrawlTaskStartsEmpty: a task whose first page is already the
// end-of-results page completes immediately.
func TestCrawlTaskStartsEmpty(t *testing.T) {
	fb := &fakeBrowser{contents: []string{readSample(t, "empty")}}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}

	s := statusCounts(t, d)
	if s.Completed != 1 || s.Papers != 0 {
		t.Fatalf("expected empty completion, got %+v", s)
	}
}

// TestCrawlTaskSearchFails: a non-blocked Search error must fail the task
// with StatusError, not silently retry or hang.
func TestCrawlTaskSearchFails(t *testing.T) {
	fb := &fakeBrowser{contents: []string{readSample(t, "normal")}, searchErr: fmt.Errorf("network unreachable")}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.Status != db.StatusError {
		t.Fatalf("expected status error, got %+v", task)
	}
	if task.Error == "" {
		t.Error("expected the search failure reason to be recorded")
	}
}

// TestCrawlTaskCaptchaNotResolvedDuringSearch: issueSearch propagates a failed
// CAPTCHA wait as a task error rather than looping forever.
func TestCrawlTaskCaptchaNotResolvedDuringSearch(t *testing.T) {
	fb := &fakeBrowser{
		contents:   []string{readSample(t, "normal")},
		searchErr:  browser.ErrBlocked,
		captchaErr: fmt.Errorf("captcha timed out"),
	}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.Status != db.StatusError {
		t.Fatalf("expected status error, got %+v", task)
	}
}

// TestCrawlTaskResumeClickNextFails: a ClickNext failure other than
// end-of-results during the resume fast-forward must fail the task, not be
// mistaken for "resume pointer past end".
func TestCrawlTaskResumeClickNextFails(t *testing.T) {
	fb := &fakeBrowser{
		contents:     []string{readSample(t, "normal"), readSample(t, "normal")},
		clickNextErr: fmt.Errorf("stale element"),
	}
	d, c := setup(t, 3, fb) // pointer 3 forces at least one resume ClickNext

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.Status != db.StatusError {
		t.Fatalf("expected status error on resume ClickNext failure, got %+v", task)
	}
	if task.Page != 3 {
		t.Errorf("resume pointer must be untouched on failure, got %d", task.Page)
	}
}

// TestCrawlTaskContentFails: a browser Content() failure mid-crawl fails the
// task with StatusError.
func TestCrawlTaskContentFails(t *testing.T) {
	fb := &fakeBrowser{contents: []string{readSample(t, "normal")}, contentErr: fmt.Errorf("tab crashed")}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.Status != db.StatusError {
		t.Fatalf("expected status error, got %+v", task)
	}
}

// TestCrawlTaskCaptchaNotResolvedDuringPagination: IsBlocked() true plus a
// failed CAPTCHA wait inside the main pagination loop fails the task instead
// of looping forever.
func TestCrawlTaskCaptchaNotResolvedDuringPagination(t *testing.T) {
	fb := &fakeBrowser{
		contents:   []string{readSample(t, "normal"), readSample(t, "empty")},
		blocked:    true,
		captchaErr: fmt.Errorf("captcha timed out"),
	}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.Status != db.StatusError {
		t.Fatalf("expected status error, got %+v", task)
	}
}

// TestCrawlTaskResumePastEnd: the resume pointer beyond the actual last page
// means the previous run finished; the task is marked completed.
func TestCrawlTaskResumePastEnd(t *testing.T) {
	fb := &fakeBrowser{contents: []string{readSample(t, "normal"), readSample(t, "empty")}}
	d, c := setup(t, 5, fb) // pointer 5, but only 2 pages exist

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}

	s := statusCounts(t, d)
	if s.Completed != 1 {
		t.Fatalf("expected completed task when resuming past end, got %+v", s)
	}
}

// setupWithRatio is setup plus an explicit MinCompletionRatio (setup's plain
// config.Config{} zero-value leaves it at 0, which makes any got>=0 count
// satisfy the shortfall check and never fire — tests exercising the shortfall
// path need it set explicitly, the way config.Load's applyDefaults would).
func setupWithRatio(t *testing.T, page int, fb *fakeBrowser, ratio float64) (*db.DB, *Crawler) {
	t.Helper()
	d, c := setup(t, page, fb)
	c.cfg.MinCompletionRatio = ratio
	return d, c
}

// TestCrawlTaskShortfallAtEmptyPage: a task that reaches the "no results, no
// next" end of pagination with far fewer papers than TotalEstimate (1000, per
// setup) is flagged incomplete instead of completed.
func TestCrawlTaskShortfallAtEmptyPage(t *testing.T) {
	fb := &fakeBrowser{contents: []string{readSample(t, "empty")}}
	d, c := setupWithRatio(t, 1, fb, 0.5)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.Status != db.StatusIncomplete {
		t.Fatalf("expected status incomplete, got %+v", task)
	}
	if task.Error == "" {
		t.Error("expected a shortfall reason to be recorded")
	}
}

// TestCrawlTaskShortfallAtNoNextPage: a task that ends via "results but no
// next page" (the default-branch completion) with far fewer papers than
// TotalEstimate is flagged incomplete, reproducing the real IJCAI 2022-2023
// case (About 371 results, 17 papers committed, no gs_n pagination at all).
func TestCrawlTaskShortfallAtNoNextPage(t *testing.T) {
	// Scholar claims 1000 and serves 3, with no next link: genuine
	// under-delivery. The claim has to be in the page, not only in the stored
	// task, because refreshEstimate now trusts the page over the plan.
	pageA := mkScholarPageCount([]string{"AAA", "BBB", "CCC"}, false, 1000)
	fb := &fakeBrowser{contents: []string{pageA}}
	d, c := setupWithRatio(t, 1, fb, 0.5) // 3 papers vs 1000 claimed

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.Status != db.StatusIncomplete {
		t.Fatalf("expected status incomplete, got %+v", task)
	}
}

// TestCrawlTaskNoShortfallWhenClose: a task landing close enough to its
// estimate (above MinCompletionRatio) stays completed, not incomplete.
func TestCrawlTaskNoShortfallWhenClose(t *testing.T) {
	normal := readSample(t, "normal") // 20 items
	fb := &fakeBrowser{contents: []string{normal, readSample(t, "empty")}}
	d, c := setup(t, 1, fb) // default zero-value ratio: shortfall never fires

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.Status != db.StatusCompleted {
		t.Fatalf("expected status completed, got %+v", task)
	}
}

// TestCrawlTaskNoShortfallWithoutEstimate: TotalEstimate <= 0 means there is
// nothing to compare against, so the task is left completed regardless of how
// few papers were found.
func TestCrawlTaskNoShortfallWithoutEstimate(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	confID, err := d.UpsertConference(ctx, "TEST", `"test conf"`)
	if err != nil {
		t.Fatalf("upsert conference: %v", err)
	}
	if _, err := d.UpsertTask(ctx, &db.Task{
		ConferenceID: confID,
		Query:        `"test conf" AND "learning"`,
		YearFrom:     2000,
		YearTo:       2026,
		// TotalEstimate left at 0: nothing to compare against.
	}); err != nil {
		t.Fatalf("upsert task: %v", err)
	}

	// A results page carrying no "About N" header at all. samples/empty.html
	// cannot serve here any more: it announces 730 results while serving none,
	// so refreshEstimate rightly gives the task an estimate and the shortfall
	// check rightly fires. What still needs covering is flagIfShortfall's guard
	// for when no count can be known at all.
	fb := &fakeBrowser{contents: []string{`<html><body><div id="gs_res_ccl"></div></body></html>`}}
	cfg := &config.Config{MaxResults: 1000, StartYear: 2000, Keywords: []string{"learning"}, MinCompletionRatio: 0.9}
	c := New(cfg, d, fb, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := c.CrawlConference(ctx, confID); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.Status != db.StatusCompleted {
		t.Fatalf("expected status completed (no estimate to compare), got %+v", task)
	}
}

// TestRerunShortfall: incomplete and error tasks are re-queued to pending with
// their page pointer untouched; completed/needs_split tasks are left alone.
func TestRerunShortfall(t *testing.T) {
	d, _ := setup(t, 1, &fakeBrowser{})
	ctx := context.Background()
	task := firstTask(t, d)

	if err := d.AdvanceTaskPage(ctx, task.ID, 4); err != nil {
		t.Fatalf("advance page: %v", err)
	}
	if err := d.SetTaskIncomplete(ctx, task.ID, "scholar under-delivered: got 17 of ~371 estimated results"); err != nil {
		t.Fatalf("set incomplete: %v", err)
	}

	n, err := d.RerunShortfallTasks(ctx, task.ConferenceID)
	if err != nil {
		t.Fatalf("rerun shortfall: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 task re-queued, got %d", n)
	}

	got := firstTask(t, d)
	if got.Status != db.StatusPending {
		t.Fatalf("expected status pending after rerun, got %+v", got)
	}
	if got.Page != 4 {
		t.Fatalf("expected page pointer untouched at 4, got %d", got.Page)
	}
	if got.Error != "" {
		t.Errorf("expected error reason cleared, got %q", got.Error)
	}
}

// TestRerunShortfallLeavesOtherStatusesAlone: completed and needs_split tasks
// must not be touched by RerunShortfallTasks.
func TestRerunShortfallLeavesOtherStatusesAlone(t *testing.T) {
	d, _ := setup(t, 1, &fakeBrowser{})
	ctx := context.Background()
	task := firstTask(t, d)

	if err := d.SetTaskStatus(ctx, task.ID, db.StatusCompleted, ""); err != nil {
		t.Fatalf("set completed: %v", err)
	}

	n, err := d.RerunShortfallTasks(ctx, task.ConferenceID)
	if err != nil {
		t.Fatalf("rerun shortfall: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 tasks re-queued, got %d", n)
	}
	if got := firstTask(t, d); got.Status != db.StatusCompleted {
		t.Fatalf("completed task must be untouched, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Planning: harvest + balanced re-split
// ---------------------------------------------------------------------------

// planCrawler builds a Crawler whose tuning fields are set for offline
// partitioning, with a conference conf.Query = `"Conf"`.
func planCrawler(t *testing.T) (*db.DB, *Crawler, db.Conference) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	confID, err := d.UpsertConference(ctx, "CONF", `"Conf"`)
	if err != nil {
		t.Fatalf("upsert conference: %v", err)
	}
	cfg := &config.Config{
		MaxResults: 100, StartYear: 2020, Keywords: []string{"foo", "bar"},
		MinBalance: 0.15, MaxProbes: 8, Headroom: 0.8,
		MinPapersToTrust: 10, MinCoverage: 0.5, MaxMinedKeywords: 200,
	}
	c := New(cfg, d, &fakeBrowser{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return d, c, db.Conference{ID: confID, Name: "CONF", Query: `"Conf"`}
}

// seedConfPapers inserts the conference's known corpus directly (as a crawled
// archive would have) so resolveYear has offline material to partition. Titles
// are either "Foo…" or "Bar…" so the words "foo"/"bar" split them 50/50.
func seedConfPapers(t *testing.T, d *db.DB, confID int64, nFoo, nBar int) {
	t.Helper()
	var papers []db.Paper
	for i := 0; i < nFoo; i++ {
		papers = append(papers, db.Paper{
			Hash: fmt.Sprintf("foo-%d", i), Title: fmt.Sprintf("A Foo Method %d", i),
			Year: 2020, ConferenceID: confID, SourcedFrom: "crawl",
		})
	}
	for i := 0; i < nBar; i++ {
		papers = append(papers, db.Paper{
			Hash: fmt.Sprintf("bar-%d", i), Title: fmt.Sprintf("A Bar Result %d", i),
			Year: 2020, ConferenceID: confID, SourcedFrom: "crawl",
		})
	}
	if _, err := d.SavePapers(context.Background(), papers); err != nil {
		t.Fatalf("seed papers: %v", err)
	}
}

// cacheCount returns a CountFunc that answers only from the count_cache (a
// test stand-in for a fully-cached, budgeted live count; no browser).
func cacheCount(d *db.DB) split.CountFunc {
	return func(query string, yFrom, yTo int) (int, bool) {
		n, ok, err := d.GetCachedCount(context.Background(), query, yFrom, yTo)
		if err != nil {
			return 0, false
		}
		return n, ok
	}
}

// bigBudget is a probe budget that never runs out during a small test.
func bigBudget() *probeBudget { return &probeBudget{left: 1000} }

// TestPlanHarvestPersistsPapers: every planning search that lands a results
// page must persist its rows the moment the page appears (no task, conference
// known, sourced_from='plan') so no planning round is wasted.
func TestPlanHarvestPersistsPapers(t *testing.T) {
	d, c, conf := planCrawler(t)
	ctx := context.Background()
	page := mkScholarPage([]string{"Alpha", "Beta"}, false) // 2 items, about 6 results
	fb := &fakeBrowser{contents: []string{page}}

	// Drive a single planning count directly (bypasses cached counts) over a
	// browser that serves our page.
	c.br = fb
	n, ok := c.countForPlan(ctx, conf.ID, `"Conf"`, 2020, 2020)
	if !ok {
		t.Fatalf("countForPlan: ok=false")
	}
	// mkScholarPage's header reports "about 6 results"; the two rows are what's
	// persisted.
	if n != 6 {
		t.Errorf("count = %d, want 6", n)
	}

	// Each harvested paper must be visible through the conference query and
	// carry no task association (a planning search is not a crawl).
	ps, err := d.PapersForConference(ctx, conf.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("plan-harvested papers = %d, want 2", len(ps))
	}
	found := false
	for _, p := range ps {
		if p.Hash == hash.PaperHash("Alpha", 2005, "Author 0") {
			found = true
			if p.TaskID != 0 {
				t.Errorf("plan-harvested task_id = %d, want 0", p.TaskID)
			}
			if p.ConferenceID != conf.ID {
				t.Errorf("plan-harvested conference_id = %d, want %d", p.ConferenceID, conf.ID)
			}
			if p.SourcedFrom != "plan" {
				t.Errorf("plan-harvested sourced_from = %q, want 'plan'", p.SourcedFrom)
			}
		}
	}
	if !found {
		t.Errorf("harvested paper Alpha not found in conference set")
	}
}

// TestBudgetCountStopsOnCancelledContext: once ctx is cancelled (Ctrl+C
// during -plan), budgetCount must not drive any further browser searches —
// it should fail fast (serving a cache hit if one exists, else returning
// ok=false) instead of letting the caller (resolveNode/resolveYear) treat a
// browser-level "context canceled" error as "this candidate didn't work,
// try the next one" and keep searching. This is the fix for a real Ctrl+C
// during -plan that kept issuing new Scholar searches for ~40s afterward.
func TestBudgetCountStopsOnCancelledContext(t *testing.T) {
	d, c, conf := planCrawler(t)
	page := mkScholarPage([]string{"Alpha", "Beta"}, false)
	fb := &fakeBrowser{contents: []string{page}}
	c.br = fb

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	budget := &probeBudget{confID: conf.ID, left: 100}
	n, ok := c.budgetCount(ctx, budget, `"Conf" AND "neural"`, 2021, 2021)
	if ok {
		t.Errorf("budgetCount on a cancelled ctx with no cache entry should return ok=false, got n=%d", n)
	}
	if fb.searchCalls != 0 {
		t.Errorf("budgetCount must not drive the browser once ctx is cancelled, got %d Search calls", fb.searchCalls)
	}

	// A cache hit is still served without touching the browser even when
	// cancelled — cheap, no browser work, no reason to block on it.
	if err := d.SetCachedCount(context.Background(), `"Conf" AND "cached"`, 2021, 2021, 42); err != nil {
		t.Fatal(err)
	}
	n, ok = c.budgetCount(ctx, budget, `"Conf" AND "cached"`, 2021, 2021)
	if !ok || n != 42 {
		t.Errorf("budgetCount on a cancelled ctx should still serve a cache hit, got n=%d ok=%v", n, ok)
	}
	if fb.searchCalls != 0 {
		t.Errorf("cache hit must not touch the browser, got %d Search calls", fb.searchCalls)
	}
}

// TestResolveYearPartitionsOverCapYear: an over-cap year whose known papers
// split cleanly on a mined keyword resolves into two pending leaves rather than
// staying a needs_split TODO.
func TestResolveYearPartitionsOverCapYear(t *testing.T) {
	d, c, conf := planCrawler(t)
	ctx := context.Background()
	seedConfPapers(t, d, conf.ID, 60, 60)
	// Known corpus of 120 papers is over the offline cap (floor(100*0.8)=80), so
	// partitioning on "foo" yields 60/60; the verified Scholar counts (cached)
	// match and both buckets are under the 100-result crawl cap.
	base := `"Conf"`
	for _, q := range []string{base + ` AND "foo"`, base + ` -"foo"`} {
		if err := d.SetCachedCount(ctx, q, 2020, 2020, 60); err != nil {
			t.Fatal(err)
		}
	}

	leaves, ok := c.resolveYear(ctx, conf, 2020, 0, []string{"foo", "bar"}, cacheCount(d), bigBudget(), false)
	if !ok {
		t.Fatalf("resolveYear failed, want ok")
	}
	if len(leaves) != 2 {
		t.Fatalf("leaves = %d, want 2", len(leaves))
	}
	for _, l := range leaves {
		if l.NeedsSplit {
			t.Errorf("leaf %q should be pending, not needs_split", l.Query)
		}
		if l.Count > c.cfg.MaxResults {
			t.Errorf("leaf %q count %d exceeds cap %d", l.Query, l.Count, c.cfg.MaxResults)
		}
		if l.YearFrom != 2020 || l.YearTo != 2020 {
			t.Errorf("leaf years = %d-%d, want 2020-2020", l.YearFrom, l.YearTo)
		}
	}
}

// TestResolveYearOfflineUndercountFallsBackToResolveOverCap: a bucket whose
// offline (known-paper) size fits but whose real Scholar count is over the cap
// is re-split online into smaller pending leaves.
//
// Corpus: 45 "Alpha Bar" + 30 "Zoo Foo" + 15 "Zoo Qux" papers. "bar" divides
// them 45/45 into two offline buckets (each ≤ 80). The "-bar" bucket is cached
// at 150 (the offline 45 under-estimates Scholar), so ResolveOverCap re-splits
// it on "foo" into 75/75. "foo" survives the corpus veto here because it
// covers 30/45 ≈ 67% of the "-bar" conditioned subset.
func TestResolveYearOfflineUndercountFallsBackToResolveOverCap(t *testing.T) {
	d, c, conf := planCrawler(t)
	ctx := context.Background()
	var papers []db.Paper
	for i := 0; i < 45; i++ {
		papers = append(papers, db.Paper{
			Hash: fmt.Sprintf("fb-%d", i), Title: fmt.Sprintf("Alpha Bar %d", i),
			Year: 2020, ConferenceID: conf.ID, SourcedFrom: "crawl",
		})
	}
	for i := 0; i < 30; i++ {
		papers = append(papers, db.Paper{
			Hash: fmt.Sprintf("zf-%d", i), Title: fmt.Sprintf("Zoo Foo %d", i),
			Year: 2020, ConferenceID: conf.ID, SourcedFrom: "crawl",
		})
	}
	for i := 0; i < 15; i++ {
		papers = append(papers, db.Paper{
			Hash: fmt.Sprintf("zq-%d", i), Title: fmt.Sprintf("Zoo Qux %d", i),
			Year: 2020, ConferenceID: conf.ID, SourcedFrom: "crawl",
		})
	}
	if _, err := d.SavePapers(ctx, papers); err != nil {
		t.Fatal(err)
	}
	base := `"Conf"`
	// "bar" bucket fits; "-bar" bucket under-counts offline (45) vs real (150).
	if err := d.SetCachedCount(ctx, base+` AND "bar"`, 2020, 2020, 60); err != nil {
		t.Fatal(err)
	}
	if err := d.SetCachedCount(ctx, base+` -"bar"`, 2020, 2020, 150); err != nil {
		t.Fatal(err)
	}
	// ResolveOverCap probes ("-bar" AND "foo") -> 75/75.
	if err := d.SetCachedCount(ctx, base+` -"bar" AND "foo"`, 2020, 2020, 75); err != nil {
		t.Fatal(err)
	}

	leaves, ok := c.resolveYear(ctx, conf, 2020, 0, []string{"foo", "bar"}, cacheCount(d), bigBudget(), false)
	if !ok {
		t.Fatalf("resolveYear failed, want ok")
	}
	// "bar" bucket (60, pending) + the "-bar" bucket re-split on "foo" into a
	// 75/75 pair.
	if len(leaves) != 3 {
		t.Fatalf("leaves = %d, want 3", len(leaves))
	}
	for _, l := range leaves {
		if l.NeedsSplit {
			t.Errorf("leaf %q should be pending, not needs_split", l.Query)
		}
		if l.Count > c.cfg.MaxResults {
			t.Errorf("leaf %q count %d exceeds cap %d", l.Query, l.Count, c.cfg.MaxResults)
		}
	}
}

// TestResolveYearThinCorpusFallsBack: fewer known papers than the trust floor
// means the offline split is too noisy; resolveYear declines (caller keeps the
// needs_split TODO and the count-probe chain takes over).
func TestResolveYearThinCorpusFallsBack(t *testing.T) {
	d, c, conf := planCrawler(t)
	seedConfPapers(t, d, conf.ID, 4, 4) // 8 papers < MinPapersToTrust (10)
	if _, ok := c.resolveYear(context.Background(), conf, 2020, 0, []string{"foo", "bar"}, cacheCount(d), bigBudget(), false); ok {
		t.Fatal("resolveYear succeeded on a thin corpus, want fallback (ok=false)")
	}
}

// TestResolveYearUsesCrossConferenceCorpus: the year's partition corpus is
// every known paper in that year, not only this conference's. A conference
// whose own corpus is below the trust floor still resolves when papers crawled
// under other venues fill out the year.
func TestResolveYearUsesCrossConferenceCorpus(t *testing.T) {
	d, c, conf := planCrawler(t)
	ctx := context.Background()
	// This conference alone: 8 papers < MinPapersToTrust — would bail offline.
	seedConfPapers(t, d, conf.ID, 4, 4)
	// Another conference's papers in the same year fill out the corpus: the
	// widened pool of 128 papers partitions foo/¬foo under the 80-doc offline
	// cap and each bucket verifies under the 100-result crawl cap.
	otherID, err := d.UpsertConference(ctx, "OTHER", `"Other"`)
	if err != nil {
		t.Fatalf("upsert other conference: %v", err)
	}
	seedConfPapers(t, d, otherID, 60, 60)
	base := `"Conf"`
	for _, q := range []string{base + ` AND "foo"`, base + ` -"foo"`} {
		if err := d.SetCachedCount(ctx, q, 2020, 2020, 60); err != nil {
			t.Fatal(err)
		}
	}
	leaves, ok := c.resolveYear(ctx, conf, 2020, 0, []string{"foo", "bar"}, cacheCount(d), bigBudget(), false)
	if !ok {
		t.Fatal("resolveYear must partition the cross-conference corpus, not just this venue's papers")
	}
	if len(leaves) != 2 {
		t.Fatalf("leaves = %d, want 2", len(leaves))
	}
}

// TestResolveYearInfeasibleCore: a corpus where every paper shares a single
// token and no keyword divides it must surface as unresolved (needs_split),
// never as an over-cap task or an infinite loop.
func TestResolveYearInfeasibleCore(t *testing.T) {
	d, c, conf := planCrawler(t)
	ctx := context.Background()
	// 120 papers all titled identically "Only": no keyword among {foo, bar}
	// divides the group, so the atom floor is hit and the year stays unresolved
	// rather than emitting an over-cap task.
	var papers []db.Paper
	for i := 0; i < 120; i++ {
		papers = append(papers, db.Paper{
			Hash: fmt.Sprintf("only-%d", i), Title: "Only", Year: 2020,
			ConferenceID: conf.ID, SourcedFrom: "crawl",
		})
	}
	if _, err := d.SavePapers(ctx, papers); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.resolveYear(ctx, conf, 2020, 0, []string{"foo", "bar"}, cacheCount(d), bigBudget(), false); ok {
		t.Fatal("resolveYear resolved an infeasible core, want ok=false (needs_split)")
	}
}

// TestResolveYearRequiresMinedKeywords: without any dividing keyword, a group
// over the offline cap hits the atom floor and stays unresolved.
func TestResolveYearRequiresMinedKeywords(t *testing.T) {
	d, c, conf := planCrawler(t)
	seedConfPapers(t, d, conf.ID, 60, 60)
	if _, ok := c.resolveYear(context.Background(), conf, 2020, 0, nil, cacheCount(d), bigBudget(), false); ok {
		t.Fatal("resolveYear with no keywords resolved, want ok=false")
	}
}

// TestResolveYearEscalatesUnresolvedGroupToFreshMining: the offline keyword
// list ("neural") is degenerate for this group — every paper mentions it, so
// it cannot divide the group and Partition lands it in Unresolved. But half
// the group's own papers share the word "bayesian" that "neural" alone would
// never surface. escalateUnresolved must mine that word FROM the unresolved
// group's own papers and successfully re-partition on it, recovering a leaf
// set that a plain retry with the same keyword list could never produce —
// matching the real scenario where a chain like "AND neural networks" leaves
// generic words (learning/data) unable to divide the branch while a term
// specific to that branch's own papers can.
func TestResolveYearEscalatesUnresolvedGroupToFreshMining(t *testing.T) {
	d, c, conf := planCrawler(t)
	ctx := context.Background()
	var papers []db.Paper
	for i := 0; i < 45; i++ {
		papers = append(papers, db.Paper{
			Hash:  fmt.Sprintf("bn-%d", i),
			Title: fmt.Sprintf("Bayesian Study %d", i),
			// "neural" is present in EVERY paper (degenerate: cannot split the
			// group), planted in the snippet so the title stays a clean word.
			Snippet: "a neural approach to probabilistic inference",
			Year:    2020, ConferenceID: conf.ID, SourcedFrom: "crawl",
		})
	}
	for i := 0; i < 45; i++ {
		papers = append(papers, db.Paper{
			Hash:    fmt.Sprintf("gd-%d", i),
			Title:   fmt.Sprintf("Gradient Study %d", i),
			Snippet: "a neural approach to probabilistic inference",
			Year:    2020, ConferenceID: conf.ID, SourcedFrom: "crawl",
		})
	}
	if _, err := d.SavePapers(ctx, papers); err != nil {
		t.Fatal(err)
	}
	// Cache the count for every plausible term escalateUnresolved's fresh
	// mining might surface from this group ("bayesian" or an n-gram built
	// around it, e.g. "bayesian study") — the test asserts on the leaves it
	// actually gets back rather than pinning one exact mined phrase. "neural"
	// itself never appears in the rendered query: it is degenerate for the
	// WHOLE group from the root (every paper mentions it), so Partition's
	// first pass never uses it as a splitting predicate — the group that
	// lands in Unresolved carries no include/exclude chain at all, and the
	// mined term becomes the bucket's only predicate.
	base := `"Conf"`
	for _, term := range []string{"bayesian", "bayesian study"} {
		for _, q := range []string{
			base + ` AND "` + term + `"`,
			base + ` -"` + term + `"`,
		} {
			if err := d.SetCachedCount(ctx, q, 2020, 2020, 45); err != nil {
				t.Fatal(err)
			}
		}
	}

	leaves, ok := c.resolveYear(ctx, conf, 2020, 0, []string{"neural"}, cacheCount(d), bigBudget(), false)
	if !ok {
		t.Fatalf("resolveYear failed even after escalating to fresh phrase mining, want ok=true")
	}
	if len(leaves) != 2 {
		t.Fatalf("leaves = %d, want 2 (45/45 split on a mined term), leaves=%+v", len(leaves), leaves)
	}
	foundBayesianSplit := false
	for _, l := range leaves {
		if l.NeedsSplit {
			t.Errorf("leaf %q should be pending, not needs_split", l.Query)
		}
		if l.Count > c.cfg.MaxResults {
			t.Errorf("leaf %q count %d exceeds cap %d", l.Query, l.Count, c.cfg.MaxResults)
		}
		if strings.Contains(l.Query, "bayesian") {
			foundBayesianSplit = true
		}
	}
	if !foundBayesianSplit {
		t.Errorf("expected a leaf split on a term mined from the group's own papers (containing 'bayesian'), got %+v", leaves)
	}
}

// TestResolveYearLowCoverageFallsBack: the coverage gate is the operational
// Feasibility guard. When the known papers are a small fraction of the true
// total (here 60/1000 = 6% < min_coverage 50%), the offline split would
// over-verify every bucket against a far larger population; resolveYear declines
// before spending any probes, keeping the year needs_split.
func TestResolveYearLowCoverageFallsBack(t *testing.T) {
	d, c, conf := planCrawler(t)
	seedConfPapers(t, d, conf.ID, 30, 30) // 60 known, but true total is 1000
	calls := 0
	count := func(q string, yf, yt int) (int, bool) {
		calls++
		return cacheCount(d)(q, yf, yt)
	}
	if _, ok := c.resolveYear(context.Background(), conf, 2020, 1000, []string{"foo", "bar"}, count, bigBudget(), false); ok {
		t.Fatal("resolveYear succeeded on a low-coverage corpus, want fallback (ok=false)")
	}
	if calls != 0 {
		t.Errorf("low-coverage resolveYear made %d count calls, want 0 (gate must bail before probing)", calls)
	}
}

// TestResolveYearRespectsSharedBudget: once the per-conference probe budget is
// spent, resolveYear bails (ok=false) instead of probing further, so a
// refractory year cannot blow the whole CAPTCHA allowance.
func TestResolveYearRespectsSharedBudget(t *testing.T) {
	d, c, conf := planCrawler(t)
	seedConfPapers(t, d, conf.ID, 60, 60)
	exhausted := &probeBudget{left: 0}
	if _, ok := c.resolveYear(context.Background(), conf, 2020, 0, []string{"foo", "bar"}, cacheCount(d), exhausted, false); ok {
		t.Fatal("resolveYear succeeded with an exhausted budget, want ok=false")
	}
}

// TestOfflineGenTasksFromCachedCounts: the -gen-tasks path builds a plan purely
// from the count cache + papers, never touching the browser. planCrawler's
// StartYear is 2020 and the scan runs through currentYear (2026), so it walks
// windows 2020-2021, 2022-2023, 2024-2025, 2026-2026. Caching each under the cap
// yields a pending task per window; nothing is over-cap or needs_split.
func TestOfflineGenTasksFromCachedCounts(t *testing.T) {
	d, c, conf := planCrawler(t)
	ctx := context.Background()
	for _, w := range [][3]int{{2020, 2021, 40}, {2022, 2023, 80}, {2024, 2025, 90}, {2026, 2026, 15}} {
		if err := d.SetCachedCount(ctx, `"Conf"`, w[0], w[1], w[2]); err != nil {
			t.Fatal(err)
		}
	}

	need, err := c.OfflineGenTasks(ctx, conf)
	if err != nil {
		t.Fatalf("OfflineGenTasks: %v", err)
	}
	if need != 0 {
		t.Errorf("needs_split count = %d, want 0", need)
	}
	tasks, err := d.AllTasks(ctx, conf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 4 {
		t.Fatalf("tasks = %d, want 4 (one per 2-year window); got %+v", len(tasks), tasks)
	}
	var overCapPending int
	for _, tsk := range tasks {
		if tsk.Status != db.StatusPending {
			t.Errorf("task %q status = %q, want pending", tsk.Query, tsk.Status)
		}
		if tsk.TotalEstimate > c.cfg.MaxResults {
			overCapPending++
		}
	}
	if overCapPending != 0 {
		t.Errorf("%d pending tasks exceed the cap; none may be over-cap", overCapPending)
	}
	// The fake browser was never searched (OfflineGenTasks read counts only).
	if fb, ok := c.br.(*fakeBrowser); ok && fb.idx != 0 {
		t.Errorf("browser was touched (idx=%d), want 0: gen-tasks must be offline", fb.idx)
	}
}

// TestViableKeywords: the dataset vetoes a candidate before Scholar is ever
// searched — a term covering 0 papers or >75% of them can never split, and
// near-duplicate substrings ("network"/"networks") probe the same coverage.
func TestViableKeywords(t *testing.T) {
	texts := []string{
		"deep learning for graphs", "graph neural net",
		"data mining approach", "data analysis method", "data pipeline",
		"data system", "data storage", "data processing", "data model", "data network",
	}
	got := viableKeywords(texts, []string{"learning", "data", "nonexistent"})
	if len(got) != 1 || got[0] != "learning" {
		t.Fatalf("viableKeywords = %v, want [learning]", got)
	}
	// "data" (10/10 = 100%) and "nonexistent" (0) are vetoed.

	deduped := dedupSubstringKeywords([]string{"networks", "network", "graph"})
	if len(deduped) != 2 || deduped[0] != "networks" || deduped[1] != "graph" {
		t.Fatalf("dedupSubstringKeywords = %v, want [networks graph]", deduped)
	}
}

// upsertYearTasks stores a ready-made partition of one year, the way a previous
// -plan would have left it.
func upsertYearTasks(t *testing.T, d *db.DB, confID int64, year int, status string, ests ...int) {
	t.Helper()
	for i, e := range ests {
		if _, err := d.UpsertTask(context.Background(), &db.Task{
			ConferenceID: confID, Query: `"Conf"`, YearFrom: year, YearTo: year,
			Keywords: []string{fmt.Sprintf("kw%d", i)}, Status: status, TotalEstimate: e,
		}); err != nil {
			t.Fatalf("upsert task: %v", err)
		}
	}
}

// TestExistingPartitionReusedWhenComplete: a year already split into buckets
// that are all under the cap and add back up to its count is kept as-is, so a
// re-plan spends no probe on it and its task keys stay stable.
func TestExistingPartitionReusedWhenComplete(t *testing.T) {
	d, c, conf := planCrawler(t)
	upsertYearTasks(t, d, conf.ID, 2016, db.StatusPending, 90, 60, 50) // sums to 200

	year := split.Leaf{Query: `"Conf"`, YearFrom: 2016, YearTo: 2016, Count: 200, HasCount: true, NeedsSplit: true}
	sub, ok := c.existingPartition(context.Background(), conf.ID, year)
	if !ok {
		t.Fatal("existingPartition = false, want the stored split to be reused")
	}
	if len(sub) != 3 {
		t.Errorf("got %d leaves, want 3", len(sub))
	}
	total := 0
	for _, l := range sub {
		total += l.Count
		if l.YearFrom != 2016 || l.YearTo != 2016 {
			t.Errorf("leaf covers %d-%d, want 2016-2016", l.YearFrom, l.YearTo)
		}
	}
	if total != 200 {
		t.Errorf("reused buckets sum to %d, want 200", total)
	}
}

// TestExistingPartitionRejected covers every way a stored split must NOT be
// trusted. The shortfall case is the one that matters most: reusing a partition
// whose buckets no longer cover the year would silently drop the missing
// bucket's papers from the crawl.
func TestExistingPartitionRejected(t *testing.T) {
	tests := []struct {
		name   string
		status string
		ests   []int
		count  int
	}{
		{"buckets no longer cover the year", db.StatusPending, []int{40, 40}, 500},
		{"a single task cannot be a split", db.StatusPending, []int{90}, 90},
		{"no stored tasks at all", db.StatusPending, nil, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, c, conf := planCrawler(t) // MaxResults is 100 here
			upsertYearTasks(t, d, conf.ID, 2016, tt.status, tt.ests...)
			year := split.Leaf{Query: `"Conf"`, YearFrom: 2016, YearTo: 2016, Count: tt.count, HasCount: true, NeedsSplit: true}
			if _, ok := c.existingPartition(context.Background(), conf.ID, year); ok {
				t.Error("existingPartition = true, want the year to be re-split")
			}
		})
	}
}

// TestExistingPartitionIgnoresOtherYears: tasks stored for a neighbouring year
// must never be mistaken for this year's split.
func TestExistingPartitionIgnoresOtherYears(t *testing.T) {
	d, c, conf := planCrawler(t)
	upsertYearTasks(t, d, conf.ID, 2015, db.StatusPending, 90, 60, 50)

	year := split.Leaf{Query: `"Conf"`, YearFrom: 2016, YearTo: 2016, Count: 200, HasCount: true, NeedsSplit: true}
	if _, ok := c.existingPartition(context.Background(), conf.ID, year); ok {
		t.Error("existingPartition = true for 2016, but only 2015 has stored tasks")
	}
}

// TestRefreshEstimateOverwritesStalePlan: the planner's figure is read once, on
// page 1, whenever the conference was last planned, and Scholar's "About N" for
// a narrow query does not hold over time — one ICRA bucket was planned at 887
// and reported 299 at crawl time. The crawl must adopt the live figure before
// collecting, so the shortfall check judges delivery against what Scholar
// actually promised on this search.
func TestRefreshEstimateOverwritesStalePlan(t *testing.T) {
	// Claims 299, serves 3, no next link. Planned estimate is 1000 (see setup).
	page := mkScholarPageCount([]string{"AAA", "BBB", "CCC"}, false, 299)
	fb := &fakeBrowser{contents: []string{page}}
	d, c := setupWithRatio(t, 1, fb, 0.5)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.TotalEstimate != 299 {
		t.Errorf("TotalEstimate = %d, want 299 (the count Scholar showed at crawl time)", task.TotalEstimate)
	}
}

// TestRefreshEstimateKeepsPlanWhenPageHasNoCount: no count on the page means
// nothing better to believe, so the planned figure must survive untouched.
func TestRefreshEstimateKeepsPlanWhenPageHasNoCount(t *testing.T) {
	fb := &fakeBrowser{contents: []string{`<html><body><div id="gs_res_ccl"></div></body></html>`}}
	d, c := setupWithRatio(t, 1, fb, 0.5)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.TotalEstimate != 1000 {
		t.Errorf("TotalEstimate = %d, want the planned 1000 left in place", task.TotalEstimate)
	}
}

// TestAlreadyCompleteSkipsResume: a task whose stored papers already cover the
// count Scholar reports must finish without walking its stale page pointer
// forward. ICRA 2014's "control" bucket sat at page 36 on a fifteen-page result
// set, so resuming meant thirty-five paced "Next" clicks to learn what the
// database already knew.
func TestAlreadyCompleteSkipsResume(t *testing.T) {
	ctx := context.Background()
	// The page reports 3 results; the task will already hold 3.
	fb := &fakeBrowser{contents: []string{mkScholarPageCount([]string{"AAA", "BBB", "CCC"}, true, 3)}}
	d, c := setup(t, 1, fb)

	// First pass collects the three papers and leaves the pointer past page 1.
	if err := c.CrawlConference(ctx, 1); err != nil {
		t.Fatalf("first crawl: %v", err)
	}

	// Re-queue it with an absurd pointer, as an over-paginated task ends up.
	if err := d.UpdateTaskPage(ctx, 1, 36); err != nil {
		t.Fatalf("UpdateTaskPage: %v", err)
	}
	if err := d.SetTaskStatus(ctx, 1, db.StatusPending, ""); err != nil {
		t.Fatalf("SetTaskStatus: %v", err)
	}

	fb.contents = []string{mkScholarPageCount([]string{"AAA", "BBB", "CCC"}, true, 3)}
	fb.nextCalls = 0
	if err := c.CrawlConference(ctx, 1); err != nil {
		t.Fatalf("second crawl: %v", err)
	}

	if fb.nextCalls != 0 {
		t.Errorf("ClickNext called %d times, want 0 — the pointer should not have been walked", fb.nextCalls)
	}
	if task := firstTask(t, d); task.Status != db.StatusCompleted {
		t.Errorf("status = %s, want completed", task.Status)
	}
}

// pageCapCrawler wires a task with a chosen estimate against a browser that
// never runs out of "Next" links, so the crawl always reaches the page cap.
func pageCapCrawler(t *testing.T, estimate int) (*db.DB, *Crawler) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	confID, err := d.UpsertConference(ctx, "TEST", `"test conf"`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertTask(ctx, &db.Task{
		ConferenceID: confID, Query: `"test conf"`, YearFrom: 2008, YearTo: 2008,
		Status: db.StatusPending, TotalEstimate: estimate,
	}); err != nil {
		t.Fatal(err)
	}

	// Every page carries a next link and fresh titles, so neither the stall
	// guard nor the exhaustion guard fires before the cap.
	pages := make([]string, 60)
	for i := range pages {
		pages[i] = mkScholarPageCount([]string{
			fmt.Sprintf("T%da", i), fmt.Sprintf("T%db", i), fmt.Sprintf("T%dc", i),
		}, true, estimate)
	}
	fb := &fakeBrowser{contents: pages}
	cfg := &config.Config{MaxResults: 1000, StartYear: 2000, Keywords: []string{"learning"}, MinCompletionRatio: 0.5}
	return d, New(cfg, d, fb, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestPageCapUnderCapIsNotASplitProblem: a bucket Scholar reports as fitting
// under max_results cannot be fixed by splitting it — the cap was reached
// because Scholar kept paginating over papers it had already served. Flagging
// needs_split there also blocks existingPartition from reusing the year.
func TestPageCapUnderCapIsNotASplitProblem(t *testing.T) {
	d, c := pageCapCrawler(t, 699) // the IROS 2008 shape
	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.Status == db.StatusNeedsSplit {
		t.Errorf("status = needs_split for a %d-result bucket; want completed or incomplete", task.TotalEstimate)
	}
}

// TestPageCapOverCapStillNeedsSplit: the original behaviour must survive for a
// bucket that really is larger than Scholar will serve.
func TestPageCapOverCapStillNeedsSplit(t *testing.T) {
	d, c := pageCapCrawler(t, 4200)
	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.Status != db.StatusNeedsSplit {
		t.Errorf("status = %s for a 4200-result bucket; want needs_split", task.Status)
	}
}

// TestExistingPartitionSurvivesOneOverCapBucket: a year whose buckets still
// cover it is reused even when one of them overflowed at crawl time. Rejecting
// the whole year sent the planner re-splitting all of ICRA 2024 over a single
// bad bucket, and the prune that followed hit page_html's foreign key.
func TestExistingPartitionSurvivesOneOverCapBucket(t *testing.T) {
	d, c, conf := planCrawler(t) // MaxResults is 100 here
	ctx := context.Background()

	// Three healthy buckets plus one over the cap; together they cover the year.
	upsertYearTasks(t, d, conf.ID, 2024, db.StatusCompleted, 90, 60)
	if _, err := d.UpsertTask(ctx, &db.Task{
		ConferenceID: conf.ID, Query: `"Conf" AND "over"`, YearFrom: 2024, YearTo: 2024,
		Keywords: []string{"over"}, Status: db.StatusNeedsSplit, TotalEstimate: 130,
	}); err != nil {
		t.Fatal(err)
	}

	year := split.Leaf{Query: `"Conf"`, YearFrom: 2024, YearTo: 2024, Count: 280, HasCount: true, NeedsSplit: true}
	sub, ok := c.existingPartition(ctx, conf.ID, year)
	if !ok {
		t.Fatal("existingPartition = false; the year is covered and must be reused")
	}
	if len(sub) != 3 {
		t.Fatalf("got %d leaves, want 3", len(sub))
	}
	flagged := 0
	for _, l := range sub {
		if l.NeedsSplit {
			flagged++
			if l.Count != 130 {
				t.Errorf("the flagged leaf has Count %d, want the 130 bucket", l.Count)
			}
		}
	}
	if flagged != 1 {
		t.Errorf("%d leaves flagged needs_split, want exactly the over-cap one", flagged)
	}
}

// TestExistingPartitionIgnoresBareYearLeaf: a failed re-split can leave the
// unresolved-year marker sitting next to the real buckets. Counting it would
// double the year's total and let a broken partition pass the coverage check.
func TestExistingPartitionIgnoresBareYearLeaf(t *testing.T) {
	d, c, conf := planCrawler(t)
	ctx := context.Background()

	upsertYearTasks(t, d, conf.ID, 2024, db.StatusPending, 90, 60) // covers 150
	if _, err := d.UpsertTask(ctx, &db.Task{
		ConferenceID: conf.ID, Query: `"Conf"`, YearFrom: 2024, YearTo: 2024,
		Status: db.StatusNeedsSplit, TotalEstimate: 900, // the bare-year marker
	}); err != nil {
		t.Fatal(err)
	}

	year := split.Leaf{Query: `"Conf"`, YearFrom: 2024, YearTo: 2024, Count: 150, HasCount: true, NeedsSplit: true}
	sub, ok := c.existingPartition(ctx, conf.ID, year)
	if !ok {
		t.Fatal("existingPartition = false; the two real buckets cover the year")
	}
	if len(sub) != 2 {
		t.Errorf("got %d leaves, want 2 — the bare-year marker is not a bucket", len(sub))
	}
	for _, l := range sub {
		if len(l.Keywords) == 0 {
			t.Errorf("leaf %q with no keywords was taken for a bucket", l.Query)
		}
	}
}

// TestRootKeywordIdentifiesTheSharedFirstPredicate: every bucket of a binary
// partition is conditioned on the root, as an include on one side and an
// exclude on the other.
func TestRootKeywordIdentifiesTheSharedFirstPredicate(t *testing.T) {
	tests := []struct {
		name    string
		buckets []partition.Bucket
		want    string
		ok      bool
	}{
		{
			name: "the IROS 2020 chain: paper roots every bucket",
			buckets: []partition.Bucket{
				{Includes: []string{"paper"}},
				{Includes: []string{"control"}, Excludes: []string{"paper"}},
				{Includes: []string{"learning"}, Excludes: []string{"paper", "control"}},
				{Excludes: []string{"paper", "control", "learning"}},
			},
			want: "paper", ok: true,
		},
		{
			name: "a balanced tree still has one root",
			buckets: []partition.Bucket{
				{Includes: []string{"a", "b"}},
				{Includes: []string{"a"}, Excludes: []string{"b"}},
				{Includes: []string{"c"}, Excludes: []string{"a"}},
				{Excludes: []string{"a", "c"}},
			},
			want: "a", ok: true,
		},
		{"a single bucket has no root", []partition.Bucket{{Includes: []string{"a"}}}, "", false},
		{
			name: "buckets that disagree yield no root",
			buckets: []partition.Bucket{
				{Includes: []string{"a"}},
				{Includes: []string{"z"}},
			},
			want: "", ok: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := rootKeyword(tt.buckets)
			if got != tt.want || ok != tt.ok {
				t.Errorf("rootKeyword = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestVerifiedRootKeywordsDropsAScholarHeavyRoot: the corpus can rank a term
// first while Scholar has it on nine documents out of ten. Measuring the root
// before anything is built on it is what stops the IROS 2020 shape — a 1040/110
// split of a 1150-result year, three useless buckets behind it.
func TestVerifiedRootKeywordsDropsAScholarHeavyRoot(t *testing.T) {
	_, c, conf := planCrawler(t)

	// 40 docs: "paper" is in 20 of them (perfect corpus balance, ranks first),
	// "topic" in 10 (a decent but lower-scoring splitter).
	var docs []partition.Doc
	body := map[string]string{}
	for i := range 40 {
		h := fmt.Sprintf("d%02d", i)
		docs = append(docs, partition.Doc{Hash: h, Year: 2020})
		switch {
		case i < 10:
			body[h] = "paper topic"
		case i < 20:
			body[h] = "paper"
		default:
			body[h] = "something else"
		}
	}
	text := func(d partition.Doc) string { return body[d.Hash] }

	// Scholar says "paper" covers 1040 of a 1150-result year: 90%, useless.
	counts := map[string]int{
		conf.Query + ` AND "paper"`: 1040,
		conf.Query + ` AND "topic"`: 500,
	}
	count := func(q string, _, _ int) (int, bool) {
		n, ok := counts[q]
		return n, ok
	}

	got := c.verifiedRootKeywords(conf, 2020, 1150, docs,
		[]string{"paper", "topic"}, text, 20, count, bigBudget())

	for _, k := range got {
		if k == "paper" {
			t.Error(`"paper" survived: it covers 90% of the year on Scholar and can root nothing`)
		}
	}
	if len(got) == 0 {
		t.Error("every candidate was dropped; the usable one must remain")
	}
}

// TestVerifiedRootKeywordsKeepsAnAgreeingRoot: when the corpus and Scholar
// agree, the offline choice must stand and cost nothing further.
func TestVerifiedRootKeywordsKeepsAnAgreeingRoot(t *testing.T) {
	_, c, conf := planCrawler(t)

	var docs []partition.Doc
	body := map[string]string{}
	for i := range 40 {
		h := fmt.Sprintf("d%02d", i)
		docs = append(docs, partition.Doc{Hash: h, Year: 2020})
		if i < 20 {
			body[h] = "control"
		} else {
			body[h] = "other"
		}
	}
	text := func(d partition.Doc) string { return body[d.Hash] }

	probes := 0
	count := func(q string, _, _ int) (int, bool) {
		probes++
		return 500, true // half of a 1000-result year: healthy
	}

	got := c.verifiedRootKeywords(conf, 2020, 1000, docs,
		[]string{"control"}, text, 20, count, bigBudget())

	if len(got) != 1 || got[0] != "control" {
		t.Errorf("candidates = %v, want [control] kept", got)
	}
	if probes != 1 {
		t.Errorf("%d probes spent, want exactly 1 — only the finalist is measured", probes)
	}
}

// TestOfflineDocLimitIsDrivenByScholarNotCorpusSize: the partitioner counts
// documents while max_results counts Scholar results. Mixing the two made the
// bucket count follow the corpus, so it grew with every venue crawled — CHI
// 2016 drew six buckets from 2901 mostly-unrelated documents for a year of
// 1150 that needs two.
func TestOfflineDocLimitIsDrivenByScholarNotCorpusSize(t *testing.T) {
	const maxResults, headroom = 1000, 0.9 // 900 results per bucket

	groups := func(corpus, trueCount int) int {
		lim := offlineDocLimit(maxResults, headroom, corpus, trueCount)
		return (corpus + lim - 1) / lim // ceil
	}

	// CHI 2016, as observed.
	if got := groups(2901, 1150); got != 2 {
		t.Errorf("CHI 2016 (D=2901, N=1150) -> %d buckets, want 2", got)
	}

	// The same year once four more venues have been crawled: the corpus
	// quadruples, the answer must not move.
	for _, corpus := range []int{2901, 6000, 12000, 40000} {
		if got := groups(corpus, 1150); got != 2 {
			t.Errorf("D=%d, N=1150 -> %d buckets, want 2 regardless of corpus size", corpus, got)
		}
	}

	// A year that genuinely needs four.
	if got := groups(3000, 3480); got != 4 {
		t.Errorf("N=3480 -> %d buckets, want 4", got)
	}

	// A year already under the budget needs exactly one.
	if got := groups(2901, 800); got != 1 {
		t.Errorf("N=800 -> %d buckets, want 1", got)
	}
}

// TestOfflineDocLimitFallsBackWithoutACount: with no usable count there is no
// ratio to scale by, so the raw result budget stands rather than a number
// derived from a division by zero.
func TestOfflineDocLimitFallsBackWithoutACount(t *testing.T) {
	for _, tc := range []struct{ corpus, trueCount int }{{2901, 0}, {2901, -1}, {0, 1150}} {
		if got := offlineDocLimit(1000, 0.9, tc.corpus, tc.trueCount); got != 900 {
			t.Errorf("offlineDocLimit(corpus=%d, count=%d) = %d, want the raw 900",
				tc.corpus, tc.trueCount, got)
		}
	}
	if got := offlineDocLimit(1000, 0.9, 1, 100000); got < 1 {
		t.Errorf("limit = %d, must never fall below 1", got)
	}
}
