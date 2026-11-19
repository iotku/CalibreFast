package main

// The bulk editor's write queue. A library on slow storage can take seconds
// to write a change to (each book's metadata.opf, metadata.db), and the
// editor shouldn't wait for it. So POST /api/library/bulk only checks the
// change, saves it to calibrefast.db and answers with the rows as they will
// be, foreseen from metadata.db; one worker then makes the changes in the
// order they came, a book at a time, as bulkEditBooksAs does.
//
// Safety comes first: a change that can't be made safely fails, and says
// so, rather than writing over something.
//
//   - Saved before answering. The change and its books (bulk_edit_queue,
//     bulk_edit_books) are committed to calibrefast.db before the 202; if
//     that fails, the editor is told and nothing is made.
//   - Written against the book as it was when queued. Each book records its
//     last_modified then (its base). The write checks it in the same transaction as the write itself
//     (updateBookIfUnchanged), so a book changed meanwhile (Calibre, another
//     server, anything) isn't written over: that book fails. A book with an
//     earlier queued change is written against the last_modified that change
//     leaves (or, if it failed, against the same base, so it fails too when
//     the book was changed elsewhere).
//   - Each book's outcome is committed as it's made, with the base of the
//     next change to that book, so after a stop or crash only books without
//     one are made. A book written just before a crash, its outcome not yet
//     saved, then fails as changed elsewhere (its last_modified is the new
//     one): never written twice over a change.
//   - Failures are kept (bulk_edit_failures) until an editor has seen them
//     in the bulk editor, whether or not the page that made them is open.
//   - A panic making a book is that book's failure (bulkEditBook).
//   - On a normal stop the server gives the queue up to shutdownTimeout to
//     empty, then finishes the book being written and stops; the rest is
//     made when it starts again, with the same checks. Requests waiting for
//     the queue meanwhile are refused (503) rather than run ahead of it.
//
// Until a book's queued changes are all made, the table (GET
// /api/library/table) shows it as they will leave it. GET
// /api/library/bulk/jobs says how far each change has got, with the rows
// as written (and any book that couldn't be changed) once it's done.
//
// Other changes to the library (the metadata editor, merges, removing
// books, undo) wait for the queue first (afterBulkEdits, afterBookEdits),
// so they start from the books as the queued changes leave them.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iotku/CalibreFast/pkg/opf"
)

// queuedEdit is a change waiting in the queue.
type queuedEdit struct {
	id      int64
	uuids   []string
	base    []string // per book: the last_modified it's written against; "" until the change before it to the book is made
	outcome []string // per book: "" until made, then "changed", "unchanged" or "failed"
	edit    bulkEdit
	by      *User
}

func (j *queuedEdit) made() int {
	n := 0
	for _, o := range j.outcome {
		if o != "" {
			n++
		}
	}
	return n
}

// bulkJob is how far a queued change has got (TS: BulkJob in web/types.ts).
type bulkJob struct {
	State  string      `json:"state"` // "queued", "running", "done"; "unknown" for an id the server doesn't know
	Done   int         `json:"done"`  // books gone through
	Total  int         `json:"total"`
	Result *bulkResult `json:"result,omitempty"` // when done: what was written
}

// pendingRow is a book with queued changes: its row as they'll leave it.
type pendingRow struct {
	row  tableBook
	jobs int // queued changes still to make to it
	// next: when the last of them has been made to it (its change not yet
	// finished with every book), the last_modified it left: the base of a
	// change queued now.
	next string
}

// bulkJobsKept: how many finished changes the server remembers for the
// editors to ask about.
const bulkJobsKept = 500

var errBulkStopping = errors.New("the server is stopping; try again when it's back")

var bulkQueue = struct {
	mu       sync.Mutex
	changed  *sync.Cond // the queue or a change's progress changed
	waiting  []*queuedEdit
	jobs     map[int64]*bulkJob
	finished []int64 // oldest first, to forget
	rows     map[string]*pendingRow
	lastID   int64 // ids when there's no app database (tests)
	worker   bool  // the worker goroutine is running
	stopping bool  // the server is stopping: start no more books
}{jobs: map[int64]*bulkJob{}, rows: map[string]*pendingRow{}}

