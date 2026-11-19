package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func postBulk(t *testing.T, body string) bulkResult {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/library/bulk", bytes.NewBufferString(body))
	req = req.WithContext(withUserCtx(req, &User{Username: "admin", IsAdmin: true}))
	bulkEditHandler(rec, req)
	var res bulkResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	return res
}

func tableRows(t *testing.T) map[string]tableBook {
	t.Helper()
	rec := httptest.NewRecorder()
	libraryTableHandler(rec, httptest.NewRequest("GET", "/api/library/table", nil))
	var table struct{ Books []tableBook }
	if err := json.Unmarshal(rec.Body.Bytes(), &table); err != nil {
		t.Fatal(err)
	}
	out := map[string]tableBook{}
	for _, b := range table.Books {
		out[b.UUID] = b
	}
	return out
}

func bulkJobState(t *testing.T, id int64) bulkJob {
	t.Helper()
	rec := httptest.NewRecorder()
	bulkJobsHandler(rec, httptest.NewRequest("GET", "/api/library/bulk/jobs?ids="+strconv.FormatInt(id, 10), nil))
	var out struct{ Jobs map[string]bulkJob }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Jobs[strconv.FormatInt(id, 10)]
}

// A bulk edit is answered before it's written: the answer, and the table,
// show the books as it will leave them; the job says when it's written.
func TestBulkQueueAnswersBeforeWriting(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, []string{"t1"}, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, []string{"t2"}, nil)

	bulkMu.Lock() // the worker can't write yet
	res := postBulk(t, `{"uuids":["`+a.uuid+`","`+b.uuid+`"],"edit":{"add_tags":["new"],"remove_tags":["t2"]}}`)
	if res.Changed != 2 || len(res.Books) != 2 || res.Job == 0 {
		t.Fatalf("answer %+v", res)
	}
	for _, r := range res.Books {
		want := []string{"t1", "new"}
		if r.UUID == b.uuid {
			want = []string{"new"}
		}
		if !slices.Equal(r.Tags, want) {
			t.Errorf("%s: foreseen tags %v, want %v", r.Title, r.Tags, want)
		}
	}
	// A second change to the same book starts from the first's result.
	res2 := postBulk(t, `{"uuids":["`+a.uuid+`"],"edit":{"title":"Alpha Two"}}`)
	if len(res2.Books) != 1 || res2.Books[0].Title != "Alpha Two" || !slices.Equal(res2.Books[0].Tags, []string{"t1", "new"}) {
		t.Errorf("second answer %+v", res2.Books)
	}
	if r := tableRow(t, db, a.uuid); r.Title != "Alpha" {
		t.Fatalf("written before the worker could: %+v", r)
	}
	if r := tableRows(t)[a.uuid]; r.Title != "Alpha Two" || !slices.Equal(r.Tags, []string{"t1", "new"}) {
		t.Errorf("table shows %+v", r)
	}
	if s := bulkJobState(t, res.Job).State; s != "queued" && s != "running" {
		t.Errorf("job %s", s)
	}
	bulkMu.Unlock()
	waitBulkEdits()

	if r := tableRow(t, db, a.uuid); r.Title != "Alpha Two" || !slices.Equal(r.Tags, []string{"new", "t1"}) && !slices.Equal(r.Tags, []string{"t1", "new"}) {
		t.Errorf("Alpha written %+v", r)
	}
	if r := tableRow(t, db, b.uuid); !slices.Equal(r.Tags, []string{"new"}) {
		t.Errorf("Beta written %+v", r)
	}
	j := bulkJobState(t, res.Job)
	if j.State != "done" || j.Result == nil || j.Result.Changed != 2 || j.Done != 2 {
		t.Errorf("job %+v", j)
	}
	var left int
	if err := appDB.QueryRow(`SELECT COUNT(*) FROM bulk_edit_queue`).Scan(&left); err != nil || left != 0 {
		t.Errorf("queue rows left: %d %v", left, err)
	}
	if s := bulkJobState(t, 999999).State; s != "unknown" {
		t.Errorf("unknown job: %s", s)
	}
}

