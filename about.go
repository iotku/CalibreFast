package main

// The privacy notice and terms (/about/privacy, /about/terms): public, like
// the sign-in page, because GitHub and Google want to see them when they
// review the sign-in setup. They say what the service does with accounts
// and nothing about the library, so like the sign-in page they carry no
// site name, nav or scripts.

import "net/http"

type aboutPage struct {
	Kind  string // "privacy" or "terms"
	Title string
}

func aboutHandler(kind, title string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Referrer-Policy", "no-referrer")
		logErr(templates.ExecuteTemplate(w, "about.html", aboutPage{Kind: kind, Title: title}), "render about")
	}
}
