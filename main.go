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
	"strings"
	"time"

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

var coverIndex = make(map[string]string)
var formatCache = make(map[string][]string)

func coverHandler(w http.ResponseWriter, r *http.Request) {
	// uuid comes from URL: /cover/{uuid}
	uuid := filepath.Base(r.URL.Path)

	if uuid == "" {
		http.Error(w, "missing uuid", http.StatusBadRequest)
		return
	}

	bookPath := coverIndex[uuid]

	// Build safe path
	baseDir := "/data/CALIBRE/Calibre/E-Books"

	coverPath := filepath.Join(baseDir, bookPath, "cover.jpg")

	// prevent path traversal escape (extra safety)
	coverPath, err := filepath.Abs(coverPath)
	if err != nil {
		http.Error(w, "invalid path", 500)
		return
	}

	// ensure file exists
	f, err := os.Open(coverPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Header().Set("Content-Type", "image/jpeg")

	http.ServeContent(w, r, "cover.jpg", fileModTime(f), f)
}

// Why do we even need this lol
func fileModTime(f *os.File) (t time.Time) {
	stat, err := f.Stat()
	if err != nil {
		return
	}
	return stat.ModTime()
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

	//server cover getter
	http.HandleFunc("/cover/", coverHandler)

	// server get formats
	http.HandleFunc("/formats/", formatsHandler)

	// Downloads
	http.HandleFunc("/download/", downloadHandler)

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
	rows, err := db.Query("SELECT title, author_sort, series_index, pubdate, path, uuid FROM books ORDER BY timestamp DESC")
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

		if err != nil {
			panic(err)
		}

		// cache cover path
		coverIndex[book.UUID] = book.Path

		// add book to the arrays
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

func resolveFormats(uuid, baseDir, path string) []string {
	// check cache first
	if f, ok := formatCache[uuid]; ok {
		return f
	}

	full := filepath.Join(baseDir, path)

	entries, err := os.ReadDir(full)
	if err != nil {
		//println("resolvefmt: " + err.Error())
		return []string{}
	}

	formats := []string{}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		switch filepath.Ext(e.Name()) {
		case ".epub":
			formats = append(formats, "epub")
		case ".pdf":
			formats = append(formats, "pdf")
		case ".mobi":
			formats = append(formats, "mobi")
		}
	}

	// store in cache
	formatCache[uuid] = formats

	return formats
}

func formatsHandler(w http.ResponseWriter, r *http.Request) {
	uuid := filepath.Base(r.URL.Path)

	path, ok := coverIndex[uuid]
	if !ok {
		http.NotFound(w, r)
		return
	}

	baseDir := "/data/CALIBRE/Calibre/E-Books"

	formats := resolveFormats(uuid, baseDir, path)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(formats)
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, "bad request", 400)
		return
	}

	uuid := parts[2]
	format := parts[3]

	path, ok := coverIndex[uuid]
	if !ok {
		println("download: " + uuid + " not found")
		http.NotFound(w, r)
		return
	}

	baseDir := "/data/CALIBRE/Calibre/E-Books"

	// ensure cache exists (lazy fallback)
	formats := resolveFormats(uuid, baseDir, path)
	if len(formats) == 0 {
		println("download: " + uuid + " not found resolve fmts")
		http.NotFound(w, r)
		return
	}

	file := filepath.Join(baseDir, path, "book."+format)

	if _, err := os.Stat(file); err != nil {
		println("download: " + uuid + " not found file stat")
		http.NotFound(w, r)
		return
	}

	w.Header().Set(
		"Content-Disposition",
		`attachment; filename="book.`+format+`"`,
	)

	http.ServeFile(w, r, file)
}
