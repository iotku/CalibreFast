package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iotku/CalibreFast/pkg/koreader"
	"github.com/iotku/CalibreFast/pkg/koreader/koreadertest"
)

func koMux() *http.ServeMux {
	mux := annotationMux()
	mux.HandleFunc("GET /api/me/annotations/{uuid}/koreader", requireUser(koreaderExportHandler))
	mux.HandleFunc("POST /api/me/annotations/{uuid}/koreader", requireUser(koreaderImportHandler))
	return mux
}

func listAnnotations(t *testing.T, mux http.Handler, u *User, book string) []annotationRec {
	t.Helper()
	var out struct{ Annotations []annotationRec }
	_ = json.Unmarshal(call(t, mux, u, "GET", "/api/me/annotations/"+book, "").Body.Bytes(), &out)
	return out.Annotations
}

func koImport(t *testing.T, mux http.Handler, u *User, book, format, file string) koreaderImport {
	t.Helper()
	rec := call(t, mux, u, "POST", "/api/me/annotations/"+book+"/koreader?format="+format, file)
	if rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	var res koreaderImport
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	return res
}

func TestKOReaderExportImportEPUB(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	other, _ := createUser("other", "longenough-pw", false)
	b := books[0]
	libraryBook(t, b, "A Book - Someone", "epub", koreadertest.KOEPUB(t))
	mux := koMux()
	now := time.Now().UnixMilli()

	if rec := call(t, mux, nil, "GET", "/api/me/annotations/"+b+"/koreader?format=epub", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous export: %d", rec.Code)
	}
	if rec := call(t, mux, nil, "POST", "/api/me/annotations/"+b+"/koreader?format=epub", "return {}"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous import: %d", rec.Code)
	}

	// A highlight made in the web reader: "two three & four", across <b>.
	web := annotationRec{
		Format: "epub", Anchor: "epubcfi(/6/4!/4/2/4,/2/1:0,/3:13)", Text: "two three & four", Note: "a \"note\"\nin two lines",
		Color: "green", Chapter: "Two", Page: 3, CreatedAt: now - 90_000, UpdatedAt: now - 60_000,
	}
	body, _ := json.Marshal(web)
	if rec := call(t, mux, u, "PUT", "/api/me/annotations/"+b+"/web-1", string(body)); rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	// One whose chapter this book doesn't have is left out, not guessed at.
	lost := web
	lost.Anchor = "epubcfi(/6/40!/4/2,/1:0,/1:3)"
	body, _ = json.Marshal(lost)
	call(t, mux, u, "PUT", "/api/me/annotations/"+b+"/web-2", string(body))

	rec := call(t, mux, u, "GET", "/api/me/annotations/"+b+"/koreader?format=epub", "")
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Disposition"), `"A Book - Someone.epub.annotations.lua"`) {
		t.Fatalf("export: %d %q", rec.Code, rec.Header().Get("Content-Disposition"))
	}
	if rec.Header().Get("X-Annotations-Exported") != "1" || rec.Header().Get("X-Annotations-Skipped") != "1" {
		t.Fatalf("exported %s, skipped %s", rec.Header().Get("X-Annotations-Exported"), rec.Header().Get("X-Annotations-Skipped"))
	}
	file := rec.Body.String()
	tab, err := koreader.ParseLuaTable(file)
	if err != nil {
		t.Fatalf("the export doesn't parse: %v\n%s", err, file)
	}
	// Dated so that KOReader's import removes nothing of its own; not marked as paged.
	if tab.Str("datetime") != koreader.Oldest || tab.Str("device_id") != koreaderDevice || tab.Hash["paging"] != nil {
		t.Errorf("header: %+v", tab.Hash)
	}
	item := tab.Table("annotations").Arr[0].(*koreader.LuaTable)
	for key, want := range map[string]string{
		"pos0":     "/body/DocFragment[2]/body/div/p[2]/b/text().0",
		"pos1":     "/body/DocFragment[2]/body/div/p[2]/text()[2].13",
		"page":     "/body/DocFragment[2]/body/div/p[2]/b/text().0",
		"text":     web.Text,
		"note":     web.Note,
		"color":    "green",
		"drawer":   "lighten",
		"chapter":  "Two",
		"datetime": time.UnixMilli(web.CreatedAt).Format(koreader.TimeLayout),
		"epubcfi":  web.Anchor,
	} {
		if got := item.Str(key); got != want {
			t.Errorf("%s: %q, want %q", key, got, want)
		}
	}
	if n, _ := item.Num("pageno"); n != 3 {
		t.Errorf("pageno: %v", n)
	}

	// Back in, for the same user: it is the highlight they already have.
	if res := koImport(t, mux, u, b, "epub", file); res != (koreaderImport{Unchanged: 1}) {
		t.Fatalf("re-import: %+v", res)
	}
	// For someone else: the same place and text.
	if res := koImport(t, mux, other, b, "epub", file); res != (koreaderImport{Added: 1}) {
		t.Fatalf("import: %+v", res)
	}
	got := listAnnotations(t, mux, other, b)
	if len(got) != 1 || got[0].Anchor != web.Anchor || got[0].Text != web.Text || got[0].Note != web.Note ||
		got[0].Color != "green" || got[0].Page != 3 || !strings.HasPrefix(got[0].ID, koreader.IDPrefix) {
		t.Fatalf("imported: %+v", got)
	}
	if res := koImport(t, mux, other, b, "epub", file); res != (koreaderImport{Unchanged: 1}) {
		t.Fatalf("importing twice: %+v", res)
	}
	// Deleted here, it stays deleted when the same file comes again.
	del := got[0]
	del.Deleted, del.UpdatedAt = true, now
	body, _ = json.Marshal(del)
	call(t, mux, other, "PUT", "/api/me/annotations/"+b+"/"+del.ID, string(body))
	if res := koImport(t, mux, other, b, "epub", file); res.Added != 0 || res.Updated != 0 {
		t.Fatalf("a deleted highlight came back: %+v", res)
	}
}

