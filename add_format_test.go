package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// readerRows puts a place, a bookmark and a highlight in format for user 7.
func readerRows(t *testing.T, uuid, format, id string) {
	t.Helper()
	for _, q := range []string{
		`INSERT OR IGNORE INTO users (id, username, created_at) VALUES (7, 'reader', 0)`,
		`INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at) VALUES (7, ?, ?, 'pos-` + id + `', 0.5, 9)`,
		`INSERT INTO bookmarks (user_id, book_uuid, client_id, format, position, created_at) VALUES (7, ?, ?, ?, 'bm', 1)`,
		`INSERT INTO annotations (user_id, book_uuid, client_id, format, anchor, created_at, updated_at) VALUES (7, ?, ?, ?, 'cfi', 1, 1)`,
	} {
		var err error
		switch {
		case strings.Contains(q, "users"):
			_, err = appDB.Exec(q)
		case strings.Contains(q, "reading_progress"):
			_, err = appDB.Exec(q, uuid, format)
		default:
			_, err = appDB.Exec(q, uuid, "c-"+id, format)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecycleFormatAndRestore(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	addFormat(t, a, "Alpha", "pdf", "PDF!")
	readerRows(t, a.uuid, "epub", "e")
	readerRows(t, a.uuid, "pdf", "p")

	batch, err := recycleFormat(a.uuid, "EPUB", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if formats, _ := bookFormats(a.id); !slices.Equal(formats, []string{"pdf"}) {
		t.Errorf("formats after removal %v", formats)
	}
	if _, err := os.Stat(filepath.Join(baseDir, a.path, "Alpha.epub")); !os.IsNotExist(err) {
		t.Error("the EPUB is still in the library")
	}
	if len(batch.Formats) != 1 || batch.Formats[0].Format != "EPUB" || batch.Formats[0].Replaced {
		t.Fatalf("batch %+v", batch.Formats)
	}
	if got := readFile(t, batch.formatPath(&batch.Formats[0])); got != "EPUB!" {
		t.Errorf("binned file %q", got)
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM annotations WHERE book_uuid = ? AND format = 'epub'`, a.uuid); n != 0 {
		t.Error("EPUB highlights stayed")
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM reading_progress WHERE book_uuid = ? AND format = 'epub'`, a.uuid); n != 0 {
		t.Error("EPUB place stayed")
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM annotations WHERE book_uuid = ? AND format = 'pdf'`, a.uuid); n != 1 {
		t.Error("PDF highlights went too")
	}

	// The last file is the book.
	if _, err := recycleFormat(a.uuid, "pdf", "admin"); err != errLastFormat {
		t.Errorf("removing the last file: %v", err)
	}
	if _, err := recycleFormat(a.uuid, "epub", "admin"); err != errNoSuchFormat {
		t.Errorf("removing a format it hasn't: %v", err)
	}

	// Restored: file, row and readers' rows.
	res, err := restoreFromBin(batch.ID, nil, []string{a.uuid + "/EPUB"}, "admin")
	if err != nil || len(res) != 1 || res[0].Error != "" {
		t.Fatalf("restore %+v %v", res, err)
	}
	if formats, _ := bookFormats(a.id); !slices.Equal(formats, []string{"epub", "pdf"}) {
		t.Errorf("formats after restore %v", formats)
	}
	if got := readFile(t, filepath.Join(baseDir, a.path, "Alpha.epub")); got != "EPUB!" {
		t.Errorf("restored file %q", got)
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM annotations WHERE book_uuid = ? AND format = 'epub'`, a.uuid); n != 1 {
		t.Error("EPUB highlight not restored")
	}
	// Twice: it's back already.
	if res, _ := restoreFromBin(batch.ID, nil, []string{a.uuid + "/EPUB"}, "admin"); len(res) != 1 || res[0].Error == "" {
		t.Errorf("second restore %+v", res)
	}
}

func TestMergeReaderDataByFile(t *testing.T) {
	db := setupRecycleLibrary(t)
	// Every test book's EPUB is "EPUB!": A's and C's are the same file.
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Alpha b", []string{"Ann"}, nil, nil)
	c := addOPFTestBook(t, db, "Alpha c", []string{"Ann"}, nil, nil)
	if err := os.WriteFile(filepath.Join(baseDir, b.path, "Alpha b.epub"), []byte("ANOTHER EDITION"), 0o644); err != nil {
		t.Fatal(err)
	}
	readerRows(t, b.uuid, "epub", "b")
	readerRows(t, c.uuid, "epub", "c")

	res, err := mergeBooks(&mergeRequest{
		ApplyMetadataRequest: ApplyMetadataRequest{Title: "Alpha", Creators: "Ann"},
		Keep:                 a.uuid,
		UUIDs:                []string{a.uuid, b.uuid, c.uuid},
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	// C's EPUB is the one kept, byte for byte: everything follows. B's
	// differs: its place does, its bookmark and highlight don't.
	for table, want := range map[string]int{"annotations": 1, "bookmarks": 1} {
		if n := count(t, appDB, `SELECT COUNT(*) FROM `+table+` WHERE book_uuid = ? AND client_id LIKE 'c-c-m%'`, a.uuid); n != want {
			t.Errorf("C's %s: %d", table, n)
		}
		if n := count(t, appDB, `SELECT COUNT(*) FROM `+table+` WHERE book_uuid = ? AND client_id LIKE 'c-b-m%'`, a.uuid); n != 0 {
			t.Errorf("B's %s were carried over to a different file", table)
		}
	}
	if res.Dropped != 2 {
		t.Errorf("dropped %d, want B's bookmark and highlight", res.Dropped)
	}
	var pos string
	if err := appDB.QueryRow(`SELECT position FROM reading_progress WHERE book_uuid = ? AND format = 'epub'`, a.uuid).Scan(&pos); err != nil {
		t.Fatal(err)
	}
	if pos != "pos-b" && pos != "pos-c" {
		t.Errorf("place %q", pos)
	}
	if got := readFile(t, filepath.Join(baseDir, a.path, "Alpha.epub")); got != "EPUB!" {
		t.Errorf("kept EPUB %q", got)
	}
}

func setupAddFormat(t *testing.T) (*http.ServeMux, *User) {
	t.Helper()
	origWrites, origImports := allowWrites, imports
	allowWrites = true
	inbox, err := openInbox(filepath.Join(t.TempDir(), "imports"))
	if err != nil {
		t.Fatal(err)
	}
	imports = inbox
	t.Cleanup(func() { allowWrites, imports = origWrites, origImports })
	admin, err := createUser("admin", "longenough-pw", true)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	withMux(mux, registerAddFormatRoutes)
	return mux, admin
}

func uploadNewFiles(t *testing.T, mux *http.ServeMux, who *User, uuid string, files map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, body := range files {
		fw, _ := mw.CreateFormFile("files", name)
		fw.Write([]byte(body))
	}
	mw.Close()
	req := asUser(httptest.NewRequest("POST", "/book/add-format/"+uuid, &buf), who)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAddFormat(t *testing.T) {
	db := setupRecycleLibrary(t)
	mux, admin := setupAddFormat(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	readerRows(t, a.uuid, "epub", "a")
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, asUser(httptest.NewRequest(method, path, strings.NewReader(body)), admin))
		return rec
	}

	// The same EPUB again: nothing to add.
	if rec := uploadNewFiles(t, mux, admin, a.uuid, map[string]string{"x.epub": "EPUB!"}); rec.Code != http.StatusConflict {
		t.Fatalf("identical file: %d %s", rec.Code, rec.Body)
	}
	if rec := uploadNewFiles(t, mux, admin, a.uuid, map[string]string{"x.txt": "hi"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("not a book: %d", rec.Code)
	}

	// A new PDF, and a different EPUB.
	rec := uploadNewFiles(t, mux, admin, a.uuid, map[string]string{"Alpha.pdf": "NEW PDF", "Alpha 2.epub": "NEW EPUB"})
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	var up struct{ ID, URL string }
	_ = json.Unmarshal(rec.Body.Bytes(), &up)
	if it := imports.get(up.ID); it == nil || it.Into != a.uuid || len(it.Files) != 2 {
		t.Fatalf("waiting item %+v", it)
	}
	if rec := do("GET", up.URL, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Add files, keep metadata") {
		t.Fatalf("review page: %d", rec.Code)
	}
	rec = do("GET", up.URL+"/metadata", "")
	var sources []UploadedBookMeta
	_ = json.Unmarshal(rec.Body.Bytes(), &sources)
	if len(sources) == 0 || sources[0].Format != "current" || sources[0].Title != "Alpha" {
		t.Fatalf("sources %d: %+v", rec.Code, sources)
	}

	// Added: the PDF, and the new EPUB over the old one (the default).
	rec = do("POST", up.URL+"/apply", `{"title":"Alpha","creators":"Ann"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	var out struct{ Batch, Warning string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Warning != "" {
		t.Errorf("warning %q", out.Warning)
	}
	if formats, _ := bookFormats(a.id); !slices.Equal(formats, []string{"epub", "pdf"}) {
		t.Errorf("formats %v", formats)
	}
	dir := filepath.Join(baseDir, a.path)
	if got := readFile(t, filepath.Join(dir, "Alpha.epub")); got != "NEW EPUB" {
		t.Errorf("epub %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "Alpha.pdf")); got != "NEW PDF" {
		t.Errorf("pdf %q", got)
	}
	if imports.get(up.ID) != nil {
		t.Error("still waiting in the queue")
	}
	batch, err := loadRecycleBatch(out.Batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Formats) != 1 || !batch.Formats[0].Replaced || readFile(t, batch.formatPath(&batch.Formats[0])) != "EPUB!" {
		t.Fatalf("replaced %+v", batch.Formats)
	}
	if batch.MergedInto == nil || !slices.Equal(batch.MergedInto.AddedFiles, []string{"EPUB", "PDF"}) {
		t.Errorf("note %+v", batch.MergedInto)
	}
	// Readers: the place in the EPUB stays, the bookmark and highlight
	// went to the bin with the old file.
	if n := count(t, appDB, `SELECT COUNT(*) FROM reading_progress WHERE book_uuid = ? AND format = 'epub'`, a.uuid); n != 1 {
		t.Error("place lost")
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM annotations WHERE book_uuid = ?`, a.uuid); n != 0 {
		t.Error("highlight in the old EPUB stayed")
	}
}

func TestAddFormatKeepsOwnFileAndRefusesAnotherBooksFile(t *testing.T) {
	db := setupRecycleLibrary(t)
	mux, admin := setupAddFormat(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	// Same title, another author: a possible match, so its files are checked.
	other := addOPFTestBook(t, db, "Alpha", []string{"Zed"}, nil, nil)
	addFormat(t, other, "Alpha", "pdf", "ZETA PDF")
	do := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, asUser(httptest.NewRequest("POST", path, strings.NewReader(body)), admin))
		return rec
	}

	// The book's own EPUB is kept over the new one: nothing is replaced.
	rec := uploadNewFiles(t, mux, admin, a.uuid, map[string]string{"a.epub": "OTHER EPUB"})
	var up struct{ URL string }
	_ = json.Unmarshal(rec.Body.Bytes(), &up)
	rec = do(up.URL+"/apply", `{"title":"Alpha","creators":"Ann","formats":{"epub":"`+a.uuid+`"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	if got := readFile(t, filepath.Join(baseDir, a.path, "Alpha.epub")); got != "EPUB!" {
		t.Errorf("own EPUB replaced: %q", got)
	}

	// The other Alpha's PDF is that book's file: merge it instead.
	rec = uploadNewFiles(t, mux, admin, a.uuid, map[string]string{"Alpha.pdf": "ZETA PDF"})
	_ = json.Unmarshal(rec.Body.Bytes(), &up)
	rec = do(up.URL+"/apply", `{"title":"Alpha","creators":"Ann"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), other.uuid) {
		t.Fatalf("another book's file: %d %s", rec.Code, rec.Body)
	}
	if formats, _ := bookFormats(a.id); !slices.Equal(formats, []string{"epub"}) {
		t.Errorf("formats %v", formats)
	}
}

func TestDuplicatesByISBN(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Something else", []string{"Bob"}, nil, nil)
	if _, err := db.Exec(`INSERT INTO identifiers (book, type, val) VALUES (?, 'isbn', '0-306-40615-2')`, b.id); err != nil {
		t.Fatal(err)
	}
	check, err := findDuplicatesISBN(db, baseDir, "Unrelated", nil, nil, a.uuid, []string{"9780306406157"})
	if err != nil {
		t.Fatal(err)
	}
	if len(check.Books) != 1 || check.Books[0].UUID != b.uuid || !check.Books[0].SameISBN || check.Books[0].SameAuthors {
		t.Errorf("by ISBN: %+v", check.Books)
	}
}
