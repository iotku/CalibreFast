package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A notice's link goes into an href on every page: only a path on this
// site or an https URL gets through.
func TestSystemNoticeHref(t *testing.T) {
	for in, ok := range map[string]bool{
		"":                             true,
		"/about":                       true,
		"/author/Ashton, Hugh?x=1#top": true,
		"https://status.example.com/":  true,
		"  https://example.com/new  ":  true,
		"//evil.example/":              false,
		"/\\evil.example":              false,
		"javascript:alert(1)":          false,
		"JavaScript:alert(1)":          false,
		"data:text/html,<b>x</b>":      false,
		"http://example.com/":          false,
		"https://user:pw@example.com/": false,
		"https:///nohost":              false,
		"about":                        false,
		"/ok\nSet-Cookie: x":           false,
		"https://example.com/\x00":     false,
	} {
		_, err := systemNoticeHref(in)
		if (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", in, err, ok)
		}
	}
}

func TestSystemNoticeValidate(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Hour).UnixMilli()

	in := systemNoticeInput{Text: "  Down\n for   maintenance ", LinkHref: "/about", ExpiresAt: later}
	if err := in.validate(now); err != nil {
		t.Fatal(err)
	}
	if in.Text != "Down for maintenance" || in.LinkLabel != "More" {
		t.Errorf("tidied to %+v", in)
	}
	// No link: no label either.
	in = systemNoticeInput{Text: "x", LinkLabel: "Read", ExpiresAt: later}
	if err := in.validate(now); err != nil || in.LinkLabel != "" {
		t.Errorf("label without a link: %+v, %v", in, err)
	}

	for name, bad := range map[string]systemNoticeInput{
		"empty":       {Text: "   ", ExpiresAt: later},
		"too long":    {Text: strings.Repeat("é", systemNoticeTextMax+1), ExpiresAt: later},
		"long label":  {Text: "x", LinkHref: "/a", LinkLabel: strings.Repeat("a", systemNoticeLabelMax+1), ExpiresAt: later},
		"bad link":    {Text: "x", LinkHref: "javascript:alert(1)", ExpiresAt: later},
		"ended":       {Text: "x", ExpiresAt: now.Add(-time.Minute).UnixMilli()},
		"no end":      {Text: "x"},
		"over a year": {Text: "x", ExpiresAt: now.Add(systemNoticeMaxLife + time.Hour).UnixMilli()},
	} {
		if err := bad.validate(now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (&systemNoticeInput{Text: strings.Repeat("é", systemNoticeTextMax), ExpiresAt: later}).validate(now); err != nil {
		t.Errorf("text of exactly the limit (in characters): %v", err)
	}
}

// Admins post and end notices; everyone gets the ones in force with
// /api/me, without who posted them; others can't reach the admin API.
func TestSystemNoticesAPI(t *testing.T) {
	setupTestAppDB(t)
	admin, err := createUser("admin1", "correct horse battery", true)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := createUser("reader1", "correct horse battery", false)
	if err != nil {
		t.Fatal(err)
	}

	call := func(u *User, h http.HandlerFunc, method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if u != nil {
			req = asUser(req, u)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	post := func(u *User, body string) *httptest.ResponseRecorder {
		return call(u, requireAdminUser(systemNoticeCreateHandler), http.MethodPost, "/api/admin/system-notices", body)
	}
	meNotices := func(u *User) []map[string]any {
		t.Helper()
		var me struct {
			Notices []map[string]any `json:"system_notices"`
		}
		rec := call(u, meHandler, http.MethodGet, "/api/me", "")
		if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
			t.Fatal(err)
		}
		return me.Notices
	}

	until := time.Now().Add(2 * time.Hour).UnixMilli()
	body := `{"text":"Down Sunday 02:00–03:00 UTC","link_href":"https://status.example.com/","link_label":"Status","expires_at":` + jsonInt(until) + `}`

	if rec := post(reader, body); rec.Code != http.StatusNotFound {
		t.Errorf("non-admin post: %d", rec.Code)
	}
	if rec := post(nil, body); rec.Code != http.StatusNotFound {
		t.Errorf("anonymous post: %d", rec.Code)
	}
	if rec := post(admin, `{"text":"x","link_href":"javascript:alert(1)","expires_at":`+jsonInt(until)+`}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad link: %d", rec.Code)
	}
	if got := meNotices(reader); len(got) != 0 {
		t.Fatalf("notices before any: %v", got)
	}

	rec := post(admin, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	var made systemNotice
	if err := json.Unmarshal(rec.Body.Bytes(), &made); err != nil || made.ID == 0 || made.CreatedBy != "admin1" {
		t.Fatalf("posted %+v, %v", made, err)
	}

	// An expired one is never sent.
	if _, err := appDB.Exec(`INSERT INTO system_notices (text, created_at, expires_at) VALUES ('old', 0, ?)`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	got := meNotices(reader)
	if len(got) != 1 || got[0]["text"] != "Down Sunday 02:00–03:00 UTC" || got[0]["link_href"] != "https://status.example.com/" || got[0]["link_label"] != "Status" {
		t.Fatalf("/api/me notices = %v", got)
	}
	if _, ok := got[0]["created_by"]; ok {
		t.Error("/api/me says who posted it")
	}

	rec = call(admin, requireAdminUser(systemNoticesHandler), http.MethodGet, "/api/admin/system-notices", "")
	var list struct{ Notices []systemNotice }
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Notices) != 1 || list.Notices[0].CreatedBy != "admin1" {
		t.Fatalf("admin list: %s", rec.Body)
	}
	if rec := call(reader, requireAdminUser(systemNoticesHandler), http.MethodGet, "/api/admin/system-notices", ""); rec.Code != http.StatusNotFound {
		t.Errorf("non-admin list: %d", rec.Code)
	}

	del := func(u *User, id int64) int {
		req := httptest.NewRequest(http.MethodDelete, "/api/admin/system-notices/x", nil)
		req.SetPathValue("id", jsonInt(id))
		if u != nil {
			req = asUser(req, u)
		}
		rec := httptest.NewRecorder()
		requireAdminUser(systemNoticeDeleteHandler)(rec, req)
		return rec.Code
	}
	if code := del(reader, made.ID); code != http.StatusNotFound {
		t.Errorf("non-admin delete: %d", code)
	}
	if code := del(admin, made.ID); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code := del(admin, made.ID); code != http.StatusNotFound {
		t.Errorf("delete again: %d", code)
	}
	if got := meNotices(reader); len(got) != 0 {
		t.Errorf("after ending it: %v", got)
	}
}

// Signed out, visitors get the notices only where they see the bar: a
// public catalog.
func TestSystemNoticesForVisitors(t *testing.T) {
	setupTestAppDB(t)
	defer func(v bool) { publicCatalog = v }(publicCatalog)
	if _, err := addSystemNotice(systemNoticeInput{Text: "Moving to books.example.com", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, "admin1", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, public := range []bool{false, true} {
		publicCatalog = public
		rec := httptest.NewRecorder()
		meHandler(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["system_notices"]; ok != public {
			t.Errorf("public catalog %v: notices sent = %v", public, ok)
		}
		if strings.Contains(rec.Body.String(), "admin1") {
			t.Error("a visitor was told who posted it")
		}
	}
}

func TestSystemNoticesLimit(t *testing.T) {
	setupTestAppDB(t)
	now := time.Now()
	in := systemNoticeInput{Text: "x", ExpiresAt: now.Add(time.Hour).UnixMilli()}
	for i := 0; i < systemNoticesMax; i++ {
		if _, err := addSystemNotice(in, "a", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := addSystemNotice(in, "a", now); err != errTooManyNotices {
		t.Errorf("one over the limit: %v", err)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
