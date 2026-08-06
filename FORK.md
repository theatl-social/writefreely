# theATL.social fork of WriteFreely

Based on upstream [WriteFreely](https://github.com/writefreely/writefreely) **v0.17.1**.
Licensed under the AGPL-3.0, same as upstream. Source is published here to satisfy
AGPL §13, since we run a modified build as a public network service at
<https://write.theatl.social>.

## What diverges from upstream

**Per-user blog limits.** Upstream's `max_blogs` is a single global config value, and
`newCollection` never reads it — it only hides a button in the web UI. We enforce a
per-user limit so membership tiers map to real blog allowances.

| Change | Location |
|---|---|
| `max_blogs` column on `users`, added idempotently at boot | `maxblogs.go`, called from `app.go` `ConnectToDatabase` |
| Limit enforced when a blog is created | `maxblogs.go` `checkBlogLimit`, called from `collections.go` `newCollection` |
| Limit also enforced on the claim-posts path | `maxblogs.go` `countRequestedNewBlogs` + `checkBlogLimitN`, called from `posts.go` `addPost` |
| `POST /api/internal/user/{username}/max-blogs` to set a user's limit | `maxblogs_api.go`, registered in `routes.go` |
| "New blog" UI affordance gated on the user's own allowance, not the instance-wide fallback | `account.go` `viewCollections`, via `checkBlogLimit` |

Everything else is upstream. The internal endpoint requires the
`WRITEFREELY_API_SECRET` environment variable (32+ characters) in an
`X-WriteFreely-Secret` header, and is additionally blocked at our reverse proxy.

## Why the schema change avoids the migration system

`migrations/migrations.go` registers migrations in a flat ordered slice where
`CurrentVer()` is `len(migrations)`. Appending ours would collide with the next
upstream migration and cause a recorded version to re-run our `ALTER`. Instead the
column is added by an idempotent guard at startup, keeping `migrations.go` out of
the merge surface entirely.

## Merge policy

Only `app.go`, `account.go`, `collections.go`, `routes.go`, and `posts.go` are
modified, by one line or a short block each, all marked with `theATL fork:`
comments. Merge upstream releases onto `theatl-main`; conflicts should be
confined to those five files.

Three extra steps, each earned by something that already bit us or nearly did:

- **Before pushing an upstream tag to origin, disable Actions.** Pushing
  `v0.17.0`/`v0.17.1` ran the workflow embedded in **those old commits** —
  upstream's multi-arch `:latest` publisher — not ours. Two runs started and had
  to be cancelled mid-build.
- **Re-enumerate `CreateCollection` call sites on every upstream merge.** There
  are three today: `collections.go` (gated), `database.go:1756` via `addPost`
  (gated at the handler), and `CreateCollectionFromToken` (`database.go:290`,
  zero callers). If upstream wires the third to a route, the cap silently gains
  a hole and `database.go` is off our budget.
- **Bump the AGPL §5(a) notice count** — it is five files now.

## Known upstream test failures

CI skips exactly these two **subtests** by their fully slash-qualified path
(see `.github/workflows/ci.yml`'s `Test` step — the `/subtest` qualifier matters:
without it, `-skip` matches the parent test name and drops every subtest beneath
it, e.g. all five of `TestUpdatesRoundTrip`'s subtests instead of just the one
that's broken). Both are pre-existing at v0.17.1, in files outside the
merge-surface budget above, and unrelated to anything in this fork:

- `TestViewOauthCallback/success` (`oauth_test.go`) — the test's mock config never
  sets `App.OpenRegistration`, so `oauth.go`'s registration-blocked branch fires
  and returns a redirect the test doesn't expect. (Only subtest on this test
  today; qualified anyway so it stays correct if upstream adds more.)
- `TestUpdatesRoundTrip/Release_URL` (`updates_test.go`) — a race: the cache's
  version-check network call runs in an unsynchronized goroutine, and the
  `Release_URL` subtest reads the result before it's populated. The other four
  subtests (`New_Updates_Cache`, `Check_Now`, `Are_Available`, `Latest_Version`)
  are unaffected and run normally.

Revisit both on every upstream merge and delete the corresponding `-skip` entry
the moment a release fixes the underlying subtest.

## CI divergence

`.github/workflows/ci.yml` is ours; upstream has no Go CI at all.

`.github/workflows/docker-publish.yml` **replaces** upstream's wholesale. Upstream's
built multi-arch (amd64 + arm64, via QEMU and buildx) and published `:latest` on
`main`/`develop`. Ours publishes a single immutable `<version>-<n>-g<sha>` tag
derived from `git describe`, amd64 only, on pushes to `theatl-main`.

Two reasons: `:latest` is banned across this fleet because it makes a running
container unreconstructable, and the only consumer is one x86_64 OVH host, so the
arm64 half of the matrix was build time spent on an artifact nobody pulls.

If a future upstream merge conflicts on this file, take ours — but re-check
whether arm64 has become necessary before assuming that still holds.

## Known limits

**Check-then-act race on the blog cap.** `checkBlogLimit`/`checkBlogLimitN`
count a user's existing blogs, then the caller (`newCollection` or
`ClaimPosts`) inserts — with no transaction spanning the two. N concurrent
requests from the same user can each read the same pre-insert count and each
pass, so the cap can be overshot by a small number of blogs under concurrent
requests.

Accepted as-is: members are paying, real-identity accounts, the requests are
auditable after the fact, and the ceiling on the internal endpoint's `max_blogs`
value (1000) bounds how bad a single racing burst could be. The mitigation is
detective, not preventive: the member site's nightly audit should compare each
user's *actual* blog count against their allowance, not only push allowances
one-way — so an overshoot gets caught and reconciled rather than silently
persisting.
