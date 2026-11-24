package main

import (
	"testing"

	"github.com/iotku/CalibreFast/internal/hardcoverseries"
	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/textkey"
)

func pos(f float64) *float64 { return &f }

func TestHCTitleKey(t *testing.T) {
	for in, want := range map[string]string{
		"The Hobbit":                          "hobbit",
		"Leviathan Wakes (The Expanse #1)":    "leviathan wakes",
		"Caliban's War: The Expanse Book Two": "calibans war",
		"Éclair  &  Café [Book 3]":            "eclair and cafe",
		"A":                                   "a",
		"An Echo of Things to Come":           "echo of things to come",
	} {
		if got := textkey.Title(in); got != want {
			t.Errorf("textkey.Title(%q) = %q, want %q", in, got, want)
		}
	}
	if textkey.Name("James S.A. Corey") != textkey.Name("james s. a. corey") {
		t.Error("author initials don't match")
	}
}

func TestHCMatchSeries(t *testing.T) {
	writer := []string{"James S. A. Corey"}
	d := &hardcover.SeriesDetail{Series: hardcover.Series{ID: 42, Name: "The Expanse"}, Entries: []hardcover.Entry{
		{BookID: 1, Title: "Leviathan Wakes", Position: pos(1), Authors: writer, ISBNs: []string{"9780316129084"}},
		{BookID: 2, Title: "Caliban's War", Position: pos(2), Authors: writer},
		{BookID: 3, Title: "Abaddon's Gate", Position: pos(3), Authors: writer},
		{BookID: 4, Title: "Cibola Burn", Position: pos(4), Authors: writer},
		{BookID: 5, Title: "The Butcher of Anderson Station", Position: pos(1.5), Authors: writer},
		{BookID: 6, Title: "Nemesis Games", Position: pos(5), Authors: writer},
		{BookID: 7, Title: "Babylon's Ashes", Position: pos(6), Authors: writer},
		{BookID: 8, Title: "Persepolis Rising", Position: pos(7), Authors: writer},
		{BookID: 9, Title: "Tiamat's Wrath", Position: pos(8), Authors: writer},
		{BookID: 10, Title: "Memory's Legion", Authors: writer},
	}}
	members := []string{"Expanse", "The Expanse"} // a group of two names
	lib := hardcoverseries.Library{
		Members: members,
		InSeries: []hardcoverseries.LibBook{
			// By ISBN, numbered as on Hardcover, under the group's other name.
			{UUID: "lw", Title: "Leviathan Wakes (Expanse #1)", Authors: []string{"Someone Else"}, Series: "Expanse", Index: 1},
			// Same title in the series, numbered differently.
			{UUID: "cw", Title: "Caliban's War", Authors: writer, Series: "The Expanse", Index: 3},
			// Not on Hardcover's list.
			{UUID: "fan", Title: "Fan Fiction", Authors: writer, Series: "The Expanse", Index: 20},
		},
		ByISBN: map[string][]hardcoverseries.LibBook{
			"9780316129084": {{UUID: "lw", Title: "Leviathan Wakes (Expanse #1)", Series: "Expanse", Index: 1}},
		},
	}
	byTitle := map[string][]hardcoverseries.LibBook{
		// No series, same author: a strong match, ticked.
		"cibola burn": {{UUID: "cb", Title: "Cibola Burn: A Novel", Authors: []string{"james s.a. corey"}}},
		// In another series: suggested, not ticked.
		"nemesis games": {{UUID: "ng", Title: "Nemesis Games", Authors: writer, Series: "Other", Index: 2}},
		// Same title, other author: weak, not ticked.
		"babylons ashes": {{UUID: "ba", Title: "Babylon's Ashes", Authors: []string{"Nobody"}}},
		// Dismissed before.
		"persepolis rising": {{UUID: "pr", Title: "Persepolis Rising", Authors: writer}},
	}
	lib.ByTitle = func(k string) []hardcoverseries.LibBook { return byTitle[k] }
	phs := []*placeholder{
		// Abaddon's Gate already has a placeholder (series and number).
		{UUID: "ph3", Meta: OPFMetadata{Title: "Book three", Series: "the expanse", SeriesIndex: "3"}},
		// Tiamat's Wrath: made from Hardcover before.
		{UUID: "ph8", Meta: OPFMetadata{Title: "TW", Identifiers: []opfIdentifier{{Scheme: hardcover.IDScheme, Value: "9"}}}},
	}
	rev := hardcoverseries.Match(d, lib, hcPlaceholderViews(phs), map[int64]bool{8: true})

	type row struct {
		book    string
		entry   int64
		checked bool
		fix     bool
		ph      string
		str     string
	}
	got := func(ss []hardcoverseries.Suggestion) []row {
		var out []row
		for _, s := range ss {
			r := row{checked: s.Checked, fix: s.Fix, ph: s.Placeholder, str: s.Strength}
			if s.Book != nil {
				r.book = s.Book.UUID
			}
			if s.Entry != nil {
				r.entry = s.Entry.BookID
			}
			out = append(out, r)
		}
		return out
	}
	check := func(name string, ss []hardcoverseries.Suggestion, want []row) {
		t.Helper()
		g := got(ss)
		if len(g) != len(want) {
			t.Errorf("%s: %+v, want %+v", name, g, want)
			return
		}
		for i := range g {
			if g[i] != want[i] {
				t.Errorf("%s[%d]: %+v, want %+v", name, i, g[i], want[i])
			}
		}
	}
	check("present", rev.Present, []row{
		{book: "lw", entry: 1, str: hardcoverseries.MatchByISBN},
		{book: "cw", entry: 2, fix: true, str: hardcoverseries.MatchBySeries},
	})
	check("untagged", rev.Untagged, []row{
		{book: "cb", entry: 4, checked: true, str: hardcoverseries.MatchByTitle},
		{book: "ng", entry: 6, str: hardcoverseries.MatchByTitle},
		{book: "ba", entry: 7, str: hardcoverseries.MatchWeak},
	})
	check("missing", rev.Missing, []row{
		{entry: 3, ph: "ph3"},
		{entry: 5},            // a novella: not ticked
		{entry: 9, ph: "ph8"}, // made from Hardcover before
		{entry: 10},           // not numbered
	})
	check("extra", rev.Extra, []row{{book: "fan"}})
	check("dismissed", rev.Dismissed, []row{{book: "pr", entry: 8, str: hardcoverseries.MatchByTitle}})
	// Whole numbers 1–8 less the dismissed 7; 1 and 2 are here, tagged.
	if rev.Numbered != 7 || rev.Have != 2 || rev.Complete() {
		t.Errorf("numbered %d, have %d", rev.Numbered, rev.Have)
	}
	// Untagged 3, the renumbering, novella and unnumbered: 6 decisions.
	if n := rev.Pending(); n != 6 {
		t.Errorf("pending %d, want 6", n)
	}
	// Dismissing the renumbering leaves it present, without a fix.
	rev = hardcoverseries.Match(d, lib, hcPlaceholderViews(phs), map[int64]bool{2: true})
	if !(len(rev.Present) == 2 && !rev.Present[1].Fix) {
		t.Errorf("dismissed renumbering: %+v", rev.Present)
	}
}

