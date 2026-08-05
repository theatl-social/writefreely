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
	"database/sql"
	"fmt"
	"net/http"

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

// checkBlogLimit returns an error if the user has reached their blog allowance.
// A limit of zero or less means unlimited.
//
// This is the enforcement upstream lacks: config.AppCfg.CanCreateBlogs exists but
// its only caller is account.go, where it merely hides a button in the web UI.
// newCollection never consulted any limit, so POST /api/collections was unbounded.
func (app *App) checkBlogLimit(userID int64) error {
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

	// limit is guaranteed > 0 here, so it is safe to convert to uint64 for the
	// comparison against count without a negative value wrapping around.
	if count >= uint64(limit) {
		return impart.HTTPError{
			Status:  http.StatusForbidden,
			Message: "You've reached the number of blogs included with your membership.",
		}
	}
	return nil
}
