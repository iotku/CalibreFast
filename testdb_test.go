package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/iotku/CalibreFast/internal/hardcoversync"
)

// The fully migrated app DB is built once per test binary and kept in memory;
// each test writes a copy into its own temp dir, so openAppDB's migrate() finds
// the schema current and does nothing.
var (
	appDBTemplateOnce sync.Once
	appDBTemplate     []byte
	appDBTemplateErr  error
)

func buildAppDBTemplate() ([]byte, error) {
	dir, err := os.MkdirTemp("", "calibrefast-appdb-template-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	db, err := openAppDB(dir)
	if err != nil {
		return nil, err
	}
	// Fold the WAL into the main file so the single file is the whole DB.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		db.Close()
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, appDBFile))
}

// openTestAppDB opens a fresh, fully migrated app DB in a per-test temp dir.
// The caller owns the handle; it is not installed as the global appDB.
func openTestAppDB(t *testing.T) *sql.DB {
	t.Helper()
	appDBTemplateOnce.Do(func() { appDBTemplate, appDBTemplateErr = buildAppDBTemplate() })
	if appDBTemplateErr != nil {
		t.Fatalf("build app db template: %v", appDBTemplateErr)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, appDBFile), appDBTemplate, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := openAppDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// setupTestAppDB installs a fresh app DB as the global appDB for one test,
// with Hardcover reading sync (off, as on a server without a client id)
// series links and author lookups on it, restoring the previous ones when the test ends.
func setupTestAppDB(t *testing.T) {
	t.Helper()
	db := openTestAppDB(t)
	orig, origSync, origLinks, origAuthors := appDB, hcSync, hcLinks, hcAuthors
	appDB = db
	hcAuthors = newHCAuthors()
	hcSync, hcLinks = newHCSync(hardcoversync.Config{}), newHCLinks(hcAuthors)
	t.Cleanup(func() { db.Close(); appDB, hcSync, hcLinks, hcAuthors = orig, origSync, origLinks, origAuthors })
	resetKosyncCaches()
}

func schemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// A brand-new DB runs every migration and records the latest version.
func TestMigrationsApplyFromScratch(t *testing.T) {
	db, err := openAppDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v := schemaVersion(t, db); v != len(migrations) {
		t.Fatalf("schema version %d, want %d", v, len(migrations))
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != len(migrations) {
		t.Fatalf("%d schema_version rows, want one per migration (%d)", rows, len(migrations))
	}
}

// A DB left at any older version upgrades to the latest one, and the copied
// template reports the same version (so its migrate() is a no-op).
func TestMigrationsUpgradeFromOlderVersion(t *testing.T) {
	for _, from := range []int{1, len(migrations) / 2, len(migrations) - 1} {
		dir := t.TempDir()
		db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, appDBFile)+"?_foreign_keys=on")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		for i, m := range migrations[:from] {
			if _, err := db.Exec(m); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
			if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
				t.Fatal(err)
			}
		}
		db.Close()

		db, err = openAppDB(dir)
		if err != nil {
			t.Fatalf("upgrade from %d: %v", from, err)
		}
		if v := schemaVersion(t, db); v != len(migrations) {
			t.Errorf("upgrade from %d: version %d, want %d", from, v, len(migrations))
		}
		db.Close()
	}
	tmpl := openTestAppDB(t)
	defer tmpl.Close()
	if v := schemaVersion(t, tmpl); v != len(migrations) {
		t.Errorf("template copy: version %d, want %d", v, len(migrations))
	}
}
