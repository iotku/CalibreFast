package main

// Duplicate checks, before a book is added to the library (uploads and the
// import queue) and for the metadata editor's warning.
//
// The library is searched for books with the same title (case aside). One
// with the same authors too is a likely duplicate. One with only the title
// is a duplicate only if it holds a file identical (SHA-256) to one of the
// book's: two books can share a title. A likely duplicate can still be
// added if the admin says so (override); a file the library already holds,
// byte for byte, can't be added again.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// dupFile is one of the files of the book being checked.
type dupFile struct {
	Path   string // where it is now
	Format string // "epub", "pdf", … (lower case)
}

// duplicateBook is a library book that looks like the one being checked
// (TS: DuplicateBook in web/types.ts).
type duplicateBook struct {
	UUID    string   `json:"uuid"`
	Title   string   `json:"title"`
	Authors []string `json:"authors"`
	// SameAuthors: same title and authors. SameFile: it holds a file
	// identical to one of the checked book's. SameISBN: it has one of the
	// ISBNs asked about (findDuplicatesISBN).
	SameAuthors bool `json:"same_authors"`
	SameFile    bool `json:"same_file"`
	SameISBN    bool `json:"same_isbn,omitempty"`
}

// duplicateCheck is what findDuplicates found (TS: DuplicateCheck).
type duplicateCheck struct {
	Books []duplicateBook `json:"books"`
}

func (c *duplicateCheck) found() bool { return c != nil && len(c.Books) > 0 }

// SameFile reports whether the library already holds one of the files:
// then no override adds the book.
func (c *duplicateCheck) SameFile() bool {
	if c == nil {
		return false
	}
	for _, b := range c.Books {
		if b.SameFile {
			return true
		}
	}
	return false
}

// blocks reports whether the check stops the book being added.
func (c *duplicateCheck) blocks(override bool) bool {
	return c.found() && (!override || c.SameFile())
}

// duplicateError is what adding a book that blocks answers.
type duplicateError struct{ check *duplicateCheck }

func (e *duplicateError) Error() string {
	b := e.check.Books[0]
	if e.check.SameFile() {
		for _, d := range e.check.Books {
			if d.SameFile {
				b = d
				break
			}
		}
		return fmt.Sprintf("the library already has this file: %q (%s)", b.Title, strings.Join(b.Authors, " & "))
	}
	return fmt.Sprintf("possibly already in the library: %q (%s)", b.Title, strings.Join(b.Authors, " & "))
}

