package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func listOrderMux() *http.ServeMux {
	mux := shelfMux()
	mux.HandleFunc("GET /me/{list}", requireUser(meListHandler))
	mux.HandleFunc("PUT /api/me/books/{uuid}/favorite", requireUser(setFavoriteHandler))
	mux.HandleFunc("PUT /api/me/books/{uuid}/status", requireUser(setStatusHandler))
	mux.HandleFunc("PUT /api/me/lists/{list}/order", requireUser(listOrderHandler))
	mux.HandleFunc("PUT /api/shelves/{id}/order", requireUser(shelfBookOrderHandler))
	return mux
}

func listUUIDs(t *testing.T, mux *http.ServeMux, u *User, path string) []string {
	t.Helper()
	rec := call(t, mux, u, "GET", path+"?api=1&all=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
	}
	var books []Book
	_ = json.Unmarshal(rec.Body.Bytes(), &books)
	out := []string{}
	for _, b := range books {
		out = append(out, b.UUID)
	}
	return out
}

func orderBody(uuids ...string) string {
	b, _ := json.Marshal(map[string][]string{"order": uuids})
	return string(b)
}

func TestListOrder(t *testing.T) {
	alice, books := setupLibraryAndUser(t, 4)
	bob, _ := createUser("bob", "longenough-pw", false)
	mux := listOrderMux()
	a, b, c, d := books[0], books[1], books[2], books[3]

	// Favorites: newest first until arranged.
	for i, u := range []string{a, b, c} {
		call(t, mux, alice, "PUT", "/api/me/books/"+u+"/favorite", `{"favorite":true}`)
		_, _ = appDB.Exec(`UPDATE favorites SET created_at = ? WHERE book_uuid = ?`, 100+i, u)
	}
	if got := listUUIDs(t, mux, alice, "/me/favorites"); !slices.Equal(got, []string{c, b, a}) {
		t.Fatalf("default order: %v", got)
	}
	if rec := call(t, mux, alice, "PUT", "/api/me/lists/favorites/order", orderBody(a, c, b)); rec.Code != http.StatusNoContent {
		t.Fatalf("arrange: %d %s", rec.Code, rec.Body)
	}
	if got := listUUIDs(t, mux, alice, "/me/favorites"); !slices.Equal(got, []string{a, c, b}) {
		t.Fatalf("arranged: %v", got)
	}
	// A new favorite comes first, the rest keep their places.
	call(t, mux, alice, "PUT", "/api/me/books/"+d+"/favorite", `{"favorite":true}`)
	if got := listUUIDs(t, mux, alice, "/me/favorites"); !slices.Equal(got, []string{d, a, c, b}) {
		t.Fatalf("after adding: %v", got)
	}
	// Bob's order doesn't touch Alice's.
	call(t, mux, bob, "PUT", "/api/me/lists/favorites/order", orderBody(b, a))
	if got := listUUIDs(t, mux, alice, "/me/favorites"); !slices.Equal(got, []string{d, a, c, b}) {
		t.Fatalf("after bob: %v", got)
	}

	// Bad orders and lists.
	for _, tc := range []struct{ path, body string }{
		{"/api/me/lists/favorites/order", orderBody(a, a)},
		{"/api/me/lists/favorites/order", `{"order":[""]}`},
		{"/api/me/lists/favorites/order", `nope`},
	} {
		if c := call(t, mux, alice, "PUT", tc.path, tc.body).Code; c != http.StatusBadRequest {
			t.Errorf("%s %s: %d", tc.path, tc.body, c)
		}
	}
	if c := call(t, mux, alice, "PUT", "/api/me/lists/reading/order", orderBody(a)).Code; c != http.StatusNotFound {
		t.Errorf("reading isn't arranged: %d", c)
	}

	// Want to read: arranged; a book that moves on to Finished loses its place.
	for _, u := range []string{a, b, c} {
		call(t, mux, alice, "PUT", "/api/me/books/"+u+"/status", `{"status":"wanted"}`)
	}
	call(t, mux, alice, "PUT", "/api/me/lists/wanted/order", orderBody(b, c, a))
	if got := listUUIDs(t, mux, alice, "/me/wanted"); !slices.Equal(got, []string{b, c, a}) {
		t.Fatalf("wanted: %v", got)
	}
	if got, _ := wantedInOrder(alice); !slices.Equal(got, []string{b, c, a}) {
		t.Fatalf("home's wanted: %v", got)
	}
	call(t, mux, alice, "PUT", "/api/me/books/"+b+"/status", `{"status":"finished"}`)
	var pos *int
	_ = appDB.QueryRow(`SELECT position FROM reading_status WHERE book_uuid = ? AND user_id = ?`, b, alice.ID).Scan(&pos)
	if pos != nil {
		t.Errorf("finished book kept its wanted place: %d", *pos)
	}
	if got := listUUIDs(t, mux, alice, "/me/wanted"); !slices.Equal(got, []string{c, a}) {
		t.Fatalf("wanted after finishing one: %v", got)
	}
	// The grid page offers Edit.
	if rec := call(t, mux, alice, "GET", "/me/wanted", ""); !strings.Contains(rec.Body.String(), `id="list-edit"`) {
		t.Errorf("no Edit button on Want to read")
	}
	if rec := call(t, mux, alice, "GET", "/me/reading", ""); strings.Contains(rec.Body.String(), `id="list-edit"`) {
		t.Errorf("Edit button on Reading")
	}
}

