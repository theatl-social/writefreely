package writefreely

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/sessions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/writeas/impart"
	"github.com/writefreely/writefreely/config"
)

// pendingReconciliationRequest builds a request carrying whatever
// setPendingReconciliation just wrote, by round-tripping through a real
// ResponseRecorder -- the same technique the rest of this file's tests use
// to carry a session cookie from one handler call into the next.
func pendingReconciliationRequest(t *testing.T, app *App, p pendingReconciliation) *http.Request {
	t.Helper()
	setupReq := httptest.NewRequest("GET", "/oauth/callback/generic", nil)
	setupRR := httptest.NewRecorder()
	require.NoError(t, setPendingReconciliation(app, setupRR, setupReq, p))

	req := httptest.NewRequest("GET", "/oauth/reconciling/finish", nil)
	for _, c := range setupRR.Result().Cookies() {
		req.AddCookie(c)
	}
	return req
}

func TestPendingReconciliationRoundTrip(t *testing.T) {
	store := sessions.NewCookieStore([]byte("secret-key"))
	app := &App{sessionStore: store}

	want := pendingReconciliation{
		RemoteUserID: "round-trip-1",
		Username:     "roundtripper",
		DisplayName:  "Round Tripper",
		Provider:     "generic",
		ClientID:     "client-round-trip",
		AccessToken:  "test-access-token",
	}
	req := pendingReconciliationRequest(t, app, want)
	rr := httptest.NewRecorder()

	got, err := getAndClearPendingReconciliation(app, rr, req)
	require.NoError(t, err)
	require.NotNil(t, got, "expected to read back what was just stored")
	assert.Equal(t, want, *got)

	// This is hygiene, not a server-enforced single-use guarantee (see
	// getAndClearPendingReconciliation's doc comment for why that's fine
	// here): the response must tell the browser to drop the cookie, so a
	// normal follow-up visit -- not a captured-and-replayed copy -- won't
	// find it again.
	var sawExpiredPendingCookie bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == reconcileCookieName && c.MaxAge < 0 {
			sawExpiredPendingCookie = true
		}
	}
	assert.True(t, sawExpiredPendingCookie, "expected the response to expire the pending-reconciliation cookie")
}

func TestViewOauthReconcilingFinishNoPendingState(t *testing.T) {
	store := sessions.NewCookieStore([]byte("secret-key"))
	app := &App{sessionStore: store}

	req := httptest.NewRequest("GET", "/oauth/reconciling/finish", nil)
	rr := httptest.NewRecorder()

	err := viewOauthReconcilingFinish(app, rr, req)
	require.Error(t, err)
	httpErr, ok := err.(impart.HTTPError)
	require.True(t, ok, "expected impart.HTTPError, got %T", err)
	assert.Equal(t, http.StatusFound, httpErr.Status)
	assert.Equal(t, "/oauth/generic", httpErr.Message, "nothing to retry -- send them back to start a fresh login, not a confusing error")
}

