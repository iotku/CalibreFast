package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// sparseSample is the conformance suite's file S: 1 GiB + 1 KiB of zeros,
// except that at each of the twelve sample offsets the offset is written as
// 16 zero-padded decimal digits. It is served from memory.
type sparseSample struct{}

const sparseSize = 1<<30 + 1024

func (sparseSample) ReadAt(p []byte, off int64) (int, error) {
	if off >= sparseSize {
		return 0, io.EOF
	}
	n := len(p)
	if off+int64(n) > sparseSize {
		n = int(sparseSize - off)
	}
	clear(p[:n])
	for i := -1; i <= 10; i++ {
		var at int64
		if i >= 0 {
			at = 1024 << (2 * i)
		}
		mark := []byte(fmt.Sprintf("%016d", at))
		for j := range mark {
			if k := at + int64(j) - off; k >= 0 && k < int64(n) {
				p[k] = mark[j]
			}
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestPartialMD5(t *testing.T) {
	// Values from the conformance suite, computed there by a verbatim LuaJIT
	// transcription of KOReader's util.partialMD5 that also reproduces
	// KOReader's own test vectors.
	for _, tc := range []struct {
		n    int
		want string
	}{
		{500, "21f0df72cce9bc7da8dae2512ee5feed"},     // one short sample
		{1024, "f4cd1641040a17288bb6104d9e66bdb5"},    // one sample
		{1025, "170a888db01c00d8fc3e5d8d84de5838"},    // 0, then 1 byte at 1024
		{2048, "d0e59a0c7b893c3b8d6a9bddbd64e631"},    // 0, 1024
		{3000, "d0e59a0c7b893c3b8d6a9bddbd64e631"},    // same samples as 2048
		{200000, "1784b150454ef4cf6780ceb94af0386f"},  // 0 … 64 KiB
		{1050000, "0fca5c751677ecbc2dcfe80ea75ace32"}, // 0 … 1 MiB
	} {
		got, err := partialMD5(bytes.NewReader(patterned(tc.n)))
		if err != nil || got != tc.want {
			t.Errorf("P(%d) = %s, %v; want %s", tc.n, got, err, tc.want)
		}
	}
	// All twelve samples (first at 0, last at 1 GiB).
	if got, _ := partialMD5(sparseSample{}); got != "266ff27c24919f4cec96f90f9b3231ca" {
		t.Errorf("S = %s", got)
	}
	// From a file, as books are read.
	path := filepath.Join(t.TempDir(), "p.bin")
	if err := os.WriteFile(path, patterned(200000), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := partialMD5File(path); err != nil || got != "1784b150454ef4cf6780ceb94af0386f" {
		t.Errorf("file: %s, %v", got, err)
	}
}

// With KOREADER_TEST_DATA set to a checkout of github.com/koreader/test-data,
// also check the values KOReader's own tests pin (spec/unit/util_spec.lua).
func TestPartialMD5KOReaderFixtures(t *testing.T) {
	dir := os.Getenv("KOREADER_TEST_DATA")
	if dir == "" {
		t.Skip("KOREADER_TEST_DATA not set")
	}
	for file, want := range map[string]string{
		"leaves.epub": "59d481d168cca6267322f150c5f6a2a3",
		"tall.pdf":    "41cce710f34e5ec21315e19c99821415",
	} {
		if got, err := partialMD5File(filepath.Join(dir, file)); err != nil || got != want {
			t.Errorf("%s = %s, %v; want %s", file, got, err, want)
		}
	}
}

func TestFileNameDigest(t *testing.T) {
	// md5("leaves.epub"), as KOReader's filename matching method sends it.
	if got := fileNameDigest("leaves.epub"); got != md5Hex("leaves.epub") || len(got) != 32 {
		t.Fatal(got)
	}
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
	want, _ := partialMD5(bytes.NewReader(pdf))

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
	if b, ok := resolveDocument(u.ID, fileNameDigest("Book 0 - A.pdf")); !ok || b.UUID != books[0] {
		t.Fatalf("by file name: %+v %v", b, ok)
	}
	if _, ok := resolveDocument(u.ID, md5Hex("not in the library")); ok {
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
	now, _ := partialMD5(bytes.NewReader(changed))
	if b, ok := resolveDocument(u.ID, now); !ok || b.UUID != books[0] {
		t.Fatalf("new digest: %+v %v", b, ok)
	}

	// Two books with the same file name match neither.
	addBookFile(t, books[1], "Book 0 - A", "pdf", patterned(10))
	if _, ok := libraryNames.lookup(fileNameDigest("Book 0 - A.pdf")); ok {
		t.Fatal("ambiguous file name resolved")
	}
}

func TestHashWebReadBooks(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	epub := patterned(3000)
	addBookFile(t, books[0], "Web", "epub", epub)
	want, _ := partialMD5(bytes.NewReader(epub))
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
