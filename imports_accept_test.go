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

	accept := func(u *User, id string) (int, map[string]string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/import/"+id+"/accept", nil)
		req.SetPathValue("id", id)
		requireWrites(importAcceptHandler)(rec, req.WithContext(withUserCtx(req, u)))
		var out map[string]string
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	items := w.inbox.list()
	if c, _ := accept(editor, items[0].ID); c != http.StatusNotFound {
		t.Errorf("an editor mass imported: %d", c)
	}
	for _, it := range items {
		if c, out := accept(admin, it.ID); c != http.StatusOK || out["uuid"] == "" || out["reason"] != "" {
			t.Fatalf("accept %q: %d %v", it.Merged.Title, c, out)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/imports/accept-done", nil)
	requireWrites(importAcceptDoneHandler)(rec, req.WithContext(withUserCtx(req, admin)))
	if rec.Code != http.StatusNoContent {
		t.Errorf("done: %d", rec.Code)
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
	again := w.inbox.list()[0]
	if c, out := accept(admin, again.ID); c != http.StatusOK || out["uuid"] != "" || out["reason"] == "" || w.inbox.count() != 1 {
		t.Fatalf("duplicate: %d %v", c, out)
	}
}
