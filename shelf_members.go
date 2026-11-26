package main

// Shelf collaborators: the owner of a shared shelf invites other users to
// help with it, by username. An invite is only a question: nothing changes
// until the invited user accepts it, and then they're a member, who adds,
// removes and arranges the shelf's books (shelfForBooks). The shelf's name,
// description, sharing, members and deletion stay the owner's.
//
//   - Only the owner invites, only while the shelf is shared, only accounts
//     in use, and not someone who blocked them.
//   - Limits keep one account from flooding others: maxInvitesPerDay sent
//     a day, maxPendingInvites unanswered at once, maxShelfPeople members
//     and invites on a shelf.
//   - The invited user gets a notification (shelf_invite, notifications.go)
//     and answers on /shelves: Accept, Decline, or Decline and block (no
//     more invites from that user, until they unblock them).
//   - Unsharing a shelf withdraws its unanswered invites; members stay, and
//     still see it. The owner can remove a member; a member can leave.
//   - Every invite, answer, refusal and removal is in shelf_invite_log, which
//     admins read on /admin/shelf-invites to spot accounts sending
//     unwanted invites. It keeps names as they were and outlives accounts
//     and shelves, for shelfInviteLogKeep.
//
// Routes (routes.go):
//
//	GET    /api/shelves/{id}/members                 owner and members: members, and (owner) pending invites
//	POST   /api/shelves/{id}/invites  {"username"}   owner
//	DELETE /api/shelves/{id}/invites/{invite}        owner: withdraw
//	DELETE /api/shelves/{id}/members/{user}          owner: remove; the member themself: leave
//	GET    /api/me/shelf-invites                     invites to me, and whom I've blocked
//	POST   /api/me/shelf-invites/{invite}/accept
//	POST   /api/me/shelf-invites/{invite}/decline    {"block": true} also blocks the inviter
//	DELETE /api/me/shelf-blocks/{user}               unblock
//	GET    /admin/shelf-invites                      the log (admins)

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxInvitesPerDay   = 20 // invites one user sends in 24 hours, every shelf together
	maxPendingInvites  = 50 // one user's unanswered invites at once
	maxShelfPeople     = 25 // members and unanswered invites on one shelf
	shelfInviteLogKeep = 365 * 24 * time.Hour
	shelfInviteLogPage = 300 // entries on the admin page
)

// Log actions (shelf_invite_log.action). From is always the inviter (the
// shelf's owner), To the invited user, whoever did it.
const (
	inviteSent     = "sent"
	inviteRefused  = "refused"  // not sent: detail says why
	inviteAccepted = "accepted" // by To
	inviteDeclined = "declined" // by To
	inviteBlocked  = "blocked"  // declined by To, who blocks From
	inviteUnblock  = "unblocked"
	inviteRevoked  = "withdrawn" // by From (or the shelf unshared or deleted)
	memberRemoved  = "removed"   // From took To off the shelf
	memberLeft     = "left"      // To left the shelf
)

// inviteActor is a user as the log names them: id 0 for none.
type inviteActor struct {
	ID   int64
	Name string
}

func actorOf(u *User) inviteActor {
	if u == nil {
		return inviteActor{}
	}
	return inviteActor{u.ID, u.Username}
}

