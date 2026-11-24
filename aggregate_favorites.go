package main

// Favorite authors, series, tags, publishers and languages: per user, by
// the name the value is browsed as (after groups), like tag and series pins.
//
//   - PUT / DELETE /api/me/favorites/{kind}/{name}: favorite or not. Only
//     values in the library can be favorited. A row whose value later
//     leaves the library (or is grouped away) is kept but not listed.
//   - GET /api/me/favorites: the user's favorites with their links and
//     counts. Pages stay the same for everyone: the browser keeps this
//     list (web/favorites.ts) and asks again only when /api/me's `favs`
//     (users.favs_changed) differs from the one it kept.
//   - Favorites weigh in Recommended for you (recommend.go) and are shown
//     on the aggregate pages, /account and /me/favorites: authors (with
//     their pictures) and series (as a shelf shows a series: the first
//     book's cover under a "Series · N books" banner) each a carousel
//     above the favorite books, other kinds as chips under them.
//
// Rows imported from Hardcover (origin 'hardcover', not built yet) are
// tombstoned on removal (removed_at) so a later import doesn't add them
// again; favoriting one again clears it.

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const maxAggFavorites = 500

// aggFavorite is one of the user's favorites as listed.
type aggFavorite struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Label string `json:"label,omitempty"` // a language's display name
	Href  string `json:"href"`
	Count int    `json:"count"`
	// HCFollowed: an author the user follows on Hardcover, as far as is
	// known here (hcFollowedHere).
	HCFollowed bool `json:"hc_followed,omitempty"`
}

// aggValue: the library's value of category c browsed as raw (after
// groups), or ok false when there's none. From the aggregate cache: no
// library read.
func aggValue(c *category, raw string) (it AggregateItem, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxDiscoverTagNameLen || strings.ContainsRune(raw, 0) {
		return it, false
	}
	name, _, _ := groups.resolve(c.Key, raw)
	for _, v := range getAggregateData(c).All {
		if strings.EqualFold(v.Name, name) {
			return v, true
		}
	}
	return it, false
}

// aggFavNames: u's live rows of kind that are browsed as name (a raw name
// grouped since counts as its group).
func aggFavNames(u *User, kind, name string) ([]string, error) {
	rows, err := scanStrings(appDB.Query(`SELECT name FROM aggregate_favorites
		WHERE user_id = ? AND kind = ? AND removed_at IS NULL`, u.ID, kind))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range rows {
		if g, _, _ := groups.resolve(kind, n); strings.EqualFold(g, name) {
			out = append(out, n)
		}
	}
	return out, nil
}

// userAggFavorites: u's favorites that are in the library, newest first,
// each value once.
func userAggFavorites(u *User) ([]aggFavorite, error) {
	rows, err := appDB.Query(`SELECT kind, name FROM aggregate_favorites
		WHERE user_id = ? AND removed_at IS NULL ORDER BY created_at DESC, name`, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []aggFavorite{}
	seen := map[string]bool{}
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			return nil, err
		}
		c := categoryByKey(kind)
		if c == nil {
			continue
		}
		v, ok := aggValue(c, name)
		key := kind + "/" + strings.ToLower(v.Name)
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, aggFavorite{Kind: kind, Name: v.Name, Label: v.Display, Href: c.href(v.Name), Count: v.Count})
	}
	return out, rows.Err()
}

// favCard is a favorite in the carousel on /me/favorites (filtered.html).
type favCard struct {
	aggFavorite
	KindLabel string // "Author"
	ImageURL  string // the entry's picture (aggregate_meta, else an author's from Hardcover), "" for none
	Initial   string // shown in place of a picture
}

// userFavCards: u's favorites as cards, by kind (authors first), newest first.
func userFavCards(u *User) ([]favCard, error) {
	favs, err := userAggFavorites(u)
	if err != nil {
		return nil, err
	}
	out := make([]favCard, 0, len(favs))
	for _, f := range favs {
		c := categoryByKey(f.Kind)
		label := cmp.Or(f.Label, f.Name)
		card := favCard{aggFavorite: f, KindLabel: strings.ToUpper(c.Label[:1]) + c.Label[1:]}
		if r, _ := utf8.DecodeRuneInString(label); r != utf8.RuneError {
			card.Initial = strings.ToUpper(string(r))
		}
		if m, ok := aggregateMetaFor(f.Kind, f.Name); ok {
			card.ImageURL = m.imageURL()
		}
		if card.ImageURL == "" && f.Kind == "author" {
			card.ImageURL = hcAuthorImageURL(hcAuthors.For(f.Name))
		}
		out = append(out, card)
	}
	// Authors first, then by the categories' order; newest first in each.
	kindAt := func(k string) int { return slices.IndexFunc(categories, func(c *category) bool { return c.Key == k }) }
	slices.SortStableFunc(out, func(a, b favCard) int { return kindAt(a.Kind) - kindAt(b.Kind) })
	return out, nil
}

