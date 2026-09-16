// Package crawl orchestrates the plan → crawl → replan lifecycle.
//
// PlanConference turns one conference's broad query into a set of disjoint
// leaf tasks (year ranges, then sequential keyword subtraction), loading each
// leaf's "About N results" count through the browser and caching it in the DB
// so re-planning never re-queries Scholar. CrawlConference then walks every
// pending task page by page, committing each page atomically (raw HTML +
// dedup'd papers + task pointer) so a crash or a CAPTCHA break mid-task
// resumes exactly where it stopped.
package crawl

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"

	"gscolar-scraper/internal/browser"
	"gscolar-scraper/internal/config"
	"gscolar-scraper/internal/db"
	"gscolar-scraper/internal/hash"
	"gscolar-scraper/internal/mine"
	"gscolar-scraper/internal/model"
	"gscolar-scraper/internal/parse"
	"gscolar-scraper/internal/partition"
	"gscolar-scraper/internal/split"
)

// sparseRetries is how many consecutive empty-but-paginated pages a task may
// show before it is flagged needs_split (Scholar occasionally "vanishes" after
// page ~6-7; we retry a few times before treating it as a TODO).
const sparseRetries = 3

// Browserer is the slice of the browser package the crawler needs, so the
// crawl logic can be tested offline against recorded HTML with a fake.
type Browserer interface {
	Search(query string, yearFrom, yearTo int) error
	Content() (string, error)
	ClickNext() error
	IsBlocked() bool
	WaitForCaptchaResolved() error
	IncreaseThrottle()
	PauseBetweenPages()
	PauseBetweenSearches()
	PausePlanning()
	MaybeReadingScroll() error
}

// Crawler ties the config, database and browser together.
type Crawler struct {
	cfg *config.Config
	db  *db.DB
	br  Browserer
	log *slog.Logger

	// bgTexts is the lazily loaded TF-IDF background corpus (every paper's
	// text in the DB), fetched once per Crawler.
	bgOnce  sync.Once
	bgTexts []string

	// minedKws is the TF-IDF-ranked mined keyword list, computed at most once
	// per Crawler and persisted in keyword_rank across runs.
	kwOnce   sync.Once
	minedKws []string
}

// New builds a Crawler. The browser must already be connected.
func New(cfg *config.Config, d *db.DB, br Browserer, l *slog.Logger) *Crawler {
	return &Crawler{cfg: cfg, db: d, br: br, log: l}
}

// ---------------------------------------------------------------------------
// Planning
// ---------------------------------------------------------------------------

// PlanConference computes the leaf plan for one conference, upserts every leaf
// as a task and prunes obsolete needs_split TODOs. Returns how many leaves are
// still flagged for re-splitting (>=0 even on a fully clean plan).
//
// Splitting is decided on the scraped corpus, not by probing Scholar:
//
//   - The plan walk itself performs no keyword probes (maxKeywords=0); it only
//     pays for the window/year result counts, and an over-cap single year comes
//     out as a needs_split leaf.
//   - resolveYear then picks split keywords offline: the year's known papers
//     are partitioned by the Balance-ranked candidates (mined from the DB,
//     merged with the configured terms) into buckets of at most
//     floor(limit*headroom), and each bucket query is verified with a single
//     count probe. A bucket the offline estimate under-counted is re-split
//     online via split.ResolveOverCap; a year whose corpus is too thin to
//     trust falls back to the bounded online probe so a fresh conference can
//     still bootstrap. Only what cannot be brought under the cap stays a
//     needs_split TODO.
//
// probeBudget is a shared cap on real Scholar count searches (browser
// round-trips) for one conference plan. Cache hits are free; only an actual
// browser search costs budget. When the budget is exhausted, the count reports
// ok=false so both split.Plan and resolveYear stop probing further and the plan
// still terminates (writing whatever tasks it has) instead of exploding the
// CAPTCHA budget on a refractory year.
type probeBudget struct {
	confID int64
	left   int
}

// exhausted reports whether the per-conference probe budget is spent.
func (b *probeBudget) exhausted() bool { return b == nil || b.left <= 0 }

func (c *Crawler) PlanConference(ctx context.Context, conf db.Conference) (int, error) {
	budget := &probeBudget{confID: conf.ID, left: c.cfg.MaxProbesPerConf}
	live := func(query string, yearFrom, yearTo int) (int, bool) {
		return c.budgetCount(ctx, budget, query, yearFrom, yearTo)
	}

	// Keyword mining runs on its own goroutine: it is pure DB+CPU work and
	// overlaps the plan's slow count probes (each search carries a human-like
	// pause), so by the time an over-cap year needs split candidates the
	// TF-IDF ranking is ready.
	kwCh := make(chan []string, 1)
	go func() { kwCh <- c.planKeywords(ctx, conf) }()

	// maxKeywords=0 disables the inline online split inside split.Plan: an
	// over-cap single year is emitted as a needs_split leaf and resolved below
	// by resolveYear, which picks split keywords on the conference's known
	// papers (offline corpus partitioning) and spends live Scholar probes only
	// verifying the resulting bucket counts. Probing candidate keywords on
	// Scholar itself — one search per keyword per node — is what burned the
	// probe budget on useless terms and risks tripping the block detection.
	leaves, err := split.Plan(conf.Query, c.cfg.StartYear, currentYear(), c.cfg.MaxResults, 0, c.cfg.MaxProbes, nil, live, c.cfg.MinBalance)
	if err != nil {
		return 0, err
	}

	pl := <-kwCh

	// Resolve any single-year over-cap leaf into finer pending tasks. The probe
	// budget is shared so no single year can consume the conference's whole
	// CAPTCHA allowance.
	var resolved []split.Leaf
	for _, l := range leaves {
		if !l.NeedsSplit || l.YearFrom != l.YearTo {
			resolved = append(resolved, l)
			continue
		}
		sub, ok := c.resolveYear(ctx, conf, l.YearFrom, l.Count, pl, live, budget)
		if ok {
			resolved = append(resolved, sub...)
		} else {
			resolved = append(resolved, l)
		}
	}

	return c.emitTasks(ctx, conf, resolved)
}

