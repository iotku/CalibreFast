package main

// Looking up a book's metadata on Open Library and Google Books, for the
// metadata editor's "Look up" button (/metadata/lookup). Each source's
// answer becomes one more option in the editor, next to "current" and the
// uploaded formats, and the admin picks values field by field. Nothing is
// written until they apply their changes.
//
// Privacy: the library is private, and a lookup tells a third party about a
// book in it. So a lookup only ever happens on a click, sends only the ISBN
// (or the title and first author) with a generic User-Agent, and covers are
// fetched by the server, so the browser never contacts those sites.
// -no-lookups turns lookups off.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// noLookups (-no-lookups) turns off every request to Open Library and
// Google Books.
var noLookups bool

var bookLookups = &bookLookup{
	client:       &http.Client{Timeout: 15 * time.Second},
	openLibrary:  "https://openlibrary.org",
	olCovers:     "https://covers.openlibrary.org",
	googleBooks:  "https://www.googleapis.com/books/v1",
	googleCovers: "https://books.google.com",
	cache:        newLookupCache(24*time.Hour, 500),
}

// lookupUserAgent identifies the software, not the user or the library.
const lookupUserAgent = "CalibreFast (+https://github.com/iotku/CalibreFast)"

// configureLookups reads the optional Google Books API key. Without one,
// Google counts requests against a small shared quota, often used up.
func configureLookups() {
	bookLookups.googleKey = os.Getenv("CF_GOOGLE_BOOKS_KEY")
	if noLookups {
		log.Println("-no-lookups: the metadata editor won't look books up on Open Library or Google Books")
	}
}

