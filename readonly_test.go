package main

import (
	"crypto/sha256"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// libraryState is every file under dir: its contents' hash, size and
// modification time.
func libraryState(t *testing.T, dir string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		state[p] = string(sum[:]) + info.ModTime().String()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// A path SQLite's URI form has to escape opens read-only, and refuses writes.
func TestCalibreDSNReadOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My Library #1 100%?")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "metadata.db")
	// The same URI form, allowed to create it (mode=rwc).
	rw, err := sql.Open("sqlite3", strings.Replace(calibreDSN(path, false), "mode=rw", "mode=rwc", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(`CREATE TABLE books (id INTEGER PRIMARY KEY, title TEXT); INSERT INTO books (title) VALUES ('Dune')`); err != nil {
		t.Fatal(err)
	}
	rw.Close()

	ro, err := sql.Open("sqlite3", calibreDSN(path, true))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var title string
	if err := ro.QueryRow(`SELECT title FROM books`).Scan(&title); err != nil || title != "Dune" {
		t.Fatalf("read through %q: %q, %v", calibreDSN(path, true), title, err)
	}
	if _, err := ro.Exec(`UPDATE books SET title = 'Changed'`); err == nil || !strings.Contains(err.Error(), "readonly") {
		t.Errorf("write through a read-only connection: %v", err)
	}
	// Nothing next to it: no journal, no new file.
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("files beside metadata.db: %v", entries)
	}
	// Read-write never creates a database that isn't there.
	missing, _ := sql.Open("sqlite3", calibreDSN(filepath.Join(dir, "missing.db"), false))
	defer missing.Close()
	if err := missing.Ping(); err == nil {
		t.Error("read-write open created a missing database")
	}
}

// Without -allow-writes, starting on a library and using it leaves every file
// in it as it was, and nothing can write to metadata.db, even code that
// tries. It also works on a library that's read-only on disk.
func TestReadOnlyServerLeavesLibraryUntouched(t *testing.T) {
	db, dir := setupTestDB(t)
	_, uuid, bookPath, err := insertBookIntoCalibreDB(db, &UploadedBookMeta{OPFMetadata: OPFMetadata{Title: "Dune", Creators: []string{"Frank Herbert"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, bookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bookPath, "Dune.epub"), []byte("not really an epub"), 0o644); err != nil {
		t.Fatal(err)
	}
	db.Close()

	origDB, origBase, origWrites := calibreDB, baseDir, allowWrites
	t.Cleanup(func() {
		if calibreDB != nil && calibreDB != origDB {
			calibreDB.Close()
		}
		calibreDB, baseDir, allowWrites = origDB, origBase, origWrites
		libraryCatalog.Store(nil) // writeLibraryPages built one for this library
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil {
				_ = os.Chmod(p, 0o755) // so the temp dir can be removed
			}
			return nil
		})
		os.RemoveAll(dir)
	})
	baseDir, allowWrites = dir, false
	t.Chdir(t.TempDir()) // ./pages

	before := libraryState(t, dir)
	time.Sleep(10 * time.Millisecond) // a write would show in the modification times

	// Startup, as main does it.
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	initCalibreDB()

	// Reading works.
	if !bookExists(uuid) {
		t.Error("the book isn't found")
	}
	if books, err := booksByUUIDs([]string{uuid}); err != nil || len(books) != 1 {
		t.Errorf("booksByUUIDs: %v, %v", books, err)
	}
	if got := resumeURL(uuid, ""); got != "/read?uuid="+uuid {
		t.Errorf("resumeURL %q", got)
	}

	// Writing doesn't, wherever it comes from.
	if _, err := calibreDB.Exec(`UPDATE books SET title = 'Changed'`); err == nil {
		t.Error("an UPDATE through calibreDB succeeded")
	}
	if _, err := updateBookInCalibreDB(calibreDB, baseDir, uuid, &OPFMetadata{Title: "Changed", Creators: []string{"Someone"}}); err == nil {
		t.Error("the metadata editor's save succeeded")
	}

	// The checks above ran on writable files, so it's the read-only
	// connection that refused. Now read-only on disk too, as a user trying
	// the server out might make it (no effect when the tests run as root):
	// starting and reading still work.
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o555)
		}
		return os.Chmod(p, 0o444)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatalf("startup on a read-only library: %v", err)
	}
	calibreDB.Close()
	initCalibreDB()
	if !bookExists(uuid) {
		t.Error("the book isn't found on a read-only library")
	}

	if after := libraryState(t, dir); len(after) != len(before) {
		t.Errorf("files in the library: %d before, %d after", len(before), len(after))
	} else {
		for p, s := range before {
			if after[p] != s {
				t.Errorf("%s changed", p)
			}
		}
	}
}
