package hardcoversync

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// testSchema is the part of the app's database the Service uses, as the
// app's migrations leave it.
const testSchema = `
CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT NOT NULL, deleted_at INTEGER);
CREATE TABLE reading_status (user_id INTEGER NOT NULL, book_uuid TEXT NOT NULL, status TEXT NOT NULL,
	updated_at INTEGER NOT NULL, PRIMARY KEY (user_id, book_uuid));
CREATE TABLE reading_events (user_id INTEGER NOT NULL, book_uuid TEXT NOT NULL, kind TEXT NOT NULL, at INTEGER NOT NULL);
CREATE TABLE aggregate_favorites (user_id INTEGER NOT NULL, kind TEXT NOT NULL, name TEXT NOT NULL COLLATE NOCASE,
	origin TEXT NOT NULL DEFAULT 'local', hc_ref INTEGER, removed_at INTEGER, created_at INTEGER NOT NULL, hc_id INTEGER,
	PRIMARY KEY (user_id, kind, name));
CREATE TABLE hardcover_authors (name TEXT PRIMARY KEY COLLATE NOCASE, hc_id INTEGER NOT NULL DEFAULT 0, checked_at INTEGER NOT NULL);
CREATE TABLE hardcover_links (user_id INTEGER PRIMARY KEY, hc_user_id INTEGER NOT NULL, hc_username TEXT NOT NULL DEFAULT '',
	access_token TEXT NOT NULL, refresh_token TEXT NOT NULL, access_expires INTEGER NOT NULL, scope TEXT NOT NULL DEFAULT '',
	linked_at INTEGER NOT NULL, broken TEXT NOT NULL DEFAULT '', share_all INTEGER NOT NULL DEFAULT 0,
	share_progress INTEGER NOT NULL DEFAULT 0, share_dates INTEGER NOT NULL DEFAULT 0, share_follows INTEGER NOT NULL DEFAULT 0,
	unfollow_default INTEGER NOT NULL DEFAULT 1, relink_notified TEXT NOT NULL DEFAULT '');
CREATE TABLE hardcover_shared (user_id INTEGER NOT NULL, book_uuid TEXT NOT NULL, shared_at INTEGER NOT NULL, PRIMARY KEY (user_id, book_uuid));
CREATE TABLE hardcover_unshared (user_id INTEGER NOT NULL, book_uuid TEXT NOT NULL, unshared_at INTEGER NOT NULL, PRIMARY KEY (user_id, book_uuid));
CREATE TABLE hardcover_books (book_uuid TEXT PRIMARY KEY, hc_book_id INTEGER NOT NULL DEFAULT 0, hc_edition_id INTEGER NOT NULL DEFAULT 0,
	pages INTEGER NOT NULL DEFAULT 0, resolved_at INTEGER NOT NULL, slug TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '');
CREATE TABLE hardcover_user_books (user_id INTEGER NOT NULL, book_uuid TEXT NOT NULL, hc_book_id INTEGER NOT NULL,
	hc_edition_id INTEGER NOT NULL DEFAULT 0, pages INTEGER NOT NULL DEFAULT 0, slug TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '', chosen_at INTEGER NOT NULL, PRIMARY KEY (user_id, book_uuid));
CREATE TABLE hardcover_sync (user_id INTEGER NOT NULL, book_uuid TEXT NOT NULL, due_at INTEGER NOT NULL DEFAULT 0,
	attempts INTEGER NOT NULL DEFAULT 0, changes INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
	started_on TEXT NOT NULL DEFAULT '', finished_on TEXT NOT NULL DEFAULT '', sent_status INTEGER NOT NULL DEFAULT 0,
	sent_pages INTEGER NOT NULL DEFAULT 0, sent_at INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (user_id, book_uuid));
CREATE TABLE hardcover_follow_queue (user_id INTEGER NOT NULL, name TEXT NOT NULL COLLATE NOCASE, op TEXT NOT NULL,
	hc_id INTEGER NOT NULL DEFAULT 0, due_at INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (user_id, name));
CREATE TABLE hardcover_follows (user_id INTEGER NOT NULL, hc_id INTEGER NOT NULL, hc_name TEXT NOT NULL, follow_id INTEGER NOT NULL,
	state TEXT NOT NULL, local_name TEXT NOT NULL DEFAULT '', candidates TEXT NOT NULL DEFAULT '[]', seen_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, hc_id));
CREATE TABLE hardcover_aggregate_matches (user_id INTEGER NOT NULL, kind TEXT NOT NULL, hc_id INTEGER NOT NULL,
	local_name TEXT COLLATE NOCASE, decision TEXT NOT NULL, created_at INTEGER NOT NULL, UNIQUE (user_id, kind, hc_id, local_name));
`

