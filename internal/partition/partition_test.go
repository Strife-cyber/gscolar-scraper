package partition

import (
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
	for i := 0; i < 3; i++ {
		assign("alpha beta")
	}
	for i := 0; i < 3; i++ {
		assign("alpha")
	}
	for i := 0; i < 3; i++ {
		assign("beta")
	}
	for i := 0; i < 3; i++ {
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

// TestPartitionInfeasible: 10 keyword-identical documents under a cap of 5 — no
// keyword divides them, so the group is Unresolved (the atom floor), never an
// over-cap bucket and never an infinite loop.
func TestPartitionInfeasible(t *testing.T) {
	var ds []Doc
	membership = map[string]string{}
	for i := 0; i < 10; i++ {
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