// emitTasks upserts every leaf as a task (pending, or needs_split with a reason)
// and prunes obsolete tasks, returning how many leaves remained needs_split.
func (c *Crawler) emitTasks(ctx context.Context, conf db.Conference, resolved []split.Leaf) (int, error) {
	keepKeys := make([]string, 0, len(resolved))
	needsSplit := 0
	for _, l := range resolved {
		// An over-cap leaf is a re-split TODO, not crawlable work: mark it
		// needs_split at plan time so the crawler skips it (UpsertTask would
		// otherwise create it 'pending' and the crawl would burn its page budget
		// discovering it exceeds max_results).
		t := &db.Task{
			ConferenceID:  conf.ID,
			Query:         l.Query,
			YearFrom:      l.YearFrom,
			YearTo:        l.YearTo,
			Keywords:      l.Keywords,
			TotalEstimate: l.Count,
		}
		if l.NeedsSplit {
			t.Status = db.StatusNeedsSplit
		} else {
			t.Status = db.StatusPending
		}
		id, err := c.db.UpsertTask(ctx, t)
		if err != nil {
			return 0, err
		}
		if l.NeedsSplit {
			// Record why this leaf is a TODO (over max_results), self-documenting
			// the flag the same way the crawl-time page-cap backstop does.
			_ = c.db.SetTaskNeedsSplit(ctx, id,
				fmt.Sprintf("estimate %d exceeds the %d-result cap", l.Count, c.cfg.MaxResults))
		}
		keepKeys = append(keepKeys, t.Key())
		if l.NeedsSplit {
			needsSplit++
		}
	}

	if err := c.db.ReplaceConferencePlan(ctx, conf.ID, keepKeys); err != nil {
		return 0, err
	}
	c.log.Info(fmt.Sprintf("planned %s: %d tasks (%d need re-split)", conf.Name, len(resolved), needsSplit))
	return needsSplit, nil
}

// planKeywords returns the keyword candidates fed to the planner: splitter
// words mined from EVERY stored paper (not just this conference's) merged with
// the configured defaults, minus degenerate terms that are substrings of the
// venue query. Candidates are ranked by Balance × IDF over the same whole-DB
// corpus — a word must divide the corpus evenly AND be topically distinctive
// to rank high, so generic vocabulary can never masquerade as a splitter.
// Mined keywords come first so the partition sees the best splitters first.
func (c *Crawler) planKeywords(ctx context.Context, conf db.Conference) []string {
	mined := c.minedKeywords(ctx)
	seen := map[string]bool{}
	var kws []string
	for _, w := range append(mined, c.cfg.Keywords...) {
		if seen[w] || w == "" {
			continue
		}
		seen[w] = true
		kws = append(kws, w)
	}
	// The dataset vetoes a candidate before Scholar is ever searched: a term
	// covering 0 known papers or >75% of them can never produce a clean split,
	// and plural/substring variants ("network" vs "networks") probe the same
	// coverage twice.
	kws = viableKeywords(c.backgroundTexts(ctx), dedupSubstringKeywords(kws))
	return split.DropDegenerateKeywords(conf.Query, kws)
}

// viableKeywords drops candidates the corpus already proves cannot split: a
// term appearing in 0 texts has no evidence of coverage, and a term appearing
// in more than 75% of them can only return a dominant count — probing either
// on Scholar wastes a search.
func viableKeywords(texts []string, kws []string) []string {
	if len(texts) == 0 || len(kws) == 0 {
		return kws
	}
	lower := make([]string, 0, len(texts))
	for _, t := range texts {
		lower = append(lower, strings.ToLower(t))
	}
	const maxCoverage = 0.75
	out := make([]string, 0, len(kws))
	for _, kw := range kws {
		lkw := strings.ToLower(kw)
		with := 0
		for _, t := range lower {
			if strings.Contains(t, lkw) {
				with++
			}
		}
		if with == 0 || float64(with)/float64(len(lower)) > maxCoverage {
			continue
		}
		out = append(out, kw)
	}
	return out
}

// dedupSubstringKeywords drops a candidate that shares a strict substring
// relation with an earlier (better-ranked) candidate OF THE SAME TOKEN
// LENGTH: "network"/"networks" cover nearly the same papers, so the second
// probe buys nothing. A phrase is never deduped against its component words —
// "deep learning" covers a strictly smaller set than "learning".
func dedupSubstringKeywords(kws []string) []string {
	out := make([]string, 0, len(kws))
	for i, kw := range kws {
		dup := false
		for j := range i {
			a, b := kws[j], kw
			if len(strings.Fields(a)) != len(strings.Fields(b)) {
				continue // different phrase lengths split differently
			}
			if len(a) != len(b) && (strings.Contains(a, b) || strings.Contains(b, a)) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, kw)
		}
	}
	return out
}

// minedKeywords returns the TF-IDF-ranked mined keyword list. It is computed
// at most once per Crawler AND persisted in the keyword_rank table keyed by
// corpus size: a run that finds the same paper count reuses the previous
// ranking instantly instead of re-tokenizing the whole corpus at startup.
// The moment any new paper is stored, PaperCount changes and the ranking is
// re-mined once.
func (c *Crawler) minedKeywords(ctx context.Context) []string {
	c.kwOnce.Do(func() {
		n, err := c.db.PaperCount(ctx)
		// The cache key encodes the n-gram level too: a ranking mined
		// words-only must not be reused after phrase mining is enabled.
		cacheKey := n*1000 + c.cfg.MineMaxNGram
		if err == nil {
			if kws, ok, lerr := c.db.LoadKeywordRank(ctx, cacheKey); lerr == nil && ok {
				c.minedKws = kws
				return
			}
		}
		texts := c.backgroundTexts(ctx)
		opts := mine.DefaultOptions()
		opts.MinBalance = c.cfg.MinBalance
		opts.MaxNGram = c.cfg.MineMaxNGram
		if c.cfg.MineBigrams && opts.MaxNGram < 2 {
			opts.MaxNGram = 2 // legacy flag: enable bigram phrases
		}
		opts.PhraseBoost = c.cfg.MinePhraseBoost
		opts.MaxCandidates = c.cfg.MaxMinedKeywords
		mp := make([]mine.Paper, 0, len(texts))
		for i, t := range texts {
			mp = append(mp, mine.Paper{Hash: fmt.Sprintf("bg-%d", i), Title: t})
		}
		cands := mine.MineIDF(mp, texts, opts)
		kws := make([]string, 0, len(cands))
		for _, cd := range cands {
			kws = append(kws, cd.Keyword)
		}
		if err == nil {
			if serr := c.db.SaveKeywordRank(ctx, cacheKey, kws); serr != nil {
				c.log.Info(fmt.Sprintf("persist keyword ranking: %v", serr))
			}
		}
		c.minedKws = kws
	})
	return c.minedKws
}

