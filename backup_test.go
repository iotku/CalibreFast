package main

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// useTestBackups points backups at a temporary folder.
func useTestBackups(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "backups")
	origDir, origKeep := backupDir, backupKeep
	backupDir, backupKeep = dir, 3
	t.Cleanup(func() { backupDir, backupKeep = origDir, origKeep })
	return dir
}

func resetSecretKeyCache() {
	secretKey.Lock()
	secretKey.key, secretKey.path = nil, ""
	secretKey.Unlock()
}

func openBackup(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestAppBackupAndKeys(t *testing.T) {
	setupTestAppDB(t)
	keyPath := useTestSecretKey(t)
	dir := useTestBackups(t)
	if _, err := createUser("alice", "longenough-pw", true); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)

	// Nothing sealed yet: no key to copy.
	name, err := makeBackup(backupApp, t0, false)
	if err != nil {
		t.Fatal(err)
	}
	if sc, _ := readSidecar(name); sc.Key != "" {
		t.Errorf("key %q with no secrets.key", sc.Key)
	}

	// A sealed value, and two backups with the same key: one copy of it.
	mustExec(t, appDB, `CREATE TABLE sealed (ctx TEXT, v TEXT)`)
	v1, err := sealSecret("test", "first secret")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, appDB, `INSERT INTO sealed VALUES ('test', ?)`, v1)
	b1, err := makeBackup(backupApp, t0.Add(time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := makeBackup(backupApp, t0.Add(2*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	keys, _ := os.ReadDir(keyCopyDir())
	if len(keys) != 1 {
		t.Fatalf("%d key copies, want 1", len(keys))
	}
	if filepath.Dir(keyCopyDir()) != filepath.Dir(keyPath) {
		t.Errorf("key copies in %s, not beside secrets.key", keyCopyDir())
	}
	sc1, _ := readSidecar(b1)
	if sc1.Key == "" || mode(t, keyBackupPath(sc1.Key)) != 0o600 || mode(t, filepath.Join(dir, b1)) != 0o600 {
		t.Errorf("key %q, modes key %v db %v", sc1.Key, mode(t, keyBackupPath(sc1.Key)), mode(t, filepath.Join(dir, b1)))
	}

	// The backup is a whole database: the same users.
	var n int
	if err := openBackup(t, filepath.Join(dir, b1)).QueryRow(`SELECT COUNT(*) FROM users WHERE username = 'alice'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("users in backup: %d %v", n, err)
	}

	// The key file is replaced (lost, then made again by the next seal).
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	resetSecretKeyCache()
	if _, err := sealSecret("test", "second secret"); err != nil {
		t.Fatal(err)
	}
	b3, err := makeBackup(backupApp, t0.Add(3*time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	sc3, _ := readSidecar(b3)
	if sc3.Key == "" || sc3.Key == sc1.Key {
		t.Fatalf("new key %q, old %q", sc3.Key, sc1.Key)
	}

	// The old backup's sealed value opens with the key its sidecar names.
	var sealed string
	if err := openBackup(t, filepath.Join(dir, b1)).QueryRow(`SELECT v FROM sealed`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	oldKey := keyBackupPath(sc1.Key)
	secretKeyPath = func() string { return oldKey }
	resetSecretKeyCache()
	if got, err := openSecret("test", sealed); err != nil || got != "first secret" {
		t.Errorf("restored secret %q %v", got, err)
	}
	secretKeyPath = func() string { return keyPath }
	resetSecretKeyCache()

	// Pruning: the newest 3 (backupKeep) stay, and the old key goes only once
	// no kept backup needs it.
	if _, err := makeBackup(backupApp, t0.Add(4*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	list, _ := listBackups()
	if len(list) != 3 || list[0].At != t0.Add(4*time.Hour) {
		t.Fatalf("kept %+v", list)
	}
	if _, err := os.Stat(keyBackupPath(sc1.Key)); err != nil {
		t.Error("old key removed while a kept backup needs it")
	}
	for i := 5; i <= 7; i++ {
		if _, err := makeBackup(backupApp, t0.Add(time.Duration(i)*time.Hour), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(keyBackupPath(sc1.Key)); err == nil {
		t.Error("old key kept after its backups were pruned")
	}
	if _, err := os.Stat(keyBackupPath(sc3.Key)); err != nil {
		t.Error("current key removed")
	}
	// Nothing half-made, and no key anywhere in -backup-dir: a backup and
	// its key are kept apart (docs/SECURITY.md).
	key, _ := os.ReadFile(keyPath)
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(p, ".partial") || strings.HasSuffix(p, ".key") {
			t.Errorf("left %s", p)
		}
		if data, _ := os.ReadFile(p); bytes.Contains(data, key) {
			t.Errorf("%s holds the key", p)
		}
		return nil
	})
}

func TestBackupsDueSchedule(t *testing.T) {
	setupTestAppDB(t)
	useTestSecretKey(t)
	useTestBackups(t)
	db := catalogFixture(t)
	addTestBook(t, db, "A", []string{"Ann"}, nil, "")
	t0 := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	count := func(kind string) int {
		list, _ := listBackups()
		n := 0
		for _, b := range list {
			if b.Kind == kind {
				n++
			}
		}
		return n
	}

	backupsDue(t0)
	if count(backupApp) != 1 || count(backupLibrary) != 1 {
		t.Fatalf("first run: app %d, library %d", count(backupApp), count(backupLibrary))
	}
	// A restart an hour later: nothing new.
	backupsDue(t0.Add(time.Hour))
	if count(backupApp) != 1 || count(backupLibrary) != 1 {
		t.Errorf("after an hour: app %d, library %d", count(backupApp), count(backupLibrary))
	}
	// A day: the app; a week with metadata.db unchanged: no library backup.
	backupsDue(t0.Add(25 * time.Hour))
	backupsDue(t0.Add(8 * 24 * time.Hour))
	if count(backupApp) != 3 || count(backupLibrary) != 1 {
		t.Errorf("unchanged library: app %d, library %d", count(backupApp), count(backupLibrary))
	}
	// Changed: backed up.
	addTestBook(t, db, "B", []string{"Bob"}, nil, "")
	backupsDue(t0.Add(9 * 24 * time.Hour))
	if count(backupLibrary) != 2 {
		t.Errorf("changed library: %d backups", count(backupLibrary))
	}
	list, _ := listBackups()
	lib, _ := newestBackup(list, backupLibrary)
	var n int
	if err := openBackup(t, filepath.Join(backupRoot(), lib.Name)).QueryRow(`SELECT COUNT(*) FROM books`).Scan(&n); err != nil || n != 2 {
		t.Errorf("books in the library backup: %d %v", n, err)
	}
}

func TestBackupRoutes(t *testing.T) {
	setupTestAppDB(t)
	useTestSecretKey(t)
	useTestBackups(t)
	admin, err := createUser("root", "longenough-pw", true)
	if err != nil {
		t.Fatal(err)
	}
	user, err := createUser("reader", "longenough-pw", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sealSecret("test", "x"); err != nil { // so there's a key to include
		t.Fatal(err)
	}
	name, err := makeBackup(backupApp, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/admin/backups", requireAdminUser(backupNowHandler))
	mux.HandleFunc("GET /api/admin/backups/{name}", requireAdminUser(backupDownloadHandler))
	do := func(method, path string, u *User) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if u != nil {
			req = asUser(req, u)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	for _, u := range []*User{nil, user} {
		if rec := do("GET", "/api/admin/backups/"+name, u); rec.Code != http.StatusNotFound {
			t.Errorf("download as %v: %d", u, rec.Code)
		}
		if rec := do("POST", "/api/admin/backups?kind=app", u); rec.Code != http.StatusNotFound {
			t.Errorf("back up as %v: %d", u, rec.Code)
		}
	}
	for _, bad := range []string{"..%2Fcalibrefast.db", "app-20260101-030000.db", "keys", "x.db"} {
		if rec := do("GET", "/api/admin/backups/"+bad, admin); rec.Code != http.StatusNotFound {
			t.Errorf("download %q: %d", bad, rec.Code)
		}
	}
	if rec := do("GET", "/api/admin/backups?kind=app", admin); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET back up now: %d", rec.Code)
	}
	if rec := do("POST", "/api/admin/backups?kind=nope", admin); rec.Code != http.StatusBadRequest {
		t.Errorf("bad kind: %d", rec.Code)
	}

	rec := do("GET", "/api/admin/backups/"+name, admin)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("download: %d %v", rec.Code, rec.Header())
	}
	body := rec.Body.Bytes()
	if !bytes.HasPrefix(body, []byte("SQLite format 3\x00")) {
		t.Error("the download isn't the database")
	}
	key, _ := os.ReadFile(secretKeyPath())
	if len(key) == 0 || bytes.Contains(body, key) {
		t.Error("the download holds secrets.key")
	}
}
