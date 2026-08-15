// Package hash computes the deduplication key for a paper: a SHA-256 over the
// normalized title, publication year and the first author's last name.
package hash

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode"
)

// PaperHash returns the stable unique key for a paper.
//
// The components are chosen to be:
//   - robust: the title is normalized (case, punctuation, whitespace) so
//     Scholar's formatting quirks do not break uniqueness;
//   - discriminative: (title + year) alone already separates nearly all real
//     papers; the first author's last name additionally separates the rare
//     same-title-same-year collisions.
//
// When no author can be parsed the last-name component degrades to the empty
// string rather than failing, so papers still get a usable key.
func PaperHash(title string, year int, authors string) string {
	h := sha256.New()
	h.Write([]byte(NormalizeTitle(title)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(year)))
	h.Write([]byte{0})
	h.Write([]byte(NormalizeAuthorLast(FirstAuthorLast(authors))))
	return hex.EncodeToString(h.Sum(nil))
}

// NormalizeTitle lowercases, collapses whitespace and turns punctuation into
// word separators, so that "Attention Is All You Need!", "Model-Agnostic
// Meta-Learning" and "model agnostic meta learning" all hash equal.
func NormalizeTitle(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r) || unicode.IsControl(r) || unicode.IsPunct(r) || unicode.IsSymbol(r):
			// whitespace and punctuation both act as word separators
			prevSpace = true
		default:
			if prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

// FirstAuthorLast extracts the last name of the first author from a
// comma-separated author list. It handles both "Lastname, F." and "F.
// Lastname" orderings and strips trailing initials and suffixes.
func FirstAuthorLast(authors string) string {
	first := strings.TrimSpace(authors)
	if i := strings.IndexByte(first, ','); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(first)
	if first == "" {
		return ""
	}

	fields := strings.Fields(first)
	// "F. Lastname" ordering: the last field is the surname.
	last := fields[len(fields)-1]
	if len(fields) > 1 && len([]rune(last)) <= 3 && strings.HasSuffix(last, ".") {
		// e.g. "F. Smith" -> "Smith" (single initial won't be a surname)
		return normalizeSurname(fields[len(fields)-2])
	}
	return normalizeSurname(last)
}

// normalizeSurname strips punctuation and trailing digits.
func normalizeSurname(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r):
			return unicode.ToLower(r)
		default:
			return -1
		}
	}, s)
}

// NormalizeAuthorLast lowercases and strips punctuation from a surname so the
// hash is robust to spelling/case variants.
func NormalizeAuthorLast(s string) string { return normalizeSurname(s) }
