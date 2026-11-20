package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// The live fragment is admins' only, and carries the sections the page
// swaps in: uptime, every service's budget, storage.
func TestServerLive(t *testing.T) {
	h := requireAdminUser(serverLive)
	for _, u := range []*User{nil, {ID: 2, Username: "reader"}, {ID: 3, Username: "ed", IsEditor: true}} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/admin/server/live", nil)
		if u != nil {
			req = asUser(req, u)
		}
		h(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%v: status %d, want 404", u, rec.Code)
		}
	}

	oldHC, oldKey := hcClient, googleBooksKey
	t.Cleanup(func() { hcClient, googleBooksKey = oldHC, oldKey })
	hcClient = hardcover.New(hardcover.Config{Token: "t", Budget: hardcover.NewBudget(60, 10, 4500)})
	googleBooksKey = ""

	rec := httptest.NewRecorder()
	h(rec, asUser(httptest.NewRequest(http.MethodGet, "/admin/server/live", nil), &User{ID: 1, Username: "boss", IsAdmin: true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`id="server-facts"`, `id="server-budgets"`, `id="server-storage"`,
		"Hardcover", "0 of 4,500 requests used today", "10 of 10 ready now, refilling 60 a minute",
		"Open Library", "of 1,000 requests used today", "CF_GOOGLE_BOOKS_KEY isn&#39;t set"} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "Stop server") {
		t.Errorf("fragment has more than the live sections:\n%s", body)
	}
}

func TestHardcoverBudgetCountsHardcoversOwn(t *testing.T) {
	oldHC := hcClient
	t.Cleanup(func() { hcClient = oldHC })
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	b := hardcover.NewBudget(60, 10, 4500)
	b.Reserve(now, 3)
	b.Observe(now, `"daily";r=4000;t=43200`)
	hcClient = hardcover.New(hardcover.Config{Token: "t", Budget: b})
	got := hardcoverBudget(now)
	if got.Used != 500 || !strings.Contains(got.Daily, "500 of 4,500") || got.Note == "" {
		t.Errorf("got %+v, want Hardcover's count of 500 used, with a note", got)
	}
	if got.Rate != "7 of 10 ready now, refilling 60 a minute." {
		t.Errorf("rate %q", got.Rate)
	}
	if got.Resets != "Resets at midnight UTC, in 12h 0m." {
		t.Errorf("resets %q", got.Resets)
	}
}
