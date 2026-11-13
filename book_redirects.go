package main

// Links to books merged into another (merge.go) keep working: the book
// page, the readers and downloads send an old uuid on to the kept book.
// book_redirects (calibrefast.db) holds old uuid -> kept uuid. A later merge
// of the kept book re-points them, so there are no chains; restoring a
// merged book from the Recycling Bin drops its redirect, since it's itself
// again. A redirect is only looked at when the uuid isn't in the library.

import (
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// noteMerged records that others were merged into keep.
func noteMerged(keep string, others []string) {
	if appDB == nil || len(others) == 0 {
		return
	}
	tx, err := appDB.Begin()
	if err != nil {
		log.Printf("redirects: %v", err)
		return
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	// Books merged into these before now lead to keep too.
	if _, err := tx.Exec(`UPDATE book_redirects SET new_uuid = ? WHERE new_uuid IN (`+placeholders(len(others))+`)`, append([]any{keep}, toArgs(others)...)...); err != nil {
		log.Printf("redirects: %v", err)
		return
	}
	for _, o := range others {
		if _, err := tx.Exec(`INSERT INTO book_redirects (old_uuid, new_uuid, created_at) VALUES (?, ?, ?)
			ON CONFLICT (old_uuid) DO UPDATE SET new_uuid = excluded.new_uuid, created_at = excluded.created_at`, o, keep, now); err != nil {
			log.Printf("redirects: %v", err)
			return
		}
	}
	// The kept book is itself, whatever it was merged into before a restore.
	if _, err := tx.Exec(`DELETE FROM book_redirects WHERE old_uuid = ?`, keep); err != nil {
		log.Printf("redirects: %v", err)
		return
	}
	logErr(tx.Commit(), "redirects: save")
}

// forgetRedirect drops uuid's redirect: the book is in the library again.
func forgetRedirect(uuid string) {
	if appDB != nil {
		logErr(execErr(appDB.Exec(`DELETE FROM book_redirects WHERE old_uuid = ?`, uuid)), "redirects: forget "+uuid)
	}
}

// mergedInto is the book uuid was merged into, if it was ("" if not).
func mergedInto(uuid string) string {
	if appDB == nil || !isSafeUUIDComponent(uuid) {
		return ""
	}
	var to string
	_ = appDB.QueryRow(`SELECT new_uuid FROM book_redirects WHERE old_uuid = ?`, uuid).Scan(&to)
	return to
}

// redirectMerged answers a request for a book that isn't in the library
// with a redirect to the book it was merged into: the same address with the
// uuid replaced (in the path and in ?uuid=). It reports whether it did.
func redirectMerged(w http.ResponseWriter, r *http.Request, uuid string) bool {
	to := mergedInto(uuid)
	if to == "" {
		return false
	}
	u := *r.URL
	u.Path = strings.Replace(u.Path, "/"+uuid, "/"+url.PathEscape(to), 1)
	u.RawPath = ""
	if q := u.Query(); q.Get("uuid") == uuid {
		q.Set("uuid", to)
		u.RawQuery = q.Encode()
	}
	u.Scheme, u.Host = "", ""
	// Not permanent: restoring the old book from the bin undoes it.
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.RequestURI(), http.StatusFound)
	return true
}
