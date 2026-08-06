package writefreely

import (
	"bytes"
	"context"
	"database/sql"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	wclog "github.com/writeas/web-core/log"
	"github.com/writefreely/writefreely/config"
	"github.com/writefreely/writefreely/key"
)

func TestEnsureOauthPreauthTable(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}

		_, _ = ds.Exec("DROP TABLE IF EXISTS oauth_preauth")

		assert.NoError(t, ds.ensureOauthPreauthTable())

		_, err := ds.Exec(
			"INSERT INTO oauth_preauth (remote_user_id, provider, client_id, max_blogs) VALUES (?, ?, ?, ?)",
			"12345", "generic", "test-client", 3)
		assert.NoError(t, err)

		// Second call is a no-op, not an error, and does not disturb existing rows.
		assert.NoError(t, ds.ensureOauthPreauthTable())

		var maxBlogs int
		assert.NoError(t, ds.QueryRow(
			"SELECT max_blogs FROM oauth_preauth WHERE remote_user_id = ?", "12345").Scan(&maxBlogs))
		assert.Equal(t, 3, maxBlogs)
	})
}

func TestGetUpsertDeleteOauthPreauth(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		assert.NoError(t, ds.ensureOauthPreauthTable())

		_, found, err := ds.GetOauthPreauth("999", "generic", "client-a")
		assert.NoError(t, err)
		assert.False(t, found, "no row yet")

		assert.NoError(t, ds.UpsertOauthPreauth("999", "generic", "client-a", 3))
		limit, found, err := ds.GetOauthPreauth("999", "generic", "client-a")
		assert.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, 3, limit)

		// Upsert again with a different value — must update, not error or duplicate.
		assert.NoError(t, ds.UpsertOauthPreauth("999", "generic", "client-a", 15))
		limit, found, err = ds.GetOauthPreauth("999", "generic", "client-a")
		assert.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, 15, limit)

		assert.NoError(t, ds.DeleteOauthPreauth("999", "generic", "client-a"))
		_, found, err = ds.GetOauthPreauth("999", "generic", "client-a")
		assert.NoError(t, err)
		assert.False(t, found, "deleted row must not be found")

		// Deleting a nonexistent row is a no-op, not an error.
		assert.NoError(t, ds.DeleteOauthPreauth("999", "generic", "client-a"))
	})
}

