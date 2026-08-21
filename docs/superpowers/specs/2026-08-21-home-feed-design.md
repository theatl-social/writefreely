# Home feed for write.theatl.social — design

**Date:** 2026-08-21
**Repo:** theatl-writefreely (fork of WriteFreely v0.17.1)
**Branch target:** `theatl-main`

## Status and scope

This is **sub-project A of three**. The original request covered three
independent subsystems; they were decomposed so each gets its own spec, plan,
and merge-surface budget:

| | Sub-project | State |
|---|---|---|
| **A** | Home feed — latest blogs and posts for anon and logged-in visitors | **this document** |
| B | Media upload and storage, inline HTML + ActivityPub attachments | not started |
| C | Comments mapped to ActivityPub replies | not started |

B and C are explicitly out of scope here. See "Out of scope" at the end for the
findings from A's exploration that they depend on, recorded so they are not
rediscovered from scratch.

## Problem

`handleViewHome` (`app.go:256`) sends anonymous visitors to the marketing
landing page and logged-in members straight into the editor (the Pad). Neither
audience is ever shown what is being written on the instance.

Upstream's Reader (`/read`, `read.go`) already aggregates recent posts across
every public blog and is enabled on this instance (`local_timeline = true` in
`config.ini`). It is reachable only from a nav link, it lists posts and never
blogs, and the home route never reaches it because that path is gated on
`chorus`, which is false here.

There is no page anywhere on the instance that lists the blogs it hosts.

## Goal

`https://write.theatl.social/` renders a discovery digest — recent posts in the
main column, recently-active blogs in a secondary strip — identically for
anonymous and logged-in visitors. A full blog directory lives behind it at
`/blogs`. The editor remains one click away for members.

## Decisions

Each of these was decided during brainstorming; the rationale is recorded
because the reasoning, not the conclusion, is what a future upstream merge or
follow-up change needs.

### 1. Eligibility: public blogs only, and flip the default for new blogs

The feed filters on `c.privacy = 1` (`CollPublic`) and `u.status = 0`
(`UserActive`, `users.go:25`) — the same filter both existing discovery queries
already use (`FetchPublicPosts` in `read.go`, `GetPublicCollections` in
`database.go:1989`).

The complication: `default_visibility` is **unset** in `config.ini`, so
`defaultVisibility()` (`collections.go:174`) falls through to `CollUnlisted`.
Every blog created on this instance to date is unlisted, and unlisted blogs are
excluded by that filter. A feed shipped against it will therefore be sparse or
empty at launch — correctly, not because of a bug.

`default_visibility = public` is set in `config.ini` (and
`config.ini.example`) so *newly created* blogs land in the directory. Nothing
existing is rewritten. Existing unlisted blogs stay out until their owner opts
in via blog settings.

Rejected: including unlisted blogs in the feed. "Unlisted" in WriteFreely does
not mean private — unlisted blogs have public URLs and federate — but the one
thing it withholds is directory listing, which is precisely what this feature
is. Retroactively listing blogs whose owners left the default alone would
override a choice the UI presented to them as meaningful.

### 2. Blog ordering: most recently posted to

`collections` has no `created` column (`schema.sql:103`), so "newest blog" is
not a query that can be written today. It could be added via the fork's
established idempotent boot-time `ALTER` pattern (`maxblogs.go`), but ordering
by most-recent-post is both free and more useful: it surfaces active blogs
rather than abandoned empty ones.

Blogs with zero posts do not appear at all (enforced by `INNER JOIN posts`).

### 3. Logged-in visitors get the feed, not the editor

`/` means one thing for everyone. The Pad stays reachable at `/new`, which is
already routed (`routes.go:220`), surfaced by a "Write" affordance in the nav.

### 4. Home is a digest; the full lists live elsewhere

Home shows the 10 most recent posts and the 8 most recently active blogs
(see the constants table under "Caching"). "More posts" links to the existing
paginated `/read`. "All blogs" links to a new paginated `/blogs`. The posts
side therefore costs nothing — `/read` is already built, routed, and cached.

