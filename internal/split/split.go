// Package split turns a conference's broad query into a set of disjoint,
// crawlable tasks. It is pure logic: given a function that reports Scholar's
// "About N results" count for any (query, year range), it returns a plan of
// leaf tasks whose union covers the whole result space.
//
// Two-level partition:
//
//  1. Year ranges. If the full 2000→present range exceeds the cap, the range
//     is recursively halved (loading each half's count) until every leaf is
//     within the cap or shrinks to a single year. This is an exact, exhaustive
//     partition — every paper has exactly one year, so nothing overlaps or
//     falls between the cracks.
//
//  2. Sequential keyword subtraction (for a single year still over the cap).
//     With an ordered keyword list [A, B, C, …] we emit the chain
//
//         q AND "A"
//         q AND "B" -"A"
//         q AND "C" -"A" -"B"
//         …        (each paper lands in the first keyword bucket it matches)
//         q -"A" -"B" -"C" …   (residual: papers matching none of the words)
//
//     Every leaf that still exceeds the cap is flagged NeedsSplit instead of
//     being subdivided further — it is recorded as a TODO for a later,
//     conference-specific re-split (the spec's "note it and tackle it later").
package split

import "fmt"

// Leaf is one crawlable task produced by the planner.
type Leaf struct {
	Query      string   // effective search-box text
	YearFrom   int      // sidebar "From" bound (inclusive)
	YearTo     int      // sidebar "To" bound (inclusive)
	Keywords   []string // ordered chain that produced this leaf (provenance)
	Count      int      // "About N results"
	HasCount   bool     // whether Scholar reported a count
	NeedsSplit bool     // count exceeds the cap -> TODO for a later re-split
}

// CountFunc reports the estimated result count for a search-box query within
// a year range (both set via the Scholar UI). ok is false when the count
// cannot be read.
type CountFunc func(query string, yearFrom, yearTo int) (count int, ok bool)

// Plan partitions the [yearFrom, yearTo] result space of baseQuery into
// disjoint leaf tasks, recursively halving oversized year ranges and then
// applying the sequential keyword chain to any single year still over the cap.
func Plan(baseQuery string, yearFrom, yearTo, limit int, keywords []string, count CountFunc) ([]Leaf, error) {
	if baseQuery == "" {
		return nil, fmt.Errorf("split: empty base query")
	}
	if yearTo < yearFrom {
		return nil, fmt.Errorf("split: yearTo %d < yearFrom %d", yearTo, yearFrom)
	}
	return planRange(baseQuery, yearFrom, yearTo, limit, keywords, count)
}

func planRange(baseQuery string, ylo, yhi, limit int, keywords []string, count CountFunc) ([]Leaf, error) {
	n, ok := count(baseQuery, ylo, yhi)
	if !ok || n <= limit {
		return []Leaf{{
			Query:      baseQuery,
			YearFrom:   ylo,
			YearTo:     yhi,
			Count:      n,
			HasCount:   ok,
			NeedsSplit: ok && n > limit,
		}}, nil
	}

	// Over the cap. Can we still narrow the year range?
	if ylo < yhi {
		mid := (ylo + yhi) / 2
		left, err := planRange(baseQuery, ylo, mid, limit, keywords, count)
		if err != nil {
			return nil, err
		}
		right, err := planRange(baseQuery, mid+1, yhi, limit, keywords, count)
		if err != nil {
			return nil, err
		}
		return append(left, right...), nil
	}

	// A single year over the cap: sequential keyword chain.
	return chainSplit(baseQuery, ylo, yhi, limit, keywords, count)
}

// chainSplit emits the ordered subtraction chain for one year. The residual
// bucket (papers matching none of the keywords) is always appended so the
// union of all leaves is exhaustive.
func chainSplit(baseQuery string, ylo, yhi, limit int, keywords []string, count CountFunc) ([]Leaf, error) {
	if len(keywords) == 0 {
		n, ok := count(baseQuery, ylo, yhi)
		return []Leaf{{
			Query:      baseQuery,
			YearFrom:   ylo,
			YearTo:     yhi,
			Count:      n,
			HasCount:   ok,
			NeedsSplit: ok && n > limit,
		}}, nil
	}

	var leaves []Leaf
	used := []string{}
	for _, w := range keywords {
		q := baseQuery + ` AND "` + w + `"`
		for _, p := range used {
			q += ` -"` + p + `"`
		}
		n, ok := count(q, ylo, yhi)
		leaf := Leaf{
			Query:      q,
			YearFrom:   ylo,
			YearTo:     yhi,
			Keywords:   append(append([]string{}, used...), w),
			Count:      n,
			HasCount:   ok,
			NeedsSplit: ok && n > limit,
		}
		leaves = append(leaves, leaf)
		used = append(used, w)
	}

	// Residual: base query minus every keyword.
	q := baseQuery
	for _, w := range keywords {
		q += ` -"` + w + `"`
	}
	n, ok := count(q, ylo, yhi)
	leaves = append(leaves, Leaf{
		Query:      q,
		YearFrom:   ylo,
		YearTo:     yhi,
		Keywords:   append([]string{}, keywords...),
		Count:      n,
		HasCount:   ok,
		NeedsSplit: ok && n > limit,
	})
	return leaves, nil
}

// WithinLimit reports whether every leaf of the plan is safe to crawl (no
// needs_split TODO outstanding).
func WithinLimit(leaves []Leaf) bool {
	for _, l := range leaves {
		if l.NeedsSplit {
			return false
		}
	}
	return true
}
