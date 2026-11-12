package main

// Reading position and bookmarks, synced from the built-in readers.
//
// The readers keep their own copy in localStorage (so reading works while the
// server is down) and sync with these endpoints when signed in, so another
// device or a fresh login can resume. Conflicts are resolved last-writer-wins
// on the client's timestamp, clamped so a device with a clock set in the
// future can't lock in its position forever.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	maxPositionLen   = 2000 // EPUB CFIs can be long-ish
	maxLabelLen      = 200
	maxBookmarksBook = 500
	finishedPercent  = 0.97 // "moved through the book": treat as finished
)

type progressRec struct {
	Format    string  `json:"format"`
	Position  string  `json:"position"`
	Percent   float64 `json:"percent"`
	UpdatedAt int64   `json:"updated_at"` // ms
}

type bookmarkRec struct {
	ID        string `json:"id"` // client-generated
	Format    string `json:"format"`
	Position  string `json:"position"`
	Label     string `json:"label"`
	CreatedAt int64  `json:"created_at"` // ms
}

func validFormat(f string) bool { return f == "epub" || f == "pdf" }

func getProgress(userID int64, uuid, format string) (*progressRec, error) {
	p := progressRec{Format: format}
	err := appDB.QueryRow(`SELECT position, percent, updated_at FROM reading_progress WHERE user_id = ? AND book_uuid = ? AND format = ?`,
		userID, uuid, format).Scan(&p.Position, &p.Percent, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// progressGetHandler returns the latest position: the web readers' own, or a
// KOReader device's (kosync.go) when that is newer.
func progressGetHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	format := r.URL.Query().Get("format")
	if !validFormat(format) {
		http.Error(w, "format must be epub or pdf", http.StatusBadRequest)
		return
	}
	uuid := r.PathValue("uuid")
	p, err := getProgress(u.ID, uuid, format)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if d := deviceProgressForWeb(u.ID, uuid, format); d != nil && (p == nil || d.UpdatedAt > p.UpdatedAt) {
		p = d
	}
	writeJSON(w, http.StatusOK, map[string]any{"progress": p})
}

// progressPutHandler stores a position if it's newer than what's stored,
// and advances the reading status automatically. Returns the stored record
// (which may be the newer one from another device).
func progressPutHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	var in progressRec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	in.Position = strings.TrimSpace(in.Position)
	if !validFormat(in.Format) || in.Position == "" || len(in.Position) > maxPositionLen {
		http.Error(w, "need format (epub|pdf) and a position", http.StatusBadRequest)
		return
	}
	if in.Percent < 0 {
		in.Percent = 0
	} else if in.Percent > 1 {
		in.Percent = 1
	}
	now := time.Now().UnixMilli()
	if in.UpdatedAt <= 0 || in.UpdatedAt > now+5*60*1000 {
		in.UpdatedAt = now // missing or implausibly far in the future
	}
	if _, err := appDB.Exec(`
		INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, book_uuid, format) DO UPDATE SET
			position = excluded.position, percent = excluded.percent, updated_at = excluded.updated_at
		WHERE excluded.updated_at >= reading_progress.updated_at`,
		u.ID, uuid, in.Format, in.Position, in.Percent, in.UpdatedAt); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}

	// Moving through a book marks it as being read; reaching the end, finished.
	status := "reading"
	if in.Percent >= finishedPercent {
		status = "finished"
	}
	_ = setStatus(u.ID, uuid, status, "auto")

	stored, err := getProgress(u.ID, uuid, in.Format)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	st, src := readStatus(u.ID, uuid)
	writeJSON(w, http.StatusOK, map[string]any{"progress": stored, "status": st, "status_source": src})
}

func bookmarksGetHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	rows, err := appDB.Query(`SELECT client_id, format, position, label, created_at FROM bookmarks
		WHERE user_id = ? AND book_uuid = ? ORDER BY created_at`, u.ID, r.PathValue("uuid"))
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []bookmarkRec{}
	for rows.Next() {
		var b bookmarkRec
		if rows.Scan(&b.ID, &b.Format, &b.Position, &b.Label, &b.CreatedAt) == nil {
			out = append(out, b)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bookmarks": out})
}

func bookmarkPutHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var in bookmarkRec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	in.Position = strings.TrimSpace(in.Position)
	in.Label = strings.TrimSpace(in.Label)
	if len(id) == 0 || len(id) > 64 || !validFormat(in.Format) || in.Position == "" ||
		len(in.Position) > maxPositionLen || len(in.Label) > maxLabelLen {
		http.Error(w, "invalid bookmark", http.StatusBadRequest)
		return
	}
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM bookmarks WHERE user_id = ? AND book_uuid = ?`, u.ID, uuid).Scan(&n)
	if n >= maxBookmarksBook {
		http.Error(w, "too many bookmarks for this book", http.StatusConflict)
		return
	}
	if in.CreatedAt <= 0 {
		in.CreatedAt = time.Now().UnixMilli()
	}
	// Bookmark ids are scoped to the user; refuse to move another book's id.
	if _, err := appDB.Exec(`
		INSERT INTO bookmarks (user_id, book_uuid, client_id, format, position, label, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, client_id) DO UPDATE SET label = excluded.label
		WHERE bookmarks.book_uuid = excluded.book_uuid`,
		u.ID, uuid, id, in.Format, in.Position, in.Label, in.CreatedAt); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	in.ID = id
	writeJSON(w, http.StatusOK, in)
}

func bookmarkDeleteHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	_, err := appDB.Exec(`DELETE FROM bookmarks WHERE user_id = ? AND book_uuid = ? AND client_id = ?`,
		u.ID, r.PathValue("uuid"), r.PathValue("id"))
	if err != nil {
		http.Error(w, "could not delete", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func registerProgressRoutes() {
	http.HandleFunc("GET /api/me/reading", requireUser(readingHandler))
	http.HandleFunc("GET /api/me/home", requireUser(homeHandler))
	http.HandleFunc("GET /api/books", booksHandler)
	http.HandleFunc("GET /api/me/progress/{uuid}", requireUser(progressGetHandler))
	http.HandleFunc("PUT /api/me/progress/{uuid}", requireUser(progressPutHandler))
	http.HandleFunc("GET /api/me/bookmarks/{uuid}", requireUser(bookmarksGetHandler))
	http.HandleFunc("PUT /api/me/bookmarks/{uuid}/{id}", requireUser(bookmarkPutHandler))
	http.HandleFunc("DELETE /api/me/bookmarks/{uuid}/{id}", requireUser(bookmarkDeleteHandler))
}
