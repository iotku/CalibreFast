package main

import (
	"path/filepath"
	"testing"
)

// A book uploaded (or queued for import) without a series takes the series
// and number of the placeholder it stands for, by title and author, and
// its Hardcover identifiers. The placeholder goes once the book is in.
func TestPlaceholderSeriesOnUpload(t *testing.T) {
	db := withTestLibrary(t)
	setupTestAppDB(t)
	origData := dataDir
	dataDir = t.TempDir()
	placeholdersChanged()
	t.Cleanup(func() { dataDir = origData; placeholdersChanged() })
	save := func(title, author, series, index string, ids ...opfIdentifier) *placeholder {
		t.Helper()
		p := &placeholder{Meta: OPFMetadata{Title: title, Creators: []string{author}, Series: series, SeriesIndex: index, Identifiers: ids}}
		if err := savePlaceholder(p, nil); err != nil {
			t.Fatal(err)
		}
		return p
	}
	hero := save("The Last Hero", "Terry Pratchett", "Discworld", "27",
		opfIdentifier{Scheme: "hardcover-id", Value: "27"}, opfIdentifier{Scheme: "hardcover", Value: "the-last-hero"})
	other := save("Mort", "Terry Pratchett", "Discworld", "4")

	// Uploaded with a subtitle and the author's name the other way round.
	res := uploadFiles(t, "hero.epub", titledEPUB(t, "The Last Hero: A Discworld Fable", "Pratchett, Terry", "x"), false)
	if res.UUID == "" {
		t.Fatalf("upload: %+v", res)
	}
	var series string
	var index float64
	if err := db.QueryRow(`SELECT s.name, b.series_index FROM books b JOIN books_series_link l ON l.book = b.id JOIN series s ON s.id = l.series WHERE b.uuid = ?`,
		res.UUID).Scan(&series, &index); err != nil || series != "Discworld" || index != 27 {
		t.Errorf("series %q #%v, err %v", series, index, err)
	}
	ids := map[string]string{}
	rows, err := db.Query(`SELECT i.type, i.val FROM identifiers i JOIN books b ON b.id = i.book WHERE b.uuid = ?`, res.UUID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		ids[k] = v
	}
	rows.Close()
	if ids["hardcover"] != "the-last-hero" || ids["hardcover-id"] != "27" {
		t.Errorf("identifiers %v", ids)
	}
	if placeholderByUUID(hero.UUID) != nil {
		t.Error("the placeholder is still there")
	}
	if u := placeholderReplacedBy(hero.UUID); u != res.UUID {
		t.Errorf("the placeholder's page leads to %q, want %q", u, res.UUID)
	}
	if placeholderByUUID(other.UUID) == nil {
		t.Error("another placeholder went")
	}

	// Another author's book of the same title: left alone.
	res = uploadFiles(t, "mort.epub", titledEPUB(t, "Mort", "Someone Else", "y"), false)
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM books b JOIN books_series_link l ON l.book = b.id WHERE b.uuid = ?`, res.UUID).Scan(&n)
	if n != 0 || placeholderByUUID(other.UUID) == nil {
		t.Errorf("other author's Mort: %d series, placeholder kept %v", n, placeholderByUUID(other.UUID) != nil)
	}

	// A book with its own series keeps it.
	m := OPFMetadata{Title: "Mort", Creators: []string{"Terry Pratchett"}, Series: "Mine", SeriesIndex: "1"}
	if fillFromPlaceholder(&m) != nil || m.Series != "Mine" {
		t.Errorf("own series: %+v", m)
	}
	// Two placeholders in different series could be it: neither is taken.
	save("Mort", "Terry Pratchett", "Death", "1")
	m = OPFMetadata{Title: "Mort", Creators: []string{"Terry Pratchett"}}
	if fillFromPlaceholder(&m) != nil || m.Series != "" {
		t.Errorf("ambiguous: %+v", m)
	}
}

// Queued for import, the review starts with the placeholder's series; the
// placeholder stays until the book is added.
func TestPlaceholderSeriesOnImport(t *testing.T) {
	withTestLibrary(t)
	setupTestAppDB(t)
	origData := dataDir
	dataDir = t.TempDir()
	placeholdersChanged()
	t.Cleanup(func() { dataDir = origData; placeholdersChanged() })
	p := &placeholder{Meta: OPFMetadata{Title: "Small Gods", Creators: []string{"Terry Pratchett"}, Series: "Discworld", SeriesIndex: "13"}}
	if err := savePlaceholder(p, nil); err != nil {
		t.Fatal(err)
	}
	w, dir := newTestWatcher(t)
	writeTestFile(t, filepath.Join(dir, "Small Gods.epub"), titledEPUB(t, "Small Gods", "Terry Pratchett", "z"))
	w.scan()
	w.scan()
	items := w.inbox.list()
	if len(items) != 1 {
		t.Fatalf("%d items", len(items))
	}
	if m := items[0].Merged; m.Series != "Discworld" || m.SeriesIndex != "13" {
		t.Errorf("queued with series %q #%s", m.Series, m.SeriesIndex)
	}
	if placeholderByUUID(p.UUID) == nil {
		t.Error("queuing deleted the placeholder")
	}
}

// The placeholder's metadata is a source in the editor: in the import
// review, and for the book an upload added (though the placeholder is gone).
func TestPlaceholderAsSource(t *testing.T) {
	withTestLibrary(t)
	setupTestAppDB(t)
	origData := dataDir
	dataDir = t.TempDir()
	placeholdersChanged()
	t.Cleanup(func() { dataDir = origData; placeholdersChanged() })
	save := func(title, series, index, desc string) *placeholder {
		t.Helper()
		p := &placeholder{Meta: OPFMetadata{Title: title, Creators: []string{"Terry Pratchett"}, Series: series, SeriesIndex: index, Description: desc}}
		if err := savePlaceholder(p, nil); err != nil {
			t.Fatal(err)
		}
		return p
	}
	save("Small Gods", "Discworld", "13", "Om, as a tortoise.")
	save("Eric", "Discworld", "9", "Faust, briefly.")

	// The import review.
	w, dir := newTestWatcher(t)
	writeTestFile(t, filepath.Join(dir, "Small Gods.epub"), titledEPUB(t, "Small Gods", "Terry Pratchett", "z"))
	w.scan()
	w.scan()
	it := w.inbox.list()[0]
	srcs := importSources(it)
	last := srcs[len(srcs)-1]
	if last.Format != "Placeholder" || last.Description != "Om, as a tortoise." || last.Series != "Discworld" || last.SeriesIndex != "13" {
		t.Errorf("review sources end with %+v", last)
	}

	// An upload: the book's editor offers it, the placeholder deleted.
	res := uploadFiles(t, "eric.epub", titledEPUB(t, "Eric", "Terry Pratchett", "e"), false)
	if res.UUID == "" || res.SeriesFromPlaceholder != "Discworld #9" {
		t.Fatalf("upload: %+v", res)
	}
	v, _ := bookMetadataCache.Load(res.UUID)
	cached, _ := v.([]*UploadedBookMeta)
	var found bool
	for _, m := range cached {
		found = found || m.Format == "Placeholder" && m.Description == "Faust, briefly."
	}
	if !found {
		t.Errorf("editor sources %+v", cached)
	}
}
