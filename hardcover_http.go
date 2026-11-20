package main

// Hardcover's pages and API (hardcover.go): the review of a series
// (/missing/hardcover?series=), an author's series for a batch
// (/missing/hardcover/author?author=), and the requests behind them. Editors
// only, on a server with -allow-writes, lookups on and a token set.

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// hcAwaitTime: how long a click waits for its request before answering
// that it is queued (the page then polls).
var hcAwaitTime = 5 * time.Second

// HardcoverUsage is the plan's use, for the pages.
type HardcoverUsage struct {
	Used     int    `json:"used"`
	Limit    int    `json:"limit"`
	ResetsIn string `json:"resetsIn"`         // "7h 12m"
	Paused   string `json:"paused,omitempty"` // until when, "15:04 UTC"
}

func hcUsageNow() HardcoverUsage {
	if hcClient == nil {
		return HardcoverUsage{}
	}
	now := hcNow()
	u := hcClient.Budget.Usage(now)
	out := HardcoverUsage{Used: u.Used, Limit: u.Limit, ResetsIn: hcDuration(u.ResetsAt.Sub(now))}
	if u.Remote >= 0 && u.Limit-u.Used > u.Remote {
		out.Used = u.Limit - u.Remote // Hardcover counts other tools' use too
	}
	if !u.PausedUntil.IsZero() {
		out.Paused = u.PausedUntil.UTC().Format("15:04 UTC")
	}
	return out
}

func hcDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

