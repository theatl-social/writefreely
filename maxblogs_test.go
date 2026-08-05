package writefreely

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/writeas/impart"
	"github.com/writefreely/writefreely/config"
)

func TestEnsureMaxBlogsColumn(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}

		// Start from a table with no max_blogs column.
		_, _ = ds.Exec("ALTER TABLE users DROP COLUMN max_blogs")

		// First call adds it.
		assert.NoError(t, ds.ensureMaxBlogsColumn())

		var v sql.NullInt64
		err := ds.QueryRow("SELECT max_blogs FROM users LIMIT 1").Scan(&v)
		assert.True(t, err == nil || err == sql.ErrNoRows,
			"column should be queryable after ensure, got %v", err)

		// Second call is a no-op, not an error.
		assert.NoError(t, ds.ensureMaxBlogsColumn())
	})
}

func TestEnsureMaxBlogsColumnNoUsersTable(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		_, err := ds.Exec("DROP TABLE IF EXISTS users")
		assert.NoError(t, err)

		// An uninitialised database must not be a startup error.
		assert.NoError(t, ds.ensureMaxBlogsColumn())
	})
}

func TestEnsureMaxBlogsColumnPropagatesRealErrors(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}

		// Close the connection so the probes fail for a reason that is NOT
		// "the users table does not exist yet". A guard that infers
		// table-absence from any error would swallow this and return nil.
		assert.NoError(t, db.Close())

		assert.Error(t, ds.ensureMaxBlogsColumn(),
			"a connection failure must not be reported as 'table missing'")
	})
}

func TestEffectiveMaxBlogs(t *testing.T) {
	cfg := config.New()
	cfg.App.MaxBlogs = 1

	tests := []struct {
		name    string
		perUser sql.NullInt64
		want    int
	}{
		{"unset falls back to config", sql.NullInt64{Valid: false}, 1},
		{"per-user overrides config", sql.NullInt64{Int64: 15, Valid: true}, 15},
		{"per-user zero means unlimited", sql.NullInt64{Int64: 0, Valid: true}, 0},
		{"per-user one", sql.NullInt64{Int64: 1, Valid: true}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, effectiveMaxBlogs(cfg, tc.perUser))
		})
	}
}

func TestGetUserMaxBlogs(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		assert.NoError(t, ds.ensureMaxBlogsColumn())

		res, err := ds.Exec(
			"INSERT INTO users (username, password, email, created) VALUES (?, ?, ?, NOW())",
			"limituser", "x", nil)
		assert.NoError(t, err)
		uid, err := res.LastInsertId()
		assert.NoError(t, err)

		// Unset by default.
		got, err := ds.GetUserMaxBlogs(uid)
		assert.NoError(t, err)
		assert.False(t, got.Valid)

		// Reads back what was written.
		_, err = ds.Exec("UPDATE users SET max_blogs = ? WHERE id = ?", 3, uid)
		assert.NoError(t, err)

		got, err = ds.GetUserMaxBlogs(uid)
		assert.NoError(t, err)
		assert.True(t, got.Valid)
		assert.Equal(t, int64(3), got.Int64)

		// An explicit 0 must survive the round-trip as Valid, not collapse into
		// "unset". The two mean different things: 0 is "unlimited", NULL is
		// "fall back to the instance default". Task 4 branches on that.
		_, err = ds.Exec("UPDATE users SET max_blogs = ? WHERE id = ?", 0, uid)
		assert.NoError(t, err)

		got, err = ds.GetUserMaxBlogs(uid)
		assert.NoError(t, err)
		assert.True(t, got.Valid, "an explicit 0 must read back Valid, not NULL")
		assert.Equal(t, int64(0), got.Int64)

		// A user that does not exist must be distinguishable from a user with no
		// limit set — both would otherwise present as an invalid NullInt64.
		got, err = ds.GetUserMaxBlogs(999999)
		assert.Equal(t, ErrUserNotFound, err)
		assert.False(t, got.Valid)
	})
}

