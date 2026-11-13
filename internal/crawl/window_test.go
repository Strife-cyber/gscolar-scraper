package crawl

import (
	"context"
	"fmt"
	"testing"

	"gscolar-scraper/internal/db"
)

// seedYearPapers inserts nFoo "Foo" and nBar "Bar" papers of one year.
func seedYearPapers(t *testing.T, d *db.DB, confID int64, year, nFoo, nBar int) {
	t.Helper()
	var papers []db.Paper
	for i := 0; i < nFoo; i++ {
		papers = append(papers, db.Paper{
			Hash: fmt.Sprintf("foo-%d-%d", year, i), Title: fmt.Sprintf("A Foo Method %d", i),
			Year: year, ConferenceID: confID, SourcedFrom: "crawl",
		})
	}
	for i := 0; i < nBar; i++ {
		papers = append(papers, db.Paper{
			Hash: fmt.Sprintf("bar-%d-%d", year, i), Title: fmt.Sprintf("A Bar Model %d", i),
			Year: year, ConferenceID: confID, SourcedFrom: "crawl",
		})
	}
	if _, err := d.SavePapers(context.Background(), papers); err != nil {
		t.Fatal(err)
	}
}

// TestResolveYearThinYearUsesWindow: a year known too thinly to trust is
// partitioned offline on its neighbours' papers (year±2) when they are
// numerous enough, instead of falling back to the online probe.
//
// 2022 knows 3 papers for 160 Scholar results (2%, under min_coverage 0.5);
// 2020-2024 knows 400, split 200/200 by "foo". The resolve runs offline, so
// reaching the online fallback would fail it: success proves the window was
// used.
func TestResolveYearThinYearUsesWindow(t *testing.T) {
	d, c, conf := planCrawler(t)
	c.cfg.CoverageWindowYears = 2
	c.cfg.MinWindowPapers = 300
	ctx := context.Background()
	seedYearPapers(t, d, conf.ID, 2022, 2, 1)
	for _, y := range []int{2020, 2021, 2023, 2024} {
		seedYearPapers(t, d, conf.ID, y, 50, 50)
	}
	for _, q := range []string{`"Conf" AND "foo"`, `"Conf" -"foo"`} {
		if err := d.SetCachedCount(ctx, q, 2022, 2022, 80); err != nil {
			t.Fatal(err)
		}
	}

	leaves, ok := c.resolveYear(ctx, conf, 2022, 160, []string{"foo", "bar"}, cacheCount(d), bigBudget(), true)
	if !ok {
		t.Fatal("resolveYear failed: the 2020-2024 window should have been partitioned offline")
	}
	if len(leaves) != 2 {
		t.Fatalf("leaves = %+v, want the 2 foo/-foo buckets", leaves)
	}
	for _, l := range leaves {
		if l.YearFrom != 2022 || l.YearTo != 2022 {
			t.Errorf("leaf %q covers %d-%d, want 2022 only", l.Query, l.YearFrom, l.YearTo)
		}
	}
}

// TestResolveYearThinWindowFallsBack: a window under min_window_papers is not
// trusted either, so the thin year still goes to the online fallback (which
// the offline flag turns into a failure here).
func TestResolveYearThinWindowFallsBack(t *testing.T) {
	d, c, conf := planCrawler(t)
	c.cfg.CoverageWindowYears = 2
	c.cfg.MinWindowPapers = 300
	ctx := context.Background()
	seedYearPapers(t, d, conf.ID, 2022, 2, 1)
	seedYearPapers(t, d, conf.ID, 2021, 50, 50) // 103 papers in the window

	if _, ok := c.resolveYear(ctx, conf, 2022, 160, []string{"foo", "bar"}, cacheCount(d), bigBudget(), true); ok {
		t.Error("resolveYear succeeded on a 103-paper window; min_window_papers is 300")
	}
}
