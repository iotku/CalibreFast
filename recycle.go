package main

// The Recycling Bin. Admins remove books from the library (POST
// /api/library/delete) and can put them back (POST
// /api/recycle-bin/{batch}/restore), but nothing on the web can empty the
// bin: that is done on the server (-empty-recycle-bin, or deleting the
// folders), so a stolen admin session can't destroy the backups too.
//
// Each removal (one book or many) is a batch, a folder under the bin:
//
//	<bin>/<batch id>/metadata.db      the library's metadata.db just before the removal
//	<bin>/<batch id>/batch.json       who removed which books, from where (recycleBatch)
//	<bin>/<batch id>/books/<book id>/ each book's folder, moved there as it was
//
// Restoring a book copies its rows back from the batch's metadata.db (so
// whatever changed in the library since is kept) and moves its folder back.
// The whole metadata.db is there for the server's owner to roll the library
// back by hand.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// recycleDir is -recycle-bin; "" means <datadir>/recycle-bin.
	recycleDir string
	// recycleMu serializes removals and restores.
	recycleMu sync.Mutex
)

const (
	recycleBatchFile = "batch.json"
	recycleDBFile    = "metadata.db"
	recycleBooksDir  = "books"
	// recycleMaxBooks is the most books one request may remove.
	recycleMaxBooks = 1000
)

