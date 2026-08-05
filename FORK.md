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
| `POST /api/internal/user/{username}/max-blogs` to set a user's limit | `maxblogs.go`, registered in `routes.go` |

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

Only `app.go`, `collections.go`, and `routes.go` are modified, by one line or a short
block each, all marked with `theATL fork:` comments. Merge upstream releases onto
`theatl-main`; conflicts should be confined to those three files.

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
