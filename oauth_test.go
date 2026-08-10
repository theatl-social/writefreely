/*
 * Copyright © 2019-2021 Musing Studio LLC.
 *
 * This file is part of WriteFreely.
 *
 * WriteFreely is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License, included
 * in the LICENSE file in this source code package.
 */

package writefreely

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/writeas/impart"
	"github.com/writeas/web-core/id"
	"github.com/writefreely/writefreely/config"
	"github.com/writefreely/writefreely/key"
)

type MockOAuthDatastoreProvider struct {
	DoDB           func() OAuthDatastore
	DoConfig       func() *config.Config
	DoSessionStore func() sessions.Store
}

type MockOAuthDatastore struct {
	DoGenerateOAuthState func(context.Context, string, string, int64, string) (string, error)
	DoValidateOAuthState func(context.Context, string) (string, string, int64, string, error)
	DoGetIDForRemoteUser func(context.Context, string, string, string) (int64, error)
	DoCreateUser         func(*config.Config, *User, string) error
	DoRecordRemoteUserID func(context.Context, int64, string, string, string, string) error
	DoGetUserByID        func(int64) (*User, error)
}

var _ OAuthDatastore = &MockOAuthDatastore{}

type StringReadCloser struct {
	*strings.Reader
}

func (src *StringReadCloser) Close() error {
	return nil
}

type MockHTTPClient struct {
	DoDo func(req *http.Request) (*http.Response, error)
}

func (m *MockHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if m.DoDo != nil {
		return m.DoDo(req)
	}
	return &http.Response{}, nil
}

func (m *MockOAuthDatastoreProvider) SessionStore() sessions.Store {
	if m.DoSessionStore != nil {
		return m.DoSessionStore()
	}
	return sessions.NewCookieStore([]byte("secret-key"))
}

func (m *MockOAuthDatastoreProvider) DB() OAuthDatastore {
	if m.DoDB != nil {
		return m.DoDB()
	}
	return &MockOAuthDatastore{}
}

func (m *MockOAuthDatastoreProvider) Config() *config.Config {
	if m.DoConfig != nil {
		return m.DoConfig()
	}
	cfg := config.New()
	cfg.UseSQLite(true)
	cfg.WriteAsOauth = config.WriteAsOauthCfg{
		ClientID:        "development",
		ClientSecret:    "development",
		AuthLocation:    "https://write.as/oauth/login",
		TokenLocation:   "https://write.as/oauth/token",
		InspectLocation: "https://write.as/oauth/inspect",
	}
	cfg.SlackOauth = config.SlackOauthCfg{
		ClientID:     "development",
		ClientSecret: "development",
		TeamID:       "development",
	}
	return cfg
}

func (m *MockOAuthDatastore) ValidateOAuthState(ctx context.Context, state string) (string, string, int64, string, error) {
	if m.DoValidateOAuthState != nil {
		return m.DoValidateOAuthState(ctx, state)
	}
	return "", "", 0, "", nil
}

func (m *MockOAuthDatastore) GetIDForRemoteUser(ctx context.Context, remoteUserID, provider, clientID string) (int64, error) {
	if m.DoGetIDForRemoteUser != nil {
		return m.DoGetIDForRemoteUser(ctx, remoteUserID, provider, clientID)
	}
	return -1, nil
}

func (m *MockOAuthDatastore) CreateUser(cfg *config.Config, u *User, username, description string) error {
	if m.DoCreateUser != nil {
		return m.DoCreateUser(cfg, u, username)
	}
	u.ID = 1
	return nil
}

func (m *MockOAuthDatastore) RecordRemoteUserID(ctx context.Context, localUserID int64, remoteUserID, provider, clientID, accessToken string) error {
	if m.DoRecordRemoteUserID != nil {
		return m.DoRecordRemoteUserID(ctx, localUserID, remoteUserID, provider, clientID, accessToken)
	}
	return nil
}

func (m *MockOAuthDatastore) GetUserByID(userID int64) (*User, error) {
	if m.DoGetUserByID != nil {
		return m.DoGetUserByID(userID)
	}
	user := &User{}
	return user, nil
}

func (m *MockOAuthDatastore) GenerateOAuthState(ctx context.Context, provider string, clientID string, attachUserID int64, inviteCode string) (string, error) {
	if m.DoGenerateOAuthState != nil {
		return m.DoGenerateOAuthState(ctx, provider, clientID, attachUserID, inviteCode)
	}
	return id.Generate62RandomString(14), nil
}