var recycleBatchIDRe = regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{8}$`)

// recycleBinDir is the folder that holds the batches.
func recycleBinDir() string {
	if recycleDir != "" {
		return recycleDir
	}
	return filepath.Join(dataDir, "recycle-bin")
}

// recycledBook is one book in a batch.
type recycledBook struct {
	ID      int64    `json:"id"` // books.id when it was removed; its folder is books/<id>
	UUID    string   `json:"uuid"`
	Title   string   `json:"title"`
	Authors string   `json:"authors"` // "A & B"
	Path    string   `json:"path"`    // books.path: its folder, relative to the library
	Formats []string `json:"formats"`
	Size    int64    `json:"size"`     // bytes in its folder
	NoFiles bool     `json:"no_files"` // its folder wasn't in the library: only rows were removed
	// ReaderData counts readers' rows about it, by table (bookmarks,
	// annotations, …), copied to reader-data.json (recycle_userdata.go).
	ReaderData map[string]int `json:"reader_data,omitempty"`

	RestoredAt string `json:"restored_at,omitempty"` // RFC 3339
	RestoredBy string `json:"restored_by,omitempty"`
}

// recycleBatch is batch.json.
type recycleBatch struct {
	ID        string         `json:"id"`
	DeletedAt string         `json:"deleted_at"` // RFC 3339
	DeletedBy string         `json:"deleted_by"`
	Books     []recycledBook `json:"books"`
	// MergedInto: the batch is the books a merge took into another (merge.go).
	MergedInto *mergeNote `json:"merged_into,omitempty"`
}

func (b *recycleBatch) dir() string { return filepath.Join(recycleBinDir(), b.ID) }

func (b *recycleBatch) bookDir(id int64) string {
	return filepath.Join(b.dir(), recycleBooksDir, strconv.FormatInt(id, 10))
}

func (b *recycleBatch) save() error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(b.dir(), recycleBatchFile)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func loadRecycleBatch(id string) (*recycleBatch, error) {
	if !recycleBatchIDRe.MatchString(id) {
		return nil, fs.ErrNotExist
	}
	data, err := os.ReadFile(filepath.Join(recycleBinDir(), id, recycleBatchFile))
	if err != nil {
		return nil, err
	}
	var b recycleBatch
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("%s: %w", recycleBatchFile, err)
	}
	if b.ID != id {
		return nil, fmt.Errorf("%s names batch %q", recycleBatchFile, b.ID)
	}
	return &b, nil
}

func newRecycleBatchID(now time.Time) string {
	var r [4]byte
	_, _ = rand.Read(r[:])
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(r[:])
}

// libraryBookDir is a book's folder from its books.path, refusing anything
// that isn't strictly inside the library: an empty path would be the whole
// library.
func libraryBookDir(path string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(path))
	if path == "" || clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe book path %q", path)
	}
	return filepath.Join(baseDir, clean), nil
}

// ---------------------------------------------------------------------------
// Removing
// ---------------------------------------------------------------------------

// recycleBooks removes the books with these UUIDs from the library into a
// new batch. Either all of them are removed or none.
func recycleBooks(uuids []string, by string) (*recycleBatch, error) {
	recycleMu.Lock()
	defer recycleMu.Unlock()

	seen := map[string]bool{}
	batch := &recycleBatch{DeletedBy: by}
	for _, u := range uuids {
		if seen[u] {
			continue
		}
		seen[u] = true
		b := recycledBook{UUID: u}
		err := calibreDB.QueryRow(`SELECT b.id, b.title, `+bookAuthorsSQL+`, b.path FROM books b WHERE b.uuid = ?`, u).
			Scan(&b.ID, &b.Title, &b.Authors, &b.Path)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", errNoSuchBook, u)
		}
		if err != nil {
			return nil, err
		}
		if b.Formats, err = bookFormats(b.ID); err != nil {
			return nil, err
		}
		dir, err := libraryBookDir(b.Path)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", b.Title, err)
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			b.NoFiles = true
		} else {
			b.Size = dirSize(dir)
		}
		batch.Books = append(batch.Books, b)
	}
	if len(batch.Books) == 0 {
		return nil, errors.New("no books to remove")
	}

	// Calibre's triggers update its full-text tables on every removal.
	if err := checkVirtualTables(context.Background(), calibreDB, "main"); err != nil {
		return nil, err
	}

	now := time.Now()
	batch.ID = newRecycleBatchID(now)
	batch.DeletedAt = now.UTC().Format(time.RFC3339)
	if err := os.MkdirAll(recycleBinDir(), 0o700); err != nil {
		return nil, fmt.Errorf("recycling bin: %w", err)
	}
	if err := os.Mkdir(batch.dir(), 0o700); err != nil {
		return nil, fmt.Errorf("recycling bin: %w", err)
	}
	abandon := func() { logErr(os.RemoveAll(batch.dir()), "recycling bin: couldn't remove abandoned batch "+batch.ID) }

	// The database as it is now, before anything is removed.
	if _, err := calibreDB.Exec(`VACUUM INTO ?`, filepath.Join(batch.dir(), recycleDBFile)); err != nil {
		abandon()
		return nil, fmt.Errorf("couldn't back up metadata.db: %w", err)
	}
	if err := saveReaderData(batch); err != nil {
		abandon()
		return nil, fmt.Errorf("couldn't back up readers' bookmarks and notes: %w", err)
	}
	if err := batch.save(); err != nil {
		abandon()
		return nil, fmt.Errorf("recycling bin: %w", err)
	}

	// The folders, then the rows. If anything fails, the moved folders go
	// back and the batch is dropped.
	if err := os.Mkdir(filepath.Join(batch.dir(), recycleBooksDir), 0o700); err != nil {
		abandon()
		return nil, fmt.Errorf("recycling bin: %w", err)
	}
	var moved []recycledBook
	putBack := func() bool {
		ok := true
		for _, b := range moved {
			dir, _ := libraryBookDir(b.Path)
			if err := moveDir(batch.bookDir(b.ID), dir); err != nil {
				log.Printf("recycling bin: COULDN'T PUT %q BACK: %v (its files are in %s)", b.Title, err, batch.bookDir(b.ID))
				ok = false
			}
		}
		return ok
	}
	for _, b := range batch.Books {
		if b.NoFiles {
			continue
		}
		dir, _ := libraryBookDir(b.Path)
		if err := moveDir(dir, batch.bookDir(b.ID)); err != nil {
			if putBack() {
				abandon()
			}
			return nil, fmt.Errorf("couldn't move %q to the recycling bin: %w", b.Title, err)
		}
		moved = append(moved, b)
	}
	ids := make([]int64, len(batch.Books))
	for i, b := range batch.Books {
		ids[i] = b.ID
	}
	if err := deleteBooksFromCalibreDB(calibreDB, ids); err != nil {
		if putBack() {
			abandon()
		}
		return nil, fmt.Errorf("couldn't remove the books from metadata.db: %w", err)
	}

	for _, b := range moved {
		// The author's folder, once it's empty, as Calibre does.
		dir, _ := libraryBookDir(b.Path)
		if parent := filepath.Dir(dir); parent != filepath.Clean(baseDir) {
			_ = os.Remove(parent)
		}
	}
	removed := make([]string, len(batch.Books))
	for i, b := range batch.Books {
		forgetBook(b.UUID)
		removed[i] = b.UUID
	}
	catalogRemoved(removed...)
	logErr(removeFromPages(removed), "recycling bin: couldn't take the books out of the library pages")
	refreshAllCaches()

	titles := make([]string, len(batch.Books))
	for i, b := range batch.Books {
		titles[i] = fmt.Sprintf("%q (%s)", b.Title, b.UUID)
	}
	log.Printf("recycling bin: %s moved %d book(s) to batch %s: %s", by, len(batch.Books), batch.ID, strings.Join(titles, ", "))
	return batch, nil
}

var errNoSuchBook = errors.New("no such book in the library")

func bookFormats(id int64) ([]string, error) {
	rows, err := calibreDB.Query(`SELECT format FROM data WHERE book = ? ORDER BY format`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	formats := []string{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		formats = append(formats, strings.ToLower(f))
	}
	return formats, rows.Err()
}

// forgetBook drops what's cached about a book that left (or came back to)
// the library.
func forgetBook(uuid string) {
	uuidPathIndex.Delete(uuid)
	formatCache.Delete(uuid)
	modTimeCache.Delete(uuid)
	noCover.Delete(uuid)
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// moveDir moves a folder to dst, which must not exist. It renames when it
// can, otherwise (another filesystem) copies and then removes the original.
// If it fails, src is still whole and dst doesn't exist.
func moveDir(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s already exists", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyTree(src, dst); err != nil {
		_ = os.RemoveAll(dst)
		return err
	}
	if err := os.RemoveAll(src); err != nil {
		// Both copies may be partly there now; the new one is whole.
		log.Printf("moved %s to %s but couldn't remove the original: %v", src, dst, err)
	}
	return nil
}

// copyTree copies a folder of regular files and folders, keeping
// modification times (cover caching follows cover.jpg's).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := os.Mkdir(target, info.Mode().Perm()|0o700); err != nil {
				return err
			}
		case d.Type().IsRegular():
			if err := copyFile(p, target); err != nil {
				return err
			}
			_ = os.Chmod(target, info.Mode().Perm())
			_ = os.Chtimes(target, info.ModTime(), info.ModTime())
		default:
			return fmt.Errorf("%s isn't a regular file", p)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Rows: tables that refer to books
// ---------------------------------------------------------------------------

type sqlQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// tableColumns is a table's columns, in order; nil if there's no such table.
func tableColumns(ctx context.Context, q sqlQueryer, schema, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?, ?)`, table, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

