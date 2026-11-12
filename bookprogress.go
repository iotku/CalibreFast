package main

// How far a user is through their books, from whichever saved last: the web
// readers or a KOReader device. The library grid marks the books being read
// with it, and the book popup says where you are ("Page 5 of 25 · synced 2
// minutes ago from Kobo Clara").

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// bookProgress is a user's latest position in a book.
type bookProgress struct {
	Format    string
	Position  string // a page number for PDFs
	Percent   float64
	UpdatedAt int64  // ms
	Device    string // KOReader device; "" for the web readers
}

// latestProgress returns the user's newest position in each book, or in
// only that book when only isn't "". Device positions count when their
// document is already matched to a library file; nothing is hashed here.
func latestProgress(userID int64, only string) map[string]bookProgress {
	out := map[string]bookProgress{}
	keep := func(uuid string, p bookProgress) {
		if only != "" && uuid != only {
			return
		}
		if cur, ok := out[uuid]; !ok || p.UpdatedAt > cur.UpdatedAt {
			out[uuid] = p
		}
	}

	q := `SELECT book_uuid, format, position, percent, updated_at FROM reading_progress WHERE user_id = ?`
	args := []any{userID}
	if only != "" {
		q += ` AND book_uuid = ?`
		args = append(args, only)
	}
	if rows, err := appDB.Query(q, args...); err == nil {
		for rows.Next() {
			var uuid string
			var p bookProgress
			if rows.Scan(&uuid, &p.Format, &p.Position, &p.Percent, &p.UpdatedAt) == nil {
				keep(uuid, p)
			}
		}
		rows.Close()
	}

	type deviceRow struct {
		document string
		book     bookFile
		p        bookProgress
	}
	var devices []deviceRow
	if rows, err := appDB.Query(`SELECT p.document, p.progress, p.percentage, p.device, p.updated_at,
			COALESCE(d.book_uuid, ''), COALESCE(d.format, '')
		FROM device_progress p LEFT JOIN book_digests d ON d.digest = p.document
		WHERE p.user_id = ?`, userID); err == nil {
		for rows.Next() {
			var r deviceRow
			if rows.Scan(&r.document, &r.p.Position, &r.p.Percent, &r.p.Device, &r.p.UpdatedAt, &r.book.UUID, &r.book.Format) == nil {
				devices = append(devices, r)
			}
		}
		rows.Close()
	}
	for _, r := range devices {
		if r.book.UUID == "" {
			var ok bool
			if r.book, ok = libraryNames.lookup(r.document); !ok {
				continue
			}
		}
		r.p.Format = r.book.Format
		r.p.Percent = clamp01(r.p.Percent)
		keep(r.book.UUID, r.p)
	}
	return out
}

// readingHandler: GET /api/me/reading — what the library grid marks on the
// user's cards: the books being read and how far along each is (0..1, null
// when no position is known), where to resume each, and the books that are
// favorites, wanted ("want to read") or finished. The username says whose
// answer a browser has cached.
func readingHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	reading := map[string]*float64{}
	resume := map[string]string{}
	wanted, finished, favorites := []string{}, []string{}, []string{}

	rows, err := appDB.Query(`SELECT book_uuid, status FROM reading_status WHERE user_id = ?`, u.ID)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	var readingUUIDs []string
	for rows.Next() {
		var uuid, status string
		if rows.Scan(&uuid, &status) != nil {
			continue
		}
		switch status {
		case "reading":
			readingUUIDs = append(readingUUIDs, uuid)
		case "wanted":
			wanted = append(wanted, uuid)
		case "finished":
			finished = append(finished, uuid)
		}
	}
	rows.Close()
	if favorites, err = scanStrings(appDB.Query(`SELECT book_uuid FROM favorites WHERE user_id = ?`, u.ID)); err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if favorites == nil {
		favorites = []string{}
	}

	progress := latestProgress(u.ID, "")
	for _, uuid := range readingUUIDs {
		p, ok := progress[uuid]
		if ok {
			pct := math.Round(p.Percent*1000) / 1000
			reading[uuid] = &pct
		} else {
			reading[uuid] = nil
		}
		if url := resumeURL(uuid, p.Format); url != "" {
			resume[uuid] = url
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": u.Username, "reading": reading, "resume": resume,
		"wanted": wanted, "finished": finished, "favorites": favorites,
	})
}

