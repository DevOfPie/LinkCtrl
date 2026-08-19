package observability

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestClassifySurfaceIsBounded(t *testing.T) {
	cases := map[string]Surface{
		"/healthz":                SurfaceOps,
		"/readyz":                 SurfaceOps,
		"/api/v1/links":           SurfaceAPI,
		"/api/v1/links/abc/stats": SurfaceAPI,
		"/static/css/app.css":     SurfaceStatic,
		"/":                       SurfaceWeb,
		"/login":                  SurfaceWeb,
		"/dashboard":              SurfaceWeb,
		"/links":                  SurfaceWeb,
		"/links/019fb1-uuid":      SurfaceWeb,
		"/keys/abc/revoke":        SurfaceWeb,
		"/account/password":       SurfaceWeb,
		"/docs":                   SurfaceWeb,
		"/github":                 SurfaceRedirect,
		"/loginlike":              SurfaceRedirect,
		"/linksy":                 SurfaceRedirect,
		"/abc123":                 SurfaceRedirect,
		"/../../etc/passwd":       SurfaceRedirect,
		"":                        SurfaceRedirect,
		"/api":                    SurfaceRedirect,
	}
	for path, want := range cases {
		if got := ClassifySurface(path); got != want {
			t.Errorf("ClassifySurface(%q) = %q, want %q", path, got, want)
		}
	}
}

// The redirect namespace is chosen by whoever sends the request. If a path ever
// became part of a label, a scanner could mint unbounded series and take the
// process down through the metrics endpoint.
func TestSurfaceCardinalityIsFixed(t *testing.T) {
	seen := map[Surface]bool{}
	for _, path := range []string{
		"/a", "/b", "/c-random-1", "/c-random-2", "/api/v1/x", "/static/y",
		"/login", "/healthz", "/", "/zzz/nested/deep",
	} {
		seen[ClassifySurface(path)] = true
	}
	if len(seen) > 5 {
		t.Fatalf("classification produced %d distinct labels; the set must stay fixed", len(seen))
	}
	for s := range seen {
		switch s {
		case SurfaceRedirect, SurfaceAPI, SurfaceWeb, SurfaceStatic, SurfaceOps:
		default:
			t.Errorf("unexpected surface label %q", s)
		}
	}
}

func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics // as constructed by tests and by the CLI

	// None of these may panic, and the middleware must pass traffic through.
	m.ObserveRedirect("redirect", "memory", time.Millisecond)
	m.ObserveJob("rollup", nil)
	m.ObserveJob("rollup", errors.New("boom"))
	m.ObserveJobSkipped("rollup")
	// The add-on trio (M60, and the refusal counter M62). The first two are called
	// from addon.Open, which runs before anything else at boot and is handed whatever
	// metrics the caller has — the CLI has none. The third is called from a host
	// function, on whatever goroutine the guest runs on, and reaches the same nil.
	m.ObserveAddonLoad("minimal", "loaded")
	m.SetAddonInfo("minimal", "1.0.0", 1, "required", "config.read")
	m.ObserveAddonRefusal("minimal", "storage.own_schema")
	m.Register(nil)
	if m.Gather() != nil {
		t.Error("nil metrics returned a registry")
	}

	called := false
	h := m.HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anything", nil))
	if !called || rec.Code != http.StatusTeapot {
		t.Error("nil metrics middleware swallowed the request")
	}

	// The scrape handler must answer rather than panic.
	rec = httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("nil metrics scrape returned %d, want 404", rec.Code)
	}
}

func TestHTTPMiddlewareRecordsSurfaceMethodAndClass(t *testing.T) {
	m := NewMetrics()
	h := m.HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/broken":
			w.WriteHeader(http.StatusInternalServerError)
		case "/api/v1/quiet":
			// Writes nothing at all: still a 200 on the wire, and must be
			// counted as one rather than as an empty label.
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/links", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/broken", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/quiet", nil),
		httptest.NewRequest(http.MethodPost, "/login", nil),
		httptest.NewRequest(http.MethodGet, "/some-alias", nil),
	} {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	expect := func(surface, method, status string, want float64) {
		t.Helper()
		got := testutil.ToFloat64(m.httpRequests.WithLabelValues(surface, method, status))
		if got != want {
			t.Errorf("requests{%s,%s,%s} = %v, want %v", surface, method, status, got, want)
		}
	}
	expect("api", "GET", "2xx", 2) // /links and the silent handler
	expect("api", "GET", "5xx", 1)
	expect("web", "POST", "2xx", 1)
	expect("redirect", "GET", "2xx", 1)

	if n := testutil.CollectAndCount(m.httpDuration); n == 0 {
		t.Error("no duration observations recorded")
	}
}

func TestObserveRedirectLabelsNeverEmpty(t *testing.T) {
	m := NewMetrics()
	m.ObserveRedirect("not_found", "", time.Millisecond)
	if got := testutil.ToFloat64(m.redirects.WithLabelValues("not_found", "none")); got != 1 {
		t.Errorf("an empty cache tier was not normalised to \"none\": %v", got)
	}
}

// The SLO is stated as p99 under 20ms for cache hits, so the histogram has to
// have a boundary at exactly 0.02 and enough resolution below it for a p99
// estimate to mean something.
func TestRedirectBucketsSupportTheSLO(t *testing.T) {
	var hasTarget int
	var below int
	for _, b := range redirectBuckets {
		if b == 0.02 {
			hasTarget++
		}
		if b < 0.02 {
			below++
		}
	}
	if hasTarget != 1 {
		t.Error("no bucket boundary at the 20ms target; \"fraction under SLO\" would need interpolation")
	}
	if below < 5 {
		t.Errorf("only %d buckets below the target; a p99 estimate would be a guess", below)
	}
}

func TestScrapeExposesTheExpectedFamilies(t *testing.T) {
	m := NewMetrics()
	m.ObserveRedirect("redirect", "memory", 300*time.Microsecond)
	m.ObserveJob("rollup", nil)

	fake := &fakeIngest{depth: 7, enqueued: 100, dropped: 3, flushed: 97, batches: 5}
	m.Register(NewIngestCollector(fake))

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		`linkctrl_redirect_duration_seconds_bucket{cache="memory",outcome="redirect",le="0.02"} 1`,
		`linkctrl_redirects_total{cache="memory",outcome="redirect"} 1`,
		`linkctrl_job_runs_total{job="rollup",result="ok"} 1`,
		`linkctrl_analytics_queue_depth 7`,
		`linkctrl_analytics_events_dropped_total 3`,
		"linkctrl_build_info",
		"linkctrl_job_last_success_timestamp_seconds",
		// Runtime and process collectors, which are the first thing anyone
		// wants when a container misbehaves.
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape output is missing %q", want)
		}
	}

	// A histogram, not a summary: quantiles must be computed across instances
	// at query time, not baked in per process.
	if strings.Contains(body, `linkctrl_redirect_duration_seconds{quantile=`) {
		t.Error("redirect latency is a summary; per-instance quantiles cannot be aggregated")
	}
}

func TestIngestCollectorHandlesNilStats(t *testing.T) {
	m := NewMetrics()
	m.Register(NewIngestCollector(nil))
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape with a nil ingester returned %d", rec.Code)
	}
}

type fakeIngest struct {
	depth                                       int
	enqueued, dropped, flushed, failed, batches int64
}

func (f *fakeIngest) QueueDepth() int { return f.depth }
func (f *fakeIngest) Counters() (int64, int64, int64, int64, int64) {
	return f.enqueued, f.dropped, f.flushed, f.failed, f.batches
}
