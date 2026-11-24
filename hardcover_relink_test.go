package main

import "testing"

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
	hcNotifyRelink()
	if n := relinks(); len(n) != 0 {
		t.Fatalf("new link: %+v", n)
	}
	set(`UPDATE hardcover_links SET relink_notified = '' WHERE user_id = ?`, u.ID)
	hcNotifyRelink()
	n := relinks()
	if len(n) != 1 || n[0].Title != "bring in the authors you follow there and follow the authors you star here" || n[0].Href != "/account#hardcover" {
		t.Fatalf("old link: %+v", n)
	}
	// Once: not again at the next start, nor once dismissed.
	hcNotifyRelink()
	set(`DELETE FROM notifications WHERE user_id = ?`, u.ID)
	hcNotifyRelink()
	if n := relinks(); len(n) != 0 {
		t.Fatalf("told twice: %+v", n)
	}
	// Linking asks for something new again: told again.
	set(`UPDATE hardcover_links SET relink_notified = 'read:me' WHERE user_id = ?`, u.ID)
	hcNotifyRelink()
	if n := relinks(); len(n) != 1 {
		t.Fatalf("next change: %+v", n)
	}
	// Linking again: done with.
	hcRelinked(u.ID)
	if n := relinks(); len(n) != 0 {
		t.Fatalf("relinked: %+v", n)
	}
	// A link with everything already: nothing to tell.
	set(`UPDATE hardcover_links SET relink_notified = '', scope = scope || ' read:social write:social' WHERE user_id = ?`, u.ID)
	hcNotifyRelink()
	if n := relinks(); len(n) != 0 {
		t.Fatalf("all scopes: %+v", n)
	}
	if got := joinAnd([]string{"a", "b", "c"}); got != "a, b and c" {
		t.Fatalf("joinAnd: %q", got)
	}
}
