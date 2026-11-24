// Package textkey turns text into keys for comparing and sorting it the way
// a reader would: without case, accents or punctuation.
package textkey

import (
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Fold lower-cases s, drops accents and punctuation ("&" is "and") and
// collapses spaces. Apostrophes and periods join what's around them:
// "Caliban's" is "calibans", "S.A." is "sa".
func Fold(s string) string {
	var b strings.Builder
	space := true
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case r == '&':
			if !space {
				b.WriteByte(' ')
			}
			b.WriteString("and ")
			space = true
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			space = false
		case r == '\'' || r == '’' || r == '.':
			// "Caliban's" = "Calibans"; "S.A." = "SA"
		default:
			if !space {
				b.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// Name is a person's name for matching: its words folded and sorted, so
// "James S.A. Corey", "james s. a. corey" and "Corey, James S. A." are the
// same.
func Name(name string) string {
	words := strings.Fields(Fold(strings.ReplaceAll(name, ".", " ")))
	slices.Sort(words)
	return strings.Join(words, " ")
}

// LooseName is a name's surname and first initial ("tolkien j" for
// "J. R. R. Tolkien" or "Tolkien, John"), for near matches; "" for a name
// of one word.
func LooseName(name string) string {
	if last, first, ok := strings.Cut(name, ","); ok {
		name = first + " " + last
	}
	words := strings.Fields(Fold(strings.ReplaceAll(name, ".", " ")))
	if len(words) < 2 {
		return ""
	}
	return words[len(words)-1] + " " + words[0][:1]
}