type bookTable struct {
	name string
	cols []string
}

// buildWithFTS5 explains a build without SQLite's FTS5 module.
const buildWithFTS5 = "CalibreFast was built without SQLite's FTS5 module, which Calibre's full-text tables (annotations_fts) need. Rebuild with: go build -tags sqlite_fts5 ."

// checkVirtualTables opens each of a database's virtual tables (Calibre's
// full-text search over annotations), as removing or restoring a book
// writes to them through Calibre's triggers. It says what's missing rather
// than failing halfway.
func checkVirtualTables(ctx context.Context, q sqlQueryer, schema string) error {
	rows, err := q.QueryContext(ctx, `SELECT name FROM `+quoteIdent(schema)+`.sqlite_master WHERE type = 'table' AND sql LIKE 'CREATE VIRTUAL%'`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	rows.Close()
	for _, n := range names {
		r, err := q.QueryContext(ctx, `SELECT 1 FROM `+quoteIdent(schema)+`.`+quoteIdent(n)+` LIMIT 0`)
		if err != nil {
			if strings.Contains(err.Error(), "no such module: fts5") {
				return errors.New(buildWithFTS5)
			}
			return fmt.Errorf("can't open Calibre's table %s: %w", n, err)
		}
		r.Close()
	}
	return nil
}

// tablesReferringToBooks lists the tables with a "book" column: Calibre's
// links (authors, tags, …), comments, formats, identifiers, custom columns,
// and whatever newer versions add. Virtual tables (full-text search) have
// none, and are skipped, as they may need modules only Calibre has.
func tablesReferringToBooks(ctx context.Context, q sqlQueryer, schema string) ([]bookTable, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM `+quoteIdent(schema)+`.sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name <> 'books'
		AND sql NOT LIKE 'CREATE VIRTUAL%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []bookTable
	for _, n := range names {
		cols, err := tableColumns(ctx, q, schema, n)
		if err != nil {
			return nil, fmt.Errorf("columns of %s: %w", n, err)
		}
		if slices.Contains(cols, "book") {
			out = append(out, bookTable{n, cols})
		}
	}
	return out, nil
}

// linkedEntity is what a link table's rows point to: books_authors_link's
// author column holds authors.id, and an author is the same one in another
// copy of the database when its name is.
type linkedEntity struct {
	column string // in the link table
	table  string // the items
	key    string // the items' column that identifies one
}

var customLinkRe = regexp.MustCompile(`^books_(custom_column_\d+)_link$`)

func linkedEntityOf(t bookTable) (linkedEntity, bool) {
	var e linkedEntity
	switch t.name {
	case "books_authors_link":
		e = linkedEntity{"author", "authors", "name"}
	case "books_publishers_link":
		e = linkedEntity{"publisher", "publishers", "name"}
	case "books_series_link":
		e = linkedEntity{"series", "series", "name"}
	case "books_tags_link":
		e = linkedEntity{"tag", "tags", "name"}
	case "books_ratings_link":
		e = linkedEntity{"rating", "ratings", "rating"}
	case "books_languages_link":
		e = linkedEntity{"lang_code", "languages", "lang_code"}
	default:
		m := customLinkRe.FindStringSubmatch(t.name)
		if m == nil {
			return e, false
		}
		e = linkedEntity{"value", m[1], "value"}
	}
	return e, slices.Contains(t.cols, e.column)
}

// cleanedEntities are the items a removal deletes once no book has them, as
// Calibre does. Restoring adds them back from the batch's copy.
var cleanedEntities = []string{"books_authors_link", "books_publishers_link", "books_series_link", "books_tags_link"}

// deleteBookFromCalibreDB removes a book's rows from metadata.db.
func deleteBookFromCalibreDB(db *sql.DB, bookID int64) error {
	return deleteBooksFromCalibreDB(db, []int64{bookID})
}

// deleteBooksFromCalibreDB removes books and every row that refers to them
// from metadata.db, in one transaction, and the authors, publishers, series
// and tags no book has any more.
func deleteBooksFromCalibreDB(db *sql.DB, ids []int64) (err error) {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	tables, err := tablesReferringToBooks(ctx, tx, "main")
	if err != nil {
		return err
	}
	in := placeholders(len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	type orphanCheck struct {
		link linkedEntity
		from string
		ids  []any
	}
	var checks []orphanCheck
	for _, t := range tables {
		e, ok := linkedEntityOf(t)
		if !ok || !slices.Contains(cleanedEntities, t.name) {
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT `+quoteIdent(e.column)+` FROM `+quoteIdent(t.name)+` WHERE book IN (`+in+`)`, args...)
		if err != nil {
			return fmt.Errorf("read %s: %w", t.name, err)
		}
		c := orphanCheck{link: e, from: t.name}
		for rows.Next() {
			var v int64
			if rows.Scan(&v) == nil {
				c.ids = append(c.ids, v)
			}
		}
		rows.Close()
		if len(c.ids) > 0 {
			checks = append(checks, c)
		}
	}

	for _, t := range tables {
		if _, err = tx.ExecContext(ctx, `DELETE FROM `+quoteIdent(t.name)+` WHERE book IN (`+in+`)`, args...); err != nil {
			return fmt.Errorf("delete from %s: %w", t.name, err)
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM books WHERE id IN (`+in+`)`, args...)
	if err != nil {
		return fmt.Errorf("delete books: %w", err)
	}
	if n, _ := res.RowsAffected(); n != int64(len(ids)) {
		err = fmt.Errorf("expected to delete %d book(s), deleted %d", len(ids), n)
		return err
	}
	for _, c := range checks {
		if _, err = tx.ExecContext(ctx, `DELETE FROM `+quoteIdent(c.link.table)+` WHERE id IN (`+placeholders(len(c.ids))+`)
			AND NOT EXISTS (SELECT 1 FROM `+quoteIdent(c.from)+` l WHERE l.`+quoteIdent(c.link.column)+` = `+quoteIdent(c.link.table)+`.id)`, c.ids...); err != nil {
			return fmt.Errorf("delete unused %s: %w", c.link.table, err)
		}
	}
	return tx.Commit()
}

// restoreBookRows copies a book's rows from a batch's metadata.db (attached
// as "bak") back into the library's, under a new id if its old one has been
// taken. Items it links to (authors, tags, …) are matched by name, and added
// back when the library no longer has them.
func restoreBookRows(ctx context.Context, bakPath, uuid string) (newID int64, err error) {
	conn, err := calibreDB.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS bak`, bakPath); err != nil {
		return 0, fmt.Errorf("open the batch's metadata.db: %w", err)
	}
	defer func() {
		logErr(func() error { _, err := conn.ExecContext(ctx, `DETACH DATABASE bak`); return err }(), "detach batch database")
	}()
	if err := checkVirtualTables(ctx, conn, "main"); err != nil {
		return 0, err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	var n int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.books WHERE uuid = ?`, uuid).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		err = errors.New("a book with its UUID is already in the library")
		return 0, err
	}
	var oldID int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM bak.books WHERE uuid = ?`, uuid).Scan(&oldID); err != nil {
		return 0, fmt.Errorf("find it in the batch's metadata.db: %w", err)
	}

	// The book: its old id if that's free (Calibre never reuses them).
	cols, err := sharedColumns(ctx, tx, "books")
	if err != nil {
		return 0, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.books WHERE id = ?`, oldID).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		cols = slices.DeleteFunc(cols, func(c string) bool { return c == "id" })
	}
	list := columnList(cols)
	res, err := tx.ExecContext(ctx, `INSERT INTO main.books (`+list+`) SELECT `+list+` FROM bak.books WHERE id = ?`, oldID)
	if err != nil {
		return 0, fmt.Errorf("insert book: %w", err)
	}
	if newID, err = res.LastInsertId(); err != nil {
		return 0, err
	}
	// Calibre's insert trigger gives every new book a new uuid (and sort).
	if _, err = tx.ExecContext(ctx, `UPDATE main.books SET uuid = ? WHERE id = ?`, uuid, newID); err != nil {
		return 0, fmt.Errorf("keep uuid: %w", err)
	}
	if slices.Contains(cols, "sort") {
		if _, err = tx.ExecContext(ctx, `UPDATE main.books SET sort = (SELECT sort FROM bak.books WHERE id = ?) WHERE id = ?`, oldID, newID); err != nil {
			return 0, fmt.Errorf("keep sort: %w", err)
		}
	}

	tables, err := tablesReferringToBooks(ctx, tx, "bak")
	if err != nil {
		return 0, err
	}
	entityIDs := map[string]map[int64]int64{} // table -> old id -> id in the library
	for _, t := range tables {
		cols, err2 := sharedColumns(ctx, tx, t.name)
		if err2 != nil {
			err = err2
			return 0, err
		}
		if cols == nil {
			log.Printf("restore %s: the library has no table %s; its rows there are left out", uuid, t.name)
			continue
		}
		cols = slices.DeleteFunc(cols, func(c string) bool { return c == "id" })
		// The book was inserted a moment ago, so any rows it has here are
		// defaults Calibre's insert trigger made (newer Calibre gives each
		// book a books_pages_link row): its own from the batch replace them,
		// when the batch has any.
		var saved int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM bak.`+quoteIdent(t.name)+` WHERE book = ?`, oldID).Scan(&saved); err != nil {
			return 0, fmt.Errorf("read %s: %w", t.name, err)
		}
		if saved == 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM main.`+quoteIdent(t.name)+` WHERE book = ?`, newID); err != nil {
			return 0, fmt.Errorf("clear %s: %w", t.name, err)
		}
		e, isLink := linkedEntityOf(t)
		if !isLink {
			sel := make([]string, len(cols))
			for i, c := range cols {
				if c == "book" {
					sel[i] = "?"
				} else {
					sel[i] = quoteIdent(c)
				}
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO main.`+quoteIdent(t.name)+` (`+columnList(cols)+`) SELECT `+strings.Join(sel, ", ")+` FROM bak.`+quoteIdent(t.name)+` WHERE book = ?`, newID, oldID); err != nil {
				return 0, fmt.Errorf("restore %s: %w", t.name, err)
			}
			continue
		}
		if err = restoreLinks(ctx, tx, t.name, cols, e, oldID, newID, entityIDs); err != nil {
			return 0, err
		}
	}
	return newID, tx.Commit()
}

func columnList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quoteIdent(c)
	}
	return strings.Join(q, ", ")
}

// sharedColumns is the columns a table has in both the library ("main") and
// the batch ("bak"); nil if either lacks it.
func sharedColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	live, err := tableColumns(ctx, tx, "main", table)
	if err != nil || live == nil {
		return nil, err
	}
	old, err := tableColumns(ctx, tx, "bak", table)
	if err != nil || old == nil {
		return nil, err
	}
	var out []string
	for _, c := range old {
		if slices.Contains(live, c) {
			out = append(out, c)
		}
	}
	return out, nil
}

// restoreLinks copies one link table's rows for a book, pointing them at the
// library's own items.
func restoreLinks(ctx context.Context, tx *sql.Tx, table string, cols []string, e linkedEntity, oldID, newID int64, ids map[string]map[int64]int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+columnList(cols)+` FROM bak.`+quoteIdent(table)+` WHERE book = ?`, oldID)
	if err != nil {
		return fmt.Errorf("read %s: %w", table, err)
	}
	var all [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return err
		}
		all = append(all, vals)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	ei := slices.Index(cols, e.column)
	bi := slices.Index(cols, "book")
	for _, vals := range all {
		old, ok := vals[ei].(int64)
		if !ok {
			continue
		}
		id, err := restoreEntity(ctx, tx, e, old, ids)
		if err != nil {
			return fmt.Errorf("restore %s: %w", e.table, err)
		}
		vals[ei], vals[bi] = id, newID
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.`+quoteIdent(table)+` (`+columnList(cols)+`) VALUES (`+placeholders(len(cols))+`)`, vals...); err != nil {
			return fmt.Errorf("restore %s: %w", table, err)
		}
	}
	return nil
}

