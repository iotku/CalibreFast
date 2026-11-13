package main

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFillSeries(t *testing.T) {
	calibre := OPFMetadata{Meta: []opfMeta{{Name: "calibre:series", Content: "Dune"}, {Name: "calibre:series_index", Content: "2.0"}}}
	calibre.fillSeries()
	if calibre.Series != "Dune" || calibre.SeriesIndex != "2" {
		t.Fatalf("calibre meta: %q %q", calibre.Series, calibre.SeriesIndex)
	}
	epub3 := OPFMetadata{Meta: []opfMeta{
		{Property: "belongs-to-collection", ID: "c1", Value: "Box Set"},
		{Property: "collection-type", Refines: "#c1", Value: "set"},
		{Property: "belongs-to-collection", ID: "c2", Value: "The Expanse"},
		{Property: "collection-type", Refines: "#c2", Value: "series"},
		{Property: "group-position", Refines: "#c2", Value: "3"},
	}}
	epub3.fillSeries()
	if epub3.Series != "The Expanse" || epub3.SeriesIndex != "3" {
		t.Fatalf("EPUB 3 meta: %q %q", epub3.Series, epub3.SeriesIndex)
	}

	// Writing back replaces both kinds with Calibre's.
	epub3.Series, epub3.SeriesIndex = "Expanse", "3.5"
	epub3.syncSeriesMeta()
	if got := epub3.getMeta("calibre:series"); got != "Expanse" || epub3.getMeta("calibre:series_index") != "3.5" || len(epub3.Meta) != 2 {
		t.Fatalf("after sync: %+v", epub3.Meta)
	}
	epub3.Series = ""
	epub3.syncSeriesMeta()
	if len(epub3.Meta) != 0 {
		t.Fatalf("after removing: %+v", epub3.Meta)
	}

	for index, want := range map[string]float64{"": 1, "2": 2, " 2.5 ": 2.5} {
		if _, f, err := cleanSeries("Dune", index); err != nil || f != want {
			t.Fatalf("cleanSeries(%q): %v %v", index, f, err)
		}
	}
	for _, bad := range []string{"two", "-1", "NaN"} {
		if _, _, err := cleanSeries("Dune", bad); err == nil {
			t.Fatalf("cleanSeries(%q) accepted", bad)
		}
	}
}

