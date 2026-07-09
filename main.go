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

	"github.com/google/uuid"
	"github.com/mattn/go-sqlite3"
	_ "github.com/mattn/go-sqlite3"
)

var (
	calibreDB *sql.DB
	SiteTitle = "Library"
)

var (
	baseDir  string
	cacheDir string
	hostPort string
)

func initCalibreDB() {
	sql.Register("sqlite3_calibre",
		&sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				if err := conn.RegisterFunc("title_sort", titleSort, true); err != nil {
					return err
				}

				if err := conn.RegisterFunc("uuid4", func() string {
					return uuid.New().String()
				}, true); err != nil {
					return err
				}

				return nil
			},
		})
	var err error
	calibreDB, err = sql.Open("sqlite3_calibre", filepath.Join(baseDir, "metadata.db"))
	if err != nil {
		log.Fatal("failed to open search db: ", err)
	}
	calibreDB.SetMaxOpenConns(1) // sqlite doesn't like concurrent writers, reads are fine with 1
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

	return os.WriteFile(filename, data, 0o644)
}

func main() {
	flag.StringVar(&baseDir, "basedir", "", "path to Calibre library root")
	flag.StringVar(&cacheDir, "cachedir", "", "path to cache location")
	flag.StringVar(&hostPort, "port", "8080", "port to listen on")
	flag.Parse()
	if baseDir == "" {
		log.Fatal("missing -basedir")
	}

	dbPath := filepath.Join(baseDir, "metadata.db")

	if _, err := os.Stat(dbPath); err != nil {
		log.Fatal("metadata.db not found at: ", dbPath)
	}
	generatePages()
	initCalibreDB()
	serveLibraryHTTP()
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

	args = append(args, offset)
	rows, err := calibreDB.Query(query, args...)
	if err != nil {
		log.Println("search error:", err)
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	defer func(rows *sql.Rows) {
		logErr(rows.Close(), "could not close rows in searchHandler")
	}(rows)
	err = encodeBooksJSONFromRows(rows, w)
	if err != nil {
		panic("failed to encode books json: " + err.Error())
	}
}

// encodeBooksJSONFromRows runs for dynamic operations that hit the database to
// retrieve lists of books and encode them as JSON.
func encodeBooksJSONFromRows(rows *sql.Rows, w http.ResponseWriter) error {
	books := make([]Book, 0)
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.UUID, &b.Title, &b.AuthorSort, &b.PubDate, &b.Path); err != nil {
			continue
		}
		coverIndex.Store(b.UUID, b.Path)
		books = append(books, b)
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(books)
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

func serveLibraryHTTP() {
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
		logErr(json.NewEncoder(w).Encode(LibraryInfo{
			TotalPages: totalPages,
		}), "could not encode library info")
	})

	// serve css/js
	http.Handle(
		"/static/",
		http.StripPrefix(
			"/static/",
			http.FileServer(http.Dir("./static")),
		),
	)

	// server cover getter
	http.HandleFunc("/cover/", coverHandler)
	http.HandleFunc("/cover-thumb/", coverThumbHandler)

	// server get formats
	http.HandleFunc("/formats/", formatsHandler)

	// Downloads
	http.HandleFunc("/download/", downloadHandler)

	// Uploads
	http.HandleFunc("/upload", uploadHandler)

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
		aggregateHandler(w, "authors")
	})

	http.HandleFunc("/api/publishers", func(w http.ResponseWriter, r *http.Request) {
		aggregateHandler(w, "publishers")
	})

	http.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		aggregateHandler(w, "tags")
	})

	// page routes
	for _, page := range []string{"authors", "publishers", "tags"} {
		p := page
		http.HandleFunc("/"+p, func(w http.ResponseWriter, r *http.Request) {
			err := templates.ExecuteTemplate(w, "aggregate.html", PageData{Title: SiteTitle})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		})
	}

	http.HandleFunc("/author/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api") == "1" {
			filteredBooksHandler(w, r, "author")
			return
		}
		logErr(templates.ExecuteTemplate(w, "filtered.html", PageData{Title: SiteTitle}), "failed to execute author filtered template")
	})
	http.HandleFunc("/publisher/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api") == "1" {
			filteredBooksHandler(w, r, "publisher")
			return
		}
		logErr(templates.ExecuteTemplate(w, "filtered.html", PageData{Title: SiteTitle}), "failed to execute publisher filtered template")
	})
	http.HandleFunc("/tag/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api") == "1" {
			filteredBooksHandler(w, r, "tag")
			return
		}
		logErr(templates.ExecuteTemplate(w, "filtered.html", PageData{Title: SiteTitle}), "failed to execute tag filtered template")
	})
	log.Println("Listening on :" + hostPort)
	log.Fatal(http.ListenAndServe(":"+hostPort, nil))
}

func logErr(err error, why string) {
	if err != nil {
		log.Println(why + " :: " + err.Error())
	}
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

	bookPath, ok := uuidPathIndex.Load(uuid)
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

// generatePages reads the Calibre metadata.db and generates paginated JSON files for the library main view.
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
		uuidPathIndex.Store(book.UUID, book.Path)

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

// resolveFormats returns a []string of filepaths to matching e-book formats
func resolveFormats(uuid, baseDir, path string) []string {
	// check cache first
	if f, ok := formatCache.Load(uuid); ok {
		return f.([]string)
	}

	// check baseDir in filesystem for e-book matching formats
	full := filepath.Join(baseDir, path)

	entries, err := os.ReadDir(full)
	if err != nil {
		return []string{}
	}

	var formats []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		} else if hasBookExt(e.Name()) {
			formats = append(formats, e.Name())
		}
	}

	// store in cache
	formatCache.Store(uuid, formats)

	return formats
}

// hasBookExt takes a filepath and returns true if the extension matches
// a well known e-book format (e.g. .epub, .pdf, .mobi, etc)
func hasBookExt(path string) bool {
	ext := filepath.Ext(path)
	return ext == ".epub" || ext == ".pdf" || ext == ".mobi" || ext == ".azw3" || ext == ".djvu"
}

func formatsHandler(w http.ResponseWriter, r *http.Request) {
	uuid := filepath.Base(r.URL.Path)

	path, ok := uuidPathIndex.Load(uuid)
	if !ok {
		http.NotFound(w, r)
		return
	}

	files := resolveFormats(uuid, baseDir, path.(string))

	// convert to UI-friendly format list
	formats := extractFormats(files)

	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(formats), "failed to encode formats json")
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

	value, ok := uuidPathIndex.Load(uuid)
	path, _ := value.(string) // TODO: Maybe we should check ok value here too
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
