package main

// The bulk editor's write queue. A library on slow storage can take seconds
// to write a change to (each book's metadata.opf, metadata.db), and the
// editor shouldn't wait for it. So POST /api/library/bulk only checks the
// change, saves it to calibrefast.db (bulk_edit_queue) and answers with the
// rows as they will be, foreseen from metadata.db; one worker then makes
// the changes in the order they came, a book at a time, as bulkEditBooksAs
// does.
//
// Until a book's queued changes are all made, the table (GET
// /api/library/table) shows it as they will leave it. GET
// /api/library/bulk/jobs says how far each change has got, with the rows
// as written (and any book that couldn't be changed) once it's done.
//
// A change stays in bulk_edit_queue until it's been made to every book, so
// one the server stopped or crashed in the middle of is finished when it
// starts again. Making a change twice to a book does nothing more (every
// bulkEdit sets, adds or removes values), so a book written just before a
// crash is safe to write again. On a normal stop the server first gives
// the queue up to shutdownTimeout to empty.
//
// Other changes to the library (the metadata editor, merges, removing
// books, undo) wait for the queue to empty first (waitBulkEdits), so they
// start from the books as the queued changes leave them.

import (
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iotku/CalibreFast/pkg/opf"
)

// queuedEdit is a change waiting in the queue.
type queuedEdit struct {
	id    int64
	uuids []string
	edit  bulkEdit
	by    *User
	done  int // books already written: the worker resumes after them
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
}

// bulkJobsKept: how many finished changes the server remembers for the
// editors to ask about.
const bulkJobsKept = 500

var bulkQueue = struct {
	mu       sync.Mutex
	changed  *sync.Cond // the queue or a change's progress changed
	waiting  []*queuedEdit
	running  bool // the worker is making waiting[0]
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
	for chunk := range slices.Chunk(read, 500) {
		got, err := tableBooks(calibreDB, chunk)
		if err != nil {
			return res, err
		}
		for _, b := range got {
			rows[b.UUID] = b
		}
	}
	var queued []string
	next := map[string]tableBook{}
	for _, u := range uuids {
		b, ok := rows[u]
		if !ok {
			res.Failed = append(res.Failed, bulkFailure{u, errNoSuchBook.Error() + ": " + u})
			continue
		}
		// Every book is queued, even one this doesn't foresee a change to:
		// the change is made to its metadata.opf, which may not match.
		queued = append(queued, u)
		if now, changed := predictRow(b, edit); changed {
			next[u] = now
			res.Books = append(res.Books, now)
		} else {
			res.Unchanged++
		}
	}
	res.Changed = len(res.Books)
	if len(queued) == 0 {
		return res, nil
	}

	job := &queuedEdit{uuids: queued, edit: *edit, by: by}
	id, err := saveQueuedEdit(job)
	if err != nil {
		return res, err
	}
	job.id, res.Job = id, id
	for _, u := range queued {
		p := q.rows[u]
		if p == nil {
			p = &pendingRow{row: rows[u]}
			q.rows[u] = p
		}
		if now, ok := next[u]; ok {
			p.row = now
		}
		p.jobs++
	}
	q.jobs[id] = &bulkJob{State: "queued", Total: len(queued)}
	q.waiting = append(q.waiting, job)
	startBulkWorker()
	q.changed.Broadcast()
	return res, nil
}

