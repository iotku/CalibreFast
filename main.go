package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

	// allowWrites enables every operation that changes the library on disk
	// (metadata edits, uploads, covers, groups). Off by default: there is no
	// authentication, so anyone who can reach the server could otherwise
	// modify the library.
	allowWrites bool
)

// csrfProtect rejects cross-origin state-changing requests (POST, PUT,
// DELETE, PATCH) from browsers, using Sec-Fetch-Site / Origin. Without it,
// any website open in the user's browser could submit uploads or metadata
// edits to a server on their network while -allow-writes is on. Same-origin
// requests and non-browser clients (no such headers, e.g. curl) pass; GET and
// HEAD always pass, so every write handler must reject GET (they do).
func csrfProtect(h http.Handler) http.Handler {
	p := http.NewCrossOriginProtection()
	p.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("blocked cross-origin %s %s (Origin %q, Sec-Fetch-Site %q)",
			r.Method, r.URL.Path, r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site"))
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
	}))
	return p.Handler(h)
}

// requireWrites refuses a handler unless the server was started with
// -allow-writes. Enforced server-side; hiding the UI is only cosmetic.
//
// Library changes need BOTH: the server started with -allow-writes (a
// server-wide kill switch) AND a signed-in admin. Anonymous visitors and
// regular users can never modify metadata.db or files in the library.
func requireWrites(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowWrites {
			http.Error(w, "This library is read-only. Restart the server with -allow-writes to enable editing, uploads and grouping.", http.StatusForbidden)
			return
		}
		u := currentUser(r)
		if u == nil {
			if wantsHTML(r) {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Error(w, "sign in as an admin to change the library", http.StatusUnauthorized)
			return
		}
		if !u.IsAdmin {
			http.Error(w, "only admins can change the library", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

var registerCalibreOnce sync.Once

func registerCalibreDriver() {
	registerCalibreOnce.Do(func() {
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
	})
}

func initCalibreDB() {
	registerCalibreDriver()
	var err error
	calibreDB, err = sql.Open("sqlite3_calibre", filepath.Join(baseDir, "metadata.db"))
	if err != nil {
		log.Fatal("failed to open search db: ", err)
	}
	// TODO: Performance, maybe we can increase this later
	calibreDB.SetMaxOpenConns(1) // supposedly, sqlite doesn't like concurrent writers, reads are fine with 1
}

type Book struct {
	Title        string  `json:"title"`
	AuthorSort   string  `json:"author_sort"`
	SeriesIndex  float64 `json:"series_index"`
	PubDate      string  `json:"pubdate"`
	Path         string  `json:"path"` // e.g "Author Name/Book Title" not direct link to specific file format
	UUID         string  `json:"uuid"`
	LastModified string  `json:"last_modified"`
}

type LibraryInfo struct {
	TotalPages int `json:"total_pages"`
}

var totalPages = 0

type PageData struct {
	Title string
}

type EditBookPageData struct { // TODO I Hate this, these page structs are all over the place, we should unify them
	UUID         string
	Metadata     *OPFMetadata
	Date         string // YYYY-MM-DD for <input type="date">, "" if missing/unparseable
	RawDate      string // original dc:date when it couldn't be parsed (kept on save)
	CoverModTime int64
}

func writePage(page int, books []Book) error {
	data, err := json.Marshal(books)
	if err != nil {
		return err
	}

	if err := os.MkdirAll("pages", 0o755); err != nil {
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
	flag.BoolVar(&allowWrites, "allow-writes", false, "allow admins to change the library (metadata edits, uploads, covers, groups)")
	flag.StringVar(&dataDir, "datadir", "data", "where CalibreFast keeps its own database (accounts, favorites, progress, shelves)")
	flag.BoolVar(&requireLogin, "require-login", false, "require signing in to browse anything (even the catalog)")
	flag.BoolVar(&publicRead, "public-read", false, "public read-only mode: let anonymous visitors read and download books (by default they can only browse the catalog)")
	flag.BoolVar(&openRegistration, "open-registration", false, "let anyone create an account (password, or GitHub/Google sign-in)")
	flag.StringVar(&publicURL, "public-url", "", "external base URL, e.g. https://books.example.com (needed for GitHub/Google sign-in)")
	createUserName := flag.String("create-user", "", "create a user (or reset their password) and exit; password from $CF_PASSWORD or stdin")
	createAdmin := flag.Bool("admin", false, "with -create-user: make the user an admin")
	flag.Parse()

	var err error
	if appDB, err = openAppDB(dataDir); err != nil {
		log.Fatal("app database: ", err)
	}
	if *createUserName != "" {
		if err := runCreateUser(*createUserName, *createAdmin); err != nil {
			log.Fatal(err)
		}
		return
	}
	pruneSessions()
	configureOAuth()
	if allowWrites {
		log.Println("-allow-writes: admins can change the library")
	} else {
		log.Println("read-only mode: start with -allow-writes to let admins edit, upload and group")
	}
	if adminCount() == 0 {
		log.Println("no admin account yet: create one with  -create-user NAME -admin")
	}
	if baseDir == "" {
		log.Fatal("missing -basedir")
	}

	dbPath := filepath.Join(baseDir, "metadata.db")

	if _, err := os.Stat(dbPath); err != nil {
		log.Fatal("metadata.db not found at: ", dbPath)
	}
	generatePages()
	initCalibreDB()
	groups = loadGroups(filepath.Join(baseDir, groupsFileName))
	preloadCaches()
	serveLibraryHTTP()
}

const searchWhere = `
FROM books b
LEFT JOIN books_authors_link bal ON bal.book = b.id
LEFT JOIN authors a ON a.id = bal.author
WHERE (b.title LIKE ? OR a.name LIKE ? OR a.sort LIKE ?)`

// searchTotalPages counts matching books and converts to a page count.
func searchTotalPages(q string) int {
	like := "%" + q + "%"
	var count int
	err := calibreDB.QueryRow(`SELECT COUNT(DISTINCT b.id) `+searchWhere, like, like, like).Scan(&count)
	if err != nil {
		log.Println("search count error:", err)
		return 0
	}
	return (count + booksPerPage - 1) / booksPerPage
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
	offset := (page - 1) * booksPerPage

	query := `
SELECT
    b.uuid,
    b.title,
    GROUP_CONCAT(COALESCE(a.sort, a.name, ''), ' & ') as author_sort,
    b.pubdate,
    b.path,
    b.last_modified` + searchWhere + `
GROUP BY b.id
ORDER BY b.sort
LIMIT ? OFFSET ?`

	rows, err := calibreDB.Query(query, like, like, like, booksPerPage, offset)
	if err != nil {
		log.Println("search error:", err)
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	defer func(rows *sql.Rows) {
		logErr(rows.Close(), "could not close rows in searchHandler")
	}(rows)
	logErr(encodeBooksJSONFromRows(rows, w), "failed to encode search results")
}

// encodeBooksJSONFromRows runs for dynamic operations that hit the database to
// retrieve lists of books and encode them as JSON.
func encodeBooksJSONFromRows(rows *sql.Rows, w http.ResponseWriter) error {
	books := make([]Book, 0)
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.UUID, &b.Title, &b.AuthorSort, &b.PubDate, &b.Path, &b.LastModified); err != nil {
			continue
		}
		uuidPathIndex.Store(b.UUID, b.Path)
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

	// serve css/js. Short max-age: no requests while fresh, and edits still
	// reach browsers within a couple of minutes (then a cheap 304 revalidation).
	staticFiles := http.StripPrefix("/static/", http.FileServer(http.Dir("./static")))
	http.Handle(
		"/static/",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "max-age=120")
			staticFiles.ServeHTTP(w, r)
		}),
	)

	// server cover getter
	http.HandleFunc("/cover/", coverHandler)            // Full cover image
	http.HandleFunc("/cover-thumb/", coverThumbHandler) // Thumbnail cover image

	// server get formats
	http.HandleFunc("/formats/", formatsHandler)

	// Downloads
	http.HandleFunc("/download/", requireReader(downloadHandler))

	// Uploads
	http.HandleFunc("/upload", requireWrites(uploadHandler))

	// Cover upload and management
	http.HandleFunc("/upload-cover/{uuid}", requireWrites(uploadCoverHandler))
	http.HandleFunc("/cover-status/{uuid}", coverStatusHandler)
	http.HandleFunc("/revert-cover/{uuid}", requireWrites(revertCoverHandler))

	// Individual Book metadata editor
	http.HandleFunc("/book/edit/{uuid}", requireWrites(func(w http.ResponseWriter, r *http.Request) {
		uuid := r.PathValue("uuid")
		if uuid == "" {
			http.Error(w, "Missing UUID", http.StatusBadRequest)
			return
		}

		bookPath, ok := uuidPathIndex.Load(uuid)
		if !ok {
			var err error
			bookPath, err = resolveBookPath(uuid)
			if err != nil {
				http.Error(w, "book not found", http.StatusNotFound)
				return
			}
		}

		opfPath := filepath.Join(baseDir, bookPath.(string), "metadata.opf")
		existingOPF, err := loadOPF(opfPath)
		if err != nil {
			http.Error(w, "Failed to load metadata.opf: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// A missing or odd date must not block editing.
		var dateStr, rawDate string
		if raw := strings.TrimSpace(existingOPF.Metadata.Date); raw != "" {
			if parsedDate, err := parseAnyDate(raw); err == nil {
				dateStr = parsedDate.Format("2006-01-02")
			} else {
				rawDate = raw
			}
		}

		coverStat, cErr := os.Stat(filepath.Join(baseDir, bookPath.(string), "cover.jpg"))
		var coverModTime int64
		if cErr == nil {
			coverModTime = coverStat.ModTime().UnixNano()
		}

		EditBookPageData := EditBookPageData{
			UUID:         uuid,
			Metadata:     &existingOPF.Metadata,
			Date:         dateStr,
			RawDate:      rawDate,
			CoverModTime: coverModTime,
		}

		err = templates.ExecuteTemplate(w, "book-edit.html", EditBookPageData)
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	}))

	// Get metadata options for a book
	http.HandleFunc("/metadata/{uuid}", GetOptionsForBook)

	// Apply updated metadata to a book
	http.HandleFunc("/metadata/{uuid}/apply", requireWrites(applyMetadataHandler))

	// Book Display
	http.HandleFunc("/book/get/", bookHandler)

	http.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		data := SearchPageData{Title: SiteTitle, Query: q}
		if q != "" {
			data.TotalPages = searchTotalPages(q)
		}
		err := templates.ExecuteTemplate(w, "search.html", data)
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	})

	http.HandleFunc("/api/search", searchHandler)

	http.HandleFunc("/read", requireReader(func(w http.ResponseWriter, r *http.Request) {
		err := templates.ExecuteTemplate(w, "reader.html", PageData{Title: SiteTitle})
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	}))

	// TODO: Can we combine this with the /read endpoint?
	http.HandleFunc("/read-pdf", requireReader(func(w http.ResponseWriter, r *http.Request) {
		err := templates.ExecuteTemplate(w, "reader-pdf.html", PageData{Title: SiteTitle})
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	}))

	http.HandleFunc("/epub/", requireReader(epubFileHandler))

	http.HandleFunc("/view/", requireReader(viewHandler))

	// authors/publishers/tags/languages: aggregate pages, filtered pages,
	// JSON APIs and the group editor API
	registerCategoryRoutes()

	log.Println("Listening on :" + hostPort)
	registerAuthRoutes()
	log.Fatal(http.ListenAndServe(":"+hostPort, csrfProtect(withAuth(http.DefaultServeMux))))
}

func logErr(err error, why string) {
	if err != nil {
		log.Println(why + " :: " + err.Error())
	}
}

func resolveBookPath(uuid string) (string, error) {
	if val, ok := uuidPathIndex.Load(uuid); ok {
		if path, ok2 := val.(string); ok2 && path != "" {
			return path, nil
		}
	}
	var path string
	err := calibreDB.QueryRow("SELECT path FROM books WHERE uuid = ?", uuid).Scan(&path)
	if err != nil {
		return "", err
	}
	uuidPathIndex.Store(uuid, path)
	return path, nil
}

// GetOptionsForBook retrieves available metadata options for a book (current merged + upload cached).
func GetOptionsForBook(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	if uuid == "" {
		http.Error(w, "Missing UUID", http.StatusBadRequest)
		return
	}

	path, err := resolveBookPath(uuid)
	if err != nil {
		http.Error(w, "No metadata found for this UUID", http.StatusNotFound)
		return
	}

	var metas []*UploadedBookMeta

	// Always try to include the current on-disk merged metadata.
	opf, err := loadOPF(filepath.Join(baseDir, path, "metadata.opf"))
	if err == nil {
		opf.Metadata.Date = normalizeDateOnly(opf.Metadata.Date)
		metas = append(metas, &UploadedBookMeta{
			OPFMetadata: opf.Metadata,
			Format:      "current",
			FileName:    "",
		})
	}

	// If there are cached per-format options from an upload, include those too.
	if val, ok := bookMetadataCache.Load(uuid); ok {
		cached := val.([]*UploadedBookMeta)
		metas = append(metas, cached...)
	}

	if len(metas) == 0 {
		http.Error(w, "No metadata found for this UUID", http.StatusNotFound)
		return
	}

	w.Header().Set("content-type", "application/json")
	json.NewEncoder(w).Encode(metas)
}

type ApplyMetadataRequest struct {
	Title       string         `json:"title"`
	Creators    string         `json:"creators"`
	Description string         `json:"description"`
	Publisher   string         `json:"publisher"`
	Date        string         `json:"date"`
	Languages   *string        `json:"languages"` // "eng, fra"; nil = not sent
	Language    string         `json:"language"`  // legacy single-language field
	Subjects    string         `json:"subjects"`
	Identifiers identifierList `json:"identifiers"`
}

// requestLanguages prefers the multi-language "languages" field and falls
// back to the legacy single "language" field.
func requestLanguages(req ApplyMetadataRequest) []string {
	if req.Languages != nil {
		return parseLanguagesString(*req.Languages)
	}
	return parseLanguagesString(req.Language)
}

// identifierList accepts a JSON array of {scheme, value, id} objects (lossless,
// used by the editor) or the legacy "scheme: value, ..." string.
type identifierList []opfIdentifier

func (l *identifierList) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*l = parseIdentifiersString(s)
		return nil
	}
	var ids []opfIdentifier
	if err := json.Unmarshal(data, &ids); err != nil {
		return err
	}
	out := make([]opfIdentifier, 0, len(ids))
	for _, id := range ids {
		id.Scheme = strings.TrimSpace(id.Scheme)
		id.Value = strings.TrimSpace(id.Value)
		id.ID = strings.TrimSpace(id.ID)
		if id.Scheme == "" && id.Value == "" {
			continue // blank row, nothing to keep
		}
		out = append(out, id)
	}
	*l = out
	return nil
}