func TestViewOauthInit(t *testing.T) {

	t.Run("success", func(t *testing.T) {
		app := &MockOAuthDatastoreProvider{}
		h := oauthHandler{
			Config:   app.Config(),
			DB:       app.DB(),
			Store:    app.SessionStore(),
			EmailKey: []byte{0xd, 0xe, 0xc, 0xa, 0xf, 0xf, 0xb, 0xa, 0xd},
			oauthClient: writeAsOauthClient{
				ClientID:         app.Config().WriteAsOauth.ClientID,
				ClientSecret:     app.Config().WriteAsOauth.ClientSecret,
				ExchangeLocation: app.Config().WriteAsOauth.TokenLocation,
				InspectLocation:  app.Config().WriteAsOauth.InspectLocation,
				AuthLocation:     app.Config().WriteAsOauth.AuthLocation,
				CallbackLocation: "http://localhost/oauth/callback",
				HttpClient:       nil,
			},
		}
		req, err := http.NewRequest("GET", "/oauth/client", nil)
		assert.NoError(t, err)
		rr := httptest.NewRecorder()
		err = h.viewOauthInit(nil, rr, req)
		assert.NotNil(t, err)
		httpErr, ok := err.(impart.HTTPError)
		assert.True(t, ok)
		assert.Equal(t, http.StatusTemporaryRedirect, httpErr.Status)
		assert.NotEmpty(t, httpErr.Message)
		locURI, err := url.Parse(httpErr.Message)
		assert.NoError(t, err)
		assert.Equal(t, "/oauth/login", locURI.Path)
		assert.Equal(t, "development", locURI.Query().Get("client_id"))
		assert.Equal(t, "http://localhost/oauth/callback", locURI.Query().Get("redirect_uri"))
		assert.Equal(t, "code", locURI.Query().Get("response_type"))
		assert.NotEmpty(t, locURI.Query().Get("state"))
	})

	t.Run("state failure", func(t *testing.T) {
		app := &MockOAuthDatastoreProvider{
			DoDB: func() OAuthDatastore {
				return &MockOAuthDatastore{
					DoGenerateOAuthState: func(ctx context.Context, provider, clientID string, attachUserID int64, inviteCode string) (string, error) {
						return "", fmt.Errorf("pretend unable to write state error")
					},
				}
			},
		}
		h := oauthHandler{
			Config:   app.Config(),
			DB:       app.DB(),
			Store:    app.SessionStore(),
			EmailKey: []byte{0xd, 0xe, 0xc, 0xa, 0xf, 0xf, 0xb, 0xa, 0xd},
			oauthClient: writeAsOauthClient{
				ClientID:         app.Config().WriteAsOauth.ClientID,
				ClientSecret:     app.Config().WriteAsOauth.ClientSecret,
				ExchangeLocation: app.Config().WriteAsOauth.TokenLocation,
				InspectLocation:  app.Config().WriteAsOauth.InspectLocation,
				AuthLocation:     app.Config().WriteAsOauth.AuthLocation,
				CallbackLocation: "http://localhost/oauth/callback",
				HttpClient:       nil,
			},
		}
		req, err := http.NewRequest("GET", "/oauth/client", nil)
		assert.NoError(t, err)
		rr := httptest.NewRecorder()
		err = h.viewOauthInit(nil, rr, req)
		httpErr, ok := err.(impart.HTTPError)
		assert.True(t, ok)
		assert.NotEmpty(t, httpErr.Message)
		assert.Equal(t, http.StatusInternalServerError, httpErr.Status)
		assert.Equal(t, "could not prepare oauth redirect url", httpErr.Message)
	})
}

func TestViewOauthCallback(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		// theATL fork: this upstream subtest is stale, not just skipped in CI
		// for a benign reason. It predates the fork's JIT branch in
		// viewOauthCallback (oauth.go), which now runs unconditionally BEFORE
		// the registration-blocked branch this subtest was written to reach,
		// and calls app.db.GetOauthPreauth via the concrete *datastore -- a
		// call this subtest's app (built below with a nil db field) cannot
		// serve, so it panics (nil pointer dereference) rather than returning
		// the redirect the assertions below expect. That panic aborts the
		// entire package test binary, silently taking every other test in it
		// down too -- including this fork's two JIT/security regression tests
		// -- which is exactly the failure mode CI's `-skip` flag was masking.
		//
		// A real *datastore doesn't rescue this subtest either: with no
		// oauth_preauth row for this identity, the JIT branch now correctly
		// returns 403 Forbidden, not the 307 redirect this subtest asserts --
		// upstream's assumption that an unrecognized OAuth identity falls
		// through to open registration no longer holds in this fork by
		// design. See TestViewOauthCallbackJITProvisioning for coverage of
		// the actual current behavior, and FORK.md for the JIT design.
		//
		// Skipping in-code (rather than relying solely on ci.yml's `-skip`
		// flag) so a future maintainer who removes that flag -- reasonably,
		// since its documented rationale no longer describes reality -- can't
		// silently reintroduce the panic and disable the JIT tests with it.
		t.Skip("stale upstream subtest: superseded by the fork's unconditional JIT gate in oauth.go; see comment above and TestViewOauthCallbackJITProvisioning")

		app := &MockOAuthDatastoreProvider{}
		h := oauthHandler{
			Config:   app.Config(),
			DB:       app.DB(),
			Store:    app.SessionStore(),
			EmailKey: []byte{0xd, 0xe, 0xc, 0xa, 0xf, 0xf, 0xb, 0xa, 0xd},
			oauthClient: writeAsOauthClient{
				ClientID:         app.Config().WriteAsOauth.ClientID,
				ClientSecret:     app.Config().WriteAsOauth.ClientSecret,
				ExchangeLocation: app.Config().WriteAsOauth.TokenLocation,
				InspectLocation:  app.Config().WriteAsOauth.InspectLocation,
				AuthLocation:     app.Config().WriteAsOauth.AuthLocation,
				CallbackLocation: "http://localhost/oauth/callback",
				HttpClient: &MockHTTPClient{
					DoDo: func(req *http.Request) (*http.Response, error) {
						switch req.URL.String() {
						case "https://write.as/oauth/token":
							return &http.Response{
								StatusCode: 200,
								Body:       &StringReadCloser{strings.NewReader(`{"access_token": "access_token", "expires_in": 1000, "refresh_token": "refresh_token", "token_type": "access"}`)},
							}, nil
						case "https://write.as/oauth/inspect":
							return &http.Response{
								StatusCode: 200,
								Body:       &StringReadCloser{strings.NewReader(`{"client_id": "development", "user_id": "1", "expires_at": "2019-12-19T11:42:01Z", "username": "nick", "email": "nick@testing.write.as"}`)},
							}, nil
						}

						return &http.Response{
							StatusCode: http.StatusNotFound,
						}, nil
					},
				},
			},
		}
		req, err := http.NewRequest("GET", "/oauth/callback", nil)
		assert.NoError(t, err)
		rr := httptest.NewRecorder()
		err = h.viewOauthCallback(&App{cfg: app.Config(), sessionStore: app.SessionStore()}, rr, req)
		assert.NoError(t, err)
		assert.Equal(t, http.StatusTemporaryRedirect, rr.Code)
	})
}

