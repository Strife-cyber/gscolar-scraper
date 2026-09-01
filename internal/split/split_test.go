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

// TestTwoYearLeaf: a 2-year window under the cap stays one task.
func TestTwoYearLeaf(t *testing.T) {
	q := `"ICML"`
	table := map[string]int{key(q, 2000, 2001): 900}
	leaves, err := Plan(q, 2000, 2001, 1000, []string{"learning", "neural"}, fakeCount(table), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 1 {
		t.Fatalf("got %d leaves, want 1", len(leaves))
	}
	if leaves[0].NeedsSplit || leaves[0].YearFrom != 2000 || leaves[0].YearTo != 2001 {
		t.Errorf("unexpected leaf: %+v", leaves[0])
	}
}

// TestTwoYearWindowSplit: a 2-year window over the cap narrows to its two
// single years.
func TestTwoYearWindowSplit(t *testing.T) {
	q := `"ICML"`
	table := map[string]int{
		key(q, 2000, 2001): 5000,
		key(q, 2000, 2000): 800,
		key(q, 2001, 2001): 700,
	}
	leaves, err := Plan(q, 2000, 2001, 1000, []string{"learning"}, fakeCount(table), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 2 {
		t.Fatalf("got %d leaves, want 2: %+v", len(leaves), leaves)
	}
	if leaves[0].YearFrom != 2000 || leaves[0].YearTo != 2000 {
		t.Errorf("leaf 0 range = %d-%d, want 2000-2000", leaves[0].YearFrom, leaves[0].YearTo)
	}
	if leaves[1].YearFrom != 2001 || leaves[1].YearTo != 2001 {
		t.Errorf("leaf 1 range = %d-%d, want 2001-2001", leaves[1].YearFrom, leaves[1].YearTo)
	}
	for _, l := range leaves {
		if l.NeedsSplit {
			t.Errorf("leaf %+v should be within limit", l)
		}
	}
}

// TestMultiWindowPlan: a mixture across several windows — the oversized pair
// splits into single years while the rest stay as 2-year windows.
func TestMultiWindowPlan(t *testing.T) {
	q := `"NeurIPS"`
	table := map[string]int{
		key(q, 2000, 2001): 5000, // over cap -> single years
		key(q, 2000, 2000): 900,
		key(q, 2001, 2001): 900,
		key(q, 2002, 2003): 900,
		key(q, 2004, 2004): 900, // trailing odd year
	}
	leaves, err := Plan(q, 2000, 2004, 1000, []string{"learning"}, fakeCount(table), 5)
	if err != nil {
		t.Fatal(err)
	}
	expect := [][2]int{{2000, 2000}, {2001, 2001}, {2002, 2003}, {2004, 2004}}
	if len(leaves) != len(expect) {
		t.Fatalf("got %d leaves, want %d: %+v", len(leaves), len(expect), leaves)
	}
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

// TestNoFullRangeCount: the planner must never issue a count for the whole
// range; it walks 2-year windows from the start, so no single search spans
// more than two years.
func TestNoFullRangeCount(t *testing.T) {
	q := `"ICML"`
	limit := 1000
	var asked [][2]int
	count := func(query string, yearFrom, yearTo int) (int, bool) {
		asked = append(asked, [2]int{yearFrom, yearTo})
		if yearTo == yearFrom {
			return 900, true
		}
		return 1500, true // any 2-year window is over the cap
	}
	leaves, err := Plan(q, 2000, 2005, limit, []string{"learning"}, count, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range asked {
		if p == [2]int{2000, 2005} {
			t.Fatal("whole-range count must never be requested")
		}
		if p[1]-p[0] > 1 {
			t.Fatalf("a count spanned more than two years: %v", p)
		}
	}
	if len(asked) == 0 || asked[0] != [2]int{2000, 2001} {
		t.Fatalf("first count must be the first 2-year window, got %v", asked)
	}
	if len(leaves) != 6 {
		t.Fatalf("got %d leaves, want 6 (each year separate): %+v", len(leaves), leaves)
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

	leaves, err := Plan(q, 2020, 2020, 1000, words, fakeCount(table), 5)
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
		key(q, 2020, 2020):                             3000,
		key(q+` AND "learning"`, 2020, 2020):           1200, // still over cap -> TODO
		key(q+` AND "neural" -"learning"`, 2020, 2020): 300,
		key(q+` -"learning" -"neural"`, 2020, 2020):    150,
	}
	leaves, err := Plan(q, 2020, 2020, 1000, words, fakeCount(table), 5)
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

// TestChainStopsWhenKeywordCoversAll: when the first keyword already accounts
// for every result (its bucket equals the base count), the chain verifies the
// remaining residual is zero and stops — no searches for the later keywords.
func TestChainStopsWhenKeywordCoversAll(t *testing.T) {
	q := `"ICML"`
	base := 582
	table := map[string]int{
		key(q, 2020, 2020):                   base, // single year over the cap
		key(q+` AND "learning"`, 2020, 2020): base, // covers everything
		key(q+` -"learning"`, 2020, 2020):    0,    // verified: nothing left
	}
	var asked []string
	planCount := func(query string, yearFrom, yearTo int) (int, bool) {
		asked = append(asked, query)
		return fakeCount(table)(query, yearFrom, yearTo)
	}
	leaves, err := Plan(q, 2020, 2020, 300, []string{"learning", "neural", "network"}, planCount, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 1 {
		t.Fatalf("got %d leaves, want 1: %+v", len(leaves), leaves)
	}
	if leaves[0].Query != q+` AND "learning"` {
		t.Errorf("leaf query = %q, want %q", leaves[0].Query, q+` AND "learning"`)
	}
	if !leaves[0].NeedsSplit {
		t.Error("582 > 300 should be flagged needs_split")
	}
	// Only the base, the first bucket and the verification residual were counted
	// — never "neural" or "network".
	if len(asked) != 3 {
		t.Fatalf("count called %d times, want 3: %q", len(asked), asked)
	}
}

// TestChainSkipsZeroBucketButContinues: a keyword bucket with zero results is
// omitted, but the chain keeps going — a later keyword can still have papers.
func TestChainSkipsZeroBucketButContinues(t *testing.T) {
	q := `"ICML"`
	table := map[string]int{
		key(q, 2020, 2020):                                        900,
		key(q+` AND "learning"`, 2020, 2020):                      100,
		key(q+` AND "neural" -"learning"`, 2020, 2020):            0, // empty -> skipped
		key(q+` AND "network" -"learning" -"neural"`, 2020, 2020): 150,
		key(q+` -"learning" -"neural" -"network"`, 2020, 2020):    20,
	}
	leaves, err := Plan(q, 2020, 2020, 300, []string{"learning", "neural", "network"}, fakeCount(table), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 3 {
		t.Fatalf("got %d leaves, want 3 (neural skipped): %+v", len(leaves), leaves)
	}
	want := []string{
		q + ` AND "learning"`,
		q + ` AND "network" -"learning" -"neural"`,
		q + ` -"learning" -"neural" -"network"`,
	}
	for i, wq := range want {
		if leaves[i].Query != wq {
			t.Errorf("leaf %d query = %q, want %q", i, leaves[i].Query, wq)
		}
	}
}

// TestChainDropsDegenerateKeywords: a keyword that is part of the venue name
// ("learning" in "International Conference on Machine Learning") cannot split
// the space — including it matches every paper and excluding it removes every
// paper. It is dropped so the remaining keywords actually partition the year.
func TestChainDropsDegenerateKeywords(t *testing.T) {
	base := `"International Conference on Machine Learning"`
	table := map[string]int{
		key(base, 2002, 2002):                 582,
		key(base+` AND "neural"`, 2002, 2002): 200,
		key(base+` -"neural"`, 2002, 2002):    300,
	}
	leaves, err := Plan(base, 2002, 2002, 300, []string{"learning", "neural"}, fakeCount(table), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 2 {
		t.Fatalf("got %d leaves, want 2: %+v", len(leaves), leaves)
	}
	want := []string{
		base + ` AND "neural"`,
		base + ` -"neural"`,
	}
	for i, wq := range want {
		if leaves[i].Query != wq {
			t.Errorf("leaf %d query = %q, want %q", i, leaves[i].Query, wq)
		}
		if strings.Contains(strings.ToLower(leaves[i].Query), `"learning"`) {
			t.Errorf("leaf %d still references the degenerate keyword: %q", i, leaves[i].Query)
		}
	}
	if leaves[0].NeedsSplit || leaves[1].NeedsSplit {
		t.Error("both leaves are within the 300 cap and must not be flagged")
	}
}

// TestChainAllKeywordsDegenerate: when every keyword is part of the venue name
// the chain has nothing to split with and falls back to a single needs_split
// leaf (the TODO flag), matching the no-keywords behaviour.
func TestChainAllKeywordsDegenerate(t *testing.T) {
	base := `"International Conference on Machine Learning"`
	table := map[string]int{key(base, 2002, 2002): 582}
	leaves, err := Plan(base, 2002, 2002, 300, []string{"learning", "conference"}, fakeCount(table), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 1 || !leaves[0].NeedsSplit {
		t.Fatalf("expected one needs_split leaf, got %+v", leaves)
	}
	if leaves[0].Query != base {
		t.Errorf("leaf query = %q, want %q", leaves[0].Query, base)
	}
}

// TestChainCapsKeywordCount: maxKeywords bounds every query to that many
// keyword terms. Keywords beyond the cap are never searched (their papers land
// in the residual), so no query accumulates an unbounded subtraction list and
// the partition stays exhaustive.
func TestChainCapsKeywordCount(t *testing.T) {
	base := `"ICML"`
	words := []string{"neural", "network", "deep", "reinforcement", "attention", "graph"}
	table := map[string]int{
		key(base, 2020, 2020): 900,
	}
	for i, w := range words[:3] {
		q := base + ` AND "` + w + `"`
		for _, p := range words[:i] {
			q += ` -"` + p + `"`
		}
		table[key(q, 2020, 2020)] = 100
	}
	// The residual excludes only the first maxKeywords (3), not all six.
	table[key(base+` -"neural" -"network" -"deep"`, 2020, 2020)] = 300

	var asked []string
	planCount := func(query string, yFrom, yTo int) (int, bool) {
		asked = append(asked, query)
		return fakeCount(table)(query, yFrom, yTo)
	}
	leaves, err := Plan(base, 2020, 2020, 300, words, planCount, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 4 {
		t.Fatalf("got %d leaves, want 4: %+v", len(leaves), leaves)
	}
	want := []string{
		base + ` AND "neural"`,
		base + ` AND "network" -"neural"`,
		base + ` AND "deep" -"neural" -"network"`,
		base + ` -"neural" -"network" -"deep"`,
	}
	for i, wq := range want {
		if leaves[i].Query != wq {
			t.Errorf("leaf %d query = %q, want %q", i, leaves[i].Query, wq)
		}
	}
	for _, q := range asked {
		if strings.Contains(q, `"reinforcement"`) || strings.Contains(q, `"attention"`) || strings.Contains(q, `"graph"`) {
			t.Errorf("keywords beyond the cap must never be searched: %q", q)
		}
	}
}

// TestChainZeroMaxKeywords: maxKeywords 0 disables keyword splitting entirely
// and the year falls back to a single needs_split leaf, like having no
// keywords at all.
func TestChainZeroMaxKeywords(t *testing.T) {
	base := `"ICML"`
	table := map[string]int{key(base, 2020, 2020): 2000}
	leaves, err := Plan(base, 2020, 2020, 1000, []string{"neural", "network"}, fakeCount(table), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 1 || !leaves[0].NeedsSplit {
		t.Fatalf("expected one needs_split leaf, got %+v", leaves)
	}
	if leaves[0].Query != base {
		t.Errorf("leaf query = %q, want %q", leaves[0].Query, base)
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

// TestResolveOverCap: a 1200-result leaf under a 1000 cap resolves into two
// under-cap leaves by the best-balanced keyword ("neural", 600/600).
func TestResolveOverCap(t *testing.T) {
	base := `"ICML"`
	leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: 1200, HasCount: true}
	table := map[string]int{
		key(base+` AND "learning"`, 2020, 2020): 700,
		key(base+` AND "neural"`, 2020, 2020):   600,
	}
	out, ok := ResolveOverCap(leaf, []string{"learning", "neural"}, fakeCount(table), 4, 0.1, 1000)
	if !ok {
		t.Fatalf("expected resolution, ok=false")
	}
	if len(out) != 2 {
		t.Fatalf("got %d leaves, want 2: %+v", len(out), out)
	}
	want := []string{
		base + ` AND "neural"`,
		base + ` -"neural"`,
	}
	for i, wq := range want {
		if out[i].Query != wq {
			t.Errorf("leaf %d query = %q, want %q", i, out[i].Query, wq)
		}
		if out[i].NeedsSplit {
			t.Errorf("leaf %d must not be flagged needs_split", i)
		}
	}
	if !WithinLimit(out) {
		t.Error("WithinLimit(out) should be true")
	}
}

// TestResolveOverCapRespectsProbeBudget: probesPerNode limits count() calls; a
// node beyond the first keyword is never probed.
func TestResolveOverCapRespectsProbeBudget(t *testing.T) {
	base := `"ICML"`
	leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: 1200, HasCount: true}
	table := map[string]int{
		key(base+` AND "learning"`, 2020, 2020): 700,
		key(base+` AND "neural"`, 2020, 2020):   600,
	}
	calls := 0
	planCount := func(query string, yFrom, yTo int) (int, bool) {
		calls++
		return fakeCount(table)(query, yFrom, yTo)
	}
	out, ok := ResolveOverCap(leaf, []string{"learning", "neural"}, planCount, 1, 0.1, 1000)
	if !ok {
		t.Fatalf("expected resolution, ok=false")
	}
	if calls > 1 {
		t.Errorf("count() called %d times, want at most 1 with probesPerNode=1", calls)
	}
	if len(out) == 0 {
		t.Fatal("expected leaves")
	}
	// With only "learning" probed, the split is on "learning" (700/500).
	for _, l := range out {
		if strings.Contains(l.Query, `"neural"`) {
			t.Errorf("neural must never be probed or referenced under budget 1: %q", l.Query)
		}
	}
}

// TestResolveOverCapNoDividingKeyword: every keyword covers all (or none) of the
// results, so no split is possible -> ok=false.
func TestResolveOverCapNoDividingKeyword(t *testing.T) {
	base := `"ICML"`
	leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: 1200, HasCount: true}
	table := map[string]int{
		key(base+` AND "learning"`, 2020, 2020): 1200, // covers everything
		key(base+` AND "neural"`, 2020, 2020):   0,    // matches nothing
	}
	out, ok := ResolveOverCap(leaf, []string{"learning", "neural"}, fakeCount(table), 4, 0.1, 1000)
	if ok {
		t.Fatalf("expected ok=false, got resolved leaves %+v", out)
	}
	if out != nil {
		t.Errorf("expected nil leaves on failure, got %+v", out)
	}
}

// TestResolveOverCapMinBalance: a lopsided keyword below minBalance is not used,
// so the split falls through to ok=false.
func TestResolveOverCapMinBalance(t *testing.T) {
	base := `"ICML"`
	leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: 990, HasCount: true}
	// 990/10 is balance ~0.02, below minBalance 0.5 -> rejected.
	table := map[string]int{
		key(base+` AND "learning"`, 2020, 2020): 980,
	}
	out, ok := ResolveOverCap(leaf, []string{"learning"}, fakeCount(table), 4, 0.5, 500)
	if ok {
		t.Fatalf("expected ok=false for lopsided split, got %+v", out)
	}
}

// TestResolveOverCapMultiLevelSimulated: a 2500-result leaf under a 1000 cap is
// split into leaves that partition a simulated paper set exactly. The count
// function evaluates the AND/- predicates against ground-truth papers, so the
// resulting buckets must cover every paper once and stay under the cap — the
// paper's partition and capacity invariants, tested against real query shapes.
func TestResolveOverCapMultiLevelSimulated(t *testing.T) {
	// 2500 papers: "neural" on 1300, "attention" on 1600, "graph" on 900; the
	// three keywords overlap partially. Each paper's keyword set is whatever the
	// built-in generator assigns.
	papers := genPapers(2500)
	count := func(query string, yFrom, yTo int) (int, bool) {
		return membershipCount(papers, query), true
	}
	base := `"ICML"`
	leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: len(papers), HasCount: true}
	out, ok := ResolveOverCap(leaf, []string{"neural", "attention", "graph"}, count, 6, 0.05, 1000)
	if !ok {
		t.Fatalf("expected resolution, ok=false")
	}
	for _, l := range out {
		if l.Count > 1000 {
			t.Errorf("leaf %q count %d exceeds cap", l.Query, l.Count)
		}
	}
	// Capacity invariants everywhere: the sum of leaf counts equals the total.
	sum := 0
	for _, l := range out {
		sum += l.Count
	}
	if sum != len(papers) {
		t.Errorf("sum of leaf counts %d != total papers %d", sum, len(papers))
	}
}

// genPapers produces n papers with the known keyword distribution used above.
func genPapers(n int) []map[string]bool {
	out := make([]map[string]bool, n)
	for i := 0; i < n; i++ {
		kw := map[string]bool{}
		if i%19 < 10 { // ~10/19 have "neural"
			kw["neural"] = true
		}
		if i%31 < 20 { // ~20/31 have "attention"
			kw["attention"] = true
		}
		if i%50 < 9 { // ~9/50 have "graph"
			kw["graph"] = true
		}
		out[i] = kw
	}
	return out
}

// membershipCount evaluates a Scholar query string (AND "a" / -"b" predicates)
// against a ground-truth paper set, returning how many papers match. This makes
// the splitter run against a real count oracle instead of a hand-keyed table.
func membershipCount(papers []map[string]bool, query string) int {
	includes, excludes := predicates(query)
	n := 0
	for _, kw := range papers {
		ok := true
		for _, w := range includes {
			if !kw[w] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		for _, w := range excludes {
			if kw[w] {
				ok = false
				break
			}
		}
		if ok {
			n++
		}
	}
	return n
}

// predicates parses ` AND "a"` and ` -"b"` suffixes out of a query string.
func predicates(query string) (includes, excludes []string) {
	for i := 0; i < len(query); i++ {
		if query[i] == '"' {
			j := i + 1
			for j < len(query) && query[j] != '"' {
				j++
			}
			word := query[i+1 : j]
			if i >= 5 && query[i-5:i] == " AND " {
				includes = append(includes, word)
			} else if i >= 2 && query[i-2:i] == " -" {
				excludes = append(excludes, word)
			}
			i = j
		}
	}
	return includes, excludes
}

// TestNoKeywords: a single year over the cap with no keywords becomes one
// flagged TODO leaf.
func TestNoKeywords(t *testing.T) {
	q := `"X"`
	table := map[string]int{key(q, 2020, 2020): 2000}
	leaves, err := Plan(q, 2020, 2020, 1000, nil, fakeCount(table), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 1 || !leaves[0].NeedsSplit {
		t.Fatalf("expected one needs_split leaf, got %+v", leaves)
	}
}
