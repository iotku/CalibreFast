package main

// The bulk metadata editor (/library/edit, web/library-edit.ts): every book
// as a row of a table the browser filters, sorts and searches, so duplicates
// and faulty tags or languages can be found, then edited a book or a
// selection at a time.
//
// GET /api/library/table answers the rows, from metadata.db. POST
// /api/library/bulk changes some fields of a set of books: each book's
// metadata.opf is read (as the metadata editor does), the changes are made
// to it, and it's saved with updateBookInCalibreDB, the editor's own write.
// A book the change makes no difference to isn't written.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// bulkMaxBooks bounds one bulk request.
const bulkMaxBooks = 50000

// bulkMu: one bulk edit or merge at a time.
var bulkMu sync.Mutex

func registerBulkEditRoutes() {
	http.HandleFunc("GET /library/edit", requireWrites(bulkEditPage))
	http.HandleFunc("GET /api/library/table", requireWrites(libraryTableHandler))
	http.HandleFunc("POST /api/library/bulk", requireWrites(bulkEditHandler))
}

func bulkEditPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "library-edit.html", PageData{Title: SiteTitle}), "render library-edit")
}

// tableBook is one row of the bulk editor (TS: TableBook in web/types.ts).
type tableBook struct {
	ID          int64    `json:"id"`
	UUID        string   `json:"uuid"`
	Title       string   `json:"title"`
	Authors     string   `json:"authors"` // "A & B"
	Series      string   `json:"series"`
	SeriesIndex float64  `json:"series_index"`
	Publisher   string   `json:"publisher"`
	PubDate     string   `json:"pubdate"` // YYYY-MM-DD, "" for none
	Added       string   `json:"added"`   // YYYY-MM-DD
	Tags        []string `json:"tags"`
	Languages   []string `json:"languages"`
	Formats     []string `json:"formats"` // lower case
	// FormatsFromFolder: metadata.db lists no files for the book, so Formats
	// are the book files in its folder, as the book page and downloads find
	// them (resolveFormats). Calibre doesn't list these.
	FormatsFromFolder bool   `json:"formats_from_folder,omitempty"`
	ISBN              string `json:"isbn"`
	HasCover          bool   `json:"has_cover"`
	path              string
}

// tableSep separates the values group_concat joins: no tag, language or
// format holds it.
const tableSep = "\x1f"

const tableBooksSQL = `SELECT b.id, b.uuid, b.title, ` + bookAuthorsSQL + `, b.series_index,
	COALESCE((SELECT s.name FROM books_series_link x JOIN series s ON s.id = x.series WHERE x.book = b.id LIMIT 1), ''),
	COALESCE((SELECT p.name FROM books_publishers_link x JOIN publishers p ON p.id = x.publisher WHERE x.book = b.id LIMIT 1), ''),
	COALESCE(b.pubdate, ''), COALESCE(b.timestamp, ''),
	COALESCE((SELECT group_concat(t.name, char(31)) FROM books_tags_link x JOIN tags t ON t.id = x.tag WHERE x.book = b.id), ''),
	COALESCE((SELECT group_concat(l.lang_code, char(31) ORDER BY x.item_order) FROM books_languages_link x JOIN languages l ON l.id = x.lang_code WHERE x.book = b.id), ''),
	COALESCE((SELECT group_concat(lower(d.format), char(31) ORDER BY d.format) FROM data d WHERE d.book = b.id), ''),
	COALESCE((SELECT i.val FROM identifiers i WHERE i.book = b.id AND lower(i.type) = 'isbn' LIMIT 1), ''),
	b.has_cover, b.path
	FROM books b`

func splitConcat(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, tableSep)
}

// tableDate is a Calibre date's day, "" for Calibre's "no date" (year 101).
func tableDate(s string) string {
	if len(s) < 10 || strings.HasPrefix(s, "0101-01-01") {
		return ""
	}
	return s[:10]
}

