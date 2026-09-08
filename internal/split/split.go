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
//  2. Recursive balance-guided keyword splitting (for a single year still over
//     the cap), via ResolveOverCap. At each over-cap node, up to probesPerNode
//     keyword candidates are counted in ranked order and the first whose
//     with/without split keeps neither side above 75% of the node (and clears
//     minBalance) is chosen — probing stops there rather than comparing every
//     candidate, so a working splitter costs one Scholar search per node.
//     If no probed candidate clears that bar, the best divider is used anyway
//     (a forced split): conjuncts can only shrink the count, so the oversized
//     side recurses and chains further keywords until it fits or the depth
//     bound is hit. The node then
//     becomes exactly two disjoint children:
//
//     parent AND "keyword"
//     parent AND NOT "keyword"
//
//     and recursion continues independently on each until every leaf is under
//     the cap. A keyword is never reused on a branch once it has split that
//     branch's ancestor. Degenerate keywords (substrings of the base query,
//     which cannot split anything) are dropped before recursion begins, and at
//     most maxKeywords candidates are considered.
//
//     If a node cannot be proven to split (no candidate keyword divides it
//     within budget, or every candidate falls below minBalance, or the keyword
//     or probe budget is exhausted), the whole subtree fails atomically: no
//     partial set of resolved leaves is returned, and the original node is kept
//     as a single NeedsSplit leaf — a TODO for a later, conference-specific
//     re-split (the spec's "note it and tackle it later").
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
// years, and recursively splitting any single year still over the cap via
// ResolveOverCap.
//
// maxKeywords caps the number of candidate keywords considered for recursive
// splitting (see ResolveOverCap for the >=0 / ==0 / <0 convention). maxProbes
// is the per-node probe budget and minBalance the minimum acceptable
// with/without balance; both are forwarded to ResolveOverCap unchanged.
func Plan(baseQuery string, yearFrom, yearTo, limit, maxKeywords, maxProbes int, keywords []string, count CountFunc, minBalance float64) ([]Leaf, error) {
	if baseQuery == "" {
		return nil, fmt.Errorf("split: empty base query")
	}
	if yearTo < yearFrom {
		return nil, fmt.Errorf("split: yearTo %d < yearFrom %d", yearTo, yearFrom)
	}
	if minBalance < 0.0 || minBalance > 1.0 {
		return nil, fmt.Errorf("split: minBalance %v out of range [0.0, 1.0]", minBalance)
	}
	return planRange(baseQuery, yearFrom, yearTo, limit, keywords, count, maxKeywords, maxProbes, minBalance)
}

// planRange walks [ylo, yhi] in chronological 2-year windows so the plan reads
// 2000-2001, 2002-2003, … rather than starting from the whole-range count.
func planRange(baseQuery string, ylo, yhi, limit int, keywords []string, count CountFunc, maxKeywords, maxProbes int, minBalance float64) ([]Leaf, error) {
	var leaves []Leaf
	for a := ylo; a <= yhi; a += 2 {
		b := a + 1
		if b > yhi {
			b = a // trailing odd year
		}
		ws, err := planWindow(baseQuery, a, b, limit, keywords, count, maxKeywords, maxProbes, minBalance)
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
func planWindow(baseQuery string, ylo, yhi, limit int, keywords []string, count CountFunc, maxKeywords, maxProbes int, minBalance float64) ([]Leaf, error) {
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
			l, err := planWindow(baseQuery, y, y, limit, keywords, count, maxKeywords, maxProbes, minBalance)
			if err != nil {
				return nil, err
			}
			leaves = append(leaves, l...)
		}
		return leaves, nil
	}

	// A single year over the cap: recursively split it via ResolveOverCap.
	// Degenerate keywords (substrings of the base query) can never divide the
	// space, and are dropped before the maxKeywords cap is applied so they
	// don't waste one of the limited candidate slots.
	leaf := Leaf{
		Query:      baseQuery,
		YearFrom:   ylo,
		YearTo:     yhi,
		Count:      n,
		HasCount:   true,
		NeedsSplit: true,
	}

	cand := dropDegenerateKeywords(baseQuery, keywords)
	cand = capKeywords(cand, maxKeywords)

	resolved, ok := ResolveOverCap(leaf, cand, count, maxProbes, minBalance, limit)
	if !ok {
		return []Leaf{leaf}, nil
	}

	return resolved, nil
}

