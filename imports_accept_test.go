package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestAcceptAllImports(t *testing.T) {
	db := withTestLibrary(t)
	setupTestAppDB(t)
	w, dir := newTestWatcher(t)
	origImports, origWrites := imports, allowWrites
	imports, allowWrites = w.inbox, true
	t.Cleanup(func() { imports, allowWrites = origImports, origWrites })
	admin, _ := createUser("massadmin", "correct horse battery", true)
	editor, _ := createUser("masseditor", "correct horse battery", false)
	mustExec(t, appDB, `UPDATE users SET is_editor = 1 WHERE id = ?`, editor.ID)
	editor, _ = getUserByID(editor.ID)

	epub := importTestEPUB(t)
	writeTestFile(t, filepath.Join(dir, "one", "Book.epub"), epub)
	writeTestFile(t, filepath.Join(dir, "two", "Other.pdf"), []byte("%PDF-1.4 other"))
	w.scan()
	w.scan()
	if w.inbox.count() != 2 {
		t.Fatalf("%d waiting", w.inbox.count())
	}

	post := func(u *User) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/imports/accept-all", nil)
		requireWrites(importAcceptAllHandler)(rec, req.WithContext(withUserCtx(req, u)))
		return rec
	}
	if c := post(editor).Code; c != http.StatusNotFound {
		t.Errorf("an editor mass imported: %d", c)
	}
	rec := post(admin)
	var res acceptAllResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || res.Added != 2 || len(res.Left) != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var books int
	db.QueryRow(`SELECT COUNT(*) FROM books`).Scan(&books)
	if books != 2 || w.inbox.count() != 0 {
		t.Fatalf("%d books, %d waiting", books, w.inbox.count())
	}
	t.Cleanup(func() { uuidPathIndex.Clear(); bookMetadataCache.Clear() })

	// The same EPUB again: identical to a library file, so it stays, with why.
	writeTestFile(t, filepath.Join(dir, "three", "Again.epub"), epub)
	w.scan()
	w.scan()
	rec = post(admin)
	res = acceptAllResult{}
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Added != 0 || len(res.Left) != 1 || res.Left[0].Reason == "" || w.inbox.count() != 1 {
		t.Fatalf("duplicate: %s", rec.Body)
	}
}
