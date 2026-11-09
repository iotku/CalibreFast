package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ---------------------------------------------------------------------------
// Configuration (set from flags in main)
// ---------------------------------------------------------------------------

var (
	dataDir            string // -datadir: where calibrefast.db lives
	publicCatalog      bool   // -public: anonymous visitors may browse the catalog
	publicRead         bool   // -public-read: anonymous visitors may also read/download (implies -public)
	allowRegistrations bool   // -allow-registrations: anyone may create an account from the web
	publicURL          string // -public-url: external base URL (OAuth redirects, Secure cookies)
)

const (
	sessionCookie   = "cf_session"
	sessionLifetime = 30 * 24 * time.Hour
	minPasswordLen  = 10
	bcryptCost      = 12
)

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
	Disabled bool   `json:"disabled"`
	HasPass  bool   `json:"has_password"`
}

var (
	errBadCredentials = errors.New("invalid username or password")
	errUsernameTaken  = errors.New("that username is taken")
	errWeakPassword   = fmt.Errorf("password must be at least %d characters", minPasswordLen)
	errBadUsername    = errors.New("username must be 3–32 letters, digits, '.', '_' or '-'")
	usernameRe        = regexp.MustCompile(`^[A-Za-z0-9._-]{3,32}$`)
)

// dummyHash is compared against when a username doesn't exist, so a failed
// login takes the same time either way (no username enumeration by timing).
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("calibrefast-timing-dummy"), bcryptCost)

func hashPassword(pw string) (string, error) {
	if len(pw) < minPasswordLen {
		return "", errWeakPassword
	}
	if len(pw) > 72 { // bcrypt only uses the first 72 bytes; refuse rather than silently truncate
		return "", errors.New("password must be at most 72 bytes")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(h), err
}

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var hash sql.NullString
	if err := row.Scan(&u.ID, &u.Username, &hash, &u.IsAdmin, &u.Disabled); err != nil {
		return nil, err
	}
	u.HasPass = hash.Valid && hash.String != ""
	return &u, nil
}

const userCols = `id, username, password_hash, is_admin, disabled`

func getUserByID(id int64) (*User, error) {
	return scanUser(appDB.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func listUsers() ([]User, error) {
	rows, err := appDB.Query(`SELECT ` + userCols + ` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// createUser makes a password account. password may be "" for OAuth-only.
func createUser(username, password string, admin bool) (*User, error) {
	username = strings.TrimSpace(username)
	if !usernameRe.MatchString(username) {
		return nil, errBadUsername
	}
	var hash any
	if password != "" {
		h, err := hashPassword(password)
		if err != nil {
			return nil, err
		}
		hash = h
	}
	res, err := appDB.Exec(`INSERT INTO users (username, password_hash, is_admin, created_at) VALUES (?, ?, ?, ?)`,
		username, hash, admin, nowUnix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, errUsernameTaken
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return getUserByID(id)
}

func setPassword(userID int64, password string) error {
	h, err := hashPassword(password)
	if err != nil {
		return err
	}
	_, err = appDB.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, h, userID)
	return err
}

// authenticate checks a username/password. Same error (and similar timing)
// for unknown users, wrong passwords, OAuth-only and disabled accounts.
func authenticate(username, password string) (*User, error) {
	var u User
	var hash sql.NullString
	err := appDB.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ?`, strings.TrimSpace(username)).
		Scan(&u.ID, &u.Username, &hash, &u.IsAdmin, &u.Disabled)
	if err != nil || !hash.Valid || hash.String == "" {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, errBadCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(hash.String), []byte(password)) != nil || u.Disabled {
		return nil, errBadCredentials
	}
	u.HasPass = true
	return &u, nil
}

// adminCount is used to stop the last admin demoting/deleting themselves.
func adminCount() int {
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = 1 AND disabled = 0`).Scan(&n)
	return n
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// secureCookies: Secure flag when served over HTTPS (directly or per -public-url).
func secureCookies(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(strings.ToLower(publicURL), "https://")
}

func startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	token := randomToken(32)
	now := time.Now()
	if _, err := appDB.Exec(`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hashToken(token), userID, now.Unix(), now.Add(sessionLifetime).Unix()); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  now.Add(sessionLifetime),
		HttpOnly: true,
		Secure:   secureCookies(r),
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_, _ = appDB.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secureCookies(r), SameSite: http.SameSiteLaxMode})
}

