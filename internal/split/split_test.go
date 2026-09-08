package split

import (
	"fmt"
	"testing"
)

// fakeCount builds a CountFunc from a table. A missing key simulates a count
// that could not be read (ok=false).
func fakeCount(table map[string]int) CountFunc {
	return func(query string, yearFrom, yearTo int) (int, bool) {
		key := fmt.Sprintf("%s|%d|%d", query, yearFrom, yearTo)
		n, ok := table[key]
		return n, ok
	}
}

func key(query string, ylo, yhi int) string { return fmt.Sprintf("%s|%d|%d", query, ylo, yhi) }

// ---------------------------------------------------------------------------
// Year windowing (Plan / planWindow / planRange)
// ---------------------------------------------------------------------------

// TestTwoYearLeaf: a 2-year window under the cap stays one task, and produces
// exactly one leaf with NeedsSplit=false.
func TestTwoYearLeaf(t *testing.T) {
	q := `"ICML"`
	table := map[string]int{key(q, 2000, 2001): 900}
	leaves, err := Plan(q, 2000, 2001, 1000, 5, 8, []string{"learning", "neural"}, fakeCount(table), 0.15)
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

// TestUnderLimitNoProbe (Test 1): a node already under the cap must not
// trigger any keyword count probe.
func TestUnderLimitNoProbe(t *testing.T) {
	q := `"ICML"`
	calls := 0
	count := func(query string, yFrom, yTo int) (int, bool) {
		calls++
		if query == q && yFrom == 2020 && yTo == 2020 {
			return 500, true
		}
		t.Fatalf("unexpected probe for query %q", query)
		return 0, false
	}
	leaves, err := Plan(q, 2020, 2020, 1000, 5, 8, []string{"learning", "neural"}, count, 0.15)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 1 || leaves[0].NeedsSplit {
		t.Fatalf("expected one within-cap leaf, got %+v", leaves)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 count call (the base window), got %d", calls)
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
	leaves, err := Plan(q, 2000, 2001, 1000, 5, 8, []string{"learning"}, fakeCount(table), 0.15)
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

// TestMultiWindowPlan (Test 11): a mixture across several windows — the
// oversized pair splits into single years while the rest stay as 2-year
// windows, including a trailing odd year.
func TestMultiWindowPlan(t *testing.T) {
	q := `"NeurIPS"`
	table := map[string]int{
		key(q, 2000, 2001): 5000, // over cap -> single years
		key(q, 2000, 2000): 900,
		key(q, 2001, 2001): 900,
		key(q, 2002, 2003): 900,
		key(q, 2004, 2004): 900, // trailing odd year
	}
	leaves, err := Plan(q, 2000, 2004, 1000, 5, 8, []string{"learning"}, fakeCount(table), 0.15)
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
	leaves, err := Plan(q, 2000, 2005, limit, 5, 8, []string{"learning"}, count, 0.15)
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

// TestNoKeywords (Test 2, via Plan): a single year over the cap with no
// keywords becomes one flagged TODO leaf; ResolveOverCap itself must fail.
func TestNoKeywords(t *testing.T) {
	q := `"X"`
	table := map[string]int{key(q, 2020, 2020): 2000}
	leaves, err := Plan(q, 2020, 2020, 1000, 5, 8, nil, fakeCount(table), 0.15)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 1 || !leaves[0].NeedsSplit {
		t.Fatalf("expected one needs_split leaf, got %+v", leaves)
	}
}

// TestResolveOverCapNoKeywords (Test 2): ResolveOverCap on an over-limit leaf
// with zero usable keywords must fail resolution outright.
func TestResolveOverCapNoKeywords(t *testing.T) {
	leaf := Leaf{Query: `"X"`, YearFrom: 2020, YearTo: 2020, Count: 2000, HasCount: true}
	out, ok := ResolveOverCap(leaf, nil, fakeCount(nil), 8, 0.15, 1000)
	if ok {
		t.Fatalf("expected ok=false, got %+v", out)
	}
	if out != nil {
		t.Errorf("expected nil leaves on failure, got %+v", out)
	}
}

// TestAllKeywordsDegenerate: when every keyword is part of the venue name the
// planner has nothing to split with (they are filtered before ResolveOverCap
// even runs) and falls back to a single needs_split leaf.
func TestAllKeywordsDegenerate(t *testing.T) {
	base := `"International Conference on Machine Learning"`
	table := map[string]int{key(base, 2002, 2002): 582}
	leaves, err := Plan(base, 2002, 2002, 300, 5, 8, []string{"learning", "conference"}, fakeCount(table), 0.15)
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

// TestZeroMaxKeywords: maxKeywords 0 disables keyword splitting entirely and
// the year falls back to a single needs_split leaf, like having no keywords.
func TestZeroMaxKeywords(t *testing.T) {
	base := `"ICML"`
	calls := 0
	count := func(query string, yFrom, yTo int) (int, bool) {
		calls++
		if query == base {
			return 2000, true
		}
		t.Fatalf("unexpected probe for query %q with maxKeywords=0", query)
		return 0, false
	}
	leaves, err := Plan(base, 2020, 2020, 1000, 0, 8, []string{"neural", "network"}, count, 0.15)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 1 || !leaves[0].NeedsSplit {
		t.Fatalf("expected one needs_split leaf, got %+v", leaves)
	}
	if leaves[0].Query != base {
		t.Errorf("leaf query = %q, want %q", leaves[0].Query, base)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 count call (the base window), got %d", calls)
	}
}

// TestNegativeMaxKeywordsIsUnbounded: the documented convention is that a
// negative maxKeywords leaves the keyword list uncapped.
func TestNegativeMaxKeywordsIsUnbounded(t *testing.T) {
	got := capKeywords([]string{"a", "b", "c"}, -1)
	if len(got) != 3 {
		t.Fatalf("capKeywords with negative maxKeywords = %v, want all 3 kept", got)
	}
}

// TestDropDegenerateBeforeMaxKeywordsCap: degenerate keywords must not consume
// one of the maxKeywords slots — they're dropped first.
func TestDropDegenerateBeforeMaxKeywordsCap(t *testing.T) {
	base := `"International Conference on Machine Learning"`
	// "learning" is degenerate. With maxKeywords=1 the single surviving slot
	// must go to "neural", not be wasted on the degenerate word.
	table := map[string]int{
		key(base, 2020, 2020):                 900,
		key(base+` AND "neural"`, 2020, 2020): 400,
	}
	var asked []string
	count := func(query string, yFrom, yTo int) (int, bool) {
		asked = append(asked, query)
		return fakeCount(table)(query, yFrom, yTo)
	}
	leaves, err := Plan(base, 2020, 2020, 500, 1, 8, []string{"learning", "neural"}, count, 0.1)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 2 {
		t.Fatalf("got %d leaves, want 2: %+v", len(leaves), leaves)
	}
	for _, q := range asked {
		if q == base+` AND "learning"` {
			t.Fatalf("degenerate keyword must never be probed: %q", q)
		}
	}
}

// TestPlanRejectsInvalidMinBalance: minBalance outside [0,1] is rejected at
// the public API boundary.
func TestPlanRejectsInvalidMinBalance(t *testing.T) {
	q := `"ICML"`
	if _, err := Plan(q, 2020, 2020, 1000, 5, 8, nil, fakeCount(nil), -0.1); err == nil {
		t.Error("expected error for minBalance < 0")
	}
	if _, err := Plan(q, 2020, 2020, 1000, 5, 8, nil, fakeCount(nil), 1.1); err == nil {
		t.Error("expected error for minBalance > 1")
	}
}

// ---------------------------------------------------------------------------
// ResolveOverCap / resolveNode
// ---------------------------------------------------------------------------

// TestResolveOverCap: a 1200-result leaf under a 1000 cap resolves into two
// under-cap leaves on the FIRST acceptable splitter in ranked order — once
// "learning" returns a valid 700/500 split, "neural" is never even probed
// (one Scholar search per node, not one per candidate).
func TestResolveOverCap(t *testing.T) {
	base := `"ICML"`
	leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: 1200, HasCount: true}
	table := map[string]int{
		key(base+` AND "learning"`, 2020, 2020): 700,
		key(base+` AND "neural"`, 2020, 2020):   600, // better balance, never reached
	}
	probed := 0
	count := func(q string, yf, yt int) (int, bool) {
		probed++
		return fakeCount(table)(q, yf, yt)
	}
	out, ok := ResolveOverCap(leaf, []string{"learning", "neural"}, count, 4, 0.1, 1000)
	if !ok {
		t.Fatalf("expected resolution, ok=false")
	}
	if len(out) != 2 {
		t.Fatalf("got %d leaves, want 2: %+v", len(out), out)
	}
	want := []string{
		base + ` AND "learning"`,
		base + ` -"learning"`,
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
	if probed != 1 {
		t.Errorf("probed %d candidates, want exactly 1 (stop at first acceptable split)", probed)
	}
}

// TestProbeBudgetPerNode (Test 3): probesPerNode is a per-node budget, not a
// global one. The root consumes probesPerNode probes selecting its splitter,
// and each recursive child gets its own fresh probesPerNode budget again. This
// test must fail if a global probe counter is reintroduced.
func TestProbeBudgetPerNode(t *testing.T) {
	base := `"ICML"`
	// 2400 results split evenly: "A" 1200/1200 at the root.
	// Left child (with "A"): "B" splits it 600/600.
	// Right child (without "A"): "C" splits it 600/600.
	// Each node has 2 keyword candidates to probe, so probesPerNode=2 must
	// suffice at every node if the budget truly resets per node.
	table := map[string]int{
		key(base+` AND "A"`, 2020, 2020):              1200,
		key(base+` AND "B"`, 2020, 2020):              50, // decoy at root, worse balance than A
		key(base+` AND "A" AND "B"`, 2020, 2020):      600,
		key(base+` AND "A" AND "C" -"B"`, 2020, 2020): 10, // decoy in left subtree
		key(base+` AND "C" -"A"`, 2020, 2020):         600,
		key(base+` AND "B" -"A" -"C"`, 2020, 2020):    10, // decoy in right subtree
	}
	calls := map[string]int{}
	count := func(query string, yFrom, yTo int) (int, bool) {
		calls[query]++
		return fakeCount(table)(query, yFrom, yTo)
	}
	leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: 2400, HasCount: true}
	// Root candidates order: A (0.0 balance-winner), B. Left/right subtrees see
	// remaining keywords [B, C] or [A dropped] depending on branch; construct
	// keyword order so each node still has exactly 2 candidates within budget.
	out, ok := ResolveOverCap(leaf, []string{"A", "B", "C"}, count, 2, 0.1, 1000)
	if !ok {
		t.Fatalf("expected resolution, ok=false")
	}
	for _, l := range out {
		if l.Count > 1000 {
			t.Errorf("leaf %q count %d exceeds cap", l.Query, l.Count)
		}
	}
	// Every individual query was probed at most once per node invocation; the
	// key correctness signal is that resolution succeeded at all, which is only
	// possible if each node (root, left child, right child) got its own budget
	// of 2 probes rather than sharing a global budget of 2 across the whole
	// recursion (which would starve the children).
	totalCalls := 0
	for _, n := range calls {
		totalCalls += n
	}
	if totalCalls < 3 {
		t.Fatalf("expected probing at multiple nodes (root + 2 children), got %d total probe calls: %v", totalCalls, calls)
	}
}

// TestResolveOverCapRespectsProbeBudget: probesPerNode limits count() calls at
// a single node; a keyword beyond the first is never probed.
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
	for _, l := range out {
		if containsWord(l.Query, `"neural"`) {
			t.Errorf("neural must never be probed or referenced under budget 1: %q", l.Query)
		}
	}
}

func containsWord(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestResolveOverCapNoDividingKeyword: every keyword covers all (or none) of
// the results, so no split is possible -> ok=false.
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

// TestInvalidCounts (Test 4): counts of 0, size, size+1, and negative values
// can never become valid split candidates.
func TestInvalidCounts(t *testing.T) {
	base := `"ICML"`
	size := 1000
	cases := []struct {
		name string
		n    int
	}{
		{"zero", 0},
		{"equalsSize", size},
		{"exceedsSize", size + 1},
		{"negative", -5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: size, HasCount: true}
			table := map[string]int{
				key(base+` AND "kw"`, 2020, 2020): c.n,
			}
			out, ok := ResolveOverCap(leaf, []string{"kw"}, fakeCount(table), 4, 0.0, 500)
			if ok {
				t.Fatalf("count=%d should never be a valid split, got resolved %+v", c.n, out)
			}
			if out != nil {
				t.Errorf("expected nil leaves on failure, got %+v", out)
			}
		})
	}
}

// TestUnknownCountNeverUsed: an unknown count (ok=false) must never be used to
// derive a without-count or select a splitter.
func TestUnknownCountNeverUsed(t *testing.T) {
	base := `"ICML"`
	leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: 1000, HasCount: true}
	// "unknown" is not in the table -> ok=false. "good" is a valid splitter.
	table := map[string]int{
		key(base+` AND "good"`, 2020, 2020): 500,
	}
	out, ok := ResolveOverCap(leaf, []string{"unknown", "good"}, fakeCount(table), 4, 0.1, 800)
	if !ok {
		t.Fatalf("expected resolution via the good keyword, got ok=false")
	}
	for _, l := range out {
		if containsWord(l.Query, `"unknown"`) {
			t.Errorf("unknown-count keyword must never appear in a resolved leaf: %q", l.Query)
		}
	}
}

// TestResolveOverCapMinBalance (Test 5): a lopsided keyword below minBalance
// is not used, so the split falls through to ok=false; a balanced candidate
// among several is preferred when it clears the threshold.
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

// TestBalanceThresholdPrefersBalancedCandidate (Test 5): given a lopsided
// candidate A and a balanced candidate B, B is selected.
func TestBalanceThresholdPrefersBalancedCandidate(t *testing.T) {
	base := `"ICML"`
	leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: 1000, HasCount: true}
	table := map[string]int{
		key(base+` AND "A"`, 2020, 2020): 950, // lopsided: balance ~0.1
		key(base+` AND "B"`, 2020, 2020): 500, // perfectly balanced
	}
	out, ok := ResolveOverCap(leaf, []string{"A", "B"}, fakeCount(table), 4, 0.3, 600)
	if !ok {
		t.Fatalf("expected resolution, ok=false")
	}
	for _, l := range out {
		if containsWord(l.Query, `"A"`) {
			t.Errorf("lopsided candidate A must not be selected when B is balanced: %q", l.Query)
		}
	}
}

