//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

const (
	appHost  = "manage.links.test"
	linkHost = "lnk.links.test"

	splitOwnerEmail    = "split-owner@example.com"
	splitOwnerPassword = "correct-horse-battery"
)

// splitFixture is the whole product served on two hostnames.
//
// Requests go to one httptest server and carry a Host header, which is exactly
// what a reverse proxy in front of a single listener does. The alternative —
// two listeners — would test something the deployment does not do.
type splitFixture struct {
	t        *testing.T
	server   *httptest.Server
	client   *http.Client
	tripwire *tripwireAuthenticator
	links    *link.Service
	owner    *auth.Identity
	pool     *pgxpool.Pool
	auth     *auth.Service
}

// splitConfig builds a real Config through Parse, because the split is decided
// by unexported parsed origins that a hand-built struct cannot set — the same
// reason a test cannot accidentally half-enable it.
func splitConfig(t *testing.T) config.Config {
	t.Helper()
	for k, v := range map[string]string{
		"LINKCTRL_APP_ENV":        "development",
		"LINKCTRL_BASE_URL":       "http://" + appHost,
		"LINKCTRL_APP_BASE_URL":   "http://" + appHost,
		"LINKCTRL_LINK_BASE_URL":  "http://" + linkHost,
		"LINKCTRL_API_KEY_PEPPER": strings.Repeat("p", 48),
		"LINKCTRL_DATABASE_URL":   "postgres://u:p@127.0.0.1:5432/linkctrl?sslmode=disable",
		"LINKCTRL_SECURE_COOKIES": "false",
		"LINKCTRL_SIGNUP_MODE":    "open",
	} {
		t.Setenv(k, v)
	}
	cfg, err := config.Parse()
	if err != nil {
		t.Fatalf("parse split-host configuration: %v", err)
	}
	if !cfg.SplitHosts() {
		t.Fatal("configuration did not register as split-host; the rest of this test proves nothing")
	}
	return cfg
}

