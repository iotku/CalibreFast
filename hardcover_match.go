package main

// Matching a Hardcover series' books (hardcover.go) against the library:
// which are here and tagged with the series, which are here but not tagged
// (or numbered differently), which aren't here at all (placeholders to
// make), and which of the series' library books Hardcover doesn't list.
// Pure: the library comes in as hcLibrary, so it is tested without one.

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/isbn"
	"golang.org/x/text/unicode/norm"
)

// hcLibBook is a library book as matching sees it.
type hcLibBook struct {
	UUID    string   `json:"uuid"`
	Title   string   `json:"title"`
	Authors []string `json:"authors"`
	Series  string   `json:"series,omitempty"` // its series, "" for none
	Index   float64  `json:"index,omitempty"`
	ISBNs   []string `json:"-"`
}

// hcLibrary is what matching needs from the library.
type hcLibrary struct {
	// inSeries: the books tagged with the series (any member of its group).
	inSeries []hcLibBook
	// byTitle: library books with a title key (hcTitleKey or
	// hcFullTitleKey) of key.
	byTitle func(key string) []hcLibBook
	// byISBN: library books with this ISBN-13.
	byISBN map[string][]hcLibBook
	// members: the series' names (its group's members), any case.
	members []string
}

// Match strengths.
const (
	hcByISBN   = "isbn"
	hcByTitle  = "title+author"
	hcBySeries = "title in series" // same title, already in the series
	hcWeak     = "title only"
)

// hcSuggestion is one line of the review.
type hcSuggestion struct {
	Entry    *hardcover.Entry `json:"entry,omitempty"` // nil for a book Hardcover doesn't list
	Book     *hcLibBook       `json:"book,omitempty"`  // nil for a missing book
	Strength string           `json:"strength,omitempty"`
	// Checked: pre-ticked in the review (a confident suggestion).
	Checked bool   `json:"checked"`
	Note    string `json:"note,omitempty"`
	// Fix: a present book whose number differs from Hardcover's position.
	Fix bool `json:"fix,omitempty"`
	// Placeholder: a missing book that already has one (nothing to do).
	Placeholder string `json:"placeholder,omitempty"`
}

// hcReview is a series' review.
type hcReview struct {
	Present  []hcSuggestion `json:"present"`  // here and tagged
	Untagged []hcSuggestion `json:"untagged"` // here, not tagged with the series
	Missing  []hcSuggestion `json:"missing"`  // not here
	Extra    []hcSuggestion `json:"extra"`    // here, tagged, not on Hardcover
	Hidden   int            `json:"hidden"`   // dismissed suggestions left out
}

// pending: suggestions waiting for a decision (the count shown on /missing).
func (r *hcReview) pending() int {
	n := len(r.Untagged)
	for _, s := range r.Missing {
		if s.Placeholder == "" {
			n++
		}
	}
	for _, s := range r.Present {
		if s.Fix {
			n++
		}
	}
	return n
}

// hcTitleKey is a title for matching: case, accents, punctuation, a
// leading article, a subtitle and a trailing "(Series #3)" don't count.
// Where titles share what comes before their colon ("Doctor Who: Shada",
// "Doctor Who: The Ark") the subtitle is the title: hcFullTitleKey.
func hcTitleKey(title string) string {
	t := hcTrimTitle(title)
	if i := strings.IndexAny(t, ":"); i > 0 {
		t = t[:i]
	}
	return hcFoldTitle(t)
}

// hcFullTitleKey is hcTitleKey with the subtitle kept.
func hcFullTitleKey(title string) string { return hcFoldTitle(hcTrimTitle(title)) }

// hcTitleKeys: the keys a title is found by: its full key, and its key
// without the subtitle unless that is common (see hcMatchSeries).
func hcTitleKeys(title string, common map[string]bool) []string {
	keys := []string{hcFullTitleKey(title)}
	if k := hcTitleKey(title); k != keys[0] && !common[k] {
		keys = append(keys, k)
	}
	return keys
}