// backgroundTexts returns every stored paper's text, fetched once per Crawler
// and reused for the IDF denominator of every mining pass. A load failure
// degrades gracefully to plain Balance ranking (empty background → IDF 1.0).
func (c *Crawler) backgroundTexts(ctx context.Context) []string {
	c.bgOnce.Do(func() {
		texts, err := c.db.AllPaperTexts(ctx)
		if err != nil {
			c.log.Info(fmt.Sprintf("load background corpus for TF-IDF: %v", err))
			return
		}
		c.bgTexts = texts
	})
	return c.bgTexts
}

// resolveYear brings a single over-cap year under the cap using offline
// partitioning of every known paper in that year (across all conferences),
// verified with one count probe per bucket and re-split online where the
// estimate under-counts. It returns
// pending leaves on success and ok=false when the corpus is too thin, too
// under-covered, the shared budget is exhausted, or some atom cannot be brought
// under the cap (caller keeps the year's needs_split TODO untouched).
//
// trueCount is the year's verified total (the over-cap leaf's Count). The
// coverage gate is the paper's Feasibility guard in operational form: offline
// partitioning is only as good as the corpus it saw. If the known papers are a
// small fraction of the true total, every bucket over-verifies and the probes
// explode (each count() is an expensive Scholar search), so instead of trusting
// a biased sample we bail to the bounded count-probe chain.
func (c *Crawler) resolveYear(ctx context.Context, conf db.Conference, year, trueCount int, keywords []string, count split.CountFunc, budget *probeBudget) ([]split.Leaf, bool) {
	// The corpus is every known paper in this year across ALL conferences, not
	// just this venue's: a keyword that divides the year's result space is
	// equally evidenced by papers crawled under other venues, and restricting
	// to this conference's rows alone would leave most years too thin to
	// partition. Buckets are still verified against this conference's query, so
	// an over-broad corpus can only over-estimate a bucket — never under-count.
	papers, err := c.db.PapersInYears(ctx, year, year)
	if err != nil {
		return nil, false
	}
	// The branch-conditional corpus veto: a candidate may only be probed on a
	// branch when the papers already satisfying that branch's predicates show
	// it can divide — the term must appear in more than 0 and at most ~75% of
	// the conditioned subset. "data" inside the AND "network" branch is
	// measured against network papers only, so a term universal inside the
	// parent's subset is never searched there.
	yearTexts := make([]string, 0, len(papers))
	for _, p := range papers {
		yearTexts = append(yearTexts, p.Title+" "+p.Snippet)
	}
	veto := corpusVeto(yearTexts, nil, nil)
	coverage := 1.0
	if trueCount > len(papers) {
		coverage = float64(len(papers)) / float64(trueCount)
	}
	// A thin or under-covered corpus cannot yield a trustworthy offline split:
	// fall back to an online probe of the year's real results. budget.exhausted()
	// is checked before any spending.
	if budget.exhausted() {
		return nil, false
	}
	trust := len(papers) >= c.cfg.MinPapersToTrust && coverage >= c.cfg.MinCoverage

	if trust {
		byHash := make(map[string]db.Paper, len(papers))
		docs := make([]partition.Doc, 0, len(papers))
		for _, p := range papers {
			byHash[p.Hash] = p
			docs = append(docs, partition.Doc{Hash: p.Hash, Year: p.Year})
		}
		text := func(d partition.Doc) string {
			p := byHash[d.Hash]
			return p.Title + " " + p.Snippet
		}
		limit := max(int(float64(c.cfg.MaxResults)*c.cfg.Headroom), 1)

		out := partition.PartitionOpts(docs, keywords, text, limit, partition.Options{
			MinBalance:  c.cfg.MinBalance,
			PhraseBoost: c.cfg.MinePhraseBoost,
		})
		// An unresolved group means every candidate in `keywords` is either
		// near-universal or near-absent inside that specific group — the
		// classic case being a chain like "AND neural networks" where generic
		// splitters ("learning", "data") are already true for almost every
		// paper in the group. Rather than giving up on the whole year, mine
		// fresh phrase candidates from JUST that group's own papers (escalating
		// past the configured mine_max_ngram once, since a wider phrase is
		// more likely to isolate a stubborn subset than another generic word)
		// and retry partitioning only the unresolved groups with the enlarged
		// candidate list. This costs zero Scholar searches — it is pure
		// offline re-mining of papers already in the DB.
		if len(out.Unresolved) > 0 {
			out = escalateUnresolved(out, docs, keywords, text, limit, byHash, c.cfg)
		}
		var sub []split.Leaf
		offlineOK := true
		for _, b := range out.Buckets {
			if budget.exhausted() {
				offlineOK = false
				break
			}
			q := b.Query(conf.Query)
			n, okC := count(q, year, year)
			if !okC || budget.exhausted() {
				offlineOK = false
				break
			}
			leaf := split.Leaf{Query: q, YearFrom: year, YearTo: year, Keywords: append(append([]string{}, b.Includes...), b.Excludes...), Count: n, HasCount: true}
			if n <= c.cfg.MaxResults {
				leaf.NeedsSplit = false
				sub = append(sub, leaf)
				continue
			}
			// Offline underestimate (a bucket that "fit" offline is really bigger):
			// re-split it online, still under the shared probe budget.
			leaf.NeedsSplit = true
			// The leaf's own predicates are the veto's base context: a
			// candidate is measured inside the bucket's conditioned corpus.
			// The rank uses the same base predicates so a chained keyword
			// inside this bucket is scored on how well it splits the
			// bucket's subset, not the whole year.
			bv := corpusVeto(yearTexts, b.Includes, b.Excludes)
			br := corpusRank(yearTexts, b.Includes, b.Excludes, c.cfg.MinePhraseBoost)
			rs, ok := split.ResolveOverCapVR(leaf, keywords, liveOnly(count, budget), bv, br, c.cfg.MaxProbes, c.cfg.MinBalance, c.cfg.MaxResults)
			if !ok || budget.exhausted() {
				offlineOK = false
				break
			}
			sub = append(sub, rs...)
		}
		// An unresolved atom floor is a feasibility ceiling: if the offline pass
		// fully resolves the year, return it. Otherwise try an online probe.
		if offlineOK && len(out.Unresolved) == 0 {
			return sub, true
		}
	}

	// Online probe: search the bare year query, parse the results, mine fresh
	// keyword candidates from that year, and try to split with those candidates.
	if budget.exhausted() {
		return nil, false
	}
	budget.left--

	query := conf.Query
	for {
		err := c.br.Search(query, year, year)
		if errors.Is(err, browser.ErrBlocked) {
			if werr := c.br.WaitForCaptchaResolved(); werr != nil {
				return nil, false
			}
			c.br.IncreaseThrottle()
			continue
		}
		if err != nil {
			return nil, false
		}
		break
	}

	html, err := c.br.Content()
	if err != nil {
		return nil, false
	}
	p, err := parse.Parse(html)
	if err != nil {
		return nil, false
	}
	if p.Blocked {
		return nil, false
	}

	mp := make([]mine.Paper, 0, len(p.Items))
	for _, it := range p.Items {
		mp = append(mp, mine.Paper{
			Hash:    hash.PaperHash(it.Title, it.Year, it.Authors),
			Title:   it.Title,
			Snippet: it.Snippet,
			Year:    it.Year,
		})
	}
	if len(mp) == 0 {
		return nil, false
	}
	// The harvest page is ~10 items, so the corpus-level MinFreq would drop
	// nearly every word. A page-level candidate only needs to show up more than
	// once to be worth a single verification probe — ResolveOverCap checks the
	// real count anyway.
	opts := mine.DefaultOptions()
	opts.MinFreq = 2
	opts.MinBalance = c.cfg.MinBalance
	opts.MaxNGram = c.cfg.MineMaxNGram
	if c.cfg.MineBigrams && opts.MaxNGram < 2 {
		opts.MaxNGram = 2 // legacy flag: enable bigram phrases
	}
	opts.PhraseBoost = c.cfg.MinePhraseBoost
	opts.MaxCandidates = c.cfg.MaxMinedKeywords
	cands := mine.MineIDF(mp, c.backgroundTexts(ctx), opts)
	fresh := make([]string, 0, len(cands))
	for _, cd := range cands {
		fresh = append(fresh, cd.Keyword)
	}
	// Fresh page terms refine the corpus+config list but are not required:
	// `keywords` already carries the conference's mined/configured splitters,
	// so an empty fresh list must not stop the split attempt — bailing here is
	// what left every big year needs_split without ever probing an AND term.
	merged := split.DropDegenerateKeywords(conf.Query, append(append([]string{}, keywords...), fresh...))
	// The flat veto over the whole year corpus prunes the obvious dead ends
	// up front; corpusVeto then re-checks each surviving term CONDITIONALLY
	// inside resolveNode, so a term that is uniform within a branch's subset
	// is never probed there either.
	merged = viableKeywords(yearTexts, dedupSubstringKeywords(merged))
	if len(merged) == 0 {
		return nil, false
	}
	rank := corpusRank(yearTexts, nil, nil, c.cfg.MinePhraseBoost)
	return split.ResolveOverCapVR(split.Leaf{
		Query:    query,
		YearFrom: year,
		YearTo:   year,
		Count:    trueCount,
		HasCount: true,
	}, merged, liveOnly(count, budget), veto, rank, c.cfg.MaxProbes, c.cfg.MinBalance, c.cfg.MaxResults)
}

