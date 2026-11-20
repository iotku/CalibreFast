package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func statsMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me/stats", requireUser(statsHandler))
	mux.HandleFunc("PUT /api/me/activity", requireUser(activityPutHandler))
	mux.HandleFunc("DELETE /api/me/activity", requireUser(activityDeleteHandler))
	mux.HandleFunc("PUT /api/me/prefs", requireUser(putPrefsHandler))
	return mux
}

func activityBody(client, day, uuid string, pages, seconds int) string {
	return fmt.Sprintf(`{"client_id":%q,"entries":[{"day":%q,"book_uuid":%q,"pages":%d,"seconds":%d}]}`, client, day, uuid, pages, seconds)
}

func getStats(t *testing.T, mux http.Handler, u *User, today string) statsAnswer {
	t.Helper()
	rec := call(t, mux, u, "GET", "/api/me/stats?today="+today+"&tz=0", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats: %d %s", rec.Code, rec.Body)
	}
	var s statsAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Totals per browser: sending again keeps the larger; browsers add up.
func TestActivityPut(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	mux := statsMux()
	today := time.Now().Format(dayLayout)
	put := func(body string) int { return call(t, mux, u, "PUT", "/api/me/activity", body).Code }

	for _, body := range []string{
		activityBody("phone", today, books[0], 5, 120),
		activityBody("phone", today, books[0], 5, 120), // repeated after a lost answer
		activityBody("phone", today, books[0], 3, 60),  // older, arriving late
		activityBody("laptop", today, books[0], 2, 30),
		activityBody("laptop", today, "no-such-book", 9, 9), // skipped
	} {
		if c := put(body); c != http.StatusNoContent {
			t.Fatalf("%s: %d", body, c)
		}
	}
	s := getStats(t, mux, u, today)
	if s.TodayPages != 7 || s.TodaySeconds != 150 || s.Totals.Pages != 7 {
		t.Errorf("today %d pages %d s, total %d; want 7, 150, 7", s.TodayPages, s.TodaySeconds, s.Totals.Pages)
	}
	if s.Streak.Current != 1 || len(s.Days) != 1 {
		t.Errorf("streak %+v, days %v", s.Streak, s.Days)
	}

	old := time.Now().AddDate(-2, 0, 0).Format(dayLayout)
	for _, body := range []string{
		`not json`,
		activityBody("", today, books[0], 1, 1),
		activityBody("bad id!", today, books[0], 1, 1),
		activityBody("phone", "2026-13-45", books[0], 1, 1),
		activityBody("phone", "yesterday", books[0], 1, 1),
		activityBody("phone", old, books[0], 1, 1),
		activityBody("phone", today, books[0], -1, 1),
		activityBody("phone", today, books[0], 1, 90000),
		activityBody("phone", today, books[0], 5000, 1),
	} {
		if c := put(body); c != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, c)
		}
	}
	if c := call(t, mux, u, "GET", "/api/me/activity", "").Code; c != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d, want 405", c)
	}
	if c := call(t, mux, nil, "PUT", "/api/me/activity", activityBody("x", today, books[0], 1, 1)).Code; c != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", c)
	}
	if c := call(t, mux, nil, "GET", "/api/me/stats?today="+today, "").Code; c != http.StatusUnauthorized {
		t.Errorf("signed out stats: %d, want 401", c)
	}
	if c := call(t, mux, u, "GET", "/api/me/stats?today=nope", "").Code; c != http.StatusBadRequest {
		t.Errorf("bad today: %d, want 400", c)
	}
}

// With tracking off nothing is recorded; deleting forgets everything.
func TestActivityOptOut(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	mux := statsMux()
	today := time.Now().Format(dayLayout)
	if c := call(t, mux, u, "PUT", "/api/me/activity", activityBody("p", today, books[0], 4, 100)).Code; c != http.StatusNoContent {
		t.Fatal(c)
	}
	_ = setStatus(u.ID, books[0], "finished", "auto")
	if c := call(t, mux, u, "DELETE", "/api/me/activity", "").Code; c != http.StatusNoContent {
		t.Fatal(c)
	}
	if s := getStats(t, mux, u, today); s.Totals.Pages != 0 || s.Totals.Finished != 0 {
		t.Fatalf("after delete: %+v", s.Totals)
	}

	if c := call(t, mux, u, "PUT", "/api/me/prefs", `{"track_reading":false}`).Code; c != http.StatusNoContent {
		t.Fatal(c)
	}
	call(t, mux, u, "PUT", "/api/me/activity", activityBody("p", today, books[0], 4, 100))
	if _, err := setManualStatus(u.ID, books[0], "reading", nowMilli()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM reading_activity`); n != 0 {
		t.Errorf("%d activity rows with tracking off", n)
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM reading_events`); n != 0 {
		t.Errorf("%d events with tracking off", n)
	}
	s := getStats(t, mux, u, today)
	if s.Tracking {
		t.Error("tracking still on")
	}
	// A partial PUT (as the guest-import one) keeps the opt-out.
	call(t, mux, u, "PUT", "/api/me/prefs", `{"format":"pdf"}`)
	if p, _ := userPrefsFor(u); p.TrackReading || p.Format != "pdf" {
		t.Errorf("prefs %+v", p)
	}
	for _, body := range []string{`{"daily_pages":-1}`, `{"yearly_books":5000}`} {
		if c := call(t, mux, u, "PUT", "/api/me/prefs", body).Code; c != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, c)
		}
	}
}