### 5. Layout: posts primary, blogs secondary

Recent posts take the main column using `read.tmpl`'s existing card markup.
Active blogs sit in a narrower strip. Readers arrive wanting something to read;
blogs are the follow-up action.

### 6. Directory URL is `/blogs`, and `blog`/`blogs` become reserved aliases

`blogs` is not in `author/author.go`'s `reservedUsernames` map, so a member can
claim it as a blog alias. In practice the two would coexist (`/blogs` vs
`/blogs/`, since `StrictSlash` is not set on the router) but that is a
collision waiting to surprise someone.

`blog` and `blogs` are added to `reservedUsernames`. This makes
`author/author.go` a **seventh** modified upstream file, against a fork policy
that has held the line at six. Accepted deliberately: it is a two-line addition
to a static map that upstream changes rarely, so the conflict surface is
close to zero, and the alternative (`/browse`, which is already reserved) trades
a permanently worse URL for a merge risk that does not really exist.
`FORK.md`'s merge-policy section and its AGPL §5(a) notice count are updated to
seven.

### 7. Rejected approaches

**`chorus = true`.** Makes `/` render the Reader with one config line and no Go
code. Also swaps every member's blog to `chorus-collection.tmpl`, rewrites
hashtag links to instance-wide `/read/t/` (`postrender.go:167`), and changes
`SignupPath` (`config/config.go:263`). Changes every blog on the instance in
order to change one page.

**Adding the blogs list to `read.tmpl` and making `/` render the Reader.**
Fewest new files, and home and `/read` stay literally identical. Requires
editing `read.go` — an upstream file not currently on the merge budget, and
exactly the file upstream touches whenever it revisits the Reader.

## Architecture

### Components

| Unit | Purpose | Depends on |
|---|---|---|
| `home.go` (new, fork-owned) | `viewHome`, `viewBlogsDirectory`, the active-blogs query, and its cache | `app.timeline`, `app.db`, `memo` |
| `templates/home.tmpl` (new) | Digest page | `base.tmpl` |
| `templates/blogs.tmpl` (new) | Paginated directory | `base.tmpl` |
| `app.go` (modified) | Route home traffic to `viewHome`; hold and initialize the blogs cache | `home.go` |
| `routes.go` (modified) | Register `/blogs` routes | `home.go` |
| `base.tmpl` (modified) | Nav: Home link, Write affordance | — |
| `author/author.go` (modified) | Reserve `blog` and `blogs` | — |

### Routes

Registered in the "Handle special pages first" block of `routes.go`
(around line 206–213), which is **required**: `routes.go:230-232` is a
catch-all mapping `/{collection}` to blog rendering, and anything registered
after it loses.

```
GET /              → handleViewHome (existing, modified) → viewHome
GET /blogs         → viewBlogsDirectory
GET /blogs/p/{page:[0-9]+} → viewBlogsDirectory
```

`/` itself stays registered where it is (`routes.go:236`) — only the handler's
internal branching changes.

### `handleViewHome` change

One short block in the multi-user branch of `handleViewHome` (`app.go:256`),
placed immediately after the existing `Chorus` branch and **before** the
`if u != nil { return handleViewPad(...) }` branch that currently sends members
to the editor:

```go
// theATL fork: show the discovery feed at / for everyone, rather than the
// landing page (anon) or the editor (logged in).
if !app.cfg.App.Private || u != nil {
    return viewHome(app, w, r)
}
```

Mirroring the `Chorus` branch's own guard shape keeps the private-instance
behaviour correct without restating it. Everything else in the handler is
untouched, so all of these keep working exactly as today:

- `SingleUser` → blog index (first branch, never reached here)
- `?landing=1` → landing page
- `Private` + anonymous → login page
- `LandingPath()` redirect when `landing` is configured
- `/signup` → landing page (separate route, `routes.go:209`)

### Data layer