func newSplit(t *testing.T) *splitFixture {
	t.Helper()
	pool := newDB(t)
	cfg := splitConfig(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: cfg.Auth.SessionAbsoluteTTL, Idle: cfg.Auth.SessionIdleTTL},
	})
	resolver := redirect.NewResolver(pool, nil, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute,
	})
	// Wired the way main.go does: the handler reads through the service and the
	// service invalidates the handler, so the cache is exercised rather than
	// bypassed.
	rootRedirect := &httpx.RootRedirect{Status: http.StatusFound}
	linkSvc := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: cfg.LinkOrigin(), Cache: resolver,
		SplitHosts: true, RootCache: rootRedirect,
	})
	rootRedirect.Load = linkSvc.LoadRootRedirect
	stats := analytics.NewReader(pool)

	renderer, err := ui.New()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	dom, err := dbgen.New(pool).ResolveDefaultDomain(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// The tripwire fails the test if the link host ever resolves a session.
	tripwire := &tripwireAuthenticator{t: t}

	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config:        cfg,
		Health:        &httpx.Health{DB: pool},
		Auth:          authSvc,
		Links:         linkSvc,
		Stats:         stats,
		Authenticator: tripwire,
		RootRedirect:  rootRedirect,
		Redirect: &httpx.RedirectHandler{
			Resolver: resolver, DomainID: dom.ID, Status: http.StatusFound,
		},
		Web: &httpx.Web{
			UI: renderer, Config: cfg, Auth: authSvc, Links: linkSvc, Stats: stats,
		},
	}))
	t.Cleanup(srv.Close)

	// Links are seeded through the service rather than over HTTP: the fixture's
	// tripwire fails the test on any session lookup, so this fixture cannot
	// authenticate a browser without tripping the thing it is here to prove.
	owner, err := authSvc.Register(context.Background(), auth.RegisterInput{
		Email: splitOwnerEmail, Password: splitOwnerPassword, IsFirstUser: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	return &splitFixture{
		t: t, server: srv, tripwire: tripwire, links: linkSvc, owner: owner,
		pool: pool, auth: authSvc,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// get sends a request to the single listener, addressed to one of the hosts.
func (f *splitFixture) get(host, path string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.server.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Host = host
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// createLinkForRouting seeds one link. What is under test is which host serves
// it, so this goes through the service rather than the create API.
func (f *splitFixture) createLinkForRouting(alias, url string) {
	f.t.Helper()
	l, err := f.links.Create(f.t.Context(), f.owner, link.CreateInput{URL: url, Alias: alias})
	if err != nil {
		f.t.Fatalf("create link %q: %v", alias, err)
	}
	// The short URL a client is handed must name the link host, or the split
	// is invisible to everyone except the router.
	if want := "http://" + linkHost + "/" + alias; l.ShortURL != want {
		f.t.Errorf("ShortURL = %q, want %q", l.ShortURL, want)
	}
}

func TestSplitHostsServeOnlyTheirOwnPaths(t *testing.T) {
	f := newSplit(t)

	cases := []struct {
		name, host, path string
		want             int
	}{
		// The dashboard belongs to the management host.
		{"login on app host", appHost, "/login", http.StatusOK},
		// The fixture already registered an owner, so setup is claimed and
		// bounces to the login form. A 303 here still says what this asserts:
		// the app tree answered rather than the alias catch-all.
		{"setup on app host", appHost, "/setup", http.StatusSeeOther},
		{"openapi on app host", appHost, "/api/v1/openapi.json", http.StatusOK},

		// ...and is absent from the link host. /login there is not a dashboard
		// route at all: it is an alias lookup that misses, which is why the
		// reserved-word list still has to hold on a split deployment.
		{"login on link host", linkHost, "/login", http.StatusNotFound},
		{"dashboard on link host", linkHost, "/dashboard", http.StatusNotFound},
		{"api on link host", linkHost, "/api/v1/openapi.json", http.StatusNotFound},
		{"static on link host", linkHost, "/static/app.css", http.StatusNotFound},

		// An alias is served by the link host and nowhere else.
		{"unknown alias on link host", linkHost, "/nosuchalias", http.StatusNotFound},
		{"alias shape on app host", appHost, "/nosuchalias", http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.get(tc.host, tc.path)
			if resp.StatusCode != tc.want {
				t.Errorf("GET %s%s = %d, want %d", tc.host, tc.path, resp.StatusCode, tc.want)
			}
		})
	}
}

// The dashboard host must not resolve aliases even when the alias exists —
// otherwise every link has two public URLs and the split is cosmetic.
func TestExistingAliasResolvesOnlyOnTheLinkHost(t *testing.T) {
	f := newSplit(t)
	f.createLinkForRouting("splitroute", "https://example.com/target")

	resp := f.get(linkHost, "/splitroute")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("alias on the link host = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://example.com/target" {
		t.Errorf("Location = %q", got)
	}

	resp = f.get(appHost, "/splitroute")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("the same alias on the dashboard host = %d, want 404; a link with two "+
			"public URLs defeats the point of separating them", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("the dashboard host redirected to %q; a cross-host redirect driven by "+
			"the alias namespace is an open redirector", loc)
	}
}

// Probes do not know the operator's hostnames. The image's own healthcheck asks
// 127.0.0.1, and a load balancer uses whatever it likes.
func TestHealthAnswersOnEveryHost(t *testing.T) {
	f := newSplit(t)

	for _, host := range []string{appHost, linkHost, "127.0.0.1:8080", "anything.else"} {
		t.Run(host, func(t *testing.T) {
			if resp := f.get(host, "/healthz"); resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s/healthz = %d, want 200", host, resp.StatusCode)
			}
		})
	}
}

// An unrecognized host gets the operational endpoints and nothing else. Serving
// links under any name pointed at this address is the decision custom domains
// have to make deliberately, with verification behind it.
func TestUnknownHostServesNoLinksAndNoDashboard(t *testing.T) {
	f := newSplit(t)
	f.createLinkForRouting("strangerhost", "https://example.com/target")

	for _, path := range []string{"/strangerhost", "/login", "/api/v1/openapi.json"} {
		t.Run(path, func(t *testing.T) {
			resp := f.get("someone-elses-domain.test", path)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET someone-elses-domain.test%s = %d, want 404", path, resp.StatusCode)
			}
		})
	}
}

// The security property the split exists for.
//
// __Host- cookies carry no Domain attribute, so a browser will not send the
// session to another host. That holds by construction today and would be
// destroyed by a later change setting Domain to "make cookies work across
// subdomains" — a change that looks like a fix. This is the test that stops it.
func TestSessionCookieCannotReachTheLinkHost(t *testing.T) {
	f := newSplit(t)

	// A real sign-in, because the login *page* sets no cookie: asserting over
	// the cookies of a GET /login response is a loop that never runs, and it
	// passes just as happily when the cookie is domain-scoped.
	form := url.Values{"email": {splitOwnerEmail}, "password": {splitOwnerPassword}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		f.server.URL+"/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = appHost
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303; without a session there is no cookie to check", resp.StatusCode)
	}

	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName(false) {
			session = c
		}
	}
	if session == nil {
		t.Fatal("login set no session cookie; this test would otherwise assert nothing")
	}
	if session.Domain != "" {
		t.Errorf("session cookie carries Domain=%q; a domain-scoped session cookie is sent "+
			"to every sibling host, including the one serving untrusted links", session.Domain)
	}

	// And even if one is sent anyway — a stolen cookie, a broken client — the
	// link host must not resolve it. The redirect tree has no session
	// middleware at all, so the tripwire is what proves it stayed that way.
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+"/anyalias", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = linkHost
	req.AddCookie(&http.Cookie{Name: auth.CookieName(false), Value: "a-real-looking-token"})

	got, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = got.Body.Close() }()

	if f.tripwire.called.Load() {
		t.Fatal("the link host performed a session lookup for a request carrying a " +
			"session cookie; the redirect tree must never authenticate")
	}
}
