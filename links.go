package main

// Admin-generated one-time links, for handing someone access directly (e.g.
// in a Discord DM):
//
//   - sign-in link: signs in as an existing user (password reset, or someone
//     who only uses GitHub/Google);
//   - invite link: lets someone create an account (password or GitHub/Google)
//     even with registration closed, optionally as an admin.
//
// Security:
//   - 256-bit random tokens; only a SHA-256 hash is stored; shown once.
//   - Single use, enforced by one conditional UPDATE (no double use in a race).
//   - Expire after 1 hour, 1 day or 7 days; admins can list and revoke them.
//   - Opening a link (GET) never uses it up: chat apps fetch links to build
//     previews, which would otherwise burn them. The person confirms with a
//     button (POST), which is also covered by the cross-site protection.
//   - Pages are no-store, noindex, and sent with Referrer-Policy: no-referrer
//     so the token doesn't leak through the Referer header.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var linkLifetimes = map[string]time.Duration{"1h": time.Hour, "1d": 24 * time.Hour, "7d": 7 * 24 * time.Hour}

type loginLink struct {
	ID          string `json:"id"` // short prefix of the token hash (for revoking)
	Kind        string `json:"kind"`
	UserID      int64  `json:"user_id,omitempty"`
	Username    string `json:"username,omitempty"`
	InviteAdmin bool   `json:"invite_admin"`
	Note        string `json:"note"`
	CreatedBy   string `json:"created_by"`
	ExpiresAt   int64  `json:"expires_at"`
}

var (
	errLinkInvalid = errors.New("this link is invalid, has expired or has already been used")
)

// lookupLink returns an unused, unexpired link by token (without using it).
func lookupLink(token string) (*loginLink, error) {
	if token == "" {
		return nil, errLinkInvalid
	}
	var l loginLink
	var uid sql.NullInt64
	var uname sql.NullString
	err := appDB.QueryRow(`
		SELECT l.kind, l.user_id, u.username, l.invite_admin, l.note, l.expires_at
		FROM login_links l LEFT JOIN users u ON u.id = l.user_id
		WHERE l.token_hash = ? AND l.used_at IS NULL AND l.expires_at > ?`,
		hashToken(token), nowUnix()).Scan(&l.Kind, &uid, &uname, &l.InviteAdmin, &l.Note, &l.ExpiresAt)
	if err != nil {
		return nil, errLinkInvalid
	}
	l.UserID, l.Username = uid.Int64, uname.String
	if l.Kind == "login" && l.UserID == 0 {
		return nil, errLinkInvalid // user was deleted
	}
	return &l, nil
}

