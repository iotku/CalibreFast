package main

// Shelves on the Discover page (docs/notes/Discover.md, section 4a).
//
//   - Admins feature global shelves (shelves.featured_pos, which is also
//     their order). Featured shelves are on every signed-in user's page
//     until that user unpins them, and are what signed-out visitors see
//     (pages/discover/featured.json).
//   - Users pin any shelf they can see (shelf_pins). Unpinning a featured
//     shelf stores pinned = 0, so it stays off their page even if it's
//     featured again later.
//   - A user's rows: their pinned rows by position, then featured shelves
//     they have no row for, by featured order. Reordering stores every row
//     shown, so the order the user made stays as it is; a shelf featured
//     after that comes after it.
//   - Users order their own shelves on /shelves (shelves.position).
//
// All app data: nothing here reads or writes the library. Book details for
// the rows come from the catalog.

import (
	"bytes"
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"hash/fnv"
	"log"
	"math/rand/v2"
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
	shelfRowBooks = 24 // books per shelf row
	shelfRowsMax  = 12 // shelf rows on a page
	maxOrderIDs   = 1000
)

// homeShelf is a pinned row on the Discover page: a shelf, or a tag the
// user pinned (Kind "tag": no ID, Name is the tag as browsed, after groups).
type homeShelf struct {
	Kind     string `json:"kind"` // "shelf" or "tag"
	ID       int64  `json:"id,omitempty"`
	Name     string `json:"name"`
	Href     string `json:"href"`
	Global   bool   `json:"global,omitempty"`
	Featured bool   `json:"featured,omitempty"`
	Count    int    `json:"count"`
	Books    []Book `json:"books"`
}