// tableBooks reads the rows: every book, or those with these uuids.
func tableBooks(db *sql.DB, uuids []string) ([]tableBook, error) {
	q := tableBooksSQL
	var args []any
	if uuids != nil {
		if len(uuids) == 0 {
			return []tableBook{}, nil
		}
		q += ` WHERE b.uuid IN (` + placeholders(len(uuids)) + `)`
		args = toArgs(uuids)
	}
	rows, err := db.Query(q+` ORDER BY b.sort COLLATE NOCASE, b.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []tableBook{}
	for rows.Next() {
		var b tableBook
		var pub, added, tags, langs, formats string
		if err := rows.Scan(&b.ID, &b.UUID, &b.Title, &b.Authors, &b.SeriesIndex, &b.Series, &b.Publisher,
			&pub, &added, &tags, &langs, &formats, &b.ISBN, &b.HasCover, &b.path); err != nil {
			return nil, err
		}
		b.PubDate, b.Added = tableDate(pub), tableDate(added)
		b.Tags, b.Languages, b.Formats = splitConcat(tags), splitConcat(langs), splitConcat(formats)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if b := &out[i]; len(b.Formats) == 0 {
			b.Formats = folderFormats(b.UUID, b.path)
			b.FormatsFromFolder = len(b.Formats) > 0
		}
	}
	return out, nil
}

// folderFormats are the formats of the book files in a book's folder.
func folderFormats(uuid, path string) []string {
	if _, err := libraryBookDir(path); err != nil {
		return []string{}
	}
	out := []string{}
	for _, f := range extractFormats(resolveFormats(uuid, baseDir, path)) {
		if f = strings.ToLower(f); !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out
}

func libraryTableHandler(w http.ResponseWriter, r *http.Request) {
	books, err := tableBooks(calibreDB, nil)
	if err != nil {
		log.Println("library table:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	notDups, err := notDuplicateSets()
	if err != nil {
		log.Println("library table: not duplicates:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	logErr(json.NewEncoder(w).Encode(map[string]any{"books": books, "not_duplicates": notDups}), "encode library table")
}

// bulkEdit is what to change in each book (TS: BulkEdit in web/types.ts).
// A nil field is left alone; "Set" fields replace the book's value.
type bulkEdit struct {
	Title     *string `json:"title,omitempty"`
	Authors   *string `json:"authors,omitempty"` // "A & B"
	Publisher *string `json:"publisher,omitempty"`
	Date      *string `json:"date,omitempty"` // YYYY-MM-DD, "" for none

	Tags       *string  `json:"tags,omitempty"` // "a, b": the book's tags become these
	AddTags    []string `json:"add_tags,omitempty"`
	RemoveTags []string `json:"remove_tags,omitempty"` // any case

	Languages       *string  `json:"languages,omitempty"` // "eng, fra"
	AddLanguages    []string `json:"add_languages,omitempty"`
	RemoveLanguages []string `json:"remove_languages,omitempty"` // "en" removes "eng" too
}

func (e *bulkEdit) empty() bool {
	return e.Title == nil && e.Authors == nil && e.Publisher == nil && e.Date == nil &&
		e.Tags == nil && len(e.AddTags) == 0 && len(e.RemoveTags) == 0 &&
		e.Languages == nil && len(e.AddLanguages) == 0 && len(e.RemoveLanguages) == 0
}

// apply makes the changes to m, and says whether anything changed.
func (e *bulkEdit) apply(m *OPFMetadata) bool {
	before, _ := json.Marshal(m)
	if e.Title != nil && strings.TrimSpace(*e.Title) != "" {
		m.Title = strings.TrimSpace(*e.Title)
	}
	if e.Authors != nil {
		if a := splitAuthors(*e.Authors); len(a) > 0 {
			m.Creators = a
		}
	}
	if e.Publisher != nil {
		m.Publisher = strings.TrimSpace(*e.Publisher)
	}
	if e.Date != nil {
		if d := strings.TrimSpace(*e.Date); d == "" {
			m.Date = ""
		} else {
			m.Date = normalizeDate(d)
		}
	}

	if e.Tags != nil {
		m.Subjects = parseSubjectsString(*e.Tags)
	}
	if len(e.RemoveTags) > 0 {
		gone := map[string]bool{}
		for _, t := range e.RemoveTags {
			gone[strings.ToLower(strings.TrimSpace(t))] = true
		}
		m.Subjects = slices.DeleteFunc(slices.Clone(m.Subjects), func(t string) bool { return gone[strings.ToLower(strings.TrimSpace(t))] })
	}
	if len(e.AddTags) > 0 {
		m.Subjects = append(slices.Clone(m.Subjects), e.AddTags...)
	}
	m.Subjects = uniqueTags(m.Subjects)

	if e.Languages != nil {
		m.Languages = parseLanguagesString(*e.Languages)
	}
	if len(e.RemoveLanguages) > 0 {
		gone := map[string]bool{}
		for _, l := range e.RemoveLanguages {
			gone[canonicalLanguage(l)] = true
			gone[strings.ToLower(strings.TrimSpace(l))] = true
		}
		m.Languages = slices.DeleteFunc(slices.Clone(m.Languages), func(l string) bool {
			return gone[strings.ToLower(strings.TrimSpace(l))] || gone[canonicalLanguage(l)]
		})
	}
	if len(e.AddLanguages) > 0 {
		have := map[string]bool{}
		for _, l := range m.Languages {
			have[canonicalLanguage(l)] = true
		}
		langs := slices.Clone(m.Languages)
		for _, l := range cleanLanguages(e.AddLanguages) {
			if !have[canonicalLanguage(l)] {
				have[canonicalLanguage(l)] = true
				langs = append(langs, l)
			}
		}
		m.Languages = langs
	}
	langs := cleanLanguages(m.Languages)
	if len(langs) == 0 && len(m.Languages) > 0 {
		langs = []string{undefinedLanguage} // "und" stays "und"
	}
	m.Languages = langs

	after, _ := json.Marshal(m)
	return string(before) != string(after)
}

// bookEditMetadata is a book's metadata as the editor starts from it: its
// metadata.opf, with the date as metadata.db stores dates.
func bookEditMetadata(uuid string) (*OPFMetadata, error) {
	path, err := resolveBookPath(uuid)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errNoSuchBook, uuid)
	}
	opf, err := loadOPF(filepath.Join(baseDir, path, "metadata.opf"))
	if err != nil {
		return nil, fmt.Errorf("couldn't read metadata.opf: %w", err)
	}
	m := opf.Metadata
	if strings.TrimSpace(m.Date) != "" {
		m.Date = normalizeDate(m.Date)
	}
	return &m, nil
}

// bulkFailure is a book a bulk edit couldn't change.
type bulkFailure struct {
	UUID  string `json:"uuid"`
	Error string `json:"error"`
}

type bulkResult struct {
	Changed   int           `json:"changed"`   // books written
	Unchanged int           `json:"unchanged"` // books the edit made no difference to
	Failed    []bulkFailure `json:"failed"`
	Books     []tableBook   `json:"books"` // the changed books' rows, as they are now
}

// bulkEditBooks makes the edit to each book.
func bulkEditBooks(uuids []string, edit *bulkEdit) bulkResult {
	bulkMu.Lock()
	defer bulkMu.Unlock()
	res := bulkResult{Failed: []bulkFailure{}, Books: []tableBook{}}
	var changed []string
	for _, u := range uuids {
		m, err := bookEditMetadata(u)
		if err != nil {
			res.Failed = append(res.Failed, bulkFailure{u, err.Error()})
			continue
		}
		if !edit.apply(m) {
			res.Unchanged++
			continue
		}
		if _, err := updateBookInCalibreDB(calibreDB, baseDir, u, m); err != nil {
			log.Printf("bulk edit %s: %v", u, err)
			res.Failed = append(res.Failed, bulkFailure{u, err.Error()})
			continue
		}
		bookMetadataCache.Delete(u)
		changed = append(changed, u)
	}
	res.Changed = len(changed)
	if len(changed) > 0 {
		logErr(refreshInPages(changed), "bulk edit: couldn't update the library pages")
		generatePage1()
		refreshAllCaches()
		for chunk := range slices.Chunk(changed, 500) {
			rows, err := tableBooks(calibreDB, chunk)
			if err != nil {
				log.Println("bulk edit: reading rows back:", err)
				break
			}
			res.Books = append(res.Books, rows...)
		}
	}
	return res
}

type bulkEditRequest struct {
	UUIDs []string `json:"uuids"`
	Edit  bulkEdit `json:"edit"`
}

func bulkEditHandler(w http.ResponseWriter, r *http.Request) {
	var req bulkEditRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.UUIDs) == 0 {
		http.Error(w, "no books named", http.StatusBadRequest)
		return
	}
	if len(req.UUIDs) > bulkMaxBooks {
		http.Error(w, fmt.Sprintf("at most %d books at a time", bulkMaxBooks), http.StatusBadRequest)
		return
	}
	for _, u := range req.UUIDs {
		if !isSafeUUIDComponent(u) {
			http.Error(w, "bad book id", http.StatusBadRequest)
			return
		}
	}
	if req.Edit.empty() {
		http.Error(w, "nothing to change", http.StatusBadRequest)
		return
	}
	res := bulkEditBooks(uniqueStrings(req.UUIDs), &req.Edit)
	if res.Changed > 0 {
		log.Printf("bulk edit: %s changed %d book(s) (%d unchanged, %d failed)", currentUser(r).Username, res.Changed, res.Unchanged, len(res.Failed))
	}
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(res), "encode bulk edit")
}

// uniqueStrings drops repeats, keeping the first of each in order.
func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
