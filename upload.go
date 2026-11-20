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
	"strings"
	"sync"
	"time"

	"github.com/iotku/CalibreFast/pkg/epubzip"
	"github.com/iotku/CalibreFast/pkg/opf"
	"github.com/iotku/CalibreFast/pkg/xml11"
	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
)

var bookMetadataCache sync.Map // Key: SessionID (string), Value: []UploadedBookMeta

// UploadedBookMeta holds all metadata we can extract from an ebook file.
type UploadedBookMeta struct {
	OPFMetadata
	Format   string // "epub" | "pdf" | ...
	FileName string // original filename
}

// --- EPUB metadata extraction ---
func extractEPUBMeta(r io.ReaderAt, size int64, filename string) (*UploadedBookMeta, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("not a valid zip/epub: %w", err)
	}

	opfPath, err := epubzip.PackagePath(zr)
	if err != nil {
		return nil, err
	}
	opfData, err := epubzip.ReadEntry(zr, opfPath, 16<<20)
	if err != nil {
		return nil, fmt.Errorf("could not read OPF file: %w", err)
	}

	var pkg OPF
	if err := xml11.Unmarshal(opfData, &pkg); err != nil {
		return nil, fmt.Errorf("failed to parse OPF: %w", err)
	}

	m := pkg.Metadata
	m.FillSeries()
	meta := &UploadedBookMeta{OPFMetadata: m, Format: "epub", FileName: filename}

	return meta, nil
}

