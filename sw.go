package main

// The service worker (web/sw.ts) and the offline page it shows. They let
// EPUBs kept in the browser be read with no server; see web/book-cache.ts.

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// readerVendor are the libraries the EPUB reader loads (templates/reader.html),
// kept by the service worker with our own scripts.
var readerVendor = []string{
	"/static/vendor/jszip@3.10.2/dist/jszip.min.js",
	"/static/vendor/epubjs@0.3.93/dist/epub.min.js",
}

var workerScript = sync.OnceValues(func() ([]byte, error) {
	file, ok := assetNames["sw.js"]
	if !ok {
		return nil, errNoWorker
	}
	body := assetFiles["/"+distDir+"/"+file]
	if body == nil {
		return nil, errNoWorker
	}
	// The scripts and styles to keep: this build's, plus the reader's libraries.
	var keep []string
	for name, f := range assetNames {
		if name != "sw.js" && !strings.HasSuffix(f, ".map") {
			keep = append(keep, "/"+distDir+"/"+f)
		}
	}
	for _, v := range readerVendor {
		if assetFiles[v] != nil {
			keep = append(keep, v)
		}
	}
	sort.Strings(keep)
	list, err := json.Marshal(keep)
	if err != nil {
		return nil, err
	}
	// The list goes in front, so the worker changes with every build (and
	// browsers install the new one).
	return append([]byte("self.CF_ASSETS="+string(list)+";\n"), body.body...), nil
})

type workerError string

func (e workerError) Error() string { return string(e) }

const errNoWorker = workerError("sw.js isn't built; run `go generate`")

// serveWorker: GET /sw.js. Revalidated every time (browsers check it for
// updates), and allowed to control the whole site.
func serveWorker(w http.ResponseWriter, r *http.Request) {
	body, err := workerScript()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Service-Worker-Allowed", "/")
	_, _ = w.Write(body)
}

// offlineHandler: GET /offline, the page the service worker shows when the
// server can't be reached. It's the same for everyone (the list of kept
// books is filled in by the browser), so it needs no sign-in.
func offlineHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	logErr(templates.ExecuteTemplate(w, "offline.html", PageData{Title: SiteTitle}), "render offline page")
}

func registerWorkerRoutes() {
	http.HandleFunc("GET /sw.js", serveWorker)
	http.HandleFunc("GET /offline", offlineHandler)
}
