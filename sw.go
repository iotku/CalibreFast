package main

// The service worker (web/sw.ts), the offline page it shows, and the web
// app manifest. Together they make CalibreFast an app that can be installed
// ("Install app" / "Add to Home screen" in Chrome on Android) and that reads
// EPUBs kept in the browser with no server; see web/book-cache.ts and
// web/offline.ts.

import (
	"encoding/json"
	"html/template"
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
// books is filled in by the browser). On a private server it, like /sw.js,
// needs a session: the worker is only registered by signed-in pages, and
// fetches both (with the cookie) when it installs.
func offlineHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	logErr(templates.ExecuteTemplate(w, "offline.html", PageData{Title: SiteTitle}), "render offline page")
}

// iconDir holds the app's icons (static/icons/, loaded like static/vendor).
// They're public: the sign-in page shows no icon, but the browser may fetch
// them without a session. Their names carry a version, as they're cached
// for good: draw a new one under a new name.
const iconDir = "static/icons"

var appIcons = []struct{ Src, Sizes string }{
	{"/static/icons/icon-192-v1.png", "192x192"},
	{"/static/icons/icon-512-v1.png", "512x512"},
}

// themeColor: the top bar's colour, for the browser's own bars and the
// installed app's title bar; backgroundColor: the page's, for its splash screen.
const (
	themeColor      = "#141414"
	backgroundColor = "#111111"
)

// appHead is the template function appHead: what every signed-in page's
// <head> has for the installed app. The manifest needs a session on a
// private library (it names the library), so it's fetched with the cookie.
// {{ appHead "reader" }} leaves out the theme colour: the readers set their
// own, from the reading theme (web/reader-chrome.ts).
func appHead(page ...string) template.HTML {
	theme := `
    <meta name="theme-color" content="` + themeColor + `">`
	if len(page) > 0 && page[0] == "reader" {
		theme = ""
	}
	return template.HTML(`<link rel="manifest" href="/manifest.webmanifest" crossorigin="use-credentials">` + theme + `
    <link rel="icon" type="image/png" sizes="32x32" href="/static/icons/icon-32-v1.png">
    <link rel="apple-touch-icon" href="/static/icons/icon-180-v1.png">`)
}

// manifestHandler: GET /manifest.webmanifest. The app opens on the library;
// offline, that's the offline page (web/sw.ts), with the books kept here.
func manifestHandler(w http.ResponseWriter, r *http.Request) {
	type icon struct {
		Src     string `json:"src"`
		Sizes   string `json:"sizes"`
		Type    string `json:"type"`
		Purpose string `json:"purpose"`
	}
	var icons []icon
	for _, i := range appIcons {
		// The icons fill their square (no transparent corners), with the
		// book inside the middle 80%: fine as is and when masked to a shape.
		icons = append(icons, icon{i.Src, i.Sizes, "image/png", "any"}, icon{i.Src, i.Sizes, "image/png", "maskable"})
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "no-cache")
	logErr(json.NewEncoder(w).Encode(map[string]any{
		"id":               "/",
		"name":             SiteTitle,
		"short_name":       SiteTitle,
		"start_url":        "/",
		"scope":            "/",
		"display":          "standalone",
		"background_color": backgroundColor,
		"theme_color":      themeColor,
		"icons":            icons,
	}), "write manifest")
}

func registerWorkerRoutes() {
	http.HandleFunc("GET /sw.js", serveWorker)
	http.HandleFunc("GET /offline", offlineHandler)
	http.HandleFunc("GET /manifest.webmanifest", manifestHandler)
}
