package main

// Matching KOReader's document ids to books in the library.
//
// KOReader names a book by one of two ids (its "document matching method"):
//   - binary (the default): a partial MD5 of the file's contents, from
//     twelve 1 KiB samples. A file downloaded from CalibreFast matches; a copy
//     Calibre changed while sending it to a device does not.
//   - filename: the MD5 of the file's name. Calibre usually keeps a book's
//     file name when it sends it, so this also matches those copies.
//
// Partial MD5s are worked out lazily: when a book is opened in the web reader
// or downloaded, and, when a device asks about a book nothing matches, for
// the books the user has read on the web. They are cached in the app
// database with the file's size and modification time, so a changed file is
// hashed again. File-name ids come from Calibre's database, without reading
// any files.

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iotku/CalibreFast/pkg/kosync"
	"github.com/iotku/CalibreFast/pkg/logsafe"
)

// bookFile is one format of a book in the library.
type bookFile struct {
	UUID   string
	Format string // lower-case extension: "epub", "pdf", …
}

// bookFilePath is the file of a book's format on disk.
func bookFilePath(uuid, format string) (string, error) {
	dir, err := resolveBookPath(uuid)
	if err != nil {
		return "", err
	}
	for _, name := range resolveFormats(uuid, baseDir, dir) {
		if strings.EqualFold(strings.TrimPrefix(filepath.Ext(name), "."), format) {
			return filepath.Join(baseDir, dir, name), nil
		}
	}
	return "", os.ErrNotExist
}

// bookDigest returns the partial MD5 of a book's format file: from the cache
// when the file's size and modification time haven't changed, otherwise read
// from the file and cached.
func bookDigest(uuid, format string) (string, error) {
	path, err := bookFilePath(uuid, format)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(path)
	if err != nil {
		_, _ = appDB.Exec(`DELETE FROM book_digests WHERE book_uuid = ? AND format = ?`, uuid, format)
		return "", err
	}
	var digest string
	var size, mtime int64
	err = appDB.QueryRow(`SELECT digest, size, mtime FROM book_digests WHERE book_uuid = ? AND format = ?`,
		uuid, format).Scan(&digest, &size, &mtime)
	if err == nil && size == fi.Size() && mtime == fi.ModTime().UnixNano() {
		return digest, nil
	}
	if digest, err = kosync.PartialMD5File(path); err != nil {
		return "", err
	}
	_, err = appDB.Exec(`INSERT INTO book_digests (book_uuid, format, digest, size, mtime) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(book_uuid, format) DO UPDATE SET digest = excluded.digest, size = excluded.size, mtime = excluded.mtime`,
		uuid, format, digest, fi.Size(), fi.ModTime().UnixNano())
	return digest, err
}

// bookDocumentIDs are the ids a KOReader device may use for a book's format:
// its partial MD5 and the MD5 of its file name.
func bookDocumentIDs(uuid, format string) []string {
	var ids []string
	if d, err := bookDigest(uuid, format); err == nil {
		ids = append(ids, d)
	}
	if path, err := bookFilePath(uuid, format); err == nil {
		ids = append(ids, kosync.FileNameDigest(filepath.Base(path)))
	}
	return ids
}

// noteDownload hashes a format file once it has been downloaded (a 12 KiB
// read, cached), so a device that opens the download is recognised before
// the book is read on the web.
func noteDownload(uuid, format string) {
	if appDB == nil {
		return
	}
	if _, err := bookDigest(uuid, strings.ToLower(format)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("kosync: hash %s/%s: %v", logsafe.Text(uuid), logsafe.Text(format), logsafe.Text(err))
	}
}

// ---------------------------------------------------------------------------
// File-name ids for the whole library, from Calibre's database
// ---------------------------------------------------------------------------

type nameIndex struct {
	mu    sync.Mutex
	built time.Time
	ids   map[string]bookFile // md5(file name) → book; {} when ambiguous
	// rebuilding: a background rebuild is under way (lookup starts one).
	rebuilding atomic.Bool
}

var libraryNames = &nameIndex{}

const nameIndexMaxAge = time.Minute

