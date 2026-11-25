package main

// Per-user reading data: favorites ("starred" books) and reading status.
// Everything here lives in the app database and needs a signed-in user;
// none of it touches Calibre's metadata.db or the library folder.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Book lookups (app DB stores UUIDs; details come from metadata.db)
// ---------------------------------------------------------------------------

// bookExists reports whether a UUID is in the Calibre library.
func bookExists(uuid string) bool {
	if _, ok := currentCatalog().lookup(uuid); ok {
		return true
	}
	var one int
	return calibreDB.QueryRow(`SELECT 1 FROM books WHERE uuid = ?`, uuid).Scan(&one) == nil
}

// booksByUUIDs returns grid entries for uuids, in the same order. Books no
// longer in the library are skipped. From the catalog; only books it doesn't
// have yet (added a moment ago) are read from metadata.db. A shelf's series
// keys are series cards (shelf_series.go).
func booksByUUIDs(uuids []string) ([]Book, error) {
	out := make([]Book, 0, len(uuids))
	if len(uuids) == 0 {
		return out, nil
	}
	cat := currentCatalog()
	var missing, seriesKeys []string
	var phs map[string]*placeholder // books not in the library yet that were saved (notifications.go)
	for _, u := range uuids {
		if _, ok := shelfSeriesName(u); ok {
			seriesKeys = append(seriesKeys, u)
		} else if _, ok := cat.lookup(u); !ok {
			if phs == nil {
				phs = map[string]*placeholder{}
				for _, p := range allPlaceholders() {
					phs[p.UUID] = p
				}
			}
			if phs[u] == nil {
				missing = append(missing, u)
			}
		}
	}
	series := seriesEntries(cat, seriesKeys)
	byUUID := map[string]Book{}
	if len(missing) > 0 {
		var err error
		if byUUID, err = booksByUUIDsFromDB(missing); err != nil {
			return nil, err
		}
	}
	for _, u := range uuids {
		if b, ok := series[u]; ok {
			out = append(out, b)
		} else if b, ok := cat.lookup(u); ok {
			out = append(out, b.Book)
		} else if p := phs[u]; p != nil {
			out = append(out, p.book())
		} else if b, ok := byUUID[u]; ok {
			out = append(out, b)
		}
	}
	return out, nil
}

func booksByUUIDsFromDB(uuids []string) (map[string]Book, error) {
	rows, err := calibreDB.Query(`
		SELECT b.uuid, b.title, `+bookAuthorsSQL+`,
		       b.pubdate, b.path, b.last_modified
		FROM books b
		WHERE b.uuid IN (`+placeholders(len(uuids))+`)`, toArgs(uuids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byUUID := map[string]Book{}
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.UUID, &b.Title, &b.Authors, &b.PubDate, &b.Path, &b.LastModified); err != nil {
			continue
		}
		b.LastModified = coverStamp(b.LastModified)
		uuidPathIndex.Store(b.UUID, b.Path)
		byUUID[b.UUID] = b
	}
	return byUUID, rows.Err()
}

// ---------------------------------------------------------------------------
// Book-list pages (favorites, reading, shelves…) on the shared grid
// ---------------------------------------------------------------------------

// uuidList is a paginated source of book UUIDs for one list.
type uuidList struct {
	Title     string
	Sub       string // small text next to the title
	EmptyText string
	BaseURL   string // the page's own path; ?api=1&page=N serves JSON
	Count     func() (int, error)
	Page      func(limit, offset int) ([]string, error)
	Extra     map[string]any // template extras (e.g. shelf controls)
}

