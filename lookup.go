package main

// Looking a book up online, for the metadata editor (/metadata/lookup).
// Open Library first; Wikidata when Open Library doesn't know the ISBN; Google
// Books only when the admin asks for it (it often has the best description).
// Each answer becomes one more option in the editor, next to "current" and
// the uploaded formats. Nothing is written until the admin applies changes.
//
// CalibreFast is meant to be self-contained, so lookups are kept to a
// minimum:
//   - one source per request, only when an admin clicks, for one book: never
//     on page load, upload, when an ISBN is found, or in the background. The
//     editor asks the next source only if the first found nothing, or when
//     the admin asks.
//   - only the ISBN (or the title and first author) is sent, with a generic
//     User-Agent
//   - answers are cached; each service has its own rate and daily limits
//     (serviceLimit), and a 429 pauses it for its Retry-After
//   - covers come from Open Library only, fetched by the server, and only
//     when the admin asks
//   - -no-lookups turns everything off; -no-google-books turns off Google

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
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// noLookups (-no-lookups) turns off every request to another site.
	noLookups bool
	// noGoogleBooks (-no-google-books) turns off Google Books only.
	noGoogleBooks bool
)

// Sources, as the editor names them (?source=) and shows them (Format).
const (
	openLibraryName = "Open Library"
	wikidataName    = "Wikidata"
	googleBooksName = "Google Books"
)

var lookupSources = map[string]string{"openlibrary": openLibraryName, "wikidata": wikidataName, "google": googleBooksName}

var bookLookups = &bookLookup{
	client:      &http.Client{Timeout: 15 * time.Second},
	openLibrary: "https://openlibrary.org",
	olCovers:    "https://covers.openlibrary.org",
	wikidata:    "https://query.wikidata.org/sparql",
	googleBooks: "https://www.googleapis.com/books/v1",
	cache:       newLookupCache(24*time.Hour, 500),
	limits: map[string]*serviceLimit{
		// An Open Library lookup is up to 7 requests (edition, work,
		// authors), plus a cover.
		openLibraryName: newServiceLimit(1, 20, 1000),
		wikidataName:    newServiceLimit(0.5, 10, 500), // 2 queries a lookup
		googleBooksName: newServiceLimit(0.1, 5, 100),  // 1 request a lookup
	},
}

// lookupUserAgent identifies the software and its version, not the user or
// the library.
var lookupUserAgent = "CalibreFast/" + appVersion() + " (+https://github.com/iotku/CalibreFast)"

// appVersion is the module version of an installed build, else the commit
// it was built from.
func appVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			return s.Value[:7]
		}
	}
	return "dev"
}

