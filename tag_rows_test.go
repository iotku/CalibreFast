package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

func TestTagRows(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	origGroups := groups
	groups = loadGroups(filepath.Join(t.TempDir(), groupsFileName))
	t.Cleanup(func() { groups = origGroups })
	if _, err := groups.set("tag", "Sci-Fi", Group{Name: "SF", Members: []string{"Sci-Fi", "Science Fiction"}}); err != nil {
		t.Fatal(err)
	}

	// 20 books, all "Fiction"; then a few smaller tags.
	tagsOf := func(i int) []string {
		tags := []string{"Fiction"}
		if i < 5 {
			tags = append(tags, "Space Opera")
		}
		if i >= 5 && i < 8 {
			tags = append(tags, "Sci-Fi")
		}
		if i >= 8 && i < 10 {
			tags = append(tags, "Science Fiction")
		}
		if i == 10 || i == 11 {
			tags = append(tags, "Rare")
		}
		if i >= 12 && i < 16 {
			tags = append(tags, "Owned")
		}
		return tags
	}
	byTag := map[string][]string{}
	for i := range 20 {
		uuid := addTestBook(t, db, "Book "+strconv.Itoa(i), []string{"A"}, tagsOf(i), "", "epub")
		for _, tag := range tagsOf(i) {
			byTag[tag] = append(byTag[tag], uuid)
		}
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	admin, _ := createUser("boss", "a long enough password", true)
	reader, _ := createUser("reader", "a long enough password", false)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/admin/discover", requireAdminUser(discoverSettingsHandler))
	mux.HandleFunc("PUT /api/admin/discover/settings", requireAdminUser(discoverSettingsPutHandler))
	mux.HandleFunc("PUT /api/admin/discover/tags/{name}", requireAdminUser(discoverTagPutHandler))
	mux.HandleFunc("DELETE /api/admin/discover/tags/{name}", requireAdminUser(discoverTagDeleteHandler))
	mux.Handle("GET /pages/", servePages("pages"))
	do := func(who *User, method, path, body string, want int) []byte {
		t.Helper()
		rec := call(t, mux, who, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s (want %d)", method, path, rec.Code, rec.Body, want)
		}
		return rec.Body.Bytes()
	}
	rows := func() map[string]int { // name -> count
		out := map[string]int{}
		for _, r := range currentTagRows().index {
			out[r.Name] = r.Count
		}
		return out
	}
	names := func() []string {
		var out []string
		for _, r := range currentTagRows().index {
			out = append(out, r.Name)
		}
		return out
	}

	// Fiction is on everything (too broad); Rare has too few books; the
	// group stands for both its members.
	if got := names(); !slices.Equal(got, []string{"SF", "Space Opera", "Owned"}) {
		t.Fatalf("rows: %q", got)
	}
	if rows()["SF"] != 5 {
		t.Fatalf("SF counts its members' books once each: %v", rows())
	}
	// Each file: the tag's books (the group's, for SF), at most tagRowBooks.
	for _, r := range currentTagRows().index {
		data, err := os.ReadFile(tagRowFile(r.N))
		if err != nil {
			t.Fatal(err)
		}
		var books []Book
		_ = json.Unmarshal(data, &books)
		want := byTag[r.Name]
		if r.Name == "SF" {
			want = append(slices.Clone(byTag["Sci-Fi"]), byTag["Science Fiction"]...)
		}
		if len(books) != len(want) {
			t.Fatalf("%s: %d books, want %d", r.Name, len(books), len(want))
		}
		for _, b := range books {
			if !slices.Contains(want, b.UUID) {
				t.Fatalf("%s holds %s", r.Name, b.Title)
			}
		}
	}
	// Cached for good under the rows' version, revalidated otherwise.
	v := currentTagRows().version
	rec := call(t, mux, reader, "GET", "/pages/discover/tag-0.json?v="+v, "")
	if cc := rec.Header().Get("Cache-Control"); rec.Code != 200 || cc != "private, max-age=31536000, immutable" {
		t.Fatalf("tag file: %d %q", rec.Code, cc)
	}
	if cc := call(t, mux, reader, "GET", "/pages/discover/tag-0.json?v=old", "").Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("stale tag file: %q", cc)
	}

	// Admins only.
	do(reader, "GET", "/api/admin/discover", "", http.StatusNotFound)
	do(reader, "PUT", "/api/admin/discover/tags/Owned", `{"mode":"deny"}`, http.StatusNotFound)

	// Include and exclude: Rare gets a row despite its size, Fiction despite
	// its breadth; Owned goes. A member's name stands for its group.
	do(admin, "PUT", "/api/admin/discover/tags/Rare", `{"mode":"include"}`, http.StatusNoContent)
	do(admin, "PUT", "/api/admin/discover/tags/Fiction", `{"mode":"include"}`, http.StatusNoContent)
	do(admin, "PUT", "/api/admin/discover/tags/Owned", `{"mode":"deny"}`, http.StatusNoContent)
	do(admin, "PUT", "/api/admin/discover/tags/Science%20Fiction", `{"mode":"deny"}`, http.StatusNoContent)
	do(admin, "PUT", "/api/admin/discover/tags/Owned", `{"mode":"maybe"}`, http.StatusBadRequest)
	if got := names(); !slices.Equal(got, []string{"Fiction", "Rare", "Space Opera"}) {
		t.Fatalf("rows after include/deny: %q", got)
	}
	if v2 := currentTagRows().version; v2 == v {
		t.Fatal("version unchanged")
	}
	if _, err := os.Stat(tagRowFile(3)); err == nil {
		t.Fatal("a file from the longer list was left behind")
	}
	var answer struct {
		Share float64           `json:"broad_tag_share"`
		Deny  []string          `json:"deny"`
		Tags  []discoverTagInfo `json:"tags"`
	}
	_ = json.Unmarshal(do(admin, "GET", "/api/admin/discover", "", http.StatusOK), &answer)
	status := map[string]string{}
	for _, tg := range answer.Tags {
		status[tg.Name] = tg.Status
	}
	if answer.Share != defaultBroadTagShare || !slices.Equal(answer.Deny, []string{"Owned", "SF"}) ||
		status["SF"] != tagDenied || status["Fiction"] != tagIncluded || status["Space Opera"] != tagInRows {
		t.Fatalf("settings: %+v", answer)
	}

	// Back to the default, and a lower cutoff: 5 of 20 is over 20%.
	do(admin, "DELETE", "/api/admin/discover/tags/Fiction", "", http.StatusNoContent)
	do(admin, "DELETE", "/api/admin/discover/tags/Sci-Fi", "", http.StatusNoContent) // the group's entry
	do(admin, "PUT", "/api/admin/discover/settings", `{"broad_tag_share":0}`, http.StatusBadRequest)
	do(admin, "PUT", "/api/admin/discover/settings", `{"broad_tag_share":0.2}`, http.StatusNoContent)
	if got := names(); !slices.Equal(got, []string{"Rare"}) {
		t.Fatalf("rows at 20%%: %q", got)
	}
	_ = json.Unmarshal(do(admin, "GET", "/api/admin/discover", "", http.StatusOK), &answer)
	for _, tg := range answer.Tags {
		if (tg.Name == "SF" || tg.Name == "Space Opera") && tg.Status != tagBroad {
			t.Fatalf("%s at 20%%: %+v", tg.Name, tg)
		}
	}
}