// hcTrimTitle drops a trailing parenthesized or bracketed part:
// "(The Expanse #1)".
func hcTrimTitle(title string) string {
	t := strings.TrimSpace(title)
	for _, p := range [][2]string{{"(", ")"}, {"[", "]"}} {
		if strings.HasSuffix(t, p[1]) {
			if i := strings.LastIndex(t, p[0]); i > 0 {
				t = strings.TrimSpace(t[:i])
			}
		}
	}
	return t
}

func hcFoldTitle(t string) string {
	k := foldWords(t)
	for _, a := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(k, a) && len(k) > len(a) {
			k = k[len(a):]
			break
		}
	}
	return k
}

// hcNameKey is an author's name for matching: "James S.A. Corey",
// "james s. a. corey" and "Corey, James S. A." are the same.
func hcNameKey(name string) string {
	words := strings.Fields(foldWords(strings.ReplaceAll(name, ".", " ")))
	slices.Sort(words)
	return strings.Join(words, " ")
}

// hcSeriesKey is a series' name for comparing: case, accents, punctuation
// and a leading "The" don't count. Unlike a title, what follows a colon
// does ("Doctor Who: Target Books" isn't "Doctor Who: BBC Radio").
func hcSeriesKey(name string) string {
	k := foldWords(name)
	if strings.HasPrefix(k, "the ") && len(k) > 4 {
		k = k[4:]
	}
	return k
}

