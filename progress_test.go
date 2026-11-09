package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func progressMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me/progress/{uuid}", requireUser(progressGetHandler))
	mux.HandleFunc("PUT /api/me/progress/{uuid}", requireUser(progressPutHandler))
	mux.HandleFunc("GET /api/me/bookmarks/{uuid}", requireUser(bookmarksGetHandler))
	mux.HandleFunc("PUT /api/me/bookmarks/{uuid}/{id}", requireUser(bookmarkPutHandler))
	mux.HandleFunc("DELETE /api/me/bookmarks/{uuid}/{id}", requireUser(bookmarkDeleteHandler))
	return mux
}

func call(t *testing.T, mux http.Handler, u *User, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if u != nil {
		req = asUser(req, u)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestProgressSync(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	b := books[0]
	mux := progressMux()
	put := func(pos string, pct float64, at int64) progressRec {
		rec := call(t, mux, u, "PUT", "/api/me/progress/"+b,
			`{"format":"epub","position":"`+pos+`","percent":`+jsonNum(pct)+`,"updated_at":`+itoa(at)+`}`)
		if rec.Code != 200 {
			t.Fatalf("put: %d %s", rec.Code, rec.Body)
		}
		var out struct{ Progress progressRec }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Progress
	}
	now := time.Now().UnixMilli()

	if rec := call(t, mux, nil, "PUT", "/api/me/progress/"+b, `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	put("cfi-a", 0.10, now-2000)
	if s, src := readStatus(u.ID, b); s != "reading" || src != "auto" {
		t.Fatalf("moving through the book should mark reading: %s/%s", s, src)
	}
	// An older write (another device that was offline) must not win…
	if got := put("cfi-old", 0.05, now-5000); got.Position != "cfi-a" {
		t.Fatalf("older write won: %+v", got)
	}
	// …a newer one does.
	if got := put("cfi-b", 0.50, now-1000); got.Position != "cfi-b" {
		t.Fatalf("newer write lost: %+v", got)
	}
	// A clock far in the future is clamped to now, so it can't lock others out.
	if got := put("cfi-future", 0.6, now+365*24*3600*1000); got.UpdatedAt > time.Now().UnixMilli()+1000 {
		t.Fatalf("future timestamp not clamped: %d", got.UpdatedAt)
	}
	if got := put("cfi-end", 0.99, time.Now().UnixMilli()+500); got.Position != "cfi-end" {
		t.Fatalf("latest position: %+v", got)
	}
	if s, _ := readStatus(u.ID, b); s != "finished" {
		t.Fatalf("reaching the end should mark finished: %s", s)
	}

	var got struct{ Progress *progressRec }
	_ = json.Unmarshal(call(t, mux, u, "GET", "/api/me/progress/"+b+"?format=epub", "").Body.Bytes(), &got)
	if got.Progress == nil || got.Progress.Position != "cfi-end" {
		t.Fatalf("GET progress: %+v", got.Progress)
	}
	// Formats are tracked separately.
	got.Progress = nil
	_ = json.Unmarshal(call(t, mux, u, "GET", "/api/me/progress/"+b+"?format=pdf", "").Body.Bytes(), &got)
	if got.Progress != nil {
		t.Fatal("pdf progress should be empty")
	}
	if rec := call(t, mux, u, "PUT", "/api/me/progress/"+b, `{"format":"mobi","position":"x"}`); rec.Code != 400 {
		t.Fatalf("bad format: %d", rec.Code)
	}
}

func TestBookmarksSync(t *testing.T) {
	u, books := setupLibraryAndUser(t, 2)
	other, _ := createUser("other", "longenough-pw", false)
	mux := progressMux()
	list := func(who *User, book string) []bookmarkRec {
		var out struct{ Bookmarks []bookmarkRec }
		_ = json.Unmarshal(call(t, mux, who, "GET", "/api/me/bookmarks/"+book, "").Body.Bytes(), &out)
		return out.Bookmarks
	}

	if rec := call(t, mux, u, "PUT", "/api/me/bookmarks/"+books[0]+"/bm-1", `{"format":"pdf","position":"12","label":"Page 12","created_at":1}`); rec.Code != 200 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	// Re-sending the same id (offline retry) doesn't duplicate.
	call(t, mux, u, "PUT", "/api/me/bookmarks/"+books[0]+"/bm-1", `{"format":"pdf","position":"12","label":"Page 12!","created_at":1}`)
	if bm := list(u, books[0]); len(bm) != 1 || bm[0].Label != "Page 12!" {
		t.Fatalf("after retry: %+v", bm)
	}
	// The same client id can't be hijacked onto another book.
	call(t, mux, u, "PUT", "/api/me/bookmarks/"+books[1]+"/bm-1", `{"format":"pdf","position":"1","label":"x"}`)
	if bm := list(u, books[1]); len(bm) != 0 {
		t.Fatalf("id moved across books: %+v", bm)
	}
	if bm := list(other, books[0]); len(bm) != 0 {
		t.Fatal("bookmarks leaked across users")
	}
	if rec := call(t, mux, u, "DELETE", "/api/me/bookmarks/"+books[0]+"/bm-1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if bm := list(u, books[0]); len(bm) != 0 {
		t.Fatal("bookmark not deleted")
	}
}

func jsonNum(f float64) string { b, _ := json.Marshal(f); return string(b) }
