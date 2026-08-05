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
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"

	"github.com/gorilla/mux"
	"github.com/writeas/web-core/log"
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

// maxBlogsCeiling bounds what the setter will accept. The column is INT, so an
// unbounded value either errors under MySQL strict mode or truncates silently.
const maxBlogsCeiling = 1000

// handleSetMaxBlogs serves POST /api/internal/user/{username}/max-blogs.
//
// Authorisation is a shared secret in X-WriteFreely-Secret, checked in constant
// time BEFORE the body is read or the username resolved, so an unauthenticated
// prober cannot use this endpoint as a username-existence oracle. The route is
// ALSO denied at the reverse proxy; the secret is the second layer, present
// because this fork is public and therefore documents the route's existence.
func handleSetMaxBlogs(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := os.Getenv("WRITEFREELY_API_SECRET")
		if len(secret) < 32 {
			log.Error("max_blogs: WRITEFREELY_API_SECRET unset or under 32 chars; refusing request")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		// Hash both sides first: ConstantTimeCompare returns early when the
		// lengths differ, which would leak the secret's length to timing.
		provided := sha256.Sum256([]byte(r.Header.Get("X-WriteFreely-Secret")))
		expected := sha256.Sum256([]byte(secret))
		if subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// MaxBlogs is a *int so an absent field is distinguishable from an
		// explicit 0. This matters far more than it looks: 0 means UNLIMITED to
		// effectiveMaxBlogs, so decoding a missing field as 0 would make a
		// malformed request silently REMOVE a member's cap and return 200.
		// DisallowUnknownFields turns a misspelled key into a 400 rather than
		// the same silent grant.
		var body struct {
			MaxBlogs *int `json:"max_blogs"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil || body.MaxBlogs == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Tier allowances are 1, 3 and 15. Zero is REJECTED rather than treated
		// as unlimited: nothing in the member site ever intends it, and accepting
		// it would put an unlimited grant one typo away from a downgrade job.
		if *body.MaxBlogs < 1 || *body.MaxBlogs > maxBlogsCeiling {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		username := mux.Vars(r)["username"]
		if err := app.db.SetUserMaxBlogs(username, *body.MaxBlogs); err != nil {
			if err == ErrUserNotFound {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			log.Error("max_blogs: set failed for %q: %v", username, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Echo what was applied so the member site can verify rather than assume.
		log.Info("max_blogs: set %q to %d", username, *body.MaxBlogs)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"username":  username,
			"max_blogs": *body.MaxBlogs,
		})
	}
}
