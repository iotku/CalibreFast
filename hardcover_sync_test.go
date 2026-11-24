package main

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

func useTestSecretKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), secretKeyFile)
	orig := secretKeyPath
	secretKeyPath = func() string { return path }
	t.Cleanup(func() {
		secretKeyPath = orig
		secretKey.Lock()
		secretKey.key, secretKey.path = nil, ""
		secretKey.Unlock()
	})
	return path
}

func TestSecretBox(t *testing.T) {
	path := useTestSecretKey(t)
	if _, err := openSecret("a", "v1:AAAA"); err == nil {
		t.Error("opened without a key")
	}
	sealed, err := sealSecret("hardcover_links.access_token:1", "hc_at_secret")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "secret") || !strings.HasPrefix(sealed, sealPrefix) {
		t.Errorf("sealed: %q", sealed)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v %v", fi, err)
	}
	if got, err := openSecret("hardcover_links.access_token:1", sealed); err != nil || got != "hc_at_secret" {
		t.Errorf("open: %q %v", got, err)
	}
	for _, ctx := range []string{"hardcover_links.access_token:2", "hardcover_links.refresh_token:1"} {
		if _, err := openSecret(ctx, sealed); err == nil {
			t.Errorf("opened in another row's context %q", ctx)
		}
	}
	again, _ := sealSecret("x", "same")
	again2, _ := sealSecret("x", "same")
	if again == again2 {
		t.Error("the same secret sealed the same twice (nonce reused?)")
	}
	if _, err := openSecret("hardcover_links.access_token:1", sealed[:len(sealed)-4]+"AAAA"); err == nil {
		t.Error("a tampered value opened")
	}
}

// fakeHardcoverAccount is Hardcover for one user: OAuth (device sign-in,
// rotating refresh tokens) and the GraphQL calls sync makes, with a
// library that remembers what it was sent.
type fakeHardcoverAccount struct {
	srv *httptest.Server
	mu  sync.Mutex

	access, refresh string // the tokens that work now
	serial          int
	refuseRefresh   bool
	revoked         []string

	books   map[int64]*fakeUserBook // by book id
	follows string                  // FollowedAuthors' follows, as JSON (hardcover_follows_test.go)
	ops     []string
	lastObj map[string]map[string]any
}

type fakeUserBook struct {
	id, status, edition int64
	reads               []fakeRead
}

type fakeRead struct {
	id                int64
	pages             int
	started, finished string
}

