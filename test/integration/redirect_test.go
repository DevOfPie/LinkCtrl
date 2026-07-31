//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// redirectFixture wires the real router with the redirect handler attached.
type redirectFixture struct {
	*apiFixture
	resolver *redirect.Resolver
	domainID uuid.UUID
}

func newRedirect(t *testing.T) *redirectFixture {
	t.Helper()
	pool := newDB(t)

	cfg := config.Config{
		AppEnv: config.Development, BaseURL: "http://links.test", SecureCookies: false,
	}
	cfg.Auth.SignupMode = config.SignupOpen
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour
	cfg.Redirect.DefaultStatus = http.StatusFound

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: cfg.Auth.SessionAbsoluteTTL, Idle: cfg.Auth.SessionIdleTTL},
	})

	// No Redis: the resolver must work with memory plus Postgres alone, which
	// is also the degraded path a cache outage produces.
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

	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config: cfg,
		Health: &httpx.Health{DB: pool},
		Auth:   authSvc,
		Links:  linkSvc,
		Redirect: &httpx.RedirectHandler{
			Resolver: resolver, DomainID: dom.ID, Status: http.StatusFound,
		},
	}))
	t.Cleanup(srv.Close)

	jar, _ := newCookieJar()
	f := &apiFixture{t: t, server: srv, client: &http.Client{
		Jar: jar,
		// Redirects must be observed, not followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, pool: pool}

	return &redirectFixture{apiFixture: f, resolver: resolver, domainID: dom.ID}
}

func (f *redirectFixture) createLink(body map[string]any) string {
	f.t.Helper()
	resp := f.do(http.MethodPost, "/api/v1/links", body)
	if resp.StatusCode != http.StatusCreated {
		f.t.Fatalf("create link returned %d", resp.StatusCode)
	}
	var created struct{ Alias string }
	f.decode(resp, &created)
	return created.Alias
}

func (f *redirectFixture) get(path string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.server.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

// TestRedirectMatrix covers every state a visitor can land on.
func TestRedirectMatrix(t *testing.T) {
	f := newRedirect(t)
	f.setupOwner()

	active := f.createLink(map[string]any{"url": "https://example.com/target"})

	expiring := f.createLink(map[string]any{
		"url":        "https://example.com/expiring",
		"expires_at": time.Now().Add(2 * time.Hour).Format(time.RFC3339),
	})
	// Backdate rather than waiting.
	if _, err := f.pool.Exec(t.Context(),
		"UPDATE links SET expires_at = now() - interval '1 hour' WHERE alias = $1", expiring); err != nil {
		t.Fatal(err)
	}

	archived := f.createLink(map[string]any{"url": "https://example.com/archived"})
	if _, err := f.pool.Exec(t.Context(),
		"UPDATE links SET status = 'archived' WHERE alias = $1", archived); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantTarget string
	}{
		{"active link redirects", "/" + active, http.StatusFound, "https://example.com/target"},
		{"uppercase alias resolves", "/" + strings.ToUpper(active), http.StatusFound, "https://example.com/target"},
		{"expired link is gone", "/" + expiring, http.StatusGone, ""},
		{"archived link is not found", "/" + archived, http.StatusNotFound, ""},
		{"unknown alias", "/definitelynothere", http.StatusNotFound, ""},
		{"too short", "/a", http.StatusNotFound, ""},
		{"over-length alias", "/" + strings.Repeat("a", 200), http.StatusNotFound, ""},
		{"trailing slash is not an alias", "/" + active + "/", http.StatusNotFound, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.get(tc.path)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantTarget != "" {
				if got := resp.Header.Get("Location"); got != tc.wantTarget {
					t.Errorf("Location = %q, want %q", got, tc.wantTarget)
				}
			}
		})
	}
}

