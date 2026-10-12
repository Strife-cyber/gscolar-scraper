package browser

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"

	"gscolar-scraper/internal/notify"
	"gscolar-scraper/internal/parse"
)

// Scholar element selectors. The search box, results and pagination selectors
// are stable Scholar IDs/classes. The advanced-search dialog is opened from the
// hamburger menu; its drawer link id differs between the homepage (gs_hp_drw_adv)
// and the results page (gs_res_drw_adv), so both are handled.
const (
	// scholarBase carries no hl=; scholarURL() appends it from the configured
	// locale so Scholar cannot fall back to the IP's language (see
	// BrowserConfig.Locale for why hl=fr is actively harmful here).
	selScholarPage = "https://scholar.google.com/"
	selSearchInput = `#gs_hdr_tsi`
	selResults     = `.gs_r.gs_or`
	selNext        = `#gs_n a[href*="start="] span.gs_ico_nav_next`

	// Advanced search dialog (hamburger menu → "Advanced search" → #gs_asd).
	// "Return articles published in" is Scholar's source: operator, which is
	// what the conference queries want. The year fields live in the same dialog.
	selMenuBtn        = `#gs_hdr_mnu`
	selMenuDrawer     = `#gs_hdr_drw`
	selAdvLinkHome    = `#gs_hp_drw_adv`  // homepage drawer
	selAdvLinkResults = `#gs_res_drw_adv` // results-page drawer
	selAdvDialog      = `#gs_asd`
	selAdvQuery       = `#gs_asd_q`   // "with all the words"
	selAdvExactPhrase = `#gs_asd_epq` // "with the exact phrase"
	selAdvWithout     = `#gs_asd_eq`  // "without the words"
	selAdvPublication = `#gs_asd_pub` // "Return articles published in"
	selAdvYearFrom    = `#gs_asd_ylo`
	selAdvYearTo      = `#gs_asd_yhi`
	selAdvSubmit      = `#gs_asd_psb`

	// "include citations" toggle. Scholar renders it as an <a role="checkbox">
	// (twice: once in the sidebar, once in the results dropdown), where
	// aria-checked is the CURRENT state and the href carries the state the link
	// would switch TO. So an element whose href sets as_vis=1 AND whose
	// aria-checked is "true" is exactly "citations are included, click me to
	// exclude them". Matching on the href rather than the label keeps this
	// locale-independent — "inclure les citations" / "include citations" is
	// translated, the parameter is not. The neighbouring "include patents"
	// toggle flips as_sdt instead, so it can never match this selector.
	selCitationsIncluded = `a.gs_in_cb[href*="as_vis=1"][aria-checked="true"]`
)

// citationFilterAttempts bounds applyCitationFilter's re-find-and-click loop.
const citationFilterAttempts = 3

// emptyPollsRequired is how many consecutive 400ms polls a fully-rendered,
// row-less results page must hold before WaitForResults accepts it as "no
// results". See WaitForResults for why this is deliberately generous.
const emptyPollsRequired = 15

// hardBlockTimeoutMS bounds the wait on a block page that offers no CAPTCHA,
// when the config leaves captcha_timeout_ms at "wait forever". There is nothing
// for a human to do on such a page, so an unbounded wait just hangs the run.
const hardBlockTimeoutMS = 900000 // 15 min

// hasCaptchaChallenge reports whether the current page offers something
// solvable, as opposed to a flat refusal.
func (b *Browser) hasCaptchaChallenge() bool {
	html, err := b.page.HTML()
	if err != nil {
		return false
	}
	return parse.HasCaptchaChallenge(html)
}

// blockMessage is the operator-facing text for each kind of block. Telling the
// user to "solve it in the browser window" when Google served a page with no
// widget on it is worse than useless: it implies the run can be rescued by
// hand when only time or a different IP will clear it.
func blockMessage(solvable bool) string {
	if solvable {
		return "CAPTCHA detected. Solve it in the browser window. " +
			"The scraper will resume automatically once Scholar is reachable again."
	}
	return "Google is REFUSING requests from this network — a block page with no CAPTCHA on it. " +
		"Nothing to solve in the browser: the block is on the IP address, not on this session. " +
		"The scraper will keep checking for a while, then give up so you can retry later or from another network."
}

