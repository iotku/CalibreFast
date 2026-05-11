package main

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	_ "github.com/mattn/go-sqlite3"
)

type Book struct {
	Title       string  `json:"title"`
	AuthorSort  string  `json:"author_sort"`
	SeriesIndex float64 `json:"series_index"`
	PubDate     string  `json:"pubdate"`
	Path        string  `json:"path"`
	UUID        string  `json:"uuid"`
}

type PageData struct {
	Title string
}

func writePage(page int, books []Book) error {
	data, err := json.MarshalIndent(books, "", "  ")
	if err != nil {
		return err
	}

	filename := filepath.Join(
		"pages",
		"page"+strconv.Itoa(page)+".json",
	)

	return os.WriteFile(filename, data, 0644)
}

func main() {
	generatePages()
	serveLibraryHttp()
}

func serveLibraryHttp() {
	tmpl := template.Must(
		template.ParseFiles("templates/index.html"),
	)

	// homepage
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		data := PageData{
			Title: "My Calibre Library",
		}

		err := tmpl.Execute(w, data)
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	})

	// serve generated json pages
	http.Handle(
		"/pages/",
		http.StripPrefix(
			"/pages/",
			http.FileServer(http.Dir("./pages")),
		),
	)

	// serve css/js
	http.Handle(
		"/static/",
		http.StripPrefix(
			"/static/",
			http.FileServer(http.Dir("./static")),
		),
	)

	log.Println("Listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
func generatePages() {
	db, err := sql.Open("sqlite3", "/data/CALIBRE/Calibre/E-Books/metadata.db")
	defer func(db *sql.DB) {
		err := db.Close()
		if err != nil {
			panic(err)
		}
	}(db)

	if err != nil {
		panic(err)
	}

	// create pagenated entries as json for each book in metadata.db (50 books/page)
	rows, err := db.Query("SELECT title, author_sort, series_index, pubdate, path, uuid FROM books")
	if err != nil {
		panic(err)
	}

	defer func(rows *sql.Rows) {
		err := rows.Close()
		if err != nil {
			panic(err)
		}
	}(rows)
	page := 0
	booksPerPage := 50
	books := make([]Book, 0, booksPerPage)

	for rows.Next() {
		var book Book

		err = rows.Scan(
			&book.Title,
			&book.AuthorSort,
			&book.SeriesIndex,
			&book.PubDate,
			&book.Path,
			&book.UUID,
		)
		println("Title:" + book.Title)

		if err != nil {
			panic(err)
		}

		books = append(books, book)

		if len(books) >= booksPerPage {
			// save to file
			err := writePage(page, books)
			if err != nil {
				panic(err)
			}
			page++
			books = make([]Book, 0, booksPerPage)
		}
	}

	// write final partial page
	if len(books) > 0 {
		err = writePage(page, books)
		if err != nil {
			panic(err)
		}
	}

}
