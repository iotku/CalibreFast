package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/iotku/CalibreFast/pkg/langcodes"
)

// ---------------------------------------------------------------------------
// Category registry
// ---------------------------------------------------------------------------

// category describes one browsable Calibre link table.
type category struct {
	Key       string // singular id used in the groups store: "language"
	Plural    string // aggregate page + JSON API path: "languages"
	URLPrefix string // filtered page path: "lang" -> /lang/{value}
	Title     string // aggregate page title
	Label     string // singular, for UI text: "language"
	Table     string // entity table
	NameCol   string // display/value column in Table
	LinkTable string // books_<x>_link
	LinkCol   string // column in LinkTable referencing Table.id
	// ShowSingles: the aggregate page lists values with a single book too
	// (otherwise only the search box finds them).
	ShowSingles bool
	// SortCol: Table's column to order A–Z by ("Tolkien, J. R. R."), or ""
	// for NameCol.
	SortCol string
}

var categories = []*category{
	{"author", "authors", "author", "Authors", "author", "authors", "name", "books_authors_link", "author", false, "sort"},
	{"publisher", "publishers", "publisher", "Publishers", "publisher", "publishers", "name", "books_publishers_link", "publisher", false, "sort"},
	{"tag", "tags", "tag", "Tags", "tag", "tags", "name", "books_tags_link", "tag", false, ""},
	{"language", "languages", "lang", "Languages", "language", "languages", "lang_code", "books_languages_link", "lang_code", true, ""},
	{"series", "series", "series", "Series", "series", "series", "name", "books_series_link", "series", false, "sort"},
}

// bookOrder: how a filtered page lists its books. A series reads in order:
// as seriesCompare (series.go) has it.
func (c *category) bookOrder() string {
	if c.Key == "series" {
		return seriesOrderSQL
	}
	return "b.sort"
}

func categoryByKey(key string) *category {
	for _, c := range categories {
		if c.Key == key {
			return c
		}
	}
	return nil
}

func (c *category) href(name string) string {
	return "/" + c.URLPrefix + "/" + url.PathEscape(name)
}

// ---------------------------------------------------------------------------
// Page data
// ---------------------------------------------------------------------------

type AggregateItem struct {
	Name    string   `json:"name"`
	Count   int      `json:"count"`
	Slug    string   `json:"slug"`
	Display string   `json:"display,omitempty"` // human label when it differs from Name (languages)
	Members []string `json:"members,omitempty"` // set when Name is a group
	Sort    string   `json:"-"`                 // A–Z key when it isn't the label: an author's "Surname, First"
}

// AggregateDisplayItem is used by the server-rendered aggregate template.
type AggregateDisplayItem struct {
	Name    string // label shown
	Sub     string // raw value shown small when Name is a display name, e.g. "eng"
	Href    string // path-escaped link, e.g. "/author/Terry%20Pratchett"
	Count   int
	Grouped bool
	Tier    int    // 1–5, the cloud's text size (aggregateTiers)
	Sort    string // A–Z key, "" for Name
	// Status: how much of a series is here (seriesStatuses), nil when it
	// isn't counted; set per request, never cached.
	Status *seriesStatus
}

// AggregatePageData is passed to aggregate.html for server-side rendering.
type AggregatePageData struct {
	Title    string
	Active   string // top-bar nav item: the category's plural, e.g. "authors"
	Label    string // "author", for the search box
	Prefix   string // filtered page path prefix: "/author/"
	DataURL  string // every entry as JSON (/api/<plural>), for the search box
	Items    []AggregateDisplayItem
	Featured []FeaturedCard
	Discover []AggregateDisplayItem // a random few, single-book ones too
	Tiers    string                 // tier thresholds, "3,5,9,20", for the search box's results
	// Series only: how many are complete (seriesStatuses), and whether the
	// page lists only those (?complete=1).
	CompleteCount int
	CompleteOnly  bool
}

