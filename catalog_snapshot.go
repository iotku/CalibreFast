package main

// The catalog's snapshot on fast local storage (<cachedir>/catalog.snap), so
// a restart with an unchanged library doesn't read it all again: one stat
// and a 28-byte header read of metadata.db instead of a pass over every table, on storage that may be
// slow, networked or asleep (docs/notes/Discover.md, "Storage model").
//
// A snapshot is only trusted when it describes exactly the metadata.db on
// disk:
//   - Its key is metadata.db's (and any WAL's) size, modification time and
//     header change counter, taken before the reads it holds, so a change during them makes
//     the key stale rather than the snapshot.
//   - After that, only changes CalibreFast makes and tells the catalog about
//     are folded in. A commit by anything else (Calibre desktop,
//     calibre-server) shows as a new PRAGMA data_version on calibreDB; the
//     snapshot is then deleted and not written again this run, so the next
//     start reads the library.

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// catalogSnapshotVersion changes whenever catalogBook or the snapshot's
// layout does: an older snapshot is then ignored.
const catalogSnapshotVersion = 1

type libraryKey struct {
	Library     string // -basedir, absolute
	DBSize      int64
	DBModified  int64 // UnixNano
	WALSize     int64 // -1: none
	WALModified int64
	// SQLite's file change counter (header offset 24), bumped by every
	// commit: catches a change the modification time's resolution would
	// hide (some network and FAT filesystems keep only whole seconds).
	ChangeCounter uint32
}

type catalogSnapshot struct {
	Version     int
	Key         libraryKey
	Books       []catalogBook
	TagNames    map[int32]string
	AuthorNames map[int32]string
	SeriesNames map[int32]string
}

func catalogSnapshotPath() string { return filepath.Join(cacheDir, "catalog.snap") }

// currentLibraryKey stats metadata.db and reads its header's change counter. ok is false when it can't be trusted
// as a key: a rollback journal is there (a write is in progress, or one was
// interrupted and SQLite will roll it back).
func currentLibraryKey() (key libraryKey, ok bool, err error) {
	db := filepath.Join(baseDir, "metadata.db")
	info, err := libraryStat(db)
	if err != nil {
		return key, false, err
	}
	abs, _ := filepath.Abs(baseDir)
	key = libraryKey{Library: abs, DBSize: info.Size(), DBModified: info.ModTime().UnixNano(), WALSize: -1}
	if _, err := libraryStat(db + "-journal"); err == nil {
		return key, false, nil
	}
	if wal, err := libraryStat(db + "-wal"); err == nil {
		key.WALSize, key.WALModified = wal.Size(), wal.ModTime().UnixNano()
	}
	f, err := libraryOpen(db)
	if err != nil {
		return key, false, err
	}
	defer f.Close()
	var header [28]byte
	if _, err := f.ReadAt(header[:], 0); err != nil {
		return key, false, err
	}
	key.ChangeCounter = binary.BigEndian.Uint32(header[24:28])
	return key, true, nil
}

// loadCatalogSnapshot returns the saved catalog if its key is key.
func loadCatalogSnapshot(key libraryKey) (*catalog, bool) {
	if cacheDir == "" {
		return nil, false
	}
	data, err := os.ReadFile(catalogSnapshotPath())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Println("catalog snapshot:", err)
		}
		return nil, false
	}
	var s catalogSnapshot
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&s); err != nil {
		log.Println("catalog snapshot: unreadable, reading the library:", err)
		return nil, false
	}
	if s.Version != catalogSnapshotVersion || s.Key != key {
		return nil, false
	}
	c := &catalog{
		books:       s.Books,
		byUUID:      make(map[string]int32, len(s.Books)),
		tagNames:    nonNil(s.TagNames),
		authorNames: nonNil(s.AuthorNames),
		seriesNames: nonNil(s.SeriesNames),
	}
	for i := range c.books {
		c.byUUID[c.books[i].UUID] = int32(i)
	}
	c.shuffle = randomOrder(0, len(c.books)) // a new order every start
	return c, true
}

func nonNil(m map[int32]string) map[int32]string {
	if m == nil {
		return map[int32]string{}
	}
	return m
}

