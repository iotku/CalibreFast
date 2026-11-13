package main

// Single files in the Recycling Bin: one format of a book that stays in the
// library. An admin removes a format from the book page (POST
// /api/library/books/{uuid}/formats/{format}/remove), and merging or adding
// a file (merge.go, add_format.go) puts a file it replaces here too. Either
// way it is a batch entry (recycledFormat) that can be restored, unless the
// book has a file in that format again.
//
//	<bin>/<batch id>/formats/<file>                   the file, as it was in the book's folder
//	<bin>/<batch id>/formats/<file>.reader-data.json  readers' rows in that format taken with it
//
// Readers' bookmarks and highlights are tied to the exact file they were
// made in, so they go with it: a different file in the same format would
// put them in the wrong places. A removal takes the places too (the book has
// nothing left to open them in); a replacement leaves them, so readers start
// near where they were.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const recycleFormatsDir = "formats"

// recycledFormat is one file in a batch (TS: RecycledFormat in web/types.ts).
type recycledFormat struct {
	UUID    string `json:"uuid"` // the book it was taken from
	Title   string `json:"title"`
	Authors string `json:"authors"`
	Format  string `json:"format"` // "EPUB"
	File    string `json:"file"`   // its name in the book's folder
	Stored  string `json:"stored"` // its name under the batch's formats/
	Size    int64  `json:"size"`
	// Replaced: a merge or a new file took its place (else it was removed).
	Replaced   bool           `json:"replaced,omitempty"`
	ReaderData map[string]int `json:"reader_data,omitempty"` // rows taken with it, by table
	RestoredAt string         `json:"restored_at,omitempty"`
	RestoredBy string         `json:"restored_by,omitempty"`
}

// key names the file in a restore request.
func (f *recycledFormat) key() string { return f.UUID + "/" + f.Format }

func (b *recycleBatch) formatPath(f *recycledFormat) string {
	return filepath.Join(b.dir(), recycleFormatsDir, f.Stored)
}

func (b *recycleBatch) formatInBin(f *recycledFormat) bool {
	st, err := os.Stat(b.formatPath(f))
	return err == nil && st.Mode().IsRegular()
}

var (
	errNoSuchFormat = errors.New("the book has no file in that format")
	errLastFormat   = errors.New("that is the book's only file: remove the book instead")
)

// moveFormatFile moves a book's file into the batch's formats/ folder and
// returns its name there.
func (b *recycleBatch) moveFormatFile(path string) (string, error) {
	dir := filepath.Join(b.dir(), recycleFormatsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := filepath.Base(path)
	for i := 2; ; i++ {
		if _, err := os.Lstat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) {
			break
		}
		ext := filepath.Ext(path)
		name = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(filepath.Base(path), ext), i, ext)
	}
	return name, moveFile(path, filepath.Join(dir, name))
}