// FilteredPageData is passed to filtered.html.
type FilteredPageData struct {
	Title       string // site title
	Name        string // group name, or the raw value
	Display     string // heading: Name, or a language's display name
	Sub         string // raw value shown small next to Display when they differ
	SuggestName string // default for the editor's display-name field
	Category    string // category key, e.g. "language"
	Active      string // top-bar nav item, e.g. "languages" or "favorites"
	Label       string // "language"
	Key         string // value from the URL (used to address the group)
	Members     []string
	Grouped     bool
	GroupsError string // non-empty when grouping is read-only
	Writable    bool   // an admin, with -allow-writes (shows the group editor)
	Featured    bool   // on the aggregate page's Featured list
	Description string // the entry's description (aggregate_meta)
	ImageURL    string // the entry's picture, "" for none
	PageURL     string // API base URL; template appends &page={n}
	// Missing: placeholders among the books (placeholders.go), counted in
	// TotalBooks. NewPlaceholder: where admins start one for this page.
	Missing        int
	NewPlaceholder string
	// Hardcover: where editors identify this series, or this author's
	// series, on Hardcover (hardcover.go); "" when it isn't set up.
	Hardcover string
	// A series: how much of it is here (series_complete.go), and for
	// editors,
	SeriesStatus *seriesStatus
	CanVerify    bool
	// the highest number and number of books they gave it (0: none), and
	// the ones to offer.
	VerifiedTotal, VerifiedBooks int
	SuggestTotal, SuggestBooks   int
	TotalBooks                   int
	EmptyText                    string         // shown when there are no books (default: "No books found.")
	Series                       []AuthorSeries // an author's series, shown above the books
	Extra                        map[string]any // page-specific extras (e.g. shelf controls)
}

// SearchPageData is passed to search.html.
type SearchPageData struct {
	Title      string // site title
	Query      string // pre-filled query from URL
	TotalBooks int
}

// ---------------------------------------------------------------------------
// Caches — populated at startup, refreshed on book upload / metadata / group edits.
// ---------------------------------------------------------------------------

var (
	// aggregateGen changes whenever an aggregate cache is rebuilt: the
	// search box's /api/<plural>?v= URL, so a browser never searches a
	// stale copy.
	aggregateGen atomic.Uint64
	// aggregateDisplayCache: plural ("authors") -> aggregateDisplay
	aggregateDisplayCache sync.Map
	// aggregateJSONCache: plural -> pre-serialised []AggregateItem
	aggregateJSONCache sync.Map
	// filteredTotalCache: "key/lowercased resolved name" -> book count
	filteredTotalCache sync.Map
)

// booksPerPage is how many books one page of JSON holds, in every
// paginated view. The grid lays books out by index, not by page
// (web/app-htmx.ts), so it needn't divide the column counts.
const booksPerPage = 60

// preloadCaches populates the aggregate caches at startup.
func preloadCaches() {
	for _, c := range categories {
		refreshAggregateCache(c)
	}
	log.Println("caches: aggregates pre-loaded")
}

func refreshAggregateCache(c *category) {
	items := queryAggregateItems(c)
	if data, err := json.Marshal(items); err == nil {
		aggregateJSONCache.Store(c.Plural, data)
	}
	aggregateDisplayCache.Store(c.Plural, computeAggregateDisplay(c, items))
	aggregateGen.Add(1)
}

