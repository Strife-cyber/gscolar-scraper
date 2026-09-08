// Package mine extracts keyword candidates from an already-scraped paper
// corpus and scores each one for discriminative (split) power, entirely
// offline — no Scholar round-trips.
//
// The score is the paper's Balance measure: for a candidate keyword k over a
// set of papers, With = papers whose text contains k, Without = the rest, and
//
//	Balance = min(With, Without) / (total / 2)
//
// clamped to [0,1]. Balance ≈ 1 means the keyword divides the corpus almost
// exactly in half (a great splitter); Balance ≈ 0 means it appears in almost
// every paper or almost none (it merely overlaps). A candidate is dropped when
// it is too frequent to be discriminating or too rare to estimate reliably.
// Candidates that are degenerate substrings of the venue query (so they match
// every paper) are handled by the caller via split.DropDegenerateKeywords.
package mine

import (
	"sort"
	"strings"
)

// Paper is the slice of the crawled record that candidate scoring needs.
type Paper struct {
	Hash    string
	Title   string
	Snippet string
	Year    int
}

// Options tunes candidate mining and scoring.
type Options struct {
	// MinBalance drops candidates whose Balance is below this threshold.
	MinBalance float64
	// MinFreq drops candidates that appear in fewer than this many papers;
	// the With/Without split is too noisy below it to trust.
	MinFreq int
	// MaxFreqFraction drops candidates appearing in more than this fraction of
	// all papers — such a word cannot divide the corpus meaningfully.
	MaxFreqFraction float64
	// Bigrams enables 2-word phrase candidates in addition to single tokens.
	Bigrams bool
	// BigramSep is the max position gap between the two words of a bigram.
	// Only meaningful when Bigrams is true.
	BigramSep int
	// MaxCandidates caps how many scored candidates are returned, best first.
	MaxCandidates int
}

// Candidate is one mined keyword with its offline discriminative statistics.
type Candidate struct {
	Keyword string
	With    int
	Without int
	Total   int
	Balance float64
}

// DefaultOptions returns sensible defaults for a corpus of thousands of papers.
func DefaultOptions() Options {
	return Options{
		MinBalance:      0.15,
		MinFreq:         3,
		MaxFreqFraction: 0.95,
		Bigrams:         false,
		BigramSep:       2,
		MaxCandidates:   200,
	}
}

// Tokenize splits s into lowercase word tokens, dropping punctuation, tokens
// shorter than 3 characters, pure-digit tokens, and stopwords.
func Tokenize(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z')
	})
	toks := make([]string, 0, len(fields))
	for _, f := range fields {
		w := strings.ToLower(f)
		if len(w) < 3 || stopWords[w] || isDigits(w) {
			continue
		}
		toks = append(toks, w)
	}
	return toks
}

// stopWords are function words that carry no topical signal. Deliberately a
// short list: domain terms like "learning" or "network" must never be culled.
// Auxiliary verbs, pronouns, determiners and generic adverbs are included so
// they cannot masquerade as high-Balance splitters — words like "has", "have"
// and "been" split titles evenly but barely reduce a Scholar count.
var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "for": true, "with": true,
	"on": true, "at": true, "in": true, "from": true, "to": true, "and": true,
	"or": true, "is": true, "are": true, "be": true, "using": true, "based": true,
	"towards": true, "via": true, "this": true, "that": true, "we": true,
	"it": true, "its": true, "by": true, "into": true, "than": true, "as": true,
	"their": true, "over": true, "under": true, "between": true,
	// auxiliary / modal verbs
	"has": true, "have": true, "had": true, "been": true, "being": true,
	"was": true, "were": true, "am": true, "do": true, "does": true,
	"did": true, "doing": true, "done": true, "will": true, "would": true,
	"shall": true, "should": true, "can": true, "could": true, "may": true,
	"might": true, "must": true, "ought": true,
	// pronouns and possessives
	"i": true, "you": true, "he": true, "she": true, "they": true,
	"me": true, "him": true, "her": true, "us": true, "them": true,
	"my": true, "mine": true, "your": true, "yours": true, "his": true,
	"hers": true, "our": true, "ours": true, "theirs": true,
	"myself": true, "yourself": true, "himself": true, "herself": true,
	"itself": true, "ourselves": true, "themselves": true,
	// determiners / quantifiers / conjunctions / adverbs
	"these": true, "those": true, "all": true, "any": true, "both": true,
	"each": true, "few": true, "more": true, "most": true, "other": true,
	"others": true, "some": true, "such": true, "no": true, "not": true,
	"only": true, "own": true, "same": true, "so": true, "too": true,
	"very": true, "just": true, "but": true, "if": true, "then": true,
	"else": true, "when": true, "where": true, "why": true, "how": true,
	"what": true, "which": true, "who": true, "whom": true, "here": true,
	"there": true, "again": true, "further": true, "once": true,
	"during": true, "before": true, "after": true, "above": true,
	"below": true, "through": true, "while": true, "also": true,
	"new": true, "one": true, "two": true, "three": true, "use": true,
	"used": true, "get": true, "got": true, "make": true, "made": true,
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// doc holds one paper's normalized, deduplicated token streams.
type doc struct {
	title map[string]bool
	snip  map[string]bool
	// titleList and snipList keep order so bigram adjacency is testable.
	titleList []string
	snipList  []string
}