func newFakeHardcoverAccount(t *testing.T) *fakeHardcoverAccount {
	f := &fakeHardcoverAccount{books: map[int64]*fakeUserBook{}, lastObj: map[string]map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHardcoverAccount) issue(w io.Writer) {
	f.serial++
	n := itoa(int64(f.serial))
	f.access, f.refresh = "hc_at_"+n, "hc_rt_"+n
	io.WriteString(w, `{"access_token":"`+f.access+`","refresh_token":"`+f.refresh+`","expires_in":604800,"scope":"read:me read:library write:library read:catalog"}`)
}

func (f *fakeHardcoverAccount) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path != "/graphql" {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/device":
			io.WriteString(w, `{"device_code":"dc","user_code":"WXYZ-0001","expires_in":600,"interval":5}`)
		case "/token":
			switch r.PostForm.Get("grant_type") {
			case "refresh_token":
				if f.refuseRefresh || r.PostForm.Get("refresh_token") != f.refresh {
					w.WriteHeader(400)
					io.WriteString(w, `{"error":"invalid_grant"}`)
					return
				}
			}
			f.issue(w)
		case "/revoke":
			f.revoked = append(f.revoked, r.PostForm.Get("token"))
			f.access, f.refresh = "", ""
		}
		return
	}
	if f.access == "" || r.Header.Get("Authorization") != "Bearer "+f.access {
		w.WriteHeader(401)
		io.WriteString(w, `{"error":"invalid_token"}`)
		return
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	op, _, _ := strings.Cut(strings.Fields(req.Query)[1], "(")
	f.ops = append(f.ops, op)
	obj, _ := req.Variables["object"].(map[string]any)
	f.lastObj[op] = obj
	num := func(v any) int64 { n, _ := v.(float64); return int64(n) }
	today := time.Now().Format(time.DateOnly)
	switch op {
	case "Me":
		io.WriteString(w, `{"data":{"me":[{"id":77,"username":"hc-reader"}]}}`)
	case "FollowedAuthors":
		io.WriteString(w, `{"data":{"follows":`+cmp.Or(f.follows, "[]")+`}}`)
	case "SyncEditions":
		io.WriteString(w, `{"data":{"editions":[{"id":501,"book_id":50,"isbn_13":"9780316129084","pages":300}]}}`)
	case "SyncBooks":
		// Book 60 is on Hardcover, by its id or slug.
		ids, _ := req.Variables["ids"].([]any)
		slugs, _ := req.Variables["slugs"].([]any)
		if slices.Contains(ids, any(float64(60))) || slices.Contains(slugs, any("the-right-book")) {
			io.WriteString(w, `{"data":{"books":[{"id":60,"slug":"the-right-book","title":"The Right Book","pages":250,"editions":[{"id":601,"pages":250}]}]}}`)
			return
		}
		io.WriteString(w, `{"data":{"books":[]}}`)
	case "SearchBooks":
		io.WriteString(w, `{"data":{"search":{"results":{"hits":[{"document":{"id":"60"}}]}}}}`)
	case "BooksForLookup":
		io.WriteString(w, `{"data":{"books":[{"id":60,"slug":"the-right-book","title":"The Right Book","release_date":"2021-03-01","contributions":[{"author":{"name":"A. Writer"}}]}]}}`)
	case "UserBook":
		b := f.books[num(req.Variables["book"])]
		if b == nil {
			io.WriteString(w, `{"data":{"user_books":[]}}`)
			return
		}
		reads := []map[string]any{}
		if n := len(b.reads); n > 0 {
			rd := b.reads[n-1]
			m := map[string]any{"id": rd.id, "progress_pages": rd.pages, "started_at": rd.started, "finished_at": nil}
			if rd.finished != "" {
				m["finished_at"] = rd.finished
			}
			reads = append(reads, m)
		}
		out, _ := json.Marshal(map[string]any{"data": map[string]any{"user_books": []any{map[string]any{
			"id": b.id, "status_id": b.status, "edition_id": b.edition, "edition": map[string]any{"pages": 300}, "user_book_reads": reads}}}})
		w.Write(out)
	case "InsertUserBook", "UpdateUserBook":
		var b *fakeUserBook
		if op == "InsertUserBook" {
			b = &fakeUserBook{id: 900 + num(obj["book_id"]), edition: num(obj["edition_id"])}
			f.books[num(obj["book_id"])] = b
		} else {
			for _, x := range f.books {
				if x.id == num(req.Variables["id"]) {
					b = x
				}
			}
		}
		st := num(obj["status_id"])
		// As Hardcover does: reading starts a read, read finishes it.
		switch {
		case st == 2 && (len(b.reads) == 0 || b.reads[len(b.reads)-1].finished != ""):
			b.reads = append(b.reads, fakeRead{id: int64(len(b.reads) + 1), started: today})
		case st == 3:
			if len(b.reads) == 0 || b.reads[len(b.reads)-1].finished != "" {
				b.reads = append(b.reads, fakeRead{id: int64(len(b.reads) + 1), started: today})
			}
			b.reads[len(b.reads)-1].finished = today
		}
		b.status = st
		io.WriteString(w, `{"data":{"`+map[string]string{"InsertUserBook": "insert_user_book", "UpdateUserBook": "update_user_book"}[op]+`":{"id":`+itoa(b.id)+`,"error":null}}}`)
	case "UpdateUserBookRead":
		for _, b := range f.books {
			for i := range b.reads {
				if b.reads[i].id == num(req.Variables["id"]) {
					if p, ok := obj["progress_pages"]; ok {
						b.reads[i].pages = int(num(p))
					}
					if s, ok := obj["started_at"].(string); ok {
						b.reads[i].started = s
					}
					if s, ok := obj["finished_at"].(string); ok {
						b.reads[i].finished = s
					}
				}
			}
		}
		io.WriteString(w, `{"data":{"update_user_book_read":{"id":1,"error":null}}}`)
	default:
		io.WriteString(w, `{"errors":[{"message":"unexpected"}]}`)
	}
}

func (f *fakeHardcoverAccount) opsSince(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops[n:]...)
}

