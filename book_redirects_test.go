package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMergedBooksRedirect(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Alpha b", []string{"Ann"}, nil, nil)
	c := addOPFTestBook(t, db, "Alpha c", []string{"Ann"}, nil, nil)
	merge := func(keep string, ids ...string) *mergeResult {
		t.Helper()
		res, err := mergeBooks(&mergeRequest{ApplyMetadataRequest: ApplyMetadataRequest{Title: "Alpha", Creators: "Ann"}, Keep: keep, UUIDs: append([]string{keep}, ids...)}, "admin")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	merge(b.uuid, a.uuid)        // A into B…
	res := merge(c.uuid, b.uuid) // …then B into C: A leads to C too
	for _, old := range []string{a.uuid, b.uuid} {
		if got := mergedInto(old); got != c.uuid {
			t.Errorf("%s leads to %q, want C", old, got)
		}
	}

	req := httptest.NewRequest("GET", "/read?uuid="+a.uuid+"&cfi=x", nil)
	rec := httptest.NewRecorder()
	if !redirectMerged(rec, req, a.uuid) || rec.Code != http.StatusFound {
		t.Fatalf("no redirect: %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/read?cfi=x&uuid="+c.uuid {
		t.Errorf("reader redirect to %q", loc)
	}
	rec = httptest.NewRecorder()
	redirectMerged(rec, httptest.NewRequest("GET", "/download/"+a.uuid+"/epub", nil), a.uuid)
	if loc := rec.Header().Get("Location"); loc != "/download/"+c.uuid+"/epub" {
		t.Errorf("download redirect to %q", loc)
	}
	rec = httptest.NewRecorder()
	bookHandler(rec, httptest.NewRequest("GET", "/book/get/"+b.uuid, nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/book/get/"+c.uuid {
		t.Errorf("book page: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// B restored from the bin is itself again; A still leads to C.
	if _, err := restoreRecycled(res.Batch, []string{b.uuid}, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := mergedInto(b.uuid); got != "" {
		t.Errorf("restored book still redirects to %q", got)
	}
	if got := mergedInto(a.uuid); got != c.uuid {
		t.Errorf("A leads to %q", got)
	}
}
