package main

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Only an admin can stop the server: everyone else gets the 404 of an
// unknown path, and no stop is asked for.
func TestServerStopAdminOnly(t *testing.T) {
	h := requireAdminUser(serverStopHandler)
	drain := func() string {
		select {
		case why := <-stopRequests:
			return why
		default:
			return ""
		}
	}
	drain()

	for _, tc := range []struct {
		name string
		u    *User
	}{
		{"visitor", nil},
		{"user", &User{ID: 2, Username: "reader"}},
		{"editor", &User{ID: 3, Username: "ed", IsEditor: true}},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/admin/server/stop", nil)
		if tc.u != nil {
			req = asUser(req, tc.u)
		}
		h(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", tc.name, rec.Code)
		}
		if why := drain(); why != "" {
			t.Errorf("%s: asked the server to stop (%q)", tc.name, why)
		}
	}

	rec := httptest.NewRecorder()
	h(rec, asUser(httptest.NewRequest(http.MethodPost, "/api/admin/server/stop", nil), &User{ID: 1, Username: "boss", IsAdmin: true}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("admin: status %d, want 202", rec.Code)
	}
	if why := drain(); !strings.Contains(why, "boss") {
		t.Fatalf("admin: stop request %q, want one naming the admin", why)
	}
}

func TestApplyConfigFile(t *testing.T) {
	newFlags := func() (*flag.FlagSet, *string, *string, *bool, *bool) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		base := fs.String("basedir", "", "")
		port := fs.String("port", "8080", "")
		writes := fs.Bool("allow-writes", false, "")
		public := fs.Bool("public", false, "")
		fs.String("create-user", "", "")
		return fs, base, port, writes, public
	}
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "calibrefast.conf")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	fs, base, port, writes, public := newFlags()
	p := write(`# comment
basedir = "/srv/Calibre Library"

-port=9090
allow-writes
public = false
`)
	if err := applyConfigFile(fs, p, nil); err != nil {
		t.Fatal(err)
	}
	if *base != "/srv/Calibre Library" || *port != "9090" || !*writes || *public {
		t.Fatalf("got basedir=%q port=%q allow-writes=%v public=%v", *base, *port, *writes, *public)
	}

	// The command line wins.
	fs, _, port, _, _ = newFlags()
	if err := applyConfigFile(fs, p, map[string]bool{"port": true}); err != nil {
		t.Fatal(err)
	}
	if *port != "8080" {
		t.Fatalf("port %q: the command line's should win", *port)
	}

	for body, want := range map[string]string{
		"nope = 1":            "unknown option",
		"create-user = bob":   "command line",
		"basedir":             "needs a value",
		"port = \"9090":       "bad quoted value",
		"allow-writes = mayb": "allow-writes",
	} {
		fs, _, _, _, _ := newFlags()
		err := applyConfigFile(fs, write(body), nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want one mentioning %q", body, err, want)
		}
	}
}