// A series is complete when every whole-numbered book on the list is here
// and tagged; novellas, unnumbered books and dismissed ones don't count.
// A dismissal holds for any of the books merged into an entry.
func TestHCComplete(t *testing.T) {
	d := &hardcover.SeriesDetail{Entries: []hardcover.Entry{
		{BookID: 1, Title: "One", Position: pos(1)},
		{BookID: 2, Title: "Two", Position: pos(2)},
		{BookID: 3, Title: "Two and a Half", Position: pos(2.5)},
		{BookID: 4, Title: "Omnibus", Position: pos(3), AltIDs: []int64{40}},
		{BookID: 5, Title: "Extra"},
	}}
	lib := hardcoverseries.Library{Members: []string{"S"}, InSeries: []hardcoverseries.LibBook{
		{UUID: "a", Title: "One", Series: "S", Index: 1},
		{UUID: "b", Title: "Two", Series: "S", Index: 2},
	}}
	rev := hardcoverseries.Match(d, lib, nil, nil)
	if rev.Numbered != 3 || rev.Have != 2 || rev.Complete() {
		t.Errorf("numbered %d, have %d, complete %v", rev.Numbered, rev.Have, rev.Complete())
	}
	// "Omnibus" turned down when another of its books was shown.
	rev = hardcoverseries.Match(d, lib, nil, map[int64]bool{40: true})
	if rev.Numbered != 2 || rev.Have != 2 || !rev.Complete() || len(rev.Dismissed) != 1 {
		t.Errorf("numbered %d, have %d, complete %v, dismissed %d", rev.Numbered, rev.Have, rev.Complete(), len(rev.Dismissed))
	}
	// A placeholder is still a missing book.
	ph := []*placeholder{{UUID: "p", Meta: OPFMetadata{Title: "Omnibus", Series: "S", SeriesIndex: "3"}}}
	if rev = hardcoverseries.Match(d, lib, hcPlaceholderViews(ph), nil); rev.Complete() {
		t.Error("complete with a placeholder")
	}
	// Here, but not tagged: not complete.
	two := lib.InSeries[1]
	two.Series = ""
	lib.ByTitle = func(k string) []hardcoverseries.LibBook {
		if k == "two" {
			return []hardcoverseries.LibBook{two}
		}
		return nil
	}
	lib.InSeries = lib.InSeries[:1]
	if rev = hardcoverseries.Match(d, lib, nil, map[int64]bool{4: true}); rev.Complete() || rev.Numbered != 2 || rev.Have != 1 {
		t.Errorf("untagged: numbered %d, have %d", rev.Numbered, rev.Have)
	}
	if (&hardcoverseries.Review{}).Complete() {
		t.Error("an empty list is complete")
	}
}

