package partition

import (
	"fmt"
	"strings"
	"testing"
)

// docs builds the two-keyword corpus:
//
//	d0,d1,d2   "alpha beta"
//	d3,d4,d5   "alpha"
//	d6,d7,d8   "beta"
//	d9,d10,d11 "common only"
//
// With limit 5 and keywords ranked [alpha, beta], alpha splits 12->6/6, then
// beta splits each 6 into 3/3, yielding four buckets of 3.
func docs() []Doc {
	var out []Doc
	n := 0
	assign := func(text string) {
		out = append(out, Doc{Hash: "h" + string(rune('a'+n)), Year: 2020})
		membership[out[len(out)-1].Hash] = text
		n++
	}
	for range 3 {
		assign("alpha beta")
	}
	for range 3 {
		assign("alpha")
	}
	for range 3 {
		assign("beta")
	}
	for range 3 {
		assign("common only")
	}
	return out
}

var membership = map[string]string{}

func text(d Doc) string { return membership[d.Hash] }

// TestPartitionTwoKeywordSplit: alpha then beta split the corpus into four
// under-cap buckets with the correct include/exclude predicate paths.
func TestPartitionTwoKeywordSplit(t *testing.T) {
	ds := docs()
	out := Partition(ds, []string{"alpha", "beta"}, text, 5)

	if len(out.Buckets) != 4 {
		t.Fatalf("got %d buckets, want 4: %+v", len(out.Buckets), out.Buckets)
	}
	if len(out.Unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %+v", out.Unresolved)
	}

	want := []struct {
		size    int
		include []string
		exclude []string
	}{
		{3, []string{"alpha", "beta"}, nil},
		{3, []string{"alpha"}, []string{"beta"}},
		{3, []string{"beta"}, []string{"alpha"}},
		{3, nil, []string{"alpha", "beta"}},
	}
	for i, w := range want {
		b := out.Buckets[i]
		if len(b.Papers) != w.size {
			t.Errorf("bucket %d has %d papers, want %d", i, len(b.Papers), w.size)
		}
		if !eqStrings(b.Includes, w.include) {
			t.Errorf("bucket %d includes = %v, want %v", i, b.Includes, w.include)
		}
		if !eqStrings(b.Excludes, w.exclude) {
			t.Errorf("bucket %d excludes = %v, want %v", i, b.Excludes, w.exclude)
		}
	}
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPartitionEveryDocExactlyOnce: union of all buckets + unresolved covers the
// corpus exactly once (no drops, no duplicates).
func TestPartitionEveryDocExactlyOnce(t *testing.T) {
	ds := docs()
	out := Partition(ds, []string{"alpha", "beta"}, text, 5)

	seen := map[string]bool{}
	for _, b := range out.Buckets {
		for _, h := range b.Papers {
			if seen[h] {
				t.Errorf("duplicate paper %q", h)
			}
			seen[h] = true
		}
	}
	for _, b := range out.Unresolved {
		for _, h := range b.Papers {
			if seen[h] {
				t.Errorf("duplicate paper %q", h)
			}
			seen[h] = true
		}
	}
	if len(seen) != len(ds) {
		t.Fatalf("covered %d docs, want %d", len(seen), len(ds))
	}
}

// TestPartitionCapacity: every emitted bucket is at or under the limit.
func TestPartitionCapacity(t *testing.T) {
	ds := docs()
	out := Partition(ds, []string{"alpha", "beta"}, text, 5)
	for i, b := range out.Buckets {
		if len(b.Papers) > 5 {
			t.Errorf("bucket %d has %d papers, over limit", i, len(b.Papers))
		}
	}
}

// TestPartitionPicksConditionallyBalancedChild: inside a node, a higher-ranked
// keyword that divides the subset lopsidedly (9/1) must lose to a lower-ranked
// one that splits it evenly (5/5) — the child competes on the parent's subset,
// not on its global rank.
func TestPartitionPicksConditionallyBalancedChild(t *testing.T) {
	membership = map[string]string{}
	var ds []Doc
	n := 0
	assign := func(s string) {
		h := "h" + string(rune('a'+n)) + string(rune('0'+n))
		ds = append(ds, Doc{Hash: h, Year: 2020})
		membership[h] = s
		n++
	}
	// 10 "alpha" docs: "loppy" is lopsided inside the subset (9/1) while "even"
	// splits it exactly in half (5/5).
	for range 5 {
		assign("alpha loppy even")
	}
	for range 4 {
		assign("alpha loppy")
	}
	assign("alpha")
	// 10 docs with none of the keywords: an unresolved atom floor.
	for range 10 {
		assign("plain")
	}
	out := Partition(ds, []string{"alpha", "loppy", "even"}, text, 5)

	for _, b := range out.Buckets {
		for _, kw := range b.Includes {
			if kw == "loppy" {
				t.Errorf("lopsided 'loppy' (9/1) must not be chosen over even 'even' (5/5): %+v", b)
			}
		}
		for _, kw := range b.Excludes {
			if kw == "loppy" {
				t.Errorf("lopsided 'loppy' must not appear as an exclude either: %+v", b)
			}
		}
	}
	// Expect: root splits on alpha (10/10), the alpha side splits on "even"
	// (5/5) into two buckets of 5, and the 10 plain docs are unresolved.
	var alphaEven, alphaNotEven int
	for _, b := range out.Buckets {
		if eqStrings(b.Includes, []string{"alpha", "even"}) {
			alphaEven = len(b.Papers)
		}
		if eqStrings(b.Includes, []string{"alpha"}) && eqStrings(b.Excludes, []string{"even"}) {
			alphaNotEven = len(b.Papers)
		}
	}
	if alphaEven != 5 || alphaNotEven != 5 {
		t.Errorf("alpha branch = (%d, %d), want (5,5) via 'even': %+v", alphaEven, alphaNotEven, out.Buckets)
	}
	if len(out.Unresolved) != 1 || len(out.Unresolved[0].Papers) != 10 {
		t.Errorf("expected one unresolved group of 10, got %+v", out.Unresolved)
	}
}

// TestPartitionInfeasible: 10 keyword-identical documents under a cap of 5 — no
// keyword divides them, so the group is Unresolved (the atom floor), never an
// over-cap bucket and never an infinite loop.
func TestPartitionInfeasible(t *testing.T) {
	var ds []Doc
	membership = map[string]string{}
	for i := range 10 {
		d := Doc{Hash: "same" + string(rune('0'+i)), Year: 2020}
		ds = append(ds, d)
		membership[d.Hash] = "convolutional network" // every doc identical
	}

	out := Partition(ds, []string{"convolutional", "network", "model"}, text, 5)
	if len(out.Buckets) != 0 {
		t.Fatalf("infeasible corpus must have zero buckets, got %+v", out.Buckets)
	}
	if len(out.Unresolved) != 1 {
		t.Fatalf("want exactly 1 unresolved group, got %d: %+v", len(out.Unresolved), out.Unresolved)
	}
	if len(out.Unresolved[0].Papers) != 10 {
		t.Errorf("unresolved group has %d papers, want 10", len(out.Unresolved[0].Papers))
	}
}

// TestPartitionDeterministic: identical input produces identical buckets.
func TestPartitionDeterministic(t *testing.T) {
	ds := docs()
	o1 := Partition(ds, []string{"alpha", "beta"}, text, 5)
	o2 := Partition(ds, []string{"alpha", "beta"}, text, 5)
	if len(o1.Buckets) != len(o2.Buckets) {
		t.Fatalf("bucket count differs across runs")
	}
	for i := range o1.Buckets {
		if !eqStrings(o1.Buckets[i].Papers, o2.Buckets[i].Papers) {
			t.Errorf("bucket %d papers differ across runs", i)
		}
		if !eqStrings(o1.Buckets[i].Includes, o2.Buckets[i].Includes) {
			t.Errorf("bucket %d includes differ across runs", i)
		}
		if !eqStrings(o1.Buckets[i].Excludes, o2.Buckets[i].Excludes) {
			t.Errorf("bucket %d excludes differ across runs", i)
		}
	}
}

// TestBucketQuery: the Query method renders the include/exclude predicates into
// a Scholar query over the base venue phrase.
func TestBucketQuery(t *testing.T) {
	base := `"International Conference on Machine Learning"`
	b := Bucket{Includes: []string{"neural", "reinforcement"}, Excludes: []string{"vision"}}
	got := b.Query(base)
	for _, wantS := range []string{`"neural"`, `"reinforcement"`, `-"vision"`, ` AND `} {
		if !strings.Contains(got, wantS) {
			t.Errorf("query %q missing %q", got, wantS)
		}
	}
	var b2 Bucket
	if q := b2.Query(base); q != base {
		t.Errorf("empty bucket query = %q, want %q", q, base)
	}
}

// TestPartitionNoDocs: an empty corpus is a no-op.
func TestPartitionNoDocs(t *testing.T) {
	out := Partition(nil, []string{"alpha"}, func(d Doc) string { return "" }, 5)
	if len(out.Buckets) != 0 || len(out.Unresolved) != 0 {
		t.Fatalf("empty corpus should produce nothing, got %+v", out)
	}
}

// TestPartitionMinBalanceRejectsNearDegenerateChain reproduces the real bug:
// "neural networks" splits the corpus cleanly (50/50), but "network" is a
// near-superset of "neural networks" — almost every paper in the
// "neural networks" branch also contains "network" alone, so chaining it
// there barely divides anything further (90/10 inside that branch). Plain
// Partition has no quality floor and picks it anyway because SOME candidate
// must win; PartitionMinBalance must instead treat that branch as
// unresolved rather than emit a near-useless "AND neural networks AND
// network" leaf.
func TestPartitionMinBalanceRejectsNearDegenerateChain(t *testing.T) {
	var docs []Doc
	mem := map[string]string{}
	add := func(hash, text string) {
		docs = append(docs, Doc{Hash: hash, Year: 2020})
		mem[hash] = text
	}
	txt := func(d Doc) string { return mem[d.Hash] }

	// 20 papers total. "neural networks" covers exactly the first 10 (clean
	// 10/10 split). Within that branch, "network" covers 9 of the 10 (a
	// near-degenerate 9/1 split — conditional balance ~0.2) because it is a
	// near-superset of the phrase; the other 10 papers never mention
	// "network" at all so the whole-corpus stats would look fine, but the
	// CONDITIONAL split inside the branch is what matters.
	for i := 0; i < 9; i++ {
		add(fmt.Sprintf("nn-both-%d", i), "neural networks and network topology")
	}
	add("nn-solo", "neural networks only, no other term")
	for i := 0; i < 10; i++ {
		add(fmt.Sprintf("other-%d", i), "gradient descent optimization")
	}

	keywords := []string{"neural networks", "network"}
	// limit=8 forces both the root (20 docs) and the "neural networks" branch
	// (10 docs) to look for a divider.
	out := PartitionMinBalance(docs, keywords, txt, 8, 0.3)

	for _, b := range out.Buckets {
		hasPhrase := false
		hasWord := false
		for _, inc := range b.Includes {
			if inc == "neural networks" {
				hasPhrase = true
			}
			if inc == "network" {
				hasWord = true
			}
		}
		if hasPhrase && hasWord {
			t.Errorf("bucket chained 'neural networks' AND 'network' despite failing the minBalance floor: %+v", b)
		}
	}
	// The "neural networks" branch (10 docs, over the limit=8 cap) must
	// surface as Unresolved since "network" is its only candidate and it
	// fails the floor — not silently accepted as a near-useless split.
	foundUnresolvedPhraseBranch := false
	for _, u := range out.Unresolved {
		for _, inc := range u.Includes {
			if inc == "neural networks" {
				foundUnresolvedPhraseBranch = true
			}
		}
	}
	if !foundUnresolvedPhraseBranch {
		t.Errorf("expected the 'neural networks' branch to be Unresolved (no candidate clears minBalance), got buckets=%+v unresolved=%+v", out.Buckets, out.Unresolved)
	}
}

// TestPartitionOptsPhraseBoostBreaksTies: at a node where a phrase and a
// word split the group EQUALLY well (identical conditional Balance),
// PhraseBoost must make the phrase win instead of the word — this is the
// lever for getting more multi-phrase chains: without a boost, a tie always
// goes to whichever candidate is earlier in the ranked list, and since a
// corpus typically has far more candidate WORDS than PHRASES, a word wins
// most ties by sheer numeric advantage even when a phrase would split just
// as well. Boosting phrase scores at EVERY node (not just the top-level
// mining rank) lets a phrase win the first split, and then win again at the
// next branch down, producing the "two phrases chained" result.
func TestPartitionOptsPhraseBoostBreaksTies(t *testing.T) {
	var docs []Doc
	mem := map[string]string{}
	add := func(hash, text string) {
		docs = append(docs, Doc{Hash: hash, Year: 2020})
		mem[hash] = text
	}
	txt := func(d Doc) string { return mem[d.Hash] }

	// "alpha" (word) and "alpha beta" (phrase) split the 12-doc corpus
	// IDENTICALLY: both cover exactly the same 6 docs (every "alpha" doc here
	// also says "alpha beta"), so their conditional Balance is tied at the
	// root.
	for i := 0; i < 6; i++ {
		add(fmt.Sprintf("a-%d", i), "alpha beta")
	}
	for i := 0; i < 6; i++ {
		add(fmt.Sprintf("b-%d", i), "gamma delta")
	}

	// Without a boost, "alpha" (ranked first) wins the tie. limit=6 so the
	// root's 6/6 split alone resolves both sides.
	plain := PartitionOpts(docs, []string{"alpha", "alpha beta"}, txt, 6, Options{})
	wantWord := false
	for _, b := range plain.Buckets {
		for _, inc := range b.Includes {
			if inc == "alpha" {
				wantWord = true
			}
		}
	}
	if !wantWord {
		t.Fatalf("test setup: expected 'alpha' to win the tie without a boost, got %+v", plain.Buckets)
	}

	// With PhraseBoost > 1.0, the phrase must win the same tie instead.
	boosted := PartitionOpts(docs, []string{"alpha", "alpha beta"}, txt, 6, Options{PhraseBoost: 1.5})
	gotPhrase := false
	for _, b := range boosted.Buckets {
		for _, inc := range b.Includes {
			if inc == "alpha beta" {
				gotPhrase = true
			}
			if inc == "alpha" {
				t.Errorf("PhraseBoost should have made 'alpha beta' win the tie, but 'alpha' was used: %+v", b)
			}
		}
	}
	if !gotPhrase {
		t.Errorf("expected 'alpha beta' to win the boosted tie, got %+v", boosted.Buckets)
	}
}
