package main

// Looking a book up online, for the metadata editor (/metadata/lookup). The
// clients for Open Library and Google Books are in pkg/metalookup; this is
// the configuration, the handlers, and the editor's JSON.
// Open Library: by ISBN when the book has one, else (or when the ISBN finds
// nothing) by title and author, which can find several books. Google Books
// (only with an API key, CF_GOOGLE_BOOKS_KEY) and Hardcover (only with
// CF_HARDCOVER_TOKEN, hardcover.go) only when the admin asks for
// them (Google often has the best description, Hardcover the series). The
// matches the admin picks become options in the editor, next to "current"
// and the uploaded formats. Nothing is written until the admin applies
// changes.
//
// CalibreFast is meant to be self-contained, so lookups are kept to a
// minimum:
//   - one source per request, only when an admin clicks, for one book: never
//     on page load, upload, when an ISBN is found, or in the background. The
//     editor searches by title only if the ISBN found nothing, or when the
//     admin asks.
//   - only the ISBN (or the title and first author) is sent, with a generic
//     User-Agent
//   - answers are cached; each service has its own rate and daily limits
//     (metalookup.Limiter), and a 429 pauses it for its Retry-After
//   - covers come from Open Library or Hardcover, fetched by the server, and
//     only when the admin asks
//   - -no-lookups turns everything off; -no-google-books turns off Google,
//     as does having no CF_GOOGLE_BOOKS_KEY
//
// Series on Hardcover (hardcover.go) are the exception to "one book": an
// editor links a whole series, or ticks some of an author's series, and the
// requests wait in a queue for Hardcover's limits. They too run only after a
// click, and never for the whole library.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/isbn"
	"github.com/iotku/CalibreFast/pkg/metalookup"
)

var (
	// noLookups (-no-lookups) turns off every request to another site.
	noLookups bool
	// noGoogleBooks (-no-google-books) turns off Google Books only.
	noGoogleBooks bool
)

// Sources, as the editor names them (?source=) and shows them (Format).
const (
	openLibraryName = metalookup.OpenLibrary
	googleBooksName = metalookup.GoogleBooks
)

// hardcoverName is Hardcover as a lookup source. It uses the same client
// and request budget as series on Hardcover (hardcover.go): a lookup is two
// requests.
const hardcoverName = metalookup.Hardcover

var lookupSources = map[string]string{"openlibrary": openLibraryName, "google": googleBooksName, "hardcover": hardcoverName}

// googleBooksKey is CF_GOOGLE_BOOKS_KEY: Google Books is offered only with one.
var googleBooksKey string

// bookLookups asks Open Library, Google Books and Hardcover (configureLookups
// sets it up with the API key and the Hardcover client).
var bookLookups = newBookLookups("", nil)

