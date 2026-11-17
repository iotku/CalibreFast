package main

// Mass import (/import, admins only): every waiting book is added with the
// metadata its files offered, unreviewed. The admin fixes what's wrong
// later. Covers: the page has already given each book without one its
// EPUB's cover or its PDF's first page (web/imports.ts), and the server
// tries its own files once more (import_covers.go).
//
// Nothing is forced: a book the library may already have (an identical
// file, or the same title and authors), new files for a library book
// (reviewed against it), or one that fails to add stays in the queue with
// the reason, for a review.

import (
	"database/sql"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"strings"

	"github.com/iotku/CalibreFast/pkg/opf"
)

// importAcceptHandler is POST /import/{id}/accept: one book of a mass
// import, which the page sends one at a time to show its progress. The
// library's pages aren't rewritten for each (hundreds may follow): the
// page asks for that every so often, and at the end (importAcceptDoneHandler).
// reason says why it's still waiting ("" once added).
func importAcceptHandler(w http.ResponseWriter, r *http.Request) {
	it := importFromRequest(w, r)
	if it == nil {
		return
	}
	uuid, reason := acceptImport(it, currentUser(r))
	if uuid != "" {
		catalogChanged(uuid)
		log.Printf("imports: %q mass-imported by %s", it.Merged.Title, currentUser(r).Username)
	}
	writeJSON(w, http.StatusOK, map[string]string{"uuid": uuid, "reason": reason})
}

// importAcceptDoneHandler is POST /api/imports/accept-done: the books
// mass-imported so far show on the library's pages.
func importAcceptDoneHandler(w http.ResponseWriter, r *http.Request) {
	refreshLibraryPages()
	refreshAllCaches()
	w.WriteHeader(http.StatusNoContent)
}

// acceptImport adds one waiting book as its files describe it. reason says
// why it wasn't ("" and no uuid: someone else took it meanwhile).
func acceptImport(it *pendingImport, by *User) (uuid, reason string) {
	if it.Into != "" {
		return "", "new files for a library book: review them against it"
	}
	meta := it.Merged.OPFMetadata
	if _, _, err := opf.CleanSeries(meta.Series, meta.SeriesIndex); err != nil {
		return "", err.Error()
	}
	extractImportCover(imports.itemDir(it.ID), it.Files)
	uuid, err := imports.approve(it.ID, meta, false)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ""
	}
	if dup, ok := errors.AsType[*duplicateError](err); ok {
		return "", dup.Error()
	}
	if err != nil {
		log.Printf("imports: mass import of %q: %v", it.Merged.Title, err)
		return "", "couldn't add it: " + strings.TrimSpace(err.Error())
	}
	logBookEdit(by, "import", uuid, nil)
	if it.Placeholder != "" {
		p := placeholderByUUID(it.Placeholder)
		if err := deletePlaceholder(it.Placeholder); err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("imports: placeholder %s: %v", it.Placeholder, err)
		} else if p != nil {
			logEdit(by, "placeholder added", logKindPlaceholder, p.UUID, p.Meta.Title, snapPlaceholder(p), nil)
		}
	}
	return uuid, ""
}
