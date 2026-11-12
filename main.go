package main

// Browser code is TypeScript (and CSS) in web/: tsgo, the Go-native
// TypeScript compiler, type-checks it, then tools/jsbuild bundles and
// minifies it into static/dist/ with esbuild (see assets.go). Both are
// pinned in their own module, tools/go.mod, so they don't raise the Go
// version or dependencies needed to build the server.
//go:generate go tool -modfile=tools/go.mod tsgo -p tsconfig.json
//go:generate go -C tools run ./jsbuild

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	Authors      string  `json:"authors"` // names, "A & B" (bookAuthorsSQL)
	SeriesIndex  float64 `json:"series_index"`
	PubDate      string  `json:"pubdate"`
	Path         string  `json:"path"` // e.g "Author Name/Book Title" not direct link to specific file format
	UUID         string  `json:"uuid"`
	LastModified string  `json:"last_modified"`
}

type LibraryInfo struct {
	TotalBooks int `json:"total_books"`
	PageSize   int `json:"page_size"`
	TotalPages int `json:"total_pages"`
}

// IndexPageData is passed to index.html.
type IndexPageData struct {
	Title        string
	TotalBooks   int
	PagesVersion string
}

var (
	// totalBooks is how many books pages/*.json hold, set when they're written.
	totalBooks int
	// pagesVersion changes whenever pages/*.json are rewritten. index.html
	// requests pageN.json?v=<it>, so the files can be cached for good.
	pagesVersion atomic.Int64
)

func currentPagesVersion() string { return strconv.FormatInt(pagesVersion.Load(), 36) }

type PageData struct {
	Title string
}

// ReaderPageData is what the readers' templates need (templates/reader-parts.html).
type ReaderPageData struct {
	Title     string
	BookTitle string // "" when ?uuid names no book; the page says so itself
	Search    bool   // offer in-book search (EPUB)
	// Preview: opened from the metadata editor to look something up. Nothing
	// is saved (position, bookmarks, highlights, the reading status, a copy
	// kept offline); web/reader-preview.ts. Always so for a book in the
	// import queue (?import=<id>), which isn't in the library yet.
	Preview bool
}

// readerPage serves a reader's page. The book itself is fetched by the
// page's script, from /download (or /import/{id}/file for a waiting book).
func readerPage(name string, search bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		data := ReaderPageData{Title: SiteTitle, Search: search, Preview: q.Get("preview") != "" || q.Has("import")}
		if id := q.Get("import"); id != "" {
			if isSafeUUIDComponent(id) {
				if it := imports.get(id); it != nil {
					data.BookTitle = it.Merged.Title
				}
			}
		} else {
			_ = calibreDB.QueryRow(`SELECT title FROM books WHERE uuid = ?`, q.Get("uuid")).Scan(&data.BookTitle)
		}
		if data.Preview {
			w.Header().Set("Cache-Control", "no-store")
		}
		if err := templates.ExecuteTemplate(w, name, data); err != nil {
			http.Error(w, err.Error(), 500)
		}
	}
}

type EditBookPageData struct { // TODO I Hate this, these page structs are all over the place, we should unify them
	UUID         string
	Metadata     *OPFMetadata
	Date         string // YYYY-MM-DD for <input type="date">, "" if missing/unparseable
	RawDate      string // original dc:date when it couldn't be parsed (kept on save)
	CoverModTime int64
	Import       *importPageData // a book waiting in the import queue (imports.go); nil for one in the library
	ReadLink     string          // the book in a reader's preview mode, to look things up ("" without an EPUB or PDF)
	Lookups      bool            // show the online lookup (Open Library); off with -no-lookups
	GoogleBooks  bool            // offer Google Books: only with CF_GOOGLE_BOOKS_KEY, and not -no-google-books
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

	// Write and rename, so a request never sees half a file.
	tmp := filename + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filename)
}

