package hardcoverseries

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// ---------------------------------------------------------------------------
// The queue
// ---------------------------------------------------------------------------

// Job kinds.
const (
	JobCandidates = "candidates" // find the series on Hardcover
	JobLink       = "link"       // link to payload.hcId and fetch its list
	JobRefresh    = "refresh"    // fetch a linked series' list again
	JobBatch      = "batch"      // refresh, or link when there's one sure candidate
)

// Job is a row of hardcover_jobs (TS: HardcoverJob in web/types.ts).
type Job struct {
	ID         int64           `json:"id"`
	Kind       string          `json:"kind"`
	Series     string          `json:"series"`
	Batch      string          `json:"batch,omitempty"`
	State      string          `json:"state"`
	NotBefore  int64           `json:"notBefore,omitempty"`
	CreatedAt  int64           `json:"createdAt"`
	FinishedAt int64           `json:"finishedAt,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	payload    string
}

type Payload struct {
	HCID  int64   `json:"hcId,omitempty"`  // one series (queued before parts were)
	HCIDs []int64 `json:"hcIds,omitempty"` // a series, or several parts of one
}

func (p Payload) ids() []int64 {
	if len(p.HCIDs) > 0 {
		return p.HCIDs
	}
	if p.HCID > 0 {
		return []int64{p.HCID}
	}
	return nil
}

// Candidates is a candidates job's result.
type Candidates struct {
	Candidates []Candidate `json:"candidates"`
}

type Candidate struct {
	hardcover.Series
	Hits int    `json:"hits,omitempty"` // library ISBNs found in it
	URL  string `json:"url,omitempty"`
	// Group: candidates with the same name (folded) share it: Hardcover
	// splits some series into one per author.
	Group string `json:"group"`
	// Pick: in the group that looks like this series (ticked in the picker).
	Pick bool `json:"pick,omitempty"`
	// Sure: a batch links Pick's group on its own.
	Sure bool `json:"sure,omitempty"`
	// Evidence: what ties it to the library's series.
	Evidence string `json:"evidence,omitempty"`
}

// LinkResult is a link, refresh or batch job's result.
type LinkResult struct {
	Linked      bool        `json:"linked"`
	HCName      string      `json:"hcName,omitempty"`
	Books       int         `json:"books,omitempty"`
	Parts       int         `json:"parts,omitempty"`  // Hardcover series it is linked to
	Review      string      `json:"review,omitempty"` // the review page
	NeedsChoice bool        `json:"needsChoice,omitempty"`
	Candidates  []Candidate `json:"candidates,omitempty"`
}

const jobCols = `id, kind, series, batch, state, not_before, created_at, finished_at, result, error, payload`

func scanJob(sc interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var result string
	if err := sc.Scan(&j.ID, &j.Kind, &j.Series, &j.Batch, &j.State, &j.NotBefore, &j.CreatedAt, &j.FinishedAt, &result, &j.Error, &j.payload); err != nil {
		return nil, err
	}
	if result != "" {
		j.Result = json.RawMessage(result)
	}
	return &j, nil
}

func (s *Service) JobByID(id int64) (*Job, error) {
	return scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM hardcover_jobs WHERE id = ?`, id))
}

