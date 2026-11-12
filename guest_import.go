package main

// Bringing reading done signed out into an account (docs/notes/Discover.md,
// section 5b). The browser keeps a guest's positions, highlights and
// bookmarks under their own names (web/local-keys.ts); after signing in the
// user can review them on /account/import and add them to the account.
//
//   POST /api/me/import/preview: the guest records, per book; answers how
//     each compares with the account: new, the same as what's there, a
//     position that differs from the account's (for the user to choose),
//     or a book no longer in the library. Nothing is written.
//   POST /api/me/import: the same records and the user's choices; stores
//     them. Highlights and bookmarks are never lost on either side: new ones
//     are added, the same passage highlighted on both sides keeps the
//     account's with the device's note added to it. A position is taken
//     when chosen; picking the device's older one over the account's makes
//     it the newest, so every device (KOReader included) moves to it.
//     Running it again changes nothing.
//
// Everything here is the app database: the library isn't read (books come
// from the catalog) or written.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	maxGuestBooks = 500
	maxGuestBody  = 8 << 20
	noteMergeSep  = "\n\n"
)

// guestBook is one book's records from the browser.
type guestBook struct {
	UUID        string          `json:"uuid"`
	Positions   []progressRec   `json:"positions"` // one per format
	Annotations []annotationRec `json:"annotations"`
	Bookmarks   []bookmarkRec   `json:"bookmarks"`
}

// accountPosition: the account's newest position in a book, from any
// device.
type accountPosition struct {
	Format    string  `json:"format"`
	Percent   float64 `json:"percent"`
	UpdatedAt int64   `json:"updated_at"`
	Device    string  `json:"device,omitempty"` // a KOReader device; "" for the web readers
}

type importPreview struct {
	UUID  string `json:"uuid"`
	Book  *Book  `json:"book,omitempty"` // nil: not in the library any more
	State string `json:"state"`          // new, same, conflict, missing
	// Position: the device's newest, and the account's when it has one
	// that differs (State "conflict"); the device's alone when it's new.
	Device         *progressRec     `json:"device,omitempty"`
	Account        *accountPosition `json:"account,omitempty"`
	NewAnnotations int              `json:"new_annotations"`
	MergedNotes    int              `json:"merged_notes"` // the same passage on both sides, notes to add
	NewBookmarks   int              `json:"new_bookmarks"`
	Status         string           `json:"status"` // the account's: "", wanted, reading, finished
}

func readGuestBooks(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxGuestBody)).Decode(into); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return false
	}
	return true
}

// cleanGuestBook drops what the readers wouldn't have saved, and keeps the
// newest position per format.
func cleanGuestBook(b guestBook) guestBook {
	out := guestBook{UUID: strings.TrimSpace(b.UUID)}
	for _, p := range b.Positions {
		p.Position = strings.TrimSpace(p.Position)
		if !validFormat(p.Format) || p.Position == "" || len(p.Position) > maxPositionLen {
			continue
		}
		p.Percent = min(1, max(0, p.Percent))
		if i := slices.IndexFunc(out.Positions, func(q progressRec) bool { return q.Format == p.Format }); i >= 0 {
			if p.UpdatedAt > out.Positions[i].UpdatedAt {
				out.Positions[i] = p
			}
			continue
		}
		out.Positions = append(out.Positions, p)
	}
	for _, a := range b.Annotations {
		a.Anchor = strings.TrimSpace(a.Anchor)
		if a.Color == "" {
			a.Color = annotationColors[0]
		}
		if a.Deleted || len(a.ID) == 0 || len(a.ID) > 64 || !validFormat(a.Format) || a.Anchor == "" || len(a.Anchor) > maxAnchorLen ||
			len(a.Text) > maxAnnotationText || len(a.Note) > maxAnnotationText || len(a.Chapter) > maxLabelLen ||
			a.Page < 0 || !slices.Contains(annotationColors, a.Color) {
			continue
		}
		out.Annotations = append(out.Annotations, a)
	}
	for _, m := range b.Bookmarks {
		m.Position = strings.TrimSpace(m.Position)
		m.Label = strings.TrimSpace(m.Label)
		if len(m.ID) == 0 || len(m.ID) > 64 || !validFormat(m.Format) || m.Position == "" ||
			len(m.Position) > maxPositionLen || len(m.Label) > maxLabelLen {
			continue
		}
		out.Bookmarks = append(out.Bookmarks, m)
	}
	return out
}

// newestPosition is the guest's most recent position in the book, if any.
func (b guestBook) newestPosition() *progressRec {
	var best *progressRec
	for i := range b.Positions {
		if best == nil || b.Positions[i].UpdatedAt > best.UpdatedAt {
			best = &b.Positions[i]
		}
	}
	return best
}