// escalateUnresolved retries every Unresolved group from an initial
// partition.Partition pass with a richer, group-specific candidate list: a
// group lands in Unresolved because none of `keywords` divides it (each one
// is near-universal or near-absent among exactly those papers), so trying
// the SAME list again can never help — what the group needs is a term mined
// FROM ITS OWN papers, at a wider phrase length than the corpus-wide mining
// pass used (mine_max_ngram+1), since a longer, more specific phrase is more
// likely to isolate a stubborn subset than another generic word. This is
// pure offline re-mining of papers already in the DB — no Scholar searches.
// Buckets already resolved by the first pass are kept as-is.
func escalateUnresolved(out partition.Outcome, allDocs []partition.Doc, keywords []string, text func(partition.Doc) string, limit int, byHash map[string]db.Paper, cfg *config.Config) partition.Outcome {
	docByHash := make(map[string]partition.Doc, len(allDocs))
	for _, d := range allDocs {
		docByHash[d.Hash] = d
	}

	final := partition.Outcome{Buckets: append([]partition.Bucket{}, out.Buckets...)}
	for _, group := range out.Unresolved {
		groupDocs := make([]partition.Doc, 0, len(group.Papers))
		mp := make([]mine.Paper, 0, len(group.Papers))
		for _, h := range group.Papers {
			d, ok := docByHash[h]
			if !ok {
				continue
			}
			groupDocs = append(groupDocs, d)
			p := byHash[h]
			mp = append(mp, mine.Paper{Hash: h, Title: p.Title, Snippet: p.Snippet, Year: p.Year})
		}
		if len(groupDocs) == 0 {
			final.Unresolved = append(final.Unresolved, group)
			continue
		}

		opts := mine.DefaultOptions()
		opts.MinFreq = 2 // a group is a small slice of the corpus; the global MinFreq would drop everything
		opts.MinBalance = cfg.MinBalance
		opts.MaxNGram = cfg.MineMaxNGram + 1 // escalate one phrase length past the corpus-wide pass
		opts.PhraseBoost = cfg.MinePhraseBoost
		opts.MaxCandidates = cfg.MaxMinedKeywords
		fresh := mine.MineIDF(mp, nil, opts)

		merged := make([]string, 0, len(keywords)+len(fresh))
		seen := make(map[string]bool, len(keywords)+len(fresh))
		for _, kw := range keywords {
			if !seen[kw] {
				seen[kw] = true
				merged = append(merged, kw)
			}
		}
		for _, cd := range fresh {
			if !seen[cd.Keyword] {
				seen[cd.Keyword] = true
				merged = append(merged, cd.Keyword)
			}
		}
		if len(merged) == len(keywords) {
			// Mining found nothing new for this group — retrying would just
			// reproduce the same Unresolved result.
			final.Unresolved = append(final.Unresolved, group)
			continue
		}

		sub := partition.PartitionOpts(groupDocs, merged, text, limit, partition.Options{
			MinBalance:  cfg.MinBalance,
			PhraseBoost: cfg.MinePhraseBoost,
		})
		// The sub-partition's buckets carry only the NEW predicates relative to
		// this group; prefix them with the group's own include/exclude chain so
		// the rendered query still carries the full path from the root.
		for _, b := range sub.Buckets {
			b.Includes = append(append([]string{}, group.Includes...), b.Includes...)
			b.Excludes = append(append([]string{}, group.Excludes...), b.Excludes...)
			final.Buckets = append(final.Buckets, b)
		}
		for _, u := range sub.Unresolved {
			u.Includes = append(append([]string{}, group.Includes...), u.Includes...)
			u.Excludes = append(append([]string{}, group.Excludes...), u.Excludes...)
			final.Unresolved = append(final.Unresolved, u)
		}
	}
	return final
}