func userFromSession(r *http.Request) *User {
	if appDB == nil {
		return nil
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	u, err := scanUser(appDB.QueryRow(`
		SELECT u.id, u.username, u.password_hash, u.is_admin, u.disabled
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, hashToken(c.Value), nowUnix()))
	if err != nil || u.Disabled {
		return nil
	}
	return u
}

func pruneSessions() {
	_, _ = appDB.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, nowUnix())
}

// ---------------------------------------------------------------------------
// Request context
// ---------------------------------------------------------------------------

type ctxKey int

const userKey ctxKey = 0

// currentUser is the signed-in user for this request, or nil.
func currentUser(r *http.Request) *User {
	u, _ := r.Context().Value(userKey).(*User)
	return u
}

func isAdmin(r *http.Request) bool {
	u := currentUser(r)
	return u != nil && u.IsAdmin
}

// canEditLibrary: library changes need BOTH an admin session and a server
// started with -allow-writes (kept as a server-wide kill switch).
func canEditLibrary(r *http.Request) bool {
	return allowWrites && isAdmin(r)
}

// isPublicPath: reachable without signing in even when the library is
// private (sign-in and registration themselves, and static assets).
func isPublicPath(p string) bool {
	return p == "/login" || p == "/logout" || p == "/register" || p == "/api/me" ||
		strings.HasPrefix(p, "/auth/") || strings.HasPrefix(p, "/static/")
}

// withAuth loads the session user into the request context and, with
// unless the server runs with -public, keeps anonymous visitors out of
// everything but sign-in: a private library doesn't expose its catalog,
// covers or book UUIDs.
func withAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := userFromSession(r)
		if u != nil {
			r = r.WithContext(context.WithValue(r.Context(), userKey, u))
		}
		if !publicCatalog && u == nil && !isPublicPath(r.URL.Path) {
			if wantsHTML(r) {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			} else {
				http.Error(w, "sign in required", http.StatusUnauthorized)
			}
			return
		}
		h.ServeHTTP(w, r)
	})
}

func wantsHTML(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") &&
		r.Header.Get("HX-Request") == ""
}

// requireUser: any signed-in user (personal data in the app DB).
func requireUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) == nil {
			if wantsHTML(r) {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Error(w, "sign in required", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// requireReader guards book contents (readers, downloads, EPUB/PDF files):
// sign-in required unless the server runs in public read-only mode
// (-public-read). The catalog (covers, metadata, search) stays browsable.
func requireReader(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !publicRead && currentUser(r) == nil {
			if wantsHTML(r) {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Error(w, "sign in to read or download books", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// requireAdminUser: signed-in admin (user management, global shelves).
func requireAdminUser(h http.HandlerFunc) http.HandlerFunc {
	return requireUser(func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r) {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

// ---------------------------------------------------------------------------
// Login rate limiting (per client IP and per username)
// ---------------------------------------------------------------------------

type limiter struct {
	mu      sync.Mutex
	hits    map[string][]time.Time
	max     int
	window  time.Duration
	nowFunc func() time.Time
}

var loginLimiter = &limiter{hits: map[string][]time.Time{}, max: 10, window: 15 * time.Minute, nowFunc: time.Now}

// allow records an attempt for key and reports whether it's within the limit.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.nowFunc()
	cut := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// Handlers: sign in / out / register / account
// ---------------------------------------------------------------------------

type AuthPageData struct {
	Title        string
	Error        string
	Notice       string
	Next         string
	Username     string
	User         *User
	Providers    []string // enabled OAuth providers
	Registration bool
	Identities   []Identity
}

// safeNext only allows local redirects ("/x", not "//evil" or "https://…").
func safeNext(next string) string {
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") && !strings.HasPrefix(next, "/\\") {
		return next
	}
	return "/"
}

func renderAuthPage(w http.ResponseWriter, name string, data AuthPageData, status int) {
	data.Title = SiteTitle
	data.Providers = enabledProviders()
	data.Registration = allowRegistrations
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	logErr(templates.ExecuteTemplate(w, name, data), "render "+name)
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.FormValue("next"))
	if r.Method == http.MethodGet {
		if currentUser(r) != nil {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		renderAuthPage(w, "login.html", AuthPageData{Next: next, Error: r.URL.Query().Get("error")}, http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	username := r.FormValue("username")
	data := AuthPageData{Next: next, Username: username}
	if !loginLimiter.allow("ip:"+clientIP(r)) || !loginLimiter.allow("user:"+strings.ToLower(username)) {
		data.Error = "Too many sign-in attempts. Try again in a few minutes."
		renderAuthPage(w, "login.html", data, http.StatusTooManyRequests)
		return
	}
	u, err := authenticate(username, r.FormValue("password"))
	if err != nil {
		data.Error = "Invalid username or password."
		renderAuthPage(w, "login.html", data, http.StatusUnauthorized)
		return
	}
	if err := startSession(w, r, u.ID); err != nil {
		log.Println("start session:", err)
		http.Error(w, "could not sign in", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func registerHandler(w http.ResponseWriter, r *http.Request) {
	if !allowRegistrations {
		http.Error(w, "registration is closed; ask an admin for an account", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := AuthPageData{Username: r.FormValue("username"), Next: "/"}
	if !loginLimiter.allow("ip:" + clientIP(r)) {
		data.Error = "Too many attempts. Try again in a few minutes."
		renderAuthPage(w, "login.html", data, http.StatusTooManyRequests)
		return
	}
	if r.FormValue("password") != r.FormValue("password2") {
		data.Error = "Passwords don't match."
		renderAuthPage(w, "login.html", data, http.StatusBadRequest)
		return
	}
	u, err := createUser(r.FormValue("username"), r.FormValue("password"), false)
	if err != nil {
		data.Error = err.Error()
		renderAuthPage(w, "login.html", data, http.StatusBadRequest)
		return
	}
	if err := startSession(w, r, u.ID); err != nil {
		http.Error(w, "could not sign in", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	endSession(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func accountHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	data := AuthPageData{User: u, Identities: identitiesFor(u.ID), Notice: r.URL.Query().Get("notice"), Error: r.URL.Query().Get("error")}
	if r.Method == http.MethodPost {
		// change password (current password required if one is set)
		if u.HasPass {
			if _, err := authenticate(u.Username, r.FormValue("current")); err != nil {
				data.Error = "Current password is wrong."
				renderAuthPage(w, "account.html", data, http.StatusBadRequest)
				return
			}
		}
		if r.FormValue("password") != r.FormValue("password2") {
			data.Error = "New passwords don't match."
			renderAuthPage(w, "account.html", data, http.StatusBadRequest)
			return
		}
		if err := setPassword(u.ID, r.FormValue("password")); err != nil {
			data.Error = err.Error()
			renderAuthPage(w, "account.html", data, http.StatusBadRequest)
			return
		}
		// Sign out other devices; keep this one.
		if c, err := r.Cookie(sessionCookie); err == nil {
			_, _ = appDB.Exec(`DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, u.ID, hashToken(c.Value))
		}
		http.Redirect(w, r, "/account?notice="+url.QueryEscape("Password changed. Other devices were signed out."), http.StatusSeeOther)
		return
	}
	renderAuthPage(w, "account.html", data, http.StatusOK)
}

