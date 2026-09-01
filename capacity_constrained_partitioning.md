# Capacity-Constrained Keyword Partitioning

### A practical algorithm for splitting a large document corpus into ≤ N-page buckets using only keyword membership, with a Go reference implementation

---

## Abstract

This paper describes, end-to-end, an algorithm for taking a corpus of documents that already lives in a database and splitting it into groups ("buckets") such that no bucket exceeds a fixed capacity (for example, 500 pages), using **only keyword membership** as the partitioning mechanism. It explains why the two obvious approaches — brute-force enumeration of every keyword combination (`2^K`), and a naive linear/greedy partition — both fail in practice, and derives the actual approach that should be built: a two-stage system consisting of **atomic signature grouping** followed by **recursive, balance-guided capacity splitting**, implemented efficiently with bitsets. Every step is explained in plain language first, then formalized, then implemented in Go. The paper closes with a full complexity analysis (best, average, and worst case), a discussion of the one hard failure mode this approach cannot avoid (keyword-inseparable documents), and extensions using scoring functions and embeddings.

---

## Table of Contents

1. Problem Statement
2. Assumptions About the Existing Database
3. Why the Obvious Approaches Fail
4. The Core Theoretical Insight: Keyword Atoms
5. System Architecture Overview
6. Step-by-Step Algorithm and Go Implementation
7. Complexity Analysis
8. Handling Infeasibility and Edge Cases
9. Optional Extensions
10. Testing and Validation Strategy
11. Summary

---

## 1. Problem Statement