**Posts — no new query.** `viewHome` calls
`updateTimelineCache(app.timeline, false)` and slices the first N entries out
of `*app.timeline.posts`. This is the same `memo.Memo` that serves `/read`,
initialized at `app.go:473` under the `local_timeline` guard this instance
already satisfies.

Consequences, all desirable: home and `/read` agree by construction; they share
one cache refresh rather than two; and home inherits `tlMaxAuthorPosts` (5
posts per author) so one prolific member cannot fill the page.

**Blogs — one new query**, behind its own `memo.Memo` with the same
`tlCacheDur` (10 minutes):

```sql
SELECT c.id, c.alias, c.title, c.description, c.view_count,
       MAX(p.created) AS last_post, COUNT(p.id) AS post_count
  FROM collections c
 INNER JOIN users u ON u.id = c.owner_id
 INNER JOIN posts p ON p.collection_id = c.id
 WHERE c.privacy = 1
   AND u.status = 0
   AND p.created <= /* db.now() */
 GROUP BY c.id
 ORDER BY last_post DESC
```

Notes on each clause:

- `INNER JOIN posts` (not `LEFT JOIN`) is what excludes empty blogs.
- `p.created <= now()` excludes scheduled future posts, matching
  `FetchPublicPosts`.
- `u.status = 0` drops silenced users' blogs automatically.
- `db.now()` is used rather than a literal so the SQLite build keeps working.
- Only `privacy = 1` rows are selected, so the result contains nothing that is
  not already public.

### Caching

A small fork-owned struct in `home.go`, held on `App` next to `timeline`,
plus the row type its query scans into:

```go
type homeFeed struct {
    m     *memo.Memo
    blogs *[]HomeBlog
}

// HomeBlog is one row of the active-blogs query. It is deliberately not a
// Collection: the feed needs aggregates (LastPost, PostCount) that Collection
// has no field for, and needs none of Collection's style/script/format
// columns.
type HomeBlog struct {
    ID          int64
    Alias       string
    Title       string
    Description string
    Views       int64
    LastPost    time.Time
    PostCount   int64

    hostName string // set from cfg.App.Host after scan, for URL building
}
```

`HomeBlog` gets a `CanonicalURL()` and a `DisplayTitle()` mirroring
`Collection`'s, so the templates read the same way as the rest of the codebase.

**Display counts** are named constants in `home.go`, not literals scattered
through handlers and templates:

| Constant | Value | Meaning |
|---|---|---|
| `homePostLimit` | 10 | post cards in the home main column |
| `homeBlogLimit` | 8 | blogs in the home secondary strip |
| `blogsPerPage` | 24 | blogs per page in the `/blogs` directory |

These are starting values chosen to fill a screen without paginating the home
page; they are cheap to tune because nothing else depends on them.

`App` gains one field (`homeFeed *homeFeed`) and `app.go` gains an
initialization call alongside `initLocalTimeline` at `app.go:473`, under the
same `LocalTimeline` guard — the feed depends on `app.timeline` being non-nil,
so the two must share a lifetime.

Refresh follows `updateTimelineCache`'s shape exactly: populate on first use,
re-fetch when `m.Invalidate()` reports the TTL elapsed, log and keep serving
stale data on query error.

### Templates

Both are new, fork-owned, and **self-contained**. `InitTemplates`
(`templates.go:127`) auto-discovers every `.tmpl` directly under `templates/`,
so no Go registration change is needed; each automatically gets `base.tmpl`,
`include/footer.tmpl`, and `user/include/silenced.tmpl` parsed alongside it.

They deliberately do **not** use `include/posts.tmpl`. That partial is only
parsed for a hardcoded list of template names (`templates.go:73`) and is
coupled to a single-blog context (`$.Alias`, `$.IsOwner`, `$.Collections`,
`$.Format`). `read.tmpl` renders its own cards inline for the same reason.

- **`home.tmpl`** — main column: `homePostLimit` post cards reusing `read.tmpl`'s
  `.preview` / `.read-more` / `p.source` markup and its height-collapse script.
  Secondary strip: `homeBlogLimit` blogs showing title, description, last-post date, and
  post count. Main column footed by "More posts →" (`/read`); strip footed by
  "All blogs →" (`/blogs`).
