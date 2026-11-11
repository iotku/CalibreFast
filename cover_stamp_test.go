package main

import (
	"os"
	"testing"
)

// A new cover marks the book modified in metadata.db, as Calibre does.
func TestCoverChangedMarksModified(t *testing.T) {
	db, tempDir := setupTestDB(t)
	t.Cleanup(func() { db.Close(); os.RemoveAll(tempDir) })
	origDB := calibreDB
	calibreDB = db
	t.Cleanup(func() { calibreDB = origDB })

	_, uuid, _, err := insertBookIntoCalibreDB(db, &UploadedBookMeta{OPFMetadata: OPFMetadata{Title: "Covered", Creators: []string{"A"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const old = "2020-01-02 03:04:05+00:00"
	if _, err := db.Exec(`UPDATE books SET has_cover = 0, last_modified = ? WHERE uuid = ?`, old, uuid); err != nil {
		t.Fatal(err)
	}

	coverChanged(uuid)

	var hasCover bool
	var modified string
	if err := db.QueryRow(`SELECT has_cover, last_modified FROM books WHERE uuid = ?`, uuid).Scan(&hasCover, &modified); err != nil {
		t.Fatal(err)
	}
	if !hasCover || coverStamp(modified) == coverStamp(old) {
		t.Errorf("has_cover=%t last_modified=%q, want true and newer than %q", hasCover, modified, old)
	}
}
