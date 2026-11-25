package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iotku/CalibreFast/pkg/kosync"
)

// patterned is P(n) from the kosync conformance vectors
// (github.com/pid1/kosync-conformance, SPEC.md §8.6): byte i is
// (i*31 + i>>8) mod 256.
func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i>>8)
	}
	return b
}

// addBookFile writes a format file into a test book's folder and records it
// in Calibre's data table. It returns the file's path.
func addBookFile(t *testing.T, uuid, name, format string, content []byte) string {
	t.Helper()
	var id int64
	var dir string
	if err := calibreDB.QueryRow(`SELECT id, path FROM books WHERE uuid = ?`, uuid).Scan(&id, &dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(baseDir, dir, name+"."+format)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := calibreDB.Exec(`INSERT INTO data (book, format, uncompressed_size, name) VALUES (?, ?, ?, ?)`,
		id, format, len(content), name); err != nil {
		t.Fatal(err)
	}
	libraryNames.mu.Lock()
	libraryNames.ids = nil // pick up the new file
	libraryNames.mu.Unlock()
	return path
}

func TestBookDigestCacheAndResolve(t *testing.T) {
	u, books := setupLibraryAndUser(t, 2)
	pdf := patterned(5000)
	path := addBookFile(t, books[0], "Book 0 - A", "pdf", pdf)
	want, _ := kosync.PartialMD5(bytes.NewReader(pdf))

	if _, ok := resolveDocument(u.ID, want); ok {
		t.Fatal("an unhashed file must not be found by content (hashing is lazy)")
	}
	if got, err := bookDigest(books[0], "pdf"); err != nil || got != want {
		t.Fatalf("bookDigest = %s, %v", got, err)
	}
	if b, ok := resolveDocument(u.ID, want); !ok || b != (bookFile{books[0], "pdf"}) {
		t.Fatalf("by content: %+v %v", b, ok)
	}
	// File-name ids come from Calibre's database, without hashing anything.
	if b, ok := resolveDocument(u.ID, kosync.FileNameDigest("Book 0 - A.pdf")); !ok || b.UUID != books[0] {
		t.Fatalf("by file name: %+v %v", b, ok)
	}
	if _, ok := resolveDocument(u.ID, kosync.MD5Hex("not in the library")); ok {
		t.Fatal("unknown id resolved")
	}

	// A changed file is hashed again; its old id no longer matches.
	changed := patterned(6000)
	changed[0] ^= 0xff
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(path, later, later)
	if _, ok := resolveDocument(u.ID, want); ok {
		t.Fatal("stale digest still matched")
	}
	now, _ := kosync.PartialMD5(bytes.NewReader(changed))
	if b, ok := resolveDocument(u.ID, now); !ok || b.UUID != books[0] {
		t.Fatalf("new digest: %+v %v", b, ok)
	}

	// Two books with the same file name match neither.
	addBookFile(t, books[1], "Book 0 - A", "pdf", patterned(10))
	if _, ok := libraryNames.lookup(kosync.FileNameDigest("Book 0 - A.pdf")); ok {
		t.Fatal("ambiguous file name resolved")
	}
}

func TestHashWebReadBooks(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	epub := patterned(3000)
	addBookFile(t, books[0], "Web", "epub", epub)
	want, _ := kosync.PartialMD5(bytes.NewReader(epub))
	// Read on the web before any hashing.
	if _, err := appDB.Exec(`INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at)
		VALUES (?, ?, 'epub', 'epubcfi(/6/2!/4)', 0.1, 1)`, u.ID, books[0]); err != nil {
		t.Fatal(err)
	}
	if hashWebReadBooks(u.ID, 0) {
		t.Fatal("no budget: nothing should be hashed")
	}
	// A device asking about it finds it.
	if b, ok := resolveDocument(u.ID, want); !ok || b.UUID != books[0] {
		t.Fatal("web-read book not found")
	}
	if b, ok := digestMatch(want); !ok || b.UUID != books[0] {
		t.Fatal("web-read book not hashed")
	}
}

// A stale file-name index still answers at once, and is rebuilt in the
// background so a book added since is found soon after.
func TestNameIndexRebuildsInBackground(t *testing.T) {
	_, books := setupLibraryAndUser(t, 2)
	addBookFile(t, books[0], "First", "epub", []byte("a"))
	if _, ok := libraryNames.lookup(kosync.FileNameDigest("First.epub")); !ok {
		t.Fatal("first lookup builds the index")
	}
	var id int64
	if err := calibreDB.QueryRow(`SELECT id FROM books WHERE uuid = ?`, books[1]).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := calibreDB.Exec(`INSERT INTO data (book, format, uncompressed_size, name) VALUES (?, 'EPUB', 1, 'Second')`, id); err != nil {
		t.Fatal(err)
	}
	second := kosync.FileNameDigest("Second.epub")
	if _, ok := libraryNames.lookup(second); ok {
		t.Fatal("a fresh index isn't rebuilt")
	}
	libraryNames.mu.Lock()
	libraryNames.built = time.Now().Add(-2 * nameIndexMaxAge)
	libraryNames.mu.Unlock()
	if _, ok := libraryNames.lookup(second); ok {
		t.Fatal("a stale index answers from what it has, without waiting for the rebuild")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, ok := libraryNames.lookup(second); ok {
			if b.UUID != books[1] {
				t.Fatalf("found %+v", b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background rebuild never found the new file")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