// restoreEntity is the library's id for the batch's item old (an author,
// tag, …): the one with the same name, or a copy of the batch's.
func restoreEntity(ctx context.Context, tx *sql.Tx, e linkedEntity, old int64, ids map[string]map[int64]int64) (int64, error) {
	if id, ok := ids[e.table][old]; ok {
		return id, nil
	}
	var key any
	if err := tx.QueryRowContext(ctx, `SELECT `+quoteIdent(e.key)+` FROM bak.`+quoteIdent(e.table)+` WHERE id = ?`, old).Scan(&key); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM main.`+quoteIdent(e.table)+` WHERE `+quoteIdent(e.key)+` = ?`, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		cols, err2 := sharedColumns(ctx, tx, e.table)
		if err2 != nil {
			return 0, err2
		}
		if cols == nil {
			return 0, fmt.Errorf("the library has no table %s", e.table)
		}
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.`+quoteIdent(e.table)+` WHERE id = ?`, old).Scan(&taken); err != nil {
			return 0, err
		}
		if taken > 0 {
			cols = slices.DeleteFunc(cols, func(c string) bool { return c == "id" })
		}
		list := columnList(cols)
		res, err3 := tx.ExecContext(ctx, `INSERT INTO main.`+quoteIdent(e.table)+` (`+list+`) SELECT `+list+` FROM bak.`+quoteIdent(e.table)+` WHERE id = ?`, old)
		if err3 != nil {
			return 0, err3
		}
		if id, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	if ids[e.table] == nil {
		ids[e.table] = map[int64]int64{}
	}
	ids[e.table][old] = id
	return id, nil
}

