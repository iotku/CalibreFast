package main

// Syncing a placeholder to Hardcover: a user who Saved a placeholder can
// send it to their Hardcover account as want to read, like any book they
// share (hardcover_sync.go). Hardcover knows it by the placeholder's own
// identifiers (hardcover-id, hardcover slug, ISBN), which are the
// library's, not the user's: the first user to find it on Hardcover (by
// its ISBN when they sync, or picking it from a search) gives the
// placeholder its hardcover-id and slug, for everyone. Once it has them,
// only an editor changes them: from the same search, or the Hardcover
// field of the placeholder's editor. When a library book replaces the
// placeholder, its sharing moves to the book (hcPlaceholderReplaced).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/isbn"
)

// hcPlaceholderMu: claiming a placeholder's Hardcover book is a read, a
// check and a write; two users finding it at once mustn't both win.
var hcPlaceholderMu sync.Mutex

// errHCPlaceholderSet: the placeholder has a Hardcover book already, and
// only an editor may change it.
var errHCPlaceholderSet = errors.New("this placeholder already has a Hardcover book; an editor can change it")

// hcSyncable: a book that can be sent to Hardcover: one in the library,
// or a placeholder.
func hcSyncable(uuid string) bool {
	return uuid != "" && (placeholderByUUID(uuid) != nil || bookExists(uuid))
}