// Enqueue queues a job. A series already waiting for the same kind of
// job isn't queued twice: that job is answered.
func (s *Service) Enqueue(kind, series, batch string, p Payload, byUserID int64) (int64, error) {
	payload, _ := json.Marshal(p)
	var id int64
	err := s.db.QueryRow(`SELECT id FROM hardcover_jobs WHERE kind = ? AND series = ? AND payload = ? AND state IN ('queued', 'held', 'running') ORDER BY id LIMIT 1`,
		kind, series, string(payload)).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var uid any
	if byUserID > 0 {
		uid = byUserID
	}
	res, err := s.db.Exec(`INSERT INTO hardcover_jobs (kind, series, payload, batch, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		kind, series, string(payload), batch, uid, s.now().Unix())
	if err != nil {
		return 0, err
	}
	s.wakeUp()
	return res.LastInsertId()
}

func (s *Service) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// notify tells those waiting for the job that it has moved on.
func (s *Service) notify(id int64) {
	s.mu.Lock()
	for _, ch := range s.waiters[id] {
		close(ch)
	}
	delete(s.waiters, id)
	s.mu.Unlock()
}

// Await waits up to d for the job to finish or be held, and answers it.
func (s *Service) Await(ctx context.Context, id int64, d time.Duration) (*Job, error) {
	deadline := time.Now().Add(d)
	for {
		ch := make(chan struct{})
		s.mu.Lock()
		s.waiters[id] = append(s.waiters[id], ch)
		s.mu.Unlock()
		j, err := s.JobByID(id)
		if err != nil || j.State == "done" || j.State == "failed" || j.State == "held" || time.Now().After(deadline) {
			return j, err
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return j, nil
		case <-time.After(time.Until(deadline)):
			return s.JobByID(id)
		}
	}
}

func (s *Service) finish(j *Job, state string, result any, msg string) {
	var res []byte
	if result != nil {
		res, _ = json.Marshal(result)
	}
	if _, err := s.db.Exec(`UPDATE hardcover_jobs SET state = ?, result = ?, error = ?, finished_at = ? WHERE id = ?`,
		state, string(res), msg, s.now().Unix(), j.ID); err != nil {
		log.Println("hardcover queue:", err)
	}
	s.notify(j.ID)
}

// Step runs the oldest job that may run at now: editors' requests first,
// then Background jobs, one at most every Background.Spacing. ran: it ran
// one (or held it); next: when the next waiting one may run (zero: none
// waiting).
func (s *Service) Step(ctx context.Context) (ran bool, next time.Time) {
	now := s.now()
	s.mu.Lock()
	nextBackground := s.nextBackground
	s.mu.Unlock()
	backgroundMay := !now.Before(nextBackground)
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM hardcover_jobs
		WHERE state IN ('queued', 'held') AND not_before <= ? AND (kind != ? OR ?)
		ORDER BY kind = ?, id LIMIT 1`, now.Unix(), s.bgKind(), backgroundMay, s.bgKind()))
	if errors.Is(err, sql.ErrNoRows) {
		var nb sql.NullInt64
		_ = s.db.QueryRow(`SELECT MIN(not_before) FROM hardcover_jobs WHERE state = 'held'`).Scan(&nb)
		if !backgroundMay {
			var n int
			_ = s.db.QueryRow(`SELECT COUNT(*) FROM hardcover_jobs WHERE kind = ? AND state IN ('queued', 'held') AND not_before <= ?`,
				s.bgKind(), now.Unix()).Scan(&n)
			if n > 0 && (!nb.Valid || nextBackground.Unix() < nb.Int64) {
				return false, nextBackground
			}
		}
		if nb.Valid {
			return false, time.Unix(nb.Int64, 0)
		}
		return false, time.Time{}
	}
	if err != nil {
		log.Println("hardcover queue:", err)
		return false, now.Add(time.Minute)
	}
	if _, err := s.db.Exec(`UPDATE hardcover_jobs SET state = 'running' WHERE id = ?`, j.ID); err != nil {
		log.Println("hardcover queue:", err)
		return false, now.Add(time.Minute)
	}
	s.notify(j.ID)
	if j.Kind == s.bgKind() {
		s.mu.Lock()
		s.nextBackground = now.Add(s.bgSpacing())
		s.mu.Unlock()
	}
	result, err := s.run(ctx, j)
	defer s.SaveUsage()
	var wait *hardcover.WaitError
	var held *HeldError
	switch {
	case ctx.Err() != nil:
		_, _ = s.db.Exec(`UPDATE hardcover_jobs SET state = 'queued' WHERE id = ?`, j.ID)
	case errors.As(err, &held):
		// Held, with every job of its kind: editors' requests still run.
		_, _ = s.db.Exec(`UPDATE hardcover_jobs SET state = 'held', not_before = ?, error = ? WHERE kind = ? AND state IN ('queued', 'held') AND not_before < ? OR id = ?`,
			held.Until.Unix(), err.Error(), s.bgKind(), held.Until.Unix(), j.ID)
		s.notifyHeld()
	case errors.As(err, &wait):
		// Held, as is every job waiting behind it: none could run before.
		_, _ = s.db.Exec(`UPDATE hardcover_jobs SET state = 'held', not_before = ?, error = ? WHERE state IN ('queued', 'held') AND not_before < ? OR id = ?`,
			wait.Until.Unix(), wait.Error(), wait.Until.Unix(), j.ID)
		s.notifyHeld()
	case err != nil:
		s.finish(j, "failed", nil, err.Error())
	default:
		s.finish(j, "done", result, "")
	}
	return true, time.Time{}
}

// notifyHeld wakes everyone waiting: jobs were held.
func (s *Service) notifyHeld() {
	s.mu.Lock()
	for id, chs := range s.waiters {
		for _, ch := range chs {
			close(ch)
		}
		delete(s.waiters, id)
	}
	s.mu.Unlock()
}

