// Package split turns a conference's broad query into a set of disjoint,
// crawlable tasks. It is pure logic: given a function that reports Scholar's
// "About N results" count for any (query, year range), it returns a plan of
// leaf tasks whose union covers the whole result space.
//
// Two-level partition:
//
//  1. Year windows. The range is walked in 2-year windows (2000-2001,
//     2002-2003, …; a trailing odd year is its own single-year window) instead
//     of being queried whole — Scholar's count for a 20+ year range is a rough
//     estimate, and the crawl should proceed in natural chronological pairs. A
//     window that fits under the cap becomes one leaf; a 2-year window over the
//     cap narrows to its two single years, each becoming a leaf when it fits.
//     This is an exact, exhaustive partition — every paper has exactly one
//     year, so nothing overlaps or falls between the cracks.
//
//  2. Sequential keyword subtraction (for a single year still over the cap).
//     With an ordered keyword list [A, B, C, …] we emit the chain
//
//     q AND "A"
//     q AND "B" -"A"
//     q AND "C" -"A" -"B"
//     …        (each paper lands in the first keyword bucket it matches)
//     q -"A" -"B" -"C" …   (residual: papers matching none of the words)
//
//     Buckets that count as 0 results are omitted (they contribute no papers),
//     and if one keyword already accounts for every result the chain stops
//     early — later buckets and the residual are then provably empty.
//
//     A keyword that is a substring of the base query is dropped before the
//     chain runs. Such a keyword appears in every paper's publication field
//     (e.g. "learning" for "International Conference on Machine Learning"), so
//     including it captures every result and excluding it removes every result —
//     it cannot split the space and would otherwise force the chain to collapse
//     into a single leaf.
//
//     Only the first maxKeywords keywords are used, so every query carries at
//     most that many keyword terms (no unbounded subtraction lists); papers
//     matching keywords beyond the cap land in the residual bucket.
//
//     Every leaf that still exceeds the cap is flagged NeedsSplit instead of
//     being subdivided further — it is recorded as a TODO for a later,
//     conference-specific re-split (the spec's "note it and tackle it later").
package split

import (
	"fmt"
	"strings"
)

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
// disjoint leaf tasks, walking the range in 2-year windows (a trailing odd year
// is its own window), narrowing an oversized 2-year window to its two single
// years, and applying the sequential keyword chain to any single year still
// over the cap.
//
// maxKeywords caps the number of keywords the chain may use: it bounds the
// length of every generated query to at most maxKeywords keyword terms (the
// include word plus its excludes), and papers matching keywords beyond the cap
// fall into the residual bucket.
func Plan(baseQuery string, yearFrom, yearTo, limit int, keywords []string, count CountFunc, maxKeywords int) ([]Leaf, error) {
	if baseQuery == "" {
		return nil, fmt.Errorf("split: empty base query")
	}
	if yearTo < yearFrom {
		return nil, fmt.Errorf("split: yearTo %d < yearFrom %d", yearTo, yearFrom)
	}
	return planRange(baseQuery, yearFrom, yearTo, limit, keywords, count, maxKeywords)
}

// planRange walks [ylo, yhi] in chronological 2-year windows so the plan reads
// 2000-2001, 2002-2003, … rather than starting from the whole-range count.
func planRange(baseQuery string, ylo, yhi, limit int, keywords []string, count CountFunc, maxKeywords int) ([]Leaf, error) {
	var leaves []Leaf
	for a := ylo; a <= yhi; a += 2 {
		b := a + 1
		if b > yhi {
			b = a // trailing odd year
		}
		ws, err := planWindow(baseQuery, a, b, limit, keywords, count, maxKeywords)
		if err != nil {
			return nil, err
		}
		leaves = append(leaves, ws...)
	}
	return leaves, nil
}

// planWindow plans one base window of one or two years. A window under the cap
// (or whose count could not be read) is a single leaf; a 2-year window over the
// cap narrows to its two single years, each planned recursively.
func planWindow(baseQuery string, ylo, yhi, limit int, keywords []string, count CountFunc, maxKeywords int) ([]Leaf, error) {
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

	// Over the cap: a 2-year window narrows to its two single years.
	if ylo < yhi {
		var leaves []Leaf
		for _, y := range []int{ylo, yhi} {
			l, err := planWindow(baseQuery, y, y, limit, keywords, count, maxKeywords)
			if err != nil {
				return nil, err
			}
			leaves = append(leaves, l...)
		}
		return leaves, nil
	}

	// A single year over the cap: sequential keyword chain. The base count is
	// passed along so the chain can detect the (common) case where one keyword
	// already covers every result and stop early.
	return chainSplit(baseQuery, ylo, yhi, limit, keywords, count, n, maxKeywords)
}

