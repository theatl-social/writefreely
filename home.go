/*
 * Copyright © 2026 theATL.social
 *
 * This file is part of the theATL.social fork of WriteFreely.
 *
 * WriteFreely is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License, included
 * in the LICENSE file in this source code package.
 */

package writefreely

import (
	"net/http"
	"time"

	"github.com/writeas/impart"
	"github.com/writeas/web-core/log"
	"github.com/writeas/web-core/memo"
)

const (
	// homePostLimit is how many recent posts the home digest shows before
	// deferring to /read.
	homePostLimit = 10
	// homeBlogLimit is how many blogs the home digest's secondary strip shows
	// before deferring to /blogs.
	homeBlogLimit = 8
	// blogsPerPage is the /blogs directory's page size.
	blogsPerPage = 24
)

// HomeBlog is one row of the active-blogs query. It follows CollectionObj's
// pattern (collections.go): embed Collection, add the aggregates Collection
// has no field for. Embedding means CanonicalURL(), DisplayTitle(), and the
// unexported hostName come for free, and the columns the feed does not need
// (style_sheet, script, format, post_signature) simply go unscanned.
type HomeBlog struct {
	Collection
	LastPost  time.Time
	PostCount int64
}

// fetchActiveBlogs returns every public blog with at least one published post,
// ordered by most recent post first. It satisfies memo.Func, which is why it
// returns interface{} rather than []HomeBlog.
//
// INNER JOIN posts (not LEFT JOIN) is what excludes blogs with no posts at
// all; `p.created <= now()` excludes scheduled-future posts, matching
// FetchPublicPosts in read.go; u.status = 0 is UserActive, so a silenced
// owner's blogs drop out of the directory automatically.
func (app *App) fetchActiveBlogs() (interface{}, error) {
	rows, err := app.db.Query(`SELECT c.id, c.alias, c.title, c.description, c.view_count,
		MAX(p.created) AS last_post, COUNT(p.id) AS post_count
	FROM collections c
	INNER JOIN users u ON u.id = c.owner_id
	INNER JOIN posts p ON p.collection_id = c.id
	WHERE c.privacy = 1 AND u.status = 0 AND p.created <= ` + app.db.now() + `
	GROUP BY c.id
	ORDER BY last_post DESC`)
	if err != nil {
		log.Error("[HOME] Failed selecting active blogs: %v", err)
		return nil, impart.HTTPError{Status: http.StatusInternalServerError, Message: "Couldn't retrieve blogs."}
	}
	defer rows.Close()

	blogs := []HomeBlog{}
	for rows.Next() {
		b := HomeBlog{}
		if err := rows.Scan(&b.ID, &b.Alias, &b.Title, &b.Description, &b.Views,
			&b.LastPost, &b.PostCount); err != nil {
			log.Error("[HOME] Unable to scan blog row, skipping: %v", err)
			continue
		}
		// Collection.CanonicalURL() reads the unexported hostName; without
		// this the template renders a host-less URL.
		b.hostName = app.cfg.App.Host
		b.Public = true
		blogs = append(blogs, b)
	}
	if err := rows.Err(); err != nil {
		log.Error("[HOME] Error after Next() on blog rows: %v", err)
	}

	return blogs, nil
}

// homeFeed caches the active-blogs query. It deliberately mirrors
// localTimeline (read.go): a memo.Memo plus the last good result, refreshed
// lazily on read rather than by a background ticker.
type homeFeed struct {
	m     *memo.Memo
	blogs *[]HomeBlog
}

func initHomeFeed(app *App) {
	app.homeFeed = &homeFeed{
		m: memo.New(app.fetchActiveBlogs, tlCacheDur),
	}
}

// updateHomeBlogsCache refreshes the blogs cache if it is cold, if reset is
// true, or if the memo's TTL has elapsed. It follows updateTimelineCache's
// shape (read.go), including its failure behaviour: on a query error it logs
// and leaves the previous result in place, because half a stale home page
// beats an empty one.
func updateHomeBlogsCache(app *App, reset bool) {
	tl := app.homeFeed
	if tl == nil {
		return
	}
	if reset {
		tl.m.Reset()
	}

	if tl.blogs == nil || reset || tl.m.Invalidate() {
		log.Info("[HOME] Updating active blogs cache")

		blogsInterface, err := tl.m.Get()
		if err != nil {
			log.Error("[HOME] Unable to cache blogs: %v", err)
			return
		}
		cast := blogsInterface.([]HomeBlog)
		tl.blogs = &cast
	}
}
