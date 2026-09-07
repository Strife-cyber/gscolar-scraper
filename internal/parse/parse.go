// Package parse extracts structured data from Google Scholar result pages.
//
// All parsing is driven by the stable CSS class names Scholar uses
// (.gs_r.gs_or result rows, h3.gs_rt titles, div.gs_a metadata, div.gs_rs
// snippets, #gs_ab_md result counts, #gs_n pagination). Where the UI text is
// localized (the "About N results" count, citation counts, "Next" link) the
// parser avoids relying on the words and instead anchors on structure or on
// the number itself.
package parse

import (
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"

	"gscolar-scraper/internal/model"
)

// Page is the parsed view of one results page.
type Page struct {
	Items     []model.ResultItem
	HasNext   bool
	Blocked   bool
	NoResults bool // Scholar's "did not match any articles" empty state
	Count     int
	HasCount  bool
}

// Parse inspects a full results-page HTML string (as produced by
// page.Content()) and returns everything the crawler needs for one page.
func Parse(htmlStr string) (Page, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlStr))
	if err != nil {
		return Page{}, err
	}
	var p Page
	p.Blocked = isBlockedDoc(doc)
	p.Count, p.HasCount = resultsCount(doc)
	p.HasNext = hasNext(doc)
	p.NoResults = noResults(doc)
	if p.NoResults {
		// A settled zero-result page carries no count text at all ("did not
		// match any articles"), so report an exact 0 instead of "unknown". This
		// lets the planner cache the count and stop re-searching empty buckets.
		p.Count, p.HasCount = 0, true
	}

	doc.Find(".gs_r.gs_or").Each(func(_ int, block *goquery.Selection) {
		if item, ok := parseItem(block); ok {
			p.Items = append(p.Items, item)
		}
	})
	return p, nil
}

// IsBlocked reports whether htmlStr is a CAPTCHA / block page.
func IsBlocked(htmlStr string) bool {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlStr))
	if err != nil {
		return false
	}
	return isBlockedDoc(doc)
}

func isBlockedDoc(doc *goquery.Document) bool {
	if doc.Find("form#captcha-form").Length() > 0 {
		return true
	}
	if doc.Find(".g-recaptcha").Length() > 0 {
		return true
	}
	if doc.Find("#g-recaptcha-response").Length() > 0 {
		return true
	}
	if doc.Find("#captcha, div#sorry").Length() > 0 {
		return true
	}
	return hasBlockedText(doc)
}

// blockedPhrases are literal text fragments Google shows on its "unusual
// traffic" / sorry pages. Matching is case-insensitive and applied to the
// document's full text content.
var blockedPhrases = []string{
	"unusual traffic",
	"our systems have detected",
	"please show you're not a robot",
	"please show you’re not a robot", // curly apostrophe variant
	"please try again later",
	"sorry",
	"/sorry/",
	"detected unusual traffic",
}

// hasBlockedText reports whether the page body contains a known Google block
// phrase. To limit false positives (e.g., a Scholar snippet mentioning the
// words), a phrase only counts when the page carries no Scholar result
// structure.
func hasBlockedText(doc *goquery.Document) bool {
	if doc.Find(".gs_r.gs_or, #gs_res_ccl").Length() > 0 {
		return false
	}
	text := strings.ToLower(doc.Text())
	for _, phrase := range blockedPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// HasNext reports whether the page has a live "Next" link. The selector
// targets the next-arrow icon inside an anchor whose href carries a "start="
// parameter, so a next page actually exists.
func HasNext(htmlStr string) bool {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlStr))
	if err != nil {
		return false
	}
	return hasNext(doc)
}

func hasNext(doc *goquery.Document) bool {
	return doc.Find(`#gs_n a[href*="start="] span.gs_ico_nav_next`).Length() > 0
}

// noResults reports whether the page is Scholar's zero-result state: the
// results-header shell (#gs_ab_md) has been rendered but no result rows and no
// pagination exist. Scholar renders that header empty on a "did not match any
// articles" page instead of showing a count. A page still streaming its first
// rows is deliberately NOT distinguished here — the caller is expected to have
// waited for the page to settle (see browser.WaitForResults).
func noResults(doc *goquery.Document) bool {
	if doc.Find(".gs_r.gs_or").Length() > 0 {
		return false
	}
	if doc.Find(`#gs_n a[href*="start="]`).Length() > 0 {
		return false
	}
	return doc.Find("#gs_ab_md").Length() > 0
}

// ---------------------------------------------------------------------------
// Result count
// ---------------------------------------------------------------------------

// resultsCount parses the "About N results" total from the #gs_ab_md element.
// Scholar localizes this text ("Page 3 sur environ 282 résultats", "Page 3 of
// about 282 results"), so we anchor on the localized "results" keyword and
// take the number immediately before it. The number group is non-greedy so it
// stops at the last digit; the separator handles both ASCII spaces and the
// U+00A0 non-breaking spaces Scholar emits.
var countRe = regexp.MustCompile(`(?i)\b(\d[\d\s\x{00A0}.,]*?)[\s\x{00A0}]+(?:results|r[ée]sultats|resultats|ergebnisse)`)

