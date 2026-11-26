package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func sharedShelfMux() *http.ServeMux {
	mux := pinMux()
	mux.HandleFunc("POST /api/shelves/{id}/books", requireUser(shelfAddBooksHandler))
	mux.HandleFunc("PUT /api/shelves/{id}/order", requireUser(shelfBookOrderHandler))
	return mux
}

// A shared shelf: everyone can see it and pin it; only its owner changes it.
// Unshared (or its owner's account disabled), it's private again.
func TestSharedShelves(t *testing.T) {
	alice, books := setupLibraryAndUser(t, 2)
	bob, _ := createUser("bob", "longenough-pw", false)
	admin, _ := createUser("boss", "longenough-pw", true)
	mux := sharedShelfMux()
	do := func(who *User, method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		rec := call(t, mux, who, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s %s: %d %s (want %d)", method, path, body, rec.Code, rec.Body, want)
		}
		return rec
	}
	var s Shelf
	_ = json.Unmarshal(do(alice, "POST", "/api/shelves", `{"name":"Alice's picks"}`, http.StatusCreated).Body.Bytes(), &s)
	path := "/api/shelves/" + itoa(s.ID)
	do(alice, "PUT", path+"/books/"+books[0], "", http.StatusNoContent)

	// Private: bob can't see it.
	do(bob, "GET", "/shelf/"+itoa(s.ID)+"?api=1", "", http.StatusNotFound)
	do(bob, "PUT", "/api/me/pins/"+itoa(s.ID), "", http.StatusNotFound)

	// Only the owner shares it; a global shelf can't be "shared".
	do(bob, "PATCH", path, `{"shared":true}`, http.StatusNotFound)
	do(alice, "PATCH", path, `{"shared":true}`, http.StatusOK)
	var g Shelf
	_ = json.Unmarshal(do(admin, "POST", "/api/shelves", `{"name":"Staff","global":true}`, http.StatusCreated).Body.Bytes(), &g)
	do(admin, "PATCH", "/api/shelves/"+itoa(g.ID), `{"shared":true}`, http.StatusBadRequest)

	// Shared: bob and a signed-out visitor see it; bob can't change it.
	do(bob, "GET", "/shelf/"+itoa(s.ID)+"?api=1", "", http.StatusOK)
	do(nil, "GET", "/shelf/"+itoa(s.ID)+"?api=1", "", http.StatusOK)
	page := do(bob, "GET", "/shelf/"+itoa(s.ID), "", http.StatusOK).Body.String()
	if !strings.Contains(page, "shared by reader") {
		t.Fatalf("shelf page doesn't say who shared it")
	}
	do(bob, "PUT", path+"/books/"+books[1], "", http.StatusForbidden)
	do(bob, "POST", path+"/books", `{"uuids":["`+books[1]+`"]}`, http.StatusForbidden)
	do(bob, "PATCH", path, `{"name":"mine"}`, http.StatusForbidden)
	do(bob, "DELETE", path, "", http.StatusForbidden)
	do(bob, "DELETE", path+"/books/"+books[0], "", http.StatusForbidden)

	// It's listed as shared (with its owner), not as bob's.
	var lists struct{ Mine, Global, Shared []Shelf }
	_ = json.Unmarshal(do(bob, "GET", "/api/shelves", "", http.StatusOK).Body.Bytes(), &lists)
	if len(lists.Mine) != 0 || len(lists.Shared) != 1 || lists.Shared[0].Owner != "reader" || lists.Shared[0].CanEdit {
		t.Fatalf("bob's lists: %+v", lists)
	}
	_ = json.Unmarshal(do(alice, "GET", "/api/shelves", "", http.StatusOK).Body.Bytes(), &lists)
	if len(lists.Mine) != 1 || !lists.Mine[0].Shared || len(lists.Shared) != 0 {
		t.Fatalf("alice's lists: %+v", lists)
	}

	// Bob finds it in More shelves, pins it, and it moves to his rows.
	home := func(u *User) homeAnswer {
		t.Helper()
		rec := httptest.NewRecorder()
		homeHandler(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/me/home", nil), u))
		var h homeAnswer
		_ = json.Unmarshal(rec.Body.Bytes(), &h)
		return h
	}
	names := func(rows []homeShelf) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Name)
		}
		return out
	}
	if h := home(bob); !slices.Equal(names(h.MoreShelves), []string{"Alice's picks"}) || h.MoreShelves[0].Owner != "reader" {
		t.Fatalf("bob's more shelves: %+v", h.MoreShelves)
	}
	// Alice's own shelf isn't something for her to find; the empty global isn't either.
	if h := home(alice); len(h.MoreShelves) != 0 {
		t.Fatalf("alice's more shelves: %+v", h.MoreShelves)
	}
	do(bob, "PUT", "/api/me/pins/"+itoa(s.ID), "", http.StatusNoContent)
	if h := home(bob); !slices.Equal(names(h.Shelves), []string{"Alice's picks"}) || len(h.MoreShelves) != 0 {
		t.Fatalf("bob's home after pinning: %+v / %+v", h.Shelves, h.MoreShelves)
	}

	// Unshared: gone from bob's page and lists, and private again.
	do(alice, "PATCH", path, `{"shared":false}`, http.StatusOK)
	if h := home(bob); len(h.Shelves) != 0 {
		t.Fatalf("unshared shelf still on bob's page: %+v", h.Shelves)
	}
	do(bob, "GET", "/shelf/"+itoa(s.ID)+"?api=1", "", http.StatusNotFound)

	// Shared again, then alice's account disabled: hidden from others.
	do(alice, "PATCH", path, `{"shared":true}`, http.StatusOK)
	do(bob, "GET", "/shelf/"+itoa(s.ID)+"?api=1", "", http.StatusOK)
	if _, err := appDB.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, alice.ID); err != nil {
		t.Fatal(err)
	}
	do(bob, "GET", "/shelf/"+itoa(s.ID)+"?api=1", "", http.StatusNotFound)
	_ = json.Unmarshal(do(bob, "GET", "/api/shelves", "", http.StatusOK).Body.Bytes(), &lists)
	if len(lists.Shared) != 0 {
		t.Fatalf("a disabled user's shared shelf listed: %+v", lists.Shared)
	}
}

