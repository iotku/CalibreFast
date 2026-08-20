package main

import (
	"bytes"
	"html/template"
	"net/http"
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

type BookPageData struct {
	UUID string

	Title       string
	Authors     []string
	Description template.HTML
	Publisher   string
	Date        string
	Language    string
	Tags        []string

	Formats []string
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
	if t, err := time.Parse(time.RFC3339, opf.Metadata.Date); err == nil {
		formattedDate = t.Format("January 2, 2006")
	}

	data := BookPageData{
		UUID: uuid,

		Title:     opf.Metadata.Title,
		Authors:   opf.Metadata.Creators,
		Publisher: opf.Metadata.Publisher,
		Date:      formattedDate,
		Language:  opf.Metadata.Language,
		Tags:      opf.Metadata.Subjects,

		Description: template.HTML(
			descriptionPolicy.Sanitize(
				opf.Metadata.Description,
			),
		),

		Formats: formats,
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
