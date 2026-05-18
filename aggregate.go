package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
)

type AggregateItem struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Slug  string `json:"slug"`
}

func aggregateHandler(w http.ResponseWriter, r *http.Request, category string) {
	switch category {
	case "authors", "publishers", "tags":
		// valid
	default:
		http.Error(w, "invalid category", http.StatusBadRequest)
		return
	}
	linkCol := strings.TrimSuffix(category, "s")

	query := fmt.Sprintf(`
        SELECT c.name, COUNT(l.book) AS count
        FROM %s c
        JOIN books_%s_link l ON l.%s = c.id
        GROUP BY c.id
        ORDER BY count DESC
    `, category, category, linkCol)
	rows, err := searchDB.Query(query)

	if err != nil {
		log.Println("aggregate error:", err)
		http.Error(w, "query failed", 500)
		return
	}
	defer rows.Close()

	items := make([]AggregateItem, 0)
	for rows.Next() {
		var item AggregateItem
		if err := rows.Scan(&item.Name, &item.Count); err != nil {
			continue
		}
		item.Slug = item.Name
		items = append(items, item)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

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
	offset := (page - 1) * 30 // TODO: Remove magic number, should match query LIMIT

	var query string
	table := category + "s"
	linkCol := category
	query = fmt.Sprintf(`
		SELECT b.uuid, b.title,
			GROUP_CONCAT(DISTINCT a.sort) AS author_sort,
			b.pubdate, b.path
		FROM books b
		JOIN books_%s_link x ON x.book = b.id
		JOIN %s t ON t.id = x.%s
		LEFT JOIN books_authors_link bal ON bal.book = b.id
		LEFT JOIN authors a ON a.id = bal.author
		WHERE t.name = ?
		GROUP BY b.id
		ORDER BY b.sort
		LIMIT 30 OFFSET ?`,
		table,
		table,
		linkCol,
	)

	rows, err := searchDB.Query(query, name, offset)
	if err != nil {
		log.Println("filtered books error:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	books := make([]Book, 0)
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.UUID, &b.Title, &b.AuthorSort, &b.PubDate, &b.Path); err != nil {
			continue
		}
		coverIndex.Store(b.UUID, b.Path)
		b.Path = ""
		books = append(books, b)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(books)
}
