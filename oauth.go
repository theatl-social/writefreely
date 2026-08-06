/*
 * Copyright © 2019-2021 Musing Studio LLC.
 *
 * This file is part of WriteFreely.
 *
 * WriteFreely is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License, included
 * in the LICENSE file in this source code package.
 */

/*
 * Modified 2026 by theATL.social: added just-in-time account provisioning
 * gated on a pre-authorization pushed by the member site. See FORK.md.
 */

package writefreely

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"github.com/writeas/impart"
	"github.com/writeas/web-core/log"
	"github.com/writefreely/writefreely/author"
	"github.com/writefreely/writefreely/config"
)

// OAuthButtons holds display information for different OAuth providers we support.
type OAuthButtons struct {
	SlackEnabled       bool
	WriteAsEnabled     bool
	GitLabEnabled      bool
	GitLabDisplayName  string
	GiteaEnabled       bool
	GiteaDisplayName   string
	GenericEnabled     bool
	GenericDisplayName string
}

// NewOAuthButtons creates a new OAuthButtons struct based on our app configuration.
func NewOAuthButtons(cfg *config.Config) *OAuthButtons {
	return &OAuthButtons{
		SlackEnabled:       cfg.SlackOauth.ClientID != "",
		WriteAsEnabled:     cfg.WriteAsOauth.ClientID != "",
		GitLabEnabled:      cfg.GitlabOauth.ClientID != "",
		GitLabDisplayName:  config.OrDefaultString(cfg.GitlabOauth.DisplayName, gitlabDisplayName),
		GiteaEnabled:       cfg.GiteaOauth.ClientID != "",
		GiteaDisplayName:   config.OrDefaultString(cfg.GiteaOauth.DisplayName, giteaDisplayName),
		GenericEnabled:     cfg.GenericOauth.ClientID != "",
		GenericDisplayName: config.OrDefaultString(cfg.GenericOauth.DisplayName, genericOauthDisplayName),
	}
}

// TokenResponse contains data returned when a token is created either
// through a code exchange or using a refresh token.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
}

// InspectResponse contains data returned when an access token is inspected.
type InspectResponse struct {
	ClientID    string    `json:"client_id"`
	UserID      string    `json:"user_id"`
	ExpiresAt   time.Time `json:"expires_at"`
	Username    string    `json:"username"`
	DisplayName string    `json:"-"`
	Email       string    `json:"email"`
	Error       string    `json:"error"`
}

// tokenRequestMaxLen is the most bytes that we'll read from the /oauth/token
// endpoint. One megabyte is plenty.
const tokenRequestMaxLen = 1000000

// infoRequestMaxLen is the most bytes that we'll read from the
// /oauth/inspect endpoint.
const infoRequestMaxLen = 1000000

// OAuthDatastoreProvider provides a minimal interface of data store, config,
// and session store for use with the oauth handlers.
type OAuthDatastoreProvider interface {
	DB() OAuthDatastore
	Config() *config.Config
	SessionStore() sessions.Store
}

// OAuthDatastore provides a minimal interface of data store methods used in
// oauth functionality.
type OAuthDatastore interface {
	GetIDForRemoteUser(context.Context, string, string, string) (int64, error)
	RecordRemoteUserID(context.Context, int64, string, string, string, string) error
	ValidateOAuthState(context.Context, string) (string, string, int64, string, error)
	GenerateOAuthState(context.Context, string, string, int64, string) (string, error)

	CreateUser(*config.Config, *User, string, string) error
	GetUserByID(int64) (*User, error)
}

type HttpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type oauthClient interface {
	GetProvider() string
	GetClientID() string
	GetCallbackLocation() string
	buildLoginURL(state string) (string, error)
	exchangeOauthCode(ctx context.Context, code string) (*TokenResponse, error)
	inspectOauthAccessToken(ctx context.Context, accessToken string) (*InspectResponse, error)
}

type callbackProxyClient struct {
	server           string
	callbackLocation string
	httpClient       HttpClient
}

type oauthHandler struct {
	Config        *config.Config
	DB            OAuthDatastore
	Store         sessions.Store
	EmailKey      []byte
	oauthClient   oauthClient
	callbackProxy *callbackProxyClient
}

