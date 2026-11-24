package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/iotku/CalibreFast/internal/hardcoversync"
	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// A Saved placeholder is sent as want to read. The first to find it on
// Hardcover names its Hardcover book for everyone; after that only an
// editor changes it. Replaced by a library book, its sharing moves there.
func TestHardcoverPlaceholderSync(t *testing.T) {
	u, books, f := setupHardcoverSync(t)
	origData, origWrites := dataDir, allowWrites
	dataDir, allowWrites = t.TempDir(), true
	placeholdersChanged()
	t.Cleanup(func() { dataDir, allowWrites = origData, origWrites; placeholdersChanged() })
	linkHardcover(t, u)
	mux := http.NewServeMux()
	registerHardcoverSyncRoutes(mux)
	do := func(method, path, body string, want int) []byte {
		t.Helper()
		rec := call(t, mux, u, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s (want %d)", method, path, rec.Code, rec.Body, want)
		}
		return rec.Body.Bytes()
	}
	state := func(id string) hardcoversync.BookStatus {
		t.Helper()
		var st hardcoversync.BookStatus
		if err := json.Unmarshal(do("GET", "/api/me/books/"+id+"/hardcover", "", 200), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	// No ISBN: shared and Saved, Hardcover can't find it.
	p := &placeholder{Meta: OPFMetadata{Title: "Coming Soon", Creators: []string{"A. Writer"}}}
	if err := savePlaceholder(p, nil); err != nil {
		t.Fatal(err)
	}
	base := "/api/me/books/" + p.UUID + "/hardcover"
	if st := state(p.UUID); !st.Placeholder || !st.CanChoose || st.State != "off" {
		t.Fatalf("before sharing: %+v", st)
	}
	do("PUT", base, `{"share":true}`, 200)
	if st := state(p.UUID); st.State != "no_status" {
		t.Errorf("shared, not saved: %+v", st)
	}
	if err := setStatus(u.ID, p.UUID, "wanted", "manual"); err != nil {
		t.Fatal(err)
	}
	step(t)
	if st := state(p.UUID); st.State != "not_found" || st.Match != nil || st.Query != "Coming Soon A. Writer" {
		t.Fatalf("not found: %+v", st)
	}
	if rec := call(t, mux, u, "DELETE", base+"/match", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("reset on a placeholder: %d", rec.Code)
	}

	// The first to choose names it, for the placeholder, not just for them.
	do("PUT", base+"/match", `{"book_id":60}`, 200)
	ph := placeholderByUUID(p.UUID)
	if identifierValue(ph.Meta, hardcover.IDScheme) != "60" || identifierValue(ph.Meta, hardcover.SlugScheme) != "the-right-book" {
		t.Fatalf("placeholder's identifiers: %+v", ph.Meta.Identifiers)
	}
	if _, ok := hcSync.UserMatch(u.ID, p.UUID); ok {
		t.Error("stored as the user's own match")
	}
	step(t)
	st := state(p.UUID)
	if st.State != "sent" || st.Match == nil || st.Match.BookID != 60 || st.Match.Chosen || st.CanChoose {
		t.Fatalf("after choosing: %+v %+v", st, st.Match)
	}
	if b := f.books[60]; b == nil || b.status != hardcover.StatusWantToRead {
		t.Fatalf("not sent as want to read: %+v", f.books[60])
	}

	// Now only an editor changes it.
	do("PUT", base+"/match", `{"book_id":60}`, http.StatusForbidden)
	editor := *u
	editor.IsEditor = true
	if rec := call(t, mux, &editor, "PUT", base+"/match", `{"book_id":60}`); rec.Code != 200 {
		t.Errorf("editor: %d %s", rec.Code, rec.Body)
	}
	if err := hcSetPlaceholderBook(p.UUID, hardcover.Edition{BookID: 61}, u, true, 0); err != errPlaceholderSyncSet {
		t.Errorf("a second claim: %v", err)
	}

	// With an ISBN Hardcover knows: found when sent, and the placeholder
	// takes the Hardcover book it was found as.
	q := &placeholder{Meta: OPFMetadata{Title: "Has ISBN", Identifiers: []opfIdentifier{{Scheme: "ISBN", Value: "9780316129084"}}}}
	if err := savePlaceholder(q, nil); err != nil {
		t.Fatal(err)
	}
	do("PUT", "/api/me/books/"+q.UUID+"/hardcover", `{"share":true}`, 200)
	if err := setStatus(u.ID, q.UUID, "wanted", "manual"); err != nil {
		t.Fatal(err)
	}
	step(t)
	if got := identifierValue(placeholderByUUID(q.UUID).Meta, hardcover.IDScheme); got != "50" {
		t.Errorf("found by ISBN: hardcover-id %q", got)
	}
	if b := f.books[50]; b == nil || b.status != hardcover.StatusWantToRead || b.edition != 501 {
		t.Errorf("by ISBN: %+v", f.books[50])
	}

	// A library book replaces it: the sharing (and the Saved) move there.
	if err := replacePlaceholder(q.UUID, books[1]); err != nil {
		t.Fatal(err)
	}
	if !hcSync.IsBookShared(u.ID, books[1]) || hcSync.IsBookShared(u.ID, q.UUID) {
		t.Errorf("sharing didn't move: book %v, placeholder %v", hcSync.IsBookShared(u.ID, books[1]), hcSync.IsBookShared(u.ID, q.UUID))
	}
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_sync WHERE book_uuid = ?`, q.UUID).Scan(&n)
	if n != 0 {
		t.Errorf("%d sync rows left for the placeholder", n)
	}
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_sync WHERE book_uuid = ? AND due_at > 0`, books[1]).Scan(&n)
	if n != 1 {
		t.Errorf("the book isn't queued: %d", n)
	}
}

// An editor sets a placeholder's Hardcover book in its editor: an id or a
// hardcover.app link; empty clears it, anything else is refused.
func TestPlaceholderHardcoverField(t *testing.T) {
	setupLibraryAndUser(t, 1)
	origData, origWrites, origClient := dataDir, allowWrites, hcClient
	dataDir, allowWrites, hcClient = t.TempDir(), true, nil // not asked: no server token
	placeholdersChanged()
	t.Cleanup(func() { dataDir, allowWrites, hcClient = origData, origWrites, origClient; placeholdersChanged() })
	admin, _ := createUser("boss", "a long enough password", true)
	mux := placeholderMux()

	p := &placeholder{Meta: OPFMetadata{Title: "Later", Identifiers: []opfIdentifier{{Scheme: "ISBN", Value: "9780316129084"}}}}
	if err := savePlaceholder(p, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := appDB.Exec(`INSERT INTO hardcover_books (book_uuid, hc_book_id, hc_edition_id, pages, slug, title, resolved_at) VALUES (?, 5, 0, 0, '', '', 1)`, p.UUID); err != nil {
		t.Fatal(err)
	}
	put := func(field string, want int) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"title": "Later", "isbn": "9780316129084", "hardcover": field})
		if rec := call(t, mux, admin, "PUT", "/api/placeholders/"+p.UUID, string(body)); rec.Code != want {
			t.Fatalf("%q: %d %s", field, rec.Code, rec.Body)
		}
	}
	put("60", 200)
	m := placeholderByUUID(p.UUID).Meta
	if identifierValue(m, hardcover.IDScheme) != "60" || placeholderISBN(m) == "" {
		t.Fatalf("by id: %+v", m.Identifiers)
	}
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_books WHERE book_uuid = ?`, p.UUID).Scan(&n)
	if n != 0 {
		t.Error("the old lookup was kept")
	}
	if got := hcPlaceholders.FieldValue(m); got != "60" {
		t.Errorf("field shows %q", got)
	}
	put("https://hardcover.app/books/the-right-book", 200)
	m = placeholderByUUID(p.UUID).Meta
	if identifierValue(m, hardcover.SlugScheme) != "the-right-book" || identifierValue(m, hardcover.IDScheme) != "" {
		t.Fatalf("by link: %+v", m.Identifiers)
	}
	if got := hcPlaceholders.FieldValue(m); got != "https://hardcover.app/books/the-right-book" {
		t.Errorf("field shows %q", got)
	}
	for _, bad := range []string{"javascript:alert(1)", "https://evil.example/books/x", "-3"} {
		put(bad, http.StatusBadRequest)
	}
	put("", 200)
	if ids := hcIdentifiers(placeholderByUUID(p.UUID).Meta); len(ids) != 0 {
		t.Errorf("not cleared: %+v", ids)
	}
	// A form without the field leaves them alone.
	put("60", 200)
	if rec := call(t, mux, admin, "PUT", "/api/placeholders/"+p.UUID, `{"title":"Later"}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if identifierValue(placeholderByUUID(p.UUID).Meta, hardcover.IDScheme) != "60" {
		t.Error("dropped by a form without the field")
	}
	if _, err := hcPlaceholders.ParseField(context.Background(), strings.Repeat("9", 30)); err == nil {
		t.Error("an id too big was taken")
	}
}