func TestCheckBlogLimit(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		assert.NoError(t, ds.ensureMaxBlogsColumn())

		cfg := config.New()
		cfg.App.MaxBlogs = 1
		app := &App{db: ds, cfg: cfg}

		res, err := ds.Exec(
			"INSERT INTO users (username, password, email, created) VALUES (?, ?, ?, NOW())",
			"capuser", "x", nil)
		assert.NoError(t, err)
		uid, _ := res.LastInsertId()

		addBlog := func(alias string) {
			_, err := ds.Exec(
				"INSERT INTO collections (alias, title, description, privacy, owner_id, view_count) VALUES (?, ?, '', 1, ?, 0)",
				alias, alias, uid)
			assert.NoError(t, err)
		}

		// Ponce: 3 blogs.
		_, err = ds.Exec("UPDATE users SET max_blogs = ? WHERE id = ?", 3, uid)
		assert.NoError(t, err)

		assert.NoError(t, app.checkBlogLimit(uid), "0 of 3 should be allowed")
		addBlog("cap-a")
		addBlog("cap-b")
		assert.NoError(t, app.checkBlogLimit(uid), "2 of 3 should be allowed")

		addBlog("cap-c")
		err = app.checkBlogLimit(uid)
		assert.Error(t, err, "3 of 3 must be refused")
		httpErr, ok := err.(impart.HTTPError)
		assert.True(t, ok, "expected impart.HTTPError, got %T", err)
		assert.Equal(t, http.StatusForbidden, httpErr.Status)

		// Zero means unlimited, even when already over the old cap.
		_, err = ds.Exec("UPDATE users SET max_blogs = ? WHERE id = ?", 0, uid)
		assert.NoError(t, err)
		assert.NoError(t, app.checkBlogLimit(uid), "0 means unlimited")

		// NULL falls back to the config value of 1, and the user has 3.
		_, err = ds.Exec("UPDATE users SET max_blogs = NULL WHERE id = ?", uid)
		assert.NoError(t, err)
		assert.Error(t, app.checkBlogLimit(uid), "NULL should fall back to cfg.App.MaxBlogs = 1")
	})
}

func TestCountRequestedNewBlogs(t *testing.T) {
	mk := func(alias string, create bool) ClaimPostRequest {
		return ClaimPostRequest{CollectionAlias: alias, CreateCollection: create}
	}

	tests := []struct {
		name      string
		claims    []ClaimPostRequest
		collAlias string
		want      int
	}{
		{"nil-safe", nil, "", 0},
		{"no creates", []ClaimPostRequest{mk("a", false), mk("b", false)}, "", 0},
		{"one create", []ClaimPostRequest{mk("a", true)}, "", 1},
		{"three distinct creates", []ClaimPostRequest{mk("a", true), mk("b", true), mk("c", true)}, "", 3},
		{"duplicates count once", []ClaimPostRequest{mk("a", true), mk("a", true)}, "", 1},
		{"create with empty alias is not a blog", []ClaimPostRequest{mk("", true)}, "", 0},
		{"url alias wins over per-post alias", []ClaimPostRequest{mk("a", true), mk("b", true)}, "fromurl", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var c *[]ClaimPostRequest
			if tc.claims != nil {
				c = &tc.claims
			}
			assert.Equal(t, tc.want, countRequestedNewBlogs(c, tc.collAlias))
		})
	}
}

func TestCheckBlogLimitN(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		assert.NoError(t, ds.ensureMaxBlogsColumn())

		cfg := config.New()
		cfg.App.MaxBlogs = 1
		app := &App{db: ds, cfg: cfg}

		res, err := ds.Exec(
			"INSERT INTO users (username, password, email, created) VALUES (?, ?, ?, NOW())",
			"bulkuser", "x", nil)
		assert.NoError(t, err)
		uid, _ := res.LastInsertId()

		_, err = ds.Exec("UPDATE users SET max_blogs = ? WHERE id = ?", 3, uid)
		assert.NoError(t, err)

		// Nothing created yet: room for exactly three, not four.
		assert.NoError(t, app.checkBlogLimitN(uid, 3), "3 of 3 at once must be allowed")
		assert.Error(t, app.checkBlogLimitN(uid, 4), "4 of 3 at once must be refused")

		_, err = ds.Exec(
			"INSERT INTO collections (alias, title, description, privacy, owner_id, view_count) VALUES (?, ?, '', 1, ?, 0)",
			"bulk-a", "bulk-a", uid)
		assert.NoError(t, err)

		// One used: room for two more, not three.
		assert.NoError(t, app.checkBlogLimitN(uid, 2))
		assert.Error(t, app.checkBlogLimitN(uid, 3),
			"a bulk request must not be able to exceed the cap in one call")

		// The single-blog wrapper keeps its original meaning.
		assert.NoError(t, app.checkBlogLimit(uid))
	})
}