// servePages serves /pages/ from dir (pages/*.json). A request with the
// current ?v= (what index.html asks for) may be cached for good: the
// version changes when the files do. Anything else is revalidated.
func servePages(dir string) http.Handler {
	files := http.StripPrefix("/pages/", http.FileServer(http.Dir(dir)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") == currentPagesVersion() {
			w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func main() {
	flag.StringVar(&baseDir, "basedir", "", "path to Calibre library root")
	flag.StringVar(&cacheDir, "cachedir", "", "path to cache location")
	flag.StringVar(&hostPort, "port", "8080", "port to listen on")
	flag.BoolVar(&allowWrites, "allow-writes", false, "allow admins to change the library (metadata edits, uploads, covers, groups)")
	flag.StringVar(&dataDir, "datadir", "data", "where CalibreFast keeps its own database (accounts, favorites, progress, shelves)")
	flag.BoolVar(&publicCatalog, "public", false, "let anonymous visitors browse the library catalog (by default everything requires signing in)")
	flag.BoolVar(&publicRead, "public-read", false, "public read-only mode: anonymous visitors can also read and download books (implies -public)")
	flag.BoolVar(&allowRegistrations, "allow-registrations", false, "let anyone create an account from the web interface (only on trusted networks: bots will try)")
	flag.BoolVar(&noLookups, "no-lookups", false, "never look books up online (hides the metadata editor's lookup bar)")
	flag.BoolVar(&noGoogleBooks, "no-google-books", false, "never offer Google Books in the metadata editor (Open Library only)")
	flag.StringVar(&publicURL, "public-url", "", "external base URL, e.g. https://books.example.com (needed for GitHub/Google sign-in)")
	flag.StringVar(&watchDir, "watch-dir", "", "folder to watch for new books; they wait in the import queue for an admin to review (needs -allow-writes)")
	flag.BoolVar(&watchForcePolling, "watch-dir-force-polling", false, "check -watch-dir every 30 seconds instead of relying on the system to report changes (for network and container mounts where it doesn't)")
	flag.StringVar(&recycleDir, "recycle-bin", "", "where books removed from the library are kept until emptied on the server (default <datadir>/recycle-bin)")
	emptyRecycle := flag.String("empty-recycle-bin", "", "permanently delete Recycling Bin batches older than this (e.g. 30d, 12h; 0 for all) and exit")
	createUserName := flag.String("create-user", "", "create a user (or reset their password) and exit; password from $CF_PASSWORD or stdin")
	createAdmin := flag.Bool("admin", false, "with -create-user: make the user an admin")
	flag.Parse()

	if *emptyRecycle != "" {
		age, err := parseAge(*emptyRecycle)
		if err != nil {
			log.Fatal(err)
		}
		if err := emptyRecycleBin(age, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}

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
	if publicRead {
		publicCatalog = true
	}
	pruneSessions()
	configureOAuth()
	configureLookups()
	if allowWrites {
		log.Println("-allow-writes: admins can change the library")
	} else {
		log.Println("read-only mode: start with -allow-writes to let admins edit, upload and group")
	}
	switch {
	case publicRead:
		log.Println("-public-read: anyone who can reach this server can browse, read and download every book")
	case publicCatalog:
		log.Println("-public: anyone who can reach this server can browse the catalog; reading needs an account")
	default:
		log.Println("private library: sign-in required for everything (use -public to open the catalog)")
	}
	if allowRegistrations {
		log.Println("WARNING: -allow-registrations: anyone who can reach this server can create an account; bots will try. Only use on trusted networks")
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
	startImports()
	serveLibraryHTTP()
}

const searchWhere = `
FROM books b
LEFT JOIN books_authors_link bal ON bal.book = b.id
LEFT JOIN authors a ON a.id = bal.author
WHERE (b.title LIKE ? OR a.name LIKE ? OR a.sort LIKE ?)`

// searchCount counts the books matching q.
func searchCount(q string) int {
	like := "%" + q + "%"
	var count int
	err := calibreDB.QueryRow(`SELECT COUNT(DISTINCT b.id) `+searchWhere, like, like, like).Scan(&count)
	if err != nil {
		log.Println("search count error:", err)
		return 0
	}
	return count
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
    ` + bookAuthorsSQL + ` AS authors,
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

// coverStamp turns a Calibre last_modified value into the ?t= cache-buster
// used on cover URLs ("20060102150405"). Every page must produce the same
// string for a book, or the browser treats the thumbnail URLs as different
// images and refetches them (e.g. the book modal after a search).
func coverStamp(lastModified string) string {
	s := strings.TrimSpace(lastModified)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05-07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format("20060102150405")
		}
	}
	// Unknown format: keep its digits so the value still changes with edits.
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

// encodeBooksJSONFromRows runs for dynamic operations that hit the database to
// retrieve lists of books and encode them as JSON.
func encodeBooksJSONFromRows(rows *sql.Rows, w http.ResponseWriter) error {
	books := make([]Book, 0)
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.UUID, &b.Title, &b.Authors, &b.PubDate, &b.Path, &b.LastModified); err != nil {
			continue
		}
		b.LastModified = coverStamp(b.LastModified)
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
	template.New("").Funcs(template.FuncMap{
		"topbar":       topbarData,
		"asset":        assetURL,
		"booksPerPage": func() int { return booksPerPage },
	}).ParseGlob("templates/*.html"),
)

// topbarData builds the data for templates/topbar.html from key/value pairs,
// e.g. {{ topbar "Active" "authors" "Title" .Display }}. See that file for
// the keys.
func topbarData(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, errors.New("topbar: odd number of arguments")
	}
	m := map[string]any{"Site": SiteTitle, "Active": "", "Title": "", "NavTitle": false, "Query": "", "NoSearch": false}
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("topbar: key %v is not a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

func serveLibraryHTTP() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		data := IndexPageData{
			Title:        "Library",
			TotalBooks:   totalBooks,
			PagesVersion: currentPagesVersion(),
		}

		err := templates.ExecuteTemplate(w, "index.html", data)
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	})

	// serve generated json pages
	http.Handle("/pages/", servePages("pages"))

	http.HandleFunc("/library-info", func(w http.ResponseWriter, r *http.Request) {
		logErr(json.NewEncoder(w).Encode(LibraryInfo{
			TotalBooks: totalBooks,
			PageSize:   booksPerPage,
			TotalPages: (totalBooks + booksPerPage - 1) / booksPerPage,
		}), "could not encode library info")
	})

	// Scripts, styles and libraries (assets.go): cached for good.
	http.HandleFunc("GET /static/dist/", serveAsset)
	http.HandleFunc("GET /static/vendor/", serveAsset)

	// server cover getter
	http.HandleFunc("/cover/", coverHandler)            // Full cover image
	http.HandleFunc("/cover-thumb/", coverThumbHandler) // Thumbnail cover image
	go runThumbBacklog()                                // thumbnails of cancelled requests, made when idle

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

	// The watch folder's import queue (imports.go)
	registerImportRoutes()

	// Removing books, and the Recycling Bin (recycle.go)
	registerRecycleRoutes()

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
			ReadLink:     previewReadLink("uuid", uuid, extractFormats(resolveFormats(uuid, baseDir, bookPath.(string)))),
			Lookups:      !noLookups,
			GoogleBooks:  googleBooksEnabled(),
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

	// ISBNs found in the book's own files (isbn.go)
	http.HandleFunc("/metadata/{uuid}/isbns", requireWrites(isbnCandidatesHandler))

	// Metadata from Open Library and Google Books, and Open Library covers
	// (lookup.go)
	http.HandleFunc("GET /metadata/lookup", requireWrites(requireLookups(lookupHandler)))
	http.HandleFunc("GET /metadata/lookup/cover", requireWrites(requireLookups(lookupCoverHandler)))

	// Book Display
	http.HandleFunc("/book/get/", bookHandler)

	http.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		data := SearchPageData{Title: SiteTitle, Query: q}
		if q != "" {
			data.TotalBooks = searchCount(q)
		}
		err := templates.ExecuteTemplate(w, "search.html", data)
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	})

	http.HandleFunc("/api/search", searchHandler)

	http.HandleFunc("/read", requireReader(readerPage("reader.html", true)))
	http.HandleFunc("/read-pdf", requireReader(readerPage("reader-pdf.html", false)))

	http.HandleFunc("/view/", requireReader(viewHandler))

	// authors/publishers/tags/languages: aggregate pages, filtered pages,
	// JSON APIs and the group editor API
	registerCategoryRoutes()
	registerWorkerRoutes() // offline reading (sw.go)

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

	// The editor doesn't show library identifiers (calibre, uuid): saving
	// keeps the book's own. Copies, so the cached options keep theirs.
	shown := make([]UploadedBookMeta, len(metas))
	for i, m := range metas {
		shown[i] = *m
		_, shown[i].Identifiers = splitLibraryIdentifiers(m.Identifiers)
	}

	w.Header().Set("content-type", "application/json")
	json.NewEncoder(w).Encode(shown)
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

// parseCreatorsString reads the editor's authors field ("A & B":
// splitAuthors), with "Unknown" for none, as Calibre does.
func parseCreatorsString(s string) []string {
	if creators := splitAuthors(s); len(creators) > 0 {
		return creators
	}
	return []string{"Unknown"}
}

func parseSubjectsString(s string) []string {
	return uniqueTags(strings.Split(s, ","))
}

// uniqueTags trims tags and drops empty ones and repeats, ignoring case: the
// first spelling stays. A book never has the same tag twice, whether it came
// from the editor, a file or a lookup.
func uniqueTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if k := strings.ToLower(t); t != "" && !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	return out
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

// metadataFromRequest is the metadata the editor sent, tidied: a book always
// has a title and an author ("Unknown", as Calibre does).
func metadataFromRequest(req ApplyMetadataRequest) OPFMetadata {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "Unknown"
	}
	return OPFMetadata{
		Title:       title,
		Creators:    parseCreatorsString(req.Creators),
		Description: strings.TrimSpace(req.Description),
		Publisher:   strings.TrimSpace(req.Publisher),
		Date:        normalizeDate(req.Date),
		Languages:   requestLanguages(req),
		Subjects:    parseSubjectsString(req.Subjects),
		Identifiers: req.Identifiers,
	}
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

	// <input type="date"> can't display a date we couldn't parse, so it comes
	// back empty. Don't let that silently overwrite the stored value.
	if strings.TrimSpace(req.Date) == "" {
		if raw := unparseableStoredDate(uuid); raw != "" {
			req.Date = raw
		}
	}

	meta := metadataFromRequest(req)

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
	if err := writeLibraryPages(); err != nil {
		panic(err)
	}
}

