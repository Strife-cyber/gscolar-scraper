package crawl

import (
	"context"
	"testing"

	"gscolar-scraper/internal/db"
	"gscolar-scraper/internal/split"
)

// TestOfflineGenTasksPlansEveryName: a venue with an alias gets a plan for
// each name over the whole year range, all under the same conference, and
// planning the second name does not prune the first one's tasks.
func TestOfflineGenTasksPlansEveryName(t *testing.T) {
	d, c, conf := planCrawler(t)
	ctx := context.Background()
	if err := d.SetConferenceAliases(ctx, conf.ID, []string{`"Old Conf"`}); err != nil {
		t.Fatal(err)
	}
	conf.Aliases = []string{`"Old Conf"`}
	for _, name := range conf.Names() {
		for _, w := range [][3]int{{2020, 2021, 40}, {2022, 2023, 80}, {2024, 2025, 90}, {2026, 2026, 15}} {
			if err := d.SetCachedCount(ctx, name, w[0], w[1], w[2]); err != nil {
				t.Fatal(err)
			}
		}
	}

	if _, err := c.OfflineGenTasks(ctx, conf); err != nil {
		t.Fatalf("OfflineGenTasks: %v", err)
	}
	tasks, err := d.AllTasks(ctx, conf.ID)
	if err != nil {
		t.Fatal(err)
	}
	perName := map[string]int{}
	for _, tsk := range tasks {
		perName[tsk.Query]++
	}
	for _, name := range conf.Names() {
		if perName[name] != 4 {
			t.Errorf("%s: %d tasks, want 4 (one per window over the whole range); all: %v", name, perName[name], perName)
		}
	}

	// A second run must be idempotent: no name's tasks pruned by another's.
	if _, err := c.OfflineGenTasks(ctx, conf); err != nil {
		t.Fatalf("second OfflineGenTasks: %v", err)
	}
	again, err := d.AllTasks(ctx, conf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(tasks) {
		t.Errorf("re-plan changed the task count from %d to %d", len(tasks), len(again))
	}
}

// TestExistingPartitionIgnoresOtherNames: one name's split of a year is not
// reused as the split of the same year under another name.
func TestExistingPartitionIgnoresOtherNames(t *testing.T) {
	d, c, conf := planCrawler(t)
	upsertYearTasks(t, d, conf.ID, 2016, db.StatusPending, 90, 60, 50) // "Conf" buckets summing to 200

	other := split.Leaf{Query: `"Old Conf"`, YearFrom: 2016, YearTo: 2016, Count: 200, HasCount: true, NeedsSplit: true}
	if _, ok := c.existingPartition(context.Background(), conf.ID, other); ok {
		t.Error(`"Old Conf" 2016 reused the split built for "Conf"`)
	}
	own := split.Leaf{Query: `"Conf"`, YearFrom: 2016, YearTo: 2016, Count: 200, HasCount: true, NeedsSplit: true}
	if _, ok := c.existingPartition(context.Background(), conf.ID, own); !ok {
		t.Error(`"Conf" 2016 should still reuse its own split`)
	}
}

// TestResolveYearKeepsBranchScholarSaysFits: a branch the known corpus puts
// over the offline limit, but whose Scholar count is under max_results ×
// headroom, is kept as one bucket instead of being divided further.
//
// Corpus: 100 "foo" + 100 "bar" papers, offline limit 80 per group. "foo"
// splits the year 100/100; both sides read as over 80 offline, but Scholar
// counts each at 70, so neither is divided — two buckets, no keyword chain.
func TestResolveYearKeepsBranchScholarSaysFits(t *testing.T) {
	d, c, conf := planCrawler(t)
	ctx := context.Background()
	seedConfPapers(t, d, conf.ID, 100, 100)
	base := `"Conf"`
	for _, q := range []string{base + ` AND "foo"`, base + ` -"foo"`, base + ` AND "bar"`, base + ` -"bar"`} {
		if err := d.SetCachedCount(ctx, q, 2020, 2020, 70); err != nil {
			t.Fatal(err)
		}
	}

	leaves, ok := c.resolveYear(ctx, conf, 2020, 0, []string{"foo", "bar"}, cacheCount(d), bigBudget(), false)
	if !ok {
		t.Fatal("resolveYear failed, want ok")
	}
	if len(leaves) != 2 {
		t.Fatalf("leaves = %+v, want 2 whole branches", leaves)
	}
	for _, l := range leaves {
		if len(l.Keywords) != 1 {
			t.Errorf("leaf %q has keywords %v, want the single root keyword", l.Query, l.Keywords)
		}
	}
}
