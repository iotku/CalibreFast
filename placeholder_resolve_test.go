package main

import (
	"net/http"
	"testing"
)

// A library book edited to be a placeholder's book replaces it: same title
// by one of its authors, same ISBN, or same title, series and number.
// Nothing else does.
func TestPlaceholderResolvedByEdit(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	origData := dataDir
	dataDir = t.TempDir()
	placeholdersChanged()
	t.Cleanup(func() { dataDir = origData; placeholdersChanged() })
	save := func(m OPFMetadata) *placeholder {
		t.Helper()
		p := &placeholder{Meta: m}
		if err := savePlaceholder(p, nil); err != nil {
			t.Fatal(err)
		}
		return p
	}
	byAuthor := save(OPFMetadata{Title: "Mort", Creators: []string{"Terry Pratchett"}, Series: "Discworld", SeriesIndex: "4"})
	byISBN := save(OPFMetadata{Title: "Pyramids", Creators: []string{"Someone"}, Identifiers: []opfIdentifier{{Scheme: "ISBN", Value: "978-0-552-15293-8"}}})
	bySeries := save(OPFMetadata{Title: "Eric", Creators: []string{"Nobody"}, Series: "Discworld", SeriesIndex: "9"})
	kept := save(OPFMetadata{Title: "Sourcery", Creators: []string{"Terry Pratchett"}, Series: "Discworld", SeriesIndex: "5"})

	a := addTestBook(t, db, "Untitled", []string{"Terry Pratchett"}, nil, "")
	b := addTestBook(t, db, "Something", []string{"Anyone"}, nil, "")
	c := addTestBook(t, db, "Eric", []string{"Pratchett, Terry"}, nil, "")
	d := addTestBook(t, db, "Other", []string{"Terry Pratchett"}, nil, "")
	cat, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(cat)
	there := func(p *placeholder) bool { return placeholderByUUID(p.UUID) != nil }
	flush := func(uuids ...string) {
		t.Helper()
		catalogChanged(uuids...)
		if err := catalogFlush(); err != nil {
			t.Fatal(err)
		}
	}

	// Edits that match nothing leave them all.
	mustExec(t, db, `UPDATE books SET title = 'Still Untitled' WHERE uuid = ?`, a)
	flush(a, b, c, d)
	for _, p := range []*placeholder{byAuthor, byISBN, bySeries, kept} {
		if !there(p) {
			t.Fatalf("%q went on an unrelated edit", p.Meta.Title)
		}
	}

	// Title and author (a subtitle aside).
	mustExec(t, db, `UPDATE books SET title = 'Mort: A Discworld Novel' WHERE uuid = ?`, a)
	flush(a)
	if there(byAuthor) {
		t.Error("title and author: placeholder kept")
	}

	// ISBN (any form of it).
	mustExec(t, db, `INSERT INTO identifiers (book, type, val) SELECT id, 'isbn', '9780552152938' FROM books WHERE uuid = ?`, b)
	flush(b)
	if there(byISBN) {
		t.Error("ISBN: placeholder kept")
	}

	// Title, series and number (the authors differ).
	mustExec(t, db, `INSERT OR IGNORE INTO series (name) VALUES ('Discworld')`)
	mustExec(t, db, `INSERT INTO books_series_link (book, series) SELECT b.id, s.id FROM books b, series s WHERE b.uuid = ? AND s.name = 'Discworld'`, c)
	mustExec(t, db, `UPDATE books SET series_index = 9 WHERE uuid = ?`, c)
	flush(c)
	if there(bySeries) {
		t.Error("title, series and number: placeholder kept")
	}

	// Series and number alone (another title): not enough.
	mustExec(t, db, `INSERT INTO books_series_link (book, series) SELECT b.id, s.id FROM books b, series s WHERE b.uuid = ? AND s.name = 'Discworld'`, d)
	mustExec(t, db, `UPDATE books SET series_index = 5 WHERE uuid = ?`, d)
	flush(d)
	if !there(kept) {
		t.Error("series and number alone replaced a placeholder")
	}

	// Their pages lead to the book that replaced them; a placeholder removed
	// by hand has none.
	mux := placeholderMux()
	for p, book := range map[*placeholder]string{byAuthor: a, byISBN: b, bySeries: c} {
		rec := call(t, mux, nil, "GET", "/missing/"+p.UUID, "")
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/book/get/"+book {
			t.Errorf("%q: %d %q", p.Meta.Title, rec.Code, rec.Header().Get("Location"))
		}
	}
	if err := deletePlaceholder(kept.UUID); err != nil {
		t.Fatal(err)
	}
	if rec := call(t, mux, nil, "GET", "/missing/"+kept.UUID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("removed by hand: %d", rec.Code)
	}
	// Restored (an undo): its own page again.
	if err := putPlaceholder(byAuthor.UUID, snapPlaceholder(byAuthor)); err != nil {
		t.Fatal(err)
	}
	if rec := call(t, mux, nil, "GET", "/missing/"+byAuthor.UUID, ""); rec.Code != http.StatusOK {
		t.Errorf("restored: %d", rec.Code)
	}

	// It's in the edit log, so it can be undone.
	var n int
	appDB.QueryRow(`SELECT COUNT(*) FROM edit_log WHERE action = 'placeholder in library'`).Scan(&n)
	if n != 3 {
		t.Errorf("%d edit log entries, want 3", n)
	}
}