// The splitter emits main-box style queries such as
//
//	"International Conference on Machine Learning" AND "learning" -"neural"
//
// but the advanced-search form has no single field that accepts that syntax.
// decomposeAdvancedQuery translates such a query into the equivalent form
// fields:
//
//	source:"Venue" (or a bare quoted phrase) → "published in" (as_publication)
//	AND "a phrase" (first multi-word AND term) → "with the exact phrase"
//	                                            (as_epq) — the field the form
//	                                            actually provides for this
//	AND "word" / AND "another phrase"          → "with all the words" (as_q);
//	                                            only the FIRST AND phrase can
//	                                            go in as_epq (it is a single
//	                                            text field, so it cannot hold
//	                                            two independent phrase
//	                                            constraints); any further
//	                                            phrase term keeps its quotes
//	                                            in as_q instead
//	-"word" / -"a phrase"                     → "without the words" (as_eq),
//	                                            quoted the same way
var (
	advSourceRe = regexp.MustCompile(`source:"([^"]*)"`)
	advAndRe    = regexp.MustCompile(`\bAND\s+"([^"]*)"`)
	advNotRe    = regexp.MustCompile(`-"([^"]*)"`)
)

// decomposeAdvancedQuery returns the venue, exact-phrase, all-words and
// without-words parts of a main-box query. ok is false when nothing could be
// placed in any field.
func decomposeAdvancedQuery(q string) (venue, exactPhrase, allWords, withoutWords string, ok bool) {
	if m := advSourceRe.FindStringSubmatch(q); m != nil {
		venue = m[1]
	}
	for _, m := range advAndRe.FindAllStringSubmatch(q, -1) {
		// The first multi-word AND term is the one Scholar's dedicated
		// "exact phrase" field is for; a single word has no phrase to
		// preserve, so it goes straight to "all the words" as before.
		if exactPhrase == "" && strings.Contains(m[1], " ") {
			exactPhrase = m[1]
			continue
		}
		if allWords != "" {
			allWords += " "
		}
		allWords += quotePhrase(m[1])
	}
	for _, m := range advNotRe.FindAllStringSubmatch(q, -1) {
		if withoutWords != "" {
			withoutWords += " "
		}
		withoutWords += quotePhrase(m[1])
	}

	// Whatever is left after stripping source:/AND/-NOT is the base phrase.
	rest := advSourceRe.ReplaceAllString(q, " ")
	rest = advAndRe.ReplaceAllString(rest, " ")
	rest = advNotRe.ReplaceAllString(rest, " ")
	rest = strings.TrimSpace(rest)
	if venue == "" && rest != "" {
		// A quoted base phrase (the conference config style) is treated as the
		// publication to honor the source: search the config expects. Bare text
		// falls back to the "with all the words" field.
		if len(rest) >= 2 && strings.HasPrefix(rest, `"`) && strings.HasSuffix(rest, `"`) {
			venue = rest[1 : len(rest)-1]
		} else if allWords == "" {
			allWords = rest
		}
	}

	return venue, exactPhrase, allWords, withoutWords, venue != "" || exactPhrase != "" || allWords != "" || withoutWords != ""
}

// quotePhrase re-quotes a multi-word term so Scholar's "all the words" /
// "without the words" boxes treat it as a phrase instead of loose terms —
// `deep learning` would otherwise match both words anywhere.
func quotePhrase(term string) string {
	if strings.Contains(term, " ") {
		return `"` + term + `"`
	}
	return term
}

// scholarURL is the Scholar entry point with the configured UI language pinned
// in the query string. hl= is the parameter Scholar echoes into every
// subsequent link, so setting it once here carries through the whole session.
func (b *Browser) scholarURL() string {
	lang := b.locale()
	if i := strings.Index(lang, "-"); i > 0 {
		lang = lang[:i]
	}
	return selScholarPage + "?hl=" + lang
}

// Search runs the full navigation recipe for one task: open Scholar, open the
// advanced-search dialog from the hamburger menu, fill the equivalent form
// fields for the query (venue → "published in", keywords → "with/without the
// words") and the year range, then submit and wait for results.
func (b *Browser) Search(query string, yearFrom, yearTo int) error {
	if err := b.page.Navigate(b.scholarURL()); err != nil {
		return err
	}
	// The Scholar homepage has no results to wait for — wait for the search box
	// (present on both the homepage and the results pages) instead.
	//
	// There is deliberately no wait on document.readyState: Scholar serves this
	// page as a stream that never finishes parsing (readyState stays 'loading'
	// even on a fully rendered, interactive page), so any readyState-based gate
	// times out. Scholar's click/typing bindings are attached synchronously as
	// the head script parses, i.e. before the search box even exists, so by the
	// time waitForSearchBox returns they are live. Real readiness is enforced by
	// the element waits below and typeInto's verify-and-retype.
	if err := b.waitForSearchBox(60 * time.Second); err != nil {
		return err
	}

	if err := b.openAdvancedSearch(); err != nil {
		return err
	}
	if err := b.fillAdvancedSearch(query, yearFrom, yearTo); err != nil {
		return err
	}
	return b.submitAdvancedSearch()
}