// refreshAllCaches rebuilds every cache. Called after book upload, metadata
// edits and group edits, where the exact scope of changes is hard to predict.
func refreshAllCaches() {
	for _, c := range categories {
		refreshAggregateCache(c)
	}
	filteredTotalCache.Clear()
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toArgs(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// countBooks returns the number of distinct books linked to any of values.
func countBooks(c *category, values []string) int {
	if len(values) == 0 {
		return 0
	}
	query := fmt.Sprintf(`
		SELECT COUNT(DISTINCT x.book)
		FROM %s x
		JOIN %s t ON t.id = x.%s
		WHERE t.%s IN (%s)`,
		c.LinkTable, c.Table, c.LinkCol, c.NameCol, placeholders(len(values)))
	var n int
	if err := calibreDB.QueryRow(query, toArgs(values)...).Scan(&n); err != nil {
		log.Printf("countBooks(%s): %v", c.Key, err)
		return 0
	}
	return n
}

// queryRawItems lists every value in the category with its book count.
func queryRawItems(c *category) []AggregateItem {
	sortExpr := "''"
	if c.SortCol != "" {
		sortExpr = "COALESCE(t." + c.SortCol + ", '')"
	}
	query := fmt.Sprintf(`
		SELECT t.%s, %s, COUNT(x.book) AS count
		FROM %s t
		JOIN %s x ON x.%s = t.id
		GROUP BY t.id
		ORDER BY count DESC`,
		c.NameCol, sortExpr, c.Table, c.LinkTable, c.LinkCol)

	rows, err := calibreDB.Query(query)
	if err != nil {
		log.Printf("queryRawItems(%s): %v", c.Key, err)
		return nil
	}
	defer func(rows *sql.Rows) {
		logErr(rows.Close(), "failed to close rows in queryRawItems")
	}(rows)

	items := make([]AggregateItem, 0)
	for rows.Next() {
		var item AggregateItem
		if err := rows.Scan(&item.Name, &item.Sort, &item.Count); err != nil {
			continue
		}
		item.Slug = item.Name
		if item.Sort == item.Name {
			item.Sort = ""
		}
		if c.Key == "language" {
			if name, ok := langcodes.DisplayName(item.Name); ok {
				item.Display = name
			}
		}
		items = append(items, item)
	}
	return items
}

// queryAggregateItems lists values with grouped ones collapsed into a single
// entry per group, counting distinct books so overlaps aren't double-counted.
func queryAggregateItems(c *category) []AggregateItem {
	raw := queryRawItems(c)
	gs := groups.all(c.Key)
	if len(gs) == 0 {
		return raw
	}

	memberOf := map[string]int{}
	for i, g := range gs {
		for _, m := range g.Members {
			memberOf[strings.ToLower(m)] = i
		}
	}

	items := make([]AggregateItem, 0, len(raw))
	present := make([]bool, len(gs))
	for _, it := range raw {
		if i, ok := memberOf[strings.ToLower(it.Name)]; ok {
			present[i] = true
			continue
		}
		items = append(items, it)
	}
	for i, g := range gs {
		if !present[i] {
			continue // no books in any member: nothing to browse
		}
		items = append(items, AggregateItem{
			Name:    g.Name,
			Slug:    g.Name,
			Count:   countBooks(c, g.Members),
			Members: g.Members,
		})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Count > items[j].Count })
	return items
}

// aggregateDisplay is what the aggregate page shows of a category, cached.
type aggregateDisplay struct {
	Items []AggregateDisplayItem // the cloud: values with more than one book (or all)
	All   []AggregateItem        // every value, for the Discover sample
	Tiers [4]int                 // minimum counts of tiers 2–5
}

// aggregateTiers splits counts (sorted, largest first) into five text
// sizes by rank, not by ratio to the largest: with a linear scale one
// huge entry shrinks everything else to the same small size. Tier 5 is
// about the top 1%, 4 down to 5%, 3 down to 15%, 2 down to 40%, 1 the
// rest. Equal counts share a tier, and each tier needs more books than
// the one below, so a list of all-equal counts is all tier 1.
func aggregateTiers(counts []int) (t [4]int) {
	n := len(counts)
	if n == 0 {
		return [4]int{2, 3, 4, 5}
	}
	at := func(frac float64) int { return counts[min(n-1, int(frac*float64(n)))] }
	t = [4]int{at(0.40), at(0.15), at(0.05), at(0.01)}
	lowest := counts[n-1]
	if t[0] <= lowest {
		t[0] = lowest + 1
	}
	for i := 1; i < len(t); i++ {
		if t[i] <= t[i-1] {
			t[i] = t[i-1] + 1
		}
	}
	return t
}

// tierOf is count's tier, 1–5, under thresholds t.
func tierOf(t [4]int, count int) int {
	tier := 1
	for _, lo := range t {
		if count >= lo {
			tier++
		}
	}
	return tier
}

func displayItem(c *category, item AggregateItem, tiers [4]int) AggregateDisplayItem {
	label := item.Name
	if item.Display != "" {
		label = item.Display
	}
	sub := ""
	if label != item.Name {
		sub = item.Name
	}
	return AggregateDisplayItem{
		Name:    label,
		Sub:     sub,
		Href:    c.href(item.Name),
		Count:   item.Count,
		Grouped: len(item.Members) > 0,
		Tier:    tierOf(tiers, item.Count),
		Sort:    item.Sort,
	}
}

func computeAggregateDisplay(c *category, items []AggregateItem) aggregateDisplay {
	shown := items
	if !c.ShowSingles {
		shown = make([]AggregateItem, 0, len(items))
		for _, item := range items {
			if item.Count >= 2 { // the rest: found by the search box and Discover
				shown = append(shown, item)
			}
		}
	}
	counts := make([]int, len(shown))
	for i, item := range shown {
		counts[i] = item.Count // sorted descending, as items is
	}
	d := aggregateDisplay{All: items, Tiers: aggregateTiers(counts)}
	d.Items = make([]AggregateDisplayItem, len(shown))
	for i, item := range shown {
		d.Items[i] = displayItem(c, item, d.Tiers)
	}
	return d
}

func getAggregateData(c *category) aggregateDisplay {
	if val, ok := aggregateDisplayCache.Load(c.Plural); ok {
		return val.(aggregateDisplay)
	}
	refreshAggregateCache(c)
	val, _ := aggregateDisplayCache.Load(c.Plural)
	d, _ := val.(aggregateDisplay)
	return d
}

func getAggregateDisplay(c *category) []AggregateDisplayItem {
	return getAggregateData(c).Items
}

// discoverCount: how many entries the Discover row shows. It shows only
// when there are more than discoverMin values: below that the cloud has
// room for all of them anyway.
const (
	discoverCount = 12
	discoverMin   = 3 * discoverCount
)

// discoverSample picks n entries at random from every value, single-book
// ones too: the long tail outnumbers the rest, so most are ones the cloud
// shows small or not at all.
func discoverSample(c *category, d aggregateDisplay, n int) []AggregateDisplayItem {
	if len(d.All) <= discoverMin {
		return nil
	}
	out := make([]AggregateDisplayItem, 0, n)
	for _, i := range rand.Perm(len(d.All))[:n] {
		out = append(out, displayItem(c, d.All[i], d.Tiers))
	}
	return out
}

// getFilteredTotal returns the book count for a resolved entity, cached.
func getFilteredTotal(c *category, name string, members []string) int {
	key := c.Key + "/" + strings.ToLower(name)
	if val, ok := filteredTotalCache.Load(key); ok {
		return val.(int)
	}
	total := countBooks(c, members)
	filteredTotalCache.Store(key, total)
	return total
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// pathValue extracts the value after "/<prefix>/" (already URL-decoded).
func pathValue(r *http.Request) string {
	parts := strings.SplitN(r.URL.Path, "/", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// aggregateHandler serves /api/<plural> JSON from cache.
func aggregateHandler(w http.ResponseWriter, c *category) {
	data, ok := aggregateJSONCache.Load(c.Plural)
	if !ok {
		refreshAggregateCache(c)
		data, _ = aggregateJSONCache.Load(c.Plural)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=3600, stale-while-revalidate=86400")
	if b, ok := data.([]byte); ok {
		_, _ = w.Write(b)
	} else {
		_, _ = w.Write([]byte("[]"))
	}
}

// filteredBooksHandler serves a page of books for a value or its group.
func filteredBooksHandler(w http.ResponseWriter, r *http.Request, c *category) {
	value := pathValue(r)
	if value == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	_, members, _ := groups.resolve(c.Key, value)

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * booksPerPage

	// Books not in the library yet go among the rest (placeholders.go).
	if phs := placeholdersFor(c, members); len(phs) > 0 {
		books, err := filteredPageWithPlaceholders(c, members, phs, page)
		if err != nil {
			log.Println("filtered books error:", err)
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, books)
		return
	}

	query := fmt.Sprintf(`
		SELECT b.uuid, b.title, `+bookAuthorsSQL+` AS authors,
			b.pubdate, b.path, b.last_modified
		FROM books b
		JOIN %s x ON x.book = b.id
		JOIN %s t ON t.id = x.%s
		WHERE t.%s IN (%s)
		GROUP BY b.id
		ORDER BY %s
		LIMIT ? OFFSET ?`,
		c.LinkTable, c.Table, c.LinkCol, c.NameCol, placeholders(len(members)), c.bookOrder())

	args := append(toArgs(members), booksPerPage, offset)
	rows, err := calibreDB.Query(query, args...)
	if err != nil {
		log.Println("filtered books error:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer func(rows *sql.Rows) {
		logErr(rows.Close(), "failed to close rows in filteredBooksHandler")
	}(rows)

	logErr(encodeBooksJSONFromRows(rows, w), "filtered books encode")
}

// filteredPageHandler serves the HTML shell (or the JSON API with ?api=1).
func filteredPageHandler(c *category) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api") == "1" {
			filteredBooksHandler(w, r, c)
			return
		}
		value := pathValue(r)
		if value == "" {
			http.NotFound(w, r)
			return
		}
		name, members, grouped := groups.resolve(c.Key, value)
		display, suggest := name, name
		if c.Key == "language" && !grouped {
			if dn, ok := langcodes.DisplayName(name); ok {
				display = dn
				// Grouping "en" with others defaults to "English", not "en (US)".
				suggest, _ = langcodes.DisplayName(strings.SplitN(strings.ReplaceAll(name, "_", "-"), "-", 2)[0])
			}
		}
		sub := ""
		if display != name {
			sub = name
		}
		data := FilteredPageData{
			Title:       SiteTitle,
			Name:        name,
			Display:     display,
			Sub:         sub,
			SuggestName: suggest,
			Category:    c.Key,
			Active:      c.Plural,
			Label:       c.Label,
			Key:         value,
			Members:     members,
			Grouped:     grouped,
			PageURL:     c.href(value) + "?api=1",
			TotalBooks:  getFilteredTotal(c, name, members),
			Writable:    canAdminLibrary(r), // groups and featured entries: admins
		}
		inLibrary := data.TotalBooks
		data.Missing = len(placeholdersFor(c, members))
		data.TotalBooks += data.Missing
		if data.Missing > 0 {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if canEditLibrary(r) && (c.Key == "series" || c.Key == "author") {
			q := url.Values{}
			if c.Key == "series" {
				q.Set("series", name)
			} else {
				q.Set("authors", name)
			}
			data.NewPlaceholder = "/missing/new?" + q.Encode()
			if hardcoverEnabled() {
				if c.Key == "series" {
					data.Hardcover = hcReviewURL(name)
				} else {
					data.Hardcover = "/missing/hardcover/author?" + url.Values{"author": {name}}.Encode()
				}
			}
		}
		if c.Key == "author" {
			data.Series = authorSeries(members)
			if st := seriesStatuses(); len(st) > 0 {
				for i := range data.Series {
					data.Series[i].Status = statusPtr(st, data.Series[i].Name)
				}
			}
		}
		if c.Key == "series" {
			if st, ok := seriesStatusOf(name); ok {
				data.SeriesStatus = &st
			}
			if data.CanVerify = canEditLibrary(r) && inLibrary > 0; data.CanVerify {
				v := seriesVerified(name)
				data.VerifiedTotal, data.VerifiedBooks = v.Total, v.Books
				data.SuggestTotal, data.SuggestBooks = v.Total, v.Books
				if v.Total == 0 && v.Books == 0 {
					// Nothing said yet: offer what the library has. A series
					// with no book numbered past #1 is counted by its books.
					books, top, numbered := seriesShape(members)
					if numbered {
						data.SuggestTotal = top
					} else {
						data.SuggestBooks = books
					}
				}
			}
		}
		if m, ok := aggregateMetaFor(c.Key, name); ok {
			data.Description, data.ImageURL = m.Description, m.imageURL()
		}
		if data.Writable {
			data.Featured = isFeaturedAggregate(c.Key, name)
		}
		if groups.readOnly != nil {
			data.GroupsError = groups.readOnly.Error()
		}
		// A tag can be pinned to the user's home page (shelf_pins.go), and
		// so can a series (series_pins.go).
		if u := currentUser(r); u != nil && c.Key == "tag" && inLibrary > 0 {
			w.Header().Set("Cache-Control", "private, no-store")
			data.Extra = map[string]any{"TagPin": name, "Pinned": tagPinnedFor(u, name)}
		}
		// A series can be pinned to home (series_pins.go) and go on a shelf,
		// whole (shelf_series.go).
		if u := currentUser(r); u != nil && c.Key == "series" && inLibrary > 0 {
			w.Header().Set("Cache-Control", "private, no-store")
			if data.Extra == nil {
				data.Extra = map[string]any{}
			}
			data.Extra["SeriesPin"] = name
			data.Extra["Pinned"] = seriesPinnedFor(u, name)
			data.Extra["SeriesShelf"] = name
			data.Extra["SeriesShelves"] = seriesShelfMarks(u, name)
		}
		logErr(templates.ExecuteTemplate(w, "filtered.html", data), "failed to execute filtered template")
	}
}

// aggregatePageHandler serves the server-rendered tag cloud.
func aggregatePageHandler(c *category) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d := getAggregateData(c)
		tiers := make([]string, len(d.Tiers))
		for i, t := range d.Tiers {
			tiers[i] = strconv.Itoa(t)
		}
		items, featured, discover := d.Items, featuredCards(c), discoverSample(c, d, discoverCount)
		completeOnly, completeCount := false, 0
		if st := seriesStatuses(); c.Key == "series" && len(st) > 0 {
			complete := func(name string) bool { return st[strings.ToLower(name)].Complete }
			var all []AggregateDisplayItem
			for _, it := range d.All {
				if complete(it.Name) {
					all = append(all, displayItem(c, it, d.Tiers))
				}
			}
			if completeOnly = r.URL.Query().Get("complete") == "1"; completeOnly {
				items, featured, discover = all, nil, nil
			} else {
				items = slices.Clone(items)
			}
			for i := range items {
				items[i].Status = statusPtr(st, items[i].Name)
			}
			for i := range discover {
				discover[i].Status = statusPtr(st, discover[i].Name)
			}
			completeCount = len(all)
		}
		data := AggregatePageData{
			Title:    c.Title,
			Active:   c.Plural,
			Label:    c.Label,
			Prefix:   "/" + c.URLPrefix + "/",
			DataURL:  fmt.Sprintf("/api/%s?v=%d", c.Plural, aggregateGen.Load()),
			Items:    items,
			Featured: featured,
			Discover: discover,
			Tiers:    strings.Join(tiers, ","),

			CompleteCount: completeCount,
			CompleteOnly:  completeOnly,
		}
		w.Header().Set("Cache-Control", "no-cache")
		if err := templates.ExecuteTemplate(w, "aggregate.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// groupValuesHandler: GET /api/groups/{cat}/values — every value in the
// library for the editor's suggestion list (the only source it offers).
func groupValuesHandler(w http.ResponseWriter, r *http.Request) {
	c := categoryByKey(r.PathValue("cat"))
	if c == nil {
		http.NotFound(w, r)
		return
	}
	type value struct {
		Value string `json:"value"`
		Label string `json:"label,omitempty"` // e.g. "English" for "eng"
		Count int    `json:"count"`           // books with it (the editor's common languages)
	}
	raw := queryRawItems(c)
	names := make([]value, len(raw))
	for i, it := range raw {
		names[i] = value{Value: it.Name, Label: it.Display, Count: it.Count}
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i].Value) < strings.ToLower(names[j].Value) })
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(names), "encode group values")
}

// groupEdit is what the editor on a filtered page saves.
type groupEdit struct {
	Name        string    `json:"name"`
	Members     *[]string `json:"members"`     // nil: the grouping is left as it is; empty: dissolved
	Description *string   `json:"description"` // nil: left as it is
	RemoveImage bool      `json:"removeImage"`
}

// readGroupEdit reads a group edit: a JSON body, or a multipart form with
// the JSON in "data" and a new picture (raster or SVG) in "image".
func readGroupEdit(r *http.Request) (e groupEdit, img *aggImage, err error) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		err = json.NewDecoder(r.Body).Decode(&e)
		return
	}
	r.Body = http.MaxBytesReader(nil, r.Body, coverUploadMaxBytes+1<<20)
	if err = r.ParseMultipartForm(coverUploadMaxBytes); err != nil {
		return e, nil, fmt.Errorf("failed to parse form: %w", err)
	}
	if err = json.Unmarshal([]byte(r.FormValue("data")), &e); err != nil {
		return
	}
	data, ferr := formImageFile(r, "image")
	switch {
	case errors.Is(ferr, http.ErrMissingFile):
		return e, nil, nil
	case ferr != nil:
		return e, nil, ferr
	}
	img, err = aggregateImage(data)
	return
}

