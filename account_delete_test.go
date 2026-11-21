package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/iotku/CalibreFast/pkg/kosync"
)

func deleteOwn(t *testing.T, u *User, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/account/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	deleteOwnAccountHandler(rec, asUser(req, u))
	return rec
}

func TestDeleteOwnAccountNeedsConfirmation(t *testing.T) {
	setupTestAppDB(t)
	u, _ := createUser("carol", "longenough-pw", false)
	for name, form := range map[string]url.Values{
		"no username":    {"current": {"longenough-pw"}},
		"wrong username": {"current": {"longenough-pw"}, "confirm_username": {"carl"}},
		"wrong password": {"current": {"wrong-password"}, "confirm_username": {"carol"}},
	} {
		rec := deleteOwn(t, u, form)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Delete my account") {
			t.Errorf("%s: the account page isn't shown again with the form", name)
		}
	}
	if got, _ := getUserByID(u.ID); got.DeletedAt != 0 {
		t.Fatal("deleted without confirmation")
	}
}

func TestDeleteOwnAccountAndRestore(t *testing.T) {
	setupTestAppDB(t)
	admin, _ := createUser("theadmin", "longenough-pw", true)
	u, _ := createUser("dave", "longenough-pw", false)
	if err := startSession(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := newSyncKey(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := setFavorite(u.ID, "book-1", true, nowUnix()); err != nil {
		t.Fatal(err)
	}

	rec := deleteOwn(t, u, url.Values{"current": {"longenough-pw"}, "confirm_username": {"Dave"}})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login?") {
		t.Fatalf("delete: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	got, _ := getUserByID(u.ID)
	if got == nil || got.DeletedAt == 0 || !got.Locked() {
		t.Fatalf("not marked deleted: %+v", got)
	}
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, u.ID).Scan(&n)
	if n != 0 {
		t.Error("sessions kept after deleting the account")
	}
	if _, err := authenticate("dave", "longenough-pw"); err != errBadCredentials {
		t.Errorf("deleted account signed in: %v", err)
	}
	if _, err := createUser("dave", "longenough-pw", false); err != errUsernameTaken {
		t.Errorf("username reusable while restorable: %v", err)
	}

	// Admins see it under Deleted accounts.
	page := httptest.NewRecorder()
	adminUsersPage(page, asUser(httptest.NewRequest("GET", "/admin/users", nil), admin))
	if body := page.Body.String(); !strings.Contains(body, `id="deleted-table"`) || !strings.Contains(body, `data-action="restore"`) {
		t.Error("deleted account not listed for restoring on the admin page")
	}

	// An admin restores it, with its data.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/admin/users/{id}/restore", adminRestoreUser)
	restore := func() int {
		req := httptest.NewRequest("POST", "/api/admin/users/"+itoa(u.ID)+"/restore", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, asUser(req, admin))
		return rec.Code
	}
	if c := restore(); c != http.StatusOK {
		t.Fatalf("restore: %d", c)
	}
	if c := restore(); c != http.StatusNotFound {
		t.Errorf("restoring an account that isn't deleted: %d", c)
	}
	if _, err := authenticate("dave", "longenough-pw"); err != nil {
		t.Errorf("restored account can't sign in: %v", err)
	}
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM favorites WHERE user_id = ?`, u.ID).Scan(&n)
	if n != 1 {
		t.Error("favorites lost on restore")
	}
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM sync_keys WHERE user_id = ?`, u.ID).Scan(&n)
	if n != 1 {
		t.Error("sync key lost on restore")
	}
}

func TestDeletedAccountPurgedAfterWindow(t *testing.T) {
	setupTestAppDB(t)
	u, _ := createUser("erin", "longenough-pw", false)
	keep, _ := createUser("frank", "longenough-pw", false)
	_, _ = setFavorite(u.ID, "book-1", true, nowUnix())
	at, err := markAccountDeleted(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	deleted := time.Unix(at, 0)
	purgeDeletedAccounts(deleted.Add(accountRestoreWindow - time.Minute))
	if _, err := getUserByID(u.ID); err != nil {
		t.Fatal("purged before the restore window ended")
	}
	purgeDeletedAccounts(deleted.Add(accountRestoreWindow + time.Minute))
	if _, err := getUserByID(u.ID); err == nil {
		t.Fatal("not purged after the restore window")
	}
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM favorites WHERE user_id = ?`, u.ID).Scan(&n)
	if n != 0 {
		t.Error("favorites outlived the purged account")
	}
	if _, err := getUserByID(keep.ID); err != nil {
		t.Error("an account that wasn't deleted was purged")
	}
}

func TestLastAdminCantDeleteOwnAccount(t *testing.T) {
	setupTestAppDB(t)
	a, _ := createUser("soleadmin", "longenough-pw", true)
	rec := deleteOwn(t, a, url.Values{"current": {"longenough-pw"}, "confirm_username": {"soleadmin"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("last admin deleting themselves: %d", rec.Code)
	}
	if got, _ := getUserByID(a.ID); got.DeletedAt != 0 {
		t.Fatal("last admin deleted")
	}
}

func TestDeletedAccountLocksSyncKey(t *testing.T) {
	setupTestAppDB(t)
	u, _ := createUser("gina", "longenough-pw", false)
	key, err := newSyncKey(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if syncKeyUser("gina", kosync.MD5Hex(key)) == nil {
		t.Fatal("sync key doesn't work before deleting")
	}
	if _, err := markAccountDeleted(u.ID); err != nil {
		t.Fatal(err)
	}
	if syncKeyUser("gina", kosync.MD5Hex(key)) != nil {
		t.Error("sync key works for a deleted account")
	}
}
