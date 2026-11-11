package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A new cover gives the book a new stamp everywhere its cover URLs come
// from (book page, library grid), so browsers don't keep the old image.
func TestCoverChangedNewStamp(t *testing.T) {
	db, tempDir := setupTestDB(t)
	t.Cleanup(func() { db.Close(); os.RemoveAll(tempDir) })
	origDB, origBase := calibreDB, baseDir
	calibreDB, baseDir = db, tempDir
	t.Cleanup(func() { calibreDB, baseDir = origDB, origBase })
	t.Chdir(tempDir) // pages/ is relative to the working directory

	_, uuid, _, err := insertBookIntoCalibreDB(db, &UploadedBookMeta{OPFMetadata: OPFMetadata{Title: "Covered", Creators: []string{"A"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE books SET has_cover = 0, last_modified = '2020-01-02 03:04:05+00:00' WHERE uuid = ?`, uuid); err != nil {
		t.Fatal(err)
	}
	const old = "20200102030405"
	modTimeCache.Store(uuid, old)
	pageOfBook.Store(uuid, 1)
	t.Cleanup(func() { modTimeCache.Delete(uuid); pageOfBook.Delete(uuid); uuidPathIndex.Delete(uuid) })
	if err := writePage(1, []Book{{UUID: "other", LastModified: old}, {UUID: uuid, Title: "Covered", LastModified: old}}); err != nil {
		t.Fatal(err)
	}
	version := pagesVersion.Load()

	coverChanged(uuid)

	stamp, _ := modTimeCache.Load(uuid)
	if stamp == old {
		t.Error("book page still links the old cover")
	}
	var hasCover bool
	_ = db.QueryRow(`SELECT has_cover FROM books WHERE uuid = ?`, uuid).Scan(&hasCover)
	if !hasCover {
		t.Error("has_cover not set")
	}
	data, err := os.ReadFile(filepath.Join("pages", "page1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var books []Book
	if err := json.Unmarshal(data, &books); err != nil {
		t.Fatal(err)
	}
	if len(books) != 2 || books[0].LastModified != old || books[1].LastModified != stamp {
		t.Errorf("page1 = %+v, want only %s's stamp changed to %v", books, uuid, stamp)
	}
	if pagesVersion.Load() == version {
		t.Error("pages version unchanged: browsers keep the cached page")
	}
}