// openAdvancedSearch opens the hamburger menu and clicks the "Advanced search"
// drawer link, then waits for the dialog. The drawer link id differs between
// the homepage and the results page, so both are tried.
func (b *Browser) openAdvancedSearch() error {
	menu, err := b.page.Element(selMenuBtn)
	if err != nil {
		return fmt.Errorf("hamburger menu not found: %w", err)
	}
	if err := b.humanClick(menu); err != nil {
		return err
	}
	// The drawer content is in the DOM from page load but hidden (visibility/
	// transform); wait until it actually slides in (.gs_vis) before clicking a
	// link inside it, otherwise the click lands on the page behind it. Scholar
	// also records the open drawer in the URL hash (#d=gs_hdr_drw), which is an
	// independent signal that does not depend on Runtime.evaluate.
	if err := b.waitForCondition(10*time.Second, "menu drawer not visible", func() bool {
		return b.elementExists(selMenuDrawer+`.gs_vis`) || b.hasHashAnchor("gs_hdr_drw")
	}); err != nil {
		return err
	}
	// The .gs_vis class is applied at the start of the slide-in transition; the
	// drawer needs ~150-300ms to reach translate(0,0). Clicking the link before
	// that would read its quad mid-transition (still near translate(-100%),
	// centre off-screen at x≈-114) and the click would land nowhere.
	if err := b.waitForSettled(selMenuDrawer, 5*time.Second, "menu drawer did not finish opening"); err != nil {
		return err
	}

	sel := selAdvLinkHome
	if !b.elementExists(sel) {
		sel = selAdvLinkResults
	}
	link, err := b.page.Element(sel)
	if err != nil {
		return fmt.Errorf("advanced-search link not found: %w", err)
	}
	if err := b.humanClick(link); err != nil {
		return err
	}

	if err := b.waitForCondition(10*time.Second, "advanced-search dialog not visible", func() bool {
		return b.elementExists(selAdvDialog+`.gs_vis`) || b.hasHashAnchor("gs_asd")
	}); err != nil {
		return err
	}
	// The dialog appears instantly on desktop but scales in over ~218ms on
	// touch; wait for it to settle so the Search button is clickable at its
	// final position later.
	if err := b.waitForSettled(selAdvDialog, 5*time.Second, "advanced-search dialog did not finish opening"); err != nil {
		return err
	}
	b.sleepRange(600, 1200) // let the dialog settle before typing
	return nil
}

// fillAdvancedSearch decomposes the task query into the advanced form's fields
// and types each one in with per-character delays, then sets the year range.
func (b *Browser) fillAdvancedSearch(query string, yearFrom, yearTo int) error {
	venue, exactPhrase, allWords, withoutWords, ok := decomposeAdvancedQuery(query)
	if !ok {
		return fmt.Errorf("cannot translate query %q into advanced-search fields", query)
	}
	if venue != "" {
		if err := b.typeInto(selAdvPublication, venue); err != nil {
			return err
		}
	}
	if exactPhrase != "" {
		if err := b.typeInto(selAdvExactPhrase, exactPhrase); err != nil {
			return err
		}
	}
	if allWords != "" {
		if err := b.typeInto(selAdvQuery, allWords); err != nil {
			return err
		}
	}
	if withoutWords != "" {
		if err := b.typeInto(selAdvWithout, withoutWords); err != nil {
			return err
		}
	}
	if yearFrom > 0 {
		if err := b.typeInto(selAdvYearFrom, strconv.Itoa(yearFrom)); err != nil {
			return err
		}
	}
	if yearTo > 0 {
		if err := b.typeInto(selAdvYearTo, strconv.Itoa(yearTo)); err != nil {
			return err
		}
	}
	return nil
}

