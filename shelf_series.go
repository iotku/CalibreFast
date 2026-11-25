package main

// A whole series on a shelf. It's a shelf_books row like a book's, whose
// book_uuid is the key "series:<name>" (the series as browsed: its group's
// name if it's grouped), so it has a place among the shelf's books and is
// arranged with them (list_order.go) without anything new in the schema.
//
// Shown as one card: the first book's cover with a "Series · N books"
// banner, linking to the series page. The books are the series' books as
// the library has them now; a series with none left (renamed, or its books
// removed) is skipped like a removed book.
//
//   - PUT    /api/shelves/{id}/series/{name...}
//   - DELETE /api/shelves/{id}/series/{name...}
//   - GET    /series-popup/{name...}: its card in the book popup
//
// Nothing that works on real books (merges, redirects, the Recycling Bin,
// the readers) ever matches a key: they look rows up by book uuids, and a
// key is never one.

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	seriesKeyPrefix = "series:"
	maxShelfKey     = 1024 // longest book_uuid a shelf takes (a series' key)
)

// seriesCard: what a series entry adds to its grid entry (Book.SeriesCard).
type seriesCard struct {
	Name  string `json:"name"`
	Count int    `json:"count"` // books in the series
	Cover string `json:"cover"` // the first book's uuid: the card's cover
	Href  string `json:"href"`  // the series page
}

func seriesShelfKey(name string) string { return seriesKeyPrefix + name }

// shelfSeriesName: the series a shelf entry's key names, if it's one.
func shelfSeriesName(key string) (string, bool) { return strings.CutPrefix(key, seriesKeyPrefix) }

// seriesReadingOrder: series index, then the oldest published, then title.
// TODO: dedupe with seriesLess (workstream E1) once both are merged.
func seriesReadingOrder(a, b catalogBook) int {
	if c := cmp.Compare(a.SeriesIndex, b.SeriesIndex); c != 0 {
		return c
	}
	if c := strings.Compare(a.PubDate, b.PubDate); c != 0 {
		return c
	}
	if c := strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)); c != 0 {
		return c
	}
	return cmp.Compare(a.ID, b.ID)
}

// resolvedSeries: a series as browsed (a group's name, or as the library
// spells it) and its books, unsorted.
type resolvedSeries struct {
	name    string
	grouped bool
	books   []catalogBook
}

// resolveSeries: the series each series key among keys names, by key, in
// one pass of the catalog. A key whose series has no books has none.
func resolveSeries(c *catalog, keys []string) map[string]*resolvedSeries {
	wants := map[string]*resolvedSeries{}      // key -> its series
	byMember := map[string][]*resolvedSeries{} // lower-cased series name -> who wants it
	for _, k := range keys {
		value, ok := shelfSeriesName(k)
		if !ok || value == "" || wants[k] != nil {
			continue
		}
		name, members, grouped := groups.resolve("series", value)
		w := &resolvedSeries{name: name, grouped: grouped}
		wants[k] = w
		for _, m := range members {
			byMember[strings.ToLower(m)] = append(byMember[strings.ToLower(m)], w)
		}
	}
	if len(wants) == 0 {
		return nil
	}
	ids := map[int32][]*resolvedSeries{}
	for id, n := range c.seriesNames {
		if ws := byMember[strings.ToLower(n)]; ws != nil {
			ids[id] = ws
			for _, w := range ws {
				if !w.grouped {
					w.name = n
				}
			}
		}
	}
	for _, b := range c.books {
		if b.Series == 0 || b.Removed {
			continue
		}
		for _, w := range ids[b.Series] {
			w.books = append(w.books, b)
		}
	}
	return wants
}

// seriesEntries: grid entries for the series keys among keys, by key.
// Keys of series with no books are left out.
func seriesEntries(c *catalog, keys []string) map[string]Book {
	wants := resolveSeries(c, keys)
	if len(wants) == 0 {
		return nil
	}
	out := make(map[string]Book, len(wants))
	for k, w := range wants {
		if len(w.books) == 0 {
			continue
		}
		first := slices.MinFunc(w.books, seriesReadingOrder)
		out[k] = Book{
			UUID: k, Title: w.name, Authors: first.Authors,
			PubDate: first.PubDate, Path: first.Path, LastModified: first.LastModified,
			SeriesCard: &seriesCard{Name: w.name, Count: len(w.books), Cover: first.UUID, Href: categoryByKey("series").href(w.name)},
		}
	}
	return out
}

// seriesEntry: the grid entry for one series as browsed, if it has books.
func seriesEntry(name string) (Book, bool) {
	k := seriesShelfKey(name)
	b, ok := seriesEntries(currentCatalog(), []string{k})[k]
	return b, ok
}

// seriesShelfMarks: the shelves u can put this series on, and whether it's
// on each (the series page's and book popup's "Series on shelves").
func seriesShelfMarks(u *User, name string) []shelfMark {
	out := []shelfMark{}
	for _, m := range shelfMarks(u, seriesShelfKey(name)) {
		if m.CanEdit {
			out = append(out, m)
		}
	}
	return out
}