// serveUUIDList renders the grid page, or a JSON page of books with ?api=1.
func serveUUIDList(w http.ResponseWriter, r *http.Request, l uuidList) {
	if r.URL.Query().Get("api") == "1" {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		limit, offset := booksPerPage, (page-1)*booksPerPage
		if r.URL.Query().Get("all") == "1" { // the whole list, to arrange (list_order.go)
			limit, offset = maxArrangeBooks, 0
		}
		done := timeStage(r, "list")
		uuids, err := l.Page(limit, offset)
		done()
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		done = timeStage(r, "books")
		books, err := booksByUUIDs(uuids)
		done()
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		writeJSONTagged(w, r, books) // 304 when the browser's copy is current (the offline Reading list)
		return
	}
	n, err := l.Count()
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	data := FilteredPageData{
		Title:      SiteTitle,
		Name:       l.Title,
		Display:    l.Title,
		Sub:        l.Sub,
		Active:     strings.TrimPrefix(l.BaseURL, "/me/"),
		PageURL:    l.BaseURL + "?api=1",
		TotalBooks: n,
		EmptyText:  l.EmptyText,
		Extra:      l.Extra,
	}
	logErr(templates.ExecuteTemplate(w, "filtered.html", data), "render list page")
}

func scanStrings(rows *sql.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func countQuery(q string, args ...any) func() (int, error) {
	return func() (int, error) {
		var n int
		err := appDB.QueryRow(q, args...).Scan(&n)
		return n, err
	}
}

func favoritesList(u *User) uuidList {
	return uuidList{
		Title: "Favorites", EmptyText: "No favorites yet — star a book to add it here.", BaseURL: "/me/favorites",
		Extra: map[string]any{"Arrange": "/api/me/lists/favorites/order"},
		Count: countQuery(`SELECT COUNT(*) FROM favorites WHERE user_id = ?`, u.ID),
		Page: func(limit, offset int) ([]string, error) {
			return scanStrings(appDB.Query(`SELECT book_uuid FROM favorites WHERE user_id = ? `+favoritesOrder+` LIMIT ? OFFSET ?`, u.ID, limit, offset))
		},
	}
}

// statusList: u's books with a status, listed at /me/<page>.
func statusList(u *User, status, page, title, empty string) uuidList {
	order := statusOrder
	if status == "wanted" { // oldest first, a queue (list_order.go)
		order = wantedOrder
	} else if status == "reading" { // most recently read first, always
		order = `ORDER BY updated_at DESC, book_uuid`
	}
	var extra map[string]any
	if arrangeable[status] {
		extra = map[string]any{"Arrange": "/api/me/lists/" + status + "/order"}
	}
	return uuidList{
		Title: title, EmptyText: empty, BaseURL: "/me/" + page, Extra: extra,
		Count: countQuery(`SELECT COUNT(*) FROM reading_status WHERE user_id = ? AND status = ?`, u.ID, status),
		Page: func(limit, offset int) ([]string, error) {
			return scanStrings(appDB.Query(`SELECT book_uuid FROM reading_status WHERE user_id = ? AND status = ? `+order+` LIMIT ? OFFSET ?`, u.ID, status, limit, offset))
		},
	}
}

func meListHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	switch r.PathValue("list") {
	case "favorites":
		l := favoritesList(u)
		if r.URL.Query().Get("api") != "1" {
			// The favorite authors and series, each a carousel above the
			// books, and the rest (tags, …: rarely a picture) as chips
			// (aggregate_favorites.go).
			if err := addFavSections(u, l.Extra); err != nil {
				http.Error(w, "query failed", http.StatusInternalServerError)
				return
			}
		}
		serveUUIDList(w, r, l)
	case "saved": // the "wanted" status
		serveUUIDList(w, r, statusList(u, "wanted", "saved", "Saved", "Nothing here yet. Save a book (the bookmark on its cover, or Save in its page) to add it."))
	case "reading":
		serveUUIDList(w, r, statusList(u, "reading", "reading", "Reading", "Nothing in progress. Books you open in the reader, or mark as reading, appear here."))
	case "paused":
		serveUUIDList(w, r, statusList(u, "paused", "paused", "Paused", "Nothing paused. Set a book's status to Paused to set it aside for now."))
	case "finished":
		serveUUIDList(w, r, statusList(u, "finished", "finished", "Finished", "No finished books yet."))
	case "dropped":
		serveUUIDList(w, r, statusList(u, "dropped", "dropped", "Dropped", "Nothing dropped. Books you stop reading for good (Dropped) are listed here."))
	case "wanted": // its old address
		target := "/me/saved"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	default:
		http.NotFound(w, r)
	}
}