// A row matched to a library book shows that book under it, unless it's
// plainly the same one (same title, an author in common) and already in
// the series.
func TestHCReviewRowsLib(t *testing.T) {
	hero := &hardcover.Entry{BookID: 1, Title: "The Last Hero", Position: pos(27), Authors: []string{"Terry Pratchett"}}
	aaron := &hardcoverseries.LibBook{UUID: "u1", Title: "The Last Hero: A Life of Henry Aaron", Authors: []string{"Howard Bryant"}}
	same := &hardcoverseries.LibBook{UUID: "u2", Title: "The Last Hero", Authors: []string{"Pratchett, Terry"}, Series: "Discworld", Index: 27}
	renamed := &hardcoverseries.LibBook{UUID: "u3", Title: "The Last Hero: A Discworld Fable", Authors: []string{"Terry Pratchett"}, Series: "Discworld", Index: 27}
	rows := hcReviewRowsFor(hardcoverseries.Review{
		Untagged: []hardcoverseries.Suggestion{{Entry: hero, Book: aaron, Strength: hardcoverseries.MatchWeak}},
		Present:  []hardcoverseries.Suggestion{{Entry: hero, Book: same}, {Entry: hero, Book: renamed}},
	}, "Discworld")
	u := rows.Untagged[0]
	if u.Lib == nil || u.Lib.Title != aaron.Title || u.Lib.Authors != "Howard Bryant" || u.Lib.Href != "/book/get/u1" || u.BookHref != "" {
		t.Errorf("untagged row %+v lib %+v", u, u.Lib)
	}
	// "Not a match" is its only button: a Dismiss there would say the same.
	if !u.NotMatch || u.Dismiss {
		t.Errorf("untagged row buttons: not a match %v, dismiss %v", u.NotMatch, u.Dismiss)
	}
	if p := rows.Present[0]; p.Lib != nil || p.BookHref != "/book/get/u2" {
		t.Errorf("same book: %+v", p)
	}
	if p := rows.Present[1]; p.Lib == nil || p.Lib.Series != "Discworld #27" {
		t.Errorf("renamed book: %+v", p)
	}
}