func setupHardcoverSync(t *testing.T) (*User, []string, *fakeHardcoverAccount) {
	t.Helper()
	u, books := setupLibraryAndUser(t, 2)
	useTestSecretKey(t)
	f := newFakeHardcoverAccount(t)
	origOAuth, origRevoker, origURL, origClient, origPoll, origBudget := hcOAuth, hcRevoker, hcSyncURL, hcClient, hcPollInterval, hcUserBudget
	hcUserBudget = func() *hardcover.Budget { return hardcover.NewBudget(600, 100, 10000) }
	hcOAuth = &hardcover.OAuth{ClientID: "cid", DeviceURL: f.srv.URL + "/device", TokenURL: f.srv.URL + "/token",
		RevokeURL: f.srv.URL + "/revoke", UserAgent: "CalibreFast/test"}
	hcRevoker = hcOAuth
	hcSyncURL, hcClient = f.srv.URL+"/graphql", nil
	hcPollInterval = func(time.Duration) time.Duration { return 5 * time.Millisecond }
	t.Cleanup(func() {
		stopHardcoverSync(time.Now().Add(time.Second))
		hcOAuth, hcRevoker, hcSyncURL, hcClient, hcPollInterval, hcUserBudget = origOAuth, origRevoker, origURL, origClient, origPoll, origBudget
		hcLoadLinked()
		hcSync.Lock()
		hcSync.pending, hcSync.tokens = map[int64]*hcPendingLink{}, map[int64]*sync.Mutex{}
		hcSync.Unlock()
	})
	hcLoadLinked()
	// The first book has an ISBN Hardcover knows.
	if _, err := calibreDB.Exec(`INSERT INTO identifiers (book, type, val) SELECT id, 'isbn', '978-0-316-12908-4' FROM books WHERE uuid = ?`, books[0]); err != nil {
		t.Fatal(err)
	}
	return u, books, f
}

