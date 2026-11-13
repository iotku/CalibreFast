package main

// Readers' own data about removed books: bookmarks, highlights and notes,
// reading positions, reading status, favorites and shelves. It lives in
// CalibreFast's database (calibrefast.db), keyed by book uuid, and removing
// a book leaves it there, so it's back when the book is. Each batch also
// keeps its own copy (reader-data.json), so the bin is a whole backup: a
// restore adds back any of those rows that are gone (a live row with the
// same key is newer and wins), and the server's owner has them for a
// rollback by hand.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const recycleReaderDataFile = "reader-data.json"

// readerDataTables are the app tables with a book_uuid column that belong
// to readers. book_digests (a cache of file hashes) isn't one.
var readerDataTables = []string{"bookmarks", "annotations", "reading_progress", "reading_status", "favorites", "shelf_books"}

// readerDataTable is one table's rows for a batch's books.
type readerDataTable struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// formatReaderTables are the reader tables whose rows are about one format
// of a book (a place, bookmark or highlight in its EPUB, say).
var formatReaderTables = []string{"bookmarks", "annotations", "reading_progress"}

// collectReaderRows reads the rows of tables that match where (on a
// table's own columns), and counts them by book uuid and table.
func collectReaderRows(tables []string, where string, args ...any) (map[string]*readerDataTable, map[string]map[string]int, error) {
	data := map[string]*readerDataTable{}
	counts := map[string]map[string]int{} // uuid -> table -> rows
	for _, table := range tables {
		rows, err := appDB.Query(`SELECT * FROM `+quoteIdent(table)+` WHERE `+where, args...)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", table, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		t := &readerDataTable{Columns: cols, Rows: [][]any{}}
		bi := slices.Index(cols, "book_uuid")
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, nil, err
			}
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					vals[i] = string(b)
				}
			}
			t.Rows = append(t.Rows, vals)
			u, _ := vals[bi].(string)
			if counts[u] == nil {
				counts[u] = map[string]int{}
			}
			counts[u][table]++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		data[table] = t
	}
	return data, counts, nil
}

func writeReaderRows(path string, data map[string]*readerDataTable) error {
	out, err := json.MarshalIndent(data, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// saveReaderData writes the batch's reader-data.json and notes how many
// rows of each kind each book has (recycledBook.ReaderData).
func saveReaderData(batch *recycleBatch) error {
	if appDB == nil {
		return nil
	}
	uuids := make([]any, len(batch.Books))
	for i, b := range batch.Books {
		uuids[i] = b.UUID
	}
	data, counts, err := collectReaderRows(readerDataTables, `book_uuid IN (`+placeholders(len(uuids))+`)`, uuids...)
	if err != nil {
		return err
	}
	if err := writeReaderRows(filepath.Join(batch.dir(), recycleReaderDataFile), data); err != nil {
		return err
	}
	for i := range batch.Books {
		batch.Books[i].ReaderData = counts[batch.Books[i].UUID]
	}
	return nil
}

// restoreReaderData adds back a restored book's rows from the batch's copy
// that calibrefast.db no longer has. Rows of users or shelves deleted since
// are left out.
func restoreReaderData(batch *recycleBatch, uuid string) (int, error) {
	return restoreReaderRows(filepath.Join(batch.dir(), recycleReaderDataFile), uuid, "")
}

// restoreReaderRows adds back the rows in the file (writeReaderRows) about
// the book uuid, only those in format when it isn't "", that calibrefast.db
// no longer has.
func restoreReaderRows(path, uuid, format string) (int, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) || appDB == nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var data map[string]*readerDataTable
	if err := dec.Decode(&data); err != nil {
		return 0, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	tx, err := appDB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	added := 0
	for _, table := range readerDataTables {
		t := data[table]
		if t == nil || len(t.Rows) == 0 {
			continue
		}
		bi := slices.Index(t.Columns, "book_uuid")
		if bi < 0 {
			continue
		}
		fi := slices.Index(t.Columns, "format")
		if format != "" && fi < 0 {
			continue
		}
		// What the row points to must still be there.
		owner, ownerTable := "user_id", "users"
		if table == "shelf_books" {
			owner, ownerTable = "shelf_id", "shelves"
		}
		oi := slices.Index(t.Columns, owner)
		if oi < 0 {
			continue
		}
		stmt := `INSERT OR IGNORE INTO ` + quoteIdent(table) + ` (` + columnList(t.Columns) + `) SELECT ` + placeholders(len(t.Columns)) +
			` WHERE EXISTS (SELECT 1 FROM ` + ownerTable + ` WHERE id = ?)`
		for _, row := range t.Rows {
			if len(row) != len(t.Columns) || row[bi] != uuid {
				continue
			}
			if format != "" {
				if f, _ := row[fi].(string); !strings.EqualFold(f, format) {
					continue
				}
			}
			args := make([]any, 0, len(row)+1)
			for _, v := range row {
				args = append(args, jsonToSQL(v))
			}
			args = append(args, jsonToSQL(row[oi]))
			res, err := tx.Exec(stmt, args...)
			if err != nil {
				return 0, fmt.Errorf("restore %s: %w", table, err)
			}
			n, _ := res.RowsAffected()
			added += int(n)
		}
	}
	return added, tx.Commit()
}

// jsonToSQL turns a decoded JSON value (numbers as json.Number) back into
// what was read from SQLite.
func jsonToSQL(v any) any {
	n, ok := v.(json.Number)
	if !ok {
		return v
	}
	if i, err := n.Int64(); err == nil {
		return i
	}
	if f, err := n.Float64(); err == nil {
		return f
	}
	return n.String()
}