// fakeHost is the app: books with ISBNs, and reading statuses.
type fakeHost struct {
	mu       sync.Mutex
	isbns    map[string][]string
	status   map[string]string // by book
	progress float64
	claimed  []string
}

func (h *fakeHost) Syncable(uuid string) bool { _, ok := h.isbns[uuid]; return ok }
func (h *fakeHost) BookKeys(uuid string) ([]string, int64, string, error) {
	return h.isbns[uuid], 0, "", nil
}
func (h *fakeHost) BookTitle(uuid string) (string, string, bool) {
	return "Title " + uuid, "A & B", true
}
func (h *fakeHost) Placeholder(string) (int64, string, string, bool) { return 0, "", "", false }
func (h *fakeHost) ClaimPlaceholder(uuid string, _ hardcover.Edition, _ int64) {
	h.mu.Lock()
	h.claimed = append(h.claimed, uuid)
	h.mu.Unlock()
}
func (h *fakeHost) ReadStatus(_ int64, uuid string) string { return h.status[uuid] }
func (h *fakeHost) Progress(int64, string) (float64, time.Time, bool) {
	return h.progress, time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC), h.progress > 0
}
func (h *fakeHost) Seal(c, plain string) (string, error) { return c + "|" + plain, nil }
func (h *fakeHost) Open(c, sealed string) (string, error) {
	return strings.TrimPrefix(sealed, c+"|"), nil
}
func (h *fakeHost) ReplaceRelinkNotice(*sql.Tx, int64, string) error { return nil }
func (h *fakeHost) ClearRelinkNotice(int64) error                    { return nil }
func (h *fakeHost) ResolveAuthor(name string) (string, []string)     { return name, nil }
func (h *fakeHost) LibraryAuthors() []LibraryAuthor                  { return nil }
func (h *fakeHost) AuthorAsBrowsed(name string) (string, bool)       { return name, true }
func (h *fakeHost) MaxFavorites() int                                { return 500 }
func (h *fakeHost) TouchFavorites(*sql.Tx, int64, int64) error       { return nil }
func (h *fakeHost) FavoritesChanged(int64)                           {}

// fakeAPI is Hardcover's GraphQL API for one user: each operation answers
// its queued replies in turn, the last one again and again.
type fakeAPI struct {
	srv   *httptest.Server
	mu    sync.Mutex
	reply map[string][]string
	vars  map[string]map[string]any
	ops   []string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{reply: map[string][]string{}, vars: map[string]map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hc_at_good" {
			w.WriteHeader(401)
			return
		}
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		op, _, _ := strings.Cut(strings.Fields(req.Query)[1], "(")
		f.mu.Lock()
		f.ops = append(f.ops, op)
		f.vars[op] = req.Variables
		body := `{"errors":[{"message":"unexpected ` + op + `"}]}`
		if q := f.reply[op]; len(q) > 0 {
			body = q[0]
			if len(q) > 1 {
				f.reply[op] = q[1:]
			}
		}
		f.mu.Unlock()
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) did(op string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.ops {
		if o == op {
			return true
		}
	}
	return false
}

