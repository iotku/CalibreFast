package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// snapshotFixture: catalogFixture with ./pages in a temp dir and snapshot
// tracking reset afterwards.
func snapshotFixture(t *testing.T) *sql.DB {
	t.Helper()
	db := catalogFixture(t)
	t.Chdir(t.TempDir())
	t.Cleanup(func() {
		snapshots.Lock()
		if snapshots.timer != nil {
			snapshots.timer.Stop()
		}
		snapshots.enabled, snapshots.stale, snapshots.timer = false, false, nil
		snapshots.Unlock()
	})
	return db
}

func dbPath() string { return filepath.Join(baseDir, "metadata.db") }

// A restart with metadata.db as it was takes the catalog from the snapshot
// and doesn't read the library.
func TestCatalogSnapshotOnUnchangedLibrary(t *testing.T) {
	db := snapshotFixture(t)
	addTestBook(t, db, "Dune", []string{"Frank Herbert"}, []string{"SF"}, "Dune", "epub")
	addTestBook(t, db, "Emma", []string{"Jane Austen"}, nil, "")

	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	first := currentCatalog()
	if _, err := os.Stat(catalogSnapshotPath()); err != nil {
		t.Fatalf("no snapshot after the first start: %v", err)
	}

	builds := catalogBuilds.Load()
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	if catalogBuilds.Load() != builds {
		t.Error("the second start read the library again")
	}
	second := currentCatalog()
	if !reflect.DeepEqual(first.books, second.books) || !reflect.DeepEqual(first.tagNames, second.tagNames) ||
		!reflect.DeepEqual(first.seriesNames, second.seriesNames) || !reflect.DeepEqual(first.authorNames, second.authorNames) {
		t.Error("the snapshot's catalog differs from the one read")
	}
	checkCatalogMatchesDB(t, db, second)
	if n := totalBooks.Load(); n != 2 {
		t.Errorf("totalBooks %d", n)
	}
}

// Any change to metadata.db makes the snapshot stale, even one that keeps
// its size and (as on filesystems with whole-second times) its
// modification time: the header's change counter differs.
func TestCatalogSnapshotStaleAfterChange(t *testing.T) {
	db := snapshotFixture(t)
	uuid := addTestBook(t, db, "Dune", []string{"Frank Herbert"}, nil, "")
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dbPath())
	if err != nil {
		t.Fatal(err)
	}

	mustExec(t, db, `UPDATE books SET title = 'Dunk' WHERE uuid = ?`, uuid) // same length
	if err := os.Chtimes(dbPath(), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(dbPath()); after.Size() != info.Size() {
		t.Logf("size changed too (%d → %d)", info.Size(), after.Size())
	}

	builds := catalogBuilds.Load()
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	if catalogBuilds.Load() != builds+1 {
		t.Fatal("a changed library was taken from the snapshot")
	}
	if b, _ := currentCatalog().lookup(uuid); b.Title != "Dunk" {
		t.Errorf("title %q", b.Title)
	}
}

// A rollback journal beside metadata.db (a write in progress, or one SQLite
// will roll back) means the file isn't settled: read it.
func TestCatalogSnapshotNotUsedWithJournal(t *testing.T) {
	db := snapshotFixture(t)
	addTestBook(t, db, "Dune", []string{"Frank Herbert"}, nil, "")
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := currentLibraryKey(); err != nil || !ok {
		t.Fatalf("key without a journal: ok=%t %v", ok, err)
	}
	if err := os.WriteFile(dbPath()+"-journal", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dbPath() + "-journal")
	if _, ok, _ := currentLibraryKey(); ok {
		t.Error("a key was trusted with a journal present")
	}
}

// While running: CalibreFast's own writes keep the snapshot current; a
// commit by anything else removes it, and none is saved again this run.
func TestCatalogSnapshotTracksWriters(t *testing.T) {
	db := snapshotFixture(t)
	db.SetMaxOpenConns(1) // as initCalibreDB: data_version is per connection
	uuid := addTestBook(t, db, "Dune", []string{"Frank Herbert"}, nil, "")
	if err := trackLibraryChanges(); err != nil {
		t.Fatal(err)
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}

	// Our own edit, told to the catalog, then saved.
	mustExec(t, db, `UPDATE books SET title = 'Dune Messiah' WHERE uuid = ?`, uuid)
	catalogChanged(uuid)
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	if err := saveCatalogSnapshot(); err != nil {
		t.Fatal(err)
	}
	key, _, _ := currentLibraryKey()
	if c, ok := loadCatalogSnapshot(key); !ok {
		t.Fatal("no current snapshot after our own edit")
	} else if b, _ := c.lookup(uuid); b.Title != "Dune Messiah" {
		t.Errorf("snapshot title %q", b.Title)
	}

	// Someone else (another connection: Calibre desktop, say).
	other, err := sql.Open("sqlite3", dbPath())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	time.Sleep(10 * time.Millisecond)
	if _, err := other.Exec(`UPDATE books SET title = 'Children of Dune' WHERE uuid = ?`, uuid); err != nil {
		t.Fatal(err)
	}
	if err := saveCatalogSnapshot(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(catalogSnapshotPath()); !os.IsNotExist(err) {
		t.Errorf("snapshot kept after an outside change: %v", err)
	}
	// Our next edit doesn't bring it back this run.
	if err := saveCatalogSnapshot(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(catalogSnapshotPath()); !os.IsNotExist(err) {
		t.Error("a snapshot was saved again after an outside change")
	}
}
