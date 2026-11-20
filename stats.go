package main

// Reading stats: how much a user reads, day by day, to show them their
// progress and keep them going (the /me/stats page, web/stats.ts, and the
// card on the home page).
//
//   - reading_activity: pages turned and time spent reading, per day (the
//     reader's own local day), book and browser. The readers keep running
//     totals for the day (web/activity.ts) and send them as they are, so a
//     send repeated after a lost answer changes nothing: the server keeps
//     the larger of what it has and what it gets.
//   - reading_events: when books were started and finished, recorded as the
//     status changes (setStatus, setManualStatus). KOReader's progress
//     reaches these through setStatus, but not the activity: it says where
//     the reader is, not what was read when.
//
// Like the rest of a user's reading, these are theirs alone: no one else,
// admins included, sees them (docs/SECURITY.md). The user can stop the
// recording (track_reading, prefs.go) and delete what was recorded.

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

const (
	dayLayout = "2006-01-02"
	// Bounds on what one activity send may carry.
	maxActivityEntries = 100
	maxActivityPages   = 2000
	maxActivitySeconds = 24 * 60 * 60
	// How far back a day may be sent (a device that was offline a long
	// time), and ahead (time zones ahead of the server's).
	activityMaxAge   = 400 * 24 * time.Hour
	activityMaxAhead = 2 * 24 * time.Hour
	// A day counts toward a streak with a page turned or a minute read.
	streakMinSeconds = 60
	// The heatmap: 52 weeks before this one, and this one so far.
	heatmapWeeks = 52
)

var (
	activityClientRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	activityDayRe    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

type queryExecer interface {
	execer
	rowQuerier
}

// recordStatusEvents notes a book started or finished, as a status changes
// from prev to next, unless the user doesn't keep reading stats. Reading
// again after finishing is starting again. A finished book taken off
// Finished other than to read it again (no status, or Saved) wasn't
// finished after all: its last finish no longer counts, if it was this
// year. Earlier years are settled: a book read last year and saved to read
// again stays read last year.
func recordStatusEvents(q queryExecer, userID int64, uuid, prev, next string) error {
	if prev == "finished" && (next == "" || next == "wanted") {
		_, err := q.Exec(`DELETE FROM reading_events WHERE user_id = ? AND book_uuid = ? AND kind = 'finished' AND at >= ?
			AND at = (SELECT MAX(at) FROM reading_events WHERE user_id = ? AND book_uuid = ? AND kind = 'finished')`,
			userID, uuid, yearStartUnix(time.Now()), userID, uuid)
		return err
	}
	started := next == "reading" && prev != "reading" ||
		next == "finished" && prev != "reading" && prev != "finished"
	finished := next == "finished" && prev != "finished"
	if !started && !finished {
		return nil
	}
	if p, err := prefsOf(q, userID); err != nil || !p.TrackReading {
		return err
	}
	at := nowUnix()
	for _, ev := range []struct {
		kind string
		on   bool
	}{{"started", started}, {"finished", finished}} {
		if !ev.on {
			continue
		}
		if _, err := q.Exec(`INSERT OR IGNORE INTO reading_events (user_id, book_uuid, kind, at) VALUES (?, ?, ?, ?)`,
			userID, uuid, ev.kind, at); err != nil {
			return err
		}
	}
	return nil
}

// yearStartUnix: when now's year began, in the server's time zone (the
// readers' own isn't known here; it differs only around New Year).
func yearStartUnix(now time.Time) int64 {
	return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location()).Unix()
}

type activityEntry struct {
	Day      string `json:"day"`
	BookUUID string `json:"book_uuid"`
	Pages    int    `json:"pages"`
	Seconds  int    `json:"seconds"`
}

// validDay: d is a date (YYYY-MM-DD) near enough to now.
func validDay(d string, now time.Time) bool {
	if !activityDayRe.MatchString(d) {
		return false
	}
	t, err := time.Parse(dayLayout, d)
	return err == nil && t.After(now.Add(-activityMaxAge)) && t.Before(now.Add(activityMaxAhead))
}

