package main

import (
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/internal/hardcoversync"
)

// Users can delete their own accounts. A deleted account isn't removed at
// once: for accountRestoreWindow it stays, unusable (User.Locked), so an
// admin can restore it with everything in it, should it have been a
// mistake or someone else using a stolen sign-in. After that it's purged:
// deleted with all its reading data, as an admin's Delete does at once.

const accountRestoreWindow = 7 * 24 * time.Hour

// accountPurgeEvery: how often deleted accounts past the window are looked for.
const accountPurgeEvery = time.Hour

// purgeAt: when a deleted account is purged.
func purgeAt(deletedAt int64) time.Time {
	return time.Unix(deletedAt, 0).Add(accountRestoreWindow)
}

// deleteOwnAccountHandler: POST /account/delete. The user confirms with
// their password (if they have one) and by typing their username.
func deleteOwnAccountHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	fail := func(msg string, status int) {
		data := AuthPageData{User: u, Identities: identitiesFor(u.ID), Error: msg}
		fillSyncKeyData(&data, r)
		renderAuthPage(w, "account.html", data, status)
	}
	if !strings.EqualFold(strings.TrimSpace(r.FormValue("confirm_username")), u.Username) {
		fail("To delete your account, type your username exactly.", http.StatusBadRequest)
		return
	}
	if u.PasswordSignIn() {
		if !loginLimiter.allow("user:" + strings.ToLower(u.Username)) {
			fail("Too many attempts. Try again in a few minutes.", http.StatusTooManyRequests)
			return
		}
		if _, err := authenticate(u.Username, r.FormValue("current")); err != nil {
			fail("Your password is wrong.", http.StatusBadRequest)
			return
		}
	}
	if u.IsAdmin && adminCount() <= 1 {
		fail("You're the only admin: make someone else an admin before deleting your account.", http.StatusConflict)
		return
	}
	at, err := markAccountDeleted(u.ID)
	if err != nil {
		log.Println("delete account:", err)
		fail("Your account couldn't be deleted. Try again.", http.StatusInternalServerError)
		return
	}
	log.Printf("user %s deleted their account; it's purged after %s", u.Username, purgeAt(at).Format(time.RFC3339))
	endSession(w, r)
	msg := "Your account is deleted. Until " + purgeAt(at).Format("2 Jan 2006") +
		" an admin can restore it, if you ask them; after that it's gone for good."
	http.Redirect(w, r, "/login?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

// markAccountDeleted starts the restore window and signs the account out
// everywhere: sessions end, one-time sign-in links are dropped and in-progress
// Hardcover sign-ins stop. Sync keys, reading data and a linked Hardcover
// account are kept for a restore, but nothing uses them meanwhile (Locked,
// and hcSyncStep skips deleted users).
func markAccountDeleted(id int64) (int64, error) {
	now := nowUnix()
	tx, err := appDB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, now, id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM login_links WHERE user_id = ? AND used_at IS NULL`, id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	hcSync.CancelLink(id)
	return now, nil
}

// adminRestoreUser: POST /api/admin/users/{id}/restore. The account comes
// back as it was; its owner signs in again as before.
func adminRestoreUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	res, err := appDB.Exec(`UPDATE users SET deleted_at = NULL WHERE id = ? AND deleted_at IS NOT NULL`, id)
	if err != nil {
		http.Error(w, "restore failed", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "no deleted account with that id (it may have been purged)", http.StatusNotFound)
		return
	}
	u, _ := getUserByID(id)
	if u != nil {
		log.Printf("admin %s restored the deleted account %s", currentUser(r).Username, u.Username)
	}
	hcSync.Wake() // anything they read before deleting it can be sent again
	writeJSON(w, http.StatusOK, u)
}

// deletedAccount is a row of the admin page's "Deleted accounts".
type deletedAccount struct {
	User
	Deleted string
	Purge   string
}

func deletedAccountView(u User) deletedAccount {
	return deletedAccount{User: u, Deleted: formatDay(u.DeletedAt), Purge: formatDay(purgeAt(u.DeletedAt).Unix())}
}

// purgeUser deletes an account and everything in it (the tables reference
// users ON DELETE CASCADE), ending its Hardcover sign-in there first.
func purgeUser(u *User) error {
	if revoke, err := hcSync.Unlink(u.ID); err != nil || revoke == hardcoversync.RevokeFailed {
		log.Printf("deleting user %s: their Hardcover sign-in couldn't be revoked (it's deleted here): %v", u.Username, err)
	}
	_, err := appDB.Exec(`DELETE FROM users WHERE id = ?`, u.ID)
	return err
}

// purgeDeletedAccounts deletes the accounts deleted more than
// accountRestoreWindow before now.
func purgeDeletedAccounts(now time.Time) {
	rows, err := appDB.Query(`SELECT `+userCols+` FROM users WHERE deleted_at IS NOT NULL AND deleted_at <= ?`,
		now.Add(-accountRestoreWindow).Unix())
	if err != nil {
		log.Println("purge deleted accounts:", err)
		return
	}
	var due []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			log.Println("purge deleted accounts:", err)
			break
		}
		due = append(due, u)
	}
	rows.Close()
	for _, u := range due {
		if err := purgeUser(u); err != nil {
			log.Printf("purge deleted account %s: %v", u.Username, err)
			continue
		}
		log.Printf("purged account %s, deleted by its owner on %s", u.Username, formatDay(u.DeletedAt))
	}
}

// startAccountPurge purges due accounts now and then every accountPurgeEvery.
// The same ticker prunes notifications daily (notifications.go).
func startAccountPurge() {
	purgeDeletedAccounts(time.Now())
	maybeSweepNotifications(time.Now())
	go func() {
		for range time.Tick(accountPurgeEvery) {
			purgeDeletedAccounts(time.Now())
			maybeSweepNotifications(time.Now())
		}
	}()
}
