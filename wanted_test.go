package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// "Want to read" is a status before reading: starting the book (in a web
// reader or on a KOReader device) takes it off the list.
func TestWantedStatus(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	b := books[0]
	if err := setStatus(u.ID, b, "wanted", "manual"); err != nil {
		t.Fatal(err)
	}
	if s, _ := readStatus(u.ID, b); s != "wanted" {
		t.Fatalf("status %q, want wanted", s)
	}
	_ = setStatus(u.ID, b, "wanted", "auto") // never set automatically, and no-op
	_ = setStatus(u.ID, b, "reading", "auto")
	if s, src := readStatus(u.ID, b); s != "reading" || src != "auto" {
		t.Fatalf("starting a wanted book: %s/%s, want reading/auto", s, src)
	}
	_ = setStatus(u.ID, b, "", "manual")
	if s, _ := readStatus(u.ID, b); s != "" {
		t.Fatalf("cleared: %q", s)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /me/{list}", requireUser(meListHandler))
	_ = setStatus(u.ID, b, "wanted", "manual")
	var got []Book
	rec := call(t, mux, u, "GET", "/me/wanted?api=1&page=1", "")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || len(got) != 1 || got[0].UUID != b {
		t.Fatalf("/me/wanted: %d %s", rec.Code, rec.Body)
	}
}

// The grid's marks: favorites, wanted, finished, and where to resume a book
// being read.
func TestReadingMarks(t *testing.T) {
	u, books := setupLibraryAndUser(t, 4)
	wanted, reading, finished, noFile := books[0], books[1], books[2], books[3]
	_ = setStatus(u.ID, wanted, "wanted", "manual")
	_ = setStatus(u.ID, reading, "reading", "manual")
	_ = setStatus(u.ID, finished, "finished", "manual")
	_ = setStatus(u.ID, noFile, "reading", "manual")
	if _, err := appDB.Exec(`INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at) VALUES (?, ?, 'pdf', '3', 0.5, 1)`, u.ID, reading); err != nil {
		t.Fatal(err)
	}
	if _, err := appDB.Exec(`INSERT INTO favorites (user_id, book_uuid, created_at) VALUES (?, ?, 1)`, u.ID, wanted); err != nil {
		t.Fatal(err)
	}

	var m struct {
		Reading   map[string]*float64
		Resume    map[string]string
		Wanted    []string
		Finished  []string
		Favorites []string
	}
	rec := call(t, userActionsMux(), u, "GET", "/api/me/reading", "")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &m) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(m.Wanted) != 1 || m.Wanted[0] != wanted || len(m.Finished) != 1 || m.Finished[0] != finished ||
		len(m.Favorites) != 1 || m.Favorites[0] != wanted || len(m.Reading) != 2 {
		t.Fatalf("marks: %s", rec.Body)
	}
	// Resumes in the format last read; a book with no readable file has nowhere to resume.
	if got := m.Resume[reading]; got != "/read-pdf?uuid="+reading {
		t.Errorf("resume: %q", got)
	}
	if _, ok := m.Resume[noFile]; ok {
		t.Errorf("resume for a book without EPUB/PDF: %s", rec.Body)
	}
}

// Upgrading keeps reading statuses, and allows "wanted".
func TestMigrationWantedStatus(t *testing.T) {
	dir := t.TempDir()
	all := migrations
	migrations = all[:6]
	db, err := openAppDB(dir)
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, created_at) VALUES (1, 'reader', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO reading_status VALUES (1, 'book-a', 'finished', 'manual', 5)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err = openAppDB(dir); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer db.Close()
	var status string
	if err := db.QueryRow(`SELECT status FROM reading_status WHERE book_uuid = 'book-a'`).Scan(&status); err != nil || status != "finished" {
		t.Fatalf("kept status: %q %v", status, err)
	}
	if _, err := db.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (1, 'book-b', 'wanted', 'manual', 6)`); err != nil {
		t.Fatalf("wanted: %v", err)
	}
	// Deleting the user still deletes their statuses (the foreign key survived).
	if _, err := db.Exec(`DELETE FROM users WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM reading_status`).Scan(&n)
	if n != 0 {
		t.Errorf("%d statuses left after deleting their user", n)
	}
}
