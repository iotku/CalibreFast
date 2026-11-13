package main

// The Discover page's layout, per user (docs/notes/Discover.md):
//
//   - PUT /api/me/home-layout {"order": [...], "hidden": [...]}: the order
//     of the page's rows as the user arranged them ("k:shuffle" a section,
//     "s:7" a pinned shelf, "t:Space Opera" a pinned tag), and the sections
//     they turned off. The page merges it with what's there now (a row
//     pinned since goes beside the other pins; a section missing from the
//     order keeps its default place), so it's stored as given.
//   - PUT / DELETE /api/me/hidden-tags/{name}: a tag the user doesn't want
//     a "More in" row for (the row's ✕), and back.
//
// Both come with /api/me/home. Signed-out visitors keep theirs in the
// browser.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
)

// homeSections: the sections a user can turn off (Continue reading
// can't be). Keep in step with web/discover.ts.
var homeSections = []string{"greeting", "wanted", "recommended", "shuffle", "latest", "tags"}

const maxLayoutID = 300 // bytes: "t:" and a tag name

type homeLayout struct {
	Order  []string `json:"order"`
	Hidden []string `json:"hidden"`
}

// userHomeLayout: u's layout (empty when they haven't arranged the page).
func userHomeLayout(u *User) (homeLayout, error) {
	l := homeLayout{Order: []string{}, Hidden: []string{}}
	var js string
	err := appDB.QueryRow(`SELECT layout FROM home_layout WHERE user_id = ?`, u.ID).Scan(&js)
	if errors.Is(err, sql.ErrNoRows) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	_ = json.Unmarshal([]byte(js), &l) // unreadable: the default layout
	if l.Order == nil {
		l.Order = []string{}
	}
	if l.Hidden == nil {
		l.Hidden = []string{}
	}
	return l, nil
}

// validLayout: ids that name a row ("k:", "s:" or "t:" and something),
// without repeats; hidden only sections that can be turned off.
func validLayout(l homeLayout) bool {
	if len(l.Order) > maxOrderIDs || len(l.Hidden) > len(homeSections) {
		return false
	}
	seen := map[string]bool{}
	for _, id := range l.Order {
		k := strings.ToLower(id)
		if seen[k] || len(id) > maxLayoutID || len(id) < 3 || !slices.Contains([]string{"k:", "s:", "t:"}, id[:2]) {
			return false
		}
		seen[k] = true
	}
	for i, h := range l.Hidden {
		if !slices.Contains(homeSections, h) || slices.Contains(l.Hidden[:i], h) {
			return false
		}
	}
	return true
}

func homeLayoutHandler(w http.ResponseWriter, r *http.Request) {
	var l homeLayout
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&l); err != nil || !validLayout(l) {
		http.Error(w, "invalid layout", http.StatusBadRequest)
		return
	}
	if l.Order == nil {
		l.Order = []string{}
	}
	if l.Hidden == nil {
		l.Hidden = []string{}
	}
	js, err := json.Marshal(l)
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if _, err := appDB.Exec(`INSERT INTO home_layout (user_id, layout) VALUES (?, ?)
		ON CONFLICT (user_id) DO UPDATE SET layout = excluded.layout`, currentUser(r).ID, string(js)); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// userHiddenTags: the tags u has no "More in" rows for, as browsed now.
func userHiddenTags(u *User) ([]string, error) {
	names, err := scanStrings(appDB.Query(`SELECT name FROM hidden_tags WHERE user_id = ? ORDER BY name`, u.ID))
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, n := range names {
		if n = tagEntity(n); !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, n) }) {
			out = append(out, n)
		}
	}
	return out, nil
}

func hideTagHandler(w http.ResponseWriter, r *http.Request) {
	name := tagEntity(strings.TrimSpace(r.PathValue("name")))
	if name == "" || len(name) > maxDiscoverTagNameLen {
		http.Error(w, "no such tag", http.StatusNotFound)
		return
	}
	if _, err := appDB.Exec(`INSERT INTO hidden_tags (user_id, name) VALUES (?, ?) ON CONFLICT DO NOTHING`,
		currentUser(r).ID, name); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/me/hidden-tags/{name}: back, under any name browsed as this one.
func unhideTagHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	want := strings.ToLower(tagEntity(strings.TrimSpace(r.PathValue("name"))))
	names, err := scanStrings(appDB.Query(`SELECT name FROM hidden_tags WHERE user_id = ?`, u.ID))
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	for _, n := range names {
		if strings.ToLower(tagEntity(n)) != want {
			continue
		}
		if _, err := appDB.Exec(`DELETE FROM hidden_tags WHERE user_id = ? AND name = ?`, u.ID, n); err != nil {
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func registerHomeLayoutRoutes() {
	http.HandleFunc("PUT /api/me/home-layout", requireUser(homeLayoutHandler))
	http.HandleFunc("PUT /api/me/hidden-tags/{name}", requireUser(hideTagHandler))
	http.HandleFunc("DELETE /api/me/hidden-tags/{name}", requireUser(unhideTagHandler))
}
