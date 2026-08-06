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
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
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
		if *body.MaxBlogs < 1 || *body.MaxBlogs > oauthMaxBlogsCeiling {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		remoteUserID := mux.Vars(r)["remoteUserID"]
		provider := "generic"
		clientID := app.cfg.GenericOauth.ClientID

		localUserID, err := app.db.GetIDForRemoteUser(r.Context(), remoteUserID, provider, clientID)
		if err != nil {
			log.Error("oauth_preauth: lookup failed for %q: %v", remoteUserID, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if localUserID != -1 {
			user, err := app.db.GetUserByID(localUserID)
			if err != nil {
				log.Error("oauth_preauth: user %d not found: %v", localUserID, err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if err := app.db.SetUserMaxBlogs(user.Username, *body.MaxBlogs); err != nil {
				log.Error("oauth_preauth: set failed for %q: %v", user.Username, err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			// Defensive: a preauth row should not exist once linked, but a race
			// between provisioning and a second push could leave one. Clear it.
			if err := app.db.DeleteOauthPreauth(remoteUserID, provider, clientID); err != nil {
				log.Error("oauth_preauth: cleanup delete failed for %q (user already linked, non-fatal): %v", remoteUserID, err)
			}
		} else {
			if err := app.db.UpsertOauthPreauth(remoteUserID, provider, clientID, *body.MaxBlogs); err != nil {
				log.Error("oauth_preauth: upsert failed for %q: %v", remoteUserID, err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
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
	// a form keyed purely on the remote ID, which is unique by construction.
	// (This form is not itself run back through taken(): "user-<remoteUserID>"
	// cannot collide with the reserved-word list, which only matches exact
	// literals, and remote IDs are unique by construction.)
	return "user-" + remoteUserID
}
