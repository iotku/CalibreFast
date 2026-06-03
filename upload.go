package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
)

// UploadedBookMeta holds all metadata we can extract from an ebook file.
type UploadedBookMeta struct {
	Title      string
	Authors    []string
	Publisher  string
	PubDate    string // RFC3339
	Language   string
	Tags       []string
	Series     string
	SeriesIdx  float64
	Identifier string // ISBN or similar
	Format     string // "epub" | "pdf" | ...
	FileName   string // original filename
}

// --- EPUB metadata extraction ---

// opfPackage is the minimal OPF/DC structure we care about.
type opfPackage struct {
	XMLName  xml.Name    `xml:"package"`
	Metadata opfMetadata `xml:"metadata"`
}

type opfMetadata struct {
	Titles      []string     `xml:"title"`
	Creators    []opfCreator `xml:"creator"`
	Publisher   []string     `xml:"publisher"`
	Date        []string     `xml:"date"`
	Language    []string     `xml:"language"`
	Subjects    []string     `xml:"subject"`
	Identifiers []string     `xml:"identifier"`
	Meta        []opfMeta    `xml:"meta"`
}

type opfCreator struct {
	Name   string `xml:",chardata"`
	Role   string `xml:"role,attr"`
	FileAs string `xml:"file-as,attr"`
}

type opfMeta struct {
	Name     string `xml:"name,attr"`
	Content  string `xml:"content,attr"`
	Property string `xml:"property,attr"` // EPUB3
	Value    string `xml:",chardata"`
}

func extractEPUBMeta(r io.ReaderAt, size int64, filename string) (*UploadedBookMeta, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("not a valid zip/epub: %w", err)
	}

	// Find OPF path via META-INF/container.xml
	opfPath := ""
	for _, f := range zr.File {
		if f.Name == "META-INF/container.xml" {
			rc, err := f.Open()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			type rootfile struct {
				FullPath string `xml:"full-path,attr"`
			}
			type container struct {
				Rootfiles []rootfile `xml:"rootfiles>rootfile"`
			}
			var c container
			xml.Unmarshal(data, &c)
			if len(c.Rootfiles) > 0 {
				opfPath = c.Rootfiles[0].FullPath
			}
			break
		}
	}
	// Fallback: first .opf file found
	if opfPath == "" {
		for _, f := range zr.File {
			if strings.HasSuffix(f.Name, ".opf") {
				opfPath = f.Name
				break
			}
		}
	}
	if opfPath == "" {
		return nil, fmt.Errorf("could not find OPF in epub")
	}

	var opfData []byte
	for _, f := range zr.File {
		if f.Name == opfPath {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			opfData, _ = io.ReadAll(rc)
			rc.Close()
			break
		}
	}
	if opfData == nil {
		return nil, fmt.Errorf("could not read OPF file")
	}

	var pkg opfPackage
	if err := xml.Unmarshal(opfData, &pkg); err != nil {
		return nil, fmt.Errorf("failed to parse OPF: %w", err)
	}

	m := pkg.Metadata
	meta := &UploadedBookMeta{Format: "epub", FileName: filename}

	if len(m.Titles) > 0 {
		meta.Title = strings.TrimSpace(m.Titles[0])
	}
	for _, c := range m.Creators {
		if name := strings.TrimSpace(c.Name); name != "" {
			meta.Authors = append(meta.Authors, name)
		}
	}
	if len(m.Publisher) > 0 {
		meta.Publisher = strings.TrimSpace(m.Publisher[0])
	}
	if len(m.Date) > 0 {
		meta.PubDate = normalizeDate(m.Date[0])
	}
	if len(m.Language) > 0 {
		meta.Language = strings.TrimSpace(m.Language[0])
	}
	for _, s := range m.Subjects {
		if t := strings.TrimSpace(s); t != "" {
			meta.Tags = append(meta.Tags, t)
		}
	}
	for _, mv := range m.Meta {
		switch mv.Name {
		case "calibre:series":
			meta.Series = mv.Content
		case "calibre:series_index":
			if v, err := strconv.ParseFloat(mv.Content, 64); err == nil {
				meta.SeriesIdx = v
			}
		}
		if mv.Property == "belongs-to-collection" {
			meta.Series = strings.TrimSpace(mv.Value)
		}
	}
	for _, id := range m.Identifiers {
		if strings.Contains(strings.ToLower(id), "isbn") || isISBN(id) {
			meta.Identifier = strings.TrimSpace(id)
			break
		}
	}
	if meta.Identifier == "" && len(m.Identifiers) > 0 {
		meta.Identifier = strings.TrimSpace(m.Identifiers[0])
	}

	return meta, nil
}

