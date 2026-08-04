package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func decode(t *testing.T, rr *httptest.ResponseRecorder) Report {
	t.Helper()
	var rep Report
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return rep
}

// TestLiveDoesNotTouchDependencies is the point of having two endpoints.
//
// Liveness must answer "is this process wedged?" and nothing else. If it
// pinged the database, a database outage would make every replica fail its
// liveness probe at once, and the orchestrator would kill and restart all of
// them — turning a recoverable dependency failure into a far worse outage.
//
// Both dependencies are nil here, which would panic or fail if Live touched
// them.
func TestLiveDoesNotTouchDependencies(t *testing.T) {
	h := &Health{DB: nil, Redis: nil}

	rr := httptest.NewRecorder()
	h.Live(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with dependencies down", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestLiveStaysOKWhileDraining(t *testing.T) {
	// Liveness must remain OK during a drain: the process is healthy and
	// finishing work. Only readiness should turn negative, or the orchestrator
	// would SIGKILL the container mid-drain.
	h := &Health{}
	h.StartDraining()

	rr := httptest.NewRecorder()
	h.Live(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rr.Code != http.StatusOK {
		t.Errorf("liveness status = %d while draining, want 200", rr.Code)
	}
}

func TestReadyReportsDrainingBeforeCheckingAnything(t *testing.T) {
	h := &Health{DB: nil, Redis: nil}
	h.StartDraining()

	rr := httptest.NewRecorder()
	h.Ready(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 while draining", rr.Code)
	}
	if rep := decode(t, rr); rep.Status != "draining" {
		t.Errorf("status = %q, want draining", rep.Status)
	}
}

func TestReadyIsUnavailableWithoutPostgres(t *testing.T) {
	// Postgres is fatal: without it not a single link can be resolved.
	h := &Health{DB: nil, Redis: nil}

	rr := httptest.NewRecorder()
	h.Ready(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 with no database", rr.Code)
	}
	rep := decode(t, rr)
	if rep.Status != "unavailable" {
		t.Errorf("status = %q, want unavailable", rep.Status)
	}
	if rep.Dependencies["postgres"] == "ok" {
		t.Error("postgres reported ok when it is not configured")
	}
}

func TestReadyTreatsDisabledRedisAsFine(t *testing.T) {
	// Cache disabled is a supported configuration, not a degradation.
	h := &Health{DB: nil, Redis: nil}

	rr := httptest.NewRecorder()
	h.Ready(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if got := decode(t, rr).Dependencies["redis"]; got != "disabled" {
		t.Errorf("redis = %q, want disabled", got)
	}
}

func TestReadyResponseIsNeverCached(t *testing.T) {
	h := &Health{}
	rr := httptest.NewRecorder()
	h.Ready(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store; a cached readiness "+
			"response would keep a dead instance in rotation", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestRouterRegistersHealthEndpoints(t *testing.T) {
	h := &Health{}
	srv := httptest.NewServer(NewRouter(Deps{Health: h}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
}

// TestReservedListCoversRegisteredRoutes is the guard that stops a future
// route from being shadowed by a user-created alias. It is deliberately here,
// next to the router, rather than in the alias package: the risk is introduced
// by adding a route, so the failure should surface where routes are declared.
//
// M7 extends this to walk the live route tree once the alias catch-all exists.
//
// The paths come from a real registration pass with every dependency present,
// not from a slice declared beside the router. Reading a declared slice made
// this guard a tautology in one direction — see F85 and
// TestEveryRegisteredAppRouteIsMountedOnTheRoot.
func TestReservedListCoversRegisteredRoutes(t *testing.T) {
	app := newAppMux()
	registerAppRoutes(maximalDeps(), app)
	if len(app.patterns) < patternFloor {
		t.Fatalf("a maximal registration pass produced only %d patterns, want at least %d; "+
			"maximalDeps has stopped filling something and this guard is now checking almost nothing",
			len(app.patterns), patternFloor)
	}
	registered := topLevelSegments(append(app.mounts(), infrastructurePatterns...))

	for _, path := range registered {
		t.Run(path, func(t *testing.T) {
			if !isReserved(path) {
				t.Errorf("route %q is registered but not in internal/alias/reserved.txt; "+
					"a user could create an alias that shadows it", path)
			}
		})
	}
}