// TestViewOauthCallbackJITProvisioning covers the theATL fork's just-in-time
// provisioning branch in viewOauthCallback (oauth.go): a pending
// oauth_preauth row, pushed in advance by the member site over the internal
// network, is what allows an account to be created from a real OAuth login
// — never the reverse.
//
// This cannot reuse MockOAuthDatastore the way TestViewOauthCallback above
// does: GetOauthPreauth, SetUserMaxBlogs, DeleteOauthPreauth, and
// GetUserForAuth are defined only on the concrete *datastore (oauth_preauth.go,
// database.go), not on the oauthHandler.DB field's OAuthDatastore interface
// (oauth.go) — MockOAuthDatastore implements exactly that interface and
// nothing more. The JIT branch reaches those methods via the *App parameter
// (app.db), the same way the pre-existing GetUserInvite call a few lines
// below it already does. So this test needs a real database, following the
// withTestDB/runMySQLTests pattern used throughout maxblogs_test.go and
// oauth_preauth_test.go.
func TestViewOauthCallbackJITProvisioning(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		assert.NoError(t, ds.ensureMaxBlogsColumn())
		assert.NoError(t, ds.ensureOauthPreauthTable())

		cfg := config.New()
		// Deliberately true in every subtest below: the oauth_preauth gate must
		// hold regardless of this flag. oauth_preauth is meant to be the ONLY
		// door for OAuth-provisioned accounts — open registration must never
		// become a second one just because it happens to be turned on.
		cfg.App.OpenRegistration = true

		store := sessions.NewCookieStore([]byte("secret-key"))
		app := &App{db: ds, cfg: cfg, sessionStore: store, oauthLockDB: newTestOauthLockDB(t, db)}

		// mockRoundTrip stands in for the real Mastodon /oauth/token and
		// /oauth/inspect endpoints, following the same MockHTTPClient approach
		// TestViewOauthCallback/success uses above.
		mockRoundTrip := func(remoteUserID, username string) *MockHTTPClient {
			return &MockHTTPClient{
				DoDo: func(req *http.Request) (*http.Response, error) {
					switch req.URL.String() {
					case "https://write.as/oauth/token":
						return &http.Response{
							StatusCode: 200,
							Body: &StringReadCloser{strings.NewReader(
								`{"access_token": "access-token", "expires_in": 1000, "refresh_token": "refresh-token", "token_type": "access"}`)},
						}, nil
					case "https://write.as/oauth/inspect":
						return &http.Response{
							StatusCode: 200,
							Body: &StringReadCloser{strings.NewReader(fmt.Sprintf(
								`{"client_id": "generic", "user_id": %q, "expires_at": "2030-01-01T00:00:00Z", "username": %q, "email": "x@example.com"}`,
								remoteUserID, username))},
						}, nil
					}
					return &http.Response{StatusCode: http.StatusNotFound}, nil
				},
			}
		}

		newHandler := func(clientID string, httpClient *MockHTTPClient) oauthHandler {
			return oauthHandler{
				Config: cfg,
				DB:     ds,
				Store:  store,
				oauthClient: writeAsOauthClient{
					ClientID:         clientID,
					ClientSecret:     "development",
					ExchangeLocation: "https://write.as/oauth/token",
					InspectLocation:  "https://write.as/oauth/inspect",
					AuthLocation:     "https://write.as/oauth/login",
					CallbackLocation: "http://localhost/oauth/callback",
					HttpClient:       httpClient,
				},
			}
		}

		callbackRequest := func(t *testing.T, state string) (*http.Request, *httptest.ResponseRecorder) {
			q := url.Values{"code": {"test-code"}, "state": {state}}
			req, err := http.NewRequest("GET", "/oauth/callback/generic?"+q.Encode(), nil)
			assert.NoError(t, err)
			return req, httptest.NewRecorder()
		}

		t.Run("preauth row present: creates, links, sets limit, consumes row, logs in", func(t *testing.T) {
			const remoteUserID = "jit-11111"
			const provider = "generic"
			const clientID = "client-jit-1"

			assert.NoError(t, ds.UpsertOauthPreauth(remoteUserID, provider, clientID, 3))

			state, err := ds.GenerateOAuthState(context.Background(), provider, clientID, 0, "")
			assert.NoError(t, err)

			h := newHandler(clientID, mockRoundTrip(remoteUserID, "jitsmith"))
			req, rr := callbackRequest(t, state)

			err = h.viewOauthCallback(app, rr, req)
			assert.NoError(t, err)
			assert.Equal(t, http.StatusTemporaryRedirect, rr.Code)
			assert.Equal(t, "/", rr.Result().Header.Get("Location"))

			var sawSessionCookie bool
			for _, c := range rr.Result().Cookies() {
				if c.Name == cookieName {
					sawSessionCookie = true
				}
			}
			assert.True(t, sawSessionCookie, "expected a session cookie to be set on JIT login")

			localUserID, err := ds.GetIDForRemoteUser(context.Background(), remoteUserID, provider, clientID)
			assert.NoError(t, err)
			assert.NotEqual(t, int64(-1), localUserID, "oauth_users row should link the newly created account")

			// require, not assert: GetUserByID can return a nil *User on
			// error, and user.Username below would panic the whole test
			// binary on a nil pointer dereference if an assert-only failure
			// let execution continue past a failed lookup.
			user, err := ds.GetUserByID(localUserID)
			require.NoError(t, err)
			assert.Equal(t, "jitsmith", user.Username)

			var maxBlogs sql.NullInt64
			assert.NoError(t, ds.QueryRow("SELECT max_blogs FROM users WHERE id = ?", localUserID).Scan(&maxBlogs))
			assert.True(t, maxBlogs.Valid)
			assert.Equal(t, int64(3), maxBlogs.Int64)

			_, found, err := ds.GetOauthPreauth(remoteUserID, provider, clientID)
			assert.NoError(t, err)
			assert.False(t, found, "preauth row must be consumed (deleted) once it provisions an account")
		})

		t.Run("no preauth row: refuses cleanly, creates nothing, never reaches signup", func(t *testing.T) {
			const remoteUserID = "jit-22222"
			const provider = "generic"
			const clientID = "client-jit-2"

			var usersBefore int
			assert.NoError(t, ds.QueryRow("SELECT COUNT(*) FROM users").Scan(&usersBefore))

			state, err := ds.GenerateOAuthState(context.Background(), provider, clientID, 0, "")
			assert.NoError(t, err)

			h := newHandler(clientID, mockRoundTrip(remoteUserID, "nobody"))
			req, rr := callbackRequest(t, state)

			err = h.viewOauthCallback(app, rr, req)
			assert.Error(t, err)
			httpErr, ok := err.(impart.HTTPError)
			assert.True(t, ok, "expected impart.HTTPError, got %T", err)
			assert.Equal(t, http.StatusForbidden, httpErr.Status)
			assert.NotEmpty(t, httpErr.Message)

			localUserID, err := ds.GetIDForRemoteUser(context.Background(), remoteUserID, provider, clientID)
			assert.NoError(t, err)
			assert.Equal(t, int64(-1), localUserID, "no oauth_users row may be created for an ineligible identity")

			var usersAfter int
			assert.NoError(t, ds.QueryRow("SELECT COUNT(*) FROM users").Scan(&usersAfter))
			assert.Equal(t, usersBefore, usersAfter, "no user row may be created for an ineligible identity")

			var n int
			assert.NoError(t, ds.QueryRow("SELECT COUNT(*) FROM users WHERE username = ?", "nobody").Scan(&n))
			assert.Equal(t, 0, n, "the manual-signup username must never land in users — that page must not run")
		})

		t.Run("already linked: existing login branch runs, JIT does not re-run, preauth row left untouched", func(t *testing.T) {
			const remoteUserID = "jit-33333"
			const provider = "generic"
			const clientID = "client-jit-3"

			res, err := ds.Exec(
				"INSERT INTO users (username, password, email, created) VALUES (?, ?, ?, NOW())",
				"alreadylinked", "x", nil)
			assert.NoError(t, err)
			uid, err := res.LastInsertId()
			assert.NoError(t, err)
			assert.NoError(t, ds.RecordRemoteUserID(context.Background(), uid, remoteUserID, provider, clientID, "old-token"))

			// A stale preauth row surviving a race — see the comment in
			// oauth_preauth.go's handleSetMastodonUserMaxBlogs on the defensive
			// cleanup delete. This branch must not touch it a second time.
			assert.NoError(t, ds.UpsertOauthPreauth(remoteUserID, provider, clientID, 9))

			state, err := ds.GenerateOAuthState(context.Background(), provider, clientID, 0, "")
			assert.NoError(t, err)

			h := newHandler(clientID, mockRoundTrip(remoteUserID, "alreadylinked"))
			req, rr := callbackRequest(t, state)

			err = h.viewOauthCallback(app, rr, req)
			assert.NoError(t, err)
			assert.Equal(t, http.StatusTemporaryRedirect, rr.Code)

			// The existing-user login branch ran, not JIT: max_blogs must remain
			// untouched (still unset from the raw INSERT above).
			var maxBlogs sql.NullInt64
			assert.NoError(t, ds.QueryRow("SELECT max_blogs FROM users WHERE id = ?", uid).Scan(&maxBlogs))
			assert.False(t, maxBlogs.Valid, "the existing-login branch must not touch max_blogs")

			limit, found, err := ds.GetOauthPreauth(remoteUserID, provider, clientID)
			assert.NoError(t, err)
			assert.True(t, found, "the preauth row must survive untouched — JIT logic must not run a second time")
			assert.Equal(t, 9, limit)
		})

		t.Run("username collides with an existing collection alias: suffixed fallback used, no lockout", func(t *testing.T) {
			const remoteUserID = "jit-44444"
			const provider = "generic"
			const clientID = "client-jit-4"

			// A pre-existing, unrelated user owns a blog (collection) whose
			// alias is the exact string this Mastodon identity's username will
			// normalize to. No user is named "colliduser" -- only a collection
			// alias is taken -- so a taken() check that only queries
			// users.username (as originally written) would report this as
			// available. CreateUser would then fail on the collections.alias
			// uniqueness constraint (database.go's INSERT INTO collections),
			// and because taken() had already said "available", every retry
			// would hit the identical collision and this identity could never
			// be provisioned.
			res, err := ds.Exec(
				"INSERT INTO users (username, password, email, created) VALUES (?, ?, ?, NOW())",
				"collowner", "x", nil)
			assert.NoError(t, err)
			ownerID, err := res.LastInsertId()
			assert.NoError(t, err)
			_, err = ds.Exec(
				"INSERT INTO collections (alias, title, description, privacy, owner_id, view_count) VALUES (?, ?, '', 1, ?, 0)",
				"colliduser", "colliduser", ownerID)
			assert.NoError(t, err)

			assert.NoError(t, ds.UpsertOauthPreauth(remoteUserID, provider, clientID, 5))

			state, err := ds.GenerateOAuthState(context.Background(), provider, clientID, 0, "")
			assert.NoError(t, err)

			h := newHandler(clientID, mockRoundTrip(remoteUserID, "colliduser"))
			req, rr := callbackRequest(t, state)

			err = h.viewOauthCallback(app, rr, req)
			assert.NoError(t, err)
			assert.Equal(t, http.StatusTemporaryRedirect, rr.Code,
				"provisioning must still succeed via the suffixed fallback, not fail or lock out")

			localUserID, err := ds.GetIDForRemoteUser(context.Background(), remoteUserID, provider, clientID)
			require.NoError(t, err)
			require.NotEqual(t, int64(-1), localUserID)

			// require, not assert: GetUserByID can return a nil *User on
			// error, and user.Username below would panic the whole test
			// binary on a nil pointer dereference if an assert-only failure
			// let execution continue past a failed lookup.
			user, err := ds.GetUserByID(localUserID)
			require.NoError(t, err)
			assert.Equal(t, "colliduser-"+remoteUserID, user.Username,
				"the collection-alias collision should have been caught, forcing the ID-suffixed fallback tier")

			_, found, err := ds.GetOauthPreauth(remoteUserID, provider, clientID)
			assert.NoError(t, err)
			assert.False(t, found)
		})

		// The following two subtests cover an adversarial-review finding: the
		// JIT path's taken() closure (oauth.go) originally checked only
		// WriteFreely's uniqueness constraints, never author.IsValidUsername
		// -- the same gate account.go, app.go, collections.go, and
		// database.go all apply to every OTHER account-creation path. That
		// let a Mastodon username of "admin" or "login" get JIT-provisioned
		// verbatim (a reserved, impersonation-prone name on a public
		// instance), and let a too-short Mastodon username fall back to the
		// hardcoded literal "user" -- itself also reserved, and previously
		// accepted unconditionally rather than being suffixed.
		for _, reserved := range []string{"admin", "login"} {
			reserved := reserved
			t.Run(fmt.Sprintf("mastodon username %q is reserved: never assigned verbatim", reserved), func(t *testing.T) {
				remoteUserID := "jit-reserved-" + reserved
				const provider = "generic"
				clientID := "client-jit-reserved-" + reserved

				require.NoError(t, ds.UpsertOauthPreauth(remoteUserID, provider, clientID, 1))

				state, err := ds.GenerateOAuthState(context.Background(), provider, clientID, 0, "")
				require.NoError(t, err)

				h := newHandler(clientID, mockRoundTrip(remoteUserID, reserved))
				req, rr := callbackRequest(t, state)

				err = h.viewOauthCallback(app, rr, req)
				require.NoError(t, err)
				assert.Equal(t, http.StatusTemporaryRedirect, rr.Code)

				localUserID, err := ds.GetIDForRemoteUser(context.Background(), remoteUserID, provider, clientID)
				require.NoError(t, err)
				require.NotEqual(t, int64(-1), localUserID)

				user, err := ds.GetUserByID(localUserID)
				require.NoError(t, err)
				assert.NotEqual(t, reserved, user.Username,
					"a reserved/impersonation-prone name must never be assigned verbatim to a JIT-provisioned account")
				assert.Equal(t, reserved+"-"+remoteUserID, user.Username,
					"taken() must treat a reserved name as occupied, routing it through the same ID-suffixed fallback tier as an ordinary collision")

				var n int
				require.NoError(t, ds.QueryRow("SELECT COUNT(*) FROM users WHERE username = ?", reserved).Scan(&n))
				assert.Equal(t, 0, n, "no user row may ever be named exactly %q", reserved)
			})
		}

		t.Run("short (<3 char) mastodon username: does not land on the reserved word \"user\"", func(t *testing.T) {
			const remoteUserID = "jit-55555"
			const provider = "generic"
			const clientID = "client-jit-5"
			const shortName = "jo" // 2 chars: below author.IsValidUsername's MinUsernameLen floor

			require.NoError(t, ds.UpsertOauthPreauth(remoteUserID, provider, clientID, 1))

			state, err := ds.GenerateOAuthState(context.Background(), provider, clientID, 0, "")
			require.NoError(t, err)

			h := newHandler(clientID, mockRoundTrip(remoteUserID, shortName))
			req, rr := callbackRequest(t, state)

			err = h.viewOauthCallback(app, rr, req)
			require.NoError(t, err)
			assert.Equal(t, http.StatusTemporaryRedirect, rr.Code)

			localUserID, err := ds.GetIDForRemoteUser(context.Background(), remoteUserID, provider, clientID)
			require.NoError(t, err)
			require.NotEqual(t, int64(-1), localUserID)

			user, err := ds.GetUserByID(localUserID)
			require.NoError(t, err)
			assert.NotEqual(t, "user", user.Username,
				"a too-short mastodon username must not fall back to the reserved literal \"user\"")
			assert.Equal(t, shortName+"-"+remoteUserID, user.Username,
				"taken() must treat a too-short base as occupied, routing it through the ID-suffixed fallback tier using the ORIGINAL base, not a hardcoded literal")

			var n int
			require.NoError(t, ds.QueryRow("SELECT COUNT(*) FROM users WHERE username = ?", "user").Scan(&n))
			assert.Equal(t, 0, n, "no user row may ever be named exactly \"user\" as a result of this provisioning")
		})
	})
}

