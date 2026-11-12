package main

// "/": the Discover page (docs/notes/Discover.md). The same page for
// everyone, so it can be cached; what's personal is filled in by
// web/discover.ts. Rows so far: Shuffle (pages/shuffle/) and Recently added
// (pages/page1.json), both static files. Nothing here reads the library.

import (
	"net/http"
)

type DiscoverPageData struct {
	Title string
	// The library at a glance (what the visitor can already browse).
	Books, Authors, Tags, Languages int
	Version                         string // pagesVersion: the page files' cache stamp
	ShuffleTotal                    int
	// For the introduction's sign-in line, shown to signed-out visitors
	// (only -public servers let them see this page at all).
	Registration     bool // -allow-registrations
	ReadNeedsAccount bool // -public without -public-read
}

func countOf(key string) int {
	if c := categoryByKey(key); c != nil {
		return len(getAggregateDisplay(c))
	}
	return 0
}

func discoverHandler(w http.ResponseWriter, r *http.Request) {
	data := DiscoverPageData{
		Title:            SiteTitle,
		Books:            int(totalBooks.Load()),
		Authors:          countOf("author"),
		Tags:             countOf("tag"),
		Languages:        countOf("language"),
		Version:          currentPagesVersion(),
		ShuffleTotal:     int(shuffleTotal.Load()),
		Registration:     allowRegistrations,
		ReadNeedsAccount: publicCatalog && !publicRead,
	}
	logErr(templates.ExecuteTemplate(w, "discover.html", data), "render discover page")
}