// chainSplit emits the ordered subtraction chain for one year. The residual
// bucket (papers matching none of the keywords) is appended so the union of all
// leaves is exhaustive.
//
// Only the first maxKeywords keywords are used (0 means none). Every generated
// query therefore carries at most maxKeywords keyword terms — the include word
// plus the preceding excludes — so no search box is ever stuffed with an
// unbounded subtraction list. Keywords beyond the cap are never searched; their
// papers fall into the residual.
//
// Two empty-bucket optimizations keep the chain from searching queries that are
// provably empty:
//
//   - A keyword bucket reported as 0 results is not emitted (no paper's first
//     match is that keyword, so it contributes nothing) and later buckets are
//     unaffected.
//   - If a bucket's count equals the base count, that keyword already accounts
//     for every result; the chain verifies with a single residual search that
//     nothing remains and stops. Every later bucket and the final residual are
//     subsets of that zero set, so skipping them loses no coverage.
func chainSplit(baseQuery string, ylo, yhi, limit int, keywords []string, count CountFunc, baseCount, maxKeywords int) ([]Leaf, error) {
	// A keyword that is part of the venue phrase cannot split it (see the
	// package doc); dropping it lets the remaining keywords actually partition
	// the result space.
	keywords = dropDegenerateKeywords(baseQuery, keywords)
	if maxKeywords >= 0 && len(keywords) > maxKeywords {
		keywords = keywords[:maxKeywords]
	}
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
		if ok && n == 0 {
			used = append(used, w)
			continue
		}
		leaves = append(leaves, Leaf{
			Query:      q,
			YearFrom:   ylo,
			YearTo:     yhi,
			Keywords:   append(append([]string{}, used...), w),
			Count:      n,
			HasCount:   ok,
			NeedsSplit: ok && n > limit,
		})
		used = append(used, w)

		// Keyword covers everything already -> nothing can be left after it.
		if ok && n == baseCount {
			remQ := baseQuery
			for _, p := range used {
				remQ += ` -"` + p + `"`
			}
			if remN, remOk := count(remQ, ylo, yhi); remOk && remN == 0 {
				return leaves, nil
			}
		}
	}

	// Residual: base query minus every keyword. Skipped when known to be empty
	// (either reported zero directly, or proven zero by the early stop above).
	q := baseQuery
	for _, w := range keywords {
		q += ` -"` + w + `"`
	}
	n, ok := count(q, ylo, yhi)
	if ok && n == 0 {
		return leaves, nil
	}
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

// DropDegenerateKeywords exports the degenerate-keyword filter (keywords that
// are substrings of the venue query cannot split its result space). Callers
// that mine candidates apply it before planning or re-splitting.
func DropDegenerateKeywords(baseQuery string, keywords []string) []string {
	return dropDegenerateKeywords(baseQuery, keywords)
}

// ResolveOverCap recursively splits a single leaf that exceeds the cap into a
// set of smaller, disjoint leaves, each under limit, using count() to probe how
// many results each keyword captures. This is the paper's balance-guided
// recursive split bound to an online count oracle, where every count() is an
// expensive Scholar search.
//
// keywords must be pre-ranked best-splitter-first (e.g. descending offline
// balance). At each node up to probesPerNode candidates are counted; the one
// whose with/without split is closest to even (balance >= minBalance) is
// chosen and recursion continues on both halves. ok is false when some subtree
// cannot be brought under the cap with the given keywords/budget — the caller
// should then keep treating the leaf as needs_split rather than emit an
// over-cap task. ResolveOverCap never emits an over-cap leaf itself.
func ResolveOverCap(leaf Leaf, keywords []string, count CountFunc, probesPerNode int, minBalance float64, limit int) ([]Leaf, bool) {
	var out []Leaf
	resolved := resolveNode(leaf.Query, leaf.YearFrom, leaf.YearTo, leaf.Count, leaf.HasCount, keywords, count, probesPerNode, minBalance, limit, nil, nil, &out, 0)
	if !resolved {
		return nil, false
	}
	return out, true
}

