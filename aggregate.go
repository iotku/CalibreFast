package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

type AggregateItem struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Slug  string `json:"slug"`
}

// AggregateDisplayItem is used by the server-rendered aggregate template.
type AggregateDisplayItem struct {
	Name     string
	Href     string // path-escaped link, e.g. "/author/Terry%20Pratchett"
	Count    int
	FontSize string // e.g. "14.5"
	Opacity  string // e.g. "0.60"
}

// AggregatePageData is passed to aggregate.html for server-side rendering.
type AggregatePageData struct {
	Title      string // page title, e.g. "Authors"
	LinkPrefix string // singular form for URLs, e.g. "author"
	Items      []AggregateDisplayItem
}

// FilteredPageData is passed to filtered.html.
type FilteredPageData struct {
	Title      string // site title (for modal title prefix)
	Name       string // entity name for display, e.g. "Terry Pratchett"
	Category   string // "author" | "publisher" | "tag"
	PageURL    string // API base URL; template appends &page={n}
	TotalPages int
}

// SearchPageData is passed to search.html.
type SearchPageData struct {
	Title      string // site title
	Query      string // pre-filled query from URL
	TotalPages int
}

// ---------------------------------------------------------------------------
// Caches — populated at startup, refreshed on book upload / metadata edits.
// ---------------------------------------------------------------------------

var (
	// aggregateDisplayCache stores pre-computed display items for the
	// server-rendered aggregate pages.  Key: "authors"|"publishers"|"tags".
	aggregateDisplayCache sync.Map

	// aggregateJSONCache stores pre-serialised JSON for the /api/* aggregate
	// endpoints.  Key: "authors"|"publishers"|"tags".
	aggregateJSONCache sync.Map

	// filteredTotalCache stores per-entity total page counts.
	// Key: "author/Name" → int (total_pages).
	filteredTotalCache sync.Map
)

// booksPerPage is the LCM of the grid column counts (10/8/6/4/2) so page
// boundaries never leave gaps in the grid. Shared by every paginated view.
const booksPerPage = 120

// ---------------------------------------------------------------------------
// Startup pre-loading
// ---------------------------------------------------------------------------

// preloadCaches populates aggregate and filtered-total caches at startup.
// Called once after initCalibreDB, before serving requests.
func preloadCaches() {
	for _, category := range []string{"authors", "publishers", "tags"} {
		refreshAggregateCache(category)
	}

	preloadFilteredTotals()
	log.Println("caches: aggregates and filtered totals pre-loaded")
}

// refreshAggregateCache rebuilds both the display cache and JSON cache for a
// category.  Safe to call at any time.
func refreshAggregateCache(category string) {
	items := queryAggregateItems(category)

	// JSON cache for /api/* endpoints.
	data, err := json.Marshal(items)
	if err == nil {
		aggregateJSONCache.Store(category, data)
	}

	// Display cache for server-rendered template.
	aggregateDisplayCache.Store(category, computeAggregateDisplay(category, items))
}

// refreshAllCaches rebuilds every cache.  Called after book upload / metadata
// edit where the exact scope of changes is hard to predict.
func refreshAllCaches() {
	for _, category := range []string{"authors", "publishers", "tags"} {
		refreshAggregateCache(category)
	}
	filteredTotalCache.Clear()
	preloadFilteredTotals()
}

func preloadFilteredTotals() {
	for _, cat := range []string{"author", "publisher", "tag"} {
		table := cat + "s"
		linkCol := cat
		query := fmt.Sprintf(`
			SELECT t.name, COUNT(DISTINCT x.book)
			FROM %s t
			JOIN books_%s_link x ON x.%s = t.id
			GROUP BY t.id
		`, table, table, linkCol)

		rows, err := calibreDB.Query(query)
		if err != nil {
			log.Printf("preloadFilteredTotals(%s): %v", cat, err)
			continue
		}
		for rows.Next() {
			var name string
			var count int
			if err := rows.Scan(&name, &count); err != nil {
				continue
			}
			totalPages := (count + booksPerPage - 1) / booksPerPage
			if totalPages < 1 {
				totalPages = 1
			}
			filteredTotalCache.Store(cat+"/"+name, totalPages)
		}
		logErr(rows.Close(), "preloadFilteredTotals: close rows")
	}
}

// ---------------------------------------------------------------------------
// Display item computation
// ---------------------------------------------------------------------------

func computeAggregateDisplay(category string, items []AggregateItem) []AggregateDisplayItem {
	prefix := "/" + strings.TrimSuffix(category, "s") + "/"
	if len(items) == 0 {
		return nil
	}
	maxCount := items[0].Count // items are sorted descending by count
	if maxCount < 1 {
		maxCount = 1
	}
	display := make([]AggregateDisplayItem, len(items))
	for i, item := range items {
		ratio := float64(item.Count) / float64(maxCount)
		display[i] = AggregateDisplayItem{
			Name:     item.Name,
			Href:     prefix + url.PathEscape(item.Name),
			Count:    item.Count,
			FontSize: fmt.Sprintf("%.1f", 12+ratio*24),
			Opacity:  fmt.Sprintf("%.2f", 0.4+ratio*0.6),
		}
	}
	return display
}

func getAggregateDisplay(category string) []AggregateDisplayItem {
	if val, ok := aggregateDisplayCache.Load(category); ok {
		return val.([]AggregateDisplayItem)
	}
	// Cache miss — compute on the fly and cache.
	items := queryAggregateItems(category)
	display := computeAggregateDisplay(category, items)
	aggregateDisplayCache.Store(category, display)
	return display
}

