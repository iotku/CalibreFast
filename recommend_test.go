package main

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
)

func TestRecommend(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	add := func(title string, author string, tags []string, series string, index float64) string {
		uuid := addTestBook(t, db, title, []string{author}, tags, series, "epub")
		if series != "" {
			mustExec(t, db, `UPDATE books SET series_index = ? WHERE uuid = ?`, index, uuid)
		}
		return uuid
	}
	// A series, read up to book 2; book 3 is next.
	dune1 := add("Dune", "Herbert", []string{"Fiction", "Desert Planets"}, "Dune", 1)
	dune2 := add("Dune Messiah", "Herbert", []string{"Fiction", "Desert Planets"}, "Dune", 2)
	dune3 := add("Children of Dune", "Herbert", []string{"Fiction", "Desert Planets"}, "Dune", 3)
	add("God Emperor of Dune", "Herbert", []string{"Fiction"}, "Dune", 4)
	// Sharing the rare tag ranks above sharing only "Fiction".
	arrakis := add("Sand Worlds", "Someone", []string{"Fiction", "Desert Planets"}, "", 0)
	other := add("A Herbert Novel", "Herbert", []string{"Fiction"}, "", 0)
	var plain []string
	for i := range 10 {
		plain = append(plain, add("Plain "+strconv.Itoa(i), "Nobody", []string{"Fiction"}, "", 0))
	}
	junk := add("Owned Thing", "Nobody", []string{"Owned"}, "", 0)
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 2))

	// Nothing marked: nothing to go on.
	if recs, _, tags := recommend(readingMarks{Reading: map[string]*float64{}}, nil, rng, 0); len(recs) != 0 || len(tags) != 0 {
		t.Fatalf("no seeds: %v %v", recs, tags)
	}

	m := readingMarks{
		Reading:  map[string]*float64{dune2: nil},
		Finished: []string{dune1},
	}
	recs, _, tags := recommend(m, nil, rng, 0)
	if len(recs) != 14 { // everything not marked, apart from the Owned book (no shared tag)
		t.Fatalf("recs: %+v", recs)
	}
	// Next in series first, after the furthest one read.
	if recs[0].Book.UUID != dune3 || recs[0].Kind != "series" || recs[0].Because != "Dune Messiah" {
		t.Fatalf("first: %+v", recs[0])
	}
	// Then the rare shared tag; then the same author (the bonus); then the
	// books sharing only the common tag.
	if recs[1].Book.UUID != arrakis || recs[1].Kind != "tags" || recs[1].Because == "" {
		t.Fatalf("second: %+v", recs[1])
	}
	if a, b := recs[2].Book.Authors, recs[3].Book.Authors; a != "Herbert" || b != "Herbert" {
		t.Fatalf("same author next: %+v %+v", recs[2], recs[3])
	}
	for _, r := range recs[4:] {
		if !slices.Contains(plain, r.Book.UUID) {
			t.Fatalf("after the author's books: %+v", r)
		}
	}
	for _, r := range recs {
		if r.Book.UUID == dune1 || r.Book.UUID == dune2 || r.Book.UUID == junk {
			t.Fatalf("recommended a marked or unrelated book: %+v", r)
		}
	}
	// The rare tag first: it's on fewer books.
	if !slices.Equal(tags, []string{"Desert Planets", "Fiction"}) {
		t.Fatalf("tags: %q", tags)
	}

	// Marked books are never recommended; a wanted next-in-series still is.
	m.Wanted = []string{dune3, arrakis}
	recs, _, _ = recommend(m, nil, rng, 0)
	if recs[0].Book.UUID != dune3 {
		t.Fatalf("wanted next in series: %+v", recs[0])
	}
	for _, r := range recs[1:] {
		if r.Book.UUID == arrakis {
			t.Fatal("recommended a wanted book")
		}
	}
	_ = other

	// An excluded tag doesn't count.
	mustExecApp := func(q string, args ...any) {
		if _, err := appDB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExecApp(`INSERT INTO discover_tags (name, mode) VALUES ('Desert Planets', 'deny')`)
	tagSettingsVersion.Add(1)
	_, _, tags = recommend(m, nil, rng, 0)
	if slices.Contains(tags, "Desert Planets") {
		t.Fatalf("excluded tag scored: %q", tags)
	}
}
