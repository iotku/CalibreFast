package main

// Repairing data.name for books added by older versions of CalibreFast.
//
// Calibre finds a format's file as <books.path>/<data.name>.<format>.
// Uploads and imports used to record the uploaded file's own name there
// while saving the file under the book's title, so Calibre (and its "Check
// library") couldn't find it. At startup, each format whose file isn't where
// data.name says is pointed at the file that is there: the only file in the
// book's folder with that format's extension. Nothing is renamed or moved on
// disk, and a format with no such file, or more than one, is left alone.

import (
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// misnamedFormat is a data row whose file is at another name.
type misnamedFormat struct {
	id       int64  // data.id
	book     string // books.path, for the log
	format   string // "EPUB"
	from, to string // data.name now, and the name of the file that is there
}

// findMisnamedFormats lists the formats in metadata.db whose file isn't at
// <path>/<name>.<format> but can be told for sure: exactly one file in the
// book's folder has the format's extension.
func findMisnamedFormats(db *sql.DB, libDir string) ([]misnamedFormat, error) {
	rows, err := db.Query(`SELECT d.id, b.path, d.format, d.name FROM data d JOIN books b ON b.id = d.book`)
	if err != nil {
		return nil, err
	}
	type row struct {
		id                 int64
		path, format, name string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.path, &r.format, &r.name); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []misnamedFormat
	for _, r := range all {
		ext := "." + strings.ToLower(r.format)
		dir := filepath.Join(libDir, r.path)
		if _, err := os.Stat(filepath.Join(dir, r.name+ext)); err == nil {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // no folder: nothing to point at
		}
		var found []string
		for _, e := range entries {
			if e.Type().IsRegular() && strings.EqualFold(filepath.Ext(e.Name()), ext) {
				found = append(found, e.Name())
			}
		}
		if len(found) != 1 {
			continue // none, or can't tell which
		}
		// Calibre joins name and the lower-case format: the file's own
		// extension must be that too, or it still wouldn't be found.
		if filepath.Ext(found[0]) != ext {
			continue
		}
		out = append(out, misnamedFormat{r.id, r.path, r.format, r.name, strings.TrimSuffix(found[0], ext)})
	}
	return out, nil
}

// repairFormatNames points misnamed formats at their files (with
// -allow-writes), or says how many there are (without). Returns how many it
// repaired.
func repairFormatNames(db *sql.DB, libDir string, write bool) int {
	bad, err := findMisnamedFormats(db, libDir)
	if err != nil {
		log.Println("checking format file names:", err)
		return 0
	}
	if len(bad) == 0 {
		return 0
	}
	if !write {
		log.Printf("%d book file(s) aren't named as metadata.db says, so Calibre can't find them; start with -allow-writes to repair metadata.db", len(bad))
		return 0
	}
	tx, err := db.Begin()
	if err != nil {
		log.Println("repairing format file names:", err)
		return 0
	}
	for _, m := range bad {
		if _, err := tx.Exec(`UPDATE data SET name = ? WHERE id = ? AND name = ?`, m.to, m.id, m.from); err != nil {
			tx.Rollback()
			log.Println("repairing format file names:", err)
			return 0
		}
	}
	if err := tx.Commit(); err != nil {
		log.Println("repairing format file names:", err)
		return 0
	}
	for _, m := range bad {
		log.Printf("metadata.db: %s %s is %q, not %q: repaired", m.book, m.format, m.to, m.from)
	}
	return len(bad)
}

// ---------------------------------------------------------------------------
// Books whose files metadata.db doesn't list
// ---------------------------------------------------------------------------
//
// A book with no data rows still opens in CalibreFast, which finds its files
// in its folder (resolveFormats), but Calibre lists no formats for it. The
// bulk editor shows such books (FormatsFromFolder) and offers to add the
// rows: one per format that exactly one book file in the folder has, named
// as that file is, so Calibre finds it. Nothing on disk changes.

// unlistedFormat is a data row to add.
type unlistedFormat struct {
	format string // "EPUB"
	name   string // the file's name without its extension
	size   int64
}

// unlistedBook is a book with no data rows, and what its folder holds.
type unlistedBook struct {
	id                int64
	uuid, title, path string
	add               []unlistedFormat
	ambiguous         []string // formats with more than one file: left alone
}

// findUnlistedFormats lists the books with no data rows whose folders hold
// book files: those with these uuids, or every one (nil).
func findUnlistedFormats(db *sql.DB, libDir string, uuids []string) ([]unlistedBook, error) {
	var wanted map[string]bool // nil: every book
	if uuids != nil {
		wanted = make(map[string]bool, len(uuids))
		for _, u := range uuids {
			wanted[u] = true
		}
	}
	rows, err := db.Query(`SELECT b.id, b.uuid, b.title, b.path FROM books b WHERE NOT EXISTS (SELECT 1 FROM data d WHERE d.book = b.id)`)
	if err != nil {
		return nil, err
	}
	var books []unlistedBook
	for rows.Next() {
		var b unlistedBook
		if err := rows.Scan(&b.id, &b.uuid, &b.title, &b.path); err != nil {
			rows.Close()
			return nil, err
		}
		if wanted == nil || wanted[b.uuid] {
			books = append(books, b)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []unlistedBook
	for _, b := range books {
		if _, err := libraryBookDir(b.path); err != nil {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(libDir, b.path))
		if err != nil {
			continue
		}
		byFormat := map[string][]os.DirEntry{}
		var order []string
		for _, e := range entries {
			if !e.Type().IsRegular() || !hasBookExt(strings.ToLower(e.Name())) {
				continue
			}
			f := strings.ToUpper(strings.TrimPrefix(filepath.Ext(e.Name()), "."))
			if _, ok := byFormat[f]; !ok {
				order = append(order, f)
			}
			byFormat[f] = append(byFormat[f], e)
		}
		for _, f := range order {
			files := byFormat[f]
			if len(files) != 1 {
				b.ambiguous = append(b.ambiguous, f)
				continue
			}
			info, err := files[0].Info()
			if err != nil {
				continue
			}
			name := files[0].Name()
			b.add = append(b.add, unlistedFormat{f, strings.TrimSuffix(name, filepath.Ext(name)), info.Size()})
		}
		if len(b.add) > 0 || len(b.ambiguous) > 0 {
			out = append(out, b)
		}
	}
	return out, nil
}

// unlistedRepair is what adding the rows did (TS: FormatRepair in web/types.ts).
type unlistedRepair struct {
	Books   int           `json:"books"`   // books given rows
	Formats int           `json:"formats"` // rows added
	Skipped []bulkFailure `json:"skipped"` // formats with several files, for the admin to sort out
}

// addUnlistedFormats gives books with no data rows a row for each format
// their folder holds one file of.
func addUnlistedFormats(db *sql.DB, libDir string, uuids []string) (unlistedRepair, error) {
	res := unlistedRepair{Skipped: []bulkFailure{}}
	books, err := findUnlistedFormats(db, libDir, uuids)
	if err != nil {
		return res, err
	}
	tx, err := db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	for _, b := range books {
		for _, f := range b.ambiguous {
			res.Skipped = append(res.Skipped, bulkFailure{b.uuid, b.title + ": several " + f + " files in its folder; add it in the metadata editor or Calibre"})
		}
		if len(b.add) == 0 {
			continue
		}
		for _, f := range b.add {
			// The book has no rows: none can be there already, unless added meanwhile.
			if _, err := tx.Exec(`INSERT OR IGNORE INTO data (book, format, uncompressed_size, name) VALUES (?, ?, ?, ?)`, b.id, f.format, f.size, f.name); err != nil {
				return unlistedRepair{Skipped: []bulkFailure{}}, err
			}
			res.Formats++
		}
		res.Books++
	}
	if err := tx.Commit(); err != nil {
		return unlistedRepair{Skipped: []bulkFailure{}}, err
	}
	for _, b := range books {
		if len(b.add) > 0 {
			formatCache.Delete(b.uuid)
			log.Printf("metadata.db: %s: added %d format row(s) for files in its folder", b.path, len(b.add))
		}
	}
	return res, nil
}
