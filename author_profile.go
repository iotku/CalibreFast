package main

// Author profiles: the top of an author's page, with what Hardcover says
// of them (internal/hardcoverauthors) where an admin hasn't given the
// entry its own (aggregate_meta.go wins, field by field).
//
// Looked up only on demand: the first time a signed-in user opens an
// author's page, the page asks for it (POST /api/authors/profile, from
// web/author-profile.ts), which queues the lookup in the Hardcover queue
// (internal/hardcoverseries) and waits a little for it. Pictures are kept
// in <datadir>/author-images and served from our own origin, never
// hot-linked.

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/internal/hardcoverauthors"
	"github.com/iotku/CalibreFast/internal/hardcoverseries"
	"github.com/iotku/CalibreFast/pkg/textkey"
)

// hcAuthorImagesDir: where authors' pictures are kept, under the data
// directory, and served from.
const hcAuthorImagesDir = "author-images"

// hcAuthors keeps Hardcover's lookups of authors; nil without the app
// database.
var hcAuthors *hardcoverauthors.Service

// newHCAuthors makes the Service on the app database; nil without it.
func newHCAuthors() *hardcoverauthors.Service {
	if appDB == nil {
		return nil
	}
	return hardcoverauthors.New(hardcoverauthors.Config{
		DB:     appDB,
		Host:   hcAuthorHost{},
		Client: hcEnabledClient,
		Now:    func() time.Time { return hcNow() },
		StoreImage: func(jpg []byte) (string, error) {
			return saveHashedImage(hcAuthorImagesDir, &aggImage{jpg, "jpg"})
		},
		RemoveImage: func(name string) {
			if !aggregateImageRe.MatchString(name) {
				return
			}
			if err := os.Remove(filepath.Join(dataDir, hcAuthorImagesDir, name)); err != nil && !os.IsNotExist(err) {
				log.Printf("author image %s: %v", name, err)
			}
		},
	})
}

// hcAuthorHost is the library as author lookups see it.
type hcAuthorHost struct{}

func (hcAuthorHost) Members(name string) []string {
	_, members, _ := groups.resolve("author", name)
	return members
}

// TitleKeys: the library's titles by these authors (textkey.Title).
func (hcAuthorHost) TitleKeys(members []string) map[string]bool {
	c := currentCatalog()
	want := map[string]bool{}
	for _, m := range members {
		want[strings.ToLower(m)] = true
	}
	out := map[string]bool{}
	for i := range c.books {
		b := &c.books[i]
		if b.Removed {
			continue
		}
		for _, a := range b.AuthorIDs {
			if want[strings.ToLower(c.authorNames[a])] {
				out[textkey.Title(b.Title)] = true
				break
			}
		}
	}
	return out
}

// hcAuthorImageURL is where the author's picture is served; "" for none.
func hcAuthorImageURL(a *hardcoverauthors.Author) string {
	if a == nil || a.Image == "" || !aggregateImageRe.MatchString(a.Image) {
		return ""
	}
	return "/" + hcAuthorImagesDir + "/" + a.Image
}

// hcRequestAuthor queues a lookup of the author (as browsed), unless one
// is stored or already waiting. id 0: nothing was queued.
func hcRequestAuthor(name string, by *User) (int64, error) {
	if !hcAuthors.For(name).NeedsLookup(hcNow()) {
		return 0, nil
	}
	return hcLinks.Enqueue(hardcoverauthors.JobKind, name, "", hardcoverseries.Payload{}, hcUserID(by))
}

// ---------------------------------------------------------------------------
// The page
// ---------------------------------------------------------------------------

// AuthorProfile is what the author's page shows of Hardcover's lookup.
type AuthorProfile struct {
	Years string // "1948–2015"
	Age   string // "Age 77", "Died at 41"
	// Born and Died: the years shown (0: not known), for the Manage
	// sidebar's fields (filtered.html).
	Born, Died   int
	HardcoverURL string // their Hardcover page, "" for none
	// FromHardcover: the bio or the picture shown is Hardcover's.
	FromHardcover bool
	// Lookup: the page asks for a lookup (web/author-profile.ts).
	Lookup string
	// Refresh: an editor may have it looked up again.
	Refresh string
}

// hcFillAuthorProfile adds Hardcover's profile of the author to their page:
// the bio and picture where the page has none of its own.
func hcFillAuthorProfile(r *http.Request, data *FilteredPageData) {
	if data.Category != "author" || appDB == nil {
		return
	}
	a := hcAuthors.For(data.Name)
	p := &AuthorProfile{}
	if a.Found() {
		if data.Description == "" && a.Bio != "" {
			data.Description, p.FromHardcover = a.Bio, true
		}
		if data.ImageURL == "" && hcAuthorImageURL(a) != "" {
			data.ImageURL, p.FromHardcover = hcAuthorImageURL(a), true
		}
		p.Born, p.Died, p.HardcoverURL = a.Born, a.Died, a.HardcoverURL()
	}
	// An editor's years win over Hardcover's.
	if m, ok := aggregateMetaFor(data.Category, data.Name); ok && m.YearsSet {
		p.Born, p.Died = m.Born, m.Died
	}
	p.Years, p.Age = hardcoverauthors.LifeYears(p.Born, p.Died), hardcoverauthors.LifeAge(p.Born, p.Died, hcNow())
	q := "?" + url.Values{"name": {data.Name}}.Encode()
	if hardcoverEnabled() {
		if currentUser(r) != nil && a.NeedsLookup(hcNow()) && data.TotalBooks > data.Missing {
			p.Lookup = "/api/authors/profile" + q
		}
		if a != nil && canEditLibrary(r) {
			p.Refresh = "/api/authors/profile/refresh" + q
		}
	}
	if p.Years != "" || p.HardcoverURL != "" || p.FromHardcover || p.Lookup != "" || p.Refresh != "" {
		data.Profile = p
	}
}

