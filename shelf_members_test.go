package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func memberMux() *http.ServeMux {
	mux := sharedShelfMux()
	mux.HandleFunc("PUT /api/shelves/{id}/series/{name...}", requireUser(shelfAddSeriesHandler))
	mux.HandleFunc("GET /api/shelves/{id}/members", requireUser(shelfMembersHandler))
	mux.HandleFunc("POST /api/shelves/{id}/invites", requireUser(shelfInviteHandler))
	mux.HandleFunc("DELETE /api/shelves/{id}/invites/{invite}", requireUser(shelfRevokeInviteHandler))
	mux.HandleFunc("DELETE /api/shelves/{id}/members/{user}", requireUser(shelfRemoveMemberHandler))
	mux.HandleFunc("GET /api/me/shelf-invites", requireUser(myShelfInvitesHandler))
	mux.HandleFunc("POST /api/me/shelf-invites/{invite}/accept", requireUser(acceptShelfInviteHandler))
	mux.HandleFunc("POST /api/me/shelf-invites/{invite}/decline", requireUser(declineShelfInviteHandler))
	mux.HandleFunc("DELETE /api/me/shelf-blocks/{user}", requireUser(unblockInviterHandler))
	mux.HandleFunc("GET /admin/shelf-invites", requireAdminUser(shelfInvitesAdminPage))
	return mux
}