// --- PDF metadata extraction via pdfcpu ---

func extractPDFMeta(data []byte, filename string) *UploadedBookMeta {
	meta := &UploadedBookMeta{Format: "pdf", FileName: filename}

	info, err := pdfapi.PDFInfo(bytes.NewReader(data), filename, nil, false, nil)
	if err != nil {
		log.Printf("upload: pdfcpu PDFInfo failed for %s: %v", filename, err)
	} else {
		meta.Title = strings.TrimSpace(info.Title)
		if info.Author != "" {
			meta.Authors = []string{strings.TrimSpace(info.Author)}
		}
		// PDF Info dict has no dedicated publisher field; Subject is occasionally
		// used for it, but more often it's genuine subject matter — leave it as tags.
		if info.Subject != "" {
			meta.Tags = []string{strings.TrimSpace(info.Subject)}
		}
		meta.PubDate = normalizeDate(info.CreationDate)
		// Keywords come back as []string directly from pdfcpu
		if len(info.Keywords) > 0 {
			meta.Tags = append(meta.Tags, info.Keywords...)
		}
	}

	// Fallbacks
	if len(meta.Authors) == 0 {
		meta.Authors = []string{"Unknown"}
	}
	if meta.Title == "" {
		meta.Title = strings.TrimSuffix(filename, filepath.Ext(filename))
	}

	return meta
}

// --- Metadata merging ---

// mergeMeta picks the best fields across multiple UploadedBookMeta values.
// EPUB metadata takes priority over PDF; first non-empty value wins per field.
func mergeMeta(metas []*UploadedBookMeta) *UploadedBookMeta {
	if len(metas) == 0 {
		return &UploadedBookMeta{Title: "Unknown", Authors: []string{"Unknown"}}
	}

	// Stable sort: epub first, everything else after
	sorted := make([]*UploadedBookMeta, 0, len(metas))
	for _, m := range metas {
		if m.Format == "epub" {
			sorted = append([]*UploadedBookMeta{m}, sorted...)
		} else {
			sorted = append(sorted, m)
		}
	}

	merged := &UploadedBookMeta{}
	for _, m := range sorted {
		if merged.Title == "" && m.Title != "" {
			merged.Title = m.Title
		}
		if len(merged.Authors) == 0 && len(m.Authors) > 0 {
			merged.Authors = m.Authors
		}
		if merged.Publisher == "" && m.Publisher != "" {
			merged.Publisher = m.Publisher
		}
		if merged.PubDate == "" && m.PubDate != "" {
			merged.PubDate = m.PubDate
		}
		if merged.Language == "" && m.Language != "" {
			merged.Language = m.Language
		}
		if len(merged.Tags) == 0 && len(m.Tags) > 0 {
			merged.Tags = m.Tags
		}
		if merged.Series == "" && m.Series != "" {
			merged.Series = m.Series
			merged.SeriesIdx = m.SeriesIdx
		}
		if merged.Identifier == "" && m.Identifier != "" {
			merged.Identifier = m.Identifier
		}
	}

	if merged.Title == "" {
		merged.Title = "Unknown"
	}
	if len(merged.Authors) == 0 {
		merged.Authors = []string{"Unknown"}
	}
	if merged.Language == "" {
		merged.Language = "und"
	}
	if merged.PubDate == "" {
		merged.PubDate = "0101-01-01 00:00:00+00:00"
	}
	if merged.SeriesIdx == 0 && merged.Series != "" {
		merged.SeriesIdx = 1.0
	}

	return merged
}

// --- Calibre DB insertion ---

var titleSortRE = regexp.MustCompile(`(?i)^(a|an|the)\s+`)

func titleSort(title string) string {
	title = strings.TrimSpace(title)

	m := titleSortRE.FindStringSubmatch(title)
	if m == nil {
		return title
	}

	article := strings.TrimSpace(m[1])
	rest := strings.TrimSpace(title[len(m[0]):])

	if rest == "" {
		return title
	}

	article = strings.ToUpper(article[:1]) + strings.ToLower(article[1:])

	return rest + ", " + article
}

func calibreDateTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000000-07:00")
}

// insertBookIntoCalibreDB inserts a book and all its formats into metadata.db,
// following Calibre's schema exactly so the library remains Calibre-compatible.
func insertBookIntoCalibreDB(db *sql.DB, meta *UploadedBookMeta, formats []formatEntry) (bookID int64, bookPath string, retErr error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if retErr != nil {
			tx.Rollback()
		}
	}()

	bookUUID := uuid.New().String()
	now := calibreDateTime(time.Now())

	authorDir := sanitizePath(meta.Authors[0])
	titleDir := sanitizePath(meta.Title)
	bookPath = authorDir + "/" + titleDir // TODO: We should add the ID to the end of the bookPath, but we only find that out after we add this

	authorSort := authorSortKey(meta.Authors[0])
	res, err := tx.Exec(`
		INSERT INTO books (title, sort, author_sort, timestamp, pubdate, series_index, path, uuid, has_cover, last_modified)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		meta.Title,
		meta.Title,
		authorSort,
		now,
		meta.PubDate,
		meta.SeriesIdx,
		bookPath,
		bookUUID,
		now,
	)
	if err != nil {
		retErr = fmt.Errorf("insert book: %w", err)
		return
	}
	bookID, err = res.LastInsertId()
	if err != nil {
		retErr = fmt.Errorf("last insert id: %w", err)
		return
	}

	for _, authorName := range meta.Authors {
		authorID, aErr := upsertAuthor(tx, authorName)
		if aErr != nil {
			retErr = fmt.Errorf("upsert author %q: %w", authorName, aErr)
			return
		}
		if _, aErr = tx.Exec(`INSERT OR IGNORE INTO books_authors_link (book, author) VALUES (?, ?)`, bookID, authorID); aErr != nil {
			retErr = fmt.Errorf("link author: %w", aErr)
			return
		}
	}

	for _, f := range formats {
		if _, fErr := tx.Exec(`
			INSERT INTO data (book, format, uncompressed_size, name)
			VALUES (?, ?, ?, ?)`,
			bookID,
			strings.ToUpper(f.Ext),
			f.Size,
			strings.TrimSuffix(f.Filename, filepath.Ext(f.Filename)),
		); fErr != nil {
			retErr = fmt.Errorf("insert format %s: %w", f.Ext, fErr)
			return
		}
	}

	if meta.Publisher != "" {
		pubID, pErr := upsertSimpleEntity(tx, "publishers", meta.Publisher)
		if pErr != nil {
			retErr = fmt.Errorf("upsert publisher: %w", pErr)
			return
		}
		if _, pErr = tx.Exec(`INSERT OR IGNORE INTO books_publishers_link (book, publisher) VALUES (?, ?)`, bookID, pubID); pErr != nil {
			retErr = fmt.Errorf("link publisher: %w", pErr)
			return
		}
	}

	for _, tag := range meta.Tags {
		tagID, tErr := upsertSimpleEntity(tx, "tags", tag)
		if tErr != nil {
			retErr = fmt.Errorf("upsert tag %q: %w", tag, tErr)
			return
		}
		if _, tErr = tx.Exec(`INSERT OR IGNORE INTO books_tags_link (book, tag) VALUES (?, ?)`, bookID, tagID); tErr != nil {
			retErr = fmt.Errorf("link tag: %w", tErr)
			return
		}
	}

	if meta.Series != "" {
		seriesID, sErr := upsertSimpleEntity(tx, "series", meta.Series)
		if sErr != nil {
			retErr = fmt.Errorf("upsert series: %w", sErr)
			return
		}
		if _, sErr = tx.Exec(`INSERT OR IGNORE INTO books_series_link (book, series) VALUES (?, ?)`, bookID, seriesID); sErr != nil {
			retErr = fmt.Errorf("link series: %w", sErr)
			return
		}
	}

	// Language and identifier are best-effort; don't abort the whole insert on failure.
	if meta.Language != "" && meta.Language != "und" {
		if langID, lErr := upsertLanguage(tx, meta.Language); lErr == nil {
			tx.Exec(`INSERT OR IGNORE INTO books_languages_link (book, lang_code) VALUES (?, ?)`, bookID, langID)
		}
	}
	if meta.Identifier != "" {
		idType, idVal := parseIdentifier(meta.Identifier)
		tx.Exec(`INSERT OR IGNORE INTO identifiers (book, type, val) VALUES (?, ?, ?)`, bookID, idType, idVal)
	}

	if err = tx.Commit(); err != nil {
		retErr = fmt.Errorf("commit: %w", err)
		return
	}

	return bookID, bookPath, nil
}

type formatEntry struct {
	Ext      string
	Filename string
	Size     int64
	TempPath string
}

func upsertAuthor(tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM authors WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := tx.Exec(`INSERT INTO authors (name, sort) VALUES (?, ?)`, name, authorSortKey(name))
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	return id, err
}

func upsertSimpleEntity(tx *sql.Tx, table, name string) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM `+table+` WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := tx.Exec(`INSERT INTO `+table+` (name) VALUES (?)`, name)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	return id, err
}

func upsertLanguage(tx *sql.Tx, lang string) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM languages WHERE lang_code = ?`, lang).Scan(&id)
	if err == sql.ErrNoRows {
		res, err := tx.Exec(`INSERT INTO languages (lang_code) VALUES (?)`, lang)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	return id, err
}

