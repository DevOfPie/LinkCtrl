//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// metricsFixture is the redirect path plus the API, instrumented, with the
// scrape endpoint on a separate listener — the production arrangement.
type metricsFixture struct {
	*apiFixture
	metrics  *observability.Metrics
	scrape   *httptest.Server
	ingester *analytics.Ingester
}

func newMetricsFixture(t *testing.T) *metricsFixture {
	t.Helper()
	pool := newDB(t)

	cfg := config.Config{
		AppEnv: config.Development, BaseURL: "http://links.test", SecureCookies: false,
	}
	cfg.Auth.SignupMode = config.SignupOpen
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour
	cfg.Redirect.DefaultStatus = http.StatusFound

	metrics := observability.NewMetrics()
	metrics.Register(observability.NewPoolCollector(map[string]*pgxpool.Pool{"app": pool}))

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: cfg.Auth.SessionAbsoluteTTL, Idle: cfg.Auth.SessionIdleTTL},
	})
	resolver := redirect.NewResolver(pool, nil, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute,
	})
	linkSvc := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: cfg.BaseURL, Cache: resolver,
	})

	salts := analytics.NewSaltCache(pool)
	ingester := analytics.NewIngester(pool, salts, analytics.IngestConfig{
		QueueSize: 128, BatchSize: 10, FlushInterval: 20 * time.Millisecond,
	})
	ingester.Start()
	t.Cleanup(func() { _ = ingester.Close(context.Background()) })
	metrics.Register(observability.NewIngestCollector(ingester))

	dom, err := dbgen.New(pool).ResolveDefaultDomain(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config:  cfg,
		Health:  &httpx.Health{DB: pool},
		Auth:    authSvc,
		Links:   linkSvc,
		Metrics: metrics,
		Redirect: &httpx.RedirectHandler{
			Resolver: resolver, DomainID: dom.ID, Status: http.StatusFound,
			Metrics:  metrics,
			Recorder: clickRecorder{ingester: ingester},
		},
	}))
	t.Cleanup(srv.Close)

	// A second listener, as in production: the scrape endpoint is never on the
	// public server.
	scrape := httptest.NewServer(metrics.Handler())
	t.Cleanup(scrape.Close)

	jar, _ := newCookieJar()
	f := &apiFixture{t: t, server: srv, pool: pool, client: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	return &metricsFixture{apiFixture: f, metrics: metrics, scrape: scrape, ingester: ingester}
}

// clickRecorder mirrors the adapter in the composition root, which lives in
// package main and so cannot be imported.
type clickRecorder struct{ ingester *analytics.Ingester }

func (c clickRecorder) Record(ev httpx.ClickEvent) {
	addr, _ := netip.ParseAddr(ev.IP)
	c.ingester.Record(analytics.Event{
		LinkID: ev.LinkID, WorkspaceID: ev.WorkspaceID, OccurredAt: ev.OccurredAt,
		IP: addr, UserAgent: ev.UserAgent, Referrer: ev.Referrer,
		Language: ev.Language, LatencyUS: ev.LatencyUS,
	})
}

// get issues an unauthenticated GET against the public listener, without
// following redirects — a 302 is the result under test, not a step.
func (f *metricsFixture) get(path string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.server.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func (f *metricsFixture) scrapeText() string {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.scrape.URL, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("scrape returned %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// sampleCount reads a counter or histogram sample out of the scrape body, so
// the assertions are made against what a Prometheus server would actually see
// rather than against in-process state.
func sampleCount(t *testing.T, body, series string) float64 {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, series+" ") {
			var v float64
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, series+" "), "%g", &v); err != nil {
				t.Fatalf("cannot parse %q: %v", line, err)
			}
			return v
		}
	}
	return 0
}

