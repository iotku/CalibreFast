package main

// Sort options for the book grids: Latest, the filtered pages (an author,
// tag, series, …) and search. The order is ?sort=<key>, one of a fixed list;
// anything else is the page's default. The page shows a select above the
// grid (templates/grid-sort.html, web/grid-sort.ts), which also remembers the
// choice in the browser.
//
// Latest and search sort the in-memory catalog (no metadata.db reads); the
// filtered pages, which read metadata.db anyway, map the key to a fixed SQL
// ORDER BY (aggregate.go). Shelves and the /me lists keep the order their
// owner arranged.

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/iotku/CalibreFast/pkg/textkey"
)

// Sort keys. User input is only ever compared against these.
const (
	sortByBest      = "best"      // search: relevance
	sortByAdded     = "added"     // newest added first
	sortByTitle     = "title"     // A–Z by title sort ("Hobbit, The")
	sortByAuthor    = "author"    // A–Z by the first author's sort name
	sortBySeries    = "series"    // by series, in reading order; books in none after
	sortByPublished = "published" // newest published first; unknown dates last
	sortByRating    = "rating"    // highest rated first; unrated last (rating.go)
)

var sortLabels = map[string]string{
	sortByBest:      "Best match",
	sortByAdded:     "Recently added",
	sortByTitle:     "Title",
	sortByAuthor:    "Author",
	sortBySeries:    "Series",
	sortByPublished: "Published",
	sortByRating:    "Rating",
}

// The sorts each kind of page offers, its default first.
var (
	// The Latest page is only ever newest added first, for now: the grid
	// names the order, without a select (templates/grid-sort.html). The
	// API still sorts it the other ways.
	latestSorts    = []string{sortByAdded}
	latestAPISorts = []string{sortByAdded, sortByTitle, sortByAuthor, sortBySeries, sortByPublished, sortByRating}
	filteredSorts  = []string{sortByTitle, sortByAuthor, sortBySeries, sortByAdded, sortByPublished, sortByRating}
	seriesSorts    = []string{sortBySeries, sortByTitle, sortByAdded, sortByPublished, sortByRating}
	searchSorts    = []string{sortByBest, sortByTitle, sortByAuthor, sortBySeries, sortByAdded, sortByPublished, sortByRating}
)

// pickSort is the requested sort if it's one of allowed, else the default
// (allowed[0]).
func pickSort(r *http.Request, allowed []string) string {
	if s := r.URL.Query().Get("sort"); slices.Contains(allowed, s) {
		return s
	}
	return allowed[0]
}

// gridSort is what templates/grid-sort.html needs.
type gridSort struct {
	Kind    string // the localStorage key's suffix: latest, author, tag, …
	Current string
	Default string
	Options []sortOption
}

type sortOption struct {
	Key, Label string
}

func newGridSort(kind, current string, allowed []string) gridSort {
	g := gridSort{Kind: kind, Current: current, Default: allowed[0]}
	for _, k := range allowed {
		g.Options = append(g.Options, sortOption{k, sortLabels[k]})
	}
	return g
}

// ---------------------------------------------------------------------------
// Sorting the catalog
// ---------------------------------------------------------------------------

// sortKeys: each book's folded keys, made once per catalog.
type sortKeys struct {
	cat    *catalog
	title  []string
	author []string
	series []string // "" when in none
	pub    []string // "" when unknown
}

var sortKeyCache struct {
	sync.Mutex
	keys   *sortKeys
	orders map[string][]int32 // Latest, by sort key
}

