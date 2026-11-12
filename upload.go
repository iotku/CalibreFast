package main

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
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
	"sync"
	"time"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
)

var bookMetadataCache sync.Map  // Key: SessionID (string), Value: []UploadedBookMeta
const undefinedLanguage = "und" // ISO 639-2 code for undetermined language

// UploadedBookMeta holds all metadata we can extract from an ebook file.
type UploadedBookMeta struct {
	OPFMetadata
	Format   string // "epub" | "pdf" | ...
	FileName string // original filename
}

// epubPackagePath is the path of an EPUB's package document (the OPF):
// from META-INF/container.xml, else the first .opf in the archive.
func epubPackagePath(zr *zip.Reader) (string, error) {
	if data, err := readZipEntry(zr, "META-INF/container.xml", 1<<20); err == nil {
		var c struct {
			Rootfiles []struct {
				FullPath string `xml:"full-path,attr"`
			} `xml:"rootfiles>rootfile"`
		}
		if xmlUnmarshal(data, &c) == nil && len(c.Rootfiles) > 0 && c.Rootfiles[0].FullPath != "" {
			return c.Rootfiles[0].FullPath, nil
		}
	}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".opf") {
			return f.Name, nil
		}
	}
	return "", errors.New("could not find OPF in epub")
}

// readZipEntry reads one file from a zip, refusing entries over max bytes.
func readZipEntry(zr *zip.Reader, name string, max int64) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, max+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > max {
			return nil, fmt.Errorf("%s is larger than %d bytes", name, max)
		}
		return data, nil
	}
	return nil, fmt.Errorf("%s not found", name)
}

// --- EPUB metadata extraction ---
func extractEPUBMeta(r io.ReaderAt, size int64, filename string) (*UploadedBookMeta, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("not a valid zip/epub: %w", err)
	}

	opfPath, err := epubPackagePath(zr)
	if err != nil {
		return nil, err
	}
	opfData, err := readZipEntry(zr, opfPath, 16<<20)
	if err != nil {
		return nil, fmt.Errorf("could not read OPF file: %w", err)
	}

	var pkg OPF
	if err := xmlUnmarshal(opfData, &pkg); err != nil {
		return nil, fmt.Errorf("failed to parse OPF: %w", err)
	}

	m := pkg.Metadata
	meta := &UploadedBookMeta{OPFMetadata: m, Format: "epub", FileName: filename}

	return meta, nil
}

// --- PDF metadata extraction via pdfcpu ---
func extractPDFMeta(r io.ReadSeeker, filename string) *OPF {
	opf := &OPF{
		Metadata: OPFMetadata{},
	}
	meta := &opf.Metadata
	meta.Languages = []string{undefinedLanguage} // undetermined; caller can override

	info, err := pdfapi.PDFInfo(context.Background(), r, filename, nil, false, nil)
	if err != nil {
		log.Printf("upload: pdfcpu PDFInfo failed for %s: %v", filename, err)
	} else {
		meta.Title = strings.TrimSpace(info.Title)
		if info.Author != "" {
			meta.Creators = []string{strings.TrimSpace(info.Author)}
		}
		if info.Subject != "" {
			meta.Subjects = []string{strings.TrimSpace(info.Subject)}
		}
		meta.Date = normalizeDate(info.CreationDate)
		if len(info.Keywords) > 0 {
			meta.Subjects = append(meta.Subjects, info.Keywords...)
		}
	}

	if len(meta.Creators) == 0 {
		meta.Creators = []string{"Unknown"}
	}
	if meta.Title == "" {
		meta.Title = strings.TrimSuffix(filename, filepath.Ext(filename))
	}
	return opf
}