// takeFormatRows backs up readers' rows of tables about f's book in its
// format next to its file, deletes them from calibrefast.db, and counts
// them in f.ReaderData.
func (b *recycleBatch) takeFormatRows(f *recycledFormat, tables []string) error {
	if appDB == nil || len(tables) == 0 {
		return nil
	}
	format := strings.ToLower(f.Format)
	data, counts, err := collectReaderRows(tables, `book_uuid = ? AND lower(format) = ?`, f.UUID, format)
	if err != nil {
		return err
	}
	if len(counts[f.UUID]) == 0 {
		return nil
	}
	if err := writeReaderRows(b.formatPath(f)+".reader-data.json", data); err != nil {
		return err
	}
	tx, err := appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range tables {
		if _, err := tx.Exec(`DELETE FROM `+quoteIdent(t)+` WHERE book_uuid = ? AND lower(format) = ?`, f.UUID, format); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	f.ReaderData = counts[f.UUID]
	return nil
}

// formatsChanged updates what's kept about a book's files after one was
// added, removed or replaced.
func formatsChanged(uuid string) {
	formatCache.Delete(uuid)
	bookMetadataCache.Delete(uuid)
	if appDB != nil {
		// KOReader's hashes of the old files.
		logErr(execErr(appDB.Exec(`DELETE FROM book_digests WHERE book_uuid = ?`, uuid)), "formats: forget "+uuid+"'s file hashes")
	}
	catalogChanged(uuid)
	refreshPagesFor(uuid)
}

func execErr(_ sql.Result, err error) error { return err }

// recycleFormat removes the book's file in format (and its data row) to a
// new batch, with readers' places, bookmarks and highlights in it. A book's
// last file can't be removed this way: that's removing the book.
func recycleFormat(uuid, format, by string) (*recycleBatch, error) {
	bulkMu.Lock()
	defer bulkMu.Unlock()
	recycleMu.Lock()
	defer recycleMu.Unlock()

	format = strings.ToLower(format)
	f := recycledFormat{UUID: uuid, Format: strings.ToUpper(format)}
	var id int64
	var path string
	err := calibreDB.QueryRow(`SELECT b.id, b.title, `+bookAuthorsSQL+`, b.path FROM books b WHERE b.uuid = ?`, uuid).
		Scan(&id, &f.Title, &f.Authors, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", errNoSuchBook, uuid)
	}
	if err != nil {
		return nil, err
	}
	var rows int
	if err := calibreDB.QueryRow(`SELECT COUNT(*) FROM data WHERE book = ? AND lower(format) = ?`, id, format).Scan(&rows); err != nil {
		return nil, err
	}
	files, err := bookFiles(calibreDB, baseDir, id, path)
	if err != nil {
		return nil, err
	}
	var matches []string
	others := map[string]bool{}
	for _, df := range files {
		if df.Format == format {
			matches = append(matches, df.Path)
		} else {
			others[df.Format] = true
		}
	}
	switch {
	case rows == 0:
		return nil, errNoSuchFormat
	case len(others) == 0:
		return nil, errLastFormat
	case len(matches) > 1:
		return nil, fmt.Errorf("the book's folder has %d %s files: remove the one you don't want on the server", len(matches), f.Format)
	}

	batch := &recycleBatch{DeletedBy: by}
	abandon, err := batch.create()
	if err != nil {
		return nil, err
	}
	var moved string // where the file is in the batch
	if len(matches) == 1 {
		st, err := os.Stat(matches[0])
		if err != nil {
			abandon()
			return nil, err
		}
		f.File, f.Size = filepath.Base(matches[0]), st.Size()
		if f.Stored, err = batch.moveFormatFile(matches[0]); err != nil {
			abandon()
			return nil, fmt.Errorf("couldn't move the %s to the recycling bin: %w", f.Format, err)
		}
		moved = batch.formatPath(&f)
	}
	if _, err := calibreDB.Exec(`DELETE FROM data WHERE book = ? AND lower(format) = ?`, id, format); err != nil {
		if moved != "" {
			if mErr := moveFile(moved, matches[0]); mErr != nil {
				log.Printf("recycling bin: COULDN'T PUT %s BACK: %v (it is %s)", matches[0], mErr, moved)
				return nil, fmt.Errorf("couldn't remove the %s from metadata.db: %w", f.Format, err)
			}
		}
		abandon()
		return nil, fmt.Errorf("couldn't remove the %s from metadata.db: %w", f.Format, err)
	}
	if f.Stored == "" {
		// Listed, but the file was already gone: the entry is only the row.
		f.Stored = f.Format + ".missing"
	}
	if err := batch.takeFormatRows(&f, formatReaderTables); err != nil {
		log.Printf("recycling bin: %q's %s is removed, but readers' rows in it couldn't be taken: %v", f.Title, f.Format, err)
	}
	batch.Formats = append(batch.Formats, f)
	if err := batch.save(); err != nil {
		log.Printf("recycling bin: couldn't save batch %s: %v", batch.ID, err)
	}
	formatsChanged(uuid)
	log.Printf("recycling bin: %s moved %q's %s (%s) to batch %s", by, f.Title, f.Format, uuid, batch.ID)
	return batch, nil
}

// restoreFormat puts a file back in its book. The book must be in the
// library without a file in that format. Called with recycleMu held.
func (b *recycleBatch) restoreFormat(f *recycledFormat) error {
	if f.RestoredAt != "" {
		return errors.New("already restored")
	}
	if !b.formatInBin(f) {
		return errors.New("its file is no longer in the recycling bin")
	}
	var id int64
	var path string
	err := calibreDB.QueryRow(`SELECT id, path FROM books WHERE uuid = ?`, f.UUID).Scan(&id, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("its book isn't in the library (restore the book first)")
	}
	if err != nil {
		return err
	}
	format := strings.ToLower(f.Format)
	var n int
	if err := calibreDB.QueryRow(`SELECT COUNT(*) FROM data WHERE book = ? AND lower(format) = ?`, id, format).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("the book has a %s again: remove that one first", f.Format)
	}
	dir, err := libraryBookDir(path)
	if err != nil {
		return err
	}
	name := f.File
	if name == "" || name != filepath.Base(name) || !strings.EqualFold(filepath.Ext(name), "."+format) {
		return fmt.Errorf("bad file name %q", name)
	}
	dest := filepath.Join(dir, name)
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("the book's folder already has a file %s", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := moveFile(b.formatPath(f), dest); err != nil {
		return fmt.Errorf("couldn't move the file back: %w", err)
	}
	// Calibre finds the file as <data.name>.<format>.
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	if _, err := calibreDB.Exec(`INSERT INTO data (book, format, uncompressed_size, name) VALUES (?, ?, ?, ?)`, id, f.Format, f.Size, stem); err != nil {
		if mErr := moveFile(dest, b.formatPath(f)); mErr != nil {
			log.Printf("recycling bin: couldn't move %s back to batch %s: %v", dest, b.ID, mErr)
		}
		return fmt.Errorf("couldn't add it back to metadata.db: %w", err)
	}
	if n, err := restoreReaderRows(b.formatPath(f)+".reader-data.json", f.UUID, format); err != nil {
		log.Printf("recycling bin: %q's %s is back, but its readers' data couldn't be checked: %v", f.Title, f.Format, err)
	} else if n > 0 {
		log.Printf("recycling bin: added back %d bookmark/note/progress row(s) for %q's %s", n, f.Title, f.Format)
	}
	formatsChanged(f.UUID)
	return nil
}

// POST /api/library/books/{uuid}/formats/{format}/remove: the batch, or 409
// when it's the book's last file.
func recycleFormatHandler(w http.ResponseWriter, r *http.Request) {
	uuid, format := r.PathValue("uuid"), strings.ToLower(r.PathValue("format"))
	if !isSafeUUIDComponent(uuid) || !hasBookExt("."+format) {
		http.Error(w, "bad book or format", http.StatusBadRequest)
		return
	}
	batch, err := recycleFormat(uuid, format, currentUser(r).Username)
	switch {
	case errors.Is(err, errNoSuchBook), errors.Is(err, errNoSuchFormat):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case errors.Is(err, errLastFormat):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "last": true})
		return
	case err != nil:
		log.Println("recycling bin:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}