// TestBalanceThresholdAllBelowMinFails: if every candidate is below
// minBalance, resolution must fail.
func TestBalanceThresholdAllBelowMinFails(t *testing.T) {
	base := `"ICML"`
	leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: 1000, HasCount: true}
	table := map[string]int{
		key(base+` AND "A"`, 2020, 2020): 950,
		key(base+` AND "B"`, 2020, 2020): 900,
	}
	out, ok := ResolveOverCap(leaf, []string{"A", "B"}, fakeCount(table), 4, 0.5, 600)
	if ok {
		t.Fatalf("expected ok=false, all candidates below minBalance, got %+v", out)
	}
}

// TestDegenerateKeywordNeverProbed (Test 6): a keyword that is a substring of
// the base query must be removed before ResolveOverCap probes anything — the
// planner filters it via dropDegenerateKeywords before recursion starts.
func TestDegenerateKeywordNeverProbed(t *testing.T) {
	base := `"International Conference on Machine Learning"`
	table := map[string]int{
		key(base, 2020, 2020):                  5000,
		key(base+` AND "systems"`, 2020, 2020): 2400,
	}
	var asked []string
	count := func(query string, yFrom, yTo int) (int, bool) {
		asked = append(asked, query)
		return fakeCount(table)(query, yFrom, yTo)
	}
	leaves, err := Plan(base, 2020, 2020, 3000, 5, 8, []string{"learning", "systems"}, count, 0.1)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range asked {
		if containsWord(q, `"learning"`) {
			t.Fatalf("degenerate keyword must never reach count(): %q", q)
		}
	}
	if len(leaves) != 2 {
		t.Fatalf("got %d leaves, want 2: %+v", len(leaves), leaves)
	}
}

