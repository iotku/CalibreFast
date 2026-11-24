package main

// Backups (-backup-dir, default <datadir>/backups):
//
//   - app-YYYYMMDD-HHMMSS.db: the app database (accounts, reading, highlights,
//     shelves: data kept nowhere else), daily. Its sidecar .json names the
//     secrets.key it was sealed with, by fingerprint.
//
// secrets.key (secretbox.go) is never rotated, so it isn't copied daily, and
// never into -backup-dir: whoever has both the database and its key can use
// every linked Hardcover sign-in (docs/SECURITY.md), so the two are kept
// apart. A copy of each key a kept backup needs stays beside the live one,
// <datadir>/secrets.key.d/secrets-<fingerprint>.key (0600), in case the
// key file is replaced or deleted.
//   - library-YYYYMMDD-HHMMSS.db: Calibre's metadata.db, weekly, and only if
//     it changed since the last one (its sidecar keeps metadata.db's key,
//     catalog_snapshot.go).
//
// Each is made with VACUUM INTO (a consistent copy while the server runs) to
// a .partial name, then renamed, so a crash never leaves half a backup. The
// newest -backup-keep of each kind are kept. Admins list, start and
// download them on the Server page (server.go, templates/admin-server.html).
// The files are as sensitive as -datadir itself: owner-only.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	backupsOn   = true // -backups
	backupDir   string // -backup-dir; "" is <datadir>/backups
	backupKeep  = 7    // -backup-keep, per kind
	backupEvery = map[string]time.Duration{backupApp: 24 * time.Hour, backupLibrary: 7 * 24 * time.Hour}
)

const (
	backupApp     = "app"
	backupLibrary = "library"
	backupStamp   = "20060102-150405"
	backupCheck   = time.Hour // how often the scheduler looks
)

var backupNameRe = regexp.MustCompile(`^(app|library)-(\d{8}-\d{6})\.db$`)

func backupRoot() string {
	if backupDir != "" {
		return backupDir
	}
	return filepath.Join(dataDir, "backups")
}

// backupSidecar is what's kept beside a backup.
type backupSidecar struct {
	Key     string      `json:"key,omitempty"`     // app: the secrets.key fingerprint ("" if there was none)
	Library *libraryKey `json:"library,omitempty"` // library: metadata.db as it was
}

// backupInfo is one backup, as the Server page lists it.
type backupInfo struct {
	Name string
	Kind string
	At   time.Time
	Size int64
	Key  string // app backups: the key it goes with
}

func (b backupInfo) When() string     { return b.At.Local().Format("2006-01-02 15:04") }
func (b backupInfo) SizeText() string { return humanSize(b.Size) }
func (b backupInfo) Label() string {
	if b.Kind == backupApp {
		return "App data"
	}
	return "Calibre library (metadata.db)"
}

// listBackups lists the backups, newest first.
func listBackups() ([]backupInfo, error) {
	entries, err := os.ReadDir(backupRoot())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []backupInfo
	for _, e := range entries {
		m := backupNameRe.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		at, err := time.ParseInLocation(backupStamp, m[2], time.UTC)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		b := backupInfo{Name: e.Name(), Kind: m[1], At: at, Size: info.Size()}
		if sc, err := readSidecar(e.Name()); err == nil {
			b.Key = sc.Key
		}
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b backupInfo) int { return b.At.Compare(a.At) })
	return out, nil
}

func sidecarPath(name string) string {
	return filepath.Join(backupRoot(), strings.TrimSuffix(name, ".db")+".json")
}

func readSidecar(name string) (backupSidecar, error) {
	var sc backupSidecar
	data, err := os.ReadFile(sidecarPath(name))
	if err != nil {
		return sc, err
	}
	return sc, json.Unmarshal(data, &sc)
}

// newestBackup is the newest backup of kind, if any.
func newestBackup(list []backupInfo, kind string) (backupInfo, bool) {
	for _, b := range list {
		if b.Kind == kind {
			return b, true
		}
	}
	return backupInfo{}, false
}

// ---------------------------------------------------------------------------
// Making them
// ---------------------------------------------------------------------------

// backupMu: one backup at a time.
var backupMu sync.Mutex

// backupState is what the Server page shows while one runs, or after one failed.
var backupState struct {
	sync.Mutex
	running string // kind
	lastErr string
}