// shelfSeriesKeyFor: the key for the series named in the path, after
// checking it's in the library (404 if not), as browsed.
func shelfSeriesKeyFor(w http.ResponseWriter, r *http.Request) (string, bool) {
	b, ok := seriesEntry(r.PathValue("name"))
	if !ok {
		http.Error(w, "no such series", http.StatusNotFound)
		return "", false
	}
	key := seriesShelfKey(b.SeriesCard.Name)
	if len(key) > maxShelfKey {
		http.Error(w, "series name too long", http.StatusBadRequest)
		return "", false
	}
	return key, true
}

func shelfAddSeriesHandler(w http.ResponseWriter, r *http.Request) {
	s, ok := shelfForEdit(w, r)
	if !ok {
		return
	}
	key, ok := shelfSeriesKeyFor(w, r)
	if !ok {
		return
	}
	if s.Count >= maxBooksPerShelf {
		http.Error(w, "this shelf is full", http.StatusConflict)
		return
	}
	if _, err := appDB.Exec(`INSERT OR IGNORE INTO shelf_books (shelf_id, book_uuid, added_at) VALUES (?, ?, ?)`, s.ID, key, nowUnix()); err != nil {
		http.Error(w, "could not add", http.StatusInternalServerError)
		return
	}
	_, _ = appDB.Exec(`UPDATE shelves SET updated_at = ? WHERE id = ?`, nowUnix(), s.ID)
	shelfChanged(s)
	w.WriteHeader(http.StatusNoContent)
}

// shelfRemoveSeriesHandler takes the key as given too, so a series that's
// gone from the library can still be taken off.
func shelfRemoveSeriesHandler(w http.ResponseWriter, r *http.Request) {
	s, ok := shelfForEdit(w, r)
	if !ok {
		return
	}
	keys := []any{seriesShelfKey(r.PathValue("name"))}
	if b, ok := seriesEntry(r.PathValue("name")); ok {
		keys = append(keys, seriesShelfKey(b.SeriesCard.Name))
	}
	if _, err := appDB.Exec(`DELETE FROM shelf_books WHERE shelf_id = ? AND book_uuid IN (`+placeholders(len(keys))+`)`,
		append([]any{s.ID}, keys...)...); err != nil {
		http.Error(w, "could not remove", http.StatusInternalServerError)
		return
	}
	shelfChanged(s)
	w.WriteHeader(http.StatusNoContent)
}

// seriesPopupBook: a line of the series popup's list.
type seriesPopupBook struct {
	UUID, Title, Index string
}

// SeriesPopupData is for the "series-popup" template (series-popup.html).
type SeriesPopupData struct {
	Name, Href, Label string
	Authors           []placeholderLink
	AuthorNames       string // for the cover's placeholder
	Cover             string // the first book's uuid
	CoverStamp        string
	CoverHue          int
	Books             []seriesPopupBook
	LoggedIn          bool
	Shelves           []shelfMark // the shelves u can put it on (series-shelf.ts)
	OnShelves         int
}

// GET /series-popup/{name...}: a shelf's series card in the book popup,
// stepped onto from a book beside it (modal.ts). The series as browsed.
func seriesPopupHandler(w http.ResponseWriter, r *http.Request) {
	k := seriesShelfKey(r.PathValue("name"))
	s := resolveSeries(currentCatalog(), []string{k})[k]
	if s == nil || len(s.books) == 0 {
		http.NotFound(w, r)
		return
	}
	books := slices.Clone(s.books)
	slices.SortFunc(books, seriesReadingOrder)
	d := SeriesPopupData{
		Name:       s.name,
		Href:       categoryByKey("series").href(s.name),
		Label:      seriesLabel(len(books)),
		Cover:      books[0].UUID,
		CoverStamp: books[0].LastModified,
		CoverHue:   coverHue(books[0].UUID), // the first book's, as its own popup has it
	}
	seen := map[string]bool{}
	var names []string
	for _, b := range books {
		d.Books = append(d.Books, seriesPopupBook{b.UUID, b.Title, strconv.FormatFloat(b.SeriesIndex, 'f', -1, 64)})
		for a := range strings.SplitSeq(b.Authors, " & ") {
			if a = strings.TrimSpace(a); a != "" && !seen[strings.ToLower(a)] {
				seen[strings.ToLower(a)] = true
				names = append(names, a)
				d.Authors = append(d.Authors, placeholderLink{a, categoryByKey("author").href(a)})
			}
		}
	}
	d.AuthorNames = strings.Join(names, ", ")
	if u := currentUser(r); u != nil {
		d.LoggedIn = true
		d.Shelves = seriesShelfMarks(u, s.name)
		for _, m := range d.Shelves {
			if m.Has {
				d.OnShelves++
			}
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Vary", "HX-Request")
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, d.Href, http.StatusFound) // not the popup: its page
		return
	}
	logErr(templates.ExecuteTemplate(w, "series-popup", d), "series popup")
}

// seriesLabel: "Series · 5 books", as on the card (utils.ts seriesLabel).
func seriesLabel(n int) string {
	if n == 1 {
		return "Series · 1 book"
	}
	return "Series · " + strconv.Itoa(n) + " books"
}