// favRow is a carousel of favorite cards on /me/favorites: authors, or series.
type favRow struct {
	ID    string // "author": the row's element ids
	Title string // "Authors"
	Cards []favCard
	// Books: series rows only, the series' grid entries (seriesEntry, as a
	// shelf has them) as JSON, which the page draws as the grid draws a
	// shelf's series (bookCard in web/utils.ts) in place of Cards.
	Books string
}

// addFavSections puts u's favorites for /me/favorites in extra:
// FavRows (a carousel each for authors and series, those with any),
// FavChips (the other kinds) under the heading FavChipsTitle ("Tags &
// publishers"), and FavBooks when there's any of them, for a heading
// over the books.
func addFavSections(u *User, extra map[string]any) error {
	cards, err := userFavCards(u)
	if err != nil {
		return err
	}
	var rows []favRow
	var chips []favCard
	var chipKinds []string
	for _, key := range []string{"author", "series"} {
		row := favRow{ID: key, Title: categoryByKey(key).Title}
		for _, c := range cards {
			if c.Kind == key {
				row.Cards = append(row.Cards, c)
			}
		}
		if key == "series" {
			books := []Book{}
			for _, c := range row.Cards {
				if b, ok := seriesEntry(c.Name); ok {
					books = append(books, b)
				}
			}
			js, err := json.Marshal(books)
			if err != nil {
				return err
			}
			row.Books = string(js)
		}
		if len(row.Cards) > 0 {
			rows = append(rows, row)
		}
	}
	for _, c := range cards {
		if c.Kind == "author" || c.Kind == "series" {
			continue
		}
		chips = append(chips, c)
		if t := categoryByKey(c.Kind).Title; !slices.Contains(chipKinds, t) {
			chipKinds = append(chipKinds, t)
		}
	}
	extra["FavRows"], extra["FavChips"] = rows, chips
	// "Tags", "Tags & publishers", "Tags, publishers & languages"
	for i := 1; i < len(chipKinds); i++ {
		chipKinds[i] = strings.ToLower(chipKinds[i])
	}
	switch n := len(chipKinds); n {
	case 0:
	case 1:
		extra["FavChipsTitle"] = chipKinds[0]
	default:
		extra["FavChipsTitle"] = strings.Join(chipKinds[:n-1], ", ") + " & " + chipKinds[n-1]
	}
	extra["FavBooks"] = len(cards) > 0
	return nil
}

// followedBooks: Discover's "New from authors you follow" (home.go): the
// newest added books by u's favorite authors (after groups), up to n,
// leaving out those u finished or dropped; and whether u has any favorite
// authors in the library at all. From the catalog: no library read.
func followedBooks(u *User, m readingMarks, n int) (books []Book, following bool, err error) {
	favs, err := userRecFavs(u)
	if err != nil {
		return nil, false, err
	}
	c := currentCatalog()
	authors := favs.ids("author", c.authorNames)
	books = []Book{}
	if len(authors) == 0 {
		return books, false, nil
	}
	done := map[string]bool{}
	for _, uuid := range slices.Concat(m.Finished, m.Dropped) {
		done[uuid] = true
	}
	for _, slot := range latestOrder(c, "") {
		if len(books) >= n {
			break
		}
		b := &c.books[slot]
		if done[b.UUID] {
			continue
		}
		if slices.ContainsFunc(b.AuthorIDs, func(a int32) bool { return authors[a] }) {
			books = append(books, b.Book)
		}
	}
	return books, true, nil
}

// favsChanged: when u's favorites last changed (unix ms), for /api/me.
func favsChanged(u *User) int64 {
	var t int64
	_ = appDB.QueryRow(`SELECT favs_changed FROM users WHERE id = ?`, u.ID).Scan(&t)
	return t
}

// aggFavTarget: the request's category and value, or a 404 written.
func aggFavTarget(w http.ResponseWriter, r *http.Request) (*category, AggregateItem, bool) {
	c := categoryByKey(r.PathValue("kind"))
	if c == nil {
		http.Error(w, "no such kind", http.StatusNotFound)
		return nil, AggregateItem{}, false
	}
	v, ok := aggValue(c, r.PathValue("name"))
	if !ok {
		http.Error(w, "no such "+c.Label, http.StatusNotFound)
		return nil, AggregateItem{}, false
	}
	return c, v, true
}

