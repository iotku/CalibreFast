# Syncing starred authors with Hardcover follows

Phase 0 of syncing CalibreFast's favorite authors and series
(`aggregate_favorites.go`) with Hardcover's **Follow** feature: what
Hardcover's API offers. Research only; nothing here is built yet.

Sources are the files of Hardcover's API documentation repository,
[hardcoverapp/hardcover-docs](https://github.com/hardcoverapp/hardcover-docs),
read on 2026-11-24: `schema.graphql`, `capability-scopes.json`,
`field-descriptions.json` and `row-limits.json`. docs.hardcover.app and
api.hardcover.app couldn't be reached from where this was written, so
**nothing below has been tried against the live API** (see "Not yet
confirmed").

## What exists

**Follows are one polymorphic table**, `follows`, for authors, series and
other things alike (`schema.graphql`, `type follows`):

| column | type |
|---|---|
| `id` | `bigint!` |
| `user_id` | `bigint!` |
| `followable_type` | `String!` |
| `followable_id` | `bigint!` |
| `created_at`, `updated_at` | `timestamp!` |

with object relationships `author: authors`, `series: series`, `book`,
`edition`, `list`, `publisher`, `character` and `user: users!`: whichever
one `followable_type` names is set. It can be filtered by `user_id`,
`followable_type` and `followable_id` (`follows_bool_exp`), and a user has a
`follows` array relationship ("All follow relationships",
`field-descriptions.json`). At most **1000** rows per query
(`row-limits.json`), so a long list pages with `limit`/`offset`.

People are followed through a separate table, `followed_users`, which this
doesn't need.

**`followable_type` values.** Not listed anywhere, but each followable
type documents its `object_type` as a constant (`field-descriptions.json`):
`authors` → `'Author'`, `series` → `'Series'`, `publishers` →
`'Publisher'`. These are the class names of a Rails-style polymorphic
association, which is what `followable_type`/`followable_id` is, so
following an author is almost certainly `followable_type: "Author"` and a
series `"Series"`. **Confirm before relying on it.**

**Mutations** (`mutation_root`):

```graphql
insert_follow(followable_id: Int!, followable_type: String!): FollowType
delete_follow(followable_id: Int!, followable_type: String!): FollowDeleteType

type FollowType       { error: String, follow: follows, followable_id: Int, followable_type: String, id: Int }
type FollowDeleteType { error: String, success: Boolean! }
```

Note `delete_follow` takes the followed object, not the follow's `id`.
There are no author- or series-specific follow mutations; `insert_author`
and `update_author` are catalog edits.

**Scopes** (`capability-scopes.json`):

| operation | scope |
|---|---|
| read `follows`, `follows_aggregate`, `follows_by_pk` | `read:social` |
| `insert_follow`, `delete_follow` | `write:social` ("Follow users, like content, block users") |
| read `authors` | `read:catalog` (already ours) |

Reading sync asks for `read:me read:library write:library read:catalog`
(`SyncScopes`, `pkg/hardcover/oauth.go`), so **follows need two more scopes,
and every already-linked account would have to link again** to grant them.

## Matching to the library

A follow gives a Hardcover author or series **id** (and, through the
relationship, its `name` and `slug`). CalibreFast already keeps Hardcover
ids by name:

- authors: `hardcover_authors (name → hc_id, hc_slug, hc_name)`, filled when
  an author's profile is looked up (`hardcover_authors.go`);
- series: `hardcover_series (name → hc_id, hc_slug, hc_name)`, set when a
  series is linked to Hardcover (`hcSaveLink`, `hardcover.go`).

So for import, a followed id that's in those tables is an **exact match**
(the library's author or series with that `hc_id`); otherwise the follow's
name is matched as planned (exact name: imported; anything looser: offered
for confirmation, with "not a match" kept in
`hardcover_aggregate_matches`). For push, a starred author or series needs
its `hc_id`: from those tables, else looked up (the existing author lookup
does this already).

## Fit with the plan (`aggregate_favorites`)

- **Import** (follows → stars): `follows(where: {user_id: {_eq: me.id},
  followable_type: {_in: ["Author", "Series"]}})`, paged by 1000, then
  matched as above. Imported stars get `origin = 'hardcover'`; removing one
  in CalibreFast tombstones it (`removed_at`) so the next import doesn't
  bring it back. **Read-only on Hardcover.**
- **Push** (stars → follows, add-only): `insert_follow` for a starred author
  or series with an `hc_id` that isn't followed yet; store the returned
  follow `id` in `hc_ref`. Already followed (it's in the import's list):
  leave `hc_ref` empty, as it isn't ours.
- **Unstar**: `delete_follow` only when `hc_ref` is set, i.e. CalibreFast
  created that follow. Because `delete_follow` names the author, not the
  follow, check first (a `follows_by_pk(id: hc_ref)` read) that our follow
  still exists, so a follow the user removed and re-added on Hardcover
  themselves is left alone.
- **Consent**: like reading sync, off until the user turns it on for their
  linked account (a `share_favorites`-style setting), and only offered once
  the link carries `read:social` / `write:social`.

**Go**, on both import and push, pending the two checks below.

## Not yet confirmed

1. That `followable_type` is `"Author"` / `"Series"`: one read of a test
   account's `follows` after following an author and a series on the site.
2. That Hardcover lets a third-party OAuth app request `read:social` and
   `write:social` (its discovery document's `scopes_supported`,
   https://api.hardcover.app/.well-known/oauth-authorization-server), and
   whether personal API tokens carry them.
3. Rate limits for these calls: not in the documentation files read; the
   client's existing per-token `Budget` applies either way.

Each needs the live API: api.hardcover.app reachable, and a token for (1).
