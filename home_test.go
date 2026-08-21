package writefreely

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/sessions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/writeas/impart"
	"github.com/writeas/web-core/memo"
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

func TestUpdateHomeBlogsCache(t *testing.T) {
	if !runMySQLTests() {
		t.Skip("skipping mysql tests")
	}
	withTestDB(t, func(db *sql.DB) {
		ds := &datastore{DB: db, driverName: driverMySQL}
		cfg := config.New()
		cfg.App.SingleUser = false
		cfg.App.Host = "https://write.example.test"
		app := &App{db: ds, cfg: cfg}

		seedBlog(t, db, "one", 1, 0, []string{"INTERVAL 1 HOUR"})
		initHomeFeed(app)
		require.NotNil(t, app.homeFeed, "initHomeFeed must populate the field")
		assert.Nil(t, app.homeFeed.blogs, "cache starts cold, not eagerly fetched")

		updateHomeBlogsCache(app, false)
		require.NotNil(t, app.homeFeed.blogs)
		assert.Equal(t, []string{"one"}, aliasesOf(*app.homeFeed.blogs))

		// A failing query must not blank a populated cache: the home page
		// should keep serving slightly stale blogs rather than an empty strip.
		require.NoError(t, db.Close())
		updateHomeBlogsCache(app, true)
		require.NotNil(t, app.homeFeed.blogs, "a failed refresh must not nil the cache")
		assert.Equal(t, []string{"one"}, aliasesOf(*app.homeFeed.blogs),
			"stale data is served on refresh failure")
	})
}

// homeTestApp builds an App wired enough to render the home page: templates
// loaded, a cookie session store (pageForReq dereferences it), and both
// caches present but cold.
func homeTestApp(t *testing.T) *App {
	t.Helper()

	cfg := config.New()
	// config.New() defaults SingleUser to true, which is not this fork's
	// deployment shape and routes handleViewHome down a different branch.
	cfg.App.SingleUser = false
	cfg.App.LocalTimeline = true
	cfg.App.Host = "https://write.example.test"

	if err := InitTemplates(cfg); err != nil {
		t.Fatalf("InitTemplates: %v (expected templates/ and pages/ relative to the test binary's working directory)", err)
	}

	app := &App{
		cfg:          cfg,
		db:           homeTestDatastore(t),
		sessionStore: sessions.NewCookieStore([]byte("secret-key")),
	}
	initLocalTimeline(app)
	initHomeFeed(app)
	return app
}

// homeTestDatastore backs app.db with a real (in-memory) sqlite connection
// instead of leaving it nil.
//
// handleViewHome's existing (pre-fork) branches -- the landing page at
// ?landing=1, and the Pad for logged-in users -- both reach into app.db
// (landing fetches the configurable banner/body content via
// GetDynamicContent; the Pad fetches the user's blogs) regardless of this
// fork's change. A nil app.db panics the instant either path runs, which
// would make TestHandleViewHomeStillHonoursForcedLanding fail for a reason
// that has nothing to do with this task's routing change and is just as
// true before it as after. Only the `appcontent` table is created, which is
// all GetDynamicContent needs; every other db-shaped path some of these
// tests take (e.g. SingleUser's handleViewCollection, or the Pad's other
// queries) still hits a missing table and errors or panics, which those
// tests already tolerate or don't reach.
func homeTestDatastore(t *testing.T) *datastore {
	t.Helper()

	sqlDB, err := sql.Open("sqlite3_with_regex", ":memory:")
	require.NoError(t, err)
	// Keep every query on the same connection -- ":memory:" gives each new
	// connection its own empty database otherwise.
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })

	_, err = sqlDB.Exec(`CREATE TABLE appcontent (
		id TEXT NOT NULL PRIMARY KEY,
		title TEXT NOT NULL DEFAULT '',
		content TEXT NOT NULL DEFAULT '',
		updated DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		content_type TEXT NOT NULL DEFAULT ''
	)`)
	require.NoError(t, err)

	return &datastore{DB: sqlDB, driverName: driverSQLite}
}

func TestViewHomeRendersWithColdCaches(t *testing.T) {
	app := homeTestApp(t)

	// Neither cache has ever been populated. app.timeline.posts is a nil
	// *[]PublicPost here, which is the single most likely panic in this
	// feature: slicing it without a nil check dereferences nil.
	//
	// homeTestApp's App has a nil app.db, so the real database-backed memos
	// (app.FetchPublicPosts, app.fetchActiveBlogs) would panic on a nil
	// dereference the moment a cold cache triggers a fetch. That would look
	// like proof of the very nil-deref bug this test exists to prevent, when
	// it is really just a test fixture touching a database it was never given.
	// Substitute memos that fail cleanly instead, so the test exercises the
	// real production path: the first fetch fails at boot, updateTimelineCache
	// (and updateHomeBlogsCache) log and leave the cache nil, and viewHome
	// must render anyway without dereferencing it.
	app.timeline = &localTimeline{
		postsPerPage: tlPostsPerPage,
		m: memo.New(func() (interface{}, error) {
			return nil, errors.New("simulated fetch failure")
		}, tlCacheDur),
	}
	app.homeFeed = &homeFeed{
		m: memo.New(func() (interface{}, error) {
			return nil, errors.New("simulated fetch failure")
		}, tlCacheDur),
	}

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	require.NotPanics(t, func() {
		if err := viewHome(app, w, req); err != nil {
			t.Fatalf("viewHome: %v", err)
		}
	})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "<html", "the base template must have rendered")
	// The empty state is a first-class requirement, not a fallback: blogs
	// default to unlisted on this instance, so an empty feed is the expected
	// launch condition and has to explain how to appear in it.
	assert.Contains(t, w.Body.String(), "blog settings",
		"the empty state must tell people how to get listed")
}

