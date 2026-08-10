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
)

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
