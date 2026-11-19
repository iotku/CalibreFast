package main

// Quick edits to waiting books (/import/edit): the bulk editor's table
// (templates/library-edit.html, web/library-edit.ts) over the import queue
// instead of the library, so titles, authors, tags or series can be fixed
// for many books before Mass Import or review.
//
// An edit changes a book's Merged metadata in its item.json, the starting
// metadata that the queue's list, its review page and Mass Import all read.
// What each file says (Sources) stays as it was, still offered on the review
// page. Nothing here touches the library: adding the book checks it for
// duplicates as always (approve).

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

func importEditPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "library-edit.html", map[string]any{
		"Title": SiteTitle, "Queue": true,
	}), "render import edit")
}

// importTableHandler is GET /api/imports/table: the waiting books as the
// bulk editor's rows. New files for a library book (Into) aren't listed:
// their metadata is that book's.
func importTableHandler(w http.ResponseWriter, r *http.Request) {
	rows, checking := importTableRows(imports.list())
	if checking {
		imports.checkDuplicatesSoon()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	logErr(json.NewEncoder(w).Encode(map[string]any{"books": rows, "not_duplicates": []any{}}), "encode import table")
}

// importTableRows are the rows for these waiting books; checking is true
// when one of them hasn't been checked for duplicates since the library
// last changed.
func importTableRows(list []*pendingImport) (rows []tableBook, checking bool) {
	rows = make([]tableBook, 0, len(list))
	for i, it := range list {
		if it.Into != "" {
			continue
		}
		row := importTableRow(it)
		row.ID = int64(i + 1) // the queue's order, oldest first
		dup, ok, stale := imports.cachedDuplicates(it)
		switch {
		case !ok:
			row.Library = "checking"
		case dup.SameFile():
			row.Library = "same file"
		case dup.found():
			row.Library = "in library"
		default:
			row.Library = "new"
		}
		checking = checking || !ok || stale
		rows = append(rows, row)
	}
	return rows, checking
}

func importTableRow(it *pendingImport) tableBook {
	m := it.Merged.OPFMetadata
	m.FillSeries() // queued before the editor showed series
	row := tableBook{
		UUID:      it.ID,
		Title:     m.Title,
		Authors:   strings.Join(m.Creators, " & "),
		Series:    m.Series,
		Publisher: m.Publisher,
		PubDate:   tableDate(normalizeDateOnly(m.Date)),
		Added:     it.Added.Local().Format("2006-01-02"),
		Tags:      slices.Clone(m.Subjects),
		Languages: slices.Clone(m.Languages),
		Formats:   []string{},
		ISBN:      placeholderISBN(m),
	}
	if row.PubDate == "" && m.Date != "" && !strings.HasPrefix(normalizeDateOnly(m.Date), "0101-01-01") {
		row.PubDate = m.Date // not a date it can read: shown as it is
	}
	if m.Series != "" {
		row.SeriesIndex, _ = strconv.ParseFloat(m.SeriesIndex, 64)
	}
	if row.Tags == nil {
		row.Tags = []string{}
	}
	if row.Languages == nil {
		row.Languages = []string{}
	}
	for _, f := range it.Files {
		if f := strings.ToLower(f.Format); !slices.Contains(row.Formats, f) {
			row.Formats = append(row.Formats, f)
		}
	}
	slices.Sort(row.Formats)
	row.HasCover, _ = coverFilesIn(imports.itemDir(it.ID))
	return row
}

// errNotWaiting: the book has been added or discarded (or is being).
var errNotWaiting = errors.New("no longer in the queue: added or discarded meanwhile")

// editMerged makes the edit to each waiting book's Merged metadata. Each is
// changed on a copy, written to its item.json, and the copy put in place
// of the old one, all under b.mu, so a book being added or discarded (taken
// out of items first) is never edited, and whoever holds the old one (Mass
// Import reading it) reads it whole. b.mu is let go between books, so the
// queue's list answers during a long edit.
func (b *importInbox) editMerged(ids []string, edit *bulkEdit) (changed []*pendingImport, res bulkResult) {
	res = bulkResult{Failed: []bulkFailure{}, Books: []tableBook{}}
	recheck := false
	for _, id := range ids {
		it, rename, err := b.editOne(id, edit)
		switch {
		case err != nil:
			res.Failed = append(res.Failed, bulkFailure{id, err.Error()})
		case it == nil:
			res.Unchanged++
		default:
			changed = append(changed, it)
			if rename {
				b.forgetDuplicates(id)
				recheck = true
			}
		}
	}
	res.Changed = len(changed)
	if recheck {
		b.checkDuplicatesSoon()
	}
	return changed, res
}

// editOne is editMerged for one book: the new copy (nil if the edit makes
// no difference), and whether its title or authors changed.
func (b *importInbox) editOne(id string, edit *bulkEdit) (*pendingImport, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	it := b.items[id]
	if it == nil {
		return nil, false, errNotWaiting
	}
	if it.Into != "" {
		return nil, false, errors.New("new files for a library book: review them against it")
	}
	next := cloneImport(it)
	m := &next.Merged.OPFMetadata
	m.FillSeries()
	if !edit.apply(m) {
		return nil, false, nil
	}
	if edit.Series != nil || edit.SeriesIndex != nil {
		// approve takes the series from Meta (SeriesSet isn't stored).
		m.SyncSeriesMeta()
	}
	// The folder of the item found, by its own id: never a path made from
	// the request's (which validate checked, and which named a queued book).
	if err := writeItemJSON(b.itemDir(it.ID), next); err != nil {
		log.Printf("imports: editing %s: %v", id, err)
		return nil, false, err
	}
	b.items[id] = next
	rename := m.Title != it.Merged.Title || !slices.Equal(m.Creators, it.Merged.Creators)
	return next, rename, nil
}

// cloneImport is a copy of it whose Merged metadata can be changed without
// touching it: Merged's lists are copied. Files and Sources are shared, as
// nothing changes them once the book is queued.
func cloneImport(it *pendingImport) *pendingImport {
	out := *it
	m := &out.Merged.OPFMetadata
	m.Languages = slices.Clone(m.Languages)
	m.Identifiers = slices.Clone(m.Identifiers)
	m.Subjects = slices.Clone(m.Subjects)
	m.Creators = slices.Clone(m.Creators)
	m.Meta = slices.Clone(m.Meta)
	return &out
}

// importBulkHandler is POST /api/imports/bulk: the bulk editor's request
// (bulkEditRequest, uuids being the queue's ids), answered with the
// changed books' rows.
func importBulkHandler(w http.ResponseWriter, r *http.Request) {
	var req bulkEditRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := req.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	changed, res := imports.editMerged(uniqueStrings(req.UUIDs), &req.Edit)
	if len(changed) > 0 {
		log.Printf("imports: %s edited %d waiting book(s) (%d unchanged, %d failed)", currentUser(r).Username, res.Changed, res.Unchanged, len(res.Failed))
		ids := map[string]bool{}
		for _, it := range changed {
			ids[it.ID] = true
		}
		rows, _ := importTableRows(imports.list()) // numbered in the queue's order
		res.Books = slices.DeleteFunc(rows, func(b tableBook) bool { return !ids[b.UUID] })
	}
	writeJSON(w, http.StatusOK, res)
}