// TestKeywordNotReusedInDescendants (Test 7): after splitting on a keyword,
// no descendant of either branch may probe it again.
func TestKeywordNotReusedInDescendants(t *testing.T) {
	base := `"ICML"`
	leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: 4000, HasCount: true}
	table := map[string]int{
		key(base+` AND "A"`, 2020, 2020):         2000, // root split, balanced
		key(base+` AND "A" AND "B"`, 2020, 2020): 1000,
		key(base+` AND "B" -"A"`, 2020, 2020):    1000,
	}
	var asked []string
	count := func(query string, yFrom, yTo int) (int, bool) {
		asked = append(asked, query)
		return fakeCount(table)(query, yFrom, yTo)
	}
	out, ok := ResolveOverCap(leaf, []string{"A", "B"}, count, 4, 0.1, 1500)
	if !ok {
		t.Fatalf("expected resolution, ok=false")
	}
	// Once A is selected as the root splitter, no later probe query may name A
	// again as a candidate predicate (it may still appear once, fixed, in the
	// with-branch's accumulated include list) — i.e. no query's predicate list
	// contains "A" more than once.
	for _, q := range asked {
		includes, excludes := predicates(q)
		count := 0
		for _, w := range append(append([]string{}, includes...), excludes...) {
			if w == "A" {
				count++
			}
		}
		if count > 1 {
			t.Fatalf("query names A more than once: %q", q)
		}
	}
	if !WithinLimit(out) {
		t.Error("expected all leaves within limit")
	}
}

