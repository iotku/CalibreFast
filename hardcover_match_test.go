package main

import (
	"testing"

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
	lib := hcLibrary{
		members: members,
		inSeries: []hcLibBook{
			// By ISBN, numbered as on Hardcover, under the group's other name.
			{UUID: "lw", Title: "Leviathan Wakes (Expanse #1)", Authors: []string{"Someone Else"}, Series: "Expanse", Index: 1},
			// Same title in the series, numbered differently.
			{UUID: "cw", Title: "Caliban's War", Authors: writer, Series: "The Expanse", Index: 3},
			// Not on Hardcover's list.
			{UUID: "fan", Title: "Fan Fiction", Authors: writer, Series: "The Expanse", Index: 20},
		},
		byISBN: map[string][]hcLibBook{
			"9780316129084": {{UUID: "lw", Title: "Leviathan Wakes (Expanse #1)", Series: "Expanse", Index: 1}},
		},
	}
	byTitle := map[string][]hcLibBook{
		// No series, same author: a strong match, ticked.
		"cibola burn": {{UUID: "cb", Title: "Cibola Burn: A Novel", Authors: []string{"james s.a. corey"}}},
		// In another series: suggested, not ticked.
		"nemesis games": {{UUID: "ng", Title: "Nemesis Games", Authors: writer, Series: "Other", Index: 2}},
		// Same title, other author: weak, not ticked.
		"babylons ashes": {{UUID: "ba", Title: "Babylon's Ashes", Authors: []string{"Nobody"}}},
		// Dismissed before.
		"persepolis rising": {{UUID: "pr", Title: "Persepolis Rising", Authors: writer}},
	}
	lib.byTitle = func(k string) []hcLibBook { return byTitle[k] }
	phs := []*placeholder{
		// Abaddon's Gate already has a placeholder (series and number).
		{UUID: "ph3", Meta: OPFMetadata{Title: "Book three", Series: "the expanse", SeriesIndex: "3"}},
		// Tiamat's Wrath: made from Hardcover before.
		{UUID: "ph8", Meta: OPFMetadata{Title: "TW", Identifiers: []opfIdentifier{{Scheme: hardcover.IDScheme, Value: "9"}}}},
	}
	rev := hcMatchSeries(d, lib, phs, map[int64]bool{8: true})

	type row struct {
		book    string
		entry   int64
		checked bool
		fix     bool
		ph      string
		str     string
	}
	got := func(ss []hcSuggestion) []row {
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
	check := func(name string, ss []hcSuggestion, want []row) {
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
		{book: "lw", entry: 1, str: hcByISBN},
		{book: "cw", entry: 2, fix: true, str: hcBySeries},
	})
	check("untagged", rev.Untagged, []row{
		{book: "cb", entry: 4, checked: true, str: hcByTitle},
		{book: "ng", entry: 6, str: hcByTitle},
		{book: "ba", entry: 7, str: hcWeak},
	})
	check("missing", rev.Missing, []row{
		{entry: 3, ph: "ph3"},
		{entry: 5},            // a novella: not ticked
		{entry: 9, ph: "ph8"}, // made from Hardcover before
		{entry: 10},           // not numbered
	})
	check("extra", rev.Extra, []row{{book: "fan"}})
	check("dismissed", rev.Dismissed, []row{{book: "pr", entry: 8, str: hcByTitle}})
	// Whole numbers 1–8 less the dismissed 7; 1 and 2 are here, tagged.
	if rev.Numbered != 7 || rev.Have != 2 || rev.complete() {
		t.Errorf("numbered %d, have %d", rev.Numbered, rev.Have)
	}
	// Untagged 3, the renumbering, novella and unnumbered: 6 decisions.
	if n := rev.pending(); n != 6 {
		t.Errorf("pending %d, want 6", n)
	}
	// Dismissing the renumbering leaves it present, without a fix.
	rev = hcMatchSeries(d, lib, phs, map[int64]bool{2: true})
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
	lib := hcLibrary{members: []string{"S"}, inSeries: []hcLibBook{
		{UUID: "a", Title: "One", Series: "S", Index: 1},
		{UUID: "b", Title: "Two", Series: "S", Index: 2},
	}}
	rev := hcMatchSeries(d, lib, nil, nil)
	if rev.Numbered != 3 || rev.Have != 2 || rev.complete() {
		t.Errorf("numbered %d, have %d, complete %v", rev.Numbered, rev.Have, rev.complete())
	}
	// "Omnibus" turned down when another of its books was shown.
	rev = hcMatchSeries(d, lib, nil, map[int64]bool{40: true})
	if rev.Numbered != 2 || rev.Have != 2 || !rev.complete() || len(rev.Dismissed) != 1 {
		t.Errorf("numbered %d, have %d, complete %v, dismissed %d", rev.Numbered, rev.Have, rev.complete(), len(rev.Dismissed))
	}
	// A placeholder is still a missing book.
	ph := []*placeholder{{UUID: "p", Meta: OPFMetadata{Title: "Omnibus", Series: "S", SeriesIndex: "3"}}}
	if rev = hcMatchSeries(d, lib, ph, nil); rev.complete() {
		t.Error("complete with a placeholder")
	}
	// Here, but not tagged: not complete.
	two := lib.inSeries[1]
	two.Series = ""
	lib.byTitle = func(k string) []hcLibBook {
		if k == "two" {
			return []hcLibBook{two}
		}
		return nil
	}
	lib.inSeries = lib.inSeries[:1]
	if rev = hcMatchSeries(d, lib, nil, map[int64]bool{4: true}); rev.complete() || rev.Numbered != 2 || rev.Have != 1 {
		t.Errorf("untagged: numbered %d, have %d", rev.Numbered, rev.Have)
	}
	if (&hcReview{}).complete() {
		t.Error("an empty list is complete")
	}
}

