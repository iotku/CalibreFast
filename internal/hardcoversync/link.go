package hardcoversync

// Linking a user's own Hardcover account, on the account page, for reading
// sync (sync.go). Hardcover's OAuth device grant: we get a code,
// the user enters it at hardcover.app/link and approves, and our polling
// gets their tokens. No password or token passes through the browser, and
// none is ever shown or sent to it.

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// PendingLink is a sign-in in progress (or just failed: err).
type PendingLink struct {
	UserCode string
	OpenURL  string // Hardcover's code page, with the code filled in when it gave one
	Expires  time.Time
	err      string
	cancel   context.CancelFunc
}

// Pending is a copy of userID's sign-in in progress, or nil.
func (s *Service) Pending(userID int64) *PendingLink {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending[userID]
	if p == nil {
		return nil
	}
	if p.err == "" && s.now().After(p.Expires) {
		delete(s.pending, userID)
		return nil
	}
	cp := *p
	return &cp
}

func (s *Service) endPending(userID int64, p *PendingLink, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[userID] != p {
		return
	}
	if errMsg == "" {
		delete(s.pending, userID)
	} else {
		p.err = errMsg
	}
}

// StartLink starts a sign-in for userID, or answers the one in progress.
func (s *Service) StartLink(userID int64) (*PendingLink, error) {
	if p := s.Pending(userID); p != nil && p.err == "" {
		return p, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	d, err := s.oauth.StartDevice(ctx, hardcover.SyncScopes)
	cancel()
	if err != nil {
		return nil, err
	}
	pctx, pcancel := context.WithDeadline(context.Background(), d.Expires)
	open := d.VerificationURIComplete
	if open == "" {
		open = d.VerificationURI
	}
	p := &PendingLink{UserCode: d.UserCode, OpenURL: open, Expires: d.Expires, cancel: pcancel}
	s.mu.Lock()
	if old := s.pending[userID]; old != nil {
		old.cancel()
	}
	s.pending[userID] = p
	s.mu.Unlock()
	d.Interval = s.pollInterval(d.Interval)
	go s.runLink(pctx, userID, d, p)
	return p, nil
}

// runLink polls until the user approves the sign-in, declines it, or the
// code expires, and keeps the tokens.
func (s *Service) runLink(ctx context.Context, userID int64, d *hardcover.DeviceAuth, p *PendingLink) {
	defer p.cancel()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("hardcover link: %v", r)
			s.endPending(userID, p, "Linking failed. Try again.")
		}
	}()
	interval := d.Interval
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				s.endPending(userID, p, "The code expired before it was entered on Hardcover. Try again.")
			}
			return
		case <-time.After(interval):
		}
		t, err := s.oauth.PollDevice(ctx, d.DeviceCode)
		switch {
		case errors.Is(err, hardcover.ErrAuthorizationPending):
			continue
		case errors.Is(err, hardcover.ErrSlowDown):
			interval += 5 * time.Second
			continue
		case errors.Is(err, hardcover.ErrAccessDenied), errors.Is(err, hardcover.ErrExpiredCode):
			s.endPending(userID, p, err.Error()+".")
			return
		case err != nil:
			if ctx.Err() != nil {
				continue // reported above
			}
			log.Println("hardcover link:", err)
			continue // unreachable for now: try again until the code expires
		}
		me, err := hardcover.New(hardcover.Config{
			URL: s.syncURL, Token: t.Access, UserAgent: s.userAgent, Budget: hardcover.FreePlan(), Now: s.clock,
		}).Me(ctx)
		if err == nil {
			err = s.saveAccount(userID, me, t)
		}
		if err != nil {
			log.Println("hardcover link:", err)
			rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = s.oauth.Revoke(rctx, t.Refresh)
			cancel()
			s.endPending(userID, p, "Hardcover accepted the code, but the link couldn't be completed. Try again.")
			return
		}
		s.setLinked(userID, true)
		log.Printf("hardcover sync: user %d linked Hardcover account %d", userID, me.ID)
		s.endPending(userID, p, "")
		return
	}
}

// Revoke is what became of a sign-in on Hardcover when a link ended.
type Revoke int

const (
	RevokeNone   Revoke = iota // no usable sign-in was kept: nothing to revoke
	Revoked                    // Hardcover ended it
	RevokeFailed               // Hardcover couldn't be asked, or refused
)