// makeBackup makes a backup of kind now, unless force is false and
// metadata.db hasn't changed since the last library backup. It returns the
// new backup's name ("" when skipped).
func makeBackup(kind string, now time.Time, force bool) (string, error) {
	backupMu.Lock()
	defer backupMu.Unlock()
	backupState.Lock()
	backupState.running = kind
	backupState.Unlock()
	name, err := makeBackupLocked(kind, now, force)
	backupState.Lock()
	backupState.running = ""
	if err != nil {
		backupState.lastErr = fmt.Sprintf("%s backup failed: %v", kind, err)
	} else {
		backupState.lastErr = ""
	}
	backupState.Unlock()
	return name, err
}

func makeBackupLocked(kind string, now time.Time, force bool) (string, error) {
	root := backupRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	var sc backupSidecar
	switch kind {
	case backupApp:
		if appDB == nil {
			return "", errors.New("no app database")
		}
		key, err := backupSecretKey()
		if err != nil {
			return "", fmt.Errorf("secrets.key: %w", err)
		}
		sc.Key = key
	case backupLibrary:
		if calibreDB == nil {
			return "", errors.New("no library")
		}
		lk, ok, err := currentLibraryKey()
		if err == nil && ok {
			sc.Library = &lk
			if !force {
				list, _ := listBackups()
				if last, found := newestBackup(list, backupLibrary); found {
					if prev, err := readSidecar(last.Name); err == nil && prev.Library != nil && *prev.Library == lk {
						return "", nil // unchanged since the last one
					}
				}
			}
		}
	default:
		return "", fmt.Errorf("unknown backup kind %q", kind)
	}

	start := time.Now()
	prefix := backupApp // a constant, whatever kind's string came from
	if kind == backupLibrary {
		prefix = backupLibrary
	}
	name := prefix + "-" + now.UTC().Format(backupStamp) + ".db"
	final := filepath.Join(root, name)
	if _, err := os.Stat(final); err == nil {
		return "", fmt.Errorf("%s already exists", name)
	}
	partial := final + ".partial"
	_ = os.Remove(partial) // left by a crash: VACUUM INTO won't overwrite
	var err error
	if kind == backupApp {
		_, err = appDB.Exec(`VACUUM INTO ?`, partial)
	} else {
		_, err = calibreDB.Exec(`VACUUM INTO ?`, partial)
	}
	if err != nil {
		_ = os.Remove(partial)
		return "", err
	}
	if err := os.Chmod(partial, 0o600); err != nil {
		_ = os.Remove(partial)
		return "", err
	}
	data, _ := json.Marshal(sc)
	if err := writeFileAtomic(sidecarPath(name), data, 0o600); err != nil {
		_ = os.Remove(partial)
		return "", err
	}
	if err := os.Rename(partial, final); err != nil {
		_ = os.Remove(partial)
		_ = os.Remove(sidecarPath(name))
		return "", err
	}
	info, _ := os.Stat(final)
	var size int64
	if info != nil {
		size = info.Size()
	}
	log.Printf("backup: %s (%s) in %s", name, humanSize(size), time.Since(start).Round(time.Millisecond))
	pruneBackups(kind)
	return name, nil
}

// backupSecretKey copies secrets.key into keyCopyDir unless a copy of this key is
// there already, and returns its fingerprint ("" when there's no key yet:
// nothing has been sealed).
func backupSecretKey() (string, error) {
	key, err := os.ReadFile(secretKeyPath())
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(key)
	fp := hex.EncodeToString(sum[:])[:16]
	path := keyBackupPath(fp)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(key) {
		return fp, nil
	}
	return fp, writeFileAtomic(path, key, 0o600)
}

// keyCopyDir is where copies of secrets.key are kept: beside it, in
// -datadir, not in -backup-dir.
func keyCopyDir() string { return secretKeyPath() + ".d" }

func keyBackupPath(fp string) string {
	return filepath.Join(keyCopyDir(), "secrets-"+fp+".key")
}