func newBookLookups(googleKey string, hc *hardcover.Client) *metalookup.Client {
	return metalookup.New(metalookup.Config{
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		UserAgent:  lookupUserAgent,
		GoogleKey:  googleKey,
		Hardcover:  hc,
		Cache:      metalookup.NewCache(24*time.Hour, 500),
		Limits: map[string]*metalookup.Limiter{
			// An Open Library lookup is up to 7 requests (edition, work,
			// authors; or a search and its works), plus a cover.
			openLibraryName: metalookup.NewLimiter(1, 20, 1000),
			googleBooksName: metalookup.NewLimiter(0.1, 5, 100), // 1 request a lookup
		},
	})
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

// configureLookups reads the optional Google Books API key. Google Books is
// offered only with one: without, Google counts requests against a shared
// quota that's usually used up, and Open Library is the focus anyway.
func configureLookups() {
	googleBooksKey = os.Getenv("CF_GOOGLE_BOOKS_KEY")
	configureHardcover()
	bookLookups = newBookLookups(googleBooksKey, hcClient)
	configureHardcoverSync()
	switch {
	case noLookups:
		log.Println("-no-lookups: the metadata editor won't look books up online")
	case noGoogleBooks:
		log.Println("-no-google-books: the metadata editor won't offer Google Books")
	case googleBooksKey == "":
		log.Println("no CF_GOOGLE_BOOKS_KEY: the metadata editor won't offer Google Books")
	}
}

// googleBooksEnabled: the editor offers Google Books (Try Google Books).
func googleBooksEnabled() bool {
	return !noLookups && !noGoogleBooks && googleBooksKey != ""
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

// lookupMatch is one book a source found (TS: LookupMatch in web/types.ts).
type lookupMatch struct {
	Option *UploadedBookMeta `json:"option"`
	Cover  string            `json:"cover,omitempty"` // its cover, through /metadata/lookup/cover
	// HasISBN: one of its editions has the ISBN asked about (?match_isbn=).
	// The editions' ISBNs are kept on the server only (there can be
	// hundreds).
	HasISBN bool `json:"hasIsbn,omitempty"`
}

// lookupResult is a source's answer (TS: LookupResult in web/types.ts).
type lookupResult struct {
	Source  string        `json:"source"`            // "Open Library", "Google Books"
	Matches []lookupMatch `json:"matches,omitempty"` // best first; none: no match, or an error
	Error   string        `json:"error,omitempty"`
}

// lookupHandler: GET /metadata/lookup?source=openlibrary|google&isbn=…
// (or &title=…&author=…). With a title, &match_isbn=… marks the matches with
// an edition of that ISBN (HasISBN); it's compared here, not sent anywhere.
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
	if name == googleBooksName && !googleBooksEnabled() {
		http.Error(w, "Google Books is turned off on this server (-no-google-books, or no CF_GOOGLE_BOOKS_KEY).", http.StatusForbidden)
		return
	}
	if name == hardcoverName && !hardcoverEnabled() {
		http.Error(w, "Hardcover is turned off on this server (-no-hardcover, or no CF_HARDCOVER_TOKEN).", http.StatusForbidden)
		return
	}
	q := metalookup.Query{
		Title:  strings.TrimSpace(r.URL.Query().Get("title")),
		Author: strings.TrimSpace(r.URL.Query().Get("author")),
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("isbn")); raw != "" {
		if q.ISBN = isbn.Normalize(raw); q.ISBN == "" {
			http.Error(w, raw+" isn't a valid ISBN (wrong length or check digit)", http.StatusBadRequest)
			return
		}
	} else if q.Title == "" {
		http.Error(w, "need an ISBN or a title", http.StatusBadRequest)
		return
	}
	wait := lookupWait(r)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second+wait)
	defer cancel()
	w.Header().Set("Content-Type", "application/json")
	var res metalookup.Result
	if wait > 0 {
		res = bookLookups.LookupWaiting(ctx, name, q, wait)
	} else {
		res = bookLookups.Lookup(ctx, name, q)
	}
	if want := isbn.Normalize(r.URL.Query().Get("match_isbn")); want != "" && q.ISBN == "" {
		res = res.MarkISBN(want)
	}
	logErr(json.NewEncoder(w).Encode(newLookupResult(res)), "could not encode lookup result")
}

// newLookupResult is the editor's form of a lookup's answer: each match an
// option named after its source, with its cover through
// /metadata/lookup/cover. The metadata is copied: res may be the cache's.
func newLookupResult(res metalookup.Result) lookupResult {
	out := lookupResult{Source: res.Source, Error: res.Error}
	for _, m := range res.Matches {
		out.Matches = append(out.Matches, lookupMatch{
			Option:  &UploadedBookMeta{OPFMetadata: *m.Meta, Format: res.Source},
			Cover:   lookupCoverURL(res.Source, m.CoverID),
			HasISBN: m.HasISBN,
		})
	}
	return out
}

// coverSources are the sources with covers, as /metadata/lookup/cover names
// them (?src=).
var coverSources = map[string]string{"openlibrary": openLibraryName, "hardcover": hardcoverName}

// lookupCoverURL is a source's cover id's URL through /metadata/lookup/cover;
// no cover id, no cover to offer.
func lookupCoverURL(source string, id int64) string {
	for src, name := range coverSources {
		if name == source && id > 0 {
			return fmt.Sprintf("/metadata/lookup/cover?src=%s&id=%d", src, id)
		}
	}
	return ""
}

