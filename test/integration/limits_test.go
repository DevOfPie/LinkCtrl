//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// limitedFixture is a throttled server plus its scrape endpoint, so a test can
// assert both the refusal and the series an operator would see.
type limitedFixture struct {
	*redirectFixture
	scrape *httptest.Server
}

func (f *limitedFixture) scrapeText() string {
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
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// newLimited builds a server with the rate limits actually switched on.
//
// The limiters come from httpx.NewLimiters, so these tests cover the wiring from
// configuration to enforcement rather than a limiter constructed by hand. Other
// fixtures leave the limits at zero, which is why the rest of the suite can make
// hundreds of requests from one address without tripping anything.
func newLimited(t *testing.T, mutate func(*config.Config)) *limitedFixture {
	t.Helper()
	pool := newDB(t)

	cfg := config.Config{
		AppEnv: config.Development, BaseURL: "http://links.test", SecureCookies: false,
	}
	cfg.Auth.SignupMode = config.SignupOpen
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour
	cfg.Redirect.DefaultStatus = http.StatusFound
	mutate(&cfg)

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
	dom, err := dbgen.New(pool).ResolveDefaultDomain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := ui.New()
	if err != nil {
		t.Fatal(err)
	}

	limits := httpx.NewLimiters(cfg, nil, nil)
	metrics := observability.NewMetrics()
	metrics.Register(observability.NewLimiterCollector(limits.Stats()))

	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config:  cfg,
		Health:  &httpx.Health{DB: pool},
		Auth:    authSvc,
		Links:   linkSvc,
		Metrics: metrics,
		Limits:  limits,
		Redirect: &httpx.RedirectHandler{
			Resolver: resolver, DomainID: dom.ID, Status: http.StatusFound,
			NotFoundLimiter: limits.NotFound, Metrics: metrics,
		},
		Stats: analytics.NewReader(pool),
		Web: &httpx.Web{
			UI: renderer, Config: cfg, Auth: authSvc, Links: linkSvc,
			Stats: analytics.NewReader(pool),
		},
	}))
	t.Cleanup(srv.Close)

	scrape := httptest.NewServer(metrics.Handler())
	t.Cleanup(scrape.Close)

	jar, _ := newCookieJar()
	f := &apiFixture{t: t, server: srv, client: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, pool: pool}

	return &limitedFixture{
		redirectFixture: &redirectFixture{apiFixture: f, resolver: resolver, domainID: dom.ID},
		scrape:          scrape,
	}
}

// assertThrottled checks the shape of a refusal, which is as much part of the
// contract as the status: a client that cannot tell "slow down" from "account
// locked" — both 429 — will retry the wrong one.
func assertThrottled(t *testing.T, resp *http.Response) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || ra < 1 {
		t.Errorf("Retry-After = %q, want a positive integer", resp.Header.Get("Retry-After"))
	}
	var p struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem document: %v", err)
	}
	if p.Type != "https://linkctrl.dev/problems/rate-limited" {
		t.Errorf("problem type = %q, want the rate-limited type", p.Type)
	}
	if p.Status != http.StatusTooManyRequests {
		t.Errorf("problem status = %d, want 429", p.Status)
	}
}

// Per-IP login throttling exists alongside the per-account lockout, not instead
// of it: one address guessing across many accounts never trips a per-account
// counter.
func TestLoginRateLimitIsPerAddressAndSurvivesChangingAccounts(t *testing.T) {
	f := newLimited(t, func(c *config.Config) {
		c.Auth.LoginRatePerMin = 4
		c.Auth.LockoutThreshold = 5 // higher than the rate, so the rate is what fires
	})

	// setup + login spend two of the four.
	f.setupOwner()

	// Two more credential requests are allowed, each naming a different account
	// so no lockout counter accumulates.
	for i := range 2 {
		resp := f.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
			"email":    "nobody" + strconv.Itoa(i) + "@example.com",
			"password": "whatever-long-enough",
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401 (allowed, and wrong)", i+1, resp.StatusCode)
		}
		f.decode(resp, nil)
	}

	assertThrottled(t, f.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "nobody9@example.com", "password": "whatever-long-enough",
	}))
}

