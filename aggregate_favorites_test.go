package main

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"
)

func favMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me", meHandler)
	mux.HandleFunc("GET /api/me/favorites", requireUser(aggFavoritesHandler))
	mux.HandleFunc("PUT /api/me/favorites/{kind}/{name}", requireUser(aggFavoriteHandler))
	mux.HandleFunc("DELETE /api/me/favorites/{kind}/{name}", requireUser(aggUnfavoriteHandler))
	return mux
}

// favFixture: a library with two authors and a series, and its caches.
func favFixture(t *testing.T) (herbert, other string) {
	t.Helper()
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	herbert = addTestBook(t, db, "Dune", []string{"Frank Herbert"}, []string{"Fiction"}, "Dune", "epub")
	other = addTestBook(t, db, "Elsewhere", []string{"Someone Else"}, []string{"Fiction"}, "", "epub")
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	refreshAllCaches()
	return herbert, other
}

func TestAggregateFavorites(t *testing.T) {
	favFixture(t)
	alice, _ := createUser("alice", "a long enough password", false)
	bob, _ := createUser("bob", "a long enough password", false)
	mux := favMux()
	do := func(u *User, method, path string, want int) []byte {
		t.Helper()
		rec := call(t, mux, u, method, path, "")
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s (want %d)", method, path, rec.Code, rec.Body, want)
		}
		return rec.Body.Bytes()
	}
	list := func(u *User) []aggFavorite {
		t.Helper()
		var got struct {
			Favs      int64         `json:"favs"`
			Favorites []aggFavorite `json:"favorites"`
		}
		if err := json.Unmarshal(do(u, "GET", "/api/me/favorites", 200), &got); err != nil {
			t.Fatal(err)
		}
		return got.Favorites
	}
	stamp := func(u *User) int64 {
		t.Helper()
		var me struct {
			Favs int64 `json:"favs"`
		}
		if err := json.Unmarshal(do(u, "GET", "/api/me", 200), &me); err != nil {
			t.Fatal(err)
		}
		return me.Favs
	}

	do(nil, "PUT", "/api/me/favorites/author/Frank%20Herbert", 401)
	do(alice, "PUT", "/api/me/favorites/nope/Frank%20Herbert", 404)
	do(alice, "PUT", "/api/me/favorites/author/Nobody%20At%20All", 404)
	do(alice, "GET", "/api/me/favorites/author/Frank%20Herbert", 405)

	s0 := stamp(alice)
	do(alice, "PUT", "/api/me/favorites/author/frank%20herbert", 204) // any case: the library's name kept
	do(alice, "PUT", "/api/me/favorites/author/Frank%20Herbert", 204)
	do(alice, "PUT", "/api/me/favorites/series/Dune", 204)
	got := list(alice)
	if len(got) != 2 || got[0].Name != "Dune" || got[1].Name != "Frank Herbert" || got[1].Href != "/author/Frank%20Herbert" || got[1].Count != 1 {
		t.Fatalf("list: %+v", got)
	}
	s1 := stamp(alice)
	if s1 <= s0 {
		t.Fatalf("stamp didn't change: %d -> %d", s0, s1)
	}
	if len(list(bob)) != 0 {
		t.Fatal("bob sees alice's favorites")
	}

	do(alice, "DELETE", "/api/me/favorites/author/FRANK%20HERBERT", 204)
	if got := list(alice); len(got) != 1 || got[0].Kind != "series" {
		t.Fatalf("after delete: %+v", got)
	}
	if s2 := stamp(alice); s2 <= s1 {
		t.Fatalf("stamp didn't change on delete: %d -> %d", s1, s2)
	}
	var n int
	favScan(t, `SELECT COUNT(*) FROM aggregate_favorites WHERE user_id = ? AND kind = 'author'`, alice.ID, &n)
	if n != 0 {
		t.Fatalf("a local favorite is deleted, not kept: %d rows", n)
	}

	// An imported row is tombstoned, and favoriting it again revives it.
	if _, err := appDB.Exec(`INSERT INTO aggregate_favorites (user_id, kind, name, origin, created_at)
		VALUES (?, 'author', 'Someone Else', 'hardcover', 1)`, alice.ID); err != nil {
		t.Fatal(err)
	}
	do(alice, "DELETE", "/api/me/favorites/author/Someone%20Else", 204)
	var removed *int64
	favScan(t, `SELECT removed_at FROM aggregate_favorites WHERE user_id = ? AND name = 'Someone Else'`, alice.ID, &removed)
	if removed == nil || len(list(alice)) != 1 {
		t.Fatal("imported row not tombstoned")
	}
	do(alice, "PUT", "/api/me/favorites/author/Someone%20Else", 204)
	if len(list(alice)) != 2 {
		t.Fatal("tombstoned row not revived")
	}

	// Deleting the user takes their favorites with them.
	if _, err := appDB.Exec(`DELETE FROM users WHERE id = ?`, alice.ID); err != nil {
		t.Fatal(err)
	}
	favScan(t, `SELECT COUNT(*) FROM aggregate_favorites WHERE user_id = ?`, alice.ID, &n)
	if n != 0 {
		t.Fatalf("%d rows left after the user", n)
	}
}

