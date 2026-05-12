package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"html/template"
	"image"
	"image/jpeg"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	flag.Parse()
	if baseDir == "" {
		log.Fatal("missing -basedir")
	}

	dbPath := filepath.Join(baseDir, "metadata.db")

	if _, err := os.Stat(dbPath); err != nil {
		log.Fatal("metadata.db not found at: ", dbPath)
	}
	generatePages()
	serveLibraryHttp()
}

var baseDir string
var cacheDir string

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

	thumb := resizeToWidth(img, 300)

	out, err := os.Create(thumbPath)
	if err == nil {
		jpeg.Encode(out, thumb, &jpeg.Options{Quality: 75})
		out.Close()
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Header().Set("X-Cache-Hit", "false")
	jpeg.Encode(w, thumb, &jpeg.Options{Quality: 75})
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
	// homepage
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		data := PageData{
			Title: "My Calibre Library",
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

	log.Println("Listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
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
