//go:build sqlite_fts5 || fts5

package main

import (
	"testing"
)

// Calibre's metadata.db keeps full-text indexes of annotations (FTS5),
// updated by triggers whenever annotations change, including when a book
// is deleted. Removing and restoring must keep them consistent.
func TestRecycleCalibreAnnotationsFTS(t *testing.T) {
	db := setupRecycleLibrary(t)
	if _, err := db.Exec(`
	DROP TABLE annotations_fts_data; -- setupRecycleLibrary's stand-in
	CREATE TABLE annotations (id INTEGER PRIMARY KEY, book INTEGER NOT NULL, format TEXT NOT NULL COLLATE NOCASE,
		user_type TEXT NOT NULL, user TEXT NOT NULL, timestamp REAL NOT NULL, annot_id TEXT NOT NULL,
		annot_type TEXT NOT NULL, annot_data TEXT NOT NULL, searchable_text TEXT NOT NULL DEFAULT "",
		UNIQUE(book, user_type, user, format, annot_type, annot_id));
	CREATE VIRTUAL TABLE annotations_fts USING fts5(searchable_text, content = 'annotations', content_rowid = 'id', tokenize = 'unicode61 remove_diacritics 2');
	CREATE VIRTUAL TABLE annotations_fts_stemmed USING fts5(searchable_text, content = 'annotations', content_rowid = 'id', tokenize = 'porter unicode61 remove_diacritics 2');
	CREATE TRIGGER annotations_fts_insert_trg AFTER INSERT ON annotations BEGIN
		INSERT INTO annotations_fts(rowid, searchable_text) VALUES (NEW.id, NEW.searchable_text);
		INSERT INTO annotations_fts_stemmed(rowid, searchable_text) VALUES (NEW.id, NEW.searchable_text);
	END;
	CREATE TRIGGER annotations_fts_delete_trg AFTER DELETE ON annotations BEGIN
		INSERT INTO annotations_fts(annotations_fts, rowid, searchable_text) VALUES('delete', OLD.id, OLD.searchable_text);
		INSERT INTO annotations_fts_stemmed(annotations_fts_stemmed, rowid, searchable_text) VALUES('delete', OLD.id, OLD.searchable_text);
	END;
	DROP TRIGGER books_delete_trg;
	CREATE TRIGGER books_delete_trg AFTER DELETE ON books BEGIN
		DELETE FROM books_authors_link WHERE book=OLD.id;
		DELETE FROM data WHERE book=OLD.id;
		DELETE FROM comments WHERE book=OLD.id;
		DELETE FROM annotations WHERE book=OLD.id;
	END;`); err != nil {
		t.Fatal(err)
	}
	a := addRecycleTestBook(t, db, "Alpha", []string{"A"}, nil)
	plain := addRecycleTestBook(t, db, "Plain", []string{"B"}, nil) // no annotations
	if _, err := db.Exec(`INSERT INTO annotations (book, format, user_type, user, timestamp, annot_id, annot_type, annot_data, searchable_text)
		VALUES (?, 'EPUB', 'local', 'viewer', 1, 'x1', 'highlight', '{}', 'whales are mammals')`, a.id); err != nil {
		t.Fatal(err)
	}
	matches := func(q string) int {
		t.Helper()
		return count(t, db, `SELECT COUNT(*) FROM annotations_fts WHERE annotations_fts MATCH ?`, q)
	}
	check := func() {
		t.Helper()
		for _, tbl := range []string{"annotations_fts", "annotations_fts_stemmed"} {
			if _, err := db.Exec(`INSERT INTO ` + tbl + `(` + tbl + `) VALUES('integrity-check')`); err != nil {
				t.Fatalf("%s: %v", tbl, err)
			}
		}
	}

	batch, err := recycleBooks([]string{a.uuid, plain.uuid}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM annotations`); n != 0 || matches("whales") != 0 {
		t.Fatalf("annotations left: %d rows, %d matches", n, matches("whales"))
	}
	check()

	if res, err := restoreRecycled(batch.ID, nil, "admin"); err != nil || len(res) != 2 || res[0].Error != "" || res[1].Error != "" {
		t.Fatalf("restore: %+v %v", res, err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM annotations WHERE book = ?`, a.id); n != 1 || matches("whales") != 1 {
		t.Fatalf("annotation not back: %d rows, %d matches", n, matches("whales"))
	}
	check()
}
