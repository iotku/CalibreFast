package main

// Series identified on Hardcover (hardcover.app). An editor links a series
// (as browsed, after groups) to a Hardcover series; its book list is
// fetched then and whenever the editor asks again, and kept with when it
// was fetched. Matching it against the library (hardcover_match.go) gives
// the review at /missing/hardcover?series=: books here, books here without
// the series tag, and books not here, which can become placeholders.
// Nothing is written to the library or the placeholders until the editor
// applies the review.
//
// Every request to Hardcover goes through one queue (hardcover_jobs), run
// by one worker, within the plan's limits (hardcover.Budget, its use saved
// in hardcover_usage). When the limits are used up, jobs are held, not
// failed, and run when they are restored (by midnight UTC for the daily
// one). Nothing is asked of Hardcover except when an editor clicks: one
// series, or the series of one author they ticked (at most
// hcBatchMax): never the whole library.
//
// Only the series' name, the ISBNs of its books in the library and
// Hardcover ids are sent, with the server's API token
// (CF_HARDCOVER_TOKEN), which is never logged or shown. What Hardcover
// answers is untrusted: placeholders made from it go through
// cleanPlaceholderMeta, and pages show it through html/template or DOM APIs.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/isbn"
)

var (
	// noHardcover (-no-hardcover) turns off Hardcover only.
	noHardcover bool
	// hcClient asks Hardcover; nil without CF_HARDCOVER_TOKEN.
	hcClient *hardcover.Client
	// hcNow is the clock (tests).
	hcNow = time.Now
)

// hcBatchMax: the most series one batch may hold.
const hcBatchMax = 25

// hcMaxParts: the most Hardcover series one series may be linked to.
const hcMaxParts = 60

// hcStaleAfter: a series' list is shown as stale after this.
const hcStaleAfter = 30 * 24 * time.Hour

func configureHardcover() {
	token := os.Getenv("CF_HARDCOVER_TOKEN")
	hcClient = nil
	switch {
	case noLookups:
		return
	case noHardcover:
		log.Println("-no-hardcover: series won't be identified on Hardcover")
		return
	case strings.TrimSpace(token) == "":
		return
	}
	hcClient = hardcover.New(hardcover.Config{
		Token:     token,
		UserAgent: lookupUserAgent,
		Budget:    hardcover.FreePlan(),
	})
}

// hardcoverEnabled: series can be identified on Hardcover.
func hardcoverEnabled() bool { return !noLookups && !noHardcover && hcClient != nil }