// corpusVeto builds a split.VetoFunc over a set of document texts: a keyword
// may be probed on a branch only when the texts already satisfying that
// branch's include/exclude predicates show it can divide — the term must
// appear in more than 0 and at most ~75% of the conditioned subset.
// baseInc/baseExc carry predicates already baked into the leaf's query (the
// resolver starts its own inc/exc empty). With no corpus evidence for a
// branch the probe is allowed through.
func corpusVeto(texts []string, baseInc, baseExc []string) split.VetoFunc {
	if len(texts) == 0 {
		return nil
	}
	lower := lowerAll(texts)
	lbaseInc := lowerAll(baseInc)
	lbaseExc := lowerAll(baseExc)
	return func(inc, exc []string, kw string) bool {
		subset, with := conditionedCounts(lower, lbaseInc, lbaseExc, inc, exc, kw)
		if subset == 0 {
			return true // no corpus evidence for this branch — cannot prove
		}
		return with > 0 && float64(with)/float64(subset) <= 0.75
	}
}

// corpusRank builds a split.RankFunc that reorders a node's candidates by
// their CONDITIONAL Balance over the known corpus subset satisfying the
// branch's predicates (baseInc/baseExc, then the node's own inc/exc) — best
// splitter of THIS branch first. This is the fix for a keyword that ranks
// well globally (e.g. "learning", "data") but is near-uniform inside a
// branch already conditioned on another term (e.g. "AND neural networks",
// where most papers mention both): its conditional Balance is low even
// though its global rank is high, so it must not keep monopolizing the
// limited probesPerNode budget ahead of a globally-weaker but
// branch-discriminating term (e.g. "bayesian"). The corpus scan is the same
// data corpusVeto already reads — no extra Scholar searches.
//
// A candidate the corpus proves cannot divide the branch at all (with=0 or
// with=subset) sorts last rather than being dropped, so ResolveOverCap's
// own veto (if any) or live probe still gets the final say — corpusRank only
// reorders, it never removes a candidate the caller didn't already veto.
// phraseBoost multiplies a multi-word candidate's conditional-Balance sort
// score at every branch, matching partition.Options.PhraseBoost — it never
// affects resolveNode's own minBalance gate (that still checks the real
// live-probed Balance), only which eligible candidate gets probed first.
func corpusRank(texts []string, baseInc, baseExc []string, phraseBoost float64) split.RankFunc {
	if len(texts) == 0 {
		return nil
	}
	if phraseBoost <= 0 {
		phraseBoost = 1.0
	}
	lower := lowerAll(texts)
	lbaseInc := lowerAll(baseInc)
	lbaseExc := lowerAll(baseExc)
	return func(inc, exc []string, candidates []string) []string {
		type scored struct {
			kw    string
			score float64
		}
		out := make([]scored, len(candidates))
		for i, kw := range candidates {
			subset, with := conditionedCounts(lower, lbaseInc, lbaseExc, inc, exc, kw)
			b := -1.0 // no corpus evidence: sort after every scored candidate but preserve relative order among themselves via a stable sort
			if subset > 0 {
				b = subsetBalanceOf(with, subset-with)
				if strings.Contains(kw, " ") {
					b *= phraseBoost
				}
			}
			out[i] = scored{kw: kw, score: b}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
		ranked := make([]string, len(out))
		for i, s := range out {
			ranked[i] = s.kw
		}
		return ranked
	}
}

// conditionedCounts scans lower for the subset of texts satisfying
// baseInc/baseExc (already lowercased) plus the node's own inc/exc, and
// returns that subset's size and how many of those texts also contain kw.
func conditionedCounts(lower, lbaseInc, lbaseExc, inc, exc []string, kw string) (subset, with int) {
	lkw := strings.ToLower(kw)
	for _, t := range lower {
		ok := true
		for _, w := range lbaseInc {
			if !strings.Contains(t, w) {
				ok = false
				break
			}
		}
		for _, w := range inc {
			if ok && !strings.Contains(t, strings.ToLower(w)) {
				ok = false
			}
		}
		for _, w := range lbaseExc {
			if ok && strings.Contains(t, w) {
				ok = false
			}
		}
		for _, w := range exc {
			if ok && strings.Contains(t, strings.ToLower(w)) {
				ok = false
			}
		}
		if !ok {
			continue
		}
		subset++
		if strings.Contains(t, lkw) {
			with++
		}
	}
	return subset, with
}

// subsetBalanceOf is the paper's Balance measure restricted to a branch's
// conditioned subset: 1.0 for an even split of that subset, ~0 for a
// lopsided one.
func subsetBalanceOf(with, without int) float64 {
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

func lowerAll(ws []string) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, strings.ToLower(w))
	}
	return out
}

// liveOnly wraps count so that once the shared budget is spent the wrapper
// reports ok=false, halting ResolveOverCap's recursion instead of probing more.
func liveOnly(count split.CountFunc, budget *probeBudget) split.CountFunc {
	return func(q string, yFrom, yTo int) (int, bool) {
		if budget.exhausted() {
			return 0, false
		}
		return count(q, yFrom, yTo)
	}
}