func TestSetUserMaxBlogs(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		assert.NoError(t, ds.ensureMaxBlogsColumn())

		_, err := ds.Exec(
			"INSERT INTO users (username, password, email, created) VALUES (?, ?, ?, NOW())",
			"setuser", "x", nil)
		assert.NoError(t, err)

		assert.NoError(t, ds.SetUserMaxBlogs("setuser", 15))

		var got sql.NullInt64
		assert.NoError(t, ds.QueryRow("SELECT max_blogs FROM users WHERE username = ?", "setuser").Scan(&got))
		assert.True(t, got.Valid)
		assert.Equal(t, int64(15), got.Int64)

		// Setting the same value again must still succeed. MySQL reports zero
		// affected rows for a no-op UPDATE, so an implementation keying off
		// RowsAffected would wrongly report the user as missing.
		assert.NoError(t, ds.SetUserMaxBlogs("setuser", 15))

		assert.Equal(t, ErrUserNotFound, ds.SetUserMaxBlogs("nobody", 3))
	})
}

func TestHandleSetMaxBlogsAuth(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		assert.NoError(t, ds.ensureMaxBlogsColumn())
		_, err := ds.Exec(
			"INSERT INTO users (username, password, email, created) VALUES (?, ?, ?, NOW())",
			"authuser", "x", nil)
		assert.NoError(t, err)

		app := &App{db: ds, cfg: config.New()}
		secret := "0123456789abcdef0123456789abcdef"
		t.Setenv("WRITEFREELY_API_SECRET", secret)

		router := mux.NewRouter()
		router.HandleFunc("/api/internal/user/{username}/max-blogs",
			handleSetMaxBlogs(app)).Methods("POST")

		post := func(user, sec string, body string) int {
			r := httptest.NewRequest("POST",
				"/api/internal/user/"+user+"/max-blogs", strings.NewReader(body))
			if sec != "" {
				r.Header.Set("X-WriteFreely-Secret", sec)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			return w.Code
		}

		assert.Equal(t, http.StatusUnauthorized, post("authuser", "", `{"max_blogs":3}`),
			"missing secret must be rejected")
		assert.Equal(t, http.StatusUnauthorized, post("authuser", "wrong-but-32-chars-long-xxxxxxxx", `{"max_blogs":3}`),
			"wrong secret must be rejected")
		assert.Equal(t, http.StatusOK, post("authuser", secret, `{"max_blogs":3}`))
		assert.Equal(t, http.StatusNotFound, post("nobody", secret, `{"max_blogs":3}`))
		assert.Equal(t, http.StatusBadRequest, post("authuser", secret, `not json`))
		assert.Equal(t, http.StatusBadRequest, post("authuser", secret, `{"max_blogs":-1}`))

		// An absent, null, zero or misspelled max_blogs must be a 400, never a
		// silent success. 0 means UNLIMITED to effectiveMaxBlogs, so accepting
		// any of these would remove the member's cap and return 200.
		for _, bad := range []string{`{}`, `{"max_blogs":null}`, `{"max_blogs":0}`, `{"maxblogs":5}`, `{"max_blogs":99999}`} {
			assert.Equal(t, http.StatusBadRequest, post("authuser", secret, bad),
				"payload %s must be refused", bad)
		}

		// ...and none of them may have changed the stored value.
		var after sql.NullInt64
		assert.NoError(t, ds.QueryRow("SELECT max_blogs FROM users WHERE username = ?", "authuser").Scan(&after))
		assert.True(t, after.Valid)
		assert.Equal(t, int64(3), after.Int64, "a refused payload must not modify the allowance")

		var got sql.NullInt64
		assert.NoError(t, ds.QueryRow("SELECT max_blogs FROM users WHERE username = ?", "authuser").Scan(&got))
		assert.Equal(t, int64(3), got.Int64)
	})
}

func TestHandleSetMaxBlogsRefusesWeakSecret(t *testing.T) {
	app := &App{cfg: config.New()}
	t.Setenv("WRITEFREELY_API_SECRET", "tooshort")

	router := mux.NewRouter()
	router.HandleFunc("/api/internal/user/{username}/max-blogs",
		handleSetMaxBlogs(app)).Methods("POST")

	r := httptest.NewRequest("POST", "/api/internal/user/x/max-blogs",
		strings.NewReader(`{"max_blogs":3}`))
	r.Header.Set("X-WriteFreely-Secret", "tooshort")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"a short secret must fail closed, never authorise")
}

func TestHandleSetMaxBlogsRefusesUnsetSecret(t *testing.T) {
	app := &App{cfg: config.New()}
	// Genuinely unset, not merely short — this is the state a misconfigured
	// deploy is actually in, and it must not be treated as permission.
	t.Setenv("WRITEFREELY_API_SECRET", "")

	router := mux.NewRouter()
	router.HandleFunc("/api/internal/user/{username}/max-blogs",
		handleSetMaxBlogs(app)).Methods("POST")

	r := httptest.NewRequest("POST", "/api/internal/user/x/max-blogs",
		strings.NewReader(`{"max_blogs":3}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"an unset secret must fail closed")
}
