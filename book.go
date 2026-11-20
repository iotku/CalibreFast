package main

import (
	"bytes"
	"fmt"
	"html/template"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/iotku/CalibreFast/pkg/opf"
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
	// The series as metadata.db has it ("" without one), and the book's
	// number in it ("2", "2.5").
	Series, SeriesIndex string

	Writable bool // server started with -allow-writes (shows the Edit button)

	Formats    []string
	ModTime    string // cache-buster for cover-thumb (matches the library grid's)
	CoverStamp string // cache-buster for the full cover: cover.jpg's modification time

	HasCover bool // cover.jpg exists; otherwise only the placeholder is shown
	// cover.jpg's size in pixels (0 when unknown): the page reserves the
	// cover's shape before any image loads, so nothing moves when it does.
	CoverW, CoverH int
	CoverHue int  // placeholder colour, as on the book's library card
}

// The book page shows this many tags and authors; a "+N more" button
// (user.ts) shows the rest.
const (
	bookPageTagLimit    = 10
	bookPageAuthorLimit = 5
)

// TagLimit and AuthorLimit are bookPageTagLimit and bookPageAuthorLimit, for the template.
func (BookPageData) TagLimit() int    { return bookPageTagLimit }
func (BookPageData) AuthorLimit() int { return bookPageAuthorLimit }

// MoreTags and MoreAuthors are how many are left out until "+N more" is clicked.
func (d BookPageData) MoreTags() int    { return max(0, len(d.Tags)-bookPageTagLimit) }
func (d BookPageData) MoreAuthors() int { return max(0, len(d.Authors)-bookPageAuthorLimit) }

// coverHue is the hue of a book's placeholder cover: the same string hash as
// coverHue in web/utils.ts (JavaScript's 32-bit arithmetic), so the book page
// matches the book's card in the library.
func coverHue(uuid string) int {
	var h int32
	for _, c := range utf16.Encode([]rune(uuid)) {
		h = h<<5 - h + int32(c)
	}
	return int(max(int64(h), -int64(h)) % 360)
}

func bookHandler(w http.ResponseWriter, r *http.Request) {
	uuid := filepath.Base(r.URL.Path)

	path, err := resolveBookPath(uuid)
	if err != nil {
		if redirectMerged(w, r, uuid) {
			return
		}
		// A placeholder's #book/ link (the popup steps onto placeholders).
		if p := placeholderByUUID(uuid); p != nil {
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, p.href(), http.StatusFound)
			return
		}
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
	coverStat, err := os.Stat(filepath.Join(baseDir, path, "cover.jpg"))
	hasCover := err == nil
	var coverTime int64
	if hasCover {
		coverTime = coverStat.ModTime().UnixNano()
	}

	// Groups version: the language label depends on group names.
	// Varies with whether this viewer may edit (shows the Edit button).
	// Server start time: a restart (e.g. after updating templates) must not
	// keep serving browsers a cached page in the old layout.
	// The cover's modification time (a new cover doesn't touch the OPF), so
	// the page links to the new one.
	// The modal (an htmx request) gets the bare fragment; a direct visit gets
	// the full page with the top bar.
	standalone := r.Header.Get("HX-Request") == ""
	etag := fmt.Sprintf(`"%x-%x-%t-%x-%t-%x"`, opfStat.ModTime().UnixNano(), groups.currentVersion(), canEditLibrary(r), serverStarted, standalone, coverTime)
	w.Header().Set("ETag", etag)
	w.Header().Set("Vary", "HX-Request")
	// Revalidated every time (a cheap 304), so a cover or metadata change
	// shows as soon as the book is opened again.
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	opfDoc, err := opf.Load(opfPath)
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

	formattedDate := opfDoc.Metadata.Date
	if t, err := opf.ParseAnyDate(opfDoc.Metadata.Date); err == nil {
		formattedDate = t.Format("January 2, 2006")
	}

	modTime, ok := modTimeCache.Load(uuid) // cache-busting modtime for cover.jpg and cover-thumb.jpg
	if !ok {
		modTime = ""
	}

	data := BookPageData{
		UUID:       uuid,
		Standalone: standalone,

		Title:     opfDoc.Metadata.Title,
		Authors:   opfDoc.Metadata.Creators,
		Publisher: opfDoc.Metadata.Publisher,
		Date:      formattedDate,
		Languages: bookLanguages(opfDoc.Metadata.Languages),
		Writable:  canEditLibrary(r),
		Tags:      opfDoc.Metadata.Subjects,

		Description: template.HTML(
			descriptionPolicy.Sanitize(
				opfDoc.Metadata.Description,
			),
		),

		Formats:    formats,
		ModTime:    modTime.(string),
		CoverStamp: strconv.FormatInt(coverTime, 36),

		HasCover: hasCover,
		CoverHue: coverHue(uuid),
	}
	if hasCover {
		data.CoverW, data.CoverH = imageSize(filepath.Join(baseDir, path, "cover.jpg"))
	}
	data.Series, data.SeriesIndex = librarySeriesFields(uuid)
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

// imageSize is an image file's width and height, read from its header
// (0, 0 when it can't be read).
func imageSize(p string) (int, int) {
	f, err := os.Open(p)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// BookLanguage is one language tag in the book modal.
type BookLanguage struct {
	Code string // raw code, used in the link
	Name string // group name or display name, e.g. "English"
}

func bookLanguages(codes []string) []BookLanguage {
	out := []BookLanguage{}
	for _, c := range opf.CleanLanguages(codes) {
		out = append(out, BookLanguage{Code: c, Name: languageLabel(c)})
	}
	return out
}
