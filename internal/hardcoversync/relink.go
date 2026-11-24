package hardcoversync

// Telling users to link Hardcover again when linking asks for more than it
// did when they linked (hardcover.SyncScopes grew): a link keeps the
// permissions (scopes) it was given, so what needs the new ones doesn't
// work for them until they link again.
//
// At startup, with Hardcover sync on, each working link missing some of
// SyncScopes gets one notification (notifications.go, kind
// hardcover_relink) naming what linking again adds, and linking to the
// account page. hardcover_links.relink_notified keeps the scopes asked
// for at the time, so a dismissed notification doesn't come back until
// linking asks for something new again. Linking again removes it.

import (
	"log"
	"strings"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// scopeFeatures: what each scope beyond reading sync's first ones lets
// a user do, for the notification. A scope not listed adds nothing
// worth a notification by itself.
var scopeFeatures = []struct{ scope, feature string }{
	{"read:social", "bring in the authors you follow there"},
	{"write:social", "follow the authors you star here"},
}

// relinkFeatures: what linking again would let a link with this scope
// do; none: nothing to tell.
func relinkFeatures(scope string) []string {
	var out []string
	for _, f := range scopeFeatures {
		if strings.Contains(" "+hardcover.SyncScopes+" ", " "+f.scope+" ") && !hardcover.HasScope(scope, f.scope) {
			out = append(out, f.feature)
		}
	}
	return out
}

// NotifyRelink notifies each working link that's missing scopes, once
// per SyncScopes.
func (s *Service) NotifyRelink() {
	type link struct {
		userID int64
		scope  string
	}
	rows, err := s.db.Query(`SELECT l.user_id, l.scope FROM hardcover_links l JOIN users u ON u.id = l.user_id AND u.deleted_at IS NULL
		WHERE l.broken = '' AND l.relink_notified <> ?`, hardcover.SyncScopes)
	if err != nil {
		log.Println("hardcover relink:", err)
		return
	}
	var links []link
	for rows.Next() {
		var l link
		if rows.Scan(&l.userID, &l.scope) == nil {
			links = append(links, l)
		}
	}
	rows.Close()
	for _, l := range links {
		if err := s.notifyRelinkUser(l.userID, l.scope); err != nil {
			log.Printf("hardcover relink: user %d: %v", l.userID, err)
		}
	}
}

func (s *Service) notifyRelinkUser(userID int64, scope string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if features := relinkFeatures(scope); len(features) > 0 {
		// One at a time: an older one (an earlier change) is replaced.
		if err := s.host.ReplaceRelinkNotice(tx, userID, joinAnd(features)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE hardcover_links SET relink_notified = ? WHERE user_id = ?`, hardcover.SyncScopes, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// relinked: userID just linked Hardcover: the notification is done with.
func (s *Service) relinked(userID int64) {
	if err := s.host.ClearRelinkNotice(userID); err != nil {
		log.Println("hardcover relink:", err)
	}
	if _, err := s.db.Exec(`UPDATE hardcover_links SET relink_notified = ? WHERE user_id = ?`, hardcover.SyncScopes, userID); err != nil {
		log.Println("hardcover relink:", err)
	}
}

// joinAnd: "a", "a and b", "a, b and c".
func joinAnd(items []string) string {
	if n := len(items); n > 1 {
		return strings.Join(items[:n-1], ", ") + " and " + items[n-1]
	}
	return strings.Join(items, "")
}
