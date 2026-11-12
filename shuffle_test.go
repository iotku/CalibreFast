package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// readShufflePages returns every book in pages/shuffle/, in order.
func readShufflePages(t *testing.T) []Book {
	t.Helper()
	var all []Book
	for page := 1; ; page++ {
		data, err := os.ReadFile(shufflePageFile(page))
		if os.IsNotExist(err) {
			return all
		}
		if err != nil {
			t.Fatal(err)
		}
		var books []Book
		if err := json.Unmarshal(data, &books); err != nil {
			t.Fatal(err)
		}
		all = append(all, books...)
	}
}

// Every book once, in the shuffle files and the count; uploads go at the
// end and leave the earlier files alone; removals take the book out.
func TestShufflePages(t *testing.T) {
	db := catalogFixture(t)
	t.Chdir(t.TempDir())
	var uuids []string
	for i := range booksPerPage + 10 {
		uuids = append(uuids, addTestBook(t, db, "Book "+strconv.Itoa(i), []string{"A"}, nil, ""))
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}

	books := readShufflePages(t)
	seen := map[string]int{}
	for _, b := range books {
		seen[b.UUID]++
	}
	if len(books) != len(uuids) || len(seen) != len(uuids) || shuffleTotal.Load() != int64(len(uuids)) {
		t.Fatalf("%d books in the shuffle files (%d distinct), shuffleTotal %d, want %d", len(books), len(seen), shuffleTotal.Load(), len(uuids))
	}
	inOrder := true
	for i, b := range books[:20] {
		inOrder = inOrder && b.UUID == uuids[len(uuids)-1-i]
	}
	if inOrder {
		t.Error("the shuffle is in Latest order")
	}

	// An upload: at the end; page 1 untouched.
	page1, _ := os.ReadFile(shufflePageFile(1))
	added := addTestBook(t, db, "New", []string{"A"}, nil, "")
	catalogChanged(added)
	refreshLibraryPages()
	books = readShufflePages(t)
	if len(books) != len(uuids)+1 || books[len(books)-1].UUID != added {
		t.Errorf("after an upload: %d books, last %s", len(books), books[len(books)-1].UUID)
	}
	if now, _ := os.ReadFile(shufflePageFile(1)); !bytes.Equal(now, page1) {
		t.Error("an upload rewrote shuffle page 1")
	}

	// A removal: gone from the files and the count.
	gone := books[3].UUID
	catalogRemoved(gone)
	refreshShufflePages()
	for _, b := range readShufflePages(t) {
		if b.UUID == gone {
			t.Error("a removed book is still in the shuffle files")
		}
	}
	if shuffleTotal.Load() != int64(len(uuids)) {
		t.Errorf("shuffleTotal %d after a removal", shuffleTotal.Load())
	}
}

// Signed in, /shuffle leaves out the books the user finished.
func TestShuffleListSkipsFinished(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	var uuids []string
	for i := range 5 {
		uuids = append(uuids, addTestBook(t, db, "Book "+strconv.Itoa(i), []string{"A"}, nil, ""))
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	u, err := createUser("reader", "a long enough password", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range uuids[:2] {
		if err := setStatus(u.ID, id, "finished", "manual"); err != nil {
			t.Fatal(err)
		}
	}
	if err := setStatus(u.ID, uuids[2], "reading", "manual"); err != nil {
		t.Fatal(err)
	}

	l := shuffleList(u)
	if n, err := l.Count(); err != nil || n != 3 {
		t.Fatalf("count %d, %v; want 3", n, err)
	}
	first, _ := l.Page(2, 0)
	rest, _ := l.Page(2, 2)
	got := append(first, rest...)
	if len(got) != 3 {
		t.Fatalf("pages %v %v", first, rest)
	}
	for _, id := range got {
		if id == uuids[0] || id == uuids[1] {
			t.Errorf("finished book %s listed", id)
		}
	}

	// The page itself: signed in, the per-user list; signed out, the files.
	rec := httptest.NewRecorder()
	shuffleHandler(rec, asUser(httptest.NewRequest(http.MethodGet, "/shuffle", nil), u))
	if body := rec.Body.String(); !strings.Contains(body, `data-page-url="/shuffle?api=1&page={n}"`) || !strings.Contains(body, `data-total-books="3"`) {
		t.Errorf("signed-in page: %s", body)
	}
	rec = httptest.NewRecorder()
	shuffleHandler(rec, httptest.NewRequest(http.MethodGet, "/shuffle", nil))
	if body := rec.Body.String(); !strings.Contains(body, `data-page-url="/pages/shuffle/page{n}.json?v=`) || !strings.Contains(body, `data-total-books="5"`) {
		t.Errorf("signed-out page: %s", body)
	}
	rec = httptest.NewRecorder()
	latestHandler(rec, httptest.NewRequest(http.MethodGet, "/latest", nil))
	if body := rec.Body.String(); !strings.Contains(body, `data-page-url="/pages/page{n}.json?v=`) {
		t.Errorf("latest page: %s", body)
	}
}