// --- PDF metadata extraction via pdfcpu ---
func extractPDFMeta(r io.ReadSeeker, filename string) *OPF {
	opfDoc := &OPF{
		Metadata: OPFMetadata{},
	}
	meta := &opfDoc.Metadata
	meta.Languages = []string{opf.UndefinedLanguage} // undetermined; caller can override

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
	return opfDoc
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
		if len(opf.CleanLanguages(m.Languages)) > len(opf.CleanLanguages(merged.Languages)) {
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
	if len(opf.CleanLanguages(merged.Languages)) == 0 {
		merged.Languages = []string{opf.UndefinedLanguage}
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
	// Stem: the file's data.name in metadata.db, when it isn't the book's
	// usual bookFileStem (addBookToLibrary).
	Stem string
}

// upsertAuthor finds or adds the author named name (as shown: "Battey,
// Nick"). It's stored as Calibre stores it ("Battey| Nick": authorDBName);
// a row an older CalibreFast stored with the comma is found too.
func upsertAuthor(tx *sql.Tx, name string) (int64, error) {
	var id int64
	stored := authorDBName(name)
	err := tx.QueryRow(`SELECT id FROM authors WHERE name = ? OR name = ? ORDER BY name = ? DESC, id LIMIT 1`, stored, name, stored).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := tx.Exec(`INSERT INTO authors (name, sort) VALUES (?, ?)`, stored, authorSortKey(name))
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

// authorSortKey is a name's sort form: "Alan Donovan" → "Donovan, Alan". A
// name already written "Last, First" is its own.
func authorSortKey(name string) string {
	if strings.Contains(name, ",") {
		return strings.Join(strings.Fields(name), " ")
	}
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
	if s == "" {
		return "0101-01-01 00:00:00+00:00"
	}
	t, err := opf.ParseAnyDate(s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2006-01-02 15:04:05-07:00")
}

func normalizeDateOnly(s string) string {
	if s == "" {
		return ""
	}

	t, err := opf.ParseAnyDate(s)
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
	// SeriesFromPlaceholder: "Series #n", when the files had no series and
	// the placeholder for the book gave it.
	SeriesFromPlaceholder string `json:"seriesFromPlaceholder,omitempty"`
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
	for _, res := range results {
		if res.UUID != "" && res.Error == "" && res.Duplicate == nil {
			catalogChanged(res.UUID)
			logBookEdit(currentUser(r), "upload", res.UUID, nil)
		}
	}

	refreshLibraryPages()
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
	for _, e := range entries {
		formats = append(formats, e.Ext)
	}
	result := UploadResult{Title: merged.Title, Authors: merged.Creators, Formats: formats, Files: names}
	// The placeholder this book stands for, if any: before it's added,
	// which deletes it. With no series in the files, its says which.
	ph := fillFromPlaceholder(&merged.OPFMetadata)
	if ph != nil {
		log.Printf("upload: %q: series %s #%s from its placeholder", merged.Title, merged.Series, merged.SeriesIndex)
		result.SeriesFromPlaceholder = merged.Series + " #" + merged.SeriesIndex
	} else {
		ph = matchingPlaceholder(&merged.OPFMetadata)
	}
	dupFiles := make([]dupFile, 0, len(entries))
	for _, e := range entries {
		dupFiles = append(dupFiles, dupFile{e.TempPath, e.Ext})
	}

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

	// Store all the metadatas for each format so we can retrieve them later to present to the user to select,
	// and the placeholder's, now deleted.
	if ph != nil {
		metas = append(metas, placeholderSource(ph))
	}
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

// addBookToLibrary is in add_journal.go.

// moveFile renames src to dst, copying when they are on different
// filesystems. If it fails, src is still there and dst is not. A copy is
// written beside dst under a ".partial" name and renamed into place once
// whole, so even if the process dies part way through, dst is either
// absent or complete (and then src may be too, until it's removed).
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	tmp := dst + partialSuffix
	if err := copyFileSync(src, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Remove(src); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// partialSuffix marks a copy moveFile hasn't finished.
const partialSuffix = ".partial"

func copyFile(src, dst string) error { return copyFileWith(src, dst, false) }

// copyFileSync is copyFile, flushed to disk before it returns.
func copyFileSync(src, dst string) error { return copyFileWith(src, dst, true) }

func copyFileWith(src, dst string, sync bool) error {
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
	if err == nil && sync {
		err = out.Sync()
	}
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

// refreshLibraryPages brings pages/*.json up to date after books were added
// or edited: the catalog's queued changes are applied, then the pages are
// written from it in Latest order (only the files that change). So a new
// book shifts the others along instead of pushing one off page 1, and the
// grid's count (totalBooks) includes it.
func refreshLibraryPages() {
	if libraryCatalog.Load() == nil {
		writePage1FromDB() // no catalog (tests that don't build one)
		return
	}
	logErr(catalogFlush(), "library pages: couldn't apply the catalog's changes")
	pagesMu.Lock()
	defer pagesMu.Unlock()
	_, err := writePagesFromCatalog(currentCatalog())
	logErr(err, "library pages: couldn't write")
}

// writePage1FromDB rewrites pages/page1.json with the booksPerPage most
// recently added books, from metadata.db.
func writePage1FromDB() {
	pagesMu.Lock()
	defer pagesMu.Unlock()
	rows, err := calibreDB.Query(`
		SELECT b.title, `+bookAuthorsSQL+`, b.series_index, b.pubdate, b.path, b.uuid, b.last_modified
		FROM books b
		ORDER BY b.timestamp DESC
		LIMIT ?`, booksPerPage)
	if err != nil {
		log.Println("library pages: query failed:", err)
		return
	}
	defer rows.Close()

	books := make([]Book, 0, booksPerPage)
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.Title, &b.Authors, &b.SeriesIndex, &b.PubDate, &b.Path, &b.UUID, &b.LastModified); err != nil {
			continue
		}
		b.LastModified = coverStamp(b.LastModified)
		uuidPathIndex.Store(b.UUID, b.Path)
		notePage(b.UUID, 1)
		books = append(books, b)
	}

	if err := writePage(1, books); err != nil {
		log.Println("library pages: write failed:", err)
		return
	}
	pagesVersion.Store(time.Now().UnixNano())
}
