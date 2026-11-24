package main

// The app's side of Hardcover reading sync (internal/hardcoversync): the
// Service, made from the server's configuration, and hcHost, which gives it
// the app's books, placeholders, reading, starred authors, secrets and
// notifications.

import (
	"database/sql"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/internal/hardcoversync"
	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/isbn"
)

var (
	// noHardcoverSync (-no-hardcover-sync) turns off linking Hardcover
	// accounts, and sending to linked ones.
	noHardcoverSync bool
	// hcSyncClientID is CF_HARDCOVER_CLIENT_ID: the server's own OAuth app
	// on Hardcover ("": none).
	hcSyncClientID string
	// hcSync is reading sync; nil without the app database.
	hcSync *hardcoversync.Service
)

// configureHardcoverSync reads the server's own OAuth app on Hardcover
// (CF_HARDCOVER_CLIENT_ID). No client id ships with CalibreFast: a public
// client has no secret, so anyone could start sign-ins under a shared one.
// With one app per server, the approval screen on Hardcover names who
// actually runs the server, and only that admin answers for it. Without
// one, or with -no-hardcover(-sync), sync is off, but a user who linked
// before can still unlink (and so revoke) whenever there is a client id.
func configureHardcoverSync() {
	hcSyncClientID = strings.TrimSpace(os.Getenv("CF_HARDCOVER_CLIENT_ID"))
	var cfg hardcoversync.Config
	if hcSyncClientID != "" {
		cfg.Revoker = &hardcover.OAuth{ClientID: hcSyncClientID, UserAgent: lookupUserAgent}
		if !noHardcover && !noHardcoverSync {
			cfg.OAuth = &hardcover.OAuth{ClientID: hcSyncClientID, UserAgent: lookupUserAgent}
		}
	}
	hcSync = newHCSync(cfg)
}

// newHCSync makes the Service with the app's database and host; nil
// without the app database.
func newHCSync(cfg hardcoversync.Config) *hardcoversync.Service {
	if appDB == nil {
		return nil
	}
	cfg.DB, cfg.Host = appDB, hcHost{}
	if cfg.UserAgent == "" {
		cfg.UserAgent = lookupUserAgent
	}
	cfg.Now = func() time.Time { return hcNow() }
	cfg.Catalog = func() *hardcover.Client {
		if hardcoverEnabled() {
			return hcClient
		}
		return nil
	}
	return hardcoversync.New(cfg)
}

// hardcoverSyncEnabled: users may link Hardcover accounts.
func hardcoverSyncEnabled() bool { return hcSync.Enabled() }

// startHardcoverSync starts the worker. It needs no -allow-writes: it
// changes nothing in the library.
func startHardcoverSync() { hcSync.Start() }

// stopHardcoverSync stops the worker and any sign-ins in progress.
func stopHardcoverSync(deadline time.Time) { hcSync.Stop(deadline) }

// hcView is u's Hardcover section on the account page; nil when it's off
// on this server (admins get State "off", saying how to turn it on).
func hcView(u *User, notice string) *hardcoversync.AccountView {
	if v := hcSync.View(u.ID, notice); v != nil {
		return v
	}
	if u.IsAdmin && !noHardcover && !noHardcoverSync && hcSyncClientID == "" {
		return &hardcoversync.AccountView{State: "off", Notice: notice, Scopes: hardcover.SyncScopes}
	}
	if notice != "" {
		return &hardcoversync.AccountView{State: "gone", Notice: notice}
	}
	return nil
}

// hcHost is hardcoversync.Host.
type hcHost struct{}

func (hcHost) Syncable(uuid string) bool { return hcSyncable(uuid) }

func (hcHost) BookKeys(uuid string) ([]string, int64, string, error) { return hcBookKeys(uuid) }

func (hcHost) BookTitle(uuid string) (string, string, bool) { return hcBookTitle(uuid) }

func (hcHost) Placeholder(uuid string) (hcID int64, slug, title string, ok bool) {
	p := placeholderByUUID(uuid)
	if p == nil {
		return 0, "", "", false
	}
	return hcPlaceholderBookID(p), hcBookSlug(p.Meta), p.Meta.Title, true
}