// PUT /api/me/activity: {client_id, entries: [{day, book_uuid, pages,
// seconds}]}, a browser's totals for the days and books it has. Books no
// longer in the library are skipped (a device that kept them offline would
// otherwise send them forever).
func activityPutHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct {
		ClientID string          `json:"client_id"`
		Entries  []activityEntry `json:"entries"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !activityClientRe.MatchString(req.ClientID) || len(req.Entries) > maxActivityEntries {
		http.Error(w, "invalid activity", http.StatusBadRequest)
		return
	}
	now := time.Now()
	for _, e := range req.Entries {
		if !validDay(e.Day, now) || e.BookUUID == "" || len(e.BookUUID) > 64 ||
			e.Pages < 0 || e.Pages > maxActivityPages || e.Seconds < 0 || e.Seconds > maxActivitySeconds {
			http.Error(w, "invalid activity", http.StatusBadRequest)
			return
		}
	}
	prefs, err := userPrefsFor(u)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if !prefs.TrackReading {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	tx, err := appDB.Begin()
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	for _, e := range req.Entries {
		if (e.Pages == 0 && e.Seconds == 0) || !bookExists(e.BookUUID) {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO reading_activity (user_id, day, book_uuid, client_id, pages, seconds, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (user_id, day, book_uuid, client_id) DO UPDATE SET
				pages = MAX(pages, excluded.pages), seconds = MAX(seconds, excluded.seconds), updated_at = excluded.updated_at`,
			u.ID, e.Day, e.BookUUID, req.ClientID, e.Pages, e.Seconds, nowMilli()); err != nil {
			log.Println("save activity:", err)
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/me/activity: forget the user's reading stats.
func activityDeleteHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	for _, table := range []string{"reading_activity", "reading_events"} {
		if _, err := appDB.Exec(`DELETE FROM `+table+` WHERE user_id = ?`, u.ID); err != nil {
			http.Error(w, "could not delete", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

type dayActivity struct {
	Day     string `json:"day"`
	Pages   int    `json:"pages"`
	Seconds int    `json:"seconds"`
}

type streaks struct {
	Current int `json:"current"`
	Longest int `json:"longest"`
}

func (d dayActivity) active() bool { return d.Pages > 0 || d.Seconds >= streakMinSeconds }

// streaksOf: the runs of days in a row with reading in days (any order),
// as of today. The current run still stands on a day not read yet, as long
// as the day before was.
func streaksOf(days []dayActivity, today time.Time) streaks {
	read := map[string]bool{}
	for _, d := range days {
		if d.active() {
			read[d.Day] = true
		}
	}
	var s streaks
	for day := range read {
		t, err := time.Parse(dayLayout, day)
		if err != nil || read[t.AddDate(0, 0, -1).Format(dayLayout)] {
			continue // not the first day of a run
		}
		n := 1
		for read[t.AddDate(0, 0, n).Format(dayLayout)] {
			n++
		}
		s.Longest = max(s.Longest, n)
	}
	t := today
	if !read[t.Format(dayLayout)] {
		t = t.AddDate(0, 0, -1)
	}
	for read[t.Format(dayLayout)] {
		s.Current++
		t = t.AddDate(0, 0, -1)
	}
	return s
}

// heatmapStart: the Sunday 52 weeks before today's week; the heatmap's
// first column.
func heatmapStart(today time.Time) time.Time {
	return today.AddDate(0, 0, -int(today.Weekday())-7*heatmapWeeks)
}

type statsAnswer struct {
	Tracking bool          `json:"tracking"`
	Today    string        `json:"today"`
	From     string        `json:"from"`
	Days     []dayActivity `json:"days"` // From to Today, only days with any reading
	Streak   streaks       `json:"streak"`
	// Today's reading, and the week's (today and the 6 days before).
	TodayPages   int `json:"today_pages"`
	TodaySeconds int `json:"today_seconds"`
	WeekPages    int `json:"week_pages"`
	WeekSeconds  int `json:"week_seconds"`
	Goal         struct {
		DailyPages  int  `json:"daily_pages"`
		YearlyBooks int  `json:"yearly_books"`
		Notice      bool `json:"notice"` // goal_notice: told in the reader
	} `json:"goal"`
	Year struct {
		Year     int    `json:"year"`
		Started  int    `json:"started"`
		Finished int    `json:"finished"`
		Books    []Book `json:"books"` // finished this year, latest first
	} `json:"year"`
	Totals struct {
		Pages    int `json:"pages"`
		Seconds  int `json:"seconds"`
		Finished int `json:"finished"`
	} `json:"totals"`
}

// GET /api/me/stats?today=YYYY-MM-DD&tz=M: the user's reading stats, as of
// the day it is where they are (today), whose clock is M minutes behind UTC
// (JavaScript's getTimezoneOffset), for when this year began there.
func statsHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	now := time.Now()
	todayStr := r.URL.Query().Get("today")
	if !validDay(todayStr, now) {
		http.Error(w, "today: YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	today, _ := time.Parse(dayLayout, todayStr)
	tz, err := strconv.Atoi(r.URL.Query().Get("tz"))
	if err != nil || tz < -16*60 || tz > 16*60 {
		tz = 0
	}
	ans, err := userStats(u, today, tz)
	if err != nil {
		log.Println("stats:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, ans)
}

func userStats(u *User, today time.Time, tzOffsetMin int) (*statsAnswer, error) {
	prefs, err := userPrefsFor(u)
	if err != nil {
		return nil, err
	}
	ans := &statsAnswer{Tracking: prefs.TrackReading, Today: today.Format(dayLayout), Days: []dayActivity{}}
	ans.Goal.DailyPages, ans.Goal.YearlyBooks, ans.Goal.Notice = prefs.DailyPages, prefs.YearlyBooks, prefs.GoalNotice
	from := heatmapStart(today)
	ans.From = from.Format(dayLayout)
	weekFrom := today.AddDate(0, 0, -6).Format(dayLayout)

	// Every day read (for the longest streak), with the heatmap's among them.
	rows, err := appDB.Query(`SELECT day, SUM(pages), SUM(seconds) FROM reading_activity
		WHERE user_id = ? AND day <= ? GROUP BY day ORDER BY day`, u.ID, ans.Today)
	if err != nil {
		return nil, err
	}
	var all []dayActivity
	for rows.Next() {
		var d dayActivity
		if err := rows.Scan(&d.Day, &d.Pages, &d.Seconds); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, d)
		ans.Totals.Pages += d.Pages
		ans.Totals.Seconds += d.Seconds
		if d.Day >= ans.From {
			ans.Days = append(ans.Days, d)
		}
		if d.Day >= weekFrom {
			ans.WeekPages += d.Pages
			ans.WeekSeconds += d.Seconds
		}
		if d.Day == ans.Today {
			ans.TodayPages, ans.TodaySeconds = d.Pages, d.Seconds
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ans.Streak = streaksOf(all, today)

	// This year where the user is: from midnight on 1 January there.
	ans.Year.Year = today.Year()
	yearStart := time.Date(today.Year(), 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(tzOffsetMin) * time.Minute).Unix()
	if err := appDB.QueryRow(`SELECT COUNT(DISTINCT book_uuid) FROM reading_events WHERE user_id = ? AND kind = 'started' AND at >= ?`,
		u.ID, yearStart).Scan(&ans.Year.Started); err != nil {
		return nil, err
	}
	if err := appDB.QueryRow(`SELECT COUNT(DISTINCT book_uuid) FROM reading_events WHERE user_id = ? AND kind = 'finished'`,
		u.ID).Scan(&ans.Totals.Finished); err != nil {
		return nil, err
	}
	rows, err = appDB.Query(`SELECT book_uuid FROM reading_events WHERE user_id = ? AND kind = 'finished' AND at >= ?
		GROUP BY book_uuid ORDER BY MAX(at) DESC`, u.ID, yearStart)
	if err != nil {
		return nil, err
	}
	var finished []string
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			rows.Close()
			return nil, err
		}
		finished = append(finished, uuid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ans.Year.Finished = len(finished)
	if ans.Year.Books, err = booksByUUIDs(finished); err != nil {
		return nil, err
	}
	return ans, nil
}

// GET /me/stats: the page (web/stats.ts fills it).
func statsPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "stats.html", map[string]any{"Title": SiteTitle}), "render stats")
}

func registerStatsRoutes() {
	http.HandleFunc("GET /me/stats", requireUser(statsPage))
	http.HandleFunc("GET /api/me/stats", requireUser(statsHandler))
	http.HandleFunc("PUT /api/me/activity", requireUser(activityPutHandler))
	http.HandleFunc("DELETE /api/me/activity", requireUser(activityDeleteHandler))
}