// resumeURL is the web reader for a book, in the format last read when there
// is one (format: "epub", "pdf", or a KOReader device's format), else EPUB,
// else PDF. "" when the book has neither.
func resumeURL(uuid, format string) string {
	readers := map[string]string{"epub": "/read?uuid=", "pdf": "/read-pdf?uuid="}
	q := url.QueryEscape(uuid)
	if r, ok := readers[strings.ToLower(format)]; ok {
		return r + q
	}
	// The catalog knows the formats from metadata.db; the book's folder (on
	// the library's storage) is only listed for books with no data rows.
	if b, ok := currentCatalog().lookup(uuid); ok && len(b.Formats) > 0 {
		for _, ext := range []string{"epub", "pdf"} {
			if slices.Contains(b.Formats, ext) {
				return readers[ext] + q
			}
		}
		return ""
	}
	path, ok := uuidPathIndex.Load(uuid)
	if !ok {
		var err error
		if path, err = resolveBookPath(uuid); err != nil {
			return ""
		}
	}
	files := resolveFormats(uuid, baseDir, path.(string))
	for _, ext := range []string{"epub", "pdf"} {
		for _, f := range files {
			if strings.EqualFold(filepath.Ext(f), "."+ext) {
				return readers[ext] + q
			}
		}
	}
	return ""
}

// progressNote describes a position for the book popup, e.g. "Page 5 of 25 ·
// synced 2 minutes ago from Kobo Clara" or "42% · synced yesterday".
func progressNote(uuid string, p bookProgress, now time.Time) string {
	where := "Started"
	if page, err := strconv.Atoi(p.Position); p.Format == "pdf" && err == nil && page > 0 {
		where = "Page " + strconv.Itoa(page)
		if total := pdfPageCount(uuid); total >= page {
			where += " of " + strconv.Itoa(total)
		}
	} else if pct := math.Round(p.Percent * 100); pct >= 1 {
		where = fmt.Sprintf("%.0f%%", pct)
	} else if p.Percent > 0 {
		where = "<1%"
	}
	note := where + " · synced " + timeAgo(now.Sub(time.UnixMilli(p.UpdatedAt)), time.UnixMilli(p.UpdatedAt))
	if p.Device != "" {
		note += " from " + p.Device
	}
	return note
}

// timeAgo: "just now", "5 seconds ago", "3 minutes ago", "yesterday", or the
// date for anything older than a month.
func timeAgo(d time.Duration, at time.Time) string {
	unit := func(n int, name string) string {
		if n == 1 {
			return "1 " + name + " ago"
		}
		return strconv.Itoa(n) + " " + name + "s ago"
	}
	switch {
	case d < 10*time.Second:
		return "just now"
	case d < time.Minute:
		return unit(int(d/time.Second), "second")
	case d < time.Hour:
		return unit(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return unit(int(d/time.Hour), "hour")
	case d < 48*time.Hour:
		return "yesterday"
	case d < 30*24*time.Hour:
		return unit(int(d/(24*time.Hour)), "day")
	}
	return "on " + at.Format("2 Jan 2006")
}

// pdfPageCounts caches page counts by file, while the file is unchanged.
var pdfPageCounts = struct {
	sync.Mutex
	m map[string]pdfCount
}{m: map[string]pdfCount{}}

type pdfCount struct {
	size, mtime int64
	pages       int
}

// pdfPageCount is the number of pages in a book's PDF, or 0 if unknown.
// Only the PDF's page tree is read, not every page.
func pdfPageCount(uuid string) int {
	path, err := bookFilePath(uuid, "pdf")
	if err != nil {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	pdfPageCounts.Lock()
	c, ok := pdfPageCounts.m[path]
	pdfPageCounts.Unlock()
	if ok && c.size == fi.Size() && c.mtime == fi.ModTime().UnixNano() {
		return c.pages
	}
	c = pdfCount{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
	if f, err := os.Open(path); err == nil {
		if ctx, err := pdfapi.ReadContext(context.Background(), f, model.NewDefaultConfiguration()); err == nil && ctx.EnsurePageCount() == nil {
			c.pages = ctx.PageCount
		}
		f.Close()
	}
	pdfPageCounts.Lock()
	pdfPageCounts.m[path] = c
	pdfPageCounts.Unlock()
	return c.pages
}