func TestAggregateFavoritesCap(t *testing.T) {
	favFixture(t)
	alice, _ := createUser("alice", "a long enough password", false)
	for i := range maxAggFavorites {
		if _, err := appDB.Exec(`INSERT INTO aggregate_favorites (user_id, kind, name, created_at) VALUES (?, 'tag', ?, 1)`,
			alice.ID, fmt.Sprintf("t%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if rec := call(t, favMux(), alice, "PUT", "/api/me/favorites/author/Frank%20Herbert", ""); rec.Code != http.StatusConflict {
		t.Fatalf("past the cap: %d", rec.Code)
	}
}

func TestRecommendFavorites(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	add := func(title, author string, tags []string, series string) string {
		return addTestBook(t, db, title, []string{author}, tags, series, "epub")
	}
	liked := add("Liked Author Book", "Liked", []string{"Fiction"}, "")
	var plain []string
	for _, title := range []string{"P1", "P2", "P3", "P4", "P5"} {
		plain = append(plain, add(title, "Plain", []string{"Fiction"}, ""))
	}
	saga := add("Saga One", "Saga Writer", []string{"Epic"}, "Saga")
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	rng := func() *rand.Rand { return rand.New(rand.NewPCG(1, 2)) }
	none := readingMarks{Reading: map[string]*float64{}}

	// No marks, no favorites: nothing.
	if l, _ := rankRecs(none, nil, nil, rng()); l.len() != 0 {
		t.Fatalf("nothing to go on: %d", l.len())
	}
	// Favorites alone: the author's book first among the tag's books, and
	// the favorite series' first book with the next-in-series ones.
	favs := recFavs{"author": {"liked": true}, "tag": {"fiction": true}, "series": {"saga": true}}
	l, _ := rankRecs(none, favs, nil, rng())
	if l.len() == 0 || l.uuid(0) != saga || l.at(0).Kind != "series" {
		t.Fatalf("first: %+v", l.page(0))
	}
	if l.uuid(1) != liked {
		t.Fatalf("favorite author's book not next: %+v", l.page(0))
	}
	// With a seed of the plain author, the favorite author still wins.
	m := readingMarks{Reading: map[string]*float64{}, Finished: []string{plain[0]}}
	l, _ = rankRecs(m, recFavs{"author": {"liked": true}}, nil, rng())
	if l.uuid(0) != liked {
		t.Fatalf("favorite author vs seed author: %+v", l.page(0))
	}
}

// favScan scans one row of the app database into dest.
func favScan(t *testing.T, q string, arg any, dest any) {
	t.Helper()
	if err := appDB.QueryRow(q, arg).Scan(dest); err != nil {
		t.Fatal(err)
	}
}

// /me/favorites: the favorite authors, series, … as cards above the books,
// authors first, with an author's picture from Hardcover where there is one.
func TestFavoritesPageCarousel(t *testing.T) {
	favFixture(t)
	alice, _ := createUser("alice", "a long enough password", false)
	mux := favMux()
	mux.HandleFunc("GET /me/{list}", requireUser(meListHandler))
	for _, p := range []string{"/api/me/favorites/series/Dune", "/api/me/favorites/author/Frank%20Herbert"} {
		if rec := call(t, mux, alice, "PUT", p, ""); rec.Code != 204 {
			t.Fatalf("PUT %s: %d", p, rec.Code)
		}
	}
	img := "0123456789abcdef0123456789abcdef.jpg"
	if _, err := appDB.Exec(`INSERT INTO hardcover_authors (name, hc_id, image, checked_at) VALUES ('Frank Herbert', 1, ?, 1)`, img); err != nil {
		t.Fatal(err)
	}
	cards, err := userFavCards(alice)
	if err != nil || len(cards) != 2 || cards[0].Name != "Frank Herbert" || cards[0].ImageURL != "/author-images/"+img ||
		cards[1].Name != "Dune" || cards[1].ImageURL != "" || cards[1].Initial != "D" || cards[1].KindLabel != "Series" {
		t.Fatalf("cards: %+v %v", cards, err)
	}
	rec := call(t, mux, alice, "GET", "/me/favorites", "")
	body := rec.Body.String()
	// Authors, then series, each its own carousel, then the books' heading.
	at := func(sub string) int {
		t.Helper()
		i := strings.Index(body, sub)
		if i < 0 {
			t.Fatalf("no %s on the page", sub)
		}
		return i
	}
	// Series: their grid entries, for the page to draw as a shelf's series cards.
	if !strings.Contains(body, `data-books="[{`) || !strings.Contains(body, `&#34;uuid&#34;:&#34;series:Dune&#34;`) || !strings.Contains(body, `&#34;series_card&#34;:{&#34;name&#34;:&#34;Dune&#34;,&#34;count&#34;:1`) {
		t.Fatal("series row without its entries")
	}
	if rec.Code != 200 || strings.Contains(body, `class="fav-chips"`) || !strings.Contains(body, `src="/author-images/`+img+`"`) ||
		!(at(`id="fav-author-list"`) < at(`id="fav-series-list"`) && at(`id="fav-series-list"`) < at(`<h2>Books</h2>`) && at(`<h2>Books</h2>`) < at(`id="books"`)) {
		t.Fatalf("page: %d", rec.Code)
	}
	// A tag: a chip under its heading, between the carousels and the books.
	if rec := call(t, mux, alice, "PUT", "/api/me/favorites/tag/Fiction", ""); rec.Code != 204 {
		t.Fatalf("PUT tag: %d", rec.Code)
	}
	body = call(t, mux, alice, "GET", "/me/favorites", "").Body.String()
	if strings.Contains(body[:at(`class="fav-chips"`)], ">Fiction") || !strings.Contains(body, `<a class="agg-chip" href="/tag/Fiction" title="Tag">Fiction`) ||
		!(at(`id="fav-series-list"`) < at(`id="fav-chips-h">Tags</h2>`) && at(`id="fav-chips-h">Tags</h2>`) < at(`<h2>Books</h2>`)) {
		t.Fatal("tag not a chip between the carousels and the books")
	}
	// None: no carousel.
	bob, _ := createUser("bob", "a long enough password", false)
	if body := call(t, mux, bob, "GET", "/me/favorites", "").Body.String(); strings.Contains(body, "fav-aggs") || strings.Contains(body, "<h2>Books</h2>") {
		t.Fatal("carousel or books heading without favorites")
	}
}
