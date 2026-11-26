package main

// The cover updater (/library/covers, web/library-covers.ts): an editor
// picks books in the grid (select-books.ts), the page looks each one up on
// Hardcover and Open Library (/metadata/lookup, one book at a time) and
// shows its cover next to the one found, and the editor saves the ones they
// tick. Only books someone picked, never the whole library.
//
// Saving takes a source and a cover id, never an image or a URL: the server
// fetches the cover itself (fetchLookupCover, usually the copy the preview
// already fetched). Each saved cover is its own "cover update" entry in the
// edit log (editlog.go), so each can be undone on its own, later as well,
// and an undo is refused when the cover has changed since.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/pkg/metalookup"
)

const (
	// coverRefreshMax is the most books one run takes.
	coverRefreshMax = 200
	// coverUpdateAction names the updater's entries in the edit log.
	coverUpdateAction = "cover update"
	// coverHistoryMax is how many past updates the page lists.
	coverHistoryMax = 200

	// The most requests one book's lookup can take, for the page's warning
	// before it starts: an Open Library lookup is up to 7 requests
	// (edition, work, authors; or a search and its works) and its cover one
	// more; a Hardcover lookup is 2, and its covers aren't counted.
	olCoverCost = 1
)

var (
	olLookupCost = metalookup.LookupCost(openLibraryName)
	hcLookupCost = metalookup.LookupCost(hardcoverName)
)

func coverRefreshPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "library-covers.html", map[string]any{
		"Title": SiteTitle, "Max": coverRefreshMax,
	}), "render library-covers")
}

// coverRefreshBook is one picked book (TS: CoverRefreshBook in web/types.ts).
type coverRefreshBook struct {
	UUID     string `json:"uuid"`
	Title    string `json:"title"`
	Authors  string `json:"authors"` // "A & B", with commas (authorNameSQL)
	ISBN     string `json:"isbn"`
	HasCover bool   `json:"has_cover"`
	Cover    string `json:"cover,omitempty"` // /cover/{uuid}?t=…
}

const coverRefreshBookSQL = `SELECT b.uuid, b.title, ` + bookAuthorsSQL + `,
	COALESCE((SELECT i.val FROM identifiers i WHERE i.book = b.id AND lower(i.type) = 'isbn' LIMIT 1), ''),
	b.has_cover
	FROM books b WHERE b.uuid = ?`

// bookCoverURL is the book's cover with its current stamp.
func bookCoverURL(uuid string) string {
	u := "/cover/" + uuid
	if stamp, err := bookCoverStamp(uuid); err == nil && stamp != "" {
		u += "?t=" + stamp
	}
	return u
}

