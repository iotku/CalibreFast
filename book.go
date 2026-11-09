package main

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
)

var descriptionPolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()

	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.RequireCrossOriginAnonymous(true)

	return p
}()

// serverStarted is part of page ETags so a restart invalidates cached pages.
var serverStarted = time.Now().UnixNano()

type BookPageData struct {
	UUID       string
	Standalone bool // opened directly (permalink), not into the modal: show the top bar

	Title       string
	Authors     []string
	Description template.HTML
	Publisher   string
	Date        string
	Languages   []BookLanguage
	Tags        []string

	Writable bool // server started with -allow-writes (shows the Edit button)

	Formats []string
	ModTime string
}

func bookHandler(w http.ResponseWriter, r *http.Request) {
	uuid := filepath.Base(r.URL.Path)

	path, err := resolveBookPath(uuid)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	opfPath := filepath.Join(
		baseDir,
		path,
		"metadata.opf",
	)

	opfStat, err := os.Stat(opfPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Groups version: the language label depends on group names.
	// Varies with whether this viewer may edit (shows the Edit button).
	// Server start time: a restart (e.g. after updating templates) must not
	// keep serving browsers a cached page in the old layout.
	// The modal (an htmx request) gets the bare fragment; a direct visit gets
	// the full page with the top bar.
	standalone := r.Header.Get("HX-Request") == ""
	etag := fmt.Sprintf(`"%x-%x-%t-%x-%t"`, opfStat.ModTime().UnixNano(), groups.currentVersion(), canEditLibrary(r), serverStarted, standalone)
	w.Header().Set("ETag", etag)
	w.Header().Set("Vary", "HX-Request")
	w.Header().Set("Cache-Control", "private, max-age=30")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	opf, err := loadOPF(opfPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	files := resolveFormats(uuid, baseDir, path)

	formats := make([]string, 0, len(files))

	for _, f := range files {
		ext := strings.TrimPrefix(filepath.Ext(f), ".")
		formats = append(formats, ext)
	}

	formattedDate := opf.Metadata.Date
	if t, err := parseAnyDate(opf.Metadata.Date); err == nil {
		formattedDate = t.Format("January 2, 2006")
	}

	modTime, ok := modTimeCache.Load(uuid) // cache-busting modtime for cover.jpg and cover-thumb.jpg
	if !ok {
		modTime = ""
	}

	data := BookPageData{
		UUID:       uuid,
		Standalone: standalone,

		Title:     opf.Metadata.Title,
		Authors:   opf.Metadata.Creators,
		Publisher: opf.Metadata.Publisher,
		Date:      formattedDate,
		Languages: bookLanguages(opf.Metadata.Languages),
		Writable:  canEditLibrary(r),
		Tags:      opf.Metadata.Subjects,

		Description: template.HTML(
			descriptionPolicy.Sanitize(
				opf.Metadata.Description,
			),
		),

		Formats: formats,
		ModTime: modTime.(string), // cache-busting modtime for cover.jpg and cover-thumb.jpg
	}
	// render into buffer first
	var buf bytes.Buffer

	err = templates.ExecuteTemplate(&buf, "book.html", data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// BookLanguage is one language tag in the book modal.
type BookLanguage struct {
	Code string // raw code, used in the link
	Name string // group name or display name, e.g. "English"
}

func bookLanguages(codes []string) []BookLanguage {
	out := []BookLanguage{}
	for _, c := range cleanLanguages(codes) {
		out = append(out, BookLanguage{Code: c, Name: languageLabel(c)})
	}
	return out
}