// A library book an editor said isn't a Hardcover book is passed over:
// the next one with the title is matched, else the book is missing. It
// holds for any book merged into the entry.
func TestHCRefused(t *testing.T) {
	d := &hardcover.SeriesDetail{Entries: []hardcover.Entry{
		{BookID: 27, Title: "The Last Hero", Position: pos(27), Authors: []string{"Terry Pratchett"}, AltIDs: []int64{270}},
	}}
	books := []hardcoverseries.LibBook{
		{UUID: "aaron", Title: "The Last Hero: A Life of Henry Aaron", Authors: []string{"Howard Bryant"}},
		{UUID: "other", Title: "The Last Hero", Authors: []string{"Somebody"}},
	}
	lib := hardcoverseries.Library{Members: []string{"Discworld"}, ByTitle: func(k string) []hardcoverseries.LibBook {
		if k == "last hero" {
			return books
		}
		return nil
	}}
	pick := func() string {
		rev := hardcoverseries.Match(d, lib, nil, nil)
		if len(rev.Untagged) == 1 {
			return rev.Untagged[0].Book.UUID
		}
		return ""
	}
	if got := pick(); got != "aaron" {
		t.Fatalf("matched %q", got)
	}
	lib.Refused = map[int64]map[string]bool{270: {"aaron": true}}
	if got := pick(); got != "other" {
		t.Errorf("after refusing aaron: %q", got)
	}
	lib.Refused[27] = map[string]bool{"other": true}
	if rev := hardcoverseries.Match(d, lib, nil, nil); len(rev.Missing) != 1 || len(rev.Untagged) != 0 {
		t.Errorf("both refused: %+v", rev)
	}
	// Listed with the dismissed (to undo), once each, under the entry.
	lib.Book = func(u string) (hardcoverseries.LibBook, bool) {
		for _, b := range books {
			if b.UUID == u {
				return b, true
			}
		}
		return hardcoverseries.LibBook{}, false
	}
	rev := hardcoverseries.Match(d, lib, nil, nil)
	if len(rev.Dismissed) != 2 || !rev.Dismissed[0].Refused || rev.Dismissed[0].Book.UUID != "aaron" || rev.Dismissed[1].Book.UUID != "other" ||
		rev.Dismissed[0].Entry.BookID != 27 || rev.Complete() != false {
		t.Errorf("dismissed %+v", rev.Dismissed)
	}
}

func TestHCKeys(t *testing.T) {
	if textkey.Series("Doctor Who: Target Books") == textkey.Series("Doctor Who: BBC Radio Collection") {
		t.Error("series names differing after a colon are the same")
	}
	if textkey.Series("The Expanse") != textkey.Series("expanse") {
		t.Error("a leading The counts in a series name")
	}
	for _, n := range []string{"Corey, James S. A.", "James S.A. Corey", "james s a corey"} {
		if textkey.Name(n) != textkey.Name("James S. A. Corey") {
			t.Errorf("%q isn't James S. A. Corey", n)
		}
	}
}

func TestHCMatchSharedPrefix(t *testing.T) {
	who := []string{"Terrance Dicks"}
	d := &hardcover.SeriesDetail{Entries: []hardcover.Entry{
		{BookID: 1, Title: "Doctor Who: The Myth Makers", Position: pos(19), Authors: []string{"Donald Cotton"}},
		{BookID: 2, Title: "Doctor Who and the Daleks", Position: pos(2), Authors: []string{"David Whitaker"}, AltTitles: []string{"Dr. Who - The Daleks"}},
		{BookID: 3, Title: "Doctor Who: Shada", Position: pos(110), Authors: who},
	}}
	lib := hardcoverseries.Library{
		Members: []string{"Doctor Who: Target"},
		InSeries: []hardcoverseries.LibBook{
			{UUID: "shada", Title: "Doctor Who: Shada", Series: "Doctor Who: Target", Index: 110},
			{UUID: "daleks", Title: "Dr. Who - The Daleks", Series: "Doctor Who: Target", Index: 2},
		},
	}
	rev := hardcoverseries.Match(d, lib, nil, nil)
	got := map[int64]string{}
	for _, s := range rev.Present {
		got[s.Entry.BookID] = s.Book.UUID
	}
	if got[3] != "shada" || got[2] != "daleks" || len(got) != 2 {
		t.Errorf("present %v", got)
	}
	if len(rev.Missing) != 1 || rev.Missing[0].Entry.BookID != 1 {
		t.Errorf("The Myth Makers isn't missing: %+v", rev.Missing)
	}
}

