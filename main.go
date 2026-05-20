package main

import (
	"database/sql"
	"encoding/json"
	"flag"
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

var searchDB *sql.DB
var SiteTitle = "Library"

var baseDir string
var cacheDir string
var hostport string

func initSearchDB() {
	var err error
	searchDB, err = sql.Open("sqlite3", filepath.Join(baseDir, "metadata.db"))
	if err != nil {
		log.Fatal("failed to open search db: ", err)
	}
	searchDB.SetMaxOpenConns(1) // sqlite doesn't like concurrent writers, reads are fine with 1
}

type Book struct {
	Title       string  `json:"title"`
	AuthorSort  string  `json:"author_sort"`
	SeriesIndex float64 `json:"series_index"`
	PubDate     string  `json:"pubdate"`
	Path        string  `json:"path"`
	UUID        string  `json:"uuid"`
}

type LibraryInfo struct {
	TotalPages int `json:"total_pages"`
}

var totalPages = 0

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
	flag.StringVar(&baseDir, "basedir", "", "path to Calibre library root")
	flag.StringVar(&cacheDir, "cachedir", "", "path to cache location")
	flag.StringVar(&hostport, "port", "8080", "port to listen on")
	flag.Parse()
	if baseDir == "" {
		log.Fatal("missing -basedir")
	}

	dbPath := filepath.Join(baseDir, "metadata.db")

	if _, err := os.Stat(dbPath); err != nil {
		log.Fatal("metadata.db not found at: ", dbPath)
	}
	generatePages()
	initSearchDB()
	serveLibraryHttp()
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "missing q", http.StatusBadRequest)
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	fromYear := r.URL.Query().Get("from")
	toYear := r.URL.Query().Get("to")

	like := "%" + q + "%"
	offset := (page - 1) * 50

	query := `
SELECT 
    b.uuid, 
    b.title, 
    GROUP_CONCAT(COALESCE(a.sort, a.name, ''), ' & ') as author_sort,
    b.pubdate, 
    b.path 
FROM books b 
LEFT JOIN books_authors_link bal ON bal.book = b.id 
LEFT JOIN authors a ON a.id = bal.author 
WHERE (b.title LIKE ? OR a.name LIKE ? OR a.sort LIKE ?) 
GROUP BY b.uuid, b.title, b.pubdate, b.path
ORDER BY b.sort 
LIMIT 50 OFFSET ?`
	args := []any{like, like, like, offset}

	// TODO: I don't expect this actually works
	if fromYear != "" {
		query += " AND DATE(b.pubdate) >= DATE(?)"
		args = append(args, fromYear+"-01-01")
	}
	if toYear != "" {
		query += " AND DATE(b.pubdate) <= DATE(?)"
		args = append(args, toYear+"-12-31")
	}
	// TODO: END TODO

	args = append(args, offset)
	rows, err := searchDB.Query(query, args...)
	if err != nil {
		log.Println("search error:", err)
		http.Error(w, "search failed", http.StatusInternalServerError)
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

// Why do we even need this lol
func fileModTime(f *os.File) (t time.Time) {
	stat, err := f.Stat()
	if err != nil {
		return
	}
	return stat.ModTime()
}

var templates = template.Must(
	template.ParseGlob("templates/*.html"),
)

func serveLibraryHttp() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		data := PageData{
			Title: "Library",
		}

		err := templates.ExecuteTemplate(w, "index.html", data)
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

	http.HandleFunc("/library-info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(LibraryInfo{
			TotalPages: totalPages,
		})
	})

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
	http.HandleFunc("/cover-thumb/", coverThumbHandler)

	// server get formats
	http.HandleFunc("/formats/", formatsHandler)

	// Downloads
	http.HandleFunc("/download/", downloadHandler)

	// Book Display
	http.HandleFunc("/book/", bookHandler)

	http.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		err := templates.ExecuteTemplate(w, "search.html", PageData{Title: SiteTitle})
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	})

	http.HandleFunc("/api/search", searchHandler)

	http.HandleFunc("/read", func(w http.ResponseWriter, r *http.Request) {
		err := templates.ExecuteTemplate(w, "reader.html", PageData{Title: SiteTitle})
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	})

	// TODO: Can we combine this with the /read endpoint?
	http.HandleFunc("/read-pdf", func(w http.ResponseWriter, r *http.Request) {
		err := templates.ExecuteTemplate(w, "reader-pdf.html", PageData{Title: SiteTitle})
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	})

	http.HandleFunc("/epub/", epubFileHandler)

	http.HandleFunc("/view/", viewHandler)

	http.HandleFunc("/api/authors", func(w http.ResponseWriter, r *http.Request) {
		aggregateHandler(w, r, "authors")
	})

	http.HandleFunc("/api/publishers", func(w http.ResponseWriter, r *http.Request) {
		aggregateHandler(w, r, "publishers")
	})

	http.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		aggregateHandler(w, r, "tags")
	})

	// page routes
	for _, page := range []string{"authors", "publishers", "tags"} {
		p := page
		http.HandleFunc("/"+p, func(w http.ResponseWriter, r *http.Request) {
			err := templates.ExecuteTemplate(w, "aggregate.html", PageData{Title: SiteTitle})
			if err != nil {
				http.Error(w, err.Error(), 500)
			}
		})
	}

	http.HandleFunc("/author/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api") == "1" {
			filteredBooksHandler(w, r, "author")
			return
		}
		templates.ExecuteTemplate(w, "filtered.html", PageData{Title: SiteTitle})
	})
	http.HandleFunc("/publisher/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api") == "1" {
			filteredBooksHandler(w, r, "publisher")
			return
		}
		templates.ExecuteTemplate(w, "filtered.html", PageData{Title: SiteTitle})
	})
	http.HandleFunc("/tag/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api") == "1" {
			filteredBooksHandler(w, r, "tag")
			return
		}
		templates.ExecuteTemplate(w, "filtered.html", PageData{Title: SiteTitle})
	})
	log.Println("Listening on :" + hostport)
	log.Fatal(http.ListenAndServe(":"+hostport, nil))
}

func viewHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	uuid := parts[2]
	format := parts[3]

	if format != "pdf" {
		http.Error(w, "only pdf supported", http.StatusBadRequest)
		return
	}

	bookPath, ok := coverIndex.Load(uuid)
	if !ok {
		http.NotFound(w, r)
		return
	}

	files := resolveFormats(uuid, baseDir, bookPath.(string))
	var target string
	for _, f := range files {
		if strings.TrimPrefix(filepath.Ext(f), ".") == format {
			target = f
			break
		}
	}

	if target == "" {
		http.NotFound(w, r)
		return
	}

	fullPath := filepath.Join(baseDir, bookPath.(string), target)

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="`+target+`"`)
	http.ServeFile(w, r, fullPath)
}

func generatePages() {
	db, err := sql.Open("sqlite3", filepath.Join(baseDir, "metadata.db"))
	defer func(db *sql.DB) {
		err := db.Close()
		if err != nil {
			panic(err)
		}
	}(db)

	if err != nil {
		panic(err)
	}

	// create paginated entries as json for each book in metadata.db (50 books/page)
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
	page := 1
	booksPerPage := 30
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
		coverIndex.Store(book.UUID, book.Path)

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

	totalPages = page
}

func resolveFormats(uuid, baseDir, path string) []string {
	// check cache first
	if f, ok := formatCache.Load(uuid); ok {
		return f.([]string)
	}

	full := filepath.Join(baseDir, path)

	entries, err := os.ReadDir(full)
	if err != nil {
		return []string{}
	}

	formats := []string{}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".epub", ".pdf", ".mobi", ".azw3", ".djvu":
			formats = append(formats, e.Name())
		}
	}

	// store in cache
	formatCache.Store(uuid, formats)

	return formats
}

func formatsHandler(w http.ResponseWriter, r *http.Request) {
	uuid := filepath.Base(r.URL.Path)

	path, ok := coverIndex.Load(uuid)
	if !ok {
		http.NotFound(w, r)
		return
	}

	files := resolveFormats(uuid, baseDir, path.(string))

	// convert to UI-friendly format list
	formats := extractFormats(files)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(formats)
}

func extractFormats(files []string) []string {
	formats := make([]string, 0, len(files))

	for _, f := range files {
		ext := strings.TrimPrefix(filepath.Ext(f), ".")
		formats = append(formats, ext)
	}

	return formats
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	uuid := parts[2]
	format := parts[3] // epub, pdf, mobi

	value, ok := coverIndex.Load(uuid)
	path, ok2 := value.(string)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// ensure cache exists (lazy init)
	value, ok = formatCache.Load(uuid)
	files, ok2 := value.([]string) // typecast
	if !ok || !ok2 {
		files = resolveFormats(uuid, baseDir, path)
		if len(files) == 0 {
			http.NotFound(w, r)
			return
		}
	}

	// find matching file by extension
	var target string
	for _, f := range files {
		if strings.TrimPrefix(filepath.Ext(f), ".") == format {
			target = f
			break
		}
	}

	if target == "" {
		http.NotFound(w, r)
		return
	}

	fullPath := filepath.Join(baseDir, path, target)

	// safety check
	if _, err := os.Stat(fullPath); err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set(
		"Content-Disposition",
		`attachment; filename="`+target+`"`,
	)

	http.ServeFile(w, r, fullPath)
}