func (h oauthHandler) viewOauthInit(app *App, w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	var attachUser int64
	if attach := r.URL.Query().Get("attach"); attach == "t" {
		user, _ := getUserAndSession(app, r)
		if user == nil {
			return impart.HTTPError{http.StatusInternalServerError, "cannot attach auth to user: user not found in session"}
		}
		attachUser = user.ID
	}

	state, err := h.DB.GenerateOAuthState(ctx, h.oauthClient.GetProvider(), h.oauthClient.GetClientID(), attachUser, r.FormValue("invite_code"))
	if err != nil {
		log.Error("viewOauthInit error: %s", err)
		return impart.HTTPError{http.StatusInternalServerError, "could not prepare oauth redirect url"}
	}

	if h.callbackProxy != nil {
		if err := h.callbackProxy.register(ctx, state); err != nil {
			log.Error("viewOauthInit error: %s", err)
			return impart.HTTPError{http.StatusInternalServerError, "could not register state server"}
		}
	}

	location, err := h.oauthClient.buildLoginURL(state)
	if err != nil {
		log.Error("viewOauthInit error: %s", err)
		return impart.HTTPError{http.StatusInternalServerError, "could not prepare oauth redirect url"}
	}
	return impart.HTTPError{http.StatusTemporaryRedirect, location}
}

func configureSlackOauth(parentHandler *Handler, r *mux.Router, app *App) {
	if app.Config().SlackOauth.ClientID != "" {
		callbackLocation := app.Config().App.Host + "/oauth/callback/slack"

		var stateRegisterClient *callbackProxyClient = nil
		if app.Config().SlackOauth.CallbackProxyAPI != "" {
			stateRegisterClient = &callbackProxyClient{
				server:           app.Config().SlackOauth.CallbackProxyAPI,
				callbackLocation: app.Config().App.Host + "/oauth/callback/slack",
				httpClient:       config.DefaultHTTPClient(),
			}
			callbackLocation = app.Config().SlackOauth.CallbackProxy
		}
		oauthClient := slackOauthClient{
			ClientID:         app.Config().SlackOauth.ClientID,
			ClientSecret:     app.Config().SlackOauth.ClientSecret,
			TeamID:           app.Config().SlackOauth.TeamID,
			HttpClient:       config.DefaultHTTPClient(),
			CallbackLocation: callbackLocation,
		}
		configureOauthRoutes(parentHandler, r, app, oauthClient, stateRegisterClient)
	}
}

func configureWriteAsOauth(parentHandler *Handler, r *mux.Router, app *App) {
	if app.Config().WriteAsOauth.ClientID != "" {
		callbackLocation := app.Config().App.Host + "/oauth/callback/write.as"

		var callbackProxy *callbackProxyClient = nil
		if app.Config().WriteAsOauth.CallbackProxy != "" {
			callbackProxy = &callbackProxyClient{
				server:           app.Config().WriteAsOauth.CallbackProxyAPI,
				callbackLocation: app.Config().App.Host + "/oauth/callback/write.as",
				httpClient:       config.DefaultHTTPClient(),
			}
			callbackLocation = app.Config().WriteAsOauth.CallbackProxy
		}

		oauthClient := writeAsOauthClient{
			ClientID:         app.Config().WriteAsOauth.ClientID,
			ClientSecret:     app.Config().WriteAsOauth.ClientSecret,
			ExchangeLocation: config.OrDefaultString(app.Config().WriteAsOauth.TokenLocation, writeAsExchangeLocation),
			InspectLocation:  config.OrDefaultString(app.Config().WriteAsOauth.InspectLocation, writeAsIdentityLocation),
			AuthLocation:     config.OrDefaultString(app.Config().WriteAsOauth.AuthLocation, writeAsAuthLocation),
			HttpClient:       config.DefaultHTTPClient(),
			CallbackLocation: callbackLocation,
		}
		configureOauthRoutes(parentHandler, r, app, oauthClient, callbackProxy)
	}
}

func configureGitlabOauth(parentHandler *Handler, r *mux.Router, app *App) {
	if app.Config().GitlabOauth.ClientID != "" {
		callbackLocation := app.Config().App.Host + "/oauth/callback/gitlab"

		var callbackProxy *callbackProxyClient = nil
		if app.Config().GitlabOauth.CallbackProxy != "" {
			callbackProxy = &callbackProxyClient{
				server:           app.Config().GitlabOauth.CallbackProxyAPI,
				callbackLocation: app.Config().App.Host + "/oauth/callback/gitlab",
				httpClient:       config.DefaultHTTPClient(),
			}
			callbackLocation = app.Config().GitlabOauth.CallbackProxy
		}

		address := config.OrDefaultString(app.Config().GitlabOauth.Host, gitlabHost)
		oauthClient := gitlabOauthClient{
			ClientID:         app.Config().GitlabOauth.ClientID,
			ClientSecret:     app.Config().GitlabOauth.ClientSecret,
			ExchangeLocation: address + "/oauth/token",
			InspectLocation:  address + "/api/v4/user",
			AuthLocation:     address + "/oauth/authorize",
			HttpClient:       config.DefaultHTTPClient(),
			CallbackLocation: callbackLocation,
		}
		configureOauthRoutes(parentHandler, r, app, oauthClient, callbackProxy)
	}
}