- **`blogs.tmpl`** — full directory, `blogsPerPage` per page, same blog card
  shape. Paging reuses the prev/next markup from `read.tmpl` rather than
  `include/posts.tmpl`'s `"paging"` block, which is not parsed for these
  templates.

**Empty state is a first-class requirement, not a fallback.** Because existing
blogs remain unlisted until their owners act, both pages will be sparse at
launch. Each needs copy that explains how to appear — pointing at blog settings
→ visibility — rather than `read.tmpl`'s bare "No posts here yet!".

### Navigation

`base.tmpl` (already fork-modified, e.g. the join.theatl.social About link):

- Un-gate the "Home" nav link from `.Chorus` (currently line 42) so it shows
  whenever `.LocalTimeline` and `.CanViewReader` hold.
- Add a "Write" affordance for logged-in users pointing at `/new`, following
  the shape of the existing Chorus-only "New Post" button (line 57).
- Keep the existing "Reader" link; it is now a sibling of Home rather than the
  only way in.

### Configuration

`config.ini` and `config.ini.example` gain `default_visibility = public` in the
`[app]` section.

## Merge-surface accounting

| File | Status | Change |
|---|---|---|
| `app.go` | already on budget | `handleViewHome` block; one `App` field; one init call |
| `routes.go` | already on budget | 2 route registrations |
| `author/author.go` | **new to budget (7th)** | 2 entries in `reservedUsernames` |
| `base.tmpl` | already fork-modified | nav changes |
| `home.go` | new, fork-owned | handlers, query, cache |
| `home.tmpl`, `blogs.tmpl` | new, fork-owned | auto-registered |
| `home_test.go` | new, fork-owned | tests |
| `config.ini`, `config.ini.example` | fork-owned | one line each |
| `FORK.md` | fork-owned | document this feature; budget 6 → 7 |

Untouched and deliberately so: `read.go`, `collections.go`, `posts.go`,
`database.go`, `templates.go`, `migrations/`.

Per `FORK.md` convention, every upstream-file edit carries a `theATL fork:`
comment, and `home.go` / `home_test.go` carry their own full copyright header
rather than fork markers.

## Error handling

- **Cold or failed post cache.** `app.timeline.posts` is a `*[]PublicPost` that
  is nil until the first successful fetch, and `updateTimelineCache` logs and
  leaves it nil on query failure. `viewHome` must nil-check before slicing.
  This is the single most likely panic in the feature and is covered by a test.
- **Blog query failure.** Log and render with whatever the cache last held; if
  it never populated, render the empty state. A failed blog query must not take
  down the whole home page, since the posts half is independent.
- **`local_timeline = false`.** `app.timeline` is nil and `initLocalTimeline`
  never runs. `viewHome` returns the same 404 `viewLocalTimeline` does rather
  than dereferencing nil. `/` becoming a 404 in that configuration is
  acceptable because this instance sets it true and the fork owns the config.
- **Private instance.** Gated by the `!Private || u != nil` guard, matching
  `CanViewReader` (`app.go:426`).

## Testing

Following the fork's existing table-driven style (`maxblogs_test.go`,
`oauth_preauth_test.go`), in a new `home_test.go`:

**`viewHome` rendering**
- Cache unpopulated (`app.timeline.posts == nil`) — renders, does not panic
- Cache holds fewer posts than the display limit — renders all of them
- Cache holds more — renders exactly the limit
- No eligible blogs — renders the empty state, not a blank strip

**Active-blogs query**
- Silenced owner (`u.status != 0`) excluded
- Unlisted blog (`privacy != 1`) excluded
- Blog with no posts excluded
- Blog whose only post is scheduled in the future excluded
- Ordering is by most-recent post descending, not by title or id

**`handleViewHome` routing**
- Anonymous → feed
- Logged-in → feed, *not* the Pad (this is the behaviour change; assert it
  directly)