// oauthTestMockRoundTrip stands in for the real Mastodon /oauth/token and
// /oauth/inspect endpoints, shared by the revoke and race tests below --
// same approach as TestViewOauthCallbackJITProvisioning's local closure of
// the same name, factored out because both tests below need it too.
func oauthTestMockRoundTrip(remoteUserID, username string) *MockHTTPClient {
	return &MockHTTPClient{
		DoDo: func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://write.as/oauth/token":
				return &http.Response{
					StatusCode: 200,
					Body: &StringReadCloser{strings.NewReader(
						`{"access_token": "access-token", "expires_in": 1000, "refresh_token": "refresh-token", "token_type": "access"}`)},
				}, nil
			case "https://write.as/oauth/inspect":
				return &http.Response{
					StatusCode: 200,
					Body: &StringReadCloser{strings.NewReader(fmt.Sprintf(
						`{"client_id": "generic", "user_id": %q, "expires_at": "2030-01-01T00:00:00Z", "username": %q, "email": "x@example.com"}`,
						remoteUserID, username))},
				}, nil
			}
			return &http.Response{StatusCode: http.StatusNotFound}, nil
		},
	}
}

// TestOauthPreauthRevokeThenLoginRefuses carries
// TestHandleSetMastodonUserMaxBlogsRevokePending (oauth_preauth_test.go) one
// step further: past the revoke call itself, through an actual OAuth login
// attempt for the now-revoked identity. Before the fix, an unconsumed
// preauth grant had no way to be revoked at all -- it lived forever until a
// login consumed it or a later push overwrote it, so a member who cancelled
// their membership before ever logging in kept a permanently valid grant.
// This proves the fix closes that gap end-to-end: revoke, then attempt to
// actually use the (now-nonexistent) grant, and confirm it's refused exactly
// like an identity that was never preauthorized at all -- 403, no account,
// no oauth_users link.
func TestOauthPreauthRevokeThenLoginRefuses(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		assert.NoError(t, ds.ensureMaxBlogsColumn())
		assert.NoError(t, ds.ensureOauthPreauthTable())

		cfg := config.New()
		cfg.GenericOauth.ClientID = "client-revoke-1"
		store := sessions.NewCookieStore([]byte("secret-key"))
		app := &App{db: ds, cfg: cfg, sessionStore: store, oauthLockDB: newTestOauthLockDB(t, db)}

		router := mux.NewRouter()
		router.HandleFunc("/api/internal/mastodon-user/{remoteUserID}/max-blogs",
			handleSetMastodonUserMaxBlogs(app)).Methods("POST")
		secret := "0123456789abcdef0123456789abcdef"
		t.Setenv("WRITEFREELY_API_SECRET", secret)

		push := func(remoteID, body string) int {
			r := httptest.NewRequest("POST", "/api/internal/mastodon-user/"+remoteID+"/max-blogs",
				strings.NewReader(body))
			r.Header.Set("X-WriteFreely-Secret", secret)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			return w.Code
		}

		const remoteUserID = "revoke-11111"
		const provider = "generic"
		clientID := cfg.GenericOauth.ClientID

		// A membership grant is pushed...
		require.Equal(t, http.StatusOK, push(remoteUserID, `{"max_blogs":15}`))
		_, found, err := ds.GetOauthPreauth(remoteUserID, provider, clientID)
		require.NoError(t, err)
		require.True(t, found, "sanity: the grant must exist before it can be revoked")

		// ...then the membership is cancelled before the member ever logs in.
		require.Equal(t, http.StatusOK, push(remoteUserID, `{"max_blogs":0}`))
		_, found, err = ds.GetOauthPreauth(remoteUserID, provider, clientID)
		require.NoError(t, err)
		require.False(t, found, "sanity: revoke must delete the unconsumed preauth row")

		// A subsequent login attempt for this identity must find no grant and
		// refuse cleanly -- not fall through to any signup path, and not
		// create an account.
		state, err := ds.GenerateOAuthState(context.Background(), provider, clientID, 0, "")
		require.NoError(t, err)

		h := oauthHandler{
			Config: cfg,
			DB:     ds,
			Store:  store,
			oauthClient: writeAsOauthClient{
				ClientID:         clientID,
				ClientSecret:     "development",
				ExchangeLocation: "https://write.as/oauth/token",
				InspectLocation:  "https://write.as/oauth/inspect",
				AuthLocation:     "https://write.as/oauth/login",
				CallbackLocation: "http://localhost/oauth/callback",
				HttpClient:       oauthTestMockRoundTrip(remoteUserID, "revokeduser"),
			},
		}
		q := url.Values{"code": {"test-code"}, "state": {state}}
		req, err := http.NewRequest("GET", "/oauth/callback/generic?"+q.Encode(), nil)
		require.NoError(t, err)
		rr := httptest.NewRecorder()

		err = h.viewOauthCallback(app, rr, req)
		require.Error(t, err)
		httpErr, ok := err.(impart.HTTPError)
		require.True(t, ok, "expected impart.HTTPError, got %T", err)
		assert.Equal(t, http.StatusForbidden, httpErr.Status)

		localUserID, err := ds.GetIDForRemoteUser(context.Background(), remoteUserID, provider, clientID)
		require.NoError(t, err)
		assert.Equal(t, int64(-1), localUserID, "no account may be created for a revoked identity")

		var n int
		require.NoError(t, ds.QueryRow("SELECT COUNT(*) FROM users WHERE username = ?", "revokeduser").Scan(&n))
		assert.Equal(t, 0, n)
	})
}

