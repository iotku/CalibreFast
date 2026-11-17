package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDiscardExistingFiles(t *testing.T) {
	withTestLibrary(t)
	setupTestAppDB(t)
	w, dir := newTestWatcher(t)
	origImports := imports
	imports = w.inbox
	t.Cleanup(func() { imports = origImports })
	admin, _ := createUser("dupadmin", "correct horse battery", true)

	epub, pdf := importTestEPUB(t), []byte("%PDF-1.4 not really")
	add := func(files map[string][]byte) *pendingImport {
		t.Helper()
		before := w.inbox.list()
		for name, data := range files {
			writeTestFile(t, filepath.Join(dir, name), data)
		}
		w.scan()
		w.scan()
		for _, it := range w.inbox.list() {
			if !slices.Contains(before, it) {
				return it
			}
		}
		t.Fatal("not queued")
		return nil
	}

	// In the library: an EPUB and a PDF.
	first := add(map[string][]byte{"A.epub": epub, "A.pdf": pdf})
	book, err := w.inbox.approve(first.ID, metadataFromRequest(ApplyMetadataRequest{Title: "Uploaded", Creators: "Someone"}), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { uuidPathIndex.Delete(book); bookMetadataCache.Delete(book) })

	whole := add(map[string][]byte{"B.epub": epub, "B.pdf": pdf})            // both in the library
	mixed := add(map[string][]byte{"C.epub": epub, "C.mobi": []byte("new")}) // only the EPUB
	other := add(map[string][]byte{"D.pdf": []byte("%PDF-1.4 another")})     // nothing

	// From one book's review page, with the editor's title: only its
	// identical file goes, and the page to go on to is the rest's.
	page := add(map[string][]byte{"E.epub": epub, "E.azw3": []byte("new too")})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/import/"+page.ID+"/discard-identical", strings.NewReader(`{"title":"Uploaded","creators":"Someone"}`))
	req.SetPathValue("id", page.ID)
	importDiscardIdenticalHandler(rec, req.WithContext(withUserCtx(req, admin)))
	var one struct {
		Discarded int
		URL       string
	}
	json.Unmarshal(rec.Body.Bytes(), &one)
	if rec.Code != http.StatusOK || one.Discarded != 1 || one.URL != "/book/add-format/"+book+"/"+page.ID {
		t.Fatalf("discard-identical: %d %s", rec.Code, rec.Body)
	}
	if left := w.inbox.get(page.ID); left == nil || len(left.Files) != 1 || left.Files[0].Name != "E.azw3" {
		t.Fatalf("left waiting: %+v", left)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/imports/discard-existing", nil)
	importDiscardExistingHandler(rec, req.WithContext(withUserCtx(req, admin)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var res discardExistingResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Discarded != 1 || res.Split != 1 || res.Files != 3 || len(res.Errors) > 0 {
		t.Fatalf("result %+v", res)
	}

	if w.inbox.get(whole.ID) != nil {
		t.Error("a book whose files are all in the library is still waiting")
	}
	if w.inbox.get(other.ID) == nil {
		t.Error("a book with none of its files in the library was discarded")
	}
	kept := w.inbox.get(mixed.ID)
	if kept == nil || len(kept.Files) != 1 || kept.Files[0].Name != "C.mobi" || len(kept.Sources) != 1 {
		t.Fatalf("the mixed book keeps %+v", kept)
	}
	if kept.Into != book {
		t.Errorf("its other format is for %q, want the library book %q", kept.Into, book)
	}
	if !exists(filepath.Join(w.inbox.itemDir(mixed.ID), "C.mobi")) || exists(filepath.Join(w.inbox.itemDir(mixed.ID), "C.epub")) {
		t.Error("files not split")
	}
	again, err := loadPendingImport(w.inbox.itemDir(mixed.ID), mixed.ID)
	if err != nil || again.Into != book || len(again.Files) != 1 {
		t.Errorf("item.json after the split: %+v %v", again, err)
	}

	// The split-off EPUB was discarded as a book of its own: undoable.
	var off string
	if err := appDB.QueryRow(`SELECT target FROM edit_log WHERE action = 'discard import' AND target != ? ORDER BY id DESC LIMIT 1`, whole.ID).Scan(&off); err != nil {
		t.Fatal(err)
	}
	if err := revertImportDiscard(&editLogEntry{Target: off}, admin); err != nil {
		t.Fatal(err)
	}
	if back := w.inbox.get(off); back == nil || len(back.Files) != 1 || back.Files[0].Name != "C.epub" {
		t.Errorf("undone: %+v", back)
	}
}
