package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// setupRecycleLibrary: a test library with Calibre's delete trigger, a
// custom column and its insert check, and books whose folders have files.
func setupRecycleLibrary(t *testing.T) *sql.DB {
	t.Helper()
	db, dir := setupTestDB(t)
	t.Cleanup(func() { db.Close(); os.RemoveAll(dir) })
	origDB, origBase, origData, origRecycle := calibreDB, baseDir, dataDir, recycleDir
	calibreDB, baseDir, dataDir, recycleDir = db, dir, t.TempDir(), ""
	t.Cleanup(func() { calibreDB, baseDir, dataDir, recycleDir = origDB, origBase, origData, origRecycle })
	t.Chdir(t.TempDir()) // pages/*.json
	setupTestAppDB(t)
	if _, err := db.Exec(`
	CREATE TABLE custom_column_1 (id INTEGER PRIMARY KEY, value TEXT NOT NULL, UNIQUE(value));
	CREATE TABLE books_custom_column_1_link (id INTEGER PRIMARY KEY, book INTEGER NOT NULL, value INTEGER NOT NULL, UNIQUE(book, value));
	CREATE TABLE annotations_fts_data (id INTEGER PRIMARY KEY, block BLOB);
	CREATE TRIGGER books_delete_trg AFTER DELETE ON books BEGIN
		DELETE FROM books_authors_link WHERE book=OLD.id;
		DELETE FROM data WHERE book=OLD.id;
		DELETE FROM comments WHERE book=OLD.id;
	END;
	CREATE TRIGGER fkc_insert_books_authors_link BEFORE INSERT ON books_authors_link BEGIN
		SELECT CASE
			WHEN (SELECT id from books WHERE id=NEW.book) IS NULL THEN RAISE(ABORT, 'Foreign key violation: book not in books')
			WHEN (SELECT id from authors WHERE id=NEW.author) IS NULL THEN RAISE(ABORT, 'Foreign key violation: author not in authors')
		END;
	END;`); err != nil {
		t.Fatal(err)
	}
	return db
}

type testBook struct {
	id         int64
	uuid, path string
}

