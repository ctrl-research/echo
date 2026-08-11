package library

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// leadingArticles are stripped during normalisation so that "The Beatles" and
// "Beatles" reconcile to one artist. English only: this is a heuristic for
// matching, not a linguistic claim, and applying it to every language would
// mangle names like the Dutch "De Staat" into something users never type.
var leadingArticles = []string{"the ", "a ", "an "}

// Normalize reduces a name to a reconciliation key: casefolded, diacritics
// removed, punctuation dropped, whitespace collapsed, leading article stripped.
//
// This is what makes "Björk", "bjork", and "BJORK" one artist, and it is the
// same folding the search index applies, so a user's query and the stored key
// agree.
func Normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}

	if folded, err := foldDiacritics(s); err == nil {
		s = folded
	}

	// Punctuation to spaces rather than nothing: "rock/pop" must become two
	// words, not "rockpop".
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	s = strings.Join(strings.Fields(b.String()), " ")

	for _, article := range leadingArticles {
		if strings.HasPrefix(s, article) {
			// Only strip when something remains: an artist actually called
			// "The" must not normalise to the empty string.
			if rest := strings.TrimPrefix(s, article); rest != "" {
				s = rest
			}
			break
		}
	}
	return s
}

// foldDiacritics decomposes and drops combining marks: "Björk" → "Bjork".
func foldDiacritics(s string) (string, error) {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, s)
	return out, err
}
