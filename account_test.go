/*
 * Copyright © 2026 theATL.social.
 *
 * This file is part of the theATL.social fork of WriteFreely and is licensed
 * under the GNU Affero General Public License, included in the LICENSE file in
 * this source code package.
 *
 * Tests for the theATL fork's AllowDisconnect gate on removeOauth. See
 * FORK.md.
 */

package writefreely

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/writeas/impart"
	"github.com/writefreely/writefreely/config"
)

// TestRemoveOauthRejectsGenericDisconnectWhenDisallowed proves the fix for a
// critical finding an adversarial whole-branch review confirmed by direct
// bypass: removeOauth (account.go) had NO server-side check of
// GenericOauth.AllowDisconnect at all. The only place that config value was
// ever consulted was templates/user/settings.tmpl, deciding whether to SHOW
// a disconnect button in the UI -- the handler itself enforced nothing, so a
// direct POST here, bypassing the UI entirely, succeeded regardless of
// config.
//
// That matters far more on this deployment than on a stock instance: every
// account here is provisioned via OAuth JIT (oauth.go) and is therefore
// guaranteed passwordless and emailless (see FORK.md) -- disconnecting one
// is permanent and unrecoverable, with no password login and no email for
// password reset.
//
// This does exactly what the review did: calls the handler directly (not
// through a router, not through the UI) with AllowDisconnect = false, and
// confirms the link survives.
func TestRemoveOauthRejectsGenericDisconnectWhenDisallowed(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}

		cfg := config.New()
		cfg.GenericOauth.ClientID = "client-disconnect-1"
		cfg.GenericOauth.AllowDisconnect = false
		app := &App{db: ds, cfg: cfg}

		res, err := ds.Exec(
			"INSERT INTO users (username, password, email, created) VALUES (?, ?, ?, NOW())",
			"disconnectme", "x", nil)
		require.NoError(t, err)
		uid, err := res.LastInsertId()
		require.NoError(t, err)
		require.NoError(t, ds.RecordRemoteUserID(context.Background(), uid, "remote-disconnect-1", "generic", cfg.GenericOauth.ClientID, "tok"))

		form := url.Values{
			"provider":       {"generic"},
			"client_id":      {cfg.GenericOauth.ClientID},
			"remote_user_id": {"remote-disconnect-1"},
		}
		r := httptest.NewRequest("POST", "/api/me/oauth/remove", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()

		err = removeOauth(app, &User{ID: uid, Username: "disconnectme"}, w, r)
		require.Error(t, err, "removeOauth must refuse when AllowDisconnect is false")
		httpErr, ok := err.(impart.HTTPError)
		require.True(t, ok, "expected impart.HTTPError, got %T", err)
		assert.Equal(t, http.StatusForbidden, httpErr.Status)

		var n int
		require.NoError(t, ds.QueryRow(
			"SELECT COUNT(*) FROM oauth_users WHERE user_id = ? AND provider = 'generic' AND remote_user_id = ?",
			uid, "remote-disconnect-1").Scan(&n))
		assert.Equal(t, 1, n, "the oauth_users link must NOT be removed when disconnect is disallowed")
	})
}

// TestRemoveOauthRejectsNonCanonicalProviderStrings is the third round on
// this bug. The first fix had no server-side AllowDisconnect check at all.
// The second added one, but compared the raw form value with Go's
// case-sensitive ==, so provider=Generic (mixed case) slipped past the check
// while oauth_users.provider's MariaDB collation (utf8mb4_uca1400_ai_ci,
// case-/pad-insensitive) still matched it for the DELETE. The third fix
// (strings.ToLower + TrimSpace) closed that specific gap but was itself
// incomplete: the collation is ALSO accent- and full-width-insensitive, and
// an adversarial review demonstrated three live bypasses the normalized
// comparison still missed -- provider=generíc, genërìc, and full-width
// ｇｅｎｅｒｉｃ all failed the normalized "== generic" check yet still matched
// the collation, so the link was deleted anyway.
//
// This exercises all five bypass strings the last three rounds either missed
// or (for the first two, mixed-case/padded) previously fixed via the
// AllowDisconnect gate rather than up front. Under the current fix
// (isKnownOauthProvider, oauth_preauth.go) every one of them is now an exact
// mismatch against the canonical provider allowlist, so all five are rejected
// with 400 Bad Request BEFORE the AllowDisconnect gate or the RemoveOauth
// database call ever run -- proven here by AllowDisconnect being left at its
// zero value (false) but the rejection still coming back 400, not 403, and by
// the link surviving in the database.
func TestRemoveOauthRejectsNonCanonicalProviderStrings(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}

	cases := []struct {
		name     string
		provider string
	}{
		{"mixed_case", "Generic"},
		{"padded", " generic "},
		{"accent_i_acute", "generíc"},
		{"accent_e_diaeresis_i_grave", "genërìc"},
		{"fullwidth", "ｇｅｎｅｒｉｃ"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTestDB(t, func(db *sql.DB) {
				ds := &datastore{DB: db, driverName: driverMySQL}

				cfg := config.New()
				cfg.GenericOauth.ClientID = "client-disconnect-bypass-" + tc.name
				// Deliberately the PERMISSIVE setting. If the allowlist check
				// were accidentally skipped or short-circuited, the request
				// would fall through to the AllowDisconnect gate, which would
				// let it through (since it's true) rather than fail loudly --
				// this setting means a passing test can only mean the
				// allowlist itself did the rejecting, not that some other
				// gate happened to also catch it.
				cfg.GenericOauth.AllowDisconnect = true
				app := &App{db: ds, cfg: cfg}

				res, err := ds.Exec(
					"INSERT INTO users (username, password, email, created) VALUES (?, ?, ?, NOW())",
					"disconnect-"+tc.name, "x", nil)
				require.NoError(t, err)
				uid, err := res.LastInsertId()
				require.NoError(t, err)
				require.NoError(t, ds.RecordRemoteUserID(context.Background(), uid, "remote-"+tc.name, "generic", cfg.GenericOauth.ClientID, "tok"))

				form := url.Values{
					"provider":       {tc.provider},
					"client_id":      {cfg.GenericOauth.ClientID},
					"remote_user_id": {"remote-" + tc.name},
				}
				r := httptest.NewRequest("POST", "/api/me/oauth/remove", strings.NewReader(form.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				w := httptest.NewRecorder()

				err = removeOauth(app, &User{ID: uid, Username: "disconnect-" + tc.name}, w, r)
				require.Error(t, err, "removeOauth must reject a non-canonical provider value even when AllowDisconnect is true")
				httpErr, ok := err.(impart.HTTPError)
				require.True(t, ok, "expected impart.HTTPError, got %T", err)
				assert.Equal(t, http.StatusBadRequest, httpErr.Status,
					"a non-canonical provider value must be rejected by the allowlist (400), not fall through to the AllowDisconnect gate (403) or succeed (302)")

				var n int
				require.NoError(t, ds.QueryRow(
					"SELECT COUNT(*) FROM oauth_users WHERE user_id = ? AND provider = 'generic' AND remote_user_id = ?",
					uid, "remote-"+tc.name).Scan(&n))
				assert.Equal(t, 1, n, "the oauth_users link must NOT be removed via a non-canonical provider value bypass")
			})
		})
	}
}

