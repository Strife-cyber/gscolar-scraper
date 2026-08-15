package parse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func readSample(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read sample %s: %v", name, err)
	}
	return string(b)
}

// TestParseNormal exercises the real results page: items, count, next link.
func TestParseNormal(t *testing.T) {
	html := readSample(t, "normal.html")
	p, err := Parse(html)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Blocked {
		t.Error("normal page should not be flagged as blocked")
	}
	if !p.HasNext {
		t.Error("normal page should have a Next link")
	}
	if !p.HasCount {
		t.Fatal("normal page should expose a result count")
	}
	if p.Count != 282 {
		t.Errorf("count = %d, want 282", p.Count)
	}
	if len(p.Items) == 0 {
		t.Fatal("expected result items, got none")
	}
	first := p.Items[0]
	if first.Title == "" {
		t.Error("first item has empty title")
	}
	if first.ScholarID == "" {
		t.Error("first item missing data-cid")
	}
	if first.Year != 2001 {
		t.Errorf("first item year = %d, want 2001", first.Year)
	}
	if first.Authors == "" {
		t.Error("first item missing authors")
	}
	if !first.CitationOnly {
		t.Errorf("first item should be a [CITATION] entry, got: %+v", first)
	}
}

// TestParseEmpty exercises the sparse/empty page: no items, no Next.
func TestParseEmpty(t *testing.T) {
	html := readSample(t, "empty.html")
	p, err := Parse(html)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Blocked {
		t.Error("empty page should not be flagged as blocked")
	}
	if p.HasNext {
		t.Error("empty page should have no Next link")
	}
	if !p.HasCount {
		t.Fatal("empty page still shows a total in the header")
	}
	if p.Count != 730 {
		t.Errorf("count = %d, want 730", p.Count)
	}
	if len(p.Items) != 0 {
		t.Errorf("expected 0 items on the sparse page, got %d", len(p.Items))
	}
}

// TestParseCaptcha verifies CAPTCHA detection against the real block page.
func TestParseCaptcha(t *testing.T) {
	html := readSample(t, "captcha.html")
	p, err := Parse(html)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !p.Blocked {
		t.Error("captcha page must be detected as blocked")
	}
	if p.HasNext {
		t.Error("captcha page should have no Next link")
	}
	if len(p.Items) != 0 {
		t.Errorf("captcha page should have no items, got %d", len(p.Items))
	}
	if !IsBlocked(html) {
		t.Error("IsBlocked should return true for the captcha sample")
	}
}

// TestParseLinkedItem covers the regular (non-citation) result layout, which
// is not present in the supplied samples, via an inline fixture.
func TestParseLinkedItem(t *testing.T) {
	const html = `<!DOCTYPE html>
<html><head></head><body>
<div id="gs_ab_md"><div class="gs_ab_mdw">About 1,230 results (0.05 sec)</div></div>
<div id="gs_res_ccl_mid">
  <div class="gs_r gs_or gs_scl" data-cid="abc123">
    <div class="gs_ri">
      <h3 class="gs_rt"><a href="https://example.org/paper">Attention Is All You Need</a></h3>
      <div class="gs_a">Ashish Vaswani, Noam Shazeer&nbsp;- Advances in Neural Information Processing Systems, 2017 - <b>Curran</b></div>
      <div class="gs_rs">The dominant sequence transduction models are based on complex recurrent networks.</div>
      <div class="gs_fl gs_flb"><a href="javascript:void(0)">Save</a> <a href="/scholar?cites=999&amp;hl=en">Cited by 12345</a></div>
    </div>
  </div>
  <div class="gs_r gs_or gs_scl" data-cid="def456">
    <div class="gs_ri">
      <h3 class="gs_rt"><a href="https://example.org/paper2">A Second Paper</a></h3>
      <div class="gs_a">J Lockwood - A Journal, 2020 - Press</div>
      <div class="gs_rs"></div>
      <div class="gs_fl gs_flb"><a href="javascript:void(0)">Save</a></div>
    </div>
  </div>
</div>
<div id="gs_n" role="navigation"><a href="/scholar?start=20&amp;q=x"><span class="gs_ico gs_ico_nav_next"></span>Next</a></div>
</body></html>`

	p, err := Parse(html)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !p.HasCount || p.Count != 1230 {
		t.Fatalf("count = %d, has = %v; want 1230", p.Count, p.HasCount)
	}
	if !p.HasNext {
		t.Error("expected Next link")
	}
	if len(p.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(p.Items))
	}

	a := p.Items[0]
	if a.Title != "Attention Is All You Need" {
		t.Errorf("title = %q", a.Title)
	}
	if a.URL != "https://example.org/paper" {
		t.Errorf("url = %q", a.URL)
	}
	if a.Authors != "Ashish Vaswani, Noam Shazeer" {
		t.Errorf("authors = %q", a.Authors)
	}
	if a.Year != 2017 {
		t.Errorf("year = %d, want 2017", a.Year)
	}
	if a.Venue != "Advances in Neural Information Processing Systems" {
		t.Errorf("venue = %q", a.Venue)
	}
	if !a.HasCitations || a.Citations != 12345 {
		t.Errorf("citations = %d (has=%v), want 12345", a.Citations, a.HasCitations)
	}
	if a.ScholarID != "abc123" {
		t.Errorf("scholar id = %q", a.ScholarID)
	}
	if a.CitationOnly {
		t.Error("linked item must not be citation-only")
	}

	b := p.Items[1]
	if b.HasCitations {
		t.Error("item without a cites= link must have no citation count")
	}
	if b.Snippet != "" {
		t.Errorf("empty snippet expected, got %q", b.Snippet)
	}
}

