package main

// Merging books: several library books that are one book (an EPUB added
// once and its PDF another time, say) become one, with every format, the
// metadata picked field by field in the metadata editor, and one cover.
//
// /book/merge?ids=a,b,c&keep=a is the metadata editor (book-edit.html with
// EditBookPageData.Merge). Its sources are each book's metadata (the kept
// one's is "current") and what each of their files says. POST /merge/apply
// does the merge (mergeBooks):
//
//  1. The files taken from the other books (a format the kept book lacks,
//     or one chosen over its own) and the chosen cover are copied into the
//     kept book's folder under temporary names.
//  2. The other books go to the Recycling Bin (recycleBooks), whole: they
//     can be restored as they were. The batch notes the merge.
//  3. The copies get their real names and data rows. A file of the kept
//     book's that one of them replaces goes to the batch as a file of its
//     own (recycle_formats.go), so nothing is deleted and it can be put back.
//  4. Readers' data about the other books (favorites, reading status,
//     places, bookmarks, highlights, shelves) is copied to the kept book;
//     the originals stay with the old uuids for a restore. Bookmarks and
//     highlights are tied to the file they were made in, so they follow only
//     where a book's file in that format is byte for byte the one kept
//     (mergeReaderData); places always do.
//  5. The metadata is saved as the editor saves it (updateBookInCalibreDB).
//
// Adding a new file to a book (add_format.go) is the same merge with one of
// the "books" being the uploaded file, waiting in the import queue.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// mergeMaxBooks bounds one merge.
const mergeMaxBooks = 20

// recycleReplacedDir held, in a merge's batch, the kept book's files that
// the merge replaced, before they became the batch's Formats.
const recycleReplacedDir = "replaced"

// mergeNote is in batch.json for a batch a merge made.
type mergeNote struct {
	UUID  string `json:"uuid"`
	Title string `json:"title"`
	// AddedFiles: the batch is from adding new files to the book
	// (add_format.go), and holds the files they replaced.
	AddedFiles []string `json:"added_files,omitempty"`
	// Replaced: file names under replaced/, in batches from before replaced
	// files were the batch's Formats.
	Replaced []string `json:"replaced,omitempty"`
}

// mergeBook is one of the books being merged.
type mergeBook struct {
	Letter       string // "A", "B", … in the order named
	ID           int64
	UUID         string // for a new file, incomingUUID
	Import       string // a new file: its id in the import queue (add_format.go)
	Title        string
	Authors      string
	Path         string
	Dir          string
	Files        map[string]string // format -> file
	HasCover     bool
	CoverModTime int64
}

// incomingUUID stands for a new file's "book" (add_format.go) in a merge's
// choices.
const incomingUUID = "new"

// Incoming: the new file being added to a book, not a library book.
func (b *mergeBook) Incoming() bool { return b.Import != "" }

// Link is where the page links the book.
func (b *mergeBook) Link() string {
	if b.Incoming() {
		return ""
	}
	return "/book/get/" + b.UUID
}

// CoverURL is the book's cover thumbnail.
func (b *mergeBook) CoverURL() string {
	if b.Incoming() {
		return "/import/" + b.Import + "/cover"
	}
	return fmt.Sprintf("/cover-thumb/%s?t=%d", b.UUID, b.CoverModTime)
}

// DownloadURL is the book's file in format.
func (b *mergeBook) DownloadURL(format string) string {
	if b.Incoming() {
		return "/import/" + b.Import + "/file/" + format
	}
	return "/download/" + b.UUID + "/" + format
}

// mergeFormatView is one format a merge can keep, for the page.
type mergeFormatView struct {
	Ext     string
	Choices []mergeFileView
	// Identical: several books have this format, all byte for byte the same
	// file (SHA-256), so which one is kept makes no difference.
	Identical bool
}

type mergeFileView struct {
	Letter, UUID string
	Size         string
	Download     string // the file, to compare
	// Hash: the start of the file's SHA-256, so the page can say whose
	// bookmarks and highlights follow the file chosen ("": unknown).
	Hash   string
	Chosen bool
	// SameAs: the letter of an earlier book whose file this one is
	// identical to, when not all of them are.
	SameAs string
}