// TestRedirectIsNeverCachedPermanently guards the core product promise. A 301
// is cached by browsers and intermediaries indefinitely, so editing a link
// afterwards would have no effect for anyone who had already followed it.
func TestRedirectIsNeverCachedPermanently(t *testing.T) {
	f := newRedirect(t)
	f.setupOwner()
	alias := f.createLink(map[string]any{"url": "https://example.com/one"})

	resp := f.get("/" + alias)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusPermanentRedirect {
		t.Fatalf("status = %d; a permanent redirect makes later edits ineffective", resp.StatusCode)
	}
	cc := resp.Header.Get("Cache-Control")
	if !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// TestEditingDestinationTakesEffectImmediately is the whole point of the
// invalidation path: a stale cache here means an edit silently does nothing.
func TestEditingDestinationTakesEffectImmediately(t *testing.T) {
	f := newRedirect(t)
	f.setupOwner()

	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{"url": "https://example.com/before"})
	var created struct{ ID, Alias string }
	f.decode(resp, &created)

	// Warm the cache.
	r1 := f.get("/" + created.Alias)
	_ = r1.Body.Close()
	if got := r1.Header.Get("Location"); got != "https://example.com/before" {
		t.Fatalf("Location = %q before edit", got)
	}

	resp = f.do(http.MethodPatch, "/api/v1/links/"+created.ID, map[string]any{
		"url": "https://example.com/after",
	})
	f.decode(resp, nil)

	r2 := f.get("/" + created.Alias)
	defer func() { _ = r2.Body.Close() }()
	if got := r2.Header.Get("Location"); got != "https://example.com/after" {
		t.Errorf("Location = %q after edit, want the new destination; the cache was not invalidated", got)
	}
}

// TestNegativeCacheDoesNotShadowANewLink is the failure the plan flagged: an
// alias probed before it exists gets cached as 404, and without invalidation
// on create the new link looks broken for the whole negative TTL.
func TestNegativeCacheDoesNotShadowANewLink(t *testing.T) {
	f := newRedirect(t)
	f.setupOwner()

	// Probe first, populating the negative cache.
	probe := f.get("/reserved-one")
	_ = probe.Body.Close()
	if probe.StatusCode != http.StatusNotFound {
		t.Fatalf("probe returned %d, want 404", probe.StatusCode)
	}

	f.createLink(map[string]any{"url": "https://example.com/new", "alias": "reserved-one"})

	resp := f.get("/reserved-one")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d after creating a previously-probed alias, want 302; "+
			"the negative cache entry was not cleared on create", resp.StatusCode)
	}
}

func TestDeletedAndArchivedLinksStopRedirecting(t *testing.T) {
	f := newRedirect(t)
	f.setupOwner()

	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{"url": "https://example.com/x"})
	var created struct{ ID, Alias string }
	f.decode(resp, &created)

	warm := f.get("/" + created.Alias)
	_ = warm.Body.Close()

	resp = f.do(http.MethodPost, "/api/v1/links/"+created.ID+"/archive", nil)
	f.decode(resp, nil)

	after := f.get("/" + created.Alias)
	_ = after.Body.Close()
	if after.StatusCode != http.StatusNotFound {
		t.Errorf("archived link returned %d, want 404", after.StatusCode)
	}

	resp = f.do(http.MethodPost, "/api/v1/links/"+created.ID+"/restore", nil)
	f.decode(resp, nil)

	restored := f.get("/" + created.Alias)
	_ = restored.Body.Close()
	if restored.StatusCode != http.StatusFound {
		t.Errorf("restored link returned %d, want 302", restored.StatusCode)
	}

	resp = f.do(http.MethodDelete, "/api/v1/links/"+created.ID, nil)
	_ = resp.Body.Close()

	deleted := f.get("/" + created.Alias)
	_ = deleted.Body.Close()
	if deleted.StatusCode != http.StatusNotFound {
		t.Errorf("deleted link returned %d, want 404", deleted.StatusCode)
	}
}

func TestHeadRequestGetsHeadersWithoutBody(t *testing.T) {
	f := newRedirect(t)
	f.setupOwner()
	alias := f.createLink(map[string]any{"url": "https://example.com/head"})

	req, _ := http.NewRequest(http.MethodHead, f.server.URL+"/"+alias, nil)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://example.com/head" {
		t.Errorf("Location = %q", got)
	}
}

func TestErrorPagesAreNotIndexable(t *testing.T) {
	f := newRedirect(t)
	// A shortener accumulates dead aliases; letting a crawler index them is
	// pure noise.
	resp := f.get("/nothinghere")
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Errorf("X-Robots-Tag = %q, want noindex", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want an HTML error page", ct)
	}
}