func TestShelfBulkAdd(t *testing.T) {
	alice, books := setupLibraryAndUser(t, 3)
	mux := sharedShelfMux()
	var s Shelf
	_ = json.Unmarshal(call(t, mux, alice, "POST", "/api/shelves", `{"name":"Many"}`).Body.Bytes(), &s)
	path := "/api/shelves/" + itoa(s.ID) + "/books"
	if c := call(t, mux, nil, "POST", path, `{"uuids":["`+books[0]+`"]}`).Code; c != http.StatusUnauthorized {
		t.Fatalf("signed out: %d", c)
	}
	if c := call(t, mux, alice, "GET", path, "").Code; c != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", c)
	}
	if c := call(t, mux, alice, "POST", path, `{"uuids":[]}`).Code; c != http.StatusBadRequest {
		t.Fatalf("none: %d", c)
	}
	many := make([]string, maxBulkShelfAdd+1)
	for i := range many {
		many[i] = books[0]
	}
	js, _ := json.Marshal(map[string][]string{"uuids": many})
	if c := call(t, mux, alice, "POST", path, string(js)).Code; c != http.StatusBadRequest {
		t.Fatalf("too many: %d", c)
	}

	add := func(uuids ...string) int {
		t.Helper()
		js, _ := json.Marshal(map[string][]string{"uuids": uuids})
		rec := call(t, mux, alice, "POST", path, string(js))
		if rec.Code != http.StatusOK {
			t.Fatalf("add %q: %d %s", uuids, rec.Code, rec.Body)
		}
		var res struct{ Added int }
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		return res.Added
	}
	// Unknown books, repeats and a series key are skipped.
	if n := add(books[0], books[1], books[0], "not-a-book", "series:Dune"); n != 2 {
		t.Fatalf("added %d, want 2", n)
	}
	if n := add(books[1], books[2]); n != 1 {
		t.Fatalf("added %d, want 1 (one was there already)", n)
	}
	if got, _ := getShelf(s.ID); got.Count != 3 {
		t.Fatalf("shelf has %d entries, want 3", got.Count)
	}
}

func TestShelfSort(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	var b []string
	for _, title := range []string{"Charlie", "Alpha", "Bravo"} {
		b = append(b, addTestBook(t, db, title, []string{"A"}, nil, "", "epub"))
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	alice, _ := createUser("alice", "a long enough password", false)
	mux := sharedShelfMux()
	var s Shelf
	_ = json.Unmarshal(call(t, mux, alice, "POST", "/api/shelves", `{"name":"Sorted"}`).Body.Bytes(), &s)
	// Added Bravo, Charlie, Alpha, a second apart.
	for i, u := range []string{b[2], b[0], b[1]} {
		if _, err := appDB.Exec(`INSERT INTO shelf_books (shelf_id, book_uuid, added_at) VALUES (?, ?, ?)`, s.ID, u, 100+i); err != nil {
			t.Fatal(err)
		}
	}
	// Arranged: Charlie, Alpha, Bravo.
	order, _ := json.Marshal(map[string][]string{"order": {b[0], b[1], b[2]}})
	if c := call(t, mux, alice, "PUT", "/api/shelves/"+itoa(s.ID)+"/order", string(order)).Code; c != http.StatusNoContent {
		t.Fatalf("arrange: %d", c)
	}
	titles := func(sort string) []string {
		t.Helper()
		var shown []Book
		rec := call(t, mux, alice, "GET", "/shelf/"+itoa(s.ID)+"?api=1&page=1&sort="+sort, "")
		_ = json.Unmarshal(rec.Body.Bytes(), &shown)
		var out []string
		for _, x := range shown {
			out = append(out, x.Title)
		}
		return out
	}
	for sort, want := range map[string][]string{
		"":         {"Charlie", "Alpha", "Bravo"},
		"arranged": {"Charlie", "Alpha", "Bravo"},
		"bogus":    {"Charlie", "Alpha", "Bravo"},
		"title":    {"Alpha", "Bravo", "Charlie"},
		"added":    {"Alpha", "Charlie", "Bravo"},
	} {
		if got := titles(sort); !slices.Equal(got, want) {
			t.Errorf("sort %q: %q, want %q", sort, got, want)
		}
	}
	// Arrange is offered in the shelf's own order only.
	if page := call(t, mux, alice, "GET", "/shelf/"+itoa(s.ID)+"?sort=title", "").Body.String(); strings.Contains(page, `id="list-edit"`) {
		t.Errorf("Arrange offered while sorted by title")
	}
	if page := call(t, mux, alice, "GET", "/shelf/"+itoa(s.ID), "").Body.String(); !strings.Contains(page, `id="list-edit"`) || !strings.Contains(page, `data-remove=`) {
		t.Errorf("Arrange (with Remove) missing in shelf order")
	}
}
