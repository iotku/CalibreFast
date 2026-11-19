package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func postImportBulk(t *testing.T, u *User, body any) (int, bulkResult) {
	t.Helper()
	data, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/imports/bulk", bytes.NewReader(data))
	if u != nil {
		req = req.WithContext(withUserCtx(req, u))
	}
	requireEditor(importBulkHandler)(rec, req)
	var res bulkResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	return rec.Code, res
}

func strp(s string) *string { return &s }

// waitDuplicateChecks waits for checkDuplicatesSoon's run, which reads the
// globals the test's cleanup puts back.
func waitDuplicateChecks(b *importInbox) {
	for {
		b.dupMu.Lock()
		running := b.dupRunning
		b.dupMu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func TestImportQueueQuickEdits(t *testing.T) {
	db := withTestLibrary(t)
	setupTestAppDB(t)
	w, dir := newTestWatcher(t)
	origImports, origWrites := imports, allowWrites
	imports, allowWrites = w.inbox, true
	t.Cleanup(func() { imports, allowWrites = origImports, origWrites })
	t.Cleanup(func() { uuidPathIndex.Clear(); bookMetadataCache.Clear() })
	t.Cleanup(func() { waitDuplicateChecks(w.inbox) })
	admin, _ := createUser("queueadmin", "correct horse battery", true)
	editor, _ := createUser("queueeditor", "correct horse battery", false)
	mustExec(t, appDB, `UPDATE users SET is_editor = 1 WHERE id = ?`, editor.ID)
	editor, _ = getUserByID(editor.ID)
	reader, _ := createUser("queuereader", "correct horse battery", false)

	writeTestFile(t, filepath.Join(dir, "one", "Book.epub"), importTestEPUB(t))
	writeTestFile(t, filepath.Join(dir, "two", "Other.pdf"), []byte("%PDF-1.4 other"))
	w.scan()
	w.scan()
	items := w.inbox.list()
	if len(items) != 2 {
		t.Fatalf("%d waiting", len(items))
	}
	epub, pdf := items[0], items[1]
	if epub.Files[0].Format != "epub" {
		epub, pdf = pdf, epub
	}
	sourcesBefore, _ := json.Marshal(epub.Sources)

	// Only editors, only with writes, only good ids, only real changes.
	edit := bulkEditRequest{UUIDs: []string{epub.ID}, Edit: bulkEdit{
		Title: strp("Edited Title"), Authors: strp("Jane Doe & John Roe"),
		Series: strp("Saga"), SeriesIndex: strp("2"), AddTags: []string{"Queued"},
	}}
	if c, _ := postImportBulk(t, reader, edit); c != http.StatusNotFound {
		t.Errorf("a reader edited the queue: %d", c)
	}
	allowWrites = false
	if c, _ := postImportBulk(t, editor, edit); c != http.StatusForbidden {
		t.Errorf("read-only server: %d", c)
	}
	allowWrites = true
	if c, _ := postImportBulk(t, editor, bulkEditRequest{UUIDs: []string{"../x"}, Edit: edit.Edit}); c != http.StatusBadRequest {
		t.Errorf("bad id: %d", c)
	}
	if c, _ := postImportBulk(t, editor, bulkEditRequest{UUIDs: []string{epub.ID}}); c != http.StatusBadRequest {
		t.Errorf("empty edit: %d", c)
	}

	// Its duplicate check before the edit, which the edit must drop.
	w.inbox.duplicates(epub)
	if _, ok, _ := w.inbox.cachedDuplicates(epub); !ok {
		t.Fatal("not checked")
	}

	c, res := postImportBulk(t, editor, edit)
	if c != http.StatusOK || res.Changed != 1 || len(res.Books) != 1 {
		t.Fatalf("edit: %d %+v", c, res)
	}
	row := res.Books[0]
	if row.UUID != epub.ID || row.Title != "Edited Title" || row.Authors != "Jane Doe & John Roe" ||
		row.Series != "Saga" || row.SeriesIndex != 2 || !slices.Contains(row.Tags, "Queued") {
		t.Errorf("row: %+v", row)
	}
	now := w.inbox.get(epub.ID)
	if now == epub {
		t.Fatal("edited in place: whoever held the old copy saw it change")
	}
	if epub.Merged.Title == "Edited Title" {
		t.Error("the old copy changed")
	}
	if got, _ := json.Marshal(now.Sources); !bytes.Equal(got, sourcesBefore) {
		t.Error("what the files say changed")
	}
	if now.Merged.GetMeta("calibre:series") != "Saga" || now.Merged.GetMeta("calibre:series_index") != "2" {
		t.Errorf("series not in Meta (what approve reads): %+v", now.Merged.Meta)
	}
	// The check kept is of the edited copy (the background recheck may
	// have made it already), never of the one before.
	checkedFor := func() *pendingImport {
		w.inbox.dupMu.Lock()
		defer w.inbox.dupMu.Unlock()
		return w.inbox.dups[epub.ID].it
	}
	if checkedFor() == epub {
		t.Error("the duplicate check made before the edit is still kept")
	}
	// A check of the old copy finishing after the edit isn't kept.
	w.inbox.keepDuplicates(epub, &duplicateCheck{}, nil)
	if checkedFor() == epub {
		t.Error("a check of the title before the edit was kept")
	}
	// Nor shown for the new one, were it kept.
	w.inbox.dupMu.Lock()
	saved := w.inbox.dups[epub.ID]
	w.inbox.dups[epub.ID] = dupResult{&duplicateCheck{}, nil, epub}
	w.inbox.dupMu.Unlock()
	if _, ok, _ := w.inbox.cachedDuplicates(now); ok {
		t.Error("a check of the old copy is shown for the edited one")
	}
	w.inbox.dupMu.Lock()
	w.inbox.dups[epub.ID] = saved
	w.inbox.dupMu.Unlock()

	// On disk too: a restart finds the edit.
	again, err := openInbox(w.inbox.dir)
	if err != nil {
		t.Fatal(err)
	}
	if m := again.get(epub.ID).Merged; m.Title != "Edited Title" || !slices.Equal(m.Creators, []string{"Jane Doe", "John Roe"}) {
		t.Errorf("after a restart: %+v", m.OPFMetadata)
	}

	// The same edit again changes nothing.
	if _, res := postImportBulk(t, editor, edit); res.Changed != 0 || res.Unchanged != 1 {
		t.Errorf("again: %+v", res)
	}

	// A book being added (taken out of the queue) isn't edited.
	taken := w.inbox.take(pdf.ID)
	if _, res := postImportBulk(t, editor, bulkEditRequest{UUIDs: []string{pdf.ID}, Edit: bulkEdit{Title: strp("X")}}); len(res.Failed) != 1 || res.Changed != 0 {
		t.Errorf("taken book: %+v", res)
	}
	w.inbox.put(taken)

	// The table lists both, as edited.
	rows, _ := importTableRows(w.inbox.list())
	if len(rows) != 2 || rows[0].ID == rows[1].ID {
		t.Fatalf("rows: %+v", rows)
	}

	// Mass Import adds it as edited.
	uuid, reason := acceptImport(w.inbox.get(epub.ID), admin)
	if uuid == "" {
		t.Fatalf("accept: %s", reason)
	}
	var title, series string
	db.QueryRow(`SELECT b.title, COALESCE((SELECT s.name FROM books_series_link x JOIN series s ON s.id = x.series WHERE x.book = b.id), '')
		FROM books b WHERE b.uuid = ?`, uuid).Scan(&title, &series)
	if title != "Edited Title" || series != "Saga" {
		t.Errorf("added as %q, series %q", title, series)
	}

	// The other book renamed to the one just added: it's a duplicate now,
	// and adding it is refused (approve checks metadata.db itself).
	_, res = postImportBulk(t, editor, bulkEditRequest{UUIDs: []string{pdf.ID}, Edit: bulkEdit{Title: strp("Edited Title"), Authors: strp("Jane Doe & John Roe")}})
	if res.Changed != 1 {
		t.Fatalf("rename: %+v", res)
	}
	if !w.inbox.duplicates(w.inbox.get(pdf.ID)).found() {
		t.Error("the renamed book isn't a duplicate")
	}
	rows, _ = importTableRows(w.inbox.list())
	if len(rows) != 1 || rows[0].Library != "in library" {
		t.Errorf("rows after the recheck: %+v", rows)
	}
	if uuid, reason := acceptImport(w.inbox.get(pdf.ID), admin); uuid != "" || reason == "" {
		t.Errorf("a duplicate was added: %q %q", uuid, reason)
	}
}

// Edits while the list and Mass Import read the queue (go test -race).
func TestImportQueueEditsWhileReading(t *testing.T) {
	withTestLibrary(t)
	w, dir := newTestWatcher(t)
	origImports := imports
	imports = w.inbox
	t.Cleanup(func() { imports = origImports })
	t.Cleanup(func() { waitDuplicateChecks(w.inbox) })
	writeTestFile(t, filepath.Join(dir, "a", "A.pdf"), []byte("%PDF-1.4 a"))
	w.scan()
	w.scan()
	id := w.inbox.list()[0].ID

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, it := range w.inbox.list() {
				_ = it.Merged.Title + it.Merged.Series
				_ = len(it.Merged.Subjects)
			}
			importTableRows(w.inbox.list())
		}
	})
	for i := range 50 {
		title := "Title " + string(rune('a'+i%26))
		w.inbox.editMerged([]string{id}, &bulkEdit{Title: &title, AddTags: []string{title}})
	}
	close(stop)
	wg.Wait()
	if n := len(w.inbox.get(id).Merged.Subjects); n == 0 {
		t.Error("no tags after the edits")
	}
}