// --- Metadata merging ---
func mergeMeta(metas []*UploadedBookMeta) *UploadedBookMeta {
	if len(metas) == 0 {
		return &UploadedBookMeta{
			OPFMetadata: OPFMetadata{
				Title:    "Unknown",
				Creators: []string{"Unknown"},
			},
		}
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
		if len(m.Title) > len(merged.Title) {
			merged.Title = m.Title
		}
		if len(m.Creators) > len(merged.Creators) {
			merged.Creators = m.Creators
		}
		if len(m.Publisher) > len(merged.Publisher) {
			merged.Publisher = m.Publisher
		}
		if len(m.Date) > len(merged.Date) {
			merged.Date = m.Date
		}
		// Prefer the source declaring the most real (non-"und") languages.
		if len(cleanLanguages(m.Languages)) > len(cleanLanguages(merged.Languages)) {
			merged.Languages = m.Languages
		}
		if len(m.Subjects) > len(merged.Subjects) {
			merged.Subjects = m.Subjects
		}
		if len(m.Description) > len(merged.Description) {
			merged.Description = m.Description
		}
		// TODO: confirm series and seriesIdx is in meta
		if len(m.Meta) > len(merged.Meta) {
			merged.Meta = m.Meta
		}
		if len(m.Identifiers) > len(merged.Identifiers) {
			merged.Identifiers = m.Identifiers
		}
	}

	// A title from any file's metadata beats a filename; only when none has
	// one does the first file's name (an EPUB's, if there is one) stand in.
	if strings.TrimSpace(merged.Title) == "" {
		merged.Title = "Unknown"
		for _, m := range sorted {
			if stem := strings.TrimSuffix(m.FileName, filepath.Ext(m.FileName)); stem != "" {
				merged.Title = stem
				break
			}
		}
	}
	if len(merged.Creators) == 0 {
		merged.Creators = []string{"Unknown"}
	}
	if len(cleanLanguages(merged.Languages)) == 0 {
		merged.Languages = []string{undefinedLanguage}
	}
	if merged.Date == "" {
		merged.Date = "0101-01-01 00:00:00+00:00"
	}
	// TODO: do we really want to set a series idx at all if there is none?
	// if merged.SeriesIdx == 0 && merged.Series != "" {
	// 	merged.SeriesIdx = 1.0
	// }
	//
	// TODO: should we set a default empty SUBJECT (tags) array?
	return merged
}

type formatEntry struct { // TODO: I don't really like this hanging around randomly
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

func parseIdentifier(identifier opfIdentifier) (string, string) {
	scheme := strings.ToLower(strings.TrimSpace(identifier.Scheme))
	value := strings.TrimSpace(identifier.Value)
	// normalize common schemes
	switch scheme {
	case "isbn":
		return "isbn", strings.ReplaceAll(value, "-", "")
	case "uuid":
		return "uuid", value
	default:
		return scheme, value
	}
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

func parseAnyDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty date string")
	}

	clean := s
	// Handle PDF date prefix "D:" or "d:"
	if strings.HasPrefix(clean, "D:") || strings.HasPrefix(clean, "d:") {
		clean = clean[2:]
	}
	// Remove PDF timezone apostrophes e.g. +00'00' or -05'00'
	if strings.Contains(clean, "'") {
		clean = strings.ReplaceAll(clean, "'", "")
	}

	layouts := []string{
		"2006-01-02 15:04:05.000000-07:00",
		"2006-01-02 15:04:05.000000",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02",
		"2006-01",
		"2006",
		"January 2, 2006",
		"2 January 2006",
		"Jan 2, 2006",
		"Jan 2006",
		"January 2006",
		"20060102150405-0700",
		"20060102150405Z0700",
		"20060102150405Z",
		"20060102150405",
		"20060102",
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, clean); err == nil {
			return t, nil
		}
	}

	if clean != s {
		for _, layout := range layouts {
			if t, err := time.Parse(layout, s); err == nil {
				return t, nil
			}
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse date %q", s)
}

func normalizeDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "0101-01-01 00:00:00+00:00"
	}
	t, err := parseAnyDate(s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2006-01-02 15:04:05-07:00")
}

func normalizeDateOnly(s string) string {
	if s == "" {
		return ""
	}

	t, err := parseAnyDate(s)
	if err != nil {
		return s
	}

	return t.Format("2006-01-02")
}

// --- HTTP handler ---

