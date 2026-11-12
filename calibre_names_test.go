//go:build sqlite_fts5 || fts5

package main

import (
	"path/filepath"
	"testing"
)

func TestRepairFormatNames(t *testing.T) {
	db := setupRealCalibreLibrary(t)

	// add puts a book in metadata.db with these formats (data.name as
	// recorded) and these files in its folder.
	add := func(title string, names map[string]string, files ...string) (id int64, dir string) {
		t.Helper()
		var formats []formatEntry
		for ext := range names {
			formats = append(formats, formatEntry{Ext: ext, Filename: title + "." + ext, Size: 4})
		}
		id, _, path, err := insertBookIntoCalibreDB(db, &UploadedBookMeta{OPFMetadata: OPFMetadata{Title: title, Creators: []string{"A"}}}, formats)
		if err != nil {
			t.Fatal(err)
		}
		for ext, name := range names {
			if _, err := db.Exec(`UPDATE data SET name = ? WHERE book = ? AND format = upper(?)`, name, id, ext); err != nil {
				t.Fatal(err)
			}
		}
		dir = filepath.Join(baseDir, path)
		for _, f := range files {
			writeTestFile(t, filepath.Join(dir, f), []byte("book"))
		}
		return id, dir
	}
	name := func(id int64, format string) string {
		t.Helper()
		var n string
		if err := db.QueryRow(`SELECT name FROM data WHERE book = ? AND format = ?`, id, format).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Added by an older version: recorded under the upload's name.
	misnamed, _ := add("Dune", map[string]string{"epub": "dune_scan_v2", "pdf": "Dune"}, "Dune.epub", "Dune.pdf")
	right, _ := add("Emma", map[string]string{"epub": "Emma"}, "Emma.epub")
	twoFiles, _ := add("Persuasion", map[string]string{"epub": "p"}, "Persuasion.epub", "Persuasion_x.epub")
	noFile, _ := add("Missing", map[string]string{"epub": "gone"})
	// Calibre looks for ".epub": an ".EPUB" file can't be fixed by the name alone.
	upper, _ := add("Upper", map[string]string{"epub": "u"}, "Upper.EPUB")

	// Read-only: nothing changes.
	if n := repairFormatNames(db, baseDir, false); n != 0 || name(misnamed, "EPUB") != "dune_scan_v2" {
		t.Fatalf("read-only repaired %d, name %q", n, name(misnamed, "EPUB"))
	}

	if n := repairFormatNames(db, baseDir, true); n != 1 {
		t.Errorf("repaired %d formats, want 1", n)
	}
	for _, c := range []struct {
		id     int64
		format string
		want   string
	}{
		{misnamed, "EPUB", "Dune"},
		{misnamed, "PDF", "Dune"},
		{right, "EPUB", "Emma"},
		{twoFiles, "EPUB", "p"},
		{noFile, "EPUB", "gone"},
		{upper, "EPUB", "u"},
	} {
		if got := name(c.id, c.format); got != c.want {
			t.Errorf("book %d %s: data.name %q, want %q", c.id, c.format, got, c.want)
		}
	}
	if n := repairFormatNames(db, baseDir, true); n != 0 {
		t.Errorf("a second run repaired %d", n)
	}
}