func resultsCount(doc *goquery.Document) (int, bool) {
	text := strings.TrimSpace(doc.Find("#gs_ab_md").Text())
	if text == "" {
		return 0, false
	}
	m := countRe.FindAllStringSubmatch(text, -1)
	if len(m) == 0 {
		return 0, false
	}
	last := m[len(m)-1][1] // last "… <n> results" occurrence
	n, ok := stripToInt(last)
	return n, ok
}

// stripToInt removes every non-digit from s (thousand separators such as
// spaces, dots or commas) and parses the result.
func stripToInt(s string) (int, bool) {
	var sb strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	if sb.Len() == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(sb.String())
	if err != nil {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------
// Result items
// ---------------------------------------------------------------------------

func parseItem(block *goquery.Selection) (model.ResultItem, bool) {
	// Title container: normally h3.gs_rt, but Scholar occasionally emits
	// variant markup — try a fallback chain before giving up.
	h3 := block.Find("h3.gs_rt").First()
	if h3.Length() == 0 {
		h3 = block.Find(".gs_rt").First()
	}
	if h3.Length() == 0 {
		// Last resort: any heading inside the result body.
		h3 = block.Find(".gs_ri h3").First()
	}
	if h3.Length() == 0 {
		slog.Error(fmt.Sprintf("parse: no title element in result block: %.500s", ItemHTML(block)))
		return model.ResultItem{}, false
	}

	var it model.ResultItem
	it.ScholarID = block.AttrOr("data-cid", "")
	it.RawHTML = ItemHTML(block)

	// Title: either a linked heading or a [CITATION] entry. The link may live
	// directly inside the heading or in a nested span, so search the whole
	// container before falling back to its raw text.
	var a *goquery.Selection
	h3.Find("a").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		// Skip footer links (Cited by / Save / Related) that can appear first
		// when the fallback container is the whole .gs_ri body.
		if s.ParentsFiltered(".gs_fl").Length() > 0 {
			return true
		}
		a = s
		return false
	})
	if a != nil {
		it.Title = normSpace(a.Text())
		it.URL = a.AttrOr("href", "")
	}
	if it.Title == "" {
		h3.Find(".gs_ctu").Remove()
		it.Title = normSpace(h3.Text())
		if it.URL == "" {
			it.CitationOnly = true
		}
	}

	// Authors / venue / year line.
	it.Authors, it.Venue, it.Year = parseMetaLine(normSpace(block.Find("div.gs_a").Text()))

	// Snippet (often empty).
	it.Snippet = normSpace(block.Find("div.gs_rs").Text())

	// Citation count lives in the anchor that links to the cites= cluster.
	if cites := block.Find(`a[href*="cites="]`).First(); cites.Length() > 0 {
		if n, ok := firstInt(cites.Text()); ok {
			it.Citations = n
			it.HasCitations = true
		}
	}

	if it.Title == "" {
		slog.Error(fmt.Sprintf("parse: empty title in result block: %.500s", it.RawHTML))
		return model.ResultItem{}, false
	}
	return it, true
}

// parseMetaLine splits the "authors - venue, year - publisher" line. Authors
// are everything before the first " - "; the year is the last 4-digit year in
// the remainder; the venue is what precedes that year.
func parseMetaLine(line string) (authors, venue string, year int) {
	parts := strings.Split(line, " - ")
	authors = strings.TrimSpace(parts[0])
	rest := ""
	if len(parts) > 1 {
		rest = strings.Join(parts[1:], " - ")
	}
	year = lastYear(rest)
	if year != 0 {
		ys := strconv.Itoa(year)
		if idx := strings.LastIndex(rest, ys); idx > 0 {
			venue = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(rest[:idx]), ","))
		}
	} else if rest != "" {
		// No year present ("authors - venue"): the remainder is the venue.
		venue = strings.TrimSpace(rest)
	}
	return authors, venue, year
}

var yearRe = regexp.MustCompile(`(?:19|20)\d{2}`)

// lastYear returns the last 4-digit year (1900-2099) in s, else 0.
func lastYear(s string) int {
	matches := yearRe.FindAllString(s, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		if n, err := strconv.Atoi(matches[i]); err == nil && n >= 1900 && n <= 2099 {
			return n
		}
	}
	return 0
}

// intRe extracts the first integer from localized text such as "Cité 3 fois"
// or "Cited by 3", tolerating thousand separators.
var intRe = regexp.MustCompile(`\d[\d\s\x{00A0}.,]*`)

func firstInt(s string) (int, bool) {
	m := intRe.FindString(s)
	if m == "" {
		return 0, false
	}
	return stripToInt(m)
}

// ItemHTML renders the outer HTML of a .gs_r.gs_or block, used to store the
// per-paper raw snippet in the papers table.
func ItemHTML(block *goquery.Selection) string {
	node := block.Get(0)
	if node == nil {
		return ""
	}
	var sb strings.Builder
	if err := html.Render(&sb, node); err != nil {
		return ""
	}
	return sb.String()
}

// normSpace collapses runs of whitespace (including &nbsp;) to single spaces.
func normSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
