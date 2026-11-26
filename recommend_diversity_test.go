package main

import (
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// recDiversityFixture: 30 Space books by one author (and, from book 20,
// under a pen name grouped with them), 30 by others, and 30 Other books
// so Space says something.
func recDiversityFixture(t *testing.T) (prolific, others []string) {
	t.Helper()
	origGroups := groups
	groups = loadGroups(filepath.Join(t.TempDir(), groupsFileName))
	t.Cleanup(func() { groups = origGroups })
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	if _, err := groups.set("author", "Prolific", Group{Name: "Prolific", Members: []string{"Prolific", "Pen Name"}}); err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		name := "Prolific"
		if i >= 20 {
			name = "Pen Name"
		}
		prolific = append(prolific, addTestBook(t, db, "P "+strconv.Itoa(i), []string{name}, []string{"Space"}, "", "epub"))
		others = append(others, addTestBook(t, db, "O "+strconv.Itoa(i), []string{"Author " + strconv.Itoa(i)}, []string{"Space"}, "", "epub"))
		addTestBook(t, db, "X "+strconv.Itoa(i), []string{"Someone"}, []string{"Other"}, "", "epub")
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	return prolific, others
}

// No author (a group counting as one) has more than two books in a row
// while there's anything else to show, and nothing is dropped for it.
func TestRecommendAuthorCap(t *testing.T) {
	prolific, _ := recDiversityFixture(t)
	m := readingMarks{Reading: map[string]*float64{prolific[0]: nil}}
	for seed := range uint64(5) {
		l, _ := rankRecs(m, recFavs{"author": {"prolific": true}}, nil, rand.New(rand.NewPCG(seed, 0)))
		n := 0
		for _, u := range l.pageUUIDs(0) {
			if slices.Contains(prolific, u) {
				n++
			}
		}
		if n > recAuthorPerRow {
			t.Fatalf("seed %d: %d of the favorite author's books in the first row", seed, n)
		}
		all := map[string]bool{}
		for i := range l.len() {
			all[l.uuid(i)] = true
		}
		for _, u := range prolific[1:] {
			if !all[u] {
				t.Fatalf("seed %d: a book left out for the cap: %s", seed, u)
			}
		}
	}
}

// A favorite tag that isn't among the top tags still gives candidates.
func TestRecommendFavoriteTagOutsideTop(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	// The seed has more tags than recTopTags, all rarer than Liked.
	var seedTags []string
	for i := range recTopTags + 2 {
		seedTags = append(seedTags, "Seed "+strconv.Itoa(i))
	}
	seed := addTestBook(t, db, "Seed", []string{"A"}, seedTags, "", "epub")
	for i := range recTopTags + 2 {
		addTestBook(t, db, "S"+strconv.Itoa(i), []string{"A"}, []string{seedTags[i]}, "", "epub")
	}
	var liked []string
	for i := range 20 {
		liked = append(liked, addTestBook(t, db, "L"+strconv.Itoa(i), []string{"B"}, []string{"Liked"}, "", "epub"))
		addTestBook(t, db, "Filler "+strconv.Itoa(i), []string{"C"}, nil, "", "epub")
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	m := readingMarks{Reading: map[string]*float64{seed: nil}}
	l, _ := rankRecs(m, recFavs{"tag": {"liked": true}}, nil, rand.New(rand.NewPCG(1, 2)))
	found := 0
	for i := range l.len() {
		if slices.Contains(liked, l.uuid(i)) && l.items[i].kind == "tags" {
			found++
		}
	}
	if found < len(liked)/2 {
		t.Fatalf("favorite tag's books as candidates: %d of %d", found, len(liked))
	}
}

// Wildcards: a couple a row, from outside the candidates, never a marked or
// hidden book, and the same for the same seed.
func TestRecommendWildcards(t *testing.T) {
	prolific, others := recDiversityFixture(t)
	m := readingMarks{Reading: map[string]*float64{others[0]: nil}, Finished: []string{prolific[0]}}
	hide := map[string]bool{others[1]: true}
	list := func() (l recList) {
		l, _ = rankRecs(m, nil, hide, rand.New(rand.NewPCG(7, 0)))
		return l
	}
	l := list()
	for p := range l.pages() {
		wild := 0
		for i := p * recRowMax; i < min(l.len(), (p+1)*recRowMax); i++ {
			it := l.items[i]
			if it.kind != "discover" {
				continue
			}
			wild++
			u := l.uuid(i)
			if u == others[0] || u == prolific[0] || hide[u] || slices.Contains(others, u) || slices.Contains(prolific, u) {
				t.Fatalf("page %d: wildcard %s is marked, hidden or a candidate", p, u)
			}
			if i-p*recRowMax < recRowMax/2 {
				t.Fatalf("page %d: wildcard in the front half, at %d", p, i-p*recRowMax)
			}
		}
		if wild > recWildcards {
			t.Fatalf("page %d: %d wildcards", p, wild)
		}
	}
	again := list()
	if !slices.Equal(l.pageUUIDs(0), again.pageUUIDs(0)) {
		t.Fatal("the same seed gave a different row")
	}
}

// A gap in the row as shown is filled within the author cap, counting the
// books kept; with nothing else left, by whatever there is.
func TestSettleRowAuthorCap(t *testing.T) {
	c := newCatalog()
	ix := &recIndex{cat: c}
	l := recList{cat: c, ix: ix}
	// b0, b1 by author 1 (shown); b2 gone; then b3, b4 by author 1 and b5 by 2.
	for i, a := range []int32{1, 1, 1, 1, 1, 2} {
		u := "b" + strconv.Itoa(i)
		c.byUUID[u] = int32(i)
		c.books = append(c.books, catalogBook{Book: Book{UUID: u}, AuthorIDs: []int32{a}})
		if i >= 3 {
			l.items = append(l.items, recItem{slot: int32(i), kind: "discover", authors: []int32{a}})
		}
	}
	c.books[2].Removed = true
	shown := []recShown{{UUID: "b0", Kind: "discover"}, {UUID: "b1", Kind: "discover"}, {UUID: "b2", Kind: "discover"}}
	marks := readingMarks{Reading: map[string]*float64{}}
	row, _ := settleRow(c, shown, l, 0, marks, nil)
	got := []string{}
	for _, r := range row {
		got = append(got, r.Book.UUID)
	}
	// b2's place goes to b5 (author 2); author 1's others only after.
	if want := []string{"b0", "b1", "b5", "b3", "b4"}; !slices.Equal(got, want) {
		t.Fatalf("row %v, want %v", got, want)
	}
}
