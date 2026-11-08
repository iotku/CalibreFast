package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGroupStore_SetResolveExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, groupsFileName)
	s := loadGroups(path)

	if _, err := s.set("language", "en", Group{Name: "English", Members: []string{"en", "eng", " ENG ", ""}}); err != nil {
		t.Fatal(err)
	}
	name, members, grouped := s.resolve("language", "EN")
	if !grouped || name != "English" || !slices.Equal(members, []string{"en", "eng"}) {
		t.Fatalf("resolve by member: %q %v %v", name, members, grouped)
	}
	if name, _, _ := s.resolve("language", "english"); name != "English" {
		t.Fatalf("resolve by name: %q", name)
	}
	if name, members, grouped := s.resolve("language", "fr"); grouped || name != "fr" || !slices.Equal(members, []string{"fr"}) {
		t.Fatalf("ungrouped resolve: %q %v %v", name, members, grouped)
	}

	// Moving "eng" into another group takes it out of English.
	if _, err := s.set("language", "en-US", Group{Name: "US English", Members: []string{"en-US", "eng"}}); err != nil {
		t.Fatal(err)
	}
	if _, m, _ := s.resolve("language", "English"); !slices.Equal(m, []string{"en"}) {
		t.Fatalf("eng not moved out of English: %v", m)
	}

	// Name collisions are refused, nothing changes.
	if _, err := s.set("language", "fr", Group{Name: "english", Members: []string{"fr"}}); err != errGroupNameTaken {
		t.Fatalf("expected name collision, got %v", err)
	}

	// Emptying a group dissolves it.
	if g, err := s.set("language", "US English", Group{Name: "US English"}); err != nil || g.Name != "" {
		t.Fatalf("dissolve: %+v %v", g, err)
	}
	if _, _, grouped := s.resolve("language", "en-US"); grouped {
		t.Fatal("en-US still grouped after dissolve")
	}

	// Persisted, with a backup of the previous version.
	reloaded := loadGroups(path)
	if got := reloaded.all("language"); len(got) != 1 || got[0].Name != "English" {
		t.Fatalf("reloaded: %+v", got)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("expected backup file: %v", err)
	}
}

// A malformed groups file must never be overwritten.
func TestGroupStore_MalformedFileIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), groupsFileName)
	orig := []byte(`{"language": [{"name": "English", "members": ["en", "eng"]}`) // truncated
	if err := os.WriteFile(path, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	s := loadGroups(path)
	if s.readOnly == nil {
		t.Fatal("expected read-only store")
	}
	if _, err := s.set("language", "en", Group{Name: "X", Members: []string{"en", "fr"}}); err == nil {
		t.Fatal("expected save to be refused")
	}
	if got, _ := os.ReadFile(path); string(got) != string(orig) {
		t.Fatalf("malformed file was modified:\n%s", got)
	}
}

func TestLanguageGroups_FilteredAndAggregate(t *testing.T) {
	db, tempDir := setupTestDB(t)
	defer os.RemoveAll(tempDir)
	defer db.Close()

	origDB, origBaseDir, origGroups := calibreDB, baseDir, groups
	calibreDB, baseDir = db, tempDir
	groups = loadGroups(filepath.Join(tempDir, groupsFileName))
	defer func() { calibreDB, baseDir, groups = origDB, origBaseDir, origGroups }()
	defer refreshAllCaches()

	insert := func(title, lang string) int64 {
		id, _, _, err := insertBookIntoCalibreDB(db, &UploadedBookMeta{
			OPFMetadata: OPFMetadata{Title: title, Creators: []string{"A"}, Languages: []string{lang}},
		}, nil)
		if err != nil {
			t.Fatalf("insert %s: %v", title, err)
		}
		return id
	}
	insert("One", "en")
	both := insert("Two", "eng")
	insert("Three", "fr")
	// A book tagged with both variants must be counted once.
	if _, err := db.Exec(`INSERT INTO books_languages_link (book, lang_code, item_order)
		SELECT ?, id, 1 FROM languages WHERE lang_code = 'en'`, both); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	lang := categoryByKey("language")
	mux.HandleFunc("/lang/", filteredPageHandler(lang))
	mux.HandleFunc("PUT /api/groups/{cat}/{value...}", groupPutHandler)
	server := httptest.NewServer(mux)
	defer server.Close()

	countBooksAt := func(path string) int {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var books []Book
		if err := json.NewDecoder(resp.Body).Decode(&books); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return len(books)
	}

	if n := countBooksAt("/lang/en?api=1"); n != 2 {
		t.Fatalf("ungrouped en: expected 2 books, got %d", n)
	}

	req, _ := http.NewRequest(http.MethodPut, server.URL+"/api/groups/language/en",
		strings.NewReader(`{"name":"English","members":["en","eng"]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ URL string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || out.URL != "/lang/English" {
		t.Fatalf("PUT group: status %d url %q", resp.StatusCode, out.URL)
	}

	for _, p := range []string{"/lang/English?api=1", "/lang/eng?api=1", "/lang/EN?api=1"} {
		if n := countBooksAt(p); n != 2 {
			t.Errorf("%s: expected 2 distinct books, got %d", p, n)
		}
	}

	items := queryAggregateItems(lang)
	var english *AggregateItem
	for i := range items {
		if items[i].Name == "en" || items[i].Name == "eng" {
			t.Errorf("grouped member %q listed separately", items[i].Name)
		}
		if items[i].Name == "English" {
			english = &items[i]
		}
	}
	if english == nil || english.Count != 2 {
		t.Fatalf("expected English group with 2 distinct books, got %+v", items)
	}
}