func init() { bulkQueue.changed = sync.NewCond(&bulkQueue.mu) }

// predictRow is a row as the edit will leave it, and whether it changes.
// It's foreseen from metadata.db's values; the write starts from the
// book's metadata.opf, so the result may differ slightly (the job's result
// has the rows as written).
func predictRow(b tableBook, e *bulkEdit) (tableBook, bool) {
	m := OPFMetadata{
		Title: b.Title, Creators: splitAuthors(b.Authors), Publisher: b.Publisher, Series: b.Series,
		Subjects: slices.Clone(b.Tags), Languages: slices.Clone(b.Languages),
	}
	if b.Series != "" {
		m.SeriesIndex = strconv.FormatFloat(b.SeriesIndex, 'f', -1, 64)
	}
	if b.PubDate != "" {
		m.Date = normalizeDate(b.PubDate)
	}
	e.apply(&m)
	out := b
	out.Title, out.Authors, out.Publisher = m.Title, strings.Join(m.Creators, " & "), m.Publisher
	out.PubDate = tableDate(m.Date)
	out.Series, out.SeriesIndex = m.Series, 1
	if i, err := strconv.ParseFloat(m.SeriesIndex, 64); err == nil && m.Series != "" {
		out.SeriesIndex = i
	}
	out.Tags = slices.Clone(m.Subjects)
	out.Languages = slices.DeleteFunc(slices.Clone(m.Languages), func(l string) bool { return l == opf.UndefinedLanguage })
	before, _ := json.Marshal(b)
	after, _ := json.Marshal(out)
	return out, string(before) != string(after)
}

// queueBulkEdit queues the edit of these books and answers the result it
// foresees: the rows of the books it will change, and the books that
// aren't in the library.
func queueBulkEdit(uuids []string, edit *bulkEdit, by *User) (bulkResult, error) {
	res := bulkResult{Failed: []bulkFailure{}, Books: []tableBook{}}
	q := &bulkQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopping {
		return res, errBulkStopping
	}

	// A book with queued changes starts from its row as they'll leave it;
	// any other from metadata.db (no change to it is being made: the lock
	// keeps the worker from finishing one meanwhile).
	rows := make(map[string]tableBook, len(uuids))
	var read []string
	for _, u := range uuids {
		if p := q.rows[u]; p != nil {
			rows[u] = p.row
		} else {
			read = append(read, u)
		}
	}
	fresh := map[string]bool{}
	for chunk := range slices.Chunk(read, 500) {
		got, err := tableBooks(calibreDB, chunk)
		if err != nil {
			return res, err
		}
		for _, b := range got {
			rows[b.UUID] = b
			fresh[b.UUID] = true
		}
	}
	job := &queuedEdit{edit: *edit, by: by}
	next := map[string]tableBook{}
	for _, u := range uuids {
		b, ok := rows[u]
		if !ok {
			res.Failed = append(res.Failed, bulkFailure{u, errNoSuchBook.Error() + ": " + u})
			continue
		}
		// Every book is queued, even one this doesn't foresee a change to:
		// the change is made to its metadata.opf, which may not match.
		job.uuids = append(job.uuids, u)
		base := "" // set when the change before it to this book is made
		if fresh[u] {
			base = b.modified
			if base == "" {
				base = "-" // no last_modified to check: it fails rather than writes unchecked
			}
		} else if p := q.rows[u]; p.next != "" {
			base = p.next // that change has been made to it already
		}
		job.base = append(job.base, base)
		if now, changed := predictRow(b, edit); changed {
			next[u] = now
			res.Books = append(res.Books, now)
		} else {
			res.Unchanged++
		}
	}
	res.Changed = len(res.Books)
	if len(job.uuids) == 0 {
		return res, nil
	}
	job.outcome = make([]string, len(job.uuids))

	id, err := saveQueuedEdit(job)
	if err != nil {
		return res, err
	}
	job.id, res.Job = id, id
	for _, u := range job.uuids {
		p := q.rows[u]
		if p == nil {
			p = &pendingRow{row: rows[u]}
			q.rows[u] = p
		}
		if now, ok := next[u]; ok {
			p.row = now
		}
		p.jobs++
		p.next = "" // this change is the last to it now
	}
	q.jobs[id] = &bulkJob{State: "queued", Total: len(job.uuids)}
	q.waiting = append(q.waiting, job)
	startBulkWorker()
	q.changed.Broadcast()
	return res, nil
}

