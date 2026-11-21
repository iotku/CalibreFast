package main

// Linking a user's own Hardcover account, on the account page, for reading
// sync (hardcover_sync.go). Hardcover's OAuth device grant: we get a code,
// the user enters it at hardcover.app/link and approves, and our polling
// gets their tokens. No password or token passes through the browser, and
// none is ever shown or sent to it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// hcPollInterval is how often a sign-in is polled, as Hardcover asked
// (tests shorten it).
var hcPollInterval = func(d time.Duration) time.Duration { return d }

// hcPendingLink is a sign-in in progress (or just failed: err).
type hcPendingLink struct {
	UserCode string
	OpenURL  string // Hardcover's code page, with the code filled in when it gave one
	Expires  time.Time
	err      string
	cancel   context.CancelFunc
}

// hcPending is a copy of userID's sign-in in progress, or nil.
func hcPending(userID int64) *hcPendingLink {
	hcSync.Lock()
	defer hcSync.Unlock()
	p := hcSync.pending[userID]
	if p == nil {
		return nil
	}
	if p.err == "" && hcNow().After(p.Expires) {
		delete(hcSync.pending, userID)
		return nil
	}
	cp := *p
	return &cp
}

func hcEndPending(userID int64, p *hcPendingLink, errMsg string) {
	hcSync.Lock()
	defer hcSync.Unlock()
	if hcSync.pending[userID] != p {
		return
	}
	if errMsg == "" {
		delete(hcSync.pending, userID)
	} else {
		p.err = errMsg
	}
}

// hcStartLink starts a sign-in for userID, or answers the one in progress.
func hcStartLink(userID int64) (*hcPendingLink, error) {
	if p := hcPending(userID); p != nil && p.err == "" {
		return p, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	d, err := hcOAuth.StartDevice(ctx, hardcover.SyncScopes)
	cancel()
	if err != nil {
		return nil, err
	}
	pctx, pcancel := context.WithDeadline(context.Background(), d.Expires)
	open := d.VerificationURIComplete
	if open == "" {
		open = d.VerificationURI
	}
	p := &hcPendingLink{UserCode: d.UserCode, OpenURL: open, Expires: d.Expires, cancel: pcancel}
	hcSync.Lock()
	if old := hcSync.pending[userID]; old != nil {
		old.cancel()
	}
	hcSync.pending[userID] = p
	hcSync.Unlock()
	d.Interval = hcPollInterval(d.Interval)
	go hcRunLink(pctx, userID, d, p)
	return p, nil
}

// hcRunLink polls until the user approves the sign-in, declines it, or the
// code expires, and keeps the tokens.
func hcRunLink(ctx context.Context, userID int64, d *hardcover.DeviceAuth, p *hcPendingLink) {
	defer p.cancel()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("hardcover link: %v", r)
			hcEndPending(userID, p, "Linking failed. Try again.")
		}
	}()
	interval := d.Interval
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				hcEndPending(userID, p, "The code expired before it was entered on Hardcover. Try again.")
			}
			return
		case <-time.After(interval):
		}
		t, err := hcOAuth.PollDevice(ctx, d.DeviceCode)
		switch {
		case errors.Is(err, hardcover.ErrAuthorizationPending):
			continue
		case errors.Is(err, hardcover.ErrSlowDown):
			interval += 5 * time.Second
			continue
		case errors.Is(err, hardcover.ErrAccessDenied), errors.Is(err, hardcover.ErrExpiredCode):
			hcEndPending(userID, p, err.Error()+".")
			return
		case err != nil:
			if ctx.Err() != nil {
				continue // reported above
			}
			log.Println("hardcover link:", err)
			continue // unreachable for now: try again until the code expires
		}
		me, err := hardcover.New(hardcover.Config{
			URL: hcSyncURL, Token: t.Access, UserAgent: lookupUserAgent, Budget: hardcover.FreePlan(), Now: hcNow,
		}).Me(ctx)
		if err == nil {
			err = hcSaveAccount(userID, me, t)
		}
		if err != nil {
			log.Println("hardcover link:", err)
			rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = hcOAuth.Revoke(rctx, t.Refresh)
			cancel()
			hcEndPending(userID, p, "Hardcover accepted the code, but the link couldn't be completed. Try again.")
			return
		}
		hcSetLinked(userID, true)
		log.Printf("hardcover sync: user %d linked Hardcover account %d", userID, me.ID)
		hcEndPending(userID, p, "")
		return
	}
}

// hcRevoke is what became of a sign-in on Hardcover when a link ended.
type hcRevoke int

const (
	hcRevokeNone   hcRevoke = iota // no usable sign-in was kept: nothing to revoke
	hcRevoked                      // Hardcover ended it
	hcRevokeFailed                 // Hardcover couldn't be asked, or refused
)

