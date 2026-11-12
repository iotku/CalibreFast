//go:build !sqlite_fts5 && !fts5

package main

import (
	"strings"
	"testing"
)

// Built without FTS5, a library with Calibre's full-text tables is refused
// with a message that says how to build, before anything is touched.
func TestRecycleWithoutFTS5(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addRecycleTestBook(t, db, "Alpha", []string{"A"}, nil)
	// This build can't create the table, so write its schema entry as Calibre would have.
	if _, err := db.Exec(`PRAGMA writable_schema = ON;
		INSERT INTO sqlite_master (type, name, tbl_name, rootpage, sql) VALUES ('table', 'annotations_fts', 'annotations_fts', 0,
			'CREATE VIRTUAL TABLE annotations_fts USING fts5(searchable_text, tokenize = ''unicode61 remove_diacritics 2'')');
		PRAGMA writable_schema = OFF;`); err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(0) // a new connection reads the schema again
	db.SetMaxIdleConns(0)
	db.SetMaxOpenConns(1)

	_, err := recycleBooks([]string{a.uuid}, "admin")
	if err == nil || !strings.Contains(err.Error(), "-tags sqlite_fts5") {
		t.Fatalf("got %v", err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books`); n != 1 {
		t.Fatal("book removed")
	}
	if !exists(baseDir + "/" + a.path) {
		t.Fatal("files moved")
	}
	if b, _ := listRecycleBatches(); len(b) != 0 {
		t.Fatal("batch left behind")
	}
}