// Unlink ends userID's link: the sign-in is revoked on Hardcover
// (when it answers), and the tokens, waiting changes and sharing choices
// are deleted here whatever it answers.
func (s *Service) Unlink(userID int64) (Revoke, error) {
	if s == nil {
		return RevokeNone, nil
	}
	s.CancelLink(userID)
	l, err := s.loadAccount(userID)
	if err != nil {
		return RevokeNone, err
	}
	revoke := RevokeNone
	if l != nil && l.refresh != "" {
		revoke = RevokeFailed
		ref, err := s.host.Open(sealContext("refresh_token", userID), l.refresh)
		switch {
		case err != nil:
			log.Printf("hardcover unlink: user %d: the sign-in can't be read, so it can't be revoked", userID)
		case s.revoker == nil:
			log.Printf("hardcover unlink: user %d: no Hardcover client id, so the sign-in can't be revoked", userID)
		default:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := s.revoker.Revoke(ctx, ref); err != nil {
				log.Println("hardcover unlink:", err)
			} else {
				revoke = Revoked
			}
			cancel()
		}
	}
	s.setLinked(userID, false)
	// The stars imported from Hardcover stay: they're the user's now.
	for _, table := range []string{"hardcover_sync", "hardcover_shared", "hardcover_unshared", "hardcover_user_books", "hardcover_follows", "hardcover_follow_queue", "hardcover_links"} {
		if _, err := s.db.Exec(`DELETE FROM `+table+` WHERE user_id = ?`, userID); err != nil {
			return revoke, err
		}
	}
	return revoke, nil
}

// AccountView is the account page's Hardcover section.
type AccountView struct {
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
	Summary   SyncSummary
	Share     Share
	Shared    []SharedBook // the books shared, newest first
	Problems  []SharedBook // shared books that can't be sent (not found, or failing)
	Unshared  []SharedBook // sharing every book: those left out, newest first
	Follows   *FollowsView // the authors followed on Hardcover (follows.go)
	Scopes    string       // "off": the scopes the app must allow (hardcover.SyncScopes)
}

// SharedBook is a book a user shared, for the account page's list.
type SharedBook struct {
	UUID, Title, Authors string
}

// sharedBooks lists the library books userID shared.
func (s *Service) sharedBooks(userID int64) []SharedBook {
	return s.listBooks(`SELECT book_uuid FROM hardcover_shared WHERE user_id = ? ORDER BY shared_at DESC`, userID)
}

// unsharedBooks lists the library books userID left out of sharing every book.
func (s *Service) unsharedBooks(userID int64) []SharedBook {
	return s.listBooks(`SELECT book_uuid FROM hardcover_unshared WHERE user_id = ? ORDER BY unshared_at DESC`, userID)
}

// listBooks: the library books whose uuids query lists (those no longer in the library left out).
func (s *Service) listBooks(query string, userID int64) []SharedBook {
	rows, err := s.db.Query(query, userID)
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
	var out []SharedBook
	for _, u := range uuids {
		if title, authors, ok := s.host.BookTitle(u); ok {
			out = append(out, SharedBook{UUID: u, Title: title, Authors: authors})
		}
	}
	return out
}

// View is userID's Hardcover section on the account page. With sync off,
// a user still linked from before gets State "disabled" (they can see it
// and unlink); anyone else nil.
func (s *Service) View(userID int64, notice string) *AccountView {
	if s == nil {
		return nil
	}
	if !s.Enabled() {
		if l, err := s.loadAccount(userID); err == nil && l != nil {
			return &AccountView{State: "disabled", Notice: notice, Username: l.Username, LinkedAt: formatDay(l.LinkedAt.Unix())}
		}
		return nil
	}
	v := &AccountView{State: "unlinked", Notice: notice}
	if p := s.Pending(userID); p != nil {
		if p.err != "" {
			v.Error = p.err
			s.mu.Lock() // shown once
			if q := s.pending[userID]; q != nil && q.err != "" {
				delete(s.pending, userID)
			}
			s.mu.Unlock()
		} else {
			v.State, v.UserCode, v.VerifyURL, v.OpenURL = "pending", p.UserCode, hardcover.VerificationURL, p.OpenURL
			v.Expires = p.Expires.Format("15:04")
			return v
		}
	}
	l, err := s.loadAccount(userID)
	if err != nil || l == nil {
		return v
	}
	v.Username, v.LinkedAt = l.Username, formatDay(l.LinkedAt.Unix())
	if l.Broken != "" {
		v.State, v.Broken = "broken", l.Broken
		return v
	}
	v.State, v.Summary = "linked", s.Summary(userID)
	v.Share, v.Shared, v.Problems = s.LoadShare(userID), s.sharedBooks(userID), s.ProblemBooks(userID)
	if v.Share.All {
		v.Unshared = s.unsharedBooks(userID)
	}
	v.Follows = s.Follows(userID)
	return v
}

// CancelLink ends userID's sign-in in progress, if any.
func (s *Service) CancelLink(userID int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if p := s.pending[userID]; p != nil {
		p.cancel()
		delete(s.pending, userID)
	}
	s.mu.Unlock()
}
