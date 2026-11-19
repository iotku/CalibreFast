package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// registerAllEditRoutes serves every route that changes the library or
// manages the server, on mux.
func registerAllEditRoutes(mux *http.ServeMux) {
	withMux(mux, func() {
		http.HandleFunc("/", indexHandler)
		registerAuthRoutes()
		registerBookEditRoutes()
		registerImportRoutes()
		registerRecycleRoutes()
		registerEditLogRoutes()
		registerMergeRoutes()
		registerAddFormatRoutes()
		registerPlaceholderRoutes()
		registerBulkEditRoutes()
		registerLibraryRepairRoutes()
		registerNotDuplicateRoutes()
		http.HandleFunc("PUT /api/groups/{cat}/{value...}", requireWrites(groupPutHandler))
		http.HandleFunc("PUT /api/aggregates/{cat}/featured/{value...}", requireWrites(featuredHandler))
		http.HandleFunc("DELETE /api/aggregates/{cat}/featured/{value...}", requireWrites(featuredHandler))
	})
}

// What an editor may do (requireEditor) and what only an admin may
// (requireWrites, requireAdminUser). With writes off, a guard that lets a
// user through answers 403 naming -allow-writes before its handler runs;
// one that doesn't answers like a path that doesn't exist.
var (
	editorRoutes = []struct{ method, path string }{
		{"POST", "/upload"},
		{"POST", "/upload-cover/a"},
		{"POST", "/revert-cover/a"},
		{"GET", "/book/edit/a"},
		{"POST", "/metadata/a/apply"},
		{"GET", "/metadata/a/isbns"},
		{"GET", "/metadata/lookup?isbn=1"},
		{"GET", "/import"},
		{"GET", "/api/imports"},
		{"GET", "/import/a"},
		{"POST", "/import/a/apply"},
		{"POST", "/import/a/discard"},
		{"POST", "/import/a/cover"},
		{"GET", "/recycle-bin"},
		{"GET", "/api/recycle-bin"},
		{"POST", "/api/recycle-bin/b/restore"},
		{"POST", "/api/library/delete"},
		{"POST", "/api/library/books/a/formats/epub/remove"},
		{"GET", "/book/merge?ids=a,b"},
		{"POST", "/merge/apply"},
		{"POST", "/book/add-format/a"},
		{"POST", "/book/add-format/a/b/apply"},
		{"GET", "/missing"},
		{"GET", "/missing/new"},
		{"POST", "/api/placeholders"},
		{"PUT", "/api/placeholders/a"},
		{"DELETE", "/api/placeholders/a"},
		{"POST", "/api/placeholders/a/cover"},
		{"POST", "/api/placeholders/a/files"},
		{"GET", "/library/edit"},
		{"GET", "/api/library/table"},
		{"POST", "/api/library/bulk"},
		{"POST", "/api/library/not-duplicates"},
		{"DELETE", "/api/library/not-duplicates/1"},
	}
	// Admin routes behind requireWrites: admins pass, with -allow-writes.
	adminWriteRoutes = []struct{ method, path string }{
		{"POST", "/api/library/repair-formats"},
		{"GET", "/admin/repairs"},
		{"GET", "/api/admin/repairs"},
		{"POST", "/api/admin/repairs/format-names"},
		{"PUT", "/api/groups/author/x"},
		{"PUT", "/api/aggregates/author/featured/x"},
		{"DELETE", "/api/aggregates/author/featured/x"},
		{"POST", "/api/admin/activity/revert"},
	}
	// Admin routes behind requireAdminUser (no -allow-writes needed).
	adminRoutes = []struct{ method, path string }{
		{"GET", "/admin/users"},
		{"POST", "/api/admin/users"},
		{"PATCH", "/api/admin/users/1"},
		{"DELETE", "/api/admin/users/1"},
		{"GET", "/api/admin/links"},
		{"POST", "/api/admin/links"},
		{"GET", "/admin/activity"},
		{"GET", "/api/admin/activity"},
	}
)

