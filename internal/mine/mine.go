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
	"math"
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
	// MaxNGram enables contiguous n-gram phrase candidates of up to this many
	// tokens (2 = "federated learning", 3 = "graph neural networks"). 0/1 means
	// single words only. Unlike the legacy Bigrams window, n-gram candidates
	// are verbatim phrases: membership is contiguous containment in the raw
	// token stream, matching Scholar's quoted-phrase operator.
	MaxNGram int
	// Bigrams (legacy) enables 2-word phrase candidates — equivalent to
	// MaxNGram >= 2.
	Bigrams bool
	// BigramSep is the max position gap between the two words of a bigram.
	// Only meaningful when Bigrams is true.
	BigramSep int
	// MaxCandidates caps how many scored candidates are returned, best first.
	MaxCandidates int
	// PhraseBoost multiplies a multi-word candidate's ranking score before
	// Mine/MineIDF sort by it (1.0 = no effect, the default). A phrase that
	// clears MinBalance/MinFreq is a stronger splitter to send to Scholar than
	// a same-scoring single word: Scholar's quoted "..." phrase match is a
	// stricter, less ambiguous predicate than a bare word (which can match
	// unrelated senses or substrings across the corpus), so it is more likely
	// to reproduce the offline Balance when verified live. A boost >1 nudges
	// phrases ahead of comparably-scored single words without overriding a
	// clearly better word; it does not affect the score used for MinBalance
	// filtering, only the sort order used to pick which candidates get probed
	// or partitioned on first.
	PhraseBoost float64
}

// Candidate is one mined keyword with its offline discriminative statistics.
type Candidate struct {
	Keyword string
	With    int
	Without int
	Total   int
	Balance float64
	// IDF is the candidate's inverse document frequency over the background
	// corpus passed to MineIDF (1.0 when no background is supplied).
	IDF float64
}

