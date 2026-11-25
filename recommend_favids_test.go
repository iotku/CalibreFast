package main

import (
	"path/filepath"
	"testing"
)

// favIDs and favTags (from the favorites, by name) find what asking has of
// every name in the library would, with groups: a group favorite covers its
// members, and a name now in a group isn't a favorite under its own name.
func TestFavIDsMatchesEveryName(t *testing.T) {
	origGroups := groups
	groups = loadGroups(filepath.Join(t.TempDir(), groupsFileName))
	t.Cleanup(func() { groups = origGroups })
	db := catalogFixture(t)
	addTestBook(t, db, "One", []string{"J. R. R. Tolkien"}, []string{"Sci-Fi"}, "")
	addTestBook(t, db, "Two", []string{"John Tolkien", "Ann Lee"}, []string{"Science Fiction", "Poetry"}, "")
	addTestBook(t, db, "Three", []string{"Bob Day"}, []string{"History"}, "")
	addTestBook(t, db, "Four", []string{"Cy Ray"}, nil, "")
	for _, g := range []struct {
		cat, name string
		members   []string
	}{
		{"author", "Tolkien", []string{"J. R. R. Tolkien", "John Tolkien"}},
		{"author", "Bobs", []string{"Bob Day"}},
		{"tag", "SF", []string{"Sci-Fi", "Science Fiction"}},
	} {
		if _, err := groups.set(g.cat, g.name, Group{Name: g.name, Members: g.members}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	was := libraryCatalog.Load()
	libraryCatalog.Store(c)
	t.Cleanup(func() { libraryCatalog.Store(was) })
	ix := currentRecIndex()

	favs := recFavs{
		"author": {"tolkien": true, "ann lee": true, "bob day": true}, // Bob Day is now in Bobs
		"tag":    {"sf": true, "poetry": true},
	}
	every := func(names map[int32]string, kind string) map[int32]bool {
		out := map[int32]bool{}
		for id, n := range names {
			if favs.has(kind, n) {
				out[id] = true
			}
		}
		return out
	}
	got, want := ix.favIDs(favs, "author"), every(c.authorNames, "author")
	if len(want) != 3 || len(got) != len(want) {
		t.Fatalf("authors: got %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("author %q missing", c.authorNames[id])
		}
	}
	wantTags := map[int32]bool{}
	for e, n := range ix.names {
		if favs.has("tag", n) {
			wantTags[int32(e)] = true
		}
	}
	gotTags := ix.favTags(favs)
	if len(wantTags) != 2 || len(gotTags) != len(wantTags) {
		t.Fatalf("tags: got %v, want %v", gotTags, wantTags)
	}
	for e := range wantTags {
		if !gotTags[e] {
			t.Errorf("tag %q missing", ix.names[e])
		}
	}
}