// hcUnlinkAccount ends userID's link: the sign-in is revoked on Hardcover
// (when it answers), and the tokens, waiting changes and sharing choices
// are deleted here whatever it answers.
func hcUnlinkAccount(userID int64) (hcRevoke, error) {
	hcSync.Lock()
	if p := hcSync.pending[userID]; p != nil {
		p.cancel()
		delete(hcSync.pending, userID)
	}
	hcSync.Unlock()
	l, err := hcLoadAccount(userID)
	if err != nil {
		return hcRevokeNone, err
	}
	revoke := hcRevokeNone
	if l != nil && l.refresh != "" {
		revoke = hcRevokeFailed
		ref, err := openSecret(hcSealContext("refresh_token", userID), l.refresh)
		switch {
		case err != nil:
			log.Printf("hardcover unlink: user %d: the sign-in can't be read, so it can't be revoked", userID)
		case hcRevoker == nil:
			log.Printf("hardcover unlink: user %d: no Hardcover client id, so the sign-in can't be revoked", userID)
		default:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := hcRevoker.Revoke(ctx, ref); err != nil {
				log.Println("hardcover unlink:", err)
			} else {
				revoke = hcRevoked
			}
			cancel()
		}
	}
	hcSetLinked(userID, false)
	for _, table := range []string{"hardcover_sync", "hardcover_shared", "hardcover_user_books", "hardcover_links"} {
		if _, err := appDB.Exec(`DELETE FROM `+table+` WHERE user_id = ?`, userID); err != nil {
			return revoke, err
		}
	}
	return revoke, nil
}

// hcAccountView is the account page's Hardcover section.
type hcAccountView struct {
	State     string // "unlinked", "pending", "linked" or "broken"; "off" (admins only): not set up on this server; "disabled": off, but this user is still linked
	Username  string
	LinkedAt  string
	Broken    string
	UserCode  string
	VerifyURL string // where to enter the code (shown)
	OpenURL   string // the button's link: VerifyURL, with the code filled in if Hardcover allows
	Expires   string
	Error     string // a sign-in that just failed
	Notice    string
	Summary   hcSyncSummary
	Share     hcShare
	Shared    []hcSharedBook // the books shared, newest first
	Problems  []hcSharedBook // shared books that can't be sent (not found, or failing)
}

// hcSharedBook is a book a user shared, for the account page's list.
type hcSharedBook struct {
	UUID, Title, Authors string
}

// hcSharedBooks lists the library books userID shared.
func hcSharedBooks(userID int64) []hcSharedBook {
	rows, err := appDB.Query(`SELECT book_uuid FROM hardcover_shared WHERE user_id = ? ORDER BY shared_at DESC`, userID)
	if err != nil {
		return nil
	}
	var uuids []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			uuids = append(uuids, u)
		}
	}
	rows.Close()
	var out []hcSharedBook
	for _, u := range uuids {
		if title, authors, ok := hcBookTitle(u); ok {
			out = append(out, hcSharedBook{UUID: u, Title: title, Authors: authors})
		}
	}
	return out
}

// hcStatus is GET /api/me/hardcover: the section's state, for the page to
// follow a sign-in in progress. Never a token.
type hcStatus struct {
	State    string         `json:"state"`
	Username string         `json:"username,omitempty"`
	UserCode string         `json:"user_code,omitempty"`
	Summary  *hcSyncSummary `json:"summary,omitempty"`
}

// hcView is u's Hardcover section; nil when it's off on this server
// (admins get State "off", saying how to turn it on).
func hcView(u *User, notice string) *hcAccountView {
	if !hardcoverSyncEnabled() {
		// Still linked from before: they can see it and unlink.
		if l, err := hcLoadAccount(u.ID); err == nil && l != nil {
			return &hcAccountView{State: "disabled", Notice: notice, Username: l.Username, LinkedAt: formatDay(l.LinkedAt.Unix())}
		}
		if u.IsAdmin && !noHardcover && !noHardcoverSync && hcRevoker == nil {
			return &hcAccountView{State: "off", Notice: notice}
		}
		if notice != "" {
			return &hcAccountView{State: "gone", Notice: notice}
		}
		return nil
	}
	userID := u.ID
	v := &hcAccountView{State: "unlinked", Notice: notice}
	if p := hcPending(userID); p != nil {
		if p.err != "" {
			v.Error = p.err
			hcSync.Lock() // shown once
			if q := hcSync.pending[userID]; q != nil && q.err != "" {
				delete(hcSync.pending, userID)
			}
			hcSync.Unlock()
		} else {
			v.State, v.UserCode, v.VerifyURL, v.OpenURL = "pending", p.UserCode, hardcover.VerificationURL, p.OpenURL
			v.Expires = p.Expires.Format("15:04")
			return v
		}
	}
	l, err := hcLoadAccount(userID)
	if err != nil || l == nil {
		return v
	}
	v.Username, v.LinkedAt = l.Username, formatDay(l.LinkedAt.Unix())
	if l.Broken != "" {
		v.State, v.Broken = "broken", l.Broken
		return v
	}
	v.State, v.Summary = "linked", hcSummary(userID)
	v.Share, v.Shared, v.Problems = hcLoadShare(userID), hcSharedBooks(userID), hcProblemBooks(userID)
	return v
}

func hardcoverStatusHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	v := hcView(u, "")
	if v == nil || v.State == "off" || v.State == "gone" {
		writeJSON(w, http.StatusOK, hcStatus{State: "off"})
		return
	}
	st := hcStatus{State: v.State, Username: v.Username, UserCode: v.UserCode}
	if v.State == "linked" {
		st.Summary = &v.Summary
	}
	writeJSON(w, http.StatusOK, st)
}

// hardcoverAccountHandler: POST /account/hardcover
// (action=link|cancel|unlink|settings|backfill).
func hardcoverAccountHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	action := r.FormValue("action")
	// Taking back access always works; starting or changing sharing only
	// while sync is on.
	if !hardcoverSyncEnabled() && action != "unlink" && action != "cancel" {
		http.Error(w, "Hardcover sync is off on this server.", http.StatusForbidden)
		return
	}
	back := func(notice string) {
		q := ""
		if notice != "" {
			q = "?hardcover=" + url.QueryEscape(notice)
		}
		http.Redirect(w, r, "/account"+q+"#hardcover", http.StatusSeeOther)
	}
	switch action {
	case "link":
		if _, err := hcStartLink(u.ID); err != nil {
			log.Println("hardcover link:", err)
			back("Couldn't start linking Hardcover: " + err.Error())
			return
		}
		back("")
	case "cancel":
		hcSync.Lock()
		if p := hcSync.pending[u.ID]; p != nil {
			p.cancel()
			delete(hcSync.pending, u.ID)
		}
		hcSync.Unlock()
		back("")
	case "unlink":
		revoke, err := hcUnlinkAccount(u.ID)
		if err != nil {
			log.Println("hardcover unlink:", err)
			http.Error(w, "could not unlink Hardcover", http.StatusInternalServerError)
			return
		}
		log.Printf("user %s unlinked Hardcover", u.Username)
		switch revoke {
		case hcRevokeFailed:
			back("Unlinked, and your Hardcover sign-in is deleted here. Hardcover couldn't be told to end it, though: to be sure, remove CalibreFast under Authorized Apps in Hardcover's settings.")
		default:
			back("Hardcover unlinked: CalibreFast is signed out of your Hardcover account and nothing more will be sent there.")
		}
	case "settings":
		if !hcIsLinked(u.ID) {
			back("Link Hardcover first.")
			return
		}
		s := hcShare{All: r.FormValue("books") == "all", Progress: r.FormValue("progress") == "on", Dates: r.FormValue("dates") == "on"}
		if err := hcSaveShare(u.ID, s); err != nil {
			log.Println("hardcover settings:", err)
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
		back("Saved. It applies to what's sent from now on.")
	case "backfill":
		if !hcIsLinked(u.ID) {
			back("Link Hardcover first.")
			return
		}
		n, err := hcSyncBackfill(u.ID)
		if err != nil {
			log.Println("hardcover backfill:", err)
			http.Error(w, "could not queue the books", http.StatusInternalServerError)
			return
		}
		back(fmt.Sprintf("%d book(s)", n) + " queued to send to Hardcover.")
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

// hardcoverShareHandler: PUT /api/me/books/{uuid}/hardcover {"share": bool}
// shares a book with the user's Hardcover account, or stops sharing it.
func hardcoverShareHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	if !hardcoverSyncEnabled() || !hcIsLinked(u.ID) {
		http.Error(w, "Link Hardcover on your account page first.", http.StatusConflict)
		return
	}
	var req struct {
		Share *bool `json:"share"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || req.Share == nil {
		http.Error(w, `need {"share": true|false}`, http.StatusBadRequest)
		return
	}
	if err := hcSetBookShared(u.ID, uuid, *req.Share); err != nil {
		log.Println("hardcover share:", err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"share": hcIsBookShared(u.ID, uuid), "all": hcLoadShare(u.ID).All})
}

func registerHardcoverSyncRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /api/me/books/{uuid}/hardcover", requireUser(hardcoverShareHandler))
	mux.HandleFunc("GET /api/me/books/{uuid}/hardcover", requireUser(hardcoverBookStatusHandler))
	mux.HandleFunc("GET /api/me/books/{uuid}/hardcover/search", requireUser(hardcoverBookSearchHandler))
	mux.HandleFunc("PUT /api/me/books/{uuid}/hardcover/match", requireUser(hardcoverBookMatchHandler))
	mux.HandleFunc("DELETE /api/me/books/{uuid}/hardcover/match", requireUser(hardcoverBookMatchHandler))
	mux.HandleFunc("POST /account/hardcover", requireUser(hardcoverAccountHandler))
	mux.HandleFunc("GET /api/me/hardcover", requireUser(hardcoverStatusHandler))
}
