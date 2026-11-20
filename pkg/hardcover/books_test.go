package hardcover

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// lookupServer answers the lookup queries, and serves covers, for any host
// (the client dials it whatever the URL says), so cover URLs can be on
// assets.hardcover.app.
func lookupServer(t *testing.T) (*Client, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var posts atomic.Int32
	var coverAuth atomic.Value
	coverAuth.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/graphql", func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var data string
		switch {
		case strings.Contains(req.Query, "EditionByISBN"):
			if req.Variables["isbn13"] != "9780552152938" || req.Variables["isbn10"] != "0552152935" {
				data = `{"editions":[]}`
				break
			}
			data = `{"editions":[{"id":9,"book_id":"42","title":"The Last Hero","subtitle":"A Discworld Fable","isbn_13":"9780552152938",
				"release_date":"2002-10-01","pages":176,"cached_image":{"url":"https://assets.hardcover.app/edition/9/cover.jpg"},
				"publisher":{"name":"Corgi"},"language":{"code2":"en","code3":"eng"}}]}`
		case strings.Contains(req.Query, "SearchBooks"):
			if req.Variables["q"] != "The Last Hero Terry Pratchett" {
				t.Errorf("search sent %q", req.Variables["q"])
			}
			data = `{"search":{"results":"{\"hits\":[{\"document\":{\"id\":\"43\"}},{\"document\":{\"id\":\"42\"}}]}"}}`
		case strings.Contains(req.Query, "BooksForLookup"):
			data = `{"books":[
				{"id":42,"slug":"the-last-hero","title":"The Last Hero","description":"Rincewind returns.\n\nAgain.",
				 "release_date":"2001-10-01","cached_image":{"url":"https://evil.example/x.jpg"},
				 "cached_tags":{"Genre":[{"tag":"Fantasy"},{"tag":"Humor"},{"tag":"Fantasy"}],"Mood":[{"tag":"funny"}]},
				 "contributions":[{"author":{"name":"Terry Pratchett"}},{"author":{"name":"Paul Kidby"}}],
				 "book_series":[{"position":27,"series":{"name":"Discworld"}}]},
				{"id":43,"slug":"bad slug<>","title":"The Last Hero: A Life of Henry Aaron","cached_image":{"url":"https://assets.hardcover.app/book/43.svg"},
				 "contributions":[{"author":{"name":"Howard Bryant"}}],"book_series":[]}]}`
		default:
			t.Errorf("unexpected query %q", req.Query)
		}
		_, _ = io.WriteString(w, `{"data":`+data+`}`)
	})
	mux.HandleFunc("GET /edition/9/cover.jpg", func(w http.ResponseWriter, r *http.Request) {
		coverAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = io.WriteString(w, "JPEG")
	})
	mux.HandleFunc("GET /book/43.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = io.WriteString(w, "<svg/>")
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	c := New(Config{URL: "https://api.hardcover.app/v1/graphql", Token: "secret-token", UserAgent: "test",
		HTTPClient: &http.Client{Transport: tr, Timeout: 5 * time.Second}})
	return c, &posts, &coverAuth
}

func TestLookupBooksByISBN(t *testing.T) {
	c, posts, coverAuth := lookupServer(t)
	ctx := context.Background()
	books, err := c.LookupBooks(ctx, BookQuery{ISBN: "9780552152938"})
	if err != nil || len(books) != 1 {
		t.Fatalf("books %+v, err %v", books, err)
	}
	b := books[0]
	if b.Title != "The Last Hero" || b.Subtitle != "A Discworld Fable" || b.Publisher != "Corgi" || b.Released != "2002-10-01" ||
		b.ISBN13 != "9780552152938" || b.Pages != 176 || len(b.Languages) != 1 || b.Languages[0] != "eng" {
		t.Errorf("edition values: %+v", b)
	}
	if strings.Join(b.Authors, ",") != "Terry Pratchett,Paul Kidby" || b.Series != "Discworld" || b.Position == nil || *b.Position != 27 {
		t.Errorf("book values: %+v", b)
	}
	if strings.Join(b.Genres, ",") != "Fantasy,Humor" || b.Slug != "the-last-hero" {
		t.Errorf("genres %v, slug %q", b.Genres, b.Slug)
	}
	// The edition's cover, on hardcover.app, replaced the book's off-site one.
	if !b.HasCover() {
		t.Fatal("no cover")
	}
	img, ctype, err := c.FetchCover(ctx, 42)
	if err != nil || string(img) != "JPEG" || ctype != "image/jpeg" {
		t.Fatalf("cover %q %q %v", img, ctype, err)
	}
	if a := coverAuth.Load().(string); a != "" {
		t.Errorf("the cover request carried Authorization %q", a)
	}
	// Cached, "no match" too.
	n := posts.Load()
	if _, err := c.LookupBooks(ctx, BookQuery{ISBN: "9780552152938"}); err != nil || posts.Load() != n {
		t.Errorf("not cached: %d requests, err %v", posts.Load()-n, err)
	}
	if got, err := c.LookupBooks(ctx, BookQuery{ISBN: "9780000000002"}); err != nil || len(got) != 0 {
		t.Errorf("unknown ISBN: %v %v", got, err)
	}
	n = posts.Load()
	_, _ = c.LookupBooks(ctx, BookQuery{ISBN: "9780000000002"})
	if posts.Load() != n {
		t.Error("no match wasn't cached")
	}
}

func TestLookupBooksBySearch(t *testing.T) {
	c, _, _ := lookupServer(t)
	ctx := context.Background()
	books, err := c.LookupBooks(ctx, BookQuery{Title: " The Last  Hero", Author: "Terry Pratchett"})
	if err != nil || len(books) != 2 {
		t.Fatalf("books %+v, err %v", books, err)
	}
	// The search's order, not the books query's.
	if books[0].ID != 43 || books[1].ID != 42 {
		t.Errorf("order %d, %d", books[0].ID, books[1].ID)
	}
	if books[0].Slug != "" {
		t.Errorf("bad slug kept: %q", books[0].Slug)
	}
	// Off-site cover: not offered, and FetchCover won't go there.
	if books[1].HasCover() {
		t.Error("offered a cover from evil.example")
	}
	if _, _, err := c.FetchCover(ctx, 42); !errors.Is(err, ErrNoCover) {
		t.Errorf("off-site cover: %v", err)
	}
	// An SVG is refused: served from our origin, it could run script.
	if _, _, err := c.FetchCover(ctx, 43); !errors.Is(err, ErrNoCover) {
		t.Errorf("SVG cover: %v", err)
	}
	// A book no lookup named.
	if _, _, err := c.FetchCover(ctx, 7); !errors.Is(err, ErrNoCover) {
		t.Errorf("unknown book: %v", err)
	}
}

func TestISBN10Of(t *testing.T) {
	for in, want := range map[string]string{"9780552152938": "0552152935", "9780306406157": "0306406152", "9791234567896": "", "978030640615": "",
		"9780804429573": "080442957X"} {
		if got := isbn10Of(in); got != want {
			t.Errorf("isbn10Of(%s) = %q, want %q", in, got, want)
		}
	}
}