// ---------------------------------------------------------------------------
// Database queries
// ---------------------------------------------------------------------------

func queryAggregateItems(category string) []AggregateItem {
	switch category {
	case "authors", "publishers", "tags":
		// valid
	default:
		return nil
	}
	linkCol := strings.TrimSuffix(category, "s")

	query := fmt.Sprintf(`
		SELECT c.name, COUNT(l.book) AS count
		FROM %s c
		JOIN books_%s_link l ON l.%s = c.id
		GROUP BY c.id
		ORDER BY count DESC
	`, category, category, linkCol)

	rows, err := calibreDB.Query(query)
	if err != nil {
		log.Println("queryAggregateItems error:", err)
		return nil
	}
	defer func(rows *sql.Rows) {
		logErr(rows.Close(), "failed to close rows in queryAggregateItems")
	}(rows)

	items := make([]AggregateItem, 0)
	for rows.Next() {
		var item AggregateItem
		if err := rows.Scan(&item.Name, &item.Count); err != nil {
			continue
		}
		item.Slug = item.Name
		items = append(items, item)
	}
	return items
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// aggregateHandler serves the JSON API for /api/authors, /api/publishers,
// /api/tags.  Serves from cache with long cache-control headers.
func aggregateHandler(w http.ResponseWriter, category string) {
	if data, ok := aggregateJSONCache.Load(category); ok {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=3600, stale-while-revalidate=86400")
		_, _ = w.Write(data.([]byte))
		return
	}

	// Fallback: query (also populates the cache).
	refreshAggregateCache(category)
	data, _ := aggregateJSONCache.Load(category)
	w.Header().Set("Content-Type", "application/json")
	if b, ok := data.([]byte); ok {
		_, _ = w.Write(b)
	} else {
		_, _ = w.Write([]byte("[]"))
	}
}

// filteredBooksHandler serves paginated books for a category entity.
func filteredBooksHandler(w http.ResponseWriter, r *http.Request, category string) {
	parts := strings.SplitN(r.URL.Path, "/", 3)
	if len(parts) < 3 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := parts[2]

	switch category {
	case "author", "publisher", "tag":
	// valid
	default:
		http.Error(w, "unknown category", http.StatusBadRequest)
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * booksPerPage

	table := category + "s"
	linkCol := category
	query := fmt.Sprintf(
		`
		SELECT b.uuid, b.title,
			GROUP_CONCAT(DISTINCT a.sort) AS author_sort,
			b.pubdate, b.path, b.last_modified
		FROM books b
		JOIN books_%s_link x ON x.book = b.id
		JOIN %s t ON t.id = x.%s
		LEFT JOIN books_authors_link bal ON bal.book = b.id
		LEFT JOIN authors a ON a.id = bal.author
		WHERE t.name = ?
		GROUP BY b.id
		ORDER BY b.sort
		LIMIT ? OFFSET ?`,
		table,
		table,
		linkCol,
	)

	rows, err := calibreDB.Query(query, name, booksPerPage, offset)
	if err != nil {
		log.Println("filtered books error:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer func(rows *sql.Rows) {
		logErr(rows.Close(), "failed to close rows in filteredBooksHandler")
	}(rows)

	err = encodeBooksJSONFromRows(rows, w)
	if err != nil {
		log.Println("filtered books error:", err)
	}
}

// getFilteredTotal returns total pages for an entity, cached.
func getFilteredTotal(category, name string) int {
	key := category + "/" + name
	if val, ok := filteredTotalCache.Load(key); ok {
		return val.(int)
	}
	total := computeFilteredTotal(category, name)
	filteredTotalCache.Store(key, total)
	return total
}

// filteredPageHandler serves the HTML shell (or the JSON API with ?api=1).
func filteredPageHandler(category string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api") == "1" {
			filteredBooksHandler(w, r, category)
			return
		}
		parts := strings.SplitN(r.URL.Path, "/", 3)
		if len(parts) < 3 || parts[2] == "" {
			http.NotFound(w, r)
			return
		}
		name := parts[2]
		data := FilteredPageData{
			Title:      SiteTitle,
			Name:       name,
			Category:   category,
			PageURL:    "/" + category + "/" + url.PathEscape(name) + "?api=1",
			TotalPages: getFilteredTotal(category, name),
		}
		logErr(templates.ExecuteTemplate(w, "filtered.html", data), "failed to execute filtered template")
	}
}

// aggregatePageHandler serves the server-rendered tag cloud.
func aggregatePageHandler(category string) http.HandlerFunc {
	title := strings.ToUpper(category[:1]) + category[1:]
	return func(w http.ResponseWriter, r *http.Request) {
		data := AggregatePageData{
			Title:      title,
			LinkPrefix: strings.TrimSuffix(category, "s"),
			Items:      getAggregateDisplay(category),
		}
		w.Header().Set("Cache-Control", "no-cache")
		if err := templates.ExecuteTemplate(w, "aggregate.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func computeFilteredTotal(category, name string) int {
	table := category + "s"
	linkCol := category
	query := fmt.Sprintf(`
		SELECT COUNT(DISTINCT x.book)
		FROM books_%s_link x
		JOIN %s t ON t.id = x.%s
		WHERE t.name = ?
	`, table, table, linkCol)

	var count int
	if err := calibreDB.QueryRow(query, name).Scan(&count); err != nil {
		return 0
	}
	return (count + booksPerPage - 1) / booksPerPage
}
