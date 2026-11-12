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