func linkHardcover(t *testing.T, u *User) {
	t.Helper()
	p, err := hcStartLink(u.ID)
	if err != nil || p.UserCode != "WXYZ-0001" {
		t.Fatalf("start: %+v %v", p, err)
	}
	if v := hcView(u, ""); v.State != "pending" || v.UserCode != "WXYZ-0001" || v.VerifyURL != hardcover.VerificationURL || v.OpenURL != hardcover.VerificationURL {
		t.Errorf("pending view: %+v", v)
	}
	for i := 0; !hcIsLinked(u.ID); i++ {
		if i > 400 {
			t.Fatalf("not linked: %+v", hcView(u, ""))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// step runs the worker until nothing is due now.
func step(t *testing.T) {
	t.Helper()
	for range 10 {
		if ran, _ := hcSyncStep(context.Background()); !ran {
			return
		}
	}
	t.Fatal("still running after 10 steps")
}

func TestHardcoverSyncLinkAndSend(t *testing.T) {
	u, books, f := setupHardcoverSync(t)

	// Nothing is recorded before linking.
	if err := setStatus(u.ID, books[0], "wanted", "manual"); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_sync`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows queued for a user without Hardcover", n)
	}

	linkHardcover(t, u)
	var acc, ref string
	_ = appDB.QueryRow(`SELECT access_token, refresh_token FROM hardcover_links WHERE user_id = ?`, u.ID).Scan(&acc, &ref)
	if strings.Contains(acc+ref, "hc_") || acc == "" {
		t.Errorf("tokens stored in the clear: %q %q", acc, ref)
	}
	v := hcView(u, "")
	if v.State != "linked" || v.Username != "hc-reader" {
		t.Errorf("linked view: %+v", v)
	}
	rec := httptest.NewRecorder()
	hardcoverStatusHandler(rec, asUser(httptest.NewRequest("GET", "/api/me/hardcover", nil), u))
	if body := rec.Body.String(); !strings.Contains(body, `"state":"linked"`) || strings.Contains(body, "hc_at_") || strings.Contains(body, "hc_rt_") {
		t.Errorf("status JSON: %s", body)
	}

	// Linking shares nothing: a book not chosen sends nothing, not even a
	// lookup, and leaves no note to send later.
	ops0 := len(f.ops)
	if err := setStatus(u.ID, books[0], "reading", "manual"); err != nil {
		t.Fatal(err)
	}
	hcSyncTouch(u.ID, books[0], 0)
	step(t)
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_sync`).Scan(&n)
	if got := f.opsSince(ops0); len(got) != 0 || n != 0 || f.books[50] != nil {
		t.Fatalf("an unshared book was sent or noted: ops %v, %d rows", got, n)
	}

	// Shared: sent at once as it stands, as Currently Reading with its
	// edition. Progress and dates aren't chosen yet.
	if err := hcSetBookShared(u.ID, books[0], true); err != nil {
		t.Fatal(err)
	}
	if _, err := appDB.Exec(`INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at) VALUES (?, ?, 'epub', 'x', 0.2, ?)`,
		u.ID, books[0], time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	step(t)
	b := f.books[50]
	if b == nil || b.status != 2 || b.edition != 501 || len(b.reads) != 1 {
		t.Fatalf("on Hardcover: %+v (ops %v)", b, f.ops)
	}
	if b.reads[0].pages != 0 || f.lastObj["UpdateUserBookRead"] != nil {
		t.Errorf("progress sent without being chosen: %+v %v", b.reads[0], f.lastObj["UpdateUserBookRead"])
	}

	// Progress, once chosen, waits a few minutes, then goes as pages,
	// short of the end.
	if err := hcSaveShare(u.ID, hcShare{Progress: true, Dates: true}); err != nil {
		t.Fatal(err)
	}
	_, _ = appDB.Exec(`UPDATE reading_progress SET percent = 0.5`)
	hcSyncTouch(u.ID, books[0], hcProgressDelay)
	ops := len(f.ops)
	step(t)
	if got := f.opsSince(ops); len(got) != 0 {
		t.Errorf("progress sent before its delay: %v", got)
	}
	_, _ = appDB.Exec(`UPDATE hardcover_sync SET due_at = ?`, time.Now().Unix())
	step(t)
	if b.reads[0].pages != 150 {
		t.Errorf("progress pages %d, want 150 (ops %v)", b.reads[0].pages, f.opsSince(ops))
	}
	_, _ = appDB.Exec(`UPDATE reading_progress SET percent = 0.999`)
	hcSyncTouch(u.ID, books[0], 0)
	step(t)
	if b.reads[0].pages != 299 || b.status != 2 {
		t.Errorf("near the end: %d pages, status %d: the last page would finish it on Hardcover", b.reads[0].pages, b.status)
	}

	// Finishing marks it Read; nothing new is sent when nothing changed.
	if err := setStatus(u.ID, books[0], "finished", "manual"); err != nil {
		t.Fatal(err)
	}
	step(t)
	if b.status != 3 || b.reads[0].finished == "" {
		t.Errorf("finished: %+v", b)
	}
	ops = len(f.ops)
	hcSyncTouch(u.ID, books[0], 0)
	step(t)
	if got := f.opsSince(ops); len(got) != 1 || got[0] != "UserBook" {
		t.Errorf("unchanged book sent again: %v", got)
	}

	// Clearing a status here deletes nothing there.
	if err := setStatus(u.ID, books[0], "", "manual"); err != nil {
		t.Fatal(err)
	}
	ops = len(f.ops)
	step(t)
	if got := f.opsSince(ops); len(got) != 0 || b.status != 3 {
		t.Errorf("a cleared status was sent: %v", got)
	}

	// No longer shared: nothing more is sent, and nothing waits.
	if err := setStatus(u.ID, books[0], "reading", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := hcSetBookShared(u.ID, books[0], false); err != nil {
		t.Fatal(err)
	}
	ops = len(f.ops)
	hcSyncTouch(u.ID, books[0], 0)
	step(t)
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_sync WHERE book_uuid = ?`, books[0]).Scan(&n)
	if got := f.opsSince(ops); len(got) != 0 || n != 0 || b.status != 3 {
		t.Errorf("an unshared book was sent: %v, %d rows", got, n)
	}
	// A row queued before it was unshared isn't sent either.
	_, _ = appDB.Exec(`INSERT INTO hardcover_sync (user_id, book_uuid, due_at) VALUES (?, ?, ?)`, u.ID, books[0], time.Now().Unix())
	step(t)
	if got := f.opsSince(ops); len(got) != 0 {
		t.Errorf("a stale row was sent: %v", got)
	}

	// Every book, once chosen: a book Hardcover doesn't know is skipped,
	// and said so. Going back to chosen books drops what waited for others.
	if err := hcSaveShare(u.ID, hcShare{All: true}); err != nil {
		t.Fatal(err)
	}
	if err := setStatus(u.ID, books[1], "reading", "manual"); err != nil {
		t.Fatal(err)
	}
	step(t)
	if s := hcSummary(u.ID); s.Skipped != 1 || s.Waiting != 0 {
		t.Errorf("summary: %+v", s)
	}
	hcSyncTouch(u.ID, books[0], time.Hour)
	if err := hcSaveShare(u.ID, hcShare{}); err != nil {
		t.Fatal(err)
	}
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_sync`).Scan(&n)
	if n != 0 {
		t.Errorf("%d rows left for books no longer shared", n)
	}

	// Unlinking forgets the choices too.
	_ = hcSetBookShared(u.ID, books[0], true)
	if _, err := hcUnlinkAccount(u.ID); err != nil {
		t.Fatal(err)
	}
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_shared`).Scan(&n)
	if n != 0 {
		t.Errorf("choices kept after unlinking")
	}
}

func TestHardcoverShareAPI(t *testing.T) {
	u, books, _ := setupHardcoverSync(t)
	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/api/me/books/"+books[0]+"/hardcover", strings.NewReader(body))
		req.SetPathValue("uuid", books[0])
		rec := httptest.NewRecorder()
		hardcoverShareHandler(rec, asUser(req, u))
		return rec
	}
	if rec := put(`{"share":true}`); rec.Code != http.StatusConflict {
		t.Errorf("not linked: %d", rec.Code)
	}
	linkHardcover(t, u)
	if rec := put(`{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no choice: %d", rec.Code)
	}
	if rec := put(`{"share":true}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"share":true`) || !hcIsBookShared(u.ID, books[0]) {
		t.Errorf("share: %d %s", rec.Code, rec.Body)
	}
	if rec := put(`{"share":false}`); rec.Code != 200 || hcIsBookShared(u.ID, books[0]) {
		t.Errorf("unshare: %d %s", rec.Code, rec.Body)
	}
	mux := http.NewServeMux()
	registerHardcoverSyncRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, asUser(httptest.NewRequest("POST", "/api/me/books/"+books[0]+"/hardcover", strings.NewReader(`{"share":true}`)), u))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/me/books/"+books[0]+"/hardcover", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET signed out: %d", rec.Code)
	}
	// The book's controls offer the choice only to linked users.
	if v := hcView(u, ""); v.State != "linked" || v.Share.All {
		t.Errorf("view: %+v", v)
	}
}

