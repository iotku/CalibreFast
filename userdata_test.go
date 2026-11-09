package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// setupLibraryAndUser: a calibre test DB with n books and a signed-in user.
func setupLibraryAndUser(t *testing.T, n int) (*User, []string) {
	t.Helper()
	db, tempDir := setupTestDB(t)
	t.Cleanup(func() { db.Close(); os.RemoveAll(tempDir) })
	origDB, origBase := calibreDB, baseDir
	calibreDB, baseDir = db, tempDir
	t.Cleanup(func() { calibreDB, baseDir = origDB, origBase })
	setupTestAppDB(t)
	u, err := createUser("reader", "longenough-pw", false)
	if err != nil {
		t.Fatal(err)
	}
	var uuids []string
	for i := 0; i < n; i++ {
		_, id, _, err := insertBookIntoCalibreDB(db, &UploadedBookMeta{OPFMetadata: OPFMetadata{Title: "Book " + itoa(int64(i)), Creators: []string{"A"}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		uuids = append(uuids, id)
	}
	return u, uuids
}

func asUser(r *http.Request, u *User) *http.Request { return r.WithContext(withUserCtx(r, u)) }

func TestStatusRules(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	b := books[0]
	get := func() string { s, _ := readStatus(u.ID, b); return s }

	_ = setStatus(u.ID, b, "reading", "auto")
	if get() != "reading" {
		t.Fatal("auto: none → reading")
	}
	_ = setStatus(u.ID, b, "finished", "auto")
	if get() != "finished" {
		t.Fatal("auto: reading → finished")
	}
	_ = setStatus(u.ID, b, "reading", "auto") // re-opening a finished book
	if get() != "finished" {
		t.Fatal("auto must never move a book backwards")
	}
	_ = setStatus(u.ID, b, "reading", "manual")
	if s, src := readStatus(u.ID, b); s != "reading" || src != "manual" {
		t.Fatalf("manual change must apply: %s/%s", s, src)
	}
	_ = setStatus(u.ID, b, "", "manual")
	if get() != "" {
		t.Fatal("manual clear")
	}
	if err := setStatus(u.ID, b, "bogus", "manual"); err != errBadStatus {
		t.Fatalf("bad status: %v", err)
	}
}

func TestFavoritesAPIAndList(t *testing.T) {
	u, books := setupLibraryAndUser(t, 3)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/me/books/{uuid}/favorite", requireUser(setFavoriteHandler))
	mux.HandleFunc("GET /api/me/books/{uuid}", requireUser(bookStateHandler))
	mux.HandleFunc("GET /me/{list}", requireUser(meListHandler))

	do := func(method, path, body string, who *User) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if who != nil {
			req = asUser(req, who)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("PUT", "/api/me/books/"+books[0]+"/favorite", `{"favorite":true}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous favorite: %d", rec.Code)
	}
	if rec := do("PUT", "/api/me/books/not-a-book/favorite", `{"favorite":true}`, u); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown book: %d", rec.Code)
	}
	for _, b := range []string{books[0], books[2]} {
		if rec := do("PUT", "/api/me/books/"+b+"/favorite", `{"favorite":true}`, u); rec.Code != 200 {
			t.Fatalf("favorite: %d %s", rec.Code, rec.Body)
		}
	}
	var st bookState
	_ = json.Unmarshal(do("GET", "/api/me/books/"+books[2], "", u).Body.Bytes(), &st)
	if !st.Favorite {
		t.Fatal("state should show favorite")
	}

	var listed []Book
	_ = json.Unmarshal(do("GET", "/me/favorites?api=1&page=1", "", u).Body.Bytes(), &listed)
	if len(listed) != 2 {
		t.Fatalf("favorites list: %d books", len(listed))
	}

	// Another user sees their own (empty) favorites.
	other, _ := createUser("other", "longenough-pw", false)
	listed = nil
	_ = json.Unmarshal(do("GET", "/me/favorites?api=1&page=1", "", other).Body.Bytes(), &listed)
	if len(listed) != 0 {
		t.Fatal("favorites leaked across users")
	}

	do("PUT", "/api/me/books/"+books[0]+"/favorite", `{"favorite":false}`, u)
	listed = nil
	_ = json.Unmarshal(do("GET", "/me/favorites?api=1&page=1", "", u).Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].UUID != books[2] {
		t.Fatalf("after unfavorite: %+v", listed)
	}
}