// TestOauthLoginRaceWithMaxBlogsPushIsLinearizable is a best-effort,
// real-timing integration companion to
// TestHandleSetMastodonUserMaxBlogsWaitsForInFlightLogin (oauth_preauth_test.go)
// -- that other test is the primary, deterministic proof of the fix (it does
// not depend on winning any timing race, and reliably fails if the lock is
// removed). This one runs the two real code paths concurrently, unsynchronized
// by the test itself, and checks the same invariants; it is valuable as an
// integration sanity check under real goroutine/DB-driver scheduling, but on
// its own it is NOT sufficient proof -- in local testing, push's read and
// write are two back-to-back local DB calls with almost no gap between them,
// so push usually completes entirely before login gets anywhere near its own
// writes, and this test was observed to pass even with the lock removed
// simply because that particular interleaving never got exercised. Real
// production traffic has login doing a genuinely slow, real HTTPS round trip
// to Mastodon between its own read and write (not mocked-instant like here),
// which is what actually opens the window this whole fix is about.
//
// This test reproduces, end-to-end, the TOCTOU race an adversarial
// whole-branch review found and confirmed against a real database:
// handleSetMastodonUserMaxBlogs (oauth_preauth.go) used to read
// GetIDForRemoteUser and then branch on that read with no synchronization
// against viewOauthCallback's JIT branch (oauth.go) doing the equivalent
// check for the SAME identity. Interleaved, a downgrade push could read "not
// linked" while a concurrent login was mid-flight: the login would consume
// the OLD preauth value and link the account, then the push -- still
// believing the identity unlinked -- would upsert a NEW preauth row carrying
// the downgraded value. Net effect: the push returned 200 OK, but the
// account kept its OLD allowance, AND a preauth row was resurrected for an
// identity that was already provisioned (one no future login would ever
// consume, since JIT only calls GetOauthPreauth once GetIDForRemoteUser
// first comes back unlinked).
//
// The fix (withOauthIdentityLock, oauth_preauth.go) makes the two
// operations mutually exclusive per identity, but does not dictate which one
// wins a given race -- and it doesn't need to. Whichever wins, the combined
// outcome must be equivalent to SOME sequential ordering of the two ("push
// then login" or "login then push"), and both of those orderings converge on
// the exact same two invariants asserted below:
//   - push then login: push upserts a preauth row carrying the new value;
//     login then consumes THAT row, so the account is created with the new
//     value and the row is deleted.
//   - login then push: login consumes the OLD preauth value and links the
//     account; push then finds the identity linked and updates
//     users.max_blogs to the new value directly.
//
// Either way, the account ends up linked, holding the PUSHED value, with no
// preauth row left over. A run that violates either invariant is exactly the
// lost-downgrade/resurrected-grant bug this test exists to catch. This runs
// many trials with a start barrier releasing both goroutines together, so
// real scheduling and network-round-trip jitter (against a real MariaDB) has
// real opportunity to produce both orderings across the run, not just one.
func TestOauthLoginRaceWithMaxBlogsPushIsLinearizable(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		assert.NoError(t, ds.ensureMaxBlogsColumn())
		assert.NoError(t, ds.ensureOauthPreauthTable())

		cfg := config.New()
		cfg.GenericOauth.ClientID = "client-race-1"
		store := sessions.NewCookieStore([]byte("secret-key"))
		app := &App{db: ds, cfg: cfg, sessionStore: store, oauthLockDB: newTestOauthLockDB(t, db)}
		clientID := cfg.GenericOauth.ClientID

		router := mux.NewRouter()
		router.HandleFunc("/api/internal/mastodon-user/{remoteUserID}/max-blogs",
			handleSetMastodonUserMaxBlogs(app)).Methods("POST")
		secret := "0123456789abcdef0123456789abcdef"
		t.Setenv("WRITEFREELY_API_SECRET", secret)

		push := func(remoteID string, maxBlogs int) int {
			r := httptest.NewRequest("POST", "/api/internal/mastodon-user/"+remoteID+"/max-blogs",
				strings.NewReader(fmt.Sprintf(`{"max_blogs":%d}`, maxBlogs)))
			r.Header.Set("X-WriteFreely-Secret", secret)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			return w.Code
		}

		const trials = 15
		const initialMaxBlogs = 15
		const pushedMaxBlogs = 3

		for trial := 0; trial < trials; trial++ {
			remoteUserID := fmt.Sprintf("race-%d", trial)
			username := fmt.Sprintf("racer%d", trial)

			require.NoError(t, ds.UpsertOauthPreauth(remoteUserID, "generic", clientID, initialMaxBlogs))

			state, err := ds.GenerateOAuthState(context.Background(), "generic", clientID, 0, "")
			require.NoError(t, err)

			h := oauthHandler{
				Config: cfg,
				DB:     ds,
				Store:  store,
				oauthClient: writeAsOauthClient{
					ClientID:         clientID,
					ClientSecret:     "development",
					ExchangeLocation: "https://write.as/oauth/token",
					InspectLocation:  "https://write.as/oauth/inspect",
					AuthLocation:     "https://write.as/oauth/login",
					CallbackLocation: "http://localhost/oauth/callback",
					HttpClient:       oauthTestMockRoundTrip(remoteUserID, username),
				},
			}
			q := url.Values{"code": {"test-code"}, "state": {state}}
			req, err := http.NewRequest("GET", "/oauth/callback/generic?"+q.Encode(), nil)
			require.NoError(t, err)
			rr := httptest.NewRecorder()

			var wg sync.WaitGroup
			start := make(chan struct{})
			var loginErr error
			var pushStatus int

			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				loginErr = h.viewOauthCallback(app, rr, req)
			}()
			go func() {
				defer wg.Done()
				<-start
				pushStatus = push(remoteUserID, pushedMaxBlogs)
			}()
			close(start)
			wg.Wait()

			require.NoError(t, loginErr, "trial %d: login must always succeed regardless of race outcome", trial)
			require.Equal(t, http.StatusOK, pushStatus, "trial %d: push must always report success", trial)

			localUserID, err := ds.GetIDForRemoteUser(context.Background(), remoteUserID, "generic", clientID)
			require.NoError(t, err)
			require.NotEqual(t, int64(-1), localUserID, "trial %d: identity must end up linked to an account", trial)

			var gotMaxBlogs sql.NullInt64
			require.NoError(t, ds.QueryRow("SELECT max_blogs FROM users WHERE id = ?", localUserID).Scan(&gotMaxBlogs))
			assert.True(t, gotMaxBlogs.Valid, "trial %d", trial)
			assert.Equal(t, int64(pushedMaxBlogs), gotMaxBlogs.Int64,
				"trial %d: the push's value must never be silently lost, regardless of which operation the race let win -- "+
					"a stale value here is exactly the 'lost downgrade' bug", trial)

			_, found, err := ds.GetOauthPreauth(remoteUserID, "generic", clientID)
			require.NoError(t, err)
			assert.False(t, found,
				"trial %d: no preauth row may survive once the identity is linked -- a surviving row here is exactly "+
					"the 'resurrected grant' bug", trial)
		}
	})
}

