package writefreely

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/writefreely/writefreely/config"
)

// seedBlog inserts a user, a collection, and n posts, and returns the
// collection id. `privacy` is the collection's visibility (1 = public),
// `status` is the user's (0 = active). `postAge` is a MySQL interval
// expression applied to the post's created timestamp, e.g. "INTERVAL 1 DAY"
// for a day-old post or "INTERVAL -1 DAY" for one scheduled in the future.
func seedBlog(t *testing.T, db *sql.DB, alias string, privacy, status int, postAges []string) int64 {
	t.Helper()

	res, err := db.Exec(
		"INSERT INTO users (username, password, email, created, status) VALUES (?, 'x', '', NOW(), ?)",
		alias+"-owner", status)
	require.NoError(t, err)
	ownerID, err := res.LastInsertId()
	require.NoError(t, err)

	res, err = db.Exec(
		"INSERT INTO collections (alias, title, description, privacy, owner_id, view_count) VALUES (?, ?, '', ?, ?, 0)",
		alias, "Title "+alias, privacy, ownerID)
	require.NoError(t, err)
	collID, err := res.LastInsertId()
	require.NoError(t, err)

	for i, age := range postAges {
		_, err = db.Exec(
			"INSERT INTO posts (id, slug, privacy, owner_id, collection_id, created, updated, view_count, title, content, text_appearance) "+
				"VALUES (?, ?, 1, ?, ?, DATE_SUB(NOW(), "+age+"), NOW(), 0, 'T', 'C', 'norm')",
			alias+"-post-"+string(rune('a'+i)), "slug-"+string(rune('a'+i)), ownerID, collID)
		require.NoError(t, err)
	}
	return collID
}

func aliasesOf(blogs []HomeBlog) []string {
	out := make([]string, 0, len(blogs))
	for _, b := range blogs {
		out = append(out, b.Alias)
	}
	return out
}

func TestFetchActiveBlogs(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		cfg := config.New()
		cfg.App.SingleUser = false
		cfg.App.Host = "https://write.example.test"
		app := &App{db: ds, cfg: cfg}

		// Eligible, oldest activity.
		seedBlog(t, db, "stale", 1, 0, []string{"INTERVAL 30 DAY"})
		// Eligible, newest activity — must sort first.
		seedBlog(t, db, "fresh", 1, 0, []string{"INTERVAL 30 DAY", "INTERVAL 1 HOUR"})
		// Ineligible for four distinct reasons.
		seedBlog(t, db, "unlisted", 0, 0, []string{"INTERVAL 1 HOUR"})
		seedBlog(t, db, "silenced", 1, 1, []string{"INTERVAL 1 HOUR"})
		seedBlog(t, db, "empty", 1, 0, nil)
		seedBlog(t, db, "scheduled", 1, 0, []string{"INTERVAL -1 DAY"})

		got, err := app.fetchActiveBlogs()
		require.NoError(t, err)
		blogs := got.([]HomeBlog)

		assert.Equal(t, []string{"fresh", "stale"}, aliasesOf(blogs),
			"only public blogs with at least one already-published post, most recent first")

		require.Len(t, blogs, 2)
		assert.Equal(t, int64(2), blogs[0].PostCount, "fresh has two published posts")
		assert.Equal(t, int64(1), blogs[1].PostCount)
		assert.False(t, blogs[0].LastPost.IsZero(), "LastPost must be scanned, not left zero")
		assert.Equal(t, "https://write.example.test/fresh/", blogs[0].CanonicalURL(),
			"hostName must be set after scan or CanonicalURL loses the host")
	})
}
