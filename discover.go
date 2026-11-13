package main

// "/": the Discover page (docs/notes/Discover.md). The same page for
// everyone, so it can be cached; what's personal is filled in by
// web/discover.ts, including the order of the rows (home_layout.go).
// Nothing here reads the library.

import (
	"net/http"
)

type DiscoverPageData struct {
	Title   string
	Books   int
	Version string // pagesVersion: the page files' cache stamp
	// featured.json's stamp; "" when no shelf is featured. Signed-out
	// visitors' shelf rows (signed-in users' come with /api/me/home).
	FeaturedVersion string
	// The tag rows' index (JSON).
	TagRows      string
	ShuffleTotal int
	// For the greeting's sign-in line, shown to signed-out visitors
	// (only -public servers let them see this page at all).
	Registration     bool // -allow-registrations
	ReadNeedsAccount bool // -public without -public-read
}

func discoverHandler(w http.ResponseWriter, r *http.Request) {
	tags := currentTagRows()
	data := DiscoverPageData{
		Title:            SiteTitle,
		Books:            int(totalBooks.Load()),
		Version:          currentPagesVersion(),
		FeaturedVersion:  currentFeaturedVersion(),
		TagRows:          tags.json,
		ShuffleTotal:     int(shuffleTotal.Load()),
		Registration:     allowRegistrations,
		ReadNeedsAccount: publicCatalog && !publicRead,
	}
	logErr(templates.ExecuteTemplate(w, "discover.html", data), "render discover page")
}