// A change the server stopped in the middle of is saved with how far it
// got, and finished when the server starts again.
func TestBulkQueueResumesAfterStop(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, nil, nil)

	bulkMu.Lock()
	res := postBulk(t, `{"uuids":["`+a.uuid+`","`+b.uuid+`"],"edit":{"publisher":"Pub"}}`)
	stopped := make(chan struct{})
	go func() { stopBulkEdits(time.Now()); close(stopped) }()
	for { // the deadline has passed: wait for stopBulkEdits to say stop
		bulkQueue.mu.Lock()
		s := bulkQueue.stopping
		bulkQueue.mu.Unlock()
		if s {
			break
		}
		time.Sleep(time.Millisecond)
	}
	bulkMu.Unlock() // the book being written is finished, then it stops
	<-stopped

	outcomes := map[int]sql.NullString{}
	rows, err := appDB.Query(`SELECT pos, outcome FROM bulk_edit_books WHERE job_id = ?`, res.Job)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var pos int
		var o sql.NullString
		if err := rows.Scan(&pos, &o); err != nil {
			t.Fatal(err)
		}
		outcomes[pos] = o
	}
	rows.Close()
	if outcomes[0].String != "changed" || outcomes[1].Valid {
		t.Fatalf("saved outcomes %v", outcomes)
	}
	if r := tableRow(t, db, b.uuid); r.Publisher == "Pub" {
		t.Fatalf("Beta written after the stop: %+v", r)
	}

	restartBulkQueue()
	if r := tableRows(t)[b.uuid]; r.Publisher != "Pub" {
		t.Errorf("table before it's written: %+v", r)
	}
	waitBulkEdits()
	for _, u := range []string{a.uuid, b.uuid} {
		if r := tableRow(t, db, u); r.Publisher != "Pub" {
			t.Errorf("%s: %+v", r.Title, r)
		}
	}
	var left int
	if err := appDB.QueryRow(`SELECT COUNT(*) FROM bulk_edit_queue`).Scan(&left); err != nil || left != 0 {
		t.Errorf("queue rows left: %d %v", left, err)
	}
}

// restartBulkQueue is the queue as a server starting again finds it.
func restartBulkQueue() {
	bulkQueue.mu.Lock()
	bulkQueue.stopping, bulkQueue.waiting, bulkQueue.rows = false, nil, map[string]*pendingRow{}
	bulkQueue.mu.Unlock()
	loadBulkQueue()
}

func keptFailures(t *testing.T) []keptBulkFailure {
	t.Helper()
	rec := httptest.NewRecorder()
	bulkFailuresHandler(rec, httptest.NewRequest("GET", "/api/library/bulk/failures", nil))
	var out struct{ Failures []keptBulkFailure }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return out.Failures
}

