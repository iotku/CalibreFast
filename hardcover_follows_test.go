package main

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/iotku/CalibreFast/internal/hardcoversync"
)

// Importing the authors followed on Hardcover as starred authors.
func TestHardcoverFollowsImport(t *testing.T) {
	u, _, f := setupHardcoverSync(t)
	for _, a := range []string{"Frank Herbert", "J. R. R. Tolkien", "Ursula K. Le Guin"} {
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
	// A lookup tied Frank Herbert to Hardcover's author 900.
	if _, err := appDB.Exec(`INSERT INTO hardcover_authors (name, hc_id, checked_at) VALUES ('Frank Herbert', 900, 1)`); err != nil {
		t.Fatal(err)
	}
	f.follows = `[
		{"id": 1, "author": {"id": 900, "name": "Franklin Herbert", "slug": "fh"}},
		{"id": 2, "author": {"id": 901, "name": "J.R.R. Tolkien", "slug": "jrrt"}},
		{"id": 3, "author": {"id": 902, "name": "Ursula Le Guin", "slug": "ulg"}},
		{"id": 4, "author": {"id": 903, "name": "Iain M. Banks", "slug": "imb"}},
		{"id": 5, "author": null}]`
	ctx := context.Background()

	// Linked before follows could be read: link again first.
	if _, err := hcSync.ImportFollows(ctx, u.ID); !errors.Is(err, hardcoversync.ErrFollowScope) {
		t.Fatalf("old link: %v", err)
	}
	if _, err := appDB.Exec(`UPDATE hardcover_links SET scope = scope || ' read:social write:social' WHERE user_id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}

	res, err := hcSync.ImportFollows(ctx, u.ID)
	if err != nil || res != (hardcoversync.FollowsResult{Total: 4, Starred: 2, Waiting: 1, Confirm: 1, Added: 3}) {
		t.Fatalf("import: %+v %v", res, err)
	}
	stars := func() []string {
		t.Helper()
		names, err := scanStrings(appDB.Query(`SELECT name || '/' || origin || '/' || COALESCE(hc_id, 0) FROM aggregate_favorites
			WHERE user_id = ? AND removed_at IS NULL ORDER BY name`, u.ID))
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	// By the stored id, by name (periods and spacing don't count), and one
	// the library doesn't have, waiting under Hardcover's name.
	want := []string{"Frank Herbert/hardcover/900", "Iain M. Banks/hardcover/903", "J. R. R. Tolkien/hardcover/901"}
	if got := stars(); !slices.Equal(got, want) {
		t.Fatalf("stars: %v", got)
	}
	v := hcSync.Follows(u.ID)
	if !v.CanRead || v.Total != 4 || len(v.Confirm) != 1 || !slices.Equal(v.Confirm[0].Candidates, []string{"Ursula K. Le Guin"}) {
		t.Fatalf("view: %+v", v)
	}

	// The near match: only what was offered can be chosen; choosing stars it,
	// undoing takes the star off and offers it again; "none" leaves it waiting.
	if err := hcSync.FollowDecision(u.ID, 902, "Frank Herbert", false); !errors.Is(err, hardcoversync.ErrNoFollow) {
		t.Fatalf("not offered: %v", err)
	}
	if err := hcSync.FollowDecision(u.ID, 902, "Ursula K. Le Guin", false); err != nil {
		t.Fatal(err)
	}
	if got := stars(); !slices.Contains(got, "Ursula K. Le Guin/hardcover/902") {
		t.Fatalf("chosen: %v", got)
	}
	if v := hcSync.Follows(u.ID); len(v.Confirm) != 0 || !slices.ContainsFunc(v.Starred, func(r hardcoversync.FollowRow) bool { return r.HCID == 902 && r.Decided }) {
		t.Fatalf("after choosing: %+v", v)
	}
	if err := hcSync.FollowDecision(u.ID, 902, "", true); err != nil {
		t.Fatal(err)
	}
	if got := stars(); slices.Contains(got, "Ursula K. Le Guin/hardcover/902") || len(hcSync.Follows(u.ID).Confirm) != 1 {
		t.Fatalf("undone: %v", got)
	}
	if err := hcSync.FollowDecision(u.ID, 902, "", false); err != nil {
		t.Fatal(err)
	}
	if got := stars(); !slices.Contains(got, "Ursula Le Guin/hardcover/902") {
		t.Fatalf("none of these: %v", got)
	}
	// And the choice holds on the next import.
	if res, err := hcSync.ImportFollows(ctx, u.ID); err != nil || res.Confirm != 0 || res.Added != 0 {
		t.Fatalf("again: %+v %v", res, err)
	}

	// A star taken off here stays off.
	if rec := call(t, favMux(), u, "DELETE", "/api/me/favorites/author/J.%20R.%20R.%20Tolkien", ""); rec.Code != 204 {
		t.Fatalf("unstar: %d", rec.Code)
	}
	if res, err := hcSync.ImportFollows(ctx, u.ID); err != nil || res.Skipped != 1 || slices.ContainsFunc(stars(), func(s string) bool { return s[0] == 'J' }) {
		t.Fatalf("taken off: %+v %v %v", res, err, stars())
	}

	// A book by the waiting author arrives, written "Banks, Iain M.": the
	// star becomes that author's, and the user hears of it.
	banks := addTestBook(t, calibreDB, "Consider Phlebas", []string{"Banks, Iain M."}, nil, "", "epub")
	catalogChanged(banks)
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	favNotifyWG.Wait()
	if got := stars(); !slices.Contains(got, "Banks, Iain M./hardcover/903") || slices.Contains(got, "Iain M. Banks/hardcover/903") {
		t.Fatalf("woken: %v", got)
	}
	ns, err := userNotifications(u.ID)
	if err != nil || len(ns) != 1 || ns[0].Kind != notifyFavoriteAuthor || ns[0].BookUUID != banks {
		t.Fatalf("told: %+v %v", ns, err)
	}

	// Unlinking forgets the import, not the stars.
	if _, err := hcSync.Unlink(u.ID); err != nil {
		t.Fatal(err)
	}
	if v := hcSync.Follows(u.ID); v.Total != 0 || len(stars()) != 3 {
		t.Fatalf("unlinked: %+v %v", v, stars())
	}
}
