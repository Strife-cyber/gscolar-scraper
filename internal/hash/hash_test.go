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
