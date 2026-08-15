package crawl

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-rod/rod"

	"gscolar-scraper/internal/config"
	"gscolar-scraper/internal/db"
)

// fakeBrowser serves recorded pages in order. A fresh Search rewinds to the
// first page; ClickNext advances. It satisfies the Browserer interface so the
// whole crawl loop runs offline.
type fakeBrowser struct {
	contents []string // page HTML in the order ClickNext would reach them
	idx      int
}

func (f *fakeBrowser) Search(_ string, _, _ int) error { f.idx = 0; return nil }
func (f *fakeBrowser) Content() (string, error) {
	if f.idx >= len(f.contents) {
		return "", nil
	}
	return f.contents[f.idx], nil
}
func (f *fakeBrowser) ClickNext() error {
	if f.idx+1 >= len(f.contents) {
		return &rod.ElementNotFoundError{}
	}
	f.idx++
	return nil
}
func (f *fakeBrowser) IsBlocked() bool                     { return false }
func (f *fakeBrowser) WaitForCaptchaResolved() error       { return nil }
func (f *fakeBrowser) IncreaseThrottle()                   {}
func (f *fakeBrowser) PauseBetweenPages()                  {}
func (f *fakeBrowser) PauseBetweenSearches()               {}
func (f *fakeBrowser) PausePlanning()                      {}
func (f *fakeBrowser) MaybeReadingScroll() error           { return nil }

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

// TestCrawlTaskDedupsAcrossPages: the same papers re-appearing on a later page
// must not create duplicates (INSERT OR IGNORE on the content hash).
func TestCrawlTaskDedupsAcrossPages(t *testing.T) {
	normal := readSample(t, "normal")
	fb := &fakeBrowser{contents: []string{normal, normal, readSample(t, "empty")}}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}

	s := statusCounts(t, d)
	if s.Completed != 1 {
		t.Fatalf("expected completed task, got %+v", s)
	}
	if s.Pages != 3 {
		t.Fatalf("expected 3 pages crawled, got %d", s.Pages)
	}
	if s.Papers != 20 {
		t.Fatalf("expected 20 unique papers despite duplicates, got %d", s.Papers)
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
