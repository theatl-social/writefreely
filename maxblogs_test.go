package writefreely

import (
	"database/sql"
	"net/http"
	"testing"

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