func TestShelfBookOrder(t *testing.T) {
	alice, books := setupLibraryAndUser(t, 3)
	bob, _ := createUser("bob", "longenough-pw", false)
	mux := listOrderMux()
	a, b, c := books[0], books[1], books[2]

	rec := call(t, mux, alice, "POST", "/api/shelves", `{"name":"Mine"}`)
	var s Shelf
	_ = json.Unmarshal(rec.Body.Bytes(), &s)
	path := "/api/shelves/" + itoa(s.ID)
	for i, u := range []string{a, b} {
		call(t, mux, alice, "PUT", path+"/books/"+u, "")
		_, _ = appDB.Exec(`UPDATE shelf_books SET added_at = ? WHERE book_uuid = ?`, 100+i, u)
	}
	if rec := call(t, mux, alice, "PUT", path+"/order", orderBody(b, a)); rec.Code != http.StatusNoContent {
		t.Fatalf("arrange: %d %s", rec.Code, rec.Body)
	}
	// A book added since goes last.
	call(t, mux, alice, "PUT", path+"/books/"+c, "")
	if got := listUUIDs(t, mux, alice, "/shelf/"+itoa(s.ID)); !slices.Equal(got, []string{b, a, c}) {
		t.Fatalf("shelf: %v", got)
	}
	// The home page's row has the same order.
	cat, err := buildCatalog(calibreDB)
	if err != nil {
		t.Fatal(err)
	}
	orig := libraryCatalog.Swap(cat)
	t.Cleanup(func() { libraryCatalog.Store(orig) })
	hs := []homeShelf{{ID: s.ID}}
	if err := fillShelfBooks(hs); err != nil {
		t.Fatal(err)
	}
	var row []string
	for _, bk := range hs[0].Books {
		row = append(row, bk.UUID)
	}
	if !slices.Equal(row, []string{b, a, c}) {
		t.Fatalf("home row: %v", row)
	}
	// Someone else's shelf: not theirs to arrange.
	if code := call(t, mux, bob, "PUT", path+"/order", orderBody(a, b)).Code; code != http.StatusNotFound {
		t.Fatalf("bob: %d", code)
	}
	if rec := call(t, mux, alice, "GET", "/shelf/"+itoa(s.ID), ""); !strings.Contains(rec.Body.String(), `data-arrange="/api/shelves/`+itoa(s.ID)+`/order"`) {
		t.Errorf("no Edit button on the shelf")
	}
}
