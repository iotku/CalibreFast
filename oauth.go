package main

// GitHub and Google sign-in (OAuth 2.0 authorization code flow + PKCE),
// implemented on net/http to avoid extra dependencies.
//
// Security notes:
//   - state is random, stored server-side (10 min) and bound to the browser
//     by an HttpOnly cookie; the callback needs both to match (no login CSRF).
//   - PKCE (S256) is used for both providers.
//   - Accounts are matched by the provider's stable user id, never by email,
//     so someone can't take over an account by registering its email address
//     with a provider. Linking is explicit, from the account page.
//   - New accounts are only created on sign-in with -allow-registrations.
//
// Configure with environment variables (kept out of `ps`):
//   CF_GITHUB_CLIENT_ID / CF_GITHUB_CLIENT_SECRET
//   CF_GOOGLE_CLIENT_ID / CF_GOOGLE_CLIENT_SECRET
// plus -public-url; callback URLs are <public-url>/auth/<provider>/callback.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iotku/CalibreFast/pkg/logsafe"
)

// Identity is an external sign-in (GitHub, Google) linked to a user.
type Identity struct {
	Provider string
	Subject  string
	Email    string
	Login    string
}

// Display is what the account page shows for a linked sign-in: the login
// name, or the email address when there is none. An email is cut short
// (maskEmail): Cloudflare's email obfuscation rewrites whole addresses in
// a page, even a private one, and the start is enough to tell which
// account is linked.
func (i Identity) Display() string {
	if i.Login != "" {
		return maskEmail(i.Login)
	}
	return maskEmail(i.Email)
}

// maskEmail cuts the end off an email address ("name@gmail.com" becomes
// "name@gma…"), so it no longer looks like one to anything scanning the
// page. Text without an "@" is returned as it is.
func maskEmail(s string) string {
	local, domain, ok := strings.Cut(s, "@")
	if !ok {
		return s
	}
	if r := []rune(domain); len(r) > 3 {
		domain = string(r[:3])
	}
	return local + "@" + domain + "…"
}

// oauthUser is what a provider tells us about the person signing in.
type oauthUser struct {
	Subject string // stable provider id
	Login   string // GitHub username, or ""
	Email   string // for display only
	Name    string
}

type oauthProvider struct {
	Name         string
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	Scopes       []string
	ExtraAuth    url.Values
	// fetchUser looks up the signed-in user with the access token.
	fetchUser func(ctx context.Context, p *oauthProvider, token string) (*oauthUser, error)
	// API endpoints (overridable in tests).
	UserURL   string
	EmailsURL string
}

var (
	oauthProviders = map[string]*oauthProvider{}
	oauthHTTP      = &http.Client{Timeout: 10 * time.Second}
)

func githubProvider(id, secret string) *oauthProvider {
	return &oauthProvider{
		Name: "github", ClientID: id, ClientSecret: secret,
		AuthURL:   "https://github.com/login/oauth/authorize",
		TokenURL:  "https://github.com/login/oauth/access_token",
		Scopes:    []string{"read:user", "user:email"},
		UserURL:   "https://api.github.com/user",
		EmailsURL: "https://api.github.com/user/emails",
		fetchUser: fetchGitHubUser,
	}
}

func googleProvider(id, secret string) *oauthProvider {
	return &oauthProvider{
		Name: "google", ClientID: id, ClientSecret: secret,
		AuthURL:   "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:  "https://oauth2.googleapis.com/token",
		Scopes:    []string{"openid", "email", "profile"},
		ExtraAuth: url.Values{"prompt": {"select_account"}},
		UserURL:   "https://openidconnect.googleapis.com/v1/userinfo",
		fetchUser: fetchGoogleUser,
	}
}