func TestHandleSetMastodonUserMaxBlogs(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		assert.NoError(t, ds.ensureMaxBlogsColumn())
		assert.NoError(t, ds.ensureOauthPreauthTable())

		app := &App{db: ds, cfg: config.New()}
		secret := "0123456789abcdef0123456789abcdef"
		t.Setenv("WRITEFREELY_API_SECRET", secret)

		router := mux.NewRouter()
		router.HandleFunc("/api/internal/mastodon-user/{remoteUserID}/max-blogs",
			handleSetMastodonUserMaxBlogs(app)).Methods("POST")

		post := func(remoteID, sec, body string) int {
			r := httptest.NewRequest("POST",
				"/api/internal/mastodon-user/"+remoteID+"/max-blogs", strings.NewReader(body))
			if sec != "" {
				r.Header.Set("X-WriteFreely-Secret", sec)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			return w.Code
		}

		// No local user linked yet — must upsert a preauth row, not error.
		assert.Equal(t, http.StatusOK, post("54321", secret, `{"max_blogs":3}`))
		limit, found, err := ds.GetOauthPreauth("54321", "generic", app.cfg.GenericOauth.ClientID)
		assert.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, 3, limit)

		// Same fail-closed regression coverage as the shipped setter, exercised
		// here against the pre-account (oauth_preauth) branch.
		assert.Equal(t, http.StatusUnauthorized, post("54321", "", `{"max_blogs":3}`),
			"missing secret must be rejected")
		assert.Equal(t, http.StatusUnauthorized, post("54321", "wrong-but-32-chars-long-xxxxxxxx", `{"max_blogs":3}`),
			"wrong secret must be rejected — exercises the ConstantTimeCompare mismatch path on two equal-length digests, not just the empty-secret case")
		for _, bad := range []string{
			`not json`,
			`{}`,
			`{"max_blogs":null}`,
			`{"max_blogs":0}`,
			`{"max_blogs":-1}`,
			`{"max_blogs":99999}`,
			`{"maxblogs":5}`,
		} {
			assert.Equal(t, http.StatusBadRequest, post("54321", secret, bad),
				"payload %s must be refused", bad)
		}

		// None of the rejected calls above may have touched the preauth row
		// they were about to consume — the fail-closed guarantee is about the
		// stored state surviving intact, not just the returned status code.
		limit, found, err = ds.GetOauthPreauth("54321", "generic", app.cfg.GenericOauth.ClientID)
		assert.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, 3, limit, "a rejected payload must not modify the pre-authorized limit")

		// Now simulate an already-provisioned user linked to this remote ID:
		// create a local user and link it, then push again — must update
		// users.max_blogs directly and NOT touch oauth_preauth.
		res, err := ds.Exec(
			"INSERT INTO users (username, password, email, created) VALUES (?, ?, ?, NOW())",
			"linkeduser", "x", nil)
		assert.NoError(t, err)
		uid, _ := res.LastInsertId()
		// NOTE: deviation from the task brief, which specified ExecContext(nil, ...)
		// here. A nil context.Context makes database/sql's (*DB).conn panic on
		// ctx.Done() while holding db.mu (not deferred), and the panic unwind then
		// runs withTestDB's cleanup, which calls db.Close() on the same *sql.DB and
		// self-deadlocks re-acquiring db.mu — hanging the whole test binary until
		// the go test timeout. context.Background() is the correct context here.
		_, err = ds.ExecContext(context.Background(),
			"INSERT INTO oauth_users (user_id, remote_user_id, provider, client_id, access_token) VALUES (?, ?, ?, ?, ?)",
			uid, "54321", "generic", app.cfg.GenericOauth.ClientID, "tok")
		assert.NoError(t, err)

		assert.Equal(t, http.StatusOK, post("54321", secret, `{"max_blogs":15}`))

		var gotMaxBlogs sql.NullInt64
		assert.NoError(t, ds.QueryRow("SELECT max_blogs FROM users WHERE id = ?", uid).Scan(&gotMaxBlogs))
		assert.True(t, gotMaxBlogs.Valid)
		assert.Equal(t, int64(15), gotMaxBlogs.Int64)

		// The preauth row from the FIRST push must be gone by now — consumed at
		// link time — not left stale at its old value of 3.
		_, found, err = ds.GetOauthPreauth("54321", "generic", app.cfg.GenericOauth.ClientID)
		assert.NoError(t, err)
		assert.False(t, found)

		// Repeat the auth/payload rejection sweep against the now-linked
		// (post-account) branch — same fail-closed contract, different storage.
		assert.Equal(t, http.StatusUnauthorized, post("54321", "", `{"max_blogs":7}`),
			"missing secret must be rejected")
		assert.Equal(t, http.StatusUnauthorized, post("54321", "wrong-but-32-chars-long-xxxxxxxx", `{"max_blogs":7}`),
			"wrong secret must be rejected")
		for _, bad := range []string{
			`not json`,
			`{}`,
			`{"max_blogs":null}`,
			`{"max_blogs":0}`,
			`{"max_blogs":-1}`,
			`{"max_blogs":99999}`,
			`{"maxblogs":5}`,
		} {
			assert.Equal(t, http.StatusBadRequest, post("54321", secret, bad),
				"payload %s must be refused", bad)
		}

		// ...and none of them may have changed the stored value.
		assert.NoError(t, ds.QueryRow("SELECT max_blogs FROM users WHERE id = ?", uid).Scan(&gotMaxBlogs))
		assert.True(t, gotMaxBlogs.Valid)
		assert.Equal(t, int64(15), gotMaxBlogs.Int64, "a refused payload must not modify the allowance")
	})
}

func TestHandleSetMastodonUserMaxBlogsRefusesWeakSecret(t *testing.T) {
	app := &App{cfg: config.New()}
	t.Setenv("WRITEFREELY_API_SECRET", "tooshort")

	router := mux.NewRouter()
	router.HandleFunc("/api/internal/mastodon-user/{remoteUserID}/max-blogs",
		handleSetMastodonUserMaxBlogs(app)).Methods("POST")

	r := httptest.NewRequest("POST", "/api/internal/mastodon-user/54321/max-blogs",
		strings.NewReader(`{"max_blogs":3}`))
	r.Header.Set("X-WriteFreely-Secret", "tooshort")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"a short secret must fail closed, never authorise")
}