// foldWords lower-cases s, drops accents and punctuation ("&" is "and")
// and collapses spaces.
func foldWords(s string) string {
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

func hcSharesAuthor(a, b []string) bool {
	for _, x := range a {
		kx := hcNameKey(x)
		if kx == "" {
			continue
		}
		for _, y := range b {
			if hcNameKey(y) == kx {
				return true
			}
		}
	}
	return false
}

func hcInMembers(series string, members []string) bool {
	return series != "" && slices.ContainsFunc(members, func(m string) bool { return strings.EqualFold(m, series) })
}

// hcPositionText is a position as the review shows and the library stores it.
func hcPositionText(p *float64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

func hcWhole(p *float64) bool { return p != nil && *p >= 1 && *p == math.Trunc(*p) }

// hcMatchSeries matches d's books against lib. placeholders are the
// existing ones (a missing book that has one needs nothing); dismissed are
// Hardcover book ids an editor turned down.
func hcMatchSeries(d *hardcover.SeriesDetail, lib hcLibrary, placeholders []*placeholder, dismissed map[int64]bool) hcReview {
	var rev hcReview
	used := map[string]bool{} // library uuids matched to an entry
	byTitle := lib.byTitle
	if byTitle == nil {
		byTitle = func(string) []hcLibBook { return nil }
	}
	// A short title shared by two of the list's books ("Doctor Who" in
	// "Doctor Who: Shada") or two of the series' books here names none.
	common := map[string]bool{}
	seen := map[string]bool{}
	note := func(t string) {
		if k := hcTitleKey(t); k != hcFullTitleKey(t) {
			common[k] = common[k] || seen[k]
			seen[k] = true
		}
	}
	for _, e := range d.Entries {
		note(e.Title)
	}
	clear(seen)
	for _, b := range lib.inSeries {
		note(b.Title)
	}
	inSeriesByKey := map[string][]int{}
	for i, b := range lib.inSeries {
		for _, k := range hcTitleKeys(b.Title, common) {
			inSeriesByKey[k] = append(inSeriesByKey[k], i)
		}
	}

	for i := range d.Entries {
		e := &d.Entries[i]
		var keys []string
		addKeys := func(t string) {
			for _, k := range hcTitleKeys(t, common) {
				if k != "" && !slices.Contains(keys, k) {
					keys = append(keys, k)
				}
			}
		}
		addKeys(e.Title)
		if e.Subtitle != "" {
			addKeys(e.Title + ": " + e.Subtitle)
		}
		// The same book entered in another part of the series.
		for _, t := range e.AltTitles {
			addKeys(t)
		}
		var book *hcLibBook
		strength := ""
		take := func(b hcLibBook, s string) {
			cp := b
			book, strength = &cp, s
		}
		// 1. An ISBN of one of its editions; a book in the series first.
		var byISBN []hcLibBook
		for _, n := range e.ISBNs {
			for _, b := range lib.byISBN[isbn.Normalize(n)] {
				if !used[b.UUID] && !slices.ContainsFunc(byISBN, func(x hcLibBook) bool { return x.UUID == b.UUID }) {
					byISBN = append(byISBN, b)
				}
			}
		}
		slices.SortStableFunc(byISBN, func(a, b hcLibBook) int {
			return cmp.Compare(boolInt(!hcInMembers(a.Series, lib.members)), boolInt(!hcInMembers(b.Series, lib.members)))
		})
		if len(byISBN) > 0 {
			take(byISBN[0], hcByISBN)
		}
		// 2. The same title among the series' own books.
		if book == nil {
			for _, k := range keys {
				for _, j := range inSeriesByKey[k] {
					if b := lib.inSeries[j]; !used[b.UUID] {
						take(b, hcBySeries)
						break
					}
				}
				if book != nil {
					break
				}
			}
		}
		// 3. The same title in the library: by one of its authors, else weak.
		if book == nil {
			var weak *hcLibBook
			for _, k := range keys {
				for _, b := range byTitle(k) {
					if used[b.UUID] {
						continue
					}
					if hcSharesAuthor(b.Authors, e.Authors) {
						take(b, hcByTitle)
						break
					}
					if weak == nil {
						cp := b
						weak = &cp
					}
				}
				if book != nil {
					break
				}
			}
			if book == nil && weak != nil {
				book, strength = weak, hcWeak
			}
		}

		if book == nil {
			s := hcSuggestion{Entry: e, Checked: hcWhole(e.Position)}
			if ph := hcPlaceholderFor(e, lib.members, placeholders); ph != nil {
				s.Placeholder, s.Checked = ph.UUID, false
			} else if dismissed[e.BookID] {
				rev.Hidden++
				continue
			}
			switch {
			case e.Position == nil:
				s.Note = "not numbered on Hardcover"
			case !hcWhole(e.Position):
				s.Note = "a novella or extra (" + hcPositionText(e.Position) + ")"
			}
			rev.Missing = append(rev.Missing, s)
			continue
		}
		used[book.UUID] = true
		s := hcSuggestion{Entry: e, Book: book, Strength: strength}
		if hcInMembers(book.Series, lib.members) {
			if e.Position != nil && *e.Position != book.Index {
				if dismissed[e.BookID] {
					s.Note = "numbered " + strconv.FormatFloat(book.Index, 'f', -1, 64) + " here"
				} else {
					s.Fix = true
					s.Note = "numbered " + strconv.FormatFloat(book.Index, 'f', -1, 64) + " here, " + hcPositionText(e.Position) + " on Hardcover"
				}
			}
			rev.Present = append(rev.Present, s)
			continue
		}
		if dismissed[e.BookID] {
			rev.Hidden++
			continue
		}
		if book.Series != "" {
			s.Note = "in " + book.Series + " #" + strconv.FormatFloat(book.Index, 'f', -1, 64)
		}
		s.Checked = strength != hcWeak && book.Series == "" && e.Position != nil
		rev.Untagged = append(rev.Untagged, s)
	}
	for _, b := range lib.inSeries {
		if !used[b.UUID] {
			cp := b
			rev.Extra = append(rev.Extra, hcSuggestion{Book: &cp})
		}
	}
	return rev
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// hcPlaceholderFor: a placeholder that already stands for e: made from it
// (its Hardcover id), in the series with its number, or with its title by
// one of its authors.
func hcPlaceholderFor(e *hardcover.Entry, members []string, phs []*placeholder) *placeholder {
	id := strconv.FormatInt(e.BookID, 10)
	key := hcFullTitleKey(e.Title)
	for _, p := range phs {
		for _, x := range p.Meta.Identifiers {
			if strings.EqualFold(x.Scheme, hcIdentifierScheme) && x.Value == id {
				return p
			}
		}
		if e.Position != nil && hcInMembers(p.Meta.Series, members) && p.index() == *e.Position {
			return p
		}
		if hcFullTitleKey(p.Meta.Title) == key && hcSharesAuthor(p.Meta.Creators, e.Authors) {
			return p
		}
	}
	return nil
}

// hcIdentifierScheme names a Hardcover book id in a placeholder's identifiers.
const hcIdentifierScheme = "hardcover-id"
