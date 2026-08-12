/*
 * Copyright © 2018-2021 Musing Studio LLC.
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
	"net/http"
	"os"
	"time"

	"github.com/writeas/impart"
	"github.com/writeas/web-core/log"
	"github.com/writefreely/writefreely/author"
	"github.com/writefreely/writefreely/page"
)

// theATL fork: real-time reconciliation retry.
//
// pushWriteFreelyAllowanceIfEligible (member-site's allowance.ts) is the only
// thing that ever writes an oauth_preauth row, and it only ever runs as a
// side effect of an authenticated member-site request. A member who tries
// write.theatl.social before ever authenticating to members.theatl.social --
// or whose one push attempt silently failed -- has no preauth row and used
// to just get a raw 403 here (see the 2026-08-12-writefreely-preauth-gap
// incident: an active, paying member hit exactly this).
//
// Rather than fail immediately the first time attemptOAuthLogin comes back
// not-eligible, viewOauthCallback now stashes this identity (see
// pendingReconciliation below) and sends the browser to /oauth/reconciling,
// which shows a brief "we're checking" page before retrying. The retry
// (viewOauthReconcilingFinish) calls member-site's own billing/status
// endpoint synchronously, using the Mastodon access token obtained moments
// ago during this exact OAuth exchange -- the same live reconciliation logic
// a normal authenticated page load would have triggered, just invoked
// on-demand instead of waiting for one to happen. Only if that still leaves
// the identity ineligible does the user see a final, friendly error page.

const (
	reconcileCookieName = "wf-reconcile-pending"
	// Single-use and short-lived: this cookie only needs to survive the
	// wait-page redirect (a couple of seconds), not a real session.
	reconcileCookieMaxAge = 300 // 5 minutes
)

type pendingReconciliation struct {
	RemoteUserID string
	Username     string
	DisplayName  string
	Provider     string
	ClientID     string
	AccessToken  string
}

// setPendingReconciliation stashes just enough state to retry a single OAuth
// identity check, signed and encrypted the same way as WriteFreely's own
// login session (app.sessionStore), under a separate cookie name so it can't
// collide with or be confused for a real logged-in session.
func setPendingReconciliation(app *App, w http.ResponseWriter, r *http.Request, p pendingReconciliation) error {
	session, err := app.sessionStore.New(r, reconcileCookieName)
	if err != nil {
		// sessions.Store.New never fails outright (it returns a fresh session
		// even on decode error), but check anyway rather than assume.
		log.Error("oauth reconcile: sessionStore.New error (continuing with fresh session): %v", err)
	}
	session.Options.MaxAge = reconcileCookieMaxAge
	session.Values["remoteUserID"] = p.RemoteUserID
	session.Values["username"] = p.Username
	session.Values["displayName"] = p.DisplayName
	session.Values["provider"] = p.Provider
	session.Values["clientID"] = p.ClientID
	session.Values["accessToken"] = p.AccessToken
	return session.Save(r, w)
}

// getAndClearPendingReconciliation reads back what setPendingReconciliation
// stored, and immediately invalidates the cookie regardless of what the
// caller does next -- this state is meant to be used exactly once.
func getAndClearPendingReconciliation(app *App, w http.ResponseWriter, r *http.Request) (*pendingReconciliation, error) {
	session, err := app.sessionStore.Get(r, reconcileCookieName)
	if err != nil {
		return nil, err
	}

	remoteUserID, _ := session.Values["remoteUserID"].(string)
	if remoteUserID == "" {
		// No pending state (never set, already consumed, or the 5-minute
		// window lapsed). Not an error -- the caller decides what to do.
		return nil, nil
	}
	p := &pendingReconciliation{RemoteUserID: remoteUserID}
	p.Username, _ = session.Values["username"].(string)
	p.DisplayName, _ = session.Values["displayName"].(string)
	p.Provider, _ = session.Values["provider"].(string)
	p.ClientID, _ = session.Values["clientID"].(string)
	p.AccessToken, _ = session.Values["accessToken"].(string)

	session.Options.MaxAge = -1
	if err := session.Save(r, w); err != nil {
		log.Error("oauth reconcile: failed to clear pending session: %v", err)
	}
	return p, nil
}

// attemptOAuthLogin checks whether remoteUserID is already linked to a
// WriteFreely account (logging them in if so), or JIT-provisions a new
// account if an eligible oauth_preauth grant now exists. It fully writes the
// HTTP response itself (redirect + session cookie) whenever it returns
// handled=true; the caller must not write anything further in that case.
//
// Extracted from viewOauthCallback so the exact same concurrency-safe
// check-and-provision logic can run a second time from
// viewOauthReconcilingFinish, after a real-time reconciliation attempt,
// without duplicating it. withOauthIdentityLock (oauth_preauth.go) guards
// everything below, keyed on this exact identity (remoteUserID, provider,
// clientID) -- without it, this JIT path and handleSetMastodonUserMaxBlogs's
// check-then-write (oauth_preauth.go) can interleave: an allowance push can
// observe "not linked" while a login for the same identity is concurrently
// creating the account, silently losing the push (200 OK, but the account
// keeps its OLD allowance) and resurrecting a preauth row against an
// identity that is now already provisioned -- see withOauthIdentityLock's
// doc comment for the full race and why a plain transaction alone can't
// close it.
func attemptOAuthLogin(
	ctx context.Context,
	app *App,
	w http.ResponseWriter,
	r *http.Request,
	remoteUserID, mastodonUsername, displayName, provider, clientID, accessToken string,
) (handled bool, err error) {
	lockErr := withOauthIdentityLock(ctx, app, remoteUserID, provider, clientID, func() error {
		// Re-check linkage inside the lock: an earlier GetIDForRemoteUser
		// check (e.g. the one at the top of viewOauthCallback) can be stale
		// by the time we get here -- two near-simultaneous logins for the
		// same Mastodon identity, or this login racing
		// handleSetMastodonUserMaxBlogs, which takes this same per-identity
		// lock before its own linked/not-linked branch. If someone else won
		// that race and linked this identity while we waited for the lock,
		// recover by logging the now-existing account in rather than
		// creating a second, differently-suffixed duplicate.
		relinkedID, err := app.db.GetIDForRemoteUser(ctx, remoteUserID, provider, clientID)
		if err != nil {
			log.Error("Unable to GetIDForRemoteUser: %s", err)
			return err
		}
		if relinkedID != -1 {
			handled = true
			user, err := app.db.GetUserByID(relinkedID)
			if err != nil {
				log.Error("Unable to GetUserByID %d: %s", relinkedID, err)
				return err
			}
			if err := loginOrFail(app.sessionStore, w, r, user); err != nil {
				log.Error("Unable to loginOrFail %d: %s", user.ID, err)
				return err
			}
			return nil
		}

		maxBlogs, eligible, err := app.db.GetOauthPreauth(remoteUserID, provider, clientID)
		if err != nil {
			return err
		}
		if !eligible {
			return nil
		}
		handled = true

		// CreateUser enforces uniqueness of this string against THREE tables,
		// not just users.username: collections.alias (INSERT INTO
		// collections, database.go ~line 248, rolls back and returns 409 on
		// collision) and posts.id via PostIDExists (database.go ~line 215,
		// checked up front, also 409). taken() must cover all three, or a
		// collision against a collection alias or post ID that
		// GetUserForAuth alone can't see reports "available" when it isn't
		// -- normalizeOauthUsername never tries its suffixed fallback, and
		// every retry then hits the identical collision, locking that
		// member out permanently.
		//
		// taken() must ALSO cover validity, not just uniqueness: every other
		// account-creation path in this codebase (account.go, app.go,
		// collections.go, database.go) gates on author.IsValidUsername,
		// which rejects both too-short names and a reserved-word list
		// ("admin", "login", "user", ...) that nothing in WriteFreely's own
		// uniqueness tables would ever flag as occupied. Without this, the
		// JIT path could hand a real Mastodon member a
		// reserved/impersonation-prone username (e.g. "admin") purely
		// because no WriteFreely account happened to be sitting on it yet.
		// Treating "invalid" the same as "taken" here makes
		// normalizeOauthUsername's existing suffix-fallback tiers route
		// around it automatically -- no separate reserved-word logic needed
		// there.
		username := normalizeOauthUsername(mastodonUsername, remoteUserID, func(u string) bool {
			if !author.IsValidUsername(app.cfg, u) {
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
			// No password: this account is only ever reachable via OAuth
			// login. CreateUser's INSERT writes u.HashedPass directly into a
			// NOT NULL column, so this must be a non-nil empty slice, not
			// the zero value -- the same convention oauth_signup.go uses
			// when no password is submitted (hashedPass := []byte{}).
			HashedPass: []byte{},
			Created:    time.Now().Truncate(time.Second).UTC(),
		}
		if err := app.db.CreateUser(app.cfg, newUser, displayName, ""); err != nil {
			log.Error("oauth JIT: CreateUser failed for %q (remote user %s, provider %s, client %s): %v", username, remoteUserID, provider, clientID, err)
			return err
		}
		if err := app.db.SetUserMaxBlogs(newUser.Username, maxBlogs); err != nil {
			log.Error("oauth JIT: created user %q but failed to set max_blogs: %v", newUser.Username, err)
			// Do not fail the login over this -- the user account exists and
			// is usable; worst case they fall back to the instance default
			// limit until the next allowance push or the reconciliation
			// sweep corrects it.
		}
		if err := app.db.RecordRemoteUserID(ctx, newUser.ID, remoteUserID, provider, clientID, accessToken); err != nil {
			// CreateUser has already committed users/collections rows at
			// this point -- this is a partial failure, not a clean
			// rollback. The account now exists but is unlinked, and
			// normalizeOauthUsername's idempotency guarantee does NOT cover
			// this case (see the caveat on its doc comment in
			// oauth_preauth.go): a retry will see this username as taken
			// and provision a SECOND, differently-named account rather than
			// completing this one. Log loudly so this is discoverable and
			// manually fixable rather than a silent 500.
			log.Error("oauth JIT: created user id=%d username=%q but FAILED to link remote user %s (provider %s, client %s): %v -- this account is orphaned and needs manual reconciliation", newUser.ID, newUser.Username, remoteUserID, provider, clientID, err)
			return err
		}
		if err := app.db.DeleteOauthPreauth(remoteUserID, provider, clientID); err != nil {
			log.Error("oauth JIT: provisioned user %q but failed to delete preauth row: %v", newUser.Username, err)
		}

		if err := loginOrFail(app.sessionStore, w, r, newUser); err != nil {
			log.Error("Unable to loginOrFail %d: %s", newUser.ID, err)
			return err
		}
		return nil
	})
	if lockErr != nil {
		return false, lockErr
	}
	return handled, nil
}

// triggerMemberSiteReconciliation calls member-site's own billing endpoint
// with the member's just-obtained Mastodon access token, which as a side
// effect re-derives their tier and pushes an updated oauth_preauth grant if
// they're eligible (allowance.ts's pushWriteFreelyAllowanceIfEligible) --
// the exact same logic a normal authenticated member-site page load would
// have triggered. Errors are logged, not returned: the caller re-checks
// eligibility regardless of whether this call succeeds (a concurrent
// reconciliation sweep run, for instance, may have already fixed it).
func triggerMemberSiteReconciliation(ctx context.Context, accessToken string) {
	baseURL := os.Getenv("MEMBERSITE_INTERNAL_URL")
	if baseURL == "" {
		log.Error("oauth reconcile: MEMBERSITE_INTERNAL_URL not set, skipping real-time reconciliation call")
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+"/api/user/billing", nil)
	if err != nil {
		log.Error("oauth reconcile: failed to build member-site request: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := reconcileHTTPClient.Do(req)
	if err != nil {
		log.Error("oauth reconcile: member-site call failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Error("oauth reconcile: member-site returned status %d", resp.StatusCode)
	}
}

var reconcileHTTPClient = &http.Client{Timeout: 10 * time.Second}

// viewOauthReconciling renders the brief "hang on" interstitial that
// replaces an immediate hard failure the first time attemptOAuthLogin comes
// back not-eligible. It always succeeds (a static page); the real work
// happens once the page's own redirect lands on viewOauthReconcilingFinish.
func viewOauthReconciling(app *App, w http.ResponseWriter, r *http.Request) error {
	p := struct {
		page.StaticPage
	}{
		StaticPage: pageForReq(app, r),
	}
	return renderPage(w, "oauth-reconciling.tmpl", p)
}

// viewOauthReconcilingFinish performs the actual reconciliation retry: ask
// member-site to re-check this member's tier and push an updated grant if
// eligible, then re-run the same login/JIT check as the original callback.
// Succeeds either by logging the member in (handled=true) or by rendering a
// clear final error page -- this is the one place a genuinely-not-eligible
// visitor ends up, instead of the raw JSON error this replaces.
func viewOauthReconcilingFinish(app *App, w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	pending, err := getAndClearPendingReconciliation(app, w, r)
	if err != nil {
		log.Error("oauth reconcile: failed to read pending state: %v", err)
	}
	if pending == nil {
		// Nothing to retry (expired, already used, or reached directly) --
		// there's no identity left to check. Send them back to start over
		// rather than show a confusing error about an attempt that never
		// happened.
		return impart.HTTPError{http.StatusFound, "/oauth/generic"}
	}

	triggerMemberSiteReconciliation(ctx, pending.AccessToken)

	handled, err := attemptOAuthLogin(ctx, app, w, r, pending.RemoteUserID, pending.Username, pending.DisplayName, pending.Provider, pending.ClientID, pending.AccessToken)
	if err != nil {
		return impart.HTTPError{http.StatusInternalServerError, err.Error()}
	}
	if handled {
		return nil
	}

	p := struct {
		page.StaticPage
	}{
		StaticPage: pageForReq(app, r),
	}
	w.WriteHeader(http.StatusForbidden)
	return renderPage(w, "oauth-not-eligible.tmpl", p)
}
