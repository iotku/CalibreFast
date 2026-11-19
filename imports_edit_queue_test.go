package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A waiting book with a writer that never runs (a slow disk): edits are
// journalled, and item.json is written only when flushAll is called.
func newSlowInbox(t *testing.T, files ...string) (*folderWatcher, *pendingImport) {
	t.Helper()
	setupTestAppDB(t)
	w, dir := newTestWatcher(t)
	for _, f := range files {
		data := []byte("%PDF-1.4 " + f)
		if filepath.Ext(f) == ".epub" {
			data = importTestEPUB(t)
		}
		writeTestFile(t, filepath.Join(dir, f), data)
	}
	w.scan()
	w.scan()
	items := w.inbox.list()
	if len(items) != 1 {
		t.Fatalf("%d waiting", len(items))
	}
	w.inbox.mu.Lock()
	w.inbox.flushWake = make(chan struct{}, 1)
	w.inbox.mu.Unlock()
	return w, items[0]
}

func journalRows(t *testing.T, id string) int {
	t.Helper()
	var n int
	if err := appDB.QueryRow(`SELECT COUNT(*) FROM import_edit_journal WHERE item_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func itemJSONTitle(t *testing.T, dir, id string) string {
	t.Helper()
	it, err := loadPendingImport(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	return it.Merged.Title
}

// An edit is answered once it's in calibrefast.db; item.json follows. A
// crash before it does loses nothing.
func TestImportEditSavedBeforeItemJSON(t *testing.T) {
	w, it := newSlowInbox(t, "Book.epub")
	dir := w.inbox.itemDir(it.ID)

	if _, res := w.inbox.editMerged([]string{it.ID}, &bulkEdit{Title: strp("Queued Title")}); res.Changed != 1 {
		t.Fatalf("%+v", res)
	}
	if w.inbox.get(it.ID).Merged.Title != "Queued Title" {
		t.Error("not in memory")
	}
	if title := itemJSONTitle(t, dir, it.ID); title == "Queued Title" {
		t.Fatal("item.json written before answering")
	}
	if journalRows(t, it.ID) != 1 {
		t.Fatal("not journalled")
	}

	// The server crashes now, and starts again.
	again, err := openInbox(w.inbox.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.replayImportEdits(); err != nil {
		t.Fatal(err)
	}
	if again.get(it.ID).Merged.Title != "Queued Title" {
		t.Error("lost in a restart")
	}

	// A second edit before the first is written: both end up in item.json.
	w.inbox.editMerged([]string{it.ID}, &bulkEdit{AddTags: []string{"later"}})
	if w.inbox.flushAll(time.Time{}) {
		t.Fatal("still behind")
	}
	got, err := loadPendingImport(dir, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Merged.Title != "Queued Title" || len(got.Merged.Subjects) == 0 || got.Merged.Subjects[len(got.Merged.Subjects)-1] != "later" {
		t.Errorf("item.json %+v", got.Merged.OPFMetadata)
	}
	if journalRows(t, it.ID) != 0 {
		t.Error("journal row left after writing")
	}
}

// A newer edit made while an older one is being written keeps its row.
func TestImportEditNewerKeepsRow(t *testing.T) {
	w, it := newSlowInbox(t, "Book.epub")
	w.inbox.editMerged([]string{it.ID}, &bulkEdit{Title: strp("One")})
	w.inbox.mu.Lock()
	v := w.inbox.dirty[it.ID]
	w.inbox.mu.Unlock()
	w.inbox.editMerged([]string{it.ID}, &bulkEdit{Title: strp("Two")})
	// The row for v is gone only if v is the latest.
	if _, err := appDB.Exec(`DELETE FROM import_edit_journal WHERE item_id = ? AND version = ?`, it.ID, v); err != nil {
		t.Fatal(err)
	}
	if journalRows(t, it.ID) != 1 {
		t.Error("the newer edit's row went with the older one")
	}
}

// Discarding a book saves its edits into the discarded copy first.
func TestImportEditDiscardKeepsEdits(t *testing.T) {
	w, it := newSlowInbox(t, "Book.epub")
	w.inbox.editMerged([]string{it.ID}, &bulkEdit{Title: strp("Edited Then Discarded")})
	if err := w.inbox.discard(it.ID); err != nil {
		t.Fatal(err)
	}
	if title := itemJSONTitle(t, filepath.Join(w.inbox.discardedDir(), it.ID), it.ID); title != "Edited Then Discarded" {
		t.Errorf("discarded copy has %q", title)
	}
	if journalRows(t, it.ID) != 0 {
		t.Error("journal row left")
	}
}

// Splitting starts both books again from their files: the edit isn't
// replayed over the one that stays.
func TestImportEditSplitDropsEdits(t *testing.T) {
	w, it := newSlowInbox(t, "Book.epub", "Book.pdf")
	if len(it.Files) != 2 {
		t.Fatalf("files %+v", it.Files)
	}
	w.inbox.editMerged([]string{it.ID}, &bulkEdit{Title: strp("Before Split")})
	if _, err := w.inbox.splitOff(it.ID, map[string]bool{it.Files[1].Name: true}, ""); err != nil {
		t.Fatal(err)
	}
	if journalRows(t, it.ID) != 0 {
		t.Error("journal row left")
	}
	again, err := openInbox(w.inbox.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.replayImportEdits(); err != nil {
		t.Fatal(err)
	}
	if kept := again.get(it.ID); kept == nil || len(kept.Files) != 1 {
		t.Errorf("after split and restart: %+v", kept)
	}
}

// On start, a row for a book that's left the folder is dropped; one for
// another queue folder is kept.
func TestImportEditReplayDropsGone(t *testing.T) {
	w, _ := newSlowInbox(t, "Book.epub")
	gone, other := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	mustExec(t, appDB, `INSERT INTO import_edit_journal (item_id, dir, version, merged, updated_at) VALUES (?, ?, 1, '{}', 0), (?, ?, 2, '{}', 0)`,
		gone, w.inbox.dir, other, filepath.Join(os.TempDir(), "elsewhere"))
	if err := w.inbox.replayImportEdits(); err != nil {
		t.Fatal(err)
	}
	if journalRows(t, gone) != 0 {
		t.Error("row for a book no longer waiting kept")
	}
	if journalRows(t, other) != 1 {
		t.Error("row for another queue folder dropped")
	}
}

// The writer brings item.json up to date by itself.
func TestImportEditWriterWrites(t *testing.T) {
	w, it := newSlowInbox(t, "Book.epub")
	w.inbox.mu.Lock()
	w.inbox.flushWake = nil
	w.inbox.mu.Unlock()
	w.inbox.startItemWriter()
	w.inbox.editMerged([]string{it.ID}, &bulkEdit{Title: strp("Written Later")})
	deadline := time.Now().Add(5 * time.Second)
	for itemJSONTitle(t, w.inbox.itemDir(it.ID), it.ID) != "Written Later" || journalRows(t, it.ID) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("item.json never written")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
