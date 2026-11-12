package main

// /shuffle: the whole library in a random order (docs/notes/Discover.md,
// section 2), on the same grid as the library.
//   - Signed out (-public): the static pages/shuffle/*.json, written from
//     the catalog's random order: the same files for everyone, cached for
//     good.
//   - Signed in: the same order without the books the user has finished
//     (they're probably looking for something new), served from the
//     catalog: nothing is read from the library.

import (
	"net/http"
)

func shuffleHandler(w http.ResponseWriter, r *http.Request) {
	if u := currentUser(r); u != nil {
		serveUUIDList(w, r, shuffleList(u))
		return
	}
	data := IndexPageData{
		Title:      "Shuffle — " + SiteTitle,
		Heading:    "Shuffle",
		TotalBooks: int(shuffleTotal.Load()),
		PagesDir:   "/pages/shuffle/",
		Version:    currentPagesVersion(),
	}
	logErr(templates.ExecuteTemplate(w, "index.html", data), "render shuffle page")
}

const shuffleEmpty = "Nothing left to shuffle: you've finished every book here."

// shuffleList is the catalog's random order without the books u finished.
func shuffleList(u *User) uuidList {
	finished := func() (map[string]bool, error) {
		uuids, err := scanStrings(appDB.Query(`SELECT book_uuid FROM reading_status WHERE user_id = ? AND status = 'finished'`, u.ID))
		set := make(map[string]bool, len(uuids))
		for _, id := range uuids {
			set[id] = true
		}
		return set, err
	}
	// walk calls f with each book to show, in order, until it returns false.
	walk := func(f func(uuid string) bool) error {
		skip, err := finished()
		if err != nil {
			return err
		}
		c := currentCatalog()
		for _, i := range c.shuffle {
			b := &c.books[i]
			if b.Removed || skip[b.UUID] {
				continue
			}
			if !f(b.UUID) {
				break
			}
		}
		return nil
	}
	return uuidList{
		Title:     "Shuffle",
		Sub:       "without the books you've finished",
		EmptyText: shuffleEmpty,
		BaseURL:   "/shuffle",
		Count: func() (int, error) {
			n := 0
			err := walk(func(string) bool { n++; return true })
			return n, err
		},
		Page: func(limit, offset int) ([]string, error) {
			var out []string
			n := 0
			err := walk(func(uuid string) bool {
				if n >= offset {
					out = append(out, uuid)
				}
				n++
				return len(out) < limit
			})
			return out, err
		},
	}
}