// TestRemoveOauthAllowsGenericDisconnectWhenAllowed is the control case: the
// handler must still actually work end to end when config explicitly permits
// disconnecting the generic provider -- the fix must not have turned this
// into a permanent no-op.
func TestRemoveOauthAllowsGenericDisconnectWhenAllowed(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}

		cfg := config.New()
		cfg.GenericOauth.ClientID = "client-disconnect-2"
		cfg.GenericOauth.AllowDisconnect = true
		app := &App{db: ds, cfg: cfg}

		res, err := ds.Exec(
			"INSERT INTO users (username, password, email, created) VALUES (?, ?, ?, NOW())",
			"disconnectme2", "x", nil)
		require.NoError(t, err)
		uid, err := res.LastInsertId()
		require.NoError(t, err)
		require.NoError(t, ds.RecordRemoteUserID(context.Background(), uid, "remote-disconnect-2", "generic", cfg.GenericOauth.ClientID, "tok"))

		form := url.Values{
			"provider":       {"generic"},
			"client_id":      {cfg.GenericOauth.ClientID},
			"remote_user_id": {"remote-disconnect-2"},
		}
		r := httptest.NewRequest("POST", "/api/me/oauth/remove", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()

		err = removeOauth(app, &User{ID: uid, Username: "disconnectme2"}, w, r)
		// removeOauth always returns an impart.HTTPError, even on success --
		// a 302 redirect back to /me/settings.
		require.Error(t, err)
		httpErr, ok := err.(impart.HTTPError)
		require.True(t, ok, "expected impart.HTTPError, got %T", err)
		assert.Equal(t, http.StatusFound, httpErr.Status)

		var n int
		require.NoError(t, ds.QueryRow(
			"SELECT COUNT(*) FROM oauth_users WHERE user_id = ? AND provider = 'generic' AND remote_user_id = ?",
			uid, "remote-disconnect-2").Scan(&n))
		assert.Equal(t, 0, n, "the oauth_users link must be removed when disconnect is allowed")
	})
}

// TestRemoveOauthNonGenericProviderIgnoresAllowDisconnect matches the
// template's own gating exactly: viewSettings (account.go) only ever sets
// AllowDisconnect for the "generic" provider's oauthAccountInfo entries; the
// other providers (slack, write.as, gitlab, gitea) get an unconditional
// remove button in templates/user/settings.tmpl regardless of this config
// value, because they don't carry the same "permanently unrecoverable JIT
// account" risk this fork introduced. The handler-level fix must match that
// scope exactly, not gate every provider on a Generic-specific setting.
func TestRemoveOauthNonGenericProviderIgnoresAllowDisconnect(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}

		cfg := config.New()
		cfg.GenericOauth.AllowDisconnect = false // must not matter for "slack"
		app := &App{db: ds, cfg: cfg}

		res, err := ds.Exec(
			"INSERT INTO users (username, password, email, created) VALUES (?, ?, ?, NOW())",
			"slackuser", "x", nil)
		require.NoError(t, err)
		uid, err := res.LastInsertId()
		require.NoError(t, err)
		require.NoError(t, ds.RecordRemoteUserID(context.Background(), uid, "remote-slack-1", "slack", "slack-client", "tok"))

		form := url.Values{
			"provider":       {"slack"},
			"client_id":      {"slack-client"},
			"remote_user_id": {"remote-slack-1"},
		}
		r := httptest.NewRequest("POST", "/api/me/oauth/remove", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()

		err = removeOauth(app, &User{ID: uid, Username: "slackuser"}, w, r)
		require.Error(t, err)
		httpErr, ok := err.(impart.HTTPError)
		require.True(t, ok, "expected impart.HTTPError, got %T", err)
		assert.Equal(t, http.StatusFound, httpErr.Status,
			"non-generic providers must not be gated by GenericOauth.AllowDisconnect")

		var n int
		require.NoError(t, ds.QueryRow(
			"SELECT COUNT(*) FROM oauth_users WHERE user_id = ? AND provider = 'slack' AND remote_user_id = ?",
			uid, "remote-slack-1").Scan(&n))
		assert.Equal(t, 0, n)
	})
}
