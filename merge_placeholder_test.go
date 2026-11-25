package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// mergeTestPlaceholder saves a placeholder for the merge tests, and has
// reader 7 Save it.
func mergeTestPlaceholder(t *testing.T, m OPFMetadata) *placeholder {
	t.Helper()
	placeholdersChanged()
	t.Cleanup(placeholdersChanged)
	p := &placeholder{Meta: m, Source: "hardcover"}
	if err := savePlaceholder(p, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := appDB.Exec(`INSERT OR IGNORE INTO users (id, username, created_at) VALUES (7, 'reader', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := appDB.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (7, ?, 'wanted', 'manual', 1)`, p.UUID); err != nil {
		t.Fatal(err)
	}
	return p
}

func messiahMeta() OPFMetadata {
	return OPFMetadata{
		Title:       "Dune Messiah",
		Creators:    []string{"Frank Herbert"},
		Series:      "Dune",
		SeriesIndex: "2",
		Description: "The placeholder's description",
		Identifiers: []opfIdentifier{
			{Scheme: hardcover.IDScheme, Value: "1234"},
			{Scheme: hardcover.SlugScheme, Value: "dune-messiah"},
			{Scheme: "ISBN", Value: "9780593098233"},
		},
	}
}

// checkPlaceholderReplaced: p is gone, replaced by book uuid through a
// merge, and its reader's Saved moved to the book.
func checkPlaceholderReplaced(t *testing.T, p *placeholder, uuid string) {
	t.Helper()
	if placeholderByUUID(p.UUID) != nil {
		t.Fatal("the placeholder is still there")
	}
	var by, why string
	if err := appDB.QueryRow(`SELECT replaced_by, replaced_why FROM placeholders WHERE uuid = ?`, p.UUID).Scan(&by, &why); err != nil {
		t.Fatal(err)
	}
	if by != uuid || why != whyMerged {
		t.Errorf("replaced by %q (%q), want %q (%q)", by, why, uuid, whyMerged)
	}
	if got := placeholderReplacedBy(p.UUID); got != uuid {
		t.Errorf("its page leads to %q, want %q", got, uuid)
	}
	var status string
	if err := appDB.QueryRow(`SELECT status FROM reading_status WHERE user_id = 7 AND book_uuid = ?`, uuid).Scan(&status); err != nil || status != "wanted" {
		t.Errorf("the reader's Saved on the book: %q, %v", status, err)
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM reading_status WHERE book_uuid = ?`, p.UUID); n != 0 {
		t.Error("the Saved is still on the placeholder")
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM edit_log WHERE kind = ? AND target = ?`, logKindPlaceholder, p.UUID); n != 1 {
		t.Errorf("%d edit log entries for the placeholder, want 1", n)
	}
}

// One library book merged with a placeholder: the editor starts from the
// book with the placeholder's series, number and Hardcover identifiers;
// the merge edits the book and the book replaces the placeholder.
func TestMergePlaceholderIntoBook(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Dune Messiah", []string{"Frank Herbert"}, []string{"sf"}, []string{"eng"})
	other := addOPFTestBook(t, db, "Other", []string{"Zed"}, nil, nil)
	p := mergeTestPlaceholder(t, messiahMeta())

	books, err := loadMergeBooksMin([]string{a.uuid}, 1)
	if err != nil {
		t.Fatal(err)
	}
	srcs := mergeSourcesWith(books, a.uuid, p)
	if len(srcs) < 2 || srcs[0].Format != "current" || srcs[1].Format != placeholderSourceFormat {
		t.Fatalf("sources %+v", srcs)
	}
	cur := srcs[0]
	if cur.Title != "Dune Messiah" || cur.Series != "Dune" || cur.SeriesIndex != "2" {
		t.Errorf("current: %q in %q #%q", cur.Title, cur.Series, cur.SeriesIndex)
	}
	if cur.Description != "About Dune Messiah" {
		t.Errorf("the book's own description should stay: %q", cur.Description)
	}
	if identifierValue(cur.OPFMetadata, hardcover.IDScheme) != "1234" || identifierValue(cur.OPFMetadata, hardcover.SlugScheme) != "dune-messiah" {
		t.Errorf("current's identifiers %+v lack the placeholder's Hardcover ones", cur.Identifiers)
	}
	if placeholderISBN(cur.OPFMetadata) != "9780593098233" {
		t.Errorf("current's identifiers %+v lack the placeholder's ISBN", cur.Identifiers)
	}

	// Without the placeholder, one book is no merge.
	if _, err := mergeBooks(&mergeRequest{Keep: a.uuid, UUIDs: []string{a.uuid}}, "admin"); err == nil {
		t.Fatal("a single book merged without a placeholder")
	}

	// The form as the editor sends it from current.
	series, index := cur.Series, cur.SeriesIndex
	req := &mergeRequest{
		ApplyMetadataRequest: ApplyMetadataRequest{
			Title: cur.Title, Creators: strings.Join(cur.Creators, " & "), Subjects: "sf", Description: cur.Description,
			Identifiers: identifierList(cur.Identifiers), Series: &series, SeriesIndex: &index,
		},
		Keep:        a.uuid,
		UUIDs:       []string{a.uuid},
		Placeholder: p.UUID,
	}
	admin, _ := createUser("boss", "a long enough password", true)
	res, err := mergeBooksAs(req, admin)
	if err != nil {
		t.Fatal(err)
	}
	if res.UUID != a.uuid || res.Merged != 0 || res.Batch != "" {
		t.Errorf("result %+v", res)
	}
	row := tableRow(t, db, a.uuid)
	if row.Series != "Dune" || row.SeriesIndex != 2 {
		t.Errorf("book's series %q #%v", row.Series, row.SeriesIndex)
	}
	var hcID string
	if err := db.QueryRow(`SELECT val FROM identifiers WHERE book = ? AND type = ?`, a.id, hardcover.IDScheme).Scan(&hcID); err != nil || hcID != "1234" {
		t.Errorf("book's hardcover-id %q, %v", hcID, err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books WHERE uuid IN (?, ?)`, a.uuid, other.uuid); n != 2 {
		t.Errorf("%d of the two books left", n)
	}
	checkPlaceholderReplaced(t, p, a.uuid)
	var who string
	if err := appDB.QueryRow(`SELECT replaced_user FROM placeholders WHERE uuid = ?`, p.UUID).Scan(&who); err != nil || who != "boss" {
		t.Errorf("replaced by user %q, %v", who, err)
	}

	// Merging it again: it's gone, and nothing is merged.
	if _, err := mergeBooksAs(req, admin); !errors.Is(err, errNoSuchPlaceholder) {
		t.Errorf("merging a removed placeholder: %v", err)
	}
}

// Two library books and a placeholder, through the real route: the books
// are merged as any merge is, and the kept one replaces the placeholder.
// Only an editor on a server with -allow-writes may, and only by POST.
func TestMergePlaceholderRoute(t *testing.T) {
	db := setupRecycleLibrary(t)
	orig := allowWrites
	t.Cleanup(func() { allowWrites = orig })
	allowWrites = true
	a := addOPFTestBook(t, db, "Dune Messiah", []string{"Frank Herbert"}, nil, nil)
	b := addOPFTestBook(t, db, "Dune Messiah (pdf)", []string{"Frank Herbert"}, nil, nil)
	addFormat(t, b, "Dune Messiah (pdf)", "pdf", "PDF!")
	p := mergeTestPlaceholder(t, messiahMeta())

	mux := http.NewServeMux()
	withMux(mux, registerMergeRoutes)
	admin, _ := createUser("boss", "a long enough password", true)
	reader, _ := createUser("alice", "a long enough password", false)

	body, _ := json.Marshal(map[string]any{
		"title": "Dune Messiah", "creators": "Frank Herbert", "series": "Dune", "series_index": "2",
		"keep": a.uuid, "uuids": []string{a.uuid, b.uuid}, "placeholder": p.UUID,
	})
	// Refused: not an editor, or not a POST. Nothing changes.
	if rec := call(t, mux, reader, "POST", "/merge/apply", string(body)); rec.Code != http.StatusNotFound {
		t.Errorf("reader: %d", rec.Code)
	}
	if rec := call(t, mux, nil, "POST", "/merge/apply", string(body)); rec.Code != http.StatusNotFound {
		t.Errorf("anonymous: %d", rec.Code)
	}
	if rec := call(t, mux, admin, "GET", "/merge/apply", string(body)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
	allowWrites = false
	if rec := call(t, mux, admin, "POST", "/merge/apply", string(body)); rec.Code != http.StatusForbidden {
		t.Errorf("read-only server: %d", rec.Code)
	}
	allowWrites = true
	if placeholderByUUID(p.UUID) == nil || count(t, db, `SELECT COUNT(*) FROM books`) != 2 {
		t.Fatal("a refused merge changed something")
	}

	// A placeholder that isn't there: 404, nothing merged.
	bad := strings.Replace(string(body), p.UUID, "00000000-0000-0000-0000-000000000000", 1)
	if rec := call(t, mux, admin, "POST", "/merge/apply", bad); rec.Code != http.StatusNotFound {
		t.Errorf("missing placeholder: %d %s", rec.Code, rec.Body)
	}
	if count(t, db, `SELECT COUNT(*) FROM books`) != 2 {
		t.Fatal("merged with a missing placeholder")
	}

	// The page and its sources, with the placeholder.
	q := "?ids=" + a.uuid + "," + b.uuid + "&placeholder=" + p.UUID
	if rec := call(t, mux, admin, "GET", "/book/merge"+q, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-placeholder="`+p.UUID+`"`) {
		t.Errorf("merge page: %d", rec.Code)
	}
	rec := call(t, mux, admin, "GET", "/merge/metadata"+q, "")
	var srcs []UploadedBookMeta
	if err := json.Unmarshal(rec.Body.Bytes(), &srcs); err != nil || len(srcs) < 2 || srcs[1].Format != placeholderSourceFormat {
		t.Errorf("merge sources: %d %v %s", rec.Code, err, rec.Body)
	}

	rec = call(t, mux, admin, "POST", "/merge/apply", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("merge: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		UUID    string
		Merged  int
		Success bool
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.UUID != a.uuid || out.Merged != 1 || !out.Success {
		t.Errorf("answer %s (%v)", rec.Body, err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books WHERE uuid = ?`, b.uuid); n != 0 {
		t.Error("the other book is still in the library")
	}
	if fs, _ := bookFormats(a.id); len(fs) != 2 {
		t.Errorf("kept book's formats %v", fs)
	}
	checkPlaceholderReplaced(t, p, a.uuid)
}

// A placeholder already replaced meanwhile (an edit's match, say) doesn't
// fail a merge that was going to replace it.
func TestResolveMergedPlaceholderGone(t *testing.T) {
	setupRecycleLibrary(t)
	p := mergeTestPlaceholder(t, messiahMeta())
	if err := deletePlaceholder(p.UUID); err != nil {
		t.Fatal(err)
	}
	if err := resolveMergedPlaceholder(p, "some-book", nil); err != nil {
		t.Errorf("already gone: %v", err)
	}
	var by string
	if err := appDB.QueryRow(`SELECT replaced_by FROM placeholders WHERE uuid = ?`, p.UUID).Scan(&by); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if by != "" {
		t.Errorf("a removed placeholder was replaced again, by %q", by)
	}
}