// capKeywords bounds keywords to at most maxKeywords candidates, preserving
// the caller's ranking (best-splitter-first). maxKeywords < 0 leaves the list
// unbounded (existing convention: negative means "no cap"); maxKeywords == 0
// yields no candidates, disabling keyword splitting entirely.
func capKeywords(keywords []string, maxKeywords int) []string {
	if maxKeywords < 0 || len(keywords) <= maxKeywords {
		return keywords
	}
	return keywords[:maxKeywords]
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
// Balance × IDF). At each node up to probesPerNode candidates are counted; the
// first one whose with/without split clears minBalance is chosen and the
// remaining candidates are never probed. ok is false when some subtree
// cannot be brought under the cap with the given keywords/budget — the caller
// should then keep treating the leaf as needs_split rather than emit an
// over-cap task. ResolveOverCap never emits an over-cap leaf itself.
func ResolveOverCap(leaf Leaf, keywords []string, count CountFunc, probesPerNode int, minBalance float64, limit int) ([]Leaf, bool) {
	var out []Leaf

	resolved := resolveNode(
		leaf.Query,
		leaf.YearFrom,
		leaf.YearTo,
		leaf.Count,
		leaf.HasCount,
		keywords,
		count,
		probesPerNode,
		minBalance,
		limit,
		nil,
		nil,
		&out,
		0)
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
		// depth > 32 is a defensive recursion bound only, not a correctness
		// mechanism — a correct keyword set never approaches it. A non-positive
		// probesPerNode simply cannot resolve an over-cap node.
		return false
	}

	// Probe candidates in ranked order; the FIRST one that produces a clean
	// split is selected and the remaining candidates are never searched. A
	// split is clean when NEITHER side keeps more than maxSideFraction of the
	// node — a keyword covering >75% of the results (or leaving >75% behind)
	// barely divides the corpus, so the next candidate is probed instead.
	//
	// When no probed candidate produces a clean split the node does NOT fail:
	// the best-dividing candidate seen is used anyway — a forced split. Each
	// conjunct can only shrink the result count, so the oversized side recurses
	// and chains a second keyword ("... AND k1 AND k2"), a third, and so on
	// until it fits or the depth bound is reached. The node only fails when no
	// probed candidate divides it at all (one side empty) or every valid
	// candidate is below minBalance.
	const maxSideFraction = 0.75
	maxSide := int(float64(size) * maxSideFraction)

	bestIdx := -1
	bestKey := ""
	bestBal := -1.0
	var bestWith, bestWithout int
	probed := 0

	for k := range keywords {
		if probed >= probesPerNode {
			break
		}
		q := leafQuery(base, appendPred(inc, keywords[k]), exc)
		n, okC := count(q, yFrom, yTo)
		probed++
		if !okC {
			continue
		}
		with := n
		if with >= size {
			continue
		}
		without := size - with
		if with <= 0 || without <= 0 {
			continue // this keyword does not divide the node
		}
		b := balanceOf(with, without)
		if b < minBalance {
			continue
		}
		if with <= maxSide && without <= maxSide {
			bestIdx, bestKey, bestWith, bestWithout = k, keywords[k], with, without
			break // clean split found — stop probing
		}
		if b > bestBal {
			// Valid but lopsided: remember it as the forced-split fallback.
			bestBal, bestIdx, bestKey, bestWith, bestWithout = b, k, keywords[k], with, without
		}
	}
	if bestKey == "" {
		return false // nothing probed divides the node at all
	}

	// Recurse on the with-side (papers containing the keyword) and the
	// without-side (papers lacking it). Both must resolve for the whole leaf to
	// resolve; children count() calls only happen for the with side since the
	// without side's size derives from the parent probe.
	remaining := dropKW(keywords, bestIdx)
	var withLeaves, withoutLeaves []Leaf
	withOK := resolveNode(
		base, yFrom, yTo, bestWith, true,
		remaining, count, probesPerNode,
		minBalance, limit,
		appendPred(inc, bestKey), exc,
		&withLeaves, depth+1)
	withoutOK := resolveNode(
		base, yFrom, yTo, bestWithout, true,
		remaining, count, probesPerNode,
		minBalance, limit,
		inc, appendPred(exc, bestKey),
		&withoutLeaves, depth+1)
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
// Either way the keyword cannot divide the node: the with-count equals the
// parent's size (rejected as with >= size) and the without-count is zero
// (rejected as without <= 0), so resolveNode would never select it anyway —
// dropping it here just saves the wasted probe and a maxKeywords slot. The
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
