package main

// Exchanging highlights with KOReader.
//
// KOReader's sync protocol (kosync_server.go) carries reading positions only, but
// KOReader has a file format of its own for moving annotations between
// devices (readerannotation.lua, since 2025): "<book file name>.annotations.lua",
// next to the book's settings or in a folder the user chooses. It writes one
// with "Export annotations" (or on closing a book), and when a book is opened
// it merges such a file in and deletes it.
//
// So there are two endpoints, for a file the user carries across (by hand,
// or with a folder synced to the device):
//
//	GET  /api/me/annotations/{uuid}/koreader?format=epub|pdf   the user's highlights, as that file
//	POST /api/me/annotations/{uuid}/koreader?format=epub|pdf   merges such a file in
//
// The file format and the conversion of positions are in pkg/koreader (an
// EPUB's text model is in its text.go; a PDF's positions are in points on
// both sides). What a file leaves out is never deleted here, and the file
// we write is dated so that KOReader, too, only adds and updates.

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"unicode/utf8"

	"github.com/iotku/CalibreFast/pkg/koreader"
)

const (
	koreaderDevice  = "CalibreFast" // KOReader skips a file with its own device id
	maxKOReaderFile = 8 << 20
)

// koreaderImport is what an import did.
type koreaderImport = koreader.ImportResult

// koreaderOptions are the app's limits and names for KOReader's files.
func koreaderOptions() koreader.Options {
	return koreader.Options{
		DeviceID:     koreaderDevice,
		Source:       "CalibreFast",
		MaxAnchorLen: maxAnchorLen,
		MaxTextLen:   maxAnnotationText,
		MaxLabelLen:  maxLabelLen,
		Colors:       annotationColors,
	}
}

// toKOHighlights converts stored annotations for koreader.File.
func toKOHighlights(list []annotationRec) []koreader.Highlight {
	out := make([]koreader.Highlight, len(list))
	for i, a := range list {
		out[i] = koreader.Highlight{
			ID: a.ID, Anchor: a.Anchor, Text: a.Text, Note: a.Note, Color: a.Color,
			Chapter: a.Chapter, Page: a.Page, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
		}
	}
	return out
}

// fromKOHighlights converts what koreader.Annotations read in a format's
// file into annotations to store.
func fromKOHighlights(format string, list []koreader.Highlight) []annotationRec {
	out := make([]annotationRec, len(list))
	for i, h := range list {
		out[i] = annotationRec{
			ID: h.ID, Format: format, Anchor: h.Anchor, Text: h.Text, Note: h.Note, Color: h.Color,
			Chapter: h.Chapter, Page: h.Page, CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt,
		}
	}
	return out
}

func koreaderExportHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	file, err := bookFilePath(uuid, format)
	if !validFormat(format) || err != nil {
		http.Error(w, "this book has no such format", http.StatusNotFound)
		return
	}
	rows, err := appDB.Query(`SELECT `+annotationCols+` FROM annotations
		WHERE user_id = ? AND book_uuid = ? AND format = ? AND deleted = 0 ORDER BY created_at, client_id`, u.ID, uuid, format)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	var list []annotationRec
	for rows.Next() {
		if a, err := scanAnnotation(rows); err == nil {
			list = append(list, a)
		}
	}
	rows.Close()
	lua, skipped := koreader.File(file, format, toKOHighlights(list), koreaderOptions())
	if lua == "" {
		http.Error(w, "could not read the book", http.StatusInternalServerError)
		return
	}
	// KOReader looks for exactly "<book file name>.annotations.lua".
	name := filepath.Base(file) + ".annotations.lua"
	w.Header().Set("Content-Type", "text/x-lua; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Annotations-Exported", fmt.Sprint(len(list)-skipped))
	w.Header().Set("X-Annotations-Skipped", fmt.Sprint(skipped))
	_, _ = io.WriteString(w, lua)
}

// ---- import ----------------------------------------------------------------------

func koreaderImportHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	file, err := bookFilePath(uuid, format)
	if !validFormat(format) || err != nil {
		http.Error(w, "this book has no such format", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxKOReaderFile))
	if err != nil {
		http.Error(w, "the file is too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !utf8.Valid(body) {
		http.Error(w, "not a KOReader annotations file", http.StatusBadRequest)
		return
	}
	table, err := koreader.ParseLuaTable(string(body))
	if err != nil {
		http.Error(w, "could not read the file: "+err.Error(), http.StatusBadRequest)
		return
	}
	highlights, res, err := koreader.Annotations(file, format, table, koreaderOptions())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	recs := fromKOHighlights(format, highlights)

	// What is stored already: by id (tombstones too, so a highlight deleted
	// here isn't brought back by the same file), and by place, so one that
	// was made here, exported and comes back from KOReader isn't doubled.
	type stored struct {
		id        string
		updatedAt int64
		deleted   bool
	}
	byID, byAnchor := map[string]stored{}, map[string]stored{}
	live := 0
	rows, err := appDB.Query(`SELECT client_id, anchor, updated_at, deleted FROM annotations
		WHERE user_id = ? AND book_uuid = ? AND format = ?`, u.ID, uuid, format)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var s stored
		var anchor string
		if rows.Scan(&s.id, &anchor, &s.updatedAt, &s.deleted) == nil {
			byID[s.id] = s
			if !s.deleted {
				byAnchor[anchor] = s
				live++
			}
		}
	}
	rows.Close()

	for _, a := range recs {
		have, exists := byID[a.ID]
		if !exists {
			have, exists = byAnchor[a.Anchor]
		}
		switch {
		case !exists:
			if live >= maxAnnotationsBook {
				res.Skipped++
				continue
			}
			live++
			res.Added++
		case a.UpdatedAt > have.updatedAt:
			a.ID = have.id
			res.Updated++
		default:
			res.Unchanged++ // what is here is as new, or was deleted here since
			continue
		}
		if err := storeAnnotation(u.ID, uuid, a.ID, a); err != nil {
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
		byID[a.ID] = stored{a.ID, a.UpdatedAt, false}
		byAnchor[a.Anchor] = byID[a.ID]
	}
	writeJSON(w, http.StatusOK, res)
}
