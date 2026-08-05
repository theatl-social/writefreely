/*
 * Copyright © 2026 theATL.social.
 *
 * This file is part of the theATL.social fork of WriteFreely and is licensed
 * under the GNU Affero General Public License, included in the LICENSE file in
 * this source code package.
 *
 * Per-user blog limits. See FORK.md.
 */

package writefreely

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/gorilla/mux"
	"github.com/writeas/impart"
	"github.com/writeas/web-core/log"
	"github.com/writefreely/writefreely/config"
)

// ensureMaxBlogsColumn adds users.max_blogs if it is missing.
//
// This deliberately does NOT use the migrations package: migrations.go registers
// migrations in a flat slice where CurrentVer() == len(migrations), so appending
// ours would collide with the next upstream migration and re-run our ALTER against
// an existing column. See FORK.md.
//
// It is driver-agnostic by probing rather than branching on PRAGMA versus
// information_schema, and it tolerates an uninitialised database because
// ConnectToDatabase runs on every serve, including before --init-db has been run.
func (db *datastore) ensureMaxBlogsColumn() error {
	var v sql.NullInt64

	// Does the column already exist?
	err := db.QueryRow("SELECT max_blogs FROM users LIMIT 1").Scan(&v)
	if err == nil || err == sql.ErrNoRows {
		return nil
	}

	// The column is absent — but the identical error appears when `users` itself
	// is missing, which is the normal state on a fresh deploy before --init-db
	// has run. Distinguish the two EXPLICITLY rather than inferring: treating any
	// error as "table missing" would swallow a permission failure, a lock-wait
	// timeout or a dropped connection, log a false reason, and leave max_blogs
	// permanently unadded with nothing visible to an operator.
	//
	// SHOW TABLES LIKE is the same technique migrations.go:135 uses for this.
	var name string
	switch tblErr := db.QueryRow("SHOW TABLES LIKE 'users'").Scan(&name); {
	case tblErr == sql.ErrNoRows:
		log.Info("max_blogs: users table not present yet; skipping. Run `writefreely --init-db`, then restart.")
		return nil
	case tblErr != nil:
		return fmt.Errorf("checking for users table: %w", tblErr)
	}

	log.Info("max_blogs: adding users.max_blogs column...")
	if _, err := db.Exec("ALTER TABLE users ADD COLUMN max_blogs INT DEFAULT NULL"); err != nil {
		return err
	}
	log.Info("max_blogs: column added.")
	return nil
}

// GetUserMaxBlogs returns the user's per-user blog limit. An invalid (NULL)
// result means no explicit limit is set for this user.
func (db *datastore) GetUserMaxBlogs(userID int64) (sql.NullInt64, error) {
	var max sql.NullInt64
	err := db.QueryRow("SELECT max_blogs FROM users WHERE id = ?", userID).Scan(&max)
	if err == sql.ErrNoRows {
		return max, ErrUserNotFound
	}
	return max, err
}

// effectiveMaxBlogs resolves the limit that applies to a user: their explicit
// per-user value if set, otherwise the instance-wide config default. A result
// of zero or less means unlimited, matching config.AppCfg.CanCreateBlogs.
func effectiveMaxBlogs(cfg *config.Config, perUser sql.NullInt64) int {
	if perUser.Valid {
		return int(perUser.Int64)
	}
	return cfg.App.MaxBlogs
}

// checkBlogLimit reports whether the user may create one more blog.
func (app *App) checkBlogLimit(userID int64) error {
	return app.checkBlogLimitN(userID, 1)
}

// checkBlogLimitN reports whether the user may create `wanted` more blogs.
//
// This is the enforcement upstream lacks: config.AppCfg.CanCreateBlogs exists but
// its only caller is account.go, where it merely hides a button in the web UI.
// Neither newCollection nor the /posts/claim path consulted any limit.
func (app *App) checkBlogLimitN(userID int64, wanted int) error {
	if wanted <= 0 {
		return nil
	}

	perUser, err := app.db.GetUserMaxBlogs(userID)
	if err != nil {
		log.Error("max_blogs: lookup failed for user %d: %v", userID, err)
		return ErrInternalGeneral
	}

	limit := effectiveMaxBlogs(app.cfg, perUser)
	if limit <= 0 {
		return nil
	}

	count, err := app.db.GetUserCollectionCount(userID)
	if err != nil {
		log.Error("max_blogs: count failed for user %d: %v", userID, err)
		return ErrInternalGeneral
	}

	// limit is guaranteed > 0 here, so converting it to uint64 cannot wrap.
	if count+uint64(wanted) > uint64(limit) {
		return impart.HTTPError{
			Status:  http.StatusForbidden,
			Message: "You've reached the number of blogs included with your membership.",
		}
	}
	return nil
}

// countRequestedNewBlogs reports how many distinct blogs a claim-posts payload
// asks to create.
//
// POST /api/posts/claim accepts create_collection per post and reaches
// db.CreateCollection directly (database.go:1756), bypassing newCollection and
// its limit check entirely — so the allowance has to be enforced on this path
// too. Distinct aliases are counted because one payload may ask for several.
//
// Aliases that already exist are counted as well, which is deliberately
// conservative: CreateCollection would reject them anyway, and over-counting
// refuses slightly early rather than letting a request slip past the cap.
func countRequestedNewBlogs(claims *[]ClaimPostRequest, collAlias string) int {
	if claims == nil {
		return 0
	}
	seen := make(map[string]bool)
	for _, c := range *claims {
		if !c.CreateCollection {
			continue
		}
		// ClaimPosts uses the URL alias when the route supplies one, and the
		// per-post alias otherwise (database.go:1743-1748).
		alias := c.CollectionAlias
		if collAlias != "" {
			alias = collAlias
		}
		if alias != "" {
			seen[alias] = true
		}
	}
	return len(seen)
}

// SetUserMaxBlogs sets a user's blog allowance by username.
//
// The row is located with a SELECT before the UPDATE rather than relying on
// RowsAffected: MySQL reports zero affected rows when an UPDATE sets a column to
// the value it already holds, which would make a legitimate no-op look like a
// missing user.
func (db *datastore) SetUserMaxBlogs(username string, max int) error {
	var id int64
	err := db.QueryRow("SELECT id FROM users WHERE username = ?", username).Scan(&id)
	if err == sql.ErrNoRows {
		return ErrUserNotFound
	} else if err != nil {
		return err
	}

	_, err = db.Exec("UPDATE users SET max_blogs = ? WHERE id = ?", max, id)
	return err
}

// handleSetMaxBlogs serves POST /api/internal/user/{username}/max-blogs.
//
// Authorisation is a shared secret in X-WriteFreely-Secret, checked in constant
// time. This route is ALSO denied at the reverse proxy; the secret is the second
// layer, present because this fork is public and therefore documents the route's
// existence to anyone who reads it.
func handleSetMaxBlogs(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := os.Getenv("WRITEFREELY_API_SECRET")
		if len(secret) < 32 {
			log.Error("max_blogs: WRITEFREELY_API_SECRET unset or under 32 chars; refusing request")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		provided := r.Header.Get("X-WriteFreely-Secret")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(secret)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var body struct {
			MaxBlogs int `json:"max_blogs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.MaxBlogs < 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		username := mux.Vars(r)["username"]
		if err := app.db.SetUserMaxBlogs(username, body.MaxBlogs); err != nil {
			if err == ErrUserNotFound {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			log.Error("max_blogs: set failed for %s: %v", username, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		log.Info("max_blogs: set %s to %d", username, body.MaxBlogs)
		w.WriteHeader(http.StatusOK)
	}
}
