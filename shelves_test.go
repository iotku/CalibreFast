package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func shelfMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /shelf/{id}", shelfPageHandler)
	mux.HandleFunc("GET /api/shelves", shelvesListHandler)
	mux.HandleFunc("POST /api/shelves", requireUser(shelfCreateHandler))
	mux.HandleFunc("PATCH /api/shelves/{id}", requireUser(shelfUpdateHandler))
	mux.HandleFunc("DELETE /api/shelves/{id}", requireUser(shelfDeleteHandler))
	mux.HandleFunc("PUT /api/shelves/{id}/books/{uuid}", requireUser(shelfAddBookHandler))
	mux.HandleFunc("DELETE /api/shelves/{id}/books/{uuid}", requireUser(shelfRemoveBookHandler))
	return mux
}

func TestShelvesPermissions(t *testing.T) {
	alice, books := setupLibraryAndUser(t, 2)
	bob, _ := createUser("bob", "longenough-pw", false)
	admin, _ := createUser("boss", "longenough-pw", true)
	mux := shelfMux()

	create := func(who *User, body string, want int) Shelf {
		rec := call(t, mux, who, "POST", "/api/shelves", body)
		if rec.Code != want {
			t.Fatalf("create %s: %d %s", body, rec.Code, rec.Body)
		}
		var s Shelf
		_ = json.Unmarshal(rec.Body.Bytes(), &s)
		return s
	}

	mine := create(alice, `{"name":"To read"}`, http.StatusCreated)
	create(alice, `{"name":"   "}`, http.StatusBadRequest)
	create(bob, `{"name":"Hack","global":true}`, http.StatusForbidden)
	create(nil, `{"name":"x"}`, http.StatusUnauthorized)
	global := create(admin, `{"name":"Staff picks","global":true}`, http.StatusCreated)

	path := func(s Shelf) string { return "/api/shelves/" + itoa(s.ID) }

	// Personal shelf: owner only; others get 404 (existence isn't leaked).
	if c := call(t, mux, alice, "PUT", path(mine)+"/books/"+books[0], "").Code; c != http.StatusNoContent {
		t.Fatalf("owner add: %d", c)
	}
	if c := call(t, mux, bob, "PUT", path(mine)+"/books/"+books[1], "").Code; c != http.StatusNotFound {
		t.Fatalf("other user add to personal shelf: %d", c)
	}
	if c := call(t, mux, bob, "GET", "/shelf/"+itoa(mine.ID)+"?api=1", "").Code; c != http.StatusNotFound {
		t.Fatalf("other user view personal shelf: %d", c)
	}
	if c := call(t, mux, nil, "GET", "/shelf/"+itoa(mine.ID), "").Code; c != http.StatusNotFound {
		t.Fatalf("anonymous view personal shelf: %d", c)
	}
	if c := call(t, mux, alice, "PUT", path(mine)+"/books/not-a-book", "").Code; c != http.StatusNotFound {
		t.Fatalf("unknown book: %d", c)
	}

	// Global shelf: everyone can view, only admins edit.
	if c := call(t, mux, admin, "PUT", path(global)+"/books/"+books[1], "").Code; c != http.StatusNoContent {
		t.Fatalf("admin add to global: %d", c)
	}
	if c := call(t, mux, bob, "PUT", path(global)+"/books/"+books[0], "").Code; c != http.StatusForbidden {
		t.Fatalf("non-admin add to global: %d", c)
	}
	if c := call(t, mux, bob, "PATCH", path(global), `{"name":"mine now"}`).Code; c != http.StatusForbidden {
		t.Fatalf("non-admin rename global: %d", c)
	}
	var shown []Book
	_ = json.Unmarshal(call(t, mux, nil, "GET", "/shelf/"+itoa(global.ID)+"?api=1&page=1", "").Body.Bytes(), &shown)
	if len(shown) != 1 || shown[0].UUID != books[1] {
		t.Fatalf("anonymous view of global shelf: %+v", shown)
	}

	// The book popup offers own shelves + editable globals; read-only globals
	// only when the book is already on them.
	marks := shelfMarks(bob, books[0])
	if len(marks) != 0 {
		t.Fatalf("bob should see no shelves for book 0: %+v", marks)
	}
	marks = shelfMarks(bob, books[1])
	if len(marks) != 1 || !marks[0].Global || marks[0].CanEdit || !marks[0].Has {
		t.Fatalf("bob, book 1 on a global shelf: %+v", marks)
	}

	// Lists are per user.
	var lists struct{ Mine, Global []Shelf }
	_ = json.Unmarshal(call(t, mux, bob, "GET", "/api/shelves", "").Body.Bytes(), &lists)
	if len(lists.Mine) != 0 || len(lists.Global) != 1 {
		t.Fatalf("bob's shelves: %+v", lists)
	}

	// Deleting a user deletes their shelves (not global ones).
	if _, err := appDB.Exec(`DELETE FROM users WHERE id = ?`, alice.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM shelves`).Scan(&n)
	if n != 1 {
		t.Fatalf("expected only the global shelf left, got %d", n)
	}
}
