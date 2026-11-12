package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// catalogFixture: a metadata.db with calibreDB, baseDir and cacheDir pointed
// at it, restored after the test.
func catalogFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, dir := setupTestDB(t)
	origDB, origBase, origCache := calibreDB, baseDir, cacheDir
	calibreDB, baseDir, cacheDir = db, dir, filepath.Join(dir, "cache")
	libraryCatalog.Store(nil)
	t.Cleanup(func() {
		// Apply what's queued while this test's database is still open, then
		// leave no catalog: other tests read metadata.db as before.
		if err := catalogFlush(); err != nil {
			t.Error(err)
		}
		libraryCatalog.Store(nil)
		calibreDB, baseDir, cacheDir = origDB, origBase, origCache
		db.Close()
		os.RemoveAll(dir)
	})
	return db
}

func addTestBook(t *testing.T, db *sql.DB, title string, authors []string, tags []string, series string, formats ...string) string {
	t.Helper()
	id, uuid, _, err := insertBookIntoCalibreDB(db, &UploadedBookMeta{OPFMetadata: OPFMetadata{Title: title, Creators: authors}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range tags {
		mustExec(t, db, `INSERT OR IGNORE INTO tags (name) VALUES (?)`, tag)
		mustExec(t, db, `INSERT INTO books_tags_link (book, tag) SELECT ?, id FROM tags WHERE name = ?`, id, tag)
	}
	if series != "" {
		mustExec(t, db, `INSERT OR IGNORE INTO series (name) VALUES (?)`, series)
		mustExec(t, db, `INSERT INTO books_series_link (book, series) SELECT ?, id FROM series WHERE name = ?`, id, series)
	}
	for _, f := range formats {
		mustExec(t, db, `INSERT INTO data (book, format, uncompressed_size, name) VALUES (?, ?, 1, ?)`, id, strings.ToUpper(f), title)
	}
	// Distinct timestamps, newest last, so Latest order is known.
	mustExec(t, db, `UPDATE books SET timestamp = datetime('2020-01-01', '+' || id || ' minutes') WHERE id = ?`, id)
	return uuid
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// want is what the catalog should hold for a book, read the slow way.
type wantBook struct {
	title, authors string
	tags           []string
	series         string
	formats        []string
}

func readWant(t *testing.T, db *sql.DB, uuid string) wantBook {
	t.Helper()
	var w wantBook
	var tags, series, formats sql.NullString
	err := db.QueryRow(`SELECT b.title, `+bookAuthorsSQL+`,
			(SELECT group_concat(t.name, '|' ORDER BY l.id) FROM books_tags_link l JOIN tags t ON t.id = l.tag WHERE l.book = b.id),
			(SELECT s.name FROM books_series_link l JOIN series s ON s.id = l.series WHERE l.book = b.id),
			(SELECT group_concat(lower(format), '|') FROM data WHERE book = b.id)
		FROM books b WHERE b.uuid = ?`, uuid).Scan(&w.title, &w.authors, &tags, &series, &formats)
	if err != nil {
		t.Fatal(err)
	}
	if tags.String != "" {
		w.tags = strings.Split(tags.String, "|")
	}
	if formats.String != "" {
		w.formats = strings.Split(formats.String, "|")
		slices.Sort(w.formats)
	}
	w.series = series.String
	return w
}

func gotBook(c *catalog, b catalogBook) wantBook {
	g := wantBook{title: b.Title, authors: b.Book.Authors, series: c.seriesNames[b.Series], formats: slices.Sorted(slices.Values(b.Formats))}
	for _, id := range b.TagIDs {
		g.tags = append(g.tags, c.tagNames[id])
	}
	if len(g.formats) == 0 {
		g.formats = nil
	}
	return g
}

func checkCatalogMatchesDB(t *testing.T, db *sql.DB, c *catalog) {
	t.Helper()
	uuids, err := scanStrings(db.Query(`SELECT uuid FROM books`))
	if err != nil {
		t.Fatal(err)
	}
	if c.live() != len(uuids) {
		t.Errorf("catalog has %d books, metadata.db %d", c.live(), len(uuids))
	}
	for _, u := range uuids {
		b, ok := c.lookup(u)
		if !ok {
			t.Errorf("%s missing from the catalog", u)
			continue
		}
		want := readWant(t, db, u)
		if got := gotBook(c, b); !wantEqual(got, want) {
			t.Errorf("%s: catalog %+v, metadata.db %+v", u, got, want)
		}
	}
}

func wantEqual(a, b wantBook) bool {
	return a.title == b.title && a.authors == b.authors && a.series == b.series &&
		slices.Equal(a.tags, b.tags) && slices.Equal(a.formats, b.formats)
}

func TestBuildCatalog(t *testing.T) {
	db := catalogFixture(t)
	first := addTestBook(t, db, "Dune", []string{"Frank Herbert"}, []string{"Science Fiction", "Classics"}, "Dune", "epub", "pdf")
	addTestBook(t, db, "Good Omens", []string{"Terry Pratchett", "Neil Gaiman"}, []string{"Fantasy"}, "")
	last := addTestBook(t, db, "Untagged", []string{"Nobody"}, nil, "")

	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	checkCatalogMatchesDB(t, db, c)
	if c.books[0].UUID != last || c.books[len(c.books)-1].UUID != first {
		t.Errorf("not in Latest order: first %s, last %s", c.books[0].UUID, c.books[len(c.books)-1].UUID)
	}
}

// Changes read back only the books named, and leave the catalog equal to a
// fresh build.
func TestCatalogChanges(t *testing.T) {
	db := catalogFixture(t)
	a := addTestBook(t, db, "A", []string{"Ann"}, []string{"One"}, "S", "epub")
	b := addTestBook(t, db, "B", []string{"Bob"}, []string{"Two"}, "")
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)

	// An edit (new title, a new tag), a new book, a removal.
	mustExec(t, db, `UPDATE books SET title = 'A2' WHERE uuid = ?`, a)
	mustExec(t, db, `INSERT INTO tags (name) VALUES ('Brand New')`)
	mustExec(t, db, `INSERT INTO books_tags_link (book, tag) SELECT b.id, t.id FROM books b, tags t WHERE b.uuid = ? AND t.name = 'Brand New'`, a)
	added := addTestBook(t, db, "C", []string{"Cy"}, []string{"Two"}, "", "pdf")
	catalogChanged(a, added)
	catalogRemoved(b)
	before := libraryIO.Load()
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	if libraryIO.Load() == before {
		t.Error("changes should be read from metadata.db")
	}

	got := currentCatalog()
	if got == c {
		t.Fatal("catalog not replaced")
	}
	if b, _ := c.lookup(a); b.Title != "A" {
		t.Errorf("the old catalog changed: %q", b.Title)
	}
	if _, ok := got.lookup(b); ok {
		t.Error("removed book still listed")
	}
	// b is still in metadata.db here, so compare the rest with the database.
	mustExec(t, db, `DELETE FROM books_tags_link WHERE book = (SELECT id FROM books WHERE uuid = ?)`, b)
	mustExec(t, db, `DELETE FROM books_authors_link WHERE book = (SELECT id FROM books WHERE uuid = ?)`, b)
	mustExec(t, db, `DELETE FROM books WHERE uuid = ?`, b)
	checkCatalogMatchesDB(t, db, got)

	// Restored: back after a re-read. Not in metadata.db: removed.
	catalogChanged(b)
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	if _, ok := currentCatalog().lookup(b); ok {
		t.Error("a book gone from metadata.db should stay removed")
	}
}

func TestBooksByUUIDsFromCatalog(t *testing.T) {
	db := catalogFixture(t)
	a := addTestBook(t, db, "A", []string{"Ann"}, nil, "")
	b := addTestBook(t, db, "B", []string{"Bob", "Bea"}, nil, "")
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)
	late := addTestBook(t, db, "Late", []string{"Lee"}, nil, "") // not in the catalog yet

	before := libraryIO.Load()
	books, err := booksByUUIDs([]string{b, a})
	if err != nil {
		t.Fatal(err)
	}
	if libraryIO.Load() != before {
		t.Error("books in the catalog were read from metadata.db")
	}
	if len(books) != 2 || books[0].UUID != b || books[1].UUID != a || books[0].Authors != "Bob & Bea" {
		t.Errorf("got %+v", books)
	}
	// Unknown books are skipped (after asking metadata.db).
	if books, err := booksByUUIDs([]string{"no-such-book", a}); err != nil || len(books) != 1 {
		t.Errorf("with an unknown uuid: %+v, %v", books, err)
	}
	before = libraryIO.Load()
	if !bookExists(a) || libraryIO.Load() != before {
		t.Error("bookExists should answer from the catalog")
	}

	books, err = booksByUUIDs([]string{late, a})
	if err != nil || len(books) != 2 || books[0].Title != "Late" {
		t.Errorf("a book newer than the catalog: %+v, %v", books, err)
	}
}

func TestResumeURLFromCatalog(t *testing.T) {
	db := catalogFixture(t)
	both := addTestBook(t, db, "Both", []string{"A"}, nil, "", "pdf", "epub")
	pdf := addTestBook(t, db, "Pdf", []string{"A"}, nil, "", "pdf")
	mobi := addTestBook(t, db, "Mobi", []string{"A"}, nil, "", "mobi")
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)

	before := libraryIO.Load()
	for uuid, want := range map[string]string{both: "/read?uuid=" + both, pdf: "/read-pdf?uuid=" + pdf, mobi: ""} {
		if got := resumeURL(uuid, ""); got != want {
			t.Errorf("resumeURL(%s) = %q, want %q", uuid, got, want)
		}
	}
	if libraryIO.Load() != before {
		t.Error("resumeURL listed a book folder for a book with data rows")
	}
}

