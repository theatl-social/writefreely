/*
 * Copyright © 2026 theATL.social.
 *
 * This file is part of the theATL.social fork of WriteFreely and is licensed
 * under the GNU Affero General Public License, included in the LICENSE file in
 * this source code package.
 *
 * OAuth just-in-time provisioning: a member's Mastodon identity is
 * pre-authorized in advance (by the member site, over the internal network),
 * and an account is created automatically on their first real OAuth login.
 * See FORK.md.
 */

package writefreely

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/gorilla/mux"
	"github.com/writeas/web-core/log"
)

// ensureOauthPreauthTable creates oauth_preauth if it does not exist.
//
// Deliberately NOT using the migrations system, for the same reason as
// users.max_blogs: migrations.go's CurrentVer() is len(migrations), so a
// fork-owned entry collides with the next upstream migration on merge.
func (db *datastore) ensureOauthPreauthTable() error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS oauth_preauth (
		remote_user_id VARCHAR(191) NOT NULL,
		provider       VARCHAR(191) NOT NULL,
		client_id      VARCHAR(191) NOT NULL,
		max_blogs      INT NOT NULL,
		created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (remote_user_id, provider, client_id)
	)`)
	if err != nil {
		return err
	}
	log.Info("oauth_preauth: table present.")
	return nil
}

// GetOauthPreauth returns the pre-authorized blog limit for a remote OAuth
// identity, and whether a row exists at all.
func (db *datastore) GetOauthPreauth(remoteUserID, provider, clientID string) (int, bool, error) {
	var max int
	err := db.QueryRow(
		"SELECT max_blogs FROM oauth_preauth WHERE remote_user_id = ? AND provider = ? AND client_id = ?",
		remoteUserID, provider, clientID).Scan(&max)
	if err == sql.ErrNoRows {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	return max, true, nil
}

// UpsertOauthPreauth records (or updates) the blog limit a not-yet-provisioned
// Mastodon identity will get on their first login.
func (db *datastore) UpsertOauthPreauth(remoteUserID, provider, clientID string, maxBlogs int) error {
	if db.driverName == driverSQLite {
		_, err := db.Exec(
			"INSERT OR REPLACE INTO oauth_preauth (remote_user_id, provider, client_id, max_blogs) VALUES (?, ?, ?, ?)",
			remoteUserID, provider, clientID, maxBlogs)
		return err
	}
	_, err := db.Exec(
		"INSERT INTO oauth_preauth (remote_user_id, provider, client_id, max_blogs) VALUES (?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE max_blogs = ?",
		remoteUserID, provider, clientID, maxBlogs, maxBlogs)
	return err
}

// DeleteOauthPreauth removes a pre-authorization row. A no-op, not an error,
// when the row does not exist — this is called both as normal cleanup after a
// successful JIT provision and speculatively, so callers must not have to
// distinguish "deleted" from "was already gone".
func (db *datastore) DeleteOauthPreauth(remoteUserID, provider, clientID string) error {
	_, err := db.Exec(
		"DELETE FROM oauth_preauth WHERE remote_user_id = ? AND provider = ? AND client_id = ?",
		remoteUserID, provider, clientID)
	return err
}

// maxBlogsCeiling is shared with the shipped max_blogs setter's semantics:
// tier allowances are 1/3/15, and nothing legitimate ever needs more.
const oauthMaxBlogsCeiling = 1000

// oauthRevokedAccountMaxBlogs is what an already-provisioned account's
// max_blogs is set to when the member site sends the revoke signal
// (max_blogs: 0) for an identity that is already linked to an account.
//
// It cannot literally be 0: effectiveMaxBlogs/checkBlogLimitN (maxblogs.go)
// treat any stored value <= 0 as UNLIMITED, not "zero blogs allowed" -- so
// writing 0 here would grant the opposite of what "revoke" means. 1 is the
// lowest real tier and the smallest value that still means "capped" rather
// than "unlimited": it blocks creation of any additional blog beyond what
// the member already has. Existing blogs are left untouched, and this does
// NOT deprovision the account -- fully blocking/deleting an already-
// provisioned account is a separate, explicitly deferred problem (see
// FORK.md's "Known limits": there is no protection today against an admin-
// deleted account being re-provisioned by a later login).
const oauthRevokedAccountMaxBlogs = 1

// oauthIdentityLockTimeoutSeconds bounds how long a request waits for the
// per-identity advisory lock (see withOauthIdentityLock) before giving up.
const oauthIdentityLockTimeoutSeconds = 10

// oauthIdentityLockPoolMaxOpenConns sizes App.oauthLockDB (app.go), the
// dedicated connection pool withOauthIdentityLock pins its GET_LOCK/
// RELEASE_LOCK connection from -- deliberately separate from the main pool's
// MaxOpenConns(50) (app.go's connectToDatabase). See withOauthIdentityLock's
// doc comment for the full deadlock this separation fixes; this constant only
// needs to answer "how many identities can have a lock pinned at once" for a
// single community's Mastodon membership logging in, which is nowhere near
// internet-scale. Kept in the single digits deliberately: this pool exists
// ONLY to hold pinned lock connections (never runs fn()'s own queries, which
// still go through the main pool), so it does not need to track the main
// pool's size at all -- a caller beyond this many concurrent identity-locks
// simply queues briefly for a lock-pool connection via context, the same way
// it would queue for any other exhausted pool, and does not compete with or
// diminish the main pool's 50 connections in any way.
const oauthIdentityLockPoolMaxOpenConns = 8

// oauthIdentityLockName derives a MariaDB GET_LOCK() name for one OAuth
// identity. GET_LOCK() lock names are capped at 64 characters (MariaDB, like
// MySQL); remote_user_id/provider/client_id are arbitrary-length strings
// pulled from OAuth responses and app config, so this hashes them into a
// fixed-width 64-character hex name rather than concatenating them directly
// and risking truncation-induced collisions between two different identities.
func oauthIdentityLockName(remoteUserID, provider, clientID string) string {
	sum := sha256.Sum256([]byte(remoteUserID + "\x00" + provider + "\x00" + clientID))
	return fmt.Sprintf("%x", sum)
}

// withOauthIdentityLock runs fn while holding a MariaDB session-scoped
// advisory lock scoped to one OAuth identity (remote_user_id + provider +
// client_id).
//
// This closes a TOCTOU race between two independent code paths that both act
// on the same identity without ever sharing a row to lock via a plain
// SELECT ... FOR UPDATE, because neither the oauth_preauth row nor the
// oauth_users row necessarily exists yet when either path starts:
//   - handleSetMastodonUserMaxBlogs (this file) reads GetIDForRemoteUser,
//     then either updates users.max_blogs or upserts/deletes oauth_preauth
//     based on that read.
//   - viewOauthCallback's JIT branch (oauth.go) reads GetOauthPreauth, then
//     creates the account, links it, and deletes the preauth row.
//
// Interleaved without this lock, an allowance push can read "not linked"
// while a concurrent login is mid-flight, silently lose the push (the
// account keeps its old allowance but the push still reports success), and
// resurrect a preauth row for an identity that is now already provisioned
// (which a later login would never consume, since it only checks
// GetOauthPreauth when GetIDForRemoteUser first comes back unlinked).
//
// GET_LOCK()/RELEASE_LOCK() are session-scoped: they must be acquired and
// released on the SAME underlying connection, which is why this pins one via
// Conn() rather than going through the normal pooled db.Exec/QueryRow.
//
// That pinned connection comes from app.oauthLockDB -- a small, SEPARATE pool
// from app.db's own (see the App.oauthLockDB field doc comment in app.go) --
// not from app.db's pool. The work inside fn is free to use app.db's ordinary
// pooled connections for its own queries; the mutual exclusion comes entirely
// from holding the named lock for fn's duration, not from which connection
// fn's queries happen to run on.
//
// This split exists because an earlier version of this function pinned the
// connection from app.db's own pool. Under enough concurrent callers, every
// one of that pool's connections ended up pinned by a lock-holder that was
// itself blocked waiting for a SECOND connection from that same, now-
// exhausted pool to run fn() -- a hard deadlock that starved the entire
// application (not just OAuth logins), reproduced directly against a real
// MariaDB: at concurrency == MaxOpenConns, a sub-millisecond operation below
// that threshold became a total hang with 100% failure. Pinning from a
// dedicated pool instead makes that starvation structurally impossible: no
// matter how many identities are locked concurrently, pinning connections
// here can never compete for or consume connections app.db's pool needs, for
// this code's own fn() or for anything else the application is doing.
//
// This fork's only deployment target is MariaDB (see maxblogs.go's
// ensureMaxBlogsColumn for the same reasoning), so this is a no-op
// passthrough under sqlite: there is no concurrent production traffic to
// race in that configuration, GET_LOCK has no sqlite equivalent, and
// app.oauthLockDB is never opened (nil) for that driver.
func withOauthIdentityLock(ctx context.Context, app *App, remoteUserID, provider, clientID string, fn func() error) error {
	if app.db.driverName != driverMySQL {
		return fn()
	}
	if app.oauthLockDB == nil {
		// Fail loudly rather than silently falling back to pinning app.db's
		// own pool -- that fallback would quietly reintroduce the exact
		// deadlock this split exists to prevent. Every real (non-test) mysql
		// code path always has this set by connectToDatabase (app.go); seeing
		// this means a caller (very likely a test) built an *App by hand
		// without wiring it up.
		return fmt.Errorf("oauth identity lock: App.oauthLockDB is nil for the mysql driver -- refusing to fall back to the main pool")
	}

	name := oauthIdentityLockName(remoteUserID, provider, clientID)

	conn, err := app.oauthLockDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("oauth identity lock: acquiring connection: %w", err)
	}
	defer conn.Close()

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, oauthIdentityLockTimeoutSeconds).Scan(&got); err != nil {
		return fmt.Errorf("oauth identity lock: GET_LOCK: %w", err)
	}
	// GET_LOCK() returns 1 on success, 0 on timeout, NULL on error (e.g. an
	// out-of-memory condition acquiring the lock). Fail closed on both of the
	// non-success outcomes -- better a 500 the caller can retry than running
	// the guarded work unsynchronized.
	if !got.Valid || got.Int64 != 1 {
		return fmt.Errorf("oauth identity lock: could not acquire lock %q (result=%v)", name, got)
	}
	defer func() {
		// Use a background context, not ctx: if ctx is already done, we still
		// want to attempt the release rather than skip it outright. If this
		// itself fails, the lock is released anyway when the connection
		// closes (MariaDB releases session locks on disconnect), so this is
		// belt-and-suspenders, not the only path to release.
		if _, err := conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", name); err != nil {
			log.Error("oauth identity lock: RELEASE_LOCK failed for %q (will still release when the connection closes): %v", name, err)
		}
	}()

	return fn()
}

// handleSetMastodonUserMaxBlogs serves
// POST /api/internal/mastodon-user/{remoteUserID}/max-blogs.
//
// Replaces the username-keyed setter from the shipped increment. The member
// site calls this UNCONDITIONALLY on every allowance change, whether or not
// the member has logged into Write Freely yet:
//   - Already linked (GetIDForRemoteUser finds a local user): update
//     users.max_blogs directly, exactly as the old setter did, and make sure
//     no stale oauth_preauth row survives.
//   - Not yet linked: upsert oauth_preauth. The account does not exist yet;
//     JIT provisioning in oauth.go consumes this row on first login.
//
// max_blogs: 0 is a distinct REVOKE signal, not a rejected value. The member
// site sends it when a membership is cancelled before the member ever logs
// into Write Freely -- without this, a preauth row lives forever until either
// a login consumes it or a later push overwrites it, so a cancelled member
// who never logged in kept a permanently valid grant. Revoke means:
//   - Not yet linked: delete the pending preauth row outright (if any), so a
//     later login attempt finds none and is correctly refused. This does NOT
//     write a max_blogs: 0 preauth row -- if a login ever consumed one, 0
//     would flow into users.max_blogs and mean UNLIMITED there (see
//     oauthRevokedAccountMaxBlogs), the opposite of "revoked".
//   - Already linked: cap at oauthRevokedAccountMaxBlogs (1), for the same
//     "0 means unlimited" reason -- see that constant's doc comment. This
//     does not deprovision the account; that is out of scope (FORK.md).
//
// The check-then-write here (GetIDForRemoteUser, then branch) runs inside
// withOauthIdentityLock: without it, this and the JIT login path in
// oauth.go's viewOauthCallback can interleave -- a push reads "not linked"
// while a concurrent login is mid-flight, and the push silently loses (200
// OK, but the account keeps its OLD allowance) while resurrecting a preauth
// row for an identity that is now already provisioned. See that function's
// doc comment for the full race and why a plain transaction alone can't
// close it (neither row necessarily exists yet when either side starts).
//
// Same fail-closed contract as the shipped setter: constant-time secret
// check before anything else, *int + DisallowUnknownFields so an absent or
// misspelled field is a 400 rather than a silent zero, a ceiling, a body-size
// limit.
func handleSetMastodonUserMaxBlogs(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := os.Getenv("WRITEFREELY_API_SECRET")
		if len(secret) < 32 {
			log.Error("oauth_preauth: WRITEFREELY_API_SECRET unset or under 32 chars; refusing request")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		provided := sha256.Sum256([]byte(r.Header.Get("X-WriteFreely-Secret")))
		expected := sha256.Sum256([]byte(secret))
		if subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var body struct {
			MaxBlogs *int `json:"max_blogs"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil || body.MaxBlogs == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// 0 is the explicit revoke signal (see doc comment above); anything
		// else below 1, or above the ceiling, is still rejected.
		if *body.MaxBlogs < 0 || *body.MaxBlogs > oauthMaxBlogsCeiling {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		remoteUserID := mux.Vars(r)["remoteUserID"]
		provider := "generic"
		clientID := app.cfg.GenericOauth.ClientID
		revoke := *body.MaxBlogs == 0

		err := withOauthIdentityLock(r.Context(), app, remoteUserID, provider, clientID, func() error {
			localUserID, err := app.db.GetIDForRemoteUser(r.Context(), remoteUserID, provider, clientID)
			if err != nil {
				log.Error("oauth_preauth: lookup failed for %q: %v", remoteUserID, err)
				return err
			}

			if localUserID != -1 {
				user, err := app.db.GetUserByID(localUserID)
				if err != nil {
					log.Error("oauth_preauth: user %d not found: %v", localUserID, err)
					return err
				}
				effective := *body.MaxBlogs
				if revoke {
					effective = oauthRevokedAccountMaxBlogs
				}
				if err := app.db.SetUserMaxBlogs(user.Username, effective); err != nil {
					log.Error("oauth_preauth: set failed for %q: %v", user.Username, err)
					return err
				}
				// Defensive: a preauth row should not exist once linked, but a race
				// between provisioning and a second push could leave one. Clear it.
				if err := app.db.DeleteOauthPreauth(remoteUserID, provider, clientID); err != nil {
					log.Error("oauth_preauth: cleanup delete failed for %q (user already linked, non-fatal): %v", remoteUserID, err)
				}
				return nil
			}

			if revoke {
				// No account exists yet -- there is nothing to downgrade, just
				// make sure no pending grant survives for a future login to
				// consume.
				if err := app.db.DeleteOauthPreauth(remoteUserID, provider, clientID); err != nil {
					log.Error("oauth_preauth: revoke delete failed for %q: %v", remoteUserID, err)
					return err
				}
				return nil
			}

			if err := app.db.UpsertOauthPreauth(remoteUserID, provider, clientID, *body.MaxBlogs); err != nil {
				log.Error("oauth_preauth: upsert failed for %q: %v", remoteUserID, err)
				return err
			}
			return nil
		})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		log.Info("oauth_preauth: set %q to %d", remoteUserID, *body.MaxBlogs)
		w.WriteHeader(http.StatusOK)
	}
}