// configureLookups reads the optional Google Books API key. Without one,
// Google counts requests against a shared quota that's often used up.
func configureLookups() {
	bookLookups.googleKey = os.Getenv("CF_GOOGLE_BOOKS_KEY")
	switch {
	case noLookups:
		log.Println("-no-lookups: the metadata editor won't look books up online")
	case noGoogleBooks:
		log.Println("-no-google-books: the metadata editor won't offer Google Books")
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
	client      *http.Client
	openLibrary string
	olCovers    string
	wikidata    string
	googleBooks string
	googleKey   string
	cache       *lookupCache
	limits      map[string]*serviceLimit // by source name
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

// lookupResult is a source's answer (TS: LookupResult in web/types.ts).
type lookupResult struct {
	Source string            `json:"source"`           // "Open Library", "Wikidata", "Google Books"
	Option *UploadedBookMeta `json:"option,omitempty"` // nil: no match, or an error
	Cover  string            `json:"cover,omitempty"`  // its cover, through /metadata/lookup/cover
	Error  string            `json:"error,omitempty"`
}

// lookupHandler: GET /metadata/lookup?source=openlibrary|wikidata|google
// &isbn=… (or &title=…&author=…; not for Wikidata).
func lookupHandler(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	if source == "" {
		source = "openlibrary"
	}
	name, ok := lookupSources[source]
	if !ok {
		http.Error(w, "unknown source", http.StatusBadRequest)
		return
	}
	if name == googleBooksName && noGoogleBooks {
		http.Error(w, "Google Books is turned off on this server (-no-google-books).", http.StatusForbidden)
		return
	}
	q := lookupQuery{
		Title:  strings.TrimSpace(r.URL.Query().Get("title")),
		Author: strings.TrimSpace(r.URL.Query().Get("author")),
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("isbn")); raw != "" {
		if q.ISBN = normalizeISBN(raw); q.ISBN == "" {
			http.Error(w, raw+" isn't a valid ISBN (wrong length or check digit)", http.StatusBadRequest)
			return
		}
	} else if name == wikidataName {
		http.Error(w, "Wikidata lookups need an ISBN", http.StatusBadRequest)
		return
	} else if q.Title == "" {
		http.Error(w, "need an ISBN or a title", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(bookLookups.lookup(ctx, name, q)), "could not encode lookup result")
}

// lookup asks one source. Answers, including "no match", are cached; errors
// aren't.
func (l *bookLookup) lookup(ctx context.Context, source string, q lookupQuery) lookupResult {
	key := source + "\x00" + q.key()
	if res, ok := l.cache.get(key); ok {
		return res
	}
	fn := map[string]func(context.Context, lookupQuery) (*lookupResult, error){
		openLibraryName: l.openLibraryLookup,
		wikidataName:    l.wikidataLookup,
		googleBooksName: l.googleBooksLookup,
	}[source]
	res, err := fn(ctx, q)
	switch {
	case err != nil:
		log.Printf("lookup: %s: %v", source, err)
		return lookupResult{Source: source, Error: err.Error()}
	case res == nil:
		res = &lookupResult{} // no match
	}
	res.Source = source
	if res.Option != nil {
		res.Option.Format = source
		res.Option.Subjects = uniqueTags(res.Option.Subjects)
	}
	l.cache.put(key, *res)
	return *res
}

// statusError is a non-200, non-404 answer.
type statusError int

func (e statusError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

// do sends a request to a service within its limits, with the generic
// User-Agent. A 429 pauses the service for its Retry-After.
func (l *bookLookup) do(service string, req *http.Request) (*http.Response, error) {
	limit := l.limits[service]
	if err := limit.take(time.Now()); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", lookupUserAgent)
	resp, err := l.client.Do(req)
	if err != nil {
		// Drop the URL: the error is shown in the editor.
		if ue := (*url.Error)(nil); errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		wait := retryAfter(resp.Header.Get("Retry-After"), time.Now())
		limit.pause(time.Now().Add(wait))
		return nil, &rateLimitedError{wait}
	}
	return resp, nil
}

// rateLimitedError: the service answered 429.
type rateLimitedError struct{ wait time.Duration }

func (e *rateLimitedError) Error() string {
	return fmt.Sprintf("asked to slow down (HTTP 429); try again in %s", e.wait)
}

// getJSON fetches rawURL from a service into v. found is false on a 404.
func (l *bookLookup) getJSON(ctx context.Context, service, rawURL string, v any) (found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := l.do(service, req)
	if err != nil {
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

// retryAfter reads a Retry-After header (seconds or a date): at least a
// second, at most a day, and a minute when missing.
func retryAfter(h string, now time.Time) time.Duration {
	d := time.Minute
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, time.Second), 24*time.Hour).Round(time.Second)
}

// serviceLimit keeps CalibreFast well inside a service's limits: a token
// bucket (burst, then perSecond), a daily cap, and a pause after a 429.
// Over the limit a request is refused, not queued.
type serviceLimit struct {
	mu        sync.Mutex
	perSecond float64
	burst     float64
	daily     int
	tokens    float64
	last      time.Time
	day       string // the date today counts
	today     int
	pausedTil time.Time
}

func newServiceLimit(perSecond, burst float64, daily int) *serviceLimit {
	return &serviceLimit{perSecond: perSecond, burst: burst, daily: daily, tokens: burst}
}

func (s *serviceLimit) take(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.pausedTil) {
		return fmt.Errorf("asked to slow down; try again in %s", s.pausedTil.Sub(now).Round(time.Second))
	}
	if d := now.Format(time.DateOnly); d != s.day {
		s.day, s.today = d, 0
	}
	if s.today >= s.daily {
		return fmt.Errorf("CalibreFast's limit of %d requests a day is used up; try again tomorrow", s.daily)
	}
	if !s.last.IsZero() {
		s.tokens = min(s.burst, s.tokens+now.Sub(s.last).Seconds()*s.perSecond)
	}
	s.last = now
	if s.tokens < 1 {
		wait := time.Duration((1 - s.tokens) / s.perSecond * float64(time.Second))
		return fmt.Errorf("too many requests just now; try again in %s", max(wait, time.Second).Round(time.Second))
	}
	s.tokens--
	s.today++
	return nil
}

func (s *serviceLimit) pause(until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if until.After(s.pausedTil) {
		s.pausedTil = until
	}
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
	if found, err := l.getJSON(ctx, openLibraryName, l.openLibrary+"/isbn/"+q.ISBN+".json", &ed); err != nil || !found {
		return nil, err
	}
	var work olWork
	if len(ed.Works) > 0 && olKeyRe.MatchString(ed.Works[0].Key) {
		// Best effort: the edition alone is still worth showing.
		if _, err := l.getJSON(ctx, openLibraryName, l.openLibrary+ed.Works[0].Key+".json", &work); err != nil {
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
	if id := firstCover(ed.Covers, work.Covers); id > 0 { // no cover id: no cover to offer
		res.Cover = fmt.Sprintf("/metadata/lookup/cover?src=openlibrary&id=%d", id)
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
	if ok, err := l.getJSON(ctx, openLibraryName, l.openLibrary+"/search.json?"+v.Encode(), &found); err != nil || !ok || len(found.Docs) == 0 {
		return nil, err
	}
	doc := found.Docs[0]
	var work olWork
	if olKeyRe.MatchString(doc.Key) {
		if _, err := l.getJSON(ctx, openLibraryName, l.openLibrary+doc.Key+".json", &work); err != nil {
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
			if _, err := l.getJSON(ctx, openLibraryName, l.openLibrary+k.Key+".json", &a); err != nil {
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
// Wikidata
// ---------------------------------------------------------------------------

type sparqlResults struct {
	Results struct {
		Bindings []map[string]struct {
			Value string `json:"value"`
			Lang  string `json:"xml:lang"`
		} `json:"bindings"`
	} `json:"results"`
}

var wikidataQIDRe = regexp.MustCompile(`^Q[0-9]+$`)

// isbnHyphenations lists every way an ISBN-13 (and its ISBN-10) could be
// hyphenated. Wikidata stores ISBNs hyphenated (P212, P957), and where the
// hyphens go depends on the ISBN agency's ranges, so the query asks for all
// of them: an exact match is an index lookup, a normalised one a full scan.
func isbnHyphenations(isbn13 string) []string {
	out := []string{isbn13}
	split := func(prefix, body, check string) {
		for g := 1; g <= 5; g++ { // group, registrant, publication
			for r := 1; r <= 7; r++ {
				if p := len(body) - g - r; p >= 1 && p <= 6 {
					out = append(out, prefix+body[:g]+"-"+body[g:g+r]+"-"+body[g+r:]+"-"+check)
				}
			}
		}
	}
	split(isbn13[:3]+"-", isbn13[3:12], isbn13[12:])
	if strings.HasPrefix(isbn13, "978") {
		sum := 0
		for i, c := range isbn13[3:12] {
			sum += int(c-'0') * (10 - i)
		}
		check := strconv.Itoa((11 - sum%11) % 11)
		if check == "10" {
			check = "X"
		}
		isbn10 := isbn13[3:12] + check
		out = append(out, isbn10)
		split("", isbn10[:9], check)
	}
	return out
}

// wikidataLookup finds the edition with this ISBN and reads it and the work
// it's an edition of (P629): two queries, each a few index lookups.
func (l *bookLookup) wikidataLookup(ctx context.Context, q lookupQuery) (*lookupResult, error) {
	if q.ISBN == "" {
		return nil, errors.New("Wikidata lookups need an ISBN")
	}
	quoted := make([]string, 0, 64)
	for _, v := range isbnHyphenations(q.ISBN) {
		quoted = append(quoted, `"`+v+`"`)
	}
	var found sparqlResults
	if _, err := l.sparql(ctx, `SELECT ?item ?work WHERE { VALUES ?isbn { `+strings.Join(quoted, " ")+` }
		{ ?item wdt:P212 ?isbn } UNION { ?item wdt:P957 ?isbn } OPTIONAL { ?item wdt:P629 ?work } } LIMIT 1`, &found); err != nil {
		return nil, err
	}
	if len(found.Results.Bindings) == 0 {
		return nil, nil
	}
	row := found.Results.Bindings[0]
	item := strings.TrimPrefix(row["item"].Value, "http://www.wikidata.org/entity/")
	work := strings.TrimPrefix(row["work"].Value, "http://www.wikidata.org/entity/")
	if !wikidataQIDRe.MatchString(item) {
		return nil, nil
	}
	ids := []string{item}
	if wikidataQIDRe.MatchString(work) && work != item {
		ids = append(ids, work)
	}

	var vals sparqlResults
	if _, err := l.sparql(ctx, wikidataValuesQuery(ids), &vals); err != nil {
		return nil, err
	}
	var rows []wikidataValue
	for _, b := range vals.Results.Bindings {
		ord, _ := strconv.Atoi(b["ord"].Value)
		rows = append(rows, wikidataValue{From: b["from"].Value, Kind: b["kind"].Value, Ref: b["ref"].Value,
			Value: b["value"].Value, Lang: b["value"].Lang, Ord: ord})
	}
	return &lookupResult{Option: wikidataOption(q.ISBN, item, rows)}, nil
}

func (l *bookLookup) sparql(ctx context.Context, query string, v any) (bool, error) {
	return l.getJSON(ctx, wikidataName, l.wikidata+"?format=json&query="+url.QueryEscape(query), v)
}

// wikidataValuesQuery reads the values of these items (the edition, then its
// work), one per row so that rows can't multiply. The QIDs are written into
// the query: the query service only uses its indexes for constants here.
func wikidataValuesQuery(ids []string) string {
	const label = `rdfs:label ?value FILTER(LANG(?value) IN ("en", "mul"))`
	var branches []string
	for _, id := range ids {
		e, from := "wd:"+id, `BIND("`+id+`" AS ?from)`
		for _, b := range []struct{ kind, pattern string }{
			{"title", e + ` wdt:P1476 ?value`},
			{"label", e + ` ` + label},
			{"date", e + ` wdt:P577 ?value`},
			{"author", e + ` p:P50 ?st . ?st ps:P50 ?ref . OPTIONAL { ?st pq:P1545 ?ord } ?ref ` + label},
			{"publisher", e + ` wdt:P123 ?ref . ?ref ` + label},
			{"language", e + ` wdt:P407 ?ref . ?ref wdt:P218|wdt:P219|wdt:P220 ?value`},
			{"subject", e + ` wdt:P136|wdt:P921 ?ref . ?ref ` + label},
		} {
			branches = append(branches, `{ `+b.pattern+` BIND("`+b.kind+`" AS ?kind) `+from+` }`)
		}
	}
	return "SELECT ?from ?kind ?ref ?value ?ord WHERE {\n" + strings.Join(branches, "\nUNION ") + "\n}"
}

// wikidataValue is one row of wikidataValuesQuery.
type wikidataValue struct {
	From, Kind, Ref, Value, Lang string
	Ord                          int // author order (P1545), 0 if unset
}

// wikidataOption maps the values of an edition (item) and its work: the
// edition's own values first, the work's where the edition has none.
// Labels are English, else multilingual.
func wikidataOption(isbn, item string, rows []wikidataValue) *UploadedBookMeta {
	// Values of one kind, from the edition if it has any, else the work.
	pick := func(kind string) []wikidataValue {
		var mine, work []wikidataValue
		for _, r := range rows {
			if r.Kind != kind {
				continue
			}
			if r.From == item {
				mine = append(mine, r)
			} else {
				work = append(work, r)
			}
		}
		if len(mine) > 0 {
			return mine
		}
		return work
	}
	// One label per referenced item, English first, in first-seen order.
	labels := func(rs []wikidataValue) []wikidataValue {
		var out []wikidataValue
		index := map[string]int{}
		for _, r := range rs {
			if i, ok := index[r.Ref]; ok {
				if r.Lang == "en" && out[i].Lang != "en" {
					out[i].Value, out[i].Lang = r.Value, r.Lang
				}
				continue
			}
			index[r.Ref] = len(out)
			out = append(out, r)
		}
		return out
	}
	first := func(rs []wikidataValue) string {
		for _, r := range rs {
			if r.Lang == "en" || r.Lang == "" {
				return r.Value
			}
		}
		if len(rs) > 0 {
			return rs[0].Value
		}
		return ""
	}

	m := &UploadedBookMeta{OPFMetadata: OPFMetadata{
		Title:       firstNonEmpty(first(pick("title")), first(pick("label"))),
		Publisher:   first(labels(pick("publisher"))),
		Identifiers: []opfIdentifier{{Scheme: "isbn", Value: isbn}, {Scheme: "wikidata", Value: item}},
	}}
	if d := pick("date"); len(d) > 0 {
		m.Date = lookupDate(d[0].Value)
	}
	authors := labels(pick("author"))
	sort.SliceStable(authors, func(i, j int) bool {
		a, b := authors[i].Ord, authors[j].Ord
		return a != 0 && (b == 0 || a < b) // numbered first, in order
	})
	for _, a := range authors {
		m.Creators = append(m.Creators, a.Value)
	}
	seen := map[string]bool{}
	for _, l := range pick("language") {
		if c := canonicalLanguage(l.Value); !seen[l.Ref] && !slices.Contains(m.Languages, c) {
			seen[l.Ref] = true
			m.Languages = append(m.Languages, c)
		}
	}
	var subjects []wikidataValue // the edition's and the work's
	for _, r := range rows {
		if r.Kind == "subject" {
			subjects = append(subjects, r)
		}
	}
	for _, s := range labels(subjects) {
		m.Subjects = append(m.Subjects, s.Value)
	}
	if m.Subjects = uniqueTags(m.Subjects); len(m.Subjects) > 15 {
		m.Subjects = m.Subjects[:15]
	}
	return m
}

// ---------------------------------------------------------------------------
// Google Books (only when the admin asks)
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
	} `json:"volumeInfo"`
}

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
	ok, err := l.getJSON(ctx, googleBooksName, l.googleBooks+"/volumes?"+v.Encode(), &found)
	if rl := (*rateLimitedError)(nil); errors.As(err, &rl) && l.googleKey == "" {
		return nil, errors.New("Google's shared quota is used up; set CF_GOOGLE_BOOKS_KEY to use your own API key")
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
	if googleIDRe.MatchString(vol.ID) {
		m.Identifiers = append(m.Identifiers, opfIdentifier{Scheme: "google", Value: vol.ID})
	}
	return &lookupResult{Option: m}, nil
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
// fetches a cover for the editor's cover picker, when the admin asks for it. The server builds the URL from those values, so it can't be
// pointed anywhere else, and the browser never contacts Open Library.
func lookupCoverHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	l := bookLookups
	var u string
	if id := q.Get("id"); q.Get("src") == "openlibrary" && olCoverIDRe.MatchString(id) {
		u = l.olCovers + "/b/id/" + id + "-L.jpg?default=false"
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
	resp, err := l.do(openLibraryName, req)
	if err != nil {
		http.Error(w, "cover unavailable: "+err.Error(), http.StatusBadGateway)
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