// submitAdvancedSearch presses the dialog's Search button (or presses Enter as
// a fallback on Scholar versions that changed the button id) and waits for the
// results page.
func (b *Browser) submitAdvancedSearch() error {
	btn, err := b.page.Element(selAdvSubmit)
	if err == nil {
		if err := b.humanClick(btn); err != nil {
			return err
		}
	} else {
		// Scholar honors Enter in a form field as the accept action.
		if err := b.page.Keyboard.Press(input.Enter); err != nil {
			return err
		}
	}
	if err := b.WaitForResults(60 * time.Second); err != nil {
		return err
	}
	return b.applyCitationFilter()
}

// applyCitationFilter unticks Scholar's "include citations" box when the config
// asks for articles only (the default). Citation-only records — entries Scholar
// knows purely from a bibliography, with no document behind them — are counted
// in "About N results" and served as result rows, so leaving them in both
// inflates the planner's counts (a window holding ~900 real articles can read
// over the 1000 cap and trigger a keyword split that buys nothing) and writes
// non-articles into the papers table.
//
// The toggle is a link, so clicking it reloads the results with as_vis=1. That
// parameter then rides along every link Scholar renders on the new page,
// including "Next", so one click per search covers the whole pagination.
// Missing control (empty result set, block page) is not an error.
func (b *Browser) applyCitationFilter() error {
	if b.cfg.IncludeCitations {
		return nil
	}
	for attempt := 1; attempt <= citationFilterAttempts; attempt++ {
		// Idempotency guard: once the results carry as_vis=1 the citations are
		// already out, and every link Scholar renders keeps the parameter.
		if strings.Contains(b.URL(), "as_vis=1") {
			return nil
		}
		// No toggle at all: an empty result set or a block page. Not an error.
		if !b.elementExists(selCitationsIncluded) {
			return nil
		}
		// The handle must be taken fresh on every attempt. Scholar keeps
		// re-rendering its sidebar while the results stream in, and a handle
		// taken before a re-render is dead by the time humanClick calls
		// ScrollIntoView on it — CDP answers "cannot find object: … class
		// HTMLAnchorElement". That used to propagate out of Search() and lose
		// the whole count probe for that year.
		el, err := b.page.Element(selCitationsIncluded)
		if err != nil {
			b.sleepRange(400, 900)
			continue
		}
		before := b.URL()
		if err := b.humanClick(el); err != nil {
			b.sleepRange(400, 900)
			continue
		}
		for range 20 { // ~5s for the reload to register
			if u := b.URL(); u != "" && u != before {
				return b.WaitForResults(60 * time.Second)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	return fmt.Errorf("could not untick \"include citations\" after %d attempts", citationFilterAttempts)
}

// typeInto selects and types text into a form field, then reads the field's
// value back and retypes on a mismatch. Scholar's inputs are re-initialized by
// its JS shortly after they appear, which can swallow the first keystrokes of
// a fast typer; the verification makes the interaction self-healing.
func (b *Browser) typeInto(sel, text string) error {
	el, err := b.page.Element(sel)
	if err != nil {
		return fmt.Errorf("field %s not found: %w", sel, err)
	}
	var got string
	for attempt := range 3 {
		if err := el.SelectAllText(); err != nil {
			return err
		}
		if err := b.humanType(el, text); err != nil {
			return err
		}
		ro, err := el.Eval(`() => this.value`)
		if err != nil {
			return err
		}
		got = ro.Value.String()
		if got == text {
			return nil
		}
		fmt.Printf("field %s: expected %q got %q, retyping (%d/3)\n", sel, text, got, attempt+1)
	}
	return fmt.Errorf("field %s verification failed: expected %q got %q", sel, text, got)
}

// elementExists reports whether a selector currently matches in the page.
// The JS must be an arrow function, not a bare expression: go-rod's Eval wraps
// the code as `(expr).apply(this, arguments)`, so an expression throws a
// TypeError ("... .apply is not a function") and Eval always returns an error.
// page.Element is not used here because it retries until its sleeper gives up
// when the element is absent, which would block the 400ms poll loop in
// waitForCondition.
func (b *Browser) elementExists(sel string) bool {
	ro, err := b.page.Eval(fmt.Sprintf(`() => document.querySelector(%q) !== null`, sel))
	return err == nil && ro.Value.Bool()
}

// hasHashAnchor reports whether Scholar's dialog-state hash currently names id,
// i.e. the URL is of the form ...#d=<id>&t=<timestamp>. Scholar updates this
// fragment via history.pushState when it opens a drawer or dialog, so it is an
// independent open-state signal that does not depend on Runtime.evaluate (see
// elementExists) — it is read from the page info via b.URL().
func (b *Browser) hasHashAnchor(id string) bool {
	u := b.URL()
	_, after, ok := strings.Cut(u, "#d=")
	if !ok {
		return false
	}
	rest := after
	if j := strings.IndexByte(rest, '&'); j >= 0 {
		rest = rest[:j]
	}
	return rest == id
}

// ClickNext follows the results pagination "Next" link (clicked via the icon,
// not by URL editing).
//
// The click is self-verifying: the Next link's href carries a start=N offset,
// so a successful click must change the URL. If the click misses (humanClick
// scrolls the link into view first, but a transiently wrong quad or a mid-layout
// shift can still let one through), the URL stays put and the click is retried —
// a stalled pagination surfaces as an error rather than the crawl silently
// re-committing the same page until the page-cap backstop flags it.
func (b *Browser) ClickNext() error {
	// Ask whether the link exists before asking rod for it. page.Element retries
	// through rod's DefaultSleeper — BackoffSleeper(100ms, 1s, nil) — and that
	// nil is the attempt limit, so there is none: requesting an element that is
	// not on the page never returns, it just polls until the process dies. No
	// page timeout is configured here either.
	//
	// The crawl loop never hit this because it checks HasNext on the parsed HTML
	// before paginating. The resume walk does not: it clicks forward blindly to
	// reach the task's page pointer, so a pointer past the end of the results —
	// routine, since Scholar paginates beyond its own count and the pointer
	// outlives the result set — parked the whole crawl on the last page. One run
	// sat there 25 minutes for 0.6s of CPU before anyone noticed.
	//
	// elementExists is a single JS query with no retry, and returning
	// ElementNotFoundError keeps the contract the resume walk already handles:
	// "pointer past the end" means the task is done.
	if !b.elementExists(selNext) {
		return &rod.ElementNotFoundError{}
	}
	icon, err := b.page.Element(selNext)
	if err != nil {
		return fmt.Errorf("next link not found: %w", err)
	}
	link, err := icon.Parent() // the <a href="...start=..."> anchor
	if err != nil {
		return err
	}
	before := b.URL()
	for range 3 {
		if err := b.humanClick(link); err != nil {
			return err
		}
		for range 20 { // up to ~5s for the navigation to register
			if u := b.URL(); u != "" && u != before {
				return b.WaitForResults(60 * time.Second)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	return fmt.Errorf("next click did not advance the page (URL stayed %q)", before)
}

// Reload reloads the current page (used after a CAPTCHA is solved).
func (b *Browser) Reload() error {
	if err := b.page.Reload(); err != nil {
		return err
	}
	return b.WaitForResults(60 * time.Second)
}

// Content returns the full raw HTML of the current page.
func (b *Browser) Content() (string, error) {
	return b.page.HTML()
}

// URL returns the current page URL.
func (b *Browser) URL() string {
	info, err := b.page.Info()
	if err != nil {
		return ""
	}
	return info.URL
}

// IsBlocked reports whether the current page is a CAPTCHA/block page, based on
// both the DOM markers and the URL.
func (b *Browser) IsBlocked() bool {
	html, err := b.page.HTML()
	if err != nil {
		return false
	}
	if parse.IsBlocked(html) {
		return true
	}
	u := b.URL()
	return strings.Contains(u, "/sorry/") || strings.Contains(u, "captcha")
}

// WaitForResults waits until Scholar reaches a terminal state — either result
// rows are present or the page is genuinely empty — resolving CAPTCHAs and
// Google sign-in walls as they appear (each manual resolution restarts the
// deadline).
//
// The empty state must be stable across several polls before it is accepted.
// Scholar streams the page, so a results page passes through a brief "header
// shell present, rows not yet streamed" state that is indistinguishable from a
// zero-result page; requiring the state to persist (~1.6s) keeps a slow-loading
// results page from being mistaken for "did not match any articles".
func (b *Browser) WaitForResults(timeout time.Duration) error {
	emptyObs := 0
	return b.waitForCondition(timeout, "no Scholar results", func() bool {
		// 1 = result rows present.
		// 2 = no rows, no pagination, and the page is FULLY rendered.
		// 0 = still loading.
		//
		// There is deliberately no "Scholar says it found nothing" fast path.
		// The obvious candidates are not what they look like: .gs_alrt is the
		// create-an-alert widget and appears on every page, empty or not, so
		// keying on it would declare full pages empty. The wording of the real
		// message is localized. Structure is the only reliable signal here.
		//
		// The "fully rendered" test is what makes state 2 trustworthy. The
		// results header (#gs_ab_md) arrives early in Scholar's streamed
		// response, long before the rows, so "header present and no rows" was
		// true for a second or two on every normal page — and four polls
		// (~1.6s) of it was enough to declare a 900-result task empty, commit a
		// blank page and flag the task incomplete. Measured on the pages this
		// actually happened to: all of them were missing the filter toggles
		// (.gs_in_cb), which Scholar emits with the sidebar once the response
		// is complete, while all 309 correctly-crawled pages had them. So the
		// toggles are the marker that the response finished, and only then does
		// "no rows" mean "no results".
		ro, err := b.page.Eval(`() => {
			if (document.querySelectorAll('.gs_r.gs_or').length > 0) return 1;
			const md       = document.querySelector('#gs_ab_md');
			const shell    = document.querySelector('#gs_res_ccl');
			const rendered = document.querySelectorAll('.gs_in_cb').length > 0;
			const next     = document.querySelector('#gs_n a[href*="start="]');
			return (md && shell && rendered && !next) ? 2 : 0;
		}`)
		if err != nil {
			emptyObs = 0
			return false
		}
		switch ro.Value.Int() {
		case 1:
			emptyObs = 0
			return true
		case 2:
			// ~6s of a stable, fully-rendered, row-less page. Slower than the
			// old 1.6s, but this only ever delays a genuinely empty bucket;
			// getting it wrong silently drops a whole task's papers.
			emptyObs++
			return emptyObs >= emptyPollsRequired
		default:
			emptyObs = 0
			return false
		}
	})
}

// waitForSearchBox waits until Scholar's search box is present (homepage or
// results page), resolving CAPTCHAs and Google sign-in walls as they appear.
func (b *Browser) waitForSearchBox(timeout time.Duration) error {
	return b.waitForCondition(timeout, "Scholar search box not found", func() bool {
		_, err := b.page.Element(selSearchInput)
		return err == nil
	})
}

// waitForSettled waits until an element has finished its CSS transition, i.e.
// its computed transform is the identity matrix (or 'none'). Clicks on a
// mid-transition element are unreliable: el.Shape() (DOM.getContentQuads)
// returns the interpolated quad, so e.g. the hamburger drawer still translating
// in from translate(-100%,0) reports its content's centre at x≈-114 — off the
// left edge — and the mouse click lands on nothing. The arrow-function form is
// required, see elementExists.
func (b *Browser) waitForSettled(sel string, timeout time.Duration, what string) error {
	return b.waitForCondition(timeout, what, func() bool {
		ro, err := b.page.Eval(fmt.Sprintf(`() => {
			const el = document.querySelector(%q);
			if (!el) return false;
			const tr = getComputedStyle(el).transform;
			return tr === 'none' || tr === 'matrix(1, 0, 0, 1, 0, 0)';
		}`, sel))
		return err == nil && ro.Value.Bool()
	})
}

// waitForCondition polls fn until it returns true or the deadline passes. If a
// CAPTCHA or a Google sign-in wall interrupts, it blocks for the user to clear
// it (the same "solve and press Enter" flow as WaitForCaptchaResolved) and
// restarts the deadline, so user time doesn't count against the timeout.
func (b *Browser) waitForCondition(timeout time.Duration, what string, fn func() bool) error {
	deadline := time.Now().Add(timeout)
	for {
		if b.IsBlocked() {
			return ErrBlocked
		}
		if b.isLoginWall() {
			if err := b.WaitForLoginResolved(); err != nil {
				return err
			}
			deadline = time.Now().Add(timeout)
			continue
		}
		if fn() {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("%s within %v (URL=%s)", what, timeout, b.URL())
}

// isLoginWall reports whether the current page is a Google wall — Scholar
// bouncing the session to accounts.google.com to force a login, or a consent
// page — rather than a Scholar page.
func (b *Browser) isLoginWall() bool {
	u := b.URL()
	return strings.Contains(u, "accounts.google.com") ||
		strings.Contains(u, "ServiceLogin") ||
		strings.Contains(u, "/signin/") ||
		strings.Contains(u, "consent.google.com")
}

// hasVisibleScholarContent reports whether the current page is a real Scholar
// page with at least one result row, a settled empty results header, or the
// search box.
func (b *Browser) hasVisibleScholarContent() bool {
	ro, err := b.page.Eval(`() => {
		if (document.querySelectorAll('.gs_r.gs_or').length > 0) return true;
		if (document.querySelector('#gs_hdr_tsi') !== null) return true;
		const md = document.querySelector('#gs_ab_md');
		if (md) {
			return document.querySelectorAll('.gs_r.gs_or').length === 0;
		}
		return false;
	}`)
	return err == nil && ro.Value.Bool()
}

// WaitForLoginResolved polls the live page until the user has signed in and a
// real Scholar page is visible, then waits like a human returning from a break
// and reloads the page to resume.
func (b *Browser) WaitForLoginResolved() error {
	msg := "Google is showing a sign-in page. Sign in to your Google account " +
		"in the browser window. The scraper will resume automatically once " +
		"Scholar is reachable again."
	_ = notify.Captcha("Scholar Scraper", msg)
	fmt.Println("\n" + msg)

	poll := time.Duration(b.cfg.Timing.CaptchaPollIntervalMS) * time.Millisecond
	if poll <= 0 {
		poll = 10 * time.Second
	}
	timeout := b.cfg.Timing.LoginTimeoutMS
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(time.Duration(timeout) * time.Millisecond)
	}
	for {
		if !b.isLoginWall() && !b.IsBlocked() && b.hasVisibleScholarContent() {
			fmt.Println("Sign-in resolved; resuming...")
			break
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return fmt.Errorf("login wall not resolved within %d ms", timeout)
		}
		fmt.Println("Still on sign-in/block page, waiting...")
		time.Sleep(poll)
	}

	// Simulate a human returning from a break, then reload.
	b.sleepRange(b.cfg.Timing.AfterCaptchaMS[0], b.cfg.Timing.AfterCaptchaMS[1])
	return b.page.Reload()
}

// WaitForCaptchaResolved polls the live page until the CAPTCHA is gone and a
// real Scholar page is visible, then waits like a human returning from a break
// before reloading to continue.
func (b *Browser) WaitForCaptchaResolved() error {
	solvable := b.hasCaptchaChallenge()
	_ = notify.Captcha("Scholar Scraper", blockMessage(solvable))
	fmt.Println("\n" + blockMessage(solvable))

	poll := time.Duration(b.cfg.Timing.CaptchaPollIntervalMS) * time.Millisecond
	if poll <= 0 {
		poll = 10 * time.Second
	}
	// A solvable challenge may legitimately wait forever: a human is coming for
	// it. A flat refusal has nothing to wait for but time, so it always gets a
	// bound — without one the run hangs until it is killed, which is exactly
	// what a "your computer or network may be sending automated queries" page
	// did: it has no widget on it, so "solve it in the browser" was impossible
	// advice and the poll loop never terminated.
	timeout := b.cfg.Timing.CaptchaTimeoutMS
	if timeout <= 0 && !solvable {
		timeout = hardBlockTimeoutMS
	}
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(time.Duration(timeout) * time.Millisecond)
	}
	for {
		if !b.IsBlocked() && b.hasVisibleScholarContent() {
			fmt.Println("Page unblocked; resuming...")
			break
		}
		// A refusal can turn into a challenge once Google decides to offer one.
		// That makes it solvable, so lift the bound the refusal had imposed.
		if !solvable && b.hasCaptchaChallenge() {
			solvable = true
			fmt.Println(blockMessage(true))
			if b.cfg.Timing.CaptchaTimeoutMS <= 0 {
				deadline = time.Time{}
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			if solvable {
				return fmt.Errorf("CAPTCHA not resolved within %d ms", timeout)
			}
			return fmt.Errorf("still refused by Google after %d ms and no CAPTCHA was ever offered: "+
				"the block is on this IP, not on this session — retry later or from another network", timeout)
		}
		fmt.Println("Still blocked, waiting...")
		time.Sleep(poll)
	}

	// Simulate a human returning from a break, then reload and wait.
	b.sleepRange(b.cfg.Timing.AfterCaptchaMS[0], b.cfg.Timing.AfterCaptchaMS[1])
	if err := b.Reload(); err != nil && err != ErrBlocked {
		return err
	}
	return nil
}
