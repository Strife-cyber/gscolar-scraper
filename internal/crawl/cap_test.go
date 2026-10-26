package crawl

import (
	"context"
	"fmt"
	"testing"

	"gscolar-scraper/internal/db"
)

// TestResolveYearBucketAtCapIsResplit: a bucket Scholar counts at exactly
// max_results is not accepted — Scholar stops serving at that size, so it may
// already be clipped — and is re-split online into leaves strictly under it.
//
// planCrawler's cap is 100. Corpus: 45 "Alpha Bar" + 30 "Zoo Foo" + 15
// "Zoo Qux". "bar" divides it 45/45 offline; Scholar counts "AND bar" at 60
// and "-bar" at exactly 100, which is re-split on "foo" into 50/50.
func TestResolveYearBucketAtCapIsResplit(t *testing.T) {
	d, c, conf := planCrawler(t)
	ctx := context.Background()
	var papers []db.Paper
	add := func(prefix, title string, n int) {
		for i := 0; i < n; i++ {
			papers = append(papers, db.Paper{
				Hash: fmt.Sprintf("%s-%d", prefix, i), Title: fmt.Sprintf("%s %d", title, i),
				Year: 2020, ConferenceID: conf.ID, SourcedFrom: "crawl",
			})
		}
	}
	add("fb", "Alpha Bar", 45)
	add("zf", "Zoo Foo", 30)
	add("zq", "Zoo Qux", 15)
	if _, err := d.SavePapers(ctx, papers); err != nil {
		t.Fatal(err)
	}
	base := `"Conf"`
	for q, n := range map[string]int{
		base + ` AND "bar"`:        60,
		base + ` -"bar"`:           100,
		base + ` -"bar" AND "foo"`: 50,
	} {
		if err := d.SetCachedCount(ctx, q, 2020, 2020, n); err != nil {
			t.Fatal(err)
		}
	}

	leaves, ok := c.resolveYear(ctx, conf, 2020, 0, []string{"foo", "bar"}, cacheCount(d), bigBudget(), false)
	if !ok {
		t.Fatal("resolveYear failed, want ok")
	}
	if len(leaves) != 3 {
		t.Fatalf("leaves = %+v, want 3 (bar, -bar AND foo, -bar -foo)", leaves)
	}
	for _, l := range leaves {
		if l.Count >= c.cfg.MaxResults {
			t.Errorf("leaf %q kept at %d, want strictly under the %d cap", l.Query, l.Count, c.cfg.MaxResults)
		}
	}
}
