package main

// Which series are complete: every numbered book (1, 2, 3…) in the
// library. Two sources, and neither needs the other:
//
//   - verified: an editor checked the series by hand and said its highest
//     number (every whole number up to it is needed), how many books it
//     has whatever their numbers (for a series whose books aren't
//     numbered: all #1, or #0), or both (series_verified). Works with no
//     online service at all.
//   - hardcover: the series is linked to Hardcover and every numbered book
//     on its list, less those dismissed, is here (internal/hardcoverseries).
//
// An editor's word wins over Hardcover's list. The statuses are worked
// out again only when the catalog, a group, a link, a dismissal or a
// verification changes, so the series list can show them on every view.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iotku/CalibreFast/internal/hardcoverseries"
)

// Where a status comes from.
const (
	statusVerified  = "verified"
	statusHardcover = "hardcover"
)

// seriesStatus is how much of a series the library has.
type seriesStatus struct {
	Numbered int    `json:"numbered"` // numbered books the series has (0: not counted by number)
	Have     int    `json:"have"`     // how many of them are here, in the series
	Complete bool   `json:"complete"` // all of them (and Books)
	Source   string `json:"source"`   // statusVerified or statusHardcover
	Total    int    `json:"total"`    // verified: the highest number the editor gave
	// Books: verified, how many books the series has, whatever their
	// numbers (0: not counted); BooksHave: how many are in the library.
	// Placeholders don't count: they're books it lacks.
	Books     int `json:"books,omitempty"`
	BooksHave int `json:"booksHave,omitempty"`
}

// seriesCount is what an editor said of a series; 0 is not given.
type seriesCount struct {
	Total int // its highest number
	Books int // how many books, whatever their numbers
}

// Label: the source, as the pages say it.
func (s seriesStatus) Label() string {
	if s.Source == statusVerified {
		return "checked by an editor"
	}
	return "on Hardcover's list"
}

// Coverage: how many of the series' books are here, for its progress
// ring. By number, by count of books, or (both given) whichever is
// further from complete. of is 0 when the series isn't counted at all.
func (s seriesStatus) Coverage() (have, of int) {
	if s.Numbered > 0 {
		have, of = s.Have, s.Numbered
	}
	if s.Books > 0 && (of == 0 || s.BooksHave*of < have*s.Books) {
		have, of = s.BooksHave, s.Books
	}
	return min(have, of), of
}

// Percent: Coverage as 0–100, 100 only when Complete, so a nearly full
// ring never reads as done.
func (s seriesStatus) Percent() int {
	if s.Complete {
		return 100
	}
	have, of := s.Coverage()
	if of == 0 {
		return 0
	}
	return min(99, have*100/of)
}

// CoverageText: "3 of 7 books", what the ring says to screen readers.
func (s seriesStatus) CoverageText() string {
	have, of := s.Coverage()
	if of == 1 {
		return fmt.Sprintf("%d of 1 book", have)
	}
	return fmt.Sprintf("%d of %d books", have, of)
}

// seriesVerifiedMax: more numbered books than this is a typo.
const seriesVerifiedMax = 10000

var (
	seriesStatusVersion atomic.Uint64 // bumped when a link, dismissal or verification changes
	seriesStatusCache   struct {
		sync.Mutex
		cat      *catalog
		gen, ver uint64
		hc       map[string]seriesStatus // lower-cased series name ->
		verified map[string]seriesCount  // lower-cased name ->
		merged   map[string]seriesStatus
	}
)

func seriesStatusChanged() { seriesStatusVersion.Add(1) }

// seriesStatusesLocked brings the cache up to date.
func seriesStatusesLocked() {
	c := &seriesStatusCache
	cat, gen, ver := currentCatalog(), aggregateGen.Load(), seriesStatusVersion.Load()
	if c.merged != nil && c.cat == cat && c.gen == gen && c.ver == ver {
		return
	}
	c.hc, c.verified = nil, map[string]seriesCount{}
	if hardcoverEnabled() && appDB != nil {
		c.hc = hcStatuses()
	}
	merged := make(map[string]seriesStatus, len(c.hc))
	for k, st := range c.hc {
		merged[k] = st
	}
	if appDB != nil {
		if rows, err := appDB.Query(`SELECT name, total, books FROM series_verified`); err != nil {
			log.Println("series status:", err)
		} else {
			for rows.Next() {
				var name string
				var n seriesCount
				if rows.Scan(&name, &n.Total, &n.Books) == nil {
					c.verified[strings.ToLower(name)] = n
				}
			}
			rows.Close()
		}
	}
	if len(c.verified) > 0 {
		for k, st := range verifiedStatuses(cat, c.verified) {
			merged[k] = st
		}
	}
	c.merged, c.cat, c.gen, c.ver = merged, cat, gen, ver
}

// seriesStatuses: every series with a status, by lower-cased name (as
// browsed: a group's name).
func seriesStatuses() map[string]seriesStatus {
	c := &seriesStatusCache
	c.Lock()
	defer c.Unlock()
	seriesStatusesLocked()
	return c.merged
}

// seriesStatusOf: the series' status; ok is false when it has none.
func seriesStatusOf(name string) (seriesStatus, bool) {
	st, ok := seriesStatuses()[strings.ToLower(name)]
	return st, ok
}

// statusPtr: the named series' status in st, nil when it has none or
// isn't counted (so has nothing to show).
func statusPtr(st map[string]seriesStatus, name string) *seriesStatus {
	s, ok := st[strings.ToLower(name)]
	if _, of := s.Coverage(); !ok || (of == 0 && !s.Complete) {
		return nil
	}
	return &s
}