type UploadResult struct {
	Title   string   `json:"title"`
	Authors []string `json:"authors"`
	Formats []string `json:"formats"`
	UUID    string   `json:"uuid"`
	Error   string   `json:"error,omitempty"`
	// Files are the uploaded files' names, to send them again with override.
	Files []string `json:"files"`
	// Duplicate: not added, as it looks like a book already in the library
	// (duplicates.go). Sending it again with override=1 adds it, unless
	// Duplicate says the library holds one of its files byte for byte.
	Duplicate *duplicateCheck `json:"duplicate,omitempty"`
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
	override := r.FormValue("override") == "1"

	var results []UploadResult
	for _, group := range groups {
		results = append(results, processUploadGroup(group, override))
	}

	generatePage1()
	refreshAllCaches()

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

// readBookMeta reads the metadata a book file holds. p is where the file is
// now (it may be a temporary file) and name what it is called. Fields the
// file doesn't hold stay empty, so that mergeMeta prefers another format's
// real metadata; mergeMeta fills in the filename and "Unknown" last.
func readBookMeta(p, name string) *UploadedBookMeta {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	meta := &UploadedBookMeta{Format: ext, FileName: name}
	if f, err := os.Open(p); err != nil {
		log.Printf("book metadata: %s: %v", name, err)
	} else {
		switch ext {
		case "epub":
			if st, err := f.Stat(); err != nil {
				log.Printf("book metadata: %s: %v", name, err)
			} else if m, err := extractEPUBMeta(f, st.Size(), name); err != nil {
				log.Printf("book metadata: epub meta extraction failed for %s: %v", name, err)
			} else {
				meta = m
			}
		case "pdf":
			meta.OPFMetadata = extractPDFMeta(f, name).Metadata
		}
		f.Close()
	}
	// One creator can hold "A & B"; other lists are left for the editor's Split.
	meta.Creators = splitCreators(meta.Creators)
	return meta
}

func processUploadGroup(files []*multipart.FileHeader, override bool) UploadResult { // TODO: How does this determine pairs, does this actually work at all?
	var metas []*UploadedBookMeta
	var entries []formatEntry
	names := make([]string, 0, len(files))
	for _, fh := range files {
		names = append(names, fh.Filename)
	}

	for _, fh := range files {
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fh.Filename), "."))
		tmpPath, size, err := saveUploadTemp(fh, ext)
		if err != nil {
			log.Println("upload:", fh.Filename, err)
			continue
		}
		metas = append(metas, readBookMeta(tmpPath, fh.Filename))
		entries = append(entries, formatEntry{
			Ext:      ext,
			Filename: fh.Filename,
			Size:     size,
			TempPath: tmpPath,
		})
	}

	merged := mergeMeta(metas)
	formats := make([]string, 0, len(entries))
	dupFiles := make([]dupFile, 0, len(entries))
	for _, e := range entries {
		formats = append(formats, e.Ext)
		dupFiles = append(dupFiles, dupFile{e.TempPath, e.Ext})
	}
	result := UploadResult{Title: merged.Title, Authors: merged.Creators, Formats: formats, Files: names}

	// Already in the library? Then not added, unless the admin says so.
	check, err := findDuplicates(calibreDB, baseDir, merged.Title, merged.Creators, dupFiles, "")
	if err != nil {
		cleanupTemps(entries)
		result.Error = "couldn't check for duplicates: " + err.Error()
		return result
	}
	if check.blocks(override) {
		cleanupTemps(entries)
		result.Duplicate = check
		return result
	}

	bookUUID, _, err := addBookToLibrary(merged, entries)
	if err != nil {
		cleanupTemps(entries)
		result.Error = err.Error()
		return result
	}

	// Store all the metadatas for each format so we can retrieve them later to present to the user to select
	bookMetadataCache.Store(bookUUID, metas)

	result.UUID = bookUUID
	return result
}

// saveUploadTemp writes an uploaded file to a temporary file.
func saveUploadTemp(fh *multipart.FileHeader, ext string) (path string, size int64, err error) {
	f, err := fh.Open()
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	tmp, err := os.CreateTemp("", "calibrefast-upload-*."+ext)
	if err != nil {
		return "", 0, err
	}
	size, err = io.Copy(tmp, f)
	if cErr := tmp.Close(); err == nil {
		err = cErr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", 0, err
	}
	return tmp.Name(), size, nil
}

