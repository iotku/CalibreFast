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

// acceptAllResult is POST /api/imports/accept-all (TS: AcceptAllResult).
type acceptAllResult struct {
	Added int           `json:"added"`
	Left  []acceptedNot `json:"left"` // still waiting, and why
}

type acceptedNot struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

func importAcceptAllHandler(w http.ResponseWriter, r *http.Request) {
	by := currentUser(r)
	res := acceptAllResult{Left: []acceptedNot{}}
	var added []string
	for _, it := range imports.list() {
		uuid, reason := acceptImport(it, by)
		if reason != "" {
			res.Left = append(res.Left, acceptedNot{it.ID, it.Merged.Title, reason})
			continue
		}
		if uuid != "" {
			added = append(added, uuid)
		}
	}
	res.Added = len(added)
	if len(added) > 0 {
		catalogChanged(added...)
		refreshLibraryPages()
		refreshAllCaches()
		log.Printf("imports: %d books mass-imported by %s", len(added), by.Username)
	}
	writeJSON(w, http.StatusOK, res)
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