// pinnedShelves: the pinned rows on u's page, in order, at most limit:
// the shelves and tags the user placed, by position (one order for both),
// then featured shelves they have no row for, by featured order.
func pinnedShelves(u *User, limit int) ([]homeShelf, error) {
	type placed struct {
		row homeShelf
		pos sql.NullInt64
		fea sql.NullInt64
	}
	var all []placed
	rows, err := appDB.Query(`SELECT s.id, s.name, s.owner_id IS NULL, s.featured_pos IS NOT NULL,
			(SELECT COUNT(*) FROM shelf_books b WHERE b.shelf_id = s.id), p.position, s.featured_pos
		FROM shelves s LEFT JOIN shelf_pins p ON p.shelf_id = s.id AND p.user_id = ?1
		WHERE (s.owner_id IS NULL OR s.owner_id = ?1)
		  AND (p.pinned = 1 OR (p.shelf_id IS NULL AND s.featured_pos IS NOT NULL))`, u.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p placed
		p.row.Kind = "shelf"
		if err := rows.Scan(&p.row.ID, &p.row.Name, &p.row.Global, &p.row.Featured, &p.row.Count, &p.pos, &p.fea); err != nil {
			rows.Close()
			return nil, err
		}
		p.row.Href = "/shelf/" + itoa(p.row.ID)
		all = append(all, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	tags, err := appDB.Query(`SELECT name, position FROM tag_pins WHERE user_id = ?`, u.ID)
	if err != nil {
		return nil, err
	}
	seenTag := map[string]bool{}
	for tags.Next() {
		var name string
		var pos int64
		if err := tags.Scan(&name, &pos); err != nil {
			tags.Close()
			return nil, err
		}
		// As browsed now: a tag grouped since it was pinned is its group.
		name = tagEntity(name)
		if seenTag[strings.ToLower(name)] {
			continue
		}
		seenTag[strings.ToLower(name)] = true
		all = append(all, placed{
			row: homeShelf{Kind: "tag", Name: name, Href: categoryByKey("tag").href(name)},
			pos: sql.NullInt64{Int64: pos, Valid: true},
		})
	}
	tags.Close()
	if err := tags.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(all, func(a, b placed) int {
		if a.pos.Valid != b.pos.Valid {
			if a.pos.Valid {
				return -1
			}
			return 1
		}
		if a.pos.Valid {
			return cmp.Compare(a.pos.Int64, b.pos.Int64)
		}
		if c := cmp.Compare(a.fea.Int64, b.fea.Int64); c != 0 {
			return c
		}
		return cmp.Compare(a.row.ID, b.row.ID)
	})
	out := []homeShelf{}
	var shelves, tagRows []int
	for _, p := range all {
		if len(out) == limit {
			break
		}
		if p.row.Kind == "shelf" {
			shelves = append(shelves, len(out))
		} else {
			tagRows = append(tagRows, len(out))
		}
		out = append(out, p.row)
	}
	// Books: one query for the shelves, one pass of the catalog for the tags.
	sub := make([]homeShelf, len(shelves))
	for i, at := range shelves {
		sub[i] = out[at]
	}
	if err := fillShelfBooks(sub); err != nil {
		return nil, err
	}
	for i, at := range shelves {
		out[at] = sub[i]
	}
	// A tag's row starts at a random place in its books each time, so the
	// page shows different ones on every load (tag_strip.go).
	ix := currentTagIndex()
	for _, at := range tagRows {
		name := out[at].Name
		out[at].Count = ix.count(name)
		out[at].Books, _ = ix.strip(name, rand.IntN(max(1, out[at].Count)), shelfRowBooks, nil)
	}
	return out, nil
}

// featuredShelves: every featured shelf, in the featured order.
func featuredShelves() ([]homeShelf, error) {
	rows, err := appDB.Query(`SELECT s.id, s.name, 1, 1,
			(SELECT COUNT(*) FROM shelf_books b WHERE b.shelf_id = s.id)
		FROM shelves s WHERE s.owner_id IS NULL AND s.featured_pos IS NOT NULL
		ORDER BY s.featured_pos, s.id`)
	if err != nil {
		return nil, err
	}
	return scanHomeShelves(rows)
}

func scanHomeShelves(rows *sql.Rows) ([]homeShelf, error) {
	defer rows.Close()
	out := []homeShelf{}
	for rows.Next() {
		s := homeShelf{Kind: "shelf"}
		if err := rows.Scan(&s.ID, &s.Name, &s.Global, &s.Featured, &s.Count); err != nil {
			return nil, err
		}
		s.Href = "/shelf/" + itoa(s.ID)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, fillShelfBooks(out)
}

// fillShelfBooks adds the first shelfRowBooks books of each shelf, in the
// shelf page's order, in one query. Books the catalog doesn't have (removed
// since) are left out rather than looked up in the library.
func fillShelfBooks(shelves []homeShelf) error {
	if len(shelves) == 0 {
		return nil
	}
	ids := make([]any, len(shelves))
	at := map[int64]int{}
	for i, s := range shelves {
		ids[i] = s.ID
		at[s.ID] = i
		shelves[i].Books = []Book{}
	}
	rows, err := appDB.Query(`SELECT shelf_id, book_uuid FROM (
			SELECT shelf_id, book_uuid,
				ROW_NUMBER() OVER (PARTITION BY shelf_id ORDER BY added_at, book_uuid) AS n
			FROM shelf_books WHERE shelf_id IN (`+placeholders(len(ids))+`))
		WHERE n <= ? ORDER BY shelf_id, n`, append(ids, shelfRowBooks)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cat := currentCatalog()
	for rows.Next() {
		var id int64
		var uuid string
		if err := rows.Scan(&id, &uuid); err != nil {
			return err
		}
		if b, ok := cat.lookup(uuid); ok {
			shelves[at[id]].Books = append(shelves[at[id]].Books, b.Book)
		}
	}
	return rows.Err()
}

// shelfPinnedFor: whether s is on u's page.
func shelfPinnedFor(u *User, s *Shelf) bool {
	var pinned sql.NullBool
	err := appDB.QueryRow(`SELECT pinned FROM shelf_pins WHERE user_id = ? AND shelf_id = ?`, u.ID, s.ID).Scan(&pinned)
	if errors.Is(err, sql.ErrNoRows) {
		return s.Featured
	}
	return err == nil && pinned.Bool
}

// setFeatured features a global shelf (at the end of the featured order) or
// stops featuring it.
func setFeatured(id int64, on bool) error {
	if !on {
		_, err := appDB.Exec(`UPDATE shelves SET featured_pos = NULL WHERE id = ?`, id)
		return err
	}
	_, err := appDB.Exec(`UPDATE shelves SET featured_pos =
			(SELECT COALESCE(MAX(featured_pos), -1) + 1 FROM shelves)
		WHERE id = ? AND owner_id IS NULL AND featured_pos IS NULL`, id)
	return err
}

// shelfChanged: after anything changes a shelf. Global shelves may be in
// featured.json.
func shelfChanged(s *Shelf) {
	if s.Global {
		refreshFeatured()
	}
}

// ---------------------------------------------------------------------------
// featured.json: the featured shelves for signed-out visitors
// ---------------------------------------------------------------------------

var (
	featuredMu sync.Mutex
	// featuredVersion: a hash of featured.json, for its ?v= ("" when there
	// are no featured shelves, so the page doesn't ask).
	featuredVersion atomic.Pointer[string]
)

func currentFeaturedVersion() string {
	if v := featuredVersion.Load(); v != nil {
		return *v
	}
	return ""
}

func featuredFile() string { return filepath.Join("pages", "discover", "featured.json") }

// refreshFeatured writes featured.json again (when it differs). Called at
// startup, when a global shelf changes, and when the catalog does (a
// title or cover on a featured shelf).
func refreshFeatured() {
	if appDB == nil || libraryCatalog.Load() == nil {
		return
	}
	featuredMu.Lock()
	defer featuredMu.Unlock()
	shelves, err := featuredShelves()
	if err != nil {
		log.Printf("featured shelves: %v", err)
		return
	}
	data, err := json.Marshal(shelves)
	if err != nil {
		return
	}
	v := ""
	if len(shelves) > 0 {
		h := fnv.New64a()
		h.Write(data)
		v = strconv.FormatUint(h.Sum64(), 36)
	}
	if cur, err := os.ReadFile(featuredFile()); err != nil || !bytes.Equal(cur, data) {
		if err := writeJSONFile(featuredFile(), data); err != nil {
			log.Printf("featured shelves: %v", err)
			return
		}
	}
	featuredVersion.Store(&v)
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// shelfForPin: a shelf the current user can see (404 otherwise).
func shelfForPin(w http.ResponseWriter, r *http.Request) (*Shelf, bool) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return nil, false
	}
	s, err := getShelf(id)
	if err != nil || !s.canView(currentUser(r)) {
		http.Error(w, "no such shelf", http.StatusNotFound)
		return nil, false
	}
	return s, true
}

// PUT /api/me/pins/{id}: on the user's page (at the end, unless it's
// already there).
func pinHandler(w http.ResponseWriter, r *http.Request) {
	s, ok := shelfForPin(w, r)
	if !ok {
		return
	}
	u := currentUser(r)
	if !shelfPinnedFor(u, s) {
		_, err := appDB.Exec(`INSERT INTO shelf_pins (user_id, shelf_id, pinned, position)
			VALUES (?1, ?2, 1, `+nextPinPosition+`)
			ON CONFLICT (user_id, shelf_id) DO UPDATE SET pinned = 1, position = excluded.position`, u.ID, s.ID)
		if err != nil {
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/me/pins/{id}: off the user's page, for good (a featured
// shelf doesn't come back by being featured again).
func unpinHandler(w http.ResponseWriter, r *http.Request) {
	s, ok := shelfForPin(w, r)
	if !ok {
		return
	}
	u := currentUser(r)
	_, err := appDB.Exec(`INSERT INTO shelf_pins (user_id, shelf_id, pinned, position) VALUES (?, ?, 0, 0)
		ON CONFLICT (user_id, shelf_id) DO UPDATE SET pinned = 0`, u.ID, s.ID)
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// readOrder reads {"ids": [...]}: an ordered list without repeats.
func readOrder(w http.ResponseWriter, r *http.Request) ([]int64, bool) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || len(req.IDs) > maxOrderIDs {
		http.Error(w, "invalid order", http.StatusBadRequest)
		return nil, false
	}
	seen := map[int64]bool{}
	for _, id := range req.IDs {
		if seen[id] {
			http.Error(w, "invalid order", http.StatusBadRequest)
			return nil, false
		}
		seen[id] = true
	}
	return req.IDs, true
}

// renumber sets col to 0, 1, … for ids in order, then the other rows that
// have it, after them in their old order. where picks the rows (the user's
// pins, the user's shelves, the featured shelves); ids not among them are
// skipped. One transaction, so two tabs' orders never mix.
func renumber(table, key, col, where string, args []any, ids []int64) error {
	tx, err := appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT `+key+` FROM `+table+` WHERE `+where+` ORDER BY `+col+`, `+key, args...)
	if err != nil {
		return err
	}
	var have []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		have = append(have, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	in := map[int64]bool{}
	for _, id := range have {
		in[id] = true
	}
	order := make([]int64, 0, len(have))
	listed := map[int64]bool{}
	for _, id := range ids {
		if in[id] {
			order = append(order, id)
			listed[id] = true
		}
	}
	for _, id := range have {
		if !listed[id] {
			order = append(order, id)
		}
	}
	for i, id := range order {
		if _, err := tx.Exec(`UPDATE `+table+` SET `+col+` = ? WHERE `+where+` AND `+key+` = ?`,
			append(append([]any{i}, args...), id)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// nextPinPosition: after every row the user placed (?1: the user), shelves
// and tags alike.
const nextPinPosition = `(SELECT COALESCE(MAX(p), -1) + 1 FROM (
		SELECT position AS p FROM shelf_pins WHERE user_id = ?1 AND pinned = 1
		UNION ALL SELECT position FROM tag_pins WHERE user_id = ?1))`

// pinRef names a pinned row: a shelf or a tag.
type pinRef struct {
	Shelf int64  `json:"shelf,omitempty"`
	Tag   string `json:"tag,omitempty"`
}

// PUT /api/me/pins {"rows": [{"shelf": 7}, {"tag": "Space Opera"}, …]}
// (or {"ids": [7, …]}, shelves only): the order of the pinned rows on the
// user's page, as shown there. Every shelf listed is stored as pinned, so
// the featured ones the user sees keep their place from now on. Rows not
// listed (pinned in another tab meanwhile) go after, in their old order.
func pinOrderHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs  []int64  `json:"ids"`
		Rows []pinRef `json:"rows"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&req); err != nil || len(req.IDs)+len(req.Rows) > maxOrderIDs {
		http.Error(w, "invalid order", http.StatusBadRequest)
		return
	}
	refs := req.Rows
	for _, id := range req.IDs {
		refs = append(refs, pinRef{Shelf: id})
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		key := "s" + itoa(ref.Shelf)
		if ref.Tag != "" {
			key = "t" + strings.ToLower(tagEntity(ref.Tag))
		}
		if seen[key] || (ref.Tag == "") == (ref.Shelf == 0) {
			http.Error(w, "invalid order", http.StatusBadRequest)
			return
		}
		seen[key] = true
	}
	if err := savePinOrder(currentUser(r), refs); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func savePinOrder(u *User, refs []pinRef) error {
	tx, err := appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The tags pinned, by the name they're browsed as now.
	stored := map[string]string{}
	rows, err := tx.Query(`SELECT name FROM tag_pins WHERE user_id = ?`, u.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		stored[strings.ToLower(tagEntity(name))] = name
	}
	rows.Close()
	// Out of the way of the new positions, keeping their order.
	const shift = 1 << 30
	if _, err := tx.Exec(`UPDATE shelf_pins SET position = position + ? WHERE user_id = ? AND pinned = 1`, shift, u.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tag_pins SET position = position + ? WHERE user_id = ?`, shift, u.ID); err != nil {
		return err
	}
	for i, ref := range refs {
		if ref.Tag != "" {
			name, ok := stored[strings.ToLower(tagEntity(ref.Tag))]
			if !ok {
				continue // unpinned meanwhile: stays so
			}
			if _, err := tx.Exec(`UPDATE tag_pins SET position = ? WHERE user_id = ? AND name = ?`, i, u.ID, name); err != nil {
				return err
			}
			continue
		}
		// Only shelves the user can see, and only those shown on their page:
		// a shelf they unpinned (in another tab) stays unpinned.
		_, err := tx.Exec(`INSERT INTO shelf_pins (user_id, shelf_id, pinned, position)
			SELECT ?1, s.id, 1, ?3 FROM shelves s
			WHERE s.id = ?2 AND (s.owner_id IS NULL OR s.owner_id = ?1)
			  AND NOT EXISTS (SELECT 1 FROM shelf_pins p WHERE p.user_id = ?1 AND p.shelf_id = s.id AND p.pinned = 0)
			  AND (s.featured_pos IS NOT NULL OR EXISTS (SELECT 1 FROM shelf_pins p WHERE p.user_id = ?1 AND p.shelf_id = s.id))
			ON CONFLICT (user_id, shelf_id) DO UPDATE SET position = excluded.position`, u.ID, ref.Shelf, i)
		if err != nil {
			return err
		}
	}
	// 0, 1, … again, shelves and tags together.
	type row struct {
		shelf int64
		tag   string
	}
	var order []row
	q, err := tx.Query(`SELECT shelf_id, '', position FROM shelf_pins WHERE user_id = ?1 AND pinned = 1
		UNION ALL SELECT 0, name, position FROM tag_pins WHERE user_id = ?1 ORDER BY 3`, u.ID)
	if err != nil {
		return err
	}
	for q.Next() {
		var rw row
		var pos int64
		if err := q.Scan(&rw.shelf, &rw.tag, &pos); err != nil {
			q.Close()
			return err
		}
		order = append(order, rw)
	}
	q.Close()
	for i, rw := range order {
		var err error
		if rw.tag != "" {
			_, err = tx.Exec(`UPDATE tag_pins SET position = ? WHERE user_id = ? AND name = ?`, i, u.ID, rw.tag)
		} else {
			_, err = tx.Exec(`UPDATE shelf_pins SET position = ? WHERE user_id = ? AND shelf_id = ?`, i, u.ID, rw.shelf)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PUT /api/me/tag-pins/{name}: a tag on the user's page (at the end).
func tagPinHandler(w http.ResponseWriter, r *http.Request) {
	name := tagEntity(strings.TrimSpace(r.PathValue("name")))
	if name == "" || len(name) > maxDiscoverTagNameLen || currentTagIndex().count(name) == 0 {
		http.Error(w, "no such tag", http.StatusNotFound)
		return
	}
	u := currentUser(r)
	if tagPinnedFor(u, name) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := appDB.Exec(`INSERT INTO tag_pins (user_id, name, position) VALUES (?1, ?2, `+nextPinPosition+`)
		ON CONFLICT (user_id, name) DO NOTHING`, u.ID, name); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/me/tag-pins/{name}: off the user's page (under any name
// that's browsed as this one).
func tagUnpinHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	want := strings.ToLower(tagEntity(strings.TrimSpace(r.PathValue("name"))))
	names, err := scanStrings(appDB.Query(`SELECT name FROM tag_pins WHERE user_id = ?`, u.ID))
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	for _, n := range names {
		if strings.ToLower(tagEntity(n)) == want {
			if _, err := appDB.Exec(`DELETE FROM tag_pins WHERE user_id = ? AND name = ?`, u.ID, n); err != nil {
				http.Error(w, "could not save", http.StatusInternalServerError)
				return
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// tagPinnedFor: whether the tag (as browsed) is on u's page.
func tagPinnedFor(u *User, name string) bool {
	names, err := scanStrings(appDB.Query(`SELECT name FROM tag_pins WHERE user_id = ?`, u.ID))
	if err != nil {
		return false
	}
	for _, n := range names {
		if strings.EqualFold(tagEntity(n), name) {
			return true
		}
	}
	return false
}

// PUT /api/me/shelves/order {"ids": [...]}: the order of the user's own
// shelves (on /shelves).
func shelfOrderHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := readOrder(w, r)
	if !ok {
		return
	}
	u := currentUser(r)
	if err := renumber("shelves", "id", "position", "owner_id = ?", []any{u.ID}, ids); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PUT /api/admin/featured-shelves {"ids": [...]}: the featured order.
func featuredOrderHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := readOrder(w, r)
	if !ok {
		return
	}
	if err := renumber("shelves", "id", "featured_pos", "owner_id IS NULL AND featured_pos IS NOT NULL", nil, ids); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	refreshFeatured()
	w.WriteHeader(http.StatusNoContent)
}

func registerShelfPinRoutes() {
	http.HandleFunc("PUT /api/me/pins/{id}", requireUser(pinHandler))
	http.HandleFunc("DELETE /api/me/pins/{id}", requireUser(unpinHandler))
	http.HandleFunc("PUT /api/me/pins", requireUser(pinOrderHandler))
	http.HandleFunc("PUT /api/me/tag-pins/{name}", requireUser(tagPinHandler))
	http.HandleFunc("DELETE /api/me/tag-pins/{name}", requireUser(tagUnpinHandler))
	http.HandleFunc("PUT /api/me/shelves/order", requireUser(shelfOrderHandler))
	http.HandleFunc("PUT /api/admin/featured-shelves", requireAdminUser(featuredOrderHandler))
}
