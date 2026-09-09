# theATL.social fork of WriteFreely

Based on upstream [WriteFreely](https://github.com/writefreely/writefreely) **v0.17.2**.
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

**API signup invite-code gate (Findings A and B) — no longer a divergence.**
This fork carried its own fix for Finding A (`ef89388`, PR #17) between
2026-08-25 and 2026-09-09. It has been **removed**, because upstream's
`dd2de20` fixes the same hole more broadly and was backported on 2026-09-09
(see "Merge history"). Upstream's `canRegister()` gate sits in
`signupWithRegistration()`, downstream of both `signup()` and
`handleWebSignup`, so it covers strictly more paths than this fork's inline
block in `signup()` did, and additionally gates on `DisablePasswordAuth`.
The `theATL fork:` comment above the `/api/auth/signup` registration in
`routes.go` was dropped at the same time: that line is now upstream's own
behaviour, byte-identical to what the fork had.

The same backport also fixed Finding B (`/oauth/signup`'s invite-code gate),
which this fork had deliberately declined to fix on the reasoning that the
route is unrouted here and its HMAC gate is keyed on an empty
`Server.HashSeed` regardless. That reasoning is now moot; all four of
upstream's `signup_test.go` tests pass and none are skipped in CI.

**Because upstream shipped tests for both findings a full release before
shipping either fix, a future upstream merge must not be assumed to silently
carry a real fix along with its tests.** Read what upstream actually did
before assuming a passing test means a fixed hole — and before assuming a
failing one means this fork broke something.

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
`config.ini.example` ships in this repo. Production runs from
`/home/debian/docker-compose.yml` on `theatl-services-ssh.theatl.social` (the
`writefreely` service, around line 563), which bind-mounts
`/home/debian/config.ini.writefreely` on the host **read-only** into the
container as `/go/config.ini`. Two things trip people up here: the mount is
`:ro`, so you edit the host file, not the container's copy; and that host
file is owned `bin:bin` mode `-rw-r-----`, so editing it needs `sudo`.
**Merging this branch to `theatl-main` and redeploying does not change
production's `default_visibility`.** Someone has to `sudo`-edit
`config.ini.writefreely` on the host to add `default_visibility = public`
and restart the container.

**The repo's `docker-compose.prod.yml` does not describe this deployment —
do not use it as deployment guidance.** It's stale relative to what actually
runs: it says `image: writefreely` where production runs
`mikehdev/writefreely:<git-describe-tag>` (CI publishes to that name, not
`writefreely`), and it references a `./data` bind mount production doesn't
have — the real config path is `config.ini.writefreely` at the compose
file's own level, as above. This mismatch is exactly what produced the
original (wrong) version of this section, which cited `/data/config.ini` and
`docker-compose.prod.yml` from this repo instead of the live host.

Deploying this branch also means bumping the image tag in
`/home/debian/docker-compose.yml` in the same pass — the `image:` line there
carries its own inline comment with the `git describe --tags --match 'v*'
--abbrev=7 origin/theatl-main` procedure for picking the right tag; follow
that comment rather than guessing a tag here. As of this writing the running
image is `mikehdev/writefreely:0.17.1-41-g5f40531` and the live `[app]`
section has no `default_visibility` line at all — the prerequisite this
section describes is currently unmet in production.

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
`oauth.go`, `author/author.go`, and `activitypub.go` are modified, by one line
or a short block each, all marked with `theATL fork:` comments. Merge upstream
releases onto `theatl-main`; conflicts should be confined to those **eight**
files.

`activitypub.go` is the eighth, added 2026-09-09 by the ActivityPub SSRF
hardening (see "Merge history"). It is the only entry on this budget that
exists to strengthen an upstream security fix rather than to add a feature:
upstream's `isPublicIRI()` validates the hostname *before* DNS resolution,
and the transport resolves again when it dials, so `activityPubClient()` now
carries a `DialContext` that validates the address actually connected to, plus
a redirect policy. Both reuse code upstream already added to this package in
`36dd733`. **If a future upstream release hardens `activityPubClient()`
itself, drop this fork's version and take theirs** -- that would return the
budget to seven.
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
- **Bump the AGPL §5(a) notice count** — it is eight files now.

## Merge history

**Security backports from `develop` (post-`v0.17.2`, 2026-09-09).** Eight
upstream commits cherry-picked onto `theatl-main` ahead of any v0.17.3
release, because three carry published GitHub advisories and upstream cut
`v0.17.2` on 2026-08-10 at 07:27 UTC -- hours before most of these landed.
This is the one deliberate exception to "merge upstream releases": the
fixes are taken as named cherry-picks with `-x` provenance lines, not as a
merge of an unreleased branch, so a later `v0.17.3` merge sees them as
already applied.

| Upstream | Fix |
|---|---|
| `d7231d8` | Always sanitize slugs on post creation (stored-XSS via API) |
| `48083e0` | CSRF on `/me` endpoints -- **GHSA-mp2f-3fq8-r9vj** |
| `477d0de` | Only the blog owner may pin posts -- **GHSA-hwfg-mg9c-cvgf** |
| `36dd733` | SSRF in webfinger `RemoteLookup` (dial-time IP validation) |
| `3e56d3a` | SSRF via ActivityPub inbox actor/object IRI resolution |
| `c4d0ac6` | Password-protected blogs not unlocking after cookie expiry |
| `dd2de20` | Signup checks enforced across web, API, and OAuth paths |
| `7fb71bc` | Private/protected blogs readable via AP -- **GHSA-cx5r-gg25-76ph** |

Two conflicts, both trivial. `go.mod`: upstream's hunk moves `go 1.19` ->
`1.21` with a `toolchain go1.23.0` directive; dropped, since this fork is
already on `go 1.25.0` matching the `golang:1.25-alpine3.22` Dockerfile pin.
`routes.go`: a **comment-only** conflict -- see below.

`e01f7d0` is folded into `3e56d3a`'s conflict resolution rather than applied
separately. Upstream added `isPublicIRI()` to `webfinger.go`'s
`RemoteLookup()` in `3e56d3a`, then reverted that hunk six minutes later
("The SSRF fix is already handled more robustly in other pending changes")
because `36dd733`'s dial-time client already covers it without the
DNS-rebinding window a pre-request hostname check leaves open. `webfinger.go`
here is byte-identical to upstream `develop`'s final state.

**This merge shrank the fork surface rather than growing it.** `dd2de20`
supersedes this fork's own Finding A fix (`ef89388`, PR #17): upstream's
`canRegister()` lives in `signupWithRegistration()`, downstream of both
`signup()` and `handleWebSignup`, so it covers strictly more paths than the
fork's inline block in `signup()` did, and additionally gates on
`DisablePasswordAuth`. The inline block is removed, and the `theATL fork:`
comment above the `/api/auth/signup` registration in `routes.go` is dropped
because that line is now upstream's own behaviour, byte-identical to what
the fork had. Seven-file merge budget and AGPL S5(a) notice count both
unchanged at seven.

`dd2de20` also fixes **Finding B** (`oauth_signup.go`'s invite gate), which
`ef89388` explicitly left out of scope because that route is unrouted in
this fork. `TestOAuthSignupClosedRegistration` and
`TestOAuthSignupCannotSwapInviteCodeWithoutInvalidatingSignature` now pass
and have been removed from the CI skip list, which is down to one entry.

**One fork-authored addition rides along with these backports:
`activitypub.go`'s client is hardened beyond what upstream shipped.**
Upstream's `3e56d3a` guards `resolveIRI()` with `isPublicIRI()`, which parses
the URL, resolves the host, checks the IPs, and returns -- and then
`activityPubClient().Do()` resolves the host a second time when it dials. A
host whose authoritative DNS answers with a public address for the first
lookup and a private one for the second passes the check and reaches the
internal target anyway, and redirects were never checked at all. This is
reachable from `POST /api/collections/{alias}/inbox`, which is registered
with `handler.All` and is therefore unauthenticated.

`activityPubClient()` now uses `safeDialContext` -- upstream's own function
from `36dd733`, already in this package -- which resolves once and dials the
vetted IP literally, plus a `CheckRedirect` that re-validates each hop and
caps the chain at five. This is upstream's own stated preference: `e01f7d0`
reverted the pre-flight check from `webfinger.go` six minutes after adding
it, on the grounds that the dial-time client handled it "more robustly".
They simply never applied that reasoning to the ActivityPub client. Covered
by `activitypub_ssrf_test.go` (fork-owned; per the convention above,
`_test.go` files carry no AGPL header). This is what takes the merge budget
from seven files to eight.

The `CreateCollection` re-enumeration this section's "Merge policy" mandates
was performed: still exactly three call sites -- `collections.go` via
`newCollection` (gated), `database.go` via `addPost` (gated at the handler),
and `CreateCollectionFromToken`, still with **zero** callers. None of these
eight commits touched them. The per-user blog cap gained no new hole.

**`v0.17.2`** (upstream commit `acdc6b9`, tag object `e041604`, dated
2026-08-09) merged onto `theatl-main` with **zero conflicts**. All seven
modified upstream files (`app.go`, `account.go`, `collections.go`,
`routes.go`, `posts.go`, `oauth.go`, `author/author.go`) merged cleanly;
upstream's own changes in this release landed in different regions of
`account.go`, `app.go`, `collections.go`, and `posts.go` than this fork's
edits, so nothing overlapped. The merge-surface budget is therefore
**unchanged** at those same seven files, and the AGPL §5(a) notice count
stays at seven — not bumped by this merge.

The `CreateCollection` re-enumeration this section's "Merge policy" mandates
on every upstream merge was performed: still exactly three call sites today
— `collections.go` via `newCollection` (gated), `database.go` via `addPost`
(gated at the handler), and `CreateCollectionFromToken`, which still has
**zero** callers. The per-user blog cap gained no new hole from this merge.

Upstream also removed the bundled Material Icons font files
(`static/fonts/MaterialIcons-Regular.{eot,svg,ttf}`) and changed
`less/icons.less` to serve only `.woff2`/`.woff` with `font-display: block`.
This fork's CSS is built by `make ui` at image-build time rather than
committed, so it isn't visible in this diff — re-check the built icon CSS
after the first deploy on this version to confirm nothing broke.

See the OAuth token-exchange entry under "Known limits" below for one more
v0.17.2 change (`oauth_generic.go`) that needs attention on this specific
deployment before or during that deploy.

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

Upstream v0.17.2 also shipped a new `signup_test.go` with four tests for two
named security findings ("Finding A" and "Finding B"), but no corresponding
code fix for either. Verified against pristine upstream: checked out the
`v0.17.2` tag in a clean worktree with none of this fork's code and ran them
there, and all four failed identically with no fork code present — they were
upstream's own untested-but-unfixed gaps, not something this fork caused.

**All four now pass and none are skipped.** Finding A was fixed by this fork
in `ef89388` (PR #17) and is now fixed upstream by `dd2de20`, backported
2026-09-09; the fork's own version was removed as redundant. Finding B,
which this fork had deliberately declined to fix, is fixed by that same
`dd2de20` — it routes the OAuth path through `canRegister()` and includes
the invite code in the signature. The four tests are:

- `TestAPISignupClosedRegistration` (subtests `no_invite_code`,
  `bogus_invite_code`, `valid_invite_code`)
- `TestInitRoutesAlwaysRegistersAPISignup`
- `TestOAuthSignupClosedRegistration` (subtests `no_invite_code`,
  `bogus_invite_code`, `valid_invite_code`)
- `TestOAuthSignupCannotSwapInviteCodeWithoutInvalidatingSignature`

CI's `-skip` list is consequently down to a single entry
(`TestUpdatesRoundTrip/Release_URL`). Re-check all four on every upstream
merge: upstream has shipped these tests ahead of their fixes once already,
so a failure here is more likely to mean upstream regressed than that this
fork did.

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

**Three signup routes exist (`routes.go`) and they are not equivalent.**
This used to be a "closed by infrastructure, not by this repo" story for
`/auth/signup`; upstream v0.17.2 closed the code-level bypass this entry
used to describe, so the entry now records the (still-relevant) distinction
between the three routes rather than a live hole:

- `POST /api/auth/signup` IS gated by `open_registration` in config, at
  route-registration time — the handler is only mounted at all when
  `open_registration` is true.
- `POST /auth/signup` is registered UNCONDITIONALLY, regardless of
  `open_registration`. As of upstream v0.17.2, its in-app check
  (`unregisteredusers.go`'s `handleWebSignup`) no longer just checks whether
  `invite_code` is a non-empty string: when `open_registration` is false it
  now calls `app.db.GetUserInvite(ur.InviteCode)` and rejects with 403
  "Registration is closed" if no such invite exists, then checks
  `i.Active(app.db)` and rejects with 404 "Invite link has expired." if the
  invite exists but isn't currently usable. An arbitrary non-empty code no
  longer bypasses the check — the code is now validated against the
  database before account creation, closing the gap the old wording of this
  entry described. Practically, this makes the external HAProxy ACL on this
  route defense-in-depth rather than the only real gate: `open_registration
  = false` now provides actual protection on `/auth/signup` in code, not
  just at the reverse proxy.
- Only the OAuth JIT path (`oauth.go`, `oauth_preauth.go`) enforces its
  access gate in code, unconditionally, via the `oauth_preauth` table — that
  was true before this merge and is unchanged by it.

Don't conflate any of these when reasoning about what's "closed" on this
instance: `/api/auth/signup` is closed by whether it's routed at all,
`/auth/signup` is now closed by a real database check plus a redundant
external ACL, and the OAuth path is closed by `oauth_preauth`.

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

**Upstream v0.17.2 changed how OAuth client credentials are sent, and this
instance has no fallback if the wrong choice breaks it.** Previously
`oauth_generic.go`'s token exchange sent client credentials both in the POST
form body and as HTTP Basic auth. A new `GenericOauthCfg.AuthUseRequestBody`
field (`config/config.go`, ini key `auth_use_basic_auth`) now selects one or
the other: false (the default, and what this fork's config leaves unset)
sends credentials via HTTP Basic auth ONLY; true sends them in the request
body ONLY. Every account on this instance is OAuth-JIT-provisioned and
passwordless, and production runs `disable_password_auth = true` — if the
Mastodon instance at theatl.social rejects HTTP Basic auth on its token
endpoint, nobody can log in and there is no password fallback to fall back
to. Confirm the Mastodon token endpoint accepts HTTP Basic auth before or
immediately after deploying this merge.

Also note the naming trap for whoever debugs a login failure here next: the
ini key is `auth_use_basic_auth`, but setting it `true` sets
`AuthUseRequestBody = true` — which puts credentials in the request BODY,
the opposite of what the key name reads like. Reading "use_basic_auth =
true" and concluding that turns Basic auth ON is exactly backwards.

**Verification:** This risk has been empirically tested against the live
production Mastodon (2026-08-25). Two probe requests were sent to the
`https://theatl.social/oauth/token` endpoint using the production
`client_id` and `client_secret`, with a deliberately invalid authorization
code. Probe A used HTTP Basic auth only (v0.17.2 default); Probe B put
credentials in the request body (old behaviour). Both returned
`{"error":"invalid_grant", ...}` — NOT `invalid_client`. The distinction
matters: `invalid_grant` indicates client authentication succeeded and only
the bogus code was rejected (correct outcome); `invalid_client` would mean
the credentials themselves were refused. Conclusion: theatl.social's Mastodon
(Doorkeeper) accepts HTTP Basic auth on its token endpoint, so v0.17.2's
default is safe for this deployment. The probes were run on the production
host, so the client secret never left it. If a future login failure occurs
despite this verification, the remediation lever is `auth_use_basic_auth =
true` in the `[oauth.generic]` config block, which switches credentials
back into the request body (note the inverted naming again).

**The vet gate doesn't cover every fork-owned file.** The vet step in
`.github/workflows/ci.yml` greps `vet.log` for `(^|/)maxblogs[a-z_]*\.go:`
only, so a `go vet` finding in `oauth_preauth.go`, `oauth_reconcile.go`, or
`home.go` — all fork-owned, none named `maxblogs*` — would print as
"informational" and pass CI silently rather than fail the build. Pre-existing,
not fixed here: `home.go` was kept vet-clean by hand during this work in lieu
of a real gate. Before relying on this gate for anything beyond `maxblogs.go`
itself, widen the grep to name every fork-owned file (or match on the fork's
copyright header instead of specific filenames).
