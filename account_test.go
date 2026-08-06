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