// Different: several books have this format and the files aren't all the
// same, so the admin may want to download them to compare.
func (v mergeFormatView) Different() bool { return len(v.Choices) > 1 && !v.Identical }

// mergePageData is EditBookPageData.Merge.
type mergePageData struct {
	Books   []*mergeBook
	Keep    string
	IDs     string // "a,b,c", as the page was asked for
	Formats []mergeFormatView
	// Incoming: adding new files to the kept book (add_format.go), the id
	// of the waiting upload. Then the other "book" is the new files.
	Incoming string
	// IncomingFiles: the new files' names.
	IncomingFiles []string
}

func (d *mergePageData) KeepBook() *mergeBook {
	for _, b := range d.Books {
		if b.UUID == d.Keep {
			return b
		}
	}
	return nil
}

func letterOf(i int) string {
	if i < 26 {
		return string(rune('A' + i))
	}
	return fmt.Sprintf("%d", i+1)
}

// loadMergeBooks reads the books, in the order given.
func loadMergeBooks(uuids []string) ([]*mergeBook, error) {
	uuids = uniqueStrings(uuids)
	if len(uuids) < 2 {
		return nil, errors.New("name at least two books to merge")
	}
	if len(uuids) > mergeMaxBooks {
		return nil, fmt.Errorf("at most %d books at a time", mergeMaxBooks)
	}
	var out []*mergeBook
	for i, u := range uuids {
		b, err := loadMergeBook(u, letterOf(i))
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// loadMergeBook reads one library book.
func loadMergeBook(u, letter string) (*mergeBook, error) {
	if !isSafeUUIDComponent(u) {
		return nil, errors.New("bad book id")
	}
	// The uuid as metadata.db has it, not the request's: paths are built from it.
	b := &mergeBook{Letter: letter, Files: map[string]string{}}
	if err := calibreDB.QueryRow(`SELECT b.id, b.uuid, b.title, `+bookAuthorsSQL+`, b.path FROM books b WHERE b.uuid = ?`, u).
		Scan(&b.ID, &b.UUID, &b.Title, &b.Authors, &b.Path); err != nil {
		return nil, fmt.Errorf("%w: %s", errNoSuchBook, u)
	}
	dir, err := libraryBookDir(b.Path)
	if err != nil {
		return nil, err
	}
	b.Dir = dir
	files, err := bookFiles(calibreDB, baseDir, b.ID, b.Path)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if _, ok := b.Files[f.Format]; !ok {
			b.Files[f.Format] = f.Path
		}
	}
	if len(b.Files) == 0 {
		// metadata.db lists no files: the book files in its folder, as
		// the rest of CalibreFast finds them (resolveFormats).
		for _, name := range resolveFormats(b.UUID, baseDir, b.Path) {
			f := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
			if _, ok := b.Files[f]; !ok {
				b.Files[f] = filepath.Join(dir, name)
			}
		}
	}
	if st, err := os.Stat(filepath.Join(dir, "cover.jpg")); err == nil {
		b.HasCover = true
		b.CoverModTime = st.ModTime().UnixNano()
	}
	return b, nil
}

// fileHash is the file's SHA-256 ("" if it can't be read).
func fileHash(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	h, err := fileSHA256(p, st)
	if err != nil {
		log.Printf("merge: hashing %s: %v", p, err)
		return ""
	}
	return h
}

func (b *mergeBook) FormatList() []string {
	out := make([]string, 0, len(b.Files))
	for f := range b.Files {
		out = append(out, f)
	}
	slices.Sort(out)
	return out
}

// mergeFormats says, for each format any of the books has, whose file the
// merged book keeps: chosen[format] if that book has it, else the kept
// book's, else the first book's that has one.
func mergeFormats(books []*mergeBook, keep string, chosen map[string]string) map[string]*mergeBook {
	out := map[string]*mergeBook{}
	for _, b := range books {
		for f := range b.Files {
			if _, ok := out[f]; !ok {
				out[f] = b
			}
		}
	}
	for _, b := range books {
		if b.UUID != keep {
			continue
		}
		for f := range b.Files {
			out[f] = b
		}
	}
	for f, u := range chosen {
		f = strings.ToLower(f)
		for _, b := range books {
			if b.UUID == u {
				if _, ok := b.Files[f]; ok {
					out[f] = b
				}
			}
		}
	}
	return out
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

func newMergePageData(books []*mergeBook, keep, ids string, chosen map[string]string) *mergePageData {
	d := &mergePageData{Books: books, Keep: keep, IDs: ids}
	picked := mergeFormats(books, keep, chosen)
	exts := make([]string, 0, len(picked))
	for f := range picked {
		exts = append(exts, f)
	}
	slices.Sort(exts)
	for _, f := range exts {
		v := mergeFormatView{Ext: f}
		var hashes []string
		for _, b := range books {
			p, ok := b.Files[f]
			if !ok {
				continue
			}
			size, hash := "", ""
			if st, err := os.Stat(p); err == nil {
				size = humanSize(st.Size())
				hash = fileHash(p)
			}
			short := hash
			if len(short) > 16 {
				short = short[:16]
			}
			v.Choices = append(v.Choices, mergeFileView{Letter: b.Letter, UUID: b.UUID, Size: size, Download: b.DownloadURL(f), Hash: short, Chosen: picked[f] == b})
			hashes = append(hashes, hash)
		}
		if len(v.Choices) > 1 {
			v.Identical = hashes[0] != ""
			for i, h := range hashes {
				if h != hashes[0] {
					v.Identical = false
				}
				for j := range i {
					if h != "" && hashes[j] == h {
						v.Choices[i].SameAs = v.Choices[j].Letter
						break
					}
				}
			}
		}
		d.Formats = append(d.Formats, v)
	}
	return d
}

// mergeIDs reads ?ids=a,b,c and ?keep= (the first book by default).
func mergeIDs(r *http.Request) (ids []string, keep string) {
	for _, s := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			ids = append(ids, s)
		}
	}
	ids = uniqueStrings(ids)
	keep = r.URL.Query().Get("keep")
	if !slices.Contains(ids, keep) && len(ids) > 0 {
		keep = ids[0]
	}
	return ids, keep
}

