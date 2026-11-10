package main

import (
	"os"
	"reflect"
	"testing"
)

// Authors split on "&" only: other lists and "Last, First" stay as written
// until the admin clicks Split in the editor.
func TestSplitAuthors(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Alan A. A. Donovan & Brian W. Kernighan", []string{"Alan A. A. Donovan", "Brian W. Kernighan"}},
		{"Hailu, Gebreyesus; Negash, Ghirmai; Chrisman, Laura", []string{"Hailu, Gebreyesus; Negash, Ghirmai; Chrisman, Laura"}},
		{"Donovan, Alan A. A.", []string{"Donovan, Alan A. A."}},
		{"Alan Donovan, Brian Kernighan", []string{"Alan Donovan, Brian Kernighan"}},
		{"  A  Writer && a writer & ", []string{"A Writer"}}, // spaces, empties, repeats
		{"", []string{}},
	}
	for _, c := range cases {
		if got := splitAuthors(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitAuthors(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := parseCreatorsString(" & "); !reflect.DeepEqual(got, []string{"Unknown"}) {
		t.Errorf("empty field: %q", got)
	}
	got := splitCreators([]string{"Hailu, Gebreyesus & Ghirmai Negash", "ghirmai negash", "Chrisman, Laura"})
	if want := []string{"Hailu, Gebreyesus", "Ghirmai Negash", "Chrisman, Laura"}; !reflect.DeepEqual(got, want) {
		t.Errorf("splitCreators = %q, want %q", got, want)
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
