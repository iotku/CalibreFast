package hardcover

// Follows: the authors a user follows on Hardcover, for importing them as
// starred authors. Hardcover keeps every follow in one table, follows,
// with followable_type naming what's followed ("Author" for an author) and
// the object filled in through its relationship. Reading it needs the
// read:social scope (FollowScopes).

import (
	"context"
	"strings"
)

// FollowScopes are the scopes following authors needs: reading follows,
// and (for sending stars there) following and unfollowing.
const FollowScopes = "read:social write:social"

// HasScope reports whether a token's space-separated scope list grants
// want (or "all", which grants every scope).
func HasScope(scope, want string) bool {
	for s := range strings.FieldsSeq(scope) {
		if s == want || s == "all" {
			return true
		}
	}
	return false
}

// followsPage is Hardcover's most rows per query for follows.
const followsPage = 1000

var qFollowedAuthors = query{`query FollowedAuthors($user: bigint!, $offset: Int!) {
  follows(where: {user_id: {_eq: $user}, followable_type: {_eq: "Author"}}, order_by: {id: asc}, limit: 1000, offset: $offset) {
    id
    author { id name slug }
  }
}`, 1}

// FollowedAuthor is an author a user follows.
type FollowedAuthor struct {
	FollowID int64 // the follow's own id
	AuthorID int64
	Name     string
	Slug     string
}

// FollowedAuthors lists the authors user follows, oldest follow first: one
// request per thousand. Follows whose author is gone are left out.
func (c *Client) FollowedAuthors(ctx context.Context, user int64) ([]FollowedAuthor, error) {
	var out []FollowedAuthor
	for offset := 0; ; offset += followsPage {
		var data struct {
			Follows []struct {
				ID     flexInt `json:"id"`
				Author *struct {
					ID   flexInt `json:"id"`
					Name string  `json:"name"`
					Slug string  `json:"slug"`
				} `json:"author"`
			} `json:"follows"`
		}
		if err := c.post(ctx, qFollowedAuthors, map[string]any{"user": user, "offset": offset}, &data); err != nil {
			return nil, err
		}
		for _, f := range data.Follows {
			if f.Author == nil || f.Author.ID <= 0 || strings.TrimSpace(f.Author.Name) == "" {
				continue
			}
			out = append(out, FollowedAuthor{FollowID: int64(f.ID), AuthorID: int64(f.Author.ID), Name: clip(f.Author.Name), Slug: clip(f.Author.Slug)})
		}
		if len(data.Follows) < followsPage {
			return out, nil
		}
	}
}
