package main

// Placeholder books: books that aren't in the library yet (the next of a
// series, say), with their metadata and perhaps a cover, kept in the app DB
// rather than metadata.db, so that Calibre never holds books that may not
// exist (a lookup service could suggest wrong ones; source says where one
// came from).
//
// They show, marked "Not in library", on the pages of their series, author
// and publisher (filteredBooksHandler), and in On Deck when the next book of
// a series is one (nextInSeries). They aren't in Latest, search, the
// aggregate counts or anything else built from the library.
//
// Admins (those who can edit the library) list them, and the gaps in the
// library's series, at /missing; make them at /missing/new; and edit them on
// their own page, /missing/{uuid}, which anyone who can browse the library
// sees. Adding files there sends them to the import queue with the
// placeholder's metadata; approving the book replaces that placeholder, and
// only it. Adding any other book replaces those it matches: the same title
// by one of the same authors, or its series and number where the number
// tells books apart (placeholderNumberMatch: not in a series whose books
// aren't numbered). A book that matches more than one is added only once
// an editor says whether it replaces them all or none
// (placeholdersToReplace). Each removal says how it was matched and by whom,
// is in the edit log, and is listed on /missing to restore one replaced in
// error. A book uploaded or queued for import
// with no series of its own takes the series and number of the placeholder
// with its title and author first (fillFromPlaceholder): ebooks rarely say.
// A library book edited to be a placeholder's book replaces it too
// (placeholder_resolve.go).
//
// Signed-in users can Save one; they're told when it's replaced by a
// library book (notifications.go).

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/opf"
)

const placeholderCoversDir = "placeholder-covers"

// placeholder is one book not in the library.
type placeholder struct {
	UUID      string
	Meta      OPFMetadata // Series and SeriesIndex hold its series
	Cover     string      // file in placeholderCoversDir, "" for none
	Source    string      // "manual", or "hardcover" (hardcover.go)
	CreatedAt int64
	UpdatedAt int64
}

// placeholderCard: what a placeholder adds to its grid entry
// (Book.Placeholder). Its UUID is the placeholder's, never a library book's.
type placeholderCard struct {
	Href  string `json:"href"`            // its page
	Cover string `json:"cover,omitempty"` // its picture, "" for none
}

func (p *placeholder) href() string { return "/missing/" + url.PathEscape(p.UUID) }

func (p *placeholder) coverURL() string {
	if p.Cover == "" {
		return ""
	}
	return "/" + placeholderCoversDir + "/" + p.Cover
}

// index is its number in its series (1 when not given, as in Calibre).
func (p *placeholder) index() float64 {
	_, f, err := opf.CleanSeries(p.Meta.Series, p.Meta.SeriesIndex)
	if err != nil {
		return 1
	}
	return f
}

func (p *placeholder) authors() string { return strings.Join(p.Meta.Creators, " & ") }