// TestRoutesAreNotShadowedByTheCatchAll verifies that real routes still win
// over /{alias}, which is what makes the catch-all safe to register.
func TestRoutesAreNotShadowedByTheCatchAll(t *testing.T) {
	f := newRedirect(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			resp := f.get(path)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s returned %d; the alias catch-all is shadowing a real route", path, resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
				t.Errorf("%s returned %q, which looks like the redirect handler answered", path, ct)
			}
		})
	}

	// The API subtree must reach the application tree, not the catch-all.
	resp := f.get("/api/v1/me")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/v1/me returned %d, want 401 from the API tree", resp.StatusCode)
	}
}

// TestRedirectSkipsSessionLookup turns the middleware split into an enforced
// invariant. A session store that fails the test if touched proves the hot
// path never performs a session query, which is otherwise only a comment.
func TestRedirectSkipsSessionLookup(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()

	cfg := config.Config{AppEnv: config.Development, BaseURL: "http://links.test"}
	cfg.Auth.SignupMode = config.SignupOpen

	resolver := redirect.NewResolver(pool, nil, redirect.Options{TTL: time.Hour, NegativeTTL: time.Minute})
	dom, err := dbgen.New(pool).ResolveDefaultDomain(ctx)
	if err != nil {
		t.Fatal(err)
	}

	tripwire := &tripwireAuthenticator{t: t}
	handler := httpx.NewRouter(httpx.Deps{
		Config:        cfg,
		Health:        &httpx.Health{DB: pool},
		Redirect:      &httpx.RedirectHandler{Resolver: resolver, DomainID: dom.ID, Status: http.StatusFound},
		Authenticator: tripwire,
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Send a session cookie deliberately. If the hot path consulted sessions,
	// the tripwire would fire.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/someunknownalias", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName(false), Value: "any-token-at-all"})

	resp, err := (&http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if tripwire.called.Load() {
		t.Fatal("the redirect path performed a session lookup; the 20ms budget " +
			"cannot absorb a database round trip per visit")
	}
}

type tripwireAuthenticator struct {
	t      *testing.T
	called atomicBool
}

func (a *tripwireAuthenticator) Authenticate(context.Context, string) (*auth.Identity, error) {
	a.called.Store(true)
	return nil, auth.ErrSessionNotFound
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) Store(v bool) { b.mu.Lock(); b.v = v; b.mu.Unlock() }
func (b *atomicBool) Load() bool   { b.mu.Lock(); defer b.mu.Unlock(); return b.v }

// TestResolverFallsBackWhenCacheIsAbsent covers the degraded path: with no
// Redis at all, redirects must still work from Postgres.
func TestResolverWorksWithoutRedis(t *testing.T) {
	f := newRedirect(t) // constructed with a nil Redis client
	f.setupOwner()
	alias := f.createLink(map[string]any{"url": "https://example.com/nocache"})

	for i := 0; i < 3; i++ {
		resp := f.get("/" + alias)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("attempt %d returned %d with no cache available", i, resp.StatusCode)
		}
	}
}

func TestConcurrentResolvesOfTheSameAlias(t *testing.T) {
	f := newRedirect(t)
	f.setupOwner()
	alias := f.createLink(map[string]any{"url": "https://example.com/hot"})

	// Approximates a link going viral from cold: singleflight should collapse
	// these into one query rather than a stampede.
	const workers = 64
	var wg sync.WaitGroup
	errs := make([]int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := f.get("/" + alias)
			errs[i] = resp.StatusCode
			_ = resp.Body.Close()
		}(i)
	}
	wg.Wait()

	for i, code := range errs {
		if code != http.StatusFound {
			t.Fatalf("worker %d got %d, want 302", i, code)
		}
	}
}

func TestCachedRedirectLatency(t *testing.T) {
	f := newRedirect(t)
	f.setupOwner()
	alias := f.createLink(map[string]any{"url": "https://example.com/fast"})

	// Warm.
	warm := f.get("/" + alias)
	_ = warm.Body.Close()

	const n = 200
	start := time.Now()
	for i := 0; i < n; i++ {
		resp := f.get("/" + alias)
		_ = resp.Body.Close()
	}
	avg := time.Since(start) / n

	// A smoke check, not the SLO. The real measurement is the k6 run in M14
	// against the container; this only catches an accidental per-request query
	// or an obviously pathological regression. It includes full client-side
	// HTTP over loopback.
	t.Logf("average cached redirect (in-process, incl. client): %s", avg)
	if avg > 5*time.Millisecond {
		t.Errorf("average cached redirect %s is far above expectation; "+
			"something is querying per request", avg)
	}
}

var _ = pgxpool.Pool{}