// A book changed elsewhere after its edit was queued isn't written over:
// the edit, and a later one built on it, fail, and the failures are kept
// until dismissed.
func TestBulkQueueChangedElsewhereFails(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, nil, nil)

	bulkMu.Lock()
	first := postBulk(t, `{"uuids":["`+a.uuid+`","`+b.uuid+`"],"edit":{"title":"Renamed"}}`)
	second := postBulk(t, `{"uuids":["`+a.uuid+`"],"edit":{"add_tags":["later"]}}`)
	// Calibre changes Alpha meanwhile.
	if _, err := db.Exec(`UPDATE books SET title = 'Desk', last_modified = '2030-01-01 00:00:00+00:00' WHERE uuid = ?`, a.uuid); err != nil {
		t.Fatal(err)
	}
	bulkMu.Unlock()
	waitBulkEdits()

	if r := tableRow(t, db, a.uuid); r.Title != "Desk" || len(r.Tags) != 0 {
		t.Errorf("Alpha written over: %+v", r)
	}
	if r := tableRow(t, db, b.uuid); r.Title != "Renamed" {
		t.Errorf("Beta not written: %+v", r)
	}
	if j := bulkJobState(t, first.Job); j.Result == nil || j.Result.Changed != 1 || len(j.Result.Failed) != 1 || j.Result.Failed[0].UUID != a.uuid {
		t.Errorf("first %+v", j.Result)
	}
	if j := bulkJobState(t, second.Job); j.Result == nil || j.Result.Changed != 0 || len(j.Result.Failed) != 1 {
		t.Errorf("second %+v", j.Result)
	}
	kept := keptFailures(t)
	if len(kept) != 2 || kept[0].UUID != a.uuid || !strings.Contains(kept[0].Error, "changed elsewhere") || kept[0].Title != "Desk" {
		t.Fatalf("kept %+v", kept)
	}
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"ids": []int64{kept[0].ID, kept[1].ID}})
	bulkFailuresDismissHandler(rec, httptest.NewRequest("POST", "/api/library/bulk/failures/dismiss", bytes.NewReader(body)))
	if rec.Code != http.StatusNoContent || len(keptFailures(t)) != 0 {
		t.Errorf("dismiss: %d, left %v", rec.Code, keptFailures(t))
	}
}

// An edit left from before a restart is written only if the book hasn't
// changed since it was queued.
func TestBulkQueueReplayChecksBase(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, nil, nil)
	stamp := func(u string) string { m, _ := bookModified(db, u); return m }
	str := func(s string) *string { return &s }
	job := &queuedEdit{uuids: []string{a.uuid, b.uuid}, base: []string{stamp(a.uuid), stamp(b.uuid)}, outcome: []string{"", ""}, edit: bulkEdit{Publisher: str("Old edit")}}
	if _, err := saveQueuedEdit(job); err != nil {
		t.Fatal(err)
	}
	// Changed while the server was down (or ran without -allow-writes).
	if _, err := db.Exec(`UPDATE books SET last_modified = '2030-01-01 00:00:00+00:00' WHERE uuid = ?`, a.uuid); err != nil {
		t.Fatal(err)
	}
	restartBulkQueue()
	waitBulkEdits()
	if r := tableRow(t, db, a.uuid); r.Publisher == "Old edit" {
		t.Errorf("Alpha written over: %+v", r)
	}
	if r := tableRow(t, db, b.uuid); r.Publisher != "Old edit" {
		t.Errorf("Beta not written: %+v", r)
	}
	if kept := keptFailures(t); len(kept) != 1 || kept[0].UUID != a.uuid {
		t.Errorf("kept %+v", kept)
	}
}

// A panic writing a book fails that book; the queue and server go on.
func TestBulkQueuePanicFailsTheBook(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, nil, nil)
	bulkEditBookHook = func(u string) {
		if u == a.uuid {
			panic("boom")
		}
	}
	t.Cleanup(func() { bulkEditBookHook = nil })
	res := postBulk(t, `{"uuids":["`+a.uuid+`","`+b.uuid+`"],"edit":{"publisher":"P"}}`)
	waitBulkEdits()
	j := bulkJobState(t, res.Job)
	if j.Result == nil || j.Result.Changed != 1 || len(j.Result.Failed) != 1 || !strings.Contains(j.Result.Failed[0].Error, "boom") {
		t.Errorf("result %+v", j.Result)
	}
	if r := tableRow(t, db, b.uuid); r.Publisher != "P" {
		t.Errorf("Beta %+v", r)
	}
	if !bulkMu.TryLock() {
		t.Fatal("bulkMu left locked")
	}
	bulkMu.Unlock()
}