// saveQueuedEdit commits the change and its books to calibrefast.db, and
// answers its id.
func saveQueuedEdit(job *queuedEdit) (int64, error) {
	if appDB == nil {
		bulkQueue.lastID++
		return bulkQueue.lastID, nil
	}
	edit, err := json.Marshal(job.edit)
	if err != nil {
		return 0, err
	}
	var user any
	if job.by != nil && job.by.ID != 0 {
		user = job.by.ID
	}
	tx, err := appDB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`INSERT INTO bulk_edit_queue (edit, user_id, created_at) VALUES (?, ?, ?)`, string(edit), user, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, err
	}
	ins, err := tx.Prepare(`INSERT INTO bulk_edit_books (job_id, pos, uuid, base) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer ins.Close()
	for i, u := range job.uuids {
		var base any
		if job.base[i] != "" {
			base = job.base[i]
		}
		if _, err := ins.Exec(id, i, u, base); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// startBulkWorker starts the worker if it isn't running. Under bulkQueue.mu.
func startBulkWorker() {
	if !bulkQueue.worker && !bulkQueue.stopping {
		bulkQueue.worker = true
		go runBulkQueue()
	}
}

// runBulkQueue makes the queued changes, oldest first, until none are left.
// A panic outside a book's own write (bulkEditBook catches those) stops the
// queue until the server restarts, and refuses library writes that would
// go ahead of it, rather than taking the server down.
func runBulkQueue() {
	q := &bulkQueue
	defer func() {
		if p := recover(); p != nil {
			log.Printf("bulk edit queue: STOPPED until the server restarts (the queued changes are kept): panic: %v\n%s", p, debug.Stack())
			q.mu.Lock()
			q.worker, q.stopping = false, true
			q.changed.Broadcast()
			q.mu.Unlock()
		}
	}()
	for {
		q.mu.Lock()
		if len(q.waiting) == 0 || q.stopping {
			q.worker = false
			q.changed.Broadcast()
			q.mu.Unlock()
			return
		}
		job := q.waiting[0]
		q.jobs[job.id].State = "running"
		q.mu.Unlock()

		if res, finished := runQueuedEdit(job); finished {
			finishQueuedEdit(job, res)
		}
	}
}

// runQueuedEdit makes the change to each of its books without an outcome.
// It stops early, and says so, when the server is stopping.
func runQueuedEdit(job *queuedEdit) (bulkResult, bool) {
	q := &bulkQueue
	res := bulkResult{Failed: []bulkFailure{}, Books: []tableBook{}, Job: job.id}
	var changed []string
	for i, u := range job.uuids {
		switch job.outcome[i] { // made before the server last stopped
		case "changed":
			changed = append(changed, u)
			continue
		case "unchanged":
			res.Unchanged++
			continue
		case "failed":
			res.Failed = append(res.Failed, bulkFailure{u, "see the failures kept from before the server stopped"})
			continue
		}
		q.mu.Lock()
		stop := q.stopping
		q.mu.Unlock()
		if stop {
			bulkEditsMade(changed) // what it did change is in the catalog when the server starts
			return res, false
		}

		base, outcome, after := job.base[i], "", ""
		var err error
		if base == "" {
			// The change before it to this book couldn't say what it left.
			err = errors.New("couldn't tell whether the book was changed elsewhere since this edit was made; nothing was written")
		} else {
			var ok bool
			ok, after, err = bulkEditBook(u, &job.edit, job.by, base)
			if err == nil && !ok {
				outcome = "unchanged"
			} else if err == nil {
				outcome = "changed"
			}
		}
		next := after
		switch outcome {
		case "changed":
			changed = append(changed, u)
		case "unchanged":
			res.Unchanged++
			next = base
		default:
			outcome = "failed"
			// A later change to it is checked against the same base: it
			// fails too if the book was changed elsewhere, and is made if
			// this one only couldn't be written.
			next = base
			res.Failed = append(res.Failed, bulkFailure{u, err.Error()})
			log.Printf("bulk edit %s: not made: %v", u, err)
		}
		recordOutcome(job, i, outcome, next, err)
	}
	res.Changed = len(changed)
	res.Books = bulkEditsMade(changed)
	if len(res.Failed) > 0 || res.Changed > 0 {
		who := "?"
		if job.by != nil {
			who = job.by.Username
		}
		log.Printf("bulk edit: %s changed %d book(s) (%d unchanged, %d failed)", who, res.Changed, res.Unchanged, len(res.Failed))
	}
	return res, true
}

// recordOutcome commits a book's outcome, the base of the next queued
// change to it, and any failure, together, and keeps them in memory. Under
// bulkQueue.mu throughout, so a change queued meanwhile either is that
// next change or takes the base from pendingRow.next.
func recordOutcome(job *queuedEdit, i int, outcome, next string, failure error) {
	u := job.uuids[i]
	title := u
	if failure != nil {
		if b, err := tableBooks(calibreDB, []string{u}); err == nil && len(b) == 1 {
			title = b[0].Title
		}
	}
	q := &bulkQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	var later *queuedEdit
	for _, j := range q.waiting {
		if j.id > job.id && slices.Contains(j.uuids, u) {
			later = j
			break
		}
	}
	if appDB != nil {
		err := func() error {
			tx, err := appDB.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err := tx.Exec(`UPDATE bulk_edit_books SET outcome = ? WHERE job_id = ? AND pos = ?`, outcome, job.id, i); err != nil {
				return err
			}
			if later != nil {
				if _, err := tx.Exec(`UPDATE bulk_edit_books SET base = ? WHERE job_id = ? AND uuid = ? AND base IS NULL`, next, later.id, u); err != nil {
					return err
				}
			}
			if failure != nil {
				edit, _ := json.Marshal(job.edit)
				var user any
				if job.by != nil && job.by.ID != 0 {
					user = job.by.ID
				}
				if _, err := tx.Exec(`INSERT INTO bulk_edit_failures (job_id, uuid, title, edit, error, user_id, at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
					job.id, u, title, string(edit), failure.Error(), user, time.Now().Unix()); err != nil {
					return err
				}
			}
			return tx.Commit()
		}()
		if err != nil {
			// After a restart this book is made again: its write is checked
			// against the same base, and fails if it was made (its
			// last_modified moved). A later change to it has no base, and
			// fails rather than writes unchecked.
			log.Printf("bulk edit queue: saving %s's outcome: %v", u, err)
		}
	}
	job.outcome[i] = outcome
	if later != nil {
		if k := slices.Index(later.uuids, u); k >= 0 && later.base[k] == "" {
			later.base[k] = next
		}
	} else if p := q.rows[u]; p != nil {
		p.next = next
	}
	q.jobs[job.id].Done = job.made()
	q.changed.Broadcast()
}

