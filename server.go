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
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
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
	stopHardcover(time.Now().Add(5 * time.Second))          // hardcover.go
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
	Storage   []storageMeter
	Budgets   []apiBudget
	RefreshMS int64 // how often the page asks for serverLive
}

// serverData is what the Server page shows. Cheap enough to build every
// few seconds: build info and the limiters' counts are in memory, and
// serverStorage measures the disks at most every storageTTL.
func serverData() serverPageData {
	now := time.Now()
	started := time.Unix(0, serverStarted)
	info, _ := debug.ReadBuildInfo()
	goVersion := ""
	if info != nil {
		goVersion = info.GoVersion
	}
	return serverPageData{
		Title:     SiteTitle,
		Revision:  buildRevision(),
		Started:   started.Format("2006-01-02 15:04:05"),
		Uptime:    now.Sub(started).Round(time.Second).String(),
		Systemd:   underSystemd(),
		GoVersion: goVersion,
		Storage:   serverStorage(),
		Budgets:   apiBudgets(now),
		RefreshMS: serverRefresh.Milliseconds(),
	}
}

// serverRefresh is how often the Server page asks for serverLive.
const serverRefresh = 5 * time.Second

// GET /admin/server: what's running, and the Stop button.
func serverPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "admin-server.html", serverData()), "render admin-server")
}

// GET /admin/server/live: the page's sections that change on their own
// (uptime, API budgets, storage), for the page to swap in every
// serverRefresh (web/admin-server.ts).
func serverLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	logErr(templates.ExecuteTemplate(w, "server-live", serverData()), "render server-live")
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
	http.HandleFunc("GET /admin/server/live", requireAdminUser(serverLive))
	http.HandleFunc("POST /api/admin/server/stop", requireAdminUser(serverStopHandler))
}
