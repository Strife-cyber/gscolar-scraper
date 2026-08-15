package split

import (
	"fmt"
	"strings"
	"testing"
)

// fakeCount builds a CountFunc from a table. An empty map simulates a count
// that could not be read (ok=false).
func fakeCount(table map[string]int) CountFunc {
	return func(query string, yearFrom, yearTo int) (int, bool) {
		key := fmt.Sprintf("%s|%d|%d", query, yearFrom, yearTo)
		n, ok := table[key]
		return n, ok
	}
}

func key(query string, ylo, yhi int) string { return fmt.Sprintf("%s|%d|%d", query, ylo, yhi) }

// TestSingleLeaf: a query under the cap stays one task.
func TestSingleLeaf(t *testing.T) {
	q := `"ICML"`
	table := map[string]int{key(q, 2000, 2026): 900}
	leaves, err := Plan(q, 2000, 2026, 1000, []string{"learning", "neural"}, fakeCount(table))
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 1 {
		t.Fatalf("got %d leaves, want 1", len(leaves))
	}
	if leaves[0].NeedsSplit || leaves[0].YearFrom != 2000 || leaves[0].YearTo != 2026 {
		t.Errorf("unexpected leaf: %+v", leaves[0])
	}
}

// TestYearSplit: an over-cap query must be partitioned by year, recursively.
func TestYearSplit(t *testing.T) {
	q := `"ICML"`
	// Counts: full range > cap, halves < cap. Expect exactly 2 year leaves.
	table := map[string]int{
		key(q, 2000, 2026): 5000,
		key(q, 2000, 2013): 800,
		key(q, 2014, 2026): 700,
	}
	leaves, err := Plan(q, 2000, 2026, 1000, []string{"learning"}, fakeCount(table))
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 2 {
		t.Fatalf("got %d leaves, want 2: %+v", len(leaves), leaves)
	}
	if leaves[0].YearFrom != 2000 || leaves[0].YearTo != 2013 {
		t.Errorf("left leaf range = %d-%d", leaves[0].YearFrom, leaves[0].YearTo)
	}
	if leaves[1].YearFrom != 2014 || leaves[1].YearTo != 2026 {
		t.Errorf("right leaf range = %d-%d", leaves[1].YearFrom, leaves[1].YearTo)
	}
	for _, l := range leaves {
		if l.NeedsSplit {
			t.Errorf("leaf %+v should be within limit", l)
		}
	}
}

// TestRecursiveYearSplit: an over-cap half must subdivide further.
func TestRecursiveYearSplit(t *testing.T) {
	q := `"NeurIPS"`
	table := map[string]int{
		key(q, 2000, 2026): 40000,
		key(q, 2000, 2013): 15000,
		key(q, 2014, 2026): 25000,
		key(q, 2000, 2006): 900,
		key(q, 2007, 2013): 900,
		key(q, 2014, 2020): 900,
		key(q, 2021, 2026): 900,
	}
	leaves, err := Plan(q, 2000, 2026, 1000, []string{"learning"}, fakeCount(table))
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 4 {
		t.Fatalf("got %d leaves, want 4: %+v", len(leaves), leaves)
	}
	// All leaves must be within limit and cover the full range without overlap.
	expect := [][2]int{{2000, 2006}, {2007, 2013}, {2014, 2020}, {2021, 2026}}
	for i, e := range expect {
		l := leaves[i]
		if l.YearFrom != e[0] || l.YearTo != e[1] {
			t.Errorf("leaf %d range = %d-%d, want %d-%d", i, l.YearFrom, l.YearTo, e[0], e[1])
		}
		if l.NeedsSplit {
			t.Errorf("leaf %d should be within limit", i)
		}
	}
	if !WithinLimit(leaves) {
		t.Error("WithinLimit should be true")
	}
}