// claimLink marks a link used, atomically. False if it was already used,
// expired or revoked in the meantime.
func claimLink(token string) bool {
	res, err := appDB.Exec(`UPDATE login_links SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		nowUnix(), hashToken(token), nowUnix())
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// unclaimLink undoes a claim when account creation failed afterwards.
func unclaimLink(token string) {
	_, _ = appDB.Exec(`UPDATE login_links SET used_at = NULL, used_by = NULL WHERE token_hash = ?`, hashToken(token))
}

func markLinkUsedBy(token string, userID int64) {
	_, _ = appDB.Exec(`UPDATE login_links SET used_by = ? WHERE token_hash = ?`, userID, hashToken(token))
}

// baseURL for generated links: -public-url, or the address the visitor used.
// Behind an HTTPS reverse proxy CalibreFast itself serves plain HTTP; the
// proxy's X-Forwarded-Proto (or Forwarded: proto=) says the visitor used
// HTTPS. The header only changes the scheme of links shown to whoever sent
// it, so trusting it is harmless.
func baseURL(r *http.Request) string {
	if publicURL != "" {
		return strings.TrimRight(publicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || forwardedHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// forwardedHTTPS: a reverse proxy says the visitor's request was HTTPS.
func forwardedHTTPS(r *http.Request) bool {
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	if strings.EqualFold(strings.TrimSpace(proto), "https") {
		return true
	}
	first, _, _ := strings.Cut(r.Header.Get("Forwarded"), ",")
	for _, kv := range strings.Split(first, ";") {
		k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
		if strings.EqualFold(k, "proto") && strings.EqualFold(strings.Trim(v, `"`), "https") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Admin API
// ---------------------------------------------------------------------------

func adminCreateLink(w http.ResponseWriter, r *http.Request) {
	admin := currentUser(r)
	var req struct {
		Kind    string `json:"kind"`     // "login" | "invite"
		UserID  int64  `json:"user_id"`  // login links
		Admin   bool   `json:"is_admin"` // invites
		Expires string `json:"expires"`  // "1h" | "1d" | "7d"
		Note    string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	life, ok := linkLifetimes[req.Expires]
	if !ok {
		life = linkLifetimes["1d"]
	}
	req.Note = strings.TrimSpace(req.Note)
	if len(req.Note) > 200 {
		req.Note = req.Note[:200]
	}
	var userID any
	var target *User
	switch req.Kind {
	case "login":
		u, err := getUserByID(req.UserID)
		if err != nil {
			http.Error(w, "no such user", http.StatusNotFound)
			return
		}
		if u.Disabled {
			http.Error(w, "that user is disabled", http.StatusConflict)
			return
		}
		userID, target = u.ID, u
		req.Admin = false
	case "invite":
	default:
		http.Error(w, `kind must be "login" or "invite"`, http.StatusBadRequest)
		return
	}

	token := randomToken(32)
	now := time.Now()
	if _, err := appDB.Exec(`INSERT INTO login_links (token_hash, kind, user_id, invite_admin, note, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		hashToken(token), req.Kind, userID, req.Admin, req.Note, admin.ID, now.Unix(), now.Add(life).Unix()); err != nil {
		http.Error(w, "could not create link", http.StatusInternalServerError)
		return
	}
	if target != nil {
		log.Printf("admin %s created a sign-in link for %s (expires in %s)", admin.Username, target.Username, life)
	} else {
		log.Printf("admin %s created an invite link (admin=%v, expires in %s)", admin.Username, req.Admin, life)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"url":        baseURL(r) + "/l/" + token,
		"expires_at": now.Add(life).Unix(),
	})
}

func adminListLinks(w http.ResponseWriter, r *http.Request) {
	rows, err := appDB.Query(`
		SELECT substr(l.token_hash, 1, 12), l.kind, COALESCE(l.user_id, 0), COALESCE(u.username, ''),
		       l.invite_admin, l.note, COALESCE(c.username, ''), l.expires_at
		FROM login_links l
		LEFT JOIN users u ON u.id = l.user_id
		LEFT JOIN users c ON c.id = l.created_by
		WHERE l.used_at IS NULL AND l.expires_at > ?
		ORDER BY l.created_at DESC`, nowUnix())
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []loginLink{}
	for rows.Next() {
		var l loginLink
		if rows.Scan(&l.ID, &l.Kind, &l.UserID, &l.Username, &l.InviteAdmin, &l.Note, &l.CreatedBy, &l.ExpiresAt) == nil {
			out = append(out, l)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out})
}

func adminRevokeLink(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) != 12 || strings.Trim(id, "0123456789abcdef") != "" {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	res, err := appDB.Exec(`DELETE FROM login_links WHERE substr(token_hash, 1, 12) = ? AND used_at IS NULL`, id)
	if err != nil {
		http.Error(w, "could not revoke", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "no such link", http.StatusNotFound)
		return
	}
	log.Printf("admin %s revoked link %s", currentUser(r).Username, id)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// The link itself: /l/{token}
// ---------------------------------------------------------------------------

type LinkPageData struct {
	Title     string
	Token     string
	Link      *loginLink
	Error     string
	Username  string
	Providers []string
	SignedIn  *User
	Expires   string // human-readable expiry
}

func renderLinkPage(w http.ResponseWriter, data LinkPageData, status int) {
	data.Title = SiteTitle
	data.Providers = enabledProviders()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.WriteHeader(status)
	logErr(templates.ExecuteTemplate(w, "link.html", data), "render link page")
}

// linkHandler: GET shows what the link does (never uses it up); POST uses it.
func linkHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	data := LinkPageData{Token: token, SignedIn: currentUser(r)}
	if r.Method == http.MethodPost && !loginLimiter.allow("link-ip:"+clientIP(r)) {
		data.Error = "Too many attempts. Try again in a few minutes."
		renderLinkPage(w, data, http.StatusTooManyRequests)
		return
	}
	link, err := lookupLink(token)
	if err != nil {
		data.Error = "This link is invalid, has expired, or has already been used. Ask an admin for a new one."
		renderLinkPage(w, data, http.StatusNotFound)
		return
	}
	data.Link = link
	data.Expires = time.Unix(link.ExpiresAt, 0).Format("Mon 2 Jan 15:04")
	if r.Method == http.MethodGet {
		renderLinkPage(w, data, http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch link.Kind {
	case "login":
		if !claimLink(token) {
			data.Error = errLinkInvalid.Error()
			renderLinkPage(w, data, http.StatusGone)
			return
		}
		u, err := getUserByID(link.UserID)
		if err != nil || u.Disabled {
			data.Error = "This account is disabled."
			renderLinkPage(w, data, http.StatusForbidden)
			return
		}
		markLinkUsedBy(token, u.ID)
		if err := startSession(w, r, u.ID); err != nil {
			http.Error(w, "could not sign in", http.StatusInternalServerError)
			return
		}
		log.Printf("sign-in link used for %s", u.Username)
		next := "/"
		if !u.HasPass {
			next = "/account?notice=" + url.QueryEscape("Signed in. You can set a password here, or link GitHub/Google, so you can sign in again later.")
		}
		http.Redirect(w, r, next, http.StatusSeeOther)

	case "invite":
		data.Username = r.FormValue("username")
		if r.FormValue("password") != r.FormValue("password2") {
			data.Error = "Passwords don't match."
			renderLinkPage(w, data, http.StatusBadRequest)
			return
		}
		// Validate before using the invite up, so a typo doesn't waste it.
		if !usernameRe.MatchString(strings.TrimSpace(data.Username)) {
			data.Error = errBadUsername.Error()
			renderLinkPage(w, data, http.StatusBadRequest)
			return
		}
		if _, err := hashPassword(r.FormValue("password")); err != nil {
			data.Error = err.Error()
			renderLinkPage(w, data, http.StatusBadRequest)
			return
		}
		if !claimLink(token) {
			data.Error = errLinkInvalid.Error()
			renderLinkPage(w, data, http.StatusGone)
			return
		}
		u, err := createUser(data.Username, r.FormValue("password"), link.InviteAdmin)
		if err != nil {
			unclaimLink(token) // e.g. username taken: let them try another
			data.Error = err.Error()
			renderLinkPage(w, data, http.StatusBadRequest)
			return
		}
		markLinkUsedBy(token, u.ID)
		if err := startSession(w, r, u.ID); err != nil {
			http.Error(w, "could not sign in", http.StatusInternalServerError)
			return
		}
		log.Printf("invite link used: created %s (admin=%v)", u.Username, u.IsAdmin)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// claimInviteForOAuth uses an invite during GitHub/Google sign-in. Returns
// whether the new account should be an admin, or an error.
func claimInviteForOAuth(token string) (bool, error) {
	link, err := lookupLink(token)
	if err != nil || link.Kind != "invite" {
		return false, errLinkInvalid
	}
	if !claimLink(token) {
		return false, errLinkInvalid
	}
	return link.InviteAdmin, nil
}

func registerLinkRoutes() {
	http.HandleFunc("/l/{token}", linkHandler)
	http.HandleFunc("GET /api/admin/links", requireAdminUser(adminListLinks))
	http.HandleFunc("POST /api/admin/links", requireAdminUser(adminCreateLink))
	http.HandleFunc("DELETE /api/admin/links/{id}", requireAdminUser(adminRevokeLink))
}