// groupPutHandler: PUT /api/groups/{cat}/{value...} with {"name","members"}
// replaces the group that value resolves to (empty members removes it), and
// with "description", "removeImage" and an "image" file (multipart) sets
// the entry's description and picture (aggregate_meta.go). A single value
// works the same: leave out "members" and only the meta changes.
func groupPutHandler(w http.ResponseWriter, r *http.Request) {
	c := categoryByKey(r.PathValue("cat"))
	value := r.PathValue("value")
	if c == nil || value == "" {
		http.NotFound(w, r)
		return
	}
	e, img, err := readGroupEdit(r)
	if err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if e.Description != nil && len(*e.Description) > aggregateDescriptionMax {
		http.Error(w, fmt.Sprintf("the description is too long (at most %d characters)", aggregateDescriptionMax), http.StatusBadRequest)
		return
	}

	oldName, _, _ := groups.resolve(c.Key, value)
	newName := oldName
	next := value // where the page should go next
	var saved Group
	if e.Members != nil {
		g := Group{Name: e.Name, Members: *e.Members}
		saved, err = groups.set(c.Key, value, g)
		switch {
		case errors.Is(err, errGroupNameTaken):
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case errors.Is(err, errGroupsReadOnly):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		case err != nil:
			log.Println("save groups:", err)
			http.Error(w, "failed to save groups: "+err.Error(), http.StatusInternalServerError)
			return
		}
		refreshAllCaches()
		if c.Key == "tag" {
			refreshTagRows() // groups decide what a tag row is
		}
		// The group, or the original value if the group was dissolved.
		if saved.Name != "" {
			next = saved.Name
		} else if ms := cleanMembers(g.Members); len(ms) > 0 {
			next = ms[0]
		}
		newName, _, _ = groups.resolve(c.Key, next)
		renameAggregate(c.Key, oldName, newName)
	}

	if err := setAggregateMeta(c.Key, newName, metaEdit{Description: e.Description, Image: img, RemoveImage: e.RemoveImage}); err != nil {
		log.Println("save aggregate meta:", err)
		http.Error(w, "failed to save the description or picture: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(map[string]any{
		"group": saved,
		"url":   c.href(next),
	}), "encode group")
}