// Once the server is stopping, writes waiting for the queue are refused
// rather than made ahead of it, and nothing new is queued.
func TestBulkQueueStoppingRefuses(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	bulkQueue.mu.Lock()
	bulkQueue.stopping = true
	bulkQueue.mu.Unlock()
	t.Cleanup(func() {
		bulkQueue.mu.Lock()
		bulkQueue.stopping = false
		bulkQueue.mu.Unlock()
	})
	ran := false
	rec := httptest.NewRecorder()
	afterBulkEdits(func(http.ResponseWriter, *http.Request) { ran = true })(rec, httptest.NewRequest("POST", "/merge/apply", nil))
	if ran || rec.Code != http.StatusServiceUnavailable {
		t.Errorf("merge while stopping: ran %v, %d", ran, rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/library/bulk", bytes.NewBufferString(`{"uuids":["`+a.uuid+`"],"edit":{"publisher":"P"}}`))
	bulkEditHandler(rec, req.WithContext(withUserCtx(req, &User{Username: "admin", IsAdmin: true})))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("queued while stopping: %d", rec.Code)
	}
}

// Someone who can't edit gets a book's metadata at once, without waiting
// for its queued changes; an editor waits for them.
func TestAfterBookEditsWaitsOnlyForEditors(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	orig := allowWrites
	allowWrites = true
	t.Cleanup(func() { allowWrites = orig })
	bulkMu.Lock()
	postBulk(t, `{"uuids":["`+a.uuid+`"],"edit":{"publisher":"P"}}`)
	h := afterBookEdits(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	get := func(u *User) chan int {
		done := make(chan int, 1)
		go func() {
			req := httptest.NewRequest("GET", "/metadata/"+a.uuid, nil)
			req.SetPathValue("uuid", a.uuid)
			if u != nil {
				req = req.WithContext(withUserCtx(req, u))
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			done <- rec.Code
		}()
		return done
	}
	select {
	case <-get(nil):
	case <-time.After(2 * time.Second):
		t.Error("an anonymous request waited for the queue")
	}
	editor := get(&User{ID: 1, Username: "ed", IsEditor: true})
	select {
	case <-editor:
		t.Error("an editor didn't wait for the queue")
	case <-time.After(100 * time.Millisecond):
	}
	bulkMu.Unlock()
	if code := <-editor; code != http.StatusTeapot {
		t.Errorf("editor: %d", code)
	}
}

// predictRow foresees what apply makes of the row's values.
func TestPredictRow(t *testing.T) {
	b := tableBook{UUID: "u", Title: "T", Authors: "A & B", Series: "S", SeriesIndex: 2, PubDate: "2001-02-03",
		Tags: []string{"x", "y"}, Languages: []string{"eng"}}
	str := func(s string) *string { return &s }
	for _, c := range []struct {
		edit    bulkEdit
		changed bool
		check   func(tableBook) bool
	}{
		{bulkEdit{Title: str("T")}, false, nil},
		{bulkEdit{Title: str("  ")}, false, nil},
		{bulkEdit{Authors: str("C &  D")}, true, func(r tableBook) bool { return r.Authors == "C & D" }},
		{bulkEdit{Series: str("")}, true, func(r tableBook) bool { return r.Series == "" && r.SeriesIndex == 1 }},
		{bulkEdit{SeriesIndex: str("3.5")}, true, func(r tableBook) bool { return r.Series == "S" && r.SeriesIndex == 3.5 }},
		{bulkEdit{Date: str("")}, true, func(r tableBook) bool { return r.PubDate == "" }},
		{bulkEdit{Date: str("1999-12-31")}, true, func(r tableBook) bool { return r.PubDate == "1999-12-31" }},
		{bulkEdit{RemoveTags: []string{"X"}}, true, func(r tableBook) bool { return slices.Equal(r.Tags, []string{"y"}) }},
		{bulkEdit{AddTags: []string{"Y"}}, false, nil},
		{bulkEdit{RemoveLanguages: []string{"en"}}, true, func(r tableBook) bool { return len(r.Languages) == 0 }},
	} {
		got, changed := predictRow(b, &c.edit)
		if changed != c.changed || (c.check != nil && !c.check(got)) {
			e, _ := json.Marshal(c.edit)
			t.Errorf("%s: changed %v, row %+v", e, changed, got)
		}
	}
}
