/*
 * Copyright © 2026 theATL.social.
 *
 * This file is part of the theATL.social fork of WriteFreely and is licensed
 * under the GNU Affero General Public License, included in the LICENSE file in
 * this source code package.
 *
 * Basic guards on per-blog custom CSS. See FORK.md.
 */

package writefreely

import (
	"fmt"
	"net/http"
	"regexp"

	"github.com/writeas/impart"
)

// styleSheetMaxLength bounds custom per-blog CSS. The style_sheet column is
// MySQL TEXT, capped at 65,535 bytes; this leaves headroom while still being
// far larger than any legitimate blog stylesheet needs to be.
const styleSheetMaxLength = 50000

// externalResourcePattern is a basic guard, not a full CSS sanitizer: it
// catches plain @import and url() with an absolute target, not obfuscated
// forms (CSS escape sequences, etc). Custom CSS is rendered inline via
// template.CSS() (collections.go), which disables Go's auto-escaping, so an
// external @import/url() would let a compromised blog owner's account pull in
// arbitrary remote CSS or exfiltrate data through image-load side channels.
// Relative and data: URLs are left alone since neither can reach off-instance.
var externalResourcePattern = regexp.MustCompile(`(?i)@import|url\(\s*['"]?(?:[a-zA-Z][a-zA-Z0-9+.\-]*://|//)`)

// validateStyleSheet rejects custom CSS that's too long or references
// external resources. A nil style means the caller isn't updating it, so
// there's nothing to check.
func validateStyleSheet(style *string) error {
	if style == nil {
		return nil
	}
	if len(*style) > styleSheetMaxLength {
		return impart.HTTPError{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("Custom CSS is too long (max %d characters).", styleSheetMaxLength),
		}
	}
	if externalResourcePattern.MatchString(*style) {
		return impart.HTTPError{
			Status:  http.StatusBadRequest,
			Message: "Custom CSS can't reference external resources (@import or an absolute url()).",
		}
	}
	return nil
}