// Sharing every book, a book left out isn't sent, nor backfilled, until
// shared again; a merge carries the choice to the book kept.
func TestHardcoverShareAllLeaveOut(t *testing.T) {
	u, books, _ := setupHardcoverSync(t)
	linkHardcover(t, u)
	if err := hcSaveShare(u.ID, hcShare{All: true}); err != nil {
		t.Fatal(err)
	}
	if !hcShared(u.ID, books[0]) || !hcShared(u.ID, books[1]) {
		t.Fatal("every book should be shared")
	}
	if err := setStatus(u.ID, books[0], "reading", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := hcSetBookShared(u.ID, books[0], false); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_sync WHERE book_uuid = ?`, books[0]).Scan(&n)
	if hcShared(u.ID, books[0]) || !hcShared(u.ID, books[1]) || n != 0 {
		t.Errorf("left out: shared=%v other=%v, %d rows waiting", hcShared(u.ID, books[0]), hcShared(u.ID, books[1]), n)
	}
	// Not queued by a change, nor by a backfill.
	hcSyncTouch(u.ID, books[0], 0)
	if _, err := hcSyncBackfill(u.ID); err != nil {
		t.Fatal(err)
	}
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_sync WHERE book_uuid = ?`, books[0]).Scan(&n)
	if n != 0 {
		t.Errorf("a book left out was queued")
	}
	if v := hcView(u, ""); len(v.Unshared) != 1 || v.Unshared[0].UUID != books[0] {
		t.Errorf("account page's list: %+v", v.Unshared)
	}
	// Going back to chosen books and then to every book keeps it out.
	_ = hcSaveShare(u.ID, hcShare{})
	_ = hcSaveShare(u.ID, hcShare{All: true})
	if hcShared(u.ID, books[0]) {
		t.Errorf("left out forgotten when sharing every book again")
	}
	// A merge: the book kept is left out too.
	hcSyncMerged(books[1], []string{books[0]})
	if hcShared(u.ID, books[1]) {
		t.Errorf("merged book should be left out")
	}
	// Shared again: sent again.
	if err := hcSetBookShared(u.ID, books[0], true); err != nil {
		t.Fatal(err)
	}
	if !hcShared(u.ID, books[0]) {
		t.Errorf("shared again, but not shared")
	}
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_unshared WHERE book_uuid = ?`, books[0]).Scan(&n)
	if n != 0 {
		t.Errorf("still listed as left out")
	}
	// Unlinking forgets it.
	if _, err := hcUnlinkAccount(u.ID); err != nil {
		t.Fatal(err)
	}
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_unshared`).Scan(&n)
	if n != 0 {
		t.Errorf("left-out books kept after unlinking")
	}
}