// logInvite adds to the invite log, and drops entries older than
// shelfInviteLogKeep.
func logInvite(ex execer, action string, from, to inviteActor, shelfID int64, shelfName, detail string) {
	now := time.Now()
	if _, err := ex.Exec(`INSERT INTO shelf_invite_log (at, action, from_id, from_name, to_id, to_name, shelf_id, shelf_name, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, now.Unix(), action, from.ID, from.Name, to.ID, to.Name, shelfID, shelfName, detail); err != nil {
		log.Printf("shelf invite log: %v", err)
	}
	_, _ = ex.Exec(`DELETE FROM shelf_invite_log WHERE at < ?`, now.Add(-shelfInviteLogKeep).Unix())
}

// shelfInvite: an unanswered invite.
type shelfInvite struct {
	ID        int64  `json:"id"`
	ShelfID   int64  `json:"shelf_id"`
	ShelfName string `json:"shelf_name"`
	FromID    int64  `json:"from_id"`
	From      string `json:"from"`
	ToID      int64  `json:"to_id"`
	To        string `json:"to"`
	CreatedAt int64  `json:"created_at"`
	eventID   int64
}

const inviteCols = `i.id, i.shelf_id, s.name, i.from_id, f.username, i.to_id, t.username, i.created_at, i.event_id`

const inviteFrom = ` FROM shelf_invites i JOIN shelves s ON s.id = i.shelf_id
	JOIN users f ON f.id = i.from_id JOIN users t ON t.id = i.to_id `

func scanInvites(rows *sql.Rows, err error) ([]shelfInvite, error) {
	out := []shelfInvite{}
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var v shelfInvite
		if err := rows.Scan(&v.ID, &v.ShelfID, &v.ShelfName, &v.FromID, &v.From, &v.ToID, &v.To, &v.CreatedAt, &v.eventID); err != nil {
			return out, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// dropInvite deletes an invite and its notification.
func dropInvite(ex execer, v shelfInvite) error {
	if _, err := ex.Exec(`DELETE FROM shelf_invites WHERE id = ?`, v.ID); err != nil {
		return err
	}
	if v.eventID != 0 {
		_, err := ex.Exec(`DELETE FROM notification_events WHERE id = ?`, v.eventID)
		return err
	}
	return nil
}

// revokeShelfInvites withdraws every unanswered invite to s (it was
// unshared or deleted; by is who did it).
func revokeShelfInvites(s *Shelf, by *User, why string) {
	invites, err := scanInvites(appDB.Query(`SELECT `+inviteCols+inviteFrom+`WHERE i.shelf_id = ?`, s.ID))
	if err != nil {
		log.Printf("shelf invites: %v", err)
		return
	}
	for _, v := range invites {
		if err := dropInvite(appDB, v); err != nil {
			log.Printf("shelf invites: %v", err)
			continue
		}
		logInvite(appDB, inviteRevoked, inviteActor{v.FromID, v.From}, inviteActor{v.ToID, v.To}, s.ID, s.Name, why)
	}
}

// shelfMember: a member, as the owner's list shows them.
type shelfMember struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Since    int64  `json:"since"`
}

// ---------------------------------------------------------------------------
// The owner's side
// ---------------------------------------------------------------------------

// GET /api/shelves/{id}/members: the shelf's members; for its owner, also
// the invites not yet answered and whether they can invite now.
func shelfMembersHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	u := currentUser(r)
	s, err := getShelf(id)
	if err != nil || !s.canView(u) {
		http.Error(w, "no such shelf", http.StatusNotFound)
		return
	}
	owner := s.isOwner(u)
	if !owner && !s.isMember(u) {
		http.Error(w, "only the shelf's owner and members see who helps with it", http.StatusForbidden)
		return
	}
	rows, err := appDB.Query(`SELECT u.id, u.username, m.created_at FROM shelf_members m JOIN users u ON u.id = m.user_id
		WHERE m.shelf_id = ? ORDER BY u.username COLLATE NOCASE`, s.ID)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	members := []shelfMember{}
	for rows.Next() {
		var m shelfMember
		if err := rows.Scan(&m.ID, &m.Username, &m.Since); err != nil {
			rows.Close()
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		members = append(members, m)
	}
	rows.Close()
	out := map[string]any{"owner": s.Owner, "members": members, "is_owner": owner}
	if owner {
		invites, err := scanInvites(appDB.Query(`SELECT `+inviteCols+inviteFrom+`WHERE i.shelf_id = ? ORDER BY i.created_at`, s.ID))
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		out["invites"] = invites
		out["can_invite"] = s.Shared
	}
	writeJSON(w, http.StatusOK, out)
}

// errInvite: why an invite wasn't sent, and the status to answer with.
type errInvite struct {
	status int
	msg    string
	log    string // the log's detail; "" to leave it out of the log
}

func (e *errInvite) Error() string { return e.msg }

// POST /api/shelves/{id}/invites {"username": "…"}: invite a user to help.
func shelfInviteHandler(w http.ResponseWriter, r *http.Request) {
	s, ok := shelfForEdit(w, r)
	if !ok {
		return
	}
	u := currentUser(r)
	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Username)
	v, err := sendInvite(s, u, name)
	if err != nil {
		var e *errInvite
		if !errors.As(err, &e) {
			http.Error(w, "could not invite", http.StatusInternalServerError)
			return
		}
		if e.log != "" {
			to := inviteActor{Name: truncateRunes(name, 100)}
			if t, err := activeUserNamed(name); err == nil {
				to = actorOf(t)
			}
			logInvite(appDB, inviteRefused, actorOf(u), to, s.ID, s.Name, e.log)
		}
		http.Error(w, e.msg, e.status)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// activeUserNamed: the account in use with this name.
func activeUserNamed(name string) (*User, error) {
	var t User
	err := appDB.QueryRow(`SELECT id, username FROM users WHERE username = ? AND disabled = 0 AND deleted_at IS NULL`, name).
		Scan(&t.ID, &t.Username)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// sendInvite checks the rules and limits, then stores the invite and
// notifies the invited user, all in one transaction.
func sendInvite(s *Shelf, from *User, name string) (*shelfInvite, error) {
	if s.Global || !s.isOwner(from) {
		return nil, &errInvite{http.StatusForbidden, "only a shelf's owner can invite people to it", ""}
	}
	if !s.Shared {
		return nil, &errInvite{http.StatusConflict, "share the shelf with everyone first: invites are to a shared shelf", ""}
	}
	if name == "" {
		return nil, &errInvite{http.StatusBadRequest, "whom to invite? Give their username", ""}
	}
	to, err := activeUserNamed(name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &errInvite{http.StatusNotFound, "no such user", "no such user"}
	}
	if err != nil {
		return nil, err
	}
	if to.ID == from.ID {
		return nil, &errInvite{http.StatusBadRequest, "it's your shelf already", ""}
	}
	tx, err := appDB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	count := func(q string, args ...any) (int, error) {
		var n int
		err := tx.QueryRow(q, args...).Scan(&n)
		return n, err
	}
	if n, err := count(`SELECT COUNT(*) FROM shelf_members WHERE shelf_id = ? AND user_id = ?`, s.ID, to.ID); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, &errInvite{http.StatusConflict, to.Username + " already helps with this shelf", ""}
	}
	if n, err := count(`SELECT COUNT(*) FROM shelf_invites WHERE shelf_id = ? AND to_id = ?`, s.ID, to.ID); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, &errInvite{http.StatusConflict, to.Username + " is invited already", ""}
	}
	if n, err := count(`SELECT COUNT(*) FROM shelf_invite_blocks WHERE user_id = ? AND blocked_id = ?`, to.ID, from.ID); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, &errInvite{http.StatusForbidden, to.Username + " doesn't take invites from you", "blocked by them"}
	}
	if n, err := count(`SELECT (SELECT COUNT(*) FROM shelf_members WHERE shelf_id = ?1) + (SELECT COUNT(*) FROM shelf_invites WHERE shelf_id = ?1)`, s.ID); err != nil {
		return nil, err
	} else if n >= maxShelfPeople {
		return nil, &errInvite{http.StatusConflict, "a shelf can have at most " + strconv.Itoa(maxShelfPeople) + " members and invites", "shelf full"}
	}
	if n, err := count(`SELECT COUNT(*) FROM shelf_invites WHERE from_id = ?`, from.ID); err != nil {
		return nil, err
	} else if n >= maxPendingInvites {
		return nil, &errInvite{http.StatusTooManyRequests, "you have " + strconv.Itoa(n) + " invites nobody has answered yet: withdraw some first", "too many unanswered"}
	}
	if n, err := count(`SELECT COUNT(*) FROM shelf_invite_log WHERE from_id = ? AND action = ? AND at > ?`,
		from.ID, inviteSent, time.Now().Add(-24*time.Hour).Unix()); err != nil {
		return nil, err
	} else if n >= maxInvitesPerDay {
		return nil, &errInvite{http.StatusTooManyRequests, "you've sent " + strconv.Itoa(maxInvitesPerDay) + " invites today: try again tomorrow", "daily limit"}
	}
	eventID, err := addEvent(tx, notificationEvent{Kind: notifyShelfInvite, Title: from.Username, Subject: s.Name})
	if err != nil {
		return nil, err
	}
	now := nowUnix()
	res, err := tx.Exec(`INSERT INTO shelf_invites (shelf_id, from_id, to_id, event_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		s.ID, from.ID, to.ID, eventID, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := deliver(tx, to.ID, eventID, false); err != nil {
		return nil, err
	}
	logInvite(tx, inviteSent, actorOf(from), actorOf(to), s.ID, s.Name, "")
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &shelfInvite{ID: id, ShelfID: s.ID, ShelfName: s.Name, FromID: from.ID, From: from.Username, ToID: to.ID, To: to.Username, CreatedAt: now}, nil
}

// DELETE /api/shelves/{id}/invites/{invite}: the owner withdraws an invite.
func shelfRevokeInviteHandler(w http.ResponseWriter, r *http.Request) {
	s, ok := shelfForEdit(w, r)
	if !ok {
		return
	}
	iid, ok := pathID(w, r, "invite")
	if !ok {
		return
	}
	invites, err := scanInvites(appDB.Query(`SELECT `+inviteCols+inviteFrom+`WHERE i.id = ? AND i.shelf_id = ?`, iid, s.ID))
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if len(invites) == 0 {
		http.Error(w, "no such invite", http.StatusNotFound)
		return
	}
	v := invites[0]
	if err := dropInvite(appDB, v); err != nil {
		http.Error(w, "could not withdraw", http.StatusInternalServerError)
		return
	}
	logInvite(appDB, inviteRevoked, inviteActor{v.FromID, v.From}, inviteActor{v.ToID, v.To}, s.ID, s.Name, "")
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/shelves/{id}/members/{user}: the owner takes a member off,
// or a member leaves.
func shelfRemoveMemberHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	uid, ok := pathID(w, r, "user")
	if !ok {
		return
	}
	u := currentUser(r)
	s, err := getShelf(id)
	if err != nil || !s.canView(u) {
		http.Error(w, "no such shelf", http.StatusNotFound)
		return
	}
	self := uid == u.ID
	if !self && !s.isOwner(u) {
		http.Error(w, "only the shelf's owner can remove its members", http.StatusForbidden)
		return
	}
	var name string
	err = appDB.QueryRow(`SELECT u.username FROM shelf_members m JOIN users u ON u.id = m.user_id WHERE m.shelf_id = ? AND m.user_id = ?`, s.ID, uid).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "not a member", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if _, err := appDB.Exec(`DELETE FROM shelf_members WHERE shelf_id = ? AND user_id = ?`, s.ID, uid); err != nil {
		http.Error(w, "could not remove", http.StatusInternalServerError)
		return
	}
	// Their pin goes too: unshared, they couldn't see the shelf any more.
	_, _ = appDB.Exec(`DELETE FROM shelf_pins WHERE shelf_id = ? AND user_id = ?`, s.ID, uid)
	action := memberRemoved
	if self {
		action = memberLeft
	}
	logInvite(appDB, action, inviteActor{s.ownerID.Int64, s.Owner}, inviteActor{uid, name}, s.ID, s.Name, "")
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// The invited user's side
// ---------------------------------------------------------------------------

// visibleInvitesSQL: invites to ?1 from accounts in use, to shelves still
// shared (an invite from a disabled account waits, unseen).
const visibleInvitesSQL = `WHERE i.to_id = ?1 AND f.disabled = 0 AND f.deleted_at IS NULL AND s.shared = 1`

// pendingInvites: the invites waiting for u's answer, newest first.
func pendingInvites(u *User) ([]shelfInvite, error) {
	return scanInvites(appDB.Query(`SELECT `+inviteCols+inviteFrom+visibleInvitesSQL+` ORDER BY i.created_at DESC, i.id DESC`, u.ID))
}

type blockedUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// blockedInviters: whom u takes no invites from.
func blockedInviters(u *User) ([]blockedUser, error) {
	out := []blockedUser{}
	rows, err := appDB.Query(`SELECT u.id, u.username FROM shelf_invite_blocks b JOIN users u ON u.id = b.blocked_id
		WHERE b.user_id = ? ORDER BY u.username COLLATE NOCASE`, u.ID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var b blockedUser
		if err := rows.Scan(&b.ID, &b.Username); err != nil {
			return out, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GET /api/me/shelf-invites
func myShelfInvitesHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	invites, err := pendingInvites(u)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	blocked, err := blockedInviters(u)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": invites, "blocked": blocked})
}

// myInvite: the invite in the path, if it's to the current user (404 if not).
func myInvite(w http.ResponseWriter, r *http.Request) (*shelfInvite, bool) {
	iid, ok := pathID(w, r, "invite")
	if !ok {
		return nil, false
	}
	invites, err := scanInvites(appDB.Query(`SELECT `+inviteCols+inviteFrom+visibleInvitesSQL+` AND i.id = ?2`, currentUser(r).ID, iid))
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return nil, false
	}
	if len(invites) == 0 {
		http.Error(w, "no such invite", http.StatusNotFound)
		return nil, false
	}
	return &invites[0], true
}

// POST /api/me/shelf-invites/{invite}/accept: the user is a member now.
func acceptShelfInviteHandler(w http.ResponseWriter, r *http.Request) {
	v, ok := myInvite(w, r)
	if !ok {
		return
	}
	tx, err := appDB.Begin()
	if err != nil {
		http.Error(w, "could not accept", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO shelf_members (shelf_id, user_id, created_at) VALUES (?, ?, ?)`, v.ShelfID, v.ToID, nowUnix()); err != nil {
		http.Error(w, "could not accept", http.StatusInternalServerError)
		return
	}
	if err := dropInvite(tx, *v); err != nil {
		http.Error(w, "could not accept", http.StatusInternalServerError)
		return
	}
	logInvite(tx, inviteAccepted, inviteActor{v.FromID, v.From}, inviteActor{v.ToID, v.To}, v.ShelfID, v.ShelfName, "")
	if err := tx.Commit(); err != nil {
		http.Error(w, "could not accept", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shelf_id": v.ShelfID, "href": "/shelf/" + itoa(v.ShelfID)})
}

// POST /api/me/shelf-invites/{invite}/decline {"block": bool}
func declineShelfInviteHandler(w http.ResponseWriter, r *http.Request) {
	v, ok := myInvite(w, r)
	if !ok {
		return
	}
	var req struct {
		Block bool `json:"block"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
	}
	tx, err := appDB.Begin()
	if err != nil {
		http.Error(w, "could not decline", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := dropInvite(tx, *v); err != nil {
		http.Error(w, "could not decline", http.StatusInternalServerError)
		return
	}
	action := inviteDeclined
	if req.Block {
		action = inviteBlocked
		if _, err := tx.Exec(`INSERT OR IGNORE INTO shelf_invite_blocks (user_id, blocked_id, created_at) VALUES (?, ?, ?)`, v.ToID, v.FromID, nowUnix()); err != nil {
			http.Error(w, "could not block", http.StatusInternalServerError)
			return
		}
		// Their other invites to this user go too.
		others, err := scanInvites(tx.Query(`SELECT `+inviteCols+inviteFrom+`WHERE i.to_id = ? AND i.from_id = ?`, v.ToID, v.FromID))
		if err != nil {
			http.Error(w, "could not block", http.StatusInternalServerError)
			return
		}
		for _, o := range others {
			if err := dropInvite(tx, o); err != nil {
				http.Error(w, "could not block", http.StatusInternalServerError)
				return
			}
			logInvite(tx, inviteBlocked, inviteActor{o.FromID, o.From}, inviteActor{o.ToID, o.To}, o.ShelfID, o.ShelfName, "")
		}
	}
	logInvite(tx, action, inviteActor{v.FromID, v.From}, inviteActor{v.ToID, v.To}, v.ShelfID, v.ShelfName, "")
	if err := tx.Commit(); err != nil {
		http.Error(w, "could not decline", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/me/shelf-blocks/{user}: take invites from them again.
func unblockInviterHandler(w http.ResponseWriter, r *http.Request) {
	uid, ok := pathID(w, r, "user")
	if !ok {
		return
	}
	u := currentUser(r)
	var name string
	err := appDB.QueryRow(`SELECT u.username FROM shelf_invite_blocks b JOIN users u ON u.id = b.blocked_id
		WHERE b.user_id = ? AND b.blocked_id = ?`, u.ID, uid).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if _, err := appDB.Exec(`DELETE FROM shelf_invite_blocks WHERE user_id = ? AND blocked_id = ?`, u.ID, uid); err != nil {
		http.Error(w, "could not unblock", http.StatusInternalServerError)
		return
	}
	logInvite(appDB, inviteUnblock, inviteActor{uid, name}, actorOf(u), 0, "", "")
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Admins: the log
// ---------------------------------------------------------------------------

type inviteLogEntry struct {
	At                int64
	Action            string
	FromID, ToID      int64
	FromName, ToName  string
	ShelfID           int64
	ShelfName, Detail string
}

// When: the entry's time, for the page.
func (e inviteLogEntry) When() string { return time.Unix(e.At, 0).Format("2006-01-02 15:04") }

// inviteSender: one user's invites over inviteSummaryDays, for the page's
// summary: who sends many, and how they're answered.
type inviteSender struct {
	ID                                         int64
	Name                                       string
	Sent, Accepted, Declined, Blocked, Refused int
	Pending                                    int
	BlockedBy                                  int // users who block them now
}

// Suspect: worth a look: blocked by someone, or mostly turned down.
func (s inviteSender) Suspect() bool {
	answered := s.Accepted + s.Declined + s.Blocked
	return s.Blocked > 0 || s.BlockedBy > 0 || s.Refused >= 5 || (answered >= 5 && s.Accepted*4 < answered)
}

const inviteSummaryDays = 30

func inviteSenders() ([]inviteSender, error) {
	rows, err := appDB.Query(`SELECT l.from_id, MAX(l.from_name),
			SUM(l.action = 'sent'), SUM(l.action = 'accepted'), SUM(l.action = 'declined'),
			SUM(l.action = 'blocked'), SUM(l.action = 'refused'),
			(SELECT COUNT(*) FROM shelf_invites i WHERE i.from_id = l.from_id),
			(SELECT COUNT(*) FROM shelf_invite_blocks b WHERE b.blocked_id = l.from_id)
		FROM shelf_invite_log l
		WHERE l.at > ? AND l.from_id != 0 AND l.action IN ('sent', 'accepted', 'declined', 'blocked', 'refused')
		GROUP BY l.from_id
		ORDER BY SUM(l.action = 'blocked') DESC, SUM(l.action = 'sent') DESC
		LIMIT 100`, time.Now().Add(-inviteSummaryDays*24*time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []inviteSender{}
	for rows.Next() {
		var s inviteSender
		if err := rows.Scan(&s.ID, &s.Name, &s.Sent, &s.Accepted, &s.Declined, &s.Blocked, &s.Refused, &s.Pending, &s.BlockedBy); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// inviteLog: the newest entries, those involving userID if it's not 0.
func inviteLog(userID int64, limit int) ([]inviteLogEntry, error) {
	q := `SELECT at, action, from_id, from_name, to_id, to_name, shelf_id, shelf_name, detail FROM shelf_invite_log`
	args := []any{}
	if userID != 0 {
		q += ` WHERE from_id = ? OR to_id = ?`
		args = append(args, userID, userID)
	}
	q += ` ORDER BY at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := appDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []inviteLogEntry{}
	for rows.Next() {
		var e inviteLogEntry
		if err := rows.Scan(&e.At, &e.Action, &e.FromID, &e.FromName, &e.ToID, &e.ToName, &e.ShelfID, &e.ShelfName, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GET /admin/shelf-invites[?user=ID]
func shelfInvitesAdminPage(w http.ResponseWriter, r *http.Request) {
	uid, _ := strconv.ParseInt(r.URL.Query().Get("user"), 10, 64)
	senders, err := inviteSenders()
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	entries, err := inviteLog(uid, shelfInviteLogPage)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	var who string
	if uid != 0 {
		_ = appDB.QueryRow(`SELECT username FROM users WHERE id = ?`, uid).Scan(&who)
		if who == "" {
			for _, e := range entries {
				if e.FromID == uid {
					who = e.FromName
				} else if e.ToID == uid {
					who = e.ToName
				}
				if who != "" {
					break
				}
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "admin-shelf-invites.html", map[string]any{
		"Title": SiteTitle, "Senders": senders, "Entries": entries, "UserID": uid, "Who": who,
		"Days": inviteSummaryDays, "PerDay": maxInvitesPerDay, "Pending": maxPendingInvites, "PerShelf": maxShelfPeople,
	}), "render admin-shelf-invites")
}
