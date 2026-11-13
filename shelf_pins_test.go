package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

func pinMux() *http.ServeMux {
	mux := shelfMux()
	mux.HandleFunc("PUT /api/me/pins/{id}", requireUser(pinHandler))
	mux.HandleFunc("DELETE /api/me/pins/{id}", requireUser(unpinHandler))
	mux.HandleFunc("PUT /api/me/pins", requireUser(pinOrderHandler))
	mux.HandleFunc("PUT /api/me/shelves/order", requireUser(shelfOrderHandler))
	mux.HandleFunc("PUT /api/admin/featured-shelves", requireAdminUser(featuredOrderHandler))
	mux.Handle("GET /pages/", servePages("pages"))
	return mux
}

func TestShelfPins(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	var b []string
	for i := range 3 {
		b = append(b, addTestBook(t, db, "Book "+strconv.Itoa(i), []string{"A"}, nil, "", "epub"))
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	alice, _ := createUser("alice", "a long enough password", false)
	bob, _ := createUser("bob", "a long enough password", false)
	admin, _ := createUser("boss", "a long enough password", true)
	mux := pinMux()

	do := func(who *User, method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		rec := call(t, mux, who, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s %s: %d %s (want %d)", method, path, body, rec.Code, rec.Body, want)
		}
		return rec
	}
	create := func(who *User, body string) Shelf {
		t.Helper()
		var s Shelf
		_ = json.Unmarshal(do(who, "POST", "/api/shelves", body, http.StatusCreated).Body.Bytes(), &s)
		return s
	}
	id := func(s Shelf) string { return strconv.FormatInt(s.ID, 10) }
	rows := func(u *User) []string {
		t.Helper()
		got, err := pinnedShelves(u, shelfRowsMax)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, s := range got {
			names = append(names, s.Name)
		}
		return names
	}
	want := func(u *User, names ...string) {
		t.Helper()
		if got := rows(u); !slices.Equal(got, names) {
			t.Fatalf("%s's rows: %q, want %q", u.Username, got, names)
		}
	}

	picks := create(admin, `{"name":"Picks","global":true}`)
	classics := create(admin, `{"name":"Classics","global":true}`)
	plain := create(admin, `{"name":"Plain","global":true}`)
	mine := create(alice, `{"name":"Alice's"}`)
	do(admin, "PUT", "/api/shelves/"+id(picks)+"/books/"+b[1], "", http.StatusNoContent)
	do(admin, "PUT", "/api/shelves/"+id(picks)+"/books/"+b[0], "", http.StatusNoContent)

	// Only admins feature, and only global shelves.
	do(alice, "PATCH", "/api/shelves/"+id(picks), `{"featured":true}`, http.StatusForbidden)
	do(alice, "PATCH", "/api/shelves/"+id(mine), `{"featured":true}`, http.StatusBadRequest)
	if v := currentFeaturedVersion(); v != "" {
		t.Fatalf("featured version with nothing featured: %q", v)
	}
	do(admin, "PATCH", "/api/shelves/"+id(picks), `{"featured":true}`, http.StatusOK)
	do(admin, "PATCH", "/api/shelves/"+id(classics), `{"featured":true}`, http.StatusOK)

	// Featured shelves are on everyone's page by default, in featured order.
	want(alice, "Picks", "Classics")
	want(bob, "Picks", "Classics")
	got, _ := pinnedShelves(alice, shelfRowsMax)
	if len(got[0].Books) != 2 || got[0].Count != 2 || got[0].Href != "/shelf/"+id(picks) {
		t.Fatalf("Picks row: %+v", got[0])
	}

	// Signed out: featured.json, cached for good under its version.
	v := currentFeaturedVersion()
	if v == "" {
		t.Fatal("no featured version")
	}
	rec := do(nil, "GET", "/pages/discover/featured.json?v="+v, "", http.StatusOK)
	if cc := rec.Header().Get("Cache-Control"); cc != "private, max-age=31536000, immutable" {
		t.Fatalf("featured.json cache: %q", cc)
	}
	var pub []homeShelf
	_ = json.Unmarshal(rec.Body.Bytes(), &pub)
	if len(pub) != 2 || pub[0].Name != "Picks" || len(pub[0].Books) != 2 {
		t.Fatalf("featured.json: %+v", pub)
	}
	if cc := do(nil, "GET", "/pages/discover/featured.json?v=old", "", http.StatusOK).Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("stale featured.json cache: %q", cc)
	}
	// A change to a featured shelf makes a new file and version.
	do(admin, "PATCH", "/api/shelves/"+id(picks), `{"name":"Staff picks"}`, http.StatusOK)
	if currentFeaturedVersion() == v {
		t.Fatal("rename didn't change featured.json")
	}
	data, _ := os.ReadFile(featuredFile())
	_ = json.Unmarshal(data, &pub)
	if pub[0].Name != "Staff picks" {
		t.Fatalf("featured.json after rename: %+v", pub)
	}

	// The admin reorders the featured shelves.
	do(alice, "PUT", "/api/admin/featured-shelves", `{"ids":[`+id(classics)+`,`+id(picks)+`]}`, http.StatusNotFound)
	do(admin, "PUT", "/api/admin/featured-shelves", `{"ids":[`+id(classics)+`,`+id(picks)+`]}`, http.StatusNoContent)
	want(bob, "Classics", "Staff picks")

	// Unpinning sticks, also through re-featuring.
	do(alice, "DELETE", "/api/me/pins/"+id(classics), "", http.StatusNoContent)
	want(alice, "Staff picks")
	do(admin, "PATCH", "/api/shelves/"+id(classics), `{"featured":false}`, http.StatusOK)
	do(admin, "PATCH", "/api/shelves/"+id(classics), `{"featured":true}`, http.StatusOK)
	want(alice, "Staff picks")
	want(bob, "Staff picks", "Classics") // re-featured: at the end
	// Pinning it again brings it back.
	do(alice, "PUT", "/api/me/pins/"+id(classics), "", http.StatusNoContent)
	want(alice, "Classics", "Staff picks") // pinned rows first, then featured ones

	// Pins: own shelves and global ones; not someone else's personal shelf.
	do(alice, "PUT", "/api/me/pins/"+id(mine), "", http.StatusNoContent)
	do(bob, "PUT", "/api/me/pins/"+id(mine), "", http.StatusNotFound)
	do(bob, "PUT", "/api/me/pins/"+id(plain), "", http.StatusNoContent)
	want(alice, "Classics", "Alice's", "Staff picks")
	want(bob, "Plain", "Staff picks", "Classics")
	// Pinning a featured shelf already shown doesn't move it.
	do(bob, "PUT", "/api/me/pins/"+id(classics), "", http.StatusNoContent)
	want(bob, "Plain", "Staff picks", "Classics")

	// The first reorder freezes the list: a shelf featured later comes after.
	do(bob, "PUT", "/api/me/pins", `{"ids":[`+id(picks)+`,`+id(plain)+`,`+id(classics)+`,`+id(mine)+`]}`, http.StatusNoContent)
	want(bob, "Staff picks", "Plain", "Classics")
	do(admin, "PUT", "/api/admin/featured-shelves", `{"ids":[`+id(picks)+`,`+id(classics)+`]}`, http.StatusNoContent)
	want(bob, "Staff picks", "Plain", "Classics") // his own order stands
	do(admin, "PATCH", "/api/shelves/"+id(plain), `{"featured":true}`, http.StatusOK)
	want(alice, "Classics", "Alice's", "Staff picks", "Plain")
	// A reorder can't pin what the user unpinned.
	do(bob, "DELETE", "/api/me/pins/"+id(plain), "", http.StatusNoContent)
	do(bob, "PUT", "/api/me/pins", `{"ids":[`+id(plain)+`,`+id(classics)+`]}`, http.StatusNoContent)
	want(bob, "Classics", "Staff picks")
	do(bob, "PUT", "/api/me/pins", `{"ids":[1,1]}`, http.StatusBadRequest)

	// The /shelves list says what's pinned and featured.
	_, global, err := listShelves(alice)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range global {
		if s.Name == "Classics" && (!s.Featured || !s.Pinned) {
			t.Fatalf("Classics for alice: %+v", s)
		}
	}
	_, global, _ = listShelves(nil)
	for _, s := range global {
		if s.Pinned {
			t.Fatalf("pinned for a signed-out visitor: %+v", s)
		}
	}

	// Deleting a featured shelf takes it off every page and featured.json.
	do(admin, "DELETE", "/api/shelves/"+id(picks), "", http.StatusNoContent)
	want(bob, "Classics")
	data, _ = os.ReadFile(featuredFile())
	_ = json.Unmarshal(data, &pub)
	if len(pub) != 2 || pub[0].Name != "Classics" {
		t.Fatalf("featured.json after delete: %+v", pub)
	}

	// The home answer carries the rows.
	rec = httptest.NewRecorder()
	homeHandler(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/me/home", nil), alice))
	var h homeAnswer
	_ = json.Unmarshal(rec.Body.Bytes(), &h)
	if len(h.Shelves) != 3 || h.Shelves[1].Name != "Alice's" || h.Shelves[1].Global {
		t.Fatalf("home shelves: %+v", h.Shelves)
	}
}

func TestShelfOrder(t *testing.T) {
	alice, _ := setupLibraryAndUser(t, 0)
	bob, _ := createUser("bob", "longenough-pw", false)
	mux := pinMux()
	var ids []int64
	for _, n := range []string{"B", "A", "C"} {
		var s Shelf
		_ = json.Unmarshal(call(t, mux, alice, "POST", "/api/shelves", `{"name":"`+n+`"}`).Body.Bytes(), &s)
		ids = append(ids, s.ID)
	}
	names := func() []string {
		mine, _, _ := listShelves(alice)
		var out []string
		for _, s := range mine {
			out = append(out, s.Name)
		}
		return out
	}
	if got := names(); !slices.Equal(got, []string{"A", "B", "C"}) {
		t.Fatalf("by name: %q", got)
	}
	body := `{"ids":[` + itoa(ids[2]) + `,` + itoa(ids[0]) + `]}`
	// Someone else's order can't touch these.
	if c := call(t, mux, bob, "PUT", "/api/me/shelves/order", body).Code; c != http.StatusNoContent {
		t.Fatal(c)
	}
	if got := names(); !slices.Equal(got, []string{"A", "B", "C"}) {
		t.Fatalf("after bob's order: %q", got)
	}
	if c := call(t, mux, alice, "PUT", "/api/me/shelves/order", body).Code; c != http.StatusNoContent {
		t.Fatal(c)
	}
	if got := names(); !slices.Equal(got, []string{"C", "B", "A"}) {
		t.Fatalf("after reorder: %q", got)
	}
}

func TestTagPins(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	origGroups := groups
	groups = loadGroups(filepath.Join(t.TempDir(), groupsFileName))
	t.Cleanup(func() { groups = origGroups })
	if _, err := groups.set("tag", "Sci-Fi", Group{Name: "SF", Members: []string{"Sci-Fi", "Science Fiction"}}); err != nil {
		t.Fatal(err)
	}
	opera := map[string]bool{}
	for i := range 6 {
		tags := []string{"Sci-Fi"}
		if i < 3 {
			tags = []string{"Space Opera", "Science Fiction"}
		}
		uuid := addTestBook(t, db, "Book "+strconv.Itoa(i), []string{"A"}, tags, "", "epub")
		if i < 3 {
			opera[uuid] = true
		}
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	alice, _ := createUser("alice", "a long enough password", false)
	bob, _ := createUser("bob", "a long enough password", false)
	admin, _ := createUser("boss", "a long enough password", true)
	mux := pinMux()
	mux.HandleFunc("PUT /api/me/tag-pins/{name}", requireUser(tagPinHandler))
	mux.HandleFunc("DELETE /api/me/tag-pins/{name}", requireUser(tagUnpinHandler))
	do := func(who *User, method, path, body string, want int) []byte {
		t.Helper()
		rec := call(t, mux, who, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s %s: %d %s (want %d)", method, path, body, rec.Code, rec.Body, want)
		}
		return rec.Body.Bytes()
	}
	rows := func(u *User) []string {
		t.Helper()
		got, err := pinnedShelves(u, shelfRowsMax)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range got {
			out = append(out, r.Kind+":"+r.Name)
		}
		return out
	}
	var picks Shelf
	_ = json.Unmarshal(do(admin, "POST", "/api/shelves", `{"name":"Picks","global":true}`, http.StatusCreated), &picks)
	do(admin, "PATCH", "/api/shelves/"+itoa(picks.ID), `{"featured":true}`, http.StatusOK)

	// Pinned tags come after what's there; a member's name pins its group.
	do(alice, "PUT", "/api/me/tag-pins/Space%20Opera", "", http.StatusNoContent)
	do(alice, "PUT", "/api/me/tag-pins/Science%20Fiction", "", http.StatusNoContent)
	do(alice, "PUT", "/api/me/tag-pins/SF", "", http.StatusNoContent) // already pinned: no change
	do(alice, "PUT", "/api/me/tag-pins/Nothing%20Here", "", http.StatusNotFound)
	do(nil, "PUT", "/api/me/tag-pins/SF", "", http.StatusUnauthorized)
	// Pinned rows first (the tags), then the featured shelf not yet placed.
	if got := rows(alice); !slices.Equal(got, []string{"tag:Space Opera", "tag:SF", "shelf:Picks"}) {
		t.Fatalf("alice's rows: %q", got)
	}
	if got := rows(bob); !slices.Equal(got, []string{"shelf:Picks"}) {
		t.Fatalf("bob's rows: %q", got)
	}
	all, _ := pinnedShelves(alice, shelfRowsMax)
	if all[0].Count != 3 || len(all[0].Books) != 3 || all[0].Href != "/tag/Space%20Opera" || all[1].Count != 6 {
		t.Fatalf("tag rows: %+v", all[:2])
	}
	for _, b := range all[0].Books {
		if !opera[b.UUID] {
			t.Fatalf("Space Opera row holds %s", b.Title)
		}
	}

	// One order for shelves and tags.
	do(alice, "PUT", "/api/me/pins", `{"rows":[{"shelf":`+itoa(picks.ID)+`},{"tag":"sf"},{"tag":"Space Opera"}]}`, http.StatusNoContent)
	if got := rows(alice); !slices.Equal(got, []string{"shelf:Picks", "tag:SF", "tag:Space Opera"}) {
		t.Fatalf("after reorder: %q", got)
	}
	do(alice, "PUT", "/api/me/pins", `{"rows":[{"tag":"SF"},{"tag":"Sci-Fi"}]}`, http.StatusBadRequest) // the same tag twice
	do(alice, "PUT", "/api/me/pins", `{"rows":[{"tag":"SF","shelf":1}]}`, http.StatusBadRequest)
	// A row not in the list (pinned in another tab, say) goes after.
	do(alice, "PUT", "/api/me/pins", `{"rows":[{"tag":"Space Opera"},{"shelf":`+itoa(picks.ID)+`}]}`, http.StatusNoContent)
	if got := rows(alice); !slices.Equal(got, []string{"tag:Space Opera", "shelf:Picks", "tag:SF"}) {
		t.Fatalf("after partial reorder: %q", got)
	}
	// A new pin goes at the end of everything.
	var mine Shelf
	_ = json.Unmarshal(do(alice, "POST", "/api/shelves", `{"name":"Mine"}`, http.StatusCreated), &mine)
	do(alice, "PUT", "/api/shelves/"+itoa(mine.ID)+"/books/"+all[0].Books[0].UUID, "", http.StatusNoContent)
	do(alice, "PUT", "/api/me/pins/"+itoa(mine.ID), "", http.StatusNoContent)
	if got := rows(alice); !slices.Equal(got, []string{"tag:Space Opera", "shelf:Picks", "tag:SF", "shelf:Mine"}) {
		t.Fatalf("after pinning a shelf: %q", got)
	}

	// Unpinning by a member's name takes the group's row off.
	do(alice, "DELETE", "/api/me/tag-pins/Sci-Fi", "", http.StatusNoContent)
	if got := rows(alice); !slices.Equal(got, []string{"tag:Space Opera", "shelf:Picks", "shelf:Mine"}) {
		t.Fatalf("after unpinning SF: %q", got)
	}
	if tagPinnedFor(alice, "SF") || !tagPinnedFor(alice, "space opera") {
		t.Fatal("tagPinnedFor")
	}
}
