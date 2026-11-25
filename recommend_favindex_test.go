package main

import (
	"slices"
	"testing"
)

// The index of favorites' books gives what a pass over the whole catalog
// would, in the same orders: Latest for followedBooks, the shuffle for
// rankRecs. Removed books are left out.
func TestFavBooksMatchesCatalogScan(t *testing.T) {
	db := catalogFixture(t)
	for i := range 12 {
		author := []string{"Ann", "Bob", "Cy"}[i%3]
		addTestBook(t, db, "Book "+itoa(int64(i)), []string{author, "Shared " + itoa(int64(i%2))}, nil, "")
	}
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	removed := c.books[c.latest()[0]].UUID // the newest: first in Latest
	c.books[c.byUUID[removed]].Removed = true
	was := libraryCatalog.Load()
	libraryCatalog.Store(c)
	t.Cleanup(func() { libraryCatalog.Store(was) })
	ix := currentRecIndex()

	fav := map[int32]bool{}
	for id, n := range c.authorNames {
		if n == "Ann" || n == "Shared 1" {
			fav[id] = true
		}
	}
	scan := func(order []int32) (out []int32) {
		for _, s := range order {
			b := &c.books[s]
			if !b.Removed && slices.ContainsFunc(b.AuthorIDs, func(a int32) bool { return fav[a] }) {
				out = append(out, s)
			}
		}
		return out
	}
	if got, want := ix.favBooks(fav, nil, ix.latestPos), scan(c.latest()); !slices.Equal(got, want) || len(want) == 0 {
		t.Errorf("Latest order: %v, want %v", got, want)
	}
	if got, want := ix.favBooks(fav, nil, ix.shufflePos), scan(c.shuffle); !slices.Equal(got, want) {
		t.Errorf("shuffle order: %v, want %v", got, want)
	}
	if ix.total != len(c.books)-1 {
		t.Errorf("total %d, want %d", ix.total, len(c.books)-1)
	}
}