// finishQueuedEdit takes a change that's been made off the queue.
func finishQueuedEdit(job *queuedEdit, res bulkResult) {
	if appDB != nil {
		// If this fails, the change stays; every book has an outcome, so
		// making it again when the server starts writes nothing.
		_, err := appDB.Exec(`DELETE FROM bulk_edit_queue WHERE id = ?`, job.id)
		logErr(err, "bulk edit queue: removing a change made")
	}
	q := &bulkQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, u := range job.uuids {
		if p := q.rows[u]; p != nil {
			if p.jobs--; p.jobs <= 0 {
				delete(q.rows, u)
			}
		}
	}
	// A book with more changes queued is shown as they'll leave it.
	for i, b := range res.Books {
		if p := q.rows[b.UUID]; p != nil {
			res.Books[i] = p.row
		}
	}
	q.waiting = q.waiting[1:]
	q.jobs[job.id] = &bulkJob{State: "done", Done: len(job.uuids), Total: len(job.uuids), Result: &res}
	q.finished = append(q.finished, job.id)
	if len(q.finished) > bulkJobsKept {
		delete(q.jobs, q.finished[0])
		q.finished = q.finished[1:]
	}
	q.changed.Broadcast()
}

// loadBulkQueue takes up the changes left in calibrefast.db when the server
// last stopped, and starts making them.
func loadBulkQueue() {
	jobs, err := readBulkQueue()
	if err != nil {
		log.Println("bulk edit queue: NOT making the changes left from the last run:", err)
		return
	}
	if len(jobs) == 0 {
		return
	}
	log.Printf("bulk edit queue: %d change(s) left from the last run; making them now", len(jobs))

	q := &bulkQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, job := range jobs {
		var left []string
		for i, u := range job.uuids {
			if job.outcome[i] == "" {
				left = append(left, u)
			}
		}
		books, err := tableBooks(calibreDB, left)
		if err != nil {
			log.Println("bulk edit queue: reading rows:", err)
		}
		for _, b := range books {
			p := q.rows[b.UUID]
			if p == nil {
				p = &pendingRow{row: b}
				q.rows[b.UUID] = p
			}
			p.row, _ = predictRow(p.row, &job.edit)
		}
		for _, u := range job.uuids {
			if p := q.rows[u]; p != nil {
				p.jobs++
			}
		}
		q.jobs[job.id] = &bulkJob{State: "queued", Done: job.made(), Total: len(job.uuids)}
		q.waiting = append(q.waiting, job)
	}
	startBulkWorker()
}

