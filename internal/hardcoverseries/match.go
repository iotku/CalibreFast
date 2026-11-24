package hardcoverseries

// Matching a Hardcover series' books against the library:
// which are here and tagged with the series, which are here but not tagged
// (or numbered differently), which aren't here at all (placeholders to
// make), and which of the series' library books Hardcover doesn't list.
// Pure: the library comes in as Library.

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/isbn"
	"github.com/iotku/CalibreFast/pkg/textkey"
)

// LibBook is a library book as matching sees it.
type LibBook struct {
	UUID    string   `json:"uuid"`
	Title   string   `json:"title"`
	Authors []string `json:"authors"`
	Series  string   `json:"series,omitempty"` // its series, "" for none
	Index   float64  `json:"index,omitempty"`
	ISBNs   []string `json:"-"`
}

// Library is what matching needs from the library.
type Library struct {
	// InSeries: the books tagged with the series (any member of its group).
	InSeries []LibBook
	// ByTitle: library books with a title key (textkey.Title or
	// textkey.FullTitle) of key.
	ByTitle func(key string) []LibBook
	// ByISBN: library books with this ISBN-13.
	ByISBN map[string][]LibBook
	// ByHCSlug, ByHCID: library books whose "hardcover" identifier is this
	// slug (lower case) or whose "hardcover-id" is this book id: an
	// editor's say-so that the book is that one, whatever its title.
	ByHCSlug map[string][]LibBook
	ByHCID   map[int64][]LibBook
	// Members: the series' names (its group's members), any case.
	Members []string
	// Refused: Hardcover book id -> library uuids an editor said aren't
	// that book ("Not a match").
	Refused map[int64]map[string]bool
	// Book: a library book by uuid, for listing the refused ones.
	Book func(uuid string) (LibBook, bool)
}

// refuses: an editor said the library book isn't e (any of its books).
func (l *Library) refuses(e *hardcover.Entry, uuid string) bool {
	if len(l.Refused) == 0 {
		return false
	}
	return slices.ContainsFunc(e.IDs(), func(id int64) bool { return l.Refused[id][uuid] })
}

// Match strengths.
const (
	MatchByID     = "hardcover id" // the book's Hardcover identifier
	MatchByISBN   = "isbn"
	MatchByTitle  = "title+author"
	MatchBySeries = "title in series" // same title, already in the series
	MatchWeak     = "title only"
)

// Suggestion is one line of the review.
type Suggestion struct {
	Entry    *hardcover.Entry `json:"entry,omitempty"` // nil for a book Hardcover doesn't list
	Book     *LibBook         `json:"book,omitempty"`  // nil for a missing book
	Strength string           `json:"strength,omitempty"`
	// Checked: pre-ticked in the review (a confident suggestion).
	Checked bool   `json:"checked"`
	Note    string `json:"note,omitempty"`
	// Fix: a present book whose number differs from Hardcover's position.
	Fix bool `json:"fix,omitempty"`
	// Refused: an editor said Book isn't Entry ("Not a match"); listed
	// with the dismissed, to undo.
	Refused bool `json:"refused,omitempty"`
	// Placeholder: a missing book that already has one (nothing to do).
	Placeholder string `json:"placeholder,omitempty"`
}

// Review is a series' review.
type Review struct {
	Present  []Suggestion `json:"present"`  // here and tagged
	Untagged []Suggestion `json:"untagged"` // here, not tagged with the series
	Missing  []Suggestion `json:"missing"`  // not here
	Extra    []Suggestion `json:"extra"`    // here, tagged, not on Hardcover
	// Dismissed: suggestions an editor turned down, left out of the rest.
	Dismissed []Suggestion `json:"dismissed"`
	// Numbered: the list's whole-numbered books (1, 2, 3…), less those
	// dismissed; Have: how many of them are here, tagged with the series.
	Numbered, Have int
}

// Complete: every whole-numbered book on the list is here and tagged.
func (r *Review) Complete() bool { return r.Numbered > 0 && r.Have == r.Numbered }

