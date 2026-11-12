package main

import (
	"html/template"
	"net/http"
)

// Search engines are kept out unless the server is a public library that
// asks for them (-public-read and -robots-allowed). Three layers, since
// robots.txt alone is only a request: robots.txt denies everything, every
// response carries X-Robots-Tag, and every page a noindex meta tag.

// allowRobots is -robots-allowed; it has no effect without -public-read.
var allowRobots bool

func robotsAllowed() bool { return publicRead && allowRobots }

// The deny-all robots.txt names no paths, so it doesn't map the server.
const (
	robotsDeny  = "User-agent: *\nDisallow: /\n"
	robotsAllow = "User-agent: *\nDisallow:\n"
)

func robotsTxtHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if robotsAllowed() {
		_, _ = w.Write([]byte(robotsAllow))
		return
	}
	_, _ = w.Write([]byte(robotsDeny))
}

const robotsNoIndex = "noindex, nofollow, noarchive, nosnippet, noimageindex"

// robotsHeader adds X-Robots-Tag to every response, which also covers what
// a meta tag can't: covers, book files, JSON and error pages.
func robotsHeader(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !robotsAllowed() {
			w.Header().Set("X-Robots-Tag", robotsNoIndex)
		}
		h.ServeHTTP(w, r)
	})
}

// robotsMeta is the {{ robotsMeta }} template func: the noindex meta tag,
// or nothing when robots are allowed.
func robotsMeta() template.HTML {
	if robotsAllowed() {
		return ""
	}
	return `<meta name="robots" content="` + robotsNoIndex + `">`
}

func registerRobotsRoutes() {
	http.HandleFunc("GET /robots.txt", robotsTxtHandler)
}