func TestHandleSetMastodonUserMaxBlogsRefusesUnsetSecret(t *testing.T) {
	app := &App{cfg: config.New()}
	// Genuinely unset, not merely short — this is the state a misconfigured
	// deploy is actually in, and it must not be treated as permission.
	t.Setenv("WRITEFREELY_API_SECRET", "")

	router := mux.NewRouter()
	router.HandleFunc("/api/internal/mastodon-user/{remoteUserID}/max-blogs",
		handleSetMastodonUserMaxBlogs(app)).Methods("POST")

	r := httptest.NewRequest("POST", "/api/internal/mastodon-user/54321/max-blogs",
		strings.NewReader(`{"max_blogs":3}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"an unset secret must fail closed")
}

// TestMastodonUserMaxBlogsRouteIsRegistered guards against the one failure
// mode none of the tests above can catch: every test up to this point builds
// its own throwaway mux.Router and registers handleSetMastodonUserMaxBlogs by
// hand, so all of them would keep passing even if the real registration —
// write.HandleFunc("/api/internal/mastodon-user/{remoteUserID}/max-blogs", ...)
// in routes.go's InitRoutes — were deleted entirely. That is exactly the kind
// of line a conflicted upstream merge can silently drop, and this fork merges
// upstream regularly, so the risk is not hypothetical. This test builds the
// actual production router via InitRoutes and confirms the route resolves
// against it.
func TestMastodonUserMaxBlogsRouteIsRegistered(t *testing.T) {
	// Must be config.New(), not &config.Config{}: InitRoutes does
	// cfg.App.Host[strings.Index(cfg.App.Host, "://")+3:] unconditionally, which
	// panics (slice bounds out of range) on an empty Host. config.New() sets
	// Host to "http://localhost:8080".
	cfg := config.New()
	// config.New() defaults to SingleUser: true, which routes nodeInfoConfig
	// through db.GetCollectionByID(1) — a real query this test's nil db can't
	// serve. The fork's actual deployment runs multi-user (config.ini.example
	// sets single_user = false), so this also matches production's routing
	// shape, not just avoiding a crash.
	cfg.App.SingleUser = false

	if err := InitTemplates(cfg); err != nil {
		t.Fatalf("InitTemplates: %v (expected to find templates/ and pages/ "+
			"relative to the test binary's working directory)", err)
	}

	app := &App{
		cfg: cfg,
		// A couple of routes InitRoutes registers are wrapped in
		// csrf.Protect(app.keys.CSRFKey); a nil *key.Keychain panics on that field
		// access before InitRoutes ever gets to the route this test checks.
		keys: &key.Keychain{CSRFKey: []byte("0123456789abcdef0123456789abcdef")},
	}

	router := mux.NewRouter()
	InitRoutes(app, router)

	req := httptest.NewRequest("POST", "/api/internal/mastodon-user/54321/max-blogs", nil)
	var match mux.RouteMatch
	if !router.Match(req, &match) {
		t.Fatalf("POST /api/internal/mastodon-user/{remoteUserID}/max-blogs did not resolve "+
			"against the real router (match error: %v) — the registration in "+
			"routes.go's InitRoutes appears to be missing", match.MatchErr)
	}
	assert.Equal(t, "54321", match.Vars["remoteUserID"],
		"the {remoteUserID} path variable should capture the remote user ID segment")
}

func TestNormalizeOauthUsername(t *testing.T) {
	noneTaken := func(string) bool { return false }

	tests := []struct {
		name       string
		mastodon   string
		remoteID   string
		taken      func(string) bool
		want       string
	}{
		{"simple lowercase alnum passes through", "jsmith", "14882", noneTaken, "jsmith"},
		{"uppercase is lowered", "JSmith", "14882", noneTaken, "jsmith"},
		{"underscore becomes hyphen", "j_smith", "14882", noneTaken, "j-smith"},
		{"collision gets ID-suffixed", "jsmith", "14882", func(u string) bool { return u == "jsmith" }, "jsmith-14882"},
		{"collision on the suffixed form too falls back to raw ID", "jsmith", "14882",
			func(u string) bool { return u == "jsmith" || u == "jsmith-14882" }, "user-14882"},

		// The following cases simulate what the real production taken()
		// closure in oauth.go does since the fix for a reserved-name bypass:
		// it now reports author.IsValidUsername failures (reserved words,
		// too-short names) as "taken" too, not just genuine uniqueness
		// collisions against WriteFreely's own tables. normalizeOauthUsername
		// itself stays decoupled from author/config -- these taken() stand-
		// ins are what let this stay a fast, DB-less unit test while still
		// proving the fallback-tier routing an invalid/reserved base needs.
		{
			"reserved word (as flagged by taken()) is never returned verbatim",
			"admin", "77",
			func(u string) bool { return u == "admin" }, // stands in for author.IsValidUsername rejecting "admin"
			"admin-77",
		},
		{
			"another reserved word (as flagged by taken()) is never returned verbatim",
			"login", "77",
			func(u string) bool { return u == "login" },
			"login-77",
		},
		{
			// Regression guard for the hardcoded `if len(base) < 3 { base =
			// "user" }` this function used to have: that literal "user" is
			// ITSELF reserved, and was substituted in before any taken()
			// check ran, so it could be (and empirically was) handed out
			// verbatim. There is now no such hardcoded substitution --
			// short-ness is caught by taken() like everything else, and the
			// suffixed fallback uses the ORIGINAL (too-short) base, never
			// the literal "user".
			"short base (as flagged by taken()) does not fall back to the reserved literal \"user\"",
			"jo", "99",
			func(u string) bool { return len(u) < 3 }, // stands in for author.IsValidUsername's MinUsernameLen floor
			"jo-99",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeOauthUsername(tc.mastodon, tc.remoteID, tc.taken))
		})
	}
}

