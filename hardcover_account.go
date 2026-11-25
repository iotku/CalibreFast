package main

// Hardcover reading sync's pages and API (internal/hardcoversync does the
// work): linking a user's own Hardcover account on the account page
// (Hardcover's OAuth device grant: no password or token ever passes
// through the browser), what they share, their followed authors, and a
// shared book's sync on the book page, including choosing which Hardcover
// book it is. Any linked user can choose, editor or not: the choice is
// theirs and used for their own sync only; the library's identifiers are
// left alone (an editor fixes those in the metadata editor), except for a
// placeholder (hardcover_placeholder.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/internal/hardcoversync"
	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// hcStatus is GET /api/me/hardcover: the section's state, for the page to
// follow a sign-in in progress. Never a token.
type hcStatus struct {
	State    string                     `json:"state"`
	Username string                     `json:"username,omitempty"`
	UserCode string                     `json:"user_code,omitempty"`
	Summary  *hardcoversync.SyncSummary `json:"summary,omitempty"`
	// Busy: the worker is sending something of theirs now; LastRun: when it
	// last finished one (since the server started). Linked only.
	Busy    bool   `json:"busy"`
	LastRun string `json:"last_run,omitempty"`
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
		busy, last := hcSync.Activity(u.ID)
		st.Busy = busy
		if !last.IsZero() {
			st.LastRun = last.Format(time.RFC3339)
		}
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
		http.Redirect(w, r, "/me/sync"+q+"#hardcover", http.StatusSeeOther)
	}
	switch action {
	case "link":
		if _, err := hcSync.StartLink(u.ID); err != nil {
			log.Println("hardcover link:", err)
			back("Couldn't start linking Hardcover: " + err.Error())
			return
		}
		back("")
	case "cancel":
		hcSync.CancelLink(u.ID)
		back("")
	case "unlink":
		revoke, err := hcSync.Unlink(u.ID)
		if err != nil {
			log.Println("hardcover unlink:", err)
			http.Error(w, "could not unlink Hardcover", http.StatusInternalServerError)
			return
		}
		log.Printf("user %s unlinked Hardcover", u.Username)
		switch revoke {
		case hardcoversync.RevokeFailed:
			back("Unlinked, and your Hardcover sign-in is deleted here. Hardcover couldn't be told to end it, though: to be sure, remove CalibreFast under Authorized Apps in Hardcover's settings.")
		default:
			back("Hardcover unlinked: CalibreFast is signed out of your Hardcover account and nothing more will be sent there.")
		}
	case "settings":
		if !hcSync.IsLinked(u.ID) {
			back("Link Hardcover first.")
			return
		}
		s := hardcoversync.Share{All: r.FormValue("books") == "all", Progress: r.FormValue("progress") == "on", Dates: r.FormValue("dates") == "on"}
		if err := hcSync.SaveShare(u.ID, s); err != nil {
			log.Println("hardcover settings:", err)
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
		back("Saved. It applies to what's sent from now on.")
	case "follows":
		// Star the authors followed on Hardcover (internal/hardcoversync/follows.go).
		if !hcSync.IsLinked(u.ID) {
			back("Link Hardcover first.")
			return
		}
		res, err := hcSync.ImportFollows(r.Context(), u.ID)
		switch {
		case errors.Is(err, hardcoversync.ErrFollowScope):
			back("To bring in the authors you follow, link Hardcover again: it will ask to let CalibreFast see who you follow.")
		case err != nil:
			log.Println("hardcover follows:", err)
			back("Couldn't get the authors you follow from Hardcover: " + err.Error())
		case res.Total == 0:
			back("You don't follow any authors on Hardcover yet.")
		default:
			msg := fmt.Sprintf("%d followed author(s): %d starred here", res.Total, res.Starred)
			if res.Waiting > 0 {
				msg += fmt.Sprintf(", %d waiting for their books to arrive", res.Waiting)
			}
			if res.Confirm > 0 {
				msg += fmt.Sprintf(", %d for you to confirm below", res.Confirm)
			}
			back(msg + ".")
		}
	case "follows-settings":
		// Following on Hardcover the authors starred here, and the unstar
		// dialog's default (internal/hardcoversync/follow_push.go).
		if !hcSync.IsLinked(u.ID) {
			back("Link Hardcover first.")
			return
		}
		if err := hcSync.SaveFollowPrefs(u.ID, r.FormValue("share_follows") == "on", r.FormValue("unfollow_default") == "on"); err != nil {
			log.Println("hardcover follows settings:", err)
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
		back("Saved.")
	case "follow-retry":
		if err := hcSync.RetryFollow(u.ID, r.FormValue("name")); err != nil {
			log.Println("hardcover follow retry:", err)
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
		back("Trying again.")
	case "follow-choose", "follow-none", "follow-undo":
		hcID, err := strconv.ParseInt(r.FormValue("hc_id"), 10, 64)
		if err != nil || hcID <= 0 {
			http.Error(w, "bad author", http.StatusBadRequest)
			return
		}
		local := ""
		if action == "follow-choose" {
			if local = r.FormValue("name"); local == "" {
				http.Error(w, "bad author", http.StatusBadRequest)
				return
			}
		}
		switch err := hcSync.FollowDecision(u.ID, hcID, local, action == "follow-undo"); {
		case errors.Is(err, hardcoversync.ErrNoFollow):
			back("That author isn't in your followed authors any more: import them again.")
		case err != nil:
			log.Println("hardcover follow choice:", err)
			http.Error(w, "could not save", http.StatusInternalServerError)
		default:
			back("Saved.")
		}
	case "backfill":
		if !hcSync.IsLinked(u.ID) {
			back("Link Hardcover first.")
			return
		}
		n, err := hcSync.Backfill(u.ID)
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
	uuid, ok := requireSyncBook(w, r)
	if !ok {
		return
	}
	if !hardcoverSyncEnabled() || !hcSync.IsLinked(u.ID) {
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
	if err := hcSync.SetBookShared(u.ID, uuid, *req.Share); err != nil {
		log.Println("hardcover share:", err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"share": hcSync.Shared(u.ID, uuid), "all": hcSync.LoadShare(u.ID).All})
}

// hcBookStateFor: hcSync.BookState, with what the request's user may change.
func hcBookStateFor(r *http.Request, userID int64, uuid string) hardcoversync.BookStatus {
	st := hcSync.BookState(userID, uuid)
	st.CanChoose = true
	if p := placeholderByUUID(uuid); p != nil {
		st.Placeholder = true
		st.CanChoose = hcPlaceholderBookID(p) == 0 || canEditLibrary(r)
	}
	return st
}

// hcFoundBook is one search result.
type hcFoundBook struct {
	BookID  int64  `json:"book_id"`
	Slug    string `json:"slug,omitempty"`
	Title   string `json:"title"`
	Authors string `json:"authors,omitempty"`
	Year    string `json:"year,omitempty"`
}

// hcBookFor answers for a book the user shares on a linked account; else
// it writes the error and answers false.
func hcBookFor(w http.ResponseWriter, r *http.Request) (*User, string, bool) {
	u := currentUser(r)
	uuid, ok := requireSyncBook(w, r)
	if !ok {
		return nil, "", false
	}
	if !hardcoverSyncEnabled() || !hcSync.IsLinked(u.ID) {
		http.Error(w, "Link Hardcover on your account page first.", http.StatusConflict)
		return nil, "", false
	}
	return u, uuid, true
}

// hardcoverBookStatusHandler: GET /api/me/books/{uuid}/hardcover.
func hardcoverBookStatusHandler(w http.ResponseWriter, r *http.Request) {
	u, uuid, ok := hcBookFor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, hcBookStateFor(r, u.ID, uuid))
}

// hardcoverBookSearchHandler: GET /api/me/books/{uuid}/hardcover/search?q=
// Only for a book the user shares: a search tells Hardcover what it's for.
func hardcoverBookSearchHandler(w http.ResponseWriter, r *http.Request) {
	u, uuid, ok := hcBookFor(w, r)
	if !ok {
		return
	}
	if !hcSync.Shared(u.ID, uuid) {
		http.Error(w, "Turn on Sync to Hardcover first.", http.StatusConflict)
		return
	}
	q := strings.Join(strings.Fields(r.URL.Query().Get("q")), " ")
	if q == "" || len(q) > 200 {
		http.Error(w, "Type a title, or paste the book's link on hardcover.app.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	c := hcSync.CatalogClient(hcSync.UserClient(u.ID))
	out := []hcFoundBook{}
	if slug, id, isLink := hardcoversync.LinkTarget(q); isLink {
		ed, found, err := c.FindEdition(ctx, nil, id, slug)
		if err != nil {
			hcSearchFailed(w, err)
			return
		}
		if found {
			out = append(out, hcFoundBook{BookID: ed.BookID, Slug: ed.Slug, Title: ed.Title})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	books, err := c.LookupBooks(ctx, hardcover.BookQuery{Title: q})
	if err != nil {
		hcSearchFailed(w, err)
		return
	}
	for _, b := range books {
		if b.ID <= 0 {
			continue
		}
		year, _, _ := strings.Cut(b.Released, "-")
		out = append(out, hcFoundBook{BookID: b.ID, Slug: b.Slug, Title: b.Title, Authors: strings.Join(b.Authors, ", "), Year: year})
	}
	writeJSON(w, http.StatusOK, out)
}

func hcSearchFailed(w http.ResponseWriter, err error) {
	log.Println("hardcover search:", err)
	var wait *hardcover.WaitError
	if errors.As(err, &wait) {
		http.Error(w, "Hardcover is busy; try again in a minute.", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "Couldn't search Hardcover.", http.StatusBadGateway)
}

// hardcoverBookMatchHandler: PUT /api/me/books/{uuid}/hardcover/match
// {"book_id": n} sends the book as that Hardcover book (for this user);
// DELETE goes back to the match from the library's identifiers.
func hardcoverBookMatchHandler(w http.ResponseWriter, r *http.Request) {
	u, uuid, ok := hcBookFor(w, r)
	if !ok {
		return
	}
	if !hcSync.Shared(u.ID, uuid) {
		http.Error(w, "Turn on Sync to Hardcover first.", http.StatusConflict)
		return
	}
	ph := placeholderByUUID(uuid)
	if ph != nil && r.Method == http.MethodDelete {
		http.Error(w, "A placeholder's Hardcover book is the library's: choose another instead.", http.StatusBadRequest)
		return
	}
	editor := canEditLibrary(r)
	if ph != nil && !editor && hcPlaceholderBookID(ph) > 0 {
		http.Error(w, "This book already has its Hardcover book; an editor can change it.", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodDelete {
		if err := hcSync.ClearUserMatch(u.ID, uuid); err != nil {
			log.Println("hardcover match:", err)
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, hcBookStateFor(r, u.ID, uuid))
		return
	}
	var req struct {
		BookID int64 `json:"book_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || req.BookID <= 0 {
		http.Error(w, `need {"book_id": n}`, http.StatusBadRequest)
		return
	}
	// Hardcover says what the book is (its edition, pages and title):
	// nothing of it is taken from the request but the id. The edition is
	// the one with the library book's ISBN, when that's of this book.
	isbns, _, _, err := hcBookKeys(uuid)
	if err != nil {
		log.Println("hardcover match:", err)
		http.Error(w, "could not read the book", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ed, found, err := hcSync.CatalogClient(hcSync.UserClient(u.ID)).BookEdition(ctx, req.BookID, isbns)
	if err != nil {
		hcSearchFailed(w, err)
		return
	}
	if !found {
		http.Error(w, "Hardcover has no such book.", http.StatusNotFound)
		return
	}
	if ph != nil {
		// The placeholder's own identifiers, for everyone: the first to
		// choose names it; after that, only an editor changes it.
		if err := hcSetPlaceholderBook(uuid, ed, u, !editor, u.ID); errors.Is(err, errPlaceholderSyncSet) {
			http.Error(w, "Someone chose its Hardcover book a moment ago; an editor can change it.", http.StatusConflict)
			return
		} else if err != nil {
			log.Println("hardcover match:", err)
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
	} else if err := hcSync.SetUserMatch(u.ID, uuid, ed); err != nil {
		log.Println("hardcover match:", err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	hcSync.Resend(u.ID, uuid)
	writeJSON(w, http.StatusOK, hcBookStateFor(r, u.ID, uuid))
}