func configureGenericOauth(parentHandler *Handler, r *mux.Router, app *App) {
	if app.Config().GenericOauth.ClientID != "" {
		callbackLocation := app.Config().App.Host + "/oauth/callback/generic"

		var callbackProxy *callbackProxyClient = nil
		if app.Config().GenericOauth.CallbackProxy != "" {
			callbackProxy = &callbackProxyClient{
				server:           app.Config().GenericOauth.CallbackProxyAPI,
				callbackLocation: app.Config().App.Host + "/oauth/callback/generic",
				httpClient:       config.DefaultHTTPClient(),
			}
			callbackLocation = app.Config().GenericOauth.CallbackProxy
		}

		oauthClient := genericOauthClient{
			ClientID:         app.Config().GenericOauth.ClientID,
			ClientSecret:     app.Config().GenericOauth.ClientSecret,
			ExchangeLocation: app.Config().GenericOauth.Host + app.Config().GenericOauth.TokenEndpoint,
			InspectLocation:  app.Config().GenericOauth.Host + app.Config().GenericOauth.InspectEndpoint,
			AuthLocation:     app.Config().GenericOauth.Host + app.Config().GenericOauth.AuthEndpoint,
			HttpClient:       config.DefaultHTTPClient(),
			CallbackLocation: callbackLocation,
			Scope:            config.OrDefaultString(app.Config().GenericOauth.Scope, "read_user"),
			MapUserID:        config.OrDefaultString(app.Config().GenericOauth.MapUserID, "user_id"),
			MapUsername:      config.OrDefaultString(app.Config().GenericOauth.MapUsername, "username"),
			MapDisplayName:   config.OrDefaultString(app.Config().GenericOauth.MapDisplayName, "-"),
			MapEmail:         config.OrDefaultString(app.Config().GenericOauth.MapEmail, "email"),
		}
		configureOauthRoutes(parentHandler, r, app, oauthClient, callbackProxy)
	}
}

func configureGiteaOauth(parentHandler *Handler, r *mux.Router, app *App) {
	if app.Config().GiteaOauth.ClientID != "" {
		callbackLocation := app.Config().App.Host + "/oauth/callback/gitea"

		var callbackProxy *callbackProxyClient = nil
		if app.Config().GiteaOauth.CallbackProxy != "" {
			callbackProxy = &callbackProxyClient{
				server:           app.Config().GiteaOauth.CallbackProxyAPI,
				callbackLocation: app.Config().App.Host + "/oauth/callback/gitea",
				httpClient:       config.DefaultHTTPClient(),
			}
			callbackLocation = app.Config().GiteaOauth.CallbackProxy
		}

		oauthClient := giteaOauthClient{
			ClientID:         app.Config().GiteaOauth.ClientID,
			ClientSecret:     app.Config().GiteaOauth.ClientSecret,
			ExchangeLocation: app.Config().GiteaOauth.Host + "/login/oauth/access_token",
			InspectLocation:  app.Config().GiteaOauth.Host + "/login/oauth/userinfo",
			AuthLocation:     app.Config().GiteaOauth.Host + "/login/oauth/authorize",
			HttpClient:       config.DefaultHTTPClient(),
			CallbackLocation: callbackLocation,
			Scope:            "openid profile email",
			MapUserID:        "sub",
			MapUsername:      "login",
			MapDisplayName:   "full_name",
			MapEmail:         "email",
		}
		configureOauthRoutes(parentHandler, r, app, oauthClient, callbackProxy)
	}
}