// hcFillSeriesDescription adds the description Hardcover gave a linked
// series (fetched with its books, internal/hardcoverseries) where the page has none of
// its own. Plain text, shown escaped like an author's bio.
func hcFillSeriesDescription(data *FilteredPageData) {
	if data.Category != "series" || data.Description != "" || appDB == nil {
		return
	}
	l, err := hcLinks.LinkFor(data.Name)
	if err != nil || l == nil || l.Snapshot == nil || l.Snapshot.Description == "" {
		return
	}
	data.Description = l.Snapshot.Description
	data.Profile = &AuthorProfile{FromHardcover: true, HardcoverURL: l.URL()}
}

// authorHeaderData is what the top of the author's page shows, for
// rendering it again once Hardcover has answered.
func authorHeaderData(r *http.Request, value string) (FilteredPageData, bool) {
	c := categoryByKey("author")
	name, members, grouped := groups.resolve(c.Key, value)
	if !grouped {
		// As the library spells it: the queue and the store key on it.
		for _, n := range currentCatalog().authorNames {
			if strings.EqualFold(n, name) {
				name, members = n, []string{n}
				break
			}
		}
	}
	data := FilteredPageData{Name: name, Display: name, Category: c.Key, Members: members,
		TotalBooks: getFilteredTotal(c, name, members)}
	if data.TotalBooks == 0 {
		return data, false
	}
	if m, ok := aggregateMetaFor(c.Key, name); ok {
		data.Description, data.ImageURL = m.Description, m.imageURL()
	}
	data.Series = authorSeries(members)
	if st := seriesStatuses(); len(st) > 0 {
		for i := range data.Series {
			data.Series[i].Status = statusPtr(st, data.Series[i].Name)
		}
	}
	hcFillAuthorProfile(r, &data)
	return data, true
}

// POST /api/authors/profile?name=: look the author up on Hardcover, unless
// done already, and answer the top of their page (HTML) once it's there:
// 200 with it, or 202 while the lookup waits in the queue.
func authorProfileHandler(refresh bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, ok := authorHeaderData(r, r.URL.Query().Get("name"))
		if !ok {
			http.NotFound(w, r) // only authors with books here are looked up
			return
		}
		if refresh {
			if err := hcAuthors.Forget(data.Name); err != nil {
				log.Println("hardcover author:", err)
				http.Error(w, "couldn't forget the lookup", http.StatusInternalServerError)
				return
			}
		}
		id, err := hcRequestAuthor(data.Name, currentUser(r))
		if err != nil {
			log.Println("hardcover author:", err)
			http.Error(w, "couldn't queue the lookup", http.StatusInternalServerError)
			return
		}
		if id != 0 {
			if j, err := hcLinks.Await(r.Context(), id, hcAwaitTime); err != nil || j.State != "done" && j.State != "failed" {
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusAccepted)
				return
			}
			data, _ = authorHeaderData(r, data.Name)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		logErr(templates.ExecuteTemplate(w, "agg-header", data), "author profile")
	}
}

// GET /author-images/{file}: a picture from Hardcover, kept under its hash.
func authorImageHandler(w http.ResponseWriter, r *http.Request) {
	serveHashedImage(w, r, hcAuthorImagesDir)
}

// Shows: the profile has something to show in the header.
func (p *AuthorProfile) Shows() bool {
	return p != nil && (p.Years != "" || p.HardcoverURL != "" || p.FromHardcover || p.Refresh != "")
}

// descLeadMax: a description longer than this shows its first part, the
// rest behind "More".
const descLeadMax = 300

// splitDescription: a long description's first paragraphs (about
// descLeadMax/2 at least, cut at a sentence when one paragraph is long),
// and the rest.
func splitDescription(s string) (lead, more string) {
	if len(s) <= descLeadMax {
		return s, ""
	}
	paras := strings.Split(s, "\n\n")
	n := 0
	for n < len(paras) && (n == 0 || len(strings.Join(paras[:n], "\n\n")) < descLeadMax/2) {
		n++
	}
	lead = strings.Join(paras[:n], "\n\n")
	if len(lead) > descLeadMax*3/2 {
		if i := strings.LastIndex(lead[:descLeadMax], ". "); i > descLeadMax/3 {
			return lead[:i+1], strings.TrimSpace(s[i+1:])
		}
	}
	if n == len(paras) {
		return s, ""
	}
	return lead, strings.Join(paras[n:], "\n\n")
}

// DescLead and DescMore: the description, split for showing (filtered.html).
func (d FilteredPageData) DescLead() string { l, _ := splitDescription(d.Description); return l }
func (d FilteredPageData) DescMore() string { _, m := splitDescription(d.Description); return m }

// descNarrowMax: a lead longer than this (or with this many line breaks) may
// pass the four lines a phone shows of it (style.css), so it gets a "More"
// toggle there even when there is no more to show.
const descNarrowMax = 200

// DescClamps: the lead may be cut short on a phone.
func (d FilteredPageData) DescClamps() bool {
	l := d.DescLead()
	return len(l) > descNarrowMax || strings.Count(l, "\n") >= 3
}