func addRecycleTestBook(t *testing.T, db *sql.DB, title string, authors []string, tags []string) testBook {
	t.Helper()
	meta := &UploadedBookMeta{OPFMetadata: OPFMetadata{Title: title, Creators: authors, Subjects: tags, Publisher: "Pub " + title}}
	id, u, p, err := insertBookIntoCalibreDB(db, meta, []formatEntry{{Ext: "epub", Filename: title + ".epub", Size: 5}})
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(baseDir, p)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{title + ".epub": "EPUB!", "metadata.opf": "<package/>", "cover.jpg": "jpg"} {
		if err := os.WriteFile(filepath.Join(full, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO comments (book, text) VALUES (?, ?)`, id, "About "+title); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO custom_column_1 (value) VALUES ('shelf A')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO books_custom_column_1_link (book, value) SELECT ?, id FROM custom_column_1 WHERE value = 'shelf A'`, id); err != nil {
		t.Fatal(err)
	}
	return testBook{id, u, p}
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestRecycleAndRestore(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addRecycleTestBook(t, db, "Alpha", []string{"Only Alpha Author", "Shared Author"}, []string{"alpha-tag", "common"})
	b := addRecycleTestBook(t, db, "Beta", []string{"Shared Author"}, []string{"common"})
	c := addRecycleTestBook(t, db, "Gamma", []string{"Gamma Author"}, nil)

	batch, err := recycleBooks([]string{a.uuid, b.uuid, a.uuid}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Books) != 2 {
		t.Fatalf("batch has %d books, want 2 (duplicates dropped)", len(batch.Books))
	}

	// Gone from the library: rows, links, folders, and items no book has.
	if n := count(t, db, `SELECT COUNT(*) FROM books WHERE uuid IN (?, ?)`, a.uuid, b.uuid); n != 0 {
		t.Fatalf("%d removed books still in books", n)
	}
	for _, table := range []string{"books_authors_link", "books_tags_link", "books_publishers_link", "comments", "data", "books_custom_column_1_link"} {
		if n := count(t, db, `SELECT COUNT(*) FROM `+table+` WHERE book IN (?, ?)`, a.id, b.id); n != 0 {
			t.Errorf("%s still has %d rows of removed books", table, n)
		}
	}
	if n := count(t, db, `SELECT COUNT(*) FROM authors WHERE name IN ('Only Alpha Author', 'Shared Author')`); n != 0 {
		t.Errorf("unused authors kept: %d", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM tags WHERE name IN ('alpha-tag', 'common')`); n != 0 {
		t.Errorf("unused tags kept: %d", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books WHERE uuid = ?`, c.uuid); n != 1 {
		t.Fatal("the other book went too")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM authors WHERE name = 'Gamma Author'`); n != 1 {
		t.Fatal("an author still in use was removed")
	}
	if exists(filepath.Join(baseDir, a.path)) || exists(filepath.Join(baseDir, b.path)) {
		t.Fatal("folders still in the library")
	}
	if exists(filepath.Join(baseDir, filepath.Dir(a.path))) {
		t.Error("empty author folder kept")
	}
	if !exists(filepath.Join(baseDir, c.path)) {
		t.Fatal("the other book's folder went too")
	}

	// In the bin: the database as it was, and the folders as they were.
	bin := filepath.Join(dataDir, "recycle-bin", batch.ID)
	snap, err := sql.Open("sqlite3", filepath.Join(bin, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if n := count(t, snap, `SELECT COUNT(*) FROM books`); n != 3 {
		t.Fatalf("backup has %d books, want 3", n)
	}
	if got, _ := os.ReadFile(filepath.Join(bin, "books", itoa(a.id), "Alpha.epub")); string(got) != "EPUB!" {
		t.Fatalf("book file in bin: %q", got)
	}
	if listed, _ := listRecycleBatches(); len(listed) != 1 || listed[0].ID != batch.ID {
		t.Fatalf("listing: %+v", listed)
	}

	// Meanwhile a new book takes an author and the library moves on.
	addRecycleTestBook(t, db, "Delta", []string{"Shared Author"}, nil)

	res, err := restoreRecycled(batch.ID, []string{a.uuid}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Error != "" {
		t.Fatalf("restore: %+v", res)
	}
	var id int64
	var title, path, authors string
	if err := db.QueryRow(`SELECT b.id, b.title, b.path, `+bookAuthorsSQL+` FROM books b WHERE uuid = ?`, a.uuid).Scan(&id, &title, &path, &authors); err != nil {
		t.Fatal("restored book not found: ", err)
	}
	if id != a.id || title != "Alpha" || path != a.path || authors != "Only Alpha Author & Shared Author" {
		t.Fatalf("restored: id %d title %q path %q authors %q", id, title, path, authors)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM authors WHERE name = 'Shared Author'`); n != 1 {
		t.Fatalf("Shared Author %d times", n)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM books_tags_link WHERE book = ?`:            2,
		`SELECT COUNT(*) FROM books_publishers_link WHERE book = ?`:      1,
		`SELECT COUNT(*) FROM comments WHERE book = ?`:                   1,
		`SELECT COUNT(*) FROM data WHERE book = ?`:                       1,
		`SELECT COUNT(*) FROM books_custom_column_1_link WHERE book = ?`: 1,
	} {
		if n := count(t, db, q, a.id); n != want {
			t.Errorf("%s: %d, want %d", q, n, want)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(baseDir, a.path, "Alpha.epub")); string(got) != "EPUB!" {
		t.Fatalf("restored file: %q", got)
	}
	if exists(filepath.Join(bin, "books", itoa(a.id))) {
		t.Fatal("restored book's files still in the bin")
	}

	// Again: refused. The rest: restored.
	if res, _ := restoreRecycled(batch.ID, []string{a.uuid}, "admin"); len(res) != 1 || res[0].Error == "" {
		t.Fatalf("second restore: %+v", res)
	}
	if res, err := restoreRecycled(batch.ID, nil, "admin"); err != nil || len(res) != 1 || res[0].UUID != b.uuid || res[0].Error != "" {
		t.Fatalf("restore the rest: %+v %v", res, err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books`); n != 4 {
		t.Fatalf("%d books after restoring, want 4", n)
	}
	saved, _ := loadRecycleBatch(batch.ID)
	for _, bk := range saved.Books {
		if bk.RestoredAt == "" || bk.RestoredBy != "admin" {
			t.Errorf("not marked restored: %+v", bk)
		}
	}
	// The backup itself stays until it's emptied on the server.
	if !exists(filepath.Join(bin, "metadata.db")) {
		t.Fatal("backup gone after restoring")
	}
}

func TestRecycleRefusals(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addRecycleTestBook(t, db, "Alpha", []string{"A"}, nil)

	// An unknown book: nothing happens, no batch.
	if _, err := recycleBooks([]string{a.uuid, "00000000-0000-0000-0000-000000000000"}, "admin"); err == nil {
		t.Fatal("unknown book accepted")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books`); n != 1 {
		t.Fatal("a book was removed")
	}
	if listed, _ := listRecycleBatches(); len(listed) != 0 {
		t.Fatalf("batch left behind: %+v", listed)
	}

	// A path that is the whole library is never moved.
	if _, err := db.Exec(`UPDATE books SET path = '' WHERE id = ?`, a.id); err != nil {
		t.Fatal(err)
	}
	uuidPathIndex.Delete(a.uuid)
	if _, err := recycleBooks([]string{a.uuid}, "admin"); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("empty path: %v", err)
	}
	if !exists(filepath.Join(baseDir, "metadata.db")) {
		t.Fatal("library moved")
	}
	for _, p := range []string{"", ".", "..", "../x", "/etc", "a/../.."} {
		if _, err := libraryBookDir(p); err == nil {
			t.Errorf("libraryBookDir(%q) accepted", p)
		}
	}

	// Restoring onto a folder that's back in the library is refused, and
	// leaves the bin alone.
	if _, err := db.Exec(`UPDATE books SET path = ? WHERE id = ?`, "A/Alpha (1)", a.id); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(baseDir, "A/Alpha (1)"), 0o755)
	batch, err := recycleBooks([]string{a.uuid}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(baseDir, "A/Alpha (1)"), 0o755)
	if res, _ := restoreRecycled(batch.ID, nil, "admin"); len(res) != 1 || res[0].Error == "" {
		t.Fatalf("restore onto an existing folder: %+v", res)
	}
	if !exists(batch.bookDir(a.id)) {
		t.Fatal("files left the bin")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books`); n != 0 {
		t.Fatal("rows restored")
	}
	if _, err := restoreRecycled("../../etc", nil, "admin"); err == nil {
		t.Fatal("bad batch id accepted")
	}
}

func TestRecycleHTTP(t *testing.T) {
	db := setupRecycleLibrary(t)
	setupTestAppDB(t)
	origWrites := allowWrites
	allowWrites = true
	t.Cleanup(func() { allowWrites = origWrites })
	a := addRecycleTestBook(t, db, "Alpha", []string{"A"}, nil)
	admin, _ := createUser("admin", "longenough-pw", true)
	reader, _ := createUser("reader", "longenough-pw", false)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/recycle-bin", requireWrites(recycleBinHandler))
	mux.HandleFunc("POST /api/recycle-bin/{batch}/restore", requireWrites(recycleRestoreHandler))
	mux.HandleFunc("POST /api/library/delete", requireWrites(recycleDeleteHandler))
	do := func(method, path, body string, who *User) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if who != nil {
			req = asUser(req, who)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	body := `{"uuids":["` + a.uuid + `"]}`
	if rec := do("POST", "/api/library/delete", body, reader); rec.Code != http.StatusForbidden {
		t.Fatalf("reader delete: %d", rec.Code)
	}
	if rec := do("POST", "/api/library/delete", body, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous delete: %d", rec.Code)
	}
	if rec := do("POST", "/api/library/delete", `{"uuids":["../x"]}`, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad uuid: %d", rec.Code)
	}
	rec := do("POST", "/api/library/delete", body, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	var batch recycleBatch
	_ = json.Unmarshal(rec.Body.Bytes(), &batch)
	if batch.DeletedBy != "admin" || len(batch.Books) != 1 {
		t.Fatalf("batch: %+v", batch)
	}

	rec = do("GET", "/api/recycle-bin", "", admin)
	var list struct {
		Batches []recycleBatchView `json:"batches"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Batches) != 1 || !list.Batches[0].Books[0].InBin {
		t.Fatalf("list: %s", rec.Body)
	}
	if rec := do("POST", "/api/recycle-bin/"+batch.ID+"/restore", `{}`, reader); rec.Code != http.StatusForbidden {
		t.Fatalf("reader restore: %d", rec.Code)
	}
	if rec := do("POST", "/api/recycle-bin/20990101-000000-00000000/restore", `{}`, admin); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown batch: %d", rec.Code)
	}
	rec = do("POST", "/api/recycle-bin/"+batch.ID+"/restore", `{}`, admin)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), a.uuid) || strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books WHERE uuid = ?`, a.uuid); n != 1 {
		t.Fatal("not restored")
	}
	// Nothing on the web empties the bin.
	for _, m := range []string{"DELETE", "POST"} {
		if rec := do(m, "/api/recycle-bin/"+batch.ID, "", admin); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s batch: %d", m, rec.Code)
		}
	}
}

func TestEmptyRecycleBin(t *testing.T) {
	setupRecycleLibrary(t)
	for i, at := range []time.Time{time.Now().Add(-40 * 24 * time.Hour), time.Now()} {
		b := &recycleBatch{ID: newRecycleBatchID(at), DeletedAt: at.UTC().Format(time.RFC3339), DeletedBy: "x"}
		b.ID = b.ID[:len(b.ID)-1] + itoa(int64(i))
		if err := os.MkdirAll(b.dir(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := b.save(); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.MkdirAll(filepath.Join(recycleBinDir(), "not-a-batch"), 0o700)
	var out bytes.Buffer
	if err := emptyRecycleBin(30*24*time.Hour, &out); err != nil {
		t.Fatal(err)
	}
	if left, _ := listRecycleBatches(); len(left) != 1 {
		t.Fatalf("%d batches left, want 1\n%s", len(left), out.String())
	}
	if !exists(filepath.Join(recycleBinDir(), "not-a-batch")) {
		t.Fatal("removed a folder that isn't a batch")
	}
	if err := emptyRecycleBin(0, &out); err != nil {
		t.Fatal(err)
	}
	if left, _ := listRecycleBatches(); len(left) != 0 {
		t.Fatal("0 should empty it")
	}
	for in, want := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "12h": 12 * time.Hour, "0": 0} {
		if got, err := parseAge(in); err != nil || got != want {
			t.Errorf("parseAge(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "-1d", "x", "-3h"} {
		if _, err := parseAge(bad); err == nil {
			t.Errorf("parseAge(%q) accepted", bad)
		}
	}
}

func TestMoveDirAcrossCopy(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	_ = os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "sub", "f.txt"), []byte("hi"), 0o644)
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	_ = os.Chtimes(filepath.Join(src, "sub", "f.txt"), old, old)
	dst := filepath.Join(t.TempDir(), "dst")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dst, "sub", "f.txt"))
	if err != nil || !st.ModTime().Equal(old) {
		t.Fatalf("copy: %v %v", st, err)
	}
	if err := moveDir(src, dst); err == nil {
		t.Fatal("moved onto an existing folder")
	}
}

// Removing and restoring edit only the pages holding the books.
func TestRecyclePagesEditedInPlace(t *testing.T) {
	db := setupRecycleLibrary(t)
	for i := 0; i < booksPerPage+5; i++ {
		if _, _, _, err := insertBookIntoCalibreDB(db, &UploadedBookMeta{OPFMetadata: OPFMetadata{Title: "Book " + itoa(int64(i)), Creators: []string{"A"}}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	generatePages()
	total := totalBooks
	page1, _ := readPage(1)
	page2Before, _ := os.ReadFile(pageFile(2))
	victim := page1[7]

	batch, err := recycleBooks([]string{victim.UUID}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := readPage(1)
	if len(after) != booksPerPage-1 || slices.ContainsFunc(after, func(b Book) bool { return b.UUID == victim.UUID }) {
		t.Fatalf("page 1 after removal: %d books", len(after))
	}
	if page2, _ := os.ReadFile(pageFile(2)); !bytes.Equal(page2, page2Before) {
		t.Fatal("page 2 was rewritten")
	}
	if totalBooks != total {
		t.Fatalf("totalBooks %d, want %d (the gap keeps the other pages in place)", totalBooks, total)
	}

	// Restored in the same run: back in its place.
	if _, err := restoreRecycled(batch.ID, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if back, _ := readPage(1); len(back) != booksPerPage || back[7].UUID != victim.UUID {
		t.Fatalf("page 1 after restoring: %d books, [7] = %s", len(back), back[7].UUID)
	}

	// Removed, then the server restarted (pages written again), then
	// restored: at the end of the last page.
	if batch, err = recycleBooks([]string{victim.UUID}, "admin"); err != nil {
		t.Fatal(err)
	}
	generatePages()
	total = totalBooks
	if _, err := restoreRecycled(batch.ID, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	last, _ := readPage(2)
	if last[len(last)-1].UUID != victim.UUID || totalBooks != total+1 {
		t.Fatalf("after a restart: last page ends with %s, totalBooks %d (was %d)", last[len(last)-1].UUID, totalBooks, total)
	}
}

// Readers' bookmarks, notes and progress are kept with the batch, and a
// restore adds back what's gone without overwriting what changed.
func TestRecycleKeepsReaderData(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addRecycleTestBook(t, db, "Alpha", []string{"A"}, nil)
	other := addRecycleTestBook(t, db, "Other", []string{"B"}, nil)
	u, _ := createUser("reader", "longenough-pw", false)
	gone, _ := createUser("leaver", "longenough-pw", false)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := appDB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, who := range []int64{u.ID, gone.ID} {
		exec(`INSERT INTO bookmarks (user_id, book_uuid, client_id, format, position, label, created_at) VALUES (?, ?, ?, 'epub', 'epubcfi(/6/4)', 'Ch. 2', 1)`, who, a.uuid, "bm-"+itoa(who))
		exec(`INSERT INTO annotations (user_id, book_uuid, client_id, format, anchor, text, note, created_at, updated_at) VALUES (?, ?, ?, 'epub', 'epubcfi(/6/4,/1:0,/1:5)', 'Hello', 'a note', 1, 1)`, who, a.uuid, "an-"+itoa(who))
	}
	exec(`INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at) VALUES (?, ?, 'epub', 'old', 0.25, 1)`, u.ID, a.uuid)
	exec(`INSERT INTO favorites (user_id, book_uuid, created_at) VALUES (?, ?, 1)`, u.ID, a.uuid)
	exec(`INSERT INTO shelves (id, owner_id, name, created_at, updated_at) VALUES (7, ?, 'Mine', 1, 1)`, u.ID)
	exec(`INSERT INTO shelf_books (shelf_id, book_uuid, added_at) VALUES (7, ?, 1)`, a.uuid)
	exec(`INSERT INTO bookmarks (user_id, book_uuid, client_id, format, position, created_at) VALUES (?, ?, 'other-bm', 'epub', 'x', 1)`, u.ID, other.uuid)

	batch, err := recycleBooks([]string{a.uuid}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if got := batch.Books[0].ReaderData; got["bookmarks"] != 2 || got["annotations"] != 2 || got["reading_progress"] != 1 || got["shelf_books"] != 1 {
		t.Fatalf("counts: %v", got)
	}
	raw, _ := os.ReadFile(filepath.Join(batch.dir(), "reader-data.json"))
	if !strings.Contains(string(raw), "a note") || strings.Contains(string(raw), "other-bm") {
		t.Fatalf("reader-data.json:\n%s", raw)
	}
	// Removing the book doesn't touch them.
	if n := count(t, appDB, `SELECT COUNT(*) FROM annotations WHERE book_uuid = ?`, a.uuid); n != 2 {
		t.Fatalf("annotations after removal: %d", n)
	}

	// Meanwhile: some are lost, one changes, a user goes.
	exec(`DELETE FROM bookmarks WHERE user_id = ? AND book_uuid = ?`, u.ID, a.uuid)
	exec(`DELETE FROM annotations WHERE user_id = ? AND book_uuid = ?`, u.ID, a.uuid)
	exec(`UPDATE reading_progress SET position = 'newer', updated_at = 2 WHERE user_id = ?`, u.ID)
	exec(`DELETE FROM users WHERE id = ?`, gone.ID)

	if res, err := restoreRecycled(batch.ID, nil, "admin"); err != nil || len(res) != 1 || res[0].Error != "" {
		t.Fatalf("restore: %+v %v", res, err)
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM bookmarks WHERE user_id = ? AND book_uuid = ? AND label = 'Ch. 2'`, u.ID, a.uuid); n != 1 {
		t.Errorf("bookmark not back: %d", n)
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM annotations WHERE user_id = ? AND book_uuid = ? AND note = 'a note'`, u.ID, a.uuid); n != 1 {
		t.Errorf("annotation not back: %d", n)
	}
	var pos string
	_ = appDB.QueryRow(`SELECT position FROM reading_progress WHERE user_id = ?`, u.ID).Scan(&pos)
	if pos != "newer" {
		t.Errorf("progress overwritten with the old copy: %q", pos)
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM bookmarks WHERE user_id = ?`, gone.ID); n != 0 {
		t.Errorf("a deleted user's rows came back: %d", n)
	}
}
