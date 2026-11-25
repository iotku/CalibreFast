package main

import (
	"strings"
	"testing"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

func TestHardcoverRelinkNotification(t *testing.T) {
	u, _, _ := setupHardcoverSync(t)
	linkHardcover(t, u) // the fake grants reading sync's first scopes only
	relinks := func() []notification {
		t.Helper()
		ns, err := userNotifications(u.ID)
		if err != nil {
			t.Fatal(err)
		}
		var out []notification
		for _, n := range ns {
			if n.Kind == notifyHardcoverRelink {
				out = append(out, n)
			}
		}
		return out
	}
	set := func(q string, args ...any) {
		t.Helper()
		if _, err := appDB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Linking just now isn't news; a link from before this code is.
	hcSync.NotifyRelink()
	if n := relinks(); len(n) != 0 {
		t.Fatalf("new link: %+v", n)
	}
	set(`UPDATE hardcover_links SET relink_notified = '' WHERE user_id = ?`, u.ID)
	hcSync.NotifyRelink()
	n := relinks()
	if len(n) != 1 || n[0].Title != "bring in the authors you follow there and follow the authors you star here" || n[0].Href != "/account#hardcover" {
		t.Fatalf("old link: %+v", n)
	}
	// Once: not again at the next start, nor once dismissed.
	hcSync.NotifyRelink()
	if err := dismissNotifications(u.ID, n[0].ID); err != nil {
		t.Fatal(err)
	}
	relinkGone(t, u.ID) // an account kind: deleted, not kept as history
	hcSync.NotifyRelink()
	if n := relinks(); len(n) != 0 {
		t.Fatalf("told twice: %+v", n)
	}
	// Linking asks for something new again: told again.
	set(`UPDATE hardcover_links SET relink_notified = 'read:me' WHERE user_id = ?`, u.ID)
	hcSync.NotifyRelink()
	if n := relinks(); len(n) != 1 {
		t.Fatalf("next change: %+v", n)
	}
	// Linking again: done with.
	if _, err := hcSync.Unlink(u.ID); err != nil {
		t.Fatal(err)
	}
	linkHardcover(t, u)
	if n := relinks(); len(n) != 0 {
		t.Fatalf("relinked: %+v", n)
	}
	relinkGone(t, u.ID) // cleared: deleted outright

	// A link with everything already: nothing to tell.
	set(`UPDATE hardcover_links SET relink_notified = '', scope = scope || ' read:social write:social' WHERE user_id = ?`, u.ID)
	hcSync.NotifyRelink()
	if n := relinks(); len(n) != 0 {
		t.Fatalf("all scopes: %+v", n)
	}
}

// relinkGone: userID has no relink notice, active or dismissed, and no event of
// that kind is left.
func relinkGone(t *testing.T, userID int64) {
	t.Helper()
	earlier, err := dismissedNotifications(userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range earlier {
		if n.Kind == notifyHardcoverRelink {
			t.Fatalf("relink kept as history: %+v", n)
		}
	}
	var events int
	if err := appDB.QueryRow(`SELECT COUNT(*) FROM notification_events WHERE kind = ?`, notifyHardcoverRelink).Scan(&events); err != nil || events != 0 {
		t.Fatalf("relink events left: %d (%v)", events, err)
	}
}

// The admin's setup steps name every scope linking asks for, from the same
// constant, so they can't fall behind it.
func TestHardcoverSetupScopes(t *testing.T) {
	setupTestAppDB(t)
	origSync, origClientID := hcSync, hcSyncClientID
	hcSync, hcSyncClientID = nil, ""
	t.Cleanup(func() { hcSync, hcSyncClientID = origSync, origClientID })
	admin, err := createUser("admin", "a long enough password", true)
	if err != nil {
		t.Fatal(err)
	}
	v := hcView(admin, "")
	if v == nil || v.State != "off" || v.Scopes != hardcover.SyncScopes {
		t.Fatalf("view: %+v", v)
	}
	var b strings.Builder
	if err := templates.ExecuteTemplate(&b, "account.html", AuthPageData{Title: "t", User: admin, Hardcover: v}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "<code>"+hardcover.SyncScopes+"</code>") {
		t.Fatal("the setup steps don't list the scopes")
	}
}