- `?landing=1` → landing page
- `Private = true` + anonymous → login page
- `SingleUser = true` → unchanged path

**Route precedence**
- `GET /blogs` reaches `viewBlogsDirectory`, not `handleViewCollection`

**Reserved aliases**
- `IsValidUsername` rejects `blog` and `blogs`

## Pre-flight checks before deploy

1. **Is an existing collection aliased `blog` or `blogs`?** Reserving the name
   does not retroactively rename anything already created; if a row exists it
   must be renamed (and its owner told) before `/blogs` is registered, or the
   route will shadow a real blog.

   *Partially cleared, 2026-08-21:* `/blogs` returns 404 in production. That
   rules out a post with that ID (the URL currently falls through to
   `routes.go:235`'s `/{post}` catch-all), but a *collection* aliased `blogs`
   would be served at `/blogs/` **with** a trailing slash
   (`routes.go:231`), so confirm that URL 404s too before registering the
   route.

2. Confirm `local_timeline` is still `true` in the deployed config.

**Not a gate:** the number of blogs currently eligible (`privacy = 1` with at
least one post) is expected to be small at launch, and that is accepted rather
than resolved first. Only public blogs are listed; the feed fills as owners opt
in. The empty-state copy carries this, which is why it is a first-class
requirement above rather than a fallback.

## Known limits

To be added to `FORK.md`'s "Known limits" section:

**The active-blogs query does a full scan and filesort.** `posts` has no index
on `created` (`schema.sql`), and the query groups by `collection_id` while
ordering by `MAX(p.created)`. Accepted as-is: it is identical in kind to
`FetchPublicPosts`, which already scans and filesorts the same table every 10
minutes, and it sits behind the same 10-minute memo. Revisit — with an index on
`posts(collection_id, created)` — if the post count grows by an order of
magnitude or the cache TTL is shortened.

**The feed is eventually consistent, by up to 10 minutes.** A newly published
post or a blog flipped to public will not appear on `/` until the memo expires.
This matches `/read`'s existing behaviour and is what makes the feature free of
per-request database load, but it will read as a bug to a member who publishes
and immediately checks the home page. Worth saying so in member-facing docs.

**Flipping `default_visibility` does not migrate existing blogs.** The gap
between "new blogs are public" and "the blogs already on the instance are
unlisted" closes only as owners opt in. There is no backfill and deliberately
so; see Decision 1.

## Out of scope

Sub-projects B and C are separate specs. Two findings from A's exploration are
recorded here so they are not rediscovered:

**For B (media).** WriteFreely has no upload code at all — the only
`ParseMultipartForm` in the repo is the Blogger/Medium importer
(`account_import.go:59`). But the rendering and federation halves already
exist: `getSanitizationPolicy` (`postrender.go:263`) already permits `img`,
`video`, `audio`, `source`, and `iframe` with their relevant attributes, and
`PublicPost.ActivityObject` (`posts.go:1290`) already converts image URLs found
in post bodies into ActivityPub `Attachment` objects via
`activitystreams.NewImageAttachment`. B is a storage and upload-endpoint
problem, not a rendering or federation one.

**For C (comments).** The inbox handler (`activitypub.go:317`) uses a
`streams.Resolver` whose `CreateCallback` is available but unwired, and
`remote_likes` (migration v16) is a direct precedent for a replies table
alongside `remoteusers` / `remoteuserkeys`.

**`github.com/writeas/httpsig` is used only to *sign outgoing* activities**
(`activitypub.go:764`, `activitypub.go:816`). Nothing verifies signatures on
*incoming* activities, and the remote public keys stored in `remoteuserkeys`
are never read back for verification. Today the blast radius is a junk follower
row from a forged `Follow`. The moment `Create`/`Note` is accepted as a
comment, an unverified inbox means anyone on the internet can post a comment
attributed to any Mastodon account and have it render on a member's blog under
that person's name and avatar. **Incoming HTTP signature verification is a
prerequisite for C, not an enhancement to it.**
