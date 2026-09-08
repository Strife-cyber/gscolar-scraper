package mine

import "testing"

// TestBalanceFormula: a 50/50 split scores ~1.0, a 95/5 split approaches 0.
func TestBalanceFormula(t *testing.T) {
	if b := balanceOf(50, 50); b < 0.99 {
		t.Errorf("balanceOf(50,50) = %v, want ~1.0", b)
	}
	if b := balanceOf(95, 5); b < 0.05 || b > 0.15 {
		t.Errorf("balanceOf(95,5) = %v, want ~0.1", b)
	}
	if b := balanceOf(100, 0); b != 0 {
		t.Errorf("balanceOf(100,0) = %v, want 0", b)
	}
	if b := balanceOf(0, 100); b != 0 {
		t.Errorf("balanceOf(0,100) = %v, want 0", b)
	}
}

// TestMineRanksBalancedSplitterFirst: given a corpus where one word splits the
// papers most evenly, it must be the top candidate.
func TestMineRanksBalancedSplitterFirst(t *testing.T) {
	// 8 papers. "reinforcement" is on exactly 4 (balanced). "attention" is on 7
	// (lopsided). The balanced word should rank above the lopsided one.
	papers := []Paper{
		{Hash: "a", Title: "reinforcement learning for policy gradient control", Snippet: "reinforcement improves sample efficiency"},
		{Hash: "b", Title: "deep reinforcement for continuous control", Snippet: "attention is not involved"},
		{Hash: "c", Title: "reinforcement and reward shaping in games", Snippet: "multiplies the gradient"},
		{Hash: "d", Title: "policy gradient with reinforcement signal", Snippet: "convergence guarantees"},
		{Hash: "e", Title: "attention networks for sequence alignment", Snippet: "attention weights are learned"},
		{Hash: "f", Title: "self attention in transformer models", Snippet: "attention improves translation"},
		{Hash: "g", Title: "multi head attention for vision", Snippet: "patch attention works well"},
		{Hash: "h", Title: "attention over graph structures", Snippet: "correlates strongly"},
	}
	cands := Mine(papers, DefaultOptions())
	byName := map[string]Candidate{}
	for _, c := range cands {
		byName[c.Keyword] = c
	}
	rein, ok := byName["reinforcement"]
	if !ok {
		t.Fatalf("expected 'reinforcement' candidate, got %v", cands)
	}
	attn, ok := byName["attention"]
	if !ok {
		t.Fatalf("expected 'attention' candidate, got %v", cands)
	}
	// reinforcement: 4/8 -> balance 1.0; attention: 7/8 -> balance 0.25.
	if rein.Balance < attn.Balance {
		t.Errorf("reinforcement balance %.2f should rank above attention %.2f", rein.Balance, attn.Balance)
	}
	// 'learning' is a stopword? No — it is NOT in stopWords, but it appears in
	// exactly one title here (paper a) => dropped by MinFreq. Keep assertions
	// to the two words with reliable frequency.
}

// TestMineDropsDominantWord: a word on ~all papers is pruned by
// MaxFreqFraction (it cannot split the corpus).
func TestMineDropsDominantWord(t *testing.T) {
	var papers []Paper
	for i := 0; i < 20; i++ {
		prefix := "convolutional model"
		if i%2 == 1 {
			prefix = "recurrent model"
		}
		papers = append(papers, Paper{
			Hash:  "h" + string(rune('a'+i)),
			Title: prefix + " over a benchmark" + " model variants",
		})
	}
	// "model" appears in every title -> balance 0 and freq 100% -> pruned.
	cands := Mine(papers, DefaultOptions())
	for _, c := range cands {
		if c.Keyword == "model" {
			t.Errorf("dominant word 'model' should be pruned: %+v", c)
		}
	}
}

// TestMineFiltersbyMinFreq: a word on a single paper is too rare to trust.
func TestMineFiltersByMinFreq(t *testing.T) {
	papers := []Paper{
		{Hash: "a", Title: "quantum error correction with surface codes"},
		{Hash: "b", Title: "classical error correction over noisy channels"},
		{Hash: "c", Title: "linear error correction decoding"},
	}
	cands := Mine(papers, DefaultOptions())
	// "quantum" appears in a single paper (a) -> below MinFreq 3 -> pruned if
	// it has no snippet match. Others survive.
	for _, c := range cands {
		if c.Keyword == "quantum" {
			t.Errorf("single-doc word 'quantum' should be pruned by MinFreq: %+v", c)
		}
	}
}