func parseIdentifier(id string) (string, string) {
	if idx := strings.Index(id, ":"); idx >= 0 {
		return strings.ToLower(id[:idx]), id[idx+1:]
	}
	if isISBN(id) {
		return "isbn", id
	}
	return "other", id
}

var isbnRe = regexp.MustCompile(`^(?:97[89])?\d{9}[\dXx]$`)

func isISBN(s string) bool {
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	return isbnRe.MatchString(s)
}

func authorSortKey(name string) string {
	parts := strings.Fields(name)
	if len(parts) <= 1 {
		return name
	}
	last := parts[len(parts)-1]
	first := strings.Join(parts[:len(parts)-1], " ")
	return last + ", " + first
}

func sanitizePath(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ' ', r == '-', r == '_', r == '.', r == '\'':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.TrimSpace(b.String())
}

func normalizeDate(s string) string {
	s = strings.TrimSpace(s)
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02",
		"2006",
		"January 2, 2006",
		"Jan 2006",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return s
}

// --- HTTP handler ---

type UploadResult struct {
	Title   string   `json:"title"`
	Authors []string `json:"authors"`
	Formats []string `json:"formats"`
	UUID    string   `json:"uuid"`
	Error   string   `json:"error,omitempty"`
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if err := templates.ExecuteTemplate(w, "upload.html", PageData{Title: SiteTitle + " — Upload"}); err != nil {
			http.Error(w, err.Error(), 500)
		}
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 512 MB max upload
	if err := r.ParseMultipartForm(512 << 20); err != nil {
		http.Error(w, "failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	files := r.MultipartForm.File["books"]
	if len(files) == 0 {
		http.Error(w, "no files uploaded", http.StatusBadRequest)
		return
	}

	groups := groupFilesByStem(files)

	var results []UploadResult
	for _, group := range groups {
		results = append(results, processUploadGroup(group))
	}

	generatePage1()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(results); err != nil {
		log.Println("upload: failed to encode results:", err)
	}
}

// groupFilesByStem groups files that share the same base name (sans extension).
// "My Book.epub" and "My Book.pdf" map to the same stem and become one book.
func groupFilesByStem(files []*multipart.FileHeader) map[string][]*multipart.FileHeader {
	groups := make(map[string][]*multipart.FileHeader)
	for _, fh := range files {
		ext := strings.ToLower(filepath.Ext(fh.Filename))
		if !hasBookExt(ext) {
			continue
		}
		stem := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(fh.Filename, filepath.Ext(fh.Filename))))
		groups[stem] = append(groups[stem], fh)
	}
	return groups
}