// writeFileAtomic writes beside path and renames it into place.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// pruneBackups keeps the newest backupKeep of kind, and the key copies the
// app backups kept still need.
func pruneBackups(kind string) {
	list, err := listBackups()
	if err != nil {
		log.Println("backup: prune:", err)
		return
	}
	n := 0
	for _, b := range list {
		if b.Kind != kind {
			continue
		}
		n++
		if n <= max(backupKeep, 1) {
			continue
		}
		logErr(os.Remove(filepath.Join(backupRoot(), b.Name)), "backup: remove "+b.Name)
		_ = os.Remove(sidecarPath(b.Name))
	}
	if kind != backupApp {
		return
	}
	needed := map[string]bool{}
	if list, err = listBackups(); err == nil {
		for _, b := range list {
			if b.Key != "" {
				needed[b.Key] = true
			}
		}
	}
	keys, _ := os.ReadDir(keyCopyDir())
	for _, e := range keys {
		fp, ok := strings.CutPrefix(strings.TrimSuffix(e.Name(), ".key"), "secrets-")
		if ok && strings.HasSuffix(e.Name(), ".key") && !needed[fp] {
			logErr(os.Remove(filepath.Join(keyCopyDir(), e.Name())), "backup: remove key "+e.Name())
		}
	}
}

// backupsDue makes the backups that are due at now: each kind when its
// newest is older than its interval (so a restart doesn't make extra ones).
func backupsDue(now time.Time) {
	list, err := listBackups()
	if err != nil {
		log.Println("backup:", err)
		return
	}
	for _, kind := range []string{backupApp, backupLibrary} {
		if last, ok := newestBackup(list, kind); ok && now.Sub(last.At) < backupEvery[kind] {
			continue
		}
		if kind == backupLibrary && calibreDB == nil {
			continue
		}
		if _, err := makeBackup(kind, now, false); err != nil {
			log.Printf("backup: %s: %v", kind, err)
		}
	}
}

// startBackups runs backupsDue now and every backupCheck.
func startBackups() {
	if !backupsOn {
		return
	}
	go func() {
		backupsDue(time.Now())
		t := time.NewTicker(backupCheck)
		defer t.Stop()
		for now := range t.C {
			backupsDue(now)
		}
	}()
}

// ---------------------------------------------------------------------------
// Server page
// ---------------------------------------------------------------------------

// backupView is the Server page's Backups section.
type backupView struct {
	On      bool
	Dir     string
	Keep    int
	List    []backupInfo
	Running string
	Error   string
}

func backupData() backupView {
	v := backupView{On: backupsOn, Dir: backupRoot(), Keep: backupKeep}
	v.List, _ = listBackups()
	backupState.Lock()
	v.Running, v.Error = backupState.running, backupState.lastErr
	backupState.Unlock()
	return v
}

// POST /api/admin/backups (kind=app|library): make one now, in the
// background; the page's live section shows it when it's done.
func backupNowHandler(w http.ResponseWriter, r *http.Request) {
	// The kind becomes part of a file name: take it from the constants, not
	// the request.
	var kind string
	switch r.FormValue("kind") {
	case backupApp:
		kind = backupApp
	case backupLibrary:
		kind = backupLibrary
	default:
		http.Error(w, "kind must be app or library", http.StatusBadRequest)
		return
	}
	if kind == backupLibrary && calibreDB == nil {
		http.Error(w, "no library", http.StatusConflict)
		return
	}
	if !backupMu.TryLock() {
		http.Error(w, "A backup is already running.", http.StatusConflict)
		return
	}
	backupMu.Unlock()
	log.Printf("admin %q asked for a %s backup", currentUser(r).Username, kind)
	go func() {
		if _, err := makeBackup(kind, time.Now(), true); err != nil {
			log.Printf("backup: %s: %v", kind, err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

// GET /api/admin/backups/{name}: download one: the database alone. An app
// backup's key stays on the server (keyCopyDir), apart from it.
func backupDownloadHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	list, err := listBackups()
	if err != nil {
		http.Error(w, "can't list backups", http.StatusInternalServerError)
		return
	}
	i := slices.IndexFunc(list, func(b backupInfo) bool { return b.Name == name })
	if i < 0 || !backupNameRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	b := list[i]
	path := filepath.Join(backupRoot(), b.Name)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "can't read the backup", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	file := "calibrefast-" + b.Name // app: restored as calibrefast.db
	if b.Kind == backupLibrary {
		file = strings.TrimSuffix(b.Name, ".db") + "-metadata.db"
	}
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	http.ServeContent(w, r, "", b.At, f)
}