// saveQueuedEdit writes the change to bulk_edit_queue, and answers its id.
func saveQueuedEdit(job *queuedEdit) (int64, error) {
	if appDB == nil {
		bulkQueue.lastID++
		return bulkQueue.lastID, nil
	}
	uuids, err := json.Marshal(job.uuids)
	if err != nil {
		return 0, err
	}
	edit, err := json.Marshal(job.edit)
	if err != nil {
		return 0, err
	}
	var user any
	if job.by != nil && job.by.ID != 0 {
		user = job.by.ID
	}
	r, err := appDB.Exec(`INSERT INTO bulk_edit_queue (uuids, edit, user_id, created_at) VALUES (?, ?, ?, ?)`,
		string(uuids), string(edit), user, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// startBulkWorker starts the worker if it isn't running. Under bulkQueue.mu.
func startBulkWorker() {
	if !bulkQueue.worker && !bulkQueue.stopping {
		bulkQueue.worker = true
		go runBulkQueue()
	}
}

// runBulkQueue makes the queued changes, oldest first, until none are left.
func runBulkQueue() {
	q := &bulkQueue
	for {
		q.mu.Lock()
		if len(q.waiting) == 0 || q.stopping {
			q.worker, q.running = false, false
			q.changed.Broadcast()
			q.mu.Unlock()
			return
		}
		job := q.waiting[0]
		q.running = true
		q.jobs[job.id].State = "running"
		q.mu.Unlock()

		if res, finished := runQueuedEdit(job); finished {
			finishQueuedEdit(job, res)
		}
	}
}

// runQueuedEdit makes the change to each of its books not yet written. It
// stops early, and says so, when the server is stopping.
func runQueuedEdit(job *queuedEdit) (bulkResult, bool) {
	q := &bulkQueue
	res := bulkResult{Failed: []bulkFailure{}, Books: []tableBook{}, Job: job.id}
	var changed []string
	for i := job.done; i < len(job.uuids); i++ {
		q.mu.Lock()
		stop := q.stopping
		q.mu.Unlock()
		if stop {
			job.done = i
			if appDB != nil {
				_, err := appDB.Exec(`UPDATE bulk_edit_queue SET done = ? WHERE id = ?`, i, job.id)
				logErr(err, "bulk edit queue: saving progress")
			}
			// What it did change is in the catalog when the server starts.
			bulkEditsMade(changed)
			return res, false
		}
		u := job.uuids[i]
		bulkMu.Lock()
		ok, err := bulkEditBook(u, &job.edit, job.by)
		bulkMu.Unlock()
		switch {
		case err != nil:
			res.Failed = append(res.Failed, bulkFailure{u, err.Error()})
		case ok:
			changed = append(changed, u)
		default:
			res.Unchanged++
		}
		q.mu.Lock()
		q.jobs[job.id].Done = i + 1
		q.mu.Unlock()
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

// finishQueuedEdit takes a change that's been made off the queue.
func finishQueuedEdit(job *queuedEdit, res bulkResult) {
	if appDB != nil {
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
	q.running = false
	q.jobs[job.id] = &bulkJob{State: "done", Done: len(job.uuids), Total: len(job.uuids), Result: &res}
	q.finished = append(q.finished, job.id)
	if len(q.finished) > bulkJobsKept {
		delete(q.jobs, q.finished[0])
		q.finished = q.finished[1:]
	}
	q.changed.Broadcast()
}

// loadBulkQueue takes up the changes left in bulk_edit_queue when the
// server last stopped, and starts making them.
func loadBulkQueue() {
	rows, err := appDB.Query(`SELECT id, uuids, edit, user_id, done FROM bulk_edit_queue ORDER BY id`)
	if err != nil {
		log.Println("bulk edit queue:", err)
		return
	}
	var jobs []*queuedEdit
	for rows.Next() {
		var job queuedEdit
		var uuids, edit string
		var user *int64
		if err := rows.Scan(&job.id, &uuids, &edit, &user, &job.done); err != nil {
			log.Println("bulk edit queue:", err)
			continue
		}
		if err := json.Unmarshal([]byte(uuids), &job.uuids); err != nil {
			log.Printf("bulk edit queue: change %d: %v", job.id, err)
			continue
		}
		if err := json.Unmarshal([]byte(edit), &job.edit); err != nil {
			log.Printf("bulk edit queue: change %d: %v", job.id, err)
			continue
		}
		if user != nil {
			job.by, _ = getUserByID(*user)
		}
		jobs = append(jobs, &job)
	}
	logErr(rows.Err(), "bulk edit queue")
	rows.Close()
	if len(jobs) == 0 {
		return
	}
	log.Printf("bulk edit queue: %d change(s) left from the last run; making them now", len(jobs))

	q := &bulkQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, job := range jobs {
		left := job.uuids[min(job.done, len(job.uuids)):]
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
		q.jobs[job.id] = &bulkJob{State: "queued", Done: job.done, Total: len(job.uuids)}
		q.waiting = append(q.waiting, job)
	}
	startBulkWorker()
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

// waitBulkEdits waits until the queue is empty (or the server stops making
// its changes). Never call it holding bulkMu: the worker needs it.
func waitBulkEdits() {
	q := &bulkQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.waiting) > 0 && q.worker {
		q.changed.Wait()
	}
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

// afterBulkEdits runs h once the queue is empty: a change h makes to the
// library starts from the books as the queued changes leave them.
func afterBulkEdits(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		waitBulkEdits()
		h(w, r)
	}
}

// afterBookEdits runs h once the queued changes to the book in the path
// ({uuid}) are made: the metadata editor shows and saves it as they leave
// it, not as it was before them.
func afterBookEdits(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		waitBulkEditsFor(r.PathValue("uuid"))
		h(w, r)
	}
}

// waitBulkEditsFor waits until no queued change to the book is left.
func waitBulkEditsFor(uuid string) {
	q := &bulkQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.rows[uuid] != nil && q.worker {
		q.changed.Wait()
	}
}