// Nothing pops up in the reader unless asked for.
func TestGoalNoticeOffByDefault(t *testing.T) {
	if defaultPrefs().GoalNotice {
		t.Fatal("goal_notice on by default")
	}
	u, _ := setupLibraryAndUser(t, 1)
	mux := statsMux()
	call(t, mux, u, "PUT", "/api/me/prefs", `{"daily_pages":10}`)
	if s := getStats(t, mux, u, time.Now().Format(dayLayout)); s.Goal.Notice || s.Goal.DailyPages != 10 {
		t.Errorf("goal %+v", s.Goal)
	}
	call(t, mux, u, "PUT", "/api/me/prefs", `{"goal_notice":true}`)
	if s := getStats(t, mux, u, time.Now().Format(dayLayout)); !s.Goal.Notice || s.Goal.DailyPages != 10 {
		t.Errorf("goal %+v after opting in", s.Goal)
	}
}

func TestStatusEvents(t *testing.T) {
	u, books := setupLibraryAndUser(t, 2)
	b := books[0]
	events := func(kind string) int {
		return count(t, appDB, `SELECT COUNT(*) FROM reading_events WHERE book_uuid = ? AND kind = ?`, b, kind)
	}
	_ = setStatus(u.ID, b, "reading", "auto")
	_ = setStatus(u.ID, b, "reading", "auto") // no change
	if events("started") != 1 || events("finished") != 0 {
		t.Fatalf("started %d finished %d", events("started"), events("finished"))
	}
	_ = setStatus(u.ID, b, "finished", "auto")
	time.Sleep(1100 * time.Millisecond) // events are to the second
	if _, err := setManualStatus(u.ID, b, "reading", nowMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := setManualStatus(u.ID, b, "finished", nowMilli()); err != nil {
		t.Fatal(err)
	}
	if events("started") != 2 || events("finished") != 2 {
		t.Errorf("re-read: started %d finished %d, want 2 and 2", events("started"), events("finished"))
	}
	// Straight to finished (opened at the end): started too.
	_ = setStatus(u.ID, books[1], "finished", "auto")
	if n := count(t, appDB, `SELECT COUNT(*) FROM reading_events WHERE book_uuid = ?`, books[1]); n != 2 {
		t.Errorf("%d events for a book finished at once", n)
	}
	s := getStats(t, statsMux(), u, time.Now().Format(dayLayout))
	if s.Year.Finished != 2 || s.Totals.Finished != 2 || len(s.Year.Books) != 2 || s.Year.Started != 2 {
		t.Errorf("year %+v totals %+v: want 2 books started and finished, counted once each", s.Year, s.Totals)
	}
}

func TestMergeMovesActivity(t *testing.T) {
	u, books := setupLibraryAndUser(t, 2)
	mux := statsMux()
	today := time.Now().Format(dayLayout)
	call(t, mux, u, "PUT", "/api/me/activity", activityBody("p", today, books[0], 4, 100))
	call(t, mux, u, "PUT", "/api/me/activity", activityBody("p", today, books[1], 6, 50))
	_ = setStatus(u.ID, books[1], "finished", "auto")
	if _, err := mergeReaderData(books[0], []string{books[1]}, nil); err != nil {
		t.Fatal(err)
	}
	var pages, secs int
	if err := appDB.QueryRow(`SELECT pages, seconds FROM reading_activity WHERE book_uuid = ?`, books[0]).Scan(&pages, &secs); err != nil {
		t.Fatal(err)
	}
	if pages != 10 || secs != 150 {
		t.Errorf("kept book: %d pages %d s, want 10, 150", pages, secs)
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM reading_activity WHERE book_uuid = ?`, books[1]) +
		count(t, appDB, `SELECT COUNT(*) FROM reading_events WHERE book_uuid = ?`, books[1]); n != 0 {
		t.Errorf("%d rows left on the merged book", n)
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM reading_events WHERE book_uuid = ? AND kind = 'finished'`, books[0]); n != 1 {
		t.Errorf("finished events on the kept book: %d", n)
	}
}

func TestStreaks(t *testing.T) {
	day := func(s string) time.Time { d, _ := time.Parse(dayLayout, s); return d }
	act := func(days ...string) []dayActivity {
		var out []dayActivity
		for _, d := range days {
			out = append(out, dayActivity{Day: d, Pages: 1})
		}
		return out
	}
	for _, c := range []struct {
		name  string
		days  []dayActivity
		today string
		want  streaks
	}{
		{"none", nil, "2026-03-01", streaks{}},
		{"today only", act("2026-03-01"), "2026-03-01", streaks{1, 1}},
		{"yesterday still counts", act("2026-02-27", "2026-02-28"), "2026-03-01", streaks{2, 2}},
		{"broken", act("2026-02-26", "2026-02-27"), "2026-03-01", streaks{0, 2}},
		{"over month and year ends", act("2025-12-30", "2025-12-31", "2026-01-01", "2026-01-02"), "2026-01-02", streaks{4, 4}},
		{"leap day", act("2028-02-28", "2028-02-29", "2028-03-01"), "2028-03-01", streaks{3, 3}},
		{"longest earlier", act("2026-01-01", "2026-01-02", "2026-01-03", "2026-02-28", "2026-03-01"), "2026-03-01", streaks{2, 3}},
		{"a few seconds isn't reading", []dayActivity{{Day: "2026-03-01", Seconds: 10}}, "2026-03-01", streaks{}},
		{"a minute is", []dayActivity{{Day: "2026-03-01", Seconds: 60}}, "2026-03-01", streaks{1, 1}},
	} {
		if got := streaksOf(c.days, day(c.today)); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	if got := heatmapStart(day("2026-11-20")).Format(dayLayout); got != "2025-11-16" { // a Friday; 52 weeks before its Sunday
		t.Errorf("heatmap start %s", got)
	}
}
