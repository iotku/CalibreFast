package main

import (
	"os"
	"reflect"
	"testing"
)

func TestParseAuthors(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Hailu, Gebreyesus; Negash, Ghirmai; Chrisman, Laura", []string{"Gebreyesus Hailu", "Ghirmai Negash", "Laura Chrisman"}},
		{"Alan A. A. Donovan & Brian W. Kernighan", []string{"Alan A. A. Donovan", "Brian W. Kernighan"}},
		{"Alan Donovan, Brian Kernighan", []string{"Alan Donovan", "Brian Kernighan"}}, // full names: commas separate
		{"Donovan, Alan A. A.", []string{"Alan A. A. Donovan"}},                        // one "Last, First"
		{"Hailu, Gebreyesus", []string{"Gebreyesus Hailu"}},
		{"Martin Luther King, Jr.", []string{"Martin Luther King, Jr."}},   // a suffix, not a first name
		{"King, Martin Luther, Jr.", []string{"King, Martin Luther, Jr."}}, // two commas: left alone
		{"Plato", []string{"Plato"}},
		{"  A  Writer ;; a writer & ", []string{"A Writer"}}, // spaces, empties, repeats
		{"", []string{}},
	}
	for _, c := range cases {
		if got := parseAuthors(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseAuthors(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := parseCreatorsString(" ; "); !reflect.DeepEqual(got, []string{"Unknown"}) {
		t.Errorf("empty field: %q", got)
	}
	got := normalizeAuthors([]string{"Hailu, Gebreyesus; Negash, Ghirmai", "Ghirmai Negash", "Laura Chrisman"})
	if want := []string{"Gebreyesus Hailu", "Ghirmai Negash", "Laura Chrisman"}; !reflect.DeepEqual(got, want) {
		t.Errorf("normalizeAuthors = %q, want %q", got, want)
	}
}

// The grids show authors' names in Calibre's order, like the book popup,
// not books.author_sort ("Last, First").
func TestBookAuthorsSQL(t *testing.T) {
	db, tempDir := setupTestDB(t)
	defer os.RemoveAll(tempDir)
	defer db.Close()
	origDB := calibreDB
	calibreDB = db
	defer func() { calibreDB = origDB }()

	_, uuid, _, err := insertBookIntoCalibreDB(db, &UploadedBookMeta{OPFMetadata: OPFMetadata{
		Title:    "The Go Programming Language",
		Creators: []string{"Alan A. A. Donovan", "Brian W. Kernighan"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sortForm string
	db.QueryRow(`SELECT author_sort FROM books WHERE uuid = ?`, uuid).Scan(&sortForm)
	books, err := booksByUUIDs([]string{uuid})
	if err != nil || len(books) != 1 {
		t.Fatalf("booksByUUIDs: %v %v", books, err)
	}
	if got := books[0].Authors; got != "Alan A. A. Donovan & Brian W. Kernighan" {
		t.Errorf("grid authors = %q (author_sort is %q)", got, sortForm)
	}
}
