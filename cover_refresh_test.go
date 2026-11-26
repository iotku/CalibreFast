package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iotku/CalibreFast/pkg/metalookup"
)

// A cover from Open Library replaces a book's, through the route the page
// uses; each update is undone on its own, only by whoever made it (or an
// admin), and not over a later change.
func TestCoverUpdater(t *testing.T) {
	db := setupRecycleLibrary(t)
	origWrites, origLookups, origCovers, origHC := allowWrites, bookLookups, lookupCovers, hcClient
	allowWrites, hcClient = true, nil
	lookupCovers = newCoverCache(1<<20, time.Hour)
	t.Cleanup(func() { allowWrites, bookLookups, lookupCovers, hcClient = origWrites, origLookups, origCovers, origHC })

	newCover := testJPEG(t, 20, 30, 90)
	var hits atomic.Int32
	ol := http.NewServeMux()
	ol.HandleFunc("/b/id/111-L.jpg", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(newCover)
	})
	srv := httptest.NewServer(ol)
	t.Cleanup(srv.Close)
	bookLookups = metalookup.New(metalookup.Config{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		UserAgent:  lookupUserAgent,
		CoversURL:  srv.URL,
		Limits:     map[string]*metalookup.Limiter{openLibraryName: metalookup.NewLimiter(100, 100, 1000)},
	})

	mux := http.NewServeMux()
	registerAllEditRoutes(mux)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	oldCover := testJPEG(t, 24, 36, 80)
	coverPath := filepath.Join(baseDir, a.path, "cover.jpg")
	if err := os.WriteFile(coverPath, oldCover, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(`UPDATE books SET has_cover = 1 WHERE uuid = ?`, a.uuid)

	admin, _ := createUser("admin1", "correct horse battery", true)
	editor, _ := createUser("editor1", "correct horse battery", false)
	other, _ := createUser("editor2", "correct horse battery", false)
	_, _ = appDB.Exec(`UPDATE users SET is_editor = 1 WHERE id IN (?, ?)`, editor.ID, other.ID)
	editor, _ = getUserByID(editor.ID)
	other, _ = getUserByID(other.ID)

	do := func(u *User, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, asUser(httptest.NewRequest(method, path, strings.NewReader(body)), u))
		return rec
	}
	want := func(rec *httptest.ResponseRecorder, code int, what string) {
		t.Helper()
		if rec.Code != code {
			t.Fatalf("%s: %d %s, want %d", what, rec.Code, rec.Body, code)
		}
	}

	// The picked books, as the page asks for them.
	rec := do(editor, "POST", "/api/covers/books", `{"uuids":["`+a.uuid+`","`+a.uuid+`","00000000-0000-0000-0000-000000000000"]}`)
	want(rec, http.StatusOK, "books")
	var books struct{ Books []coverRefreshBook }
	_ = json.Unmarshal(rec.Body.Bytes(), &books)
	if len(books.Books) != 1 || books.Books[0].Title != "Alpha" || books.Books[0].Authors != "Ann" || !books.Books[0].HasCover ||
		!strings.HasPrefix(books.Books[0].Cover, "/cover/"+a.uuid) {
		t.Fatalf("books %s", rec.Body)
	}
	many := make([]string, coverRefreshMax+1)
	for i := range many {
		many[i] = strconv.Quote(strconv.Itoa(i))
	}
	want(do(editor, "POST", "/api/covers/books", `{"uuids":[`+strings.Join(many, ",")+`]}`), http.StatusBadRequest, "too many books")
	want(do(editor, "POST", "/api/covers/books", `{"uuids":["../x"]}`), http.StatusBadRequest, "bad uuid")

	// The budget: Open Library's day so far; Hardcover off without a client.
	rec = do(editor, "GET", "/api/covers/budget", "")
	want(rec, http.StatusOK, "budget")
	var budget struct{ Openlibrary, Hardcover coverBudget }
	_ = json.Unmarshal(rec.Body.Bytes(), &budget)
	if budget.Openlibrary.Limit != 1000 || budget.Openlibrary.Used != 0 || budget.Hardcover.Enabled {
		t.Fatalf("budget %s", rec.Body)
	}

	// The preview fetches the cover; saving it doesn't fetch it again.
	want(do(editor, "GET", "/metadata/lookup/cover?src=openlibrary&id=111", ""), http.StatusOK, "preview")
	for _, body := range []string{`{"src":"google","id":"111"}`, `{"src":"openlibrary","id":"../x"}`, `{"src":"hardcover","id":"1"}`} {
		want(do(editor, "POST", "/api/covers/"+a.uuid+"/apply", body), http.StatusBadRequest, body)
	}
	want(do(editor, "GET", "/api/covers/"+a.uuid+"/apply", ""), http.StatusNotFound, "GET apply") // the index catches GETs
	rec = do(editor, "POST", "/api/covers/"+a.uuid+"/apply", `{"src":"openlibrary","id":"111"}`)
	want(rec, http.StatusOK, "apply")
	var applied struct {
		EditID int64 `json:"edit_id"`
		Cover  string
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &applied)
	if applied.EditID == 0 || !strings.HasPrefix(applied.Cover, "/cover/"+a.uuid) {
		t.Fatalf("apply %s", rec.Body)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("cover fetched %d times, want once", n)
	}
	if cur, _ := os.ReadFile(coverPath); bytes.Equal(cur, oldCover) {
		t.Fatal("cover not replaced")
	}
	id := strconv.FormatInt(applied.EditID, 10)

	// The update is the editor's: listed for them and an admin, not another editor.
	count := func(u *User) int {
		var h struct{ Updates []coverUpdate }
		rec := do(u, "GET", "/api/covers/history", "")
		want(rec, http.StatusOK, "history")
		_ = json.Unmarshal(rec.Body.Bytes(), &h)
		return len(h.Updates)
	}
	if count(editor) != 1 || count(admin) != 1 || count(other) != 0 {
		t.Errorf("history: editor %d, admin %d, other %d", count(editor), count(admin), count(other))
	}
	rec = do(editor, "GET", "/api/covers/history/"+id+"/before", "")
	want(rec, http.StatusOK, "before")
	if !bytes.Equal(rec.Body.Bytes(), oldCover) || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Error("before isn't the old cover")
	}
	want(do(other, "GET", "/api/covers/history/"+id+"/before", ""), http.StatusNotFound, "someone else's before")
	want(do(editor, "GET", "/api/covers/history/"+id+"/sideways", ""), http.StatusNotFound, "bad side")
	want(do(other, "POST", "/api/covers/history/"+id+"/revert", ""), http.StatusNotFound, "someone else's undo")
	want(do(editor, "GET", "/api/covers/history/"+id+"/revert", ""), http.StatusNotFound, "GET revert") // no such image side

	// Only the updater's entries: a cover uploaded in the editor isn't one.
	want(do(editor, "POST", "/upload-cover/"+a.uuid, testCoverDataURL(t)), http.StatusOK, "upload")
	var uploadID int64
	_ = appDB.QueryRow(`SELECT MAX(id) FROM edit_log WHERE action = 'cover'`).Scan(&uploadID)
	want(do(admin, "POST", "/api/covers/history/"+strconv.FormatInt(uploadID, 10)+"/revert", ""), http.StatusNotFound, "not an update")

	// Changed since: refused, and the later cover stays.
	afterUpload, _ := os.ReadFile(coverPath)
	want(do(editor, "POST", "/api/covers/history/"+id+"/revert", ""), http.StatusConflict, "undo over a later change")
	if cur, _ := os.ReadFile(coverPath); !bytes.Equal(cur, afterUpload) {
		t.Error("refused undo changed the cover")
	}

	// A fresh update, undone: the cover before it is back, byte for byte.
	rec = do(editor, "POST", "/api/covers/"+a.uuid+"/apply", `{"src":"openlibrary","id":"111"}`)
	want(rec, http.StatusOK, "apply again")
	_ = json.Unmarshal(rec.Body.Bytes(), &applied)
	id2 := strconv.FormatInt(applied.EditID, 10)
	want(do(editor, "POST", "/api/covers/history/"+id2+"/revert", ""), http.StatusOK, "undo")
	if cur, _ := os.ReadFile(coverPath); !bytes.Equal(cur, afterUpload) {
		t.Error("undo didn't restore the previous cover")
	}
	want(do(editor, "POST", "/api/covers/history/"+id2+"/revert", ""), http.StatusConflict, "undo twice")
}