// hcAgo is how long ago a Unix time was, for "checked 3 days ago".
func hcAgo(unix int64) string {
	if unix == 0 {
		return "never"
	}
	d := hcNow().Sub(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// HardcoverJobView is a queued request as a page lists it.
type HardcoverJobView struct {
	ID     int64
	Kind   string
	Series string
	State  string
	Until  string // a held job: when it may run
	Error  string
	Review string
}

func hcJobView(j *hcJob) HardcoverJobView {
	v := HardcoverJobView{ID: j.ID, Kind: j.Kind, Series: j.Series, State: j.State, Error: j.Error, Review: hcReviewURL(j.Series)}
	if j.State == "held" && j.NotBefore > 0 {
		v.Until = time.Unix(j.NotBefore, 0).UTC().Format("Jan 2 15:04 UTC")
	}
	return v
}

// hcOpenJobs: the jobs waiting or running (for one series, when given).
func hcOpenJobs(series string) []HardcoverJobView {
	q := `SELECT ` + hcJobCols + ` FROM hardcover_jobs WHERE state IN ('queued', 'held', 'running')`
	var args []any
	if series != "" {
		q += ` AND series = ?`
		args = append(args, series)
	}
	rows, err := appDB.Query(q+` ORDER BY id LIMIT 200`, args...)
	if err != nil {
		log.Println("hardcover jobs:", err)
		return nil
	}
	defer rows.Close()
	var out []HardcoverJobView
	for rows.Next() {
		if j, err := scanHCJob(rows); err == nil {
			out = append(out, hcJobView(j))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The review page
// ---------------------------------------------------------------------------

// HardcoverReviewData is for hardcover-review.html.
type HardcoverReviewData struct {
	Title      string
	Series     string // as browsed
	SeriesHref string
	Linked     bool
	HCName     string
	HCURL      string
	Parts      []hcPartView // when it is linked to several
	Checked    string       // "3 days ago"
	Stale      bool
	Review     hcReview
	Complete   bool
	Rows       hcReviewRows
	Usage      HardcoverUsage
	Jobs       []HardcoverJobView
	Error      string
}

// hcReviewRow is one line of the review as the template shows it.
type hcReviewRow struct {
	Kind     string // tag, placeholder, info
	HCBookID int64
	UUID     string
	Position string
	Title    string
	Authors  string
	Released string
	HCURL    string
	BookHref string
	Strength string
	Note     string
	Checked  bool
	Action   string // what ticking it does
	Dismiss  bool   // can be dismissed
	Restore  bool   // was dismissed: can be shown again
}

type hcReviewRows struct {
	Present, Untagged, Missing, Extra, Dismissed []hcReviewRow
}

func hcBookURL(e *hardcover.Entry) string {
	if e == nil || !hardcover.ValidSlug(e.Slug) {
		return ""
	}
	return "https://hardcover.app/books/" + e.Slug
}

func hcRow(s hcSuggestion, write string) hcReviewRow {
	r := hcReviewRow{Strength: s.Strength, Note: s.Note, Checked: s.Checked}
	if e := s.Entry; e != nil {
		r.HCBookID, r.Position, r.Title, r.Released, r.HCURL = e.BookID, hcPositionText(e.Position), e.Title, e.Released, hcBookURL(e)
		r.Authors = strings.Join(e.Authors, " & ")
	}
	if b := s.Book; b != nil {
		r.UUID, r.BookHref = b.UUID, "/book/get/"+url.PathEscape(b.UUID)
		if s.Entry == nil {
			r.Title, r.Authors = b.Title, strings.Join(b.Authors, " & ")
			r.Position = strconv.FormatFloat(b.Index, 'f', -1, 64)
		}
	}
	return r
}

func hcReviewRowsFor(rev hcReview, write string) hcReviewRows {
	var rows hcReviewRows
	for _, s := range rev.Present {
		r := hcRow(s, write)
		if s.Fix {
			r.Kind, r.Dismiss = "tag", true
			r.Action = "Renumber to " + r.Position
		} else {
			r.Kind = "info"
		}
		rows.Present = append(rows.Present, r)
	}
	for _, s := range rev.Untagged {
		r := hcRow(s, write)
		r.Kind, r.Dismiss = "tag", true
		if r.Position != "" {
			r.Action = "Set series to " + write + " #" + r.Position
		} else {
			r.Action = "Set series to " + write
		}
		rows.Untagged = append(rows.Untagged, r)
	}
	for _, s := range rev.Missing {
		r := hcRow(s, write)
		if s.Placeholder != "" {
			r.Kind, r.BookHref, r.Note = "info", "/missing/"+s.Placeholder, "has a placeholder"
		} else {
			r.Kind, r.Dismiss, r.Action = "placeholder", true, "Make a placeholder"
		}
		rows.Missing = append(rows.Missing, r)
	}
	for _, s := range rev.Extra {
		r := hcRow(s, write)
		r.Kind = "info"
		rows.Extra = append(rows.Extra, r)
	}
	for _, s := range rev.Dismissed {
		r := hcRow(s, write)
		r.Kind, r.Restore = "info", true
		if s.Book != nil {
			r.Note = "matched " + s.Book.Title
		}
		rows.Dismissed = append(rows.Dismissed, r)
	}
	return rows
}

// GET /missing/hardcover?series=: a series' review.
func hardcoverReviewPage(w http.ResponseWriter, r *http.Request) {
	s, ok := hcSeries(r.URL.Query().Get("series"))
	if !ok {
		http.Error(w, "No such series in the library.", http.StatusNotFound)
		return
	}
	d := HardcoverReviewData{
		Title:      s.Name + " on Hardcover — " + SiteTitle,
		Series:     s.Name,
		SeriesHref: categoryByKey("series").href(s.Name),
		Usage:      hcUsageNow(),
		Jobs:       hcOpenJobs(s.Name),
	}
	l, err := hcLinkFor(s.Name)
	if err != nil {
		log.Println("hardcover:", err)
		d.Error = "Couldn't read this series' link."
	}
	if l != nil {
		d.Linked, d.HCName, d.HCURL, d.Checked, d.Stale = true, l.HCName, l.hardcoverURL(), hcAgo(l.CheckedAt), l.stale()
		if len(l.Parts) > 1 {
			for _, p := range l.Parts {
				d.Parts = append(d.Parts, hcPartView{Name: p.Name, Author: p.Author, Books: p.Books, URL: p.url()})
			}
		}
		if d.Review, err = hcReviewFor(s, l); err != nil {
			log.Println("hardcover review:", err)
			d.Error = "Couldn't match the list against the library."
		}
		d.Rows = hcReviewRowsFor(d.Review, s.Write)
		d.Complete = d.Review.complete()
	}
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "hardcover-review.html", d), "hardcover review page")
}

// hcPartView is one of the Hardcover series a series is linked to.
type hcPartView struct {
	Name, Author, URL string
	Books             int
}

// HardcoverAuthorData is for hardcover-author.html.
type HardcoverAuthorData struct {
	Title, Author, AuthorHref string
	Series                    []hcAuthorSeriesRow
	Usage                     HardcoverUsage
	Jobs                      []HardcoverJobView
	Max                       int
}

type hcAuthorSeriesRow struct {
	Name, Href, Review string
	Count              int
	Linked             bool
	HCName             string
	Checked            string
	Stale              bool
	Status             hcStatus
}

// GET /missing/hardcover/author?author=: an author's series, to identify
// or refresh some of them at once.
func hardcoverAuthorPage(w http.ResponseWriter, r *http.Request) {
	a := strings.TrimSpace(r.URL.Query().Get("author"))
	if a == "" {
		http.Error(w, "no author", http.StatusBadRequest)
		return
	}
	name, members, _ := groups.resolve("author", a)
	d := HardcoverAuthorData{Title: name + " on Hardcover — " + SiteTitle, Author: name,
		AuthorHref: categoryByKey("author").href(name), Usage: hcUsageNow(), Max: hcBatchMax}
	for _, s := range authorSeries(members) {
		row := hcAuthorSeriesRow{Name: s.Name, Href: s.Href, Count: s.Count, Review: hcReviewURL(s.Name)}
		if l, err := hcLinkFor(s.Name); err == nil && l != nil {
			row.Linked, row.HCName, row.Checked, row.Stale = true, l.HCName, hcAgo(l.CheckedAt), l.stale()
			row.Status, _ = hcStatusOf(s.Name)
		}
		d.Series = append(d.Series, row)
		for _, j := range hcOpenJobs(s.Name) {
			d.Jobs = append(d.Jobs, j)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "hardcover-author.html", d), "hardcover author page")
}

// MissingHardcover is the Hardcover card of /missing.
type MissingHardcover struct {
	Enabled bool
	Series  []hcMissingRow
	Usage   HardcoverUsage
	Jobs    []HardcoverJobView
}

type hcMissingRow struct {
	Name, Review, HCName, Checked string
	Stale                         bool
	Pending                       int
	Status                        hcStatus
}

func missingHardcover() *MissingHardcover {
	if !hardcoverEnabled() || appDB == nil {
		return nil
	}
	m := &MissingHardcover{Enabled: true, Usage: hcUsageNow(), Jobs: hcOpenJobs("")}
	links, err := hcAllLinks()
	if err != nil {
		log.Println("hardcover:", err)
	}
	for _, l := range links {
		row := hcMissingRow{Name: l.Name, Review: hcReviewURL(l.Name), HCName: l.HCName, Checked: hcAgo(l.CheckedAt), Stale: l.stale()}
		if s, ok := hcSeries(l.Name); ok {
			if rev, err := hcReviewFor(s, l); err == nil {
				row.Pending = rev.pending()
				row.Status = hcStatus{Numbered: rev.Numbered, Have: rev.Have, Complete: rev.complete()}
			}
		}
		m.Series = append(m.Series, row)
	}
	slices.SortStableFunc(m.Series, func(a, b hcMissingRow) int { return cmp.Compare(b.Pending, a.Pending) })
	return m
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

func hcDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return false
	}
	return true
}

// hcJobAnswer: a job as the API answers it, with the plan's use.
type hcJobAnswer struct {
	Job   *hcJob         `json:"job"`
	Until string         `json:"until,omitempty"`
	Usage HardcoverUsage `json:"usage"`
}

func hcAnswerJob(w http.ResponseWriter, r *http.Request, id int64) {
	j, err := hcAwait(r.Context(), id, hcAwaitTime)
	if err != nil {
		log.Println("hardcover:", err)
		http.Error(w, "couldn't read the request's state", http.StatusInternalServerError)
		return
	}
	a := hcJobAnswer{Job: j, Usage: hcUsageNow()}
	if j.State == "held" {
		a.Until = time.Unix(j.NotBefore, 0).UTC().Format("Jan 2 15:04 UTC")
	}
	writeJSON(w, http.StatusOK, a)
}

// POST /api/hardcover/{kind} {series, hcId}: candidates, link or refresh.
func hardcoverJobHandler(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Series string  `json:"series"`
			HCID   int64   `json:"hcId"`
			HCIDs  []int64 `json:"hcIds"` // several parts of one series
		}
		if !hcDecode(w, r, &req) {
			return
		}
		s, ok := hcSeries(req.Series)
		if !ok {
			http.Error(w, "no such series in the library", http.StatusNotFound)
			return
		}
		p := hcPayload{}
		if kind == hcJobLink {
			ids := req.HCIDs
			if req.HCID > 0 {
				ids = append(ids, req.HCID)
			}
			ids = slices.Compact(slices.Sorted(slices.Values(ids)))
			if len(ids) == 0 || ids[0] <= 0 {
				http.Error(w, "no Hardcover series given", http.StatusBadRequest)
				return
			}
			if len(ids) > hcMaxParts {
				http.Error(w, fmt.Sprintf("at most %d Hardcover series at once", hcMaxParts), http.StatusBadRequest)
				return
			}
			p.HCIDs = ids
		}
		id, err := hcEnqueue(kind, s.Name, "", p, currentUser(r))
		if err != nil {
			log.Println("hardcover:", err)
			http.Error(w, "couldn't queue the request", http.StatusInternalServerError)
			return
		}
		hcAnswerJob(w, r, id)
	}
}

