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

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
)

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

	var pkg OPF
	if err := xml.Unmarshal(opfData, &pkg); err != nil {
		return nil, fmt.Errorf("failed to parse OPF: %w", err)
	}

	m := pkg.Metadata
	meta := &UploadedBookMeta{OPFMetadata: m, Format: "epub", FileName: filename}

	return meta, nil
}

// --- PDF metadata extraction via pdfcpu ---
func extractPDFMeta(data []byte, filename string) *OPF {
	opf := &OPF{
		Metadata: OPFMetadata{},
	}
	meta := &opf.Metadata
	meta.Language = "und" // undetermined; caller can override

	info, err := pdfapi.PDFInfo(bytes.NewReader(data), filename, nil, false, nil)
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
		if len(m.Language) > len(merged.Language) {
			merged.Language = m.Language
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

	if merged.Title == "" {
		merged.Title = "Unknown"
	}
	if len(merged.Creators) == 0 {
		merged.Creators = []string{"Unknown"}
	}
	if merged.Language == "" {
		merged.Language = "und"
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

func processUploadGroup(files []*multipart.FileHeader) UploadResult { // TODO: How does this determine pairs, does this actually work at all?
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
					OPFMetadata: OPFMetadata{
						Title:    strings.TrimSuffix(fh.Filename, filepath.Ext(fh.Filename)),
						Creators: []string{"Unknown"},
					},
					Format:   "epub",
					FileName: fh.Filename,
				}
			}
		case "pdf":
			meta = &UploadedBookMeta{ // TODO check for extractPDFMeta failure
				OPFMetadata: extractPDFMeta(data, fh.Filename).Metadata,
				Format:      "pdf",
				FileName:    fh.Filename,
			}
		default:
			meta = &UploadedBookMeta{
				OPFMetadata: OPFMetadata{
					Title:    strings.TrimSuffix(fh.Filename, filepath.Ext(fh.Filename)),
					Creators: []string{"Unknown"},
				},
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

	// Try to insert into database first
	bookID, bookUUID, bookPath, err := insertBookIntoCalibreDB(calibreDB, merged, entries)
	if err != nil {
		return UploadResult{Error: "failed to insert into database: " + err.Error()}
	}

	destDir := filepath.Join(baseDir, bookPath) // NOTE: bookPath is santisized in insertBookIntoCalibre DB
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		logErr(deleteBookFromCalibreDB(calibreDB, bookID), "couldn't delete book "+strconv.FormatInt(bookID, 10)+"from calibreDB")

		cleanupTemps(entries)
		return UploadResult{Error: "failed to create book directory: " + err.Error()}
	}

	// Write metadata.opf
	opfPath := filepath.Join(destDir, "metadata.opf")
	var bookOPF = &OPF{
		Metadata: OPFMetadata{ // TODO: Can we get things to a state where we can just pass in `merged` directly?
			Title:       merged.Title,
			Description: merged.Description, // TODO: merged.Description
			Publisher:   merged.Publisher,
			Date:        merged.Date,
			Language:    merged.Language,
			Identifiers: merged.Identifiers,
			Subjects:    merged.Subjects,
			Creators:    merged.Creators,
			Meta:        merged.Meta,
			// {Name: "calibre:series", Content: "Addison-Wesley Professional Computing Series"},
			// {Name: "calibre:series_index", Content: "1"},
			// {Name: "calibre:rating", Content: "10"},
			// {Name: "calibre:timestamp", Content: "2015-10-26T00:00:00+00:00"},
		},
	}
	if err := writeOPF(bookOPF, opfPath); err != nil {
		logErr(deleteBookFromCalibreDB(calibreDB, bookID), "couldn't delete book "+strconv.FormatInt(bookID, 10)+" from calibreDB")
		cleanupTemps(entries)
		return UploadResult{Error: "failed to write metadata.opf: " + err.Error()}
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

	if bookUUID != "" {
		coverIndex.Store(bookUUID, bookPath)
	}

	formats := make([]string, 0, len(entries))
	for _, e := range entries {
		formats = append(formats, e.Ext)
	}

	return UploadResult{
		Title:   merged.Title,
		Authors: merged.Creators,
		Formats: formats,
		UUID:    bookUUID,
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
	rows, err := calibreDB.Query(`
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