// writeCatalogSnapshot saves c under key: written beside and renamed, so a
// crash never leaves half a snapshot.
func writeCatalogSnapshot(c *catalog, key libraryKey) error {
	if cacheDir == "" {
		return nil
	}
	var buf bytes.Buffer
	err := gob.NewEncoder(&buf).Encode(catalogSnapshot{
		Version: catalogSnapshotVersion, Key: key, Books: c.books,
		TagNames: c.tagNames, AuthorNames: c.authorNames, SeriesNames: c.seriesNames,
	})
	if err != nil {
		return err
	}
	path := catalogSnapshotPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// Keeping it current while the server runs
// ---------------------------------------------------------------------------

var snapshots struct {
	sync.Mutex
	enabled  bool // set by main; tests save explicitly
	timer    *time.Timer
	baseline int64 // calibreDB's PRAGMA data_version when the catalog was read
	stale    bool  // something else wrote to metadata.db: don't save this run
}

const snapshotDelay = 5 * time.Second

// trackLibraryChanges notes calibreDB's data_version as the state the
// catalog was read from, and enables saving snapshots. Call it before the
// catalog is read.
func trackLibraryChanges() error {
	v, err := libraryDataVersion()
	if err != nil {
		return err
	}
	snapshots.Lock()
	defer snapshots.Unlock()
	snapshots.baseline, snapshots.stale, snapshots.enabled = v, false, true
	return nil
}

// libraryDataVersion: SQLite changes it on a connection when another
// connection or process commits to the database, not for the connection's
// own commits. calibreDB is one connection (SetMaxOpenConns(1)).
func libraryDataVersion() (int64, error) {
	var v int64
	err := calibreDB.QueryRow(`PRAGMA data_version`).Scan(&v)
	return v, err
}

// scheduleCatalogSnapshot saves the catalog a little after the last change.
func scheduleCatalogSnapshot() {
	snapshots.Lock()
	defer snapshots.Unlock()
	if !snapshots.enabled || snapshots.stale {
		return
	}
	if snapshots.timer != nil {
		snapshots.timer.Stop()
	}
	snapshots.timer = time.AfterFunc(snapshotDelay, func() { logErr(saveCatalogSnapshot(), "catalog snapshot") })
}

// saveCatalogSnapshot saves the current catalog, unless metadata.db was
// changed by something else since it was read: then the snapshot is
// removed instead, so the next start reads the library.
func saveCatalogSnapshot() error {
	catalogApplyMu.Lock() // no change half-applied while saving
	defer catalogApplyMu.Unlock()
	snapshots.Lock()
	defer snapshots.Unlock()
	if snapshots.stale {
		return nil
	}
	v, err := libraryDataVersion()
	if err != nil {
		return err
	}
	if v != snapshots.baseline {
		snapshots.stale = true
		log.Println("catalog: metadata.db was changed outside CalibreFast; the next start reads the library again")
		if err := os.Remove(catalogSnapshotPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	key, ok, err := currentLibraryKey()
	if err != nil {
		return err
	}
	if !ok {
		// A write in progress: try again once it's done.
		snapshots.timer = time.AfterFunc(snapshotDelay, func() { logErr(saveCatalogSnapshot(), "catalog snapshot") })
		return nil
	}
	c := libraryCatalog.Load()
	if c == nil {
		return fmt.Errorf("no catalog to save")
	}
	return writeCatalogSnapshot(c, key)
}

// loadOrBuildCatalog is the catalog to start with: the snapshot when its key
// matches metadata.db now, else a full read (saved as the new snapshot).
func loadOrBuildCatalog() (*catalog, error) {
	start := time.Now()
	key, keyOK, err := currentLibraryKey()
	if err != nil {
		return nil, err
	}
	if keyOK {
		if c, ok := loadCatalogSnapshot(key); ok {
			log.Printf("catalog: %d books from the snapshot in %s (metadata.db unchanged)", len(c.books), time.Since(start).Round(time.Millisecond))
			return c, nil
		}
	}
	// Only reads: read-only even with -allow-writes.
	db, err := sql.Open("sqlite3", calibreDSN(filepath.Join(baseDir, "metadata.db"), true))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	c, err := buildCatalog(db)
	if err != nil {
		return nil, err
	}
	if keyOK {
		logErr(writeCatalogSnapshot(c, key), "catalog snapshot")
	}
	return c, nil
}