// PUT /api/me/favorites/{kind}/{name}
func aggFavoriteHandler(w http.ResponseWriter, r *http.Request) {
	c, v, ok := aggFavTarget(w, r)
	if !ok {
		return
	}
	u := currentUser(r)
	have, err := aggFavNames(u, c.Key, v.Name)
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if len(have) > 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var n int
	if err := appDB.QueryRow(`SELECT COUNT(*) FROM aggregate_favorites WHERE user_id = ? AND removed_at IS NULL`, u.ID).Scan(&n); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if n >= maxAggFavorites {
		http.Error(w, "too many favorites", http.StatusConflict)
		return
	}
	now := time.Now().UnixMilli()
	tx, err := appDB.Begin()
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO aggregate_favorites (user_id, kind, name, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, kind, name) DO UPDATE SET removed_at = NULL, created_at = excluded.created_at`,
		u.ID, c.Key, v.Name, now); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if err := touchFavs(tx, u.ID, now); err != nil || tx.Commit() != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if c.Key == "author" {
		hcSync.QueueFollow(u.ID, v.Name, "follow", 0) // if they chose to (internal/hardcoversync/follow_push.go)
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/me/favorites/{kind}/{name}: under any name browsed as this
// one. Takes values no longer in the library too, so nothing is stuck.
// ?hardcover=1 (an author): also unfollow it on Hardcover, as the user
// ticked in the unstar dialog (internal/hardcoversync/follow_push.go).
func aggUnfavoriteHandler(w http.ResponseWriter, r *http.Request) {
	c := categoryByKey(r.PathValue("kind"))
	raw := strings.TrimSpace(r.PathValue("name"))
	if c == nil || raw == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	u := currentUser(r)
	name, _, _ := groups.resolve(c.Key, raw)
	names, err := aggFavNames(u, c.Key, name)
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if len(names) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	now := time.Now().UnixMilli()
	tx, err := appDB.Begin()
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	var hcID int64 // the author's Hardcover id, if known, for unfollowing
	for _, n := range names {
		var id sql.NullInt64
		if err := tx.QueryRow(`SELECT hc_id FROM aggregate_favorites WHERE user_id = ? AND kind = ? AND name = ?`, u.ID, c.Key, n).Scan(&id); err == nil && id.Valid && hcID == 0 {
			hcID = id.Int64
		}
		// Imported rows are kept as tombstones; our own go.
		if _, err := tx.Exec(`UPDATE aggregate_favorites SET removed_at = ?
			WHERE user_id = ? AND kind = ? AND name = ? AND origin <> 'local'`, now, u.ID, c.Key, n); err != nil {
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
		if _, err := tx.Exec(`DELETE FROM aggregate_favorites
			WHERE user_id = ? AND kind = ? AND name = ? AND origin = 'local'`, u.ID, c.Key, n); err != nil {
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
	}
	if err := touchFavs(tx, u.ID, now); err != nil || tx.Commit() != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if c.Key == "author" && r.URL.Query().Get("hardcover") == "1" {
		hcSync.QueueFollow(u.ID, name, "unfollow", hcID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// touchFavs: u's favorites changed (and with them, their recommendations:
// the row as last shown is dropped so the next one is ranked afresh).
func touchFavs(tx *sql.Tx, userID, now int64) error {
	if _, err := tx.Exec(`UPDATE users SET favs_changed = MAX(favs_changed + 1, ?) WHERE id = ?`, now, userID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE rec_state SET shown = NULL WHERE user_id = ?`, userID)
	return err
}

// GET /api/me/favorites
func aggFavoritesHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	favs, err := userAggFavorites(u)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	// For the unstar dialog (internal/hardcoversync/follow_push.go): which authors are
	// followed on Hardcover, whether the user's link can unfollow, and
	// whether its box starts ticked.
	p := hcSync.LoadFollowPrefs(u.ID)
	if p.CanWrite {
		followed := hcSync.FollowedHere(u.ID)
		for i := range favs {
			favs[i].HCFollowed = favs[i].Kind == "author" && followed[strings.ToLower(favs[i].Name)]
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"favs": favsChanged(u), "favorites": favs,
		"hc_unfollow": p.CanWrite, "hc_unfollow_default": p.UnfollowDefault,
		// Authors starred here are followed there, if not yet (it's sent in
		// the background): the dialog asks for them too.
		"hc_follow_stars": p.CanWrite && p.Share})
}

// bumpFavs: something the favorites answer shows changed besides the
// stars (a follow on Hardcover, a setting): browsers ask for it again.
func bumpFavs(userID int64) {
	if _, err := appDB.Exec(`UPDATE users SET favs_changed = MAX(favs_changed + 1, ?) WHERE id = ?`, time.Now().UnixMilli(), userID); err != nil {
		log.Println("favorites:", err)
	}
}

func registerAggregateFavoriteRoutes() {
	http.HandleFunc("GET /api/me/favorites", requireUser(aggFavoritesHandler))
	http.HandleFunc("PUT /api/me/favorites/{kind}/{name}", requireUser(aggFavoriteHandler))
	http.HandleFunc("DELETE /api/me/favorites/{kind}/{name}", requireUser(aggUnfavoriteHandler))
}