// addBookToLibrary adds a book to metadata.db, writes its metadata.opf and
// moves its files (each entry's TempPath) into the library; the entries then
// name the files as they are there. If any step fails, nothing of the book is
// left in the library and its files are back where they were.
func addBookToLibrary(meta *UploadedBookMeta, entries []formatEntry) (bookUUID, bookPath string, err error) {
	if len(entries) == 0 {
		return "", "", errors.New("no book files")
	}
	bookID, bookUUID, bookPath, err := insertBookIntoCalibreDB(calibreDB, meta, entries)
	if err != nil {
		return "", "", fmt.Errorf("failed to insert into database: %w", err)
	}

	destDir := filepath.Join(baseDir, bookPath) // NOTE: bookPath is sanitized in insertBookIntoCalibreDB
	opfPath := filepath.Join(destDir, "metadata.opf")
	type move struct{ from, to string }
	var moved []move
	undo := func() {
		for _, m := range moved {
			logErr(moveFile(m.to, m.from), "couldn't move "+m.to+" back")
		}
		os.Remove(opfPath)
		os.Remove(destDir) // only if nothing else is in it
		logErr(deleteBookFromCalibreDB(calibreDB, bookID), "couldn't delete book "+strconv.FormatInt(bookID, 10)+" from calibreDB")
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		undo()
		return "", "", fmt.Errorf("failed to create book directory: %w", err)
	}

	bookOPF := &OPF{
		Metadata: OPFMetadata{ // TODO: Can we get things to a state where we can just pass in `merged` directly?
			Title:       meta.Title,
			Description: meta.Description,
			Publisher:   meta.Publisher,
			Date:        meta.Date,
			Languages:   meta.Languages,
			Identifiers: meta.Identifiers,
			Subjects:    meta.Subjects,
			Creators:    meta.Creators,
			Meta:        meta.Meta,
		},
	}
	if err := writeOPF(bookOPF, opfPath, bookUUID); err != nil {
		undo()
		return "", "", fmt.Errorf("failed to write metadata.opf: %w", err)
	}

	// Each file is named as its data.name row says (insertBookIntoCalibreDB),
	// or Calibre can't find it.
	baseName := bookFileStem(meta.Title)
	for i := range entries {
		dest := filepath.Join(destDir, baseName+"."+entries[i].Ext)
		if _, err := os.Stat(dest); err == nil {
			// Avoid overwriting an existing file; data.name follows the new name.
			stem := baseName + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
			if _, err := calibreDB.Exec(`UPDATE data SET name = ? WHERE book = ? AND format = ?`, stem, bookID, strings.ToUpper(entries[i].Ext)); err != nil {
				undo()
				return "", "", fmt.Errorf("failed to rename %s in the database: %w", entries[i].Filename, err)
			}
			dest = filepath.Join(destDir, stem+"."+entries[i].Ext)
		}
		if err := moveFile(entries[i].TempPath, dest); err != nil {
			undo()
			return "", "", fmt.Errorf("failed to move %s into the library: %w", entries[i].Filename, err)
		}
		moved = append(moved, move{entries[i].TempPath, dest})
	}
	for i, m := range moved {
		entries[i].Filename = filepath.Base(m.to)
		entries[i].TempPath = ""
	}

	uuidPathIndex.Store(bookUUID, bookPath)
	formatCache.Delete(bookUUID)
	return bookUUID, bookPath, nil
}

// moveFile renames src to dst, copying when they are on different
// filesystems. If it fails, src is still there and dst is not.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		os.Remove(dst)
		return err
	}
	if err := os.Remove(src); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
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
	_, err = io.Copy(out, in)
	if cErr := out.Close(); err == nil {
		err = cErr
	}
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
// generatePage1 rewrites pages/page1.json with the booksPerPage most recently added books.
func generatePage1() {
	pagesMu.Lock()
	defer pagesMu.Unlock()
	rows, err := calibreDB.Query(`
		SELECT b.title, `+bookAuthorsSQL+`, b.series_index, b.pubdate, b.path, b.uuid, b.last_modified
		FROM books b
		ORDER BY b.timestamp DESC
		LIMIT ?`, booksPerPage)
	if err != nil {
		log.Println("generatePage1: query failed:", err)
		return
	}
	defer rows.Close()

	books := make([]Book, 0, booksPerPage)
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.Title, &b.Authors, &b.SeriesIndex, &b.PubDate, &b.Path, &b.UUID, &b.LastModified); err != nil {
			continue
		}
		uuidPathIndex.Store(b.UUID, b.Path)
		notePage(b.UUID, 1)
		books = append(books, b)
	}

	if err := writePage(1, books); err != nil {
		log.Println("generatePage1: write failed:", err)
		return
	}
	pagesVersion.Store(time.Now().UnixNano())
}