// TestMineDeterministic: two runs over the same corpus order identically.
func TestMineDeterministic(t *testing.T) {
	papers := []Paper{
		{Hash: "a", Title: "graph neural networks for molecular property prediction"},
		{Hash: "b", Title: "graph convolutional networks for node classification"},
		{Hash: "c", Title: "temporal graph networks for traffic forecasting"},
		{Hash: "d", Title: "heterogeneous graphs in recommendation"},
	}
	o := DefaultOptions()
	o.MinFreq = 1
	o.MinBalance = 0
	o.MaxFreqFraction = 1
	c1 := Mine(papers, o)
	c2 := Mine(papers, o)
	if len(c1) != len(c2) {
		t.Fatalf("run lengths differ: %d vs %d", len(c1), len(c2))
	}
	for i := range c1 {
		if c1[i] != c2[i] {
			t.Errorf("candidate %d differs: %+v vs %+v", i, c1[i], c2[i])
		}
	}
}

// TestTokenizeHandlesVenuePhraseWord: the venue-substring word stays a token
// (the caller, split.DropDegenerateKeywords, handles venue degeneracy).
func TestTokenizeKeepsDomainWords(t *testing.T) {
	// "learning" must survive tokenization so the caller can decide to drop it.
	toks := Tokenize("International Conference on Machine Learning")
	found := false
	for _, w := range toks {
		if w == "learning" {
			found = true
		}
	}
	if !found {
		t.Error("'learning' must remain a token (venue degeneracy is the caller's job)")
	}
}

// TestDedupSubstringsKeepsMostSpecific: of two candidates where one is a strict
// substring of the other ("model" / "models"), only the higher-ranked one wins —
// the redundant substring is dropped so a split slot isn't wasted on a
// near-identical term. "model" (substring of "models") is dropped when
// "models" ranks equal or better.
func TestDedupSubstringsKeepsMostSpecific(t *testing.T) {
	cands := []Candidate{
		{Keyword: "model", Balance: 0.6, With: 60, Without: 60},
		{Keyword: "models", Balance: 0.5, With: 50, Without: 70},
		{Keyword: "large", Balance: 0.7, With: 70, Without: 50},
		{Keyword: "large language", Balance: 0.8, With: 80, Without: 40},
		{Keyword: "transformer", Balance: 0.4, With: 40, Without: 80},
	}
	out := dedupSubstrings(cands)
	got := map[string]bool{}
	for _, c := range out {
		got[c.Keyword] = true
	}
	if got["model"] {
		t.Error("'model' must be dropped (strict substring of 'models')")
	}
	if !got["models"] {
		t.Error("'models' must survive")
	}
	if !got["transformer"] {
		t.Error("'transformer' must survive (no containing keyword)")
	}
	// "large" is a substring of "large language" (higher ranked) -> dropped;
	// "large language" survives.
	if got["large"] {
		t.Error("'large' must be dropped (substring of 'large language')")
	}
	if !got["large language"] {
		t.Error("'large language' must survive")
	}
}

// TestMineSnippetBigrams: when Bigrams is enabled, adjacent token pairs from
// the snippet are mined and scored in addition to title bigrams.
func TestMineSnippetBigrams(t *testing.T) {
	papers := []Paper{
		{Hash: "a", Title: "foo", Snippet: "deep learning approach"},
		{Hash: "b", Title: "bar", Snippet: "we apply deep learning here"},
		{Hash: "c", Title: "baz", Snippet: "shallow model only"},
		{Hash: "d", Title: "qux", Snippet: "simple baseline results"},
	}
	o := DefaultOptions()
	o.Bigrams = true
	o.BigramSep = 2
	o.MinFreq = 1
	o.MinBalance = 0.001
	o.MaxFreqFraction = 1.0
	cands := Mine(papers, o)
	byName := map[string]Candidate{}
	for _, c := range cands {
		byName[c.Keyword] = c
	}
	dl, ok := byName["deep learning"]
	if !ok {
		t.Fatalf("expected 'deep learning' snippet bigram, got %v", cands)
	}
	if dl.With != 2 || dl.Without != 2 {
		t.Errorf("deep learning With/Without = %d/%d, want 2/2", dl.With, dl.Without)
	}
}