func TestViewHome404sWhenLocalTimelineDisabled(t *testing.T) {
	app := homeTestApp(t)
	app.cfg.App.LocalTimeline = false

	err := viewHome(app, httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	httpErr, ok := err.(impart.HTTPError)
	require.True(t, ok, "expected an impart.HTTPError, got %T", err)
	assert.Equal(t, http.StatusNotFound, httpErr.Status,
		"without local_timeline there is no post cache to read, so the page must 404 rather than deref nil")
}

func TestViewHomeCapsPostsAtLimit(t *testing.T) {
	app := homeTestApp(t)

	posts := make([]PublicPost, homePostLimit+5)
	app.timeline.posts = &posts
	blogs := []HomeBlog{}
	app.homeFeed.blogs = &blogs

	// Asserts on the data, not the rendered HTML: zero-value PublicPosts are
	// fine to slice but not necessarily safe to render (Collection is nil and
	// CanonicalURL walks it). Render coverage lives in the cold-cache test,
	// where the slices are legitimately empty.
	req := httptest.NewRequest("GET", "/", nil)
	assert.Len(t, *homePageData(app, req).Posts, homePostLimit,
		"the digest shows exactly homePostLimit posts even when more are cached")
}

func TestViewHomeShowsFewerPostsThanLimit(t *testing.T) {
	app := homeTestApp(t)

	posts := make([]PublicPost, 3)
	app.timeline.posts = &posts
	blogs := []HomeBlog{}
	app.homeFeed.blogs = &blogs

	req := httptest.NewRequest("GET", "/", nil)
	assert.Len(t, *homePageData(app, req).Posts, 3,
		"slicing must not pad or over-read when fewer posts than the limit are cached")
}

// handleHome renders handleViewHome and returns the response, following one
// redirect's Location header rather than the body when a redirect is issued.
func handleHome(t *testing.T, app *App, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	err := handleViewHome(app, w, req)
	if err != nil {
		// impart.HTTPError carries redirects as Status 302 + Message = target.
		if httpErr, ok := err.(impart.HTTPError); ok {
			w.Code = httpErr.Status
			w.Header().Set("Location", httpErr.Message)
			return w
		}
		t.Fatalf("handleViewHome: %v", err)
	}
	return w
}

func TestHandleViewHomeShowsFeedToAnonymous(t *testing.T) {
	app := homeTestApp(t)

	w := handleHome(t, app, httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Active blogs",
		"anonymous visitors get the feed, not the landing page")
}

func TestHandleViewHomeShowsFeedToLoggedInUsers(t *testing.T) {
	app := homeTestApp(t)

	// Establish a session the same way the app does, then replay its cookie.
	setupW := httptest.NewRecorder()
	setupReq := httptest.NewRequest("GET", "/", nil)
	session, err := app.sessionStore.Get(setupReq, cookieName)
	require.NoError(t, err)
	session.Values[cookieUserVal] = &User{Username: "member"}
	require.NoError(t, session.Save(setupReq, setupW))

	req := httptest.NewRequest("GET", "/", nil)
	for _, c := range setupW.Result().Cookies() {
		req.AddCookie(c)
	}

	w := handleHome(t, app, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Active blogs",
		"logged-in members get the feed too -- this is the behaviour change; they used to land in the editor")
}

func TestHandleViewHomeStillHonoursForcedLanding(t *testing.T) {
	app := homeTestApp(t)

	w := handleHome(t, app, httptest.NewRequest("GET", "/?landing=1", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "Active blogs",
		"?landing=1 must still reach the landing page")
}

func TestHandleViewHomeLeavesSingleUserModeAlone(t *testing.T) {
	app := homeTestApp(t)
	app.cfg.App.SingleUser = true

	w := httptest.NewRecorder()
	func() {
		// Single-user mode returns at the first line of handleViewHome into
		// handleViewCollection, which needs a database this test App does not
		// have. Panicking or erroring there is fine -- the assertion is that we
		// got THERE rather than rendering the instance feed, which would mean
		// the fork's branch had been hoisted above the SingleUser return.
		defer func() { _ = recover() }()
		_ = handleViewHome(app, w, httptest.NewRequest("GET", "/", nil))
	}()
	assert.NotContains(t, w.Body.String(), "Active blogs",
		"single-user instances render their blog index at /, never the instance feed")
}

func TestHandleViewHomeStillRedirectsAnonymousOnPrivateInstance(t *testing.T) {
	app := homeTestApp(t)
	app.cfg.App.Private = true

	w := handleHome(t, app, httptest.NewRequest("GET", "/", nil))
	assert.NotContains(t, w.Body.String(), "Active blogs",
		"a private instance must not show the feed to anonymous visitors")
}