// OfflineGenTasks builds a conference's leaf plan purely from what is already
// in the DB — count_cache (no browser, no CAPTCHA) plus the mined keywords —
// and upserts the resulting tasks exactly like a live PlanConference. It is the
// "generate tasks with what is available" command: a cache miss reports ok=false,
// so that window/year falls back to a needs_split TODO rather than driving the
// browser. Use it to materialize a crawl plan from previously-seen counts and
// papers without spending any search budget.
func (c *Crawler) OfflineGenTasks(ctx context.Context, conf db.Conference) (int, error) {
	cacheOnly := func(query string, yearFrom, yearTo int) (int, bool) {
		n, ok, err := c.db.GetCachedCount(ctx, query, yearFrom, yearTo)
		if err != nil {
			return 0, false
		}
		return n, ok
	}
	// Same as PlanConference: maxKeywords=0 keeps split.Plan from probing
	// candidate keywords; every over-cap year is resolved by resolveYear on the
	// known-paper corpus below. No background goroutine needed here — nothing
	// slow runs concurrently to overlap it with.
	pl := c.planKeywords(ctx, conf)
	leaves, err := split.Plan(conf.Query, c.cfg.StartYear, currentYear(), c.cfg.MaxResults, 0, c.cfg.MaxProbes, nil, cacheOnly, c.cfg.MinBalance)
	if err != nil {
		return 0, err
	}
	// The offline budget is effectively unlimited: cache hits are free and
	// resolveYear's verify/re-split probes also read count_cache only.
	budget := &probeBudget{confID: conf.ID, left: int(^uint(0) >> 1)} // max int

	var resolved []split.Leaf
	for _, l := range leaves {
		if !l.NeedsSplit || l.YearFrom != l.YearTo {
			resolved = append(resolved, l)
			continue
		}
		sub, ok := c.resolveYear(ctx, conf, l.YearFrom, l.Count, pl, cacheOnly, budget)
		if ok {
			resolved = append(resolved, sub...)
		} else {
			resolved = append(resolved, l)
		}
	}
	return c.emitTasks(ctx, conf, resolved)
}

// budgetCount checks the cache and, only on a miss, spends one unit of the
// shared probe budget before driving an actual browser search. When the budget
// is spent it reports ok=false (no search) so planning can end gracefully.
func (c *Crawler) budgetCount(ctx context.Context, budget *probeBudget, query string, yearFrom, yearTo int) (int, bool) {
	// Checked before every candidate, cached or not: without this, a
	// cancelled ctx (Ctrl+C during -plan) only ever surfaces once a browser
	// call itself happens to fail, and the caller (resolveNode/resolveYear)
	// just treats that as "this candidate didn't count" and moves on to the
	// NEXT candidate — burning further searches after shutdown was already
	// requested instead of stopping. A cache hit is still served (looked up
	// with a fresh context: QueryRowContext on the caller's cancelled ctx
	// would fail the local SQLite read too, and there is no browser work or
	// reason to block a cheap cache read on shutdown).
	if ctx.Err() != nil {
		if n, ok, err := c.db.GetCachedCount(context.Background(), query, yearFrom, yearTo); err == nil && ok {
			return n, true
		}
		return 0, false
	}
	if budget.exhausted() {
		return 0, false
	}
	if n, ok, err := c.db.GetCachedCount(ctx, query, yearFrom, yearTo); err == nil && ok {
		return n, true
	}
	if budget.exhausted() {
		return 0, false
	}
	budget.left--
	return c.countForPlan(ctx, budget.confID, query, yearFrom, yearTo)
}
func (c *Crawler) countForPlan(ctx context.Context, confID int64, query string, yearFrom, yearTo int) (int, bool) {
	if n, ok, err := c.db.GetCachedCount(ctx, query, yearFrom, yearTo); err == nil && ok {
		return n, true
	}

	var n int
	var ok bool
	for {
		err := c.br.Search(query, yearFrom, yearTo)
		if errors.Is(err, browser.ErrBlocked) {
			if werr := c.br.WaitForCaptchaResolved(); werr != nil {
				c.log.Info(fmt.Sprintf("plan count captcha not resolved: %v", werr))
				return 0, false
			}
			c.br.IncreaseThrottle()
			continue
		}
		if err != nil {
			c.log.Info(fmt.Sprintf("plan count search failed (%q %d-%d): %v", query, yearFrom, yearTo, err))
			return 0, false
		}
		break
	}

	// Scholar streams a results page: the result rows can appear a moment before
	// the "About N results" header is populated. Reading the page in that window
	// loses the count, and since only readable counts are cached, the query would
	// be re-searched on every future plan run. Re-read the page while rows exist
	// before giving up (a settled empty page — NoResults — already reports
	// count 0, and a page with no rows and no header won't improve on re-reads).
	var p parse.Page
	var html string
	var err error
	for read := 0; ; read++ {
		html, err = c.br.Content()
		if err != nil {
			return 0, false
		}
		p, err = parse.Parse(html)
		if err != nil {
			return 0, false
		}
		if p.Blocked {
			if werr := c.br.WaitForCaptchaResolved(); werr != nil {
				return 0, false
			}
			c.br.IncreaseThrottle()
			return 0, false
		}
		if p.HasCount || len(p.Items) == 0 || read >= 2 {
			break
		}
		// Rows present but the count header is still streaming in.
		time.Sleep(1500 * time.Millisecond)
	}

	n, ok = p.Count, p.HasCount
	if ok {
		_ = c.db.SetCachedCount(ctx, query, yearFrom, yearTo, n)
		_ = c.db.SavePlanHarvestPage(ctx, query, yearFrom, yearTo, html)
	}
	// Harvest the result rows immediately rather than discarding them: each
	// planning search that lands a page is a round-trip, so the papers it
	// returns (conference-linked, sourced_from='plan', no task yet) feed the
	// next round's keyword mining. A failure here must not fail the count (the
	// count is what drives the plan); it only skips that harvest.
	if len(p.Items) > 0 {
		if _, herr := c.db.SavePapers(ctx, papersFromItemsForPlan(p.Items, confID)); herr != nil {
			c.log.Info(fmt.Sprintf("plan harvest (%q %d-%d): %v", query, yearFrom, yearTo, herr))
		}
	}
	c.br.PausePlanning()
	return n, ok
}

// ---------------------------------------------------------------------------
// Crawling
// ---------------------------------------------------------------------------

