package main

// Tag rows on the Discover page ("More in Space Opera"; docs/notes/Discover.md,
// section 3).
//
// From the catalog, the eligible tags each get pages/discover/tag-N.json: up
// to tagRowBooks of their books, in the shuffle order (so a row isn't just
// the tag's newest books). Their index goes into the Discover HTML; the
// browser picks a few rows from it. Nothing here reads the library.
//
// Eligible tags, by name after tag groups (a group stands for its members):
//   - included by an admin: always (if they have books);
//   - denied by an admin: never;
//   - the rest: the tagRowsMax with the most books among those on at most
//     broad_tag_share of the library (25% by default), so "Fiction" or
//     "Ebook" don't make dull rows, and with at least tagRowMinBooks.
//
// The files are written again at startup, after catalog changes, and when
// tag groups or the admin's settings change; only those that differ.

import (
	"bytes"
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"hash/fnv"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	tagRowBooks           = 24
	tagRowsMax            = 40
	tagRowMinBooks        = 3
	defaultBroadTagShare  = 0.25
	settingBroadTagShare  = "discover.broad_tag_share"
	maxDiscoverTagNameLen = 255
)

// tagRow is an entry of the index in the Discover HTML.
type tagRow struct {
	N       int    `json:"n"` // pages/discover/tag-N.json
	Name    string `json:"name"`
	Href    string `json:"href"`
	Count   int    `json:"count"`
	Include bool   `json:"include,omitempty"` // chosen by an admin: weighted up
}

type tagRowsState struct {
	index   []tagRow
	json    string // index, for the HTML
	version string // hash of the index and files, for their ?v=
}

var (
	tagRowsMu sync.Mutex
	tagRows   atomic.Pointer[tagRowsState]
)

func currentTagRows() *tagRowsState {
	if s := tagRows.Load(); s != nil {
		return s
	}
	return &tagRowsState{json: "[]"}
}

func tagRowFile(n int) string {
	return filepath.Join("pages", "discover", "tag-"+strconv.Itoa(n)+".json")
}

// discoverTagSettings: the admin's lists (lower-case names) and cutoff.
type discoverTagSettings struct {
	share          float64
	include, deny  map[string]bool
	includeOrdered []string // as stored, for the admin page
	denyOrdered    []string
}

