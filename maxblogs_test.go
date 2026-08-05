package writefreely

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
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
