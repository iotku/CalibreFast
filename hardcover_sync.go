package main

// Reading sync to users' own Hardcover accounts: a user who links their
// account (hardcover_link.go) has their reading status, progress and dates
// sent there as they change here. Only one way: nothing on Hardcover is
// read back, apart from the user's entry for the book being sent, and
// nothing there is ever deleted.
//
// Changes go into hardcover_sync, one row per user and book, and a worker
// sends them. The row says only that the book is due: what's sent is the
// book's state when it's sent, so many changes (page turns) go as one, and
// their order can't come out wrong. Progress waits a few minutes for more
// (hcProgressDelay); a status goes at once.
//
// Each user's requests go with their own token, so within their own plan's
// limits (a Budget per user). A user over them, or whose sign-in Hardcover
// refuses, waits; nobody else does.

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/isbn"
)

// defaultHardcoverClientID is CalibreFast's OAuth app on Hardcover (a
// public "Mobile, desktop, or CLI" app with the device grant). The id is
// public by design: anyone can read it from the binary. Admins may use
// their own with CF_HARDCOVER_CLIENT_ID.
const defaultHardcoverClientID = ""

var (
	// noHardcoverSync (-no-hardcover-sync) turns off linking Hardcover
	// accounts, and sending to linked ones.
	noHardcoverSync bool
	// hcOAuth signs users in to Hardcover; nil when sync is off.
	hcOAuth *hardcover.OAuth
	// hcRevoker ends sign-ins when users unlink, even with sync turned off
	// (nil only with no client id at all): a user can always take back
	// the access they gave.
	hcRevoker *hardcover.OAuth
	// hcUserBudget is a user's plan limits (Hardcover's are per token).
	hcUserBudget = hardcover.FreePlan
	// hcSyncURL is the GraphQL endpoint for users' clients ("": Hardcover's;
	// tests set it).
	hcSyncURL string
)

const (
	// hcProgressDelay: progress waits this long for more before it's sent.
	hcProgressDelay = 3 * time.Minute
	// hcRefreshBefore: an access token this close to expiring is renewed.
	hcRefreshBefore = 24 * time.Hour
	// hcFoundTTL, hcMissTTL: how long a book's place on Hardcover (or its
	// absence) is trusted before it's looked up again.
	hcFoundTTL = 30 * 24 * time.Hour
	hcMissTTL  = 7 * 24 * time.Hour
	// hcMaxBackoff caps the wait after repeated failures.
	hcMaxBackoff = 24 * time.Hour
)

func configureHardcoverSync() {
	hcOAuth, hcRevoker = nil, nil
	id := strings.TrimSpace(os.Getenv("CF_HARDCOVER_CLIENT_ID"))
	if id == "" {
		id = defaultHardcoverClientID
	}
	if id != "" {
		hcRevoker = &hardcover.OAuth{ClientID: id, UserAgent: lookupUserAgent}
	}
	switch {
	case noHardcover || noHardcoverSync:
		return
	case id == "":
		return
	}
	hcOAuth = &hardcover.OAuth{ClientID: id, UserAgent: lookupUserAgent}
}

// hardcoverSyncEnabled: users may link Hardcover accounts.
func hardcoverSyncEnabled() bool { return hcOAuth != nil }

var hcSync = struct {
	sync.Mutex
	linked  map[int64]bool // users with a working link
	clients map[int64]*hardcover.Client
	tokens  map[int64]*sync.Mutex // one refresh at a time per user
	pending map[int64]*hcPendingLink
	wake    chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
}{
	linked:  map[int64]bool{},
	clients: map[int64]*hardcover.Client{},
	tokens:  map[int64]*sync.Mutex{},
	pending: map[int64]*hcPendingLink{},
	wake:    make(chan struct{}, 1),
}