func TestEditSeries(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	uuid := addTestBook(t, db, "Dune Messiah", []string{"Frank Herbert"}, nil, "", "epub")
	other := addTestBook(t, db, "Dune", []string{"Frank Herbert"}, nil, "", "epub")
	var path string
	if err := db.QueryRow(`SELECT path FROM books WHERE uuid = ?`, uuid).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(baseDir, path), 0o755); err != nil {
		t.Fatal(err)
	}
	seriesOf := func(u string) (string, float64, int) {
		t.Helper()
		var name sql.NullString
		var index float64
		var links int
		if err := db.QueryRow(`SELECT (SELECT s.name FROM books_series_link l JOIN series s ON s.id = l.series WHERE l.book = b.id),
				b.series_index, (SELECT COUNT(*) FROM books_series_link l WHERE l.book = b.id)
			FROM books b WHERE b.uuid = ?`, u).Scan(&name, &index, &links); err != nil {
			t.Fatal(err)
		}
		return name.String, index, links
	}
	save := func(body string) {
		t.Helper()
		var req ApplyMetadataRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		meta := metadataFromRequest(req)
		if _, err := updateBookInCalibreDB(db, baseDir, uuid, &meta); err != nil {
			t.Fatal(err)
		}
	}

	save(`{"title":"Dune Messiah","creators":"Frank Herbert","series":"Dune","series_index":"2"}`)
	if n, i, links := seriesOf(uuid); n != "Dune" || i != 2 || links != 1 {
		t.Fatalf("set: %q %v %d", n, i, links)
	}
	opf, err := loadOPF(filepath.Join(baseDir, path, "metadata.opf"))
	if err != nil || opf.Metadata.Series != "Dune" || opf.Metadata.SeriesIndex != "2" {
		t.Fatalf("metadata.opf: %v %+v", err, opf)
	}
	// Another series replaces it (one link, never two).
	save(`{"title":"Dune Messiah","creators":"Frank Herbert","series":"Dune Chronicles","series_index":"2.5"}`)
	if n, i, links := seriesOf(uuid); n != "Dune Chronicles" || i != 2.5 || links != 1 {
		t.Fatalf("replace: %q %v %d", n, i, links)
	}
	// Not sent: left as it is.
	save(`{"title":"Dune Messiah","creators":"Frank Herbert"}`)
	if n, _, _ := seriesOf(uuid); n != "Dune Chronicles" {
		t.Fatalf("not sent: %q", n)
	}
	// Bad number: refused before anything is written.
	var req ApplyMetadataRequest
	_ = json.Unmarshal([]byte(`{"title":"Dune Messiah","series":"Dune","series_index":"two"}`), &req)
	bad := metadataFromRequest(req)
	if _, err := updateBookInCalibreDB(db, baseDir, uuid, &bad); err == nil {
		t.Fatal("bad series number saved")
	}
	// Empty: removed, number back to 1.
	save(`{"title":"Dune Messiah","creators":"Frank Herbert","series":"","series_index":"7"}`)
	if n, i, links := seriesOf(uuid); n != "" || i != 1 || links != 0 {
		t.Fatalf("remove: %q %v %d", n, i, links)
	}
	opf, _ = loadOPF(filepath.Join(baseDir, path, "metadata.opf"))
	if opf.Metadata.getMeta("calibre:series") != "" {
		t.Fatalf("metadata.opf still has a series: %+v", opf.Metadata.Meta)
	}

	// The book page and the series page, from metadata.db.
	save(`{"title":"Dune Messiah","creators":"Frank Herbert","series":"Dune","series_index":"2"}`)
	var otherID int64
	_ = db.QueryRow(`SELECT id FROM books WHERE uuid = ?`, other).Scan(&otherID)
	tx, _ := db.Begin()
	if err := setBookSeries(tx, otherID, "Dune", 1); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	if n, i := librarySeriesFields(uuid); n != "Dune" || i != "2" {
		t.Fatalf("librarySeriesFields: %q %q", n, i)
	}
	rec := httptest.NewRecorder()
	filteredBooksHandler(rec, httptest.NewRequest("GET", "/series/Dune?api=1", nil), categoryByKey("series"))
	var books []Book
	_ = json.Unmarshal(rec.Body.Bytes(), &books)
	if len(books) != 2 || books[0].UUID != other || books[1].UUID != uuid {
		t.Fatalf("series page order: %+v", books)
	}
}

// A book added from the import queue gets the series the editor sent
// (imports.go: syncSeriesMeta on the files' meta).
func TestImportSeries(t *testing.T) {
	db := catalogFixture(t)
	var req ApplyMetadataRequest
	_ = json.Unmarshal([]byte(`{"title":"Children of Dune","creators":"Frank Herbert","series":"Dune","series_index":"3"}`), &req)
	book := &UploadedBookMeta{OPFMetadata: metadataFromRequest(req)}
	// What the files said, replaced by what was sent.
	book.Meta = []opfMeta{{Name: "calibre:series", Content: "Wrong"}, {Name: "calibre:series_index", Content: "9"}}
	book.syncSeriesMeta()
	_, uuid, _, err := insertBookIntoCalibreDB(db, book, nil)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	var index float64
	if err := db.QueryRow(`SELECT s.name, b.series_index FROM books b JOIN books_series_link l ON l.book = b.id
		JOIN series s ON s.id = l.series WHERE b.uuid = ?`, uuid).Scan(&name, &index); err != nil {
		t.Fatal(err)
	}
	if name != "Dune" || index != 3 {
		t.Fatalf("imported series: %q %v", name, index)
	}
}