// bookComparison is how a guest book's records meet the account's.
type bookComparison struct {
	newAnnotations []annotationRec
	mergeNotes     []annotationRec // the account's, with the device's note added
	newBookmarks   []bookmarkRec
	device         *progressRec
	account        *accountPosition
	positionState  string // "", "new", "same", "conflict"
}

func compareGuestBook(userID int64, b guestBook) (bookComparison, error) {
	var c bookComparison

	// Highlights: the same id is the same highlight (a copy the account
	// already has); the same place with another id is the same passage
	// highlighted on both sides.
	rows, err := appDB.Query(`SELECT `+annotationCols+` FROM annotations WHERE user_id = ? AND book_uuid = ?`, userID, b.UUID)
	if err != nil {
		return c, err
	}
	byID := map[string]annotationRec{}
	var live []annotationRec
	for rows.Next() {
		if a, err := scanAnnotation(rows); err == nil {
			byID[a.ID] = a
			if !a.Deleted {
				live = append(live, a)
			}
		}
	}
	rows.Close()
	merged := map[string]int{} // account id -> index in c.mergeNotes
	for _, g := range b.Annotations {
		if _, ok := byID[g.ID]; ok {
			continue // the account has it (or deleted it: a tombstone wins)
		}
		i := slices.IndexFunc(live, func(a annotationRec) bool { return a.Format == g.Format && a.Anchor == g.Anchor })
		if i < 0 {
			c.newAnnotations = append(c.newAnnotations, g)
			continue
		}
		note := strings.TrimSpace(g.Note)
		if note == "" {
			continue
		}
		acc := live[i]
		if k, ok := merged[acc.ID]; ok {
			acc = c.mergeNotes[k]
		}
		if strings.Contains(acc.Note, note) {
			continue // already says it
		}
		if strings.TrimSpace(acc.Note) == "" {
			acc.Note = note
		} else {
			acc.Note = strings.TrimRight(acc.Note, "\n ") + noteMergeSep + note
		}
		if len(acc.Note) > maxAnnotationText {
			acc.Note = acc.Note[:maxAnnotationText]
		}
		if k, ok := merged[acc.ID]; ok {
			c.mergeNotes[k] = acc
		} else {
			merged[acc.ID] = len(c.mergeNotes)
			c.mergeNotes = append(c.mergeNotes, acc)
		}
	}

	// Bookmarks: the same id, or the same place.
	have, err := scanStrings(appDB.Query(`SELECT client_id || char(31) || format || char(31) || position FROM bookmarks WHERE user_id = ? AND book_uuid = ?`, userID, b.UUID))
	if err != nil {
		return c, err
	}
	for _, g := range b.Bookmarks {
		if !slices.ContainsFunc(have, func(h string) bool {
			parts := strings.SplitN(h, "\x1f", 3)
			return len(parts) == 3 && (parts[0] == g.ID || (parts[1] == g.Format && parts[2] == g.Position))
		}) {
			c.newBookmarks = append(c.newBookmarks, g)
		}
	}

	// Position: the device's newest against the account's newest, from any
	// device.
	c.device = b.newestPosition()
	if c.device != nil {
		if p, ok := latestProgress(userID, b.UUID)[b.UUID]; ok {
			c.account = &accountPosition{Format: p.Format, Percent: p.Percent, UpdatedAt: p.UpdatedAt, Device: p.Device}
			if strings.EqualFold(p.Format, c.device.Format) && p.Position == c.device.Position {
				c.positionState = "same"
			} else {
				c.positionState = "conflict"
			}
		} else {
			c.positionState = "new"
		}
	}
	return c, nil
}

func guestImportPreviewHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var in struct {
		Books []guestBook `json:"books"`
	}
	if !readGuestBooks(w, r, &in) {
		return
	}
	if len(in.Books) > maxGuestBooks {
		http.Error(w, "too many books", http.StatusBadRequest)
		return
	}
	cat := currentCatalog()
	out := make([]importPreview, 0, len(in.Books))
	for _, raw := range in.Books {
		b := cleanGuestBook(raw)
		p := importPreview{UUID: b.UUID}
		cb, ok := cat.lookup(b.UUID)
		if !ok {
			p.State = "missing"
			out = append(out, p)
			continue
		}
		book := cb.Book
		p.Book = &book
		c, err := compareGuestBook(u.ID, b)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		p.Device = c.device
		p.NewAnnotations, p.MergedNotes, p.NewBookmarks = len(c.newAnnotations), len(c.mergeNotes), len(c.newBookmarks)
		p.Status, _ = readStatus(u.ID, b.UUID)
		switch {
		case c.positionState == "conflict":
			p.State, p.Account = "conflict", c.account
		case c.positionState == "new" || p.NewAnnotations+p.MergedNotes+p.NewBookmarks > 0:
			p.State = "new"
		default:
			p.State = "same"
		}
		out = append(out, p)
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, map[string]any{"books": out})
}