func catalogSortKeys(c *catalog) *sortKeys {
	sortKeyCache.Lock()
	defer sortKeyCache.Unlock()
	if k := sortKeyCache.keys; k != nil && k.cat == c {
		return k
	}
	n := len(c.books)
	k := &sortKeys{cat: c, title: make([]string, n), author: make([]string, n), series: make([]string, n), pub: make([]string, n)}
	for i := range c.books {
		b := &c.books[i]
		k.title[i] = textkey.Fold(cmp.Or(b.Sort, b.Title))
		if len(b.AuthorIDs) > 0 {
			id := b.AuthorIDs[0]
			k.author[i] = textkey.Fold(cmp.Or(c.authorSorts[id], c.authorNames[id]))
		}
		if b.Series != 0 {
			k.series[i] = textkey.Fold(c.seriesNames[b.Series])
		}
		k.pub[i] = seriesDate(b.PubDate)
	}
	sortKeyCache.keys = k
	sortKeyCache.orders = map[string][]int32{}
	return k
}

// sortSlots sorts slots (catalog c's) by key, in place and stably: books
// equal by key keep their order (newest added first, for Latest; relevance,
// for search).
func sortSlots(c *catalog, slots []int32, key string) {
	if key == sortByBest || key == sortByAdded || len(slots) < 2 {
		return // already in that order
	}
	k := catalogSortKeys(c)
	books := c.books
	// Empty keys (no author, no series, no date) go last.
	last := func(a, b string) int {
		switch {
		case a == b:
			return 0
		case a == "":
			return 1
		case b == "":
			return -1
		}
		return strings.Compare(a, b)
	}
	var less func(a, b int32) int
	switch key {
	case sortByTitle:
		less = func(a, b int32) int { return strings.Compare(k.title[a], k.title[b]) }
	case sortByAuthor:
		less = func(a, b int32) int {
			return cmp.Or(last(k.author[a], k.author[b]), strings.Compare(k.title[a], k.title[b]))
		}
	case sortBySeries:
		less = func(a, b int32) int {
			return cmp.Or(last(k.series[a], k.series[b]), cmp.Compare(books[a].SeriesIndex, books[b].SeriesIndex),
				strings.Compare(k.title[a], k.title[b]))
		}
	case sortByPublished:
		less = func(a, b int32) int {
			pa, pb := k.pub[a], k.pub[b]
			switch {
			case pa == pb:
				return 0
			case pa == "":
				return 1 // unknown dates last
			case pb == "":
				return -1
			}
			return strings.Compare(pb, pa) // newest first
		}
	case sortByRating:
		less = func(a, b int32) int {
			return cmp.Or(cmp.Compare(books[b].Rating, books[a].Rating), strings.Compare(k.title[a], k.title[b]))
		}
	default:
		return
	}
	slices.SortStableFunc(slots, less)
}

// latestOrder is every book in Latest sorted by key, cached per catalog.
func latestOrder(c *catalog, key string) []int32 {
	catalogSortKeys(c) // resets the cache for a new catalog
	sortKeyCache.Lock()
	if o, ok := sortKeyCache.orders[key]; ok && sortKeyCache.keys.cat == c {
		sortKeyCache.Unlock()
		return o
	}
	sortKeyCache.Unlock()
	o := c.latest()
	sortSlots(c, o, key)
	sortKeyCache.Lock()
	if sortKeyCache.keys != nil && sortKeyCache.keys.cat == c {
		sortKeyCache.orders[key] = o
	}
	sortKeyCache.Unlock()
	return o
}

// latestSortedHandler: GET /api/latest?sort=…&page=N, a page of Latest in
// another order than the page files' (newest added first).
func latestSortedHandler(w http.ResponseWriter, r *http.Request) {
	key := pickSort(r, latestAPISorts)
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	c := currentCatalog()
	order := latestOrder(c, key)
	writeJSON(w, http.StatusOK, pageOfSlots(c, order, page))
}

// pageOfSlots is page (from 1) of slots as grid entries.
func pageOfSlots(c *catalog, slots []int32, page int) []Book {
	books := make([]Book, 0, booksPerPage)
	from := (page - 1) * booksPerPage
	if from < 0 || from >= len(slots) {
		return books
	}
	for _, s := range slots[from:min(from+booksPerPage, len(slots))] {
		b := c.books[s].Book
		uuidPathIndex.Store(b.UUID, b.Path)
		books = append(books, b)
	}
	return books
}