// writeLibraryPages writes pages/*.json from metadata.db, at startup.
// Removing and restoring books only edit the pages concerned (library_pages.go).
func writeLibraryPages() error {
	pagesMu.Lock()
	defer pagesMu.Unlock()
	bookPages.Clear()
	pageSlots.Clear()
	db, err := sql.Open("sqlite3", filepath.Join(baseDir, "metadata.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	// create paginated entries as json for each book in metadata.db (booksPerPage per page)
	rows, err := db.Query("SELECT b.title, " + bookAuthorsSQL + ", b.series_index, b.pubdate, b.path, b.uuid, b.last_modified FROM books b ORDER BY b.timestamp DESC")
	if err != nil {
		return err
	}
	defer rows.Close()
	page := 1
	books := make([]Book, 0, booksPerPage)

	for rows.Next() {
		var book Book

		err = rows.Scan(
			&book.Title,
			&book.Authors,
			&book.SeriesIndex,
			&book.PubDate,
			&book.Path,
			&book.UUID,
			&book.LastModified,
		)
		if err != nil {
			return err
		}

		timeStr := coverStamp(book.LastModified)
		book.LastModified = timeStr

		// cache last Modified time for book from database
		modTimeCache.Store(book.UUID, timeStr)

		// cache cover path
		uuidPathIndex.Store(book.UUID, book.Path)
		notePage(book.UUID, page)

		// add book to the arrays
		books = append(books, book)

		if len(books) >= booksPerPage {
			// save to file
			if err := writePage(page, books); err != nil {
				return err
			}
			page++
			books = make([]Book, 0, booksPerPage)
		}
	}

	// write final partial page
	if len(books) > 0 {
		err = writePage(page, books)
		if err != nil {
			return err
		}
	}

	if err := rows.Err(); err != nil {
		return err
	}
	totalBooks = (page-1)*booksPerPage + len(books)
	pagesVersion.Store(time.Now().UnixNano())
	return nil
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

// previewReadLink is a book's reader in preview mode (ReaderPageData.Preview),
// for the metadata editor: EPUB when there is one, else PDF, else "". param
// names the book: "uuid" in the library, "import" in the import queue.
func previewReadLink(param, id string, formats []string) string {
	q := param + "=" + url.QueryEscape(id) + "&preview=1"
	for _, f := range formats {
		if strings.EqualFold(f, "epub") {
			return "/read?" + q
		}
	}
	for _, f := range formats {
		if strings.EqualFold(f, "pdf") {
			return "/read-pdf?" + q
		}
	}
	return ""
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
	noteDownload(uuid, format) // for KOReader progress sync (kosync_digest.go)
}