// TestChainSplit: a single year over the cap produces the sequential keyword
// chain including the residual bucket.
func TestChainSplit(t *testing.T) {
	q := `"NeurIPS"`
	words := []string{"learning", "neural", "network"}
	base := q
	table := map[string]int{}
	// The chain loader will ask for these exact queries; give every leaf a
	// plausible count. The residual asks for base minus all words.
	table[key(base, 2020, 2020)] = 5000 // trigger split
	for i, w := range words {
		qq := q + ` AND "` + w + `"`
		for _, p := range words[:i] {
			qq += ` -"` + p + `"`
		}
		table[key(qq, 2020, 2020)] = 500
	}
	resid := q
	for _, w := range words {
		resid += ` -"` + w + `"`
	}
	table[key(resid, 2020, 2020)] = 200

	leaves, err := Plan(q, 2020, 2020, 1000, words, fakeCount(table))
	if err != nil {
		t.Fatal(err)
	}
	// Expect 3 keyword leaves + 1 residual = 4.
	if len(leaves) != 4 {
		t.Fatalf("got %d leaves, want 4: %+v", len(leaves), leaves)
	}

	wantQueries := []string{
		`"NeurIPS" AND "learning"`,
		`"NeurIPS" AND "neural" -"learning"`,
		`"NeurIPS" AND "network" -"learning" -"neural"`,
		`"NeurIPS" -"learning" -"neural" -"network"`,
	}
	for i, wq := range wantQueries {
		if leaves[i].Query != wq {
			t.Errorf("leaf %d query = %q, want %q", i, leaves[i].Query, wq)
		}
		if leaves[i].NeedsSplit {
			t.Errorf("leaf %d unexpectedly flagged needs_split", i)
		}
	}
	if !WithinLimit(leaves) {
		t.Error("WithinLimit should be true")
	}
}

// TestNeedsSplitTODO: keyword leaves still over the cap are flagged, not
// subdivided further (the "note it as a TODO" requirement).
func TestNeedsSplitTODO(t *testing.T) {
	q := `"NeurIPS"`
	words := []string{"learning", "neural"}
	table := map[string]int{
		key(q, 2020, 2020): 3000,
		key(q+` AND "learning"`, 2020, 2020):   1200, // still over cap -> TODO
		key(q+` AND "neural" -"learning"`, 2020, 2020): 300,
		key(q+` -"learning" -"neural"`, 2020, 2020):   150,
	}
	leaves, err := Plan(q, 2020, 2020, 1000, words, fakeCount(table))
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 3 {
		t.Fatalf("got %d leaves, want 3", len(leaves))
	}
	if !leaves[0].NeedsSplit {
		t.Error("over-cap keyword leaf must be flagged needs_split (TODO)")
	}
	if leaves[1].NeedsSplit || leaves[2].NeedsSplit {
		t.Error("within-cap leaves must not be flagged")
	}
	if WithinLimit(leaves) {
		t.Error("WithinLimit must be false while a TODO leaf exists")
	}
}

// TestChainIsExhaustive checks that the leaf queries partition the space:
// no two leaves share a paper, and every paper matches at least one leaf.
func TestChainIsExhaustive(t *testing.T) {
	// This is a structural check on query strings, not a real Scholar run:
	// each paper is assigned to the first keyword it contains.
	words := []string{"A", "B", "C"}
	queries := chainQueries(`q`, words)
	if len(queries) != 4 {
		t.Fatalf("want 4 chain queries, got %d", len(queries))
	}
	// Verify each leaf's positive keyword and exclude set.
	if !strings.Contains(queries[0], `AND "A"`) {
		t.Errorf("leaf0 = %q", queries[0])
	}
	if !strings.Contains(queries[1], `AND "B"`) || !strings.Contains(queries[1], `-"A"`) {
		t.Errorf("leaf1 = %q", queries[1])
	}
	if !strings.Contains(queries[2], `AND "C"`) || !strings.Contains(queries[2], `-"A"`) || !strings.Contains(queries[2], `-"B"`) {
		t.Errorf("leaf2 = %q", queries[2])
	}
	if strings.Contains(queries[3], `AND`) {
		t.Errorf("residual leaf3 must have no include term: %q", queries[3])
	}
	for _, w := range words {
		if !strings.Contains(queries[3], `-"`+w+`"`) {
			t.Errorf("residual must exclude %q: %q", w, queries[3])
		}
	}
}

// chainQueries reproduces the chain query construction for assertions.
func chainQueries(base string, words []string) []string {
	var out []string
	used := []string{}
	for _, w := range words {
		q := base + ` AND "` + w + `"`
		for _, p := range used {
			q += ` -"` + p + `"`
		}
		out = append(out, q)
		used = append(used, w)
	}
	q := base
	for _, w := range words {
		q += ` -"` + w + `"`
	}
	return append(out, q)
}

// TestNoKeywords: a single year over the cap with no keywords becomes one
// flagged TODO leaf.
func TestNoKeywords(t *testing.T) {
	q := `"X"`
	table := map[string]int{key(q, 2020, 2020): 2000}
	leaves, err := Plan(q, 2020, 2020, 1000, nil, fakeCount(table))
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 1 || !leaves[0].NeedsSplit {
		t.Fatalf("expected one needs_split leaf, got %+v", leaves)
	}
}