// TestMineIDFSinksUbiquitousWord: two candidates with identical Balance on the
// target corpus — "generic" appears in every background document while "rare"
// appears in almost none. The TF-IDF re-rank must push "rare" above "generic"
// even though their Balance is equal.
func TestMineIDFSinksUbiquitousWord(t *testing.T) {
	// 8 target papers: "rare" on the first 4, "generic" on the last 4 — both
	// split the target exactly 50/50 (Balance 1.0).
	var papers []Paper
	for i := 0; i < 8; i++ {
		title := "commonword "
		if i < 4 {
			title += "rare term"
		} else {
			title += "generic term"
		}
		papers = append(papers, Paper{Hash: "p" + string(rune('a'+i)), Title: title})
	}
	// Background: "generic" in every doc, "rare" in one of a hundred.
	bg := make([]string, 100)
	for i := range bg {
		bg[i] = "generic padding " + string(rune('a'+i%26))
	}
	bg[0] += " rare"
	cands := MineIDF(papers, bg, DefaultOptions())
	if len(cands) == 0 {
		t.Fatal("no candidates")
	}
	if cands[0].Keyword != "rare" {
		t.Errorf("top candidate = %q, want 'rare' (background-ubiquitous words must sink): %v", cands[0].Keyword, cands)
	}
	var rare, generic Candidate
	for _, c := range cands {
		switch c.Keyword {
		case "rare":
			rare = c
		case "generic":
			generic = c
		}
	}
	if rare.IDF <= generic.IDF {
		t.Errorf("rare IDF %.2f must exceed generic IDF %.2f", rare.IDF, generic.IDF)
	}
}

// TestMineIDFNoBackgroundFallsBackToBalance: an empty background corpus must
// degrade to the plain Balance ranking (every IDF is 1.0).
func TestMineIDFNoBackgroundFallsBackToBalance(t *testing.T) {
	papers := []Paper{
		{Hash: "a", Title: "zebra"},
		{Hash: "b", Title: "zebra"},
		{Hash: "c", Title: "zebra"},
		{Hash: "d", Title: "zebra"},
		{Hash: "e", Title: "zebra"},
		{Hash: "f", Title: "alpha"},
	}
	o := DefaultOptions()
	o.MinFreq = 1
	o.MinBalance = 0.001
	o.MaxFreqFraction = 1.0
	cands := MineIDF(papers, nil, o)
	if len(cands) == 0 || cands[0].Keyword != "zebra" {
		t.Fatalf("empty background must fall back to Balance ranking, got %v", cands)
	}
}

// TestMineCoverageTieBreaker: when two candidates have the same Balance, the
// one covering more papers (higher With) is ranked first.
func TestMineCoverageTieBreaker(t *testing.T) {
	// 6 papers: 5 titled "zebra", 1 titled "alpha". Both words have the same
	// Balance, but "zebra" appears in far more papers and must come first.
	papers := []Paper{
		{Hash: "a", Title: "zebra"},
		{Hash: "b", Title: "zebra"},
		{Hash: "c", Title: "zebra"},
		{Hash: "d", Title: "zebra"},
		{Hash: "e", Title: "zebra"},
		{Hash: "f", Title: "alpha"},
	}
	o := DefaultOptions()
	o.Bigrams = false
	o.MinFreq = 1
	o.MinBalance = 0.001
	o.MaxFreqFraction = 1.0
	cands := Mine(papers, o)
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates, got %d: %v", len(cands), cands)
	}
	if cands[0].Keyword != "zebra" {
		t.Errorf("expected 'zebra' first, got %s", cands[0].Keyword)
	}
	if cands[1].Keyword != "alpha" {
		t.Errorf("expected 'alpha' second, got %s", cands[1].Keyword)
	}
}


// TestMinePhrases: MaxNGram=3 mines contiguous phrase candidates scored like
// words — a phrase is a verbatim contiguous run in the raw text, and a
// stopword may sit inside it ("learning in the wild") but never at the edges.
func TestMinePhrases(t *testing.T) {
	papers := []Paper{
		{Hash: "a", Title: "graph neural networks for molecules"},
		{Hash: "b", Title: "graph neural networks in chemistry"},
		{Hash: "c", Title: "convolutional models for images"},
		{Hash: "d", Title: "recurrent models for text"},
	}
	o := DefaultOptions()
	o.MaxNGram = 3
	o.MinFreq = 1
	o.MinBalance = 0.001
	o.MaxFreqFraction = 1.0
	cands := Mine(papers, o)
	byName := map[string]Candidate{}
	for _, c := range cands {
		byName[c.Keyword] = c
	}
	gnn, ok := byName["graph neural networks"]
	if !ok {
		t.Fatalf("expected trigram candidate 'graph neural networks', got %v", cands)
	}
	if gnn.With != 2 || gnn.Total != 4 {
		t.Errorf("phrase stats = %+v, want With=2 of 4", gnn)
	}
	if _, ok := byName["in the"]; ok {
		t.Error("stopword-edged junk phrase 'in the' must never be mined")
	}
	if _, ok := byName["for molecules"]; ok {
		t.Error("phrase starting with a stopword ('for ...') must not be mined")
	}
}
