package writefreely

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/writefreely/writefreely/config"
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

		// Same fail-open regression coverage as the shipped setter.
		assert.Equal(t, http.StatusUnauthorized, post("54321", "", `{"max_blogs":3}`))
		assert.Equal(t, http.StatusBadRequest, post("54321", secret, `{}`))
		assert.Equal(t, http.StatusBadRequest, post("54321", secret, `{"max_blogs":0}`))
		assert.Equal(t, http.StatusBadRequest, post("54321", secret, `{"maxblogs":5}`))

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
	})
}