// readBulkQueue reads the queued changes, oldest first. Any it can't read
// stop it: making the ones after could undo them (a later change to a book
// is written against what the earlier one leaves).
func readBulkQueue() ([]*queuedEdit, error) {
	rows, err := appDB.Query(`SELECT id, edit, user_id FROM bulk_edit_queue ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var jobs []*queuedEdit
	for rows.Next() {
		job := &queuedEdit{}
		var edit string
		var user sql.NullInt64
		if err := rows.Scan(&job.id, &edit, &user); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(edit), &job.edit); err != nil {
			rows.Close()
			return nil, fmt.Errorf("change %d: %w", job.id, err)
		}
		if user.Valid {
			job.by, _ = getUserByID(user.Int64)
		}
		jobs = append(jobs, job)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, job := range jobs {
		books, err := appDB.Query(`SELECT uuid, COALESCE(base, ''), COALESCE(outcome, '') FROM bulk_edit_books WHERE job_id = ? ORDER BY pos`, job.id)
		if err != nil {
			return nil, err
		}
		for books.Next() {
			var u, base, outcome string
			if err := books.Scan(&u, &base, &outcome); err != nil {
				books.Close()
				return nil, err
			}
			job.uuids, job.base, job.outcome = append(job.uuids, u), append(job.base, base), append(job.outcome, outcome)
		}
		books.Close()
		if err := books.Err(); err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

// pendingBulkRows puts the rows of books with queued changes in place of
// theirs from metadata.db.
func pendingBulkRows(books []tableBook) {
	q := &bulkQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.rows) == 0 {
		return
	}
	for i, b := range books {
		if p := q.rows[b.UUID]; p != nil {
			books[i] = p.row
		}
	}
}

// waitBulkEdits waits until the queue is empty. false: the server is
// stopping, and the caller mustn't change the library (it would go ahead
// of what's left in the queue). Never call it holding bulkMu: the worker
// needs it.
func waitBulkEdits() bool {
	q := &bulkQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.waiting) > 0 && q.worker {
		q.changed.Wait()
	}
	return !q.stopping
}

// waitBulkEditsFor waits until no queued change to the book is left; false
// as waitBulkEdits.
func waitBulkEditsFor(uuid string) bool {
	q := &bulkQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.rows[uuid] != nil && q.worker {
		q.changed.Wait()
	}
	return !q.stopping
}

// stopBulkEdits, as the server stops, gives the queue until the deadline to
// empty, then lets the book being written finish and stops there. What's
// left is made when the server starts again.
func stopBulkEdits(deadline time.Time) {
	q := &bulkQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	if n := len(q.waiting); n > 0 && q.worker {
		log.Printf("bulk edit queue: making the %d change(s) still waiting", n)
	}
	timer := time.AfterFunc(time.Until(deadline), func() {
		q.mu.Lock()
		q.stopping = true
		q.changed.Broadcast()
		q.mu.Unlock()
	})
	defer timer.Stop()
	for q.worker && !q.stopping {
		q.changed.Wait()
	}
	q.stopping = true
	q.changed.Broadcast() // requests waiting for the queue are refused
	for q.worker {
		q.changed.Wait()
	}
	if n := len(q.waiting); n > 0 {
		log.Printf("bulk edit queue: %d change(s) not finished; they'll be made when the server starts", n)
	}
}

// bulkJobsHandler answers how far the changes asked about (?ids=1,2) have
// got, and how many are waiting in all.
func bulkJobsHandler(w http.ResponseWriter, r *http.Request) {
	var ids []int64
	for _, s := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		ids = append(ids, id)
		if len(ids) > 200 {
			http.Error(w, "too many ids", http.StatusBadRequest)
			return
		}
	}
	q := &bulkQueue
	q.mu.Lock()
	out := map[string]bulkJob{}
	for _, id := range ids {
		if j := q.jobs[id]; j != nil {
			out[strconv.FormatInt(id, 10)] = *j
		} else {
			out[strconv.FormatInt(id, 10)] = bulkJob{State: "unknown"}
		}
	}
	waiting := len(q.waiting)
	q.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out, "waiting": waiting})
}

// bulkFailure kept until seen (TS: BulkFailureKept in web/types.ts).
type keptBulkFailure struct {
	ID    int64           `json:"id"`
	UUID  string          `json:"uuid"`
	Title string          `json:"title"`
	Edit  json.RawMessage `json:"edit"`
	Error string          `json:"error"`
	By    string          `json:"by"`
	At    int64           `json:"at"` // unix seconds
}

// bulkFailuresKeptMax: how many kept failures are listed at once.
const bulkFailuresKeptMax = 500

// bulkFailuresHandler lists the books queued changes couldn't be made to,
// newest first, with how many there are in all.
func bulkFailuresHandler(w http.ResponseWriter, r *http.Request) {
	out := []keptBulkFailure{}
	var total int
	if err := appDB.QueryRow(`SELECT COUNT(*) FROM bulk_edit_failures`).Scan(&total); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows, err := appDB.Query(`SELECT f.id, f.uuid, f.title, f.edit, f.error, COALESCE(u.username, ''), f.at
		FROM bulk_edit_failures f LEFT JOIN users u ON u.id = f.user_id ORDER BY f.id DESC LIMIT ?`, bulkFailuresKeptMax)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var f keptBulkFailure
		var edit string
		if err := rows.Scan(&f.ID, &f.UUID, &f.Title, &edit, &f.Error, &f.By, &f.At); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.Edit = json.RawMessage(edit)
		if !json.Valid(f.Edit) {
			f.Edit = json.RawMessage(`{}`)
		}
		out = append(out, f)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"failures": out, "total": total})
}

// bulkFailuresDismissHandler forgets the kept failures named ({"ids":[…]}).
func bulkFailuresDismissHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || len(req.IDs) == 0 || len(req.IDs) > bulkFailuresKeptMax {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tx, err := appDB.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	for _, id := range req.IDs {
		if _, err := tx.Exec(`DELETE FROM bulk_edit_failures WHERE id = ?`, id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// afterBulkEdits runs h once the queue is empty: a change h makes to the
// library starts from the books as the queued changes leave them.
func afterBulkEdits(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !waitBulkEdits() {
			http.Error(w, errBulkStopping.Error(), http.StatusServiceUnavailable)
			return
		}
		h(w, r)
	}
}

// afterBookEdits runs h, for someone who can edit the library, once the
// queued changes to the book in the path ({uuid}) are made: the metadata
// editor shows and saves it as they leave it, not as it was before them.
// Anyone else gets the book as it is now, without waiting.
func afterBookEdits(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if canEditLibrary(r) && !waitBulkEditsFor(r.PathValue("uuid")) {
			http.Error(w, errBulkStopping.Error(), http.StatusServiceUnavailable)
			return
		}
		h(w, r)
	}
}
