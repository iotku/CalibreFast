package hardcover

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOAuth is Hardcover's OAuth endpoints: a device sign-in approved after
// pending polls, and rotating refresh tokens that are revoked on reuse.
type fakeOAuth struct {
	srv      *httptest.Server
	mu       sync.Mutex
	pending  int             // polls to answer authorization_pending
	slow     bool            // answer slow_down once
	spent    map[string]bool // refresh tokens used
	serial   int
	revoked  []string
	lastForm url.Values
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	f := &fakeOAuth{spent: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("client_id") != "cid" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"invalid_client"}`)
			return
		}
		oauthErr := func(code string) {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"`+code+`"}`)
		}
		issue := func() {
			f.serial++
			n := string(rune('0' + f.serial))
			io.WriteString(w, `{"access_token":"hc_at_`+n+`","refresh_token":"hc_rt_`+n+`","expires_in":604800,"scope":"read:me"}`)
		}
		switch r.URL.Path {
		case "/device":
			io.WriteString(w, `{"device_code":"dc","user_code":"ABCD-1234","verification_uri":"https://evil.example/link","verification_uri_complete":"https://hardcover.app/link?code=ABCD-1234","expires_in":600,"interval":1}`)
		case "/token":
			switch r.PostForm.Get("grant_type") {
			case "urn:ietf:params:oauth:grant-type:device_code":
				switch {
				case r.PostForm.Get("device_code") != "dc":
					oauthErr("expired_token")
				case f.slow:
					f.slow = false
					oauthErr("slow_down")
				case f.pending > 0:
					f.pending--
					oauthErr("authorization_pending")
				default:
					issue()
				}
			case "refresh_token":
				rt := r.PostForm.Get("refresh_token")
				if f.spent[rt] || !strings.HasPrefix(rt, "hc_rt_") {
					oauthErr("invalid_grant")
					return
				}
				f.spent[rt] = true
				issue()
			default:
				oauthErr("unsupported_grant_type")
			}
		case "/revoke":
			f.revoked = append(f.revoked, r.PostForm.Get("token"))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOAuth) client() *OAuth {
	return &OAuth{ClientID: "cid", DeviceURL: f.srv.URL + "/device", TokenURL: f.srv.URL + "/token",
		RevokeURL: f.srv.URL + "/revoke", UserAgent: "CalibreFast/test"}
}

func TestDeviceSignIn(t *testing.T) {
	f := newFakeOAuth(t)
	f.pending, f.slow = 2, true
	o := f.client()
	ctx := context.Background()
	d, err := o.StartDevice(ctx, SyncScopes)
	if err != nil {
		t.Fatal(err)
	}
	if d.UserCode != "ABCD-1234" || d.VerificationURI != VerificationURL || d.Interval != 5*time.Second ||
		d.VerificationURIComplete != "https://hardcover.app/link?code=ABCD-1234" {
		t.Errorf("device auth %+v: the verification URL must be Hardcover's, the interval at least 5 s", d)
	}
	if f.lastForm.Get("scope") != SyncScopes {
		t.Errorf("scope sent: %q", f.lastForm.Get("scope"))
	}
	var got []error
	var tok *Token
	for range 5 {
		tok, err = o.PollDevice(ctx, d.DeviceCode)
		if err == nil {
			break
		}
		got = append(got, err)
	}
	if tok == nil || tok.Access != "hc_at_1" || tok.Refresh != "hc_rt_1" {
		t.Fatalf("token %+v, %v", tok, err)
	}
	if len(got) != 3 || !errors.Is(got[0], ErrSlowDown) || !errors.Is(got[1], ErrAuthorizationPending) {
		t.Errorf("poll errors: %v", got)
	}
	if _, err := o.PollDevice(ctx, "other"); !errors.Is(err, ErrExpiredCode) {
		t.Errorf("expired: %v", err)
	}

	// Refresh rotates; the spent token is refused.
	t2, err := o.Refresh(ctx, tok.Refresh)
	if err != nil || t2.Refresh == tok.Refresh {
		t.Fatalf("refresh: %+v %v", t2, err)
	}
	if _, err := o.Refresh(ctx, tok.Refresh); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("reused refresh token: %v", err)
	}
	if err := o.Revoke(ctx, t2.Refresh); err != nil || len(f.revoked) != 1 || f.revoked[0] != t2.Refresh {
		t.Errorf("revoke: %v %v", err, f.revoked)
	}

	bad := *o
	bad.ClientID = "nope"
	if _, err := bad.StartDevice(ctx, SyncScopes); err == nil || strings.Contains(err.Error(), f.srv.URL) {
		t.Errorf("wrong client: %v", err)
	}
	if _, err := (&OAuth{}).StartDevice(ctx, SyncScopes); err == nil {
		t.Error("no client id, yet started")
	}
}

