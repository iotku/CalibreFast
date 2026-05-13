package main

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"image"
	"image/jpeg"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var searchDB *sql.DB

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

	query := "SELECT DISTINCT b.uuid, b.title, COALESCE(a.sort, a.name, '') as author_sort, b.pubdate, b.path FROM books b LEFT JOIN books_authors_link bal ON bal.book = b.id LEFT JOIN authors a ON a.id = bal.author WHERE (b.title LIKE ? OR a.name LIKE ? OR a.sort LIKE ?) ORDER BY b.sort LIMIT 50 OFFSET ?"
	args := []any{like, like, like, offset}

	if fromYear != "" {
		query += " AND DATE(b.pubdate) >= DATE(?)"
		args = append(args, fromYear+"-01-01")
	}
	if toYear != "" {
		query += " AND DATE(b.pubdate) <= DATE(?)"
		args = append(args, toYear+"-12-31")
	}

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

var baseDir string
var cacheDir string
var hostport string

var coverIndex sync.Map               // map[string]string
var formatCache sync.Map              // map[string][]string
var imageSem = make(chan struct{}, 1) // only 2 HDD reads at once
func coverHandler(w http.ResponseWriter, r *http.Request) {
	imageSem <- struct{}{}        // acquire slot
	defer func() { <-imageSem }() // release slot
	// uuid comes from URL: /cover/{uuid}
	uuid := filepath.Base(r.URL.Path)

	if uuid == "" {
		http.Error(w, "missing uuid", http.StatusBadRequest)
		return
	}

	bookPath, ok := coverIndex.Load(uuid)
	if !ok {
		http.Error(w, "book not found", http.StatusNotFound)
	}

	// Build safe path
	coverPath := filepath.Join(baseDir, bookPath.(string), "cover.jpg")

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
func ensureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0755)
}

func coverThumbHandler(w http.ResponseWriter, r *http.Request) {
	uuid := strings.TrimPrefix(r.URL.Path, "/cover-thumb/")

	path, ok := coverIndex.Load(uuid)
	if !ok {
		http.NotFound(w, r)
		return
	}

	thumbPath := filepath.Join(cacheDir, "covers", "thumb", uuid+".jpg")
	if err := ensureDir(thumbPath); err != nil {
		println(err.Error())
	}

	// 1. FAST PATH: already cached
	if f, err := os.Open(thumbPath); err == nil {
		defer f.Close()
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.Header().Set("X-Cache-Hit", "true")
		http.ServeContent(w, r, "thumb.jpg", time.Time{}, f)
		return
	}

	// 2. SLOW PATH: generate
	origPath := filepath.Join(baseDir, path.(string), "cover.jpg")

	f, err := os.Open(origPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	thumb := resizeToWidth(img, 600)

	out, err := os.Create(thumbPath)
	if err == nil {
		jpeg.Encode(out, thumb, &jpeg.Options{Quality: 85})
		out.Close()
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Header().Set("X-Cache-Hit", "false")
	jpeg.Encode(w, thumb, &jpeg.Options{Quality: 85})
}

func resizeToWidth(img image.Image, width int) image.Image {
	bounds := img.Bounds()

	scale := float64(width) / float64(bounds.Dx())
	height := int(float64(bounds.Dy()) * scale)

	dst := image.NewRGBA(image.Rect(0, 0, width, height))

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			srcX := int(float64(x) / scale)
			srcY := int(float64(y) / scale)

			dst.Set(x, y, img.At(srcX, srcY))
		}
	}

	return dst
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
	//mime.AddExtensionType(".js", "application/javascript")
	//mime.AddExtensionType(".css", "text/css")
	// homepage
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
		err := templates.ExecuteTemplate(w, "search.html", PageData{Title: "My Calibre Library"})
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	})

	http.HandleFunc("/api/search", searchHandler)

	http.HandleFunc("/read", func(w http.ResponseWriter, r *http.Request) {
		err := templates.ExecuteTemplate(w, "reader.html", PageData{Title: "My Calibre Library"})
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	})

	http.HandleFunc("/epub/", epubFileHandler)

	http.HandleFunc("/view/", viewHandler)

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