// lookup finds the book whose file name hashes to id. Building the index
// reads every format row in metadata.db (tens of ms in a big library), so
// only the first lookup waits for it. After that an index over a minute old
// still answers, and is rebuilt in the background so new books are found.
func (n *nameIndex) lookup(id string) (bookFile, bool) {
	n.mu.Lock()
	if n.ids == nil {
		n.rebuild()
	} else if time.Since(n.built) > nameIndexMaxAge && n.rebuilding.CompareAndSwap(false, true) {
		go func() {
			defer n.rebuilding.Store(false)
			ids, err := buildNameIndex()
			if err != nil {
				log.Println("kosync: file name index:", err)
				return
			}
			n.mu.Lock()
			n.ids, n.built = ids, time.Now()
			n.mu.Unlock()
		}()
	}
	b, ok := n.ids[id]
	n.mu.Unlock()
	return b, ok && b.UUID != ""
}

// rebuild builds the index now; n.mu is held.
func (n *nameIndex) rebuild() {
	ids, err := buildNameIndex()
	if err != nil {
		log.Println("kosync: file name index:", err)
		return
	}
	n.ids, n.built = ids, time.Now()
}

func buildNameIndex() (map[string]bookFile, error) {
	if calibreDB == nil {
		return nil, errors.New("no library")
	}
	rows, err := calibreDB.Query(`SELECT b.uuid, d.format, d.name FROM data d JOIN books b ON b.id = d.book`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]bookFile{}
	for rows.Next() {
		var uuid, format, name string
		if rows.Scan(&uuid, &format, &name) != nil {
			continue
		}
		format = strings.ToLower(format)
		id := kosync.FileNameDigest(name + "." + format)
		if prev, dup := ids[id]; dup && prev.UUID != uuid {
			ids[id] = bookFile{} // two books with the same file name: match neither
			continue
		}
		ids[id] = bookFile{UUID: uuid, Format: format}
	}
	return ids, rows.Err()
}

// ---------------------------------------------------------------------------
// Resolving a device's document id
// ---------------------------------------------------------------------------

// webScans records when each user's web-read books were last all hashed.
var webScans = struct {
	sync.Mutex
	done map[int64]time.Time
}{done: map[int64]time.Time{}}

const (
	webScanInterval = 10 * time.Minute
	webScanBudget   = time.Second // per request: a device gives up after a few seconds
)

// resolveDocument finds the book format a device's document id refers to.
func resolveDocument(userID int64, id string) (bookFile, bool) {
	if b, ok := digestMatch(id); ok {
		return b, true
	}
	if b, ok := libraryNames.lookup(id); ok {
		return b, true
	}
	// A book read on the web before its file was hashed (before this feature
	// existed, say) is only found by hashing it. Only books the user read on
	// the web have a web position to offer, so hash those: for up to a
	// second per request, until all are done, then at most every 10 minutes.
	webScans.Lock()
	due := time.Since(webScans.done[userID]) > webScanInterval
	webScans.Unlock()
	if !due {
		return bookFile{}, false
	}
	if hashWebReadBooks(userID, webScanBudget) {
		webScans.Lock()
		webScans.done[userID] = time.Now()
		webScans.Unlock()
	}
	return digestMatch(id)
}

// hashWebReadBooks hashes the formats the user has read on the web that
// haven't been hashed yet. It reports whether it got through all of them
// within the budget.
func hashWebReadBooks(userID int64, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	rows, err := appDB.Query(`SELECT p.book_uuid, p.format FROM reading_progress p
		LEFT JOIN book_digests d ON d.book_uuid = p.book_uuid AND d.format = p.format
		WHERE p.user_id = ? AND d.digest IS NULL`, userID)
	if err != nil {
		return false
	}
	var todo []bookFile
	for rows.Next() {
		var b bookFile
		if rows.Scan(&b.UUID, &b.Format) == nil {
			todo = append(todo, b)
		}
	}
	rows.Close() // one connection: finish reading before hashing writes
	for _, b := range todo {
		if time.Now().After(deadline) {
			return false
		}
		_, _ = bookDigest(b.UUID, b.Format)
	}
	return true
}

// digestMatch looks id up among cached partial MD5s, checking the file
// hasn't changed since.
func digestMatch(id string) (bookFile, bool) {
	rows, err := appDB.Query(`SELECT book_uuid, format FROM book_digests WHERE digest = ?`, id)
	if err != nil {
		return bookFile{}, false
	}
	var found []bookFile
	for rows.Next() {
		var b bookFile
		if rows.Scan(&b.UUID, &b.Format) == nil {
			found = append(found, b)
		}
	}
	rows.Close()
	for _, b := range found {
		if d, err := bookDigest(b.UUID, b.Format); err == nil && d == id {
			return b, true
		}
	}
	return bookFile{}, false
}