// registerCategoryRoutes wires aggregate pages, filtered pages, JSON APIs and
// the group editor API for every category.
func registerCategoryRoutes() {
	for _, c := range categories {
		c := c
		http.HandleFunc("/"+c.Plural, aggregatePageHandler(c))
		http.HandleFunc("/"+c.URLPrefix+"/", filteredPageHandler(c))
		http.HandleFunc("/api/"+c.Plural, func(w http.ResponseWriter, r *http.Request) {
			aggregateHandler(w, c)
		})
	}
	http.HandleFunc("GET /api/groups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		logErr(json.NewEncoder(w).Encode(groups.snapshot()), "encode groups")
	})
	http.HandleFunc("GET /api/groups/{cat}/values", groupValuesHandler)
	http.HandleFunc("GET /api/languages/codes", languageCodesHandler)
	http.HandleFunc("PUT /api/groups/{cat}/{value...}", requireWrites(groupPutHandler))
	http.HandleFunc("PUT /api/aggregates/{cat}/featured/{value...}", requireWrites(featuredHandler))
	http.HandleFunc("DELETE /api/aggregates/{cat}/featured/{value...}", requireWrites(featuredHandler))
	http.HandleFunc("GET /"+aggregateImagesDir+"/{file}", aggregateImageHandler)
}