// A book without a cover is looked for on the library once, not on every
// thumbnail request; a new cover clears that.
func TestThumbRemembersMissingCover(t *testing.T) {
	db := catalogFixture(t)
	uuid := addTestBook(t, db, "Bare", []string{"A"}, nil, "")
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)
	b, _ := c.lookup(uuid)
	uuidPathIndex.Store(uuid, b.Path)
	modTimeCache.Store(uuid, b.LastModified)
	t.Cleanup(func() { forgetBook(uuid) })

	get := func() int {
		rec := httptest.NewRecorder()
		coverThumbHandler(rec, httptest.NewRequest(http.MethodGet, "/cover-thumb/"+uuid, nil))
		return rec.Code
	}
	if code := get(); code != http.StatusNotFound {
		t.Fatalf("first request: %d", code)
	}
	before := libraryIO.Load()
	if code := get(); code != http.StatusNotFound {
		t.Fatalf("second request: %d", code)
	}
	if libraryIO.Load() != before {
		t.Error("the missing cover was looked for again")
	}
	coverChanged(uuid)
	if _, missing := noCover.Load(uuid); missing {
		t.Error("a new cover should clear the missing-cover note")
	}
}

// The editor's Apply updates the catalog.
func TestApplyMetadataUpdatesCatalog(t *testing.T) {
	db := catalogFixture(t)
	uuid := addTestBook(t, db, "Before", []string{"A"}, nil, "")
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)
	b, _ := c.lookup(uuid)
	if err := os.MkdirAll(filepath.Join(baseDir, b.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir()) // generatePage1 writes ./pages

	mux := http.NewServeMux()
	mux.HandleFunc("POST /metadata/{uuid}/apply", applyMetadataHandler)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metadata/"+uuid+"/apply",
		strings.NewReader(`{"title": "After", "creators": "A", "tags": "Fresh"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	got, _ := currentCatalog().lookup(uuid)
	if got.Title != "After" {
		t.Errorf("catalog title %q after the editor's Apply", got.Title)
	}
	checkCatalogMatchesDB(t, db, currentCatalog())
}

// Readers never see a half-applied catalog (run with -race).
func TestCatalogConcurrentReads(t *testing.T) {
	db := catalogFixture(t)
	var uuids []string
	for i := range 50 {
		uuids = append(uuids, addTestBook(t, db, "Book "+string(rune('A'+i%26)), []string{"A"}, []string{"T"}, ""))
	}
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := booksByUUIDs(uuids); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	for i := range 20 {
		mustExec(t, db, `UPDATE books SET title = ? WHERE uuid = ?`, "Edited", uuids[i])
		catalogChanged(uuids[i])
		if err := catalogFlush(); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	checkCatalogMatchesDB(t, db, currentCatalog())
}