func rewriteEpubURLs(content, base string) string {
	// rewrite href="..." and src="..." that are relative (don't start with http/https/#//)
	attrs := []string{"src", "href"}
	for _, attr := range attrs {
		// find all attr="value" occurrences
		prefix := attr + `="`
		result := strings.Builder{}
		remaining := content
		for {
			idx := strings.Index(remaining, prefix)
			if idx == -1 {
				result.WriteString(remaining)
				break
			}
			result.WriteString(remaining[:idx+len(prefix)])
			remaining = remaining[idx+len(prefix):]

			// find closing quote
			end := strings.Index(remaining, `"`)
			if end == -1 {
				result.WriteString(remaining)
				break
			}
			url := remaining[:end]
			remaining = remaining[end:]

			// only rewrite relative URLs
			if !strings.HasPrefix(url, "http") &&
				!strings.HasPrefix(url, "#") &&
				!strings.HasPrefix(url, "//") &&
				!strings.HasPrefix(url, "data:") &&
				url != "" {
				result.WriteString(base + url)
			} else {
				result.WriteString(url)
			}
		}
		content = result.String()
	}
	return content
}

func epubFileHandler(w http.ResponseWriter, r *http.Request) {
	// /epub/{uuid}/{path...}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/epub/"), "/", 2)
	if len(parts) < 2 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	uuid := parts[0]
	innerPath := parts[1]

	bookPath, ok := coverIndex.Load(uuid)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// find the epub file
	files := resolveFormats(uuid, baseDir, bookPath.(string))
	var epubFile string
	for _, f := range files {
		if strings.HasSuffix(f, ".epub") {
			epubFile = filepath.Join(baseDir, bookPath.(string), f)
			break
		}
	}
	if epubFile == "" {
		http.NotFound(w, r)
		return
	}

	// open the zip
	zr, err := zip.OpenReader(epubFile)
	if err != nil {
		http.Error(w, "failed to open epub", 500)
		return
	}
	defer zr.Close()

	// find the requested file inside the zip
	for _, f := range zr.File {
		if f.Name == innerPath {
			rc, err := f.Open()
			if err != nil {
				http.Error(w, "failed to read file", 500)
				return
			}
			defer rc.Close()

			// set content type based on extension
			ext := filepath.Ext(innerPath)
			switch ext {
			case ".html", ".xhtml":
				content, err := io.ReadAll(rc)
				if err != nil {
					http.Error(w, "read error", 500)
					return
				}

				// get the directory of the current file within the epub
				fileDir := path.Dir(innerPath) // e.g. "OPS" for "OPS/chapter1.xhtml"
				base := fmt.Sprintf("/epub/%s/%s/", uuid, fileDir)

				s := rewriteEpubURLs(string(content), base)
				w.Header().Set("Content-Type", "application/xhtml+xml")
				w.Write([]byte(s))
				return
			case ".css":
				w.Header().Set("Content-Type", "text/css")
			case ".js":
				w.Header().Set("Content-Type", "application/javascript")
			case ".png":
				w.Header().Set("Content-Type", "image/png")
			case ".jpg", ".jpeg":
				w.Header().Set("Content-Type", "image/jpeg")
			case ".gif":
				w.Header().Set("Content-Type", "image/gif")
			case ".svg":
				w.Header().Set("Content-Type", "image/svg+xml")
			case ".ttf":
				w.Header().Set("Content-Type", "font/ttf")
			case ".otf":
				w.Header().Set("Content-Type", "font/otf")
			case ".woff":
				w.Header().Set("Content-Type", "font/woff")
			case ".woff2":
				w.Header().Set("Content-Type", "font/woff2")
			case ".xml", ".opf", ".ncx":
				w.Header().Set("Content-Type", "application/xml")
			case ".xpgt":
				w.WriteHeader(http.StatusNoContent)
				return
			default:
				w.Header().Set("Content-Type", "application/octet-stream")
			}

			io.Copy(w, rc)
			return
		}
	}

	http.NotFound(w, r)
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
		//println("resolvefmt: " + err.Error())
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
