package main

import (
	"os"
	"strconv"
	"testing"
	"time"
)

// Edits patch only the pages holding the books, and end exactly where a
// full rewrite would.
func TestRefreshPagesFor(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	var uuids []string
	for i := range booksPerPage*2 + 10 {
		uuids = append(uuids, addTestBook(t, db, "Book "+strconv.Itoa(i), []string{"A"}, nil, "", "epub"))
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	stat := func(path string) time.Time {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return fi.ModTime()
	}
	// Old enough that a rewrite shows.
	past := time.Now().Add(-time.Hour)
	for p := 1; p <= 3; p++ {
		_ = os.Chtimes(pageFile(p), past, past)
		_ = os.Chtimes(shufflePageFile(p), past, past)
	}
	before := pagesVersion.Load()

	// The oldest book is on the last Latest page.
	edited := uuids[0]
	mustExec(t, db, `UPDATE books SET title = 'Renamed' WHERE uuid = ?`, edited)
	catalogChanged(edited)
	refreshPagesFor(edited)

	books, err := readPage(3)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range books {
		if b.UUID == edited {
			found = b.Title == "Renamed"
		}
	}
	if !found {
		t.Fatalf("page 3 doesn't have the new title: %+v", books)
	}
	if !stat(pageFile(1)).Equal(past) || !stat(pageFile(2)).Equal(past) {
		t.Fatal("pages without the book were rewritten")
	}
	rewritten := 0
	for p := 1; p <= 3; p++ {
		if !stat(shufflePageFile(p)).Equal(past) {
			rewritten++
		}
	}
	if rewritten != 1 {
		t.Fatalf("%d shuffle pages rewritten, want 1", rewritten)
	}
	if pagesVersion.Load() == before {
		t.Fatal("pagesVersion not bumped")
	}
	// Exactly what writing every page gives.
	pagesMu.Lock()
	changed, err := writePagesFromCatalog(currentCatalog())
	pagesMu.Unlock()
	if err != nil || changed {
		t.Fatalf("a full write still had changes: %v %v", changed, err)
	}

	// A new cover changes the book's stamp in the page files.
	stamp := books[0].LastModified
	cover := books[0].UUID
	time.Sleep(1100 * time.Millisecond) // the stamp has whole seconds
	coverChanged(cover)
	books, _ = readPage(3)
	if books[0].UUID != cover || books[0].LastModified == stamp {
		t.Fatalf("cover stamp not updated: %q -> %q", stamp, books[0].LastModified)
	}
}
