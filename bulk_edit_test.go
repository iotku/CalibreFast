package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
)

// addOPFTestBook is addRecycleTestBook with a real metadata.opf, which the
// editors start from.
func addOPFTestBook(t *testing.T, db *sql.DB, title string, authors, tags, langs []string) testBook {
	t.Helper()
	b := addRecycleTestBook(t, db, title, authors, tags)
	meta := OPFMetadata{Title: title, Creators: authors, Subjects: tags, Languages: langs, Publisher: "Pub " + title, Description: "About " + title}
	if _, err := updateBookInCalibreDB(db, baseDir, b.uuid, &meta); err != nil {
		t.Fatal(err)
	}
	if err := writeOPF(&OPF{Metadata: meta}, filepath.Join(baseDir, b.path, "metadata.opf"), b.uuid); err != nil {
		t.Fatal(err)
	}
	return b
}

func tableRow(t *testing.T, db *sql.DB, uuid string) tableBook {
	t.Helper()
	rows, err := tableBooks(db, []string{uuid})
	if err != nil || len(rows) != 1 {
		t.Fatalf("table row for %s: %v (%d rows)", uuid, err, len(rows))
	}
	return rows[0]
}

func TestBulkEditTagsAndLanguages(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, []string{"Fiction", "fantasy", "bad tag"}, []string{"eng"})
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, []string{"Bad Tag", "History"}, []string{"en", "fra"})
	c := addOPFTestBook(t, db, "Gamma", []string{"Cat"}, []string{"History"}, []string{"deu"})

	res := bulkEditBooks([]string{a.uuid, b.uuid, c.uuid}, &bulkEdit{
		RemoveTags:      []string{"BAD TAG"},
		AddTags:         []string{"Fantasy", "Checked"},
		RemoveLanguages: []string{"eng"},
		AddLanguages:    []string{"spa"},
	})
	if res.Changed != 3 || len(res.Failed) != 0 {
		t.Fatalf("result %+v", res)
	}
	if got := tableRow(t, db, a.uuid); !slices.Equal(got.Tags, []string{"Checked", "Fiction", "fantasy"}) && len(got.Tags) != 3 {
		t.Errorf("Alpha's tags %v", got.Tags)
	}
	ra := tableRow(t, db, a.uuid)
	if slices.ContainsFunc(ra.Tags, func(s string) bool { return s == "bad tag" }) || !slices.Contains(ra.Tags, "Checked") {
		t.Errorf("Alpha's tags %v", ra.Tags)
	}
	if !slices.Equal(ra.Languages, []string{"spa"}) {
		t.Errorf("Alpha's languages %v, want [spa] (eng removed)", ra.Languages)
	}
	rb := tableRow(t, db, b.uuid)
	if slices.Contains(rb.Tags, "Bad Tag") {
		t.Errorf("Beta still has Bad Tag: %v", rb.Tags)
	}
	// "en" is English too: removing eng removes it.
	if !slices.Equal(rb.Languages, []string{"fra", "spa"}) {
		t.Errorf("Beta's languages %v, want [fra spa]", rb.Languages)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM tags WHERE name = 'bad tag' COLLATE NOCASE`); n != 0 {
		t.Errorf("unused tag kept")
	}
	// The OPF follows, and the rest of it is as it was.
	m, err := bookEditMetadata(b.uuid)
	if err != nil {
		t.Fatal(err)
	}
	if m.Description != "About Beta" || m.Title != "Beta" || slices.Contains(m.Subjects, "Bad Tag") {
		t.Errorf("Beta's OPF %+v", m)
	}

	// Again: nothing to do, nothing written.
	res = bulkEditBooks([]string{a.uuid, b.uuid}, &bulkEdit{RemoveTags: []string{"bad tag"}})
	if res.Changed != 0 || res.Unchanged != 2 {
		t.Errorf("second run %+v", res)
	}
}

func TestBulkEditFields(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, []string{"und"})
	title, authors, pub, date, tags := "Alpha Two", "Ann One & Bo Two", "", "2001-02-03", "x, y, X"
	res := bulkEditBooks([]string{a.uuid}, &bulkEdit{Title: &title, Authors: &authors, Publisher: &pub, Date: &date, Tags: &tags})
	if res.Changed != 1 || len(res.Books) != 1 {
		t.Fatalf("result %+v", res)
	}
	r := res.Books[0]
	if r.Title != "Alpha Two" || r.Authors != "Ann One & Bo Two" || r.Publisher != "" || r.PubDate != "2001-02-03" || !slices.Equal(r.Tags, []string{"x", "y"}) {
		t.Errorf("row %+v", r)
	}
	// "und" isn't stored in metadata.db.
	if len(r.Languages) != 0 {
		t.Errorf("languages %v", r.Languages)
	}
}

func TestBulkEditHTTP(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, []string{"t1"}, nil)

	rec := httptest.NewRecorder()
	libraryTableHandler(rec, httptest.NewRequest("GET", "/api/library/table", nil))
	var table struct{ Books []tableBook }
	if err := json.Unmarshal(rec.Body.Bytes(), &table); err != nil || len(table.Books) != 1 || table.Books[0].UUID != a.uuid {
		t.Fatalf("table: %v %s", err, rec.Body)
	}
	if !slices.Equal(table.Books[0].Formats, []string{"epub"}) {
		t.Errorf("formats %v", table.Books[0].Formats)
	}

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/library/bulk", bytes.NewBufferString(body))
		req = req.WithContext(withUserCtx(req, &User{Username: "admin", IsAdmin: true}))
		bulkEditHandler(rec, req)
		return rec
	}
	if rec := post(`{"uuids":[],"edit":{"add_tags":["x"]}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no books: %d", rec.Code)
	}
	if rec := post(`{"uuids":["` + a.uuid + `"],"edit":{}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no edit: %d", rec.Code)
	}
	if rec := post(`{"uuids":["../x"],"edit":{"add_tags":["x"]}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad uuid: %d", rec.Code)
	}
	rec = post(`{"uuids":["` + a.uuid + `","00000000-0000-0000-0000-000000000000"],"edit":{"add_tags":["x"]}}`)
	var res bulkResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if res.Changed != 1 || len(res.Failed) != 1 || res.Failed[0].UUID != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("result %+v", res)
	}
}