func processUploadGroup(files []*multipart.FileHeader) UploadResult {
	var metas []*UploadedBookMeta
	var entries []formatEntry

	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			log.Println("upload: open file header:", err)
			continue
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			log.Println("upload: read file:", err)
			continue
		}

		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fh.Filename), "."))

		var meta *UploadedBookMeta
		switch ext {
		case "epub":
			meta, err = extractEPUBMeta(bytes.NewReader(data), int64(len(data)), fh.Filename)
			if err != nil {
				log.Printf("upload: epub meta extraction failed for %s: %v", fh.Filename, err)
				meta = &UploadedBookMeta{
					Title:    strings.TrimSuffix(fh.Filename, filepath.Ext(fh.Filename)),
					Authors:  []string{"Unknown"},
					Format:   "epub",
					FileName: fh.Filename,
				}
			}
		case "pdf":
			meta = extractPDFMeta(data, fh.Filename)
		default:
			meta = &UploadedBookMeta{
				Title:    strings.TrimSuffix(fh.Filename, filepath.Ext(fh.Filename)),
				Authors:  []string{"Unknown"},
				Format:   ext,
				FileName: fh.Filename,
			}
		}
		metas = append(metas, meta)

		tmp, err := os.CreateTemp("", "calibrefast-upload-*."+ext)
		if err != nil {
			log.Println("upload: create temp:", err)
			continue
		}
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			log.Println("upload: write temp:", err)
			continue
		}
		tmp.Close()

		entries = append(entries, formatEntry{
			Ext:      ext,
			Filename: fh.Filename,
			Size:     int64(len(data)),
			TempPath: tmp.Name(),
		})
	}

	merged := mergeMeta(metas)

	destDir := filepath.Join(baseDir, sanitizePath(merged.Authors[0]), sanitizePath(merged.Title))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		cleanupTemps(entries)
		return UploadResult{Error: "failed to create book directory: " + err.Error()}
	}

	baseName := sanitizePath(merged.Title)
	for i := range entries {
		dest := filepath.Join(destDir, baseName+"."+entries[i].Ext)
		if _, err := os.Stat(dest); err == nil {
			// Avoid overwriting an existing file
			dest = filepath.Join(destDir, baseName+"_"+strconv.FormatInt(time.Now().UnixNano(), 36)+"."+entries[i].Ext)
		}
		if err := os.Rename(entries[i].TempPath, dest); err != nil {
			// os.Rename fails across mount points; fall back to copy+delete
			if err2 := copyFile(entries[i].TempPath, dest); err2 != nil {
				log.Printf("upload: failed to move %s to %s: %v / %v", entries[i].TempPath, dest, err, err2)
				os.Remove(entries[i].TempPath)
				continue
			}
			os.Remove(entries[i].TempPath)
		}
		entries[i].Filename = filepath.Base(dest)
		entries[i].TempPath = ""
	}

	bookID, bookPath, err := insertBookIntoCalibreDB(searchDB, merged, entries)
	if err != nil {
		for _, e := range entries {
			os.Remove(filepath.Join(destDir, e.Filename))
		}
		os.Remove(destDir)
		return UploadResult{Error: "failed to insert into database: " + err.Error()}
	}
	_ = bookID

	var newUUID string
	searchDB.QueryRow(`SELECT uuid FROM books WHERE path = ?`, bookPath).Scan(&newUUID)
	if newUUID != "" {
		coverIndex.Store(newUUID, bookPath)
	}

	formats := make([]string, 0, len(entries))
	for _, e := range entries {
		formats = append(formats, e.Ext)
	}

	return UploadResult{
		Title:   merged.Title,
		Authors: merged.Authors,
		Formats: formats,
		UUID:    newUUID,
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func cleanupTemps(entries []formatEntry) {
	for _, e := range entries {
		if e.TempPath != "" {
			os.Remove(e.TempPath)
		}
	}
}

// TODO: I would just want to prepend the new books (we don't actually care if page1 becomes more than 30 books, it'll be regenerated on the next run)
// generatePage1 rewrites pages/page1.json with the 30 most recently added books.
func generatePage1() {
	rows, err := searchDB.Query(`
		SELECT title, author_sort, series_index, pubdate, path, uuid
		FROM books
		ORDER BY timestamp DESC
		LIMIT 30`)
	if err != nil {
		log.Println("generatePage1: query failed:", err)
		return
	}
	defer rows.Close()

	books := make([]Book, 0, 30)
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.Title, &b.AuthorSort, &b.SeriesIndex, &b.PubDate, &b.Path, &b.UUID); err != nil {
			continue
		}
		coverIndex.Store(b.UUID, b.Path)
		books = append(books, b)
	}

	if err := writePage(1, books); err != nil {
		log.Println("generatePage1: write failed:", err)
	}
}
