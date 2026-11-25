package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// withPasswordsOff turns -disable-passwords on, with a provider set up.
func withPasswordsOff(t *testing.T) {
	t.Helper()
	origOff, origProviders := passwordsDisabled, oauthProviders
	passwordsDisabled = true
	oauthProviders = map[string]*oauthProvider{"github": githubProvider("c", "s")}
	t.Cleanup(func() { passwordsDisabled, oauthProviders = origOff, origProviders })
}

func TestDisablePasswordsRefusesPasswordSignIn(t *testing.T) {
	setupTestAppDB(t)
	if _, err := createUser("dana", "longenough-pw", false); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticate("dana", "longenough-pw"); err != nil {
		t.Fatalf("password works with the flag off: %v", err)
	}
	withPasswordsOff(t)
	if _, err := authenticate("dana", "longenough-pw"); err == nil {
		t.Fatal("password sign-in worked with -disable-passwords")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"username": {"dana"}, "password": {"longenough-pw"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginHandler(rec, req)
	if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("login: %d, cookies %v", rec.Code, rec.Result().Cookies())
	}
	// The stored hash is kept: turning the flag off restores it.
	passwordsDisabled = false
	if _, err := authenticate("dana", "longenough-pw"); err != nil {
		t.Fatalf("password gone after the flag was turned off: %v", err)
	}
}

func TestSignInAtRiskUntilLinked(t *testing.T) {
	setupTestAppDB(t)
	withPasswordsOff(t)
	u, err := createUser("erin", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !signInAtRisk(u) {
		t.Fatal("a user with no password and nothing linked isn't flagged")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/me", nil)
	meHandler(rec, req.WithContext(withUserCtx(req, u)))
	if !strings.Contains(rec.Body.String(), `"sign_in_at_risk":true`) {
		t.Fatalf("/api/me lacks the warning: %s", rec.Body)
	}
	if _, _, err := resolveOAuthUser("github", &oauthUser{Subject: "9", Login: "erin"}, u.ID, ""); err != nil {
		t.Fatal(err)
	}
	if signInAtRisk(u) {
		t.Fatal("still flagged after linking")
	}
	// A password that doesn't work here doesn't count either.
	oauthProviders = map[string]*oauthProvider{}
	if signInAtRisk(u) {
		t.Fatal("flagged where no provider is set up (nothing to link)")
	}
}

func TestUnlinkLastSignInNeedsConfirmation(t *testing.T) {
	setupTestAppDB(t)
	withPasswordsOff(t)
	u, _ := createUser("frank", "", false)
	if _, _, err := resolveOAuthUser("github", &oauthUser{Subject: "5", Login: "frank"}, u.ID, ""); err != nil {
		t.Fatal(err)
	}
	unlink := func(confirm string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/auth/github/unlink", strings.NewReader(url.Values{"confirm_username": {confirm}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetPathValue("provider", "github")
		rec := httptest.NewRecorder()
		oauthUnlink(rec, req.WithContext(withUserCtx(req, u)))
		return rec
	}
	if rec := unlink(""); !strings.Contains(rec.Header().Get("Location"), "error=") || len(identitiesFor(u.ID)) != 1 {
		t.Fatalf("unlinked the last sign-in without confirmation: %q", rec.Header().Get("Location"))
	}
	if rec := unlink("frank"); !strings.Contains(rec.Header().Get("Location"), "notice=") || len(identitiesFor(u.ID)) != 0 {
		t.Fatalf("confirmed unlink failed: %q", rec.Header().Get("Location"))
	}
}

func TestDisablePasswordsInviteMakesPasswordlessAccount(t *testing.T) {
	setupTestAppDB(t)
	withPasswordsOff(t)
	// Registration form and the account's password form are closed too.
	rec := httptest.NewRecorder()
	allowRegistrations = true
	t.Cleanup(func() { allowRegistrations = false })
	registerHandler(rec, httptest.NewRequest("POST", "/register", strings.NewReader("username=gina&password=longenough-pw&password2=longenough-pw")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("register: %d", rec.Code)
	}
}

func TestLinkedAccountsCappedPerProvider(t *testing.T) {
	setupTestAppDB(t)
	u, _ := createUser("hana", "", false)
	for i := 1; i <= maxLinkedPerProvider; i++ {
		if _, _, err := resolveOAuthUser("github", &oauthUser{Subject: itoa(int64(i))}, u.ID, ""); err != nil {
			t.Fatalf("link %d: %v", i, err)
		}
	}
	if _, _, err := resolveOAuthUser("github", &oauthUser{Subject: "99"}, u.ID, ""); err == nil {
		t.Fatal("linked one more than the cap")
	}
	// Linking an account that's already linked is not a new one; Google counts apart.
	if _, _, err := resolveOAuthUser("github", &oauthUser{Subject: "1"}, u.ID, ""); err != nil {
		t.Fatalf("relinking an existing account: %v", err)
	}
	if _, _, err := resolveOAuthUser("google", &oauthUser{Subject: "g1"}, u.ID, ""); err != nil {
		t.Fatalf("google at the github cap: %v", err)
	}
}

func TestLinkedEmailIsCutShort(t *testing.T) {
	for in, want := range map[string]string{
		"name@gmail.com": "name@gma…",
		"a@b.io":         "a@b.i…",
		"octocat":        "octocat",
		"":               "",
	} {
		if got := maskEmail(in); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", in, got, want)
		}
	}
	if got := (Identity{Email: "jo@example.org"}).Display(); got != "jo@exa…" {
		t.Errorf("Display: %q", got)
	}
	if got := (Identity{Login: "octocat", Email: "o@example.org"}).Display(); got != "octocat" {
		t.Errorf("Display with login: %q", got)
	}
}