// TestOauthSignupRouteIsNotRegistered guards the fix for a critical finding:
// POST /oauth/signup (viewOauthSignup, oauth_signup.go) creates accounts via
// CreateUser -> RecordRemoteUserID -> loginOrFail with NO oauth_preauth check
// and no OpenRegistration check. Its only gate, HashTokenParams, is an HMAC
// keyed on Config.Server.HashSeed -- unset (empty string) anywhere in this
// fork's config, making the signature trivially forgeable by anyone who wants
// to compute sha256("" + their own chosen form values). Before this task, the
// route was dormant because no [oauth.*] provider was ever configured; this
// task's own config change (enabling [oauth.generic] so JIT can work) is what
// would have activated it. The fix removes the route registration entirely
// (oauth.go's configureOauthRoutes) rather than trying to patch
// HashTokenParams, since this route specifically is meant to be closed in
// code, for good: the OAuth path's only door is the oauth_preauth-gated JIT
// branch in viewOauthCallback. This is NOT the same as self-serve signup
// being closed instance-wide -- two other signup routes exist and neither is
// closed by this fork's code: POST /api/auth/signup (routes.go) IS gated by
// open_registration at route-registration time (the handler isn't mounted
// at all when open_registration is false), but POST /auth/signup
// (routes.go) is registered UNCONDITIONALLY, and its in-app check only
// rejects when open_registration is false AND the submitted invite_code is
// empty -- any non-empty invite_code bypasses it, unchecked against the
// database. So open_registration provides no real protection on
// /auth/signup; the only actual gate on that route in this deployment is an
// external HAProxy ACL outside this repo. See FORK.md's "Known limits".
//
// This calls configureOauthRoutes directly against a bare router, rather than
// the real InitRoutes: InitRoutes also registers a catch-all blog-post-reader
// route (template "/{prefix}{collection}/{slug}", no method restriction) that
// happens to structurally match "/oauth/signup" too (as collection="oauth",
// slug="signup") -- confirmed by hand with router.Walk. Matching against the
// full router would make router.Match true regardless of whether the signup
// route itself is registered, for a reason that has nothing to do with this
// fix, and silently prove nothing. Testing configureOauthRoutes in isolation
// checks the actual thing the fix changed.
func TestOauthSignupRouteIsNotRegistered(t *testing.T) {
	cfg := config.New()
	app := &App{cfg: cfg, keys: &key.Keychain{EmailKey: []byte("0123456789abcdef")}}
	handler := NewHandler(app)

	oauthClient := writeAsOauthClient{
		ClientID:         "development",
		ClientSecret:     "development",
		ExchangeLocation: "https://write.as/oauth/token",
		InspectLocation:  "https://write.as/oauth/inspect",
		AuthLocation:     "https://write.as/oauth/login",
		CallbackLocation: "http://localhost/oauth/callback",
	}

	router := mux.NewRouter()
	configureOauthRoutes(handler, router, app, oauthClient, nil)

	// Sanity check: confirm configureOauthRoutes actually registered
	// something, so a "no match" result below means the signup route
	// specifically is absent, not that this test built an empty router.
	initReq := httptest.NewRequest("GET", "/oauth/write.as", nil)
	var initMatch mux.RouteMatch
	assert.True(t, router.Match(initReq, &initMatch),
		"sanity check: GET /oauth/write.as should resolve, confirming configureOauthRoutes ran")

	req := httptest.NewRequest("POST", "/oauth/signup", nil)
	var match mux.RouteMatch
	assert.False(t, router.Match(req, &match),
		"POST /oauth/signup must NOT resolve -- its only gate is forgeable with an empty HashSeed, "+
			"and this fork closes the OAuth self-serve path in code; it is not the app's only "+
			"signup surface (POST /api/auth/signup is gated by open_registration at route-registration "+
			"time; POST /auth/signup is registered unconditionally and is gated only by an external "+
			"HAProxy ACL, not by this app -- see FORK.md)")
}