var invalidUsernameChars = regexp.MustCompile(`[^a-z0-9-]+`)

// normalizeOauthUsername maps a Mastodon username onto a valid, available
// Write Freely username. Write Freely requires 3+ characters, letters/
// numbers/hyphens only, and rejects a reserved-word list (all enforced by
// author.IsValidUsername) — Mastodon usernames may contain underscores,
// which is not valid here, may be too short, may collide with a reserved
// name (e.g. "admin", "login"), and may already be taken.
//
// This function is deliberately decoupled from author.IsValidUsername and
// config.Config: the caller's taken() closure is expected to fold validity
// in alongside uniqueness (report an invalid or reserved candidate as
// "taken" too), so that a too-short, malformed, or reserved base -- or the
// empty string, if every character of the Mastodon username was stripped as
// invalid -- is routed through the exact same suffix-fallback tiers below as
// an ordinary uniqueness collision, rather than needing special-cased
// handling here. In particular, this means there is no hardcoded minimum
// length here: relying on taken() to reject a too-short base is what
// prevents that short-name case from being special-cased onto a literal
// fallback username that could itself collide with the reserved list (as a
// hardcoded "user" literal once did).
//
// Deterministic and idempotent: the same Mastodon identity always resolves
// to the same Write Freely username on retry, so a failed provisioning
// attempt can safely be retried without producing a different account.
//
// That guarantee assumes the caller's CreateUser and RecordRemoteUserID calls
// either both succeed or the caller has separately handled the partial-
// failure case. If CreateUser succeeds but RecordRemoteUserID then fails, the
// account it created is real and taken() will report its username as
// occupied on any retry -- normalizeOauthUsername has no way to know that
// account is orphaned, so a retry produces a SECOND, differently-suffixed
// account instead of completing the first. viewOauthCallback logs loudly on
// that specific failure so it is discoverable, but does not currently
// reconcile it automatically.
func normalizeOauthUsername(mastodonUsername, remoteUserID string, taken func(string) bool) string {
	base := strings.ToLower(mastodonUsername)
	base = invalidUsernameChars.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")

	if !taken(base) {
		return base
	}
	suffixed := base + "-" + remoteUserID
	if !taken(suffixed) {
		return suffixed
	}
	// Extremely unlikely — the base is also colliding with the suffixed form,
	// e.g. someone already registered literally "jsmith-14882". Fall back to
	// a form keyed purely on the remote ID.
	//
	// This tier is NOT actually unique by construction, despite the remote ID
	// itself being unique: a pre-existing account/collection/post literally
	// named "user-<remoteUserID>" is reachable (e.g. via the orphan-retry
	// scenario described above, or via /auth/signup's invite-code bypass --
	// see FORK.md's "Known limits"), and taken() folds exactly that kind of
	// occupancy in alongside plain username collisions. So run this tier
	// through taken() too, like every other tier, rather than assuming it
	// away. If it's ALSO occupied there is no further fallback tier -- the
	// caller's CreateUser call is about to 409 -- so log loudly here with
	// enough to actually find the collision, then return the value anyway:
	// the caller already logs the CreateUser failure itself (see
	// viewOauthCallback in oauth.go), this just makes that failure
	// explicable instead of an opaque 500.
	final := "user-" + remoteUserID
	if taken(final) {
		// This fork's JIT provisioning only ever has one identity source --
		// Mastodon via the generic OAuth provider -- the same fact
		// handleSetMastodonUserMaxBlogs (also in this file) hardcodes as
		// provider := "generic". normalizeOauthUsername itself takes no
		// provider parameter (see the doc comment above: deliberately
		// decoupled from config), so that's stated directly here rather than
		// threaded through as an argument.
		log.Error("oauth JIT: normalizeOauthUsername exhausted every fallback tier -- %q (provider generic, remote user %s) is ALSO taken; CreateUser is about to 409 with no further fallback available", final, remoteUserID)
	}
	return final
}