// TestNormalizeOauthUsernameFinalTierCollision guards the fix for the last
// fallback tier ("user-<remoteUserID>") being assumed unique by construction
// and never passed through taken() at all. That assumption doesn't fully
// hold: a pre-existing account/collection/post literally named
// "user-<remoteUserID>" is reachable -- via the orphan-retry scenario this
// function's own doc comment describes, or via /auth/signup's invite-code
// bypass (see FORK.md's "Known limits") -- and when it happens, the
// caller's CreateUser call 409s with nothing in the logs to explain why: a
// permanent, opaque lockout for that Mastodon identity.
//
// This test forces a collision at ALL THREE tiers (base, ID-suffixed, and
// the final "user-<remoteID>" form) and asserts:
//   - taken() is actually invoked with the final-tier candidate -- proving
//     the fix's core change (this tier is no longer skipped).
//   - normalizeOauthUsername still returns the final-tier value rather than
//     panicking -- there is no fourth tier, so handing it back for the
//     caller's CreateUser to 409 on is the correct, preserved behavior.
//   - a log line fires identifying the collision, containing the remote
//     user ID, the provider, and the exact username string that collided,
//     so the eventual 409 is discoverable instead of a silent 500.
func TestNormalizeOauthUsernameFinalTierCollision(t *testing.T) {
	const mastodonUsername = "jsmith"
	const remoteID = "14882"
	finalTier := "user-" + remoteID

	var takenCalls []string
	taken := func(u string) bool {
		takenCalls = append(takenCalls, u)
		return true // every tier reports occupied, including the final one
	}

	// Capture web-core/log's error output for the duration of this test, and
	// restore it afterward -- ErrorLog is a shared package-level *log.Logger.
	var logBuf bytes.Buffer
	origErrorLog := wclog.ErrorLog
	wclog.ErrorLog = stdlog.New(&logBuf, "ERROR: ", 0)
	defer func() { wclog.ErrorLog = origErrorLog }()

	var got string
	require.NotPanics(t, func() {
		got = normalizeOauthUsername(mastodonUsername, remoteID, taken)
	}, "exhausting every fallback tier must not panic -- there is no fourth tier to fall back to")

	assert.Equal(t, finalTier, got,
		"with every tier occupied, the function must still return the final-tier value -- the caller's CreateUser is expected to 409 on it, not this function")

	assert.Contains(t, takenCalls, finalTier,
		"the final fallback tier must be run through taken() like every other tier -- this is the core of the fix")

	logOutput := logBuf.String()
	assert.Contains(t, logOutput, remoteID, "log line should include the Mastodon remote user ID")
	assert.Contains(t, logOutput, "generic", "log line should include the provider")
	assert.Contains(t, logOutput, finalTier, "log line should include the exact username string that collided")
}
