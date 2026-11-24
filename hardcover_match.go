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

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/isbn"
	"github.com/iotku/CalibreFast/pkg/textkey"
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
	// byTitle: library books with a title key (textkey.Title or
	// textkey.FullTitle) of key.
	byTitle func(key string) []hcLibBook
	// byISBN: library books with this ISBN-13.
	byISBN map[string][]hcLibBook
	// byHCSlug, byHCID: library books whose "hardcover" identifier is this
	// slug (lower case) or whose "hardcover-id" is this book id: an
	// editor's say-so that the book is that one, whatever its title.
	byHCSlug map[string][]hcLibBook
	byHCID   map[int64][]hcLibBook
	// members: the series' names (its group's members), any case.
	members []string
	// refused: Hardcover book id -> library uuids an editor said aren't
	// that book ("Not a match").
	refused map[int64]map[string]bool
	// book: a library book by uuid, for listing the refused ones.
	book func(uuid string) (hcLibBook, bool)
}

// refuses: an editor said the library book isn't e (any of its books).
func (l *hcLibrary) refuses(e *hardcover.Entry, uuid string) bool {
	if len(l.refused) == 0 {
		return false
	}
	return slices.ContainsFunc(e.IDs(), func(id int64) bool { return l.refused[id][uuid] })
}

// Match strengths.
const (
	hcByID     = "hardcover id" // the book's Hardcover identifier
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
	// Refused: an editor said Book isn't Entry ("Not a match"); listed
	// with the dismissed, to undo.
	Refused bool `json:"refused,omitempty"`
	// Placeholder: a missing book that already has one (nothing to do).
	Placeholder string `json:"placeholder,omitempty"`
}

// hcReview is a series' review.
type hcReview struct {
	Present  []hcSuggestion `json:"present"`  // here and tagged
	Untagged []hcSuggestion `json:"untagged"` // here, not tagged with the series
	Missing  []hcSuggestion `json:"missing"`  // not here
	Extra    []hcSuggestion `json:"extra"`    // here, tagged, not on Hardcover
	// Dismissed: suggestions an editor turned down, left out of the rest.
	Dismissed []hcSuggestion `json:"dismissed"`
	// Numbered: the list's whole-numbered books (1, 2, 3…), less those
	// dismissed; Have: how many of them are here, tagged with the series.
	Numbered, Have int
}

// complete: every whole-numbered book on the list is here and tagged.
func (r *hcReview) complete() bool { return r.Numbered > 0 && r.Have == r.Numbered }

func (r *hcReview) status() seriesStatus {
	return seriesStatus{Numbered: r.Numbered, Have: r.Have, Complete: r.complete(), Source: statusHardcover}
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

// hcTitleKeys: the keys a title is found by: its full key, and its key
// without the subtitle unless that is common (see hcMatchSeries).
func hcTitleKeys(title string, common map[string]bool) []string {
	keys := []string{textkey.FullTitle(title)}
	if k := textkey.Title(title); k != keys[0] && !common[k] {
		keys = append(keys, k)
	}
	return keys
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
	isDismissed := func(e *hardcover.Entry) bool {
		return slices.ContainsFunc(e.IDs(), func(id int64) bool { return dismissed[id] })
	}
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
		if k := textkey.Title(t); k != textkey.FullTitle(t) {
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

	// 0. A library book that names the entry in its Hardcover identifiers.
	// Settled before the rest, so a title or ISBN match to another entry
	// can't take it first.
	byID := map[int]hcLibBook{}
	for i := range d.Entries {
		e := &d.Entries[i]
		var found []hcLibBook
		for _, slug := range e.Slugs() {
			found = append(found, lib.byHCSlug[strings.ToLower(slug)]...)
		}
		for _, id := range e.IDs() {
			found = append(found, lib.byHCID[id]...)
		}
		slices.SortStableFunc(found, func(a, b hcLibBook) int {
			return cmp.Compare(boolInt(!hcInMembers(a.Series, lib.members)), boolInt(!hcInMembers(b.Series, lib.members)))
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
		if b, ok := byID[i]; ok {
			take(b, hcByID)
		}
		// 1. An ISBN of one of its editions; a book in the series first.
		var byISBN []hcLibBook
		for _, n := range e.ISBNs {
			if book != nil {
				break
			}
			for _, b := range lib.byISBN[isbn.Normalize(n)] {
				if !used[b.UUID] && !lib.refuses(e, b.UUID) && !slices.ContainsFunc(byISBN, func(x hcLibBook) bool { return x.UUID == b.UUID }) {
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
					if b := lib.inSeries[j]; !used[b.UUID] && !lib.refuses(e, b.UUID) {
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
					if used[b.UUID] || lib.refuses(e, b.UUID) {
						continue
					}
					if textkey.SharesName(b.Authors, e.Authors) {
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

		gone := isDismissed(e)
		if book == nil {
			s := hcSuggestion{Entry: e, Checked: hcWhole(e.Position)}
			if ph := hcPlaceholderFor(e, lib.members, placeholders); ph != nil {
				s.Placeholder, s.Checked = ph.UUID, false
			} else if gone {
				s.Checked = false
				rev.Dismissed = append(rev.Dismissed, s)
				continue
			}
			if hcWhole(e.Position) {
				rev.Numbered++
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
			if hcWhole(e.Position) {
				rev.Numbered++
				rev.Have++
			}
			if e.Position != nil && *e.Position != book.Index {
				if gone {
					s.Note = "numbered " + strconv.FormatFloat(book.Index, 'f', -1, 64) + " here"
				} else {
					s.Fix = true
					s.Note = "numbered " + strconv.FormatFloat(book.Index, 'f', -1, 64) + " here, " + hcPositionText(e.Position) + " on Hardcover"
				}
			}
			rev.Present = append(rev.Present, s)
			continue
		}
		if gone {
			rev.Dismissed = append(rev.Dismissed, s)
			continue
		}
		if hcWhole(e.Position) {
			rev.Numbered++
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
	// The "Not a match"es, each under the entry it was refused for.
	if len(lib.refused) > 0 && lib.book != nil {
		for i := range d.Entries {
			e := &d.Entries[i]
			var uuids []string
			for _, id := range e.IDs() {
				for u := range lib.refused[id] {
					if !slices.Contains(uuids, u) {
						uuids = append(uuids, u)
					}
				}
			}
			slices.Sort(uuids)
			for _, u := range uuids {
				if b, ok := lib.book(u); ok {
					rev.Dismissed = append(rev.Dismissed, hcSuggestion{Entry: e, Book: &b, Refused: true, Note: "not a match"})
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

// hcPlaceholderFor: a placeholder that already stands for e: made from it
// (its Hardcover id), in the series with its number, or with its title by
// one of its authors.
func hcPlaceholderFor(e *hardcover.Entry, members []string, phs []*placeholder) *placeholder {
	id := strconv.FormatInt(e.BookID, 10)
	key := textkey.FullTitle(e.Title)
	for _, p := range phs {
		for _, x := range p.Meta.Identifiers {
			if strings.EqualFold(x.Scheme, hardcover.IDScheme) && x.Value == id {
				return p
			}
		}
		if e.Position != nil && hcInMembers(p.Meta.Series, members) && p.index() == *e.Position {
			return p
		}
		if textkey.FullTitle(p.Meta.Title) == key && textkey.SharesName(p.Meta.Creators, e.Authors) {
			return p
		}
	}
	return nil
}
