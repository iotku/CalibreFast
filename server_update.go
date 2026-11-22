package main

// Updating from the Server page: deploy/update.sh builds the latest commit
// in its own release folder while this server keeps serving, then, if
// `current` now points at another build than the one running, the server
// stops and systemd starts it again on the new build (a second or two).
// A failed build changes nothing. See docs/DEPLOY.md.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"sync"
	"time"
)

// updateTimeout bounds a build (the first one fetches modules and tools).
const updateTimeout = 15 * time.Minute

// updateOutputLines is how much of the build's output the page gets.
const updateOutputLines = 300

// updateScript is the script run, relative to the working directory: the
// release folder under systemd, or the checkout. A variable for tests.
var updateScript = filepath.Join("deploy", "update.sh")

// The commit `current` points at, and the one this binary was built from.
// Variables for tests.
var (
	currentRevision = currentReleaseRevision
	runningRevision = runningReleaseRevision
)

// updateStatus is what GET /api/admin/server/update answers.
type updateStatus struct {
	Running    bool     `json:"running"`
	Done       bool     `json:"done"`
	OK         bool     `json:"ok"`
	Restarting bool     `json:"restarting"`
	Message    string   `json:"message"`
	Output     []string `json:"output"`
	By         string   `json:"by"`
	// Started identifies this process: the page reloads once it changes
	// (the new build answering).
	Started string `json:"started"`
}

var serverUpdate struct {
	sync.Mutex
	st updateStatus
}

func updateSnapshot() updateStatus {
	serverUpdate.Lock()
	defer serverUpdate.Unlock()
	st := serverUpdate.st
	st.Output = append([]string(nil), st.Output...)
	st.Started = strconv.FormatInt(serverStarted, 36)
	return st
}

func updateLine(line string) {
	log.Printf("update> %s", line)
	serverUpdate.Lock()
	defer serverUpdate.Unlock()
	st := &serverUpdate.st
	st.Output = append(st.Output, line)
	if n := len(st.Output) - updateOutputLines; n > 0 {
		st.Output = append(st.Output[:0:0], st.Output[n:]...)
	}
}

func updateFinish(ok, restarting bool, msg string) {
	log.Printf("update: %s", msg)
	serverUpdate.Lock()
	defer serverUpdate.Unlock()
	st := &serverUpdate.st
	st.Running, st.Done, st.OK, st.Restarting, st.Message = false, true, ok, restarting, msg
}

// updateAvailable says why the Server page can't update, or "" when it can.
func updateAvailable() string {
	if !underSystemd() {
		return "Updating from here needs systemd to start the server again (deploy/systemd/)."
	}
	if fi, err := os.Stat(updateScript); err != nil || fi.IsDir() {
		return "There's no " + updateScript + " beside the server: it wasn't installed from a git checkout (docs/DEPLOY.md)."
	}
	return ""
}

// POST /api/admin/server/update: start an update (admins only).
func serverUpdateStart(w http.ResponseWriter, r *http.Request) {
	if why := updateAvailable(); why != "" {
		http.Error(w, why, http.StatusConflict)
		return
	}
	u := currentUser(r)
	serverUpdate.Lock()
	if serverUpdate.st.Running || serverUpdate.st.Restarting {
		serverUpdate.Unlock()
		http.Error(w, "An update is already running.", http.StatusConflict)
		return
	}
	serverUpdate.st = updateStatus{Running: true, By: u.Username}
	serverUpdate.Unlock()

	log.Printf("admin %q started an update", u.Username)
	go runUpdate(u.Username)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(updateSnapshot())
}

// GET /api/admin/server/update: how the update is going.
func serverUpdateStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updateSnapshot())
}

// runUpdate runs the update script, then restarts onto what it built.
func runUpdate(admin string) {
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()
	script, err := filepath.Abs(updateScript)
	if err != nil {
		updateFinish(false, false, "The update couldn't start: "+err.Error())
		return
	}
	// sh, so the script needn't be executable. It gets this process's
	// environment: the unit's PATH and Go settings, as in ExecStartPre.
	cmd := exec.CommandContext(ctx, "/bin/sh", script)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		updateFinish(false, false, "The update couldn't start: "+err.Error())
		return
	}
	read := make(chan struct{})
	go func() {
		defer close(read)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			updateLine(sc.Text())
		}
		_, _ = io.Copy(io.Discard, pr) // a line too long for the scanner
	}()
	err = cmd.Wait()
	pw.Close()
	<-read

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		updateFinish(false, false, "The update took longer than "+updateTimeout.String()+" and was stopped. The server keeps running the build it has.")
		return
	case err != nil:
		updateFinish(false, false, "The update failed ("+err.Error()+"). The server keeps running the build it has.")
		return
	}

	cur, running := currentRevision(), runningRevision()
	if cur != "" && cur == running {
		updateFinish(true, false, "Already running the latest build ("+cur[:12]+"). Nothing to restart.")
		return
	}
	what := "the new build"
	if cur != "" {
		what = cur[:12]
	}
	updateFinish(true, true, "Built. Restarting on "+what+"…")
	requestStop("update by admin " + admin)
}

var commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// currentReleaseRevision is the commit `current` points at
// (releases/<commit>, deploy/update.sh), or "" when there's no such link.
// The working directory is the release itself under systemd, or the
// checkout that has `current` in it.
func currentReleaseRevision() string {
	for _, link := range []string{"current", filepath.Join("..", "..", "current")} {
		dir, err := filepath.EvalSymlinks(link)
		if err != nil {
			continue
		}
		if rev := filepath.Base(dir); commitID.MatchString(rev) {
			return rev
		}
	}
	return ""
}

// runningReleaseRevision is the commit of the release this server runs
// from: under systemd its working directory is releases/<commit>, resolved
// when it started. Otherwise (run from the checkout) it's the commit the
// binary says it was built from, which Go may stamp stale, so a build can
// look new when it isn't: a restart too many, never one too few.
func runningReleaseRevision() string {
	if wd, err := os.Getwd(); err == nil {
		if dir, err := filepath.EvalSymlinks(wd); err == nil {
			if rev := filepath.Base(dir); commitID.MatchString(rev) && filepath.Base(filepath.Dir(dir)) == "releases" {
				return rev
			}
		}
	}
	return fullBuildRevision()
}

// fullBuildRevision is the whole commit this binary was built from, or "".
func fullBuildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}