// Choosing books, turning one off records nothing to leave out.
func TestHardcoverUnshareChosenOnly(t *testing.T) {
	u, books, _ := setupHardcoverSync(t)
	linkHardcover(t, u)
	_ = hcSetBookShared(u.ID, books[0], true)
	_ = hcSetBookShared(u.ID, books[0], false)
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_unshared`).Scan(&n)
	if n != 0 {
		t.Errorf("unsharing a chosen book left it out of every book")
	}
	_ = hcSaveShare(u.ID, hcShare{All: true})
	if !hcShared(u.ID, books[0]) {
		t.Errorf("sharing every book should include it")
	}
}

func TestHardcoverSyncTokens(t *testing.T) {
	u, books, f := setupHardcoverSync(t)
	linkHardcover(t, u)
	if err := hcSaveShare(u.ID, hcShare{All: true}); err != nil {
		t.Fatal(err)
	}
	first := f.refresh

	// An access token about to expire is renewed, and the new pair kept.
	_, _ = appDB.Exec(`UPDATE hardcover_links SET access_expires = ?`, time.Now().Add(time.Hour).Unix())
	if err := setStatus(u.ID, books[0], "wanted", "manual"); err != nil {
		t.Fatal(err)
	}
	step(t)
	if f.refresh == first || f.books[50] == nil || f.books[50].status != 1 {
		t.Fatalf("not renewed or not sent: %+v", f.books[50])
	}
	l, _ := hcLoadAccount(u.ID)
	if got, err := openSecret(hcSealContext("refresh_token", u.ID), l.refresh); err != nil || got != f.refresh {
		t.Errorf("the new refresh token wasn't kept: %v", err)
	}

	// Refused (revoked on Hardcover): the link is marked broken, and
	// nothing more is tried until it's linked again.
	f.mu.Lock()
	f.access, f.refuseRefresh = "hc_at_revoked", true
	f.mu.Unlock()
	if err := setStatus(u.ID, books[0], "reading", "manual"); err != nil {
		t.Fatal(err)
	}
	step(t)
	if v := hcView(u, ""); v.State != "broken" || hcIsLinked(u.ID) {
		t.Errorf("after refusal: %+v", v)
	}
	var acc, ref string
	_ = appDB.QueryRow(`SELECT access_token, refresh_token FROM hardcover_links WHERE user_id = ?`, u.ID).Scan(&acc, &ref)
	if acc != "" || ref != "" {
		t.Errorf("a broken link kept its tokens")
	}
	ops := len(f.ops)
	if ran, _ := hcSyncStep(context.Background()); ran || len(f.opsSince(ops)) != 0 {
		t.Error("a broken link was tried again")
	}

	// Unlinking revokes the sign-in and forgets everything.
	// Unlinking a broken link: nothing kept to revoke, and it's gone.
	f.mu.Lock()
	f.refuseRefresh = false
	f.mu.Unlock()
	if revoke, err := hcUnlinkAccount(u.ID); err != nil || revoke != hcRevokeNone {
		t.Fatal(revoke, err)
	}
	var n int
	_ = appDB.QueryRow(`SELECT (SELECT COUNT(*) FROM hardcover_links) + (SELECT COUNT(*) FROM hardcover_sync)`).Scan(&n)
	if n != 0 || len(f.revoked) != 0 {
		t.Errorf("after unlink: %d rows, revoked %v", n, f.revoked)
	}

	// A working link is revoked on Hardcover when unlinked.
	linkHardcover(t, u)
	if revoke, err := hcUnlinkAccount(u.ID); err != nil || revoke != hcRevoked || len(f.revoked) != 1 {
		t.Errorf("unlink: %v %v, revoked %v", revoke, err, f.revoked)
	}

	// Hardcover unreachable: it's still deleted here, and the user is told.
	linkHardcover(t, u)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	if revoke, err := hcUnlinkAccount(u.ID); err != nil || revoke != hcRevokeFailed {
		t.Errorf("unlink, Hardcover down: %v %v", revoke, err)
	}
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_links`).Scan(&n)
	if n != 0 {
		t.Error("the sign-in was kept because Hardcover didn't answer")
	}
}

func TestHardcoverSyncChangeWhileSending(t *testing.T) {
	u, books, _ := setupHardcoverSync(t)
	linkHardcover(t, u)
	if err := hcSetBookShared(u.ID, books[0], true); err != nil {
		t.Fatal(err)
	}
	_, _ = appDB.Exec(`DELETE FROM hardcover_sync`)
	hcSyncTouch(u.ID, books[0], hcProgressDelay)
	var due int64
	_ = appDB.QueryRow(`SELECT due_at FROM hardcover_sync`).Scan(&due)
	if time.Until(time.Unix(due, 0)) < 2*time.Minute {
		t.Errorf("progress not delayed")
	}
	hcSyncStatus(u.ID, books[0], "reading")
	_ = appDB.QueryRow(`SELECT due_at FROM hardcover_sync`).Scan(&due)
	if time.Until(time.Unix(due, 0)) > time.Second {
		t.Errorf("a status change waited for progress")
	}
	// A change made while a send is under way stays due.
	var changes int
	_ = appDB.QueryRow(`SELECT changes FROM hardcover_sync`).Scan(&changes)
	hcSyncTouch(u.ID, books[0], 0)
	_, _ = appDB.Exec(`UPDATE hardcover_sync SET due_at = 0, attempts = 0, error = '' WHERE changes = ?`, changes)
	_ = appDB.QueryRow(`SELECT due_at FROM hardcover_sync`).Scan(&due)
	if due == 0 {
		t.Error("a change made during a send was lost")
	}
	// Dates: a re-read starts anew.
	hcSyncStatus(u.ID, books[0], "finished")
	_, _ = appDB.Exec(`UPDATE hardcover_sync SET started_on = '2020-01-01'`)
	hcSyncStatus(u.ID, books[0], "reading")
	var started, finished string
	_ = appDB.QueryRow(`SELECT started_on, finished_on FROM hardcover_sync`).Scan(&started, &finished)
	if started != time.Now().Format(time.DateOnly) || finished != "" {
		t.Errorf("re-read dates: %q %q", started, finished)
	}
}

