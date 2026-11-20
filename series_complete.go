package main

// Which series are complete: every numbered book (1, 2, 3…) in the
// library. Two sources, and neither needs the other:
//
//   - verified: an editor checked the series by hand and said how many
//     numbered books it has (series_verified). Works with no online
//     service at all.
//   - hardcover: the series is linked to Hardcover and every numbered book
//     on its list, less those dismissed, is here (hardcover.go).
//
// An editor's word wins over Hardcover's list. The statuses are worked
// out again only when the catalog, a group, a link, a dismissal or a
// verification changes, so the series list can show them on every view.

import (
	"database/sql"
	"errors"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Where a status comes from.
const (
	statusVerified  = "verified"
	statusHardcover = "hardcover"
)

// seriesStatus is how much of a series the library has.
type seriesStatus struct {
	Numbered int    `json:"numbered"` // numbered books the series has
	Have     int    `json:"have"`     // how many of them are here, in the series
	Complete bool   `json:"complete"` // all of them
	Source   string `json:"source"`   // statusVerified or statusHardcover
	Total    int    `json:"total"`    // verified: the number the editor gave
}

// Label: the source, as the pages say it.
func (s seriesStatus) Label() string {
	if s.Source == statusVerified {
		return "checked by an editor"
	}
	return "on Hardcover's list"
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
		verified map[string]int          // lower-cased name -> total
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
	c.hc, c.verified = nil, map[string]int{}
	if hardcoverEnabled() && appDB != nil {
		c.hc = hcStatuses()
	}
	merged := make(map[string]seriesStatus, len(c.hc))
	for k, st := range c.hc {
		merged[k] = st
	}
	if appDB != nil {
		if rows, err := appDB.Query(`SELECT name, total FROM series_verified`); err != nil {
			log.Println("series status:", err)
		} else {
			for rows.Next() {
				var name string
				var total int
				if rows.Scan(&name, &total) == nil {
					c.verified[strings.ToLower(name)] = total
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

// seriesVerifiedTotal: the number of numbered books an editor gave the
// series (as browsed), 0 for none.
func seriesVerifiedTotal(name string) int {
	c := &seriesStatusCache
	c.Lock()
	defer c.Unlock()
	seriesStatusesLocked()
	g, _, _ := groups.resolve("series", name)
	return c.verified[strings.ToLower(g)]
}

// seriesTopNumber: the highest whole number of the series' books (members
// of a group), 0 for none.
func seriesTopNumber(members []string) int {
	cat := currentCatalog()
	top := 0
	for i := range cat.books {
		b := &cat.books[i]
		if b.Removed || b.Series == 0 || b.SeriesIndex != math.Trunc(b.SeriesIndex) || b.SeriesIndex > seriesVerifiedMax {
			continue
		}
		if hcInMembers(cat.seriesNames[b.Series], members) {
			top = max(top, int(b.SeriesIndex))
		}
	}
	return top
}

// verifiedStatuses works out the verified series' statuses from the
// catalog: the whole numbers 1…total their books have. A book numbered
// past total raises it: the series grew, and it's complete again when
// the library has the new ones too.
func verifiedStatuses(cat *catalog, totals map[string]int) map[string]seriesStatus {
	have := map[string]map[int]bool{} // lower-cased name as browsed -> numbers
	for i := range cat.books {
		b := &cat.books[i]
		if b.Removed || b.Series == 0 || b.SeriesIndex != math.Trunc(b.SeriesIndex) || b.SeriesIndex < 1 || b.SeriesIndex > seriesVerifiedMax {
			continue
		}
		g, _, _ := groups.resolve("series", cat.seriesNames[b.Series])
		k := strings.ToLower(g)
		if _, ok := totals[k]; !ok {
			continue
		}
		if have[k] == nil {
			have[k] = map[int]bool{}
		}
		have[k][int(b.SeriesIndex)] = true
	}
	out := make(map[string]seriesStatus, len(totals))
	for k, total := range totals {
		st := seriesStatus{Source: statusVerified, Total: total, Numbered: total}
		for n := range have[k] {
			st.Numbered = max(st.Numbered, n)
		}
		for n := 1; n <= st.Numbered; n++ {
			if have[k][n] {
				st.Have++
			}
		}
		st.Complete = st.Numbered > 0 && st.Have == st.Numbered
		out[k] = st
	}
	return out
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

// seriesVerifyHandler: POST /api/series/verified {series, total}: an
// editor says the series has total numbered books.
func seriesVerifyHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Series string `json:"series"`
		Total  int    `json:"total"`
	}
	if !hcDecode(w, r, &req) {
		return
	}
	if req.Total < 1 || req.Total > seriesVerifiedMax {
		http.Error(w, "the number of books must be between 1 and 10000", http.StatusBadRequest)
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
	_, err := appDB.Exec(`INSERT INTO series_verified (name, total, verified_by, verified_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET total = excluded.total, verified_by = excluded.verified_by, verified_at = excluded.verified_at`,
		s.Name, req.Total, uid, time.Now().Unix())
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