// CrawlConference processes every pending task of a conference. Failures on a
// single task (even a CAPTCHA that cannot be resolved) do not abort the
// conference; the task is marked error/needs_split and the next one runs.
func (c *Crawler) CrawlConference(ctx context.Context, confID int64) error {
	// Any task left 'running' by a crashed process must be re-queued so the
	// checkpoint (its page pointer) actually resumes it.
	if err := c.db.ResetStaleRunning(ctx, confID); err != nil {
		return err
	}
	tasks, err := c.db.PendingTasks(ctx, confID)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		c.log.Info(fmt.Sprintf("conference %d: no pending tasks", confID))
		return nil
	}
	// Reconcile each pending task's page pointer against the pages actually
	// committed in page_html, so a stale pointer is corrected before resuming.
	for i := range tasks {
		t := &tasks[i]
		committedNext, rerr := c.db.ReconcileTaskPage(ctx, t.ID)
		if rerr != nil {
			c.log.Info(fmt.Sprintf("task %d page reconciliation failed: %v", t.ID, rerr))
			continue
		}
		if committedNext > t.Page {
			if uerr := c.db.UpdateTaskPage(ctx, t.ID, committedNext); uerr != nil {
				c.log.Info(fmt.Sprintf("task %d update page failed: %v", t.ID, uerr))
				continue
			}
			t.Page = committedNext
			c.log.Info(fmt.Sprintf("task %d: reconciled resume page to %d", t.ID, committedNext))
		}
		if err := ctx.Err(); err != nil {
			c.log.Info(fmt.Sprintf("conference %d: shutdown requested, stopping before task %d", confID, t.ID))
			return err
		}
		c.br.PauseBetweenSearches()
		if err := c.crawlTask(ctx, *t); err != nil {
			c.log.Info(fmt.Sprintf("task %d failed: %v", t.ID, err))
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
		}
	}
	return nil
}

// RerunShortfall re-queues a conference's 'incomplete' and 'error' tasks back
// to 'pending' so the next CrawlConference retries them. It does not reset the
// page pointer: crawlTask's resume logic re-searches and clicks Next forward
// to wherever the task previously stopped, so a shortfall retry picks up from
// there rather than re-scraping pages already committed. Returns how many
// tasks were re-queued.
func (c *Crawler) RerunShortfall(ctx context.Context, confID int64) (int, error) {
	return c.db.RerunShortfallTasks(ctx, confID)
}

// crawlTask runs one task to completion: repeated searches (resuming at the
// task's page pointer), per-page atomic commits, CAPTCHA resolution, sparse
// page retries and the page-cap needs_split backstop.
func (c *Crawler) crawlTask(ctx context.Context, task db.Task) error {
	if err := c.db.MarkTaskRunning(ctx, task.ID); err != nil {
		return err
	}
	c.log.Info(fmt.Sprintf("task %d: %q [%d-%d] from page %d", task.ID, task.Query, task.YearFrom, task.YearTo, task.Page))

	page := max(task.Page, 1)
	if err := c.issueSearch(task); err != nil {
		_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError, err.Error())
		return err
	}

	// Resume: a fresh search always lands on page 1, so skip forward to the
	// task's page pointer. If the results actually end before that page (a
	// previous run advanced the pointer past the end), the task is done.
	for i := 1; i < page; i++ {
		err := c.br.ClickNext()
		if err != nil {
			if _, ok := errors.AsType[*rod.ElementNotFoundError](err); ok {
				_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusCompleted, "")
				c.log.Info(fmt.Sprintf("task %d: resume pointer %d past end -> completed", task.ID, page))
				return nil
			}
			_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError, err.Error())
			return err
		}
		_ = c.br.MaybeReadingScroll()
		c.br.PauseBetweenPages()
	}

	// maxPages bounds how many pages a task may consume before it is flagged for
	// re-split. Scholar currently serves 20 results per page, so derive the bound
	// from that and max_results with a little slack: a task right at its cap
	// finishes naturally, a runaway task is stopped.
	const scholarPageSize = 20
	maxPages := (c.cfg.MaxResults+scholarPageSize-1)/scholarPageSize + 2
	emptyRuns := 0
	prevKey := "" // fingerprint of the previous page's results, for stall detection

	for {
		if c.br.IsBlocked() {
			if err := c.br.WaitForCaptchaResolved(); err != nil {
				_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError, "captcha not resolved")
				return err
			}
			c.br.IncreaseThrottle()
			continue
		}

		html, err := c.br.Content()
		if err != nil {
			_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError, err.Error())
			return err
		}
		p, err := parse.Parse(html)
		if err != nil {
			_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError, err.Error())
			return err
		}

		// Stall guard: if two consecutive pages parse to the same result set, the
		// pagination is not advancing (the old off-viewport Next click silently
		// re-committed the same page until the cap flagged the task — the crawl
		// looked "stuck"). Flag it instead of committing a duplicate. Empty pages
		// return "" from itemsKey, so the sparse/end handlers below own those.
		curKey := itemsKey(p.Items)
		if curKey != "" && curKey == prevKey {
			_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError,
				"pagination stalled: identical results on consecutive pages")
			c.log.Info(fmt.Sprintf("task %d: STALL at page %d (same %d results as previous page) -> error", task.ID, page, len(p.Items)))
			return nil
		}
		prevKey = curKey

		pc := db.PageCommit{
			TaskID:     task.ID,
			PageNumber: page,
			HTML:       html,
			Papers:     papersFromItems(p.Items, task.ID),
		}

		switch {
		case len(p.Items) == 0 && p.HasNext:
			// The "vanish" case: a results page with zero rows but a live Next
			// link. Retry a few pages; if it persists, flag for re-split.
			pc.NextPage = page // do not advance the resume pointer
			if err := c.db.CommitPage(ctx, pc); err != nil {
				return err
			}
			emptyRuns++
			c.log.Info(fmt.Sprintf("task %d: sparse page %d (%d/%d)", task.ID, page, emptyRuns, sparseRetries))
			if emptyRuns >= sparseRetries {
				_ = c.db.SetTaskNeedsSplit(ctx, task.ID,
					fmt.Sprintf("empty results for %d consecutive pages from page %d", emptyRuns, page))
				c.log.Info(fmt.Sprintf("task %d: sparse results -> needs_split", task.ID))
				return nil
			}
			// fall through to paginate and try the next page

		case len(p.Items) == 0:
			// No results and no Next: natural end of the result set. Committed as
			// 'completed' here so the shortfall check below has a settled status
			// to override if Scholar under-delivered.
			pc.NextPage = page
			pc.Final = true
			if err := c.db.CommitPage(ctx, pc); err != nil {
				return err
			}
			if err := c.flagIfShortfall(ctx, task); err != nil {
				return err
			}
			c.log.Info(fmt.Sprintf("task %d: completed at page %d", task.ID, page))
			return nil

		default:
			pc.NextPage = page + 1
			if err := c.db.CommitPage(ctx, pc); err != nil {
				return err
			}
			emptyRuns = 0
			c.log.Info(fmt.Sprintf("task %d: page %d committed (%d papers)", task.ID, page, len(p.Items)))
			if !p.HasNext {
				_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusCompleted, "")
				if err := c.flagIfShortfall(ctx, task); err != nil {
					return err
				}
				c.log.Info(fmt.Sprintf("task %d: completed (no next page) at page %d", task.ID, page))
				return nil
			}
		}

		if page >= maxPages {
			_ = c.db.SetTaskNeedsSplit(ctx, task.ID,
				fmt.Sprintf("hit %d-page cap (estimate %d)", maxPages, task.TotalEstimate))
			c.log.Info(fmt.Sprintf("task %d: page cap %d reached -> needs_split", task.ID, maxPages))
			return nil
		}

		// Shutdown is only ever observed here, right after the just-scraped
		// page's CommitPage has durably saved it: the task is left 'running'
		// and CrawlConference's ResetStaleRunning flips it back to 'pending' on
		// the next start, so resume continues from the committed page pointer
		// with nothing lost. Checking mid-page (before CommitPage) would risk
		// abandoning a page Scholar already served but never persisted.
		if err := ctx.Err(); err != nil {
			c.log.Info(fmt.Sprintf("task %d: shutdown requested, stopping after page %d", task.ID, page))
			return err
		}

		// Paginate: occasional reading scroll, human pause, then click Next.
		_ = c.br.MaybeReadingScroll()
		c.br.PauseBetweenPages()
		if err := c.br.ClickNext(); err != nil {
			if errors.Is(err, browser.ErrBlocked) {
				continue // resolved at the top of the loop
			}
			_ = c.db.SetTaskStatus(ctx, task.ID, db.StatusError, err.Error())
			return err
		}
		page++
	}
}

