package hardcover

// Follows: the authors a user follows on Hardcover, for importing them as
// starred authors. Hardcover keeps every follow in one table, follows,
// with followable_type naming what's followed ("Author" for an author) and
// the object filled in through its relationship. Reading it needs the
// read:social scope (FollowScopes).

import (
	"context"
	"errors"
	"fmt"
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

var (
	qIsFollowing = query{`query IsFollowing($user: bigint!, $author: bigint!) {
  follows(where: {user_id: {_eq: $user}, followable_type: {_eq: "Author"}, followable_id: {_eq: $author}}, limit: 1) { id }
}`, 1}

	qFollowAuthor = query{`mutation FollowAuthor($author: Int!) {
  insert_follow(followable_id: $author, followable_type: "Author") { id error }
}`, 1}

	qUnfollowAuthor = query{`mutation UnfollowAuthor($author: Int!) {
  delete_follow(followable_id: $author, followable_type: "Author") { success error }
}`, 1}
)

func init() {
	for name, q := range map[string]query{
		"FollowedAuthors": qFollowedAuthors, "IsFollowing": qIsFollowing,
		"FollowAuthor": qFollowAuthor, "UnfollowAuthor": qUnfollowAuthor,
	} {
		allQueries[name] = q
	}
}

// IsFollowing answers user's follow of author's id, or 0 when they don't
// follow them.
func (c *Client) IsFollowing(ctx context.Context, user, author int64) (int64, error) {
	var data struct {
		Follows []struct {
			ID flexInt `json:"id"`
		} `json:"follows"`
	}
	if err := c.post(ctx, qIsFollowing, map[string]any{"user": user, "author": author}, &data); err != nil {
		return 0, err
	}
	if len(data.Follows) == 0 {
		return 0, nil
	}
	return int64(data.Follows[0].ID), nil
}

// FollowAuthor follows the author with this id as the token's user, and
// answers the follow's id.
func (c *Client) FollowAuthor(ctx context.Context, author int64) (int64, error) {
	var data struct {
		R mutationResult `json:"insert_follow"`
	}
	if err := c.post(ctx, qFollowAuthor, map[string]any{"author": author}, &data); err != nil {
		return 0, err
	}
	return data.R.check("follow the author")
}

// UnfollowAuthor stops the token's user following the author with this id.
func (c *Client) UnfollowAuthor(ctx context.Context, author int64) error {
	var data struct {
		R struct {
			Success bool    `json:"success"`
			Error   *string `json:"error"`
		} `json:"delete_follow"`
	}
	if err := c.post(ctx, qUnfollowAuthor, map[string]any{"author": author}, &data); err != nil {
		return err
	}
	if data.R.Error != nil && *data.R.Error != "" {
		return fmt.Errorf("Hardcover couldn't unfollow the author: %s", clip(*data.R.Error))
	}
	if !data.R.Success {
		return errors.New("Hardcover didn't unfollow the author")
	}
	return nil
}
