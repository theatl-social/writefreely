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
	"testing"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"github.com/stretchr/testify/assert"
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
		app := &App{db: ds, cfg: cfg, sessionStore: store}

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

			user, err := ds.GetUserByID(localUserID)
			assert.NoError(t, err)
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
			assert.NoError(t, err)
			assert.NotEqual(t, int64(-1), localUserID)

			user, err := ds.GetUserByID(localUserID)
			assert.NoError(t, err)
			assert.Equal(t, "colliduser-"+remoteUserID, user.Username,
				"the collection-alias collision should have been caught, forcing the ID-suffixed fallback tier")

			_, found, err := ds.GetOauthPreauth(remoteUserID, provider, clientID)
			assert.NoError(t, err)
			assert.False(t, found)
		})
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
// HashTokenParams, since self-serve signup is permanently closed by design.
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
			"and self-serve account creation is permanently closed by design")
}