// ---------------------------------------------------------------------------
// Restoring
// ---------------------------------------------------------------------------

// restoreResult is what happened to one book a restore asked for.
type restoreResult struct {
	UUID  string `json:"uuid"`
	Title string `json:"title"`
	Error string `json:"error,omitempty"`
}

// restoreRecycled puts books from a batch back in the library: those with
// these UUIDs, or every one still in the bin when there are none.
func restoreRecycled(batchID string, uuids []string, by string) ([]restoreResult, error) {
	recycleMu.Lock()
	defer recycleMu.Unlock()

	batch, err := loadRecycleBatch(batchID)
	if err != nil {
		return nil, err
	}
	bakPath := filepath.Join(batch.dir(), recycleDBFile)
	if _, err := os.Stat(bakPath); err != nil {
		return nil, fmt.Errorf("the batch's copy of metadata.db is gone: %w", err)
	}

	var results []restoreResult
	changed := false
	for i := range batch.Books {
		b := &batch.Books[i]
		if len(uuids) > 0 {
			if !slices.Contains(uuids, b.UUID) {
				continue
			}
		} else if b.RestoredAt != "" || !batch.inBin(b) {
			continue
		}
		r := restoreResult{UUID: b.UUID, Title: b.Title}
		if err := batch.restoreOne(b, bakPath); err != nil {
			r.Error = err.Error()
			log.Printf("recycling bin: %s couldn't restore %q from batch %s: %v", by, b.Title, batch.ID, err)
		} else {
			b.RestoredAt = time.Now().UTC().Format(time.RFC3339)
			b.RestoredBy = by
			changed = true
			catalogChanged(b.UUID)
			logErr(restoreToPages(b.UUID), "recycling bin: couldn't put "+b.UUID+" back in the library pages")
			log.Printf("recycling bin: %s restored %q (%s) from batch %s", by, b.Title, b.UUID, batch.ID)
		}
		results = append(results, r)
	}
	for _, u := range uuids {
		if !slices.ContainsFunc(batch.Books, func(b recycledBook) bool { return b.UUID == u }) {
			results = append(results, restoreResult{UUID: u, Error: "not in this batch"})
		}
	}
	if changed {
		logErr(batch.save(), "recycling bin: couldn't update "+batch.ID+"/"+recycleBatchFile)
		refreshAllCaches()
	}
	return results, nil
}