func TestRefreshInPages(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, nil, nil)
	pagesMu.Lock()
	if err := writePage(3, []Book{{Title: "Alpha", UUID: a.uuid}, {Title: "Beta", UUID: b.uuid}}); err != nil {
		t.Fatal(err)
	}
	notePage(a.uuid, 3)
	notePage(b.uuid, 3)
	pagesMu.Unlock()

	title := "Alpha Renamed"
	bulkEditBooks([]string{a.uuid}, &bulkEdit{Title: &title})
	books, err := readPage(3)
	if err != nil {
		t.Fatal(err)
	}
	if books[0].Title != "Alpha Renamed" || books[0].Authors != "Ann" || books[1].Title != "Beta" {
		t.Errorf("page 3 %+v", books)
	}
}

// Calibre's books_publishers_link is UNIQUE(book): a new publisher replaces
// the old one rather than being ignored (and the book left with none).
func TestBulkEditPublisherWithCalibreUniqueLink(t *testing.T) {
	db := setupRecycleLibrary(t)
	if _, err := db.Exec(`CREATE UNIQUE INDEX calibre_publisher_per_book ON books_publishers_link(book)`); err != nil {
		t.Fatal(err)
	}
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	pub := "Ace"
	if res := bulkEditBooks([]string{a.uuid}, &bulkEdit{Publisher: &pub}); res.Changed != 1 {
		t.Fatalf("result %+v", res)
	}
	if got := tableRow(t, db, a.uuid).Publisher; got != "Ace" {
		t.Errorf("publisher %q, want Ace", got)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM publishers WHERE name = 'Pub Alpha'`); n != 0 {
		t.Error("the old publisher, now unused, was kept")
	}
}

// A book whose files metadata.db doesn't list: the table shows what's in
// its folder, as the book page and downloads do, and says so.
func TestTableFormatsFromFolder(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, nil, nil)
	if _, err := db.Exec(`DELETE FROM data WHERE book = ?`, b.id); err != nil {
		t.Fatal(err)
	}
	formatCache.Delete(b.uuid)
	if r := tableRow(t, db, a.uuid); r.FormatsFromFolder || !slices.Equal(r.Formats, []string{"epub"}) {
		t.Errorf("listed book %+v", r)
	}
	if r := tableRow(t, db, b.uuid); !r.FormatsFromFolder || !slices.Equal(r.Formats, []string{"epub"}) {
		t.Errorf("unlisted book: formats %v, from folder %v", r.Formats, r.FormatsFromFolder)
	}
}
