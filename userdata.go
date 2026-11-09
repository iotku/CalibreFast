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
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Book lookups (app DB stores UUIDs; details come from metadata.db)
// ---------------------------------------------------------------------------

// bookExists reports whether a UUID is in the Calibre library.
func bookExists(uuid string) bool {
	var one int
	return calibreDB.QueryRow(`SELECT 1 FROM books WHERE uuid = ?`, uuid).Scan(&one) == nil
}

// booksByUUIDs returns grid entries for uuids, in the same order. Books no
// longer in the library are skipped.
func booksByUUIDs(uuids []string) ([]Book, error) {
	out := make([]Book, 0, len(uuids))
	if len(uuids) == 0 {
		return out, nil
	}
	rows, err := calibreDB.Query(`
		SELECT b.uuid, b.title, COALESCE(GROUP_CONCAT(DISTINCT a.sort), ''),
		       b.pubdate, b.path, b.last_modified
		FROM books b
		LEFT JOIN books_authors_link bal ON bal.book = b.id
		LEFT JOIN authors a ON a.id = bal.author
		WHERE b.uuid IN (`+placeholders(len(uuids))+`)
		GROUP BY b.id`, toArgs(uuids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byUUID := map[string]Book{}
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.UUID, &b.Title, &b.AuthorSort, &b.PubDate, &b.Path, &b.LastModified); err != nil {
			continue
		}
		uuidPathIndex.Store(b.UUID, b.Path)
		byUUID[b.UUID] = b
	}
	for _, u := range uuids {
		if b, ok := byUUID[u]; ok {
			out = append(out, b)
		}
	}
	return out, rows.Err()
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
		uuids, err := l.Page(booksPerPage, (page-1)*booksPerPage)
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
		PageURL:    l.BaseURL + "?api=1",
		TotalPages: pagesFor(n),
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
		Count: countQuery(`SELECT COUNT(*) FROM favorites WHERE user_id = ?`, u.ID),
		Page: func(limit, offset int) ([]string, error) {
			return scanStrings(appDB.Query(`SELECT book_uuid FROM favorites WHERE user_id = ? ORDER BY created_at DESC, book_uuid LIMIT ? OFFSET ?`, u.ID, limit, offset))
		},
	}
}

func statusList(u *User, status, title, empty string) uuidList {
	return uuidList{
		Title: title, EmptyText: empty, BaseURL: "/me/" + status,
		Count: countQuery(`SELECT COUNT(*) FROM reading_status WHERE user_id = ? AND status = ?`, u.ID, status),
		Page: func(limit, offset int) ([]string, error) {
			return scanStrings(appDB.Query(`SELECT book_uuid FROM reading_status WHERE user_id = ? AND status = ? ORDER BY updated_at DESC, book_uuid LIMIT ? OFFSET ?`, u.ID, status, limit, offset))
		},
	}
}

func meListHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	switch r.PathValue("list") {
	case "favorites":
		serveUUIDList(w, r, favoritesList(u))
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
	Status       string      `json:"status"`        // "", "reading", "finished"
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

var errBadStatus = errors.New(`status must be "", "reading" or "finished"`)

// setStatus records a status. Manual changes always apply. Automatic ones
// (from reader progress) only move forward: none → reading → finished, and
// never override a status the user chose themselves for that stage.
func setStatus(userID int64, uuid, status, source string) error {
	if status != "" && status != "reading" && status != "finished" {
		return errBadStatus
	}
	if source == "manual" {
		if status == "" {
			_, err := appDB.Exec(`DELETE FROM reading_status WHERE user_id = ? AND book_uuid = ?`, userID, uuid)
			return err
		}
		_, err := appDB.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (?, ?, ?, 'manual', ?)
			ON CONFLICT(user_id, book_uuid) DO UPDATE SET status = excluded.status, source = 'manual', updated_at = excluded.updated_at`,
			userID, uuid, status, nowUnix())
		return err
	}
	cur, _ := readStatus(userID, uuid)
	rank := map[string]int{"": 0, "reading": 1, "finished": 2}
	if rank[status] <= rank[cur] {
		// Not an advance. Still bump "reading" so recently read books sort first.
		if status == "reading" && cur == "reading" {
			_, _ = appDB.Exec(`UPDATE reading_status SET updated_at = ? WHERE user_id = ? AND book_uuid = ?`, nowUnix(), userID, uuid)
		}
		return nil
	}
	_, err := appDB.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (?, ?, ?, 'auto', ?)
		ON CONFLICT(user_id, book_uuid) DO UPDATE SET status = excluded.status, source = 'auto', updated_at = excluded.updated_at`,
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

func registerUserDataRoutes() {
	http.HandleFunc("GET /me/{list}", requireUser(meListHandler))
	http.HandleFunc("GET /api/me/books/{uuid}", requireUser(bookStateHandler))
	http.HandleFunc("PUT /api/me/books/{uuid}/favorite", requireUser(setFavoriteHandler))
	http.HandleFunc("PUT /api/me/books/{uuid}/status", requireUser(setStatusHandler))
}
