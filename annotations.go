package main

// Highlights and notes made in the built-in readers.
//
// Like bookmarks (progress.go), the readers keep their own copy in
// localStorage and sync here when signed in. Unlike bookmarks, an annotation
// can be edited (its note, its colour) on two devices, so every change
// carries the client's clock and the newest wins. A delete is a change too:
// the row stays as a tombstone (deleted = 1, its text cleared), so a device
// that was offline learns of the delete instead of sending the annotation
// back.
//
// What is stored is what KOReader keeps for its own highlights (text, note,
// colour, chapter, page, created and updated times), so an import or export
// in its format stays possible. Its sync protocol (kosync.go) carries reading
// positions only, so annotations don't travel to devices today.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	maxAnchorLen        = 16000 // a PDF anchor lists a rectangle per line
	maxAnnotationText   = 4000
	maxAnnotationsBook  = 2000
	annotationClockSkew = 5 * 60 * 1000 // ms a client's clock may run ahead
)

// annotationColors are KOReader's highlight colours; the readers offer some.
var annotationColors = []string{"yellow", "red", "orange", "green", "olive", "cyan", "blue", "purple", "gray"}

// annotationRec is an annotation as the readers send and receive it
// (web/annotations.ts: Annotation).
type annotationRec struct {
	ID     string `json:"id"` // client-generated
	Format string `json:"format"`
	// Anchor is where it is, in a form that survives zoom and reflow:
	//   epub: an EPUB CFI range, "epubcfi(/6/8!/4/2,/1:0,/1:42)"
	//   pdf:  {"page":3,"rects":[[x0,y0,x1,y1],…]} in PDF points from the page's top left
	Anchor    string `json:"anchor"`
	Text      string `json:"text"` // the highlighted text
	Note      string `json:"note"`
	Color     string `json:"color"`
	Chapter   string `json:"chapter"` // the chapter's title when it was made
	Page      int    `json:"page"`    // the page number shown when it was made
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"` // client clock, ms: the newest change wins
	Deleted   bool   `json:"deleted,omitempty"`
}

const annotationCols = `client_id, format, anchor, text, note, color, chapter, page, created_at, updated_at, deleted`

func scanAnnotation(row interface{ Scan(...any) error }) (annotationRec, error) {
	var a annotationRec
	err := row.Scan(&a.ID, &a.Format, &a.Anchor, &a.Text, &a.Note, &a.Color, &a.Chapter, &a.Page, &a.CreatedAt, &a.UpdatedAt, &a.Deleted)
	return a, err
}

// annotationsGetHandler lists a book's annotations, tombstones included
// (the readers need them to drop their own copies).
func annotationsGetHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	rows, err := appDB.Query(`SELECT `+annotationCols+` FROM annotations
		WHERE user_id = ? AND book_uuid = ? ORDER BY created_at, client_id`, u.ID, r.PathValue("uuid"))
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []annotationRec{}
	for rows.Next() {
		if a, err := scanAnnotation(rows); err == nil {
			out = append(out, a)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"annotations": out})
}

// annotationPutHandler stores an annotation, or a change to one, unless a
// newer change is already stored. It answers with what is stored now.
func annotationPutHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var in annotationRec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	in.Anchor = strings.TrimSpace(in.Anchor)
	in.Chapter = strings.TrimSpace(in.Chapter)
	if in.Color == "" {
		in.Color = annotationColors[0]
	}
	if len(id) == 0 || len(id) > 64 || !validFormat(in.Format) || in.Anchor == "" || len(in.Anchor) > maxAnchorLen ||
		len(in.Text) > maxAnnotationText || len(in.Note) > maxAnnotationText || len(in.Chapter) > maxLabelLen ||
		in.Page < 0 || !slices.Contains(annotationColors, in.Color) {
		http.Error(w, "invalid annotation", http.StatusBadRequest)
		return
	}
	now := time.Now().UnixMilli()
	if in.UpdatedAt <= 0 || in.UpdatedAt > now+annotationClockSkew {
		in.UpdatedAt = now // missing, or a clock set in the future that would win forever
	}
	if in.CreatedAt <= 0 || in.CreatedAt > in.UpdatedAt {
		in.CreatedAt = in.UpdatedAt
	}
	if in.Deleted {
		in.Text, in.Note = "", "" // a tombstone keeps nothing of what was written
	}

	var exists bool
	_ = appDB.QueryRow(`SELECT 1 FROM annotations WHERE user_id = ? AND client_id = ?`, u.ID, id).Scan(&exists)
	if !exists && !in.Deleted {
		var n int
		_ = appDB.QueryRow(`SELECT COUNT(*) FROM annotations WHERE user_id = ? AND book_uuid = ? AND deleted = 0`, u.ID, uuid).Scan(&n)
		if n >= maxAnnotationsBook {
			http.Error(w, "too many annotations for this book", http.StatusConflict)
			return
		}
	}
	if err := storeAnnotation(u.ID, uuid, id, in); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	stored, err := scanAnnotation(appDB.QueryRow(`SELECT `+annotationCols+` FROM annotations
		WHERE user_id = ? AND book_uuid = ? AND client_id = ?`, u.ID, uuid, id))
	if err != nil {
		// The id belongs to another book of this user's.
		http.Error(w, "this id is in use for another book", http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

// storeAnnotation adds an annotation, or applies a change to one unless a
// newer change is stored. Ids are scoped to the user; an id never moves to
// another book, and where an annotation is (format, anchor, created_at)
// never changes.
func storeAnnotation(userID int64, uuid, id string, in annotationRec) error {
	_, err := appDB.Exec(`
		INSERT INTO annotations (user_id, book_uuid, client_id, format, anchor, text, note, color, chapter, page, created_at, updated_at, deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, client_id) DO UPDATE SET
			text = excluded.text, note = excluded.note, color = excluded.color,
			updated_at = excluded.updated_at, deleted = excluded.deleted
		WHERE annotations.book_uuid = excluded.book_uuid AND excluded.updated_at >= annotations.updated_at`,
		userID, uuid, id, in.Format, in.Anchor, in.Text, in.Note, in.Color, in.Chapter, in.Page, in.CreatedAt, in.UpdatedAt, in.Deleted)
	return err
}

func registerAnnotationRoutes() {
	http.HandleFunc("GET /api/me/annotations/{uuid}", requireUser(annotationsGetHandler))
	http.HandleFunc("PUT /api/me/annotations/{uuid}/{id}", requireUser(annotationPutHandler))
	http.HandleFunc("GET /api/me/annotations/{uuid}/koreader", requireUser(koreaderExportHandler))
	http.HandleFunc("POST /api/me/annotations/{uuid}/koreader", requireUser(koreaderImportHandler))
}