func (hcHost) ClaimPlaceholder(uuid string, ed hardcover.Edition, userID int64) {
	hcClaimPlaceholder(uuid, ed, userID)
}

func (hcHost) ReadStatus(userID int64, uuid string) string {
	status, _ := readStatus(userID, uuid)
	return status
}

func (hcHost) Progress(userID int64, uuid string) (float64, time.Time, bool) {
	p, ok := latestProgress(userID, uuid)[uuid]
	return p.Percent, time.UnixMilli(p.UpdatedAt), ok
}

func (hcHost) Seal(context, plain string) (string, error)  { return sealSecret(context, plain) }
func (hcHost) Open(context, sealed string) (string, error) { return openSecret(context, sealed) }

func (hcHost) ReplaceRelinkNotice(tx *sql.Tx, userID int64, features string) error {
	if _, err := tx.Exec(`DELETE FROM notifications WHERE user_id = ? AND kind = ?`, userID, notifyHardcoverRelink); err != nil {
		return err
	}
	return addNotification(tx, userID, notifyHardcoverRelink, "", features, false, "", 1)
}

func (hcHost) ClearRelinkNotice(userID int64) error {
	_, err := appDB.Exec(`DELETE FROM notifications WHERE user_id = ? AND kind = ?`, userID, notifyHardcoverRelink)
	return err
}

func (hcHost) ResolveAuthor(name string) (string, []string) {
	g, members, _ := groups.resolve("author", name)
	return g, members
}

func (hcHost) LibraryAuthors() []hardcoversync.LibraryAuthor {
	all := getAggregateData(categoryByKey("author")).All
	out := make([]hardcoversync.LibraryAuthor, len(all))
	for i, it := range all {
		out[i] = hardcoversync.LibraryAuthor{Name: it.Name, Members: it.Members}
	}
	return out
}

func (hcHost) AuthorAsBrowsed(name string) (string, bool) {
	v, ok := aggValue(categoryByKey("author"), name)
	return v.Name, ok
}

func (hcHost) MaxFavorites() int { return maxAggFavorites }

func (hcHost) TouchFavorites(tx *sql.Tx, userID, nowMs int64) error {
	return touchFavs(tx, userID, nowMs)
}

func (hcHost) FavoritesChanged(userID int64) { bumpFavs(userID) }

// hcBookKeys: a library book's ISBN-13s, Hardcover book id and slug.
func hcBookKeys(uuid string) (isbns []string, hcID int64, slug string, err error) {
	if p := placeholderByUUID(uuid); p != nil {
		isbns, hcID, slug = hcPlaceholderKeys(p)
		return isbns, hcID, slug, nil
	}
	rows, err := calibreDB.Query(`SELECT i.type, i.val FROM identifiers i JOIN books b ON b.id = i.book
		WHERE b.uuid = ? AND lower(i.type) IN ('isbn', ?, ?)`, uuid, hardcover.IDScheme, hardcover.SlugScheme)
	if err != nil {
		return nil, 0, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var typ, val string
		if err := rows.Scan(&typ, &val); err != nil {
			return nil, 0, "", err
		}
		switch strings.ToLower(typ) {
		case "isbn":
			if n := isbn.Normalize(val); n != "" {
				isbns = append(isbns, n)
			}
		case hardcover.IDScheme:
			if id, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil && id > 0 {
				hcID = id
			}
		case hardcover.SlugScheme:
			if s := strings.TrimSpace(val); hardcover.ValidSlug(s) {
				slug = s
			}
		}
	}
	return isbns, hcID, slug, rows.Err()
}

// hcBookTitle: a library book's title and authors ("A & B"), from the
// catalog, or the library for a book added a moment ago.
func hcBookTitle(uuid string) (title, authors string, ok bool) {
	if b, ok := currentCatalog().lookup(uuid); ok {
		return b.Title, b.Authors, true
	}
	if p := placeholderByUUID(uuid); p != nil {
		return p.Meta.Title, p.authors(), true
	}
	err := calibreDB.QueryRow(`SELECT b.title, COALESCE((SELECT group_concat(a.name, ' & ') FROM books_authors_link l
		JOIN authors a ON a.id = l.author WHERE l.book = b.id), '') FROM books b WHERE b.uuid = ?`, uuid).Scan(&title, &authors)
	return title, authors, err == nil
}
