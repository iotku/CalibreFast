package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"errors"
	"mime/multipart"
	"path/filepath"
	"strings"
	"testing"
)

// titledEPUB is an EPUB with this title and author; extra (any metadata)
// makes otherwise equal books different files.
func titledEPUB(t *testing.T, title, author, extra string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{
		{"mimetype", "application/epub+zip"},
		{"META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`},
		{"content.opf", `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>` + title + `</dc:title>
    <dc:creator>` + author + `</dc:creator>
    <dc:language>en</dc:language>
    <dc:description>` + extra + `</dc:description>
  </metadata>
</package>`},
	} {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(f.body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// uploadFiles runs one upload group, as the upload page sends it.
func uploadFiles(t *testing.T, name string, data []byte, override bool) UploadResult {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("books", name)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	form, err := multipart.NewReader(&body, mw.Boundary()).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { form.RemoveAll() })
	res := processUploadGroup(form.File["books"], override)
	if res.Error != "" {
		t.Fatalf("upload %s: %s", name, res.Error)
	}
	if res.UUID != "" {
		t.Cleanup(func() { uuidPathIndex.Delete(res.UUID); bookMetadataCache.Delete(res.UUID) })
	}
	return res
}

func TestUploadDuplicates(t *testing.T) {
	db := withTestLibrary(t)
	books := func() int {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM books`).Scan(&n)
		return n
	}
	original := titledEPUB(t, "Dune", "Frank Herbert", "first")
	first := uploadFiles(t, "Dune.epub", original, false)
	if first.UUID == "" || first.Duplicate != nil {
		t.Fatalf("first upload: %+v", first)
	}

	// The same file again: refused, override or not.
	for _, override := range []bool{false, true} {
		res := uploadFiles(t, "Dune (copy).epub", original, override)
		if res.UUID != "" || !res.Duplicate.found() || !res.Duplicate.SameFile() {
			t.Errorf("same file, override %v: %+v", override, res)
		} else if b := res.Duplicate.Books[0]; b.UUID != first.UUID || !b.SameAuthors {
			t.Errorf("same file names %+v", b)
		}
		if len(res.Files) != 1 || res.Files[0] != "Dune (copy).epub" {
			t.Errorf("files to send again: %v", res.Files)
		}
	}

	// Another edition (a different file) of the same title and author:
	// refused unless overridden. Case and spacing don't hide it.
	edition := titledEPUB(t, "dune ", "FRANK  herbert", "second")
	res := uploadFiles(t, "Dune 2.epub", edition, false)
	if res.UUID != "" || !res.Duplicate.found() || res.Duplicate.SameFile() {
		t.Fatalf("other edition: %+v", res)
	}
	if b := res.Duplicate.Books[0]; !b.SameAuthors || b.SameFile || b.Title != "Dune" {
		t.Errorf("other edition names %+v", b)
	}
	if books() != 1 {
		t.Fatalf("%d books after refused uploads", books())
	}
	if res = uploadFiles(t, "Dune 2.epub", edition, true); res.UUID == "" {
		t.Fatalf("override: %+v", res)
	}

	// Same title, another author, another file: a different book.
	if res = uploadFiles(t, "Dune other.epub", titledEPUB(t, "Dune", "Someone Else", "x"), false); res.UUID == "" || res.Duplicate != nil {
		t.Errorf("same title, other author: %+v", res)
	}
	if books() != 3 {
		t.Errorf("%d books, want 3", books())
	}

	// Same title, another author, but a file the library has: a duplicate.
	copyPath := filepath.Join(t.TempDir(), "x.epub")
	writeTestFile(t, copyPath, original)
	check, err := findDuplicates(db, baseDir, "Dune", []string{"Nobody"}, []dupFile{{copyPath, "epub"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(check.Books) != 1 || check.Books[0].UUID != first.UUID || check.Books[0].SameAuthors || !check.Books[0].SameFile {
		t.Errorf("same title, same file: %+v", check)
	}

	// The editor's warning leaves the book itself out.
	check, err = libraryBookDuplicates(db, baseDir, first.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(check.Books) != 1 || check.Books[0].UUID == first.UUID || !check.Books[0].SameAuthors {
		t.Errorf("library book's duplicates: %+v", check)
	}
}

func TestApproveRefusesDuplicates(t *testing.T) {
	db := withTestLibrary(t)
	uploadFiles(t, "Dune.epub", titledEPUB(t, "Dune", "Frank Herbert", "first"), false)

	w, dir := newTestWatcher(t)
	second := titledEPUB(t, "Dune", "Frank Herbert", "second")
	// Named unlike its title: in the library it's "Dune.epub", while
	// metadata.db's data.name keeps "Dune 2nd".
	writeTestFile(t, filepath.Join(dir, "Dune 2nd.epub"), second)
	w.scan()
	w.scan()
	it := w.inbox.list()[0]
	if dup := w.inbox.duplicates(it); !dup.found() || dup.SameFile() {
		t.Errorf("queue's duplicate note: %+v", dup)
	}

	meta := metadataFromRequest(ApplyMetadataRequest{Title: "Dune", Creators: "Frank Herbert"})
	_, err := w.inbox.approve(it.ID, meta, false)
	var dup *duplicateError
	if !errors.As(err, &dup) {
		t.Fatalf("approving a duplicate: %v", err)
	}
	if w.inbox.get(it.ID) == nil {
		t.Fatal("a refused book must stay in the queue")
	}
	// Retitled by the admin: no longer a duplicate.
	other := metadataFromRequest(ApplyMetadataRequest{Title: "Dune Messiah", Creators: "Frank Herbert"})
	if check, _ := findDuplicates(db, baseDir, other.Title, other.Creators, w.inbox.dupFiles(it), ""); check.found() {
		t.Errorf("another title: %+v", check)
	}
	bookUUID, err := w.inbox.approve(it.ID, meta, true)
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	t.Cleanup(func() { uuidPathIndex.Delete(bookUUID); bookMetadataCache.Delete(bookUUID) })
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM books WHERE title = 'Dune'`).Scan(&n)
	if n != 2 {
		t.Errorf("%d Dunes, want 2", n)
	}
	// The added file is still found byte for byte.
	if res := uploadFiles(t, "Dune again.epub", second, true); res.UUID != "" || !res.Duplicate.SameFile() {
		t.Errorf("the imported file again: %+v", res)
	}
	calibreFindsFiles(t, db)
}

// calibreFindsFiles fails unless every format in metadata.db is at
// <path>/<data.name>.<format>, where Calibre looks for it.
func calibreFindsFiles(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT b.path, d.name, d.format FROM data d JOIN books b ON b.id = d.book`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var path, name, format string
		if err := rows.Scan(&path, &name, &format); err != nil {
			t.Fatal(err)
		}
		n++
		if p := filepath.Join(baseDir, path, name+"."+strings.ToLower(format)); !exists(p) {
			t.Errorf("Calibre looks for %s, which isn't there", p)
		}
	}
	if n == 0 {
		t.Error("no formats in metadata.db")
	}
}

func TestAddedFilesAreWhereCalibreLooks(t *testing.T) {
	db := withTestLibrary(t)

	// Uploaded under a name unlike its title.
	uploadFiles(t, "dune_scan_v2.epub", titledEPUB(t, "Dune", "Frank Herbert", ""), false)

	// From the import queue, two formats, retitled by the admin.
	w, dir := newTestWatcher(t)
	writeTestFile(t, filepath.Join(dir, "Book 2nd.epub"), titledEPUB(t, "Emma", "Jane Austen", ""))
	writeTestFile(t, filepath.Join(dir, "Book 2nd.pdf"), []byte("%PDF-1.4 not really"))
	w.scan()
	w.scan()
	it := w.inbox.list()[0]
	meta := metadataFromRequest(ApplyMetadataRequest{Title: "Emma: A Novel", Creators: "Jane Austen"})
	u, err := w.inbox.approve(it.ID, meta, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { uuidPathIndex.Delete(u); bookMetadataCache.Delete(u) })

	// A file already where the next book's would go: it gets another name,
	// and data.name follows. (The next book is id 3 in a fresh library.)
	writeTestFile(t, filepath.Join(baseDir, "Jane Austen", "Persuasion (3)", "Persuasion.epub"), []byte("in the way"))
	uploadFiles(t, "p.epub", titledEPUB(t, "Persuasion", "Jane Austen", ""), false)
	var name string
	db.QueryRow(`SELECT d.name FROM data d JOIN books b ON b.id = d.book WHERE b.title = 'Persuasion'`).Scan(&name)
	if name == "Persuasion" || !strings.HasPrefix(name, "Persuasion_") {
		t.Errorf("data.name after a clash: %q", name)
	}

	calibreFindsFiles(t, db)
}
