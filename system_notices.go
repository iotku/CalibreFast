package main

// System notices: announcements from an admin to everyone using the server
// (expected downtime, a new address, …). Each is a dismissable banner
// above the top bar (web/system-alerts.ts) until it expires, and is pinned
// in the notifications menu while it lasts, dismissed or not.
//
// Admins manage them on the Server page (templates/admin-server.html,
// web/admin-server.ts) through /api/admin/system-notices. Everyone gets the
// ones in force with /api/me (meHandler); signed-out visitors too, where the
// catalog is public and so they see the bar.
//
// The text and link label are shown with textContent; the link is checked
// here (a path on this site, or https) and again in the browser (safeUrl).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	systemNoticeTextMax  = 500 // characters
	systemNoticeLabelMax = 60
	systemNoticeHrefMax  = 2000
	// The longest a notice can be set to last: a year.
	systemNoticeMaxLife = 366 * 24 * time.Hour
	// At most this many in force at once.
	systemNoticesMax = 20
	// Expired notices are deleted this long after they end.
	systemNoticeKeepExpired = 30 * 24 * time.Hour
)

type systemNotice struct {
	ID        int64  `json:"id"`
	Text      string `json:"text"`
	LinkHref  string `json:"link_href,omitempty"`
	LinkLabel string `json:"link_label,omitempty"`
	CreatedAt int64  `json:"created_at"` // unix ms
	ExpiresAt int64  `json:"expires_at"` // unix ms
	// Who posted it: in the admins' list only.
	CreatedBy string `json:"created_by,omitempty"`
}

// systemNoticeHref checks a notice's link: empty, a path on this site
// ("/…", not "//…"), or an absolute https URL. Anything else (javascript:,
// data:, http:, a relative path) is refused.
func systemNoticeHref(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) > systemNoticeHrefMax || strings.ContainsAny(s, "\x00\r\n\t") {
		return "", errors.New("the link is too long or has odd characters in it")
	}
	if strings.HasPrefix(s, "/") {
		if strings.HasPrefix(s, "//") || strings.HasPrefix(s, "/\\") {
			return "", errors.New("the link must be a path on this site (/…) or an https:// address")
		}
		return s, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", errors.New("the link must be a path on this site (/…) or an https:// address")
	}
	return u.String(), nil
}

// systemNoticeInput is what an admin sends to post a notice.
type systemNoticeInput struct {
	Text      string `json:"text"`
	LinkHref  string `json:"link_href"`
	LinkLabel string `json:"link_label"`
	ExpiresAt int64  `json:"expires_at"` // unix ms
}

// validate checks and tidies a new notice: the text (one line, at most
// systemNoticeTextMax characters), the link and its label, and an end
// between now and systemNoticeMaxLife from now.
func (in *systemNoticeInput) validate(now time.Time) error {
	in.Text = strings.Join(strings.Fields(in.Text), " ")
	in.LinkLabel = strings.Join(strings.Fields(in.LinkLabel), " ")
	switch {
	case in.Text == "":
		return errors.New("write the notice's text")
	case utf8.RuneCountInString(in.Text) > systemNoticeTextMax:
		return errors.New("the text is too long (" + strconv.Itoa(systemNoticeTextMax) + " characters at most)")
	case utf8.RuneCountInString(in.LinkLabel) > systemNoticeLabelMax:
		return errors.New("the link's label is too long (" + strconv.Itoa(systemNoticeLabelMax) + " characters at most)")
	}
	href, err := systemNoticeHref(in.LinkHref)
	if err != nil {
		return err
	}
	in.LinkHref = href
	if href == "" {
		in.LinkLabel = ""
	} else if in.LinkLabel == "" {
		in.LinkLabel = "More"
	}
	if in.ExpiresAt <= now.UnixMilli() {
		return errors.New("the end must be in the future")
	}
	if in.ExpiresAt > now.Add(systemNoticeMaxLife).UnixMilli() {
		return errors.New("a notice can last a year at most")
	}
	return nil
}