// Pending: suggestions waiting for a decision (the count shown on /missing).
func (r *Review) Pending() int {
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

// titleKeys: the keys a title is found by: its full key, and its key
// without the subtitle unless that is common (see Match).
func titleKeys(title string, common map[string]bool) []string {
	keys := []string{textkey.FullTitle(title)}
	if k := textkey.Title(title); k != keys[0] && !common[k] {
		keys = append(keys, k)
	}
	return keys
}

func InMembers(series string, members []string) bool {
	return series != "" && slices.ContainsFunc(members, func(m string) bool { return strings.EqualFold(m, series) })
}

// PositionText is a position as the review shows and the library stores it.
func PositionText(p *float64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

func whole(p *float64) bool { return p != nil && *p >= 1 && *p == math.Trunc(*p) }

// Match matches d's books against lib. placeholders are the
// existing ones (a missing book that has one needs nothing); dismissed are
// Hardcover book ids an editor turned down.
func Match(d *hardcover.SeriesDetail, lib Library, placeholders []Placeholder, dismissed map[int64]bool) Review {
	var rev Review
	isDismissed := func(e *hardcover.Entry) bool {
		return slices.ContainsFunc(e.IDs(), func(id int64) bool { return dismissed[id] })
	}
	used := map[string]bool{} // library uuids matched to an entry
	byTitle := lib.ByTitle
	if byTitle == nil {
		byTitle = func(string) []LibBook { return nil }
	}
	// A short title shared by two of the list's books ("Doctor Who" in
	// "Doctor Who: Shada") or two of the series' books here names none.
	common := map[string]bool{}
	seen := map[string]bool{}
	note := func(t string) {
		if k := textkey.Title(t); k != textkey.FullTitle(t) {
			common[k] = common[k] || seen[k]
			seen[k] = true
		}
	}
	for _, e := range d.Entries {
		note(e.Title)
	}
	clear(seen)
	for _, b := range lib.InSeries {
		note(b.Title)
	}
	inSeriesByKey := map[string][]int{}
	for i, b := range lib.InSeries {
		for _, k := range titleKeys(b.Title, common) {
			inSeriesByKey[k] = append(inSeriesByKey[k], i)
		}
	}

	// 0. A library book that names the entry in its Hardcover identifiers.
	// Settled before the rest, so a title or ISBN match to another entry
	// can't take it first.
	byID := map[int]LibBook{}
	for i := range d.Entries {
		e := &d.Entries[i]
		var found []LibBook
		for _, slug := range e.Slugs() {
			found = append(found, lib.ByHCSlug[strings.ToLower(slug)]...)
		}
		for _, id := range e.IDs() {
			found = append(found, lib.ByHCID[id]...)
		}
		slices.SortStableFunc(found, func(a, b LibBook) int {
			return cmp.Compare(boolInt(!InMembers(a.Series, lib.Members)), boolInt(!InMembers(b.Series, lib.Members)))
		})
		for _, b := range found {
			if !used[b.UUID] && !lib.refuses(e, b.UUID) {
				byID[i] = b
				used[b.UUID] = true
				break
			}
		}
	}

	for i := range d.Entries {
		e := &d.Entries[i]
		var keys []string
		addKeys := func(t string) {
			for _, k := range titleKeys(t, common) {
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
		var book *LibBook
		strength := ""
		take := func(b LibBook, s string) {
			cp := b
			book, strength = &cp, s
		}
		if b, ok := byID[i]; ok {
			take(b, MatchByID)
		}
		// 1. An ISBN of one of its editions; a book in the series first.
		var byISBN []LibBook
		for _, n := range e.ISBNs {
			if book != nil {
				break
			}
			for _, b := range lib.ByISBN[isbn.Normalize(n)] {
				if !used[b.UUID] && !lib.refuses(e, b.UUID) && !slices.ContainsFunc(byISBN, func(x LibBook) bool { return x.UUID == b.UUID }) {
					byISBN = append(byISBN, b)
				}
			}
		}
		slices.SortStableFunc(byISBN, func(a, b LibBook) int {
			return cmp.Compare(boolInt(!InMembers(a.Series, lib.Members)), boolInt(!InMembers(b.Series, lib.Members)))
		})
		if len(byISBN) > 0 {
			take(byISBN[0], MatchByISBN)
		}
		// 2. The same title among the series' own books.
		if book == nil {
			for _, k := range keys {
				for _, j := range inSeriesByKey[k] {
					if b := lib.InSeries[j]; !used[b.UUID] && !lib.refuses(e, b.UUID) {
						take(b, MatchBySeries)
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
			var weak *LibBook
			for _, k := range keys {
				for _, b := range byTitle(k) {
					if used[b.UUID] || lib.refuses(e, b.UUID) {
						continue
					}
					if textkey.SharesName(b.Authors, e.Authors) {
						take(b, MatchByTitle)
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
				book, strength = weak, MatchWeak
			}
		}

		gone := isDismissed(e)
		if book == nil {
			s := Suggestion{Entry: e, Checked: whole(e.Position)}
			if ph := PlaceholderFor(e, lib.Members, placeholders); ph != nil {
				s.Placeholder, s.Checked = ph.UUID, false
			} else if gone {
				s.Checked = false
				rev.Dismissed = append(rev.Dismissed, s)
				continue
			}
			if whole(e.Position) {
				rev.Numbered++
			}
			switch {
			case e.Position == nil:
				s.Note = "not numbered on Hardcover"
			case !whole(e.Position):
				s.Note = "a novella or extra (" + PositionText(e.Position) + ")"
			}
			rev.Missing = append(rev.Missing, s)
			continue
		}
		used[book.UUID] = true
		s := Suggestion{Entry: e, Book: book, Strength: strength}
		if InMembers(book.Series, lib.Members) {
			if whole(e.Position) {
				rev.Numbered++
				rev.Have++
			}
			if e.Position != nil && *e.Position != book.Index {
				if gone {
					s.Note = "numbered " + strconv.FormatFloat(book.Index, 'f', -1, 64) + " here"
				} else {
					s.Fix = true
					s.Note = "numbered " + strconv.FormatFloat(book.Index, 'f', -1, 64) + " here, " + PositionText(e.Position) + " on Hardcover"
				}
			}
			rev.Present = append(rev.Present, s)
			continue
		}
		if gone {
			rev.Dismissed = append(rev.Dismissed, s)
			continue
		}
		if whole(e.Position) {
			rev.Numbered++
		}
		s.Checked = strength != MatchWeak && book.Series == "" && e.Position != nil
		rev.Untagged = append(rev.Untagged, s)
	}
	for _, b := range lib.InSeries {
		if !used[b.UUID] {
			cp := b
			rev.Extra = append(rev.Extra, Suggestion{Book: &cp})
		}
	}
	// The "Not a match"es, each under the entry it was refused for.
	if len(lib.Refused) > 0 && lib.Book != nil {
		for i := range d.Entries {
			e := &d.Entries[i]
			var uuids []string
			for _, id := range e.IDs() {
				for u := range lib.Refused[id] {
					if !slices.Contains(uuids, u) {
						uuids = append(uuids, u)
					}
				}
			}
			slices.Sort(uuids)
			for _, u := range uuids {
				if b, ok := lib.Book(u); ok {
					rev.Dismissed = append(rev.Dismissed, Suggestion{Entry: e, Book: &b, Refused: true, Note: "not a match"})
				}
			}
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

// PlaceholderFor: a placeholder that already stands for e: made from it
// (its Hardcover id), in the series with its number, or with its title by
// one of its authors.
func PlaceholderFor(e *hardcover.Entry, members []string, phs []Placeholder) *Placeholder {
	id := strconv.FormatInt(e.BookID, 10)
	key := textkey.FullTitle(e.Title)
	for i := range phs {
		p := &phs[i]
		if p.HCID == id {
			return p
		}
		if e.Position != nil && InMembers(p.Series, members) && p.Index == *e.Position {
			return p
		}
		if textkey.FullTitle(p.Title) == key && textkey.SharesName(p.Authors, e.Authors) {
			return p
		}
	}
	return nil
}
