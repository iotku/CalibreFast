package hardcoversync

// Reading sync to users' own Hardcover accounts: a user who links their
// account (link.go) has their reading status, progress and dates
// sent there as they change here. Only one way: nothing on Hardcover is
// read back, apart from the user's entry for the book being sent, and
// nothing there is ever deleted.
//
// Changes go into hardcover_sync, one row per user and book, and a worker
// sends them. The row says only that the book is due: what's sent is the
// book's state when it's sent, so many changes (page turns) go as one, and
// their order can't come out wrong. Progress waits a few minutes for more
// (ProgressDelay); a status goes at once.
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
	"strconv"
	"sync"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

const (
	// ProgressDelay: progress waits this long for more before it's sent.
	ProgressDelay = 3 * time.Minute
	// refreshBefore: an access token this close to expiring is renewed.
	refreshBefore = 24 * time.Hour
	// foundTTL, missTTL: how long a book's place on Hardcover (or its
	// absence) is trusted before it's looked up again.
	foundTTL = 30 * 24 * time.Hour
	missTTL  = 7 * 24 * time.Hour
	// maxBackoff caps the wait after repeated failures.
	maxBackoff = 24 * time.Hour
)

// Enabled: users may link Hardcover accounts.
func (s *Service) Enabled() bool { return s != nil && s.oauth != nil }

// loadLinked reads which users have a working link.
func (s *Service) loadLinked() {
	linked := map[int64]bool{}
	if rows, err := s.db.Query(`SELECT user_id FROM hardcover_links WHERE broken = ''`); err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				linked[id] = true
			}
		}
		rows.Close()
	}
	s.mu.Lock()
	s.linked = linked
	s.clients = map[int64]*hardcover.Client{}
	s.mu.Unlock()
}

// IsLinked: userID has a working link.
func (s *Service) IsLinked(userID int64) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.linked[userID]
}

func (s *Service) setLinked(userID int64, on bool) {
	s.mu.Lock()
	if on {
		s.linked[userID] = true
	} else {
		delete(s.linked, userID)
	}
	delete(s.clients, userID)
	s.mu.Unlock()
}

