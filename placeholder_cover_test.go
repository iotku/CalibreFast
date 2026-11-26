package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A placeholder's number is shown on its cover only when it's in a series
// and has one.
func TestPlaceholderNumber(t *testing.T) {
	for _, tc := range []struct{ series, index, want string }{
		{"Saga", "3", "3"},
		{"Saga", "2.50", "2.5"},
		{"Saga", "1", "1"},
		{"Saga", "", ""},
		{"", "4", ""},
	} {
		p := &placeholder{Meta: OPFMetadata{Title: "T", Series: tc.series, SeriesIndex: tc.index}}
		if got := p.number(); got != tc.want {
			t.Errorf("number(%q, %q) = %q, want %q", tc.series, tc.index, got, tc.want)
		}
		if got := p.book().Placeholder.Number; got != tc.want {
			t.Errorf("card number(%q, %q) = %q, want %q", tc.series, tc.index, got, tc.want)
		}
	}
}

// A cover fetched from Hardcover goes when its placeholder is removed; one
// an editor set stays (an undo may want it), and setting one clears the mark.
func TestFetchedPlaceholderCover(t *testing.T) {
	setupTestAppDB(t)
	origData := dataDir
	dataDir = t.TempDir()
	placeholdersChanged()
	t.Cleanup(func() { dataDir = origData; placeholdersChanged() })

	dir := filepath.Join(dataDir, placeholderCoversDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := func(name string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("jpeg"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	fetched := strings.Repeat("a", 32) + ".jpg"
	chosen := strings.Repeat("b", 32) + ".jpg"
	fetchedPath, chosenPath := file(fetched), file(chosen)
	exists := func(path string) bool { _, err := os.Stat(path); return err == nil }
	mark := func(id string) (cover string, flag int) {
		t.Helper()
		if err := appDB.QueryRow(`SELECT cover, cover_fetched FROM placeholders WHERE uuid = ?`, id).Scan(&cover, &flag); err != nil {
			t.Fatal(err)
		}
		return
	}

	hc := &placeholder{Meta: OPFMetadata{Title: "From Hardcover"}, Source: "hardcover"}
	if err := savePlaceholder(hc, nil); err != nil {
		t.Fatal(err)
	}
	setFetchedPlaceholderCover(placeholderByUUID(hc.UUID), fetched)
	if p := placeholderByUUID(hc.UUID); p == nil || p.Cover != fetched {
		t.Fatalf("fetched cover not set: %+v", p)
	}
	// An edit that keeps the cover keeps the mark.
	p := placeholderByUUID(hc.UUID)
	p.Meta.Publisher = "Pub"
	if err := savePlaceholder(p, nil); err != nil {
		t.Fatal(err)
	}
	if c, f := mark(hc.UUID); c != fetched || f != 1 {
		t.Errorf("after an edit: %q %d", c, f)
	}
	if err := replacePlaceholder(hc.UUID, "some-book"); err != nil {
		t.Fatal(err)
	}
	if exists(fetchedPath) {
		t.Error("fetched cover kept after the placeholder was replaced")
	}
	if c, _ := mark(hc.UUID); c != "" {
		t.Errorf("removed placeholder still names its cover %q", c)
	}

	// A fetched cover changed: setting it while the placeholder changed
	// meanwhile (another cover) doesn't overwrite it, and the file goes.
	other := &placeholder{Meta: OPFMetadata{Title: "Chosen"}, Source: "hardcover", Cover: chosen}
	if err := savePlaceholder(other, nil); err != nil {
		t.Fatal(err)
	}
	stale := *other
	stale.Cover = ""
	late := strings.Repeat("c", 32) + ".jpg"
	latePath := file(late)
	setFetchedPlaceholderCover(&stale, late)
	if c, f := mark(other.UUID); c != chosen || f != 0 || exists(latePath) {
		t.Errorf("a late fetch replaced an editor's cover: %q %d (file kept: %v)", c, f, exists(latePath))
	}
	if err := deletePlaceholder(other.UUID); err != nil {
		t.Fatal(err)
	}
	if !exists(chosenPath) {
		t.Error("an editor's cover was removed with its placeholder")
	}

	// A fetched cover replaced by an editor's loses the mark.
	third := &placeholder{Meta: OPFMetadata{Title: "Third"}, Source: "hardcover"}
	if err := savePlaceholder(third, nil); err != nil {
		t.Fatal(err)
	}
	setFetchedPlaceholderCover(placeholderByUUID(third.UUID), fetched)
	p = placeholderByUUID(third.UUID)
	p.Cover = chosen
	if err := savePlaceholder(p, nil); err != nil {
		t.Fatal(err)
	}
	if c, f := mark(third.UUID); c != chosen || f != 0 {
		t.Errorf("an editor's cover still marked fetched: %q %d", c, f)
	}
}

// Users who'd rather not see placeholders don't get them on series pages,
// in search or in On Deck.
func TestHidePlaceholdersPref(t *testing.T) {
	_, _, other := seriesFixture(t)
	origData, origWrites := dataDir, allowWrites
	dataDir, allowWrites = t.TempDir(), true
	placeholdersChanged()
	t.Cleanup(func() { dataDir, allowWrites = origData, origWrites; placeholdersChanged() })

	alice, _ := createUser("alice", "a long enough password", false)
	if err := savePlaceholder(&placeholder{Meta: OPFMetadata{Title: "Other Three", Creators: []string{"Writer"}, Series: "Other", SeriesIndex: "3"}}, nil); err != nil {
		t.Fatal(err)
	}
	mux := placeholderMux()
	mux.HandleFunc("PUT /api/me/prefs", putPrefsHandler)
	series := func() []Book {
		t.Helper()
		rec := call(t, mux, alice, "GET", "/series/Other?api=1&page=1", "")
		var books []Book
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &books) != nil {
			t.Fatalf("series page: %d %s", rec.Code, rec.Body)
		}
		return books
	}
	hasPh := func(books []Book) bool {
		return slices.ContainsFunc(books, func(b Book) bool { return b.Placeholder != nil })
	}
	if b := series(); !hasPh(b) || len(b) != 3 {
		t.Fatalf("placeholder not shown by default: %+v", b)
	}
	if b := series(); b[2].Placeholder.Number != "3" {
		t.Errorf("card number %q", b[2].Placeholder.Number)
	}

	if rec := call(t, mux, alice, "PUT", "/api/me/prefs", `{"hide_placeholders":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("prefs: %d %s", rec.Code, rec.Body)
	}
	if !hidesPlaceholders(alice) || hidesPlaceholders(nil) {
		t.Fatal("hidesPlaceholders")
	}
	if b := series(); hasPh(b) || len(b) != len(other) {
		t.Errorf("hidden, yet: %+v", b)
	}
	if body := call(t, mux, alice, "GET", "/series/Other", "").Body.String(); !strings.Contains(body, `data-total-books="2"`) {
		t.Error("series page total counts a hidden placeholder")
	}

	// On Deck: the placeholder after the one finished, unless hidden.
	m := readingMarks{Finished: []string{other[1]}}
	deck := nextInSeries(currentRecIndex(), m, nil, nil, false)
	if !hasPh(deck) {
		t.Errorf("On Deck without the placeholder: %+v", deck)
	}
	if deck := nextInSeries(currentRecIndex(), m, nil, nil, true); hasPh(deck) {
		t.Errorf("On Deck shows a hidden placeholder: %+v", deck)
	}

	// Search.
	res, err := runSearch(t.Context(), "other three")
	if err != nil {
		t.Fatal(err)
	}
	if !hasPh(res.pageOf(sortByBest, 1, true)) || hasPh(res.pageOf(sortByBest, 1, false)) {
		t.Error("search: placeholders not shown, or not hidden")
	}
}
