package hash

import "testing"

func TestPaperHashDeterministic(t *testing.T) {
	a := PaperHash("Attention Is All You Need!", 2017, "Ashish Vaswani, Noam Shazeer")
	b := PaperHash("attention is all you need", 2017, "A Vaswani, N Shazeer")
	if a != b {
		t.Errorf("normalized title+year should hash equal:\n%q\n%q", a, b)
	}
}

func TestPaperHashDifferentYear(t *testing.T) {
	a := PaperHash("Attention Is All You Need", 2017, "Ashish Vaswani")
	b := PaperHash("Attention Is All You Need", 2018, "Ashish Vaswani")
	if a == b {
		t.Error("different years must hash differently")
	}
}

func TestPaperHashDifferentTitle(t *testing.T) {
	a := PaperHash("One Paper", 2017, "Ashish Vaswani")
	b := PaperHash("Another Paper", 2017, "Ashish Vaswani")
	if a == b {
		t.Error("different titles must hash differently")
	}
}

func TestNormalizeTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  Attention   Is  ALL You Need!  ", "attention is all you need"},
		{"Model-Agnostic Meta-Learning", "model agnostic meta learning"},
		{"Deep Learning (2015)", "deep learning 2015"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := NormalizeTitle(tc.in); got != tc.want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFirstAuthorLast(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Ashish Vaswani, Noam Shazeer", "vaswani"},   // "F. Lastname" ordering
		{"Vaswani, Ashish", "vaswani"},                // "Lastname, F." ordering
		{"Vaswani, A.", "vaswani"},                    // "Lastname, A." ordering
		{"Yann LeCun", "lecun"},                       // multi-word surname-ish last word
		{"", ""},
		{"  ", ""},
	}
	for _, tc := range cases {
		got := FirstAuthorLast(tc.in)
		norm := NormalizeAuthorLast(got)
		if norm != tc.want {
			t.Errorf("FirstAuthorLast(%q) = %q (normalized %q), want %q", tc.in, got, norm, tc.want)
		}
	}
}

// TestNoAuthorsFallback: an entry with no authors still gets a stable,
// deterministic key that differs from other titles and doesn't collide with
// an authored entry.
func TestNoAuthorsFallback(t *testing.T) {
	a := PaperHash("[CITATION] Some Obscure Result", 2003, "")
	b := PaperHash("[CITATION] Some Obscure Result", 2003, "")
	if a != b {
		t.Error("author-less hash must be deterministic")
	}
	c := PaperHash("[CITATION] Another Result", 2003, "")
	if a == c {
		t.Error("different author-less titles must hash differently")
	}
	// authored entry on same title+year must not collide
	d := PaperHash("[CITATION] Some Obscure Result", 2003, "John Smith")
	if a == d {
		t.Error("author-less fallback must not collide with authored entry")
	}
}

// TestAuthorsStringFallback: unparseable but non-empty authors (e.g. only
// punctuation or a consortium name with no surname) falls back to the
// normalized authors string.
func TestAuthorsStringFallback(t *testing.T) {
	// "1234" has no letters, so no surname, but normalizes to a non-empty string.
	a := PaperHash("Some Title", 2020, "1234")
	if a == PaperHash("Some Title", 2020, "") {
		t.Error("unparseable authors should differ from empty authors")
	}
	if a != PaperHash("Some Title", 2020, "1234") {
		t.Error("unparseable authors fallback must be deterministic")
	}
}

// TestNonLatinAuthors: non-Latin author names normalize consistently and do
// not break hashing.
func TestNonLatinAuthors(t *testing.T) {
	a := PaperHash("A Paper", 2020, "李 明, 王 华")
	b := PaperHash("A Paper", 2020, "李 明, 王 华")
	if a != b {
		t.Error("non-Latin author hash must be deterministic")
	}
	if got := FirstAuthorLast("李 明, 王 华"); got == "" {
		t.Error("non-Latin first author should still parse")
	}
	c := PaperHash("A Paper", 2020, "张伟")
	if a == c {
		t.Error("different non-Latin authors must hash differently")
	}
}

// TestNormalCaseUnchanged: the primary path (title+year+first-author-last)
// still works and is stable across formatting variants.
func TestNormalCaseUnchanged(t *testing.T) {
	a := PaperHash("Attention Is All You Need!", 2017, "Ashish Vaswani, Noam Shazeer")
	b := PaperHash("attention is all you need", 2017, "Vaswani, Ashish")
	if a != b {
		t.Error("normal case should hash equal across formatting variants")
	}
}

// TestCollisionResolution: same title+year with the same first author must
// hash equal; a different first author must hash differently.
func TestCollisionResolution(t *testing.T) {
	a := PaperHash("A Boring Title", 2020, "John Smith, Alice Brown")
	b := PaperHash("A Boring Title", 2020, "John Smith, Bob White")
	c := PaperHash("A Boring Title", 2020, "Alice Brown, John Smith")
	if a != b {
		t.Error("same first author must hash equal")
	}
	if a == c {
		t.Error("different first author must hash differently")
	}
}