func TestEditorPermissionMatrix(t *testing.T) {
	setupTestAppDB(t)
	orig := allowWrites
	t.Cleanup(func() { allowWrites = orig })
	allowWrites = false
	mux := http.NewServeMux()
	registerAllEditRoutes(mux)
	reader, _ := createUser("reader1", "correct horse battery", false)
	editor, _ := createUser("editor1", "correct horse battery", false)
	if _, err := appDB.Exec(`UPDATE users SET is_editor = 1 WHERE id = ?`, editor.ID); err != nil {
		t.Fatal(err)
	}
	editor, _ = getUserByID(editor.ID) // as a session loads it
	admin, _ := createUser("admin1", "correct horse battery", true)
	if !editor.IsEditor || editor.IsAdmin {
		t.Fatalf("editor %+v", editor)
	}

	do := func(u *User, method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		if u != nil {
			req = asUser(req, u)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	missing := func(u *User, method string) string { return do(u, method, "/no-such-page").Body.String() }
	passes := func(rec *httptest.ResponseRecorder) bool {
		return rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "-allow-writes")
	}
	looksMissing := func(u *User, method string, rec *httptest.ResponseRecorder) bool {
		return rec.Code == http.StatusNotFound && rec.Body.String() == missing(u, method)
	}

	for _, rt := range editorRoutes {
		if rec := do(editor, rt.method, rt.path); !passes(rec) {
			t.Errorf("editor %s %s: %d %q, want let through", rt.method, rt.path, rec.Code, rec.Body)
		}
		if rec := do(admin, rt.method, rt.path); !passes(rec) {
			t.Errorf("admin %s %s: %d %q, want let through", rt.method, rt.path, rec.Code, rec.Body)
		}
		for _, u := range []*User{nil, reader} {
			if rec := do(u, rt.method, rt.path); !looksMissing(u, rt.method, rec) {
				t.Errorf("user=%v %s %s: %d %q, want 404", u != nil, rt.method, rt.path, rec.Code, rec.Body)
			}
		}
	}
	for _, rt := range adminWriteRoutes {
		if rec := do(admin, rt.method, rt.path); !passes(rec) {
			t.Errorf("admin %s %s: %d %q, want let through", rt.method, rt.path, rec.Code, rec.Body)
		}
	}
	for _, rt := range append(slices.Clone(adminWriteRoutes), adminRoutes...) {
		for _, u := range []*User{nil, reader, editor} {
			if rec := do(u, rt.method, rt.path); !looksMissing(u, rt.method, rec) {
				t.Errorf("user=%v %s %s: %d %q, want 404", u, rt.method, rt.path, rec.Code, rec.Body)
			}
		}
	}

	// With writes on, an editor still can't make themselves an admin.
	allowWrites = true
	if rec := do(editor, "PATCH", "/api/admin/users/"+itoa(editor.ID)); rec.Code != http.StatusNotFound {
		t.Errorf("editor promoting themselves: %d", rec.Code)
	}
	if u, _ := getUserByID(editor.ID); u.IsAdmin {
		t.Error("editor became an admin")
	}
}

// An admin makes someone an editor, and takes it back.
func TestAdminSetsEditor(t *testing.T) {
	setupTestAppDB(t)
	admin, _ := createUser("admin1", "correct horse battery", true)
	u, _ := createUser("someone", "correct horse battery", false)
	patch := func(body string) {
		req := asUser(httptest.NewRequest("PATCH", "/api/admin/users/"+itoa(u.ID), strings.NewReader(body)), admin)
		req.SetPathValue("id", itoa(u.ID))
		rec := httptest.NewRecorder()
		adminUpdateUser(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	patch(`{"is_editor":true}`)
	if got, _ := getUserByID(u.ID); !got.IsEditor || got.IsAdmin {
		t.Fatalf("after promoting: %+v", got)
	}
	patch(`{"is_editor":false}`)
	if got, _ := getUserByID(u.ID); got.IsEditor {
		t.Fatalf("after demoting: %+v", got)
	}

	rec := httptest.NewRecorder()
	adminCreateUser(rec, asUser(httptest.NewRequest("POST", "/api/admin/users",
		strings.NewReader(`{"username":"ed2","password":"correct horse battery","is_editor":true}`)), admin))
	var created User
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if got, _ := getUserByID(created.ID); got == nil || !got.IsEditor {
		t.Fatalf("created editor: %s", rec.Body)
	}
}

func testCoverDataURL(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 12))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	img.Set(1, 1, color.Black)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// An editor's changes, through the real routes, are all in the log, and an
// admin undoes all of them since a moment: the books, the cover, the
// placeholder and the removed book are as they were.
func TestEditorChangesReverted(t *testing.T) {
	db := setupRecycleLibrary(t)
	orig := allowWrites
	allowWrites = true
	t.Cleanup(func() { allowWrites = orig })
	mux := http.NewServeMux()
	registerAllEditRoutes(mux)

	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, []string{"t1"}, []string{"eng"})
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, []string{"t2"}, nil)
	c := addOPFTestBook(t, db, "Gamma", []string{"Cat"}, nil, nil)
	admin, _ := createUser("admin1", "correct horse battery", true)
	editor, _ := createUser("editor1", "correct horse battery", false)
	_, _ = appDB.Exec(`UPDATE users SET is_editor = 1 WHERE id = ?`, editor.ID)
	editor, _ = getUserByID(editor.ID)

	do := func(u *User, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := asUser(httptest.NewRequest(method, path, strings.NewReader(body)), u)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
		}
		return rec
	}
	beforeA, beforeB := snapBook(a.uuid), snapBook(b.uuid)
	coverPath := filepath.Join(baseDir, a.path, "cover.jpg")
	oldCover, _ := os.ReadFile(coverPath)
	since := time.Now().Unix()

	// The editor's changes.
	do(editor, "POST", "/metadata/"+a.uuid+"/apply", `{"title":"Alpha Vandalized","creators":"Ann","subjects":"spam"}`)
	do(editor, "POST", "/metadata/"+a.uuid+"/apply", `{"title":"Alpha Vandalized Twice","creators":"Nobody"}`)
	do(editor, "POST", "/api/library/bulk", `{"uuids":["`+b.uuid+`"],"edit":{"add_tags":["spam"]}}`)
	waitBulkEdits() // queued (bulk_queue.go): written before the next change, as the log expects
	do(editor, "POST", "/upload-cover/"+a.uuid, testCoverDataURL(t))
	do(editor, "POST", "/api/library/delete", `{"uuids":["`+c.uuid+`"]}`)
	rec := do(editor, "POST", "/api/placeholders", `{"title":"Fake Book","creators":"Ann"}`)
	var ph struct{ UUID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &ph)
	do(editor, "PUT", "/api/placeholders/"+ph.UUID, `{"title":"Fake Book 2","creators":"Ann"}`)

	if got := snapBook(a.uuid); got.Title != "Alpha Vandalized Twice" {
		t.Fatalf("edit didn't land: %+v", got)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books WHERE uuid = ?`, c.uuid); n != 0 {
		t.Fatal("Gamma not removed")
	}
	if cur, _ := os.ReadFile(coverPath); bytes.Equal(cur, oldCover) {
		t.Fatal("cover not replaced")
	}

	// The admin sees them, by user.
	var list struct{ Entries []editLogEntry }
	_ = json.Unmarshal(do(admin, "GET", "/api/admin/activity?user="+itoa(editor.ID), "").Body.Bytes(), &list)
	var actions []string
	for _, e := range list.Entries {
		if e.Username != "editor1" || e.UserID != editor.ID {
			t.Errorf("entry by %q (%d)", e.Username, e.UserID)
		}
		actions = append(actions, e.Action)
	}
	want := []string{"edit placeholder", "add placeholder", "remove", "cover", "bulk edit", "edit", "edit"}
	if !slices.Equal(actions, want) {
		t.Fatalf("logged %v, want %v", actions, want)
	}

	// An editor can't undo, or read the log.
	for _, rt := range []struct{ m, p string }{{"POST", "/api/admin/activity/revert"}, {"GET", "/api/admin/activity"}} {
		req := asUser(httptest.NewRequest(rt.m, rt.p, strings.NewReader(`{"user_id":1,"since":1}`)), editor)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("editor %s %s: %d", rt.m, rt.p, rec.Code)
		}
	}

	// Undo everything since.
	body, _ := json.Marshal(activityRevertRequest{UserID: editor.ID, Since: since})
	var res struct{ Results []revertResult }
	_ = json.Unmarshal(do(admin, "POST", "/api/admin/activity/revert", string(body)).Body.Bytes(), &res)
	if len(res.Results) != len(want) {
		t.Fatalf("results %+v", res.Results)
	}
	for _, r := range res.Results {
		if !r.Done {
			t.Errorf("not undone: %+v", r)
		}
	}
	if got := snapBook(a.uuid); !sameJSON(mustJSONBytes(beforeA), got) {
		t.Errorf("Alpha %+v, want %+v", got, beforeA)
	}
	if got := snapBook(b.uuid); !sameJSON(mustJSONBytes(beforeB), got) {
		t.Errorf("Beta %+v, want %+v", got, beforeB)
	}
	if cur, _ := os.ReadFile(coverPath); !bytes.Equal(cur, oldCover) {
		t.Errorf("cover %q, want the old one back", cur)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books WHERE uuid = ?`, c.uuid); n != 1 {
		t.Error("Gamma not restored")
	}
	if placeholderByUUID(ph.UUID) != nil {
		t.Error("the placeholder the editor added is still shown")
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM placeholders WHERE uuid = ? AND deleted_at IS NOT NULL`, ph.UUID); n != 1 {
		t.Error("the placeholder should be kept, marked removed")
	}
	if n := count(t, appDB, `SELECT COUNT(*) FROM edit_log WHERE username = 'editor1' AND reverted_at IS NULL`); n != 0 {
		t.Errorf("%d entries not marked undone", n)
	}
	// The undoing is logged too, as the admin's.
	if n := count(t, appDB, `SELECT COUNT(*) FROM edit_log WHERE username = 'admin1' AND action LIKE 'undo #%'`); n == 0 {
		t.Error("undoing not logged")
	}

	// Again: nothing left to undo.
	_ = json.Unmarshal(do(admin, "POST", "/api/admin/activity/revert", string(body)).Body.Bytes(), &res)
	for _, r := range res.Results {
		if r.Done {
			t.Errorf("undone twice: %+v", r)
		}
	}
}

// An entry someone else has built on since isn't undone: their change stays.
func TestRevertLeavesLaterChangesAlone(t *testing.T) {
	db := setupRecycleLibrary(t)
	orig := allowWrites
	allowWrites = true
	t.Cleanup(func() { allowWrites = orig })
	mux := http.NewServeMux()
	registerAllEditRoutes(mux)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, nil, nil)
	admin, _ := createUser("admin1", "correct horse battery", true)
	editor, _ := createUser("editor1", "correct horse battery", false)
	_, _ = appDB.Exec(`UPDATE users SET is_editor = 1 WHERE id = ?`, editor.ID)
	editor, _ = getUserByID(editor.ID)
	post := func(u *User, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, asUser(httptest.NewRequest("POST", path, strings.NewReader(body)), u))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		return rec
	}
	post(editor, "/metadata/"+a.uuid+"/apply", `{"title":"Alpha by Editor","creators":"Ann"}`)
	post(admin, "/metadata/"+a.uuid+"/apply", `{"title":"Alpha Fixed by Admin","creators":"Ann"}`)
	post(editor, "/metadata/"+b.uuid+"/apply", `{"title":"Beta by Editor","creators":"Bob"}`)

	var res struct{ Results []revertResult }
	_ = json.Unmarshal(post(admin, "/api/admin/activity/revert", `{"user_id":`+itoa(editor.ID)+`,"since":1}`).Body.Bytes(), &res)
	if len(res.Results) != 2 {
		t.Fatalf("results %+v", res.Results)
	}
	for _, r := range res.Results {
		switch r.Title {
		case "Beta by Editor":
			if !r.Done {
				t.Errorf("Beta not undone: %+v", r)
			}
		case "Alpha by Editor":
			if r.Done || !strings.Contains(r.Error, "someone else") {
				t.Errorf("Alpha undone over the admin's change: %+v", r)
			}
		default:
			t.Errorf("unexpected %+v", r)
		}
	}
	if got := snapBook(a.uuid).Title; got != "Alpha Fixed by Admin" {
		t.Errorf("Alpha %q", got)
	}
	if got := snapBook(b.uuid).Title; got != "Beta" {
		t.Errorf("Beta %q", got)
	}
}

// Removing a placeholder keeps it (deleted_at), and undoing brings it back.
func TestPlaceholderRemovalUndone(t *testing.T) {
	setupRecycleLibrary(t)
	editor := &User{ID: 0, Username: "ed", IsEditor: true}
	admin := &User{Username: "admin", IsAdmin: true}
	p := &placeholder{Meta: OPFMetadata{Title: "Coming Soon", Creators: []string{"Ann"}}}
	if err := savePlaceholder(p, nil); err != nil {
		t.Fatal(err)
	}
	req := asUser(httptest.NewRequest("DELETE", "/api/placeholders/"+p.UUID, nil), editor)
	req.SetPathValue("uuid", p.UUID)
	rec := httptest.NewRecorder()
	placeholderDeleteHandler(rec, req)
	if rec.Code != http.StatusNoContent || placeholderByUUID(p.UUID) != nil {
		t.Fatalf("delete: %d", rec.Code)
	}
	entries, _ := editLogQuery(0, 0, 0)
	if len(entries) != 1 || entries[0].Action != "remove placeholder" {
		t.Fatalf("log %+v", entries)
	}
	if res := revertEntries(entries, admin); !res[0].Done {
		t.Fatalf("undo %+v", res)
	}
	if got := placeholderByUUID(p.UUID); got == nil || got.Meta.Title != "Coming Soon" {
		t.Fatalf("not back: %+v", got)
	}
}

// An editor's merge is undone: the merged-away book is back, so is the
// kept book's file the merge replaced, its cover and its metadata.
func TestMergeReverted(t *testing.T) {
	db := setupRecycleLibrary(t)
	orig := allowWrites
	allowWrites = true
	t.Cleanup(func() { allowWrites = orig })
	mux := http.NewServeMux()
	registerAllEditRoutes(mux)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, []string{"a-tag"}, nil)
	b := addOPFTestBook(t, db, "Alpha (other)", []string{"Ann"}, []string{"b-tag"}, nil)
	addFormat(t, b, "Alpha (other)", "pdf", "PDF!")
	if err := os.WriteFile(filepath.Join(baseDir, b.path, "Alpha (other).epub"), []byte("B EPUB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, b.path, "cover.jpg"), []byte("b cover"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeA := snapBook(a.uuid)
	aEPUB := filepath.Join(baseDir, a.path, "Alpha.epub")
	admin, _ := createUser("admin1", "correct horse battery", true)
	editor := &User{ID: 0, Username: "editor1", IsEditor: true}

	post := func(u *User, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, asUser(httptest.NewRequest("POST", path, bytes.NewReader(data)), u))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		return rec
	}
	post(editor, "/merge/apply", mergeRequest{
		ApplyMetadataRequest: ApplyMetadataRequest{Title: "Merged", Creators: "Ann", Subjects: "a-tag, b-tag"},
		Keep:                 a.uuid,
		UUIDs:                []string{a.uuid, b.uuid},
		Formats:              map[string]string{"epub": b.uuid},
		Cover:                b.uuid,
	})
	if n := count(t, db, `SELECT COUNT(*) FROM books WHERE uuid = ?`, b.uuid); n != 0 {
		t.Fatal("B not merged away")
	}
	if got := readFile(t, aEPUB); got != "B EPUB" {
		t.Fatalf("A's EPUB %q", got)
	}

	entries, _ := editLogQuery(0, 0, 0)
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action+"/"+e.Kind)
	}
	if want := []string{"merge/recycle", "merge/cover", "merge/book"}; !slices.Equal(actions, want) {
		t.Fatalf("logged %v, want %v", actions, want)
	}
	var res struct{ Results []revertResult }
	_ = json.Unmarshal(post(admin, "/api/admin/activity/revert", map[string]any{"ids": []int64{entries[0].ID, entries[1].ID, entries[2].ID}}).Body.Bytes(), &res)
	for _, r := range res.Results {
		if !r.Done {
			t.Errorf("not undone: %+v", r)
		}
	}
	if n := count(t, db, `SELECT COUNT(*) FROM books WHERE uuid = ?`, b.uuid); n != 1 {
		t.Error("B not restored")
	}
	if got := readFile(t, aEPUB); got != "EPUB!" {
		t.Errorf("A's EPUB %q, want its own back", got)
	}
	if got := readFile(t, filepath.Join(baseDir, a.path, "cover.jpg")); got != "jpg" {
		t.Errorf("A's cover %q", got)
	}
	if got := snapBook(a.uuid); !sameJSON(mustJSONBytes(beforeA), got) {
		t.Errorf("A %+v, want %+v", got, beforeA)
	}
}

// The Activity page lists entries with their Undo buttons, and the Users
// page has the Editor box.
func TestActivityAndUsersPagesRender(t *testing.T) {
	setupTestAppDB(t)
	admin, _ := createUser("admin1", "correct horse battery", true)
	ed, _ := createUser("editor1", "correct horse battery", false)
	_, _ = appDB.Exec(`UPDATE users SET is_editor = 1 WHERE id = ?`, ed.ID)
	logEdit(ed, "edit", logKindBook, "u-1", "Some <Book>", nil, map[string]string{"title": "x"})
	id := logEdit(ed, "remove placeholder", logKindPlaceholder, "p-1", "Gone", map[string]string{"x": "y"}, nil)
	_, _ = appDB.Exec(`UPDATE edit_log SET reverted_at = 1, reverted_by = 'admin1' WHERE id = ?`, id)

	rec := httptest.NewRecorder()
	activityPage(rec, asUser(httptest.NewRequest("GET", "/admin/activity?user="+itoa(ed.ID), nil), admin))
	body := rec.Body.String()
	for _, want := range []string{`data-undo="1"`, "Some &lt;Book&gt;", "Undone by admin1", `href="/book/get/u-1"`, `id="undo-since"`, "editor1 (editor)"} {
		if !strings.Contains(body, want) {
			t.Errorf("activity page lacks %q", want)
		}
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}

	rec = httptest.NewRecorder()
	adminUsersPage(rec, asUser(httptest.NewRequest("GET", "/admin/users", nil), admin))
	if !strings.Contains(rec.Body.String(), `data-field="is_editor" checked`) {
		t.Error("users page: editor1 not shown as an editor")
	}
}
