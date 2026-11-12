//go:build sqlite_fts5 || fts5

package main

import (
	"errors"
	"os"
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

	// What the repairs page lists.
	found, err := findMisnamedFormats(db, baseDir)
	if err != nil || len(found) != 1 || found[0].title != "Dune" || found[0].from != "dune_scan_v2" || found[0].to != "Dune" {
		t.Fatalf("found %+v, %v", found, err)
	}

	// Read-only (no -allow-writes): nothing changes.
	orig := allowWrites
	t.Cleanup(func() { allowWrites = orig })
	allowWrites = false
	if n, err := repairFormatNames(db, baseDir); !errors.Is(err, errReadOnly) || n != 0 || name(misnamed, "EPUB") != "dune_scan_v2" {
		t.Fatalf("read-only repaired %d (%v), name %q", n, err, name(misnamed, "EPUB"))
	}
	allowWrites = true

	if n, err := repairFormatNames(db, baseDir); err != nil || n != 1 {
		t.Errorf("repaired %d formats (%v), want 1", n, err)
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
	if n, err := repairFormatNames(db, baseDir); err != nil || n != 0 {
		t.Errorf("a second run repaired %d (%v)", n, err)
	}
}

func TestAddUnlistedFormats(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, nil, nil)
	c := addOPFTestBook(t, db, "Gamma", []string{"Cat"}, nil, nil)
	listed := addOPFTestBook(t, db, "Listed", []string{"Lee"}, nil, nil)
	for _, x := range []testBook{a, b, c} {
		if _, err := db.Exec(`DELETE FROM data WHERE book = ?`, x.id); err != nil {
			t.Fatal(err)
		}
	}
	// Alpha: an EPUB and a PDF. Beta: two EPUBs (which one is it?).
	write := func(x testBook, name, body string) {
		if err := os.WriteFile(filepath.Join(baseDir, x.path, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(a, "alpha file.pdf", "PDF")
	write(b, "Beta copy.epub", "EPUB 2")

	// A read-only server (no -allow-writes) writes nothing.
	orig := allowWrites
	t.Cleanup(func() { allowWrites = orig })
	allowWrites = false
	if _, err := addUnlistedFormats(db, baseDir, nil); !errors.Is(err, errReadOnly) {
		t.Fatalf("read-only: %v, want errReadOnly", err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM data WHERE book IN (?, ?, ?)`, a.id, b.id, c.id); n != 0 {
		t.Fatalf("read-only server wrote %d rows", n)
	}
	allowWrites = true

	// Only the books named.
	res, err := addUnlistedFormats(db, baseDir, []string{a.uuid, b.uuid, listed.uuid})
	if err != nil {
		t.Fatal(err)
	}
	if res.Books != 1 || res.Formats != 2 || len(res.Skipped) != 1 || res.Skipped[0].UUID != b.uuid {
		t.Errorf("result %+v", res)
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM data WHERE book = ? AND format = 'PDF'`, a.id).Scan(&name); err != nil || name != "alpha file" {
		t.Errorf("Alpha's PDF row: %q, %v", name, err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM data WHERE book = ?`, c.id); n != 0 {
		t.Error("a book not named was changed")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM data WHERE book = ?`, listed.id); n != 1 {
		t.Errorf("a listed book has %d rows, want its 1", n)
	}

	// Every book: Gamma now; Alpha has rows, so it's left alone.
	if res, err = addUnlistedFormats(db, baseDir, nil); err != nil || res.Books != 1 || res.Formats != 1 {
		t.Errorf("every book: %+v, %v", res, err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM data WHERE book = ?`, a.id); n != 2 {
		t.Errorf("Alpha has %d rows, want 2", n)
	}
}