// DELETE /api/hardcover/link?series=: forget the series' link. Placeholders
// made from it stay.
func hardcoverUnlinkHandler(w http.ResponseWriter, r *http.Request) {
	s, ok := hcSeries(r.URL.Query().Get("series"))
	if !ok {
		http.Error(w, "no such series in the library", http.StatusNotFound)
		return
	}
	if err := hcUnlink(s.Name); err != nil {
		log.Println("hardcover:", err)
		http.Error(w, "couldn't unlink", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/hardcover/batch {author, series: [...]}: identify or refresh
// these series of an author, one queued request each.
func hardcoverBatchHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Author string   `json:"author"`
		Series []string `json:"series"`
	}
	if !hcDecode(w, r, &req) {
		return
	}
	_, members, _ := groups.resolve("author", strings.TrimSpace(req.Author))
	allowed := map[string]bool{}
	for _, s := range authorSeries(members) {
		allowed[strings.ToLower(s.Name)] = true
	}
	var names []string
	for _, n := range req.Series {
		if s, ok := hcSeries(n); ok && allowed[strings.ToLower(s.Name)] && !slices.Contains(names, s.Name) {
			names = append(names, s.Name)
		}
	}
	switch {
	case len(names) == 0:
		http.Error(w, "none of those are this author's series", http.StatusBadRequest)
		return
	case len(names) > hcBatchMax:
		http.Error(w, fmt.Sprintf("at most %d series at once", hcBatchMax), http.StatusBadRequest)
		return
	}
	batch := uuid.NewString()
	var ids []int64
	for _, n := range names {
		id, err := hcEnqueue(hcJobBatch, n, batch, hcPayload{}, currentUser(r))
		if err != nil {
			log.Println("hardcover:", err)
			http.Error(w, "couldn't queue the requests", http.StatusInternalServerError)
			return
		}
		ids = append(ids, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": ids, "usage": hcUsageNow()})
}

// GET /api/hardcover/jobs?ids=1,2: those requests' states.
func hardcoverJobsHandler(w http.ResponseWriter, r *http.Request) {
	jobs := []*hcJob{}
	for _, s := range strings.Split(r.URL.Query().Get("ids"), ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil || len(jobs) >= 100 {
			continue
		}
		if j, err := hcJobByID(id); err == nil {
			jobs = append(jobs, j)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "usage": hcUsageNow()})
}

// DELETE /api/hardcover/jobs/{id}: cancel a request still waiting.
func hardcoverCancelJobHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	res, err := appDB.Exec(`UPDATE hardcover_jobs SET state = 'failed', error = 'cancelled', finished_at = ? WHERE id = ? AND state IN ('queued', 'held')`,
		hcNow().Unix(), id)
	if err != nil {
		log.Println("hardcover:", err)
		http.Error(w, "couldn't cancel", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "that request isn't waiting", http.StatusConflict)
		return
	}
	hcNotify(id)
	w.WriteHeader(http.StatusNoContent)
}

// hcApplyRequest: what the editor ticked. Only Hardcover book ids and
// library uuids: the metadata comes from the stored list, never the client.
type hcApplyRequest struct {
	Series       string  `json:"series"`
	Placeholders []int64 `json:"placeholders"`
	Tag          []struct {
		UUID     string `json:"uuid"`
		HCBookID int64  `json:"hcBookId"`
	} `json:"tag"`
	Dismiss []int64 `json:"dismiss"`
}

type hcApplyResult struct {
	Placeholders int      `json:"placeholders"`
	Tagged       int      `json:"tagged"`
	Dismissed    int      `json:"dismissed"`
	Errors       []string `json:"errors,omitempty"`
}

// POST /api/hardcover/apply: make the placeholders, tag the books and
// dismiss the suggestions the editor ticked, as the review stands now.
func hardcoverApplyHandler(w http.ResponseWriter, r *http.Request) {
	var req hcApplyRequest
	if !hcDecode(w, r, &req) {
		return
	}
	s, ok := hcSeries(req.Series)
	if !ok {
		http.Error(w, "no such series in the library", http.StatusNotFound)
		return
	}
	l, err := hcLinkFor(s.Name)
	if err != nil || l == nil || l.Snapshot == nil {
		http.Error(w, "the series isn't linked to Hardcover", http.StatusConflict)
		return
	}
	rev, err := hcReviewFor(s, l)
	if err != nil {
		log.Println("hardcover apply:", err)
		http.Error(w, "couldn't match the list against the library", http.StatusInternalServerError)
		return
	}
	u := currentUser(r)
	res := hcApplyResult{}

	// Placeholders: only for entries the review has as missing, without one.
	missing := map[int64]*hardcover.Entry{}
	for _, sg := range rev.Missing {
		if sg.Placeholder == "" && sg.Entry != nil {
			missing[sg.Entry.BookID] = sg.Entry
		}
	}
	for _, id := range slices.Compact(slices.Sorted(slices.Values(req.Placeholders))) {
		e := missing[id]
		if e == nil {
			continue
		}
		m, err := cleanPlaceholderMeta(hcPlaceholderMeta(e, s.Write))
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%q: %v", e.Title, err))
			continue
		}
		p := &placeholder{Meta: m, Source: "hardcover"}
		if err := savePlaceholder(p, u); err != nil {
			log.Println("hardcover placeholder:", err)
			res.Errors = append(res.Errors, fmt.Sprintf("%q: couldn't save", e.Title))
			continue
		}
		logEdit(u, "add placeholder", logKindPlaceholder, p.UUID, p.Meta.Title, nil, snapPlaceholder(p))
		res.Placeholders++
	}

	// Series tags: only books the review matched to that entry.
	tags := map[string]*hardcover.Entry{}
	for _, sg := range slices.Concat(rev.Untagged, rev.Present) {
		if sg.Book != nil && sg.Entry != nil && (sg.Fix || !hcInMembers(sg.Book.Series, s.Members)) {
			tags[sg.Book.UUID+"\x00"+strconv.FormatInt(sg.Entry.BookID, 10)] = sg.Entry
		}
	}
	if len(req.Tag) > 0 && !allowWrites {
		res.Errors = append(res.Errors, "the library is read-only on this server")
	} else {
		done := map[string]bool{}
		for _, t := range req.Tag {
			e := tags[t.UUID+"\x00"+strconv.FormatInt(t.HCBookID, 10)]
			if e == nil || done[t.UUID] {
				continue
			}
			done[t.UUID] = true
			name := s.Write
			edit := &bulkEdit{Series: &name}
			if pos := hcPositionText(e.Position); pos != "" {
				edit.SeriesIndex = &pos
			}
			if _, err := queueBulkEdit([]string{t.UUID}, edit, u); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%q: %v", e.Title, err))
				continue
			}
			res.Tagged++
		}
	}

	// Dismissals: any of the list's books.
	inList := map[int64]bool{}
	for i := range l.Snapshot.Entries {
		for _, id := range l.Snapshot.Entries[i].IDs() {
			inList[id] = true
		}
	}
	var dismiss []int64
	for _, id := range req.Dismiss {
		if inList[id] {
			dismiss = append(dismiss, id)
		}
	}
	if n, err := hcDismiss(s.Name, dismiss); err != nil {
		log.Println("hardcover dismiss:", err)
		res.Errors = append(res.Errors, "couldn't save the dismissals")
	} else {
		res.Dismissed = n
	}
	writeJSON(w, http.StatusOK, res)
}

