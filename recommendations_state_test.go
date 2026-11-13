package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestRecommendedState(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	var b []string
	for i := range 30 {
		b = append(b, addTestBook(t, db, "Book "+strconv.Itoa(i), []string{"A"}, []string{"Space"}, "", "epub"))
		// Other books, so Space is a tag that says something.
		addTestBook(t, db, "Other "+strconv.Itoa(i), []string{"B"}, []string{"Other"}, "", "epub")
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	u, err := createUser("reader", "a long enough password", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appDB.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (?, ?, 'wanted', 'manual', 1)`, u.ID, b[0]); err != nil {
		t.Fatal(err)
	}
	ids := func(recs []recBook) string {
		var s []string
		for _, r := range recs {
			s = append(s, r.Book.UUID)
		}
		return strings.Join(s, ",")
	}
	home := func() []recBook {
		t.Helper()
		rec := httptest.NewRecorder()
		homeHandler(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/me/home", nil), u))
		var h homeAnswer
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &h) != nil {
			t.Fatalf("home: %d %s", rec.Code, rec.Body)
		}
		return h.Recommended
	}
	next := func() []recBook {
		t.Helper()
		rec := httptest.NewRecorder()
		recNextHandler(rec, asUser(httptest.NewRequest(http.MethodPost, "/api/me/recommended/next", nil), u))
		var a recNextAnswer
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &a) != nil {
			t.Fatalf("next: %d %s", rec.Code, rec.Body)
		}
		return a.Recommended
	}

	// The same row on every load.
	first := home()
	if len(first) != recRowMax {
		t.Fatalf("first page: %d books", len(first))
	}
	if again := home(); ids(again) != ids(first) {
		t.Fatalf("the row changed between loads:\n%s\n%s", ids(first), ids(again))
	}

	// Not interested: gone, for good.
	gone := first[3].Book.UUID
	rec := httptest.NewRecorder()
	req := asUser(httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"not_interested":true}`)), u)
	req.SetPathValue("uuid", gone)
	setNotInterestedHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("not interested: %d %s", rec.Code, rec.Body)
	}
	after := home()
	if strings.Contains(ids(after), gone) {
		t.Fatal("a book the user isn't interested in was recommended")
	}

	// 28 books left (30, less the wanted one and the hidden one): the second
	// page has the other 4, then back to the first.
	second := next()
	if len(second) != 28-recRowMax {
		t.Fatalf("second page: %d books", len(second))
	}
	for _, r := range second {
		if strings.Contains(ids(after), r.Book.UUID) || r.Book.UUID == gone {
			t.Fatalf("second page repeats the first or the hidden book: %s", r.Book.UUID)
		}
	}
	if ids(home()) != ids(second) {
		t.Fatal("the page asked for isn't the one loaded next")
	}
	if wrapped := next(); len(wrapped) != recRowMax || strings.Contains(ids(wrapped), gone) {
		t.Fatalf("wrapped: %d books", len(wrapped))
	}
	var page int
	if err := appDB.QueryRow(`SELECT page FROM rec_state WHERE user_id = ?`, u.ID).Scan(&page); err != nil || page != 0 {
		t.Fatalf("page after wrapping: %d %v", page, err)
	}
}