func TestDeletingUserRevokesHardcover(t *testing.T) {
	u, _, f := setupHardcoverSync(t)
	linkHardcover(t, u)
	admin, err := createUser("boss", "longenough-pw", true)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("DELETE", "/api/admin/users/"+itoa(u.ID), nil)
	req.SetPathValue("id", itoa(u.ID))
	rec := httptest.NewRecorder()
	adminDeleteUser(rec, asUser(req, admin))
	if rec.Code != http.StatusNoContent || len(f.revoked) != 1 {
		t.Errorf("delete: %d, revoked %v", rec.Code, f.revoked)
	}
}

func TestHardcoverRoutes(t *testing.T) {
	u, _, _ := setupHardcoverSync(t)
	mux := http.NewServeMux()
	registerHardcoverSyncRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/account/hardcover?action=unlink", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/account/hardcover", strings.NewReader("action=link"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusSeeOther {
		t.Errorf("signed out: %d", rec.Code)
	}
	if hcPending(u.ID) != nil {
		t.Error("a signed-out request started a sign-in")
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/me/hardcover", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status signed out: %d", rec.Code)
	}
	// Off on the server: linking is refused, but a user linked from
	// before still sees it and can unlink, which revokes it.
	linkHardcover(t, u)
	hcOAuth = nil // hcRevoker stays: the client id is still known
	post := func(action string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/account/hardcover", strings.NewReader("action="+action))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		hardcoverAccountHandler(rec, asUser(req, u))
		return rec
	}
	if rec := post("link"); rec.Code != http.StatusForbidden {
		t.Errorf("link with sync off: %d", rec.Code)
	}
	if rec := post("settings"); rec.Code != http.StatusForbidden {
		t.Errorf("settings with sync off: %d", rec.Code)
	}
	if v := hcView(u, ""); v == nil || v.State != "disabled" {
		t.Errorf("linked user, sync off: %+v", v)
	}
	if rec := post("unlink"); rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "signed+out") {
		t.Errorf("unlink with sync off: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if l, _ := hcLoadAccount(u.ID); l != nil {
		t.Error("still linked")
	}
}

