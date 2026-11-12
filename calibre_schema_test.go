//go:build sqlite_fts5 || fts5

package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestData/calibre-schema.sql is the schema (no data) of a real, current
// Calibre library: custom columns of three kinds, full-text indexes of
// annotations, page counts, book_storage, and Calibre's triggers.

var shadowTableRe = regexp.MustCompile(`^CREATE TABLE '\w+_(data|idx|docsize|config|content)'\(`)

// setupRealCalibreLibrary is a library with that schema, as calibreDB.
func setupRealCalibreLibrary(t *testing.T) *sql.DB {
	t.Helper()
	schema, err := os.ReadFile("TestData/calibre-schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	registerCalibreDriver()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3_calibre", filepath.Join(dir, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	// A dump also lists tables SQLite makes itself: sqlite_sequence (with
	// the first AUTOINCREMENT table) and each FTS5 table's shadow tables.
	var stmts []string
	for _, line := range strings.Split(string(schema), "\n") {
		if strings.HasPrefix(line, "CREATE TABLE sqlite_sequence") || shadowTableRe.MatchString(line) {
			continue
		}
		stmts = append(stmts, line)
	}
	if _, err := db.Exec(strings.Join(stmts, "\n")); err != nil {
		t.Fatal(err)
	}
	origDB, origBase, origData, origRecycle := calibreDB, baseDir, dataDir, recycleDir
	calibreDB, baseDir, dataDir, recycleDir = db, dir, t.TempDir(), ""
	t.Cleanup(func() { calibreDB, baseDir, dataDir, recycleDir = origDB, origBase, origData, origRecycle })
	t.Chdir(t.TempDir()) // pages/*.json
	setupTestAppDB(t)
	return db
}

// fill gives a book a row in every kind of table Calibre has for books.
func fillCalibreBook(t *testing.T, db *sql.DB, b testBook, n int) {
	t.Helper()
	tag := fmt.Sprint(n)
	for _, q := range []string{
		`INSERT OR IGNORE INTO custom_column_1 (value) VALUES ('genre ` + tag + `')`,
		`INSERT INTO books_custom_column_1_link (book, value) SELECT ?, id FROM custom_column_1 WHERE value = 'genre ` + tag + `'`,
		`INSERT INTO custom_column_2 (book, value) VALUES (?, 'shelf note ` + tag + `')`,
		`INSERT OR IGNORE INTO custom_column_3 (value) VALUES (` + tag + `)`,
		`INSERT INTO books_custom_column_3_link (book, value) SELECT ?, id FROM custom_column_3 WHERE value = ` + tag,
		`INSERT OR IGNORE INTO ratings (rating) VALUES (8)`,
		`INSERT INTO books_ratings_link (book, rating) SELECT ?, id FROM ratings WHERE rating = 8`,
		`INSERT OR IGNORE INTO series (name) VALUES ('The Series ` + tag + `')`,
		`INSERT INTO books_series_link (book, series) SELECT ?, id FROM series WHERE name = 'The Series ` + tag + `'`,
		`INSERT INTO annotations (book, format, user_type, user, timestamp, annot_id, annot_type, annot_data, searchable_text)
			VALUES (?, 'EPUB', 'local', 'viewer', 1, 'a` + tag + `', 'highlight', '{}', 'whales are mammals ` + tag + `')`,
		`INSERT INTO last_read_positions (book, format, user, device, cfi, epoch, pos_frac) VALUES (?, 'EPUB', 'u', 'd', '/2', 1, 0.5)`,
		`INSERT INTO conversion_options (book, format, data) VALUES (?, 'EPUB', x'00')`,
		`INSERT INTO book_storage (book, format, user_type, user, timestamp, data) VALUES (?, 'EPUB', 'local', 'viewer', 1, '{"x":1}')`,
		`INSERT INTO books_plugin_data (book, name, val) VALUES (?, 'p', 'v')`,
		`INSERT INTO metadata_dirtied (book) VALUES (?)`,
		`INSERT INTO annotations_dirtied (book) VALUES (?)`,
		`UPDATE books_pages_link SET pages = 3` + tag + `, format = 'EPUB', needs_scan = 0 WHERE book = ?`,
	} {
		args := []any{}
		if strings.Contains(q, "?") {
			args = append(args, b.id)
		}
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// describeCalibreBook is everything metadata.db holds about a book, with
// row ids left out and linked items by name, so it can be compared across a
// removal and restore.
func describeCalibreBook(t *testing.T, db *sql.DB, uuid string) string {
	t.Helper()
	ctx := context.Background()
	var id int64
	var head string
	if err := db.QueryRow(`SELECT id, title || '|' || sort || '|' || path || '|' || timestamp || '|' || pubdate || '|' ||
		series_index || '|' || author_sort || '|' || has_cover || '|' || last_modified FROM books WHERE uuid = ?`, uuid).Scan(&id, &head); err != nil {
		t.Fatalf("book %s: %v", uuid, err)
	}
	lines := []string{"books: " + head}
	tables, err := tablesReferringToBooks(ctx, db, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range tables {
		e, isLink := linkedEntityOf(tb)
		rows, err := db.Query(`SELECT `+columnList(tb.cols)+` FROM `+quoteIdent(tb.name)+` WHERE book = ?`, id)
		if err != nil {
			t.Fatal(err)
		}
		var all [][]any // read first: the library has one connection
		for rows.Next() {
			vals := make([]any, len(tb.cols))
			ptrs := make([]any, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			all = append(all, vals)
		}
		rows.Close()
		for _, vals := range all {
			var parts []string
			for i, c := range tb.cols {
				if c == "id" || c == "book" || (tb.name == "books_pages_link" && c == "timestamp") {
					continue
				}
				v := vals[i]
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				if isLink && c == e.column {
					c, v = e.table, entityKey(t, db, e, v)
				}
				parts = append(parts, fmt.Sprintf("%s=%v", c, v))
			}
			lines = append(lines, tb.name+": "+strings.Join(parts, " "))
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func entityKey(t *testing.T, db *sql.DB, e linkedEntity, id any) any {
	t.Helper()
	var key any
	if err := db.QueryRow(`SELECT `+quoteIdent(e.key)+` FROM `+quoteIdent(e.table)+` WHERE id = ?`, id).Scan(&key); err != nil {
		t.Fatalf("%s %v: %v", e.table, id, err)
	}
	if b, ok := key.([]byte); ok {
		key = string(b)
	}
	return key
}

func checkCalibreIntegrity(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, tbl := range []string{"annotations_fts", "annotations_fts_stemmed"} {
		if _, err := db.Exec(`INSERT INTO ` + tbl + `(` + tbl + `) VALUES('integrity-check')`); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, parent string
		var rowid, fk sql.NullInt64
		_ = rows.Scan(&table, &rowid, &parent, &fk)
		t.Errorf("dangling foreign key: %s row %v -> %s", table, rowid.Int64, parent)
	}
	var ok string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&ok); err != nil || ok != "ok" {
		t.Fatalf("integrity_check: %s %v", ok, err)
	}
}

// Removing books from a real Calibre library and restoring them gives back
// exactly what was there, and leaves Calibre's indexes and references sound.
func TestRecycleRealCalibreSchema(t *testing.T) {
	db := setupRealCalibreLibrary(t)
	a := addRecycleTestBook(t, db, "Moby Dick", []string{"Herman Melville", "Shared Author"}, []string{"sea", "classic"})
	b := addRecycleTestBook(t, db, "Billy Budd", []string{"Herman Melville"}, []string{"sea"})
	keep := addRecycleTestBook(t, db, "Emma", []string{"Shared Author"}, []string{"classic"})
	for i, bk := range []testBook{a, b, keep} {
		fillCalibreBook(t, db, bk, i+1)
	}
	if _, err := db.Exec(`INSERT INTO identifiers (book, type, val) VALUES (?, 'isbn', '9780000000001')`, a.id); err != nil {
		t.Fatal(err)
	}
	generatePages()
	before := map[string]string{a.uuid: describeCalibreBook(t, db, a.uuid), b.uuid: describeCalibreBook(t, db, b.uuid)}
	keepBefore := describeCalibreBook(t, db, keep.uuid)
	checkCalibreIntegrity(t, db)

	batch, err := recycleBooks([]string{a.uuid, b.uuid}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	tables, _ := tablesReferringToBooks(context.Background(), db, "main")
	for _, tb := range tables {
		if n := count(t, db, `SELECT COUNT(*) FROM `+quoteIdent(tb.name)+` WHERE book IN (?, ?)`, a.id, b.id); n != 0 {
			t.Errorf("%s still has %d rows of removed books", tb.name, n)
		}
	}
	if n := count(t, db, `SELECT COUNT(*) FROM authors WHERE name = 'Herman Melville'`); n != 0 {
		t.Error("unused author kept")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM annotations_fts WHERE annotations_fts MATCH 'whales'`); n != 1 {
		t.Errorf("full-text matches after removal: %d, want 1 (Emma's)", n)
	}
	if got := describeCalibreBook(t, db, keep.uuid); got != keepBefore {
		t.Errorf("the remaining book changed:\n%s\nwas\n%s", got, keepBefore)
	}
	checkCalibreIntegrity(t, db)

	// The library moves on: a new book takes the author back, under a new id.
	addRecycleTestBook(t, db, "Typee", []string{"Herman Melville"}, nil)

	res, err := restoreRecycled(batch.ID, nil, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Error != "" {
			t.Fatalf("restore %s: %s", r.Title, r.Error)
		}
	}
	for uuid, want := range before {
		if got := describeCalibreBook(t, db, uuid); got != want {
			t.Errorf("restored %s differs:\n--- got\n%s\n--- want\n%s", uuid, got, want)
		}
	}
	if n := count(t, db, `SELECT COUNT(*) FROM authors WHERE name = 'Herman Melville'`); n != 1 {
		t.Errorf("Herman Melville %d times", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM annotations_fts_stemmed WHERE annotations_fts_stemmed MATCH 'whale'`); n != 3 {
		t.Errorf("stemmed full-text matches after restore: %d, want 3", n)
	}
	checkCalibreIntegrity(t, db)
}