// TestViewOauthReconcilingFinishRetrySucceeds is the end-to-end proof this
// feature exists for: a member who was NOT eligible on their first attempt
// (no oauth_preauth row) becomes eligible once member-site is asked to
// reconcile them in real time, and the SAME visit's retry logs them in
// without the member doing anything else. The stub member-site server
// below stands in for the real /api/user/billing call and, as its side
// effect, inserts the oauth_preauth row directly -- mirroring what
// pushWriteFreelyAllowanceIfEligible actually does on the member-site side,
// without needing that whole separate service running here.
func TestViewOauthReconcilingFinishRetrySucceeds(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		require.NoError(t, ds.ensureMaxBlogsColumn())
		require.NoError(t, ds.ensureOauthPreauthTable())

		cfg := config.New()
		store := sessions.NewCookieStore([]byte("secret-key"))
		app := &App{db: ds, cfg: cfg, sessionStore: store, oauthLockDB: newTestOauthLockDB(t, db)}

		const remoteUserID = "reconcile-success-1"
		const provider = "generic"
		const clientID = "client-reconcile-1"
		var sawAuthHeader string

		memberSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/user/billing", r.URL.Path)
			sawAuthHeader = r.Header.Get("Authorization")
			// The real endpoint's side effect (allowance.ts's
			// pushWriteFreelyAllowanceIfEligible) is what actually matters
			// here -- simulate it directly rather than standing up member-site.
			require.NoError(t, ds.UpsertOauthPreauth(remoteUserID, provider, clientID, 3))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer memberSite.Close()
		t.Setenv("MEMBERSITE_INTERNAL_URL", memberSite.URL)

		// Sanity: genuinely not eligible before the retry runs.
		_, eligible, err := ds.GetOauthPreauth(remoteUserID, provider, clientID)
		require.NoError(t, err)
		require.False(t, eligible, "sanity: must start ineligible for this to prove anything")

		req := pendingReconciliationRequest(t, app, pendingReconciliation{
			RemoteUserID: remoteUserID,
			Username:     "reconciledmember",
			DisplayName:  "Reconciled Member",
			Provider:     provider,
			ClientID:     clientID,
			AccessToken:  "fresh-mastodon-token",
		})
		rr := httptest.NewRecorder()

		err = viewOauthReconcilingFinish(app, rr, req)
		require.NoError(t, err, "a successful retry logs the user in directly, it does not return an HTTPError")

		assert.Equal(t, "Bearer fresh-mastodon-token", sawAuthHeader, "member-site must be called with the token from this exact OAuth exchange")
		assert.Equal(t, http.StatusTemporaryRedirect, rr.Code, "loginOrFail redirects home on success")
		assert.Equal(t, "/", rr.Result().Header.Get("Location"))

		var sawSessionCookie bool
		for _, c := range rr.Result().Cookies() {
			if c.Name == cookieName {
				sawSessionCookie = true
			}
		}
		assert.True(t, sawSessionCookie, "expected a real login session cookie, not just the cleared pending-reconciliation cookie")

		localUserID, err := ds.GetIDForRemoteUser(context.Background(), remoteUserID, provider, clientID)
		require.NoError(t, err)
		assert.NotEqual(t, int64(-1), localUserID, "the account must actually be JIT-provisioned and linked")

		user, err := ds.GetUserByID(localUserID)
		require.NoError(t, err)
		assert.Equal(t, "reconciledmember", user.Username)

		_, stillEligible, err := ds.GetOauthPreauth(remoteUserID, provider, clientID)
		require.NoError(t, err)
		assert.False(t, stillEligible, "the preauth grant must be consumed, same as the direct-JIT path")
	})
}

// TestViewOauthReconcilingFinishStillNotEligible covers the other side: the
// member really isn't entitled, member-site is asked but grants nothing,
// and the visitor lands on the final friendly error page instead of being
// silently retried forever or seeing a raw JSON blob.
func TestViewOauthReconcilingFinishStillNotEligible(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		require.NoError(t, ds.ensureMaxBlogsColumn())
		require.NoError(t, ds.ensureOauthPreauthTable())

		// config.New(), not &config.Config{}: see
		// TestMastodonUserMaxBlogsRouteIsRegistered's comment
		// (oauth_preauth_test.go) for why -- InitRoutes-adjacent code paths
		// panic on an empty Host, though this test doesn't exercise that
		// directly, matching it avoids the same class of surprise.
		cfg := config.New()
		cfg.App.SingleUser = false
		if err := InitTemplates(cfg); err != nil {
			t.Fatalf("InitTemplates: %v (expected to find templates/ and pages/ "+
				"relative to the test binary's working directory)", err)
		}

		store := sessions.NewCookieStore([]byte("secret-key"))
		app := &App{db: ds, cfg: cfg, sessionStore: store, oauthLockDB: newTestOauthLockDB(t, db)}

		const remoteUserID = "reconcile-fail-1"
		const provider = "generic"
		const clientID = "client-reconcile-fail-1"
		var memberSiteWasCalled bool

		memberSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			memberSiteWasCalled = true
			// Genuinely not a member: no preauth row gets granted.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer memberSite.Close()
		t.Setenv("MEMBERSITE_INTERNAL_URL", memberSite.URL)

		req := pendingReconciliationRequest(t, app, pendingReconciliation{
			RemoteUserID: remoteUserID,
			Username:     "neverjoined",
			DisplayName:  "Never Joined",
			Provider:     provider,
			ClientID:     clientID,
			AccessToken:  "some-token",
		})
		rr := httptest.NewRecorder()

		err := viewOauthReconcilingFinish(app, rr, req)
		require.NoError(t, err, "the not-eligible outcome renders a page directly, it does not return an HTTPError")

		assert.True(t, memberSiteWasCalled, "member-site must actually be asked before giving up")
		assert.Equal(t, http.StatusForbidden, rr.Code)
		assert.Contains(t, rr.Body.String(), "couldn't find an active membership")

		localUserID, err := ds.GetIDForRemoteUser(context.Background(), remoteUserID, provider, clientID)
		require.NoError(t, err)
		assert.Equal(t, int64(-1), localUserID, "no account may be created for a genuinely ineligible identity")
	})
}
