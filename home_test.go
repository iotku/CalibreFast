package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHomeAnswer(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	var b []string
	for i := range 6 {
		b = append(b, addTestBook(t, db, "Book "+strconv.Itoa(i), []string{"A"}, nil, "", "epub"))
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	u, err := createUser("reader", "a long enough password", false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	setAt := func(uuid, status string, at int64) {
		t.Helper()
		if _, err := appDB.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (?, ?, ?, 'manual', ?)`, u.ID, uuid, status, at); err != nil {
			t.Fatal(err)
		}
	}
	// Reading: b0 marked long ago but read a minute ago; b1 marked an hour ago.
	setAt(b[0], "reading", now-86400)
	setAt(b[1], "reading", now-3600)
	if _, err := appDB.Exec(`INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at) VALUES (?, ?, 'epub', 'x', 0.42, ?)`, u.ID, b[0], (now-60)*1000); err != nil {
		t.Fatal(err)
	}
	setAt(b[2], "wanted", now-100)
	setAt(b[3], "wanted", now-10)
	setAt(b[4], "finished", now)

	io := libraryIO.Load()
	rec := httptest.NewRecorder()
	homeHandler(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/me/home", nil), u))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if libraryIO.Load() != io {
		t.Error("/api/me/home read the library")
	}
	var h homeAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	ids := func(books []Book) string {
		var s []string
		for _, x := range books {
			s = append(s, x.UUID)
		}
		return strings.Join(s, ",")
	}
	if got, want := ids(h.Reading), b[0]+","+b[1]; got != want {
		t.Errorf("reading %s, want %s (most recently active first)", got, want)
	}
	if got, want := ids(h.Wanted), b[3]+","+b[2]; got != want {
		t.Errorf("wanted %s, want %s (newest first)", got, want)
	}
	if h.ReadingCount != 2 || h.WantedCount != 2 {
		t.Errorf("counts %d %d", h.ReadingCount, h.WantedCount)
	}
	if p := h.Marks.Reading[b[0]]; p == nil || *p != 0.42 || h.Marks.Resume[b[0]] != "/read?uuid="+b[0] || len(h.Marks.Finished) != 1 {
		t.Errorf("marks %+v", h.Marks)
	}
	if h.Reading[0].Title != "Book 0" {
		t.Errorf("book details %+v", h.Reading[0])
	}
}

func TestBooksAPI(t *testing.T) {
	db := catalogFixture(t)
	t.Chdir(t.TempDir())
	a := addTestBook(t, db, "A", []string{"Ann"}, nil, "")
	c := addTestBook(t, db, "C", []string{"Cy"}, nil, "")
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	get := func(q string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		booksHandler(rec, httptest.NewRequest(http.MethodGet, "/api/books?uuids="+q, nil))
		return rec
	}
	io := libraryIO.Load()
	rec := get(c + ",not-a-book," + a + "," + c)
	var books []Book
	if err := json.Unmarshal(rec.Body.Bytes(), &books); err != nil {
		t.Fatal(err)
	}
	if len(books) != 2 || books[0].UUID != c || books[1].UUID != a {
		t.Errorf("got %+v", books)
	}
	if libraryIO.Load() != io {
		t.Error("/api/books read the library (an unknown id must not cost a read)")
	}
	var many []string
	for i := range maxBookLookup + 1 {
		many = append(many, "id"+strconv.Itoa(i))
	}
	if rec := get(strings.Join(many, ",")); rec.Code != http.StatusBadRequest {
		t.Errorf("too many: %d", rec.Code)
	}
	if rec := get(""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("none: %d %q", rec.Code, rec.Body)
	}
}