// hcStatusOf: the series' share of its Hardcover list, whatever an editor
// said; ok is false when it isn't linked.
func hcStatusOf(name string) (seriesStatus, bool) {
	c := &seriesStatusCache
	c.Lock()
	defer c.Unlock()
	seriesStatusesLocked()
	st, ok := c.hc[strings.ToLower(name)]
	return st, ok
}

// seriesVerified: what an editor said of the series (as browsed); zero
// for nothing.
func seriesVerified(name string) seriesCount {
	c := &seriesStatusCache
	c.Lock()
	defer c.Unlock()
	seriesStatusesLocked()
	g, _, _ := groups.resolve("series", name)
	return c.verified[strings.ToLower(g)]
}

// seriesShape: the series' books (members of a group): how many, their
// highest whole number, and whether any is numbered past 1 (if none is,
// the series looks unnumbered: Calibre gives a book #1 by default).
func seriesShape(members []string) (books, top int, numbered bool) {
	cat := currentCatalog()
	for i := range cat.books {
		b := &cat.books[i]
		if b.Removed || b.Series == 0 || !hardcoverseries.InMembers(cat.seriesNames[b.Series], members) {
			continue
		}
		books++
		if b.SeriesIndex > 1 {
			numbered = true
		}
		if b.SeriesIndex == math.Trunc(b.SeriesIndex) && b.SeriesIndex <= seriesVerifiedMax {
			top = max(top, int(b.SeriesIndex))
		}
	}
	return books, top, numbered
}

// verifiedStatuses works out the verified series' statuses from the
// catalog.
//   - By number: the whole numbers 1…total its books have. A book numbered
//     past total raises it: the series grew, and it's complete again when
//     the library has the new ones too.
//   - By count: how many books it has, whatever their numbers; more than
//     the count is complete too.
//
// Complete needs both, of those given.
func verifiedStatuses(cat *catalog, counts map[string]seriesCount) map[string]seriesStatus {
	have := map[string]map[int]bool{} // lower-cased name as browsed -> numbers
	books := map[string]int{}         // lower-cased name as browsed -> books
	for i := range cat.books {
		b := &cat.books[i]
		if b.Removed || b.Series == 0 {
			continue
		}
		g, _, _ := groups.resolve("series", cat.seriesNames[b.Series])
		k := strings.ToLower(g)
		if _, ok := counts[k]; !ok {
			continue
		}
		books[k]++
		if b.SeriesIndex != math.Trunc(b.SeriesIndex) || b.SeriesIndex < 1 || b.SeriesIndex > seriesVerifiedMax {
			continue
		}
		if have[k] == nil {
			have[k] = map[int]bool{}
		}
		have[k][int(b.SeriesIndex)] = true
	}
	out := make(map[string]seriesStatus, len(counts))
	for k, n := range counts {
		if n.Total <= 0 && n.Books <= 0 {
			continue
		}
		st := seriesStatus{Source: statusVerified, Total: n.Total, Complete: true}
		if n.Total > 0 {
			st.Numbered = n.Total
			for x := range have[k] {
				st.Numbered = max(st.Numbered, x)
			}
			for x := 1; x <= st.Numbered; x++ {
				if have[k][x] {
					st.Have++
				}
			}
			st.Complete = st.Have == st.Numbered
		}
		if n.Books > 0 {
			st.Books, st.BooksHave = max(n.Books, books[k]), books[k]
			st.Complete = st.Complete && st.BooksHave >= st.Books
		}
		out[k] = st
	}
	return out
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

// seriesVerifyHandler: POST /api/series/verified {series, total, books}:
// an editor gives the series' highest number, its number of books, or
// both.
func seriesVerifyHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Series string `json:"series"`
		Total  int    `json:"total"` // highest number, 0 for none
		Books  int    `json:"books"` // how many books, 0 for none
	}
	if !hcDecode(w, r, &req) {
		return
	}
	if req.Total < 0 || req.Total > seriesVerifiedMax || req.Books < 0 || req.Books > seriesVerifiedMax || req.Total+req.Books == 0 {
		http.Error(w, "give the highest number, the number of books, or both: whole numbers from 1 to 10000", http.StatusBadRequest)
		return
	}
	s, ok := hcSeries(req.Series)
	if !ok {
		http.Error(w, "no such series in the library", http.StatusNotFound)
		return
	}
	var uid any
	if u := currentUser(r); u != nil {
		uid = u.ID
	}
	_, err := appDB.Exec(`INSERT INTO series_verified (name, total, books, verified_by, verified_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET total = excluded.total, books = excluded.books, verified_by = excluded.verified_by, verified_at = excluded.verified_at`,
		s.Name, req.Total, req.Books, uid, time.Now().Unix())
	if err != nil {
		log.Println("series verify:", err)
		http.Error(w, "couldn't save", http.StatusInternalServerError)
		return
	}
	seriesStatusChanged()
	gapsSeriesChanged(s.Members...)
	st, _ := seriesStatusOf(s.Name)
	writeJSON(w, http.StatusOK, st)
}

// seriesUnverifyHandler: DELETE /api/series/verified?series=
func seriesUnverifyHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.Join(strings.Fields(r.URL.Query().Get("series")), " ")
	if name == "" {
		http.Error(w, "no series", http.StatusBadRequest)
		return
	}
	g, members, _ := groups.resolve("series", name)
	if _, err := appDB.Exec(`DELETE FROM series_verified WHERE name = ?`, g); err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Println("series verify:", err)
		http.Error(w, "couldn't save", http.StatusInternalServerError)
		return
	}
	seriesStatusChanged()
	gapsSeriesChanged(members...)
	w.WriteHeader(http.StatusNoContent)
}

func registerSeriesStatusRoutes() {
	http.HandleFunc("POST /api/series/verified", requireEditor(seriesVerifyHandler))
	http.HandleFunc("DELETE /api/series/verified", requireEditor(seriesUnverifyHandler))
}