func TestVerificationLink(t *testing.T) {
	for u, want := range map[string]bool{
		"https://hardcover.app/link?code=ABCD-1234": true,
		"https://hardcover.app/link":                true,
		"http://hardcover.app/link?code=x":          false,
		"https://hardcover.app.evil.example/link":   false,
		"https://evil@hardcover.app/link":           false,
		"https://hardcover.app/other":               false,
		"javascript:alert(1)":                       false,
		"":                                          false,
	} {
		if verificationLink(u) != want {
			t.Errorf("verificationLink(%q) != %v", u, want)
		}
	}
}

func TestValidUserCode(t *testing.T) {
	for s, want := range map[string]bool{"ABCD-1234": true, "abcd": true, "AB": false, "<b>x</b>": false, "ABCD 1234": false} {
		if validUserCode(s) != want {
			t.Errorf("validUserCode(%q) != %v", s, want)
		}
	}
}

// fakeLibrary is the GraphQL API for a signed-in user.
type fakeLibrary struct {
	srv   *httptest.Server
	token atomic.Value // the access token it accepts
	mu    sync.Mutex
	ops   []string
	vars  map[string]map[string]any
	reply map[string]string
}

func newFakeLibrary(t *testing.T) *fakeLibrary {
	f := &fakeLibrary{vars: map[string]map[string]any{}, reply: map[string]string{}}
	f.token.Store("hc_at_good")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token.Load().(string) {
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
		f.mu.Lock()
		f.ops = append(f.ops, op)
		f.vars[op] = req.Variables
		body := f.reply[op]
		f.mu.Unlock()
		if body == "" {
			body = `{"errors":[{"message":"unexpected ` + op + `"}]}`
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestTokenFuncRefreshesOnce(t *testing.T) {
	f := newFakeLibrary(t)
	f.reply["Me"] = `{"data":{"me":[{"id":7,"username":"reader"}]}}`
	var calls []bool
	c := New(Config{URL: f.srv.URL, UserAgent: "CalibreFast/test", Budget: NewBudget(60, 10, 100),
		TokenFunc: func(_ context.Context, force bool) (string, error) {
			calls = append(calls, force)
			if force {
				return "hc_at_good", nil
			}
			return "hc_at_stale", nil
		}})
	me, err := c.Me(context.Background())
	if err != nil || me.ID != 7 || me.Username != "reader" {
		t.Fatalf("Me = %+v, %v", me, err)
	}
	if len(calls) != 2 || calls[0] || !calls[1] {
		t.Errorf("token calls %v: want a stale one, then one forced", calls)
	}
	f.token.Store("hc_at_revoked")
	if _, err := c.Me(context.Background()); !errors.Is(err, ErrSignedOut) {
		t.Errorf("refused twice: %v", err)
	}
	failing := New(Config{URL: f.srv.URL, TokenFunc: func(context.Context, bool) (string, error) { return "", ErrInvalidGrant }})
	if _, err := failing.Me(context.Background()); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("token error not passed on: %v", err)
	}
}

func TestLibraryCalls(t *testing.T) {
	f := newFakeLibrary(t)
	c := New(Config{URL: f.srv.URL, Token: "hc_at_good", UserAgent: "CalibreFast/test", Budget: NewBudget(60, 10, 100)})
	ctx := context.Background()

	f.reply["SyncEditions"] = `{"data":{"editions":[{"id":10,"book_id":1,"pages":0},{"id":11,"book_id":1,"pages":320}]}}`
	e, ok, err := c.FindEdition(ctx, []string{"978-0-316-12908-4", "bad", "9780316129084"}, 0, "")
	if err != nil || !ok || e != (Edition{BookID: 1, EditionID: 11, Pages: 320}) {
		t.Fatalf("by ISBN: %+v %v %v", e, ok, err)
	}
	if isbns := f.vars["SyncEditions"]["isbns"].([]any); len(isbns) != 1 {
		t.Errorf("ISBNs sent: %v", isbns)
	}

	f.reply["SyncEditions"] = `{"data":{"editions":[]}}`
	f.reply["SyncBooks"] = `{"data":{"books":[{"id":5,"slug":"x","pages":200,"editions":[{"id":50,"pages":210}]}]}}`
	e, ok, err = c.FindEdition(ctx, []string{"9780316129084"}, 5, "../evil")
	if err != nil || !ok || e != (Edition{BookID: 5, EditionID: 50, Pages: 210}) {
		t.Fatalf("by id: %+v %v %v", e, ok, err)
	}
	if slugs := f.vars["SyncBooks"]["slugs"].([]any); len(slugs) != 0 {
		t.Errorf("an invalid slug was sent: %v", slugs)
	}
	f.reply["SyncBooks"] = `{"data":{"books":[]}}`
	if _, ok, err := c.FindEdition(ctx, nil, 0, "missing"); ok || err != nil {
		t.Errorf("no match: %v %v", ok, err)
	}
	if _, ok, err := c.FindEdition(ctx, nil, 0, ""); ok || err != nil {
		t.Errorf("nothing to look for: %v %v", ok, err)
	}

	f.reply["UserBook"] = `{"data":{"user_books":[]}}`
	if ub, err := c.UserBook(ctx, 7, 5); ub != nil || err != nil {
		t.Errorf("no entry: %+v %v", ub, err)
	}
	f.reply["UserBook"] = `{"data":{"user_books":[{"id":99,"status_id":2,"edition_id":50,"edition":{"pages":300},"user_book_reads":[{"id":3,"progress_pages":12,"started_at":"2026-11-01","finished_at":null}]}]}}`
	ub, err := c.UserBook(ctx, 7, 5)
	if err != nil || *ub != (UserBook{ID: 99, StatusID: 2, EditionID: 50, EditionPages: 300, ReadID: 3, ProgressPages: 12, StartedAt: "2026-11-01"}) {
		t.Errorf("entry: %+v %v", ub, err)
	}

	f.reply["InsertUserBook"] = `{"data":{"insert_user_book":{"id":100,"error":null}}}`
	if id, err := c.AddUserBook(ctx, 5, StatusReading, 50); err != nil || id != 100 {
		t.Errorf("add: %d %v", id, err)
	}
	obj := f.vars["InsertUserBook"]["object"].(map[string]any)
	if obj["book_id"] != 5.0 || obj["status_id"] != 2.0 || obj["edition_id"] != 50.0 {
		t.Errorf("add sent %v", obj)
	}
	f.reply["UpdateUserBook"] = `{"data":{"update_user_book":{"id":99,"error":"Book not <found>"}}}`
	if err := c.SetUserBook(ctx, 99, StatusRead, 0); err == nil || !strings.Contains(err.Error(), "not <found>") {
		t.Errorf("Hardcover's error not passed on: %v", err)
	}
	f.reply["UpdateUserBookRead"] = `{"data":{"update_user_book_read":{"id":3,"error":null}}}`
	at := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	if err := c.UpdateRead(ctx, 3, ReadUpdate{ProgressPages: 40, ActionAt: at}); err != nil {
		t.Fatal(err)
	}
	obj = f.vars["UpdateUserBookRead"]["object"].(map[string]any)
	if obj["progress_pages"] != 40.0 || obj["action_at"] != "2026-11-20T08:00:00Z" || obj["started_at"] != nil {
		t.Errorf("read update sent %v", obj)
	}
	n := len(f.ops)
	if err := c.UpdateRead(ctx, 3, ReadUpdate{ActionAt: at}); err != nil || len(f.ops) != n {
		t.Errorf("an empty update was sent")
	}
}
