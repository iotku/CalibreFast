package main

import (
	"encoding/json"
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

func aggregateHandler(w http.ResponseWriter, r *http.Request, query string) {
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

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * 50

	var query string
	switch category {
	case "author":
		query = `
            SELECT DISTINCT b.uuid, b.title,
                COALESCE(a.sort, a.name, '') as author_sort,
                b.pubdate, b.path
            FROM books b
            JOIN books_authors_link bal ON bal.book = b.id
            JOIN authors a ON a.id = bal.author
            WHERE a.name = ?
            ORDER BY b.sort LIMIT 50 OFFSET ?`
	case "publisher":
		query = `
            SELECT DISTINCT b.uuid, b.title,
                COALESCE(a.sort, a.name, '') as author_sort,
                b.pubdate, b.path
            FROM books b
            JOIN books_publishers_link bpl ON bpl.book = b.id
            JOIN publishers p ON p.id = bpl.publisher
            LEFT JOIN books_authors_link bal ON bal.book = b.id
            LEFT JOIN authors a ON a.id = bal.author
            WHERE p.name = ?
            ORDER BY b.sort LIMIT 50 OFFSET ?`
	case "tag":
		query = `
            SELECT DISTINCT b.uuid, b.title,
                COALESCE(a.sort, a.name, '') as author_sort,
                b.pubdate, b.path
            FROM books b
            JOIN books_tags_link btl ON btl.book = b.id
            JOIN tags t ON t.id = btl.tag
            LEFT JOIN books_authors_link bal ON bal.book = b.id
            LEFT JOIN authors a ON a.id = bal.author
            WHERE t.name = ?
            ORDER BY b.sort LIMIT 50 OFFSET ?`
	default:
		http.Error(w, "unknown category", http.StatusBadRequest)
		return
	}

	rows, err := searchDB.Query(query, name, offset)
	if err != nil {
		log.Println("filtered books error:", err)
		http.Error(w, "query failed", 500)
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
