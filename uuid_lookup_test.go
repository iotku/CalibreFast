package main

import (
	"slices"
	"strings"
	"testing"
)

// A book the catalog knows is found in metadata.db by its id (no scan of
// books); one it doesn't by uuid; and a stale id finds nothing rather than
// another book.
func TestBookWhereByID(t *testing.T) {
	ids := sortFixture(t) // stores a catalog with these books
	setRating(t, ids["apple"], 10)
	if where, _ := bookWhere(ids["apple"]); !strings.HasPrefix(where, "b.id = ?") {
		t.Errorf("catalogued book: %q", where)
	}
	if got := libraryRating(ids["apple"]); got != 10 {
		t.Errorf("rating by id: %d", got)
	}

	was := libraryCatalog.Load()
	t.Cleanup(func() { libraryCatalog.Store(was) })
	libraryCatalog.Store(newCatalog()) // before the first build
	if where, _ := bookWhere(ids["apple"]); where != "b.uuid = ?" {
		t.Errorf("unknown book: %q", where)
	}
	if got := libraryRating(ids["apple"]); got != 10 {
		t.Errorf("rating by uuid: %d", got)
	}

	libraryCatalog.Store(was)
	mustExec(t, calibreDB, `UPDATE books SET uuid = 'moved' WHERE uuid = ?`, ids["apple"])
	if got := libraryRating(ids["apple"]); got != 0 {
		t.Errorf("stale id found a rating: %d", got)
	}
}

// booksByUUIDs reads metadata.db only for books the catalog doesn't have
// yet, not for removed ones, and not again (for a while) for ids it didn't
// have either.
func TestBooksByUUIDsSkipsGone(t *testing.T) {
	ids := sortFixture(t)
	uuids := func(bs []Book) (out []string) {
		for _, b := range bs {
			out = append(out, b.UUID)
		}
		return out
	}

	// Removed: the catalog knows it's gone.
	mustExec(t, calibreDB, `DELETE FROM books WHERE uuid = ?`, ids["noauth"])
	catalogChanged(ids["noauth"])
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	if !currentCatalog().known(ids["noauth"]) {
		t.Fatal("a removed book stays known to the catalog")
	}
	if got, _ := booksByUUIDs([]string{ids["noauth"], ids["mango"]}); !slices.Equal(uuids(got), []string{ids["mango"]}) {
		t.Errorf("removed book: %q", uuids(got))
	}

	// Added a moment ago: not in the catalog yet, read from metadata.db.
	fresh := addTestBook(t, calibreDB, "Fresh", []string{"New Author"}, nil, "")
	if got, _ := booksByUUIDs([]string{fresh}); !slices.Equal(uuids(got), []string{fresh}) {
		t.Errorf("new book: %q", uuids(got))
	}

	// Never in the library: asked once, then skipped until the catalog changes.
	if got, _ := booksByUUIDs([]string{"no-such-book"}); len(got) != 0 {
		t.Errorf("unknown id: %q", uuids(got))
	}
	if !notInLibrary.has("no-such-book") {
		t.Error("an id metadata.db didn't have is remembered")
	}
	catalogChanged(fresh)
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	if notInLibrary.has("no-such-book") {
		t.Error("a new catalog forgets the ids metadata.db didn't have")
	}
}
