package writefreely

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
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