// configureOAuth enables providers whose credentials are in the environment.
func configureOAuth() {
	add := func(name string, mk func(id, secret string) *oauthProvider) {
		id := os.Getenv("CF_" + strings.ToUpper(name) + "_CLIENT_ID")
		secret := os.Getenv("CF_" + strings.ToUpper(name) + "_CLIENT_SECRET")
		if id == "" && secret == "" {
			return
		}
		if id == "" || secret == "" {
			log.Printf("oauth: %s needs both CF_%s_CLIENT_ID and _CLIENT_SECRET; disabled", name, strings.ToUpper(name))
			return
		}
		if publicURL == "" {
			log.Printf("oauth: %s needs -public-url (for the callback URL); disabled", name)
			return
		}
		oauthProviders[name] = mk(id, secret)
		log.Printf("oauth: %s sign-in enabled (callback %s)", name, callbackURL(name))
	}
	add("github", githubProvider)
	add("google", googleProvider)
}

func enabledProviders() []string {
	names := make([]string, 0, len(oauthProviders))
	for n := range oauthProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func callbackURL(provider string) string {
	return strings.TrimRight(publicURL, "/") + "/auth/" + provider + "/callback"
}

// ---------------------------------------------------------------------------
// Pending sign-ins (state → details), kept server-side for 10 minutes.
// ---------------------------------------------------------------------------

type pendingAuth struct {
	Provider   string
	Verifier   string // PKCE code_verifier
	Next       string
	LinkUserID int64  // non-zero: link to this signed-in user instead of signing in
	Invite     string // invite link token: may create an account even with registration closed
	Expires    time.Time
}

var (
	pendingMu sync.Mutex
	pending   = map[string]pendingAuth{}
)

const oauthStateCookie = "cf_oauth_state"

func putPending(state string, p pendingAuth) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	now := time.Now()
	for k, v := range pending { // opportunistic cleanup
		if now.After(v.Expires) {
			delete(pending, k)
		}
	}
	pending[state] = p
}

// takePending returns and removes a pending sign-in (single use).
func takePending(state string) (pendingAuth, bool) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	p, ok := pending[state]
	delete(pending, state)
	if !ok || time.Now().After(p.Expires) {
		return pendingAuth{}, false
	}
	return p, true
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// oauthStart: GET /auth/{provider}/start[?next=/path][&link=1]
func oauthStart(w http.ResponseWriter, r *http.Request) {
	p := oauthProviders[r.PathValue("provider")]
	if p == nil {
		http.NotFound(w, r)
		return
	}
	pa := pendingAuth{
		Provider: p.Name,
		Verifier: randomToken(48),
		Next:     safeNext(r.URL.Query().Get("next")),
		Expires:  time.Now().Add(10 * time.Minute),
	}
	if inv := r.URL.Query().Get("invite"); inv != "" {
		if l, err := lookupLink(inv); err != nil || l.Kind != "invite" {
			http.Redirect(w, r, "/login?error="+url.QueryEscape(errLinkInvalid.Error()), http.StatusSeeOther)
			return
		}
		pa.Invite = inv // checked again, and used up, only if an account is created
	}
	if r.URL.Query().Get("link") == "1" {
		u := currentUser(r)
		if u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		pa.LinkUserID = u.ID
		pa.Next = "/account"
	}
	state := randomToken(32)
	putPending(state, pa)
	http.SetCookie(w, &http.Cookie{
		Name: oauthStateCookie, Value: state, Path: "/auth/",
		MaxAge: 600, HttpOnly: true, Secure: secureCookies(r), SameSite: http.SameSiteLaxMode,
	})

	q := url.Values{
		"client_id":             {p.ClientID},
		"redirect_uri":          {callbackURL(p.Name)},
		"response_type":         {"code"},
		"scope":                 {strings.Join(p.Scopes, " ")},
		"state":                 {state},
		"code_challenge":        {pkceChallenge(pa.Verifier)},
		"code_challenge_method": {"S256"},
	}
	for k, v := range p.ExtraAuth {
		q[k] = v
	}
	http.Redirect(w, r, p.AuthURL+"?"+q.Encode(), http.StatusSeeOther)
}

