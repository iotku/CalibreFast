package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/iotku/CalibreFast/pkg/opf"
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
	// A PDF's "First Last;" is the same author as an EPUB's "First Last".
	got = splitCreators([]string{"Firstname Lastname;", "firstname lastname", "A Writer ; & ;"})
	if want := []string{"Firstname Lastname", "A Writer"}; !reflect.DeepEqual(got, want) {
		t.Errorf("splitCreators trailing ';' = %q, want %q", got, want)
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

// Calibre stores a comma in an author's name as "|" (metadata.db's
// authors.name): it's read back as a comma, and written that way.
func TestAuthorNamesWithCommas(t *testing.T) {
	db, tempDir := setupTestDB(t)
	defer os.RemoveAll(tempDir)
	defer db.Close()
	origDB := calibreDB
	calibreDB = db
	defer func() { calibreDB = origDB }()

	_, uuid, _, err := insertBookIntoCalibreDB(db, &UploadedBookMeta{OPFMetadata: OPFMetadata{
		Title:    "Plants",
		Creators: []string{"Battey, Nick", "Mark Fellowes"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stored, sort string
	if err := db.QueryRow(`SELECT name, sort FROM authors WHERE sort LIKE 'Battey%'`).Scan(&stored, &sort); err != nil {
		t.Fatal(err)
	}
	if stored != "Battey| Nick" || sort != "Battey, Nick" {
		t.Errorf("stored as %q (sort %q), want %q (sort %q)", stored, sort, "Battey| Nick", "Battey, Nick")
	}

	want := "Battey, Nick & Mark Fellowes"
	books, err := booksByUUIDs([]string{uuid})
	if err != nil || len(books) != 1 || books[0].Authors != want {
		t.Errorf("grid authors: %v %v, want %q", books, err, want)
	}
	table, err := tableBooks(db, []string{uuid})
	if err != nil || len(table) != 1 || table[0].Authors != want {
		t.Errorf("bulk editor authors: %v %v, want %q", table, err, want)
	}

	// The Authors page lists "Battey, Nick", and its link (/author/Battey,
	// Nick, as the book popup links it too) finds the book.
	c := categoryByKey("author")
	found := false
	for _, it := range queryRawItems(c) {
		if strings.Contains(it.Name, "|") {
			t.Errorf("Authors page lists %q", it.Name)
		}
		found = found || it.Name == "Battey, Nick"
	}
	if !found {
		t.Errorf("Authors page doesn't list %q", "Battey, Nick")
	}
	if n := countBooks(c, []string{"Battey, Nick"}); n != 1 {
		t.Errorf("countBooks(Battey, Nick) = %d, want 1", n)
	}
	rec := httptest.NewRecorder()
	filteredBooksHandler(rec, httptest.NewRequest("GET", "/author/"+url.PathEscape("Battey, Nick")+"?api=1", nil), c)
	var page []Book
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page) != 1 || page[0].UUID != uuid {
		t.Errorf("/author/Battey, Nick: %s", rec.Body.String())
	}
	if _, authors, ok := hcBookTitle(uuid); ok && authors != want {
		t.Errorf("hardcover book authors = %q, want %q", authors, want)
	}

	// The same name again is the same author, and so is a row an older
	// CalibreFast stored with the comma.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var first int64
	_ = tx.QueryRow(`SELECT id FROM authors WHERE name = 'Battey| Nick'`).Scan(&first)
	if id, err := upsertAuthor(tx, "Battey, Nick"); err != nil || id != first {
		t.Errorf("upsertAuthor again: %d %v, want %d", id, err, first)
	}
	res, err := tx.Exec(`INSERT INTO authors (name, sort) VALUES ('Old, Row', 'Old, Row')`)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := res.LastInsertId()
	if id, err := upsertAuthor(tx, "Old, Row"); err != nil || id != old {
		t.Errorf("upsertAuthor of a comma row: %d %v, want %d", id, err, old)
	}
}

// A book never has the same tag twice: tags are trimmed and repeats, in any
// case, dropped, keeping the first spelling.
func TestUniqueTags(t *testing.T) {
	got := parseSubjectsString(" Programming, go ,programming,, Go,PROGRAMMING , Fiction")
	if want := []string{"Programming", "go", "Fiction"}; !reflect.DeepEqual(got, want) {
		t.Errorf("parseSubjectsString = %q, want %q", got, want)
	}
	if got := opf.UniqueTags(nil); len(got) != 0 {
		t.Errorf("opf.UniqueTags(nil) = %q", got)
	}
}

func TestMergeMetaPrefersMetadataTitleOverFilename(t *testing.T) {
	epub := &UploadedBookMeta{OPFMetadata: OPFMetadata{Title: "Dune", Creators: []string{"Frank Herbert"}}, Format: "epub", FileName: "dune.epub"}
	pdf := &UploadedBookMeta{Format: "pdf", FileName: "Frank Herbert - Dune (1965) [scan].pdf"}
	for _, order := range [][]*UploadedBookMeta{{epub, pdf}, {pdf, epub}} {
		m := mergeMeta(order)
		if m.Title != "Dune" || !reflect.DeepEqual(m.Creators, []string{"Frank Herbert"}) {
			t.Errorf("merged = %q by %q; want Dune by Frank Herbert", m.Title, m.Creators)
		}
	}
	// No metadata title anywhere: the EPUB's filename stands in.
	m := mergeMeta([]*UploadedBookMeta{{Format: "pdf", FileName: "a long pdf name.pdf"}, {Format: "epub", FileName: "short.epub"}})
	if m.Title != "short" || !reflect.DeepEqual(m.Creators, []string{"Unknown"}) {
		t.Errorf("merged = %q by %q; want short by Unknown", m.Title, m.Creators)
	}
}