func TestAPIRateLimitAppliesToTheAPISubtree(t *testing.T) {
	f := newLimited(t, func(c *config.Config) { c.Auth.APIRatePerMin = 5 })

	// setup + login are API calls too, so they spend two.
	f.setupOwner()

	for i := range 3 {
		resp := f.do(http.MethodGet, "/api/v1/me", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, resp.StatusCode)
		}
		f.decode(resp, nil)
	}

	assertThrottled(t, f.do(http.MethodGet, "/api/v1/me", nil))
}

// The variable is named API_RATE_PER_MIN, and the dashboard is not the API. A
// person clicking around a server-rendered UI must not consume the budget their
// own scripts need — nor may readiness probes.
func TestAPIRateLimitDoesNotThrottleThePagesOrTheProbes(t *testing.T) {
	f := newLimited(t, func(c *config.Config) { c.Auth.APIRatePerMin = 2 })

	f.setupOwner() // spends both
	assertThrottled(t, f.do(http.MethodGet, "/api/v1/me", nil))

	// /login answers a signed-in visitor with a redirect to the dashboard, so the
	// assertion is "served, not throttled" rather than a specific status.
	for _, path := range []string{"/login", "/dashboard", "/healthz", "/readyz"} {
		resp := f.get(path)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 400 {
			t.Errorf("%s: status = %d, want it served while the API limit is exhausted",
				path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// The central claim of the 404 limit: it charges misses, so a working link is
// never throttled by anyone's probing — including the visitor's own.
func TestNotFoundProbeLimit(t *testing.T) {
	f := newLimited(t, func(c *config.Config) { c.Redirect.NotFoundLimit = 5 })
	f.setupOwner()

	working := f.createLink(map[string]any{"url": "https://example.com/target"})

	// Warm the in-process cache, the way real traffic to a live link does.
	resp := f.get("/" + working)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("warm-up: status = %d, want 302", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Hits do not charge, so any number of them leaves the allowance intact.
	for range 20 {
		r := f.get("/" + working)
		if r.StatusCode != http.StatusFound {
			t.Fatalf("hit: status = %d, want 302", r.StatusCode)
		}
		_ = r.Body.Close()
	}

	// Five well-formed misses are answered normally.
	for i := range 5 {
		r := f.get("/missing" + strconv.Itoa(i))
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("miss %d: status = %d, want 404", i+1, r.StatusCode)
		}
		_ = r.Body.Close()
	}

	// The sixth is refused, and the refusal costs no lookup.
	r := f.get("/missing-again")
	if r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 after the allowance is spent", r.StatusCode)
	}
	if ra, err := strconv.Atoi(r.Header.Get("Retry-After")); err != nil || ra < 1 {
		t.Errorf("Retry-After = %q, want a positive integer", r.Header.Get("Retry-After"))
	}
	_ = r.Body.Close()

	// And the point of it all: the live link still works while throttled. If this
	// fails, one misconfigured proxy takes the whole redirect surface down.
	r = f.get("/" + working)
	if r.StatusCode != http.StatusFound {
		t.Errorf("throttled request for a cached live link: status = %d, want 302", r.StatusCode)
	}
	_ = r.Body.Close()

	// Counted under the shared series, not only as a redirect outcome. The alert
	// in the runbook is written against this one.
	body := f.scrapeText()
	if got := sampleCount(t, body, `linkctrl_rate_limited_total{limit="redirect_404"}`); got < 1 {
		t.Errorf("linkctrl_rate_limited_total{limit=\"redirect_404\"} = %v, want at least 1", got)
	}
	if got := sampleCount(t, body, `linkctrl_redirects_total{cache="rejected",outcome="throttled"}`); got < 1 {
		t.Errorf("linkctrl_redirects_total{outcome=\"throttled\"} = %v, want at least 1", got)
	}
}

// A deep-link miss is a miss (M33). Registering "/{alias}/{rest...}" opened a
// second, unbounded way to ask for something that is not there — one alias and
// any suffix — and if that path were not charged, the probe limit would be
// bypassable by appending a slash to everything.
//
// The limit is also what keeps the refusal from being an existence oracle: an
// alias that exists but does not forward and an alias that never existed both
// answer 404 and both spend an allowance, so the two cannot be told apart by
// probing either.
func TestDeepLinkMissesAreChargedAsProbes(t *testing.T) {
	f := newLimited(t, func(c *config.Config) { c.Redirect.NotFoundLimit = 5 })
	f.setupOwner()

	// A real link with path forwarding off. Everything beneath it is a miss.
	working := f.createLink(map[string]any{"url": "https://example.com/target"})

	for i := range 5 {
		r := f.get("/" + working + "/deep" + strconv.Itoa(i))
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("deep miss %d: status = %d, want 404", i+1, r.StatusCode)
		}
		_ = r.Body.Close()
	}

	// The allowance is now spent, and an ordinary single-segment miss proves it:
	// nothing else in this test asked for an alias that does not exist.
	r := f.get("/neverexisted")
	if r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; deep-link misses that cost a lookup must "+
			"spend the allowance, or appending a slash defeats the limit", r.StatusCode)
	}
	_ = r.Body.Close()

	// The bare alias still works while throttled, exactly as it does for the
	// single-segment limit: only a miss is charged, and a hit is answered from
	// the in-process cache.
	r = f.get("/" + working)
	if r.StatusCode != http.StatusFound {
		t.Errorf("throttled request for the live alias: status = %d, want 302", r.StatusCode)
	}
	_ = r.Body.Close()

	// And a deep miss on that same cached alias is answered 404 rather than 429,
	// which is not an inconsistency: the throttle exists to stop a scanner
	// turning probes into database queries, and this one was answered from the
	// in-process map. The refusal is reserved for the requests that would have
	// cost a lookup.
	r = f.get("/" + working + "/deep-again")
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("throttled deep miss on a cached alias: status = %d, want 404", r.StatusCode)
	}
	_ = r.Body.Close()
}