// inBin: the book's files are still in the batch (or it had none).
func (batch *recycleBatch) inBin(b *recycledBook) bool {
	if b.NoFiles {
		return true
	}
	st, err := os.Stat(batch.bookDir(b.ID))
	return err == nil && st.IsDir()
}

func (batch *recycleBatch) restoreOne(b *recycledBook, bakPath string) error {
	if b.RestoredAt != "" {
		return errors.New("already restored")
	}
	if !batch.inBin(b) {
		return errors.New("its files are no longer in the recycling bin")
	}
	dir, err := libraryBookDir(b.Path)
	if err != nil {
		return err
	}
	if !b.NoFiles {
		if _, err := os.Lstat(dir); err == nil {
			return fmt.Errorf("the library already has a folder %s", b.Path)
		}
	}
	var n int
	if err := calibreDB.QueryRow(`SELECT COUNT(*) FROM books WHERE uuid = ?`, b.UUID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("it's already in the library")
	}
	if !b.NoFiles {
		if err := moveDir(batch.bookDir(b.ID), dir); err != nil {
			return fmt.Errorf("couldn't move its files back: %w", err)
		}
	}
	if _, err := restoreBookRows(context.Background(), bakPath, b.UUID); err != nil {
		if !b.NoFiles {
			if mErr := moveDir(dir, batch.bookDir(b.ID)); mErr != nil {
				log.Printf("recycling bin: couldn't move %s back to batch %s: %v", dir, batch.ID, mErr)
			}
		}
		return fmt.Errorf("couldn't add it back to metadata.db: %w", err)
	}
	// The book is back either way; readers' rows are there unless something
	// removed them since.
	if n, err := restoreReaderData(batch, b.UUID); err != nil {
		log.Printf("recycling bin: %q is back, but its readers' data from batch %s couldn't be checked: %v", b.Title, batch.ID, err)
	} else if n > 0 {
		log.Printf("recycling bin: added back %d bookmark/note/progress row(s) for %q from batch %s", n, b.Title, batch.ID)
	}
	forgetBook(b.UUID)
	return nil
}

