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
// have yet (added a moment ago) are read from metadata.db.
func booksByUUIDs(uuids []string) ([]Book, error) {
	out := make([]Book, 0, len(uuids))
	if len(uuids) == 0 {
		return out, nil
	}
	cat := currentCatalog()
	var missing []string
	for _, u := range uuids {
		if _, ok := cat.lookup(u); !ok {
			missing = append(missing, u)
		}
	}
	byUUID := map[string]Book{}
	if len(missing) > 0 {
		var err error
		if byUUID, err = booksByUUIDsFromDB(missing); err != nil {
			return nil, err
		}
	}
	for _, u := range uuids {
		if b, ok := cat.lookup(u); ok {
			out = append(out, b.Book)
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
		uuids, err := l.Page(limit, offset)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		books, err := booksByUUIDs(uuids)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, http.StatusOK, books)
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

func statusList(u *User, status, title, empty string) uuidList {
	order := statusOrder
	if status == "reading" { // most recently read first, always
		order = `ORDER BY updated_at DESC, book_uuid`
	}
	var extra map[string]any
	if arrangeable[status] {
		extra = map[string]any{"Arrange": "/api/me/lists/" + status + "/order"}
	}
	return uuidList{
		Title: title, EmptyText: empty, BaseURL: "/me/" + status, Extra: extra,
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
		serveUUIDList(w, r, favoritesList(u))
	case "wanted":
		serveUUIDList(w, r, statusList(u, "wanted", "Saved", "Nothing here yet. Save a book (the bookmark on its cover, or Save in its page) to add it."))
	case "reading":
		serveUUIDList(w, r, statusList(u, "reading", "Reading", "Nothing in progress. Books you open in the reader, or mark as reading, appear here."))
	case "finished":
		serveUUIDList(w, r, statusList(u, "finished", "Finished", "No finished books yet."))
	default:
		http.NotFound(w, r)
	}
}

// ---------------------------------------------------------------------------
// Favorites and status
// ---------------------------------------------------------------------------

type bookState struct {
	Favorite     bool        `json:"favorite"`
	Status       string      `json:"status"`        // "", "wanted", "reading", "finished"
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

func setFavoriteHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	var req struct {
		Favorite bool `json:"favorite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	var err error
	if req.Favorite {
		_, err = appDB.Exec(`INSERT OR IGNORE INTO favorites (user_id, book_uuid, created_at) VALUES (?, ?, ?)`, u.ID, uuid, nowUnix())
	} else {
		_, err = appDB.Exec(`DELETE FROM favorites WHERE user_id = ? AND book_uuid = ?`, u.ID, uuid)
	}
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"favorite": req.Favorite})
}

var errBadStatus = errors.New(`status must be "", "wanted", "reading" or "finished"`)

// setStatus records a status. Manual changes always apply. Automatic ones
// (from reader progress) only move forward: none or wanted → reading →
// finished, and never override a status the user chose themselves for that
// stage. Starting a wanted book takes it off Saved.
func setStatus(userID int64, uuid, status, source string) error {
	if status != "" && status != "wanted" && status != "reading" && status != "finished" {
		return errBadStatus
	}
	if source == "manual" {
		if status == "" {
			_, err := appDB.Exec(`DELETE FROM reading_status WHERE user_id = ? AND book_uuid = ?`, userID, uuid)
			return err
		}
		_, err := appDB.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (?, ?, ?, 'manual', ?)
			ON CONFLICT(user_id, book_uuid) DO UPDATE SET status = excluded.status, source = 'manual', updated_at = excluded.updated_at,
				position = CASE WHEN status = excluded.status THEN position END`,
			userID, uuid, status, nowUnix())
		return err
	}
	cur, _ := readStatus(userID, uuid)
	rank := map[string]int{"": 0, "wanted": 0, "reading": 1, "finished": 2}
	if rank[status] <= rank[cur] {
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
	return err
}

func setStatusHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	req.Status = strings.TrimSpace(req.Status)
	if err := setStatus(u.ID, uuid, req.Status, "manual"); err != nil {
		if errors.Is(err, errBadStatus) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Println("set status:", err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	status, source := readStatus(u.ID, uuid)
	writeJSON(w, http.StatusOK, map[string]string{"status": status, "status_source": source})
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
		data.Shelves = shelfMarks(u, uuid)
		for _, s := range data.Shelves {
			if s.Has {
				data.OnShelves++
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	logErr(templates.ExecuteTemplate(w, "book-user-actions", data), "render book user actions")
}

func registerUserDataRoutes() {
	http.HandleFunc("GET /me/{list}", requireUser(meListHandler))
	http.HandleFunc("GET /book/me/{uuid}", bookUserActionsHandler)
	http.HandleFunc("GET /api/me/books/{uuid}", requireUser(bookStateHandler))
	http.HandleFunc("PUT /api/me/books/{uuid}/favorite", requireUser(setFavoriteHandler))
	http.HandleFunc("PUT /api/me/books/{uuid}/status", requireUser(setStatusHandler))
	http.HandleFunc("PUT /api/me/books/{uuid}/not-interested", requireUser(setNotInterestedHandler))
	http.HandleFunc("POST /api/me/recommended/next", requireUser(recNextHandler))
}
