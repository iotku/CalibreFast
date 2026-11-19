package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The queue's list answers while a scan is still taking books in: staging
// happens without the watcher's lock, and the list says how many are
// being taken in.
func TestQueueListDoesNotWaitForStaging(t *testing.T) {
	w, dir := newTestWatcher(t)
	origImports, origWatcher := imports, watcher
	imports, watcher = w.inbox, w
	t.Cleanup(func() { imports, watcher = origImports, origWatcher })
	for i := range 3 {
		writeTestFile(t, filepath.Join(dir, fmt.Sprintf("Book %d.epub", i)), titledEPUB(t, fmt.Sprintf("Book %d", i), "A", ""))
	}
	w.scan() // seen once: taken by the next

	// Every file move waits until the test lets it go.
	for range cap(stageIO) {
		stageIO <- struct{}{}
	}
	done := make(chan int)
	go func() { done <- w.scan() }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, staging, _, _ := w.status(); staging == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("staging never started")
		}
	}
	answered := make(chan importQueue)
	go func() {
		rec := httptest.NewRecorder()
		importQueueHandler(rec, httptest.NewRequest("GET", "/api/imports", nil))
		var q importQueue
		json.Unmarshal(rec.Body.Bytes(), &q)
		answered <- q
	}()
	select {
	case q := <-answered:
		if q.Staging != 3 {
			t.Errorf("staging %d, want 3", q.Staging)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the list waited for the scan")
	}
	for range cap(stageIO) {
		<-stageIO
	}
	if n := <-done; n != 3 {
		t.Errorf("took %d, want 3", n)
	}
	if _, staging, _, _ := w.status(); staging != 0 {
		t.Errorf("still staging %d", staging)
	}
}

// Staged several at a time, every book is taken exactly once.
func TestParallelStagingTakesEachBookOnce(t *testing.T) {
	orig := importWorkers
	importWorkers = 8
	t.Cleanup(func() { importWorkers = orig })
	w, dir := newTestWatcher(t)
	const n = 25
	var want []string
	for i := range n {
		name := fmt.Sprintf("Book %02d.epub", i)
		want = append(want, name)
		sub := filepath.Join(dir, fmt.Sprintf("d%d", i%3))
		os.MkdirAll(sub, 0o755)
		writeTestFile(t, filepath.Join(sub, name), titledEPUB(t, name, "A", ""))
	}
	w.scan()
	if got := w.scan(); got != n {
		t.Fatalf("took %d, want %d", got, n)
	}
	var got []string
	for _, it := range w.inbox.list() {
		for _, f := range it.Files {
			got = append(got, f.Name)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("queued %v", got)
	}
	if w.scan() != 0 || w.inbox.count() != n {
		t.Errorf("a later scan took more: %d waiting", w.inbox.count())
	}
}

// The list's check from the in-memory catalog finds what metadata.db's
// does: same title and authors, an identical file under the same title,
// and nothing for a title alone.
func TestDupIndexAgreesWithDatabase(t *testing.T) {
	db := withTestLibrary(t)
	emma := titledEPUB(t, "Emma", "Jane Austen", "the library's")
	uploadFiles(t, "Dune.epub", titledEPUB(t, "Dune", "Frank Herbert", "first"), false)
	uploadFiles(t, "Emma.epub", emma, false)
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)
	t.Cleanup(func() { libraryCatalog.Store(nil) })
	idx := currentDupIndex()
	if idx == nil || currentDupIndex() != idx {
		t.Fatal("index not built once for the catalog")
	}

	dir := t.TempDir()
	write := func(name string, data []byte) []dupFile {
		p := filepath.Join(dir, name)
		writeTestFile(t, p, data)
		return []dupFile{{p, "epub"}}
	}
	cases := []struct {
		name, title string
		authors     []string
		files       []dupFile
		found       bool
	}{
		{"same title and authors", "  dune ", []string{"frank herbert"}, write("a.epub", titledEPUB(t, "Dune", "Frank Herbert", "other")), true},
		{"title only", "Dune", []string{"Someone Else"}, write("b.epub", titledEPUB(t, "Dune", "Someone Else", "")), false},
		{"identical file", "Emma", []string{"J. Austen"}, write("c.epub", emma), true},
		{"new book", "Persuasion", []string{"Jane Austen"}, write("d.epub", titledEPUB(t, "Persuasion", "Jane Austen", "")), false},
	}
	for _, tc := range cases {
		fromDB, err := findDuplicates(db, baseDir, tc.title, tc.authors, tc.files, "")
		if err != nil {
			t.Fatal(err)
		}
		fromIdx, err := idx.find(baseDir, tc.title, tc.authors, tc.files)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(fromDB)
		b, _ := json.Marshal(fromIdx)
		if string(a) != string(b) {
			t.Errorf("%s:\n db    %s\n index %s", tc.name, a, b)
		}
		if fromIdx.found() != tc.found {
			t.Errorf("%s: found %v", tc.name, fromIdx.found())
		}
	}
}
