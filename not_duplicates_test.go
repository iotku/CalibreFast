package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
)

func TestNotDuplicates(t *testing.T) {
	setupTestAppDB(t)
	admin := &User{Username: "admin", IsAdmin: true}
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/library/not-duplicates", bytes.NewBufferString(body))
		markNotDuplicatesHandler(rec, req.WithContext(withUserCtx(req, admin)))
		return rec
	}
	if rec := post(`{"uuids":["a-1","a-1"]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("one book: %d", rec.Code)
	}
	rec := post(`{"uuids":["a-1","b-2","a-1"]}`)
	var set notDuplicateSet
	if err := json.Unmarshal(rec.Body.Bytes(), &set); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if _, err := markNotDuplicates([]string{"c-3", "d-4", "e-5"}, "admin"); err != nil {
		t.Fatal(err)
	}
	sets, err := notDuplicateSets()
	if err != nil || len(sets) != 2 || !slices.Equal(sets[0].UUIDs, []string{"a-1", "b-2"}) || len(sets[1].UUIDs) != 3 {
		t.Fatalf("sets %+v, %v", sets, err)
	}

	del := func(id string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("DELETE", "/api/library/not-duplicates/"+id, nil)
		req.SetPathValue("id", id)
		unmarkNotDuplicatesHandler(rec, req)
		return rec.Code
	}
	if code := del("999"); code != http.StatusNotFound {
		t.Errorf("missing set: %d", code)
	}
	if code := del(strconv.FormatInt(set.ID, 10)); code != http.StatusNoContent {
		t.Errorf("unmark: %d", code)
	}
	sets, _ = notDuplicateSets()
	if len(sets) != 1 || len(sets[0].UUIDs) != 3 {
		t.Errorf("after unmark %+v", sets)
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM not_duplicate_books WHERE book_uuid IN ('a-1', 'b-2')`); n != 0 {
		t.Errorf("unmarked set's books kept: %d", n)
	}
}
