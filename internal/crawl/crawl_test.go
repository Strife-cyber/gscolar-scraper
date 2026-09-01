package crawl

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-rod/rod"

	"gscolar-scraper/internal/browser"
	"gscolar-scraper/internal/config"
	"gscolar-scraper/internal/db"
	"gscolar-scraper/internal/hash"
	"gscolar-scraper/internal/split"
)

// fakeBrowser serves recorded pages in order. A fresh Search rewinds to the
// first page; ClickNext advances. It satisfies the Browserer interface so the
// whole crawl loop runs offline. The err* fields let tests inject a failure
// from a specific method to exercise crawlTask's error paths.
type fakeBrowser struct {
	contents []string // page HTML in the order ClickNext would reach them
	idx      int

	searchErr    error // returned by Search, once, then cleared
	contentErr   error // returned by Content on every call
	clickNextErr error // returned by ClickNext instead of the normal advance/end behavior
	blocked      bool  // IsBlocked() return value
	captchaErr   error // returned by WaitForCaptchaResolved
}

func (f *fakeBrowser) Search(_ string, _, _ int) error {
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
	if f.clickNextErr != nil {
		return f.clickNextErr
	}
	if f.idx+1 >= len(f.contents) {
		return &rod.ElementNotFoundError{}
	}
	f.idx++
	return nil
}
func (f *fakeBrowser) IsBlocked() bool               { return f.blocked }
func (f *fakeBrowser) WaitForCaptchaResolved() error { return f.captchaErr }
func (f *fakeBrowser) IncreaseThrottle()             {}
func (f *fakeBrowser) PauseBetweenPages()            {}
func (f *fakeBrowser) PauseBetweenSearches()         {}
func (f *fakeBrowser) PausePlanning()                {}
func (f *fakeBrowser) MaybeReadingScroll() error     { return nil }

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
		if err := d.AdvanceTaskPage(ctx, tasks[0].ID, page); err != nil {
			t.Fatalf("advance page: %v", err)
		}
	}

	cfg := &config.Config{MaxResults: 1000, StartYear: 2000, Keywords: []string{"learning"}}
	c := New(cfg, d, fb, log.New(io.Discard, "", 0))
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

// mkScholarPage renders a minimal results page with the given titles. Items are
// keyed by (title, year, author), so pages sharing titles share papers.
func mkScholarPage(items []string, withNext bool) string {
	var sb strings.Builder
	sb.WriteString(`<html><body><div id="gs_ab_md">Page 1 of about 6 results</div>`)
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
	pageB := mkScholarPage([]string{"BBB", "CCC", "DDD"}, true)
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
	if s.Papers != 4 { // AAA BBB CCC + DDD, with BBB/CCC dedup'd
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

// TestCrawlTaskResumeAtPage: a task whose pointer is page 3 must skip straight
// to page 3 after a fresh search and only store pages 3+.
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
	if s.Pages != 2 { // only pages 3 and 4 (the end page) are stored
		t.Fatalf("expected 2 pages stored after resume, got %d", s.Pages)
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
	c := New(cfg, d, &fakeBrowser{}, log.New(io.Discard, "", 0))
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

	leaves, ok := c.resolveYear(ctx, conf, 2020, 0, []string{"foo", "bar"}, cacheCount(d), bigBudget())
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
// Corpus: 45 "Foo Bar" + 45 "Foo Zoo" papers. "bar" divides them 45/45 into two
// offline buckets (each ≤ 80). The "-bar" bucket is cached at 150 (the offline
// 45 under-estimates Scholar), so ResolveOverCap re-splits it on "foo" into
// 75/75.
func TestResolveYearOfflineUndercountFallsBackToResolveOverCap(t *testing.T) {
	d, c, conf := planCrawler(t)
	ctx := context.Background()
	var papers []db.Paper
	for i := 0; i < 45; i++ {
		papers = append(papers, db.Paper{
			Hash: fmt.Sprintf("fb-%d", i), Title: fmt.Sprintf("Foo Bar %d", i),
			Year: 2020, ConferenceID: conf.ID, SourcedFrom: "crawl",
		})
		papers = append(papers, db.Paper{
			Hash: fmt.Sprintf("fz-%d", i), Title: fmt.Sprintf("Foo Zoo %d", i),
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

	leaves, ok := c.resolveYear(ctx, conf, 2020, 0, []string{"foo", "bar"}, cacheCount(d), bigBudget())
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
	if _, ok := c.resolveYear(context.Background(), conf, 2020, 0, []string{"foo", "bar"}, cacheCount(d), bigBudget()); ok {
		t.Fatal("resolveYear succeeded on a thin corpus, want fallback (ok=false)")
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
	if _, ok := c.resolveYear(ctx, conf, 2020, 0, []string{"foo", "bar"}, cacheCount(d), bigBudget()); ok {
		t.Fatal("resolveYear resolved an infeasible core, want ok=false (needs_split)")
	}
}

// TestResolveYearRequiresMinedKeywords: without any dividing keyword, a group
// over the offline cap hits the atom floor and stays unresolved.
func TestResolveYearRequiresMinedKeywords(t *testing.T) {
	d, c, conf := planCrawler(t)
	seedConfPapers(t, d, conf.ID, 60, 60)
	if _, ok := c.resolveYear(context.Background(), conf, 2020, 0, nil, cacheCount(d), bigBudget()); ok {
		t.Fatal("resolveYear with no keywords resolved, want ok=false")
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
	if _, ok := c.resolveYear(context.Background(), conf, 2020, 1000, []string{"foo", "bar"}, count, bigBudget()); ok {
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
	if _, ok := c.resolveYear(context.Background(), conf, 2020, 0, []string{"foo", "bar"}, cacheCount(d), exhausted); ok {
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