func parseCreatorsString(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return []string{"Unknown"}
	}
	var raw []string
	if strings.Contains(s, ",") {
		raw = strings.Split(s, ",")
	} else if strings.Contains(s, " & ") {
		raw = strings.Split(s, " & ")
	} else {
		raw = []string{s}
	}
	creators := make([]string, 0, len(raw))
	for _, a := range raw {
		trimmed := strings.TrimSpace(a)
		if trimmed != "" {
			creators = append(creators, trimmed)
		}
	}
	if len(creators) == 0 {
		return []string{"Unknown"}
	}
	return creators
}

func parseSubjectsString(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return []string{}
	}
	raw := strings.Split(s, ",")
	subjects := make([]string, 0, len(raw))
	for _, sub := range raw {
		trimmed := strings.TrimSpace(sub)
		if trimmed != "" {
			subjects = append(subjects, trimmed)
		}
	}
	return subjects
}

func parseIdentifiersString(s string) []opfIdentifier {
	s = strings.TrimSpace(s)
	if s == "" {
		return []opfIdentifier{}
	}
	items := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '\n' || r == ';'
	})
	identifiers := make([]opfIdentifier, 0, len(items))
	for _, item := range items {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		if idx := strings.IndexAny(trimmed, ":="); idx != -1 {
			scheme := strings.TrimSpace(trimmed[:idx])
			val := strings.TrimSpace(trimmed[idx+1:])
			if scheme != "" && val != "" {
				identifiers = append(identifiers, opfIdentifier{
					Scheme: scheme,
					Value:  val,
				})
			}
		} else if isISBN(trimmed) {
			identifiers = append(identifiers, opfIdentifier{
				Scheme: "isbn",
				Value:  trimmed,
			})
		}
	}
	return identifiers
}