// requireLookups refuses lookups when -no-lookups is set.
func requireLookups(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if noLookups {
			http.Error(w, "Online lookups are turned off on this server (-no-lookups).", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

type bookLookup struct {
	client       *http.Client
	openLibrary  string
	olCovers     string
	googleBooks  string
	googleCovers string
	googleKey    string
	cache        *lookupCache
}

// lookupQuery is what's sent: an ISBN-13, or a title and an author.
type lookupQuery struct {
	ISBN, Title, Author string
}

func (q lookupQuery) key() string {
	if q.ISBN != "" {
		return "isbn:" + q.ISBN
	}
	return "q:" + strings.ToLower(q.Title) + "\x00" + strings.ToLower(q.Author)
}

// lookupResult is one source's answer (TS: LookupResult in web/types.ts).
type lookupResult struct {
	Source string            `json:"source"`           // "Open Library", "Google Books"
	Option *UploadedBookMeta `json:"option,omitempty"` // nil: no match, or an error
	Cover  string            `json:"cover,omitempty"`  // its cover, through /metadata/lookup/cover
	Error  string            `json:"error,omitempty"`
}

// lookupHandler: GET /metadata/lookup?isbn=… or ?title=…&author=…
func lookupHandler(w http.ResponseWriter, r *http.Request) {
	q := lookupQuery{
		Title:  strings.TrimSpace(r.URL.Query().Get("title")),
		Author: strings.TrimSpace(r.URL.Query().Get("author")),
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("isbn")); raw != "" {
		if q.ISBN = normalizeISBN(raw); q.ISBN == "" {
			http.Error(w, raw+" isn't a valid ISBN (wrong length or check digit)", http.StatusBadRequest)
			return
		}
	} else if q.Title == "" {
		http.Error(w, "need an ISBN or a title", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(bookLookups.lookup(ctx, q)), "could not encode lookup results")
}

// lookup asks every source at once. Answers, including "no match", are
// cached; errors aren't.
func (l *bookLookup) lookup(ctx context.Context, q lookupQuery) []lookupResult {
	sources := []struct {
		name string
		fn   func(context.Context, lookupQuery) (*lookupResult, error)
	}{
		{"Open Library", l.openLibraryLookup},
		{"Google Books", l.googleBooksLookup},
	}
	results := make([]lookupResult, len(sources))
	var wg sync.WaitGroup
	for i, s := range sources {
		key := s.name + "\x00" + q.key()
		if res, ok := l.cache.get(key); ok {
			results[i] = res
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.fn(ctx, q)
			switch {
			case err != nil:
				log.Printf("lookup: %s: %v", s.name, err)
				results[i] = lookupResult{Source: s.name, Error: err.Error()}
				return
			case res == nil:
				res = &lookupResult{} // no match
			}
			res.Source = s.name
			if res.Option != nil {
				res.Option.Format = s.name
			}
			l.cache.put(key, *res)
			results[i] = *res
		}()
	}
	wg.Wait()
	return results
}

// statusError is a non-200, non-404 answer.
type statusError int

func (e statusError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

// getJSON fetches rawURL into v. found is false on a 404.
func (l *bookLookup) getJSON(ctx context.Context, rawURL string, v any) (found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", lookupUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		// Drop the URL: the error is shown in the editor.
		if ue := (*url.Error)(nil); errors.As(err, &ue) {
			err = ue.Err
		}
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v)
	case http.StatusNotFound:
		return false, nil
	}
	return false, statusError(resp.StatusCode)
}

// ---------------------------------------------------------------------------
// Open Library
// ---------------------------------------------------------------------------

type olKey struct {
	Key string `json:"key"`
}

// olText is a description: a string, or {"type": "/type/text", "value": …}.
type olText string

func (t *olText) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*t = olText(s)
		return nil
	}
	var v struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*t = olText(v.Value)
	return nil
}

type olEdition struct {
	Key          string   `json:"key"` // "/books/OL…M"
	Title        string   `json:"title"`
	Subtitle     string   `json:"subtitle"`
	Publishers   []string `json:"publishers"`
	PublishDate  string   `json:"publish_date"`
	Languages    []olKey  `json:"languages"` // "/languages/eng"
	Authors      []olKey  `json:"authors"`
	Works        []olKey  `json:"works"`
	Covers       []int64  `json:"covers"`
	Subjects     []string `json:"subjects"`
	Description  olText   `json:"description"`
	Contributors []struct {
		Role string `json:"role"`
		Name string `json:"name"`
	} `json:"contributors"`
}

type olWork struct {
	Key         string   `json:"key"` // "/works/OL…W"
	Description olText   `json:"description"`
	Subjects    []string `json:"subjects"`
	Authors     []struct {
		Author olKey `json:"author"`
	} `json:"authors"`
	Covers []int64 `json:"covers"`
}

type olSearchDoc struct {
	Key              string   `json:"key"` // "/works/OL…W"
	Title            string   `json:"title"`
	Subtitle         string   `json:"subtitle"`
	AuthorName       []string `json:"author_name"`
	FirstPublishYear int      `json:"first_publish_year"`
	Publisher        []string `json:"publisher"`
	Language         []string `json:"language"`
	Subject          []string `json:"subject"`
	CoverI           int64    `json:"cover_i"`
}

// olKeyRe checks keys from Open Library before they go into a URL.
var olKeyRe = regexp.MustCompile(`^/(books|works|authors)/OL[0-9]+[MWA]$`)

func (l *bookLookup) openLibraryLookup(ctx context.Context, q lookupQuery) (*lookupResult, error) {
	if q.ISBN == "" {
		return l.openLibrarySearch(ctx, q)
	}
	var ed olEdition
	if found, err := l.getJSON(ctx, l.openLibrary+"/isbn/"+q.ISBN+".json", &ed); err != nil || !found {
		return nil, err
	}
	var work olWork
	if len(ed.Works) > 0 && olKeyRe.MatchString(ed.Works[0].Key) {
		// Best effort: the edition alone is still worth showing.
		if _, err := l.getJSON(ctx, l.openLibrary+ed.Works[0].Key+".json", &work); err != nil {
			log.Printf("lookup: Open Library work %s: %v", ed.Works[0].Key, err)
		}
	}

	authorKeys := ed.Authors
	if len(authorKeys) == 0 {
		for _, a := range work.Authors {
			authorKeys = append(authorKeys, a.Author)
		}
	}
	authors := l.olAuthorNames(ctx, authorKeys)
	if len(authors) == 0 {
		for _, c := range ed.Contributors {
			if strings.EqualFold(c.Role, "author") && c.Name != "" {
				authors = append(authors, c.Name)
			}
		}
	}

	m := &UploadedBookMeta{OPFMetadata: OPFMetadata{
		Title:       joinSubtitle(ed.Title, ed.Subtitle),
		Creators:    authors,
		Publisher:   firstNonEmpty(ed.Publishers...),
		Date:        lookupDate(ed.PublishDate),
		Description: textToHTML(firstNonEmpty(string(ed.Description), string(work.Description))),
		Subjects:    cleanOLSubjects(append(slices.Clone(work.Subjects), ed.Subjects...)),
		Identifiers: []opfIdentifier{{Scheme: "isbn", Value: q.ISBN}},
	}}
	for _, lang := range ed.Languages {
		if code := strings.TrimPrefix(lang.Key, "/languages/"); code != "" {
			m.Languages = append(m.Languages, canonicalLanguage(code))
		}
	}
	if olKeyRe.MatchString(ed.Key) {
		m.Identifiers = append(m.Identifiers, opfIdentifier{Scheme: "olid", Value: strings.TrimPrefix(ed.Key, "/books/")})
	}

	res := &lookupResult{Option: m}
	if id := firstCover(ed.Covers, work.Covers); id > 0 {
		res.Cover = fmt.Sprintf("/metadata/lookup/cover?src=openlibrary&id=%d", id)
	} else {
		res.Cover = "/metadata/lookup/cover?src=openlibrary&isbn=" + q.ISBN
	}
	return res, nil
}

// openLibrarySearch finds the best match for a title and author, as a work
// (all its editions together), so it has no ISBN.
func (l *bookLookup) openLibrarySearch(ctx context.Context, q lookupQuery) (*lookupResult, error) {
	v := url.Values{}
	v.Set("title", q.Title)
	if q.Author != "" {
		v.Set("author", q.Author)
	}
	v.Set("limit", "1")
	v.Set("fields", "key,title,subtitle,author_name,first_publish_year,publisher,language,subject,cover_i")
	var found struct {
		Docs []olSearchDoc `json:"docs"`
	}
	if ok, err := l.getJSON(ctx, l.openLibrary+"/search.json?"+v.Encode(), &found); err != nil || !ok || len(found.Docs) == 0 {
		return nil, err
	}
	doc := found.Docs[0]
	var work olWork
	if olKeyRe.MatchString(doc.Key) {
		if _, err := l.getJSON(ctx, l.openLibrary+doc.Key+".json", &work); err != nil {
			log.Printf("lookup: Open Library work %s: %v", doc.Key, err)
		}
	}
	m := &UploadedBookMeta{OPFMetadata: OPFMetadata{
		Title:       joinSubtitle(doc.Title, doc.Subtitle),
		Creators:    doc.AuthorName,
		Publisher:   firstNonEmpty(doc.Publisher...),
		Description: textToHTML(string(work.Description)),
		Subjects:    cleanOLSubjects(doc.Subject),
	}}
	if doc.FirstPublishYear > 0 {
		m.Date = fmt.Sprintf("%04d-01-01", doc.FirstPublishYear)
	}
	if len(doc.Language) == 1 { // more than one: editions in several languages
		m.Languages = []string{canonicalLanguage(doc.Language[0])}
	}
	if olKeyRe.MatchString(doc.Key) {
		m.Identifiers = []opfIdentifier{{Scheme: "olid", Value: strings.TrimPrefix(doc.Key, "/works/")}}
	}
	res := &lookupResult{Option: m}
	if id := firstCover([]int64{doc.CoverI}, work.Covers); id > 0 {
		res.Cover = fmt.Sprintf("/metadata/lookup/cover?src=openlibrary&id=%d", id)
	}
	return res, nil
}

// olAuthorNames fetches author records (at most 5, at once) for their names.
func (l *bookLookup) olAuthorNames(ctx context.Context, keys []olKey) []string {
	keys = keys[:min(len(keys), 5)]
	names := make([]string, len(keys))
	var wg sync.WaitGroup
	for i, k := range keys {
		if !olKeyRe.MatchString(k.Key) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var a struct {
				Name string `json:"name"`
			}
			if _, err := l.getJSON(ctx, l.openLibrary+k.Key+".json", &a); err != nil {
				log.Printf("lookup: Open Library author %s: %v", k.Key, err)
			}
			names[i] = strings.TrimSpace(a.Name)
		}()
	}
	wg.Wait()
	return slices.DeleteFunc(names, func(s string) bool { return s == "" })
}

