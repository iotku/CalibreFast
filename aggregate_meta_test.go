package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A one-member group under another spelling is an alias: "COMPUTERS"
// browsed and shown as "Computers". It used to be dropped as trivial
// because the names matched case-insensitively.
func TestGroupAliasKept(t *testing.T) {
	db := catalogFixture(t)
	origGroups := groups
	groups = loadGroups(filepath.Join(t.TempDir(), groupsFileName))
	t.Cleanup(func() { groups = origGroups; refreshAllCaches() })
	addTestBook(t, db, "One", []string{"A"}, []string{"COMPUTERS"}, "", "epub")
	addTestBook(t, db, "Two", []string{"B"}, []string{"COMPUTERS"}, "", "epub")

	g, err := groups.set("tag", "COMPUTERS", Group{Name: "Computers", Members: []string{"COMPUTERS"}})
	if err != nil || g.Name != "Computers" {
		t.Fatalf("alias not kept: %+v %v", g, err)
	}
	for _, v := range []string{"COMPUTERS", "computers", "Computers"} {
		name, members, grouped := groups.resolve("tag", v)
		if !grouped || name != "Computers" || !slices.Equal(members, []string{"COMPUTERS"}) {
			t.Errorf("resolve %q: %q %v %v", v, name, members, grouped)
		}
	}
	if got := groupAlias("tag", "COMPUTERS"); got != "Computers" {
		t.Errorf("groupAlias: %q", got)
	}
	if got := tagEntity("COMPUTERS"); got != "Computers" {
		t.Errorf("tagEntity: %q", got)
	}

	refreshAllCaches()
	tag := categoryByKey("tag")
	var names []string
	for _, it := range getAggregateDisplay(tag) {
		names = append(names, it.Name)
	}
	if !slices.Equal(names, []string{"Computers"}) {
		t.Errorf("aggregate page shows %v, want [Computers]", names)
	}

	// The exact same spelling is trivial: the group goes.
	if g, err := groups.set("tag", "Computers", Group{Name: "COMPUTERS", Members: []string{"COMPUTERS"}}); err != nil || g.Name != "" {
		t.Fatalf("same-name group kept: %+v %v", g, err)
	}
	if _, _, grouped := groups.resolve("tag", "COMPUTERS"); grouped {
		t.Error("still grouped")
	}
}

func aggTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, x%h, color.RGBA{200, 100, 50, 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAggregateFeaturedAndMeta(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	origGroups, origData, origWrites := groups, dataDir, allowWrites
	groups = loadGroups(filepath.Join(t.TempDir(), groupsFileName))
	dataDir, allowWrites = t.TempDir(), true
	t.Cleanup(func() { groups, dataDir, allowWrites = origGroups, origData, origWrites; refreshAllCaches() })

	addTestBook(t, db, "One", []string{"A"}, []string{"Space"}, "", "epub")
	addTestBook(t, db, "Two", []string{"A"}, []string{"Space", "Lonely"}, "", "epub")
	refreshAllCaches()
	tag := categoryByKey("tag")

	// Single-book entries are left off the page (languages excepted).
	var names []string
	for _, it := range getAggregateDisplay(tag) {
		names = append(names, it.Name)
	}
	if !slices.Equal(names, []string{"Space"}) {
		t.Errorf("tags page shows %v, want [Space]", names)
	}
	if !categoryByKey("language").ShowSingles {
		t.Error("languages should show single-book entries")
	}

	admin, _ := createUser("boss", "a long enough password", true)
	reader, _ := createUser("reader", "a long enough password", false)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/groups/{cat}/{value...}", requireWrites(groupPutHandler))
	mux.HandleFunc("PUT /api/aggregates/{cat}/featured/{value...}", requireWrites(featuredHandler))
	mux.HandleFunc("DELETE /api/aggregates/{cat}/featured/{value...}", requireWrites(featuredHandler))
	mux.HandleFunc("GET /aggregate-images/{file}", aggregateImageHandler)

	do := func(u *User, method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		rec := call(t, mux, u, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s (want %d)", method, path, rec.Code, rec.Body, want)
		}
		return rec
	}

	// Featuring is for those who can edit the library.
	do(reader, "PUT", "/api/aggregates/tag/featured/Lonely", "", http.StatusNotFound)
	do(admin, "PUT", "/api/aggregates/tag/featured/Lonely", "", http.StatusOK)
	do(admin, "PUT", "/api/aggregates/tag/featured/Space", "", http.StatusOK)
	do(admin, "PUT", "/api/aggregates/tag/featured/Space", "", http.StatusOK) // twice: no-op
	cards := featuredCards(tag)
	if len(cards) != 2 || cards[0].Name != "Lonely" || cards[1].Name != "Space" || cards[1].Count != 2 {
		t.Fatalf("featured: %+v", cards)
	}

	// A single entry's description and picture, sent with the editor's
	// multipart form; the grouping is left alone (no "members").
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("data", `{"description":"  Books about space.  "}`)
	fw, _ := mw.CreateFormFile("image", "pic.png")
	_, _ = fw.Write(aggTestPNG(t, 2000, 50))
	_ = mw.Close()
	req := asUser(httptest.NewRequest("PUT", "/api/groups/tag/Space", &body), admin)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("meta save: %d %s", rec.Code, rec.Body)
	}
	if _, _, grouped := groups.resolve("tag", "Space"); grouped {
		t.Error("saving meta made a group")
	}
	m, ok := aggregateMetaFor("tag", "Space")
	if !ok || m.Description != "Books about space." || !aggregateImageRe.MatchString(m.Image) {
		t.Fatalf("meta: %+v %v", m, ok)
	}
	imgPath := filepath.Join(dataDir, aggregateImagesDir, m.Image)
	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(f)
	f.Close()
	if err != nil || format != "jpeg" || cfg.Width != aggregateImageMaxWidth {
		t.Errorf("stored image: %s %dx%d %v", format, cfg.Width, cfg.Height, err)
	}
	if rec := do(reader, "GET", m.imageURL(), "", http.StatusOK); rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("image served as %q", rec.Header().Get("Content-Type"))
	}
	do(reader, "GET", "/aggregate-images/..%2Fcalibrefast.db", "", http.StatusNotFound)

	// Renaming (here: an alias) carries the meta and the Featured place over.
	rec = do(admin, "PUT", "/api/groups/tag/Space", `{"name":"Outer Space","members":["Space"]}`, http.StatusOK)
	var out GroupSaved
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.URL != "/tag/Outer%20Space" {
		t.Errorf("url %q", out.URL)
	}
	if m2, ok := aggregateMetaFor("tag", "Outer Space"); !ok || m2 != m {
		t.Errorf("meta not carried over: %+v", m2)
	}
	if !isFeaturedAggregate("tag", "Outer Space") || isFeaturedAggregate("tag", "Space") {
		t.Error("featured not carried over")
	}
	cards = featuredCards(tag)
	if len(cards) != 2 || cards[1].Name != "Outer Space" || cards[1].Description != "Books about space." || cards[1].ImageURL == "" {
		t.Fatalf("featured after rename: %+v", cards)
	}

	// Removing the picture deletes its file; unfeaturing by a member works.
	do(admin, "PUT", "/api/groups/tag/Space", `{"removeImage":true}`, http.StatusOK)
	if m3, _ := aggregateMetaFor("tag", "Outer Space"); m3.Image != "" || m3.Description == "" {
		t.Errorf("after removing the picture: %+v", m3)
	}
	if _, err := os.Stat(imgPath); !os.IsNotExist(err) {
		t.Errorf("image file kept: %v", err)
	}
	do(admin, "DELETE", "/api/aggregates/tag/featured/Space", "", http.StatusOK)
	if isFeaturedAggregate("tag", "Outer Space") {
		t.Error("still featured")
	}

	// An SVG picture is kept as SVG, cleaned, and served with a CSP.
	body.Reset()
	mw = multipart.NewWriter(&body)
	_ = mw.WriteField("data", `{}`)
	fw, _ = mw.CreateFormFile("image", "pic.svg")
	_, _ = fw.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 4 4" onload="alert(1)"><rect width="4" height="4"/></svg>`))
	_ = mw.Close()
	req = asUser(httptest.NewRequest("PUT", "/api/groups/tag/Space", &body), admin)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("svg save: %d %s", rec.Code, rec.Body)
	}
	m4, _ := aggregateMetaFor("tag", "Outer Space")
	if filepath.Ext(m4.Image) != ".svg" || !aggregateImageRe.MatchString(m4.Image) {
		t.Fatalf("svg stored as %q", m4.Image)
	}
	rec = do(reader, "GET", m4.imageURL(), "", http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("svg served as %q", ct)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("svg CSP %q", csp)
	}
	if strings.Contains(rec.Body.String(), "onload") || !strings.Contains(rec.Body.String(), "<rect") {
		t.Errorf("svg body %s", rec.Body)
	}
}

// GroupSaved mirrors web/types.ts for the test.
type GroupSaved struct {
	URL string `json:"url"`
}