func configureOauthRoutes(parentHandler *Handler, r *mux.Router, app *App, oauthClient oauthClient, callbackProxy *callbackProxyClient) {
	handler := &oauthHandler{
		Config:        app.Config(),
		DB:            app.DB(),
		Store:         app.SessionStore(),
		oauthClient:   oauthClient,
		EmailKey:      app.keys.EmailKey,
		callbackProxy: callbackProxy,
	}
	r.HandleFunc("/oauth/"+oauthClient.GetProvider(), parentHandler.OAuth(handler.viewOauthInit)).Methods("GET")
	r.HandleFunc("/oauth/callback/"+oauthClient.GetProvider(), parentHandler.OAuth(handler.viewOauthCallback)).Methods("GET")
	// theATL fork: the manual-signup POST route is deliberately NOT registered.
	// viewOauthSignup's only gate is HashTokenParams, an HMAC keyed on
	// h.Config.Server.HashSeed -- which is unset (empty string) in this fork's
	// config, making that signature trivially forgeable. This route, and only
	// this route, is closed in code: the OAuth path's only door is the
	// oauth_preauth-gated JIT branch in viewOauthCallback below. That is NOT
	// the same as "self-serve signup is closed instance-wide" -- two other
	// signup routes exist and neither is closed by this fork's code:
	//   - POST /api/auth/signup (routes.go) IS gated by open_registration at
	//     route-registration time: the handler isn't even mounted when
	//     open_registration is false.
	//   - POST /auth/signup (routes.go) is registered UNCONDITIONALLY,
	//     regardless of open_registration. Its in-app check
	//     (unregisteredusers.go's handleWebSignup) only rejects when
	//     open_registration is false AND the submitted invite_code is empty
	//     -- any non-empty invite_code bypasses that check, and the code is
	//     never validated against the database before the account is
	//     created (account.go's signupWithRegistration; database.go's
	//     CreateInvitedUser is a bare insert that runs AFTER CreateUser has
	//     already succeeded). So open_registration provides no real
	//     protection on this route; the only actual gate on POST
	//     /auth/signup in this deployment is an external HAProxy ACL that
	//     lives entirely outside this repo. See FORK.md's "Known limits".
	// viewOauthSignup/validateOauthSignup/showOauthSignupPage/HashTokenParams
	// remain in oauth_signup.go, unrouted but still valid Go -- Go does not
	// error on unreachable handler methods.
}