// TestResultsCount variants exercises the localized "About N results" parser.
func TestResultsCountVariants(t *testing.T) {
	cases := []struct {
		text string
		want int
		ok   bool
	}{
		{"Page 3 sur environ 282 résultats (0,05 s)", 282, true},
		{"Page 38 sur 730 résultats (0,16 s)", 730, true},
		{"About 1,230 results (0.05 sec)", 1230, true},
		{"Page 3 of about 282 results", 282, true},
		{"Environ 1 230 résultats", 1230, true},
		{"Ergebnisse: ungefähr 945 Ergebnisse", 945, true},
		{"", 0, false},
	}
	for _, tc := range cases {
		doc := docFor(t, `<div id="gs_ab_md"><div class="gs_ab_mdw">`+tc.text+`</div></div>`)
		got, ok := resultsCount(doc)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%q => (%d, %v), want (%d, %v)", tc.text, got, ok, tc.want, tc.ok)
		}
	}
}

func docFor(t *testing.T, fragment string) *goquery.Document {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(fragment))
	if err != nil {
		t.Fatalf("docFor: %v", err)
	}
	return doc
}

// TestFirstInt covers localized citation counts.
func TestFirstInt(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"Cité 3 fois", 3, true},
		{"Cited by 3", 3, true},
		{"Zitiert von 5", 5, true},
		{"Citado por 12", 12, true},
		{"Cité 1 234 fois", 1234, true},
		{"no number here", 0, false},
	}
	for _, tc := range cases {
		got, ok := firstInt(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%q => (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestParseMetaLine covers author/venue/year splitting.
func TestParseMetaLine(t *testing.T) {
	cases := []struct {
		in          string
		authors     string
		venue       string
		year        int
	}{
		{"J Lockwood - Proceedings of the... Innovative Applications of Artificial, 2001 - AAAI Press",
			"J Lockwood", "Proceedings of the... Innovative Applications of Artificial", 2001},
		{"A Vaswani, N Shazeer - Advances in Neural Information Processing Systems, 2017 - Curran",
			"A Vaswani, N Shazeer", "Advances in Neural Information Processing Systems", 2017},
		{"Only A Name - A Venue, 2020", "Only A Name", "A Venue", 2020},
		{"Solo Author - Venue", "Solo Author", "Venue", 0},
	}
	for _, tc := range cases {
		a, v, y := parseMetaLine(tc.in)
		if a != tc.authors || v != tc.venue || y != tc.year {
			t.Errorf("%q => (%q, %q, %d), want (%q, %q, %d)",
				tc.in, a, v, y, tc.authors, tc.venue, tc.year)
		}
	}
}