// flagIfShortfall overrides a just-completed task's status to 'incomplete'
// when Scholar's own pagination ended the crawl far short of TotalEstimate —
// the "About N results" header count did not translate into N retrievable
// results (see the IJCAI 2022-2023 case: About 371, page 1 had no next link
// and only 17 papers). TotalEstimate <= 0 means there is nothing to compare
// against, so the task is left completed. -rerun-shortfall re-queues an
// incomplete task to pending; its page pointer is untouched so the resume
// logic re-searches and clicks forward to right where the crawl stopped.
func (c *Crawler) flagIfShortfall(ctx context.Context, task db.Task) error {
	if task.TotalEstimate <= 0 {
		return nil
	}
	got, err := c.db.CountPapersForTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if float64(got) >= float64(task.TotalEstimate)*c.cfg.MinCompletionRatio {
		return nil
	}
	reason := fmt.Sprintf("scholar under-delivered: got %d of ~%d estimated results", got, task.TotalEstimate)
	if err := c.db.SetTaskIncomplete(ctx, task.ID, reason); err != nil {
		return err
	}
	c.log.Info(fmt.Sprintf("task %d: %s -> incomplete", task.ID, reason))
	return nil
}

// issueSearch runs a task's search, resolving CAPTCHAs as they appear.
func (c *Crawler) issueSearch(task db.Task) error {
	for {
		err := c.br.Search(task.Query, task.YearFrom, task.YearTo)
		if errors.Is(err, browser.ErrBlocked) {
			if werr := c.br.WaitForCaptchaResolved(); werr != nil {
				return werr
			}
			c.br.IncreaseThrottle()
			continue
		}
		return err
	}
}

// itemsKey fingerprints a page's parsed results so the crawl loop can detect a
// pagination stall (identical results on consecutive pages). Empty pages return
// "", which deliberately never matches, leaving the zero-results cases to the
// sparse/end-of-results handlers.
func itemsKey(items []model.ResultItem) string {
	if len(items) == 0 {
		return ""
	}
	h := fnv.New64a()
	for _, it := range items {
		_, err := fmt.Fprintf(h, "%x,", hash.PaperHash(it.Title, it.Year, it.Authors))
		if err != nil {
			return ""
		}
	}
	return fmt.Sprintf("%x", h.Sum64())
}

// papersFromItems converts parsed results into db.Paper rows keyed by the
// content hash (normalized title + year + first-author last name).
func papersFromItems(items []model.ResultItem, taskID int64) []db.Paper {
	out := make([]db.Paper, 0, len(items))
	for _, it := range items {
		p := db.Paper{
			Hash:         hash.PaperHash(it.Title, it.Year, it.Authors),
			Title:        it.Title,
			Authors:      it.Authors,
			Year:         it.Year,
			Snippet:      it.Snippet,
			Citations:    it.Citations,
			HasCitations: it.HasCitations,
			SourceURL:    it.URL,
			ScholarID:    it.ScholarID,
			RawHTML:      it.RawHTML,
			TaskID:       taskID,
		}
		out = append(out, p)
	}
	return out
}

// papersFromItemsForPlan converts parsed results harvested during planning into
// db.Paper rows carrying the conference association but no task (a planning
// search is not a crawl). Such rows are sourced_from='plan' and later healed to
// 'crawl' when a real crawl saves the same hash.
func papersFromItemsForPlan(items []model.ResultItem, confID int64) []db.Paper {
	out := make([]db.Paper, 0, len(items))
	for _, it := range items {
		p := db.Paper{
			Hash:         hash.PaperHash(it.Title, it.Year, it.Authors),
			Title:        it.Title,
			Authors:      it.Authors,
			Year:         it.Year,
			Snippet:      it.Snippet,
			Citations:    it.Citations,
			HasCitations: it.HasCitations,
			SourceURL:    it.URL,
			ScholarID:    it.ScholarID,
			RawHTML:      it.RawHTML,
			ConferenceID: confID,
			SourcedFrom:  "plan",
		}
		out = append(out, p)
	}
	return out
}

func currentYear() int {
	return time.Now().Year()
}