func registerMergeRoutes() {
	http.HandleFunc("GET /book/merge", requireWrites(mergePage))
	http.HandleFunc("GET /merge/metadata", requireWrites(mergeMetadataHandler))
	http.HandleFunc("POST /merge/apply", requireWrites(mergeApplyHandler))
}

func mergePage(w http.ResponseWriter, r *http.Request) {
	ids, keep := mergeIDs(r)
	books, err := loadMergeBooks(ids)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errNoSuchBook) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	d := newMergePageData(books, keep, strings.Join(ids, ","), nil)
	kb := d.KeepBook()
	data := EditBookPageData{
		UUID:         keep,
		CoverModTime: kb.CoverModTime,
		ReadLink:     previewReadLink("uuid", keep, kb.FormatList()),
		Lookups:      !noLookups,
		GoogleBooks:  googleBooksEnabled(),
		Merge:        d,
	}
	w.Header().Set("Cache-Control", "no-store")
	if err := templates.ExecuteTemplate(w, "book-edit.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// mergeSources are the metadata editor's sources for a merge: the kept
// book's metadata as "current", each other book's as its letter ("B"), then
// what each book's files say ("A epub", "B pdf").
func mergeSources(books []*mergeBook, keep string) []UploadedBookMeta {
	var out []UploadedBookMeta
	add := func(m UploadedBookMeta) {
		m.Date = normalizeDateOnly(m.Date)
		_, m.Identifiers = splitLibraryIdentifiers(m.Identifiers)
		out = append(out, m)
	}
	ordered := slices.Clone(books)
	slices.SortStableFunc(ordered, func(a, b *mergeBook) int {
		if a.UUID == keep {
			return -1
		}
		if b.UUID == keep {
			return 1
		}
		return 0
	})
	for _, b := range ordered {
		if b.Incoming() {
			continue // new files have no record: what they say follows
		}
		opf, err := loadOPF(filepath.Join(b.Dir, "metadata.opf"))
		if err != nil {
			log.Printf("merge: %s: %v", b.UUID, err)
			continue
		}
		label := b.Letter
		if b.UUID == keep {
			label = "current"
		}
		// The series as metadata.db has it.
		opf.Metadata.Series, opf.Metadata.SeriesIndex = librarySeriesFields(b.UUID)
		add(UploadedBookMeta{OPFMetadata: opf.Metadata, Format: label})
	}
	for _, b := range books {
		for _, f := range b.FormatList() {
			m := readBookMeta(b.Files[f], filepath.Base(b.Files[f]))
			if m.Title == "" && len(m.Creators) == 0 && m.Description == "" && m.Publisher == "" && len(m.Subjects) == 0 && len(m.Identifiers) == 0 {
				continue
			}
			m.Format = b.Letter + " " + f
			m.FileName = ""
			add(*m)
		}
	}
	return out
}

func mergeMetadataHandler(w http.ResponseWriter, r *http.Request) {
	ids, keep := mergeIDs(r)
	books, err := loadMergeBooks(ids)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	logErr(json.NewEncoder(w).Encode(mergeSources(books, keep)), "encode merge sources")
}

// mergeRequest is POST /merge/apply: the editor's values, and the merge.
type mergeRequest struct {
	ApplyMetadataRequest
	Keep    string            `json:"keep"`
	UUIDs   []string          `json:"uuids"`   // every book, the kept one too
	Formats map[string]string `json:"formats"` // format -> uuid of the book whose file to keep
	Cover   string            `json:"cover"`   // uuid of the book whose cover to keep ("": the kept book's)
}

type mergeResult struct {
	UUID   string `json:"uuid"`
	Title  string `json:"title"`
	Batch  string `json:"batch"`
	Merged int    `json:"merged"` // books merged into it
	// Dropped: readers' bookmarks and highlights not carried over, as they
	// were made in a file other than the one kept.
	Dropped int `json:"dropped"`
}

func mergeTempName(ext string) string {
	var r [6]byte
	_, _ = rand.Read(r[:])
	return ".cf-merge-" + hex.EncodeToString(r[:]) + "." + ext
}

// mergeBooks merges the books into req.Keep (see the top of this file).
func mergeBooks(req *mergeRequest, by string) (*mergeResult, error) {
	bulkMu.Lock()
	defer bulkMu.Unlock()

	books, err := loadMergeBooks(req.UUIDs)
	if err != nil {
		return nil, err
	}
	return mergeInto(books, req, by)
}

// mergeInto merges books into req.Keep, one of them; the caller holds
// bulkMu. Library books other than the kept one go to the Recycling Bin;
// a new file's "book" (Incoming) is left for the caller to clear away.
func mergeInto(books []*mergeBook, req *mergeRequest, by string) (*mergeResult, error) {
	var keep *mergeBook
	var others, recycled []*mergeBook
	for _, b := range books {
		switch {
		case b.UUID == req.Keep && !b.Incoming():
			keep = b
		default:
			others = append(others, b)
			if !b.Incoming() {
				recycled = append(recycled, b)
			}
		}
	}
	if keep == nil {
		return nil, errors.New("the book to keep must be one of the books merged")
	}
	if err := os.MkdirAll(keep.Dir, 0o755); err != nil {
		return nil, err
	}

	// Which file each format keeps, and whose files are that same file.
	picked := mergeFormats(books, keep.UUID, req.Formats)
	formats := make([]string, 0, len(picked))
	for f := range picked {
		formats = append(formats, f)
	}
	slices.Sort(formats)
	keptHash := map[string]string{} // format -> SHA-256 of the file kept
	for _, f := range formats {
		keptHash[f] = fileHash(picked[f].Files[f])
	}
	sameFile := func(b *mergeBook, f string) bool {
		p, ok := b.Files[f]
		if !ok || keptHash[f] == "" {
			return false
		}
		if picked[f] == b {
			return true
		}
		return fileHash(p) == keptHash[f]
	}

	// Whose bookmarks and highlights follow (step 4), while their files are
	// still where they were.
	same := map[string][]string{}
	for _, b := range recycled {
		for _, f := range b.FormatList() {
			if sameFile(b, f) {
				same[b.UUID] = append(same[b.UUID], f)
			}
		}
	}

	// 1. Copy what's taken from the other books in, under temporary names.
	// A file identical to the kept book's own needs no copy.
	type staged struct {
		format, temp string
		from         *mergeBook
	}
	var copies []staged
	var coverTemp string
	cleanup := func() {
		for _, c := range copies {
			os.Remove(c.temp)
		}
		if coverTemp != "" {
			os.Remove(coverTemp)
		}
	}
	replacing := false
	for _, f := range formats {
		from := picked[f]
		if from == keep || sameFile(keep, f) {
			continue
		}
		tmp := filepath.Join(keep.Dir, mergeTempName(f))
		if err := copyFile(from.Files[f], tmp); err != nil {
			os.Remove(tmp)
			cleanup()
			return nil, fmt.Errorf("couldn't copy %s's %s: %w", from.Title, strings.ToUpper(f), err)
		}
		copies = append(copies, staged{f, tmp, from})
		if _, ok := keep.Files[f]; ok {
			replacing = true
		}
	}
	if req.Cover != "" && req.Cover != keep.UUID {
		for _, b := range others {
			if b.UUID == req.Cover && b.HasCover {
				coverTemp = filepath.Join(keep.Dir, mergeTempName("jpg"))
				if err := copyFile(filepath.Join(b.Dir, "cover.jpg"), coverTemp); err != nil {
					os.Remove(coverTemp)
					coverTemp = ""
					cleanup()
					return nil, fmt.Errorf("couldn't copy the cover: %w", err)
				}
			}
		}
	}

	// 2. The other books go to the Recycling Bin, whole. With none, a batch
	// of its own holds the files replaced, if any.
	otherUUIDs := make([]string, len(recycled))
	for i, b := range recycled {
		otherUUIDs[i] = b.UUID
	}
	var batch *recycleBatch
	switch {
	case len(recycled) > 0:
		var err error
		if batch, err = recycleBooks(otherUUIDs, by); err != nil {
			cleanup()
			return nil, err
		}
	case replacing:
		recycleMu.Lock()
		batch = &recycleBatch{DeletedBy: by}
		_, err := batch.create()
		recycleMu.Unlock()
		if err != nil {
			cleanup()
			return nil, err
		}
	}

	// 3. The copies take their places.
	var errs []error
	note := &mergeNote{UUID: keep.UUID}
	stem := ""
	if err := calibreDB.QueryRow(`SELECT name FROM data WHERE book = ? ORDER BY id LIMIT 1`, keep.ID).Scan(&stem); err != nil || stem == "" {
		stem = bookFileStem(req.Title)
		if stem == "" {
			stem = bookFileStem(keep.Title)
		}
	}
	for _, c := range copies {
		st, err := os.Stat(c.temp)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		upper := strings.ToUpper(c.format)
		if c.from.Incoming() {
			note.AddedFiles = append(note.AddedFiles, upper)
		}
		if old, ok := keep.Files[c.format]; ok {
			// Replacing the kept book's own file: it goes to the batch, with
			// readers' bookmarks and highlights made in it.
			rf := recycledFormat{UUID: keep.UUID, Title: keep.Title, Authors: keep.Authors, Format: upper, File: filepath.Base(old), Replaced: true}
			if ost, err := os.Stat(old); err == nil {
				rf.Size = ost.Size()
			}
			if rf.Stored, err = batch.moveFormatFile(old); err != nil {
				errs = append(errs, fmt.Errorf("couldn't move the replaced %s aside: %w", upper, err))
				os.Remove(c.temp)
				continue
			}
			if err := os.Rename(c.temp, old); err != nil {
				errs = append(errs, fmt.Errorf("couldn't put %s's %s in place: %w", c.from.Title, upper, err))
				logErr(moveFile(batch.formatPath(&rf), old), "merge: couldn't put "+old+" back")
				os.Remove(c.temp)
				continue
			}
			if err := batch.takeFormatRows(&rf, []string{"bookmarks", "annotations"}); err != nil {
				errs = append(errs, fmt.Errorf("readers' bookmarks and highlights in the replaced %s: %w", upper, err))
			}
			batch.Formats = append(batch.Formats, rf)
			res, err := calibreDB.Exec(`UPDATE data SET uncompressed_size = ? WHERE book = ? AND format = ?`, st.Size(), keep.ID, upper)
			if err != nil {
				errs = append(errs, err)
			} else if n, _ := res.RowsAffected(); n == 0 {
				// A file metadata.db didn't list: it gets its row now.
				name := strings.TrimSuffix(filepath.Base(old), filepath.Ext(old))
				if _, err := calibreDB.Exec(`INSERT INTO data (book, format, uncompressed_size, name) VALUES (?, ?, ?, ?)`, keep.ID, upper, st.Size(), name); err != nil {
					errs = append(errs, fmt.Errorf("couldn't record %s in metadata.db: %w", upper, err))
				}
			}
			continue
		}
		name := stem
		dest := filepath.Join(keep.Dir, name+"."+c.format)
		if _, err := os.Stat(dest); err == nil {
			name = stem + "_" + strings.TrimPrefix(strings.TrimSuffix(filepath.Base(c.temp), "."+c.format), ".cf-merge-")
			dest = filepath.Join(keep.Dir, name+"."+c.format)
		}
		if err := os.Rename(c.temp, dest); err != nil {
			errs = append(errs, fmt.Errorf("couldn't put %s's %s in place: %w", c.from.Title, upper, err))
			os.Remove(c.temp)
			continue
		}
		if _, err := calibreDB.Exec(`INSERT INTO data (book, format, uncompressed_size, name) VALUES (?, ?, ?, ?)`, keep.ID, upper, st.Size(), name); err != nil {
			errs = append(errs, fmt.Errorf("couldn't record %s in metadata.db: %w", upper, err))
			os.Remove(dest)
		}
	}
	if coverTemp != "" {
		coverPath := filepath.Join(keep.Dir, "cover.jpg")
		if keep.HasCover {
			// Kept as the cover to revert to (cover dialog), as a new cover is.
			_ = copyFile(coverPath, filepath.Join(keep.Dir, "cover_backup.jpg"))
		}
		oldStamp, _ := bookCoverStamp(keep.UUID)
		if err := os.Rename(coverTemp, coverPath); err != nil {
			errs = append(errs, fmt.Errorf("couldn't put the cover in place: %w", err))
			os.Remove(coverTemp)
		} else {
			now := time.Now()
			_ = os.Chtimes(coverPath, now, now)
			if _, err := calibreDB.Exec(`UPDATE books SET has_cover = 1 WHERE id = ?`, keep.ID); err != nil {
				errs = append(errs, err)
			}
			if err := os.Remove(thumbCachePath(keep.UUID, oldStamp)); err != nil && !os.IsNotExist(err) {
				log.Printf("merge: couldn't remove %s's old thumbnail: %v", keep.UUID, err)
			}
			coverChanged(keep.UUID)
		}
	}

	// 4. Readers' data follows the books: bookmarks and highlights only
	// where the file they were made in is the one kept.
	dropped, err := mergeReaderData(keep.UUID, otherUUIDs, same)
	if err != nil {
		errs = append(errs, fmt.Errorf("readers' data: %w", err))
	}

	// 5. The metadata, as the editor saves it.
	if strings.TrimSpace(req.Date) == "" {
		if raw := unparseableStoredDate(keep.UUID); raw != "" {
			req.Date = raw
		}
	}
	meta := metadataFromRequest(req.ApplyMetadataRequest)
	if _, err := updateBookInCalibreDB(calibreDB, baseDir, keep.UUID, &meta); err != nil {
		errs = append(errs, fmt.Errorf("metadata: %w", err))
	}
	note.Title = meta.Title

	res := &mergeResult{UUID: keep.UUID, Title: meta.Title, Merged: len(recycled), Dropped: dropped}
	if batch != nil {
		recycleMu.Lock()
		batch.MergedInto = note
		logErr(batch.save(), "merge: couldn't note the merge in batch "+batch.ID)
		recycleMu.Unlock()
		res.Batch = batch.ID
	}

	formatsChanged(keep.UUID)
	refreshLibraryPages()
	refreshAllCaches()
	if len(recycled) > 0 {
		log.Printf("merge: %s merged %d book(s) into %q (%s), batch %s; %d bookmark(s)/highlight(s) not carried over", by, len(recycled), meta.Title, keep.UUID, res.Batch, dropped)
	} else {
		log.Printf("merge: %s added %s to %q (%s); replaced files in batch %q", by, strings.Join(note.AddedFiles, ", "), meta.Title, keep.UUID, res.Batch)
	}

	if err := errors.Join(errs...); err != nil {
		log.Printf("merge into %s: %v", keep.UUID, err)
		return res, fmt.Errorf("merged, but: %w", err)
	}
	return res, nil
}

// mergeReaderData copies readers' rows about the merged books to the kept
// one. A row the kept book already has (same key) stays, except for places
// and reading status, where the newest wins. Bookmarks and highlights are
// copied only in the formats same names for their book (its file is the one
// kept, byte for byte): elsewhere they'd point at the wrong places. It
// answers how many weren't copied.
func mergeReaderData(keep string, others []string, same map[string][]string) (int, error) {
	if appDB == nil || len(others) == 0 {
		return 0, nil
	}
	tx, err := appDB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	args := append([]any{keep}, toArgs(others)...)
	in := `book_uuid IN (` + placeholders(len(others)) + `)`
	stmts := []string{
		`INSERT OR IGNORE INTO favorites (user_id, book_uuid, created_at)
			SELECT user_id, ?, MIN(created_at) FROM favorites WHERE ` + in + ` GROUP BY user_id`,
		`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at)
			SELECT user_id, ?, status, source, updated_at FROM reading_status WHERE ` + in + ` AND true
			ON CONFLICT (user_id, book_uuid) DO UPDATE SET status = excluded.status, source = excluded.source, updated_at = excluded.updated_at
			WHERE excluded.updated_at > reading_status.updated_at`,
		`INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at)
			SELECT user_id, ?, format, position, percent, updated_at FROM reading_progress WHERE ` + in + ` AND true
			ON CONFLICT (user_id, book_uuid, format) DO UPDATE SET position = excluded.position, percent = excluded.percent, updated_at = excluded.updated_at
			WHERE excluded.updated_at > reading_progress.updated_at`,
		`INSERT OR IGNORE INTO shelf_books (shelf_id, book_uuid, added_at)
			SELECT shelf_id, ?, MIN(added_at) FROM shelf_books WHERE ` + in + ` GROUP BY shelf_id`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s, args...); err != nil {
			return 0, err
		}
	}
	// Bookmarks and highlights are keyed by the reader's own id, which the
	// copies keep with a suffix (the originals stay for a restore).
	dropped := 0
	for _, table := range []string{"bookmarks", "annotations"} {
		cols, err := tableColumns(context.Background(), tx, "main", table)
		if err != nil {
			return 0, err
		}
		sel := make([]string, len(cols))
		for i, c := range cols {
			switch c {
			case "book_uuid":
				sel[i] = "?"
			case "client_id":
				sel[i] = `client_id || '-m' || substr(book_uuid, 1, 8)`
			default:
				sel[i] = quoteIdent(c)
			}
		}
		live := ""
		if table == "annotations" {
			live = " AND deleted = 0" // a deleted highlight isn't lost
		}
		for _, u := range others {
			var total int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM `+quoteIdent(table)+` WHERE book_uuid = ?`+live, u).Scan(&total); err != nil {
				return 0, err
			}
			fs := same[u]
			if len(fs) == 0 {
				dropped += total
				continue
			}
			q := `INSERT OR IGNORE INTO ` + quoteIdent(table) + ` (` + columnList(cols) + `) SELECT ` + strings.Join(sel, ", ") +
				` FROM ` + quoteIdent(table) + ` WHERE book_uuid = ? AND lower(format) IN (` + placeholders(len(fs)) + `)`
			if _, err := tx.Exec(q, append([]any{keep, u}, toArgs(fs)...)...); err != nil {
				return 0, fmt.Errorf("%s: %w", table, err)
			}
			var kept int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM `+quoteIdent(table)+` WHERE book_uuid = ? AND lower(format) IN (`+placeholders(len(fs))+`)`+live, append([]any{u}, toArgs(fs)...)...).Scan(&kept); err != nil {
				return 0, err
			}
			dropped += total - kept
		}
	}
	return dropped, tx.Commit()
}

func mergeApplyHandler(w http.ResponseWriter, r *http.Request) {
	var req mergeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !slices.Contains(req.UUIDs, req.Keep) {
		http.Error(w, "the book to keep must be one of the books merged", http.StatusBadRequest)
		return
	}
	res, err := mergeBooks(&req, currentUser(r).Username)
	if res == nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errNoSuchBook) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	out := map[string]any{"uuid": res.UUID, "title": res.Title, "batch": res.Batch, "merged": res.Merged, "success": err == nil}
	if err != nil {
		out["warning"] = err.Error() // merged, with something left undone
	}
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(out), "encode merge")
}
