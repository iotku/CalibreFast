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

// The Server page's storage bar splits -datadir into the database, the
// recycle bin and the import queue, and counts a recycle bin kept elsewhere.
func TestServerStorage(t *testing.T) {
	origData, origRecycle, origImport := dataDir, recycleDir, importDir
	t.Cleanup(func() { dataDir, recycleDir, importDir = origData, origRecycle, origImport })
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

	dataDir, recycleDir, importDir = t.TempDir(), "", ""
	write(filepath.Join(dataDir, appDBFile), 1000)
	write(filepath.Join(dataDir, appDBFile+"-wal"), 200)
	write(filepath.Join(dataDir, "recycle-bin", "b1", "books", "1", "x.epub"), 3000)
	write(filepath.Join(dataDir, "imports", "7", "y.epub"), 400)
	write(filepath.Join(dataDir, aggregateImagesDir, "z.jpg"), 50)
	got := bytesOf(serverStorage())
	want := map[string]int64{"Database": 1200, "Recycle bin": 3000, "Import queue": 400, "Other data": 50}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("inside: got %v, want %v", got, want)
	}

	recycleDir = t.TempDir()
	write(filepath.Join(recycleDir, "b2", "books", "2", "a.pdf"), 5000)
	m := serverStorage()
	if got := bytesOf(m)["Recycle bin"]; got != 5000 {
		t.Errorf("elsewhere: recycle bin %d, want 5000", got)
	}
	if !strings.Contains(m.Note, recycleDir) {
		t.Errorf("note %q doesn't say where the recycle bin is", m.Note)
	}
}
