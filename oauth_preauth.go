/*
 * Copyright © 2026 theATL.social.
 *
 * This file is part of the theATL.social fork of WriteFreely and is licensed
 * under the GNU Affero General Public License, included in the LICENSE file in
 * this source code package.
 *
 * OAuth just-in-time provisioning: a member's Mastodon identity is
 * pre-authorized in advance (by the member site, over the internal network),
 * and an account is created automatically on their first real OAuth login.
 * See FORK.md.
 */

package writefreely

import (
	"github.com/writeas/web-core/log"
)

// ensureOauthPreauthTable creates oauth_preauth if it does not exist.
//
// Deliberately NOT using the migrations system, for the same reason as
// users.max_blogs: migrations.go's CurrentVer() is len(migrations), so a
// fork-owned entry collides with the next upstream migration on merge.
func (db *datastore) ensureOauthPreauthTable() error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS oauth_preauth (
		remote_user_id VARCHAR(191) NOT NULL,
		provider       VARCHAR(191) NOT NULL,
		client_id      VARCHAR(191) NOT NULL,
		max_blogs      INT NOT NULL,
		created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (remote_user_id, provider, client_id)
	)`)
	if err != nil {
		return err
	}
	log.Info("oauth_preauth: table present.")
	return nil
}