func requireHardcover(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !hardcoverEnabled() {
			http.Error(w, "Hardcover isn't set up on this server (CF_HARDCOVER_TOKEN).", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// ---------------------------------------------------------------------------
// The library, as matching sees it
// ---------------------------------------------------------------------------

// hcSeriesInfo: a series as browsed: its name (its group's, if any), its
// members, and the member to write onto books and placeholders (the one
// with the most books). ok is false when no library book is in it.
type hcSeriesInfo struct {
	Name    string
	Members []string
	Write   string
}

func hcSeries(name string) (hcSeriesInfo, bool) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return hcSeriesInfo{}, false
	}
	g, members, _ := groups.resolve("series", name)
	c := currentCatalog()
	count := map[string]int{}
	for i := range c.books {
		b := &c.books[i]
		if b.Removed || b.Series == 0 {
			continue
		}
		s := c.seriesNames[b.Series]
		if hcInMembers(s, members) {
			count[s]++
		}
	}
	if len(count) == 0 {
		return hcSeriesInfo{}, false
	}
	info := hcSeriesInfo{Name: g, Members: members}
	best := -1
	for s, n := range count {
		if n > best || (n == best && s < info.Write) {
			info.Write, best = s, n
		}
	}
	return info, true
}

// hcTitleIndex: the catalog's books by title key (with and without the
// subtitle), and by book id; built
// again when the catalog changes.
type hcTitleIndex struct {
	cat     *catalog
	byTitle map[string][]int32
	byID    map[int64]int32
}

var hcTitleIndexCache struct {
	sync.Mutex
	idx *hcTitleIndex
}

func currentHCTitleIndex() *hcTitleIndex {
	cat := currentCatalog()
	hcTitleIndexCache.Lock()
	defer hcTitleIndexCache.Unlock()
	if idx := hcTitleIndexCache.idx; idx != nil && idx.cat == cat {
		return idx
	}
	idx := &hcTitleIndex{cat: cat, byTitle: map[string][]int32{}, byID: map[int64]int32{}}
	for i := range cat.books {
		b := &cat.books[i]
		if b.Removed || b.UUID == "" {
			continue
		}
		full, short := hcFullTitleKey(b.Title), hcTitleKey(b.Title)
		idx.byTitle[full] = append(idx.byTitle[full], int32(i))
		if short != full {
			idx.byTitle[short] = append(idx.byTitle[short], int32(i))
		}
		idx.byID[b.ID] = int32(i)
	}
	hcTitleIndexCache.idx = idx
	return idx
}

func (x *hcTitleIndex) libBook(slot int32) hcLibBook {
	b := &x.cat.books[slot]
	lb := hcLibBook{UUID: b.UUID, Title: b.Title}
	for _, id := range b.AuthorIDs {
		lb.Authors = append(lb.Authors, x.cat.authorNames[id])
	}
	if b.Series != 0 {
		lb.Series, lb.Index = x.cat.seriesNames[b.Series], b.SeriesIndex
	}
	return lb
}

// hcLibraryFor: the library for matching d against series s.
func hcLibraryFor(s hcSeriesInfo, d *hardcover.SeriesDetail) (hcLibrary, error) {
	x := currentHCTitleIndex()
	lib := hcLibrary{members: s.Members, byISBN: map[string][]hcLibBook{}, refused: hcRefused(s.Name)}
	for i := range x.cat.books {
		b := &x.cat.books[i]
		if !b.Removed && b.Series != 0 && hcInMembers(x.cat.seriesNames[b.Series], s.Members) {
			lib.inSeries = append(lib.inSeries, x.libBook(int32(i)))
		}
	}
	lib.byTitle = func(key string) []hcLibBook {
		var out []hcLibBook
		for _, slot := range x.byTitle[key] {
			out = append(out, x.libBook(slot))
		}
		return out
	}
	if d == nil || calibreDB == nil {
		return lib, nil
	}
	var all []string
	for _, e := range d.Entries {
		all = append(all, e.ISBNs...)
	}
	forms := isbnForms(all)
	for chunk := range slices.Chunk(forms, 400) {
		args := make([]any, len(chunk))
		for i, f := range chunk {
			args[i] = f
		}
		rows, err := calibreDB.Query(`SELECT book, val FROM identifiers WHERE type = 'isbn' AND val IN (?`+
			strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return lib, err
		}
		for rows.Next() {
			var id int64
			var val string
			if err := rows.Scan(&id, &val); err != nil {
				rows.Close()
				return lib, err
			}
			slot, ok := x.byID[id]
			n := isbn.Normalize(val)
			if !ok || n == "" {
				continue
			}
			lib.byISBN[n] = append(lib.byISBN[n], x.libBook(slot))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return lib, err
		}
	}
	return lib, nil
}

// hcSeriesISBNs: the ISBN-13s of the series' library books (at most 50),
// for finding the series on Hardcover.
func hcSeriesISBNs(s hcSeriesInfo) ([]string, error) {
	if calibreDB == nil {
		return nil, nil
	}
	c := currentCatalog()
	var ids []any
	for i := range c.books {
		b := &c.books[i]
		if !b.Removed && b.Series != 0 && hcInMembers(c.seriesNames[b.Series], s.Members) {
			ids = append(ids, b.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 400 {
		ids = ids[:400]
	}
	rows, err := calibreDB.Query(`SELECT val FROM identifiers WHERE type = 'isbn' AND book IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, ids...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if n := isbn.Normalize(v); n != "" && !slices.Contains(out, n) && len(out) < 50 {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

// hcLink is a series linked to Hardcover: to one series there, or to
// several parts of one (hardcover_series_parts).
type hcLink struct {
	Name         string
	HCID         int64 // the first part
	Slug, HCName string
	IdentifiedAt int64
	CheckedAt    int64
	Snapshot     *hardcover.SeriesDetail
	Parts        []hcPart
}

// hcPart is one Hardcover series a library series is linked to.
type hcPart struct {
	ID     int64
	Slug   string
	Name   string
	Author string
	Books  int
}

func (p hcPart) url() string {
	if !hardcover.ValidSlug(p.Slug) {
		return ""
	}
	return "https://hardcover.app/series/" + p.Slug
}

// ids: the Hardcover series to fetch.
func (l *hcLink) ids() []int64 {
	var ids []int64
	for _, p := range l.Parts {
		ids = append(ids, p.ID)
	}
	if len(ids) == 0 && l.HCID > 0 {
		ids = []int64{l.HCID}
	}
	return ids
}

// loadParts reads the link's parts.
func (l *hcLink) loadParts() error {
	rows, err := appDB.Query(`SELECT hc_id, hc_slug, hc_name, author, books FROM hardcover_series_parts WHERE name = ? ORDER BY books DESC, hc_id`, l.Name)
	if err != nil {
		return err
	}
	defer rows.Close()
	l.Parts = nil
	for rows.Next() {
		var p hcPart
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Author, &p.Books); err != nil {
			return err
		}
		l.Parts = append(l.Parts, p)
	}
	return rows.Err()
}

func (l *hcLink) hardcoverURL() string {
	if !hardcover.ValidSlug(l.Slug) {
		return ""
	}
	return "https://hardcover.app/series/" + l.Slug
}

func (l *hcLink) stale() bool {
	return l.CheckedAt == 0 || hcNow().Sub(time.Unix(l.CheckedAt, 0)) > hcStaleAfter
}

func scanHCLink(sc interface{ Scan(...any) error }) (*hcLink, error) {
	var l hcLink
	var snap string
	if err := sc.Scan(&l.Name, &l.HCID, &l.Slug, &l.HCName, &l.IdentifiedAt, &l.CheckedAt, &snap); err != nil {
		return nil, err
	}
	if snap != "" {
		var d hardcover.SeriesDetail
		if err := json.Unmarshal([]byte(snap), &d); err == nil {
			l.Snapshot = &d
		}
	}
	return &l, nil
}

const hcLinkCols = `name, hc_id, hc_slug, hc_name, identified_at, checked_at, snapshot`

// hcLinkFor: the series' link, or nil.
func hcLinkFor(name string) (*hcLink, error) {
	l, err := scanHCLink(appDB.QueryRow(`SELECT `+hcLinkCols+` FROM hardcover_series WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l, l.loadParts()
}

func hcAllLinks() ([]*hcLink, error) {
	rows, err := appDB.Query(`SELECT ` + hcLinkCols + ` FROM hardcover_series ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*hcLink
	for rows.Next() {
		l, err := scanHCLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// hcSaveLink links name to d's series (all its parts) with d as its list.
// What was dismissed stays dismissed: Hardcover's book ids are its own,
// whichever of its series lists them.
func hcSaveLink(name string, d *hardcover.SeriesDetail, by *User) error {
	snap, err := json.Marshal(d)
	if err != nil {
		return err
	}
	var uid any
	if by != nil {
		uid = by.ID
	}
	now := hcNow().Unix()
	tx, err := appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	parts := d.AllParts()
	var oldIDs []int64
	if rows, qerr := tx.Query(`SELECT hc_id FROM hardcover_series_parts WHERE name = ?`, name); qerr == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				oldIDs = append(oldIDs, id)
			}
		}
		rows.Close()
	}
	same := len(oldIDs) > 0 && slices.ContainsFunc(parts, func(p hardcover.Series) bool { return slices.Contains(oldIDs, p.ID) })
	var old int64
	err = tx.QueryRow(`SELECT hc_id FROM hardcover_series WHERE name = ?`, name).Scan(&old)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.Exec(`INSERT INTO hardcover_series (name, hc_id, hc_slug, hc_name, identified_by, identified_at, checked_at, snapshot)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, name, d.ID, d.Slug, d.Name, uid, now, now, string(snap))
	case err != nil:
	case !same && old != d.ID:
		_, err = tx.Exec(`UPDATE hardcover_series SET hc_id = ?, hc_slug = ?, hc_name = ?, identified_by = ?, identified_at = ?, checked_at = ?, snapshot = ? WHERE name = ?`,
			d.ID, d.Slug, d.Name, uid, now, now, string(snap), name)
	default:
		_, err = tx.Exec(`UPDATE hardcover_series SET hc_id = ?, hc_slug = ?, hc_name = ?, checked_at = ?, snapshot = ? WHERE name = ?`,
			d.ID, d.Slug, d.Name, now, string(snap), name)
	}
	if err == nil {
		_, err = tx.Exec(`DELETE FROM hardcover_series_parts WHERE name = ?`, name)
	}
	for _, p := range parts {
		if err != nil {
			break
		}
		_, err = tx.Exec(`INSERT INTO hardcover_series_parts (name, hc_id, hc_slug, hc_name, author, books) VALUES (?, ?, ?, ?, ?, ?)`,
			name, p.ID, p.Slug, p.Name, p.Author, p.BooksCount)
	}
	if err != nil {
		return err
	}
	defer seriesStatusChanged()
	return tx.Commit()
}

// hcUnlink forgets the series' link. Its dismissals stay, for when it's
// linked again.
func hcUnlink(name string) error {
	defer seriesStatusChanged()
	if _, err := appDB.Exec(`DELETE FROM hardcover_series_parts WHERE name = ?`, name); err != nil {
		return err
	}
	_, err := appDB.Exec(`DELETE FROM hardcover_series WHERE name = ?`, name)
	return err
}

// hcDismiss turns down the list's book ids for the series, for good (until
// an editor restores them); hcRestore brings them back, all when ids is
// empty.
func hcDismiss(name string, ids []int64) (int, error) {
	defer seriesStatusChanged()
	n := 0
	for _, id := range ids {
		res, err := appDB.Exec(`INSERT OR IGNORE INTO hardcover_dismissed (name, hc_book_id) VALUES (?, ?)`, name, id)
		if err != nil {
			return n, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	return n, nil
}

func hcRestore(name string, ids []int64) error {
	defer seriesStatusChanged()
	if len(ids) == 0 {
		_, err := appDB.Exec(`DELETE FROM hardcover_dismissed WHERE name = ?`, name)
		return err
	}
	for _, id := range ids {
		if _, err := appDB.Exec(`DELETE FROM hardcover_dismissed WHERE name = ? AND hc_book_id = ?`, name, id); err != nil {
			return err
		}
	}
	return nil
}

// hcStatuses: every linked series' share of its Hardcover list, by
// lower-cased name (seriesStatuses caches it).
func hcStatuses() map[string]seriesStatus {
	links, err := hcAllLinks()
	if err != nil {
		log.Println("hardcover status:", err)
		return nil
	}
	m := make(map[string]seriesStatus, len(links))
	for _, l := range links {
		s, ok := hcSeries(l.Name)
		if !ok || l.Snapshot == nil {
			continue
		}
		lib, err := hcLibraryFor(s, l.Snapshot)
		if err != nil {
			log.Println("hardcover status:", err)
			continue
		}
		rev := hcMatchSeries(l.Snapshot, lib, nil, hcDismissed(s.Name))
		m[strings.ToLower(s.Name)] = rev.status()
	}
	return m
}

func hcDismissed(name string) map[int64]bool {
	out := map[int64]bool{}
	rows, err := appDB.Query(`SELECT hc_book_id FROM hardcover_dismissed WHERE name = ?`, name)
	if err != nil {
		log.Println("hardcover:", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// hcRefused: the series' "Not a match"es, Hardcover book id -> uuids.
func hcRefused(name string) map[int64]map[string]bool {
	out := map[int64]map[string]bool{}
	if appDB == nil {
		return out
	}
	rows, err := appDB.Query(`SELECT hc_book_id, uuid FROM hardcover_not_match WHERE name = ?`, name)
	if err != nil {
		log.Println("hardcover:", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var uuid string
		if rows.Scan(&id, &uuid) == nil {
			if out[id] == nil {
				out[id] = map[string]bool{}
			}
			out[id][uuid] = true
		}
	}
	return out
}

// hcReviewFor matches the link's list against the library now.
func hcReviewFor(s hcSeriesInfo, l *hcLink) (hcReview, error) {
	if l == nil || l.Snapshot == nil {
		return hcReview{}, nil
	}
	lib, err := hcLibraryFor(s, l.Snapshot)
	if err != nil {
		return hcReview{}, err
	}
	return hcMatchSeries(l.Snapshot, lib, allPlaceholders(), hcDismissed(s.Name)), nil
}

// ---------------------------------------------------------------------------
// The queue
// ---------------------------------------------------------------------------

// Job kinds.
const (
	hcJobCandidates = "candidates" // find the series on Hardcover
	hcJobLink       = "link"       // link to payload.hcId and fetch its list
	hcJobRefresh    = "refresh"    // fetch a linked series' list again
	hcJobBatch      = "batch"      // refresh, or link when there's one sure candidate
)

// hcJob is a row of hardcover_jobs (TS: HardcoverJob in web/types.ts).
type hcJob struct {
	ID         int64           `json:"id"`
	Kind       string          `json:"kind"`
	Series     string          `json:"series"`
	Batch      string          `json:"batch,omitempty"`
	State      string          `json:"state"`
	NotBefore  int64           `json:"notBefore,omitempty"`
	CreatedAt  int64           `json:"createdAt"`
	FinishedAt int64           `json:"finishedAt,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	payload    string
}

type hcPayload struct {
	HCID  int64   `json:"hcId,omitempty"`  // one series (queued before parts were)
	HCIDs []int64 `json:"hcIds,omitempty"` // a series, or several parts of one
}

func (p hcPayload) ids() []int64 {
	if len(p.HCIDs) > 0 {
		return p.HCIDs
	}
	if p.HCID > 0 {
		return []int64{p.HCID}
	}
	return nil
}

// hcCandidates is a candidates job's result.
type hcCandidates struct {
	Candidates []hcCandidate `json:"candidates"`
}

type hcCandidate struct {
	hardcover.Series
	Hits int    `json:"hits,omitempty"` // library ISBNs found in it
	URL  string `json:"url,omitempty"`
	// Group: candidates with the same name (folded) share it: Hardcover
	// splits some series into one per author.
	Group string `json:"group"`
	// Pick: in the group that looks like this series (ticked in the picker).
	Pick bool `json:"pick,omitempty"`
	// Sure: a batch links Pick's group on its own.
	Sure bool `json:"sure,omitempty"`
	// Evidence: what ties it to the library's series.
	Evidence string `json:"evidence,omitempty"`
}

// hcLinkResult is a link, refresh or batch job's result.
type hcLinkResult struct {
	Linked      bool          `json:"linked"`
	HCName      string        `json:"hcName,omitempty"`
	Books       int           `json:"books,omitempty"`
	Parts       int           `json:"parts,omitempty"`  // Hardcover series it is linked to
	Review      string        `json:"review,omitempty"` // the review page
	NeedsChoice bool          `json:"needsChoice,omitempty"`
	Candidates  []hcCandidate `json:"candidates,omitempty"`
}

var hcQueue = struct {
	sync.Mutex
	wake    chan struct{}
	waiters map[int64][]chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
}{wake: make(chan struct{}, 1), waiters: map[int64][]chan struct{}{}}

const hcJobCols = `id, kind, series, batch, state, not_before, created_at, finished_at, result, error, payload`

func scanHCJob(sc interface{ Scan(...any) error }) (*hcJob, error) {
	var j hcJob
	var result string
	if err := sc.Scan(&j.ID, &j.Kind, &j.Series, &j.Batch, &j.State, &j.NotBefore, &j.CreatedAt, &j.FinishedAt, &result, &j.Error, &j.payload); err != nil {
		return nil, err
	}
	if result != "" {
		j.Result = json.RawMessage(result)
	}
	return &j, nil
}

func hcJobByID(id int64) (*hcJob, error) {
	return scanHCJob(appDB.QueryRow(`SELECT `+hcJobCols+` FROM hardcover_jobs WHERE id = ?`, id))
}

// hcEnqueue queues a job. A series already waiting for the same kind of
// job isn't queued twice: that job is answered.
func hcEnqueue(kind, series, batch string, p hcPayload, by *User) (int64, error) {
	payload, _ := json.Marshal(p)
	var id int64
	err := appDB.QueryRow(`SELECT id FROM hardcover_jobs WHERE kind = ? AND series = ? AND payload = ? AND state IN ('queued', 'held', 'running') ORDER BY id LIMIT 1`,
		kind, series, string(payload)).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var uid any
	if by != nil {
		uid = by.ID
	}
	res, err := appDB.Exec(`INSERT INTO hardcover_jobs (kind, series, payload, batch, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		kind, series, string(payload), batch, uid, hcNow().Unix())
	if err != nil {
		return 0, err
	}
	hcWake()
	return res.LastInsertId()
}

func hcWake() {
	select {
	case hcQueue.wake <- struct{}{}:
	default:
	}
}

// hcNotify tells those waiting for the job that it has moved on.
func hcNotify(id int64) {
	hcQueue.Lock()
	for _, ch := range hcQueue.waiters[id] {
		close(ch)
	}
	delete(hcQueue.waiters, id)
	hcQueue.Unlock()
}

// hcAwait waits up to d for the job to finish or be held, and answers it.
func hcAwait(ctx context.Context, id int64, d time.Duration) (*hcJob, error) {
	deadline := time.Now().Add(d)
	for {
		ch := make(chan struct{})
		hcQueue.Lock()
		hcQueue.waiters[id] = append(hcQueue.waiters[id], ch)
		hcQueue.Unlock()
		j, err := hcJobByID(id)
		if err != nil || j.State == "done" || j.State == "failed" || j.State == "held" || time.Now().After(deadline) {
			return j, err
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return j, nil
		case <-time.After(time.Until(deadline)):
			return hcJobByID(id)
		}
	}
}

func hcFinish(j *hcJob, state string, result any, msg string) {
	var res []byte
	if result != nil {
		res, _ = json.Marshal(result)
	}
	if _, err := appDB.Exec(`UPDATE hardcover_jobs SET state = ?, result = ?, error = ?, finished_at = ? WHERE id = ?`,
		state, string(res), msg, hcNow().Unix(), j.ID); err != nil {
		log.Println("hardcover queue:", err)
	}
	hcNotify(j.ID)
}

// hcStep runs the oldest job that may run at now. ran: it ran one (or
// held it); next: when the next held one may run (zero: none waiting).
func hcStep(ctx context.Context) (ran bool, next time.Time) {
	now := hcNow()
	j, err := scanHCJob(appDB.QueryRow(`SELECT `+hcJobCols+` FROM hardcover_jobs
		WHERE state IN ('queued', 'held') AND not_before <= ? ORDER BY id LIMIT 1`, now.Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		var nb sql.NullInt64
		_ = appDB.QueryRow(`SELECT MIN(not_before) FROM hardcover_jobs WHERE state = 'held'`).Scan(&nb)
		if nb.Valid {
			return false, time.Unix(nb.Int64, 0)
		}
		return false, time.Time{}
	}
	if err != nil {
		log.Println("hardcover queue:", err)
		return false, now.Add(time.Minute)
	}
	if _, err := appDB.Exec(`UPDATE hardcover_jobs SET state = 'running' WHERE id = ?`, j.ID); err != nil {
		log.Println("hardcover queue:", err)
		return false, now.Add(time.Minute)
	}
	hcNotify(j.ID)
	result, err := hcRun(ctx, j)
	defer hcSaveUsage()
	var wait *hardcover.WaitError
	switch {
	case ctx.Err() != nil:
		_, _ = appDB.Exec(`UPDATE hardcover_jobs SET state = 'queued' WHERE id = ?`, j.ID)
	case errors.As(err, &wait):
		// Held, as is every job waiting behind it: none could run before.
		_, _ = appDB.Exec(`UPDATE hardcover_jobs SET state = 'held', not_before = ?, error = ? WHERE state IN ('queued', 'held') AND not_before < ? OR id = ?`,
			wait.Until.Unix(), wait.Error(), wait.Until.Unix(), j.ID)
		hcNotifyHeld()
	case err != nil:
		hcFinish(j, "failed", nil, err.Error())
	default:
		hcFinish(j, "done", result, "")
	}
	return true, time.Time{}
}

// hcNotifyHeld wakes everyone waiting: jobs were held.
func hcNotifyHeld() {
	hcQueue.Lock()
	for id, chs := range hcQueue.waiters {
		for _, ch := range chs {
			close(ch)
		}
		delete(hcQueue.waiters, id)
	}
	hcQueue.Unlock()
}

// hcRun does a job's requests.
func hcRun(ctx context.Context, j *hcJob) (any, error) {
	if hcClient == nil {
		return nil, errors.New("Hardcover isn't set up on this server")
	}
	var p hcPayload
	_ = json.Unmarshal([]byte(j.payload), &p)
	s, ok := hcSeries(j.Series)
	if !ok {
		return nil, fmt.Errorf("no series %q in the library", j.Series)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	switch j.Kind {
	case hcJobCandidates:
		cands, err := hcFindCandidates(ctx, s)
		if err != nil {
			return nil, err
		}
		return hcCandidates{Candidates: cands}, nil
	case hcJobLink:
		return hcFetchAndLink(ctx, s, p.ids(), j)
	case hcJobRefresh, hcJobBatch:
		l, err := hcLinkFor(s.Name)
		if err != nil {
			return nil, err
		}
		if l != nil {
			return hcFetchAndLink(ctx, s, l.ids(), j)
		}
		if j.Kind == hcJobRefresh {
			return nil, errors.New("the series isn't linked to Hardcover")
		}
		cands, err := hcFindCandidates(ctx, s)
		if err != nil {
			return nil, err
		}
		var sure []int64
		for _, c := range cands {
			if c.Sure {
				sure = append(sure, c.ID)
			}
		}
		if len(sure) == 0 {
			return hcLinkResult{NeedsChoice: true, Candidates: cands, Review: hcReviewURL(s.Name)}, nil
		}
		return hcFetchAndLink(ctx, s, sure, j)
	}
	return nil, fmt.Errorf("unknown job %q", j.Kind)
}

func hcFetchAndLink(ctx context.Context, s hcSeriesInfo, ids []int64, j *hcJob) (any, error) {
	if len(ids) == 0 {
		return nil, errors.New("no Hardcover series given")
	}
	d, err := hcClient.SeriesBooks(ctx, ids...)
	if err != nil {
		return nil, err
	}
	var by *User
	var uid sql.NullInt64
	if appDB.QueryRow(`SELECT created_by FROM hardcover_jobs WHERE id = ?`, j.ID).Scan(&uid) == nil && uid.Valid {
		by = &User{ID: uid.Int64}
	}
	if err := hcSaveLink(s.Name, d, by); err != nil {
		return nil, err
	}
	return hcLinkResult{Linked: true, HCName: d.Name, Books: len(d.Entries), Parts: len(d.AllParts()), Review: hcReviewURL(s.Name)}, nil
}

// hcFindCandidates: Hardcover series that may be this one, best first,
// grouped by name: Hardcover splits some series into one per author, all of
// the same name and numbered as one, so a library series may be several
// Hardcover series.
//
// They come from a search by name, from the series the library's ISBNs
// are in, and from every series named like the likeliest ones (which finds
// the parts a search's ten results leave out). A candidate has evidence
// when the library's ISBNs are in it, its author wrote the series' books
// here, or the search lists one of their titles. The group of the same
// name as the library's series with evidence, else the group holding the
// most library ISBNs, is ticked (Pick). A batch links it on its own (Sure)
// only when its name is the library's, or when it holds at least 3 of the
// library's ISBNs and more than twice as many as any other group.
func hcFindCandidates(ctx context.Context, s hcSeriesInfo) ([]hcCandidate, error) {
	isbns, err := hcSeriesISBNs(s)
	if err != nil {
		return nil, err
	}
	byISBN, err := hcClient.SeriesByISBNs(ctx, isbns)
	if err != nil {
		return nil, err
	}
	var search []hardcover.Series
	for _, name := range slices.Compact(append([]string{s.Name}, s.Members...)) {
		got, err := hcClient.SearchSeries(ctx, name)
		if err != nil {
			return nil, err
		}
		search = append(search, got...)
		if len(search) >= 10 {
			break
		}
	}
	names := map[string]bool{}
	for _, m := range append([]string{s.Name}, s.Members...) {
		names[hcSeriesKey(m)] = true
	}

	var out []hcCandidate
	index := map[int64]int{}
	add := func(c hcCandidate) {
		if i, ok := index[c.ID]; ok {
			o := &out[i]
			o.Hits = max(o.Hits, c.Hits)
			if o.Author == "" {
				o.Author = c.Author
			}
			if len(o.Books) == 0 {
				o.Books = c.Books
			}
			o.BooksCount = max(o.BooksCount, c.BooksCount)
			return
		}
		index[c.ID] = len(out)
		out = append(out, c)
	}
	for _, c := range byISBN {
		add(hcCandidate{Series: c.Series, Hits: c.Hits})
	}
	for _, c := range search {
		add(hcCandidate{Series: c})
	}

	titles := map[string]bool{}
	for _, t := range hcSeriesTitles(s) {
		titles[hcFullTitleKey(t)] = true
	}
	authors := hcSeriesAuthors(s)

	// Every series named like the library's, or like one with evidence.
	var lookup []string
	for _, c := range out {
		if (names[hcSeriesKey(c.Name)] || hcEvidence(c, authors, titles) != "") && !slices.Contains(lookup, c.Name) && len(lookup) < 5 {
			lookup = append(lookup, c.Name)
		}
	}
	if len(lookup) > 0 {
		same, err := hcClient.SeriesByName(ctx, lookup)
		if err != nil {
			return nil, err
		}
		for _, c := range same {
			add(hcCandidate{Series: c})
		}
	}

	return hcRankCandidates(out, names, authors, titles), nil
}

// hcEvidence: what ties c to the library's series: its ISBNs ("isbn"), its
// author ("author"), or one of its books' titles ("title"); "" for nothing.
func hcEvidence(c hcCandidate, authors []string, titles map[string]bool) string {
	switch {
	case c.Hits > 0:
		return "isbn"
	case c.Author != "" && hcSharesAuthor([]string{c.Author}, authors):
		return "author"
	case slices.ContainsFunc(c.Books, func(t string) bool { return titles[hcFullTitleKey(t)] }):
		return "title"
	}
	return ""
}

// hcRankCandidates marks out's evidence, groups, Pick and Sure, and sorts
// them (see hcFindCandidates). names and titles are keys (hcSeriesKey,
// hcFullTitleKey) of the library series' names and its books' titles.
func hcRankCandidates(out []hcCandidate, names map[string]bool, authors []string, titles map[string]bool) []hcCandidate {
	type group struct {
		hits     int
		evidence bool
		score    int  // members with evidence
		named    bool // the library's name
	}
	groups := map[string]*group{}
	for i := range out {
		c := &out[i]
		c.URL = (&hcLink{Slug: c.Slug}).hardcoverURL()
		c.Group = hcSeriesKey(c.Name)
		c.Evidence = hcEvidence(*c, authors, titles)
		g := groups[c.Group]
		if g == nil {
			g = &group{named: names[c.Group]}
			groups[c.Group] = g
		}
		g.hits += c.Hits
		if c.Evidence != "" {
			g.evidence = true
			g.score++
		}
	}
	pick, sure := "", false
	for k, g := range groups {
		if g.named && g.evidence {
			pick, sure = k, true
		}
	}
	if pick == "" {
		// Else the group with the most library ISBNs, then the most members
		// with evidence (an author or a title). Only ISBNs make it sure.
		rank := func(g *group) int { return g.hits*1000 + g.score }
		best, second := "", 0
		for k, g := range groups {
			if best == "" || rank(g) > rank(groups[best]) || (rank(g) == rank(groups[best]) && k < best) {
				if best != "" {
					second = max(second, groups[best].hits)
				}
				best = k
			} else {
				second = max(second, g.hits)
			}
		}
		if best != "" && groups[best].evidence {
			pick = best
			sure = groups[best].hits >= 3 && groups[best].hits > 2*second
		}
	}
	for i := range out {
		out[i].Pick = pick != "" && out[i].Group == pick
		out[i].Sure = out[i].Pick && sure
	}
	// The picked group first, then by ISBNs found and size.
	slices.SortStableFunc(out, func(a, b hcCandidate) int {
		if a.Pick != b.Pick {
			return boolInt(b.Pick) - boolInt(a.Pick)
		}
		if ga, gb := groups[a.Group].hits, groups[b.Group].hits; ga != gb {
			return gb - ga
		}
		if a.Group != b.Group {
			return strings.Compare(a.Group, b.Group)
		}
		if a.Hits != b.Hits {
			return b.Hits - a.Hits
		}
		return b.BooksCount - a.BooksCount
	})
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

// hcSeriesTitles: the titles of the series' books in the library.
func hcSeriesTitles(s hcSeriesInfo) []string {
	c := currentCatalog()
	var out []string
	for i := range c.books {
		b := &c.books[i]
		if !b.Removed && b.Series != 0 && hcInMembers(c.seriesNames[b.Series], s.Members) {
			out = append(out, b.Title)
		}
	}
	return out
}

func hcSeriesAuthors(s hcSeriesInfo) []string {
	c := currentCatalog()
	var out []string
	for i := range c.books {
		b := &c.books[i]
		if !b.Removed && b.Series != 0 && hcInMembers(c.seriesNames[b.Series], s.Members) {
			for _, a := range b.AuthorIDs {
				if n := c.authorNames[a]; !slices.Contains(out, n) {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

func hcReviewURL(name string) string {
	return "/missing/hardcover?series=" + url.QueryEscape(name)
}

// hcSaveUsage keeps the day's use of the plan for the next start.
func hcSaveUsage() {
	if hcClient == nil || appDB == nil {
		return
	}
	u := hcClient.Budget.Usage(hcNow())
	var paused int64
	if !u.PausedUntil.IsZero() {
		paused = u.PausedUntil.Unix()
	}
	if _, err := appDB.Exec(`INSERT INTO hardcover_usage (day, used, remote, paused_until) VALUES (?, ?, ?, ?)
		ON CONFLICT (day) DO UPDATE SET used = excluded.used, remote = excluded.remote, paused_until = excluded.paused_until`,
		u.Day, u.Used, u.Remote, paused); err != nil {
		log.Println("hardcover usage:", err)
	}
	_, _ = appDB.Exec(`DELETE FROM hardcover_usage WHERE day < ?`, hcNow().UTC().AddDate(0, 0, -7).Format(time.DateOnly))
}

// hcRestoreUsage takes back today's use, and any pause, from before a restart.
func hcRestoreUsage() {
	if hcClient == nil || appDB == nil {
		return
	}
	var u hardcover.Usage
	var paused int64
	err := appDB.QueryRow(`SELECT day, used, remote, paused_until FROM hardcover_usage ORDER BY day DESC LIMIT 1`).
		Scan(&u.Day, &u.Used, &u.Remote, &paused)
	if err != nil {
		return
	}
	if paused > 0 {
		u.PausedUntil = time.Unix(paused, 0)
	}
	hcClient.Budget.Restore(hcNow(), u)
}

// startHardcover takes up the queue left from the last run and starts the
// worker. Finished jobs are kept a week (for the batch lists).
func startHardcover() {
	if !hardcoverEnabled() || appDB == nil {
		return
	}
	hcRestoreUsage()
	_, _ = appDB.Exec(`UPDATE hardcover_jobs SET state = 'queued' WHERE state = 'running'`)
	_, _ = appDB.Exec(`DELETE FROM hardcover_jobs WHERE state IN ('done', 'failed') AND finished_at < ?`, hcNow().Add(-7*24*time.Hour).Unix())
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_jobs WHERE state IN ('queued', 'held')`).Scan(&n)
	if n > 0 {
		log.Printf("hardcover: %d request(s) left from the last run", n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hcQueue.Lock()
	hcQueue.cancel, hcQueue.done = cancel, make(chan struct{})
	done := hcQueue.done
	hcQueue.Unlock()
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				log.Printf("hardcover queue stopped: %v", p)
			}
		}()
		for {
			ran, next := hcStep(ctx)
			if ctx.Err() != nil {
				return
			}
			if ran {
				continue
			}
			wait := time.Hour
			if !next.IsZero() {
				wait = max(time.Until(next), 100*time.Millisecond)
			}
			select {
			case <-ctx.Done():
				return
			case <-hcQueue.wake:
			case <-time.After(wait):
			}
		}
	}()
}

// stopHardcover stops the worker; a job it was running is queued again.
func stopHardcover(deadline time.Time) {
	hcQueue.Lock()
	cancel, done := hcQueue.cancel, hcQueue.done
	hcQueue.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
	}
}