// normName is a title or author name for comparing: case and runs of
// spaces don't count.
func normName(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func sameNames(a, b []string) bool {
	set := map[string]bool{}
	for _, n := range a {
		if n = normName(n); n != "" {
			set[n] = true
		}
	}
	seen := map[string]bool{}
	for _, n := range b {
		if n = normName(n); n != "" {
			if !set[n] {
				return false
			}
			seen[n] = true
		}
	}
	return len(seen) == len(set)
}

// findDuplicates looks in the library for books like one with this title,
// these authors and these files. The book with uuid exclude (itself, when it
// is in the library) is left out.
func findDuplicates(db *sql.DB, libDir, title string, authors []string, files []dupFile, exclude string) (*duplicateCheck, error) {
	return findDuplicatesISBN(db, libDir, title, authors, files, exclude, nil)
}

// findDuplicatesISBN is findDuplicates that also looks at the books with
// one of these ISBNs (any form: normalizeISBN), whatever their titles.
func findDuplicatesISBN(db *sql.DB, libDir, title string, authors []string, files []dupFile, exclude string, isbns []string) (*duplicateCheck, error) {
	check := &duplicateCheck{Books: []duplicateBook{}}
	title = strings.Join(strings.Fields(title), " ")
	if db == nil {
		return check, nil
	}
	type candidate struct {
		id                int64
		uuid, title, path string
		sameTitle         bool
		sameISBN          bool
	}
	var cands []*candidate
	byID := map[int64]*candidate{}
	query := func(sameTitle bool, q string, args ...any) error {
		rows, err := db.Query(q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.id, &c.uuid, &c.title, &c.path); err != nil {
				return err
			}
			p := byID[c.id]
			if p == nil {
				p = &c
				byID[c.id] = p
				cands = append(cands, p)
			}
			if sameTitle {
				p.sameTitle = true
			} else {
				p.sameISBN = true
			}
		}
		return rows.Err()
	}
	if title != "" {
		// NOCASE: case aside (ASCII letters only, as Calibre's own title column).
		if err := query(true, `SELECT id, COALESCE(uuid, ''), title, path FROM books WHERE trim(title) = ? COLLATE NOCASE AND COALESCE(uuid, '') != ?`, title, exclude); err != nil {
			return nil, err
		}
	}
	if forms := isbnForms(isbns); len(forms) > 0 {
		if err := query(false, `SELECT b.id, COALESCE(b.uuid, ''), b.title, b.path FROM books b
			WHERE COALESCE(b.uuid, '') != ? AND b.id IN (SELECT book FROM identifiers WHERE lower(type) = 'isbn'
				AND upper(replace(replace(val, '-', ''), ' ', '')) IN (`+placeholders(len(forms))+`))`,
			append([]any{exclude}, toArgs(forms)...)...); err != nil {
			return nil, err
		}
	}

	for _, c := range cands {
		names, err := bookAuthorNames(db, c.id)
		if err != nil {
			return nil, err
		}
		d := duplicateBook{UUID: c.uuid, Title: c.title, Authors: names, SameAuthors: c.sameTitle && sameNames(names, authors), SameISBN: c.sameISBN}
		if d.SameFile, err = holdsSameFile(db, libDir, c.id, c.path, files); err != nil {
			return nil, err
		}
		if d.SameAuthors || d.SameFile || d.SameISBN {
			check.Books = append(check.Books, d)
		}
	}
	return check, nil
}

// isbnForms are the ways the ISBNs may be written in metadata.db's
// identifiers, without separators: ISBN-13, and ISBN-10 where there is one.
func isbnForms(isbns []string) []string {
	var out []string
	for _, s := range isbns {
		n := normalizeISBN(s)
		if n == "" {
			continue
		}
		out = append(out, n)
		if strings.HasPrefix(n, "978") {
			core := n[3:12]
			sum := 0
			for i := 0; i < 9; i++ {
				sum += int(core[i]-'0') * (10 - i)
			}
			check := (11 - sum%11) % 11
			if check == 10 {
				out = append(out, core+"X")
			} else {
				out = append(out, core+strconv.Itoa(check))
			}
		}
	}
	return uniqueStrings(out)
}

