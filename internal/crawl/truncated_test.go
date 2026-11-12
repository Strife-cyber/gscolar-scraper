package crawl

import (
	"context"
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