// ---------------------------------------------------------------------------
// Favorites and status
// ---------------------------------------------------------------------------

type bookState struct {
	Favorite     bool        `json:"favorite"`
	Status       string      `json:"status"`        // "", or a status (readingStatuses)
	StatusSource string      `json:"status_source"` // "manual" | "auto"
	Shelves      []shelfMark `json:"shelves"`
}

func readStatus(userID int64, uuid string) (status, source string) {
	_ = appDB.QueryRow(`SELECT status, source FROM reading_status WHERE user_id = ? AND book_uuid = ?`, userID, uuid).Scan(&status, &source)
	return
}

func bookStateHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid := r.PathValue("uuid")
	var st bookState
	var one int
	st.Favorite = appDB.QueryRow(`SELECT 1 FROM favorites WHERE user_id = ? AND book_uuid = ?`, u.ID, uuid).Scan(&one) == nil
	st.Status, st.StatusSource = readStatus(u.ID, uuid)
	st.Shelves = shelfMarks(u, uuid)
	writeJSON(w, http.StatusOK, st)
}

func requireBook(w http.ResponseWriter, r *http.Request) (string, bool) {
	uuid := r.PathValue("uuid")
	if uuid == "" || !bookExists(uuid) {
		http.Error(w, "no such book", http.StatusNotFound)
		return "", false
	}
	return uuid, true
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// claimMarkChange records that a field ("favorite", "status") of a user's
// book changed at `at` (unix ms, the server's clock), unless a later change
// is recorded already: then it returns false, and the older change loses.
// One statement, so two changes racing can't both win.
func claimMarkChange(ex execer, userID int64, uuid, field string, at int64) (bool, error) {
	res, err := ex.Exec(`INSERT INTO mark_changes (user_id, book_uuid, field, changed_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, book_uuid, field) DO UPDATE SET changed_at = excluded.changed_at
		WHERE excluded.changed_at >= mark_changes.changed_at`, userID, uuid, field, at)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// maxChangeAge bounds age_ms: a device offline longer than this is treated as
// if its change were made that long ago.
const maxChangeAge = 365 * 24 * time.Hour

// changeTime is when a change was made, on the server's clock. A device
// sending one it kept while offline says how long ago it was made (age_ms,
// measured by its own clock between the change and the send), so devices'
// clocks needn't agree. No age: made just now.
func changeTime(ageMS *int64) int64 {
	now := nowMilli()
	if ageMS == nil || *ageMS <= 0 {
		return now
	}
	return now - min(*ageMS, maxChangeAge.Milliseconds())
}

// isFavorite reports whether a user has favorited a book.
func isFavorite(userID int64, uuid string) bool {
	var one int
	return appDB.QueryRow(`SELECT 1 FROM favorites WHERE user_id = ? AND book_uuid = ?`, userID, uuid).Scan(&one) == nil
}

// setFavoriteHandler: PUT /api/me/books/{uuid}/favorite {favorite, age_ms?}.
// A change older than the last one recorded (made offline, sent later) is
// not applied; the answer says so (applied: false) and gives the favorite
// as it stands.
func setFavoriteHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	var req struct {
		Favorite bool   `json:"favorite"`
		AgeMS    *int64 `json:"age_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	applied, err := setFavorite(u.ID, uuid, req.Favorite, changeTime(req.AgeMS))
	if err != nil {
		log.Println("set favorite:", err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"favorite": isFavorite(u.ID, uuid), "applied": applied})
}

// setFavorite applies a favorite change made at `at` (unix ms), unless a
// later one is recorded. Whether it applied.
func setFavorite(userID int64, uuid string, on bool, at int64) (bool, error) {
	tx, err := appDB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if ok, err := claimMarkChange(tx, userID, uuid, "favorite", at); err != nil || !ok {
		return false, err
	}
	if on {
		_, err = tx.Exec(`INSERT OR IGNORE INTO favorites (user_id, book_uuid, created_at) VALUES (?, ?, ?)`, userID, uuid, nowUnix())
	} else {
		_, err = tx.Exec(`DELETE FROM favorites WHERE user_id = ? AND book_uuid = ?`, userID, uuid)
	}
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

var errBadStatus = errors.New(`status must be "", "wanted", "reading", "paused", "finished" or "dropped"`)

// readingStatuses: the statuses a book can have (reading_status). Paused and
// Dropped are Hardcover's Paused and Did Not Finish.
var readingStatuses = map[string]bool{"wanted": true, "reading": true, "paused": true, "finished": true, "dropped": true}

func validStatus(status string) bool { return status == "" || readingStatuses[status] }

// autoRank orders statuses for automatic changes (from reading): they only
// move a book forward. Paused ranks with not started, so reading a paused
// book resumes it. Dropped outranks them all: only the user takes a book
// off Dropped, however much of it is read afterwards.
var autoRank = map[string]int{"": 0, "wanted": 0, "paused": 0, "reading": 1, "finished": 2, "dropped": 3}

// setStatus records a status. Manual changes always apply. Automatic ones
// (from reader progress) only move forward: none, wanted or paused →
// reading → finished (autoRank), never off Dropped, and never override a
// status the user chose themselves for that stage. Starting a wanted book
// takes it off Saved; reading a paused one resumes it.
//
// Every change is recorded as made now (mark_changes), so a manual change
// made earlier on a device that was offline doesn't undo it (setManualStatus).
func setStatus(userID int64, uuid, status, source string) error {
	if !validStatus(status) {
		return errBadStatus
	}
	if source == "manual" {
		_, err := setManualStatus(userID, uuid, status, nowMilli())
		return err
	}
	cur, _ := readStatus(userID, uuid)
	if autoRank[status] <= autoRank[cur] {
		// Not an advance. Still bump "reading" so recently read books sort first.
		if status == "reading" && cur == "reading" {
			_, _ = appDB.Exec(`UPDATE reading_status SET updated_at = ? WHERE user_id = ? AND book_uuid = ?`, nowUnix(), userID, uuid)
		}
		return nil
	}
	_, err := appDB.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (?, ?, ?, 'auto', ?)
		ON CONFLICT(user_id, book_uuid) DO UPDATE SET status = excluded.status, source = 'auto', updated_at = excluded.updated_at,
			position = CASE WHEN status = excluded.status THEN position END`,
		userID, uuid, status, nowUnix())
	if err != nil {
		return err
	}
	if err := recordStatusEvents(appDB, userID, uuid, cur, status); err != nil {
		return err
	}
	if _, err = claimMarkChange(appDB, userID, uuid, "status", nowMilli()); err != nil {
		return err
	}
	hcSync.Status(userID, uuid, status) // internal/hardcoversync/sync.go
	return nil
}

// setManualStatus applies a status the user chose at `at` (unix ms), unless
// a later change is recorded (made elsewhere meanwhile). Whether it applied.
func setManualStatus(userID int64, uuid, status string, at int64) (bool, error) {
	if !validStatus(status) {
		return false, errBadStatus
	}
	tx, err := appDB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if ok, err := claimMarkChange(tx, userID, uuid, "status", at); err != nil || !ok {
		return false, err
	}
	var cur string
	_ = tx.QueryRow(`SELECT status FROM reading_status WHERE user_id = ? AND book_uuid = ?`, userID, uuid).Scan(&cur)
	if err := recordStatusEvents(tx, userID, uuid, cur, status); err != nil {
		return false, err
	}
	if status == "" {
		_, err = tx.Exec(`DELETE FROM reading_status WHERE user_id = ? AND book_uuid = ?`, userID, uuid)
	} else {
		_, err = tx.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (?, ?, ?, 'manual', ?)
			ON CONFLICT(user_id, book_uuid) DO UPDATE SET status = excluded.status, source = 'manual', updated_at = excluded.updated_at,
				position = CASE WHEN status = excluded.status THEN position END`,
			userID, uuid, status, nowUnix())
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	hcSync.Status(userID, uuid, status) // internal/hardcoversync/sync.go
	return true, nil
}

func setStatusHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid := r.PathValue("uuid")
	// A book not in the library yet (a placeholder) can be Saved, or not:
	// nothing else (notifications.go).
	placeholder := uuid != "" && !bookExists(uuid) && placeholderByUUID(uuid) != nil
	if !placeholder {
		var ok bool
		if uuid, ok = requireBook(w, r); !ok {
			return
		}
	}
	var req struct {
		Status string `json:"status"`
		AgeMS  *int64 `json:"age_ms"` // a change kept offline: how long ago it was made (changeTime)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	req.Status = strings.TrimSpace(req.Status)
	if placeholder && req.Status != "" && req.Status != "wanted" {
		http.Error(w, "this book isn't in the library yet: it can only be saved", http.StatusBadRequest)
		return
	}
	applied, err := setManualStatus(u.ID, uuid, req.Status, changeTime(req.AgeMS))
	if err != nil {
		if errors.Is(err, errBadStatus) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Println("set status:", err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	// As it stands: a change older than the last one recorded isn't applied.
	status, source := readStatus(u.ID, uuid)
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "status_source": source, "applied": applied})
}

// BookUserActionsData feeds the per-user controls fragment under a book's
// read/download buttons.
type BookUserActionsData struct {
	UUID         string
	SignedIn     bool
	Next         string
	Favorite     bool
	Status       string
	StatusSource string
	Progress     string // where the user is, e.g. "Page 5 of 25 · synced 2 minutes ago" (bookprogress.go)
	Shelves      []shelfMark
	OnShelves    int
	// The book's series (as browsed), and the shelves its whole series can
	// go on (shelf_series.go).
	Series          string
	SeriesShelves   []shelfMark
	SeriesOnShelves int
	// Hardcover: whether the book is sent to the user's Hardcover account
	// (internal/hardcoversync/sync.go); nil when they haven't linked one.
	Hardcover *bookHardcover
}

type bookHardcover struct {
	Shared bool // this book is sent: chosen, or every book is and it wasn't left out
	All    bool // every book is shared
}

// bookUserActionsHandler: GET /book/me/{uuid} — the favorite / status /
// shelves controls as HTML, loaded (with a throbber) after the book page.
// Always 200: signed-out visitors get a sign-in hint, so a loading spinner
// is never left stuck on an error response.
func bookUserActionsHandler(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	data := BookUserActionsData{UUID: uuid, Next: url.QueryEscape("/book/get/" + uuid)}
	if u := currentUser(r); u != nil {
		data.SignedIn = true
		var one int
		data.Favorite = appDB.QueryRow(`SELECT 1 FROM favorites WHERE user_id = ? AND book_uuid = ?`, u.ID, uuid).Scan(&one) == nil
		data.Status, data.StatusSource = readStatus(u.ID, uuid)
		if p, ok := latestProgress(u.ID, uuid)[uuid]; ok {
			data.Progress = progressNote(uuid, p, time.Now())
		}
		if hardcoverSyncEnabled() && hcSync.IsLinked(u.ID) {
			data.Hardcover = &bookHardcover{Shared: hcSync.Shared(u.ID, uuid), All: hcSync.LoadShare(u.ID).All}
		}
		data.Shelves = shelfMarks(u, uuid)
		for _, s := range data.Shelves {
			if s.Has {
				data.OnShelves++
			}
		}
		if name, _, ok := librarySeries(uuid); ok {
			data.Series, _, _ = groups.resolve("series", name)
			data.SeriesShelves = seriesShelfMarks(u, data.Series)
			for _, s := range data.SeriesShelves {
				if s.Has {
					data.SeriesOnShelves++
				}
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	logErr(templates.ExecuteTemplate(w, "book-user-actions", data), "render book user actions")
}