// A row matched to a library book shows that book under it, unless it's
// plainly the same one (same title, an author in common) and already in
// the series.
func TestHCReviewRowsLib(t *testing.T) {
	hero := &hardcover.Entry{BookID: 1, Title: "The Last Hero", Position: pos(27), Authors: []string{"Terry Pratchett"}}
	aaron := &hcLibBook{UUID: "u1", Title: "The Last Hero: A Life of Henry Aaron", Authors: []string{"Howard Bryant"}}
	same := &hcLibBook{UUID: "u2", Title: "The Last Hero", Authors: []string{"Pratchett, Terry"}, Series: "Discworld", Index: 27}
	renamed := &hcLibBook{UUID: "u3", Title: "The Last Hero: A Discworld Fable", Authors: []string{"Terry Pratchett"}, Series: "Discworld", Index: 27}
	rows := hcReviewRowsFor(hcReview{
		Untagged: []hcSuggestion{{Entry: hero, Book: aaron, Strength: hcWeak}},
		Present:  []hcSuggestion{{Entry: hero, Book: same}, {Entry: hero, Book: renamed}},
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
	books := []hcLibBook{
		{UUID: "aaron", Title: "The Last Hero: A Life of Henry Aaron", Authors: []string{"Howard Bryant"}},
		{UUID: "other", Title: "The Last Hero", Authors: []string{"Somebody"}},
	}
	lib := hcLibrary{members: []string{"Discworld"}, byTitle: func(k string) []hcLibBook {
		if k == "last hero" {
			return books
		}
		return nil
	}}
	pick := func() string {
		rev := hcMatchSeries(d, lib, nil, nil)
		if len(rev.Untagged) == 1 {
			return rev.Untagged[0].Book.UUID
		}
		return ""
	}
	if got := pick(); got != "aaron" {
		t.Fatalf("matched %q", got)
	}
	lib.refused = map[int64]map[string]bool{270: {"aaron": true}}
	if got := pick(); got != "other" {
		t.Errorf("after refusing aaron: %q", got)
	}
	lib.refused[27] = map[string]bool{"other": true}
	if rev := hcMatchSeries(d, lib, nil, nil); len(rev.Missing) != 1 || len(rev.Untagged) != 0 {
		t.Errorf("both refused: %+v", rev)
	}
	// Listed with the dismissed (to undo), once each, under the entry.
	lib.book = func(u string) (hcLibBook, bool) {
		for _, b := range books {
			if b.UUID == u {
				return b, true
			}
		}
		return hcLibBook{}, false
	}
	rev := hcMatchSeries(d, lib, nil, nil)
	if len(rev.Dismissed) != 2 || !rev.Dismissed[0].Refused || rev.Dismissed[0].Book.UUID != "aaron" || rev.Dismissed[1].Book.UUID != "other" ||
		rev.Dismissed[0].Entry.BookID != 27 || rev.complete() != false {
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
	// A short title shared by several books doesn't name any of them.
	common := map[string]bool{"doctor who": true}
	if k := hcTitleKeys("Doctor Who: The Myth Makers", common); len(k) != 1 || k[0] != "doctor who the myth makers" {
		t.Errorf("keys %q", k)
	}
	if k := hcTitleKeys("Caliban's War: The Expanse Book Two", nil); len(k) != 2 || k[1] != "calibans war" {
		t.Errorf("keys %q", k)
	}
}

func TestHCMatchSharedPrefix(t *testing.T) {
	who := []string{"Terrance Dicks"}
	d := &hardcover.SeriesDetail{Entries: []hardcover.Entry{
		{BookID: 1, Title: "Doctor Who: The Myth Makers", Position: pos(19), Authors: []string{"Donald Cotton"}},
		{BookID: 2, Title: "Doctor Who and the Daleks", Position: pos(2), Authors: []string{"David Whitaker"}, AltTitles: []string{"Dr. Who - The Daleks"}},
		{BookID: 3, Title: "Doctor Who: Shada", Position: pos(110), Authors: who},
	}}
	lib := hcLibrary{
		members: []string{"Doctor Who: Target"},
		inSeries: []hcLibBook{
			{UUID: "shada", Title: "Doctor Who: Shada", Series: "Doctor Who: Target", Index: 110},
			{UUID: "daleks", Title: "Dr. Who - The Daleks", Series: "Doctor Who: Target", Index: 2},
		},
	}
	rev := hcMatchSeries(d, lib, nil, nil)
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

func TestHCRankCandidates(t *testing.T) {
	cand := func(id int64, name, author string, hits int, books ...string) hcCandidate {
		return hcCandidate{Series: hardcover.Series{ID: id, Name: name, Author: author, Books: books}, Hits: hits}
	}
	picked := func(cs []hcCandidate) (ids []int64, sure bool) {
		for _, c := range cs {
			if c.Pick {
				ids = append(ids, c.ID)
				sure = c.Sure
			}
		}
		return ids, sure
	}
	// Hardcover's split series: a library series of another name; the
	// group holding its ISBNs is picked whole, parts without hits too.
	cs := hcRankCandidates([]hcCandidate{
		cand(10, "Doctor Who: BBC Radio Collection", "", 1),
		cand(1, "Doctor Who: Target Books", "Gerry Davis", 6),
		cand(2, "Doctor Who: Target Books", "Terrance Dicks", 3),
		cand(3, "Doctor Who: Target Books", "Malcolm Hulke", 0),
	}, map[string]bool{textkey.Series("Doctor Who: Target"): true}, nil, nil)
	ids, sure := picked(cs)
	if len(ids) != 3 || !sure || cs[0].ID != 1 || cs[3].ID != 10 {
		t.Errorf("split series: picked %v sure %v, order %v", ids, sure, cs)
	}
	// Same name, by the library's author: picked and sure; other authors'
	// same-named series are another group only if their name differs.
	cs = hcRankCandidates([]hcCandidate{
		cand(5, "Sherlock Holmes", "Val Andrews", 0),
		cand(6, "Sherlock Holmes", "Arthur Conan Doyle", 0),
		cand(7, "Sherlock Holmes Novels", "Arthur Conan Doyle", 0),
	}, map[string]bool{textkey.Series("Sherlock Holmes"): true}, []string{"Doyle, Arthur Conan"}, nil)
	if ids, sure := picked(cs); len(ids) != 2 || !sure {
		t.Errorf("same name: picked %v sure %v", ids, sure)
	}
	// Evidence by titles only, under another name: picked, not sure.
	cs = hcRankCandidates([]hcCandidate{cand(8, "Barsoom Novels", "", 0, "A Princess of Mars")},
		map[string]bool{textkey.Series("Barsoom"): true}, nil, nil)
	if ids, _ := picked(cs); len(ids) != 0 || cs[0].Evidence != "" {
		t.Errorf("no ISBNs, other name: picked %v", ids)
	}
	cs = hcRankCandidates([]hcCandidate{cand(8, "Barsoom", "", 0, "A Princess of Mars")},
		map[string]bool{textkey.Series("Barsoom"): true}, nil, map[string]bool{textkey.FullTitle("A Princess of Mars"): true})
	if ids, sure := picked(cs); len(ids) != 1 || !sure || cs[0].Evidence != "title" {
		t.Errorf("titles: picked %v sure %v %+v", ids, sure, cs[0])
	}
	// Titles only, under another name: the whole group ticked, not linked by a batch.
	cs = hcRankCandidates([]hcCandidate{
		cand(11, "Doctor Who: Target Books", "Gerry Davis", 0, "Doctor Who: Shada"),
		cand(12, "Doctor Who: Target Books", "Ian Briggs", 0),
		cand(13, "Doctor Who: Novels", "", 0),
	}, map[string]bool{textkey.Series("Doctor Who: Target"): true}, nil, map[string]bool{textkey.FullTitle("Doctor Who: Shada"): true})
	if ids, sure := picked(cs); len(ids) != 2 || sure {
		t.Errorf("titles, other name: picked %v sure %v", ids, sure)
	}
	// A weak lead (1 ISBN, another name) is ticked but not linked by a batch.
	cs = hcRankCandidates([]hcCandidate{cand(9, "Something Else", "", 1)}, map[string]bool{"x": true}, nil, nil)
	if ids, sure := picked(cs); len(ids) != 1 || sure {
		t.Errorf("weak lead: picked %v sure %v", ids, sure)
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
	other := hcLibBook{UUID: "x", Title: "Totally Different"}
	// Titled and ISBN'd like Alpha, but its identifier says it's Beta.
	y := hcLibBook{UUID: "y", Title: "Alpha", Series: "S", Index: 2}
	lib := hcLibrary{
		members:  []string{"S"},
		inSeries: []hcLibBook{y},
		byISBN:   map[string][]hcLibBook{"9780316129084": {y}},
		byHCSlug: map[string][]hcLibBook{"alpha": {other}, "gamma": {{UUID: "z", Title: "Z"}}, "delta-book-one": {{UUID: "w", Title: "W"}}},
		byHCID:   map[int64][]hcLibBook{20: {y}},
		refused:  map[int64]map[string]bool{3: {"z": true}},
	}
	rev := hcMatchSeries(d, lib, nil, nil)
	if len(rev.Present) != 1 || rev.Present[0].Book.UUID != "y" || rev.Present[0].Entry.BookID != 2 || rev.Present[0].Strength != hcByID {
		t.Fatalf("present: %+v", rev.Present)
	}
	if len(rev.Untagged) != 2 || rev.Untagged[0].Book.UUID != "x" || rev.Untagged[0].Entry.BookID != 1 ||
		rev.Untagged[0].Strength != hcByID || !rev.Untagged[0].Checked ||
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
	lib, err := hcLibraryFor(hcSeriesInfo{Name: "Wheel", Members: []string{"Wheel"}}, d)
	if err != nil {
		t.Fatal(err)
	}
	if got := lib.byHCSlug["my-slug"]; len(got) != 1 || got[0].UUID != other[0] {
		t.Errorf("by slug: %+v", got)
	}
	if got := lib.byHCID[77]; len(got) != 1 || got[0].UUID != wheel[0] {
		t.Errorf("by id: %+v", got)
	}
	if len(lib.byHCSlug) != 1 || len(lib.byHCID) != 1 {
		t.Errorf("extra: %+v %+v", lib.byHCSlug, lib.byHCID)
	}
	rev := hcMatchSeries(d, lib, nil, nil)
	if len(rev.Untagged) != 1 || rev.Untagged[0].Book.UUID != other[0] || rev.Untagged[0].Strength != hcByID {
		t.Errorf("untagged: %+v", rev.Untagged)
	}
}