func bookAuthorNames(db *sql.DB, bookID int64) ([]string, error) {
	rows, err := db.Query(`SELECT a.name FROM books_authors_link l JOIN authors a ON a.id = l.author WHERE l.book = ? ORDER BY l.id`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// holdsSameFile reports whether the library book holds a file identical to
// one of files. Only files of the same format and size are hashed.
func holdsSameFile(db *sql.DB, libDir string, bookID int64, bookPath string, files []dupFile) (bool, error) {
	if len(files) == 0 {
		return false, nil
	}
	lib, err := bookFiles(db, libDir, bookID, bookPath)
	if err != nil {
		return false, err
	}
	for _, l := range lib {
		li, err := os.Stat(l.Path)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.Format != l.Format {
				continue
			}
			fi, err := os.Stat(f.Path)
			if err != nil || fi.Size() != li.Size() {
				continue
			}
			a, err := fileSHA256(f.Path, fi)
			if err != nil {
				return false, err
			}
			b, err := fileSHA256(l.Path, li)
			if err != nil {
				return false, err
			}
			if a == b {
				return true, nil
			}
		}
	}
	return false, nil
}

// bookFiles is a library book's files, one per format in metadata.db. A
// file isn't always named as data.name says (books added before that was
// kept right), so when that name is missing, any file in the book's folder
// with the format's extension stands in.
func bookFiles(db *sql.DB, libDir string, bookID int64, bookPath string) ([]dupFile, error) {
	rows, err := db.Query(`SELECT format, name FROM data WHERE book = ?`, bookID)
	if err != nil {
		return nil, err
	}
	type row struct{ format, name string }
	var data []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.format, &r.name); err != nil {
			rows.Close()
			return nil, err
		}
		data = append(data, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(libDir, bookPath)
	var entries []os.DirEntry
	listed := false
	var out []dupFile
	for _, r := range data {
		format := strings.ToLower(r.format)
		p := filepath.Join(dir, r.name+"."+format)
		if _, err := os.Stat(p); err == nil {
			out = append(out, dupFile{p, format})
			continue
		}
		if !listed {
			entries, _ = os.ReadDir(dir)
			listed = true
		}
		for _, e := range entries {
			if e.Type().IsRegular() && strings.EqualFold(filepath.Ext(e.Name()), "."+format) {
				out = append(out, dupFile{filepath.Join(dir, e.Name()), format})
			}
		}
	}
	return out, nil
}

// fileHashes remembers files' hashes by path, size and modification time,
// so the import queue's list doesn't read its books (or the library's) on
// every refresh.
var fileHashes struct {
	sync.Mutex
	m map[string]string
}

const maxFileHashes = 4096

func fileSHA256(p string, info os.FileInfo) (string, error) {
	key := fmt.Sprintf("%s\x00%d\x00%d", p, info.Size(), info.ModTime().UnixNano())
	fileHashes.Lock()
	h, ok := fileHashes.m[key]
	fileHashes.Unlock()
	if ok {
		return h, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	h = hex.EncodeToString(sum.Sum(nil))
	fileHashes.Lock()
	if fileHashes.m == nil || len(fileHashes.m) >= maxFileHashes {
		fileHashes.m = map[string]string{}
	}
	fileHashes.m[key] = h
	fileHashes.Unlock()
	return h, nil
}

// libraryBookDuplicates is findDuplicates for a book in the library: other
// books like it, for the metadata editor's warning.
func libraryBookDuplicates(db *sql.DB, libDir, uuid string) (*duplicateCheck, error) {
	var id int64
	var title, path string
	if err := db.QueryRow(`SELECT id, title, path FROM books WHERE uuid = ?`, uuid).Scan(&id, &title, &path); err != nil {
		return nil, err
	}
	authors, err := bookAuthorNames(db, id)
	if err != nil {
		return nil, err
	}
	files, err := bookFiles(db, libDir, id, path)
	if err != nil {
		return nil, err
	}
	return findDuplicates(db, libDir, title, authors, files, uuid)
}

// importDuplicates is findDuplicates for a waiting book as its files say it
// is, kept for a while: the queue's list asks every few seconds.
func (b *importInbox) duplicates(it *pendingImport) *duplicateCheck {
	const keep = 30 * time.Second
	b.dupMu.Lock()
	c, ok := b.dups[it.ID]
	b.dupMu.Unlock()
	if ok && time.Since(c.at) < keep {
		return c.check
	}
	check, err := findDuplicates(calibreDB, baseDir, it.Merged.Title, it.Merged.Creators, b.dupFiles(it), "")
	if err != nil {
		logErr(err, "imports: duplicate check for "+it.ID)
		return nil
	}
	b.dupMu.Lock()
	if b.dups == nil {
		b.dups = map[string]cachedDuplicates{}
	}
	b.dups[it.ID] = cachedDuplicates{check, time.Now()}
	b.dupMu.Unlock()
	return check
}

type cachedDuplicates struct {
	check *duplicateCheck
	at    time.Time
}

func (b *importInbox) dupFiles(it *pendingImport) []dupFile {
	files := make([]dupFile, 0, len(it.Files))
	for _, f := range it.Files {
		files = append(files, dupFile{filepath.Join(b.itemDir(it.ID), f.Name), f.Format})
	}
	return files
}
