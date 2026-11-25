package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestAggregateTiers(t *testing.T) {
	tierCounts := func(counts []int) [6]int {
		var n [6]int
		th := aggregateTiers(counts)
		for _, c := range counts {
			n[tierOf(th, c)]++
		}
		return n
	}

	// A long tail: one huge entry no longer shrinks everything else to the
	// smallest size, and each tier is a band of ranks.
	var long []int
	for i := 1; i <= 1000; i++ {
		long = append(long, 2+2000/i) // descending, as the cache sorts them
	}
	if !slices.IsSortedFunc(long, func(a, b int) int { return b - a }) {
		t.Fatal("fixture not descending")
	}
	n := tierCounts(long)
	for tier := 1; tier <= 5; tier++ {
		if n[tier] == 0 {
			t.Errorf("tier %d empty: %v", tier, n)
		}
	}
	if n[5] > 20 || n[1] < 500 {
		t.Errorf("tiers out of proportion: %v", n)
	}

	// All equal: all the same (smallest) size.
	if n := tierCounts([]int{2, 2, 2, 2}); n[1] != 4 {
		t.Errorf("equal counts: %v, want all tier 1", n)
	}

	// Few, distinct: more books is never a smaller tier, and the largest
	// stands out from the smallest.
	th := aggregateTiers([]int{9, 4, 3, 2})
	if !(tierOf(th, 9) >= tierOf(th, 4) && tierOf(th, 4) >= tierOf(th, 3) && tierOf(th, 3) >= tierOf(th, 2) && tierOf(th, 9) > tierOf(th, 2)) {
		t.Errorf("tiers not monotonic: %v", th)
	}

	// Thresholds strictly rise; single-book search results get tier 1.
	for i := 1; i < len(th); i++ {
		if th[i] <= th[i-1] {
			t.Errorf("thresholds not rising: %v", th)
		}
	}
	if tierOf(th, 1) != 1 {
		t.Errorf("tierOf(1) = %d, want 1", tierOf(th, 1))
	}
	_ = aggregateTiers(nil)
}

// The order bar and Discover's "Show more" are drawn shown by the server, not
// un-hidden by aggregate.js once the page is in, so the heading row doesn't
// jump after first paint. Fewer than a dozen entries: no order bar.
func TestAggregateControlsOnFirstPaint(t *testing.T) {
	render := func(n int) string {
		t.Helper()
		data := AggregatePageData{Title: "Tags", Kind: "tag", Label: "tag", Prefix: "/tag/",
			Discover: []AggregateDisplayItem{{Name: "d", Href: "/tag/d", Count: 1, Tier: 1}}}
		for i := range n {
			data.Items = append(data.Items, AggregateDisplayItem{Name: fmt.Sprint("t", i), Href: fmt.Sprint("/tag/t", i), Count: 2, Tier: 1})
		}
		data.Total = n
		var buf bytes.Buffer
		if err := templates.ExecuteTemplate(&buf, "aggregate.html", data); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	page := render(12)
	if !strings.Contains(page, `id="agg-order" class="agg-order" role="group" aria-label="Order">`) {
		t.Error("12 entries: order bar not shown on first paint")
	}
	if !strings.Contains(page, `id="agg-reshuffle" class="agg-pill">`) {
		t.Error(`"Show more" not shown on first paint`)
	}
	if page := render(11); !strings.Contains(page, `aria-label="Order" hidden>`) || strings.Contains(page, `data-order="az"`) {
		t.Error("11 entries: order bar shown")
	}
}

// A big cloud comes in two parts: the page draws the first aggPageFirst
// entries and marks that there are more; ?part=rest draws the others, in
// the same order, each once.
func TestAggregateCloudInTwoParts(t *testing.T) {
	db := catalogFixture(t)
	a := addTestBook(t, db, "One", []string{"Ann"}, nil, "")
	b := addTestBook(t, db, "Two", []string{"Ann"}, nil, "")
	n := aggPageFirst + 25
	for i := range n {
		name := fmt.Sprintf("Tag %04d", i)
		mustExec(t, db, `INSERT INTO tags (name) VALUES (?)`, name)
		for _, u := range []string{a, b} {
			mustExec(t, db, `INSERT INTO books_tags_link (book, tag) SELECT b.id, t.id FROM books b, tags t WHERE b.uuid = ? AND t.name = ?`, u, name)
		}
	}
	c := categoryByKey("tag")
	refreshAggregateCache(c)
	get := func(path string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		aggregatePageHandler(c)(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	hrefs := func(html string) []string {
		return regexp.MustCompile(`class="agg-item[^"]*" href="(/tag/[^"]+)"`).FindAllString(html, -1)
	}
	page, rest := get("/tags"), get("/tags?part=rest")
	first, more := hrefs(page), hrefs(rest)
	if len(first) != aggPageFirst || !strings.Contains(page, `data-rest=""`) {
		t.Fatalf("page: %d entries, data-rest %v", len(first), strings.Contains(page, `data-rest=""`))
	}
	if len(more) != n-aggPageFirst || strings.Contains(rest, "<html") {
		t.Fatalf("rest: %d entries (want %d), a whole page: %v", len(more), n-aggPageFirst, strings.Contains(rest, "<html"))
	}
	seen := map[string]bool{}
	for _, h := range append(first, more...) {
		if seen[h] {
			t.Errorf("listed twice: %s", h)
		}
		seen[h] = true
	}
}