// newTestService is a Service on a fresh database, with user 1 linked to
// Hardcover user 7, sharing every book and their progress.
func newTestService(t *testing.T) (*Service, *fakeHost, *fakeAPI) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(testSchema); err != nil {
		t.Fatal(err)
	}
	h := &fakeHost{isbns: map[string][]string{"book-1": {"9780316129084"}, "book-2": {"9780000000002"}}, status: map[string]string{}}
	if _, err := db.Exec(`INSERT INTO users (id, username) VALUES (1, 'reader')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO hardcover_links (user_id, hc_user_id, access_token, refresh_token, access_expires, linked_at, share_all, share_progress)
		VALUES (1, 7, ?, ?, ?, 1, 1, 1)`, sealContext("access_token", 1)+"|hc_at_good", sealContext("refresh_token", 1)+"|hc_rt",
		time.Now().Add(30*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	f := newFakeAPI(t)
	s := New(Config{
		DB: db, Host: h, OAuth: &hardcover.OAuth{ClientID: "cid"}, SyncURL: f.srv.URL, UserAgent: "test",
		Budget: func() *hardcover.Budget { return hardcover.NewBudget(600, 100, 10000) },
	})
	return s, h, f
}

func syncRowOf(t *testing.T, s *Service, uuid string) (due int64, attempts int, msg string, sent int) {
	t.Helper()
	_ = s.db.QueryRow(`SELECT due_at, attempts, error, sent_status FROM hardcover_sync WHERE user_id = 1 AND book_uuid = ?`, uuid).
		Scan(&due, &attempts, &msg, &sent)
	return
}

func TestStepSendsReading(t *testing.T) {
	s, h, f := newTestService(t)
	if !s.IsLinked(1) || !s.Shared(1, "book-1") {
		t.Fatal("not linked and sharing")
	}
	h.status["book-1"], h.progress = "reading", 0.5
	f.reply["SyncEditions"] = []string{`{"data":{"editions":[{"id":11,"book_id":1,"pages":320,"book":{"slug":"one","title":"One"}}]}}`}
	f.reply["UserBook"] = []string{
		`{"data":{"user_books":[]}}`,
		`{"data":{"user_books":[{"id":99,"status_id":2,"edition_id":11,"edition":{"pages":320},"user_book_reads":[{"id":3,"progress_pages":0,"started_at":null,"finished_at":null}]}]}}`,
	}
	f.reply["InsertUserBook"] = []string{`{"data":{"insert_user_book":{"id":99,"error":null}}}`}
	f.reply["UpdateUserBookRead"] = []string{`{"data":{"update_user_book_read":{"id":3,"error":null}}}`}

	s.Touch(1, "book-1", 0)
	if ran, _ := s.Step(context.Background()); !ran {
		t.Fatal("nothing ran")
	}
	if obj := f.vars["InsertUserBook"]["object"].(map[string]any); obj["status_id"] != float64(hardcover.StatusReading) || obj["edition_id"] != 11.0 {
		t.Errorf("added as %v", obj)
	}
	if obj := f.vars["UpdateUserBookRead"]["object"].(map[string]any); obj["progress_pages"] != 160.0 {
		t.Errorf("progress sent as %v", obj)
	}
	if due, _, msg, sent := syncRowOf(t, s, "book-1"); due != 0 || msg != "" || sent != hardcover.StatusReading {
		t.Errorf("row: due %d, %q, sent %d", due, msg, sent)
	}
	if len(h.claimed) != 1 {
		t.Errorf("claimed %v", h.claimed)
	}
	if ran, next := s.Step(context.Background()); ran || !next.IsZero() {
		t.Errorf("more to do: %v %v", ran, next)
	}
}

func TestStepNotFoundAndBackoff(t *testing.T) {
	s, h, f := newTestService(t)
	h.status["book-1"], h.status["book-2"] = "wanted", "wanted"
	// book-1: Hardcover has no such ISBN. Not retried; shown as not found.
	f.reply["SyncEditions"] = []string{`{"data":{"editions":[]}}`}
	f.reply["SyncBooks"] = []string{`{"data":{"books":[]}}`}
	s.Touch(1, "book-1", 0)
	s.Step(context.Background())
	if due, _, msg, _ := syncRowOf(t, s, "book-1"); due != 0 || msg != NotFoundMsg {
		t.Errorf("not found: due %d, %q", due, msg)
	}
	if st := s.BookState(1, "book-1"); st.State != "not_found" || st.Query != "Title book-1 A" {
		t.Errorf("state %+v", st)
	}
	if p := s.ProblemBooks(1); len(p) != 1 || p[0].UUID != "book-1" {
		t.Errorf("problems %+v", p)
	}

	// book-2: Hardcover fails: tried again later, with the error shown.
	f.reply["SyncEditions"] = []string{`{"data":{"editions":[{"id":21,"book_id":2,"pages":100}]}}`}
	f.reply["UserBook"] = []string{`{"errors":[{"message":"down for maintenance"}]}`}
	s.Touch(1, "book-2", 0)
	s.Step(context.Background())
	due, attempts, msg, _ := syncRowOf(t, s, "book-2")
	if attempts != 1 || due <= time.Now().Unix() || !strings.Contains(msg, "down for maintenance") {
		t.Errorf("backoff: due %d, attempts %d, %q", due, attempts, msg)
	}
	if st := s.BookState(1, "book-2"); st.State != "failing" {
		t.Errorf("state %+v", st)
	}
	if f.did("InsertUserBook") {
		t.Error("sent despite the error")
	}
}

func TestOnlyLinkedAndShared(t *testing.T) {
	s, h, _ := newTestService(t)
	h.status["book-1"] = "reading"
	if err := s.SaveShare(1, Share{}); err != nil { // chosen books only, none chosen
		t.Fatal(err)
	}
	s.Touch(1, "book-1", 0)
	s.Touch(2, "book-1", 0) // not linked
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM hardcover_sync`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows for books not shared", n)
	}
	if err := s.SetBookShared(1, "book-1", true); err != nil {
		t.Fatal(err)
	}
	if due, _, _, _ := syncRowOf(t, s, "book-1"); due == 0 {
		t.Error("sharing a book doesn't queue it")
	}
	if err := s.SetBookShared(1, "book-1", false); err != nil {
		t.Fatal(err)
	}
	if s.Shared(1, "book-1") {
		t.Error("still shared")
	}
}

func TestQueueFollowNeedsTheUsersChoice(t *testing.T) {
	s, _, _ := newTestService(t)
	if _, err := s.db.Exec(`UPDATE hardcover_links SET scope = 'write:social'`); err != nil {
		t.Fatal(err)
	}
	count := func() (n int) {
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM hardcover_follow_queue`).Scan(&n)
		return
	}
	s.QueueFollow(1, "Ursula K. Le Guin", "follow", 0)
	if count() != 0 {
		t.Fatal("followed without the user turning it on")
	}
	s.QueueFollow(1, "Ursula K. Le Guin", "unfollow", 9) // asked for each time
	if count() != 1 {
		t.Fatal("unfollow not queued")
	}
	if err := s.SaveFollowPrefs(1, true, false); err != nil {
		t.Fatal(err)
	}
	s.QueueFollow(1, "Ursula K. Le Guin", "follow", 0) // replaces the unfollow
	var op string
	var hcID int64
	_ = s.db.QueryRow(`SELECT op, hc_id FROM hardcover_follow_queue`).Scan(&op, &hcID)
	if count() != 1 || op != "follow" || hcID != 9 {
		t.Errorf("queue: %d %q %d", count(), op, hcID)
	}
	if p := s.LoadFollowPrefs(1); !p.Share || p.UnfollowDefault || !p.CanWrite {
		t.Errorf("prefs %+v", p)
	}
}

// The app calls these as things change, whether or not sync is set up.
func TestNilService(t *testing.T) {
	var s *Service
	s.Touch(1, "b", 0)
	s.Status(1, "b", "reading")
	s.Wake()
	s.Merged("b", []string{"c"})
	s.QueueFollow(1, "A", "follow", 0)
	s.PlaceholderReplaced("p", "b")
	s.BookChanged("p", nil, 0)
	s.Resend(1, "b")
	s.CancelLink(1)
	s.Stop(time.Now())
	s.Start()
	if s.Enabled() || s.IsLinked(1) || s.Shared(1, "b") || s.View(1, "") != nil || s.Pending(1) != nil || s.WakeWaitingStars([]string{"A"}) != nil {
		t.Error("a nil Service did something")
	}
	if r, err := s.Unlink(1); r != RevokeNone || err != nil {
		t.Errorf("Unlink: %v %v", r, err)
	}
}