// hcLoadLinked reads which users have a working link.
func hcLoadLinked() {
	linked := map[int64]bool{}
	if rows, err := appDB.Query(`SELECT user_id FROM hardcover_links WHERE broken = ''`); err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				linked[id] = true
			}
		}
		rows.Close()
	}
	hcSync.Lock()
	hcSync.linked = linked
	hcSync.clients = map[int64]*hardcover.Client{}
	hcSync.Unlock()
}

func hcIsLinked(userID int64) bool {
	hcSync.Lock()
	defer hcSync.Unlock()
	return hcSync.linked[userID]
}

func hcSetLinked(userID int64, on bool) {
	hcSync.Lock()
	if on {
		hcSync.linked[userID] = true
	} else {
		delete(hcSync.linked, userID)
	}
	delete(hcSync.clients, userID)
	hcSync.Unlock()
}

func hcSyncWake() {
	select {
	case hcSync.wake <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

// hcAccount is a user's link, tokens still sealed.
type hcAccount struct {
	UserID, HCUserID int64
	Username         string
	access, refresh  string
	Expires          time.Time
	LinkedAt         time.Time
	Broken           string
}

func hcLoadAccount(userID int64) (*hcAccount, error) {
	l := hcAccount{UserID: userID}
	var exp, at int64
	err := appDB.QueryRow(`SELECT hc_user_id, hc_username, access_token, refresh_token, access_expires, linked_at, broken
		FROM hardcover_links WHERE user_id = ?`, userID).
		Scan(&l.HCUserID, &l.Username, &l.access, &l.refresh, &exp, &at, &l.Broken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l.Expires, l.LinkedAt = time.Unix(exp, 0), time.Unix(at, 0)
	return &l, nil
}

func hcSealContext(column string, userID int64) string {
	return "hardcover_links." + column + ":" + strconv.FormatInt(userID, 10)
}

// hcSaveAccount stores a new sign-in for userID.
func hcSaveAccount(userID int64, me hardcover.Me, t *hardcover.Token) error {
	acc, err := sealSecret(hcSealContext("access_token", userID), t.Access)
	if err != nil {
		return err
	}
	ref, err := sealSecret(hcSealContext("refresh_token", userID), t.Refresh)
	if err != nil {
		return err
	}
	_, err = appDB.Exec(`INSERT INTO hardcover_links (user_id, hc_user_id, hc_username, access_token, refresh_token, access_expires, scope, linked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET hc_user_id = excluded.hc_user_id, hc_username = excluded.hc_username,
			access_token = excluded.access_token, refresh_token = excluded.refresh_token, access_expires = excluded.access_expires,
			scope = excluded.scope, linked_at = excluded.linked_at, broken = ''`,
		userID, me.ID, me.Username, acc, ref, t.Expires.Unix(), t.Scope, hcNow().Unix())
	return err
}

// hcSaveTokens stores a renewed pair.
func hcSaveTokens(userID int64, t *hardcover.Token) error {
	acc, err := sealSecret(hcSealContext("access_token", userID), t.Access)
	if err != nil {
		return err
	}
	ref, err := sealSecret(hcSealContext("refresh_token", userID), t.Refresh)
	if err != nil {
		return err
	}
	_, err = appDB.Exec(`UPDATE hardcover_links SET access_token = ?, refresh_token = ?, access_expires = ? WHERE user_id = ?`,
		acc, ref, t.Expires.Unix(), userID)
	return err
}

// hcMarkBroken records that a user's link no longer works: nothing more is
// sent for them until they link again, and its tokens (refused, spent or
// unreadable) are deleted. The username and why stay, for the account page.
func hcMarkBroken(userID int64, why string) {
	if _, err := appDB.Exec(`UPDATE hardcover_links SET broken = ?, access_token = '', refresh_token = '', access_expires = 0 WHERE user_id = ?`,
		why, userID); err != nil {
		log.Println("hardcover sync:", err)
	}
	hcSetLinked(userID, false)
	log.Printf("hardcover sync: user %d's link stopped working: %s", userID, why)
}

var errHCNotLinked = errors.New("Hardcover isn't linked")

// hcTokenFunc gives userID's access token, renewing it when it's about to
// expire or Hardcover refused it (force). A refresh token works once, so
// renewals are one at a time, and the new pair is stored before the new
// access token is used.
func hcTokenFunc(userID int64) func(ctx context.Context, force bool) (string, error) {
	return func(ctx context.Context, force bool) (string, error) {
		hcSync.Lock()
		mu := hcSync.tokens[userID]
		if mu == nil {
			mu = &sync.Mutex{}
			hcSync.tokens[userID] = mu
		}
		hcSync.Unlock()
		mu.Lock()
		defer mu.Unlock()
		l, err := hcLoadAccount(userID)
		if err != nil {
			return "", err
		}
		if l == nil || l.Broken != "" {
			return "", errHCNotLinked
		}
		if !force && l.Expires.Sub(hcNow()) > hcRefreshBefore {
			acc, err := openSecret(hcSealContext("access_token", userID), l.access)
			if err == nil {
				return acc, nil
			}
		}
		if hcOAuth == nil {
			return "", errors.New("Hardcover sync is off on this server")
		}
		ref, err := openSecret(hcSealContext("refresh_token", userID), l.refresh)
		if err != nil {
			hcMarkBroken(userID, "its sign-in can't be read on this server; link Hardcover again")
			return "", err
		}
		t, err := hcOAuth.Refresh(ctx, ref)
		if errors.Is(err, hardcover.ErrInvalidGrant) {
			hcMarkBroken(userID, "Hardcover ended the sign-in (revoked, or unused for six months); link Hardcover again")
			return "", hardcover.ErrSignedOut
		}
		if err != nil {
			return "", err
		}
		if err := hcSaveTokens(userID, t); err != nil {
			// The old refresh token is spent; without the new one the link
			// can't be renewed next time.
			hcMarkBroken(userID, "its renewed sign-in couldn't be saved; link Hardcover again")
			return "", err
		}
		return t.Access, nil
	}
}

// hcUserClient is the client that acts as userID on Hardcover.
func hcUserClient(userID int64) *hardcover.Client {
	hcSync.Lock()
	defer hcSync.Unlock()
	if c := hcSync.clients[userID]; c != nil {
		return c
	}
	c := hardcover.New(hardcover.Config{
		URL:       hcSyncURL,
		TokenFunc: hcTokenFunc(userID),
		UserAgent: lookupUserAgent,
		Budget:    hcUserBudget(),
		Now:       hcNow,
	})
	hcSync.clients[userID] = c
	return c
}

// ---------------------------------------------------------------------------
// Changes: recorded as they're made
// ---------------------------------------------------------------------------

// hcShare is what a user chose to send to Hardcover. Nothing is sent
// about a book they haven't shared (not even to look it up there), unless
// they chose to share every book; progress and dates only if they chose.
type hcShare struct {
	All, Progress, Dates bool
}

func hcLoadShare(userID int64) hcShare {
	var s hcShare
	_ = appDB.QueryRow(`SELECT share_all, share_progress, share_dates FROM hardcover_links WHERE user_id = ?`, userID).
		Scan(&s.All, &s.Progress, &s.Dates)
	return s
}

// hcSaveShare stores a user's choices. Going back from every book to
// chosen ones drops what was waiting to be sent about the others.
func hcSaveShare(userID int64, s hcShare) error {
	tx, err := appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE hardcover_links SET share_all = ?, share_progress = ?, share_dates = ? WHERE user_id = ?`,
		s.All, s.Progress, s.Dates, userID); err != nil {
		return err
	}
	if !s.All {
		if _, err := tx.Exec(`DELETE FROM hardcover_sync WHERE user_id = ? AND book_uuid NOT IN
			(SELECT book_uuid FROM hardcover_shared WHERE user_id = ?)`, userID, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// hcShared: the user lets this book be sent (shared it, or shares every book).
func hcShared(userID int64, uuid string) bool {
	var one int
	return appDB.QueryRow(`SELECT 1 FROM hardcover_links l WHERE l.user_id = ?1 AND (l.share_all = 1 OR
		EXISTS (SELECT 1 FROM hardcover_shared s WHERE s.user_id = ?1 AND s.book_uuid = ?2))`, userID, uuid).Scan(&one) == nil
}

func hcIsBookShared(userID int64, uuid string) bool {
	var one int
	return appDB.QueryRow(`SELECT 1 FROM hardcover_shared WHERE user_id = ? AND book_uuid = ?`, userID, uuid).Scan(&one) == nil
}

// hcSetBookShared shares a book with Hardcover (it's sent now, as it
// stands) or stops sharing it (nothing more is sent; what was sent stays
// on Hardcover).
func hcSetBookShared(userID int64, uuid string, on bool) error {
	if on {
		if _, err := appDB.Exec(`INSERT OR IGNORE INTO hardcover_shared (user_id, book_uuid, shared_at) VALUES (?, ?, ?)`,
			userID, uuid, hcNow().Unix()); err != nil {
			return err
		}
		hcSyncTouch(userID, uuid, 0)
		return nil
	}
	if _, err := appDB.Exec(`DELETE FROM hardcover_shared WHERE user_id = ? AND book_uuid = ?`, userID, uuid); err != nil {
		return err
	}
	_, err := appDB.Exec(`DELETE FROM hardcover_sync WHERE user_id = ? AND book_uuid = ?`, userID, uuid)
	return err
}

// hcSyncTouch marks a user's book as due to be sent after delay (or
// sooner, if it was due sooner already). Nothing for users who haven't
// linked Hardcover, nor for books they haven't shared: not even a note.
func hcSyncTouch(userID int64, uuid string, delay time.Duration) {
	hcSyncMark(userID, uuid, "", delay)
}

// hcSyncStatus records a status change, with the date it starts or ends a
// read, and sends it at once.
func hcSyncStatus(userID int64, uuid, status string) {
	hcSyncMark(userID, uuid, status, 0)
}

func hcSyncMark(userID int64, uuid, status string, delay time.Duration) {
	if appDB == nil || !hcIsLinked(userID) || !hcShared(userID, uuid) {
		return
	}
	now := hcNow()
	today := now.Format(time.DateOnly)
	due := now.Add(delay).Unix()
	// A new read (reading, after nothing or a finished read) starts today;
	// finishing ends it today.
	_, err := appDB.Exec(`INSERT INTO hardcover_sync (user_id, book_uuid, due_at, started_on, finished_on)
		VALUES (?1, ?2, ?3, CASE WHEN ?4 = 'reading' THEN ?5 ELSE '' END, CASE WHEN ?4 = 'finished' THEN ?5 ELSE '' END)
		ON CONFLICT (user_id, book_uuid) DO UPDATE SET
			due_at = CASE WHEN due_at > 0 AND due_at < excluded.due_at THEN due_at ELSE excluded.due_at END,
			attempts = 0,
			changes = changes + 1,
			started_on = CASE WHEN ?4 = 'reading' AND (started_on = '' OR finished_on != '') THEN ?5 ELSE started_on END,
			finished_on = CASE WHEN ?4 = 'reading' THEN '' WHEN ?4 = 'finished' THEN ?5 ELSE finished_on END`,
		userID, uuid, due, status, today)
	if err != nil {
		log.Println("hardcover sync:", err)
		return
	}
	hcSyncWake()
}

// hcSyncBackfill queues a user's books being read and finished that they
// share, for a first sync. How many.
func hcSyncBackfill(userID int64) (int, error) {
	res, err := appDB.Exec(`INSERT INTO hardcover_sync (user_id, book_uuid, due_at, finished_on)
		SELECT user_id, book_uuid, ?1, CASE WHEN status = 'finished' THEN date(updated_at, 'unixepoch', 'localtime') ELSE '' END
		FROM reading_status r WHERE user_id = ?2 AND status IN ('reading', 'finished')
			AND ((SELECT share_all FROM hardcover_links WHERE user_id = ?2) = 1
				OR book_uuid IN (SELECT book_uuid FROM hardcover_shared WHERE user_id = ?2))
		ON CONFLICT (user_id, book_uuid) DO UPDATE SET due_at = excluded.due_at, attempts = 0, changes = changes + 1`,
		hcNow().Unix(), userID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	hcSyncWake()
	return int(n), nil
}

// ---------------------------------------------------------------------------
// The worker
// ---------------------------------------------------------------------------

// hcSkip: the book can't be sent (not on Hardcover); no retry.
type hcSkip struct{ why string }

func (e *hcSkip) Error() string { return e.why }

// hcSyncStep sends the book due longest. ran: it tried one; next: when
// the next is due (zero: none).
func hcSyncStep(ctx context.Context) (ran bool, next time.Time) {
	now := hcNow()
	var userID int64
	var uuid string
	var attempts, changes int
	err := appDB.QueryRow(`SELECT s.user_id, s.book_uuid, s.attempts, s.changes FROM hardcover_sync s
		JOIN hardcover_links l ON l.user_id = s.user_id AND l.broken = ''
		WHERE s.due_at > 0 AND s.due_at <= ? ORDER BY s.due_at LIMIT 1`, now.Unix()).Scan(&userID, &uuid, &attempts, &changes)
	if errors.Is(err, sql.ErrNoRows) {
		var nb sql.NullInt64
		_ = appDB.QueryRow(`SELECT MIN(s.due_at) FROM hardcover_sync s
			JOIN hardcover_links l ON l.user_id = s.user_id AND l.broken = '' WHERE s.due_at > 0`).Scan(&nb)
		if nb.Valid {
			return false, time.Unix(nb.Int64, 0)
		}
		return false, time.Time{}
	}
	if err != nil {
		log.Println("hardcover sync:", err)
		return false, now.Add(time.Minute)
	}
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	err = hcPushBook(pctx, userID, uuid)
	cancel()
	var wait *hardcover.WaitError
	var skip *hcSkip
	switch {
	case ctx.Err() != nil:
		// Stopping: it stays due.
	case err == nil:
		// Unless it changed again while it was being sent.
		_, err = appDB.Exec(`UPDATE hardcover_sync SET due_at = 0, attempts = 0, error = '' WHERE user_id = ? AND book_uuid = ? AND changes = ?`,
			userID, uuid, changes)
	case errors.As(err, &wait):
		// This user's plan is used up for now: everything of theirs waits.
		_, err = appDB.Exec(`UPDATE hardcover_sync SET due_at = ? WHERE user_id = ? AND due_at > 0 AND due_at < ?`,
			wait.Until.Unix(), userID, wait.Until.Unix())
	case errors.As(err, &skip):
		_, err = appDB.Exec(`UPDATE hardcover_sync SET due_at = 0, attempts = 0, error = ? WHERE user_id = ? AND book_uuid = ?`,
			skip.why, userID, uuid)
	case errors.Is(err, errHCNotLinked) || errors.Is(err, hardcover.ErrSignedOut):
		// Marked broken; the rows wait for a new link.
		err = nil
	default:
		backoff := min(time.Minute*time.Duration(math.Pow(4, float64(min(attempts, 6)))), hcMaxBackoff)
		msg := err.Error()
		_, err = appDB.Exec(`UPDATE hardcover_sync SET due_at = ?, attempts = attempts + 1, error = ? WHERE user_id = ? AND book_uuid = ?`,
			now.Add(backoff).Unix(), msg, userID, uuid)
	}
	if err != nil {
		log.Println("hardcover sync:", err)
	}
	return true, time.Time{}
}

// startHardcoverSync starts the worker. It needs no -allow-writes: it
// changes nothing in the library.
func startHardcoverSync() {
	if appDB == nil {
		return
	}
	hcLoadLinked()
	if !hardcoverSyncEnabled() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	hcSync.Lock()
	hcSync.cancel, hcSync.done = cancel, make(chan struct{})
	done := hcSync.done
	hcSync.Unlock()
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				log.Printf("hardcover sync stopped: %v", p)
			}
		}()
		for {
			ran, next := hcSyncStep(ctx)
			if ctx.Err() != nil {
				return
			}
			if ran {
				continue
			}
			wait := time.Hour
			if !next.IsZero() {
				wait = max(time.Until(next), 100*time.Millisecond)
			}
			select {
			case <-ctx.Done():
				return
			case <-hcSync.wake:
			case <-time.After(wait):
			}
		}
	}()
}

// stopHardcoverSync stops the worker and any sign-ins in progress.
func stopHardcoverSync(deadline time.Time) {
	hcSync.Lock()
	cancel, done := hcSync.cancel, hcSync.done
	for _, p := range hcSync.pending {
		p.cancel()
	}
	hcSync.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
	}
}

// ---------------------------------------------------------------------------
// Sending a book
// ---------------------------------------------------------------------------

var hcStatusIDs = map[string]int{
	"wanted":   hardcover.StatusWantToRead,
	"reading":  hardcover.StatusReading,
	"finished": hardcover.StatusRead,
}

// hcSyncRow is what's kept of a user's book between sends.
type hcSyncRow struct {
	StartedOn, FinishedOn string
	SentStatus, SentPages int
}

func hcLoadSyncRow(userID int64, uuid string) hcSyncRow {
	var r hcSyncRow
	_ = appDB.QueryRow(`SELECT started_on, finished_on, sent_status, sent_pages FROM hardcover_sync WHERE user_id = ? AND book_uuid = ?`,
		userID, uuid).Scan(&r.StartedOn, &r.FinishedOn, &r.SentStatus, &r.SentPages)
	return r
}

// hcPushBook sends a user's book as it stands: its status and, while it's
// being read, how far along they are; and the dates the read started and
// finished. A status cleared here is left as it is on Hardcover.
func hcPushBook(ctx context.Context, userID int64, uuid string) error {
	if !bookExists(uuid) {
		_, err := appDB.Exec(`DELETE FROM hardcover_sync WHERE user_id = ? AND book_uuid = ?`, userID, uuid)
		return err
	}
	link, err := hcLoadAccount(userID)
	if err != nil {
		return err
	}
	if link == nil || link.Broken != "" {
		return errHCNotLinked
	}
	if !hcShared(userID, uuid) {
		// No longer shared: nothing is sent.
		_, err := appDB.Exec(`DELETE FROM hardcover_sync WHERE user_id = ? AND book_uuid = ?`, userID, uuid)
		return err
	}
	share := hcLoadShare(userID)
	status, _ := readStatus(userID, uuid)
	want, ok := hcStatusIDs[status]
	if !ok {
		return nil
	}
	c := hcUserClient(userID)
	ed, found, err := hcResolveBook(ctx, c, uuid)
	if err != nil {
		return err
	}
	if !found {
		return &hcSkip{"not found on Hardcover (no ISBN or Hardcover identifier it knows)"}
	}
	row := hcLoadSyncRow(userID, uuid)
	if !share.Dates {
		row.StartedOn, row.FinishedOn = "", ""
	} else {
		row.StartedOn, row.FinishedOn = hcReadDates(userID, uuid, row)
	}

	ub, err := c.UserBook(ctx, link.HCUserID, ed.BookID)
	if err != nil {
		return err
	}
	changed := false
	switch {
	case ub == nil:
		if _, err := c.AddUserBook(ctx, ed.BookID, want, ed.EditionID); err != nil {
			return err
		}
		changed = true
	case ub.StatusID != want:
		edition := int64(0)
		if ub.EditionID == 0 {
			edition = ed.EditionID
		}
		if err := c.SetUserBook(ctx, ub.ID, want, edition); err != nil {
			return err
		}
		changed = true
	}
	if changed && want != hardcover.StatusWantToRead {
		// Hardcover started or finished a read: see it.
		if ub, err = c.UserBook(ctx, link.HCUserID, ed.BookID); err != nil {
			return err
		}
	}

	sentPages := row.SentPages
	if ub != nil && ub.ReadID > 0 {
		var u hardcover.ReadUpdate
		switch want {
		case hardcover.StatusReading:
			if ub.FinishedAt != "" {
				break // no read in progress
			}
			pages := ub.EditionPages
			if pages <= 0 {
				pages = ed.Pages
			}
			if p, ok := latestProgress(userID, uuid)[uuid]; ok && pages > 1 && share.Progress {
				// Short of the last page: reaching it would finish the read
				// on Hardcover, and here a book is finished by its status.
				n := min(int(math.Round(p.Percent*float64(pages))), pages-1)
				if n > 0 && n != ub.ProgressPages {
					u.ProgressPages, u.ActionAt = n, time.UnixMilli(p.UpdatedAt)
					sentPages = n
				}
			}
			if changed && row.StartedOn != "" && row.StartedOn != ub.StartedAt {
				u.StartedAt = row.StartedOn
			}
		case hardcover.StatusRead:
			if changed && row.FinishedOn != "" && ub.FinishedAt != "" && row.FinishedOn != ub.FinishedAt {
				u.FinishedAt = row.FinishedOn
			}
			if changed && row.StartedOn != "" && row.StartedOn != ub.StartedAt && row.StartedOn <= max(row.FinishedOn, ub.FinishedAt) {
				u.StartedAt = row.StartedOn
			}
		}
		if err := c.UpdateRead(ctx, ub.ReadID, u); err != nil {
			return err
		}
	}
	_, err = appDB.Exec(`UPDATE hardcover_sync SET sent_status = ?, sent_pages = ?, sent_at = ? WHERE user_id = ? AND book_uuid = ?`,
		want, sentPages, hcNow().Unix(), userID, uuid)
	return err
}

// hcReadDates: when the read started and finished, as recorded while the
// book was shared, else as the reading stats recorded them (stats.go).
func hcReadDates(userID int64, uuid string, row hcSyncRow) (started, finished string) {
	started, finished = row.StartedOn, row.FinishedOn
	day := func(kind string, after int64) (string, int64) {
		var at sql.NullInt64
		_ = appDB.QueryRow(`SELECT MAX(at) FROM reading_events WHERE user_id = ? AND book_uuid = ? AND kind = ? AND at >= ?`,
			userID, uuid, kind, after).Scan(&at)
		if !at.Valid {
			return "", 0
		}
		return time.Unix(at.Int64, 0).Format(time.DateOnly), at.Int64
	}
	if started == "" {
		var at int64
		if started, at = day("started", 0); started != "" && finished == "" {
			finished, _ = day("finished", at)
		}
	}
	return started, finished
}

// hcResolveBook finds which Hardcover book and edition a library book is,
// from its ISBNs and Hardcover identifiers; kept, found or not, for a
// while. It asks with the server's own token when there is one, else with
// the user's.
func hcResolveBook(ctx context.Context, c *hardcover.Client, uuid string) (hardcover.Edition, bool, error) {
	var ed hardcover.Edition
	var at int64
	err := appDB.QueryRow(`SELECT hc_book_id, hc_edition_id, pages, resolved_at FROM hardcover_books WHERE book_uuid = ?`, uuid).
		Scan(&ed.BookID, &ed.EditionID, &ed.Pages, &at)
	if err == nil {
		ttl := hcFoundTTL
		if ed.BookID == 0 {
			ttl = hcMissTTL
		}
		if hcNow().Sub(time.Unix(at, 0)) < ttl {
			return ed, ed.BookID > 0, nil
		}
	}
	isbns, hcID, slug, err := hcBookKeys(uuid)
	if err != nil {
		return ed, false, err
	}
	if hardcoverEnabled() {
		c = hcClient
	}
	ed, found, err := c.FindEdition(ctx, isbns, hcID, slug)
	if err != nil {
		return ed, false, err
	}
	if !found {
		ed = hardcover.Edition{}
	}
	_, err = appDB.Exec(`INSERT INTO hardcover_books (book_uuid, hc_book_id, hc_edition_id, pages, resolved_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (book_uuid) DO UPDATE SET hc_book_id = excluded.hc_book_id, hc_edition_id = excluded.hc_edition_id,
			pages = excluded.pages, resolved_at = excluded.resolved_at`,
		uuid, ed.BookID, ed.EditionID, ed.Pages, hcNow().Unix())
	return ed, found, err
}

// hcBookKeys: a library book's ISBN-13s, Hardcover book id and slug.
func hcBookKeys(uuid string) (isbns []string, hcID int64, slug string, err error) {
	rows, err := calibreDB.Query(`SELECT i.type, i.val FROM identifiers i JOIN books b ON b.id = i.book
		WHERE b.uuid = ? AND lower(i.type) IN ('isbn', ?, ?)`, uuid, hcIdentifierScheme, hcSlugScheme)
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
		case hcIdentifierScheme:
			if id, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil && id > 0 {
				hcID = id
			}
		case hcSlugScheme:
			if s := strings.TrimSpace(val); hardcover.ValidSlug(s) {
				slug = s
			}
		}
	}
	return isbns, hcID, slug, rows.Err()
}

// hcSyncSummary is what the account page shows of a user's sync.
type hcSyncSummary struct {
	Waiting  int    `json:"waiting"`
	Failing  int    `json:"failing"`
	Skipped  int    `json:"skipped"` // not found on Hardcover
	LastSent string `json:"last_sent,omitempty"`
	LastErr  string `json:"last_error,omitempty"`
}

func hcSummary(userID int64) hcSyncSummary {
	var s hcSyncSummary
	var last int64
	_ = appDB.QueryRow(`SELECT
			COUNT(*) FILTER (WHERE due_at > 0),
			COUNT(*) FILTER (WHERE due_at > 0 AND attempts > 0),
			COUNT(*) FILTER (WHERE due_at = 0 AND error != ''),
			COALESCE(MAX(sent_at), 0)
		FROM hardcover_sync WHERE user_id = ?`, userID).Scan(&s.Waiting, &s.Failing, &s.Skipped, &last)
	if last > 0 {
		s.LastSent = formatDay(last)
	}
	_ = appDB.QueryRow(`SELECT error FROM hardcover_sync WHERE user_id = ? AND due_at > 0 AND attempts > 0 ORDER BY due_at LIMIT 1`, userID).Scan(&s.LastErr)
	return s
}

// hcSyncMerged queues a book others were merged into, for every linked
// user with a status for it: it may be a different book on Hardcover. A
// user who shared one of the others shares the book kept.
func hcSyncMerged(keep string, others []string) {
	for _, o := range others {
		_, _ = appDB.Exec(`INSERT OR IGNORE INTO hardcover_shared (user_id, book_uuid, shared_at)
			SELECT user_id, ?, shared_at FROM hardcover_shared WHERE book_uuid = ?`, keep, o)
	}
	rows, err := appDB.Query(`SELECT user_id FROM reading_status WHERE book_uuid = ?`, keep)
	if err != nil {
		return
	}
	var users []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			users = append(users, id)
		}
	}
	rows.Close()
	for _, id := range users {
		hcSyncTouch(id, keep, 0)
	}
}