func loadDiscoverTagSettings() (discoverTagSettings, error) {
	s := discoverTagSettings{share: defaultBroadTagShare, include: map[string]bool{}, deny: map[string]bool{}}
	var v string
	err := appDB.QueryRow(`SELECT value FROM settings WHERE key = ?`, settingBroadTagShare).Scan(&v)
	if err == nil {
		if f, perr := strconv.ParseFloat(v, 64); perr == nil && f > 0 && f <= 1 {
			s.share = f
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return s, err
	}
	rows, err := appDB.Query(`SELECT name, mode FROM discover_tags ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, mode string
		if err := rows.Scan(&name, &mode); err != nil {
			return s, err
		}
		// A group's member stands for the group, as everywhere else.
		key := strings.ToLower(tagEntity(name))
		if mode == "include" {
			s.include[key] = true
			s.includeOrdered = append(s.includeOrdered, name)
		} else {
			s.deny[key] = true
			s.denyOrdered = append(s.denyOrdered, name)
		}
	}
	return s, rows.Err()
}

// tagEntity: the name a tag is browsed as (its group, if it has one).
func tagEntity(name string) string {
	n, _, _ := groups.resolve("tag", name)
	return n
}

// tagStat is a tag (after groups) and the books it's on.
type tagStat struct {
	Name  string
	Count int
	books []int32 // in shuffle order, at most tagRowBooks: filled for eligible tags only
}

// Why a tag is or isn't a row (the admin page shows it).
const (
	tagInRows   = "rows"     // has a row
	tagIncluded = "included" // has a row: included by an admin
	tagDenied   = "denied"
	tagBroad    = "broad" // on more than the cutoff's share of the library
	tagFew      = "few"   // fewer than tagRowMinBooks
	tagNotTop   = "not-top"
)

// tagStats counts the books of every tag in c (after groups), and says for
// each whether it gets a row under set. Rows first (included ones first,
// then by count), then the rest by count.
func tagStats(c *catalog, set discoverTagSettings) (stats []*tagStat, status map[string]string, total int) {
	// Each tag id's entity, once.
	entityOf := make(map[int32]*tagStat, len(c.tagNames))
	byName := map[string]*tagStat{}
	for id, name := range c.tagNames {
		e := tagEntity(name)
		key := strings.ToLower(e)
		st := byName[key]
		if st == nil {
			st = &tagStat{Name: e}
			byName[key] = st
		}
		entityOf[id] = st
	}
	seen := map[*tagStat]bool{}
	for i := range c.books {
		b := &c.books[i]
		if b.Removed {
			continue
		}
		total++
		clear(seen)
		for _, t := range b.TagIDs {
			if st := entityOf[t]; st != nil && !seen[st] {
				seen[st] = true
				st.Count++
			}
		}
	}
	status = map[string]string{}
	var rest []*tagStat
	for key, st := range byName {
		if st.Count == 0 {
			continue
		}
		switch {
		case set.include[key]:
			status[key] = tagIncluded
		case set.deny[key]:
			status[key] = tagDenied
		case float64(st.Count) > set.share*float64(total):
			status[key] = tagBroad
		case st.Count < tagRowMinBooks:
			status[key] = tagFew
		default:
			rest = append(rest, st)
			continue
		}
		stats = append(stats, st)
	}
	byCount := func(a, b *tagStat) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	}
	slices.SortFunc(rest, byCount)
	for i, st := range rest {
		if i < tagRowsMax {
			status[strings.ToLower(st.Name)] = tagInRows
		} else {
			status[strings.ToLower(st.Name)] = tagNotTop
		}
	}
	stats = append(stats, rest...)
	rank := func(st *tagStat) int {
		switch status[strings.ToLower(st.Name)] {
		case tagIncluded:
			return 0
		case tagInRows:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(stats, func(a, b *tagStat) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return byCount(a, b)
	})
	return stats, status, total
}

func hasRow(status string) bool { return status == tagInRows || status == tagIncluded }

// refreshTagRows works out the eligible tags and writes their files (those
// that changed), then the index and version the Discover page uses.
func refreshTagRows() {
	c := libraryCatalog.Load()
	if appDB == nil || c == nil {
		return
	}
	tagRowsMu.Lock()
	defer tagRowsMu.Unlock()
	set, err := loadDiscoverTagSettings()
	if err != nil {
		log.Printf("tag rows: %v", err)
		return
	}
	stats, status, _ := tagStats(c, set)
	var rows []*tagStat
	for _, st := range stats {
		if hasRow(status[strings.ToLower(st.Name)]) {
			rows = append(rows, st)
		}
	}
	// Books for the rows, in the shuffle order: one pass.
	want := map[int32][]*tagStat{} // tag id -> rows it feeds
	for id, name := range c.tagNames {
		key := strings.ToLower(tagEntity(name))
		for _, st := range rows {
			if strings.ToLower(st.Name) == key {
				want[id] = append(want[id], st)
			}
		}
	}
	added := map[*tagStat]map[int32]bool{}
	for _, st := range rows {
		added[st] = map[int32]bool{}
	}
	for _, slot := range c.shuffle {
		b := &c.books[slot]
		if b.Removed {
			continue
		}
		for _, t := range b.TagIDs {
			for _, st := range want[t] {
				if len(st.books) < tagRowBooks && !added[st][slot] {
					added[st][slot] = true
					st.books = append(st.books, slot)
				}
			}
		}
	}

	h := fnv.New64a()
	index := make([]tagRow, 0, len(rows))
	for n, st := range rows {
		books := make([]Book, len(st.books))
		for i, slot := range st.books {
			books[i] = c.books[slot].Book
		}
		data, err := json.Marshal(books)
		if err != nil {
			return
		}
		h.Write(data)
		if cur, err := os.ReadFile(tagRowFile(n)); err != nil || !bytes.Equal(cur, data) {
			if err := writeJSONFile(tagRowFile(n), data); err != nil {
				log.Printf("tag rows: %v", err)
				return
			}
		}
		index = append(index, tagRow{
			N: n, Name: st.Name, Href: categoryByKey("tag").href(st.Name),
			Count: st.Count, Include: status[strings.ToLower(st.Name)] == tagIncluded,
		})
	}
	// Files beyond the last row are left from a longer list.
	for n := len(rows); ; n++ {
		if err := os.Remove(tagRowFile(n)); err != nil {
			break
		}
	}
	idx, _ := json.Marshal(index)
	h.Write(idx)
	v := ""
	if len(index) > 0 {
		v = strconv.FormatUint(h.Sum64(), 36)
	}
	tagRows.Store(&tagRowsState{index: index, json: string(idx), version: v})
}

// ---------------------------------------------------------------------------
// Admin: /admin/discover and its API
// ---------------------------------------------------------------------------

type discoverTagInfo struct {
	Name   string  `json:"name"`
	Count  int     `json:"count"`
	Share  float64 `json:"share"`  // of the library
	Mode   string  `json:"mode"`   // "include", "deny" or ""
	Status string  `json:"status"` // tagInRows, tagBroad, …
}

// GET /api/admin/discover: the cutoff, the lists, and every tag with
// whether it has a row and why not.
func discoverSettingsHandler(w http.ResponseWriter, r *http.Request) {
	set, err := loadDiscoverTagSettings()
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	stats, status, total := tagStats(currentCatalog(), set)
	tags := make([]discoverTagInfo, 0, len(stats))
	for _, st := range stats {
		key := strings.ToLower(st.Name)
		mode := ""
		if set.include[key] {
			mode = "include"
		} else if set.deny[key] {
			mode = "deny"
		}
		share := 0.0
		if total > 0 {
			share = math.Round(float64(st.Count)/float64(total)*10000) / 10000
		}
		tags = append(tags, discoverTagInfo{Name: st.Name, Count: st.Count, Share: share, Mode: mode, Status: status[key]})
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"broad_tag_share": set.share,
		"rows_max":        tagRowsMax,
		"min_books":       tagRowMinBooks,
		"include":         stringsOrEmpty(set.includeOrdered),
		"deny":            stringsOrEmpty(set.denyOrdered),
		"tags":            tags,
	})
}

func stringsOrEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// PUT /api/admin/discover/tags/{name} {"mode": "include"|"deny"}.
func discoverTagPutHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil ||
		(req.Mode != "include" && req.Mode != "deny") || name == "" || len(name) > maxDiscoverTagNameLen {
		http.Error(w, "invalid tag setting", http.StatusBadRequest)
		return
	}
	// Stored under the name it's browsed as, so a group and its members are one entry.
	name = tagEntity(name)
	if _, err := appDB.Exec(`INSERT INTO discover_tags (name, mode) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET mode = excluded.mode`, name, req.Mode); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	tagSettingsVersion.Add(1) // recommendations leave excluded tags out
	refreshTagRows()
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/admin/discover/tags/{name}: back to the default.
func discoverTagDeleteHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if _, err := appDB.Exec(`DELETE FROM discover_tags WHERE name = ? OR name = ?`, name, tagEntity(name)); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	tagSettingsVersion.Add(1) // recommendations leave excluded tags out
	refreshTagRows()
	w.WriteHeader(http.StatusNoContent)
}

// PUT /api/admin/discover/settings {"broad_tag_share": 0.25}.
func discoverSettingsPutHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Share *float64 `json:"broad_tag_share"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil ||
		req.Share == nil || !(*req.Share > 0 && *req.Share <= 1) {
		http.Error(w, "the cutoff must be more than 0% and at most 100%", http.StatusBadRequest)
		return
	}
	if _, err := appDB.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		settingBroadTagShare, strconv.FormatFloat(*req.Share, 'f', -1, 64)); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	tagSettingsVersion.Add(1) // recommendations leave excluded tags out
	refreshTagRows()
	w.WriteHeader(http.StatusNoContent)
}

func discoverAdminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	logErr(templates.ExecuteTemplate(w, "admin-discover.html", map[string]any{"Title": SiteTitle}), "render discover settings")
}

func registerDiscoverAdminRoutes() {
	http.HandleFunc("GET /admin/discover", requireAdminUser(discoverAdminPage))
	http.HandleFunc("GET /api/admin/discover", requireAdminUser(discoverSettingsHandler))
	http.HandleFunc("PUT /api/admin/discover/settings", requireAdminUser(discoverSettingsPutHandler))
	http.HandleFunc("PUT /api/admin/discover/tags/{name}", requireAdminUser(discoverTagPutHandler))
	http.HandleFunc("DELETE /api/admin/discover/tags/{name}", requireAdminUser(discoverTagDeleteHandler))
}
