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

	"github.com/writeas/web-core/log"
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

	// It does not — but distinguish "no column" from "no table".
	var probe int
	if tblErr := db.QueryRow("SELECT 1 FROM users LIMIT 1").Scan(&probe); tblErr != nil && tblErr != sql.ErrNoRows {
		log.Info("max_blogs: users table not present yet; skipping. Run `writefreely --init-db`, then restart.")
		return nil
	}

	log.Info("max_blogs: adding users.max_blogs column...")
	if _, err := db.Exec("ALTER TABLE users ADD COLUMN max_blogs INT DEFAULT NULL"); err != nil {
		return err
	}
	log.Info("max_blogs: column added.")
	return nil
}