// ---------------------------------------------------------------------------
// Listing and emptying
// ---------------------------------------------------------------------------

// listRecycleBatches is every readable batch, newest first. Folders that
// aren't batches are left alone.
func listRecycleBatches() ([]*recycleBatch, error) {
	entries, err := os.ReadDir(recycleBinDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*recycleBatch
	for _, e := range entries {
		if !e.IsDir() || !recycleBatchIDRe.MatchString(e.Name()) {
			continue
		}
		b, err := loadRecycleBatch(e.Name())
		if err != nil {
			log.Printf("recycling bin: skipping %s: %v", e.Name(), err)
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// emptyRecycleBin deletes the batches removed more than olderThan ago. It's
// only run from the command line (-empty-recycle-bin), never from the web.
func emptyRecycleBin(olderThan time.Duration, out io.Writer) error {
	batches, err := listRecycleBatches()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-olderThan)
	kept, removed := 0, 0
	for _, b := range batches {
		at, err := time.Parse(time.RFC3339, b.DeletedAt)
		if err != nil || at.After(cutoff) {
			kept++
			continue
		}
		if err := os.RemoveAll(b.dir()); err != nil {
			return fmt.Errorf("%s: %w", b.ID, err)
		}
		removed++
		fmt.Fprintf(out, "removed %s (%d book(s), deleted by %s at %s)\n", b.ID, len(b.Books), b.DeletedBy, b.DeletedAt)
	}
	fmt.Fprintf(out, "%s: removed %d batch(es), kept %d\n", recycleBinDir(), removed, kept)
	return nil
}

// parseAge reads -empty-recycle-bin's age: a Go duration ("36h"), a number
// of days ("30d"), or "0" for everything.
func parseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad age %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	if s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad age %q (try 30d, 12h or 0)", s)
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func registerRecycleRoutes() {
	http.HandleFunc("GET /recycle-bin", requireWrites(recycleBinPage))
	http.HandleFunc("GET /api/recycle-bin", requireWrites(recycleBinHandler))
	http.HandleFunc("POST /api/recycle-bin/{batch}/restore", requireWrites(recycleRestoreHandler))
	http.HandleFunc("POST /api/library/delete", requireWrites(recycleDeleteHandler))
}

func recycleBinPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "recycle-bin.html", PageData{Title: SiteTitle}), "render recycle-bin")
}

