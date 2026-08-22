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
| "New blog" UI affordance gated on the user's own allowance, not the instance-wide fallback | `account.go` `viewCollections`, via `checkBlogLimit` |
| `SetUserMaxBlogs` — the datastore method that actually writes the column | `maxblogs_api.go` |

The setter used to have its own dedicated endpoint
(`POST /api/internal/user/{username}/max-blogs`). That endpoint was removed
entirely by the OAuth JIT provisioning work below — `SetUserMaxBlogs` is now
called from the newer, identity-keyed endpoint instead, because a member can
have an allowance pushed before their account (and therefore their username)
exists at all.

**OAuth just-in-time (JIT) account provisioning.** The member site
pre-authorizes a Mastodon identity for the OAuth path over the internal
network, in advance of any login. On that identity's first real OAuth
callback, this fork creates the local WriteFreely account automatically
instead of falling through to upstream's manual signup page — there is no
self-serve, in-app account creation on this OAuth path at all.

| Change | Location |
|---|---|
| JIT branch: on OAuth callback, if the identity isn't linked yet, check `oauth_preauth` for a pending grant and create + link the account automatically | `oauth.go` `viewOauthCallback` |
| `oauth_preauth` table (`remote_user_id`/`provider`/`client_id` → `max_blogs`), added idempotently at boot — same non-migration reasoning as `max_blogs` (see below) | `oauth_preauth.go` `ensureOauthPreauthTable`, called from `app.go` `ConnectToDatabase` |
| `POST /api/internal/mastodon-user/{remoteUserID}/max-blogs` — replaces the old username-keyed setter above. Pre-account, upserts or deletes the `oauth_preauth` row; post-account, updates `users.max_blogs` directly. `max_blogs: 0` is a distinct revoke signal, not a rejected value (see "Known limits") | `oauth_preauth.go` `handleSetMastodonUserMaxBlogs`, registered in `routes.go` |
| Mastodon username → WriteFreely username normalization (strips invalid characters, falls back through suffixed/remote-ID-keyed tiers on collision or reserved-word rejection) | `oauth_preauth.go` `normalizeOauthUsername` |
| `POST /oauth/signup` (upstream's manual account-linking page after OAuth) is deliberately NOT registered — its only gate, `HashTokenParams`, is an HMAC keyed on `Server.HashSeed`, which is empty (unset) in this fork's config and therefore trivially forgeable. The handler code still exists, unrouted, in `oauth_signup.go` | `oauth.go` `configureOauthRoutes` |
| `removeOauth` enforces `GenericOauth.AllowDisconnect` in code before disconnecting a "generic" (Mastodon) link — upstream only hid the button in the settings template, so a direct POST bypassed it regardless of config. Matters more here than on a stock instance: every account is OAuth-JIT-provisioned and therefore passwordless and emailless, so disconnecting is unrecoverable | `account.go` `removeOauth` |
| OAuth provider value is checked against an exact-match allowlist before any DB delete, not a normalized comparison — closes a bypass where MariaDB's `utf8mb4_uca1400_ai_ci` collation treats case/accent/full-width variants as equal to `"generic"` even when a Go-level normalization doesn't | `oauth_preauth.go` `isKnownOauthProvider`/`knownOauthProviders`, called from `account.go` `removeOauth` |
| Per-identity MariaDB advisory lock (`GET_LOCK`/`RELEASE_LOCK`, keyed on a SHA-256 hash of remote_user_id+provider+client_id) serializes the JIT login path against the internal endpoint's check-then-write. Closes a TOCTOU race where neither side has a row to lock via `SELECT ... FOR UPDATE`, because neither `oauth_preauth` nor `oauth_users` necessarily exists yet when either path starts | `oauth_preauth.go` `withOauthIdentityLock`, called from `oauth.go` `viewOauthCallback` and `oauth_preauth.go` `handleSetMastodonUserMaxBlogs` |
| Dedicated `app.oauthLockDB` connection pool, separate from the main pool, pins the advisory-lock connection — an earlier version pinned from the main pool and could deadlock the whole app under load (see "Known limits" for the pool's own tradeoff) | `app.go` `App.oauthLockDB` field, `connectToDatabase` |

Everything else is upstream. Both internal endpoints require the
`WRITEFREELY_API_SECRET` environment variable (32+ characters) in an
`X-WriteFreely-Secret` header, and are additionally blocked at our reverse proxy.

**Instance discovery feed.** Upstream shows a marketing landing page to
anonymous visitors at `/` and the editor to logged-in ones, and has no page
anywhere that lists the blogs an instance hosts. This fork serves a discovery
digest at `/` for everyone — recent posts beside recently-active blogs — with
a full blog directory behind it.

| Change | Location |
|---|---|
| `/` renders the digest for anonymous and logged-in visitors alike, instead of the landing page and the editor respectively | `app.go` `handleViewHome` |
| Digest handler, `/blogs` directory handler, the active-blogs query, and its cache | `home.go` |
| `homeFeed` cache field and its init, sharing `initLocalTimeline`'s `local_timeline` guard because the feed reads `app.timeline` for its posts | `app.go` `App.homeFeed`, `Initialize` |
| `/blogs` and `/blogs/p/{page}` registered with the special pages, ahead of the `/{collection}` and `/{post}` catch-alls that would otherwise swallow them | `routes.go` |
| `blog` and `blogs` reserved so no collection can claim an alias that collides with the directory route | `author/author.go` `reservedUsernames` |
| Home / Blogs nav links un-gated from Chorus mode; the existing `/me/c/` link renamed to "My blogs"; a permanent Write button, since `/` is no longer where members land | `templates/base.tmpl` |
| `default_visibility = public`, so new blogs are not created invisible to the feed and the directory | `config.ini.example` (see the deployment prerequisite below — production's `config.ini` is untracked and needs the same edit by hand) |

Recent posts reuse `app.timeline` — the `memo.Memo` that already backs `/read`
— rather than adding a second post query, so the page adds no per-request
database load and inherits upstream's per-author fairness cap. Only the
active-blogs aggregate is new.

One behaviour change falls out of where the feed sits in `handleViewHome`:
with a `landing` path configured (`App.LandingPath()`), a **logged-in** user
now gets that redirect too. Upstream only ever sent an anonymous visitor to
the landing path and always gave a logged-in user the Pad; the Pad branch
that used to sit after the landing-path check is gone, and the feed now
occupies that same spot, so the landing-path check runs — and can redirect —
before either kind of visitor reaches it. Defensible, since the operator
configured `/` to redirect, full stop, but it is a real semantic change from
upstream and worth knowing before assuming `landing` only ever affects
anonymous traffic.

### Deployment prerequisite: `default_visibility` does not get set by merging this branch

`config.ini` is untracked (`.gitignore` has `*.ini`) — only
`config.ini.example` ships in this repo. Production reads `/data/config.ini`,
bind-mounted from `./data` on the host (`docker-compose.prod.yml`).
**Merging this branch to `theatl-main` and redeploying does not change
production's `default_visibility`.** Someone has to hand-edit the production
`config.ini` to add `default_visibility = public` and restart the container.

Miss this and nothing looks broken: the app runs, existing blogs work, and
new blogs just keep defaulting to unlisted, so the feed and `/blogs` stay
permanently empty even though the feature is fully implemented.

That part is still true even with the check below — the pages still render
empty either way, silently. What's no longer true is that the *cause* is
invisible: `initHomeFeed` (`home.go`) logs a startup warning whenever
`local_timeline` is enabled and `default_visibility` isn't `public`, naming
the misconfigured setting. So a missed edit shows up in the container logs
at boot; nothing on the page itself will tell you. Still check this by hand
on every deploy that includes this branch — the warning is a safety net for
when that check is missed, not a substitute for it.

## Why the schema change avoids the migration system

`migrations/migrations.go` registers migrations in a flat ordered slice where
`CurrentVer()` is `len(migrations)`. Appending ours would collide with the next
upstream migration and cause a recorded version to re-run our `ALTER`. Instead the
column is added by an idempotent guard at startup, keeping `migrations.go` out of
the merge surface entirely.

## Merge policy

Only `app.go`, `account.go`, `collections.go`, `routes.go`, `posts.go`,
`oauth.go`, and `author/author.go` are modified, by one line or a short block
each, all marked with `theATL fork:` comments. Merge upstream releases onto
`theatl-main`; conflicts should be confined to those seven files.
`author/author.go` is the seventh, added by the discovery feed to reserve
`blog`/`blogs` in `reservedUsernames` so no collection can claim an alias
that collides with the `/blogs` route — a two-entry addition to a static map
that upstream changes rarely; the alternative was a permanently worse URL.

`templates/base.tmpl` is also fork-modified — this feature's nav changes
land there too (see the table above) — but it is not a Go file, so it sits
outside the seven-file budget above by definition, not as an exception to
it. It was already carrying unrelated fork edits before this feature (e.g.
the footer version-link fix), so this is not the first time it has diverged
from upstream, and conflicts there are still worth checking on every merge.

Wholly new, fork-owned files — `maxblogs.go`, `maxblogs_api.go`,
`oauth_preauth.go`, `oauth_reconcile.go`, and `home.go` — carry their own
full copyright header instead of a `theATL fork:` comment on an upstream
line, and are not part of this budget: there is no upstream version of them
to conflict with. Their `_test.go` counterparts (`maxblogs_test.go`,
`oauth_preauth_test.go`, `oauth_reconcile_test.go`, `home_test.go`) do NOT
carry that header — non-test fork-owned files carry it, their `_test.go`
counterparts don't — and are likewise outside this budget, for the same
reason: no upstream version exists to conflict with.
`oauth_signup.go` is unmodified upstream code left in place but unrouted
(see the "What diverges" table above) — also not on this budget, since
nothing in it was changed.

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
- **Bump the AGPL §5(a) notice count** — it is seven files now.

## Known upstream test failures

CI skips exactly this one **subtest** by its fully slash-qualified path
(see `.github/workflows/ci.yml`'s `Test` step — the `/subtest` qualifier matters:
without it, `-skip` matches the parent test name and drops every subtest beneath
it, e.g. all five of `TestUpdatesRoundTrip`'s subtests instead of just the one
that's broken). It is pre-existing at v0.17.1, in a file outside the
merge-surface budget above, and unrelated to anything in this fork:

- `TestUpdatesRoundTrip/Release_URL` (`updates_test.go`) — a race: the cache's
  version-check network call runs in an unsynchronized goroutine, and the
  `Release_URL` subtest reads the result before it's populated. The other four
  subtests (`New_Updates_Cache`, `Check_Now`, `Are_Available`, `Latest_Version`)
  are unaffected and run normally.

Revisit on every upstream merge. Delete this entry the moment a release fixes
the underlying race.

`TestViewOauthCallback/success` (`oauth_test.go`) used to have a matching
entry here too, for a rationale that went stale (a pre-existing,
fork-unrelated redirect-assertion mismatch) and was superseded by a different,
current problem: this subtest predates the fork's OAuth JIT provisioning work
(`oauth.go`, `oauth_preauth.go`), and the JIT branch now runs unconditionally
before the registration-blocked branch this subtest was written to exercise,
calling `app.db.GetOauthPreauth` via the concrete `*datastore` — a call this
subtest's intentionally-nil `app.db` can't serve, so it panics instead of
returning the redirect the test expects. A panic here would abort the whole
package test binary, silently taking this fork's own
`TestViewOauthCallbackJITProvisioning` and `TestOauthSignupRouteIsNotRegistered`
down with it. The subtest now carries its own `t.Skip` (`oauth_test.go`) with
this explanation, which fully supersedes the CI-level entry, so the entry has
been removed from `-skip` rather than kept for history. If a future upstream
merge changes this subtest enough that the in-code skip stops applying, judge
it fresh rather than restoring a CI-level entry on the old rationale.

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

**In-app self-serve signup is closed by infrastructure, not by this repo.**
Two signup routes exist (`routes.go`) and they are not equivalent:

- `POST /api/auth/signup` IS gated by `open_registration` in config, at
  route-registration time — the handler is only mounted at all when
  `open_registration` is true.
- `POST /auth/signup` is registered UNCONDITIONALLY, regardless of
  `open_registration`. Its in-app check (`unregisteredusers.go`'s
  `handleWebSignup`) only rejects when `open_registration` is false AND the
  submitted `invite_code` form field is empty — ANY non-empty `invite_code`
  bypasses that check. The code is never validated against the database
  before the account is created: `account.go`'s `signupWithRegistration`
  calls `CreateUser` first, and only afterward calls `database.go`'s
  `CreateInvitedUser`, which is a bare insert into `usersinvited` recording
  that this user supplied some invite code string — it does not check that
  the code exists, is unexpired, or is unused. So `open_registration = false`
  provides no real protection on `/auth/signup` at all; the only actual gate
  on that route in this deployment is an external HAProxy ACL that lives
  entirely outside this repository, not any config value in this app.

Only the OAuth JIT path (`oauth.go`, `oauth_preauth.go`) enforces its access
gate in code, unconditionally, via the `oauth_preauth` table. Don't conflate
any of these when reasoning about what's "closed" on this instance.

**A revoked/deleted account can still be re-provisioned by a later login.**
`handleSetMastodonUserMaxBlogs` (`oauth_preauth.go`) now supports a
`max_blogs: 0` "revoke" signal that deletes an unconsumed `oauth_preauth`
row outright, closing the gap where a cancelled membership that never logged
in kept a permanently valid grant. That is scoped narrowly to *pending*
grants, though: it says nothing about an account an admin has already
deleted at the database level. If the member site pushes a fresh allowance
for that same Mastodon identity after such a deletion (as its normal nightly
sync would), `GetIDForRemoteUser` reports "not linked" (the `oauth_users` row
is gone too), a new `oauth_preauth` row is written, and the member's next
login re-provisions a brand-new account under the same identity. Deliberately
not addressed here — narrower fixes (an explicit tombstone/deny-list keyed on
`remote_user_id`, or having the member site's deletion flow revoke first)
are possible, but out of scope for this round; noted so it isn't confused for
an oversight in the revoke fix above.

**The advisory-lock connection pool can head-of-line-block an uncontested
identity.** `app.oauthLockDB` (`app.go`) has `oauthIdentityLockPoolMaxOpenConns`
(8) connections total, shared by every call to `withOauthIdentityLock`
(`oauth_preauth.go`) regardless of which identity it locks. The *named*
MariaDB lock itself is correctly per-identity — two callers locking different
identities never wait on each other's `GET_LOCK`. But both still have to pin
a connection out of the same 8-slot pool first. Under enough concurrent
callers saturating *other* identities, a caller for a completely uncontested
identity can queue behind them for a free pool connection before it ever
gets to call `GET_LOCK` for its own lock name — a real wait, bounded by
`oauthIdentityLockTimeoutSeconds` (10s) plus the request's own context
deadline, after which it fails closed with an error rather than proceeding
unsynchronized. Found during review, not from a production incident. Accepted
as-is: 8 was sized for one community's Mastodon membership logging in (see
the constant's doc comment) — nowhere near internet-scale concurrency, so
this queueing depth doesn't bite in practice at this deployment's traffic
levels; revisit the constant if that assumption stops holding.

**Combined connection budget across the two pools.** The main pool
(`app.db`, `connectToDatabase`) is capped at `MaxOpenConns(50)`; the lock
pool (`app.oauthLockDB`) adds up to 8 more on top — 58 MariaDB connections
total from this process, plus whatever else shares that server. Fine at this
deployment's scale (one community instance behind one reverse proxy, not
internet-scale traffic); re-check MariaDB's `max_connections` and what else
contends for it before raising either number.

**The discovery feed is eventually consistent, by up to 10 minutes.** Both
`/` and `/blogs` read memoized caches on `tlCacheDur` (`read.go`), so a newly
published post, or a blog just flipped to public, does not appear until the
memo expires. This is what keeps the pages free of per-request database load,
and it matches `/read`'s long-standing behaviour — but it reads as a bug to a
member who publishes and immediately refreshes the home page. Say so in
member-facing docs rather than shortening the TTL.

**The active-blogs query does a full scan and filesort.** `posts` has no index
on `created` (`schema.sql`), and the query groups by `collection_id` while
ordering on `MAX(p.created)`. Accepted as-is: it is identical in kind to
`FetchPublicPosts` (`read.go`), which has scanned and filesorted the same
table every 10 minutes since long before this fork, and it sits behind the
same memo. Revisit — with an index on `posts(collection_id, created)` — if the
post count grows by an order of magnitude or the TTL is shortened.

**Flipping `default_visibility` does not migrate existing blogs.** New blogs
are created public and therefore listed; every blog created before this change
is unlisted and stays out of the feed and the directory until its owner opts
in. There is no backfill, deliberately: unlisted blogs are still fully public
at their own URLs and still federate, so the only thing "unlisted" withholds
is directory listing — which makes listing them retroactively an override of a
choice their owners were shown.

**The vet gate doesn't cover every fork-owned file.** The vet step in
`.github/workflows/ci.yml` greps `vet.log` for `(^|/)maxblogs[a-z_]*\.go:`
only, so a `go vet` finding in `oauth_preauth.go`, `oauth_reconcile.go`, or
`home.go` — all fork-owned, none named `maxblogs*` — would print as
"informational" and pass CI silently rather than fail the build. Pre-existing,
not fixed here: `home.go` was kept vet-clean by hand during this work in lieu
of a real gate. Before relying on this gate for anything beyond `maxblogs.go`
itself, widen the grep to name every fork-owned file (or match on the fork's
copyright header instead of specific filenames).