// knownOauthProviders is the exact, canonical set of provider identifiers
// this codebase's OAuth clients report via GetProvider(): oauth_generic.go,
// oauth_gitlab.go, oauth_gitea.go, oauth_slack.go, and oauth_writeas.go. It is
// intentionally a fixed list of literal strings rather than something built
// by instantiating those clients: GetProvider() returns a hardcoded literal
// in every implementation (it doesn't depend on config), so duplicating the
// five literals here needs no client construction (and thus no
// clientID/secret/etc from config) just to ask what their provider strings
// are. If a sixth OAuth client is ever added, its GetProvider() literal must
// be added here too.
var knownOauthProviders = map[string]bool{
	"generic":  true,
	"gitlab":   true,
	"gitea":    true,
	"slack":    true,
	"write.as": true,
}

// isKnownOauthProvider reports whether provider is an EXACT match --
// case-sensitive, untrimmed, no normalization of any kind -- for one of this
// codebase's canonical OAuth provider identifiers.
//
// Why exact match and nothing looser: oauth_users.provider is a MariaDB
// column using a case-insensitive, pad-insensitive, AND accent-insensitive
// collation (utf8mb4_uca1400_ai_ci, confirmed via SHOW FULL COLUMNS). A
// DELETE keyed on that column matches "generic", "Generic", " generic ",
// "generíc", "genërìc", and even full-width "ｇｅｎｅｒｉｃ" all the same,
// regardless of what Go-level string comparison guards it. Two earlier fixes
// here tried to make a Go comparison agree with that collation by
// normalizing harder -- first case-folding, then also trimming -- and both
// were incomplete, because no finite normalization pipeline can cover
// everything an arbitrary collation treats as equal (accent folding and
// full-width-to-halfwidth folding are just the next two gaps; there is no
// reason to believe they're the last).
//
// An allowlist sidesteps the problem instead of chasing it: it never asks
// "does this value normalize to something the database would also treat as
// generic?" -- a question that requires reverse-engineering the collation's
// entire equivalence class. It only asks "is this value byte-for-byte
// identical to one of the five strings we know are canonical?", which is a
// question this code can answer completely on its own. Anything that isn't
// an exact match -- including every one of the bypass strings above -- is
// rejected before it reaches ANY comparison, Go's or the database's, that
// could disagree about what counts as "generic".
func isKnownOauthProvider(provider string) bool {
	return knownOauthProviders[provider]
}
