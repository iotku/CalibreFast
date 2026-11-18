package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func prefsMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me", meHandler)
	mux.HandleFunc("GET /preferences", preferencesPage)
	mux.HandleFunc("PUT /api/me/prefs", requireUser(putPrefsHandler))
	mux.HandleFunc("GET /api/me/saved-books", requireUser(savedBooksHandler))
	return mux
}

// The account's preferences are saved, checked, and sent with /api/me.
func TestAccountPrefs(t *testing.T) {
	setupTestAppDB(t)
	u, err := createUser("prefs", "a long enough password", false)
	if err != nil {
		t.Fatal(err)
	}
	mux := prefsMux()
	format := func() string {
		rec := call(t, mux, u, "GET", "/api/me", "")
		var me struct{ Prefs userPrefs }
		if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
			t.Fatal(err)
		}
		return me.Prefs.Format
	}
	if f := format(); f != "last" {
		t.Fatalf("default format %q, want last", f)
	}
	if rec := call(t, mux, u, "PUT", "/api/me/prefs", `{"format":"pdf"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if f := format(); f != "pdf" {
		t.Fatalf("format %q, want pdf", f)
	}
	for _, body := range []string{`{"format":"mobi"}`, `{"format":""}`, `not json`} {
		if rec := call(t, mux, u, "PUT", "/api/me/prefs", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, rec.Code)
		}
	}
	if f := format(); f != "pdf" {
		t.Fatalf("after bad puts: %q", f)
	}
	if rec := call(t, mux, u, "GET", "/api/me/prefs", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d, want 405", rec.Code)
	}
	if rec := call(t, mux, nil, "PUT", "/api/me/prefs", `{"format":"epub"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
}

// Guests use the page too: their preferences are in their browser.
func TestPreferencesPageForGuest(t *testing.T) {
	setupTestAppDB(t)
	rec := call(t, prefsMux(), nil, "GET", "/preferences", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, want := range []string{`name="format"`, `data-pref="maxBytes"`, `id="prefs-storage"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

// An account made here carries the guest's preferences over (the cookie);
// signing in to an existing one doesn't.
func TestNewAccountCookie(t *testing.T) {
	setupTestAppDB(t)
	orig := allowRegistrations
	allowRegistrations = true
	t.Cleanup(func() { allowRegistrations = orig })
	form := url.Values{"username": {"newbie"}, "password": {"a long enough password"}, "password2": {"a long enough password"}, "next": {"/"}}
	post := func(h http.HandlerFunc, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	hasCookie := func(rec *httptest.ResponseRecorder) bool {
		for _, c := range rec.Result().Cookies() {
			if c.Name == newAccountCookie && c.Value == "1" {
				if c.HttpOnly {
					t.Error("the page's script must be able to read it")
				}
				return true
			}
		}
		return false
	}
	if rec := post(registerHandler, "/register"); rec.Code != http.StatusSeeOther || !hasCookie(rec) {
		t.Errorf("register: %d, cookie %v", rec.Code, hasCookie(rec))
	}
	if rec := post(loginHandler, "/login"); rec.Code != http.StatusSeeOther || hasCookie(rec) {
		t.Errorf("login: %d, cookie %v (want none)", rec.Code, hasCookie(rec))
	}
}

// The Saved list for downloading ahead of time: the user's own, oldest first.
func TestSavedBooks(t *testing.T) {
	u, books := setupLibraryAndUser(t, 3)
	other, err := createUser("other", "a long enough password", false)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range books {
		if err := setStatus(u.ID, b, "wanted", "manual"); err != nil {
			t.Fatal(err)
		}
		if _, err := appDB.Exec(`UPDATE reading_status SET updated_at = ? WHERE user_id = ? AND book_uuid = ?`, 1000+i, u.ID, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := setStatus(u.ID, books[1], "reading", "manual"); err != nil { // no longer Saved
		t.Fatal(err)
	}
	if err := setStatus(other.ID, books[0], "wanted", "manual"); err != nil {
		t.Fatal(err)
	}
	mux := prefsMux()
	rec := call(t, mux, u, "GET", "/api/me/saved-books?limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got []savedBook
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].UUID != books[0] || got[1].UUID != books[2] {
		t.Fatalf("got %+v, want %s then %s", got, books[0], books[2])
	}
	if got[0].SavedAt != 1000*1000 || got[0].Title == "" {
		t.Errorf("first: %+v", got[0])
	}
	// Only the oldest N, and unchanged lists aren't sent again.
	rec = call(t, mux, u, "GET", "/api/me/saved-books?limit=1", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].UUID != books[0] {
		t.Fatalf("limit=1: %s", rec.Body)
	}
	tag := rec.Header().Get("ETag")
	req := httptest.NewRequest("GET", "/api/me/saved-books?limit=1", nil)
	req.Header.Set("If-None-Match", tag)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, asUser(req, u))
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("same tag: %d %q, want an empty 304", rec.Code, rec.Body)
	}
	if err := setStatus(u.ID, books[1], "wanted", "manual"); err != nil { // saved now: the newest, not among the oldest 1
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, asUser(req, u))
	if rec.Code != http.StatusNotModified {
		t.Errorf("a newer book saved: %d, want 304 (the oldest 1 is unchanged)", rec.Code)
	}
	if err := setStatus(u.ID, books[0], "reading", "manual"); err != nil { // the oldest leaves Saved
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, asUser(req, u))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), books[2]) {
		t.Errorf("after a change: %d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{"", "?limit=0", "?limit=501", "?limit=x"} {
		if rec := call(t, mux, u, "GET", "/api/me/saved-books"+q, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d, want 400", q, rec.Code)
		}
	}
	if rec := call(t, mux, other, "GET", "/api/me/saved-books?limit=10", ""); !strings.Contains(rec.Body.String(), books[0]) || strings.Contains(rec.Body.String(), books[2]) {
		t.Errorf("other user's list: %s", rec.Body)
	}
	if rec := call(t, mux, nil, "GET", "/api/me/saved-books?limit=10", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d", rec.Code)
	}
}

// The reading marks are tagged: /api/me?reading=1&have=<tag> and
// /api/me/reading with If-None-Match don't send unchanged marks again.
func TestReadingMarksTag(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	mux := prefsMux()
	mux.HandleFunc("GET /api/me/reading", requireUser(readingHandler))
	rec := call(t, mux, u, "GET", "/api/me?reading=1", "")
	var me struct {
		Reading     *readingMarks `json:"reading"`
		ReadingTag  string        `json:"reading_tag"`
		ReadingSame bool          `json:"reading_same"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || me.Reading == nil || me.ReadingTag == "" {
		t.Fatalf("first: %s", rec.Body)
	}
	tag := me.ReadingTag
	me.Reading = nil
	rec = call(t, mux, u, "GET", "/api/me?reading=1&have="+url.QueryEscape(tag), "")
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || me.Reading != nil || !me.ReadingSame {
		t.Fatalf("same marks were sent again: %s", rec.Body)
	}
	// /api/me/reading's ETag is the same tag.
	rec = call(t, mux, u, "GET", "/api/me/reading", "")
	if rec.Header().Get("ETag") != tag {
		t.Fatalf("ETag %q, want %q", rec.Header().Get("ETag"), tag)
	}
	req := httptest.NewRequest("GET", "/api/me/reading", nil)
	req.Header.Set("If-None-Match", tag)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, asUser(req, u))
	if rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", rec.Code)
	}
	if err := setStatus(u.ID, books[0], "wanted", "manual"); err != nil {
		t.Fatal(err)
	}
	rec = call(t, mux, u, "GET", "/api/me?reading=1&have="+url.QueryEscape(tag), "")
	me.Reading, me.ReadingSame = nil, false
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || me.Reading == nil || me.ReadingSame || me.ReadingTag == tag {
		t.Fatalf("changed marks not sent: %s", rec.Body)
	}
}