// olJunkSubjects are Open Library's own shelving notes, not subjects.
var olJunkSubjects = map[string]bool{
	"accessible book": true, "protected daisy": true, "in library": true, "lending library": true,
	"large type books": true, "overdrive": true, "internet archive wishlist": true,
}

// cleanOLSubjects drops shelving notes and tagged entries ("nyt:…",
// "collectionID:…"), and repeats, keeping the first 15.
func cleanOLSubjects(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		k := strings.ToLower(s)
		if s == "" || strings.Contains(s, ":") || olJunkSubjects[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
		if len(out) == 15 {
			break
		}
	}
	return out
}

func firstCover(lists ...[]int64) int64 {
	for _, l := range lists {
		for _, id := range l {
			if id > 0 { // -1 marks a deleted cover
				return id
			}
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Google Books
// ---------------------------------------------------------------------------

type gbVolume struct {
	ID         string `json:"id"`
	VolumeInfo struct {
		Title               string   `json:"title"`
		Subtitle            string   `json:"subtitle"`
		Authors             []string `json:"authors"`
		Publisher           string   `json:"publisher"`
		PublishedDate       string   `json:"publishedDate"`
		Description         string   `json:"description"`
		Language            string   `json:"language"` // "en"
		Categories          []string `json:"categories"`
		IndustryIdentifiers []struct {
			Type       string `json:"type"` // ISBN_10, ISBN_13, OTHER
			Identifier string `json:"identifier"`
		} `json:"industryIdentifiers"`
		ImageLinks map[string]string `json:"imageLinks"`
	} `json:"volumeInfo"`
}

// googleIDRe checks volume ids before they go into a URL.
var googleIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

func (l *bookLookup) googleBooksLookup(ctx context.Context, q lookupQuery) (*lookupResult, error) {
	v := url.Values{}
	if q.ISBN != "" {
		v.Set("q", "isbn:"+q.ISBN)
	} else {
		s := `intitle:"` + strings.ReplaceAll(q.Title, `"`, "") + `"`
		if q.Author != "" {
			s += ` inauthor:"` + strings.ReplaceAll(q.Author, `"`, "") + `"`
		}
		v.Set("q", s)
	}
	v.Set("maxResults", "1")
	if l.googleKey != "" {
		v.Set("key", l.googleKey)
	}
	var found struct {
		Items []gbVolume `json:"items"`
	}
	ok, err := l.getJSON(ctx, l.googleBooks+"/volumes?"+v.Encode(), &found)
	if se := statusError(0); errors.As(err, &se) && se == http.StatusTooManyRequests {
		if l.googleKey == "" {
			return nil, errors.New("Google's shared quota is used up; set CF_GOOGLE_BOOKS_KEY to use your own API key")
		}
		return nil, errors.New("this API key's quota is used up for today")
	}
	if err != nil || !ok || len(found.Items) == 0 {
		return nil, err
	}
	vol := found.Items[0]
	info := vol.VolumeInfo
	m := &UploadedBookMeta{OPFMetadata: OPFMetadata{
		Title:     joinSubtitle(info.Title, info.Subtitle),
		Creators:  info.Authors,
		Publisher: info.Publisher,
		Date:      lookupDate(info.PublishedDate),
		Subjects:  splitGoogleCategories(info.Categories),
	}}
	if strings.Contains(info.Description, "<") {
		m.Description = info.Description // already HTML
	} else {
		m.Description = textToHTML(info.Description)
	}
	if info.Language != "" {
		m.Languages = []string{canonicalLanguage(info.Language)}
	}
	isbn := q.ISBN
	for _, id := range info.IndustryIdentifiers {
		if isbn == "" && strings.HasPrefix(id.Type, "ISBN") {
			isbn = normalizeISBN(id.Identifier)
		}
	}
	if isbn != "" {
		m.Identifiers = append(m.Identifiers, opfIdentifier{Scheme: "isbn", Value: isbn})
	}
	res := &lookupResult{Option: m}
	if googleIDRe.MatchString(vol.ID) {
		m.Identifiers = append(m.Identifiers, opfIdentifier{Scheme: "google", Value: vol.ID})
		if len(info.ImageLinks) > 0 { // without, Google serves an "image not available" picture
			res.Cover = "/metadata/lookup/cover?src=google&id=" + vol.ID
		}
	}
	return res, nil
}

// splitGoogleCategories turns "Computers / Programming / General" into
// separate tags, without "General" and repeats.
func splitGoogleCategories(cats []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range cats {
		for _, part := range strings.Split(c, "/") {
			part = strings.TrimSpace(part)
			k := strings.ToLower(part)
			if part == "" || k == "general" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, part)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Covers
// ---------------------------------------------------------------------------

var olCoverIDRe = regexp.MustCompile(`^[0-9]{1,12}$`)

// lookupCoverHandler: GET /metadata/lookup/cover?src=openlibrary&id=…
// (or &isbn=…) or ?src=google&id=… fetches a cover for the editor's cover
// picker. The server builds the URL from those values, so it can't be
// pointed anywhere else, and the browser never contacts the third party.
func lookupCoverHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	l := bookLookups
	var u string
	switch q.Get("src") {
	case "openlibrary":
		if id := q.Get("id"); olCoverIDRe.MatchString(id) {
			u = l.olCovers + "/b/id/" + id + "-L.jpg?default=false"
		} else if isbn := normalizeISBN(q.Get("isbn")); isbn != "" {
			u = l.olCovers + "/b/isbn/" + isbn + "-L.jpg?default=false"
		}
	case "google":
		if id := q.Get("id"); googleIDRe.MatchString(id) {
			u = l.googleCovers + "/books/content?id=" + id + "&printsec=frontcover&img=1"
		}
	}
	if u == "" {
		http.Error(w, "unknown cover", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", lookupUserAgent)
	resp, err := l.client.Do(req)
	if err != nil {
		http.Error(w, "cover unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	ctype := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ctype, "image/") {
		http.Error(w, "no cover", http.StatusNotFound)
		return
	}
	const maxCover = 10 << 20
	img, err := io.ReadAll(io.LimitReader(resp.Body, maxCover+1))
	if err != nil || len(img) > maxCover {
		http.Error(w, "cover unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(img)
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

// lookupCache keeps answers in memory, so looking the same book up again
// (or from another field) doesn't ask again.
type lookupCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]lookupCacheEntry
}

type lookupCacheEntry struct {
	res lookupResult
	at  time.Time
}

func newLookupCache(ttl time.Duration, max int) *lookupCache {
	return &lookupCache{ttl: ttl, max: max, entries: map[string]lookupCacheEntry{}}
}

func (c *lookupCache) get(key string) (lookupResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.at) > c.ttl {
		return lookupResult{}, false
	}
	return e.res, true
}

func (c *lookupCache) put(key string, res lookupResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		for k, e := range c.entries {
			if time.Since(e.at) > c.ttl {
				delete(c.entries, k)
			}
		}
		for k := range c.entries { // still full: drop any one
			if len(c.entries) < c.max {
				break
			}
			delete(c.entries, k)
		}
	}
	c.entries[key] = lookupCacheEntry{res: res, at: time.Now()}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func joinSubtitle(title, subtitle string) string {
	title, subtitle = strings.TrimSpace(title), strings.TrimSpace(subtitle)
	if subtitle == "" {
		return title
	}
	return title + ": " + subtitle
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

var yearRe = regexp.MustCompile(`(?:^|[^0-9])((?:1[5-9]|20)[0-9]{2})(?:[^0-9]|$)`)

// lookupDate turns a publication date ("2016", "October 26, 2015",
// "c1995") into the editor's YYYY-MM-DD, or "" if it has no year.
func lookupDate(s string) string {
	if t, err := parseAnyDate(s); err == nil {
		return t.Format("2006-01-02")
	}
	if m := yearRe.FindStringSubmatch(s); m != nil {
		return m[1] + "-01-01"
	}
	return ""
}

var blankLinesRe = regexp.MustCompile(`\n\s*\n`)

// textToHTML turns a plain-text description into HTML paragraphs.
func textToHTML(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	var b strings.Builder
	for _, p := range blankLinesRe.Split(s, -1) {
		if p = strings.TrimSpace(p); p != "" {
			b.WriteString("<p>" + strings.ReplaceAll(html.EscapeString(p), "\n", "<br>") + "</p>")
		}
	}
	return b.String()
}