// Wake has the worker look for work now.
func (s *Service) Wake() {
	if s == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

// account is a user's link, tokens still sealed.
type account struct {
	UserID, HCUserID int64
	Username         string
	access, refresh  string
	Expires          time.Time
	LinkedAt         time.Time
	Broken           string
}

func (s *Service) loadAccount(userID int64) (*account, error) {
	l := account{UserID: userID}
	var exp, at int64
	err := s.db.QueryRow(`SELECT hc_user_id, hc_username, access_token, refresh_token, access_expires, linked_at, broken
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

func sealContext(column string, userID int64) string {
	return "hardcover_links." + column + ":" + strconv.FormatInt(userID, 10)
}

// saveAccount stores a new sign-in for userID.
func (s *Service) saveAccount(userID int64, me hardcover.Me, t *hardcover.Token) error {
	acc, err := s.host.Seal(sealContext("access_token", userID), t.Access)
	if err != nil {
		return err
	}
	ref, err := s.host.Seal(sealContext("refresh_token", userID), t.Refresh)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO hardcover_links (user_id, hc_user_id, hc_username, access_token, refresh_token, access_expires, scope, linked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET hc_user_id = excluded.hc_user_id, hc_username = excluded.hc_username,
			access_token = excluded.access_token, refresh_token = excluded.refresh_token, access_expires = excluded.access_expires,
			scope = excluded.scope, linked_at = excluded.linked_at, broken = ''`,
		userID, me.ID, me.Username, acc, ref, t.Expires.Unix(), t.Scope, s.now().Unix())
	if err == nil {
		s.relinked(userID) // the "link again" notification, if any, is done with
	}
	return err
}

// saveTokens stores a renewed pair.
func (s *Service) saveTokens(userID int64, t *hardcover.Token) error {
	acc, err := s.host.Seal(sealContext("access_token", userID), t.Access)
	if err != nil {
		return err
	}
	ref, err := s.host.Seal(sealContext("refresh_token", userID), t.Refresh)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE hardcover_links SET access_token = ?, refresh_token = ?, access_expires = ? WHERE user_id = ?`,
		acc, ref, t.Expires.Unix(), userID)
	return err
}

// markBroken records that a user's link no longer works: nothing more is
// sent for them until they link again, and its tokens (refused, spent or
// unreadable) are deleted. The username and why stay, for the account page.
func (s *Service) markBroken(userID int64, why string) {
	if _, err := s.db.Exec(`UPDATE hardcover_links SET broken = ?, access_token = '', refresh_token = '', access_expires = 0 WHERE user_id = ?`,
		why, userID); err != nil {
		log.Println("hardcover sync:", err)
	}
	s.setLinked(userID, false)
	log.Printf("hardcover sync: user %d's link stopped working: %s", userID, why)
}

var ErrNotLinked = errors.New("Hardcover isn't linked")

// tokenFunc gives userID's access token, renewing it when it's about to
// expire or Hardcover refused it (force). A refresh token works once, so
// renewals are one at a time, and the new pair is stored before the new
// access token is used.
func (s *Service) tokenFunc(userID int64) func(ctx context.Context, force bool) (string, error) {
	return func(ctx context.Context, force bool) (string, error) {
		s.mu.Lock()
		mu := s.tokens[userID]
		if mu == nil {
			mu = &sync.Mutex{}
			s.tokens[userID] = mu
		}
		s.mu.Unlock()
		mu.Lock()
		defer mu.Unlock()
		l, err := s.loadAccount(userID)
		if err != nil {
			return "", err
		}
		if l == nil || l.Broken != "" {
			return "", ErrNotLinked
		}
		if !force && l.Expires.Sub(s.now()) > refreshBefore {
			acc, err := s.host.Open(sealContext("access_token", userID), l.access)
			if err == nil {
				return acc, nil
			}
		}
		if s.oauth == nil {
			return "", errors.New("Hardcover sync is off on this server")
		}
		ref, err := s.host.Open(sealContext("refresh_token", userID), l.refresh)
		if err != nil {
			s.markBroken(userID, "its sign-in can't be read on this server; link Hardcover again")
			return "", err
		}
		t, err := s.oauth.Refresh(ctx, ref)
		if errors.Is(err, hardcover.ErrInvalidGrant) {
			s.markBroken(userID, "Hardcover ended the sign-in (revoked, or unused for six months); link Hardcover again")
			return "", hardcover.ErrSignedOut
		}
		if err != nil {
			return "", err
		}
		if err := s.saveTokens(userID, t); err != nil {
			// The old refresh token is spent; without the new one the link
			// can't be renewed next time.
			s.markBroken(userID, "its renewed sign-in couldn't be saved; link Hardcover again")
			return "", err
		}
		return t.Access, nil
	}
}

// UserClient is the client that acts as userID on Hardcover.
func (s *Service) UserClient(userID int64) *hardcover.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.clients[userID]; c != nil {
		return c
	}
	c := hardcover.New(hardcover.Config{
		URL:       s.syncURL,
		TokenFunc: s.tokenFunc(userID),
		UserAgent: s.userAgent,
		Budget:    s.budget(),
		Now:       s.clock,
	})
	s.clients[userID] = c
	return c
}

// ---------------------------------------------------------------------------
// Changes: recorded as they're made
// ---------------------------------------------------------------------------

// Share is what a user chose to send to Hardcover. Nothing is sent
// about a book they haven't shared (not even to look it up there), unless
// they chose to share every book; progress and dates only if they chose.
type Share struct {
	All, Progress, Dates bool
}

func (s *Service) LoadShare(userID int64) Share {
	var sh Share
	if s == nil {
		return sh
	}
	_ = s.db.QueryRow(`SELECT share_all, share_progress, share_dates FROM hardcover_links WHERE user_id = ?`, userID).
		Scan(&sh.All, &sh.Progress, &sh.Dates)
	return sh
}

// SaveShare stores a user's choices. Going back from every book to
// chosen ones drops what was waiting to be sent about the others.
func (s *Service) SaveShare(userID int64, sh Share) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE hardcover_links SET share_all = ?, share_progress = ?, share_dates = ? WHERE user_id = ?`,
		sh.All, sh.Progress, sh.Dates, userID); err != nil {
		return err
	}
	if !sh.All {
		if _, err := tx.Exec(`DELETE FROM hardcover_sync WHERE user_id = ? AND book_uuid NOT IN
			(SELECT book_uuid FROM hardcover_shared WHERE user_id = ?)`, userID, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Shared: the user lets this book be sent: they shared it, or they
// share every book and didn't leave this one out (hardcover_unshared).
func (s *Service) Shared(userID int64, uuid string) bool {
	if s == nil {
		return false
	}
	var one int
	return s.db.QueryRow(`SELECT 1 FROM hardcover_links l WHERE l.user_id = ?1 AND (
		EXISTS (SELECT 1 FROM hardcover_shared s WHERE s.user_id = ?1 AND s.book_uuid = ?2)
		OR (l.share_all = 1 AND NOT EXISTS (SELECT 1 FROM hardcover_unshared x WHERE x.user_id = ?1 AND x.book_uuid = ?2)))`,
		userID, uuid).Scan(&one) == nil
}

// IsBookShared: the user chose this book (hardcover_shared), whether or
// not they share every book.
func (s *Service) IsBookShared(userID int64, uuid string) bool {
	var one int
	return s.db.QueryRow(`SELECT 1 FROM hardcover_shared WHERE user_id = ? AND book_uuid = ?`, userID, uuid).Scan(&one) == nil
}

// SetBookShared shares a book with Hardcover (it's sent now, as it
// stands) or stops sharing it (nothing more is sent; what was sent stays
// on Hardcover). For a user who shares every book, stopping leaves this
// one out of it (hardcover_unshared) until they share it again.
func (s *Service) SetBookShared(userID int64, uuid string, on bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if on {
		if _, err := tx.Exec(`DELETE FROM hardcover_unshared WHERE user_id = ? AND book_uuid = ?`, userID, uuid); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO hardcover_shared (user_id, book_uuid, shared_at) VALUES (?, ?, ?)`,
			userID, uuid, s.now().Unix()); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		s.Touch(userID, uuid, 0)
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM hardcover_shared WHERE user_id = ? AND book_uuid = ?`, userID, uuid); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO hardcover_unshared (user_id, book_uuid, unshared_at)
		SELECT user_id, ?, ? FROM hardcover_links WHERE user_id = ? AND share_all = 1`, uuid, s.now().Unix(), userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM hardcover_sync WHERE user_id = ? AND book_uuid = ?`, userID, uuid); err != nil {
		return err
	}
	return tx.Commit()
}

// Touch marks a user's book as due to be sent after delay (or
// sooner, if it was due sooner already). Nothing for users who haven't
// linked Hardcover, nor for books they haven't shared: not even a note.
func (s *Service) Touch(userID int64, uuid string, delay time.Duration) {
	s.mark(userID, uuid, "", delay)
}

// Status records a status change, with the date it starts or ends a
// read, and sends it at once.
func (s *Service) Status(userID int64, uuid, status string) {
	s.mark(userID, uuid, status, 0)
}

func (s *Service) mark(userID int64, uuid, status string, delay time.Duration) {
	if s == nil || !s.IsLinked(userID) || !s.Shared(userID, uuid) {
		return
	}
	now := s.now()
	today := now.Format(time.DateOnly)
	due := now.Add(delay).Unix()
	// A new read (reading, after nothing or a finished or dropped read)
	// starts today; finishing or dropping ends it today (only a finish's
	// date is sent). Pausing keeps the read going.
	_, err := s.db.Exec(`INSERT INTO hardcover_sync (user_id, book_uuid, due_at, started_on, finished_on)
		VALUES (?1, ?2, ?3, CASE WHEN ?4 = 'reading' THEN ?5 ELSE '' END, CASE WHEN ?4 IN ('finished', 'dropped') THEN ?5 ELSE '' END)
		ON CONFLICT (user_id, book_uuid) DO UPDATE SET
			due_at = CASE WHEN due_at > 0 AND due_at < excluded.due_at THEN due_at ELSE excluded.due_at END,
			attempts = 0,
			changes = changes + 1,
			started_on = CASE WHEN ?4 = 'reading' AND (started_on = '' OR finished_on != '') THEN ?5 ELSE started_on END,
			finished_on = CASE WHEN ?4 = 'reading' THEN '' WHEN ?4 IN ('finished', 'dropped') THEN ?5 ELSE finished_on END`,
		userID, uuid, due, status, today)
	if err != nil {
		log.Println("hardcover sync:", err)
		return
	}
	s.Wake()
}

// Backfill queues a user's books being read (or paused), finished
// or dropped that they share, for a first sync. How many.
func (s *Service) Backfill(userID int64) (int, error) {
	res, err := s.db.Exec(`INSERT INTO hardcover_sync (user_id, book_uuid, due_at, finished_on)
		SELECT user_id, book_uuid, ?1, CASE WHEN status = 'finished' THEN date(updated_at, 'unixepoch', 'localtime') ELSE '' END
		FROM reading_status r WHERE user_id = ?2 AND status IN ('reading', 'paused', 'finished', 'dropped')
			AND (book_uuid IN (SELECT book_uuid FROM hardcover_shared WHERE user_id = ?2)
				OR ((SELECT share_all FROM hardcover_links WHERE user_id = ?2) = 1
					AND book_uuid NOT IN (SELECT book_uuid FROM hardcover_unshared WHERE user_id = ?2)))
		ON CONFLICT (user_id, book_uuid) DO UPDATE SET due_at = excluded.due_at, attempts = 0, changes = changes + 1`,
		s.now().Unix(), userID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	s.Wake()
	return int(n), nil
}

// ---------------------------------------------------------------------------
// The worker
// ---------------------------------------------------------------------------

// skipError: the book can't be sent (not on Hardcover); no retry.
type skipError struct{ why string }

func (e *skipError) Error() string { return e.why }

// Step sends the book due longest. ran: it tried one; next: when
// the next is due (zero: none).
func (s *Service) Step(ctx context.Context) (ran bool, next time.Time) {
	now := s.now()
	var userID int64
	var uuid string
	var attempts, changes int
	err := s.db.QueryRow(`SELECT s.user_id, s.book_uuid, s.attempts, s.changes FROM hardcover_sync s
		JOIN hardcover_links l ON l.user_id = s.user_id AND l.broken = ''
		JOIN users u ON u.id = s.user_id AND u.deleted_at IS NULL
		WHERE s.due_at > 0 AND s.due_at <= ? ORDER BY s.due_at LIMIT 1`, now.Unix()).Scan(&userID, &uuid, &attempts, &changes)
	if errors.Is(err, sql.ErrNoRows) {
		var nb sql.NullInt64
		_ = s.db.QueryRow(`SELECT MIN(s.due_at) FROM hardcover_sync s
			JOIN hardcover_links l ON l.user_id = s.user_id AND l.broken = ''
			JOIN users u ON u.id = s.user_id AND u.deleted_at IS NULL WHERE s.due_at > 0`).Scan(&nb)
		if nb.Valid {
			return false, time.Unix(nb.Int64, 0)
		}
		return false, time.Time{}
	}
	if err != nil {
		log.Println("hardcover sync:", err)
		return false, now.Add(time.Minute)
	}
	done := s.working(userID)
	defer done()
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	err = s.pushBook(pctx, userID, uuid)
	cancel()
	var wait *hardcover.WaitError
	var skip *skipError
	switch {
	case ctx.Err() != nil:
		// Stopping: it stays due.
	case err == nil:
		// Unless it changed again while it was being sent.
		_, err = s.db.Exec(`UPDATE hardcover_sync SET due_at = 0, attempts = 0, error = '' WHERE user_id = ? AND book_uuid = ? AND changes = ?`,
			userID, uuid, changes)
	case errors.As(err, &wait):
		// This user's plan is used up for now: everything of theirs waits.
		_, err = s.db.Exec(`UPDATE hardcover_sync SET due_at = ? WHERE user_id = ? AND due_at > 0 AND due_at < ?`,
			wait.Until.Unix(), userID, wait.Until.Unix())
	case errors.As(err, &skip):
		log.Printf("hardcover sync: user %d, book %s: not sent: %s", userID, uuid, skip.why)
		_, err = s.db.Exec(`UPDATE hardcover_sync SET due_at = 0, attempts = 0, error = ? WHERE user_id = ? AND book_uuid = ?`,
			skip.why, userID, uuid)
	case errors.Is(err, ErrNotLinked) || errors.Is(err, hardcover.ErrSignedOut):
		// Marked broken; the rows wait for a new link.
		err = nil
	default:
		backoff := min(time.Minute*time.Duration(math.Pow(4, float64(min(attempts, 6)))), maxBackoff)
		msg := err.Error() // never holds a token (pkg/hardcover's errors don't)
		log.Printf("hardcover sync: user %d, book %s: %s; trying again in %s", userID, uuid, msg, backoff)
		_, err = s.db.Exec(`UPDATE hardcover_sync SET due_at = ?, attempts = attempts + 1, error = ? WHERE user_id = ? AND book_uuid = ?`,
			now.Add(backoff).Unix(), msg, userID, uuid)
	}
	if err != nil {
		log.Println("hardcover sync:", err)
	}
	return true, time.Time{}
}

// startHardcoverSync starts the worker. It needs no -allow-writes: it
// changes nothing in the library.
func (s *Service) Start() {
	if !s.Enabled() {
		return
	}
	s.NotifyRelink() // links older than what linking asks for now (relink.go)
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel, s.done = cancel, make(chan struct{})
	done := s.done
	s.mu.Unlock()
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				log.Printf("hardcover sync stopped: %v", p)
			}
		}()
		for {
			ran, next := s.Step(ctx)
			if !ran && ctx.Err() == nil {
				// Then follows (follow_push.go), when no book is due.
				var nextFollow time.Time
				ran, nextFollow = s.FollowStep(ctx)
				if next.IsZero() || (!nextFollow.IsZero() && nextFollow.Before(next)) {
					next = nextFollow
				}
			}
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
			case <-s.wake:
			case <-time.After(wait):
			}
		}
	}()
}