// unparseableStoredDate returns the book's current dc:date if it is set but
// not in a format parseAnyDate understands; otherwise "".
func unparseableStoredDate(uuid string) string {
	bookPath, ok := uuidPathIndex.Load(uuid)
	if !ok {
		p, err := resolveBookPath(uuid)
		if err != nil {
			return ""
		}
		bookPath = p
	}
	opf, err := loadOPF(filepath.Join(baseDir, bookPath.(string), "metadata.opf"))
	if err != nil {
		return ""
	}
	raw := strings.TrimSpace(opf.Metadata.Date)
	if raw == "" {
		return ""
	}
	if _, err := parseAnyDate(raw); err != nil {
		return raw
	}
	return ""
}

func applyMetadataHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	uuid := r.PathValue("uuid")
	if uuid == "" {
		http.Error(w, "Missing UUID", http.StatusBadRequest)
		return
	}

	var req ApplyMetadataRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "Unknown"
	}

	// <input type="date"> can't display a date we couldn't parse, so it comes
	// back empty. Don't let that silently overwrite the stored value.
	if strings.TrimSpace(req.Date) == "" {
		if raw := unparseableStoredDate(uuid); raw != "" {
			req.Date = raw
		}
	}

	meta := OPFMetadata{
		Title:       title,
		Creators:    parseCreatorsString(req.Creators),
		Description: strings.TrimSpace(req.Description),
		Publisher:   strings.TrimSpace(req.Publisher),
		Date:        normalizeDate(req.Date),
		Languages:   requestLanguages(req),
		Subjects:    parseSubjectsString(req.Subjects),
		Identifiers: req.Identifiers,
	}

	bookPath, err := updateBookInCalibreDB(calibreDB, baseDir, uuid, &meta)
	if err != nil {
		log.Println("apply metadata error:", err)
		http.Error(w, "Failed to update metadata: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Update cached format options if present
	if val, ok := bookMetadataCache.Load(uuid); ok {
		cached := val.([]*UploadedBookMeta)
		var updated []*UploadedBookMeta
		for _, m := range cached {
			if m.Format != "current" {
				updated = append(updated, m)
			}
		}
		bookMetadataCache.Store(uuid, updated)
	}

	// Refresh page1.json cache
	generatePage1()
	refreshAllCaches()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"uuid":    uuid,
		"path":    bookPath,
		"title":   meta.Title,
	})
}

// viewHandler serves the PDF version of a book for inline viewing in the browser.
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
	rows, err := db.Query("SELECT title, author_sort, series_index, pubdate, path, uuid, last_modified FROM books ORDER BY timestamp DESC")
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
			&book.LastModified,
		)
		if err != nil {
			panic(err)
		}

		// Trunicate timestamps to seconds
		t, _ := time.Parse(time.RFC3339Nano, book.LastModified) // We do have consistant times in the databse now right? right?
		timeStr := t.Format("20060102150405")                   // Truncate to seconds for cache-busting
		book.LastModified = timeStr                             // cache-busters!

		// cache last Modified time for book from database
		modTimeCache.Store(book.UUID, timeStr)

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
