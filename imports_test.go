package main

import (
	"database/sql"
	"encoding/json"
	"image"
	"image/jpeg"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// importTestEPUB is an EPUB with a title, an author, an ISBN and its own uuid.
func importTestEPUB(t *testing.T) []byte {
	t.Helper()
	return epubWithIdentifiers(t, `<dc:identifier id="uid">`+testEPUBUUID+`</dc:identifier>
		<dc:identifier opf:scheme="ISBN">9780134190440</dc:identifier>`)
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTestWatcher(t *testing.T) (*folderWatcher, string) {
	t.Helper()
	inbox, err := openInbox(filepath.Join(t.TempDir(), "imports"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return newFolderWatcher(dir, inbox), dir
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestWatchFolderTakesFinishedBooks(t *testing.T) {
	w, dir := newTestWatcher(t)
	epub := importTestEPUB(t)
	writeTestFile(t, filepath.Join(dir, "Go Book.epub"), epub)
	writeTestFile(t, filepath.Join(dir, "Go Book.PDF"), []byte("%PDF-1.4 not really"))
	writeTestFile(t, filepath.Join(dir, "sub", "Other.mobi"), []byte("mobi"))
	writeTestFile(t, filepath.Join(dir, "notes.txt"), []byte("not a book"))
	writeTestFile(t, filepath.Join(dir, ".hidden.epub"), epub)
	writeTestFile(t, filepath.Join(dir, ".stfolder", "x.epub"), epub)
	writeTestFile(t, filepath.Join(dir, "empty.epub"), nil)

	// First sight of a file isn't enough: it may still be arriving.
	if n := w.scan(); n != 0 || w.inbox.count() != 0 {
		t.Fatalf("first scan took %d book(s)", n)
	}
	if waiting, _, _ := w.status(); waiting != 4 {
		t.Errorf("waiting = %d, want 4 book files", waiting)
	}
	if n := w.scan(); n != 2 {
		t.Fatalf("second scan took %d books, want 2", n)
	}

	items := w.inbox.list()
	byTitle := map[string]*pendingImport{}
	for _, it := range items {
		byTitle[it.Merged.Title] = it
	}
	book := byTitle["Uploaded"]
	if book == nil {
		t.Fatalf("EPUB's title not read; have %v", byTitle)
	}
	var formats []string
	for _, f := range book.Files {
		formats = append(formats, f.Format)
		if !exists(filepath.Join(w.inbox.itemDir(book.ID), f.Name)) {
			t.Errorf("%s isn't in the queue's folder", f.Name)
		}
	}
	slices.Sort(formats)
	if !slices.Equal(formats, []string{"epub", "pdf"}) {
		t.Errorf("formats = %v: same-named files should be one book", formats)
	}
	if other := byTitle["Other"]; other == nil || other.From != "sub" {
		t.Errorf("book in a subfolder: %+v", other)
	}

	// Taken files leave the folder; everything else stays.
	for name, want := range map[string]bool{
		"Go Book.epub": false, "Go Book.PDF": false, "sub/Other.mobi": false,
		"notes.txt": true, ".hidden.epub": true, ".stfolder/x.epub": true, "empty.epub": true,
	} {
		if got := exists(filepath.Join(dir, filepath.FromSlash(name))); got != want {
			t.Errorf("%s still in the watch folder = %v, want %v", name, got, want)
		}
	}
	if n := w.scan(); n != 0 {
		t.Errorf("a third scan took %d more", n)
	}
}

func TestWatchFolderWaitsForACopyToFinish(t *testing.T) {
	w, dir := newTestWatcher(t)
	p := filepath.Join(dir, "Big.epub")
	writeTestFile(t, p, []byte("half"))
	w.scan()
	writeTestFile(t, p, []byte("half and the rest"))
	if n := w.scan(); n != 0 {
		t.Fatal("a file that changed between scans was taken")
	}
	// Its other format arrives late: the book waits for both.
	writeTestFile(t, filepath.Join(dir, "Big.pdf"), []byte("%PDF"))
	if n := w.scan(); n != 0 {
		t.Fatal("taken before its second format settled")
	}
	if n := w.scan(); n != 1 || len(w.inbox.list()[0].Files) != 2 {
		t.Fatalf("took %d, want one book with both files", n)
	}
}

func TestInboxSurvivesRestart(t *testing.T) {
	w, dir := newTestWatcher(t)
	writeTestFile(t, filepath.Join(dir, "Kept.epub"), importTestEPUB(t))
	w.scan()
	w.scan()
	// A folder without item.json (a crash mid-way) is skipped, not deleted.
	stray := filepath.Join(w.inbox.dir, "0000-stray")
	writeTestFile(t, filepath.Join(stray, "Lost.epub"), []byte("x"))

	again, err := openInbox(w.inbox.dir)
	if err != nil {
		t.Fatal(err)
	}
	if items := again.list(); len(items) != 1 || items[0].Merged.Title != "Uploaded" || len(items[0].Sources) != 1 {
		t.Fatalf("reloaded queue: %+v", items)
	}
	if !exists(filepath.Join(stray, "Lost.epub")) {
		t.Error("a folder the queue doesn't know was deleted")
	}
}

func TestImportSourcesHideLibraryIdentifiers(t *testing.T) {
	w, dir := newTestWatcher(t)
	writeTestFile(t, filepath.Join(dir, "Book.epub"), importTestEPUB(t))
	w.scan()
	w.scan()
	srcs := importSources(w.inbox.list()[0])
	if len(srcs) != 2 || srcs[0].Format != "current" || srcs[1].Format != "epub" {
		t.Fatalf("sources: %+v", srcs)
	}
	for _, s := range srcs {
		if len(s.Identifiers) != 1 || !strings.EqualFold(s.Identifiers[0].Scheme, "isbn") {
			t.Errorf("%s: identifiers %+v, want only the ISBN", s.Format, s.Identifiers)
		}
	}
	if srcs[0].Date != "" {
		t.Errorf("a book without a date shows %q", srcs[0].Date)
	}
}

// withTestLibrary points the library globals at a fresh test database.
func withTestLibrary(t *testing.T) *sql.DB {
	t.Helper()
	db, tempDir := setupTestDB(t)
	origDB, origBase := calibreDB, baseDir
	calibreDB, baseDir = db, tempDir
	t.Cleanup(func() {
		calibreDB, baseDir = origDB, origBase
		db.Close()
		os.RemoveAll(tempDir)
	})
	return db
}

func TestApproveAddsTheBookToTheLibrary(t *testing.T) {
	db := withTestLibrary(t)
	w, dir := newTestWatcher(t)
	writeTestFile(t, filepath.Join(dir, "Go Book.epub"), importTestEPUB(t))
	writeTestFile(t, filepath.Join(dir, "Go Book.pdf"), []byte("%PDF-1.4 not really"))
	w.scan()
	w.scan()
	it := w.inbox.list()[0]
	itemDir := w.inbox.itemDir(it.ID)

	// The cover the admin chose.
	cover, err := os.Create(filepath.Join(itemDir, "cover.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	jpeg.Encode(cover, image.NewRGBA(image.Rect(0, 0, 4, 6)), nil)
	cover.Close()

	var books int
	db.QueryRow(`SELECT COUNT(*) FROM books`).Scan(&books)
	if books != 0 {
		t.Fatalf("%d book(s) in the library before approval", books)
	}

	meta := metadataFromRequest(ApplyMetadataRequest{
		Title:       "The Go Programming Language",
		Creators:    "Alan Donovan & Brian Kernighan",
		Description: "<p>About Go.</p>",
		Publisher:   "Addison-Wesley",
		Date:        "2015-10-26",
		Subjects:    "Programming, Go",
		Identifiers: identifierList{{Scheme: "isbn", Value: "9780134190440"}},
	})
	bookUUID, err := w.inbox.approve(it.ID, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer uuidPathIndex.Delete(bookUUID)
	defer bookMetadataCache.Delete(bookUUID)

	var id int64
	var title, path, authorSort string
	var hasCover bool
	if err := db.QueryRow(`SELECT id, title, path, author_sort, has_cover FROM books WHERE uuid = ?`, bookUUID).
		Scan(&id, &title, &path, &authorSort, &hasCover); err != nil {
		t.Fatal(err)
	}
	if title != "The Go Programming Language" || !hasCover || !strings.Contains(authorSort, "Kernighan") {
		t.Errorf("book row: title %q, has_cover %v, author_sort %q", title, hasCover, authorSort)
	}
	var comment, isbn string
	db.QueryRow(`SELECT text FROM comments WHERE book = ?`, id).Scan(&comment)
	db.QueryRow(`SELECT val FROM identifiers WHERE book = ? AND type = 'isbn'`, id).Scan(&isbn)
	if comment != "<p>About Go.</p>" || isbn != "9780134190440" {
		t.Errorf("description %q, isbn %q", comment, isbn)
	}
	var formats int
	db.QueryRow(`SELECT COUNT(*) FROM data WHERE book = ?`, id).Scan(&formats)
	if formats != 2 {
		t.Errorf("%d formats in metadata.db, want 2", formats)
	}

	bookDir := filepath.Join(baseDir, path)
	for _, name := range []string{"The Go Programming Language.epub", "The Go Programming Language.pdf", "cover.jpg", "metadata.opf"} {
		if !exists(filepath.Join(bookDir, name)) {
			t.Errorf("%s isn't in the book's folder", name)
		}
	}
	opf, err := loadOPF(filepath.Join(bookDir, "metadata.opf"))
	if err != nil {
		t.Fatal(err)
	}
	// The EPUB's own uuid stays, though the editor never showed it.
	if !slices.ContainsFunc(opf.Metadata.Identifiers, func(i opfIdentifier) bool { return i.Value == testEPUBUUID }) {
		t.Errorf("the EPUB's uuid was lost: %+v", opf.Metadata.Identifiers)
	}
	if opf.Metadata.Publisher != "Addison-Wesley" || len(opf.Metadata.Creators) != 2 {
		t.Errorf("metadata.opf: %+v", opf.Metadata)
	}

	if w.inbox.count() != 0 || exists(itemDir) {
		t.Error("the book is still in the queue")
	}
	if _, err := w.inbox.approve(it.ID, meta); err != fs.ErrNotExist {
		t.Errorf("approving twice: %v", err)
	}
}

func TestApproveFailureKeepsTheBookWaiting(t *testing.T) {
	db := withTestLibrary(t)
	w, dir := newTestWatcher(t)
	writeTestFile(t, filepath.Join(dir, "Book.epub"), importTestEPUB(t))
	w.scan()
	w.scan()
	it := w.inbox.list()[0]
	file := filepath.Join(w.inbox.itemDir(it.ID), "Book.epub")

	// The author's folder can't be made: a file is in its place.
	writeTestFile(t, filepath.Join(baseDir, "Some Author"), []byte("in the way"))
	meta := metadataFromRequest(ApplyMetadataRequest{Title: "Book", Creators: "Some Author"})
	if _, err := w.inbox.approve(it.ID, meta); err == nil {
		t.Fatal("expected an error")
	}
	if w.inbox.get(it.ID) == nil || !exists(file) {
		t.Error("a book that couldn't be added must stay in the queue with its file")
	}
	var books int
	db.QueryRow(`SELECT COUNT(*) FROM books`).Scan(&books)
	if books != 0 {
		t.Errorf("%d book row(s) left in metadata.db", books)
	}
}

func TestDiscardDeletesTheFiles(t *testing.T) {
	w, dir := newTestWatcher(t)
	writeTestFile(t, filepath.Join(dir, "Junk.epub"), []byte("junk"))
	w.scan()
	w.scan()
	it := w.inbox.list()[0]
	if it.Merged.Title != "Junk" {
		t.Errorf("an unreadable EPUB is titled %q, want its file name", it.Merged.Title)
	}
	if err := w.inbox.discard(it.ID); err != nil {
		t.Fatal(err)
	}
	if w.inbox.count() != 0 || exists(w.inbox.itemDir(it.ID)) {
		t.Error("still there after discard")
	}
}

func TestCheckWatchDir(t *testing.T) {
	origBase, origData := baseDir, dataDir
	defer func() { baseDir, dataDir = origBase, origData }()
	root := t.TempDir()
	baseDir, dataDir = filepath.Join(root, "library"), filepath.Join(root, "data")
	for _, d := range []string{baseDir, dataDir, filepath.Join(baseDir, "inbox"), filepath.Join(root, "watch"), filepath.Join(root, "library2")} {
		os.MkdirAll(d, 0o755)
	}
	for dir, ok := range map[string]bool{
		filepath.Join(root, "watch"):    true,
		filepath.Join(root, "library2"): true, // a name that only starts like the library's
		baseDir:                         false,
		filepath.Join(baseDir, "inbox"): false,
		root:                            false, // holds the library
		dataDir:                         false,
		filepath.Join(root, "missing"):  false,
	} {
		if err := checkWatchDir(dir); (err == nil) != ok {
			t.Errorf("checkWatchDir(%s) = %v, want ok=%v", dir, err, ok)
		}
	}
}

func TestImportHandlers(t *testing.T) {
	setupTestAppDB(t)
	w, dir := newTestWatcher(t)
	origImports, origWrites := imports, allowWrites
	imports, allowWrites = w.inbox, true
	defer func() { imports, allowWrites = origImports, origWrites }()
	writeTestFile(t, filepath.Join(dir, "Book.epub"), importTestEPUB(t))
	w.scan()
	w.scan()
	it := w.inbox.list()[0]
	admin, _ := createUser("importadmin", "correct horse battery", true)
	reader, _ := createUser("importreader", "correct horse battery", false)

	call := func(h http.HandlerFunc, u *User, method, id string, kv ...string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/import/"+id, nil)
		req.SetPathValue("id", id)
		for i := 0; i+1 < len(kv); i += 2 {
			req.SetPathValue(kv[i], kv[i+1])
		}
		if u != nil {
			req = req.WithContext(withUserCtx(req, u))
		}
		rec := httptest.NewRecorder()
		requireWrites(h)(rec, req)
		return rec
	}

	// Only an admin gets at a waiting book.
	if c := call(importFileHandler, reader, "GET", it.ID, "format", "epub").Code; c != http.StatusForbidden {
		t.Errorf("non-admin download: %d", c)
	}
	if c := call(importFileHandler, nil, "GET", it.ID, "format", "epub").Code; c != http.StatusUnauthorized {
		t.Errorf("anonymous download: %d", c)
	}
	if rec := call(importFileHandler, admin, "GET", it.ID, "format", "epub"); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Errorf("admin download: %d", rec.Code)
	}
	if c := call(importFileHandler, admin, "GET", it.ID, "format", "pdf").Code; c != http.StatusNotFound {
		t.Errorf("a format the book doesn't have: %d", c)
	}
	for _, id := range []string{"..", "0000", "../" + it.ID} {
		if c := call(importMetadataHandler, admin, "GET", id).Code; c != http.StatusNotFound {
			t.Errorf("id %q: %d", id, c)
		}
	}

	rec := call(importISBNsHandler, admin, "GET", it.ID)
	var isbns isbnResponse
	json.Unmarshal(rec.Body.Bytes(), &isbns)
	if len(isbns.Candidates) != 1 || isbns.Candidates[0].ISBN13 != "9780134190440" {
		t.Errorf("ISBNs in the waiting book's files: %s", rec.Body)
	}

	rec = call(importQueueHandler, admin, "GET", "")
	var q importQueue
	json.Unmarshal(rec.Body.Bytes(), &q)
	if len(q.Items) != 1 || q.Items[0].Title != "Uploaded" || q.Items[0].HasCover {
		t.Errorf("queue: %s", rec.Body)
	}

	if c := call(importCoverHandler, admin, "GET", it.ID).Code; c != http.StatusNotFound {
		t.Errorf("cover before one is chosen: %d", c)
	}
	if _, err := saveCoverJPEG(w.inbox.itemDir(it.ID), image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	if c := call(importCoverHandler, admin, "GET", it.ID).Code; c != http.StatusOK {
		t.Errorf("cover: %d", c)
	}

	if c := call(importDiscardHandler, admin, "POST", it.ID).Code; c != http.StatusOK {
		t.Errorf("discard: %d", c)
	}
	if c := call(importMetadataHandler, admin, "GET", it.ID).Code; c != http.StatusNotFound {
		t.Errorf("after discard: %d", c)
	}
}

func TestNextImport(t *testing.T) {
	list := []*pendingImport{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	for id, want := range map[string]string{"a": "b", "b": "c", "c": "a", "gone": "b"} {
		if got := nextImport(list, id); got != want && !(id == "gone" && got != "") {
			t.Errorf("after %s: %q, want %q", id, got, want)
		}
	}
	if got := nextImport(list[:1], "a"); got != "" {
		t.Errorf("the only book has a next: %q", got)
	}
}

// A file must be unchanged for the settle time, however often it's looked at.
func TestWatchFolderSettleTime(t *testing.T) {
	w, dir := newTestWatcher(t)
	w.settle = 150 * time.Millisecond
	writeTestFile(t, filepath.Join(dir, "Book.epub"), importTestEPUB(t))
	w.scan()
	if n := w.scan(); n != 0 {
		t.Fatal("taken by two scans in quick succession")
	}
	if w.settling != 1 {
		t.Errorf("settling = %d: another scan is needed", w.settling)
	}
	time.Sleep(200 * time.Millisecond)
	if n := w.scan(); n != 1 || w.settling != 0 {
		t.Fatalf("after the settle time: took %d, %d still settling", n, w.settling)
	}
}

// The real thing: no scans until the system reports a change.
func TestWatchFolderRunFollowsEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on filesystem events; skipped with -short")
	}
	w, dir := newTestWatcher(t)
	w.settle = 200 * time.Millisecond
	go w.run()
	time.Sleep(300 * time.Millisecond) // the first scan, of an empty folder

	wait := func(want int) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if w.inbox.count() == want {
				return
			}
		}
		_, problem, polling := w.status()
		t.Fatalf("%d book(s) in the queue, want %d (polling=%v, problem=%q)", w.inbox.count(), want, polling, problem)
	}
	writeTestFile(t, filepath.Join(dir, "First.epub"), importTestEPUB(t))
	wait(1)
	// A folder made later is watched too.
	writeTestFile(t, filepath.Join(dir, "later", "Second.epub"), importTestEPUB(t))
	wait(2)
	if _, _, polling := w.status(); polling {
		t.Error("fell back to polling on a system that can watch folders")
	}
}

// -watch-dir-force-polling: scans on a timer, no watching.
func TestWatchFolderForcePolling(t *testing.T) {
	w, dir := newTestWatcher(t)
	w.forcePoll = true
	writeTestFile(t, filepath.Join(dir, "Book.epub"), importTestEPUB(t))
	go w.run()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, _, polling := w.status(); polling {
			// Only the first scan has run; the next is a poll interval away.
			time.Sleep(100 * time.Millisecond)
			if waiting, _, _ := w.status(); waiting != 1 || w.inbox.count() != 0 {
				t.Errorf("after the first poll: %d waiting, %d taken", waiting, w.inbox.count())
			}
			return
		}
	}
	t.Fatal("never started polling")
}