// Mine scores every candidate keyword that appears in the corpus and returns
// the best-ranked ones, best first. Membership for a word candidate is an exact
// token match against a paper's title or snippet token sets; a bigram candidate
// matches when its two words appear adjacently (within BigramSep positions).
//
// Candidates are ranked by Balance descending, then by With+Without
// (total coverage) descending, then by With descending, then alphabetically,
// for determinism.
func Mine(papers []Paper, o Options) []Candidate {
	if o.MinBalance <= 0 {
		o = DefaultOptions()
	}
	if o.MinFreq < 1 {
		o.MinFreq = 1
	}
	if o.MaxFreqFraction <= 0 {
		o.MaxFreqFraction = 1
	}
	if o.BigramSep < 1 {
		o.BigramSep = 1
	}

	docs := make([]doc, 0, len(papers))
	for _, p := range papers {
		if p.Hash == "" {
			continue
		}
		title := Tokenize(p.Title)
		snip := Tokenize(p.Snippet)
		d := doc{
			title:     toSet(title),
			snip:      toSet(snip),
			titleList: title,
			snipList:  snip,
		}
		docs = append(docs, d)
	}

	// Candidate vocabulary: every token everywhere, plus optional bigrams.
	words := map[string]bool{}
	if o.Bigrams {
		for _, d := range docs {
			for _, b := range adjacentBigrams(d.titleList, o.BigramSep) {
				words[b] = true
			}
			for _, b := range adjacentBigrams(d.snipList, o.BigramSep) {
				words[b] = true
			}
		}
	} else {
		for _, d := range docs {
			for w := range d.title {
				words[w] = true
			}
			for w := range d.snip {
				words[w] = true
			}
		}
	}

	total := len(docs)
	cands := make([]Candidate, 0, len(words))
	for w := range words {
		with := 0
		for _, d := range docs {
			if inDoc(d, w, o) {
				with++
			}
		}
		without := total - with
		balance := balanceOf(with, without)
		if balance < o.MinBalance || with < o.MinFreq {
			continue
		}
		if total > 0 && float64(with)/float64(total) > o.MaxFreqFraction {
			continue
		}
		cands = append(cands, Candidate{
			Keyword: w,
			With:    with,
			Without: without,
			Total:   with + without,
			Balance: balance,
		})
	}

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Balance != cands[j].Balance {
			return cands[i].Balance > cands[j].Balance
		}
		if cands[i].With+cands[i].Without != cands[j].With+cands[j].Without {
			return cands[i].With+cands[i].Without > cands[j].With+cands[j].Without
		}
		if cands[i].With != cands[j].With {
			return cands[i].With > cands[j].With
		}
		return cands[i].Keyword < cands[j].Keyword
	})
	cands = dedupSubstrings(cands)
	if o.MaxCandidates > 0 && len(cands) > o.MaxCandidates {
		cands = cands[:o.MaxCandidates]
	}
	return cands
}

// dedupSubstrings drops a candidate that is a strict substring of an already
// higher-ranked candidate, keeping only the more specific term. Two near-
// identical keywords ("model"/"models") are redundant — they partition the same
// docs with a marginal difference, so including both wastes a chain/probe slot
// and, on a small biased sample, lets a generic high-frequency word (e.g.
// "large") masquerade as a splitter next to its more specific form. Keeping the
// best-ranked (higher Balance) one and dropping its substrings is deterministic
// and cheap (candidates are already bounded by MaxCandidates).
func dedupSubstrings(cands []Candidate) []Candidate {
	out := make([]Candidate, 0, len(cands))
	for i, c := range cands {
		dup := false
		for j := i + 1; j < len(cands); j++ {
			// cands[j] is strictly worse or equal; a longer, better-or-equal
			// keyword that contains c as a strict substring subsumes it.
			if len(cands[j].Keyword) > len(c.Keyword) && strings.Contains(cands[j].Keyword, c.Keyword) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, c)
		}
	}
	return out
}

// inDoc reports whether doc d contains candidate kw. Bigram candidates are
// tokens joined by a space ("neural network"); anything else is a single word.
func inDoc(d doc, kw string, o Options) bool {
	if strings.Contains(kw, " ") {
		pair := strings.Split(kw, " ")
		return hasAdjacentPair(d.titleList, pair, o.BigramSep) || hasAdjacentPair(d.snipList, pair, o.BigramSep)
	}
	return d.title[kw] || d.snip[kw]
}

func toSet(toks []string) map[string]bool {
	m := make(map[string]bool, len(toks))
	for _, t := range toks {
		m[t] = true
	}
	return m
}

// adjacentBigrams joins every pair of title tokens within sep positions of each
// other into a "word1 word2" candidate.
func adjacentBigrams(toks []string, sep int) []string {
	out := make([]string, 0, len(toks))
	for i := 0; i < len(toks); i++ {
		for j := i + 1; j < len(toks) && j-i <= sep; j++ {
			out = append(out, toks[i]+" "+toks[j])
		}
	}
	return out
}

func hasAdjacentPair(toks []string, pair []string, sep int) bool {
	if len(pair) != 2 {
		return false
	}
	for i := 0; i < len(toks); i++ {
		if toks[i] != pair[0] {
			continue
		}
		for j := i + 1; j < len(toks) && j-i <= sep; j++ {
			if toks[j] == pair[1] {
				return true
			}
		}
	}
	return false
}

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