// run does a job's requests.
func (s *Service) run(ctx context.Context, j *Job) (any, error) {
	if s.client() == nil {
		return nil, errors.New("Hardcover isn't set up on this server")
	}
	if s.bg != nil && j.Kind == s.bg.Kind {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return s.bg.Run(ctx, j.Series)
	}
	var p Payload
	_ = json.Unmarshal([]byte(j.payload), &p)
	si, ok := s.host.Series(j.Series)
	if !ok {
		return nil, fmt.Errorf("no series %q in the library", j.Series)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	switch j.Kind {
	case JobCandidates:
		cands, err := s.findCandidates(ctx, si)
		if err != nil {
			return nil, err
		}
		return Candidates{Candidates: cands}, nil
	case JobLink:
		return s.fetchAndLink(ctx, si, p.ids(), j)
	case JobRefresh, JobBatch:
		l, err := s.LinkFor(si.Name)
		if err != nil {
			return nil, err
		}
		if l != nil {
			return s.fetchAndLink(ctx, si, l.ids(), j)
		}
		if j.Kind == JobRefresh {
			return nil, errors.New("the series isn't linked to Hardcover")
		}
		cands, err := s.findCandidates(ctx, si)
		if err != nil {
			return nil, err
		}
		var sure []int64
		for _, c := range cands {
			if c.Sure {
				sure = append(sure, c.ID)
			}
		}
		if len(sure) == 0 {
			return LinkResult{NeedsChoice: true, Candidates: cands, Review: s.host.ReviewURL(si.Name)}, nil
		}
		return s.fetchAndLink(ctx, si, sure, j)
	}
	return nil, fmt.Errorf("unknown job %q", j.Kind)
}

func (s *Service) fetchAndLink(ctx context.Context, si SeriesInfo, ids []int64, j *Job) (any, error) {
	if len(ids) == 0 {
		return nil, errors.New("no Hardcover series given")
	}
	d, err := s.client().SeriesBooks(ctx, ids...)
	if err != nil {
		return nil, err
	}
	var uid sql.NullInt64
	_ = s.db.QueryRow(`SELECT created_by FROM hardcover_jobs WHERE id = ?`, j.ID).Scan(&uid)
	if err := s.SaveLink(si.Name, d, uid.Int64); err != nil {
		return nil, err
	}
	return LinkResult{Linked: true, HCName: d.Name, Books: len(d.Entries), Parts: len(d.AllParts()), Review: s.host.ReviewURL(si.Name)}, nil
}

// SaveUsage keeps the day's use of the plan for the next start.
func (s *Service) SaveUsage() {
	if s.client() == nil || s.db == nil {
		return
	}
	u := s.client().Budget.Usage(s.now())
	var paused int64
	if !u.PausedUntil.IsZero() {
		paused = u.PausedUntil.Unix()
	}
	if _, err := s.db.Exec(`INSERT INTO hardcover_usage (day, used, remote, paused_until) VALUES (?, ?, ?, ?)
		ON CONFLICT (day) DO UPDATE SET used = excluded.used, remote = excluded.remote, paused_until = excluded.paused_until`,
		u.Day, u.Used, u.Remote, paused); err != nil {
		log.Println("hardcover usage:", err)
	}
	_, _ = s.db.Exec(`DELETE FROM hardcover_usage WHERE day < ?`, s.now().UTC().AddDate(0, 0, -7).Format(time.DateOnly))
}

// RestoreUsage takes back the last saved use of the plan, and any pause,
// as after a restart (Start does it).
func (s *Service) RestoreUsage() {
	if s.client() == nil || s.db == nil {
		return
	}
	var u hardcover.Usage
	var paused int64
	err := s.db.QueryRow(`SELECT day, used, remote, paused_until FROM hardcover_usage ORDER BY day DESC LIMIT 1`).
		Scan(&u.Day, &u.Used, &u.Remote, &paused)
	if err != nil {
		return
	}
	if paused > 0 {
		u.PausedUntil = time.Unix(paused, 0)
	}
	s.client().Budget.Restore(s.now(), u)
}

// startHardcover takes up the queue left from the last run and starts the
// worker. Finished jobs are kept a week (for the batch lists).
func (s *Service) Start() {
	if s == nil || s.client() == nil {
		return
	}
	s.RestoreUsage()
	_, _ = s.db.Exec(`UPDATE hardcover_jobs SET state = 'queued' WHERE state = 'running'`)
	_, _ = s.db.Exec(`DELETE FROM hardcover_jobs WHERE state IN ('done', 'failed') AND finished_at < ?`, s.now().Add(-7*24*time.Hour).Unix())
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM hardcover_jobs WHERE state IN ('queued', 'held')`).Scan(&n)
	if n > 0 {
		log.Printf("hardcover: %d request(s) left from the last run", n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel, s.done = cancel, make(chan struct{})
	done := s.done
	s.mu.Unlock()
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				log.Printf("hardcover queue stopped: %v", p)
			}
		}()
		for {
			ran, next := s.Step(ctx)
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

// stopHardcover stops the worker; a job it was running is queued again.
func (s *Service) Stop(deadline time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel, done := s.cancel, s.done
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

// OpenJobs: the editors' jobs waiting or running (for one series, when
// given), oldest first, at most 200.
func (s *Service) OpenJobs(series string) ([]*Job, error) {
	if s == nil {
		return nil, nil
	}
	q := `SELECT ` + jobCols + ` FROM hardcover_jobs WHERE state IN ('queued', 'held', 'running') AND kind != ?`
	args := []any{s.bgKind()}
	if series != "" {
		q += ` AND series = ?`
		args = append(args, series)
	}
	rows, err := s.db.Query(q+` ORDER BY id LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Cancel cancels a job still waiting; false when it isn't waiting.
func (s *Service) Cancel(id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE hardcover_jobs SET state = 'failed', error = 'cancelled', finished_at = ? WHERE id = ? AND state IN ('queued', 'held')`,
		s.now().Unix(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.notify(id)
	}
	return n > 0, nil
}