// logActions: the invite log's actions, oldest first, as "action from>to (detail)".
func logActions(t *testing.T) []string {
	t.Helper()
	rows, err := appDB.Query(`SELECT action, from_name, to_name, detail FROM shelf_invite_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a, f, to, d string
		_ = rows.Scan(&a, &f, &to, &d)
		s := a + " " + f + ">" + to
		if d != "" {
			s += " (" + d + ")"
		}
		out = append(out, s)
	}
	return out
}

func TestShelfCollaboration(t *testing.T) {
	alice, books := setupLibraryAndUser(t, 3) // "reader"
	bob, _ := createUser("bob", "longenough-pw", false)
	carol, _ := createUser("carol", "longenough-pw", false)
	admin, _ := createUser("boss", "longenough-pw", true)
	mux := memberMux()
	do := func(who *User, method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		rec := call(t, mux, who, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s %s: %d %s (want %d)", method, path, body, rec.Code, rec.Body, want)
		}
		return rec
	}
	var s Shelf
	_ = json.Unmarshal(do(alice, "POST", "/api/shelves", `{"name":"Together"}`, http.StatusCreated).Body.Bytes(), &s)
	path := "/api/shelves/" + itoa(s.ID)
	do(alice, "PUT", path+"/books/"+books[0], "", http.StatusNoContent)

	// Only to a shared shelf, only by its owner, only real accounts.
	do(alice, "POST", path+"/invites", `{"username":"bob"}`, http.StatusConflict)
	do(alice, "PATCH", path, `{"shared":true}`, http.StatusOK)
	do(bob, "POST", path+"/invites", `{"username":"carol"}`, http.StatusForbidden)
	do(alice, "POST", path+"/invites", `{"username":"nobody"}`, http.StatusNotFound)
	do(alice, "POST", path+"/invites", `{"username":"reader"}`, http.StatusBadRequest)
	var inv shelfInvite
	_ = json.Unmarshal(do(alice, "POST", path+"/invites", `{"username":"bob"}`, http.StatusCreated).Body.Bytes(), &inv)
	do(alice, "POST", path+"/invites", `{"username":"bob"}`, http.StatusConflict) // already invited

	// Bob is told, and sees it; carol can't answer it.
	ns, _ := userNotifications(bob.ID)
	if len(ns) != 1 || ns[0].Kind != notifyShelfInvite || ns[0].Title != "reader" || ns[0].Subject != "Together" || ns[0].Href != "/shelves#invites" {
		t.Fatalf("bob's notifications: %+v", ns)
	}
	var mine struct{ Invites []shelfInvite }
	_ = json.Unmarshal(do(bob, "GET", "/api/me/shelf-invites", "", http.StatusOK).Body.Bytes(), &mine)
	if len(mine.Invites) != 1 || mine.Invites[0].From != "reader" {
		t.Fatalf("bob's invites: %+v", mine)
	}
	do(carol, "POST", "/api/me/shelf-invites/"+itoa(inv.ID)+"/accept", "", http.StatusNotFound)
	// Invited isn't a member yet.
	do(bob, "PUT", path+"/books/"+books[1], "", http.StatusForbidden)

	do(bob, "POST", "/api/me/shelf-invites/"+itoa(inv.ID)+"/accept", "", http.StatusOK)
	if ns, _ := userNotifications(bob.ID); len(ns) != 0 {
		t.Fatalf("the invite's notification stayed: %+v", ns)
	}

	// A member changes the books, not the shelf.
	do(bob, "PUT", path+"/books/"+books[1], "", http.StatusNoContent)
	do(bob, "POST", path+"/books", `{"uuids":["`+books[2]+`"]}`, http.StatusOK)
	do(bob, "DELETE", path+"/books/"+books[2], "", http.StatusNoContent)
	do(bob, "PUT", path+"/order", `{"order":["`+books[1]+`","`+books[0]+`"]}`, http.StatusNoContent)
	do(bob, "PATCH", path, `{"name":"Mine now"}`, http.StatusForbidden)
	do(bob, "PATCH", path, `{"shared":false}`, http.StatusForbidden)
	do(bob, "DELETE", path, "", http.StatusForbidden)
	do(bob, "POST", path+"/invites", `{"username":"carol"}`, http.StatusForbidden)
	marks := shelfMarks(bob, books[0])
	if len(marks) != 1 || marks[0].ID != s.ID || !marks[0].CanEdit || !marks[0].Has || marks[0].Owner != "reader" {
		t.Fatalf("bob's book popup shelves: %+v", marks)
	}
	var lists struct{ Shared, Member []Shelf }
	_ = json.Unmarshal(do(bob, "GET", "/api/shelves", "", http.StatusOK).Body.Bytes(), &lists)
	if len(lists.Member) != 1 || !lists.Member[0].CanAdd || lists.Member[0].CanEdit || len(lists.Shared) != 0 {
		t.Fatalf("bob's lists: %+v", lists)
	}
	page := do(bob, "GET", "/shelf/"+itoa(s.ID), "", http.StatusOK).Body.String()
	if !strings.Contains(page, `id="list-edit"`) || strings.Contains(page, `data-shelf-action="edit"`) {
		t.Fatalf("a member's shelf page should offer Arrange, not Edit shelf")
	}
	var people struct {
		Members []shelfMember
		Invites []shelfInvite
	}
	_ = json.Unmarshal(do(bob, "GET", path+"/members", "", http.StatusOK).Body.Bytes(), &people)
	if len(people.Members) != 1 || people.Invites != nil {
		t.Fatalf("a member sees the members, not the invites: %+v", people)
	}
	do(carol, "GET", path+"/members", "", http.StatusForbidden)

	// Unshared: its unanswered invites go; bob still helps.
	_ = json.Unmarshal(do(alice, "POST", path+"/invites", `{"username":"carol"}`, http.StatusCreated).Body.Bytes(), &inv)
	do(alice, "PATCH", path, `{"shared":false}`, http.StatusOK)
	_ = json.Unmarshal(do(carol, "GET", "/api/me/shelf-invites", "", http.StatusOK).Body.Bytes(), &mine)
	if len(mine.Invites) != 0 {
		t.Fatalf("an invite to an unshared shelf stayed: %+v", mine)
	}
	if ns, _ := userNotifications(carol.ID); len(ns) != 0 {
		t.Fatalf("carol's notification stayed: %+v", ns)
	}
	do(bob, "GET", "/shelf/"+itoa(s.ID)+"?api=1", "", http.StatusOK)
	do(carol, "GET", "/shelf/"+itoa(s.ID)+"?api=1", "", http.StatusNotFound)

	// Removed: bob can't see the (unshared) shelf any more.
	do(carol, "DELETE", path+"/members/"+itoa(bob.ID), "", http.StatusNotFound)
	do(alice, "DELETE", path+"/members/"+itoa(bob.ID), "", http.StatusNoContent)
	do(bob, "GET", "/shelf/"+itoa(s.ID)+"?api=1", "", http.StatusNotFound)

	// Decline and block: no more invites from alice.
	do(alice, "PATCH", path, `{"shared":true}`, http.StatusOK)
	_ = json.Unmarshal(do(alice, "POST", path+"/invites", `{"username":"carol"}`, http.StatusCreated).Body.Bytes(), &inv)
	do(carol, "POST", "/api/me/shelf-invites/"+itoa(inv.ID)+"/decline", `{"block":true}`, http.StatusNoContent)
	do(alice, "POST", path+"/invites", `{"username":"carol"}`, http.StatusForbidden)
	do(carol, "DELETE", "/api/me/shelf-blocks/"+itoa(alice.ID), "", http.StatusNoContent)
	_ = json.Unmarshal(do(alice, "POST", path+"/invites", `{"username":"carol"}`, http.StatusCreated).Body.Bytes(), &inv)
	do(carol, "POST", "/api/me/shelf-invites/"+itoa(inv.ID)+"/decline", "", http.StatusNoContent)
	// Bob was invited again and leaves after accepting.
	_ = json.Unmarshal(do(alice, "POST", path+"/invites", `{"username":"bob"}`, http.StatusCreated).Body.Bytes(), &inv)
	do(bob, "POST", "/api/me/shelf-invites/"+itoa(inv.ID)+"/accept", "", http.StatusOK)
	do(bob, "DELETE", path+"/members/"+itoa(bob.ID), "", http.StatusNoContent)

	want := []string{
		"refused reader>nobody (no such user)",
		"sent reader>bob",
		"accepted reader>bob",
		"sent reader>carol",
		"withdrawn reader>carol (shelf no longer shared)",
		"removed reader>bob",
		"sent reader>carol",
		"blocked reader>carol",
		"refused reader>carol (blocked by them)",
		"unblocked reader>carol",
		"sent reader>carol",
		"declined reader>carol",
		"sent reader>bob",
		"accepted reader>bob",
		"left reader>bob",
	}
	if got := logActions(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("log:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Admins read it; nobody else does.
	do(bob, "GET", "/admin/shelf-invites", "", http.StatusNotFound)
	body := do(admin, "GET", "/admin/shelf-invites", "", http.StatusOK).Body.String()
	if !strings.Contains(body, "invite-suspect") || !strings.Contains(body, "blocked by them") {
		t.Fatalf("admin page: alice, blocked once, should be flagged")
	}
	do(admin, "GET", "/admin/shelf-invites?user="+itoa(carol.ID), "", http.StatusOK)

	// The log outlives the accounts in it.
	if _, err := appDB.Exec(`DELETE FROM users WHERE id = ?`, alice.ID); err != nil {
		t.Fatal(err)
	}
	if got := logActions(t); len(got) != len(want) {
		t.Fatalf("log after deleting the inviter: %d entries", len(got))
	}
}

func TestShelfInviteLimits(t *testing.T) {
	alice, _ := setupLibraryAndUser(t, 0)
	mux := memberMux()
	var s Shelf
	_ = json.Unmarshal(call(t, mux, alice, "POST", "/api/shelves", `{"name":"Busy","description":""}`).Body.Bytes(), &s)
	call(t, mux, alice, "PATCH", "/api/shelves/"+itoa(s.ID), `{"shared":true}`)
	if _, err := createUser("target", "longenough-pw", false); err != nil {
		t.Fatal(err)
	}
	// A day's worth sent already.
	now := time.Now().Unix()
	for i := 0; i < maxInvitesPerDay; i++ {
		if _, err := appDB.Exec(`INSERT INTO shelf_invite_log (at, action, from_id, from_name, to_id, to_name, shelf_id, shelf_name)
			VALUES (?, 'sent', ?, 'reader', 0, 'x', ?, 'Busy')`, now-int64(i), alice.ID, s.ID); err != nil {
			t.Fatal(err)
		}
	}
	rec := call(t, mux, alice, "POST", "/api/shelves/"+itoa(s.ID)+"/invites", `{"username":"target"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over the daily limit: %d %s", rec.Code, rec.Body)
	}
	got := logActions(t)
	if last := got[len(got)-1]; last != "refused reader>target (daily limit)" {
		t.Fatalf("refusal not logged: %q", last)
	}
	// A day later, it goes.
	if _, err := appDB.Exec(`UPDATE shelf_invite_log SET at = at - 86400`); err != nil {
		t.Fatal(err)
	}
	if c := call(t, mux, alice, "POST", "/api/shelves/"+itoa(s.ID)+"/invites", `{"username":"target"}`).Code; c != http.StatusCreated {
		t.Fatalf("after a day: %d", c)
	}
}
