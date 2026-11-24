package hardcover

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// followServer answers the follow calls with answer (by operation name)
// and records each request's variables.
func followServer(t *testing.T, answer map[string]string) (*Client, *[]map[string]any) {
	t.Helper()
	var seen []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		op, _, _ := strings.Cut(strings.Fields(req.Query)[1], "(")
		seen = append(seen, req.Variables)
		_, _ = io.WriteString(w, `{"data":`+answer[op]+`}`)
	}))
	t.Cleanup(srv.Close)
	return New(Config{URL: srv.URL, Token: "t", UserAgent: "test"}), &seen
}

func TestFollowCalls(t *testing.T) {
	ctx := context.Background()
	c, seen := followServer(t, map[string]string{
		"FollowedAuthors": `{"follows":[{"id":5,"author":{"id":9,"name":"An Author","slug":"an-author"}},{"id":6,"author":null}]}`,
		"IsFollowing":     `{"follows":[{"id":5}]}`,
		"FollowAuthor":    `{"insert_follow":{"id":12,"error":null}}`,
		"UnfollowAuthor":  `{"delete_follow":{"success":true,"error":null}}`,
	})
	if got, err := c.FollowedAuthors(ctx, 77); err != nil || len(got) != 1 || got[0] != (FollowedAuthor{FollowID: 5, AuthorID: 9, Name: "An Author", Slug: "an-author"}) {
		t.Fatalf("FollowedAuthors: %+v %v", got, err)
	}
	if id, err := c.IsFollowing(ctx, 77, 9); err != nil || id != 5 {
		t.Fatalf("IsFollowing: %d %v", id, err)
	}
	if id, err := c.FollowAuthor(ctx, 9); err != nil || id != 12 {
		t.Fatalf("FollowAuthor: %d %v", id, err)
	}
	if err := c.UnfollowAuthor(ctx, 9); err != nil {
		t.Fatalf("UnfollowAuthor: %v", err)
	}
	if v := (*seen)[1]; v["user"] != float64(77) || v["author"] != float64(9) {
		t.Fatalf("IsFollowing's variables: %v", v)
	}

	// Hardcover saying no.
	c, _ = followServer(t, map[string]string{
		"IsFollowing":    `{"follows":[]}`,
		"FollowAuthor":   `{"insert_follow":{"id":null,"error":"Already following"}}`,
		"UnfollowAuthor": `{"delete_follow":{"success":false,"error":null}}`,
	})
	if id, err := c.IsFollowing(ctx, 77, 9); err != nil || id != 0 {
		t.Fatalf("not following: %d %v", id, err)
	}
	if _, err := c.FollowAuthor(ctx, 9); err == nil || !strings.Contains(err.Error(), "Already following") {
		t.Fatalf("follow refused: %v", err)
	}
	if err := c.UnfollowAuthor(ctx, 9); err == nil {
		t.Fatal("unfollow without success")
	}
	if !HasScope("read:me all", "write:social") || HasScope("read:social", "write:social") {
		t.Fatal("HasScope")
	}
}
