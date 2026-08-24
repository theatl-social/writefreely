package author

import (
	"testing"

	"github.com/writefreely/writefreely/config"
)

// The /blogs directory route (routes.go) would be shadowed by a collection
// aliased "blogs", so both it and its singular are reserved.
func TestBlogAliasesAreReserved(t *testing.T) {
	cfg := config.New()
	cfg.Server.PagesParentDir = ".."

	for _, name := range []string{"blog", "blogs"} {
		if IsValidUsername(cfg, name) {
			t.Errorf("%q must be reserved: the /blogs route would shadow a collection with that alias", name)
		}
	}
}

// Guard against a fat-fingered map edit that reserves more than intended.
func TestOrdinaryUsernamesStillValid(t *testing.T) {
	cfg := config.New()
	cfg.Server.PagesParentDir = ".."

	for _, name := range []string{"michael", "atlwriter", "blogger", "weblog"} {
		if !IsValidUsername(cfg, name) {
			t.Errorf("%q should still be a valid username", name)
		}
	}
}
