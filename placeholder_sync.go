package main

// Placeholders on a service users sync their reading to (Hardcover:
// hardcover_placeholder.go). A user who Saved a placeholder can send it to
// their account there like any book they share. The service knows it by
// the placeholder's own identifiers in that service's schemes, which are
// the library's, not the user's: the first user to find it there gives the
// placeholder its identifiers, for everyone. Once it has them, only an
// editor changes them: from the same search, or the service's field in the
// placeholder's editor. When a library book replaces the placeholder, each
// service moves what it kept about it to the book.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// placeholderSync is a sync service, as placeholders see it.
type placeholderSync interface {
	// Schemes are the identifier schemes that name a book there; the
	// first is the one that identifies it (the others only describe it).
	Schemes() []string
	// FieldValue is what the editor's field for it starts with.
	FieldValue(m OPFMetadata) string
	// ParseField is the identifiers for what an editor typed in that
	// field; "" clears them (nil, nil).
	ParseField(ctx context.Context, field string) ([]opfIdentifier, error)
	// IdentityChanged: placeholder uuid's identifiers there changed. What
	// was looked up for it is forgotten, and every user syncing it but
	// except has it sent again.
	IdentityChanged(uuid string, except int64)
	// Replaced: library book bookUUID has replaced placeholder uuid.
	Replaced(uuid, bookUUID string)
}

// placeholderSyncs are the sync services.
var placeholderSyncs = []placeholderSync{hcPlaceholders}

// syncableBook: a book that can be synced: one in the library, or a
// placeholder.
func syncableBook(uuid string) bool {
	return uuid != "" && (placeholderByUUID(uuid) != nil || bookExists(uuid))
}

// requireSyncBook: the {uuid} of a book or placeholder; else 404.
func requireSyncBook(w http.ResponseWriter, r *http.Request) (string, bool) {
	uuid := r.PathValue("uuid")
	if !syncableBook(uuid) {
		http.Error(w, "no such book", http.StatusNotFound)
		return "", false
	}
	return uuid, true
}

// syncRef is what m is known by on s ("" for nothing).
func syncRef(m OPFMetadata, s placeholderSync) string {
	return identifierValue(m, s.Schemes()[0])
}

// withoutSyncIDs: ids less s's.
func withoutSyncIDs(ids []opfIdentifier, s placeholderSync) []opfIdentifier {
	var out []opfIdentifier
	for _, id := range ids {
		if !slices.ContainsFunc(s.Schemes(), func(sc string) bool { return strings.EqualFold(sc, id.Scheme) }) {
			out = append(out, id)
		}
	}
	return out
}

// sameSyncIDs: a and b name the same book on s the same way.
func sameSyncIDs(a, b OPFMetadata, s placeholderSync) bool {
	for _, sc := range s.Schemes() {
		if identifierValue(a, sc) != identifierValue(b, sc) {
			return false
		}
	}
	return true
}

// placeholderSyncMu: claiming a placeholder's book on a service is a read,
// a check and a write; two users finding it at once mustn't both win.
var placeholderSyncMu sync.Mutex

// errPlaceholderSyncSet: the placeholder has its book on the service
// already, and only an editor may change it.
var errPlaceholderSyncSet = errors.New("this placeholder already has its book there; an editor can change it")

// setPlaceholderSyncIDs makes ids (in s's schemes) placeholder uuid's, in
// place of any it had there. With onlyIfUnset (a user who isn't an
// editor), a placeholder already known there keeps what it has
// (errPlaceholderSyncSet; nothing, if it's the same book). The change is
// logged as by `by`. changed: it was saved; the caller tells s
// (IdentityChanged, or what it knows of the book).
func setPlaceholderSyncIDs(uuid string, s placeholderSync, ids []opfIdentifier, by *User, onlyIfUnset bool) (changed bool, err error) {
	placeholderSyncMu.Lock()
	cur := placeholderByUUID(uuid)
	if cur == nil {
		placeholderSyncMu.Unlock()
		return false, errors.New("no such placeholder")
	}
	next := OPFMetadata{Identifiers: ids}
	if have := syncRef(cur.Meta, s); have != "" && onlyIfUnset {
		placeholderSyncMu.Unlock()
		if have == syncRef(next, s) {
			return false, nil
		}
		return false, errPlaceholderSyncSet
	}
	if sameSyncIDs(cur.Meta, next, s) {
		placeholderSyncMu.Unlock()
		return false, nil
	}
	p := *cur // the cache is shared: change a copy
	before := snapPlaceholder(&p)
	p.Meta.Identifiers = append(withoutSyncIDs(p.Meta.Identifiers, s), ids...)
	err = savePlaceholder(&p, nil)
	placeholderSyncMu.Unlock()
	if err != nil {
		return false, err
	}
	logEdit(by, "edit placeholder", logKindPlaceholder, p.UUID, p.Meta.Title, before, snapPlaceholder(&p))
	return true, nil
}

// placeholderSyncField sets m's identifiers on s from the editor's field
// for it (nil: left as they are).
func placeholderSyncField(ctx context.Context, s placeholderSync, m *OPFMetadata, field *string) error {
	if field == nil {
		return nil
	}
	ids, err := s.ParseField(ctx, *field)
	if err != nil {
		return err
	}
	m.Identifiers = append(withoutSyncIDs(m.Identifiers, s), ids...)
	return nil
}

// placeholderReplacedForSyncs tells every service that library book
// bookUUID has replaced placeholder uuid.
func placeholderReplacedForSyncs(uuid, bookUUID string) {
	for _, s := range placeholderSyncs {
		s.Replaced(uuid, bookUUID)
	}
}
