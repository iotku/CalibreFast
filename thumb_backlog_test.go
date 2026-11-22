package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func drainBacklog() {
	for {
		select {
		case uuid := <-thumbBacklog:
			backlogged.Delete(uuid)
		default:
			return
		}
	}
}

func TestBacklogThumbOnce(t *testing.T) {
	defer drainBacklog()
	backlogThumb("a-1")
	backlogThumb("a-1")
	backlogThumb("b-2")
	if n := len(thumbBacklog); n != 2 {
		t.Errorf("backlog has %d entries, want 2", n)
	}
}

// A request cancelled while waiting for a slot goes to the backlog instead.
func TestCoverThumbCancelledWhileWaiting(t *testing.T) {
	defer drainBacklog()
	for range cap(imageSem) {
		imageSem <- struct{}{}
	}
	defer func() {
		for range cap(imageSem) {
			<-imageSem
		}
	}()

	// The handler's cache check looks the book up first; seed its path so
	// that lookup doesn't need Calibre's database (there's no cover.jpg
	// there, so there's no cached thumbnail either).
	uuidPathIndex.Store("c-3", "nowhere/c-3")
	defer uuidPathIndex.Delete("c-3")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/cover-thumb/c-3", nil).WithContext(ctx)
	coverThumbHandler(httptest.NewRecorder(), req)

	if _, ok := backlogged.Load("c-3"); !ok {
		t.Error("cancelled request wasn't backlogged")
	}
	if thumbRequests.Load() != 0 {
		t.Error("request still counted as in progress")
	}
}

// warmingOn lets warmThumbs queue for one test (the backlog worker doesn't
// run in tests), and empties the backlog after.
func warmingOn(t *testing.T) {
	t.Helper()
	drainBacklog()
	backlogRunning.Store(true)
	t.Cleanup(func() {
		backlogRunning.Store(false)
		drainBacklog()
	})
}

// queued waits for the warming goroutines to queue want thumbnails, and
// returns what the backlog has.
func queued(t *testing.T, want int) map[string]bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(thumbBacklog) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // and nothing more
	out := map[string]bool{}
	for {
		select {
		case u := <-thumbBacklog:
			backlogged.Delete(u)
			out[u] = true
		default:
			return out
		}
	}
}

// Only thumbnails that need making are queued: not one in the cache, nor a
// book known to have no cover; and nothing while no backlog runs.
func TestWarmThumbsSkipsWhatsThere(t *testing.T) {
	db := catalogFixture(t)
	covered, bare := thumbLibrary(t, db, 2)
	if _, _, err := loadThumb(covered[0], func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	noCover.Store(bare, struct{}{})

	warmThumbs(covered...)
	if n := len(thumbBacklog); n != 0 {
		t.Fatalf("queued %d with no backlog running", n)
	}
	warmingOn(t)
	warmThumbs(append(covered, bare, "")...)
	if got := queued(t, 1); len(got) != 1 || !got[covered[1]] {
		t.Errorf("queued %v, want only %s", got, covered[1])
	}
}

// A tag strip's answer warms the strip after it: where the row starts on
// the next visit (and what its Shuffle button shows).
func TestTagStripWarmsNextStrip(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	origGroups := groups
	groups = loadGroups(filepath.Join(t.TempDir(), groupsFileName))
	t.Cleanup(func() { groups = origGroups })
	for i := range 10 {
		addTestBook(t, db, "Book "+strconv.Itoa(i), []string{"A"}, []string{"Space Opera"}, "", "epub")
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tag-strip", tagStripHandler)
	strip := func(query string) tagStripAnswer {
		t.Helper()
		rec := call(t, mux, nil, "GET", "/api/tag-strip?tag=space%20opera&n=3&"+query, "")
		var a tagStripAnswer
		if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		return a
	}
	warmingOn(t)
	first := strip("start=0")
	next := strip("start=" + strconv.Itoa(first.Next))
	got := queued(t, 6) // this answer's next strip, and the one after next's
	for _, b := range next.Books {
		if !got[b.UUID] {
			t.Errorf("next strip's %s not warmed (got %v)", b.Title, got)
		}
	}
	for _, b := range first.Books {
		if got[b.UUID] {
			t.Errorf("%s warmed: it was just shown", b.Title)
		}
	}
}

// The books warmed for a Shuffle page are the page file's.
func TestShufflePageUUIDsMatchFiles(t *testing.T) {
	db := catalogFixture(t)
	t.Chdir(t.TempDir())
	for i := range 5 {
		addTestBook(t, db, "Book "+strconv.Itoa(i), []string{"A"}, nil, "")
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(shufflePageFile(1))
	if err != nil {
		t.Fatal(err)
	}
	var books []Book
	if err := json.Unmarshal(data, &books); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(shufflePageUUIDs(currentCatalog(), 1), ","), strings.Join(bookUUIDs(books), ","); got != want {
		t.Errorf("page 1: %s, file has %s", got, want)
	}
	if got := shufflePageUUIDs(currentCatalog(), 2); len(got) != 0 {
		t.Errorf("page 2: %v, want none", got)
	}
	for p, want := range map[string]int{"/pages/shuffle/page3.json": 3, "/pages/shuffle/page0.json": 0, "/pages/page3.json": 0, "/pages/shuffle/page3.jsonx": 0, "/pages/shuffle/pagex.json": 0} {
		if n, ok := shufflePageFromPath(p); n != want || ok != (want > 0) {
			t.Errorf("%s: %d %v", p, n, ok)
		}
	}
}

// Next's books, without making the row's recBooks.
func TestRecListPageUUIDs(t *testing.T) {
	c := newCatalog()
	for i := range 40 {
		c.books = append(c.books, catalogBook{Book: Book{UUID: "b" + strconv.Itoa(i)}})
	}
	l := recList{
		series: []recBook{{Book: Book{UUID: "s0"}}},
		n:      30,
		slot:   func(i int) int32 { return int32(i) },
		cat:    c,
	}
	p1 := l.pageUUIDs(1)
	if len(p1) != min(recRowMax, l.len()-recRowMax) || p1[0] != "b"+strconv.Itoa(recRowMax-1) {
		t.Errorf("page 1: %v", p1)
	}
	if got := l.pageUUIDs(l.pages()); len(got) != 0 {
		t.Errorf("past the last page: %v", got)
	}
}