func scanSystemNotices(rows *sql.Rows, withAuthor bool) ([]systemNotice, error) {
	defer rows.Close()
	ns := []systemNotice{}
	for rows.Next() {
		var n systemNotice
		if err := rows.Scan(&n.ID, &n.Text, &n.LinkHref, &n.LinkLabel, &n.CreatedAt, &n.ExpiresAt, &n.CreatedBy); err != nil {
			return nil, err
		}
		if !withAuthor {
			n.CreatedBy = ""
		}
		ns = append(ns, n)
	}
	return ns, rows.Err()
}

// activeSystemNotices: the notices in force at now, oldest first.
func activeSystemNotices(now time.Time, withAuthor bool) ([]systemNotice, error) {
	if appDB == nil {
		return nil, nil
	}
	rows, err := appDB.Query(`SELECT id, text, link_href, link_label, created_at, expires_at, created_by
		FROM system_notices WHERE expires_at > ? ORDER BY created_at, id`, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	return scanSystemNotices(rows, withAuthor)
}

// addSystemNotice stores a checked notice, and deletes the ones long expired.
func addSystemNotice(in systemNoticeInput, by string, now time.Time) (systemNotice, error) {
	tx, err := appDB.Begin()
	if err != nil {
		return systemNotice{}, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM system_notices WHERE expires_at > ?`, now.UnixMilli()).Scan(&n); err != nil {
		return systemNotice{}, err
	}
	if n >= systemNoticesMax {
		return systemNotice{}, errTooManyNotices
	}
	if _, err := tx.Exec(`DELETE FROM system_notices WHERE expires_at <= ?`, now.Add(-systemNoticeKeepExpired).UnixMilli()); err != nil {
		return systemNotice{}, err
	}
	sn := systemNotice{Text: in.Text, LinkHref: in.LinkHref, LinkLabel: in.LinkLabel, CreatedAt: now.UnixMilli(), ExpiresAt: in.ExpiresAt, CreatedBy: by}
	res, err := tx.Exec(`INSERT INTO system_notices (text, link_href, link_label, created_at, expires_at, created_by) VALUES (?, ?, ?, ?, ?, ?)`,
		sn.Text, sn.LinkHref, sn.LinkLabel, sn.CreatedAt, sn.ExpiresAt, sn.CreatedBy)
	if err != nil {
		return systemNotice{}, err
	}
	if sn.ID, err = res.LastInsertId(); err != nil {
		return systemNotice{}, err
	}
	return sn, tx.Commit()
}

var errTooManyNotices = errors.New("there are " + strconv.Itoa(systemNoticesMax) + " notices in force already: end one first")

// GET /api/admin/system-notices: the notices in force, with who posted them.
func systemNoticesHandler(w http.ResponseWriter, r *http.Request) {
	ns, err := activeSystemNotices(time.Now(), true)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notices": ns})
}

// POST /api/admin/system-notices: post one, shown to everyone from their next page.
func systemNoticeCreateHandler(w http.ResponseWriter, r *http.Request) {
	var in systemNoticeInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	now := time.Now()
	if err := in.validate(now); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	admin := currentUser(r).Username
	n, err := addSystemNotice(in, admin, now)
	if errors.Is(err, errTooManyNotices) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	log.Printf("admin %q posted system notice %d, until %s", admin, n.ID, time.UnixMilli(n.ExpiresAt).Format(time.RFC3339))
	writeJSON(w, http.StatusCreated, n)
}

// DELETE /api/admin/system-notices/{id}: end one now. It goes from
// everyone's bar and menu on their next page.
func systemNoticeDeleteHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	res, err := appDB.Exec(`DELETE FROM system_notices WHERE id = ?`, id)
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "no such notice", http.StatusNotFound)
		return
	}
	log.Printf("admin %q ended system notice %d", currentUser(r).Username, id)
	w.WriteHeader(http.StatusNoContent)
}
