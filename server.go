package main

// Stopping the server cleanly. SIGINT/SIGTERM (Ctrl-C, systemctl stop) and
// an admin's Stop button (/admin/server) end the same way: no new
// connections, requests in progress finish (up to shutdownTimeout), the
// databases close, and the process exits 0.
//
// Coming back is the service manager's job: the systemd unit in
// deploy/systemd/ restarts it (Restart=always), pulling and rebuilding
// first (docs/DEPLOY.md). Restarting itself without one isn't done yet.

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

// shutdownTimeout is how long requests in progress (a download, an
// upload) get to finish once a stop is asked for; then they're cut off.
const shutdownTimeout = 30 * time.Second

// stopRequests carries why the server should stop (the admin who asked).
var stopRequests = make(chan string, 1)

// requestStop asks serve to stop; a second ask while one is pending is dropped.
func requestStop(why string) {
	select {
	case stopRequests <- why:
	default:
	}
}

// serve runs the HTTP server until it's asked to stop (a signal, or an
// admin), then shuts it down cleanly and closes the databases.
func serve(addr string, h http.Handler) {
	srv := &http.Server{Addr: addr, Handler: h}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()

	var why string
	select {
	case err := <-served:
		log.Fatal(err) // couldn't listen (port in use, …)
	case sig := <-signals:
		why = "signal " + sig.String()
	case why = <-stopRequests:
	}
	signal.Stop(signals) // a second Ctrl-C kills it the usual way

	log.Printf("stopping (%s): finishing requests in progress", why)
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("requests still running after %s, closing them: %v", shutdownTimeout, err)
		_ = srv.Close()
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server: %v", err)
	}
	stopBulkEdits(time.Now().Add(shutdownTimeout))          // bulk_queue.go
	imports.stopItemWriter(time.Now().Add(shutdownTimeout)) // imports_edit_queue.go
	closeDatabases()
	log.Println("stopped")
}

func closeDatabases() {
	if appDB != nil {
		logErr(appDB.Close(), "close app database")
	}
	if calibreDB != nil {
		logErr(calibreDB.Close(), "close metadata.db")
	}
}

// underSystemd: systemd sets INVOCATION_ID for the processes of a unit,
// so a stopped server will be started again (with Restart=always).
func underSystemd() bool { return os.Getenv("INVOCATION_ID") != "" }

// buildRevision is the commit the binary was built from ("" when unknown),
// with "+" when the checkout had uncommitted changes.
func buildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev, at string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			at = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" && dirty {
		rev += "+"
	}
	if rev != "" && at != "" {
		rev += " (" + at + ")"
	}
	return rev
}

type serverPageData struct {
	Title     string
	Revision  string
	Started   string
	Uptime    string
	Systemd   bool
	GoVersion string
	Storage   storageMeter
}

// storageSeg is one part of a storage bar: the same markup and classes as
// the Preferences page's storage breakdown (bar() in web/preferences.ts).
type storageSeg struct {
	Label string
	Class string
	Bytes int64
	Size  string
	Width float64 // percent of the bar
}

type storageMeter struct {
	Title string
	Aria  string
	Segs  []storageSeg
	Note  string
}

// serverStorage measures -datadir and the recycle bin, which may be inside
// it (the default) or elsewhere (-recycle-bin). The import queue is counted
// on its own when it's inside -datadir.
func serverStorage() storageMeter {
	data := realPath(dataDir)
	bin := realPath(recycleBinDir())
	queue := realPath(importQueueDir())
	binInside := insideDir(bin, data)
	queueInside := insideDir(queue, data) && !insideDir(queue, bin)
	var db, binBytes, queueBytes, other int64
	_ = filepath.WalkDir(data, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		n := info.Size()
		switch {
		case binInside && insideDir(p, bin):
			binBytes += n
		case queueInside && insideDir(p, queue):
			queueBytes += n
		case filepath.Dir(p) == data && strings.HasPrefix(d.Name(), appDBFile):
			db += n // calibrefast.db and its -wal and -shm
		default:
			other += n
		}
		return nil
	})
	note := "In " + dataDir + "."
	if !binInside {
		binBytes = dirSize(bin)
		note = "In " + dataDir + "; the recycle bin is in " + recycleBinDir() + "."
	}
	segs := []storageSeg{
		{Label: "Database", Class: "seg-database", Bytes: db},
		{Label: "Recycle bin", Class: "seg-recycle", Bytes: binBytes},
		{Label: "Import queue", Class: "seg-imports", Bytes: queueBytes},
		{Label: "Other data", Class: "seg-other-data", Bytes: other},
	}
	total := db + binBytes + queueBytes + other
	m := storageMeter{Title: "Server data: " + humanSize(total), Note: note}
	var aria []string
	for _, s := range segs {
		if s.Bytes <= 0 {
			continue
		}
		s.Size = humanSize(s.Bytes)
		// At least a sliver, so a small part still shows.
		s.Width = max(0.6, float64(s.Bytes)/float64(total)*100)
		aria = append(aria, s.Label+" "+s.Size)
		m.Segs = append(m.Segs, s)
	}
	m.Aria = m.Title + ": " + strings.Join(aria, ", ")
	return m
}

// GET /admin/server: what's running, and the Stop button.
func serverPage(w http.ResponseWriter, r *http.Request) {
	started := time.Unix(0, serverStarted)
	info, _ := debug.ReadBuildInfo()
	goVersion := ""
	if info != nil {
		goVersion = info.GoVersion
	}
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "admin-server.html", serverPageData{
		Title:     SiteTitle,
		Revision:  buildRevision(),
		Started:   started.Format("2006-01-02 15:04:05"),
		Uptime:    time.Since(started).Round(time.Second).String(),
		Systemd:   underSystemd(),
		GoVersion: goVersion,
		Storage:   serverStorage(),
	}), "render admin-server")
}

// POST /api/admin/server/stop: stop the server cleanly. Admins only: the
// route is behind requireAdminUser, so editors, users and visitors get a 404.
func serverStopHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	log.Printf("admin %q asked the server to stop", u.Username)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]bool{"stopping": true, "restarts": underSystemd()})
	// Shutdown waits for this request to finish, so the answer gets out.
	requestStop("asked by admin " + u.Username)
}

func registerServerRoutes() {
	http.HandleFunc("GET /admin/server", requireAdminUser(serverPage))
	http.HandleFunc("POST /api/admin/server/stop", requireAdminUser(serverStopHandler))
}