func TestKOReaderImportEPUB(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	b := books[0]
	libraryBook(t, b, "Book", "epub", koreadertest.KOEPUB(t))
	mux := koMux()

	// As a device writes them. The first has exact XPointers. The second's
	// are off (another edition's markup): its text is found instead. The
	// third carries a CFI, as newer KOReader does. Then a page bookmark, a
	// highlight in a chapter this book lacks, and one whose text isn't there.
	file := `return {
    ["annotations"] = {
        [1] = { ["drawer"] = "lighten", ["color"] = "red", ["datetime"] = "2025-03-01 10:00:00",
                ["page"] = "/body/DocFragment[2]/body/div/p[1]/text().6", ["pos0"] = "/body/DocFragment[2]/body/div/p[1]/text().6",
                ["pos1"] = "/body/DocFragment[2]/body/div/p[1]/text().16", ["text"] = "beta gamma", ["chapter"] = "Two", ["pageno"] = 4 },
        [2] = { ["drawer"] = "underscore", ["datetime"] = "2025-03-01 10:05:00", ["datetime_updated"] = "2025-03-02 08:00:00",
                ["pos0"] = "/body/DocFragment[2]/body/section/p[9]/text().2", ["pos1"] = "/body/DocFragment[2]/body/section/p[9]/text().40",
                ["text"] = "three & four\n😀 five", ["note"] = "found by its text" },
        [3] = { ["drawer"] = "lighten", ["color"] = "blue", ["datetime"] = "2026-10-20 09:00:00",
                ["pos0"] = "/body/DocFragment[2]/body/p/text()[1].1", ["pos1"] = "/body/DocFragment[2]/body/p/text()[3].5",
                ["epubcfi"] = "epubcfi(/6/4!/4/4,/1:1,/5:5)", ["text"] = "Lead x y tail" },
        [4] = { ["datetime"] = "2025-03-01 11:00:00", ["page"] = "/body/DocFragment[2]/body/div/p[1]/text().0", ["note"] = "a bookmark" },
        [5] = { ["drawer"] = "lighten", ["datetime"] = "2025-03-01 12:00:00", ["pos0"] = "/body/DocFragment[9]/body/p/text().0",
                ["pos1"] = "/body/DocFragment[9]/body/p/text().5", ["text"] = "Alpha" },
        [6] = { ["drawer"] = "lighten", ["datetime"] = "2025-03-01 13:00:00", ["pos0"] = "/body/DocFragment[2]/body/table/tr/td/text().0",
                ["pos1"] = "/body/DocFragment[2]/body/table/tr/td/text().9", ["text"] = "not in this book" },
    },
    ["datetime"] = "2025-03-02 08:00:01",
    ["device_id"] = "ABCDEF",
}`
	if res := koImport(t, mux, u, b, "epub", file); res != (koreaderImport{Added: 3, Skipped: 2, Bookmarks: 1}) {
		t.Fatalf("import: %+v", res)
	}
	got := listAnnotations(t, mux, u, b)
	if len(got) != 3 {
		t.Fatalf("stored: %+v", got)
	}
	byText := map[string]annotationRec{}
	for _, a := range got {
		byText[a.Text] = a
	}
	first := byText["beta gamma"]
	if first.Anchor != "epubcfi(/6/4!/4/2/2,/1:8,/1:19)" || first.Color != "red" || first.Chapter != "Two" || first.Page != 4 ||
		first.CreatedAt != koreader.Millis("2025-03-01 10:00:00") {
		t.Errorf("exact XPointers: %+v", first)
	}
	second := byText["three & four\n😀 five"]
	if second.Anchor != "epubcfi(/6/4!/4/2/4,/3:1,/3:21)" || second.Color != "yellow" || second.Note != "found by its text" ||
		second.UpdatedAt != koreader.Millis("2025-03-02 08:00:00") {
		t.Errorf("found by text: %+v", second)
	}
	if third := byText["Lead x y tail"]; third.Anchor != "epubcfi(/6/4!/4/4,/1:1,/5:5)" || third.Color != "blue" {
		t.Errorf("with a CFI: %+v", third)
	}

	// The note is edited on the device and the file comes again: one update.
	edited := strings.Replace(file, `["text"] = "beta gamma"`, `["text"] = "beta gamma", ["note"] = "added later", ["datetime_updated"] = "2025-04-01 00:00:00"`, 1)
	if res := koImport(t, mux, u, b, "epub", edited); res.Added != 0 || res.Updated != 1 || res.Unchanged != 2 {
		t.Fatalf("after an edit on the device: %+v", res)
	}
	for _, a := range listAnnotations(t, mux, u, b) {
		if a.Text == "beta gamma" && (a.Note != "added later" || a.ID != first.ID) {
			t.Errorf("edit not applied: %+v", a)
		}
	}

	for name, bad := range map[string]string{
		"not lua":           "PK\x03\x04",
		"code":              `return { annotations = load("x")() }`,
		"no annotations":    `return { ["device_id"] = "x" }`,
		"a PDF's":           `return { ["annotations"] = {}, ["paging"] = true }`,
		"invalid UTF-8":     "return { [\"a\"] = \"\xff\" }",
		"settings, no list": `return { ["annotations"] = 5 }`,
	} {
		if rec := call(t, mux, u, "POST", "/api/me/annotations/"+b+"/koreader?format=epub", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := call(t, mux, u, "POST", "/api/me/annotations/"+b+"/koreader?format=pdf", file); rec.Code != http.StatusNotFound {
		t.Errorf("a format the book doesn't have: %d", rec.Code)
	}
}

func TestKOReaderExportImportPDF(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	other, _ := createUser("other", "longenough-pw", false)
	b := books[0]
	libraryBook(t, b, "Scan", "pdf", []byte("%PDF-1.4"))
	mux := koMux()
	now := time.Now().UnixMilli()

	web := annotationRec{
		Format: "pdf", Anchor: `{"page":3,"rects":[[116.9,82.5,464.1,95.3],[72,96.4,204.9,109.3]]}`, Text: "two lines",
		Color: "yellow", Page: 3, CreatedAt: now - 5000, UpdatedAt: now - 5000,
	}
	body, _ := json.Marshal(web)
	call(t, mux, u, "PUT", "/api/me/annotations/"+b+"/web-1", string(body))

	rec := call(t, mux, u, "GET", "/api/me/annotations/"+b+"/koreader?format=pdf", "")
	tab, err := koreader.ParseLuaTable(rec.Body.String())
	if rec.Code != 200 || err != nil || tab.Hash["paging"] != true {
		t.Fatalf("export: %d %v", rec.Code, err)
	}
	item := tab.Table("annotations").Arr[0].(*koreader.LuaTable)
	if page, _ := item.Num("page"); page != 3 {
		t.Errorf("page: %v", page)
	}
	// pos0 and pos1 are inside the first and last word; the boxes are x, y, width, height.
	pos0, pos1, boxes := item.Table("pos0"), item.Table("pos1"), item.Table("pboxes")
	if x, _ := pos0.Num("x"); x != 118.9 {
		t.Errorf("pos0.x: %v", x)
	}
	if y, _ := pos1.Num("y"); y != (96.4+109.3)/2 {
		t.Errorf("pos1.y: %v", y)
	}
	if w, _ := boxes.Arr[1].(*koreader.LuaTable).Num("w"); len(boxes.Arr) != 2 || w != 204.9-72 {
		t.Errorf("pboxes: %v", w)
	}

	if res := koImport(t, mux, other, b, "pdf", rec.Body.String()); res != (koreaderImport{Added: 1}) {
		t.Fatalf("import: %+v", res)
	}
	if got := listAnnotations(t, mux, other, b); len(got) != 1 || got[0].Anchor != web.Anchor || got[0].Text != web.Text {
		t.Fatalf("imported: %+v", got)
	}
	// A highlight without boxes gets one from its two ends.
	file := `return { ["paging"] = true, ["annotations"] = { [1] = { ["drawer"] = "lighten", ["page"] = 5, ["datetime"] = "2025-01-01 00:00:00",
		["pos0"] = { ["page"] = 5, ["x"] = 100, ["y"] = 200, ["zoom"] = 1.5, ["rotation"] = 0 },
		["pos1"] = { ["page"] = 5, ["x"] = 300, ["y"] = 200 }, ["text"] = "one line" } } }`
	if res := koImport(t, mux, other, b, "pdf", file); res.Added != 1 {
		t.Fatalf("without boxes: %+v", res)
	}
	found := false
	for _, a := range listAnnotations(t, mux, other, b) {
		found = found || a.Anchor == `{"page":5,"rects":[[100,194,300,206]]}`
	}
	if !found {
		t.Errorf("box from the ends: %+v", listAnnotations(t, mux, other, b))
	}
	if rec := call(t, mux, u, "POST", "/api/me/annotations/"+b+"/koreader?format=pdf", `return { ["annotations"] = {} }`); rec.Code != http.StatusBadRequest {
		t.Errorf("an EPUB's file for the PDF: %d", rec.Code)
	}
}