// TestDisjointRecursion (Test 8): using a fake finite paper universe, the
// generated leaf predicates must be pairwise disjoint and their union must
// cover the entire original node. This is the most important correctness
// test in the package.
func TestDisjointRecursion(t *testing.T) {
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

	// Assign every paper to whichever leaves it matches; each must match
	// exactly one.
	membership := make([]int, len(papers))
	for i := range membership {
		membership[i] = -1
	}
	for li, l := range out {
		includes, excludes := predicates(l.Query)
		for pi, kw := range papers {
			ok := true
			for _, w := range includes {
				if !kw[w] {
					ok = false
					break
				}
			}
			if ok {
				for _, w := range excludes {
					if kw[w] {
						ok = false
						break
					}
				}
			}
			if ok {
				if membership[pi] != -1 {
					t.Fatalf("paper %d matches both leaf %d and leaf %d: leaves not disjoint", pi, membership[pi], li)
				}
				membership[pi] = li
			}
		}
	}
	for pi, li := range membership {
		if li == -1 {
			t.Fatalf("paper %d matches no leaf: union is not exhaustive", pi)
		}
	}
}

// TestSuccessfulRecursiveResolution (Test 9): a few keyword splits reduce an
// over-limit node below the cap; every output leaf must be at or under the
// cap and not flagged needs_split.
func TestSuccessfulRecursiveResolution(t *testing.T) {
	papers := genPapers(2500)
	count := func(query string, yFrom, yTo int) (int, bool) {
		return membershipCount(papers, query), true
	}
	base := `"ICML"`
	limit := 1000
	leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: len(papers), HasCount: true}
	out, ok := ResolveOverCap(leaf, []string{"neural", "attention", "graph"}, count, 6, 0.05, limit)
	if !ok {
		t.Fatalf("expected resolution, ok=false")
	}
	sum := 0
	for _, l := range out {
		if l.Count > limit {
			t.Errorf("leaf %q count %d exceeds cap %d", l.Query, l.Count, limit)
		}
		if l.NeedsSplit {
			t.Errorf("leaf %q unexpectedly flagged needs_split", l.Query)
		}
		sum += l.Count
	}
	if sum != len(papers) {
		t.Errorf("sum of leaf counts %d != total papers %d", sum, len(papers))
	}
}

