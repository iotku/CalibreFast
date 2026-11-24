package main

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// followStep runs the follow sender until nothing is due now.
func followStep(t *testing.T) {
	t.Helper()
	for range 20 {
		if ran, _ := hcSync.FollowStep(context.Background()); !ran {
			return
		}
	}
	t.Fatal("follows still due")
}

// Starring authors follows them on Hardcover, if the user chose to;
// taking a star off unfollows only when they ask.
func TestHardcoverFollowPush(t *testing.T) {
	u, _, f := setupHardcoverSync(t)
	for _, a := range []string{"Frank Herbert", "Ursula K. Le Guin", "Common Name", "Nobody Known", "Imported Author"} {
		addTestBook(t, calibreDB, "A Book by "+a, []string{a}, nil, "", "epub")
	}
	c, err := buildCatalog(calibreDB)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)
	t.Cleanup(func() { _ = catalogFlush(); libraryCatalog.Store(nil) })
	refreshAllCaches()
	linkHardcover(t, u)
	if _, err := appDB.Exec(`UPDATE hardcover_links SET scope = scope || ' read:social write:social' WHERE user_id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	f.authorsNamed = map[string][]int64{"Frank Herbert": {900}, "Ursula K. Le Guin": {901}, "Common Name": {902, 903}}
	f.followed = map[int64]int64{901: 55, 904: 66} // Le Guin followed on Hardcover; and the imported author
	if _, err := appDB.Exec(`INSERT INTO aggregate_favorites (user_id, kind, name, origin, hc_id, created_at)
		VALUES (?, 'author', 'Imported Author', 'hardcover', 904, 1)`, u.ID); err != nil {
		t.Fatal(err)
	}
	mux := favMux()
	do := func(method, path string) {
		t.Helper()
		if rec := call(t, mux, u, method, path, ""); rec.Code != 204 {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
		}
	}
	queued := func() []string {
		t.Helper()
		rows, err := scanStrings(appDB.Query(`SELECT op || ':' || name FROM hardcover_follow_queue WHERE user_id = ? ORDER BY name`, u.ID))
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	star := func(name string) (hcID, ref int64) {
		t.Helper()
		_ = appDB.QueryRow(`SELECT COALESCE(hc_id, 0), COALESCE(hc_ref, 0) FROM aggregate_favorites WHERE user_id = ? AND name = ? AND removed_at IS NULL`,
			u.ID, name).Scan(&hcID, &ref)
		return
	}
	countOps := func(op string) int {
		n := 0
		for _, o := range f.ops {
			if o == op {
				n++
			}
		}
		return n
	}

	// Following isn't on: a star stays here.
	do("PUT", "/api/me/favorites/author/Frank%20Herbert")
	if q := queued(); len(q) != 0 {
		t.Fatalf("sharing off: %v", q)
	}
	// Turned on: the stars made here go (not the imported one); a follow is
	// noted on the star.
	if err := hcSync.SaveFollowPrefs(u.ID, true, true); err != nil {
		t.Fatal(err)
	}
	if q := queued(); !slices.Equal(q, []string{"follow:Frank Herbert"}) {
		t.Fatalf("turned on: %v", q)
	}
	followStep(t)
	if id, ref := star("Frank Herbert"); id != 900 || ref != 7900 || f.followed[900] != 7900 {
		t.Fatalf("followed: %d %d %v", id, ref, f.followed)
	}
	// Followed on Hardcover already: nothing sent, and it isn't ours.
	do("PUT", "/api/me/favorites/author/Ursula%20K.%20Le%20Guin")
	followStep(t)
	if id, ref := star("Ursula K. Le Guin"); id != 901 || ref != 0 || countOps("FollowAuthor") != 1 {
		t.Fatalf("already followed: %d %d %v", id, ref, f.ops)
	}
	// Several authors by that name, or none: set aside, shown, tried again on asking.
	do("PUT", "/api/me/favorites/author/Common%20Name")
	do("PUT", "/api/me/favorites/author/Nobody%20Known")
	followStep(t)
	fv := hcSync.Follows(u.ID)
	problems, here := fv.Problems, fv.FollowedHere
	if len(problems) != 2 || problems[0].Name != "Common Name" || problems[0].Error != "several authors on Hardcover have this name" ||
		problems[1].Error != "not found on Hardcover" || here != 1 {
		t.Fatalf("set aside: %+v %d", problems, here)
	}
	f.authorsNamed["Nobody Known"] = []int64{905}
	if err := hcSync.RetryFollow(u.ID, "Nobody Known"); err != nil {
		t.Fatal(err)
	}
	followStep(t)
	if _, ref := star("Nobody Known"); ref != 7905 {
		t.Fatalf("tried again: %d", ref)
	}

	// The unstar dialog's flags: followed authors, and the user's defaults.
	var ans struct {
		Favorites []aggFavorite `json:"favorites"`
		Unfollow  bool          `json:"hc_unfollow"`
		Default   bool          `json:"hc_unfollow_default"`
		Stars     bool          `json:"hc_follow_stars"`
	}
	if err := json.Unmarshal(call(t, mux, u, "GET", "/api/me/favorites", "").Body.Bytes(), &ans); err != nil {
		t.Fatal(err)
	}
	followed := map[string]bool{}
	for _, fv := range ans.Favorites {
		followed[fv.Name] = fv.HCFollowed
	}
	if !ans.Unfollow || !ans.Default || !ans.Stars || !followed["Frank Herbert"] || !followed["Ursula K. Le Guin"] || !followed["Imported Author"] || followed["Common Name"] {
		t.Fatalf("flags: %+v", ans)
	}

	// Taken off without asking to unfollow: Hardcover isn't touched.
	do("DELETE", "/api/me/favorites/author/Frank%20Herbert")
	followStep(t)
	if len(f.unfollowed) != 0 || f.followed[900] == 0 {
		t.Fatalf("kept on Hardcover: %v", f.unfollowed)
	}
	// Asked to, then starred again before it went: the follow wins.
	do("PUT", "/api/me/favorites/author/Frank%20Herbert")
	followStep(t)
	do("DELETE", "/api/me/favorites/author/Frank%20Herbert?hardcover=1")
	do("PUT", "/api/me/favorites/author/Frank%20Herbert")
	if q := queued(); !slices.Contains(q, "follow:Frank Herbert") {
		t.Fatalf("starred again: %v", q)
	}
	followStep(t)
	// Asked to: unfollowed, whoever made the follow (here the import's).
	do("DELETE", "/api/me/favorites/author/Frank%20Herbert?hardcover=1")
	do("DELETE", "/api/me/favorites/author/Imported%20Author?hardcover=1")
	followStep(t)
	if !slices.Equal(f.unfollowed, []int64{900, 904}) {
		t.Fatalf("unfollowed: %v", f.unfollowed)
	}
	// No longer followed there: nothing to unfollow.
	delete(f.followed, 901)
	do("DELETE", "/api/me/favorites/author/Ursula%20K.%20Le%20Guin?hardcover=1")
	followStep(t)
	if countOps("UnfollowAuthor") != 2 {
		t.Fatalf("not followed: %v", f.ops)
	}

	// Following turned off: what waits to follow is dropped; unfollowing
	// on asking still works.
	if err := hcSync.SaveFollowPrefs(u.ID, false, false); err != nil {
		t.Fatal(err)
	}
	do("PUT", "/api/me/favorites/author/Frank%20Herbert")
	if q := queued(); len(q) != 0 { // the two set aside were follows too
		t.Fatalf("turned off: %v", q)
	}
	do("PUT", "/api/me/favorites/author/Ursula%20K.%20Le%20Guin")
	f.followed[901] = 56
	do("DELETE", "/api/me/favorites/author/Ursula%20K.%20Le%20Guin?hardcover=1")
	followStep(t)
	if !slices.Equal(f.unfollowed, []int64{900, 904, 901}) {
		t.Fatalf("unfollow with following off: %v", f.unfollowed)
	}
	if p := hcSync.LoadFollowPrefs(u.ID); p.Share || p.UnfollowDefault || !p.CanWrite {
		t.Fatalf("prefs: %+v", p)
	}
	ans.Stars = true
	if err := json.Unmarshal(call(t, mux, u, "GET", "/api/me/favorites", "").Body.Bytes(), &ans); err != nil || ans.Stars || ans.Default {
		t.Fatalf("flags with following off: %+v %v", ans, err)
	}
	// Unlinking drops what waits.
	if _, err := hcSync.Unlink(u.ID); err != nil {
		t.Fatal(err)
	}
	if q := queued(); len(q) != 0 {
		t.Fatalf("unlinked: %v", q)
	}
}