type importResult struct {
	UUID  string `json:"uuid"`
	Error string `json:"error,omitempty"`
}

func guestImportHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var in struct {
		Books []struct {
			guestBook
			// UsePosition: take the device's position (always, for a book
			// the account has none for; the user's choice for a conflict).
			UsePosition bool `json:"use_position"`
		} `json:"books"`
	}
	if !readGuestBooks(w, r, &in) {
		return
	}
	if len(in.Books) > maxGuestBooks {
		http.Error(w, "too many books", http.StatusBadRequest)
		return
	}
	cat := currentCatalog()
	results := make([]importResult, 0, len(in.Books))
	for _, raw := range in.Books {
		b := cleanGuestBook(raw.guestBook)
		if _, ok := cat.lookup(b.UUID); !ok {
			results = append(results, importResult{UUID: b.UUID, Error: "not in the library"})
			continue
		}
		res := importResult{UUID: b.UUID}
		if err := importGuestBook(u.ID, b, raw.UsePosition); err != nil {
			res.Error = "could not save"
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// importGuestBook stores one book's guest records. Each step is safe to
// repeat, so a retry after a partial failure finishes the job.
func importGuestBook(userID int64, b guestBook, usePosition bool) error {
	c, err := compareGuestBook(userID, b)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()

	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM annotations WHERE user_id = ? AND book_uuid = ? AND deleted = 0`, userID, b.UUID).Scan(&n)
	for _, a := range c.newAnnotations {
		if n >= maxAnnotationsBook {
			break
		}
		if a.UpdatedAt <= 0 || a.UpdatedAt > now+annotationClockSkew {
			a.UpdatedAt = now
		}
		if a.CreatedAt <= 0 || a.CreatedAt > a.UpdatedAt {
			a.CreatedAt = a.UpdatedAt
		}
		if err := storeAnnotation(userID, b.UUID, a.ID, a); err != nil {
			return err
		}
		n++
	}
	for _, a := range c.mergeNotes {
		a.UpdatedAt = max(now, a.UpdatedAt+1) // newer than every copy, so it reaches the other devices
		if err := storeAnnotation(userID, b.UUID, a.ID, a); err != nil {
			return err
		}
	}

	_ = appDB.QueryRow(`SELECT COUNT(*) FROM bookmarks WHERE user_id = ? AND book_uuid = ?`, userID, b.UUID).Scan(&n)
	for _, m := range c.newBookmarks {
		if n >= maxBookmarksBook {
			break
		}
		if m.CreatedAt <= 0 {
			m.CreatedAt = now
		}
		if _, err := appDB.Exec(`INSERT INTO bookmarks (user_id, book_uuid, client_id, format, position, label, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(user_id, client_id) DO NOTHING`,
			userID, b.UUID, m.ID, m.Format, m.Position, m.Label, m.CreatedAt); err != nil {
			return err
		}
		n++
	}

	if p := c.device; p != nil && c.positionState != "same" && (usePosition || c.positionState == "new") {
		at := p.UpdatedAt
		if at <= 0 || at > now+5*60*1000 {
			at = now
		}
		// Chosen over a newer one in the account: make it the newest, or the
		// account's (and a device's) would still win everywhere.
		if c.account != nil && c.account.UpdatedAt >= at {
			at = max(now, c.account.UpdatedAt+1)
		}
		if _, err := appDB.Exec(`
			INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(user_id, book_uuid, format) DO UPDATE SET
				position = excluded.position, percent = excluded.percent, updated_at = excluded.updated_at
			WHERE excluded.updated_at >= reading_progress.updated_at`,
			userID, b.UUID, p.Format, p.Position, p.Percent, at); err != nil {
			return err
		}
		// As the readers do: reading, or finished at the end; never back.
		status := "reading"
		if p.Percent >= finishedPercent {
			status = "finished"
		}
		if err := setStatus(userID, b.UUID, status, "auto"); err != nil {
			return err
		}
	}
	return nil
}

func accountImportPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	logErr(templates.ExecuteTemplate(w, "account-import.html", PageData{Title: "Reading on this device — " + SiteTitle}), "render import page")
}

func registerGuestImportRoutes() {
	http.HandleFunc("GET /account/import", requireUser(accountImportPage))
	http.HandleFunc("POST /api/me/import/preview", requireUser(guestImportPreviewHandler))
	http.HandleFunc("POST /api/me/import", requireUser(guestImportHandler))
}
