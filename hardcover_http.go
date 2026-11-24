package main

// Hardcover's pages and API (internal/hardcoverseries): the review of a series
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
	"github.com/iotku/CalibreFast/internal/hardcoverseries"
	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/textkey"
)

// hcAwaitTime: how long a click waits for its request before answering
// that it is queued (the page then polls).
var hcAwaitTime = 5 * time.Second

// hcUserID is u's id, or 0 for none.
func hcUserID(u *User) int64 {
	if u == nil {
		return 0
	}
	return u.ID
}

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

func hcJobView(j *hardcoverseries.Job) HardcoverJobView {
	v := HardcoverJobView{ID: j.ID, Kind: j.Kind, Series: j.Series, State: j.State, Error: j.Error, Review: hcReviewURL(j.Series)}
	if j.State == "held" && j.NotBefore > 0 {
		v.Until = time.Unix(j.NotBefore, 0).UTC().Format("Jan 2 15:04 UTC")
	}
	return v
}

// hcOpenJobs: the jobs waiting or running (for one series, when given).
func hcOpenJobs(series string) []HardcoverJobView {
	jobs, err := hcLinks.OpenJobs(series)
	if err != nil {
		log.Println("hardcover jobs:", err)
	}
	var out []HardcoverJobView
	for _, j := range jobs {
		out = append(out, hcJobView(j))
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
	Review     hardcoverseries.Review
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
	NotMatch bool   // matched to a library book that may not be it
	Refused  bool   // a "Not a match" (dismissed): Restore undoes it
	// Lib: the library book matched to a Hardcover book, shown under it
	// (its title and author may not be the same: a match by title alone).
	Lib *hcLibView
}

// hcLibView is a library book as a review row shows it.
type hcLibView struct {
	Title, Authors, Href, Series string
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

func hcRow(s hardcoverseries.Suggestion, write string) hcReviewRow {
	r := hcReviewRow{Strength: s.Strength, Note: s.Note, Checked: s.Checked}
	if e := s.Entry; e != nil {
		r.HCBookID, r.Position, r.Title, r.Released, r.HCURL = e.BookID, hardcoverseries.PositionText(e.Position), e.Title, e.Released, hcBookURL(e)
		r.Authors = strings.Join(e.Authors, " & ")
	}
	if b := s.Book; b != nil {
		r.UUID, r.BookHref = b.UUID, "/book/get/"+url.PathEscape(b.UUID)
		if s.Entry == nil {
			r.Title, r.Authors = b.Title, strings.Join(b.Authors, " & ")
			r.Position = strconv.FormatFloat(b.Index, 'f', -1, 64)
		} else {
			r.Lib = &hcLibView{Title: b.Title, Authors: strings.Join(b.Authors, " & "), Href: r.BookHref}
			if b.Series != "" {
				r.Lib.Series = b.Series + " #" + strconv.FormatFloat(b.Index, 'f', -1, 64)
			}
			r.BookHref = "" // the link is on the library book's line
		}
	}
	return r
}

// hcSameBook: the library book has the Hardcover book's title and one of
// its authors, so the row needn't show it again.
func hcSameBook(s hardcoverseries.Suggestion) bool {
	return s.Book != nil && s.Entry != nil && textkey.FullTitle(s.Book.Title) == textkey.FullTitle(s.Entry.Title) &&
		textkey.SharesName(s.Book.Authors, s.Entry.Authors)
}

func hcReviewRowsFor(rev hardcoverseries.Review, write string) hcReviewRows {
	var rows hcReviewRows
	for _, s := range rev.Present {
		r := hcRow(s, write)
		if r.Lib != nil && hcSameBook(s) {
			r.BookHref, r.Lib = r.Lib.Href, nil
		}
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
		r.Kind, r.NotMatch = "tag", true // "Not a match" says it; no Dismiss too
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
		switch {
		case s.Refused:
			r.Refused, r.Note = true, "" // the library book is on its own line
		case s.Book != nil:
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
	l, err := hcLinks.LinkFor(s.Name)
	if err != nil {
		log.Println("hardcover:", err)
		d.Error = "Couldn't read this series' link."
	}
	if l != nil {
		d.Linked, d.HCName, d.HCURL, d.Checked, d.Stale = true, l.HCName, l.URL(), hcAgo(l.CheckedAt), l.Stale(hcNow())
		if len(l.Parts) > 1 {
			for _, p := range l.Parts {
				d.Parts = append(d.Parts, hcPartView{Name: p.Name, Author: p.Author, Books: p.Books, URL: p.URL()})
			}
		}
		if d.Review, err = hcLinks.ReviewFor(s, l); err != nil {
			log.Println("hardcover review:", err)
			d.Error = "Couldn't match the list against the library."
		}
		d.Rows = hcReviewRowsFor(d.Review, s.Write)
		d.Complete = d.Review.Complete()
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
	Status             seriesStatus
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
		if l, err := hcLinks.LinkFor(s.Name); err == nil && l != nil {
			row.Linked, row.HCName, row.Checked, row.Stale = true, l.HCName, hcAgo(l.CheckedAt), l.Stale(hcNow())
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
	// Complete: series Hardcover says the library has every book of, with
	// nothing waiting for a decision. Shown folded away below Series.
	Complete []hcMissingRow
	Usage    HardcoverUsage
	Jobs     []HardcoverJobView
}

type hcMissingRow struct {
	Name, Review, HCName, Checked string
	Stale                         bool
	Pending                       int
	Status                        seriesStatus
}

func missingHardcover() *MissingHardcover {
	if !hardcoverEnabled() || appDB == nil {
		return nil
	}
	m := &MissingHardcover{Enabled: true, Usage: hcUsageNow(), Jobs: hcOpenJobs("")}
	links, err := hcLinks.AllLinks()
	if err != nil {
		log.Println("hardcover:", err)
	}
	for _, l := range links {
		row := hcMissingRow{Name: l.Name, Review: hcReviewURL(l.Name), HCName: l.HCName, Checked: hcAgo(l.CheckedAt), Stale: l.Stale(hcNow())}
		if s, ok := hcSeries(l.Name); ok {
			if rev, err := hcLinks.ReviewFor(s, l); err == nil {
				row.Pending = rev.Pending()
				row.Status = hcReviewStatus(rev)
			}
		}
		if row.Status.Complete && row.Pending == 0 {
			m.Complete = append(m.Complete, row)
		} else {
			m.Series = append(m.Series, row)
		}
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
	Job   *hardcoverseries.Job `json:"job"`
	Until string               `json:"until,omitempty"`
	Usage HardcoverUsage       `json:"usage"`
}

func hcAnswerJob(w http.ResponseWriter, r *http.Request, id int64) {
	j, err := hcLinks.Await(r.Context(), id, hcAwaitTime)
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
		p := hardcoverseries.Payload{}
		if kind == hardcoverseries.JobLink {
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
		id, err := hcLinks.Enqueue(kind, s.Name, "", p, hcUserID(currentUser(r)))
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
	if err := hcLinks.Unlink(s.Name); err != nil {
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
		id, err := hcLinks.Enqueue(hardcoverseries.JobBatch, n, batch, hardcoverseries.Payload{}, hcUserID(currentUser(r)))
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
	jobs := []*hardcoverseries.Job{}
	for _, s := range strings.Split(r.URL.Query().Get("ids"), ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil || len(jobs) >= 100 {
			continue
		}
		if j, err := hcLinks.JobByID(id); err == nil {
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
	ok, err := hcLinks.Cancel(id)
	if err != nil {
		log.Println("hardcover:", err)
		http.Error(w, "couldn't cancel", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "that request isn't waiting", http.StatusConflict)
		return
	}
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
	l, err := hcLinks.LinkFor(s.Name)
	if err != nil || l == nil || l.Snapshot == nil {
		http.Error(w, "the series isn't linked to Hardcover", http.StatusConflict)
		return
	}
	rev, err := hcLinks.ReviewFor(s, l)
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
		if _, err := hcMakePlaceholder(e, s, u); err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		res.Placeholders++
	}

	// Series tags: only books the review matched to that entry.
	tags := map[string]*hardcover.Entry{}
	for _, sg := range slices.Concat(rev.Untagged, rev.Present) {
		if sg.Book != nil && sg.Entry != nil && (sg.Fix || !hardcoverseries.InMembers(sg.Book.Series, s.Members)) {
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
			if pos := hardcoverseries.PositionText(e.Position); pos != "" {
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
	if n, err := hcLinks.Dismiss(s.Name, dismiss); err != nil {
		log.Println("hardcover dismiss:", err)
		res.Errors = append(res.Errors, "couldn't save the dismissals")
	} else {
		res.Dismissed = n
	}
	writeJSON(w, http.StatusOK, res)
}

// hcMakePlaceholder makes a placeholder for e, a book of series s, from
// what Hardcover says about it. Errors are fit to show.
func hcMakePlaceholder(e *hardcover.Entry, s hardcoverseries.SeriesInfo, u *User) (*placeholder, error) {
	m, err := cleanPlaceholderMeta(hcPlaceholderMeta(e, s.Write))
	if err != nil {
		return nil, fmt.Errorf("%q: %v", e.Title, err)
	}
	p := &placeholder{Meta: m, Source: "hardcover"}
	if err := savePlaceholder(p, u); err != nil {
		log.Println("hardcover placeholder:", err)
		return nil, fmt.Errorf("%q: couldn't save", e.Title)
	}
	logEdit(u, "add placeholder", logKindPlaceholder, p.UUID, p.Meta.Title, nil, snapPlaceholder(p))
	return p, nil
}

// POST /api/hardcover/notmatch {series, uuid, hcBookId}: the library book
// the review matched to a Hardcover book isn't it. It's never matched to
// that book again, and the Hardcover book gets a placeholder (unless it
// has one), so it shows as missing from the series.
func hardcoverNotMatchHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Series   string `json:"series"`
		UUID     string `json:"uuid"`
		HCBookID int64  `json:"hcBookId"`
	}
	if !hcDecode(w, r, &req) {
		return
	}
	s, ok := hcSeries(req.Series)
	if !ok {
		http.Error(w, "no such series in the library", http.StatusNotFound)
		return
	}
	l, err := hcLinks.LinkFor(s.Name)
	if err != nil || l == nil || l.Snapshot == nil {
		http.Error(w, "the series isn't linked to Hardcover", http.StatusConflict)
		return
	}
	rev, err := hcLinks.ReviewFor(s, l)
	if err != nil {
		log.Println("hardcover not a match:", err)
		http.Error(w, "couldn't match the list against the library", http.StatusInternalServerError)
		return
	}
	// Only a match the review suggests now: the client sends ids, never metadata.
	var e *hardcover.Entry
	for _, sg := range rev.Untagged {
		if sg.Entry != nil && sg.Book != nil && sg.Book.UUID == req.UUID && sg.Entry.BookID == req.HCBookID {
			e = sg.Entry
			break
		}
	}
	if e == nil {
		http.Error(w, "that match isn't suggested any more: reload the page", http.StatusConflict)
		return
	}
	if err := hcLinks.NotMatch(s.Name, e.BookID, req.UUID); err != nil {
		log.Println("hardcover not a match:", err)
		http.Error(w, "couldn't save", http.StatusInternalServerError)
		return
	}
	var res struct {
		Placeholder string `json:"placeholder,omitempty"`
		Error       string `json:"error,omitempty"`
	}
	if ph := hardcoverseries.PlaceholderFor(e, s.Members, hcPlaceholderViews(allPlaceholders())); ph != nil {
		res.Placeholder = ph.UUID
	} else if p, err := hcMakePlaceholder(e, s, currentUser(r)); err != nil {
		res.Error = err.Error()
	} else {
		res.Placeholder = p.UUID
	}
	writeJSON(w, http.StatusOK, res)
}

// POST /api/hardcover/undismiss {series, ids, notMatch}: show dismissed
// suggestions again, and undo "Not a match"es ({uuid, hcBookId}); all of
// the series' when both are empty.
func hardcoverUndismissHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Series   string  `json:"series"`
		IDs      []int64 `json:"ids"`
		NotMatch []struct {
			UUID     string `json:"uuid"`
			HCBookID int64  `json:"hcBookId"`
		} `json:"notMatch"`
	}
	if !hcDecode(w, r, &req) {
		return
	}
	s, ok := hcSeries(req.Series)
	if !ok {
		http.Error(w, "no such series in the library", http.StatusNotFound)
		return
	}
	if len(req.IDs) > 0 || len(req.NotMatch) == 0 {
		if err := hcLinks.Restore(s.Name, req.IDs); err != nil {
			log.Println("hardcover:", err)
			http.Error(w, "couldn't save", http.StatusInternalServerError)
			return
		}
	}
	var entries []hardcover.Entry
	if l, err := hcLinks.LinkFor(s.Name); err == nil && l != nil && l.Snapshot != nil {
		entries = l.Snapshot.Entries
	}
	for _, m := range req.NotMatch {
		// The refusal may be kept against another book merged into the entry.
		ids := []int64{m.HCBookID}
		for i := range entries {
			if slices.Contains(entries[i].IDs(), m.HCBookID) {
				ids = entries[i].IDs()
				break
			}
		}
		if err := hcLinks.RestoreMatch(s.Name, m.UUID, ids); err != nil {
			log.Println("hardcover:", err)
			http.Error(w, "couldn't save", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// hcPlaceholderMeta: a placeholder for a missing book of the series
// (written as series), from what Hardcover says about it.
func hcPlaceholderMeta(e *hardcover.Entry, series string) OPFMetadata {
	m := OPFMetadata{Title: e.Title, Creators: slices.Clone(e.Authors), Series: series, SeriesIndex: hardcoverseries.PositionText(e.Position)}
	if len(e.Released) >= 4 {
		m.Date = e.Released
	}
	m.Identifiers = []opfIdentifier{{Scheme: hardcover.IDScheme, Value: strconv.FormatInt(e.BookID, 10)}}
	if hardcover.ValidSlug(e.Slug) { // for a link to its page
		m.Identifiers = append(m.Identifiers, opfIdentifier{Scheme: hardcover.SlugScheme, Value: e.Slug})
	}
	if len(e.ISBNs) > 0 {
		m.Identifiers = append(m.Identifiers, opfIdentifier{Scheme: "ISBN", Value: e.ISBNs[0]})
	}
	return m
}
