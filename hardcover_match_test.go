package main

import (
	"testing"

	"github.com/iotku/CalibreFast/pkg/hardcover"
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
		if got := hcTitleKey(in); got != want {
			t.Errorf("hcTitleKey(%q) = %q, want %q", in, got, want)
		}
	}
	if hcNameKey("James S.A. Corey") != hcNameKey("james s. a. corey") {
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
		{UUID: "ph8", Meta: OPFMetadata{Title: "TW", Identifiers: []opfIdentifier{{Scheme: hcIdentifierScheme, Value: "9"}}}},
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
	if rev.Hidden != 1 {
		t.Errorf("hidden %d, want 1 (Persepolis Rising)", rev.Hidden)
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
