//go:build unix

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeUpdate points the update at a script that prints body and exits
// with code, and pretends `current` is cur while running is running.
func fakeUpdate(t *testing.T, body string, code int, cur, running string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "update.sh")
	src := "#!/bin/sh\n" + body + "\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(script, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	oldScript, oldCur, oldRun := updateScript, currentRevision, runningRevision
	updateScript = script
	currentRevision = func() string { return cur }
	runningRevision = func() string { return running }
	t.Setenv("INVOCATION_ID", "test")
	serverUpdate.Lock()
	serverUpdate.st = updateStatus{}
	serverUpdate.Unlock()
	drainStops()
	t.Cleanup(func() {
		updateScript, currentRevision, runningRevision = oldScript, oldCur, oldRun
		drainStops()
	})
}

func drainStops() {
	select {
	case <-stopRequests:
	default:
	}
}

var (
	revA = strings.Repeat("a", 40)
	revB = strings.Repeat("b", 40)
)

// startUpdate POSTs as an admin and waits for the update to finish.
func startUpdate(t *testing.T) updateStatus {
	t.Helper()
	h := requireAdminUser(serverUpdateStart)
	rec := httptest.NewRecorder()
	h(rec, asUser(httptest.NewRequest(http.MethodPost, "/api/admin/server/update", nil), &User{ID: 1, Username: "boss", IsAdmin: true}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: status %d (%s)", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := updateSnapshot(); st.Done {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the update didn't finish")
	return updateStatus{}
}

func stopAsked() string {
	select {
	case why := <-stopRequests:
		return why
	case <-time.After(100 * time.Millisecond):
		return ""
	}
}

// A new build: the output reaches the page, and the server restarts.
func TestServerUpdateRestartsOnNewBuild(t *testing.T) {
	fakeUpdate(t, "echo 'update: building'\necho 'update: built' >&2", 0, revB, revA)
	st := startUpdate(t)
	if !st.OK || !st.Restarting {
		t.Fatalf("got %+v, want ok and restarting", st)
	}
	if got := strings.Join(st.Output, "|"); got != "update: building|update: built" {
		t.Errorf("output %q", got)
	}
	if why := stopAsked(); !strings.Contains(why, "boss") {
		t.Errorf("stop request %q, want one naming the admin", why)
	}
	// Restarting: a second update isn't started meanwhile.
	rec := httptest.NewRecorder()
	requireAdminUser(serverUpdateStart)(rec, asUser(httptest.NewRequest(http.MethodPost, "/", nil), &User{ID: 1, Username: "boss", IsAdmin: true}))
	if rec.Code != http.StatusConflict {
		t.Errorf("second update while restarting: status %d, want 409", rec.Code)
	}
}

// Nothing new: no restart.
func TestServerUpdateAlreadyCurrent(t *testing.T) {
	fakeUpdate(t, "echo 'update: already there'", 0, revA, revA)
	st := startUpdate(t)
	if !st.OK || st.Restarting {
		t.Fatalf("got %+v, want ok, not restarting", st)
	}
	if why := stopAsked(); why != "" {
		t.Errorf("asked to stop (%q) with nothing new", why)
	}
}

// A failed build: no restart, and the page says so.
func TestServerUpdateFailedBuild(t *testing.T) {
	fakeUpdate(t, "echo 'syntax error' >&2", 1, revB, revA)
	st := startUpdate(t)
	if st.OK || st.Restarting || !strings.Contains(st.Message, "keeps running") {
		t.Fatalf("got %+v, want a failure that keeps the server running", st)
	}
	if why := stopAsked(); why != "" {
		t.Errorf("asked to stop (%q) after a failed build", why)
	}
}

// Only admins, and only when systemd will start it again.
func TestServerUpdateGuards(t *testing.T) {
	fakeUpdate(t, "echo hi", 0, revB, revA)
	h := requireAdminUser(serverUpdateStart)
	for _, u := range []*User{nil, {ID: 2, Username: "reader"}, {ID: 3, Username: "ed", IsEditor: true}} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/admin/server/update", nil)
		if u != nil {
			req = asUser(req, u)
		}
		h(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%v: status %d, want 404", u, rec.Code)
		}
	}
	if updateSnapshot().Running {
		t.Fatal("a non-admin started an update")
	}

	t.Setenv("INVOCATION_ID", "")
	rec := httptest.NewRecorder()
	h(rec, asUser(httptest.NewRequest(http.MethodPost, "/api/admin/server/update", nil), &User{ID: 1, Username: "boss", IsAdmin: true}))
	if rec.Code != http.StatusConflict {
		t.Errorf("without systemd: status %d, want 409", rec.Code)
	}

	// The status is JSON, with this process's id for the page to notice a restart.
	rec = httptest.NewRecorder()
	requireAdminUser(serverUpdateStatus)(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/admin/server/update", nil), &User{ID: 1, Username: "boss", IsAdmin: true}))
	var st updateStatus
	if err := json.NewDecoder(rec.Body).Decode(&st); err != nil || st.Started == "" {
		t.Errorf("status: %v, started %q", err, st.Started)
	}
}

// `current` is read as releases/<commit>, from the checkout or a release.
func TestCurrentReleaseRevision(t *testing.T) {
	home := t.TempDir()
	rel := filepath.Join(home, "releases", revB)
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", revB), filepath.Join(home, "current")); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{home, rel} {
		t.Chdir(dir)
		if got := currentReleaseRevision(); got != revB {
			t.Errorf("from %s: got %q, want %q", dir, got, revB)
		}
	}
	// Running from the release: that's the build running.
	t.Chdir(rel)
	if got := runningReleaseRevision(); got != revB {
		t.Errorf("running from %s: got %q", rel, got)
	}
	t.Chdir(t.TempDir())
	if got := currentReleaseRevision(); got != "" {
		t.Errorf("no current: got %q", got)
	}
}