// Junk that cannot be an alias is refused on shape, so it costs no lookup — and
// therefore is not charged. Every browser requests favicon.ico, and it lands
// here; spending a visitor's allowance on it would throttle real people.
func TestMalformedPathsAreNotChargedAsProbes(t *testing.T) {
	f := newLimited(t, func(c *config.Config) { c.Redirect.NotFoundLimit = 3 })

	for _, path := range []string{
		"/favicon.ico", "/robots.txt", "/apple-touch-icon.png",
		"/wp-login.php", "/sitemap.xml", "/ab", "/x", "/-nope",
	} {
		r := f.get(path)
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, r.StatusCode)
		}
		_ = r.Body.Close()
	}

	// Far more requests than the allowance, and it is still intact: a well-formed
	// miss is answered rather than throttled.
	r := f.get("/still-here")
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404: malformed paths must not spend the allowance", r.StatusCode)
	}
	_ = r.Body.Close()
}

func TestDisabledLimitsEnforceNothing(t *testing.T) {
	f := newLimited(t, func(c *config.Config) {
		c.Auth.LoginRatePerMin = 0
		c.Auth.APIRatePerMin = 0
		c.Redirect.NotFoundLimit = 0
	})
	f.setupOwner()

	for i := range 30 {
		resp := f.do(http.MethodGet, "/api/v1/me", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("api request %d: status = %d, want 200 with the limit off", i+1, resp.StatusCode)
		}
		f.decode(resp, nil)

		r := f.get("/missing" + strconv.Itoa(i))
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("miss %d: status = %d, want 404 with the limit off", i+1, r.StatusCode)
		}
		_ = r.Body.Close()
	}
}

// A refusal has to be visible to an operator, or throttling looks like an outage
// with no cause.
func TestThrottlingIsCounted(t *testing.T) {
	f := newLimited(t, func(c *config.Config) { c.Auth.APIRatePerMin = 2 })
	f.setupOwner()
	f.decode(f.do(http.MethodGet, "/api/v1/me", nil), nil)

	body := f.scrapeText()
	if got := sampleCount(t, body, `linkctrl_rate_limited_total{limit="api"}`); got < 1 {
		t.Errorf("linkctrl_rate_limited_total{limit=\"api\"} = %v, want at least 1", got)
	}
	// The bookkeeping series exist too, so a limiter that has stopped limiting
	// because its table filled is visible rather than silent.
	if got := sampleCount(t, body, `linkctrl_rate_limit_tracked_keys{limit="api"}`); got < 1 {
		t.Errorf("linkctrl_rate_limit_tracked_keys{limit=\"api\"} = %v, want at least 1", got)
	}
}
