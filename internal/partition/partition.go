// Package partition implements the paper's capacity-constrained recursive
// splitting (its "Stage 2") over an already-scraped corpus of known documents.
// Given a set of documents, an ordered list of candidate keywords (best
// splitter first) and a function that returns each document's searchable text,
// it recursively splits any oversized group on the keyword that most evenly
// divides *that group*, producing buckets that fit under a capacity limit.
// Each bucket carries the include/exclude keyword predicates that define it,
// so it can be rendered into a Scholar query and crawled.
//
// The split is deterministic: documents are ordered by hash, every dividing
// candidate is scored by its balance inside the current group (the child
// competes on the parent's subset, so a keyword correlated with an ancestor
// predicate is near-uniform there and loses), and ties break to the earlier
// keyword in ranked order. A group that is too large AND that no remaining
// keyword can divide lands in Unresolved — the operational form of the paper's
// Feasibility Theorem: documents that share every keyword are inseparable, and
// surfacing them beats silently emitting an over-cap bucket or looping
// forever. Callers decide how to handle Unresolved (e.g. flag needs_split).
package partition

import (
	"math/bits"
	"sort"
	"strings"
)

// Doc is one known document in the corpus.
type Doc struct {
	Hash string
	Year int
}

// Bucket is a group of documents that fit within the capacity limit, described
// by the keyword predicates that selected them.
type Bucket struct {
	Papers   []string // document hashes, sorted ascending
	Includes []string // keywords every paper contains (in split order)
	Excludes []string // keywords no paper in the bucket contains
}

// Query renders this bucket as a Scholar search query: base augmented with the
// include predicates AND-ed in and the exclude predicates subtracted.
func (b Bucket) Query(base string) string {
	var sb strings.Builder
	sb.WriteString(base)
	for _, w := range b.Includes {
		sb.WriteString(` AND "`)
		sb.WriteString(w)
		sb.WriteByte('"')
	}
	for _, w := range b.Excludes {
		sb.WriteString(` -"`)
		sb.WriteString(w)
		sb.WriteByte('"')
	}
	return sb.String()
}

// Outcome is the result of partitioning.
type Outcome struct {
	Buckets    []Bucket // every bucket obeys count(Papers) <= limit
	Unresolved []Bucket // too large and no keyword could divide them
}

// textOf returns the searchable text of a document (its title and/or snippet).
type textOf func(d Doc) string

// Partition splits docs into buckets of at most limit documents. keywords must
// be pre-ranked best-splitter-first (e.g. descending mine.Candidate Balance×
// IDF); within each group the candidate with the best conditional balance is
// used — the keyword that most evenly splits *that subset* — with ties broken
// by the ranked order, so the result is deterministic.
//
// Membership is a case-insensitive substring test against the document text,
// matching Scholar's semantics.
func Partition(docs []Doc, keywords []string, text textOf, limit int) Outcome {
	// Deterministic document order.
	sorted := append([]Doc(nil), docs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Hash < sorted[j].Hash })

	kwBits := make([]*bitset, len(keywords))
	for k := range keywords {
		bs := newBitset(len(sorted))
		for i, d := range sorted {
			if containsFold(text(d), keywords[k]) {
				bs.set(i)
			}
		}
		kwBits[k] = bs
	}

	var out Outcome
	split(&out, newBitsetFull(len(sorted)), nil, nil, sorted, keywords, kwBits, limit)
	return out
}

func split(out *Outcome, set *bitset, inc, exc []string, sorted []Doc, keywords []string, kwBits []*bitset, limit int) {
	size := set.count()
	if size == 0 {
		return
	}
	if size <= limit {
		out.Buckets = append(out.Buckets, Bucket{
			Papers:   hashesOf(sorted, set),
			Includes: inc,
			Excludes: exc,
		})
		return
	}

	// Score every candidate on how evenly it divides THIS group — the child
	// competes on the parent's subset, so a keyword correlated with an ancestor
	// predicate is near-uniform inside it and loses to a conditionally
	// independent one. Ties break to the earlier keyword in ranked order.
	best := -1.0
	bestK := -1
	var bestWith, bestWithout *bitset
	for k := range keywords {
		with := set.and(kwBits[k])
		without := set.andNot(kwBits[k])
		wc, woc := with.count(), without.count()
		if wc == 0 || woc == 0 {
			continue // this keyword does not divide the group
		}
		if b := subsetBalance(wc, woc); b > best {
			best, bestK, bestWith, bestWithout = b, k, with, without
		}
	}
	if bestK < 0 {
		// No remaining keyword divides this group: an atom floor.
		out.Unresolved = append(out.Unresolved, Bucket{
			Papers:   hashesOf(sorted, set),
			Includes: inc,
			Excludes: exc,
		})
		return
	}

	split(out, bestWith, appendPred(inc, keywords[bestK]), exc, sorted, keywords, kwBits, limit)
	split(out, bestWithout, inc, appendPred(exc, keywords[bestK]), sorted, keywords, kwBits, limit)
}

// subsetBalance is the paper's Balance restricted to the current group: 1.0
// for an even split of this node, ~0 for a lopsided one.
func subsetBalance(with, without int) float64 {
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

// appendPred returns pred + [kw] on a fresh slice, so sibling recursion
// branches never alias and overwrite each other's predicate lists.
func appendPred(pred []string, kw string) []string {
	out := make([]string, len(pred), len(pred)+1)
	copy(out, pred)
	return append(out, kw)
}

func hashesOf(sorted []Doc, set *bitset) []string {
	out := make([]string, 0, set.count())
	for i := 0; i < len(sorted); i++ {
		if set.get(i) {
			out = append(out, sorted[i].Hash)
		}
	}
	return out
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// bitset is a fixed-size bit vector over the corpus (one bit per document).
type bitset struct {
	words []uint64
	n     int
}

func newBitset(n int) *bitset      { return &bitset{words: make([]uint64, (n+63)/64), n: n} }
func newBitsetFull(n int) *bitset {
	bs := newBitset(n)
	for i := 0; i < n; i++ {
		bs.set(i)
	}
	return bs
}

func (b *bitset) set(i int)       { b.words[i>>6] |= 1 << uint(i&63) }
func (b *bitset) get(i int) bool  { return b.words[i>>6]&(1<<uint(i&63)) != 0 }
func (b *bitset) count() int {
	c := 0
	for _, w := range b.words {
		c += bits.OnesCount64(w)
	}
	return c
}

func (b *bitset) and(o *bitset) *bitset {
	out := newBitset(b.n)
	for i := range b.words {
		out.words[i] = b.words[i] & o.words[i]
	}
	return out
}

func (b *bitset) andNot(o *bitset) *bitset {
	out := newBitset(b.n)
	for i := range b.words {
		out.words[i] = b.words[i] &^ o.words[i]
	}
	return out
}