// A book whose Hardcover identifier names an entry matches it whatever its
// title, ahead of a title or ISBN match to another entry.
func TestHCMatchByIdentifier(t *testing.T) {
	d := &hardcover.SeriesDetail{Entries: []hardcover.Entry{
		{BookID: 1, Slug: "alpha", Title: "Alpha", Position: pos(1), ISBNs: []string{"9780316129084"}},
		{BookID: 2, Title: "Beta", Position: pos(2), AltIDs: []int64{20}},
		{BookID: 3, Slug: "gamma", Title: "Gamma", Position: pos(3)},
		// A less-read book at the same position, kept only by its slug.
		{BookID: 4, Slug: "delta-1", Title: "Delta", Position: pos(4), AltIDs: []int64{40}, AltSlugs: []string{"delta-book-one"}},
	}}
	other := hardcoverseries.LibBook{UUID: "x", Title: "Totally Different"}
	// Titled and ISBN'd like Alpha, but its identifier says it's Beta.
	y := hardcoverseries.LibBook{UUID: "y", Title: "Alpha", Series: "S", Index: 2}
	lib := hardcoverseries.Library{
		Members:  []string{"S"},
		InSeries: []hardcoverseries.LibBook{y},
		ByISBN:   map[string][]hardcoverseries.LibBook{"9780316129084": {y}},
		ByHCSlug: map[string][]hardcoverseries.LibBook{"alpha": {other}, "gamma": {{UUID: "z", Title: "Z"}}, "delta-book-one": {{UUID: "w", Title: "W"}}},
		ByHCID:   map[int64][]hardcoverseries.LibBook{20: {y}},
		Refused:  map[int64]map[string]bool{3: {"z": true}},
	}
	rev := hardcoverseries.Match(d, lib, nil, nil)
	if len(rev.Present) != 1 || rev.Present[0].Book.UUID != "y" || rev.Present[0].Entry.BookID != 2 || rev.Present[0].Strength != hardcoverseries.MatchByID {
		t.Fatalf("present: %+v", rev.Present)
	}
	if len(rev.Untagged) != 2 || rev.Untagged[0].Book.UUID != "x" || rev.Untagged[0].Entry.BookID != 1 ||
		rev.Untagged[0].Strength != hardcoverseries.MatchByID || !rev.Untagged[0].Checked ||
		rev.Untagged[1].Book.UUID != "w" || rev.Untagged[1].Entry.BookID != 4 {
		t.Fatalf("untagged: %+v", rev.Untagged)
	}
	// "Not a match" still wins over the identifier.
	if len(rev.Missing) != 1 || rev.Missing[0].Entry.BookID != 3 {
		t.Fatalf("missing: %+v", rev.Missing)
	}
	if len(rev.Extra) != 0 {
		t.Fatalf("extra: %+v", rev.Extra)
	}
}

// hcLibraryFor finds books by their Hardcover identifiers, as the metadata
// editor saves them (or as Calibre's plugin does, in any case).
func TestHCLibraryIdentifiers(t *testing.T) {
	db, wheel, other := seriesFixture(t)
	bookID := func(uuid string) int64 {
		var id int64
		if err := db.QueryRow(`SELECT id FROM books WHERE uuid = ?`, uuid).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mustExec(t, db, `INSERT INTO identifiers (book, type, val) VALUES (?, 'hardcover', 'my-slug')`, bookID(other[0]))
	mustExec(t, db, `INSERT INTO identifiers (book, type, val) VALUES (?, 'Hardcover-ID', ' 77 ')`, bookID(wheel[0]))
	mustExec(t, db, `INSERT INTO identifiers (book, type, val) VALUES (?, 'hardcover', 'unrelated')`, bookID(other[1]))
	d := &hardcover.SeriesDetail{Entries: []hardcover.Entry{
		{BookID: 5, Slug: "My-Slug", Title: "Nothing Like It", Position: pos(1)},
		{BookID: 6, Title: "Also Unlike", Position: pos(2), AltIDs: []int64{77}},
	}}
	lib, err := hcLibraryFor(hardcoverseries.SeriesInfo{Name: "Wheel", Members: []string{"Wheel"}}, d)
	if err != nil {
		t.Fatal(err)
	}
	if got := lib.ByHCSlug["my-slug"]; len(got) != 1 || got[0].UUID != other[0] {
		t.Errorf("by slug: %+v", got)
	}
	if got := lib.ByHCID[77]; len(got) != 1 || got[0].UUID != wheel[0] {
		t.Errorf("by id: %+v", got)
	}
	if len(lib.ByHCSlug) != 1 || len(lib.ByHCID) != 1 {
		t.Errorf("extra: %+v %+v", lib.ByHCSlug, lib.ByHCID)
	}
	rev := hardcoverseries.Match(d, lib, nil, nil)
	if len(rev.Untagged) != 1 || rev.Untagged[0].Book.UUID != other[0] || rev.Untagged[0].Strength != hardcoverseries.MatchByID {
		t.Errorf("untagged: %+v", rev.Untagged)
	}
}