// DefaultOptions returns sensible defaults for a corpus of thousands of papers.
func DefaultOptions() Options {
	return Options{
		MinBalance:      0.15,
		MinFreq:         3,
		MaxFreqFraction: 0.95,
		Bigrams:         false,
		BigramSep:       2,
		MaxNGram:        1,
		MaxCandidates:   200,
		PhraseBoost:     1.0,
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
	// discourse / transition connectives: common in academic writing
	// (abstracts and snippets specifically) purely as prose glue, not
	// topical content — "however" split evenly across a corpus is a
	// writing-style artifact, not a real splitter, and must never be
	// offered as a candidate.
	"however": true, "moreover": true, "furthermore": true, "therefore": true,
	"thus": true, "hence": true, "nevertheless": true, "nonetheless": true,
	"although": true, "though": true, "despite": true, "whereas": true,
	"meanwhile": true, "additionally": true, "consequently": true,
	"accordingly": true, "otherwise": true, "instead": true, "besides": true,
	"unless": true, "since": true, "because": true, "indeed": true,
	"specifically": true, "particularly": true, "overall": true,
	"finally": true, "firstly": true, "secondly": true, "lastly": true,
	"regardless": true, "still": true,
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
	if o.MaxNGram < 1 {
		o.MaxNGram = 1
	}
	if o.Bigrams && o.MaxNGram < 2 {
		o.MaxNGram = 2 // legacy flag: contiguous bigram phrases
	}
	if o.PhraseBoost <= 0 {
		o.PhraseBoost = 1.0
	}

	// Document frequency is accumulated in a single pass per document: every
	// candidate (word or n-gram phrase) that appears in a doc increments a
	// shared counter exactly once, via a per-doc dedup set. This replaces a
	// two-phase generate-global-vocabulary-then-rescan-every-doc-per-candidate
	// approach, which was O(|candidates| x |docs| x |doc length|) — with
	// mine_max_ngram enabled over a large corpus, the candidate vocabulary
	// itself is tens of thousands of phrases, so rescanning every doc per
	// candidate dominated startup time. This pass is O(|docs| x |doc length| x
	// MaxNGram), independent of vocabulary size.
	total := 0
	df := map[string]int{}
	seen := map[string]bool{}
	for _, p := range papers {
		if p.Hash == "" {
			continue
		}
		total++
		for k := range seen {
			delete(seen, k)
		}
		titleList := tokenizeAll(p.Title)
		snipList := tokenizeAll(p.Snippet)
		addContentTokens(titleList, seen)
		addContentTokens(snipList, seen)
		if o.MaxNGram > 1 {
			ngrams(titleList, o.MaxNGram, func(g string) { seen[g] = true })
			ngrams(snipList, o.MaxNGram, func(g string) { seen[g] = true })
		}
		for w := range seen {
			df[w]++
		}
	}

	cands := make([]Candidate, 0, len(df))
	for w, with := range df {
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
		si, sj := boostedScore(cands[i], cands[i].Balance, o.PhraseBoost), boostedScore(cands[j], cands[j].Balance, o.PhraseBoost)
		if si != sj {
			return si > sj
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

// boostedScore multiplies base (Balance for Mine, Balance*IDF for MineIDF) by
// phraseBoost when the candidate is a multi-word phrase. Boosting only the
// sort key — not Balance/IDF themselves — keeps MinBalance filtering and
// reported stats unaffected by the boost.
func boostedScore(c Candidate, base, phraseBoost float64) float64 {
	if phraseBoost != 1.0 && strings.Contains(c.Keyword, " ") {
		return base * phraseBoost
	}
	return base
}

// MineIDF is Mine with an inverse-document-frequency term computed over a
// background corpus (typically every paper already in the DB). A candidate's
// final rank is Balance × IDF, so a word must both divide the target corpus
// and be topically distinctive to rank well — generic words that appear in
// every background document (the "has"/"have"/"been" class) get a near-minimum
// IDF and sink to the bottom even when their Balance is perfect.
//
// The MaxCandidates cap is applied after the IDF re-rank so a high-IDF word
// is never cut by the plain-Balance ordering.
func MineIDF(target []Paper, background []string, o Options) []Candidate {
	uncapped := o
	uncapped.MaxCandidates = 0
	cands := Mine(target, uncapped)
	if len(background) == 0 || len(cands) == 0 {
		return capCands(cands, o.MaxCandidates)
	}

	df, rawBg := docFreq(background)
	n := float64(len(background))
	for i := range cands {
		cands[i].IDF = idfOf(cands[i].Keyword, df, n, rawBg)
	}
	sort.Slice(cands, func(i, j int) bool {
		si := boostedScore(cands[i], cands[i].Balance*cands[i].IDF, o.PhraseBoost)
		sj := boostedScore(cands[j], cands[j].Balance*cands[j].IDF, o.PhraseBoost)
		if si != sj {
			return si > sj
		}
		if cands[i].With != cands[j].With {
			return cands[i].With > cands[j].With
		}
		return cands[i].Keyword < cands[j].Keyword
	})
	return capCands(dedupSubstrings(cands), o.MaxCandidates)
}

func capCands(cands []Candidate, max int) []Candidate {
	if max > 0 && len(cands) > max {
		return cands[:max]
	}
	return cands
}

// docFreq counts, per token, how many background texts contain it, and also
// returns each text as a space-padded normalized token stream so phrase
// candidates can be counted by contiguous containment.
func docFreq(texts []string) (map[string]int, []string) {
	df := make(map[string]int)
	raw := make([]string, 0, len(texts))
	for _, t := range texts {
		for w := range toSet(Tokenize(t)) {
			df[w]++
		}
		raw = append(raw, " "+strings.Join(tokenizeAll(t), " ")+" ")
	}
	return df, raw
}

// idfOf scores a candidate: log(1 + N / (1 + df)) smoothed so a term in every
// background document bottoms out near log(2) instead of -inf. A phrase
// candidate is counted by contiguous containment over the raw background
// streams — its real document frequency, which can never exceed that of its
// rarest component word.
func idfOf(kw string, df map[string]int, nDocs float64, rawBg []string) float64 {
	if nDocs <= 0 {
		return 1
	}
	if strings.Contains(kw, " ") {
		pad := " " + kw + " "
		c := 0
		for _, t := range rawBg {
			if strings.Contains(t, pad) {
				c++
			}
		}
		return math.Log(1 + nDocs/(1+float64(c)))
	}
	return math.Log(1 + nDocs/(1+float64(df[kw])))
}

// dedupSubstrings drops a candidate that is a strict substring of an already
// higher-ranked candidate OF THE SAME TOKEN LENGTH, keeping only the more
// specific term. Two near-identical keywords ("model"/"models") are
// redundant — they partition the same docs with a marginal difference, so
// including both wastes a chain/probe slot and, on a small biased sample,
// lets a generic high-frequency word (e.g. "large") masquerade as a
// splitter next to its more specific form.
//
// The token-length guard is essential: without it, a single word that is a
// substring of some longer, higher-ranked PHRASE gets dropped globally too
// ("neural" vanishes because "graph neural networks" outranks it), which
// silently destroys the single-word half of the candidate pool. A phrase is
// never deduped against its component words — "graph neural networks"
// covers a strictly smaller, more specific set of papers than "neural"
// alone, so both are independently useful splitters and must both survive.
// Keeping the best-ranked (higher Balance) one among same-length candidates
// and dropping its substrings is deterministic and cheap (candidates are
// already bounded by MaxCandidates).
func dedupSubstrings(cands []Candidate) []Candidate {
	out := make([]Candidate, 0, len(cands))
	for i, c := range cands {
		dup := false
		cLen := len(strings.Fields(c.Keyword))
		for j := i + 1; j < len(cands); j++ {
			// cands[j] is strictly worse or equal; a same-length,
			// better-or-equal keyword that contains c as a strict substring
			// subsumes it. Different token lengths split differently and are
			// never considered duplicates of each other.
			if len(strings.Fields(cands[j].Keyword)) != cLen {
				continue
			}
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

// addContentTokens marks every content token (length >= 3, not a stopword,
// not pure digits) of toks as present in seen — the single-word half of a
// document's candidate set, matching Tokenize's filter but without allocating
// an intermediate slice.
func addContentTokens(toks []string, seen map[string]bool) {
	for _, w := range toks {
		if contentToken(w) {
			seen[w] = true
		}
	}
}

// tokenizeAll is Tokenize without the content filters: stopwords and short
// tokens are kept so a mined phrase is a verbatim contiguous run that
// Scholar's quoted-phrase operator can match. Only punctuation splits the
// stream; pure-digit tokens still drop out.
func tokenizeAll(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z')
	})
	toks := make([]string, 0, len(fields))
	for _, f := range fields {
		w := strings.ToLower(f)
		if w == "" || isDigits(w) {
			continue
		}
		toks = append(toks, w)
	}
	return toks
}

// ngrams yields every contiguous n-gram of 2..maxN tokens over toks whose
// first and last tokens are content words (non-stopword, length >= 3).
// Interior tokens may be stopwords, so "learning in the wild" is mined while
// "in the" or "of a model" never are.
func ngrams(toks []string, maxN int, add func(string)) {
	for n := 2; n <= maxN && n <= len(toks); n++ {
		for i := 0; i+n <= len(toks); i++ {
			if !contentToken(toks[i]) || !contentToken(toks[i+n-1]) {
				continue
			}
			add(strings.Join(toks[i:i+n], " "))
		}
	}
}

func contentToken(w string) bool {
	return len(w) >= 3 && !stopWords[w] && !isDigits(w)
}

func toSet(toks []string) map[string]bool {
	m := make(map[string]bool, len(toks))
	for _, t := range toks {
		m[t] = true
	}
	return m
}

func balanceOf(with, without int) float64 {
	total := with + without
	if total == 0 {
		return 0
	}
	smaller := min(without, with)
	b := float64(smaller) / (float64(total) / 2.0)
	if b > 1 {
		return 1
	}
	return b
}