func (h oauthHandler) viewOauthCallback(app *App, w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	code := r.FormValue("code")
	state := r.FormValue("state")

	provider, clientID, attachUserID, inviteCode, err := h.DB.ValidateOAuthState(ctx, state)
	if err != nil {
		log.Error("Unable to ValidateOAuthState: %s", err)
		return impart.HTTPError{http.StatusInternalServerError, err.Error()}
	}

	tokenResponse, err := h.oauthClient.exchangeOauthCode(ctx, code)
	if err != nil {
		log.Error("Unable to exchangeOauthCode: %s", err)
		// TODO: show user friendly message if needed
		// TODO: show NO message for cases like user pressing "Cancel" on authorize step
		addSessionFlash(app, w, r, err.Error(), nil)
		if attachUserID > 0 {
			return impart.HTTPError{http.StatusFound, "/me/settings"}
		}
		return impart.HTTPError{http.StatusInternalServerError, err.Error()}
	}

	// Now that we have the access token, let's use it real quick to make sure
	// it really really works.
	tokenInfo, err := h.oauthClient.inspectOauthAccessToken(ctx, tokenResponse.AccessToken)
	if err != nil {
		log.Error("Unable to inspectOauthAccessToken: %s", err)
		return impart.HTTPError{http.StatusInternalServerError, err.Error()}
	}

	localUserID, err := h.DB.GetIDForRemoteUser(ctx, tokenInfo.UserID, provider, clientID)
	if err != nil {
		log.Error("Unable to GetIDForRemoteUser: %s", err)
		return impart.HTTPError{http.StatusInternalServerError, err.Error()}
	}

	if localUserID != -1 && attachUserID > 0 {
		if err = addSessionFlash(app, w, r, "This OAuth account is already attached to another user.", nil); err != nil {
			return impart.HTTPError{Status: http.StatusInternalServerError, Message: err.Error()}
		}
		return impart.HTTPError{http.StatusFound, "/me/settings"}
	}

	if localUserID != -1 {
		// Existing user, so log in now
		user, err := h.DB.GetUserByID(localUserID)
		if err != nil {
			log.Error("Unable to GetUserByID %d: %s", localUserID, err)
			return impart.HTTPError{http.StatusInternalServerError, err.Error()}
		}
		if err = loginOrFail(h.Store, w, r, user); err != nil {
			log.Error("Unable to loginOrFail %d: %s", localUserID, err)
			return impart.HTTPError{http.StatusInternalServerError, err.Error()}
		}
		return nil
	}
	if attachUserID > 0 {
		log.Info("attaching to user %d", attachUserID)
		log.Info("OAuth userid: %s", tokenInfo.UserID)
		err = h.DB.RecordRemoteUserID(r.Context(), attachUserID, tokenInfo.UserID, provider, clientID, tokenResponse.AccessToken)
		if err != nil {
			return impart.HTTPError{http.StatusInternalServerError, err.Error()}
		}
		return impart.HTTPError{http.StatusFound, "/me/settings"}
	}

	// theATL fork: just-in-time provisioning. If this Mastodon identity has a
	// pending pre-authorization (pushed by the member site in advance), create
	// the account now instead of falling through to the manual signup page.
	// GetOauthPreauth/SetUserMaxBlogs/DeleteOauthPreauth/GetUserForAuth are
	// defined only on the concrete *datastore (oauth_preauth.go, database.go),
	// not on the h.DB field's narrower OAuthDatastore interface above, so they
	// are reached via app.db here -- the same way the existing
	// app.db.GetUserInvite call a few lines below already does. See FORK.md and
	// the design spec's "Revision 2 — OAuth JIT Provisioning".
	maxBlogs, eligible, err := app.db.GetOauthPreauth(tokenInfo.UserID, provider, clientID)
	if err != nil {
		return impart.HTTPError{http.StatusInternalServerError, err.Error()}
	}
	if eligible {
		// CreateUser enforces uniqueness of this string against THREE tables,
		// not just users.username: collections.alias (INSERT INTO collections,
		// database.go ~line 248, rolls back and returns 409 on collision) and
		// posts.id via PostIDExists (database.go ~line 215, checked up front,
		// also 409). taken() must cover all three, or a collision against a
		// collection alias or post ID that GetUserForAuth alone can't see
		// reports "available" when it isn't -- normalizeOauthUsername never
		// tries its suffixed fallback, and every retry then hits the identical
		// collision, locking that member out permanently.
		//
		// taken() must ALSO cover validity, not just uniqueness: every other
		// account-creation path in this codebase (account.go, app.go,
		// collections.go, database.go) gates on author.IsValidUsername, which
		// rejects both too-short names and a reserved-word list ("admin",
		// "login", "user", ...) that nothing in WriteFreely's own uniqueness
		// tables would ever flag as occupied. Without this, the JIT path could
		// hand a real Mastodon member a reserved/impersonation-prone username
		// (e.g. "admin") purely because no WriteFreely account happened to be
		// sitting on it yet. Treating "invalid" the same as "taken" here makes
		// normalizeOauthUsername's existing suffix-fallback tiers route around
		// it automatically -- no separate reserved-word logic needed there.
		username := normalizeOauthUsername(tokenInfo.Username, tokenInfo.UserID, func(u string) bool {
			if !author.IsValidUsername(h.Config, u) {
				return true
			}
			if _, err := app.db.GetUserForAuth(u); err == nil {
				return true
			}
			if _, err := app.db.GetCollection(u); err == nil {
				return true
			}
			return app.db.PostIDExists(u)
		})

		newUser := &User{
			Username: username,
			// No password: this account is only ever reachable via OAuth login.
			// CreateUser's INSERT writes u.HashedPass directly into a NOT NULL
			// column, so this must be a non-nil empty slice, not the zero value
			// -- the same convention oauth_signup.go uses when no password is
			// submitted (hashedPass := []byte{}).
			HashedPass: []byte{},
			Created:    time.Now().Truncate(time.Second).UTC(),
		}
		if err = h.DB.CreateUser(h.Config, newUser, tokenInfo.DisplayName, ""); err != nil {
			log.Error("oauth JIT: CreateUser failed for %q (remote user %s, provider %s, client %s): %v", username, tokenInfo.UserID, provider, clientID, err)
			return impart.HTTPError{http.StatusInternalServerError, err.Error()}
		}
		if err = app.db.SetUserMaxBlogs(newUser.Username, maxBlogs); err != nil {
			log.Error("oauth JIT: created user %q but failed to set max_blogs: %v", newUser.Username, err)
			// Do not fail the login over this -- the user account exists and is
			// usable; worst case they fall back to the instance default limit
			// until the next allowance push or the nightly audit corrects it.
		}
		if err = h.DB.RecordRemoteUserID(r.Context(), newUser.ID, tokenInfo.UserID, provider, clientID, tokenResponse.AccessToken); err != nil {
			// CreateUser has already committed users/collections rows at this
			// point -- this is a partial failure, not a clean rollback. The
			// account now exists but is unlinked, and normalizeOauthUsername's
			// idempotency guarantee does NOT cover this case (see the caveat on
			// its doc comment in oauth_preauth.go): a retry will see this
			// username as taken and provision a SECOND, differently-named
			// account rather than completing this one. Log loudly so this is
			// discoverable and manually fixable rather than a silent 500.
			log.Error("oauth JIT: created user id=%d username=%q but FAILED to link remote user %s (provider %s, client %s): %v -- this account is orphaned and needs manual reconciliation", newUser.ID, newUser.Username, tokenInfo.UserID, provider, clientID, err)
			return impart.HTTPError{http.StatusInternalServerError, err.Error()}
		}
		if err = app.db.DeleteOauthPreauth(tokenInfo.UserID, provider, clientID); err != nil {
			log.Error("oauth JIT: provisioned user %q but failed to delete preauth row: %v", newUser.Username, err)
		}

		if err = loginOrFail(h.Store, w, r, newUser); err != nil {
			log.Error("Unable to loginOrFail %d: %s", newUser.ID, err)
			return impart.HTTPError{http.StatusInternalServerError, err.Error()}
		}
		return nil
	}

	// Not eligible: no account is created. This must NOT fall through to
	// showOauthSignupPage -- the OAuth path's only door is the preauth check
	// above, enforced unconditionally in code. (In-app self-serve signup is a
	// separate story per route: POST /api/auth/signup IS gated by
	// open_registration at route-registration time, but POST /auth/signup is
	// registered unconditionally and its open_registration check is bypassed
	// by any non-empty invite_code -- so in practice it is blocked only by a
	// HAProxy ACL outside this repo. See FORK.md's "Known limits". That infra
	// dependency has no bearing on this OAuth code path.)
	return impart.HTTPError{http.StatusForbidden, "This Mastodon account is not currently linked to an active theATL.social membership. If you believe this is an error, check your membership status at members.theatl.social."}

	// New user registration below.
	// First, verify that user is allowed to register
	if inviteCode != "" {
		// Verify invite code is valid
		i, err := app.db.GetUserInvite(inviteCode)
		if err != nil {
			return impart.HTTPError{http.StatusInternalServerError, err.Error()}
		}
		if !i.Active(app.db) {
			return impart.HTTPError{http.StatusNotFound, "Invite link has expired."}
		}
	} else if !app.cfg.App.OpenRegistration {
		addSessionFlash(app, w, r, ErrUserNotFound.Error(), nil)
		return impart.HTTPError{http.StatusFound, "/login"}
	}

	displayName := tokenInfo.DisplayName
	if len(displayName) == 0 {
		displayName = tokenInfo.Username
	}

	tp := &oauthSignupPageParams{
		AccessToken:     tokenResponse.AccessToken,
		TokenUsername:   tokenInfo.Username,
		TokenAlias:      tokenInfo.DisplayName,
		TokenEmail:      tokenInfo.Email,
		TokenRemoteUser: tokenInfo.UserID,
		Provider:        provider,
		ClientID:        clientID,
		InviteCode:      inviteCode,
	}
	tp.TokenHash = tp.HashTokenParams(h.Config.Server.HashSeed)

	return h.showOauthSignupPage(app, w, r, tp, nil)
}

func (r *callbackProxyClient) register(ctx context.Context, state string) error {
	form := url.Values{}
	form.Add("state", state)
	form.Add("location", r.callbackLocation)
	req, err := http.NewRequestWithContext(ctx, "POST", r.server, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", ServerUserAgent(""))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("unable register state location: %d", resp.StatusCode)
	}

	return nil
}

func limitedJsonUnmarshal(body io.ReadCloser, n int, thing interface{}) error {
	lr := io.LimitReader(body, int64(n+1))
	data, err := io.ReadAll(lr)
	if err != nil {
		return err
	}
	if len(data) == n+1 {
		return fmt.Errorf("content larger than max read allowance: %d", n)
	}
	return json.Unmarshal(data, thing)
}

func loginOrFail(store sessions.Store, w http.ResponseWriter, r *http.Request, user *User) error {
	// An error may be returned, but a valid session should always be returned.
	session, _ := store.Get(r, cookieName)
	session.Values[cookieUserVal] = user.Cookie()
	if err := session.Save(r, w); err != nil {
		fmt.Println("error saving session", err)
		return err
	}
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
	return nil
}
