package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
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
}

var categories = []*category{
	{"author", "authors", "author", "Authors", "author", "authors", "name", "books_authors_link", "author"},
	{"publisher", "publishers", "publisher", "Publishers", "publisher", "publishers", "name", "books_publishers_link", "publisher"},
	{"tag", "tags", "tag", "Tags", "tag", "tags", "name", "books_tags_link", "tag"},
	{"language", "languages", "lang", "Languages", "language", "languages", "lang_code", "books_languages_link", "lang_code"},
	{"series", "series", "series", "Series", "series", "series", "name", "books_series_link", "series"},
}

// bookOrder: how a filtered page lists its books. A series reads in order.
func (c *category) bookOrder() string {
	if c.Key == "series" {
		return "b.series_index, b.sort"
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
}

// AggregateDisplayItem is used by the server-rendered aggregate template.
type AggregateDisplayItem struct {
	Name     string // label shown
	Sub      string // raw value shown small when Name is a display name, e.g. "eng"
	Href     string // path-escaped link, e.g. "/author/Terry%20Pratchett"
	Count    int
	Grouped  bool
	Members  string // "en, eng, en-US" when grouped
	FontSize string // e.g. "14.5"
	Opacity  string // e.g. "0.60"
}

// AggregatePageData is passed to aggregate.html for server-side rendering.
type AggregatePageData struct {
	Title  string
	Active string // top-bar nav item: the category's plural, e.g. "authors"
	Items  []AggregateDisplayItem
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
	Writable    bool   // server started with -allow-writes (shows the group editor)
	PageURL     string // API base URL; template appends &page={n}
	TotalBooks  int
	EmptyText   string         // shown when there are no books (default: "No books found.")
	Extra       map[string]any // page-specific extras (e.g. shelf controls)
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
	// aggregateDisplayCache: plural ("authors") -> []AggregateDisplayItem
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
	query := fmt.Sprintf(`
		SELECT t.%s, COUNT(x.book) AS count
		FROM %s t
		JOIN %s x ON x.%s = t.id
		GROUP BY t.id
		ORDER BY count DESC`,
		c.NameCol, c.Table, c.LinkTable, c.LinkCol)

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
		if err := rows.Scan(&item.Name, &item.Count); err != nil {
			continue
		}
		item.Slug = item.Name
		if c.Key == "language" {
			if name, ok := languageDisplayName(item.Name); ok {
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

func computeAggregateDisplay(c *category, items []AggregateItem) []AggregateDisplayItem {
	if len(items) == 0 {
		return nil
	}
	maxCount := items[0].Count // sorted descending
	if maxCount < 1 {
		maxCount = 1
	}
	display := make([]AggregateDisplayItem, len(items))
	for i, item := range items {
		ratio := float64(item.Count) / float64(maxCount)
		label := item.Name
		if item.Display != "" {
			label = item.Display
		}
		sub := ""
		if label != item.Name {
			sub = item.Name
		}
		display[i] = AggregateDisplayItem{
			Name:     label,
			Sub:      sub,
			Href:     c.href(item.Name),
			Count:    item.Count,
			Grouped:  len(item.Members) > 0,
			Members:  strings.Join(item.Members, ", "),
			FontSize: fmt.Sprintf("%.1f", 12+ratio*24),
			Opacity:  fmt.Sprintf("%.2f", 0.4+ratio*0.6),
		}
	}
	return display
}

func getAggregateDisplay(c *category) []AggregateDisplayItem {
	if val, ok := aggregateDisplayCache.Load(c.Plural); ok {
		return val.([]AggregateDisplayItem)
	}
	refreshAggregateCache(c)
	val, _ := aggregateDisplayCache.Load(c.Plural)
	items, _ := val.([]AggregateDisplayItem)
	return items
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
			if dn, ok := languageDisplayName(name); ok {
				display = dn
				// Grouping "en" with others defaults to "English", not "en (US)".
				suggest, _ = languageDisplayName(strings.SplitN(strings.ReplaceAll(name, "_", "-"), "-", 2)[0])
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
			Writable:    canEditLibrary(r),
		}
		if groups.readOnly != nil {
			data.GroupsError = groups.readOnly.Error()
		}
		// A tag can be pinned to the user's home page (shelf_pins.go).
		if u := currentUser(r); u != nil && c.Key == "tag" && data.TotalBooks > 0 {
			w.Header().Set("Cache-Control", "private, no-store")
			data.Extra = map[string]any{"TagPin": name, "Pinned": tagPinnedFor(u, name)}
		}
		logErr(templates.ExecuteTemplate(w, "filtered.html", data), "failed to execute filtered template")
	}
}

// aggregatePageHandler serves the server-rendered tag cloud.
func aggregatePageHandler(c *category) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := AggregatePageData{Title: c.Title, Active: c.Plural, Items: getAggregateDisplay(c)}
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
	}
	raw := queryRawItems(c)
	names := make([]value, len(raw))
	for i, it := range raw {
		names[i] = value{Value: it.Name, Label: it.Display}
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i].Value) < strings.ToLower(names[j].Value) })
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(names), "encode group values")
}

// groupPutHandler: PUT /api/groups/{cat}/{value...} with {"name","members"}
// replaces the group that value resolves to. Empty members removes it.
func groupPutHandler(w http.ResponseWriter, r *http.Request) {
	c := categoryByKey(r.PathValue("cat"))
	value := r.PathValue("value")
	if c == nil || value == "" {
		http.NotFound(w, r)
		return
	}
	var g Group
	if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	saved, err := groups.set(c.Key, value, g)
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

	// Where the page should go next: the group, or the original value if the
	// group was dissolved.
	next := value
	if saved.Name != "" {
		next = saved.Name
	} else if len(g.Members) > 0 {
		next = cleanMembers(g.Members)[0]
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
}
