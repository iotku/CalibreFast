package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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

	var done int
	if err := appDB.QueryRow(`SELECT done FROM bulk_edit_queue WHERE id = ?`, res.Job).Scan(&done); err != nil || done != 1 {
		t.Fatalf("saved progress %d %v", done, err)
	}
	if r := tableRow(t, db, b.uuid); r.Publisher == "Pub" {
		t.Fatalf("Beta written after the stop: %+v", r)
	}

	// The server starts again.
	bulkQueue.mu.Lock()
	bulkQueue.stopping, bulkQueue.waiting, bulkQueue.rows = false, nil, map[string]*pendingRow{}
	bulkQueue.mu.Unlock()
	loadBulkQueue()
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