// recycleBookView is a book in GET /api/recycle-bin (web/types.ts RecycleBin).
type recycleBookView struct {
	recycledBook
	InBin bool `json:"in_bin"` // can be restored
}

type recycleBatchView struct {
	ID         string            `json:"id"`
	DeletedAt  string            `json:"deleted_at"`
	DeletedBy  string            `json:"deleted_by"`
	Books      []recycleBookView `json:"books"`
	MergedInto *mergeNote        `json:"merged_into,omitempty"`
}

func recycleBinHandler(w http.ResponseWriter, r *http.Request) {
	batches, err := listRecycleBatches()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views := []recycleBatchView{}
	for _, b := range batches {
		v := recycleBatchView{ID: b.ID, DeletedAt: b.DeletedAt, DeletedBy: b.DeletedBy, Books: []recycleBookView{}, MergedInto: b.MergedInto}
		for i := range b.Books {
			bk := &b.Books[i]
			v.Books = append(v.Books, recycleBookView{*bk, bk.RestoredAt == "" && b.inBin(bk)})
		}
		views = append(views, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	logErr(json.NewEncoder(w).Encode(map[string]any{"folder": recycleBinDir(), "batches": views}), "encode recycle bin")
}

type uuidsRequest struct {
	UUIDs []string `json:"uuids"`
}

func readUUIDs(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	var req uuidsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	if len(req.UUIDs) > recycleMaxBooks {
		http.Error(w, fmt.Sprintf("at most %d books at a time", recycleMaxBooks), http.StatusBadRequest)
		return nil, false
	}
	for _, u := range req.UUIDs {
		if !isSafeUUIDComponent(u) {
			http.Error(w, "bad book id", http.StatusBadRequest)
			return nil, false
		}
	}
	return req.UUIDs, true
}

func recycleDeleteHandler(w http.ResponseWriter, r *http.Request) {
	uuids, ok := readUUIDs(w, r)
	if !ok {
		return
	}
	if len(uuids) == 0 {
		http.Error(w, "no books named", http.StatusBadRequest)
		return
	}
	batch, err := recycleBooks(uuids, currentUser(r).Username)
	if errors.Is(err, errNoSuchBook) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		log.Println("recycling bin:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(batch), "encode removal")
}

func recycleRestoreHandler(w http.ResponseWriter, r *http.Request) {
	uuids, ok := readUUIDs(w, r)
	if !ok {
		return
	}
	results, err := restoreRecycled(r.PathValue("batch"), uuids, currentUser(r).Username)
	if errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "no such batch in the recycling bin (emptied on the server?)", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if results == nil {
		results = []restoreResult{}
	}
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(map[string]any{"results": results}), "encode restore")
}