// TestFailedSubtreeIsAtomic (Test 10): when one branch can resolve but the
// other cannot, the whole ResolveOverCap call must fail atomically — no
// partial result may escape.
func TestFailedSubtreeIsAtomic(t *testing.T) {
	base := `"ICML"`
	leaf := Leaf{Query: base, YearFrom: 2020, YearTo: 2020, Count: 4000, HasCount: true}
	table := map[string]int{
		// Root splits on A: with=2000 (resolvable via B), without=2000 (stuck).
		key(base+` AND "A"`, 2020, 2020):         2000,
		key(base+` AND "A" AND "B"`, 2020, 2020): 1000, // with-side resolves fine
		// without-side ("-A") has only C left, but C doesn't divide it.
		key(base+` AND "C" -"A"`, 2020, 2020): 2000, // covers everything -> no split
	}
	out, ok := ResolveOverCap(leaf, []string{"A", "B", "C"}, fakeCount(table), 4, 0.1, 1500)
	if ok {
		t.Fatalf("expected ok=false since the without-A branch cannot resolve, got %+v", out)
	}
	if out != nil {
		t.Errorf("expected nil leaves when resolution fails, got %+v", out)
	}
}

// TestPlannerFallback (Test 12): when ResolveOverCap fails, planWindow must
// return the original single-year leaf with NeedsSplit=true, not a partial or
// fabricated success.
func TestPlannerFallback(t *testing.T) {
	q := `"NeurIPS"`
	table := map[string]int{
		key(q, 2020, 2020): 3000,
		// Every keyword candidate covers everything or nothing -> cannot split.
		key(q+` AND "learning"`, 2020, 2020): 3000,
		key(q+` AND "neural"`, 2020, 2020):   0,
	}
	leaves, err := Plan(q, 2020, 2020, 1000, 5, 8, []string{"learning", "neural"}, fakeCount(table), 0.1)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 1 {
		t.Fatalf("got %d leaves, want 1 (fallback), got %+v", len(leaves), leaves)
	}
	if !leaves[0].NeedsSplit {
		t.Error("expected the original leaf to be flagged needs_split on fallback")
	}
	if leaves[0].Query != q {
		t.Errorf("fallback leaf query = %q, want original %q", leaves[0].Query, q)
	}
	if leaves[0].Count != 3000 {
		t.Errorf("fallback leaf count = %d, want 3000", leaves[0].Count)
	}
	if WithinLimit(leaves) {
		t.Error("WithinLimit must be false while the fallback TODO leaf exists")
	}
}

// ---------------------------------------------------------------------------
// Simulated paper universe helpers
// ---------------------------------------------------------------------------

// genPapers produces n papers with a known keyword distribution.
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
// against a ground-truth paper set, returning how many papers match. This runs
// the splitter against a real count oracle instead of a hand-keyed table.
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