func TestMetricsRecordTheRedirectSLO(t *testing.T) {
	f := newMetricsFixture(t)
	f.setupOwner()

	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/slo", "alias": "slometric",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create returned %d", resp.StatusCode)
	}
	f.decode(resp, nil)

	// First hit resolves from the database, later hits from the in-process
	// cache — which is exactly the distinction the SLO is stated against, and
	// the reason `cache` is a label.
	for i := range 4 {
		r := f.get("/slometric")
		_ = r.Body.Close()
		if r.StatusCode != http.StatusFound {
			t.Fatalf("hit %d returned %d, want 302", i, r.StatusCode)
		}
	}
	// A miss, and something that cannot be an alias at all.
	for _, path := range []string{"/nosuchalias", "/x"} {
		r := f.get(path)
		_ = r.Body.Close()
	}

	body := f.scrapeText()

	dbHits := sampleCount(t, body, `linkctrl_redirects_total{cache="database",outcome="redirect"}`)
	memHits := sampleCount(t, body, `linkctrl_redirects_total{cache="memory",outcome="redirect"}`)
	if dbHits < 1 {
		t.Errorf("no database-served redirect recorded: %v", dbHits)
	}
	if memHits < 1 {
		t.Errorf("no cache-served redirect recorded; the SLO is stated for cache hits: %v", memHits)
	}
	if total := dbHits + memHits; total != 4 {
		t.Errorf("recorded %v successful redirects, want 4", total)
	}

	// Unknown aliases and rejected shapes are counted, and distinguishable.
	if n := sampleCount(t, body, `linkctrl_redirects_total{cache="rejected",outcome="not_found"}`); n != 1 {
		t.Errorf("rejected-shape count = %v, want 1", n)
	}
	if n := sampleCount(t, body, `linkctrl_redirects_total{cache="negative",outcome="not_found"}`) +
		sampleCount(t, body, `linkctrl_redirects_total{cache="database",outcome="not_found"}`); n != 1 {
		t.Errorf("unknown-alias count = %v, want 1", n)
	}

	// The SLO is "p99 under 20ms for cache hits", so the 0.02 bucket must exist
	// and, in-process against a warm cache, must contain everything.
	underTarget := sampleCount(t, body,
		`linkctrl_redirect_duration_seconds_bucket{cache="memory",outcome="redirect",le="0.02"}`)
	if underTarget != memHits {
		t.Errorf("%v of %v cached redirects landed under the 20ms bucket", underTarget, memHits)
	}

	// The outer view exists too, and separates the surfaces.
	if n := sampleCount(t, body, `linkctrl_http_requests_total{method="GET",status="3xx",surface="redirect"}`); n != 4 {
		t.Errorf("http redirect 3xx count = %v, want 4", n)
	}
	if n := sampleCount(t, body, `linkctrl_http_requests_total{method="POST",status="2xx",surface="api"}`); n < 1 {
		t.Errorf("api request count = %v, want at least the create call", n)
	}
}

func TestMetricsReportPipelineAndPoolState(t *testing.T) {
	f := newMetricsFixture(t)
	f.setupOwner()

	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/pipeline", "alias": "pipemetric",
	})
	f.decode(resp, nil)
	for range 3 {
		r := f.get("/pipemetric")
		_ = r.Body.Close()
	}

	// Clicks are recorded asynchronously, so the counter is polled rather than
	// assumed: a fixed sleep would be either flaky or slow.
	var enqueued float64
	for range 50 {
		if enqueued = sampleCount(t, f.scrapeText(), "linkctrl_analytics_events_enqueued_total"); enqueued >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if enqueued < 3 {
		t.Errorf("enqueued clicks = %v, want at least 3", enqueued)
	}

	body := f.scrapeText()
	for _, series := range []string{
		"linkctrl_analytics_queue_depth",
		"linkctrl_analytics_events_dropped_total",
		`linkctrl_db_pool_max_connections{pool="app"}`,
		`linkctrl_db_pool_total_connections{pool="app"}`,
		"linkctrl_build_info",
	} {
		if !strings.Contains(body, series) {
			t.Errorf("scrape is missing %q", series)
		}
	}
	if n := sampleCount(t, body, `linkctrl_db_pool_max_connections{pool="app"}`); n < 1 {
		t.Errorf("pool max = %v; the collector is not reading live stats", n)
	}
}

// The scrape endpoint must not be reachable on the public listener. It reports
// queue depths and pool saturation, and the whole reason for a second listener
// is that this port is not published.
func TestMetricsAreNotOnThePublicListener(t *testing.T) {
	f := newMetricsFixture(t)

	// /metrics on the public server is an ordinary alias lookup: a 404 from the
	// redirect tree, never a scrape.
	resp := f.get("/metrics")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/metrics on the public listener returned %d, want 404", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "linkctrl_redirects_total") {
		t.Fatal("the public listener served metrics")
	}
}
