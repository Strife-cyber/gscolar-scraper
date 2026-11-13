package crawl

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gscolar-scraper/internal/db"
)

// TestCrawlTaskCutOffPageIsNotTheEnd: a page read before Scholar finished
// sending it (rows present, no #gs_res_ccl_bot, so no Next link either) must
// not be taken for the last page. Its rows are kept, the task goes to error
// for -rerun-shortfall, and the pointer stays on that page so the retry reads
// it whole.
func TestCrawlTaskCutOffPageIsNotTheEnd(t *testing.T) {
	full := mkScholarPage([]string{"Alpha", "Beta"}, false)
	cut := strings.Replace(full, `<div id="gs_res_ccl_bot">`, ``, 1)
	fb := &fakeBrowser{contents: []string{cut}}
	d, c := setup(t, 1, fb)

	if err := c.CrawlConference(context.Background(), 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	task := firstTask(t, d)
	if task.Status != db.StatusError {
		t.Errorf("status = %q, want error: a cut-off page is not the end of the results", task.Status)
	}
	if task.Page != 1 {
		t.Errorf("resume pointer = %d, want 1 (re-read the cut-off page)", task.Page)
	}
	if s := statusCounts(t, d); s.Papers != 2 {
		t.Errorf("papers = %d, want the cut-off page's 2 rows kept", s.Papers)
	}
}

// TestCrawlResumesOnStoredCutOffPage: when a task's last stored page was cut
// off, the crawl resumes ON that page rather than after it, so the page is
// read whole. The page-pointer reconciliation used to move past it.
func TestCrawlResumesOnStoredCutOffPage(t *testing.T) {
	full := mkScholarPage([]string{"Alpha", "Beta"}, true)
	cut := strings.Replace(mkScholarPage([]string{"Gamma"}, false), `<div id="gs_res_ccl_bot">`, ``, 1)
	ctx := context.Background()
	// Page 2 will be served whole on the retry, and is the last one.
	fb := &fakeBrowser{contents: []string{full, mkScholarPage([]string{"Gamma", "Delta"}, false)}}
	d, c := setup(t, 1, fb)
	task := firstTask(t, d)
	for i, html := range []string{full, cut} {
		if _, err := d.CommitPage(ctx, db.PageCommit{TaskID: task.ID, PageNumber: i + 1, HTML: html, NextPage: i + 2}); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.CrawlConference(ctx, 1); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	if fb.nextCalls != 1 {
		t.Errorf("Next clicked %d times, want 1 (resume on page 2, not 3)", fb.nextCalls)
	}
	// The pre-stored pages carry no paper rows, so only the re-read page 2
	// contributes: both its rows, Delta included, which the cut copy lacked.
	if s := statusCounts(t, d); s.Papers != 2 {
		t.Errorf("papers = %d, want 2 (Gamma and Delta from the re-read page 2)", s.Papers)
	}
	if _, html, _, err := d.LastStoredPage(ctx, task.ID); err != nil || !strings.Contains(html, "Delta") {
		t.Errorf("stored page 2 was not replaced by the whole re-read (err %v)", err)
	}
}

// TestCrawlStopsWhenBrowserGone: a dead browser connection stops the crawl
// at the first failing task, which is left pending, instead of failing every
// remaining task in turn.
func TestCrawlStopsWhenBrowserGone(t *testing.T) {
	fb := &fakeBrowser{searchErr: errors.New("write tcp 127.0.0.1:1->127.0.0.1:9333: use of closed network connection")}
	d, c := setup(t, 1, fb)

	err := c.CrawlConference(context.Background(), 1)
	if !errors.Is(err, ErrBrowserGone) {
		t.Fatalf("err = %v, want ErrBrowserGone", err)
	}
	if task := firstTask(t, d); task.Status != db.StatusPending {
		t.Errorf("status = %q, want pending: the task did nothing wrong", task.Status)
	}
}