We start with a corpus of documents already stored in a database. Each document is associated with some number of pages, and — for this paper — we will treat "500 pages" as a stand-in for any fixed capacity limit `B` (it could be page count, byte size, token count, or document count; the algorithm doesn't care which).

The goal is:

> Partition all documents into groups, where every group's total size is at most `B`, using only **keyword membership** as the mechanism for deciding which documents go together — while keeping as few groups as possible and keeping semantically related documents together as much as possible.

The constraint that makes this hard is the phrase **"using only keyword membership."** If we were allowed to split documents arbitrarily (e.g., "put the first 500 documents in bucket 1, the next 500 in bucket 2"), this would be trivial. But because grouping must be *explainable* in terms of keywords — bucket 1 is "contains `invoice` and `2023`", bucket 2 is "contains `invoice` and not `2023`", and so on — the problem becomes a search over a combinatorial space of keyword predicates.

This is exactly the situation described in the source discussion this paper is based on: the temptation is to reach for "generate every combination of keywords," which is a `2^K` idea, and the goal of this paper is to explain precisely why that instinct is *directionally correct* (it identifies the right mathematical structure) but *operationally wrong* (you should never actually materialize `2^K` anything), and what to build instead.

---

## 2. Assumptions About the Existing Database

Since the prompt asks us to assume a database already exists, we need to be explicit about what "already exists" means, because the shape of Stage 1 of the algorithm depends on it.

There are two realistic starting points:

**Case A — Keywords are already tagged.** There is a table (or a document field) that already associates each document with a set of keyword IDs. This is the common case if the corpus went through some ingestion pipeline that ran tagging, tf-idf keyword extraction, entity extraction, or manual labeling.

A typical relational shape:

```sql
-- documents already in the DB
CREATE TABLE documents (
    id          BIGSERIAL PRIMARY KEY,
    title       TEXT,
    page_count  INT NOT NULL
);

-- the controlled vocabulary of keywords
CREATE TABLE keywords (
    id      BIGSERIAL PRIMARY KEY,
    label   TEXT UNIQUE NOT NULL
);

-- many-to-many membership: this IS the inverted index
CREATE TABLE document_keywords (
    document_id BIGINT REFERENCES documents(id),
    keyword_id  BIGINT REFERENCES keywords(id),
    PRIMARY KEY (document_id, keyword_id)
);
```

**Case B — Keywords are not tagged yet, only full text exists.** In that case, the "get the keywords" step is actually a keyword-*extraction* step (TF-IDF, RAKE, YAKE, or an LLM-based extractor) that populates a table like the one above before Stage 1 can run. This paper focuses on the partitioning algorithm itself, so we assume Case A — the corpus already has a `document_keywords` membership table, however it was produced. If your database is currently in Case B, the only change is that Step 1 below becomes "run an extraction job that writes into `document_keywords`" instead of "read `document_keywords` directly." Everything downstream is identical.

The important design decision to make explicit up front: **keyword membership is a bipartite relation between documents and keywords, and the entire algorithm is built on top of that relation represented as bitsets.** Nothing else about the schema matters much — column names, exact table layout, whether it's Postgres, MySQL, or SQLite, are all interchangeable details.

---

## 3. Why the Obvious Approaches Fail

### 3.1 The greedy / linear partition trap

The first instinct is usually something like:

```
bucket 1 = documents containing keyword A
bucket 2 = (documents containing keyword B) minus bucket 1
bucket 3 = (documents containing keyword C) minus (bucket 1 ∪ bucket 2)
...
```

This is a **priority partition**: each keyword claims whatever is left over after the previous keywords have taken their share. The problem is immediate: if keyword `A` alone matches 2,000 documents and your capacity is 500, bucket 1 is already 4x over budget. Nothing about "subtract what came before" fixes an oversized *first* bucket. This approach conflates two different questions — "which documents share a keyword" and "how do we keep any single group under the cap" — and only answers the first one.

### 3.2 The `2^K` enumeration trap

The next instinct, on noticing the above problem, is: "what if I consider every possible combination of keywords being present or absent?" For `K` keywords, that is the set of all Boolean formulas of the form:

```
k1 ∧ k2 ∧ ¬k3 ∧ ... ∧ kK
```

There are `2^K` such formulas, and they correspond to the **atoms of the Boolean algebra generated by the keywords** — every document falls into exactly one of these atoms based on its "signature" (its yes/no membership vector across all `K` keywords).

This idea is *mathematically correct* — it is in fact the finest partition of the corpus that keyword membership can ever produce, and everything in this paper is built on it. But it is *operationally the wrong thing to compute directly*, for two independent reasons:

1. **The search space is exponential in the wrong variable.** If you have 500 keywords, `2^500` is not just large, it is meaningless — it vastly exceeds the number of atoms that can actually be *occupied*, because you only have `N` documents. At most `N` of the `2^K` atoms are non-empty. Enumerating `2^K` combinations to discover which handful are occupied is like iterating over every possible telephone number to find out which ones happen to be assigned to a customer — you should instead just look at the customer list.
2. **Most keyword combinations are far too large or far too small on their own.** As shown in the source discussion, `|A|` alone could be 5,000 and `|A ∩ B|` could still be 4,000 if the corpus is keyword-dense. The presence of many valid Boolean regions gives you no guarantee that *any* of them respects the 500-document cap. You still have to search for the specific combinations that do — enumerating the full lattice doesn't hand you the answer, it just hands you a very large haystack.

The correct move — and the central idea of this paper — is to **compute each document's signature directly (in one pass over the data) instead of enumerating the `2^K` formulas that signatures could take.** This changes the driving variable from `2^K` to `N × K`, which is enormously smaller for realistic corpora (`N = 5,000`, `K` in the hundreds).

---

## 4. The Core Theoretical Insight: Keyword Atoms

### 4.1 Signatures and equivalence classes

Define, for every document `d`, its **keyword signature**:

```
σ(d) = ( k1(d), k2(d), k3(d), ..., kK(d) )
```

where each `ki(d)` is 1 if document `d` contains keyword `i`, and 0 otherwise. This is just a bit vector of length `K`.

Two documents `d1` and `d2` are **keyword-indistinguishable** if `σ(d1) = σ(d2)`. Group all documents by their signature. Each resulting group is called an **atom** — it is the smallest group that *any* keyword-only partitioning scheme could ever produce, because no Boolean formula over the keywords can separate two documents with an identical signature. This is the formalization of the "Boolean atoms" observation from the original discussion.

### 4.2 The Feasibility Theorem

This gives us something extremely useful: a cheap, exact test for whether the problem is even solvable at all, *before* we spend any effort trying to build buckets.

> **Feasibility Theorem.** A partition of the corpus into groups of size ≤ `B`, using only keyword-membership predicates, exists **if and only if** every atom (every equivalence class under `σ`) has size ≤ `B`.

Why this is true, informally: a keyword predicate can only ever *combine* atoms (union them together into a bucket) — it can never *split* an atom, because by definition every document inside an atom answers every keyword question identically. So if one atom already has 1,200 documents that are all keyword-identical, there is no clever combination of predicates that will ever tell those 1,200 documents apart. You are stuck with a bucket of at least 1,200, full stop.

This theorem is the single most important thing to check first, because it tells you in one linear pass whether you are solving "find the best partition" (an optimization problem) or "there is no valid answer, and you need either more keywords, a bigger capacity limit, or a different partitioning mechanism" (a feasibility failure, discussed in Section 8).

---

## 5. System Architecture Overview

Putting the previous two sections together, the algorithm has two clean stages:

**Stage 1 — Atomic signature grouping.** Compute every document's signature in one pass. Group documents by identical signature. This gives you the natural, finest-grained clusters the data supports, and it gives you the feasibility check for free.

**Stage 2 — Capacity-constrained recursive splitting.** For every group produced in Stage 1 that already fits under the cap `B`, you're done — it becomes a final bucket. For every group that's still too large, recursively pick the *single best splitting keyword* (the one that divides the group as close to 50/50 as possible) and split into a "has this keyword" half and a "doesn't have this keyword" half. Repeat on each half until every resulting bucket is ≤ `B`, or until you hit an atom (Stage 1's job was to tell you exactly where those floors are).

Visually:

```
                 documents in the DB
                          │
                          ▼
             Stage 1: compute signatures
                          │
                          ▼
              group by identical signature
                          │
                          ▼
              ┌───────────────────────┐
              │   atomic groups        │
              └───────────────────────┘
                          │
              ┌───────────┴───────────┐
              ▼                       ▼
        size ≤ B                size > B
              │                       │
        final bucket        Stage 2: pick best
                             splitting keyword,
                             recurse on both halves
                                       │
                             ┌─────────┴─────────┐
                             ▼                   ▼
                        has keyword        lacks keyword
                             │                   │
                             └─────────┬─────────┘
                                       ▼
                              repeat until ≤ B
                              or atom is hit
```

Note that Stage 1 is not just a preprocessing convenience — it's what tells Stage 2 when to *stop trying*. Without it, a recursive splitter could spin forever hunting for a keyword that separates two documents that are, in fact, permanently glued together.

---

## 6. Step-by-Step Algorithm and Go Implementation

This section walks through the implementation piece by piece. All code is Go, and is written to be dropped into a real service with minimal adaptation — the only things you'd need to change are the SQL and the connection setup for your specific database.

### 6.1 Step 1 — Extracting keyword membership from the database

The first real step is turning the `document_keywords` table into an in-memory structure we can compute on quickly. We load the full membership relation once, rather than issuing per-document queries, because the entire rest of the algorithm depends on doing thousands of cheap in-memory bit operations instead of thousands of database round-trips.

```go
package partition

import (
	"context"
	"database/sql"
	"fmt"
)

// MembershipIndex holds the full document<->keyword relation loaded
// from the database, plus the ordering used to build bit positions.
type MembershipIndex struct {
	DocIDs      []int64       // stable order: index i == bit position i
	KeywordIDs  []int64       // stable order: index j == bit position j
	docPos      map[int64]int // documentID -> bit position
	keywordPos  map[int64]int // keywordID  -> bit position
}

// LoadMembershipIndex reads all documents and all document/keyword
// links from the database in two bulk queries.
func LoadMembershipIndex(ctx context.Context, db *sql.DB) (*MembershipIndex, [][2]int64, error) {
	idx := &MembershipIndex{
		docPos:     make(map[int64]int),
		keywordPos: make(map[int64]int),
	}

	// 1. Load every document ID once, in a stable order.
	docRows, err := db.QueryContext(ctx, `SELECT id FROM documents ORDER BY id`)
	if err != nil {
		return nil, nil, fmt.Errorf("loading documents: %w", err)
	}
	defer docRows.Close()
	for docRows.Next() {
		var id int64
		if err := docRows.Scan(&id); err != nil {
			return nil, nil, err
		}
		idx.docPos[id] = len(idx.DocIDs)
		idx.DocIDs = append(idx.DocIDs, id)
	}

	// 2. Load every keyword ID once, in a stable order.
	kwRows, err := db.QueryContext(ctx, `SELECT id FROM keywords ORDER BY id`)
	if err != nil {
		return nil, nil, fmt.Errorf("loading keywords: %w", err)
	}
	defer kwRows.Close()
	for kwRows.Next() {
		var id int64
		if err := kwRows.Scan(&id); err != nil {
			return nil, nil, err
		}
		idx.keywordPos[id] = len(idx.KeywordIDs)
		idx.KeywordIDs = append(idx.KeywordIDs, id)
	}

	// 3. Load the full membership relation as (document_id, keyword_id) pairs.
	linkRows, err := db.QueryContext(ctx,
		`SELECT document_id, keyword_id FROM document_keywords`)
	if err != nil {
		return nil, nil, fmt.Errorf("loading document_keywords: %w", err)
	}
	defer linkRows.Close()

	var links [][2]int64
	for linkRows.Next() {
		var docID, kwID int64
		if err := linkRows.Scan(&docID, &kwID); err != nil {
			return nil, nil, err
		}
		links = append(links, [2]int64{docID, kwID})
	}

	return idx, links, nil
}
```

**Why load everything in three bulk queries instead of one per document?** Because the whole point of everything that follows is to replace database latency with in-memory bit operations. Three sequential-scan queries against `documents`, `keywords`, and `document_keywords` are each `O(rows)` and trivially fast even at millions of rows; issuing `N` individual "give me this document's keywords" queries turns a sub-second job into a job dominated by network round-trip time.

### 6.2 Step 2 — Representing membership as bitsets

This is the step that makes the difference between the `O(N²K)` worst case and a fast, practical implementation. Instead of storing "keyword A matches documents 4, 17, 812, ..." as a list, we store it as a **bitset**: one bit per document, indexed by that document's position in `DocIDs`. Checking, intersecting, and subtracting these sets then becomes a handful of machine-word operations instead of list scans and comparisons.

```go
package partition

import "math/bits"

// Bitset is a fixed-size bit vector, one bit per document, backed by
// 64-bit words so that AND / ANDNOT / population-count run at native
// CPU speed instead of scanning individual booleans.
type Bitset struct {
	words []uint64
	n     int // number of usable bits (== number of documents)
}

func NewBitset(n int) *Bitset {
	return &Bitset{
		words: make([]uint64, (n+63)/64),
		n:     n,
	}
}

func (b *Bitset) Set(i int) {
	b.words[i>>6] |= 1 << uint(i&63)
}

func (b *Bitset) Get(i int) bool {
	return b.words[i>>6]&(1<<uint(i&63)) != 0
}

// Count returns the number of set bits, i.e. how many documents are
// currently in this set. This is the "bucket size" check.
func (b *Bitset) Count() int {
	c := 0
	for _, w := range b.words {
		c += bits.OnesCount64(w)
	}
	return c
}

// And returns a new bitset containing documents present in both b and other.
func (b *Bitset) And(other *Bitset) *Bitset {
	out := NewBitset(b.n)
	for i := range b.words {
		out.words[i] = b.words[i] & other.words[i]
	}
	return out
}

// AndNot returns a new bitset containing documents in b but not in other.
func (b *Bitset) AndNot(other *Bitset) *Bitset {
	out := NewBitset(b.n)
	for i := range b.words {
		out.words[i] = b.words[i] &^ other.words[i]
	}
	return out
}

// Indices returns the document bit positions currently set.
func (b *Bitset) Indices() []int {
	out := make([]int, 0, b.Count())
	for wi, w := range b.words {
		for w != 0 {
			tz := bits.TrailingZeros64(w)
			out = append(out, wi*64+tz)
			w &= w - 1 // clear lowest set bit
		}
	}
	return out
}
```

We now build one such bitset **per keyword**, where bit `i` means "document at position `i` contains this keyword":

```go
// BuildKeywordBitsets converts the raw (document_id, keyword_id) links
// into one bitset per keyword, indexed by keyword bit position.
func BuildKeywordBitsets(idx *MembershipIndex, links [][2]int64) []*Bitset {
	n := len(idx.DocIDs)
	bitsets := make([]*Bitset, len(idx.KeywordIDs))
	for j := range bitsets {
		bitsets[j] = NewBitset(n)
	}
	for _, link := range links {
		docID, kwID := link[0], link[1]
		docPos, ok1 := idx.docPos[docID]
		kwPos, ok2 := idx.keywordPos[kwID]
		if ok1 && ok2 {
			bitsets[kwPos].Set(docPos)
		}
	}
	return bitsets
}
```

**Why this representation specifically?** Three reasons, directly answering the "how do you avoid `2^K`" question from the source discussion:

1. Splitting a bucket by a keyword is now `bucket.And(keywordBitset)` and `bucket.AndNot(keywordBitset)` — each is a single pass over `n/64` machine words, not a document-by-document scan.
2. Counting a bucket's size (the capacity check) is a population count over the same `n/64` words.
3. We never need to materialize a keyword *combination* as its own object ahead of time — every combination is just the bitset that falls out of a sequence of `And`/`AndNot` calls, computed lazily only for the branches we actually visit.

### 6.3 Step 3 — Computing document signatures and grouping into atoms

Now we implement Stage 1 from Section 5: turn the keyword bitsets "sideways" into one signature per document, and group documents that share a signature.

```go
package partition

// Signature is a packed bit vector — one bit per keyword — representing
// a single document's full keyword membership profile.
type Signature string

// ComputeSignatures builds one Signature per document by reading the
// keyword bitsets "column-wise" (once per keyword, not once per document
// per keyword), which keeps this an O(N*K) pass over machine words.
func ComputeSignatures(numDocs int, keywordBitsets []*Bitset) []Signature {
	numKeywords := len(keywordBitsets)
	byteLen := (numKeywords + 7) / 8
	buffers := make([][]byte, numDocs)
	for i := range buffers {
		buffers[i] = make([]byte, byteLen)
	}

	for kwPos, bs := range keywordBitsets {
		for _, docPos := range bs.Indices() {
			buffers[docPos][kwPos/8] |= 1 << uint(kwPos%8)
		}
	}

	sigs := make([]Signature, numDocs)
	for i, buf := range buffers {
		sigs[i] = Signature(buf)
	}
	return sigs
}

// GroupBySignature groups document bit-positions by identical signature.
// Each resulting slice is one "atom" — the finest group keyword-only
// partitioning can ever produce.
func GroupBySignature(sigs []Signature) map[Signature][]int {
	groups := make(map[Signature][]int)
	for docPos, sig := range sigs {
		groups[sig] = append(groups[sig], docPos)
	}
	return groups
}
```

**Why iterate keyword-by-keyword instead of document-by-document?** Because `bs.Indices()` only visits documents that actually *have* that keyword, so the total work across all keywords is proportional to the number of (document, keyword) links that actually exist — not to `N × K` in the worst case where most cells are zero. For a sparse membership relation (most real keyword taggings are sparse), this is meaningfully cheaper than a dense double loop, while still being `O(N·K)` in the theoretical worst (fully dense) case.

### 6.4 Step 4 — Feasibility checking

This directly implements the Feasibility Theorem from Section 4.2. It must run **before** any recursive splitting, because it tells you whether recursive splitting can even succeed.

```go
package partition

import "fmt"

// FeasibilityReport summarizes whether a valid ≤ maxBucketSize partition
// can exist at all, given the current keyword vocabulary.
type FeasibilityReport struct {
	Feasible          bool
	OversizedAtoms    map[Signature]int // signature -> atom size, for every atom > cap
	LargestAtomSize   int
}

func CheckFeasibility(groups map[Signature][]int, maxBucketSize int) FeasibilityReport {
	report := FeasibilityReport{
		Feasible:       true,
		OversizedAtoms: make(map[Signature]int),
	}
	for sig, docs := range groups {
		size := len(docs)
		if size > report.LargestAtomSize {
			report.LargestAtomSize = size
		}
		if size > maxBucketSize {
			report.Feasible = false
			report.OversizedAtoms[sig] = size
		}
	}
	return report
}

func (r FeasibilityReport) String() string {
	if r.Feasible {
		return fmt.Sprintf("feasible: largest indivisible group has %d documents", r.LargestAtomSize)
	}
	return fmt.Sprintf("INFEASIBLE: %d keyword-atom(s) exceed the cap, largest has %d documents — "+
		"no keyword-only partition can separate these", len(r.OversizedAtoms), r.LargestAtomSize)
}
```

If `report.Feasible` is `false`, stop here and go to Section 8 — recursive splitting cannot fix this, no matter how it's tuned, because the oversized groups are made of documents that answer every available keyword question identically.

### 6.5 Step 5 — Choosing the best splitting keyword

This is the step the source discussion correctly identifies as "where I'd spend most of the engineering effort." Given an oversized bucket, we don't want just *any* keyword that reduces its size — we want the keyword that splits it as close to 50/50 as possible, because a balanced split minimizes the number of further splits needed to get everything under the cap.

```go
package partition

// SplitCandidate describes how well one keyword would split a bucket.
type SplitCandidate struct {
	KeywordPos int
	WithSize   int
	WithoutSize int
	Balance    float64 // 0 = useless split, approaches 1.0 = perfectly balanced
}

// BestSplit scores every candidate keyword against the current bucket
// and returns the one with the highest balance score. Keywords that do
// not actually divide the bucket (everyone has it, or no one does) are
// skipped, since they cannot make progress.
func BestSplit(bucket *Bitset, candidateKeywords []int, keywordBitsets []*Bitset) (SplitCandidate, bool) {
	size := bucket.Count()
	if size == 0 {
		return SplitCandidate{}, false
	}

	best := SplitCandidate{Balance: -1}
	found := false

	for _, kwPos := range candidateKeywords {
		withSize := bucket.And(keywordBitsets[kwPos]).Count()
		withoutSize := size - withSize

		if withSize == 0 || withoutSize == 0 {
			continue // this keyword does not divide this bucket at all
		}

		smaller := withSize
		if withoutSize < smaller {
			smaller = withoutSize
		}
		// Balance score: 1.0 for a perfect 50/50 split, approaching 0 for
		// a lopsided split like 990/10. This is the score() function from
		// the source discussion, formalized.
		balance := float64(smaller) / (float64(size) / 2.0)
		if balance > 1.0 {
			balance = 1.0
		}

		if balance > best.Balance {
			best = SplitCandidate{
				KeywordPos:  kwPos,
				WithSize:    withSize,
				WithoutSize: withoutSize,
				Balance:     balance,
			}
			found = true
		}
	}

	return best, found
}
```

**Why balance instead of "biggest immediate reduction"?** Because the goal is not to shrink the bucket once — it's to get every descendant of this bucket under the cap in as few splitting rounds as possible. A 1,500-document bucket split 1,490/10 has made "progress" (the parent shrank), but you're still stuck reducing a 1,490-document bucket almost from scratch. A 750/750 split, by contrast, means the next good split on either half can plausibly land both children under a 500 cap. This is exactly the distinction drawn in the source discussion between "does this keyword reduce the largest bucket" and "does this keyword actually help."

### 6.6 Step 6 — Recursive capacity partitioning

This ties Steps 4 and 5 together into the actual recursive algorithm. Note that the base case is not just "size ≤ cap" — it is also "we've run out of usable keywords," which is what happens when recursion bottoms out at an atom from Stage 1.

```go
package partition

// Bucket is one node of the partition tree.
type Bucket struct {
	Docs *Bitset // which documents (by bit position) are in this bucket
}

// PartitionOutcome separates buckets that satisfy the cap from
// atomic groups that could not be reduced further (a feasibility
// failure localized to one region of the corpus).
type PartitionOutcome struct {
	Buckets    []*Bitset
	Unresolved []*Bitset // exceed the cap AND have no splitting keyword left
}

// RecursivePartition implements Stage 2: recursively pick the best
// splitting keyword for any bucket over the cap, until every bucket is
// under the cap or no keyword can make further progress.
func RecursivePartition(bucket *Bitset, availableKeywords []int, keywordBitsets []*Bitset, maxBucketSize int) PartitionOutcome {
	size := bucket.Count()

	if size == 0 {
		return PartitionOutcome{}
	}
	if size <= maxBucketSize {
		return PartitionOutcome{Buckets: []*Bitset{bucket}}
	}

	best, found := BestSplit(bucket, availableKeywords, keywordBitsets)
	if !found {
		// No remaining keyword divides this bucket at all. Per the
		// Feasibility Theorem, this bucket sits inside — or spans — one
		// or more atoms that are individually too large to separate.
		return PartitionOutcome{Unresolved: []*Bitset{bucket}}
	}

	yes := bucket.And(keywordBitsets[best.KeywordPos])
	no := bucket.AndNot(keywordBitsets[best.KeywordPos])

	remaining := removeKeyword(availableKeywords, best.KeywordPos)

	yesOutcome := RecursivePartition(yes, remaining, keywordBitsets, maxBucketSize)
	noOutcome := RecursivePartition(no, remaining, keywordBitsets, maxBucketSize)

	return PartitionOutcome{
		Buckets:    append(yesOutcome.Buckets, noOutcome.Buckets...),
		Unresolved: append(yesOutcome.Unresolved, noOutcome.Unresolved...),
	}
}

func removeKeyword(keywords []int, remove int) []int {
	out := make([]int, 0, len(keywords)-1)
	for _, k := range keywords {
		if k != remove {
			out = append(out, k)
		}
	}
	return out
}
```

Two design choices are worth calling out explicitly:

- **We remove the chosen keyword from `availableKeywords` before recursing.** Once a keyword has been used to split a bucket, every document in that half already agrees on it (either all have it or none do), so testing it again on either child can never produce a new split — it's a wasted candidate. Dropping it keeps every recursive call's candidate list strictly shrinking, which is also what guarantees the recursion terminates (see Section 7).
- **`Unresolved` is a first-class output, not an error.** A bucket lands there when it is too big *and* no keyword can shrink it any further — which, by the Feasibility Theorem, means it fully contains at least one oversized atom. Surfacing this explicitly (rather than silently emitting an over-cap bucket) is what lets the calling code decide how to handle it (Section 8) instead of quietly violating the caller's constraint.

### 6.7 Step 7 — Persisting bucket assignments back to the database

The final step takes the in-memory `PartitionOutcome` and writes it back as bucket IDs, so downstream systems can query "give me all documents in bucket 42" the normal way.

```go
package partition

import (
	"context"
	"database/sql"
	"fmt"
)

// PersistBuckets writes a bucket_id assignment for every document,
// creating the buckets table if needed. Unresolved groups are written
// with negative bucket IDs so they are trivially distinguishable and
// can be queried/handled separately by operators.
func PersistBuckets(ctx context.Context, db *sql.DB, idx *MembershipIndex, outcome PartitionOutcome, maxBucketSize int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS document_buckets (
			document_id BIGINT PRIMARY KEY,
			bucket_id   INT NOT NULL,
			bucket_size INT NOT NULL
		)`); err != nil {
		return fmt.Errorf("creating document_buckets: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO document_buckets (document_id, bucket_id, bucket_size)
		VALUES ($1, $2, $3)
		ON CONFLICT (document_id) DO UPDATE
		SET bucket_id = EXCLUDED.bucket_id, bucket_size = EXCLUDED.bucket_size`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	bucketID := 1
	for _, b := range outcome.Buckets {
		size := b.Count()
		for _, docPos := range b.Indices() {
			docID := idx.DocIDs[docPos]
			if _, err := stmt.ExecContext(ctx, docID, bucketID, size); err != nil {
				return fmt.Errorf("writing bucket %d: %w", bucketID, err)
			}
		}
		bucketID++
	}

	unresolvedID := -1
	for _, b := range outcome.Unresolved {
		size := b.Count()
		for _, docPos := range b.Indices() {
			docID := idx.DocIDs[docPos]
			if _, err := stmt.ExecContext(ctx, docID, unresolvedID, size); err != nil {
				return fmt.Errorf("writing unresolved group %d: %w", unresolvedID, err)
			}
		}
		unresolvedID--
	}

	return tx.Commit()
}
```

Wiring the whole pipeline together end-to-end:

```go
package partition

import (
	"context"
	"database/sql"
	"fmt"
)

const MaxBucketSize = 500

func RunPartitioning(ctx context.Context, db *sql.DB) error {
	idx, links, err := LoadMembershipIndex(ctx, db)
	if err != nil {
		return err
	}

	keywordBitsets := BuildKeywordBitsets(idx, links)

	// Stage 1: signatures + atoms + feasibility.
	sigs := ComputeSignatures(len(idx.DocIDs), keywordBitsets)
	atoms := GroupBySignature(sigs)
	feasibility := CheckFeasibility(atoms, MaxBucketSize)
	fmt.Println(feasibility)
	if !feasibility.Feasible {
		fmt.Printf("proceeding anyway: %d atom(s) will land in Unresolved\n", len(feasibility.OversizedAtoms))
	}

	// Stage 2: recursive splitting, starting from the full corpus.
	all := NewBitset(len(idx.DocIDs))
	for i := range idx.DocIDs {
		all.Set(i)
	}
	allKeywords := make([]int, len(keywordBitsets))
	for i := range allKeywords {
		allKeywords[i] = i
	}

	outcome := RecursivePartition(all, allKeywords, keywordBitsets, MaxBucketSize)

	fmt.Printf("produced %d buckets, %d unresolved oversized group(s)\n",
		len(outcome.Buckets), len(outcome.Unresolved))

	return PersistBuckets(ctx, db, idx, outcome, MaxBucketSize)
}
```

Notice that Stage 2 is run starting from the **whole corpus**, not from each Stage-1 atom individually. This is deliberate: starting the recursive splitter from the full set lets it choose splits across the entire keyword vocabulary at every level, which generally produces far fewer final buckets than splitting each small atom in isolation. Stage 1's atoms are used for the feasibility check and as the mathematical floor recursion will hit — not as a mandatory starting grid for Stage 2.

---

## 7. Complexity Analysis

Let `N` = number of documents, `K` = number of keywords, `B` = capacity cap, `L` = number of (document, keyword) links that actually exist (`L ≤ N·K`, and `L ≈ N·K` only in the unrealistic fully-dense case).

### 7.1 Signature construction (Stage 1)

Building all keyword bitsets is one pass over the `L` links: **`O(L)`**.

Computing signatures from those bitsets, iterating keyword-by-keyword and only visiting set bits, is also **`O(L)`**, plus `O(N·K/8)` for allocating the zeroed signature buffers (bytes, not documents-times-keywords work). Grouping by signature (a Go map insert per document) is **`O(N)`** expected time with good hash distribution.

**Total for Stage 1: `O(L + N·K/8) ≈ O(N·K)` in the worst (dense) case, and much closer to `O(L)` for realistic sparse taggings.** This matches — and gives a precise basis for — the `O(N·K)` figure from the source discussion.

### 7.2 Recursive splitting (Stage 2) — best, average, and worst case

At every recursive call on a bucket of size `m`, evaluating all remaining candidate keywords costs `O(m/64 · |candidates|)` machine-word operations (bitset `And` plus `Count`, per candidate). The question that determines overall complexity is: **how many total "keyword tests" happen across the whole recursion, and how unbalanced are the splits?**

**Best case — every split is close to 50/50.**
The recursion depth to get from `N` down to `B` is `log2(N/B)`. At each of the `O(log(N/B))` levels, the total work across all buckets at that level is bounded by one full pass over the remaining keyword candidates against that level's total document count, i.e. `O(N·K/64)`. So:

```
Best case:  O( (N·K/64) · log2(N/B) )
```

For `N = 5,000`, `B = 500`: `log2(10) ≈ 3.3`, so roughly 4 levels — a small, fast job regardless of `K`.

**Worst case — every useful split is maximally lopsided (e.g., 4999/1 every time).**
Here the recursion depth is not `log(N/B)` but closer to `N - B`, because each split only peels off a handful of documents. Worse, at each of those `~N` levels, you are still testing candidate keywords against a bucket that has barely shrunk, so the per-level cost stays close to `O(N·K/64)` for a long time instead of shrinking geometrically. This gives:

```
Worst case:  O( (N² · K) / 64 )
```

This is the pathological case the source discussion calls out and is worth taking seriously: it happens when your keyword vocabulary is either extremely sparse (few documents match any given keyword) or extremely skewed (one dominant class overwhelms every keyword's distribution). In practice this is rare with a reasonably chosen vocabulary, but it's exactly why Step 5 (balance-guided keyword selection) exists — without it, a naive "just use whichever keyword shrinks the bucket at all" strategy is what produces this case; balance-guided selection actively steers away from it whenever a better-balanced keyword exists.

**Average / typical case.**
With a moderately well-distributed keyword vocabulary (which is common in practice — tags, categories, entities extracted from real text rarely all skew 99/1), splits tend to land somewhere between "perfectly balanced" and "somewhat lopsided," giving:

```
Typical case:  O( (N·K/64) · log(N) ), with a larger constant factor than the best case
```

### 7.3 Why bitsets change the constant factor, not just the asymptotics

The `/64` in every formula above is not decorative — it's the actual payoff of Step 2. Without bitsets, a "does bucket contain keyword" check is `O(bucket size)` per candidate (a linear scan/lookup per document). With 64-bit-word bitsets, the same check is `O(bucket size / 64)`, because `AND` and population-count operate on 64 documents per CPU instruction. For `N = 5,000`, that's roughly 78 machine words instead of 5,000 individual comparisons — a ~64x constant-factor speedup that applies uniformly to every case above, best, worst, and typical alike. (On hardware with wider SIMD registers this constant improves further, to 128, 256, or 512 documents per instruction, at the cost of more complex code — not necessary at this corpus size, but worth knowing as a future lever.)

| Stage | Complexity |
|---|---|
| Signature construction | `O(L)` ≈ `O(N·K)` worst case |
| Recursive splitting — best case | `O((N·K/64)·log(N/B))` |
| Recursive splitting — typical case | `O((N·K/64)·log N)`, larger constant |
| Recursive splitting — worst case | `O(N²·K/64)` |
| (Rejected) exhaustive `2^K` enumeration | `O(2^K)` or worse — never actually needed |

---

## 8. Handling Infeasibility and Edge Cases

Section 4.2's Feasibility Theorem guarantees that some corpora simply **cannot** be partitioned under the cap using the available keywords, no matter how clever the splitting logic is. This is not a bug to be engineered around — it is a mathematical property of the data and vocabulary you were given. What matters is having a deliberate, visible response when it happens, rather than silently emitting an oversized bucket or hanging in an infinite search for a split that doesn't exist.

Concretely, this shows up as non-empty `PartitionOutcome.Unresolved` groups. Reasonable responses, roughly in order of preference:

1. **Request a richer keyword vocabulary for just the unresolved region.** Since `Unresolved` groups are exactly the atoms (or unions of atoms) that no current keyword can divide, the most direct fix is adding one new discriminating keyword that specifically distinguishes documents inside that group — for example, a finer-grained tag, a date field turned into a keyword, or a manually reviewed sub-category. You don't need to re-tag the whole corpus, only the unresolved slice.
2. **Fall back to a secondary partitioning signal for unresolved groups only.** Section 9.2 discusses using embeddings for this — but the key point is that this fallback should be scoped narrowly, only to the documents that keyword logic provably cannot separate, not applied wholesale in place of the keyword approach.
3. **Relax the cap for that specific group, with the violation logged and visible.** Sometimes an atom of 520 documents against a cap of 500 is an acceptable, explicitly-flagged overage rather than a real problem. This should always be a deliberate, visible decision (e.g., a review dashboard, an alert) — never a silent default.
4. **Allow controlled duplication.** If the downstream consumer of these buckets can tolerate a document appearing in more than one bucket (this breaks the "partition" property into a "cover" property), an oversized atom can be split arbitrarily (e.g., by document ID) into multiple duplicate-tagged buckets. This trades strict keyword-explainability for capacity compliance and should only be chosen if the consumer genuinely doesn't need every bucket to be keyword-describable.

A second, more subtle edge case worth testing for explicitly: **near-duplicate signatures that differ only in keywords no longer in `availableKeywords`.** Because Step 6 removes a keyword from consideration for both children after using it to split, it is possible (correctly) for a bucket to become unresolved even though some *unused* keyword elsewhere in the vocabulary could have separated it, if that keyword happened to be a poor global choice earlier and was consumed by a different branch of the recursion first. This is not a bug — different branches of the recursion have different `availableKeywords` lists by design — but it means `Unresolved` should be interpreted as "not resolved by the path this run happened to take," and it is reasonable to retry an unresolved group with a widened keyword pool if the very first pass reports any unresolved output.

---

## 9. Optional Extensions

### 9.1 A combined scoring function: coverage, fragmentation, cohesion

The base algorithm above optimizes purely for balance at each split, which is a good local proxy for "few total buckets," but it says nothing about whether the resulting buckets are *semantically sensible* groupings a human would recognize, versus a technically-valid but arbitrary split. The source discussion's proposed scoring function formalizes this concern:

```
Score = α·Coverage − β·Fragmentation + γ·Cohesion
```

where `Coverage` rewards assigning every document to some bucket, `Fragmentation` penalizes splitting a semantically tight cluster of documents across many different buckets, and `Cohesion` rewards documents within a bucket sharing more than the one keyword that was used to split them. In practice, this can be layered on top of Step 5 without changing the recursion's structure: instead of `BestSplit` maximizing balance alone, it can maximize a weighted combination of balance and a cohesion measure (for example, average pairwise Jaccard similarity of keyword sets within each resulting half, computed cheaply via bitset population counts). This is a tuning knob, not a structural change — the two-stage architecture and the bitset machinery are unaffected.

### 9.2 Embedding-assisted splitting for the "no good keyword" case

When `BestSplit` returns "not found" for a bucket that's still over the cap and not shrinkable by any remaining keyword, that bucket may still be semantically splittable — just not along a *keyword* boundary that exists in the current vocabulary. This is where the source discussion's embedding suggestion becomes genuinely useful, scoped narrowly per Section 8: instead of falling back to arbitrary document-ID slicing, embed the documents in that specific unresolved group and run a capacity-constrained clustering pass (e.g., k-means with a maximum-cluster-size constraint, or an off-the-shelf balanced clustering algorithm) on just that subset. The keyword machinery remains the primary partitioning mechanism for the ~99% of the corpus it successfully resolves; embeddings are a targeted patch for the fraction of documents that keyword membership genuinely cannot distinguish — not a replacement for the keyword pipeline built above.

---

## 10. Testing and Validation Strategy

Because the algorithm's correctness rests on a small number of provable invariants, tests should check those invariants directly rather than only checking output on a handful of hand-picked examples:

- **Partition invariant:** every document ID from the input appears in exactly one bucket (or exactly one unresolved group) in the output — no document is dropped, and none is duplicated, across `outcome.Buckets` and `outcome.Unresolved` combined.
- **Capacity invariant:** every bucket in `outcome.Buckets` has `Count() <= MaxBucketSize`.
- **Explainability invariant:** every bucket can be described as a conjunction of "has keyword" / "lacks keyword" statements — this falls out of the algorithm's structure automatically, but is worth asserting on synthetic test data where the expected predicate is known ahead of time.
- **Feasibility agreement:** `CheckFeasibility`'s verdict should agree with whether `outcome.Unresolved` is empty, on the same input — if `CheckFeasibility` reports feasible but `RecursivePartition` still produces unresolved groups (or vice versa), that's a bug in one of the two, not a property of the data.
- **Determinism:** given the same database contents, two runs should produce the same bucket *assignments* (not necessarily the same bucket ID numbers) — this matters operationally, since re-running the job after adding a handful of new documents shouldn't reshuffle unrelated existing buckets. Guaranteeing this fully requires either a stable tie-breaking rule in `BestSplit` (e.g., lowest keyword ID wins ties) or accepting that only *new* documents move — both are legitimate choices, but the choice should be made explicitly and tested.
- **Synthetic pathological cases:** construct small synthetic datasets by hand for each complexity case in Section 7 (perfectly balanced keywords, a single dominant keyword, fully keyword-identical documents) and assert the algorithm produces the theoretically expected bucket count and correctly flags the identical-documents case as unresolved rather than silently violating the cap.

---

## 11. Summary

The instinct to reach for "every combination of keywords" is not wrong — it correctly identifies that the finest structure keyword membership can express is the Boolean atom / signature-equivalence-class structure described in Section 4. The mistake is in the word *enumerate*: instead of generating `2^K` candidate formulas and checking which are populated, compute each document's signature directly in one pass (`O(N·K)`, or `O(L)` for sparse data) and group by equality. That single change is what turns an intractable exponential search into a fast, linear preprocessing step — and as a side effect, it gives you an exact, cheap test (the Feasibility Theorem) for whether a valid partition can even exist before you spend any effort building one.

From there, the actual bucket-building work is a recursive splitter that, at each oversized bucket, picks the single keyword that divides it most evenly — implemented over bitsets so that every size check and every split is a handful of machine-word operations rather than a per-document scan. This two-stage design — **atomic signature grouping**, then **balance-guided recursive splitting** — is what should be built: it captures everything correct about the original `2^K` intuition, none of its computational cost, and gives you an explicit, well-defined answer for the one case (keyword-inseparable duplicate documents) that no keyword-only algorithm can ever solve.