// resolveNode is the recursive splitter behind ResolveOverCap. inc/exc are the
// positive/negative keyword predicates accumulated along the path; the current
// node covers 'size' results. A partial split is never returned: if any subtree
// fails, resolved=false and out is left in an undefined state (caller discards).
func resolveNode(base string, yFrom, yTo, size int, hasCount bool, keywords []string, count CountFunc, probesPerNode int, minBalance float64, limit int, inc, exc []string, out *[]Leaf, depth int) bool {
	if size == 0 {
		return true
	}
	if size <= limit {
		*out = append(*out, Leaf{
			Query:    leafQuery(base, inc, exc),
			YearFrom: yFrom,
			YearTo:   yTo,
			Keywords: predicateChain(inc, exc),
			Count:    size,
			HasCount: hasCount,
		})
		return true
	}
	if depth > 32 || probesPerNode <= 0 {
		// Deep runaway or a disabled probe budget: cannot provably resolve.
		return false
	}

	// Probe up to probesPerNode candidates, keep the best balance that divides.
	best := -1.0
	var bestWith, bestWithout, bestIdx int
	bestKey := ""
	probed := 0
	for k := range keywords {
		if probed >= probesPerNode {
			break
		}
		q := leafQuery(base, appendPred(inc, keywords[k]), exc)
		n, okC := count(q, yFrom, yTo)
		probed++ // a count() round-trip is the budgeted resource
		if !okC {
			continue
		}
		with := n
		without := size - with
		if with <= 0 || without <= 0 {
			continue // this keyword does not divide the node
		}
		b := balanceOf(with, without)
		if b > best {
			best, bestWith, bestWithout = b, with, without
			bestIdx, bestKey = k, keywords[k]
		}
	}
	if bestKey == "" {
		return false // no keyword divides within budget
	}

	// Recurse on the with-side (papers containing the keyword) and the
	// without-side (papers lacking it). Both must resolve for the whole leaf to
	// resolve; children count() calls only happen for the with side since the
	// without side's size derives from the parent probe.
	remaining := dropKW(keywords, bestIdx)
	var withLeaves, withoutLeaves []Leaf
	withOK := resolveNode(base, yFrom, yTo, bestWith, true, remaining, count, probesPerNode, minBalance, limit, appendPred(inc, bestKey), exc, &withLeaves, depth+1)
	withoutOK := resolveNode(base, yFrom, yTo, bestWithout, true, remaining, count, probesPerNode, minBalance, limit, inc, appendPred(exc, bestKey), &withoutLeaves, depth+1)
	if !withOK || !withoutOK {
		return false
	}
	*out = append(*out, withLeaves...)
	*out = append(*out, withoutLeaves...)
	return true
}

// leafQuery renders base plus include (AND) and exclude (-) predicates.
func leafQuery(base string, inc, exc []string) string {
	q := base
	for _, w := range inc {
		q += ` AND "` + w + `"`
	}
	for _, w := range exc {
		q += ` -"` + w + `"`
	}
	return q
}

// predicateChain is the ordered provenance list (includes then excludes).
func predicateChain(inc, exc []string) []string {
	return append(append([]string{}, inc...), exc...)
}

// appendPred returns pred + [kw] on a fresh slice so sibling recursion branches
// never alias each other's predicate lists.
func appendPred(pred []string, kw string) []string {
	out := make([]string, len(pred), len(pred)+1)
	copy(out, pred)
	return append(out, kw)
}

// dropKW removes the keyword at index pos from keywords.
func dropKW(keywords []string, pos int) []string {
	out := make([]string, 0, len(keywords)-1)
	for i, k := range keywords {
		if i != pos {
			out = append(out, k)
		}
	}
	return out
}

// balanceOf is the paper's balance score: 1.0 for a perfect 50/50 split, ~0 for
// a lopsided one.
func balanceOf(with, without int) float64 {
	total := with + without
	if total == 0 {
		return 0
	}
	smaller := with
	if without < smaller {
		smaller = without
	}
	b := float64(smaller) / (float64(total) / 2.0)
	if b > 1 {
		return 1
	}
	return b
}

// dropDegenerateKeywords removes keywords that are substrings of the base
// query, case-insensitively. The base query is the quoted venue name, and
// Scholar matches include/exclude terms against the publication field as well
// as the content, so such a keyword matches (or rules out) every paper:
//
//	base  = "International Conference on Machine Learning"   (ICML, 582 results)
//	AND "learning"  -> 582  (venue name itself contains it -> covers everything)
//	-"learning"     -> 0    (every paper's venue field contains it)
//
// Either way the chain collapses: the include triggers the early stop and the
// exclude zeroes the residual, so no keyword after it is ever tried. The
// comparison is a plain substring so "system" is dropped for a venue named
// "Systems".
func dropDegenerateKeywords(baseQuery string, keywords []string) []string {
	if len(keywords) == 0 {
		return keywords
	}
	base := strings.ToLower(baseQuery)
	out := make([]string, 0, len(keywords))
	for _, kw := range keywords {
		if strings.Contains(base, strings.ToLower(kw)) {
			continue
		}
		out = append(out, kw)
	}
	return out
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