// lookupCoverHandler: GET /metadata/lookup/cover?src=openlibrary|hardcover&id=…
// fetches a cover for the editor's cover picker, when the admin asks for it.
// Only the id is taken: Open Library's URL is built from it, Hardcover's is
// the one its lookup gave for that book, so it can't be pointed anywhere
// else, and the browser never contacts either.
func lookupCoverHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	source, ok := coverSources[q.Get("src")]
	if !ok || (source == hardcoverName && !hardcoverEnabled()) {
		http.Error(w, "unknown cover", http.StatusBadRequest)
		return
	}
	wait := lookupWait(r)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second+wait)
	defer cancel()
	img, ctype, err := fetchLookupCover(ctx, source, q.Get("id"), wait)
	if code, msg := lookupCoverError(err); code != 0 {
		http.Error(w, msg, code)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(img)
}

// lookupPatience is how long a lookup asked to wait (?wait=1, the cover
// updater's runs) waits for room under a source's limits before answering
// "try again in …": short enough that no proxy times the request out; the
// page waits out anything longer and asks again.
const lookupPatience = 30 * time.Second

// lookupWait is how long this request may wait for room: lookupPatience
// with ?wait=1, else none (the editor's single lookups answer at once).
func lookupWait(r *http.Request) time.Duration {
	if r.URL.Query().Get("wait") == "1" {
		return lookupPatience
	}
	return 0
}

// lookupCoverError is the HTTP status and message for a failed cover fetch
// (0 for none).
func lookupCoverError(err error) (int, string) {
	switch {
	case err == nil:
		return 0, ""
	case errors.Is(err, metalookup.ErrBadCoverID):
		return http.StatusBadRequest, "unknown cover"
	case errors.Is(err, metalookup.ErrNoCover):
		return http.StatusNotFound, "no cover"
	case errors.Is(err, metalookup.ErrCoverUnreadable):
		return http.StatusBadGateway, "cover unavailable"
	case errors.As(err, new(metalookup.ErrDailyLimit)):
		return http.StatusTooManyRequests, err.Error()
	}
	return http.StatusBadGateway, "cover unavailable: " + err.Error()
}

// lookupCovers keeps the covers fetched lately, so the cover updater
// (cover_refresh.go) saves the image its preview showed without asking the
// source again: an Open Library cover counts against the day's requests.
var lookupCovers = newCoverCache(32<<20, 30*time.Minute)

// fetchLookupCover is bookLookups.FetchCover through lookupCovers. With a
// wait, it first waits that long at most for room for the request (Open
// Library's covers count against its limit; Hardcover's images don't).
func fetchLookupCover(ctx context.Context, source, id string, wait time.Duration) ([]byte, string, error) {
	key := source + "\x00" + id
	if img, ctype, ok := lookupCovers.get(key); ok {
		return img, ctype, nil
	}
	if wait > 0 && source == openLibraryName {
		if err := bookLookups.WaitForRoom(ctx, source, 1, wait); err != nil {
			return nil, "", err
		}
	}
	img, ctype, err := bookLookups.FetchCover(ctx, source, id)
	if err == nil {
		lookupCovers.put(key, img, ctype)
	}
	return img, ctype, err
}

// coverCache holds images by key, at most max bytes in all, each for ttl;
// the oldest go first.
type coverCache struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	size  int
	order []string
	items map[string]coverCacheItem
}

type coverCacheItem struct {
	img   []byte
	ctype string
	at    time.Time
}

func newCoverCache(max int, ttl time.Duration) *coverCache {
	return &coverCache{max: max, ttl: ttl, items: map[string]coverCacheItem{}}
}

func (c *coverCache) get(key string) ([]byte, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	if !ok || time.Since(it.at) > c.ttl {
		return nil, "", false
	}
	return it.img, it.ctype, true
}

func (c *coverCache) put(key string, img []byte, ctype string) {
	if len(img) > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop(key)
	for c.size+len(img) > c.max && len(c.order) > 0 {
		c.drop(c.order[0])
	}
	c.items[key] = coverCacheItem{img: img, ctype: ctype, at: time.Now()}
	c.order = append(c.order, key)
	c.size += len(img)
}

func (c *coverCache) drop(key string) {
	it, ok := c.items[key]
	if !ok {
		return
	}
	delete(c.items, key)
	c.size -= len(it.img)
	if i := slices.Index(c.order, key); i >= 0 {
		c.order = slices.Delete(c.order, i, i+1)
	}
}