// meHandler: GET /api/me — who am I (used by the nav and the readers).
func meHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	u := currentUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"user": nil, "registration": allowRegistrations})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"user": u, "can_edit_library": canEditLibrary(r)})
}

// ---------------------------------------------------------------------------
// Admin: user management API
// ---------------------------------------------------------------------------

func adminUsersPage(w http.ResponseWriter, r *http.Request) {
	users, err := listUsers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "admin-users.html", map[string]any{
		"Title": SiteTitle, "Users": users, "Me": currentUser(r),
	}), "render admin-users")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func adminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Admin    bool   `json:"is_admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	u, err := createUser(req.Username, req.Password, req.Admin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("admin %s created user %s (admin=%v)", currentUser(r).Username, u.Username, u.IsAdmin)
	writeJSON(w, http.StatusCreated, u)
}

func adminUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Admin    *bool   `json:"is_admin"`
		Disabled *bool   `json:"disabled"`
		Password *string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	target, err := getUserByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	removingAdmin := target.IsAdmin && !target.Disabled &&
		((req.Admin != nil && !*req.Admin) || (req.Disabled != nil && *req.Disabled))
	if removingAdmin && adminCount() <= 1 {
		http.Error(w, "can't remove the last admin", http.StatusConflict)
		return
	}
	if req.Admin != nil {
		_, _ = appDB.Exec(`UPDATE users SET is_admin = ? WHERE id = ?`, *req.Admin, id)
	}
	if req.Disabled != nil {
		_, _ = appDB.Exec(`UPDATE users SET disabled = ? WHERE id = ?`, *req.Disabled, id)
		if *req.Disabled {
			_, _ = appDB.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
		}
	}
	if req.Password != nil {
		if err := setPassword(id, *req.Password); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = appDB.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
	}
	log.Printf("admin %s updated user %s", currentUser(r).Username, target.Username)
	u, _ := getUserByID(id)
	writeJSON(w, http.StatusOK, u)
}

func adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	target, err := getUserByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if target.IsAdmin && !target.Disabled && adminCount() <= 1 {
		http.Error(w, "can't delete the last admin", http.StatusConflict)
		return
	}
	if _, err := appDB.Exec(`DELETE FROM users WHERE id = ?`, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("admin %s deleted user %s", currentUser(r).Username, target.Username)
	w.WriteHeader(http.StatusNoContent)
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	var id int64
	if _, err := fmt.Sscan(r.PathValue(name), &id); err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

// ---------------------------------------------------------------------------
// CLI: -create-user NAME [-admin]   (password from $CF_PASSWORD or stdin)
// ---------------------------------------------------------------------------

func runCreateUser(name string, admin bool) error {
	pw := os.Getenv("CF_PASSWORD")
	if pw == "" {
		fmt.Fprintf(os.Stderr, "Password for %s (min %d chars): ", name, minPasswordLen)
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return err
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	var id int64
	err := appDB.QueryRow(`SELECT id FROM users WHERE username = ?`, name).Scan(&id)
	switch {
	case err == nil: // existing user: reset password (and promote if -admin)
		if err := setPassword(id, pw); err != nil {
			return err
		}
		if admin {
			_, _ = appDB.Exec(`UPDATE users SET is_admin = 1 WHERE id = ?`, id)
		}
		_, _ = appDB.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
		fmt.Fprintf(os.Stderr, "Updated user %s (password reset%s).\n", name, map[bool]string{true: ", admin", false: ""}[admin])
	case errors.Is(err, sql.ErrNoRows):
		if _, err := createUser(name, pw, admin); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Created %s %s.\n", map[bool]string{true: "admin", false: "user"}[admin], name)
	default:
		return err
	}
	return nil
}

// registerAuthRoutes wires sign-in, account and admin endpoints.
func registerAuthRoutes() {
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("POST /register", registerHandler)
	http.HandleFunc("POST /logout", logoutHandler)
	http.HandleFunc("GET /api/me", meHandler)
	http.HandleFunc("/account", requireUser(accountHandler))
	http.HandleFunc("GET /admin/users", requireAdminUser(adminUsersPage))
	http.HandleFunc("POST /api/admin/users", requireAdminUser(adminCreateUser))
	http.HandleFunc("PATCH /api/admin/users/{id}", requireAdminUser(adminUpdateUser))
	http.HandleFunc("DELETE /api/admin/users/{id}", requireAdminUser(adminDeleteUser))
	registerOAuthRoutes()
	registerUserDataRoutes()
	registerProgressRoutes()
	registerShelfRoutes()
}

func withUserCtx(r *http.Request, u *User) context.Context {
	return context.WithValue(r.Context(), userKey, u)
}

func itoa(n int64) string { return fmt.Sprint(n) }