// The Server page's storage bars: one per disk the active folders are on,
// with the database's size, each disk's free space, a warning when it runs
// low, and only the folders' own names.
func TestServerStorage(t *testing.T) {
	origData, origRecycle, origImport, origBase, origCache, origWatch, origDisk := dataDir, recycleDir, importDir, baseDir, cacheDir, watchDir, diskOf
	t.Cleanup(func() {
		dataDir, recycleDir, importDir, baseDir, cacheDir, watchDir, diskOf = origData, origRecycle, origImport, origBase, origCache, origWatch, origDisk
	})
	write := func(p string, n int) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bytesOf := func(m storageMeter) map[string]int64 {
		out := map[string]int64{}
		for _, s := range m.Segs {
			out[s.Label] = s.Bytes
		}
		return out
	}
	// Disks by folder: a folder under a root is on that root's disk.
	disks := map[string]diskInfo{}
	diskOf = func(p string) (diskInfo, bool) {
		for root, d := range disks {
			if insideDir(p, root) {
				return d, true
			}
		}
		return diskInfo{}, false
	}

	root := t.TempDir()
	dataDir, recycleDir, importDir, cacheDir, watchDir = filepath.Join(root, "cf-data"), "", "", "", ""
	baseDir = filepath.Join(t.TempDir(), "Calibre Library")
	write(filepath.Join(dataDir, appDBFile), 1000)
	write(filepath.Join(dataDir, appDBFile+"-wal"), 200)
	write(filepath.Join(baseDir, "metadata.db"), 10)
	disks[realPath(dataDir)] = diskInfo{ID: 1, Total: 100 << 30, Free: 40 << 30}
	disks[realPath(baseDir)] = diskInfo{ID: 2, Total: 1000 << 30, Free: 50 << 30}
	ms := measureStorage()
	if len(ms) != 2 {
		t.Fatalf("got %d bars, want 2 (data disk, library disk)", len(ms))
	}
	want := map[string]int64{"Database": 1200, "Other files": 60<<30 - 1200, "Free": 40 << 30}
	if got := bytesOf(ms[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("data disk: got %v, want %v", got, want)
	}
	if ms[0].Title != "Data folder, recycle bin and import queue: 40.0 GB free of 100.0 GB" || ms[0].Low != "" {
		t.Errorf("data disk: title %q, low %q", ms[0].Title, ms[0].Low)
	}
	if want := []string{"Data folder: cf-data", "Recycle bin: recycle-bin", "Import queue: imports"}; !reflect.DeepEqual(ms[0].Paths, want) {
		t.Errorf("data disk paths %q, want %q (names only)", ms[0].Paths, want)
	}
	if got := bytesOf(ms[1]); !reflect.DeepEqual(got, map[string]int64{"Other files": 950 << 30, "Free": 50 << 30}) {
		t.Errorf("library disk: got %v", got)
	}
	if ms[1].Low == "" || !reflect.DeepEqual(ms[1].Paths, []string{"Library: Calibre Library"}) {
		t.Errorf("library disk (5%% free): low %q, paths %q; want a warning", ms[1].Low, ms[1].Paths)
	}

	// A recycle bin elsewhere (not made yet) gets its own disk's bar.
	recycleDir = filepath.Join(t.TempDir(), "bin")
	disks[filepath.Dir(realPath(recycleDir))] = diskInfo{ID: 3, Total: 10 << 30, Free: 512 << 20}
	ms = measureStorage()
	if len(ms) != 3 || !strings.HasPrefix(ms[1].Title, "Recycle bin: ") || ms[1].Low == "" {
		t.Fatalf("recycle bin elsewhere: %+v", ms)
	}

	// Without disk information: the folders, no free space.
	diskOf = func(string) (diskInfo, bool) { return diskInfo{}, false }
	ms = measureStorage()
	if len(ms) != 1 || bytesOf(ms[0])["Free"] != 0 || !strings.Contains(ms[0].Title, "free space unknown") {
		t.Errorf("unknown disks: %+v", ms)
	}

	// The page reads a cached measurement: asked again within storageTTL,
	// the OS isn't.
	storageCache.Lock()
	storageCache.meters = nil
	storageCache.Unlock()
	calls := 0
	diskOf = func(string) (diskInfo, bool) { calls++; return diskInfo{ID: 1, Total: 1 << 30, Free: 1 << 29}, true }
	first := serverStorage()
	n := calls
	if again := serverStorage(); calls != n || !reflect.DeepEqual(again, first) {
		t.Errorf("second load within %s measured again (%d calls, then %d)", storageTTL, n, calls)
	}
	storageCache.Lock()
	storageCache.at = time.Now().Add(-storageTTL)
	storageCache.Unlock()
	serverStorage()
	if calls == n {
		t.Error("a stale measurement wasn't taken again")
	}
	storageCache.Lock()
	storageCache.meters = nil
	storageCache.Unlock()
}
