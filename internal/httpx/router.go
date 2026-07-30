package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/config"
)

// Deps are the collaborators the router needs. Kept as an explicit struct so
// adding a dependency is a visible change rather than a hidden global.
type Deps struct {
	Config config.Config
	Health *Health
}

// NewRouter builds the application handler.
//
// The route table grows in later milestones. The shape it must keep is three
// groups with different middleware stacks:
//
//  1. the hot path, GET|HEAD /{alias}, registered last as the catch-all and
//     skipping session lookup, CSRF and template rendering
//  2. /api/v1/*, session-or-bearer auth, JSON problem responses
//  3. /dashboard/*, session + CSRF + HTML
//
// Operational endpoints are registered here first so they take precedence over
// the eventual catch-all. Every top-level path registered anywhere must also
// appear in internal/alias/reserved.txt, or an alias could shadow it; M7 adds
// a test that walks the live route tree and fails the build otherwise.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", d.Health.Live)
	mux.HandleFunc("GET /readyz", d.Health.Ready)

	// Placeholder root. Replaced by the dashboard in M11 and by the alias
	// catch-all in M7.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("LinkCtrl\n"))
	})

	return mux
}