// oauthCallback: GET /auth/{provider}/callback?code=…&state=…
func oauthCallback(w http.ResponseWriter, r *http.Request) {
	p := oauthProviders[r.PathValue("provider")]
	if p == nil {
		http.NotFound(w, r)
		return
	}
	fail := func(msg string) {
		http.Redirect(w, r, "/login?error="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	q := r.URL.Query()
	state := q.Get("state")
	cookie, err := r.Cookie(oauthStateCookie)
	// Clear the state cookie whatever happens.
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Value: "", Path: "/auth/", MaxAge: -1,
		HttpOnly: true, Secure: secureCookies(r), SameSite: http.SameSiteLaxMode})
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		fail("Sign-in expired or was started in another browser. Please try again.")
		return
	}
	pa, ok := takePending(state)
	if !ok || pa.Provider != p.Name {
		fail("Sign-in expired. Please try again.")
		return
	}
	if e := q.Get("error"); e != "" {
		fail("Sign-in was cancelled.")
		return
	}
	code := q.Get("code")
	if code == "" {
		fail("Sign-in failed (no code).")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	token, err := exchangeCode(ctx, p, code, pa.Verifier)
	if err != nil {
		log.Printf("oauth %s: token exchange: %v", logsafe.Text(p.Name), logsafe.Text(err))
		fail("Sign-in failed. Please try again.")
		return
	}
	ou, err := p.fetchUser(ctx, p, token)
	if err != nil || ou.Subject == "" {
		log.Printf("oauth %s: fetch user: %v", p.Name, err)
		fail("Couldn't read your account from " + p.Name + ".")
		return
	}

	userID, msg, err := resolveOAuthUser(p.Name, ou, pa.LinkUserID, pa.Invite)
	if err != nil {
		if pa.LinkUserID != 0 {
			http.Redirect(w, r, "/account?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
			return
		}
		fail(err.Error())
		return
	}
	if pa.LinkUserID != 0 {
		http.Redirect(w, r, "/account?notice="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	if err := startSession(w, r, userID); err != nil {
		fail("Could not sign in.")
		return
	}
	if msg == msgNewAccount {
		markNewAccount(w, r)
	}
	http.Redirect(w, r, pa.Next, http.StatusSeeOther)
}

// maxLinkedPerProvider: how many GitHub (or Google) accounts one user may
// link: room for a personal, a work and a school one. Each is a full way in.
const maxLinkedPerProvider = 3

// msgNewAccount: resolveOAuthUser's message when it made a new account.
const msgNewAccount = "new account"

var (
	errNotLinked     = errors.New("no account is linked to that sign-in. Sign in another way (your password, or a sign-in link from an admin) and link it from your account page")
	errLinkedElsewhr = errors.New("that sign-in is already linked to a different account")
	errDisabled      = errors.New("this account is disabled")
)

// resolveOAuthUser maps a provider identity to a user: link (linkUserID != 0),
// existing link, or (with -allow-registrations) a new account.
func resolveOAuthUser(provider string, ou *oauthUser, linkUserID int64, invite string) (int64, string, error) {
	var owner int64
	err := appDB.QueryRow(`SELECT user_id FROM identities WHERE provider = ? AND subject = ?`, provider, ou.Subject).Scan(&owner)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, "", err
	}

	if linkUserID != 0 {
		if found && owner != linkUserID {
			return 0, "", errLinkedElsewhr
		}
		if !found {
			var n int
			_ = appDB.QueryRow(`SELECT COUNT(*) FROM identities WHERE user_id = ? AND provider = ?`, linkUserID, provider).Scan(&n)
			if n >= maxLinkedPerProvider {
				return 0, "", fmt.Errorf("you can link up to %d %s accounts. Unlink one first", maxLinkedPerProvider, provider)
			}
			if _, err := appDB.Exec(`INSERT INTO identities (provider, subject, user_id, email, login, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
				provider, ou.Subject, linkUserID, ou.Email, ou.Login, nowUnix()); err != nil {
				return 0, "", err
			}
		}
		return linkUserID, "Linked " + provider + ".", nil
	}

	if found {
		u, err := getUserByID(owner)
		if err != nil {
			return 0, "", err
		}
		if u.Locked() {
			return 0, "", errDisabled
		}
		// keep display details fresh
		_, _ = appDB.Exec(`UPDATE identities SET email = ?, login = ? WHERE provider = ? AND subject = ?`, ou.Email, ou.Login, provider, ou.Subject)
		return owner, "", nil
	}

	makeAdmin := false
	switch {
	case invite != "":
		// An admin's invite allows this account even with registration closed.
		admin, err := claimInviteForOAuth(invite)
		if err != nil {
			return 0, "", err
		}
		makeAdmin = admin
	case !allowRegistrations:
		return 0, "", errNotLinked
	}
	u, err := createUserWithFreeName(usernameFrom(ou))
	if err != nil {
		if invite != "" {
			unclaimLink(invite)
		}
		return 0, "", err
	}
	if makeAdmin {
		_, _ = appDB.Exec(`UPDATE users SET is_admin = 1 WHERE id = ?`, u.ID)
	}
	if invite != "" {
		markLinkUsedBy(invite, u.ID)
		log.Printf("invite link used: created %s via %s (admin=%v)", u.Username, provider, makeAdmin)
	}
	if _, err := appDB.Exec(`INSERT INTO identities (provider, subject, user_id, email, login, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		provider, ou.Subject, u.ID, ou.Email, ou.Login, nowUnix()); err != nil {
		return 0, "", err
	}
	log.Printf("oauth: created user %s from %s", u.Username, provider)
	return u.ID, msgNewAccount, nil
}

var nonNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func usernameFrom(ou *oauthUser) string {
	base := ou.Login
	if base == "" {
		base, _, _ = strings.Cut(ou.Email, "@")
	}
	if base == "" {
		base = ou.Name
	}
	base = strings.Trim(nonNameChars.ReplaceAllString(base, "-"), "-.")
	if len(base) > 28 {
		base = base[:28]
	}
	for len(base) < 3 {
		base += "x"
	}
	return base
}

// createUserWithFreeName creates a password-less user, adding a numeric
// suffix if the name is taken.
func createUserWithFreeName(base string) (*User, error) {
	name := base
	for i := 2; i < 1000; i++ {
		u, err := createUser(name, "", false)
		if !errors.Is(err, errUsernameTaken) {
			return u, err
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return nil, errUsernameTaken
}

// oauthUnlink: POST /auth/{provider}/unlink — never leaves an account with
// no way to sign in.
func oauthUnlink(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	provider := r.PathValue("provider")
	var others int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM identities WHERE user_id = ? AND provider <> ?`, u.ID, provider).Scan(&others)
	// The last way to sign in: allowed, but only when they type their
	// username, as the account page's warning says (like deleting it).
	lastWay := !u.PasswordSignIn() && others == 0
	if lastWay && !strings.EqualFold(strings.TrimSpace(r.FormValue("confirm_username")), u.Username) {
		http.Redirect(w, r, "/account?error="+url.QueryEscape("To unlink your only way to sign in, type your username to confirm.")+"#linked-sign-ins", http.StatusSeeOther)
		return
	}
	res, err := appDB.Exec(`DELETE FROM identities WHERE user_id = ? AND provider = ?`, u.ID, provider)
	if err != nil {
		log.Println("oauth unlink:", err)
		http.Redirect(w, r, "/account?error="+url.QueryEscape("Couldn't unlink. Try again."), http.StatusSeeOther)
		return
	}
	msg := "Unlinked " + provider + "."
	if n, _ := res.RowsAffected(); n > 0 && lastWay {
		log.Printf("user %s unlinked their last sign-in (%s): only an admin's sign-in link gets them back in", u.Username, provider)
		msg += " You're signed in until you sign out or this session ends. After that you can only get back in with a sign-in link from an admin."
	}
	http.Redirect(w, r, "/account?notice="+url.QueryEscape(msg), http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Provider calls
// ---------------------------------------------------------------------------

func exchangeCode(ctx context.Context, p *oauthProvider, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {callbackURL(p.Name)},
		"client_id":     {p.ClientID},
		"client_secret": {p.ClientSecret},
		"code_verifier": {verifier},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := doJSON(req, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("no access token (%s: %s)", out.Error, out.Description)
	}
	return out.AccessToken, nil
}

func doJSON(req *http.Request, v any) error {
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: HTTP %d", req.URL.Host, resp.StatusCode)
	}
	return json.Unmarshal(body, v)
}

func bearerGet(ctx context.Context, rawURL, token string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	return doJSON(req, v)
}

func fetchGitHubUser(ctx context.Context, p *oauthProvider, token string) (*oauthUser, error) {
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := bearerGet(ctx, p.UserURL, token, &u); err != nil {
		return nil, err
	}
	if u.ID == 0 {
		return nil, errors.New("github: missing user id")
	}
	email := u.Email
	if email == "" && p.EmailsURL != "" { // private email: ask for the primary verified one
		var emails []struct {
			Email    string `json:"email"`
			Primary  bool   `json:"primary"`
			Verified bool   `json:"verified"`
		}
		if bearerGet(ctx, p.EmailsURL, token, &emails) == nil {
			for _, e := range emails {
				if e.Primary && e.Verified {
					email = e.Email
				}
			}
		}
	}
	return &oauthUser{Subject: fmt.Sprint(u.ID), Login: u.Login, Email: email, Name: u.Name}, nil
}

func fetchGoogleUser(ctx context.Context, p *oauthProvider, token string) (*oauthUser, error) {
	var u struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := bearerGet(ctx, p.UserURL, token, &u); err != nil {
		return nil, err
	}
	if u.Sub == "" {
		return nil, errors.New("google: missing sub")
	}
	email := u.Email
	if !u.EmailVerified {
		email = ""
	}
	return &oauthUser{Subject: u.Sub, Email: email, Name: u.Name}, nil
}

func identitiesFor(userID int64) []Identity {
	rows, err := appDB.Query(`SELECT provider, subject, email, login FROM identities WHERE user_id = ? ORDER BY provider`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var i Identity
		if rows.Scan(&i.Provider, &i.Subject, &i.Email, &i.Login) == nil {
			out = append(out, i)
		}
	}
	return out
}

// checkPasswordsDisabled runs at startup, after configureOAuth, when
// -disable-passwords is on: without a provider nobody could sign in again,
// and without a linked admin the first sign-in link is the last way in.
func checkPasswordsDisabled() {
	if !passwordsDisabled {
		return
	}
	if len(oauthProviders) == 0 {
		log.Fatal("-disable-passwords needs GitHub or Google sign-in set up (CF_GITHUB_CLIENT_ID/_SECRET or CF_GOOGLE_CLIENT_ID/_SECRET, and -public-url; see docs/OAUTH.md): nobody could sign in otherwise")
	}
	var linked int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM users u WHERE u.is_admin = 1 AND u.disabled = 0 AND u.deleted_at IS NULL
		AND EXISTS (SELECT 1 FROM identities i WHERE i.user_id = u.id)`).Scan(&linked)
	if linked == 0 {
		log.Println("WARNING: -disable-passwords: no admin has a GitHub/Google sign-in linked, so none can sign in. Start without it, sign in, link one on the account page, then turn it back on")
	}
	log.Println("-disable-passwords: passwords are off; people sign in with GitHub/Google or an admin's sign-in link")
}