// POST /api/hardcover/undismiss {series, ids}: show dismissed suggestions
// again; all of the series' when ids is empty.
func hardcoverUndismissHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Series string  `json:"series"`
		IDs    []int64 `json:"ids"`
	}
	if !hcDecode(w, r, &req) {
		return
	}
	s, ok := hcSeries(req.Series)
	if !ok {
		http.Error(w, "no such series in the library", http.StatusNotFound)
		return
	}
	if err := hcRestore(s.Name, req.IDs); err != nil {
		log.Println("hardcover:", err)
		http.Error(w, "couldn't save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// hcPlaceholderMeta: a placeholder for a missing book of the series
// (written as series), from what Hardcover says about it.
func hcPlaceholderMeta(e *hardcover.Entry, series string) OPFMetadata {
	m := OPFMetadata{Title: e.Title, Creators: slices.Clone(e.Authors), Series: series, SeriesIndex: hcPositionText(e.Position)}
	if len(e.Released) >= 4 {
		m.Date = e.Released
	}
	m.Identifiers = []opfIdentifier{{Scheme: hcIdentifierScheme, Value: strconv.FormatInt(e.BookID, 10)}}
	if len(e.ISBNs) > 0 {
		m.Identifiers = append(m.Identifiers, opfIdentifier{Scheme: "ISBN", Value: e.ISBNs[0]})
	}
	return m
}

func registerHardcoverRoutes() {
	wrap := func(h http.HandlerFunc) http.HandlerFunc { return requireEditor(requireLookups(requireHardcover(h))) }
	http.HandleFunc("GET /missing/hardcover", wrap(hardcoverReviewPage))
	http.HandleFunc("GET /missing/hardcover/author", wrap(hardcoverAuthorPage))
	http.HandleFunc("POST /api/hardcover/candidates", wrap(hardcoverJobHandler(hcJobCandidates)))
	http.HandleFunc("POST /api/hardcover/link", wrap(hardcoverJobHandler(hcJobLink)))
	http.HandleFunc("POST /api/hardcover/refresh", wrap(hardcoverJobHandler(hcJobRefresh)))
	http.HandleFunc("DELETE /api/hardcover/link", wrap(hardcoverUnlinkHandler))
	http.HandleFunc("POST /api/hardcover/batch", wrap(hardcoverBatchHandler))
	http.HandleFunc("GET /api/hardcover/jobs", wrap(hardcoverJobsHandler))
	http.HandleFunc("DELETE /api/hardcover/jobs/{id}", wrap(hardcoverCancelJobHandler))
	http.HandleFunc("POST /api/hardcover/apply", wrap(hardcoverApplyHandler))
	http.HandleFunc("POST /api/hardcover/undismiss", wrap(hardcoverUndismissHandler))
}