// stopHardcoverSync stops the worker and any sign-ins in progress.
func (s *Service) Stop(deadline time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	for _, p := range s.pending {
		p.cancel()
	}
	s.mu.Unlock()
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

var statusIDs = map[string]int{
	"wanted":   hardcover.StatusWantToRead,
	"paused":   hardcover.StatusPaused,
	"dropped":  hardcover.StatusDNF,
	"reading":  hardcover.StatusReading,
	"finished": hardcover.StatusRead,
}

// syncRow is what's kept of a user's book between sends.
type syncRow struct {
	StartedOn, FinishedOn string
	SentStatus, SentPages int
}

func (s *Service) loadSyncRow(userID int64, uuid string) syncRow {
	var r syncRow
	_ = s.db.QueryRow(`SELECT started_on, finished_on, sent_status, sent_pages FROM hardcover_sync WHERE user_id = ? AND book_uuid = ?`,
		userID, uuid).Scan(&r.StartedOn, &r.FinishedOn, &r.SentStatus, &r.SentPages)
	return r
}

// pushBook sends a user's book as it stands: its status and, while it's
// being read, how far along they are; and the dates the read started and
// finished. A status cleared here is left as it is on Hardcover.
func (s *Service) pushBook(ctx context.Context, userID int64, uuid string) error {
	if !s.host.Syncable(uuid) {
		_, err := s.db.Exec(`DELETE FROM hardcover_sync WHERE user_id = ? AND book_uuid = ?`, userID, uuid)
		return err
	}
	link, err := s.loadAccount(userID)
	if err != nil {
		return err
	}
	if link == nil || link.Broken != "" {
		return ErrNotLinked
	}
	if !s.Shared(userID, uuid) {
		// No longer shared: nothing is sent.
		_, err := s.db.Exec(`DELETE FROM hardcover_sync WHERE user_id = ? AND book_uuid = ?`, userID, uuid)
		return err
	}
	share := s.LoadShare(userID)
	status := s.host.ReadStatus(userID, uuid)
	want, ok := statusIDs[status]
	if !ok {
		return nil
	}
	c := s.UserClient(userID)
	ed, found := s.UserMatch(userID, uuid)
	if !found {
		if ed, found, err = s.resolveBook(ctx, c, uuid); err != nil {
			return err
		}
	}
	if !found {
		return &skipError{NotFoundMsg}
	}
	s.host.ClaimPlaceholder(uuid, ed, userID) // the first to find it names it for everyone
	row := s.loadSyncRow(userID, uuid)
	if !share.Dates {
		row.StartedOn, row.FinishedOn = "", ""
	} else {
		row.StartedOn, row.FinishedOn = s.readDates(userID, uuid, row)
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
		case hardcover.StatusReading, hardcover.StatusPaused, hardcover.StatusDNF:
			// How far along: where they are, or where they stopped.
			if ub.FinishedAt != "" {
				break // no read in progress
			}
			pages := ub.EditionPages
			if pages <= 0 {
				pages = ed.Pages
			}
			if percent, at, ok := s.host.Progress(userID, uuid); ok && pages > 1 && share.Progress {
				// Short of the last page: reaching it would finish the read
				// on Hardcover, and here a book is finished by its status.
				n := min(int(math.Round(percent*float64(pages))), pages-1)
				if n > 0 && n != ub.ProgressPages {
					u.ProgressPages, u.ActionAt = n, at
					sentPages = n
				}
			}
			if changed && want == hardcover.StatusReading && row.StartedOn != "" && row.StartedOn != ub.StartedAt {
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
	_, err = s.db.Exec(`UPDATE hardcover_sync SET sent_status = ?, sent_pages = ?, sent_at = ? WHERE user_id = ? AND book_uuid = ?`,
		want, sentPages, s.now().Unix(), userID, uuid)
	return err
}

// readDates: when the read started and finished, as recorded while the
// book was shared, else as the reading stats recorded them (stats.go).
func (s *Service) readDates(userID int64, uuid string, row syncRow) (started, finished string) {
	started, finished = row.StartedOn, row.FinishedOn
	day := func(kind string, after int64) (string, int64) {
		var at sql.NullInt64
		_ = s.db.QueryRow(`SELECT MAX(at) FROM reading_events WHERE user_id = ? AND book_uuid = ? AND kind = ? AND at >= ?`,
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

// resolveBook finds which Hardcover book and edition a library book is,
// from its ISBNs and Hardcover identifiers; kept, found or not, for a
// while. It asks with the server's own token when there is one, else with
// the user's.
func (s *Service) resolveBook(ctx context.Context, c *hardcover.Client, uuid string) (hardcover.Edition, bool, error) {
	var ed hardcover.Edition
	var at int64
	err := s.db.QueryRow(`SELECT hc_book_id, hc_edition_id, pages, slug, title, resolved_at FROM hardcover_books WHERE book_uuid = ?`, uuid).
		Scan(&ed.BookID, &ed.EditionID, &ed.Pages, &ed.Slug, &ed.Title, &at)
	if err == nil {
		ttl := foundTTL
		if ed.BookID == 0 {
			ttl = missTTL
		}
		if s.now().Sub(time.Unix(at, 0)) < ttl {
			return ed, ed.BookID > 0, nil
		}
	}
	isbns, hcID, slug, err := s.host.BookKeys(uuid)
	if err != nil {
		return ed, false, err
	}
	ed, found, err := s.CatalogClient(c).FindEdition(ctx, isbns, hcID, slug)
	if err != nil {
		return ed, false, err
	}
	if !found {
		ed = hardcover.Edition{}
	}
	_, err = s.db.Exec(`INSERT INTO hardcover_books (book_uuid, hc_book_id, hc_edition_id, pages, slug, title, resolved_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (book_uuid) DO UPDATE SET hc_book_id = excluded.hc_book_id, hc_edition_id = excluded.hc_edition_id,
			pages = excluded.pages, slug = excluded.slug, title = excluded.title, resolved_at = excluded.resolved_at`,
		uuid, ed.BookID, ed.EditionID, ed.Pages, ed.Slug, ed.Title, s.now().Unix())
	return ed, found, err
}

// SyncSummary is what the account page shows of a user's sync.
type SyncSummary struct {
	Waiting  int    `json:"waiting"`
	Failing  int    `json:"failing"`
	Skipped  int    `json:"skipped"` // not found on Hardcover
	LastSent string `json:"last_sent,omitempty"`
	LastErr  string `json:"last_error,omitempty"`
}

// Summary is what the account page shows of userID's sync.
func (s *Service) Summary(userID int64) SyncSummary {
	var sum SyncSummary
	var last int64
	_ = s.db.QueryRow(`SELECT
			COUNT(*) FILTER (WHERE due_at > 0),
			COUNT(*) FILTER (WHERE due_at > 0 AND attempts > 0),
			COUNT(*) FILTER (WHERE due_at = 0 AND error != ''),
			COALESCE(MAX(sent_at), 0)
		FROM hardcover_sync WHERE user_id = ?`, userID).Scan(&sum.Waiting, &sum.Failing, &sum.Skipped, &last)
	if last > 0 {
		sum.LastSent = formatDay(last)
	}
	_ = s.db.QueryRow(`SELECT error FROM hardcover_sync WHERE user_id = ? AND due_at > 0 AND attempts > 0 ORDER BY due_at LIMIT 1`, userID).Scan(&sum.LastErr)
	return sum
}

// Indicator is the compact state the top bar's sync menu shows.
type Indicator struct {
	State    string `json:"state"` // "linked" or "broken"
	Busy     bool   `json:"busy"`
	Waiting  int    `json:"waiting"`
	Failing  int    `json:"failing"`
	LastSent string `json:"last_sent,omitempty"`
}

// Indicator is userID's sync state for the top bar: nil unless sync is on
// and they have a link (working or broken).
func (s *Service) Indicator(userID int64) *Indicator {
	if !s.Enabled() {
		return nil
	}
	busy, _ := s.Activity(userID)
	if !s.IsLinked(userID) {
		var broken string
		if err := s.db.QueryRow(`SELECT broken FROM hardcover_links WHERE user_id = ?`, userID).Scan(&broken); err != nil || broken == "" {
			return nil
		}
		return &Indicator{State: "broken"}
	}
	sum := s.Summary(userID)
	return &Indicator{State: "linked", Busy: busy, Waiting: sum.Waiting, Failing: sum.Failing, LastSent: sum.LastSent}
}

// Merged queues a book others were merged into, for every linked
// user with a status for it: it may be a different book on Hardcover. A
// user who shared one of the others shares the book kept.
func (s *Service) Merged(keep string, others []string) {
	if s == nil {
		return
	}
	for _, o := range others {
		_, _ = s.db.Exec(`INSERT OR IGNORE INTO hardcover_shared (user_id, book_uuid, shared_at)
			SELECT user_id, ?, shared_at FROM hardcover_shared WHERE book_uuid = ?`, keep, o)
		// Left out of every book: the book kept is too, unless the user shared one of them.
		_, _ = s.db.Exec(`INSERT OR IGNORE INTO hardcover_unshared (user_id, book_uuid, unshared_at)
			SELECT x.user_id, ?1, x.unshared_at FROM hardcover_unshared x WHERE x.book_uuid = ?2
				AND NOT EXISTS (SELECT 1 FROM hardcover_shared s WHERE s.user_id = x.user_id AND s.book_uuid = ?1)`, keep, o)
		_, _ = s.db.Exec(`INSERT OR IGNORE INTO hardcover_user_books (user_id, book_uuid, hc_book_id, hc_edition_id, pages, slug, title, chosen_at)
			SELECT user_id, ?, hc_book_id, hc_edition_id, pages, slug, title, chosen_at FROM hardcover_user_books WHERE book_uuid = ?`, keep, o)
		_, _ = s.db.Exec(`DELETE FROM hardcover_user_books WHERE book_uuid = ?`, o)
	}
	rows, err := s.db.Query(`SELECT user_id FROM reading_status WHERE book_uuid = ?`, keep)
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
		s.Touch(id, keep, 0)
	}
}
