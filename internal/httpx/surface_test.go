package httpx

import (
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// Every dashboard route the application mux is handed is counted as one.
//
// The classifier used to carry its own list of dashboard prefixes, written in
// Phase 1 and never updated, while eleven routes were added beside it —
// /notifications, /workspaces, /members, /invites, /organizations, /signup,
// /verify/, /disputes, /theme, /workspace/ and /feeds. Every one of them fell
// through to the default and was labelled `surface="redirect"` (F16). No request
// was served differently; the cost landed on the numbers, because the redirect
// surface's own latency figures were mixed with dashboard page loads whose
// budget is an order of magnitude larger.
//
// The assertion is driven from mounts() rather than from a list written here, so
// this test cannot drift from the routes either. A list checked against a copy of
// itself is what F16 was.
func TestEveryDashboardRouteIsCountedAsTheDashboard(t *testing.T) {
	app := newAppMux()
	registerAppRoutes(maximalDeps(), app)

	mounts := app.mounts()
	if len(mounts) < 10 {
		t.Fatalf("the application mux reported %d mounts, which is too few for this "+
			"to be testing anything real", len(mounts))
	}

	observability.SetWebPaths(mounts)

	checked := 0
	for _, mount := range mounts {
		// The root exact-match pattern has no path of its own; "/" is
		// classified explicitly and is covered below.
		if strings.HasPrefix(mount, "/{") {
			continue
		}
		path := strings.TrimSuffix(mount, "/")
		if path == "" {
			continue
		}
		checked++
		if got := observability.ClassifySurface(path); got != observability.SurfaceWeb {
			t.Errorf("%s is mounted on the application mux and classifies as %q, not "+
				"%q — its requests are counted and timed as short links, against a "+
				"latency budget an order of magnitude smaller (F16)",
				path, got, observability.SurfaceWeb)
		}
		// And a page below it, which is how these routes are actually reached.
		if sub := path + "/something"; observability.ClassifySurface(sub) != observability.SurfaceWeb {
			t.Errorf("%s classifies as %q, not %q", sub,
				observability.ClassifySurface(sub), observability.SurfaceWeb)
		}
	}

	if checked == 0 {
		t.Fatal("no mounted path was checked, so this test asserts nothing")
	}

	// The classification a short link must keep. An alias is exactly the shape
	// this test would break if SetWebPaths ever swallowed everything.
	if got := observability.ClassifySurface("/abc123"); got != observability.SurfaceRedirect {
		t.Errorf("a short link classifies as %q, want %q — the dashboard set has "+
			"widened to cover the redirect surface", got, observability.SurfaceRedirect)
	}
	if got := observability.ClassifySurface("/"); got != observability.SurfaceWeb {
		t.Errorf("the root classifies as %q, want %q", got, observability.SurfaceWeb)
	}
}
