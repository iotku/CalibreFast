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

// hcTitleIndex: the catalog's books by hcTitleKey, and by book id; built
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
		k := hcTitleKey(b.Title)
		idx.byTitle[k] = append(idx.byTitle[k], int32(i))
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
	lib := hcLibrary{members: s.Members, byISBN: map[string][]hcLibBook{}}
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

// hcLink is a series linked to Hardcover.
type hcLink struct {
	Name         string
	HCID         int64
	Slug, HCName string
	IdentifiedAt int64
	CheckedAt    int64
	Snapshot     *hardcover.SeriesDetail
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
	return l, err
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

// hcSaveLink links name to d's series with d as its list. Linking to
// another series forgets what was turned down for the old one.
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
	var old int64
	err = tx.QueryRow(`SELECT hc_id FROM hardcover_series WHERE name = ?`, name).Scan(&old)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.Exec(`INSERT INTO hardcover_series (name, hc_id, hc_slug, hc_name, identified_by, identified_at, checked_at, snapshot)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, name, d.ID, d.Slug, d.Name, uid, now, now, string(snap))
	case err != nil:
	case old != d.ID:
		if _, err = tx.Exec(`DELETE FROM hardcover_dismissed WHERE name = ?`, name); err == nil {
			_, err = tx.Exec(`UPDATE hardcover_series SET hc_id = ?, hc_slug = ?, hc_name = ?, identified_by = ?, identified_at = ?, checked_at = ?, snapshot = ? WHERE name = ?`,
				d.ID, d.Slug, d.Name, uid, now, now, string(snap), name)
		}
	default:
		_, err = tx.Exec(`UPDATE hardcover_series SET hc_slug = ?, hc_name = ?, checked_at = ?, snapshot = ? WHERE name = ?`,
			d.Slug, d.Name, now, string(snap), name)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func hcUnlink(name string) error {
	if _, err := appDB.Exec(`DELETE FROM hardcover_dismissed WHERE name = ?`, name); err != nil {
		return err
	}
	_, err := appDB.Exec(`DELETE FROM hardcover_series WHERE name = ?`, name)
	return err
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
	HCID int64 `json:"hcId,omitempty"`
}

// hcCandidates is a candidates job's result.
type hcCandidates struct {
	Candidates []hcCandidate `json:"candidates"`
}

type hcCandidate struct {
	hardcover.Series
	Hits int    `json:"hits,omitempty"` // library ISBNs found in it
	URL  string `json:"url,omitempty"`
	Sure bool   `json:"sure,omitempty"` // the batch would link it
}

// hcLinkResult is a link, refresh or batch job's result.
type hcLinkResult struct {
	Linked      bool          `json:"linked"`
	HCName      string        `json:"hcName,omitempty"`
	Books       int           `json:"books,omitempty"`
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
		return hcFetchAndLink(ctx, s, p.HCID, j)
	case hcJobRefresh, hcJobBatch:
		l, err := hcLinkFor(s.Name)
		if err != nil {
			return nil, err
		}
		if l != nil {
			return hcFetchAndLink(ctx, s, l.HCID, j)
		}
		if j.Kind == hcJobRefresh {
			return nil, errors.New("the series isn't linked to Hardcover")
		}
		cands, err := hcFindCandidates(ctx, s)
		if err != nil {
			return nil, err
		}
		var sure []hcCandidate
		for _, c := range cands {
			if c.Sure {
				sure = append(sure, c)
			}
		}
		if len(sure) != 1 {
			return hcLinkResult{NeedsChoice: true, Candidates: cands, Review: hcReviewURL(s.Name)}, nil
		}
		return hcFetchAndLink(ctx, s, sure[0].ID, j)
	}
	return nil, fmt.Errorf("unknown job %q", j.Kind)
}

func hcFetchAndLink(ctx context.Context, s hcSeriesInfo, id int64, j *hcJob) (any, error) {
	if id <= 0 {
		return nil, errors.New("no Hardcover series given")
	}
	d, err := hcClient.SeriesBooks(ctx, id)
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
	return hcLinkResult{Linked: true, HCName: d.Name, Books: len(d.Entries), Review: hcReviewURL(s.Name)}, nil
}

// hcFindCandidates: Hardcover series by the name, and those the series'
// library ISBNs are in, best first. Sure: the name is the same and an ISBN
// or an author agrees (what a batch links on its own).
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
	authors := hcSeriesAuthors(s)
	names := map[string]bool{}
	for _, m := range append([]string{s.Name}, s.Members...) {
		names[hcTitleKey(m)] = true
	}
	var out []hcCandidate
	index := map[int64]int{}
	add := func(c hcCandidate) {
		if i, ok := index[c.ID]; ok {
			out[i].Hits = max(out[i].Hits, c.Hits)
			if out[i].Author == "" {
				out[i].Author = c.Author
			}
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
	for i := range out {
		c := &out[i]
		c.URL = (&hcLink{Slug: c.Slug}).hardcoverURL()
		c.Sure = names[hcTitleKey(c.Name)] && (c.Hits > 0 || (c.Author != "" && hcSharesAuthor([]string{c.Author}, authors)))
	}
	slices.SortStableFunc(out, func(a, b hcCandidate) int {
		if a.Sure != b.Sure {
			return boolInt(b.Sure) - boolInt(a.Sure)
		}
		return b.Hits - a.Hits
	})
	if len(out) > 15 {
		out = out[:15]
	}
	return out, nil
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