// hcPlaceholderBookID: the Hardcover book id placeholder p is known by, 0
// for none.
func hcPlaceholderBookID(p *placeholder) int64 {
	id, err := strconv.ParseInt(identifierValue(p.Meta, hardcover.IDScheme), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// hcPlaceholderKeys: what Hardcover knows a placeholder by (hcBookKeys).
func hcPlaceholderKeys(p *placeholder) (isbns []string, hcID int64, slug string) {
	for _, id := range p.Meta.Identifiers {
		if strings.EqualFold(id.Scheme, "isbn") {
			if n := isbn.Normalize(id.Value); n != "" {
				isbns = append(isbns, n)
			}
		}
	}
	if s := identifierValue(p.Meta, hardcover.SlugScheme); hardcover.ValidSlug(s) {
		slug = s
	}
	return isbns, hcPlaceholderBookID(p), slug
}

// hcEditionIdentifiers: the identifiers that name Hardcover book ed.
func hcEditionIdentifiers(ed hardcover.Edition) []opfIdentifier {
	ids := []opfIdentifier{{Scheme: hardcover.IDScheme, Value: strconv.FormatInt(ed.BookID, 10)}}
	if hardcover.ValidSlug(ed.Slug) {
		ids = append(ids, opfIdentifier{Scheme: hardcover.SlugScheme, Value: ed.Slug})
	}
	return ids
}

// withoutHCIdentifiers: ids less the Hardcover ones.
func withoutHCIdentifiers(ids []opfIdentifier) []opfIdentifier {
	var out []opfIdentifier
	for _, id := range ids {
		if !strings.EqualFold(id.Scheme, hardcover.IDScheme) && !strings.EqualFold(id.Scheme, hardcover.SlugScheme) {
			out = append(out, id)
		}
	}
	return out
}

// hcSetPlaceholderBook makes placeholder id Hardcover book ed: its
// hardcover-id and slug, in place of any it had. With onlyIfUnset (a
// user who isn't an editor), a placeholder that has a Hardcover book
// already keeps it (errHCPlaceholderSet). The change is logged as by
// `by`, and every user syncing the placeholder but except has it sent
// again as the new book.
func hcSetPlaceholderBook(id string, ed hardcover.Edition, by *User, onlyIfUnset bool, except int64) error {
	if ed.BookID <= 0 {
		return fmt.Errorf("no Hardcover book")
	}
	hcPlaceholderMu.Lock()
	cur := placeholderByUUID(id)
	if cur == nil {
		hcPlaceholderMu.Unlock()
		return fmt.Errorf("no such placeholder")
	}
	if have := hcPlaceholderBookID(cur); have > 0 && onlyIfUnset {
		hcPlaceholderMu.Unlock()
		if have == ed.BookID {
			return nil
		}
		return errHCPlaceholderSet
	}
	if hcPlaceholderBookID(cur) == ed.BookID && identifierValue(cur.Meta, hardcover.SlugScheme) == ed.Slug {
		hcPlaceholderMu.Unlock()
		return nil
	}
	p := *cur // the cache is shared: change a copy
	before := snapPlaceholder(&p)
	p.Meta.Identifiers = append(withoutHCIdentifiers(p.Meta.Identifiers), hcEditionIdentifiers(ed)...)
	err := savePlaceholder(&p, nil)
	hcPlaceholderMu.Unlock()
	if err != nil {
		return err
	}
	logEdit(by, "edit placeholder", logKindPlaceholder, p.UUID, p.Meta.Title, before, snapPlaceholder(&p))
	hcPlaceholderBookChanged(id, &ed, except)
	return nil
}

// hcPlaceholderBookChanged: placeholder id is now Hardcover book ed (nil:
// not known). What was looked up for it is forgotten (or is ed), and every
// user syncing it but except has it sent again, a "not found" included.
func hcPlaceholderBookChanged(id string, ed *hardcover.Edition, except int64) {
	if appDB == nil {
		return
	}
	if ed != nil {
		_, _ = appDB.Exec(`INSERT INTO hardcover_books (book_uuid, hc_book_id, hc_edition_id, pages, slug, title, resolved_at) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (book_uuid) DO UPDATE SET hc_book_id = excluded.hc_book_id, hc_edition_id = excluded.hc_edition_id,
				pages = excluded.pages, slug = excluded.slug, title = excluded.title, resolved_at = excluded.resolved_at`,
			id, ed.BookID, ed.EditionID, ed.Pages, ed.Slug, ed.Title, hcNow().Unix())
	} else {
		_, _ = appDB.Exec(`DELETE FROM hardcover_books WHERE book_uuid = ?`, id)
	}
	rows, err := appDB.Query(`SELECT user_id FROM reading_status WHERE book_uuid = ? AND user_id != ?`, id, except)
	if err != nil {
		log.Println("hardcover placeholder:", err)
		return
	}
	var users []int64
	for rows.Next() {
		var u int64
		if rows.Scan(&u) == nil {
			users = append(users, u)
		}
	}
	rows.Close()
	for _, u := range users {
		hcResend(u, id) // only for those linked who share it (hcSyncMark)
	}
}

// hcClaimPlaceholder: the sync found placeholder id on Hardcover as ed
// (from its ISBN or slug). A placeholder with no hardcover-id takes it.
func hcClaimPlaceholder(id string, ed hardcover.Edition, userID int64) {
	p := placeholderByUUID(id)
	if p == nil || hcPlaceholderBookID(p) > 0 || ed.BookID <= 0 {
		return
	}
	by, _ := getUserByID(userID)
	if err := hcSetPlaceholderBook(id, ed, by, true, userID); err != nil && !errors.Is(err, errHCPlaceholderSet) {
		log.Printf("hardcover placeholder %s: %v", id, err)
	}
}

// hcPlaceholderFieldIDs: the Hardcover identifiers for what an editor
// typed in a placeholder's Hardcover field: a book id or its
// hardcover.app link. "" clears them. With the server's Hardcover token,
// Hardcover is asked, so a link gets its id and an id its slug, and a
// book it hasn't is refused; ed is then what it said.
func hcPlaceholderFieldIDs(ctx context.Context, field string) (ids []opfIdentifier, ed *hardcover.Edition, err error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return nil, nil, nil
	}
	slug, id, ok := hcLinkTarget(field)
	if !ok {
		return nil, nil, fmt.Errorf("Hardcover: give the book's id, or its link on hardcover.app")
	}
	if hardcoverEnabled() {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		found, ok, err := hcClient.FindEdition(ctx, nil, id, slug)
		if err != nil {
			log.Println("hardcover placeholder:", err)
			return nil, nil, fmt.Errorf("couldn't ask Hardcover about that book; try again later")
		}
		if !ok {
			return nil, nil, fmt.Errorf("Hardcover has no such book")
		}
		return hcEditionIdentifiers(found), &found, nil
	}
	if id > 0 {
		return []opfIdentifier{{Scheme: hardcover.IDScheme, Value: strconv.FormatInt(id, 10)}}, nil, nil
	}
	return []opfIdentifier{{Scheme: hardcover.SlugScheme, Value: slug}}, nil, nil
}

// hcPlaceholderField: what the editor's Hardcover field starts with.
func hcPlaceholderField(m OPFMetadata) string {
	if s := identifierValue(m, hardcover.SlugScheme); hardcover.ValidSlug(s) {
		return "https://hardcover.app/books/" + s
	}
	return identifierValue(m, hardcover.IDScheme)
}

// hcSameIdentifiers: a and b name the same Hardcover book the same way.
func hcSameIdentifiers(a, b OPFMetadata) bool {
	return identifierValue(a, hardcover.IDScheme) == identifierValue(b, hardcover.IDScheme) &&
		identifierValue(a, hardcover.SlugScheme) == identifierValue(b, hardcover.SlugScheme)
}

// hcPlaceholderReplaced: library book bookUUID has replaced placeholder
// id. Whoever shared the placeholder with Hardcover shares the book (and
// one who left it out of sharing every book leaves the book out), and
// what was kept about the placeholder's sync goes. The book is sent as it
// stands (its status: the Saved moved to it by notifications.go).
func hcPlaceholderReplaced(id, bookUUID string) {
	if appDB == nil || id == "" || bookUUID == "" {
		return
	}
	var users []int64
	rows, err := appDB.Query(`SELECT user_id FROM hardcover_shared WHERE book_uuid = ?`, id)
	if err == nil {
		for rows.Next() {
			var u int64
			if rows.Scan(&u) == nil {
				users = append(users, u)
			}
		}
		rows.Close()
	}
	for _, q := range []string{
		`INSERT OR IGNORE INTO hardcover_shared (user_id, book_uuid, shared_at) SELECT user_id, ?1, shared_at FROM hardcover_shared WHERE book_uuid = ?2`,
		`INSERT OR IGNORE INTO hardcover_unshared (user_id, book_uuid, unshared_at) SELECT x.user_id, ?1, x.unshared_at FROM hardcover_unshared x
			WHERE x.book_uuid = ?2 AND NOT EXISTS (SELECT 1 FROM hardcover_shared s WHERE s.user_id = x.user_id AND s.book_uuid = ?1)`,
		`DELETE FROM hardcover_shared WHERE book_uuid = ?2`,
		`DELETE FROM hardcover_unshared WHERE book_uuid = ?2`,
		`DELETE FROM hardcover_sync WHERE book_uuid = ?2`,
		`DELETE FROM hardcover_books WHERE book_uuid = ?2`,
		`DELETE FROM hardcover_user_books WHERE book_uuid = ?2`,
	} {
		if _, err := appDB.Exec(q, bookUUID, id); err != nil {
			log.Printf("hardcover placeholder %s: %v", id, err)
		}
	}
	// And those who share every book, with a status for it now.
	if rows, err := appDB.Query(`SELECT user_id FROM reading_status WHERE book_uuid = ?`, bookUUID); err == nil {
		for rows.Next() {
			var u int64
			if rows.Scan(&u) == nil {
				users = append(users, u)
			}
		}
		rows.Close()
	}
	for _, u := range users {
		hcSyncTouch(u, bookUUID, 0)
	}
}

// requireSyncBook: the {uuid} of a book or placeholder; else 404.
func requireSyncBook(w http.ResponseWriter, r *http.Request) (string, bool) {
	uuid := r.PathValue("uuid")
	if !hcSyncable(uuid) {
		http.Error(w, "no such book", http.StatusNotFound)
		return "", false
	}
	return uuid, true
}
