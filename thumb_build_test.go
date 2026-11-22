package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// thumbLibrary: books whose cover.jpg is in the library (with), and one that
// says it has a cover but has no file (bare).
func thumbLibrary(t *testing.T, db *sql.DB, with int) (covered []string, bare string) {
	t.Helper()
	cover := testJPEG(t, 1200, 1800, 90)
	for i := range with {
		uuid := addTestBook(t, db, "Covered "+string(rune('A'+i)), []string{"A"}, nil, "")
		var path string
		if err := db.QueryRow(`SELECT path FROM books WHERE uuid = ?`, uuid).Scan(&path); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(baseDir, path, "cover.jpg"), cover)
		covered = append(covered, uuid)
	}
	bare = addTestBook(t, db, "Bare", []string{"A"}, nil, "")
	if _, err := db.Exec(`UPDATE books SET has_cover = 1`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, u := range append(covered, bare) {
			forgetBook(u)
		}
	})
	return covered, bare
}

// -build-thumbnails makes what's missing, and only that; a book whose
// cover.jpg isn't there is counted, not a failure.
func TestBuildThumbnails(t *testing.T) {
	db := catalogFixture(t)
	covered, bare := thumbLibrary(t, db, 2)

	var out bytes.Buffer
	if err := buildThumbnails(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "3/3: 2 made, 0 already there, 1 without cover.jpg, 0 failed") {
		t.Errorf("first run:\n%s", out.String())
	}
	for _, u := range covered {
		if !thumbCached(u) {
			t.Errorf("%s: no thumbnail", u)
		}
	}
	if thumbCached(bare) {
		t.Error("a thumbnail for a book without cover.jpg")
	}
	// Whole files only: no temporary ones left behind.
	entries, _ := os.ReadDir(filepath.Join(cacheDir, "covers", "thumb"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("left %s", e.Name())
		}
	}

	out.Reset()
	if err := buildThumbnails(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "3/3: 0 made, 2 already there, 1 without cover.jpg, 0 failed") {
		t.Errorf("second run:\n%s", out.String())
	}
}

// Stopped (Ctrl-C): it returns at once, with nothing half-written.
func TestBuildThumbnailsStopped(t *testing.T) {
	db := catalogFixture(t)
	thumbLibrary(t, db, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if err := buildThumbnails(ctx, &out); err == nil {
		t.Error("a stopped run said it finished")
	}
	entries, _ := os.ReadDir(filepath.Join(cacheDir, "covers", "thumb"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("left %s", e.Name())
		}
	}
}