// A shared book Hardcover can't find shows so on its page, and the user
// (no editor) chooses the right book, for their own sync only.
func TestHardcoverBookMatch(t *testing.T) {
	u, books, f := setupHardcoverSync(t)
	mux := http.NewServeMux()
	registerHardcoverSyncRoutes(mux)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, asUser(httptest.NewRequest(method, path, rd), u))
		return rec
	}
	state := func() hcBookStatus {
		t.Helper()
		rec := call("GET", "/api/me/books/"+books[1]+"/hardcover", "")
		var st hcBookStatus
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &st) != nil {
			t.Fatalf("status: %d %s", rec.Code, rec.Body)
		}
		return st
	}
	base := "/api/me/books/" + books[1] + "/hardcover"
	if rec := call("GET", base, ""); rec.Code != http.StatusConflict {
		t.Errorf("status before linking: %d", rec.Code)
	}
	linkHardcover(t, u)

	// Not shared: no search, no match (a search tells Hardcover the title).
	if st := state(); st.State != "off" || st.Shared {
		t.Errorf("not shared: %+v", st)
	}
	if rec := call("GET", base+"/search?q=anything", ""); rec.Code != http.StatusConflict {
		t.Errorf("search on a book not shared: %d", rec.Code)
	}
	if rec := call("PUT", base+"/match", `{"book_id":60}`); rec.Code != http.StatusConflict {
		t.Errorf("match on a book not shared: %d", rec.Code)
	}

	// The second book has no ISBN: shared and reading, it isn't found.
	if err := setStatus(u.ID, books[1], "reading", "manual"); err != nil {
		t.Fatal(err)
	}
	if rec := call("PUT", base, `{"share":true}`); rec.Code != 200 {
		t.Fatalf("share: %d %s", rec.Code, rec.Body)
	}
	step(t)
	if st := state(); st.State != "not_found" || st.Match != nil || st.Query == "" {
		t.Fatalf("not found: %+v", st)
	}
	if p := hcProblemBooks(u.ID); len(p) != 1 || p[0].UUID != books[1] {
		t.Errorf("account page problems: %+v", p)
	}

	// Search by title, or by a pasted link; junk links search as text.
	var found []hcFoundBook
	rec := call("GET", base+"/search?q=right+book", "")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &found) != nil || len(found) != 1 ||
		found[0] != (hcFoundBook{BookID: 60, Slug: "the-right-book", Title: "The Right Book", Authors: "A. Writer", Year: "2021"}) {
		t.Fatalf("search: %d %s", rec.Code, rec.Body)
	}
	rec = call("GET", base+"/search?q="+url.QueryEscape("https://hardcover.app/books/the-right-book/editions"), "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"book_id":60`) {
		t.Errorf("by link: %d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{"https://evil.example/books/the-right-book", "javascript:alert(1)", "https://hardcover.app/books/../x"} {
		if slug, id, ok := hcLinkTarget(q); ok {
			t.Errorf("%q taken as a link: %q %d", q, slug, id)
		}
	}
	if rec := call("GET", base+"/search?q=", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("empty search: %d", rec.Code)
	}

	// Choosing it: only the id is taken from the request; Hardcover says
	// the rest. It's sent at once, as that book.
	if rec := call("PUT", base+"/match", `{"book_id":61,"title":"<b>x</b>"}`); rec.Code != http.StatusNotFound {
		t.Errorf("a book Hardcover doesn't have: %d %s", rec.Code, rec.Body)
	}
	if rec := call("PUT", base+"/match", `{"book_id":60}`); rec.Code != 200 {
		t.Fatalf("match: %d %s", rec.Code, rec.Body)
	}
	step(t)
	st := state()
	if st.State != "sent" || st.Match == nil || !st.Match.Chosen || st.Match.Title != "The Right Book" || st.Match.Slug != "the-right-book" {
		t.Fatalf("after choosing: %+v %+v", st, st.Match)
	}
	if b := f.books[60]; b == nil || b.status != hardcover.StatusReading || b.edition != 601 {
		t.Fatalf("not sent as the chosen book: %+v", f.books[60])
	}
	if p := hcProblemBooks(u.ID); len(p) != 0 {
		t.Errorf("still a problem: %+v", p)
	}
	// The library is left alone, and other users aren't affected.
	var ids int
	_ = calibreDB.QueryRow(`SELECT COUNT(*) FROM identifiers i JOIN books b ON b.id = i.book WHERE b.uuid = ?`, books[1]).Scan(&ids)
	if ids != 0 {
		t.Errorf("the library's identifiers changed: %d", ids)
	}
	if _, ok := hcUserMatch(u.ID+1, books[1]); ok {
		t.Error("another user got the match")
	}

	// Back to the library's match: not found again.
	if rec := call("DELETE", base+"/match", ""); rec.Code != 200 {
		t.Fatalf("unmatch: %d %s", rec.Code, rec.Body)
	}
	step(t)
	if st := state(); st.State != "not_found" || st.Match != nil {
		t.Errorf("after going back: %+v", st)
	}

	// Unlinking forgets the user's matches too.
	if err := hcSetUserMatch(u.ID, books[1], hardcover.Edition{BookID: 60}); err != nil {
		t.Fatal(err)
	}
	if _, err := hcUnlinkAccount(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := hcUserMatch(u.ID, books[1]); ok {
		t.Error("a match outlived the link")
	}
}

// Paused and Dropped are sent as Hardcover's Paused and Did Not Finish.
func TestHardcoverSyncPausedDropped(t *testing.T) {
	u, books, f := setupHardcoverSync(t)
	linkHardcover(t, u)
	if err := hcSetBookShared(u.ID, books[0], true); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		status string
		want   int64
	}{{"reading", hardcover.StatusReading}, {"paused", hardcover.StatusPaused}, {"reading", hardcover.StatusReading}, {"dropped", hardcover.StatusDNF}} {
		if err := setStatus(u.ID, books[0], c.status, "manual"); err != nil {
			t.Fatal(err)
		}
		step(t)
		f.mu.Lock()
		b := f.books[50]
		f.mu.Unlock()
		if b == nil || b.status != c.want {
			t.Fatalf("%s: sent as %+v, want status %d", c.status, b, c.want)
		}
	}
}