// book is its grid entry.
func (p *placeholder) book() Book {
	return Book{
		UUID:         p.UUID,
		Title:        p.Meta.Title,
		Authors:      p.authors(),
		SeriesIndex:  p.index(),
		PubDate:      p.Meta.Date,
		LastModified: strconv.FormatInt(p.UpdatedAt, 10),
		Placeholder:  &placeholderCard{Href: p.href(), Cover: p.coverURL()},
	}
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// placeholderCache: every placeholder, by title; nil until loaded and after
// a change. There are few, so pages filter them in memory.
var placeholderCache atomic.Pointer[[]*placeholder]

func allPlaceholders() []*placeholder {
	if ps := placeholderCache.Load(); ps != nil {
		return *ps
	}
	if appDB == nil {
		return nil
	}
	rows, err := appDB.Query(`SELECT uuid, meta, cover, source, created_at, updated_at FROM placeholders WHERE deleted_at IS NULL`)
	if err != nil {
		log.Println("placeholders:", err)
		return nil
	}
	defer rows.Close()
	var ps []*placeholder
	for rows.Next() {
		var p placeholder
		var meta string
		if err := rows.Scan(&p.UUID, &meta, &p.Cover, &p.Source, &p.CreatedAt, &p.UpdatedAt); err != nil {
			log.Println("placeholders:", err)
			continue
		}
		if err := json.Unmarshal([]byte(meta), &p.Meta); err != nil {
			log.Printf("placeholder %s: %v", p.UUID, err)
			continue
		}
		ps = append(ps, &p)
	}
	if err := rows.Err(); err != nil {
		log.Println("placeholders:", err)
		return nil
	}
	slices.SortFunc(ps, func(a, b *placeholder) int { return cmp.Compare(titleSort(a.Meta.Title), titleSort(b.Meta.Title)) })
	placeholderCache.Store(&ps)
	return ps
}

func placeholdersChanged() { placeholderCache.Store(nil) }

func placeholderByUUID(id string) *placeholder {
	for _, p := range allPlaceholders() {
		if p.UUID == id {
			return p
		}
	}
	return nil
}

// cleanPlaceholderMeta checks and tidies what the editor sent.
func cleanPlaceholderMeta(m OPFMetadata) (OPFMetadata, error) {
	m.Title = strings.TrimSpace(m.Title)
	if m.Title == "" || m.Title == "Unknown" {
		return m, errors.New("a placeholder needs a title")
	}
	name, index, err := opf.CleanSeries(m.Series, m.SeriesIndex)
	if err != nil {
		return m, err
	}
	m.Series = name
	m.SeriesIndex = ""
	if name != "" {
		m.SeriesIndex = strconv.FormatFloat(index, 'f', -1, 64)
	}
	m.SeriesSet = false
	if m.Date = normalizeDateOnly(strings.TrimSpace(m.Date)); len(m.Date) > 10 {
		m.Date = m.Date[:10]
	}
	if len(m.Description) > 100_000 {
		return m, errors.New("the description is too long")
	}
	m.Meta = nil
	return m, nil
}

// savePlaceholder inserts (p.UUID "") or updates a placeholder's metadata.
func savePlaceholder(p *placeholder, by *User) error {
	meta, err := json.Marshal(p.Meta)
	if err != nil {
		return err
	}
	now := nowUnix()
	p.UpdatedAt = now
	// Its series' gaps, before and after (series_gaps.go).
	if old := placeholderByUUID(p.UUID); old != nil {
		defer gapsPlaceholderChanged(old.Meta.Series)
	}
	defer gapsPlaceholderChanged(p.Meta.Series)
	if p.UUID == "" {
		p.UUID = uuid.New().String()
		p.CreatedAt = now
		if p.Source == "" {
			p.Source = "manual"
		}
		var uid any
		if by != nil {
			uid = by.ID
		}
		_, err = appDB.Exec(`INSERT INTO placeholders (uuid, meta, cover, source, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.UUID, string(meta), p.Cover, p.Source, uid, now, now)
	} else {
		_, err = appDB.Exec(`UPDATE placeholders SET meta = ?, cover = ?, updated_at = ? WHERE uuid = ?`, string(meta), p.Cover, now, p.UUID)
	}
	placeholdersChanged()
	return err
}

// Why a placeholder was removed (placeholders.replaced_why), for the edit
// log and the list of recently removed ones on /missing.
const (
	whyFilesAdded  = "files added from its page"
	whySeriesIndex = "same series and number"
	whyTitle       = "same title and author"
	whyEdited      = "a library book edited to be it"
	whyByHand      = "removed by hand"
)

// deletePlaceholder removes one. It is only marked removed (deleted_at),
// cover and all, so the removal can be undone (editlog.go).
func deletePlaceholder(id string) error { return replacePlaceholder(id, "") }

// replacePlaceholder removes one that library book bookUUID is ("": none,
// removed by hand); its page then leads to that book.
func replacePlaceholder(id, bookUUID string) error { return removePlaceholder(id, bookUUID, "", nil) }

// removePlaceholder removes placeholder id: library book bookUUID is it
// ("": none), found by why (whySeriesIndex…), by user by (nil: nobody, or
// the server). Both are kept for /missing's list of removed placeholders.
func removePlaceholder(id, bookUUID, why string, by *User) error {
	p := placeholderByUUID(id)
	if p == nil {
		return sql.ErrNoRows
	}
	user := ""
	if by != nil {
		user = by.Username
	}
	if _, err := appDB.Exec(`UPDATE placeholders SET deleted_at = ?, replaced_by = ?, replaced_why = ?, replaced_user = ? WHERE uuid = ?`,
		nowUnix(), bookUUID, why, user, id); err != nil {
		return err
	}
	placeholdersChanged()
	gapsPlaceholderChanged(p.Meta.Series)
	placeholderReplacedForSavers(p, bookUUID) // notifications.go
	return nil
}

// replacePlaceholderLogged: library book bookUUID replaces p (why: how it
// was matched), in the edit log too, so it can be undone there or restored
// from /missing.
func replacePlaceholderLogged(p *placeholder, bookUUID, why string, by *User) error {
	if err := removePlaceholder(p.UUID, bookUUID, why, by); err != nil {
		return err
	}
	log.Printf("placeholder %q is in the library now (%s)", p.Meta.Title, why)
	logEdit(by, "placeholder in library ("+why+")", logKindPlaceholder, p.UUID, p.Meta.Title, snapPlaceholder(p), nil)
	return nil
}

// removeUnusedPlaceholderCover deletes a cover no placeholder uses, removed
// ones included, and the edit log doesn't name (an undo may want it back).
func removeUnusedPlaceholderCover(name string) {
	if name == "" || !aggregateImageRe.MatchString(name) {
		return
	}
	var n int
	if err := appDB.QueryRow(`SELECT (SELECT COUNT(*) FROM placeholders WHERE cover = ?) +
		(SELECT COUNT(*) FROM edit_log WHERE kind = 'placeholder' AND (instr(before, ?) > 0 OR instr(after, ?) > 0))`,
		name, name, name).Scan(&n); err != nil || n > 0 {
		return
	}
	if err := os.Remove(filepath.Join(dataDir, placeholderCoversDir, name)); err != nil && !os.IsNotExist(err) {
		log.Printf("placeholder cover %s: %v", name, err)
	}
}

// placeholderTitleMatch: placeholder p is the book with this title and
// these authors: the same title (case, punctuation and a trailing
// "(Series #n)" aside; a subtitle on one side only is allowed) by one of
// the same authors ("Pratchett, Terry" is "Terry Pratchett").
func placeholderTitleMatch(p *placeholder, title string, authors []string) bool {
	pt, bt := hcFullTitleKey(p.Meta.Title), hcFullTitleKey(title)
	if pt == "" || bt == "" {
		return false
	}
	if pt != bt && hcTitleKey(title) != pt && hcTitleKey(p.Meta.Title) != bt {
		return false
	}
	return hcSharesAuthor(p.Meta.Creators, authors)
}

// placeholderForNewBook: the placeholder a book about to be added stands
// for, by its title and authors; nil when none, or when several in
// different series could be it.
func placeholderForNewBook(m *OPFMetadata) *placeholder {
	if appDB == nil || m == nil {
		return nil
	}
	var found *placeholder
	for _, p := range allPlaceholders() {
		if !placeholderTitleMatch(p, m.Title, m.Creators) {
			continue
		}
		if found != nil && !strings.EqualFold(found.Meta.Series, p.Meta.Series) {
			return nil // ambiguous
		}
		if found == nil {
			found = p
		}
	}
	return found
}

// knownAuthors: authors names someone ("Unknown" doesn't).
func knownAuthors(authors []string) bool {
	return slices.ContainsFunc(authors, func(a string) bool {
		a = strings.TrimSpace(a)
		return a != "" && !strings.EqualFold(a, "Unknown")
	})
}

// libraryHasSeriesNumber: a book in the library is number index of series.
func libraryHasSeriesNumber(series string, index float64) bool {
	if calibreDB == nil {
		return false
	}
	var one int
	err := calibreDB.QueryRow(`SELECT 1 FROM books b JOIN books_series_link l ON l.book = b.id JOIN series s ON s.id = l.series
		WHERE s.name = ? COLLATE NOCASE AND b.series_index = ? LIMIT 1`, series, index).Scan(&one)
	return err == nil
}

// placeholderNumberMatch: placeholder p is the book at number index of
// series (cleaned: opf.CleanSeries) by authors, by its number alone. Only
// when the number tells books apart: no other placeholder of the series
// and no book in the library has it (a series whose books aren't numbered
// has them all at #1, or #0), and p shares an author with the book where
// both name one.
func placeholderNumberMatch(p *placeholder, series string, index float64, authors []string) bool {
	if series == "" || !strings.EqualFold(strings.TrimSpace(p.Meta.Series), series) || p.index() != index {
		return false
	}
	for _, q := range allPlaceholders() {
		if q != p && strings.EqualFold(strings.TrimSpace(q.Meta.Series), series) && q.index() == index {
			return false
		}
	}
	if knownAuthors(p.Meta.Creators) && knownAuthors(authors) && !hcSharesAuthor(p.Meta.Creators, authors) {
		return false
	}
	return !libraryHasSeriesNumber(series, index)
}

// placeholderMatch is a placeholder a book being added stands for, and why.
type placeholderMatch struct {
	P   *placeholder
	Why string // whyTitle…
}

// placeholdersForBook: the placeholders a book about to be added stands
// for: the same title by one of its authors (placeholderTitleMatch), or
// its series and number where that tells (placeholderNumberMatch).
func placeholdersForBook(m *OPFMetadata) []placeholderMatch {
	if appDB == nil || m == nil {
		return nil
	}
	c := *m
	c.FillSeries()
	series, index, err := opf.CleanSeries(c.Series, c.SeriesIndex)
	if err != nil {
		series = ""
	}
	var out []placeholderMatch
	for _, p := range allPlaceholders() {
		switch {
		case placeholderTitleMatch(p, c.Title, c.Creators):
			out = append(out, placeholderMatch{p, whyTitle})
		case placeholderNumberMatch(p, series, index, c.Creators):
			out = append(out, placeholderMatch{p, whySeriesIndex})
		}
	}
	return out
}

// matchingPlaceholder: the placeholder a book about to be added stands
// for (placeholdersForBook), the one with its title first. nil when none.
func matchingPlaceholder(m *OPFMetadata) *placeholder {
	if p := placeholderForNewBook(m); p != nil {
		return p
	}
	if ms := placeholdersForBook(m); len(ms) > 0 {
		return ms[0].P
	}
	return nil
}

// How an add treats the placeholders the book matches, when more than one
// does (addOptions.Placeholders): the editor is asked first.
const (
	placeholdersAsk  = ""     // refuse the add: tooManyPlaceholdersError
	placeholdersAll  = "all"  // replace them all
	placeholdersNone = "none" // replace none
)

// tooManyPlaceholdersError: a book wasn't added, as it matches more than
// one placeholder and nobody said whether it replaces them all.
type tooManyPlaceholdersError struct{ Matches []placeholderMatch }

func (e *tooManyPlaceholdersError) Error() string {
	return fmt.Sprintf("this book matches %d placeholders: say whether it replaces them all", len(e.Matches))
}

// placeholderChoiceRow is a matched placeholder, for asking (JSON).
type placeholderChoiceRow struct {
	UUID    string `json:"uuid"`
	Title   string `json:"title"`
	Authors string `json:"authors,omitempty"`
	Series  string `json:"series,omitempty"`
	Index   string `json:"index,omitempty"`
	Why     string `json:"why"`
}

func (e *tooManyPlaceholdersError) Rows() []placeholderChoiceRow {
	out := make([]placeholderChoiceRow, 0, len(e.Matches))
	for _, m := range e.Matches {
		out = append(out, placeholderChoiceRow{UUID: m.P.UUID, Title: m.P.Meta.Title, Authors: m.P.authors(),
			Series: m.P.Meta.Series, Index: m.P.Meta.SeriesIndex, Why: m.Why})
	}
	return out
}

// placeholdersToReplace: what adding the book m replaces. forID is the
// placeholder its files were added for (from its page): only that one, if
// it's still there. Else what it matches (placeholdersForBook); when that
// is more than one, choice says which (placeholdersAll…), and with none
// given the add is refused (tooManyPlaceholdersError), so it is asked.
func placeholdersToReplace(m *OPFMetadata, forID, choice string) ([]placeholderMatch, error) {
	if forID != "" {
		if p := placeholderByUUID(forID); p != nil {
			return []placeholderMatch{{p, whyFilesAdded}}, nil
		}
	}
	ms := placeholdersForBook(m)
	if len(ms) <= 1 || choice == placeholdersAll {
		return ms, nil
	}
	if choice == placeholdersNone {
		return nil, nil
	}
	return nil, &tooManyPlaceholdersError{ms}
}

// placeholdersSettled: books just added, whose placeholders the add
// decided (resolvePlaceholders). The catalog's next reading of them isn't
// an edit to match against placeholders (placeholdersCatalogChanged), or
// "replace none", or files added for one placeholder, would be undone.
var placeholdersSettled sync.Map // book uuid -> struct{}

// resolvePlaceholders: library book bookUUID, just added, replaces the
// placeholders in ms (placeholdersToReplace), and no others.
func resolvePlaceholders(ms []placeholderMatch, bookUUID string, by *User) {
	if bookUUID != "" {
		placeholdersSettled.Store(bookUUID, struct{}{})
	}
	for _, m := range ms {
		if err := replacePlaceholderLogged(m.P, bookUUID, m.Why, by); err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("placeholder %s: %v", m.P.UUID, err)
		}
	}
}

// placeholderSourceFormat names a placeholder's metadata among the
// editor's sources.
const placeholderSourceFormat = "Placeholder"

// placeholderSource: p's metadata as a source in the editor (the import
// review's, or the added book's), for picking its values field by field.
func placeholderSource(p *placeholder) *UploadedBookMeta {
	m := p.Meta
	m.Creators = slices.Clone(m.Creators)
	m.Subjects = slices.Clone(m.Subjects)
	m.Languages = slices.Clone(m.Languages)
	m.Identifiers = slices.Clone(m.Identifiers)
	m.Meta = nil
	m.SyncSeriesMeta()
	return &UploadedBookMeta{OPFMetadata: m, Format: placeholderSourceFormat}
}

// fillFromPlaceholder: a book about to be added (uploaded, or put in the
// import queue) that has no series takes the series and number of the
// placeholder it stands for. Ebooks rarely say what series they're in; the
// placeholder (made by hand, or from a series on Hardcover) does. Its
// Hardcover identifiers come too, where the book has none. Nothing else is
// taken: the files' own metadata is kept. It returns the placeholder, or
// nil when it changed nothing.
func fillFromPlaceholder(m *OPFMetadata) *placeholder {
	if m == nil {
		return nil
	}
	m.FillSeries()
	if strings.TrimSpace(m.Series) != "" {
		return nil
	}
	p := placeholderForNewBook(m)
	if p == nil || strings.TrimSpace(p.Meta.Series) == "" {
		return nil
	}
	m.Series, m.SeriesIndex = p.Meta.Series, p.Meta.SeriesIndex
	m.SyncSeriesMeta()
	var add []opfIdentifier
	for _, id := range hcIdentifiers(p.Meta) {
		if identifierValue(*m, id.Scheme) == "" {
			add = append(add, id)
		}
	}
	m.Identifiers = append(m.Identifiers, add...)
	return p
}

// ---------------------------------------------------------------------------
// On the library's pages
// ---------------------------------------------------------------------------

// placeholdersFor: those shown on c's filtered page for members (the value,
// or its group's members). Only authors, publishers and series show them.
func placeholdersFor(c *category, members []string) []*placeholder {
	var out []*placeholder
	for _, p := range allPlaceholders() {
		var vals []string
		switch c.Key {
		case "author":
			vals = p.Meta.Creators
		case "publisher":
			vals = []string{p.Meta.Publisher}
		case "series":
			vals = []string{p.Meta.Series}
		default:
			return nil
		}
		if slices.ContainsFunc(vals, func(v string) bool {
			return v != "" && slices.ContainsFunc(members, func(m string) bool { return strings.EqualFold(m, v) })
		}) {
			out = append(out, p)
		}
	}
	return out
}

// listEntry is a book or placeholder with what a filtered page sorts by.
type listEntry struct {
	Book
	sort  string
	index float64
	date  string // seriesDate
}

// compareListEntries orders a filtered page as its SQL does (bookOrder).
func compareListEntries(series bool, a, b *listEntry) int {
	if series {
		if c := cmp.Compare(a.index, b.index); c != 0 {
			return c
		}
		if (a.date == "") != (b.date == "") {
			if a.date == "" {
				return 1
			}
			return -1
		}
		if c := strings.Compare(a.date, b.date); c != 0 {
			return c
		}
	}
	return strings.Compare(a.sort, b.sort)
}

// filteredPageWithPlaceholders: page `page` of c's filtered page for
// members, its books and the placeholders phs merged in order. In another
// order than the page's default (key), the placeholders come after the books.
func filteredPageWithPlaceholders(c *category, members []string, phs []*placeholder, page int, key string) ([]Book, error) {
	query := fmt.Sprintf(`
		SELECT b.uuid, b.title, `+bookAuthorsSQL+` AS authors,
			b.pubdate, b.path, b.last_modified, b.sort, b.series_index
		FROM books b
		JOIN %s x ON x.book = b.id
		JOIN %s t ON t.id = x.%s
		WHERE t.%s IN (%s)
		GROUP BY b.id
		ORDER BY %s`,
		c.LinkTable, c.Table, c.LinkCol, c.NameCol, placeholders(len(members)), c.bookOrder(key))
	rows, err := calibreDB.Query(query, toArgs(members)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lib []listEntry
	for rows.Next() {
		var e listEntry
		var sortKey sql.NullString
		if err := rows.Scan(&e.UUID, &e.Title, &e.Authors, &e.PubDate, &e.Path, &e.LastModified, &sortKey, &e.SeriesIndex); err != nil {
			continue
		}
		e.LastModified = coverStamp(e.LastModified)
		uuidPathIndex.Store(e.UUID, e.Path)
		e.sort, e.index, e.date = sortKey.String, e.SeriesIndex, seriesDate(e.PubDate)
		lib = append(lib, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	series := c.Key == "series"
	ph := make([]listEntry, 0, len(phs))
	for _, p := range phs {
		e := listEntry{Book: p.book(), sort: titleSort(p.Meta.Title), index: p.index(), date: seriesDate(p.Meta.Date)}
		ph = append(ph, e)
	}
	slices.SortStableFunc(ph, func(a, b listEntry) int { return compareListEntries(series, &a, &b) })

	// Merge: the library's book first where they tie. Sorted otherwise, the
	// placeholders (which have no added date, author sort, …) go last.
	merged := key == c.sorts()[0]
	all := make([]Book, 0, len(lib)+len(ph))
	i, j := 0, 0
	for i < len(lib) || j < len(ph) {
		if j == len(ph) || (i < len(lib) && (!merged || compareListEntries(series, &lib[i], &ph[j]) <= 0)) {
			all = append(all, lib[i].Book)
			i++
		} else {
			all = append(all, ph[j].Book)
			j++
		}
	}
	from := min((page-1)*booksPerPage, len(all))
	to := min(from+booksPerPage, len(all))
	return all[from:to], nil
}

// nextPlaceholder: the placeholder of series name numbered after index
// (the one finished) and before next (the library's next book; +Inf for
// none), whose number no library book in the series has (held).
func nextPlaceholder(name string, after, before float64, held map[float64]bool) *placeholder {
	var best *placeholder
	for _, p := range allPlaceholders() {
		if !strings.EqualFold(p.Meta.Series, name) {
			continue
		}
		i := p.index()
		if i <= after || i >= before || held[i] {
			continue
		}
		if best == nil || i < best.index() {
			best = p
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// Pages
// ---------------------------------------------------------------------------

// PlaceholderPageData is for placeholder.html: one placeholder, or a new
// one (New) with what the link said to start with.
type PlaceholderPageData struct {
	Title   string
	New     bool
	UUID    string
	Meta    OPFMetadata
	Index   string // its number, as typed
	Authors string // "A & B"
	Tags    string // "a, b"
	ISBN    string
	// HardcoverField: the editor's Hardcover field (hcPlaceholderField).
	HardcoverField string
	// HardcoverSync: the signed-in user's Sync to Hardcover (as on a
	// book's page; hardcover_placeholder.go); nil when not linked.
	HardcoverSync *bookHardcover
	// HardcoverURL: its page on Hardcover (hardcover_ids.go), or "".
	HardcoverURL string
	CoverURL     string
	CoverHue     int
	Source       string
	Writable     bool
	// SignedIn: Save is offered; Saved: the user saved it (notifications.go).
	SignedIn, Saved bool
	// Links for the metadata shown.
	SeriesHref    string
	AuthorLinks   []placeholderLink
	PublisherHref string
}

type placeholderLink struct{ Name, Href string }

// MissingPageData is for missing.html.
type MissingPageData struct {
	Title  string
	Series []missingSeries // placeholders by series, those with none last
	Gaps   []seriesGap
	// Hardcover: series linked to Hardcover, when it is set up (hardcover.go).
	Hardcover *MissingHardcover
	Count     int
	Writable  bool
	// Removed: placeholders removed lately, newest first, to check what
	// replaced them and restore one replaced in error.
	Removed []removedPlaceholder
}

// removedPlaceholder is a removed placeholder, for /missing.
type removedPlaceholder struct {
	UUID      string
	Title     string
	Authors   string
	Series    string
	Index     string
	Why       string // whyTitle…, "" when not known (removed before it was kept)
	By        string // who, "" for nobody (the server, or not known)
	When      string
	BookUUID  string // the library book that replaced it, "" for none
	BookTitle string // "" when it's no longer in the library
}

func (r removedPlaceholder) BookHref() string { return "/book/get/" + url.PathEscape(r.BookUUID) }

// removedPlaceholdersShown: how many /missing lists.
const removedPlaceholdersShown = 50

// removedPlaceholders: the placeholders removed last, newest first.
func removedPlaceholders(limit int) []removedPlaceholder {
	if appDB == nil {
		return nil
	}
	rows, err := appDB.Query(`SELECT uuid, meta, deleted_at, replaced_by, replaced_why, replaced_user
		FROM placeholders WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC LIMIT ?`, limit)
	if err != nil {
		log.Println("removed placeholders:", err)
		return nil
	}
	defer rows.Close()
	var out []removedPlaceholder
	for rows.Next() {
		var r removedPlaceholder
		var meta string
		var at int64
		if err := rows.Scan(&r.UUID, &meta, &at, &r.BookUUID, &r.Why, &r.By); err != nil {
			log.Println("removed placeholders:", err)
			continue
		}
		var m OPFMetadata
		if json.Unmarshal([]byte(meta), &m) != nil {
			continue
		}
		r.Title, r.Authors, r.Series, r.Index = m.Title, strings.Join(m.Creators, " & "), m.Series, m.SeriesIndex
		r.When = time.Unix(at, 0).Format("2006-01-02 15:04")
		if r.BookUUID != "" {
			r.BookTitle = bookTitle(r.BookUUID)
		}
		out = append(out, r)
	}
	return out
}

// POST /api/placeholders/{uuid}/restore: a removed placeholder back, as it
// was (replaced in error, say). In the edit log, so it can be undone.
func placeholderRestoreHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("uuid")
	if placeholderByUUID(id) != nil {
		w.WriteHeader(http.StatusNoContent) // there already
		return
	}
	var meta, cover string
	err := appDB.QueryRow(`SELECT meta, cover FROM placeholders WHERE uuid = ? AND deleted_at IS NOT NULL`, id).Scan(&meta, &cover)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "no such placeholder", http.StatusNotFound)
		return
	}
	snap := &placeholderSnap{Cover: cover}
	if err == nil {
		err = json.Unmarshal([]byte(meta), &snap.Meta)
	}
	if err == nil {
		err = putPlaceholder(id, snap)
	}
	if err != nil {
		log.Println("placeholder restore:", err)
		http.Error(w, "could not restore it", http.StatusInternalServerError)
		return
	}
	logEdit(currentUser(r), "restore placeholder", logKindPlaceholder, id, snap.Meta.Title, nil, snap)
	w.WriteHeader(http.StatusNoContent)
}

type missingSeries struct {
	Name  string // "" for placeholders in no series
	Href  string
	Books []PlaceholderRow
}

// PlaceholderRow is one line of /missing.
type PlaceholderRow struct {
	UUID, Title, Authors, Index, Date, Href, CoverURL, Source string
}

func placeholderISBN(m OPFMetadata) string {
	for _, id := range m.Identifiers {
		if strings.EqualFold(id.Scheme, "isbn") {
			return id.Value
		}
	}
	return ""
}

func placeholderPageData(p *placeholder, r *http.Request) PlaceholderPageData {
	d := PlaceholderPageData{
		Title:          p.Meta.Title + " — " + SiteTitle,
		UUID:           p.UUID,
		Meta:           p.Meta,
		Index:          p.Meta.SeriesIndex,
		Authors:        p.authors(),
		Tags:           strings.Join(p.Meta.Subjects, ", "),
		ISBN:           placeholderISBN(p.Meta),
		HardcoverField: hcPlaceholderField(p.Meta),
		HardcoverURL:   hcMetaURL(p.Meta),
		CoverURL:       p.coverURL(),
		CoverHue:       coverHue(p.UUID),
		Source:         p.Source,
		Writable:       canEditLibrary(r),
	}
	if u := currentUser(r); u != nil {
		d.SignedIn = true
		status, _ := readStatus(u.ID, p.UUID)
		d.Saved = status == "wanted"
		if hardcoverSyncEnabled() && hcSync.IsLinked(u.ID) {
			d.HardcoverSync = &bookHardcover{Shared: hcSync.Shared(u.ID, p.UUID), All: hcSync.LoadShare(u.ID).All}
		}
	}
	if p.Meta.Series != "" {
		d.SeriesHref = categoryByKey("series").href(p.Meta.Series)
	}
	for _, a := range p.Meta.Creators {
		d.AuthorLinks = append(d.AuthorLinks, placeholderLink{a, categoryByKey("author").href(a)})
	}
	if p.Meta.Publisher != "" {
		d.PublisherHref = categoryByKey("publisher").href(p.Meta.Publisher)
	}
	return d
}

// placeholderReplacedBy: the library book that replaced removed placeholder
// id, if it's still in the library; else "".
func placeholderReplacedBy(id string) string {
	if appDB == nil || id == "" {
		return ""
	}
	var u string
	if appDB.QueryRow(`SELECT replaced_by FROM placeholders WHERE uuid = ? AND deleted_at IS NOT NULL`, id).Scan(&u) != nil || u == "" {
		return ""
	}
	// metadata.db, not the catalog, which may not have the book yet.
	var one int
	if calibreDB == nil || calibreDB.QueryRow(`SELECT 1 FROM books WHERE uuid = ?`, u).Scan(&one) != nil {
		return ""
	}
	return u
}

// GET /missing/{uuid}: a placeholder's page (an editor for admins), or for
// an htmx request its view in the book popup.
func placeholderPageHandler(w http.ResponseWriter, r *http.Request) {
	p := placeholderByUUID(r.PathValue("uuid"))
	if p == nil {
		// Added to the library since: its book's page, if it's there.
		if u := placeholderReplacedBy(r.PathValue("uuid")); u != "" {
			http.Redirect(w, r, "/book/get/"+url.PathEscape(u), http.StatusSeeOther)
			return
		}
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, no-cache") // Save shows the user's own state
	w.Header().Set("Vary", "HX-Request")
	if r.Header.Get("HX-Request") != "" {
		// The book popup, stepping onto it from a book beside it (modal.ts).
		logErr(templates.ExecuteTemplate(w, "placeholder-popup", placeholderPageData(p, r)), "placeholder popup")
		return
	}
	logErr(templates.ExecuteTemplate(w, "placeholder.html", placeholderPageData(p, r)), "placeholder page")
}

// GET /missing/new?series=&index=&authors=&title=: the editor for a new one.
func newPlaceholderPageHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := PlaceholderPageData{
		Title:    "New placeholder — " + SiteTitle,
		New:      true,
		Meta:     OPFMetadata{Title: q.Get("title"), Series: q.Get("series")},
		Index:    q.Get("index"),
		Authors:  q.Get("authors"),
		Writable: true,
	}
	logErr(templates.ExecuteTemplate(w, "placeholder.html", d), "new placeholder page")
}

// GET /missing: every placeholder, and the gaps in the library's series.
func missingPageHandler(w http.ResponseWriter, r *http.Request) {
	phs := allPlaceholders()
	d := MissingPageData{Title: "Missing books — " + SiteTitle, Count: len(phs), Writable: true, Gaps: seriesGaps(), Hardcover: missingHardcover(),
		Removed: removedPlaceholders(removedPlaceholdersShown)}
	by := map[string]*missingSeries{}
	for _, p := range phs {
		k := strings.ToLower(p.Meta.Series)
		s := by[k]
		if s == nil {
			s = &missingSeries{Name: p.Meta.Series}
			if s.Name != "" {
				s.Href = categoryByKey("series").href(s.Name)
			}
			by[k] = s
		}
		s.Books = append(s.Books, PlaceholderRow{
			UUID: p.UUID, Title: p.Meta.Title, Authors: p.authors(), Index: p.Meta.SeriesIndex,
			Date: p.Meta.Date, Href: p.href(), CoverURL: p.coverURL(), Source: p.Source,
		})
	}
	for _, s := range by {
		if s.Name != "" {
			slices.SortStableFunc(s.Books, func(a, b PlaceholderRow) int {
				fa, _ := strconv.ParseFloat(a.Index, 64)
				fb, _ := strconv.ParseFloat(b.Index, 64)
				return cmp.Compare(fa, fb)
			})
		}
		d.Series = append(d.Series, *s)
	}
	slices.SortFunc(d.Series, func(a, b missingSeries) int {
		if (a.Name == "") != (b.Name == "") {
			if a.Name == "" {
				return 1
			}
			return -1
		}
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	w.Header().Set("Cache-Control", "no-cache")
	logErr(templates.ExecuteTemplate(w, "missing.html", d), "missing page")
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

// placeholderMetaFromRequest: the editor's form, as for a library book
// (ApplyMetadataRequest), plus an ISBN and its Hardcover book (an id or a
// hardcover.app link; nil when the form didn't have the field, "" to
// clear it: hcPlaceholderFieldIDs).
func placeholderMetaFromRequest(r *http.Request) (OPFMetadata, *string, error) {
	var req struct {
		ApplyMetadataRequest
		ISBN      string  `json:"isbn"`
		Hardcover *string `json:"hardcover"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
		return OPFMetadata{}, nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if req.Hardcover != nil && len(*req.Hardcover) > 500 {
		return OPFMetadata{}, nil, errors.New("Hardcover: give the book's id, or its link on hardcover.app")
	}
	m := metadataFromRequest(req.ApplyMetadataRequest)
	if strings.TrimSpace(req.Creators) == "" {
		m.Creators = nil
	}
	if isbn := strings.TrimSpace(req.ISBN); isbn != "" {
		m.Identifiers = []opfIdentifier{{Scheme: "ISBN", Value: isbn}}
	} else {
		m.Identifiers = nil
	}
	if strings.TrimSpace(req.Date) == "" {
		m.Date = ""
	}
	m, err := cleanPlaceholderMeta(m)
	return m, req.Hardcover, err
}

// placeholderHardcoverField sets m's Hardcover identifiers from the
// editor's field (nil: left as they are). ed: the book Hardcover said it
// is, when it was asked.
func placeholderHardcoverField(r *http.Request, m *OPFMetadata, field *string) (*hardcover.Edition, error) {
	if field == nil {
		return nil, nil
	}
	ids, ed, err := hcPlaceholderFieldIDs(r.Context(), *field)
	if err != nil {
		return nil, err
	}
	m.Identifiers = append(withoutHCIdentifiers(m.Identifiers), ids...)
	return ed, nil
}

// POST /api/placeholders: a new placeholder.
func placeholderCreateHandler(w http.ResponseWriter, r *http.Request) {
	m, hc, err := placeholderMetaFromRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := placeholderHardcoverField(r, &m, hc); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p := &placeholder{Meta: m}
	if err := savePlaceholder(p, currentUser(r)); err != nil {
		log.Println("placeholder:", err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	logEdit(currentUser(r), "add placeholder", logKindPlaceholder, p.UUID, p.Meta.Title, nil, snapPlaceholder(p))
	writeJSON(w, http.StatusOK, map[string]string{"uuid": p.UUID, "url": p.href()})
}

func placeholderFromRequest(w http.ResponseWriter, r *http.Request) *placeholder {
	p := placeholderByUUID(r.PathValue("uuid"))
	if p == nil {
		http.Error(w, "no such placeholder", http.StatusNotFound)
		return nil
	}
	cp := *p // the cache is shared: change a copy
	return &cp
}

// PUT /api/placeholders/{uuid}: its new metadata.
func placeholderUpdateHandler(w http.ResponseWriter, r *http.Request) {
	p := placeholderFromRequest(w, r)
	if p == nil {
		return
	}
	m, hc, err := placeholderMetaFromRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	before := snapPlaceholder(p)
	// The form has only the ISBN: the other identifiers (Hardcover's) stay.
	var kept []opfIdentifier
	for _, id := range p.Meta.Identifiers {
		if !strings.EqualFold(id.Scheme, "isbn") {
			kept = append(kept, id)
		}
	}
	m.Identifiers = append(kept, m.Identifiers...)
	// Its Hardcover book: an editor's word, for everyone who syncs it
	// (hardcover_placeholder.go).
	ed, err := placeholderHardcoverField(r, &m, hc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hcChanged := !hcSameIdentifiers(p.Meta, m)
	p.Meta = m
	if err := savePlaceholder(p, nil); err != nil {
		log.Println("placeholder:", err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if hcChanged {
		hcSync.BookChanged(p.UUID, ed, 0)
	}
	logEdit(currentUser(r), "edit placeholder", logKindPlaceholder, p.UUID, p.Meta.Title, before, snapPlaceholder(p))
	writeJSON(w, http.StatusOK, map[string]string{"uuid": p.UUID, "url": p.href()})
}

// DELETE /api/placeholders/{uuid}
func placeholderDeleteHandler(w http.ResponseWriter, r *http.Request) {
	p := placeholderByUUID(r.PathValue("uuid"))
	if err := removePlaceholder(r.PathValue("uuid"), "", whyByHand, currentUser(r)); errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "no such placeholder", http.StatusNotFound)
		return
	} else if err != nil {
		log.Println("placeholder:", err)
		http.Error(w, "could not delete", http.StatusInternalServerError)
		return
	}
	logEdit(currentUser(r), "remove placeholder", logKindPlaceholder, p.UUID, p.Meta.Title, snapPlaceholder(p), nil)
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/placeholders/{uuid}/cover (multipart "image", a picture or an
// SVG) sets its cover; DELETE removes it.
func placeholderCoverHandler(w http.ResponseWriter, r *http.Request) {
	p := placeholderFromRequest(w, r)
	if p == nil {
		return
	}
	old := p.Cover
	before := snapPlaceholder(p)
	if r.Method == http.MethodDelete {
		p.Cover = ""
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, coverUploadMaxBytes+1<<20)
		if err := r.ParseMultipartForm(coverUploadMaxBytes); err != nil {
			http.Error(w, "couldn't read the upload: "+err.Error(), http.StatusBadRequest)
			return
		}
		data, err := formImageFile(r, "image")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		img, err := aggregateImage(data)
		if err != nil {
			http.Error(w, "not a picture this library takes: "+err.Error(), http.StatusBadRequest)
			return
		}
		if p.Cover, err = saveHashedImage(placeholderCoversDir, img); err != nil {
			log.Println("placeholder cover:", err)
			http.Error(w, "could not save the picture", http.StatusInternalServerError)
			return
		}
	}
	if err := savePlaceholder(p, nil); err != nil {
		log.Println("placeholder:", err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if old != p.Cover {
		logEdit(currentUser(r), "placeholder cover", logKindPlaceholder, p.UUID, p.Meta.Title, before, snapPlaceholder(p))
		removeUnusedPlaceholderCover(old)
	}
	writeJSON(w, http.StatusOK, map[string]string{"cover": p.coverURL()})
}

// POST /api/placeholders/{uuid}/files (multipart "files"): its book's
// files. They wait in the import queue with the placeholder's metadata;
// the answer's url is their review page.
func placeholderFilesHandler(w http.ResponseWriter, r *http.Request) {
	p := placeholderFromRequest(w, r)
	if p == nil {
		return
	}
	if imports == nil {
		http.Error(w, "the import queue isn't available", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "couldn't read the upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		http.Error(w, "no files", http.StatusBadRequest)
		return
	}
	tmp, err := os.MkdirTemp("", "calibrefast-placeholder-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmp)
	seen := map[string]bool{}
	var paths []string
	for _, fh := range files {
		name := filepath.Base(strings.ReplaceAll(fh.Filename, `\`, "/"))
		format := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
		if !hasBookExt("." + format) {
			http.Error(w, fmt.Sprintf("%s isn't a book file this library takes (EPUB, PDF, MOBI, AZW3, DJVU)", name), http.StatusBadRequest)
			return
		}
		if seen[format] {
			http.Error(w, fmt.Sprintf("more than one %s: a book has one file per format", strings.ToUpper(format)), http.StatusBadRequest)
			return
		}
		seen[format] = true
		if strings.HasPrefix(name, ".") || name == "" {
			name = "book." + format
		}
		dst := filepath.Join(tmp, name)
		if err := saveMultipartFile(fh, dst); err != nil {
			http.Error(w, "couldn't save "+name+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		paths = append(paths, dst)
	}
	it, err := imports.stageForPlaceholder(paths, p)
	if err != nil {
		log.Println("placeholder files:", err)
		http.Error(w, "couldn't queue the files: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": "/import/" + it.ID})
}

// withPlaceholder: the metadata of a book from the import queue, with what
// the placeholder says in place of the files' own where it says anything.
func withPlaceholder(m OPFMetadata, p *placeholder) OPFMetadata {
	pm := p.Meta
	if pm.Title != "" {
		m.Title = pm.Title
	}
	if len(pm.Creators) > 0 {
		m.Creators = slices.Clone(pm.Creators)
	}
	if pm.Description != "" {
		m.Description = pm.Description
	}
	if pm.Publisher != "" {
		m.Publisher = pm.Publisher
	}
	if pm.Date != "" {
		m.Date = pm.Date
	}
	if len(pm.Languages) > 0 {
		m.Languages = slices.Clone(pm.Languages)
	}
	if len(pm.Subjects) > 0 {
		m.Subjects = slices.Clone(pm.Subjects)
	}
	if isbn := placeholderISBN(pm); isbn != "" {
		var ids []opfIdentifier
		for _, id := range m.Identifiers {
			if !strings.EqualFold(id.Scheme, "isbn") {
				ids = append(ids, id)
			}
		}
		m.Identifiers = append(ids, opfIdentifier{Scheme: "ISBN", Value: isbn})
	}
	// Where it is on Hardcover, so the book links there too.
	if ids := hcIdentifiers(pm); len(ids) > 0 {
		m.Identifiers = withIdentifiers(m.Identifiers, ids)
	}
	if pm.Series != "" {
		m.Series, m.SeriesIndex = pm.Series, pm.SeriesIndex
		m.SyncSeriesMeta()
	}
	return m
}

func registerPlaceholderRoutes() {
	http.HandleFunc("GET /missing", requireEditor(missingPageHandler))
	http.HandleFunc("GET /missing/new", requireEditor(newPlaceholderPageHandler))
	http.HandleFunc("GET /missing/{uuid}", placeholderPageHandler)
	http.HandleFunc("GET /"+placeholderCoversDir+"/{file}", func(w http.ResponseWriter, r *http.Request) {
		serveHashedImage(w, r, placeholderCoversDir)
	})
	http.HandleFunc("POST /api/placeholders", requireEditor(placeholderCreateHandler))
	http.HandleFunc("PUT /api/placeholders/{uuid}", requireEditor(placeholderUpdateHandler))
	http.HandleFunc("DELETE /api/placeholders/{uuid}", requireEditor(placeholderDeleteHandler))
	http.HandleFunc("POST /api/placeholders/{uuid}/cover", requireEditor(placeholderCoverHandler))
	http.HandleFunc("DELETE /api/placeholders/{uuid}/cover", requireEditor(placeholderCoverHandler))
	http.HandleFunc("POST /api/placeholders/{uuid}/files", requireEditor(placeholderFilesHandler))
	http.HandleFunc("POST /api/placeholders/{uuid}/restore", requireEditor(placeholderRestoreHandler))
}
