package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func linksApp(t *testing.T) (*httptest.Server, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/l/{token}", linkHandler)
	mux.HandleFunc("POST /api/admin/links", requireAdminUser(adminCreateLink))
	mux.HandleFunc("DELETE /api/admin/links/{id}", requireAdminUser(adminRevokeLink))
	mux.HandleFunc("GET /api/admin/links", requireAdminUser(adminListLinks))
	mux.HandleFunc("GET /api/me", meHandler)
	srv := httptest.NewServer(withAuth(mux))
	t.Cleanup(srv.Close)
	return srv, mux
}

func newLink(t *testing.T, mux http.Handler, admin *User, body string) string {
	t.Helper()
	rec := call(t, mux, admin, "POST", "/api/admin/links", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create link: %d %s", rec.Code, rec.Body)
	}
	var out struct{ URL string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.URL[strings.Index(out.URL, "/l/"):]
}

func browser() *http.Client {
	return &http.Client{Jar: newJar(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func whoAmI(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	resp, err := c.Get(base + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ User *User }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.User == nil {
		return ""
	}
	return body.User.Username
}

func TestSignInLink(t *testing.T) {
	setupTestAppDB(t)
	admin, _ := createUser("boss", "longenough-pw", true)
	sam, _ := createUser("sam", "longenough-pw", false)
	srv, mux := linksApp(t)

	if rec := call(t, mux, sam, "POST", "/api/admin/links", `{"kind":"login","user_id":1}`); rec.Code != http.StatusNotFound {
		t.Fatalf("non-admin created a link: %d", rec.Code)
	}
	path := newLink(t, mux, admin, `{"kind":"login","user_id":`+itoa(sam.ID)+`,"expires":"1h"}`)

	// Chat apps fetch links for previews: GET must never use it up.
	for i := 0; i < 3; i++ {
		resp, _ := http.Get(srv.URL + path)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("preview GET %d: %d", i, resp.StatusCode)
		}
	}
	b := browser()
	resp, _ := b.PostForm(srv.URL+path, url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("use link: %d", resp.StatusCode)
	}
	if who := whoAmI(t, b, srv.URL); who != "sam" {
		t.Fatalf("signed in as %q", who)
	}
	// Single use.
	resp, _ = browser().PostForm(srv.URL+path, url.Values{})
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("link worked twice")
	}

	// Revoked and expired links don't work.
	path2 := newLink(t, mux, admin, `{"kind":"login","user_id":`+itoa(sam.ID)+`}`)
	var list struct{ Links []loginLink }
	_ = json.Unmarshal(call(t, mux, admin, "GET", "/api/admin/links", "").Body.Bytes(), &list)
	if len(list.Links) != 1 {
		t.Fatalf("active links: %+v", list.Links)
	}
	if rec := call(t, mux, admin, "DELETE", "/api/admin/links/"+list.Links[0].ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if _, err := lookupLink(path2[3:]); err == nil {
		t.Fatal("revoked link still valid")
	}
	path3 := newLink(t, mux, admin, `{"kind":"login","user_id":`+itoa(sam.ID)+`}`)
	_, _ = appDB.Exec(`UPDATE login_links SET expires_at = 1`)
	if _, err := lookupLink(path3[3:]); err == nil {
		t.Fatal("expired link still valid")
	}
}

func TestLinkClaimIsAtomic(t *testing.T) {
	setupTestAppDB(t)
	admin, _ := createUser("boss", "longenough-pw", true)
	_, mux := linksApp(t)
	token := newLink(t, mux, admin, `{"kind":"invite"}`)[3:]
	wins := 0
	for i := 0; i < 5; i++ {
		if claimLink(token) {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("claimed %d times", wins)
	}
}

func TestInviteLink(t *testing.T) {
	setupTestAppDB(t)
	allowRegistrations = false
	admin, _ := createUser("boss", "longenough-pw", true)
	srv, mux := linksApp(t)
	path := newLink(t, mux, admin, `{"kind":"invite","is_admin":true,"note":"welcome"}`)

	b := browser()
	// A mistake doesn't use up the invite.
	resp, _ := b.PostForm(srv.URL+path, url.Values{"username": {"x"}, "password": {"longenough-pw"}, "password2": {"longenough-pw"}})
	resp.Body.Close()
	resp, _ = b.PostForm(srv.URL+path, url.Values{"username": {"boss"}, "password": {"longenough-pw"}, "password2": {"longenough-pw"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("taken username: %d", resp.StatusCode)
	}
	resp, _ = b.PostForm(srv.URL+path, url.Values{"username": {"newadmin"}, "password": {"longenough-pw"}, "password2": {"longenough-pw"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || whoAmI(t, b, srv.URL) != "newadmin" {
		t.Fatalf("invite signup: %d", resp.StatusCode)
	}
	u, _ := authenticate("newadmin", "longenough-pw")
	if u == nil || !u.IsAdmin {
		t.Fatal("invite should have created an admin")
	}
	resp, _ = browser().PostForm(srv.URL+path, url.Values{"username": {"another"}, "password": {"longenough-pw"}, "password2": {"longenough-pw"}})
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("invite worked twice")
	}
}

func TestInviteViaOAuthWithClosedRegistration(t *testing.T) {
	setupTestAppDB(t)
	allowRegistrations = false
	admin, _ := createUser("boss", "longenough-pw", true)
	_, linkMux := linksApp(t)
	token := newLink(t, linkMux, admin, `{"kind":"invite"}`)[3:]

	prov, p := fakeProvider(t, 77, "invitee")
	defer prov.Close()
	app, client := oauthApp(t, p)
	if loc := signIn(t, app, client, "?invite="+token); loc != "/" {
		t.Fatalf("invite via oauth: %q", loc)
	}
	if who := signedInAs(t, app, client); who != "invitee" {
		t.Fatalf("signed in as %q", who)
	}
	if _, err := lookupLink(token); err == nil {
		t.Fatal("invite not used up")
	}
	// Without an invite, a new GitHub account is still refused.
	prov2, p2 := fakeProvider(t, 78, "stranger")
	defer prov2.Close()
	app2, client2 := oauthApp(t, p2)
	if loc := signIn(t, app2, client2, ""); !strings.HasPrefix(loc, "/login?error=") {
		t.Fatalf("stranger let in: %q", loc)
	}
}
