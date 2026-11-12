package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// addFormat gives a test book another file.
func addFormat(t *testing.T, b testBook, title, ext, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(baseDir, b.path, title+"."+ext), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := calibreDB.Exec(`INSERT INTO data (book, format, uncompressed_size, name) VALUES (?, ?, ?, ?)`, b.id, strings.ToUpper(ext), len(body), title); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestMergeBooks(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, []string{"a-tag"}, []string{"eng"})
	b := addOPFTestBook(t, db, "Alpha (pdf)", []string{"Ann"}, []string{"b-tag"}, nil)
	addFormat(t, b, "Alpha (pdf)", "pdf", "PDF!")
	if err := os.WriteFile(filepath.Join(baseDir, b.path, "Alpha (pdf).epub"), []byte("BETTER EPUB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, b.path, "cover.jpg"), []byte("b cover"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := addOPFTestBook(t, db, "Other", []string{"Zed"}, nil, nil)

	// Readers' data on the merged-away book.
	if _, err := appDB.Exec(`INSERT INTO users (id, username, created_at) VALUES (7, 'reader', 0)`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO favorites (user_id, book_uuid, created_at) VALUES (7, '` + b.uuid + `', 1)`,
		`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (7, '` + a.uuid + `', 'wanted', 'manual', 1)`,
		`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (7, '` + b.uuid + `', 'finished', 'manual', 5)`,
		`INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at) VALUES (7, '` + b.uuid + `', 'pdf', '12', 0.5, 9)`,
		`INSERT INTO bookmarks (user_id, book_uuid, client_id, format, position, created_at) VALUES (7, '` + b.uuid + `', 'bm1', 'pdf', '3', 1)`,
	} {
		if _, err := appDB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	req := &mergeRequest{
		ApplyMetadataRequest: ApplyMetadataRequest{Title: "Alpha", Creators: "Ann", Subjects: "a-tag, b-tag", Date: "2001-01-01"},
		Keep:                 a.uuid,
		UUIDs:                []string{a.uuid, b.uuid},
		Formats:              map[string]string{"epub": b.uuid}, // B's EPUB over A's
		Cover:                b.uuid,
	}
	res, err := mergeBooks(req, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if res.UUID != a.uuid || res.Merged != 1 {
		t.Errorf("result %+v", res)
	}

	dir := filepath.Join(baseDir, a.path)
	if got := readFile(t, filepath.Join(dir, "Alpha.epub")); got != "BETTER EPUB" {
		t.Errorf("Alpha.epub = %q, want B's", got)
	}
	if got := readFile(t, filepath.Join(dir, "Alpha.pdf")); got != "PDF!" {
		t.Errorf("Alpha.pdf = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "cover.jpg")); got != "b cover" {
		t.Errorf("cover = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "cover_backup.jpg")); got != "jpg" {
		t.Errorf("cover backup = %q", got)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".cf-merge-") {
			t.Errorf("temporary file left: %s", e.Name())
		}
	}
	formats, _ := bookFormats(a.id)
	if !slices.Equal(formats, []string{"epub", "pdf"}) {
		t.Errorf("formats %v", formats)
	}
	if n := count(t, db, `SELECT uncompressed_size FROM data WHERE book = ? AND format = 'EPUB'`, a.id); n != len("BETTER EPUB") {
		t.Errorf("epub size %d", n)
	}
	row := tableRow(t, db, a.uuid)
	if !slices.Equal(row.Tags, []string{"a-tag", "b-tag"}) && !slices.Equal(row.Tags, []string{"b-tag", "a-tag"}) {
		t.Errorf("tags %v", row.Tags)
	}
	if row.PubDate != "2001-01-01" {
		t.Errorf("pubdate %q", row.PubDate)
	}

	// B is in the Recycling Bin, whole; A's replaced EPUB is in the batch.
	if n := count(t, db, `SELECT COUNT(*) FROM books WHERE uuid = ?`, b.uuid); n != 0 {
		t.Error("merged book still in the library")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books WHERE uuid = ?`, other.uuid); n != 1 {
		t.Error("an unrelated book went")
	}
	batch, err := loadRecycleBatch(res.Batch)
	if err != nil {
		t.Fatal(err)
	}
	if batch.MergedInto == nil || batch.MergedInto.UUID != a.uuid || !slices.Equal(batch.MergedInto.Replaced, []string{"Alpha.epub"}) {
		t.Errorf("batch note %+v", batch.MergedInto)
	}
	if got := readFile(t, filepath.Join(batch.dir(), recycleReplacedDir, "Alpha.epub")); got != "EPUB!" {
		t.Errorf("replaced file = %q", got)
	}
	if got := readFile(t, filepath.Join(batch.bookDir(b.id), "Alpha (pdf).pdf")); got != "PDF!" {
		t.Errorf("recycled book's pdf = %q", got)
	}

	// Readers' data was copied to A; the newer status wins.
	if n := count(t, appDB, `SELECT COUNT(*) FROM favorites WHERE user_id = 7 AND book_uuid = ?`, a.uuid); n != 1 {
		t.Error("favorite not copied")
	}
	var status string
	if err := appDB.QueryRow(`SELECT status FROM reading_status WHERE user_id = 7 AND book_uuid = ?`, a.uuid).Scan(&status); err != nil || status != "finished" {
		t.Errorf("status %q, %v", status, err)
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM reading_progress WHERE user_id = 7 AND book_uuid = ? AND format = 'pdf'`, a.uuid); n != 1 {
		t.Error("place not copied")
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM bookmarks WHERE user_id = 7 AND book_uuid = ?`, a.uuid); n != 1 {
		t.Error("bookmark not copied")
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM bookmarks WHERE user_id = 7 AND book_uuid = ?`, b.uuid); n != 1 {
		t.Error("the original bookmark should stay, for a restore")
	}

	// B can be restored as it was.
	if _, err := restoreRecycled(res.Batch, []string{b.uuid}, "admin"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books WHERE uuid = ?`, b.uuid); n != 1 {
		t.Error("not restored")
	}
}

func TestMergeRefusals(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, nil, nil)
	for name, req := range map[string]*mergeRequest{
		"one book":     {Keep: a.uuid, UUIDs: []string{a.uuid, a.uuid}},
		"keep outside": {Keep: "00000000-0000-0000-0000-000000000000", UUIDs: []string{a.uuid, b.uuid}},
		"missing book": {Keep: a.uuid, UUIDs: []string{a.uuid, "00000000-0000-0000-0000-000000000000"}},
	} {
		if _, err := mergeBooks(req, "admin"); err == nil {
			t.Errorf("%s: merged", name)
		}
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books`); n != 2 {
		t.Errorf("%d books left, want 2", n)
	}
}

func TestMergeSources(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, nil, nil)
	books, err := loadMergeBooks([]string{a.uuid, b.uuid})
	if err != nil {
		t.Fatal(err)
	}
	srcs := mergeSources(books, b.uuid)
	if len(srcs) < 2 || srcs[0].Format != "current" || srcs[0].Title != "Beta" || srcs[1].Format != "A" || srcs[1].Title != "Alpha" {
		t.Errorf("sources %+v", srcs)
	}
	d := newMergePageData(books, a.uuid, "")
	if len(d.Formats) != 1 || d.Formats[0].Ext != "epub" || len(d.Formats[0].Choices) != 2 || !d.Formats[0].Choices[0].Chosen {
		t.Errorf("formats %+v", d.Formats)
	}
}

// Files of one format: identical ones are said to be, different ones can be
// downloaded to compare.
func TestMergeFormatsCompared(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, nil, nil)
	c := addOPFTestBook(t, db, "Gamma", []string{"Cat"}, nil, nil)
	addFormat(t, a, "Alpha", "pdf", "same pdf")
	addFormat(t, b, "Beta", "pdf", "same pdf")
	if err := os.WriteFile(filepath.Join(baseDir, c.path, "Gamma.epub"), []byte("another EPUB"), 0o644); err != nil {
		t.Fatal(err)
	}
	books, err := loadMergeBooks([]string{a.uuid, b.uuid, c.uuid})
	if err != nil {
		t.Fatal(err)
	}
	d := newMergePageData(books, a.uuid, "")
	byExt := map[string]mergeFormatView{}
	for _, v := range d.Formats {
		byExt[v.Ext] = v
	}
	// EPUB: A's and B's are "EPUB!", C's differs.
	epub := byExt["epub"]
	if epub.Identical || !epub.Different() || len(epub.Choices) != 3 {
		t.Fatalf("epub %+v", epub)
	}
	if epub.Choices[0].SameAs != "" || epub.Choices[1].SameAs != "A" || epub.Choices[2].SameAs != "" {
		t.Errorf("epub same-as %+v", epub.Choices)
	}
	if pdf := byExt["pdf"]; !pdf.Identical || pdf.Different() {
		t.Errorf("pdf %+v", pdf)
	}
}
