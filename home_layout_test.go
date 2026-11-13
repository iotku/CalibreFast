package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestHomeLayout(t *testing.T) {
	setupTestAppDB(t)
	u, err := createUser("reader", "a long enough password", false)
	if err != nil {
		t.Fatal(err)
	}
	put := func(body string) int {
		rec := httptest.NewRecorder()
		homeLayoutHandler(rec, asUser(httptest.NewRequest(http.MethodPut, "/api/me/home-layout", strings.NewReader(body)), u))
		return rec.Code
	}
	if l, err := userHomeLayout(u); err != nil || len(l.Order) != 0 || len(l.Hidden) != 0 || l.Order == nil {
		t.Fatalf("no layout yet: %+v %v", l, err)
	}
	for _, bad := range []string{
		`{"order": ["k:shuffle", "k:shuffle"]}`, // a repeat
		`{"order": ["x:shuffle"]}`,              // not a row
		`{"order": ["k:"]}`,                     // nothing after the kind
		`{"hidden": ["reading"]}`,               // Continue reading can't be turned off
		`{"hidden": ["tags", "tags"]}`,          // a repeat
		`{"order": ["t:Space", "t:space"]}`,     // the same tag
		`not json`,
	} {
		if code := put(bad); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, code)
		}
	}
	if code := put(`{"order": ["k:shuffle", "s:7", "t:Space Opera", "k:reading"], "hidden": ["tags", "recommended"]}`); code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	l, err := userHomeLayout(u)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.Order, []string{"k:shuffle", "s:7", "t:Space Opera", "k:reading"}) || !slices.Equal(l.Hidden, []string{"tags", "recommended"}) {
		t.Errorf("stored %+v", l)
	}

	tag := func(method, name string) {
		t.Helper()
		rec := httptest.NewRecorder()
		r := asUser(httptest.NewRequest(method, "/api/me/hidden-tags/x", nil), u)
		r.SetPathValue("name", name)
		if method == http.MethodPut {
			hideTagHandler(rec, r)
		} else {
			unhideTagHandler(rec, r)
		}
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s %s: status %d", method, name, rec.Code)
		}
	}
	tag(http.MethodPut, "Romance")
	tag(http.MethodPut, "romance") // the same tag
	tag(http.MethodPut, "Westerns")
	if got, _ := userHiddenTags(u); !slices.Equal(got, []string{"Romance", "Westerns"}) {
		t.Errorf("hidden %v", got)
	}
	tag(http.MethodDelete, "ROMANCE")
	if got, _ := userHiddenTags(u); !slices.Equal(got, []string{"Westerns"}) {
		t.Errorf("after unhiding: %v", got)
	}

	// Both come with the home answer.
	t.Chdir(t.TempDir())
	catalogFixture(t)
	rec := httptest.NewRecorder()
	homeHandler(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/me/home", nil), u))
	if rec.Code != http.StatusOK {
		t.Fatalf("home: %d %s", rec.Code, rec.Body)
	}
	var h homeAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.Layout.Hidden, []string{"tags", "recommended"}) || !slices.Equal(h.HiddenTags, []string{"Westerns"}) {
		t.Errorf("home answer layout %+v, hidden tags %v", h.Layout, h.HiddenTags)
	}
}