// coverRefreshBooksHandler: POST /api/covers/books {uuids}: the picked
// books, in the order given; books no longer in the library are left out.
func coverRefreshBooksHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UUIDs []string `json:"uuids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	uuids := uniqueStrings(req.UUIDs)
	if len(uuids) == 0 {
		http.Error(w, "no books", http.StatusBadRequest)
		return
	}
	if len(uuids) > coverRefreshMax {
		http.Error(w, "at most "+strconv.Itoa(coverRefreshMax)+" books at a time", http.StatusBadRequest)
		return
	}
	for _, u := range uuids {
		if !isSafeUUIDComponent(u) {
			http.Error(w, "invalid UUID", http.StatusBadRequest)
			return
		}
	}
	out := []coverRefreshBook{}
	for _, u := range uuids {
		var b coverRefreshBook
		err := calibreDB.QueryRow(coverRefreshBookSQL, u).Scan(&b.UUID, &b.Title, &b.Authors, &b.ISBN, &b.HasCover)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if b.HasCover {
			b.Cover = bookCoverURL(b.UUID)
		}
		out = append(out, b)
	}
	writeJSON(w, http.StatusOK, map[string]any{"books": out})
}

// coverBudget is a source's requests today (TS: CoverBudget).
type coverBudget struct {
	Enabled  bool   `json:"enabled"`
	Used     int    `json:"used"`
	Limit    int    `json:"limit"`
	ResetsIn string `json:"resets_in,omitempty"` // "7h 12m"
	Paused   string `json:"paused,omitempty"`    // until when, "15:04 UTC"
}

// coverBudgetHandler: GET /api/covers/budget: what's left of Open
// Library's and Hardcover's requests today, and what a book can cost.
func coverBudgetHandler(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	ol := coverBudget{Enabled: true}
	if u, ok := bookLookups.Usage(openLibraryName, now); ok {
		ol.Used, ol.Limit, ol.ResetsIn = u.Used, u.Daily, hcDuration(u.ResetsAt.Sub(now))
		if !u.PausedUntil.IsZero() {
			ol.Paused = u.PausedUntil.UTC().Format("15:04 UTC")
		}
	}
	hc := coverBudget{Enabled: hardcoverEnabled()}
	if hc.Enabled {
		u := hcUsageNow()
		hc.Used, hc.Limit, hc.ResetsIn, hc.Paused = u.Used, u.Limit, u.ResetsIn, u.Paused
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"openlibrary": ol,
		"hardcover":   hc,
		"cost": map[string]int{
			"openlibrary_lookup": olLookupCost,
			"openlibrary_cover":  olCoverCost,
			"hardcover_lookup":   hcLookupCost,
		},
	})
}

// coverApplyHandler: POST /api/covers/{uuid}/apply {src, id}: the cover a
// lookup offered (/metadata/lookup/cover?src=…&id=…) becomes the book's.
func coverApplyHandler(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	if !isSafeUUIDComponent(uuid) {
		http.Error(w, "invalid UUID", http.StatusBadRequest)
		return
	}
	var req struct {
		Src string `json:"src"`
		ID  string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	source, ok := coverSources[req.Src]
	if !ok || (source == hardcoverName && !hardcoverEnabled()) {
		http.Error(w, "unknown cover", http.StatusBadRequest)
		return
	}
	if _, err := resolveBookPath(uuid); err != nil {
		http.Error(w, "book not found", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second+lookupPatience)
	defer cancel()
	img, _, err := fetchLookupCover(ctx, source, req.ID, lookupPatience) // usually the preview's copy
	if code, msg := lookupCoverError(err); code != 0 {
		http.Error(w, msg, code)
		return
	}
	data, err := coverJPEG(img)
	if err != nil {
		http.Error(w, "the cover isn't an image we can read", http.StatusBadGateway)
		return
	}
	done := coverSnapper(currentUser(r), coverUpdateAction, uuid, bookTitle(uuid))
	if err := setBookCover(uuid, data); err != nil {
		done()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"edit_id": done(), "cover": bookCoverURL(uuid)})
}

// coverUpdate is one of the updater's edit log entries (TS: CoverUpdate).
type coverUpdate struct {
	ID         int64  `json:"id"`
	UUID       string `json:"uuid"`
	Title      string `json:"title"`
	Username   string `json:"username"`
	At         int64  `json:"at"`
	HadCover   bool   `json:"had_cover"` // before: "" is no cover
	RevertedAt int64  `json:"reverted_at,omitempty"`
	RevertedBy string `json:"reverted_by,omitempty"`
	InLibrary  bool   `json:"in_library"`
}

// coverUpdates is the updater's entries, newest first: one user's
// (userID > 0), or everyone's.
func coverUpdates(userID int64, limit int) ([]editLogEntry, error) {
	q := `SELECT ` + editLogCols + ` FROM edit_log WHERE kind = ? AND action = ?`
	args := []any{logKindCover, coverUpdateAction}
	if userID > 0 {
		q += ` AND user_id = ?`
		args = append(args, userID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := appDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanEditLog(rows)
}

// coverUpdateFor is entry id if it's one of the updater's that u may see
// and undo: their own, or anyone's for an admin.
func coverUpdateFor(u *User, idText string) (*editLogEntry, bool) {
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 || u == nil {
		return nil, false
	}
	entries, err := editLogByIDs([]int64{id})
	if err != nil || len(entries) != 1 {
		return nil, false
	}
	e := &entries[0]
	if e.Kind != logKindCover || e.Action != coverUpdateAction || (!u.IsAdmin && e.UserID != u.ID) {
		return nil, false
	}
	return e, true
}

// historyUserID is whose updates u sees: all (0) for an admin.
func historyUserID(u *User) int64 {
	if u.IsAdmin {
		return 0
	}
	return u.ID
}

// coverHistoryHandler: GET /api/covers/history: the user's cover updates
// (an admin's: everyone's), newest first.
func coverHistoryHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.NotFound(w, r)
		return
	}
	entries, err := coverUpdates(historyUserID(u), coverHistoryMax)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := []coverUpdate{}
	for _, e := range entries {
		var before string
		_ = json.Unmarshal(e.Before, &before)
		_, err := resolveBookPath(e.Target)
		out = append(out, coverUpdate{
			ID: e.ID, UUID: e.Target, Title: e.Title, Username: e.Username, At: e.At,
			HadCover: before != "", RevertedAt: e.RevertedAt, RevertedBy: e.RevertedBy, InLibrary: err == nil,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"updates": out})
}

var snapshotNameRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// coverHistoryImageHandler: GET /api/covers/history/{id}/before|after: the
// cover an update replaced, or the one it saved, from the edit log's copies.
func coverHistoryImageHandler(w http.ResponseWriter, r *http.Request) {
	e, ok := coverUpdateFor(currentUser(r), r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var raw json.RawMessage
	switch r.PathValue("side") {
	case "before":
		raw = e.Before
	case "after":
		raw = e.After
	default:
		http.NotFound(w, r)
		return
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil || !snapshotNameRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(dataDir, editLogCoversDir, name+".jpg"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400") // a copy never changes
	http.ServeContent(w, r, "", time.Time{}, f)
}

// coverRevertHandler: POST /api/covers/history/{id}/revert puts back the
// cover the update replaced, unless the cover has changed since (409).
func coverRevertHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	e, ok := coverUpdateFor(u, r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !waitBulkEditsFor(e.Target) {
		http.Error(w, errBulkStopping.Error(), http.StatusServiceUnavailable)
		return
	}
	res := revertEntries([]editLogEntry{*e}, u)
	if len(res) != 1 {
		http.Error(w, "nothing undone", http.StatusInternalServerError)
		return
	}
	if !res[0].Done {
		code := http.StatusInternalServerError
		switch {
		case res[0].Error == errRevertConflict.Error():
			code = http.StatusConflict
			res[0].Error = "the cover has changed since; change it in the book's editor instead"
		case strings.Contains(res[0].Error, "already undone"):
			code = http.StatusConflict
		case res[0].Error == errNoSuchBook.Error():
			code = http.StatusNotFound
		}
		http.Error(w, res[0].Error, code)
		return
	}
	var before, cover string
	if _ = json.Unmarshal(e.Before, &before); before != "" {
		cover = bookCoverURL(e.Target)
	}
	writeJSON(w, http.StatusOK, map[string]any{"cover": cover}) // "": no cover now
}
