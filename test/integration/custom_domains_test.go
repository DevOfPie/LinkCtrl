//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/gate"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/ratelimit"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// Custom domains: the verification gate, and what it is a gate on (M40).
//
// **Every test here is about one column.** `verified_at` decides whether a Host
// header reaches the redirect tree at all, and the whole of the milestone's
// security story is that the decision cannot be reached any other way — not by
// registering a hostname, not by pointing DNS at the instance, not by a replica
// that has not heard about an un-verification yet.

const (
	customHost      = "go.custom.test"
	otherCustomHost = "camp.custom.test"
)

// stubZone is a DNS zone a test writes into: challenge record name to value.
//
// A stub rather than a real resolver, because what is under test is what this
// program does with an answer — and a test that had to publish a real TXT record
// would be a test that cannot run.
type stubZone struct {
	mu      sync.Mutex
	records map[string]string
	during  func()
}

func newStubZone() *stubZone { return &stubZone{records: map[string]string{}} }

// interrupt runs fn while the next lookup is in flight, and once.
//
// This is the whole apparatus for the race tests: the gap a verification has to
// survive is the one between reading a row and trusting what it said, and a DNS
// lookup is what holds that gap open — seconds of it, against a nameserver the
// registrant runs and can therefore make as slow as it likes.
func (z *stubZone) interrupt(fn func()) {
	z.mu.Lock()
	z.during = fn
	z.mu.Unlock()
}

func (z *stubZone) publish(name, value string) {
	z.mu.Lock()
	z.records[name] = value
	z.mu.Unlock()
}

func (z *stubZone) withdraw(name string) {
	z.mu.Lock()
	delete(z.records, name)
	z.mu.Unlock()
}

func (z *stubZone) LookupTXT(_ context.Context, name string) ([]string, error) {
	z.mu.Lock()
	v, ok := z.records[name]
	during := z.during
	z.during = nil
	z.mu.Unlock()
	// After the answer is read and before it is returned, holding no lock: the
	// answer is about the name that was asked, whatever happens to the row that
	// asked it.
	if during != nil {
		during()
	}
	if !ok {
		// The shape a real resolver returns for a name that does not exist, so
		// the message this produces is the one an operator would actually read.
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return []string{v}, nil
}

// domainFixture is the product served on the instance's own hosts and on
// whatever custom hostnames a test verifies.
type domainFixture struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	links  *link.Service
	owner  *auth.Identity
	pool   *pgxpool.Pool
	hosts  *redirect.HostCache
	zone   *stubZone
	// gates and defaultDomain exist for the gate tests in gates_test.go, which
	// need both namespaces at once: M35's reopening is about two gates that were
	// keyed on the instance default while the link lived somewhere else, and
	// nothing can see that without a second hostname.
	gates         *gate.Service
	defaultDomain uuid.UUID
	// limiter is the password limiter, with a ceiling of one guess, so a single
	// wrong password empties exactly one bucket and which bucket it was is
	// readable afterwards.
	limiter *ratelimit.Limiter
}

func newDomains(t *testing.T) *domainFixture {
	t.Helper()
	return newDomainsOn(t, splitConfig(t))
}

// newDomainsSingleHost wires the same tree on a deployment where the dashboard
// and short links share one hostname.
//
// **The fall-through arm is what differs and it is the whole of F88.** On a
// split-host instance a Host header that matches neither configured name reaches
// the ops mux and gets a 404; on a single-host instance it reaches the combined
// mux — the dashboard, the API, and aliases resolved against the *instance
// default* domain. So a verified hostname that misses the host cache fails closed
// on one deployment and wide open on the other, and only this fixture can show
// the second.
func newDomainsSingleHost(t *testing.T) *domainFixture {
	t.Helper()
	for k, v := range map[string]string{
		"LINKCTRL_APP_ENV":        "development",
		"LINKCTRL_BASE_URL":       "http://" + linkHost,
		"LINKCTRL_APP_BASE_URL":   "",
		"LINKCTRL_LINK_BASE_URL":  "",
		"LINKCTRL_API_KEY_PEPPER": strings.Repeat("p", 48),
		"LINKCTRL_DATABASE_URL":   "postgres://u:p@127.0.0.1:5432/linkctrl?sslmode=disable",
		"LINKCTRL_SECURE_COOKIES": "false",
		"LINKCTRL_SIGNUP_MODE":    "open",
	} {
		t.Setenv(k, v)
	}
	cfg, err := config.Parse()
	if err != nil {
		t.Fatalf("parse single-host configuration: %v", err)
	}
	if cfg.SplitHosts() {
		t.Fatal("configuration registered as split-host; the rest of this test proves nothing")
	}
	return newDomainsOn(t, cfg)
}

func newDomainsOn(t *testing.T, cfg config.Config) *domainFixture {
	t.Helper()
	pool := newDB(t)
	zone := newStubZone()

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: cfg.Auth.SessionAbsoluteTTL, Idle: cfg.Auth.SessionIdleTTL},
	})
	resolver := redirect.NewResolver(pool, nil, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute,
	})
	hosts := redirect.NewHostCache(pool, nil)
	gateSvc := gate.NewService(pool, gate.Config{Hasher: authSvc.Hasher()})
	limiter := ratelimit.New(1, ratelimit.Options{})

	rootRedirect := &httpx.RootRedirect{Status: http.StatusFound}
	linkSvc := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: cfg.LinkOrigin(), Cache: resolver,
		SplitHosts: cfg.SplitHosts(), RootCache: rootRedirect,
		Hasher: authSvc.Hasher(), Gates: gateSvc,
		DNS: zone,
		// Local only: one process, so the broadcast half has nothing to reach.
		// The cross-replica half is asserted separately, against two caches.
		Hosts:        redirect.BroadcastHostInvalidator{Local: hosts},
		DomainNotify: notify.NewService(pool),
		Audit:        audit.NewService(pool),
	})
	rootRedirect.Load = linkSvc.LoadRootRedirect
	hosts.OnReload = linkSvc.ForgetHostnames

	renderer, err := ui.New()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	dom, err := dbgen.New(pool).ResolveDefaultDomain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := hosts.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config:       cfg,
		Health:       &httpx.Health{DB: pool},
		Auth:         authSvc,
		Links:        linkSvc,
		Hosts:        hosts,
		DomainRoot:   &httpx.DomainRootRedirect{Status: http.StatusFound},
		TLSAsk:       &httpx.TLSAsk{Hosts: hosts},
		RootRedirect: rootRedirect,
		Redirect: &httpx.RedirectHandler{
			Resolver: resolver, DomainID: dom.ID, Status: http.StatusFound,
			Gates: gateSvc, PasswordLimiter: limiter,
		},
		Web: &httpx.Web{UI: renderer, Config: cfg, Auth: authSvc, Links: linkSvc},
	}))
	t.Cleanup(srv.Close)

	owner, err := authSvc.Register(context.Background(), auth.RegisterInput{
		Email: "domains-owner@example.com", Password: splitOwnerPassword, IsFirstUser: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	return &domainFixture{
		t: t, server: srv, links: linkSvc, owner: owner, pool: pool,
		hosts: hosts, zone: zone, gates: gateSvc, defaultDomain: dom.ID,
		limiter: limiter,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (f *domainFixture) get(host, path string) *http.Response {
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

// statusAs asks a host for a path as a particular client, which for these tests
// means with or without a user agent Classify reads as a bot.
func (f *domainFixture) statusAs(host, path, ua string) int {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.server.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Host = host
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// register adds a hostname and returns it, unverified.
func (f *domainFixture) register(hostname string) *link.Domain {
	f.t.Helper()
	d, err := f.links.RegisterDomain(f.t.Context(), f.owner, hostname)
	if err != nil {
		f.t.Fatalf("register %s: %v", hostname, err)
	}
	if d.Verified {
		f.t.Fatalf("%s was registered already verified; the gate is not a gate", hostname)
	}
	return d
}

// verify publishes the challenge and runs the check.
func (f *domainFixture) verify(d *link.Domain) *link.Domain {
	f.t.Helper()
	if d.Verification == nil {
		f.t.Fatalf("%s has no challenge to publish", d.Hostname)
	}
	f.zone.publish(d.Verification.RecordName, d.Verification.RecordData)
	out, err := f.links.VerifyDomain(f.t.Context(), f.owner, d.ID)
	if err != nil {
		f.t.Fatalf("verify %s: %v", d.Hostname, err)
	}
	if !out.Verified {
		f.t.Fatalf("%s reported unverified after a passing check", d.Hostname)
	}
	return out
}

func (f *domainFixture) createOn(domainID *uuid.UUID, alias, url string) *domain.Link {
	f.t.Helper()
	l, err := f.links.Create(f.t.Context(), f.owner, link.CreateInput{
		URL: url, Alias: alias, DomainID: domainID,
	})
	if err != nil {
		f.t.Fatalf("create %s: %v", alias, err)
	}
	return l
}

// TestAHostSpellingThatDoesNotFoldStillReachesItsOwnHostname is F88, on the
// deployment where the failure is open rather than closed.
//
// A `Host` header that names a verified hostname in a spelling the cache did not
// fold missed it and fell through — and on a single-host instance the thing
// behind the fall-through is `registerOps + registerApp + registerRedirect`. So
// the customer's own hostname answered with the dashboard, the API, and aliases
// resolved against the **instance default** domain: another workspace's link,
// served on a name its owner verified.
//
// Two spellings, because they miss for two different reasons and a fix for one
// is not a fix for the other. The trailing dot is the fully qualified name and
// `CanonicalHost` never trimmed it. A non-default port survives `CanonicalHost`
// on purpose — `SplitHosts()` compares two configured origins through it — so the
// verified-hostname cache needs `HostOnly`, which is the same normalization with
// the port dropped as well.
func TestAHostSpellingThatDoesNotFoldStillReachesItsOwnHostname(t *testing.T) {
	f := newDomainsSingleHost(t)
	d := f.verify(f.register(customHost))
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}

	// The same alias on both namespaces, so a request answered by the wrong tree
	// is answered with a destination rather than a 404 — which is what makes
	// "serves the default domain's aliases" observable instead of inferred.
	f.createOn(&d.ID, "promo", "https://example.com/the-customers")
	// The instance default named explicitly: a nil domain resolves to the
	// *workspace's* default, which D71 makes the hostname just verified above.
	f.createOn(&f.defaultDomain, "promo", "https://example.com/the-instances")

	for _, spelling := range []string{
		customHost,
		customHost + ".",
		customHost + ":8080",
		customHost + ".:8080",
		strings.ToUpper(customHost) + ".",
	} {
		t.Run(spelling, func(t *testing.T) {
			resp := f.get(spelling, "/promo")
			if resp.StatusCode != http.StatusFound {
				t.Fatalf("GET %s/promo = %d, want 302", spelling, resp.StatusCode)
			}
			if got := resp.Header.Get("Location"); got != "https://example.com/the-customers" {
				t.Errorf("Host %q resolved /promo to %q; the instance default's alias "+
					"answered on a customer's verified hostname", spelling, got)
			}

			// And the management surface is still not offered a second origin.
			// A customer's hostname serves links and nothing else, whichever way
			// the name is spelled.
			if resp := f.get(spelling, "/links"); resp.StatusCode != http.StatusNotFound {
				t.Errorf("Host %q served /links with %d; a customer's hostname must "+
					"not serve the dashboard", spelling, resp.StatusCode)
			}
		})
	}
}

// TestAConfiguredHostnameFoldsItsTrailingDot is the other half of F88, and it is
// F72's own case: on a split-host instance the miss lands on the ops mux, so the
// fully qualified spelling of the instance's *own* link host was answered 404.
func TestAConfiguredHostnameFoldsItsTrailingDot(t *testing.T) {
	f := newDomains(t)
	f.createOn(&f.defaultDomain, "folded", "https://example.com/folded")

	for _, spelling := range []string{linkHost, linkHost + "."} {
		resp := f.get(spelling, "/folded")
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("GET %s/folded = %d, want 302 — the fully qualified spelling "+
				"of the configured link host reached the ops mux", spelling, resp.StatusCode)
		}
		if got := resp.Header.Get("Location"); got != "https://example.com/folded" {
			t.Errorf("Host %q resolved /folded to %q", spelling, got)
		}
	}
}

// TestBotBlockingReachesEveryHostname is F89.
//
// The operator's bot policy is instance-wide — Plan.md says so, and the domain
// settings page and the audit record both say "every link on the instance". It
// was written to the `is_default` row alone, and `ResolveAliasForRedirect` reads
// the policy from the link's *own* domain, so no link on a verified custom
// hostname was ever blocked whatever the operator set. D71 makes a workspace's
// verified hostname the default for its new links, so the hole opened without
// anybody choosing it.
func TestBotBlockingReachesEveryHostname(t *testing.T) {
	f := newDomains(t)
	d := f.verify(f.register(customHost))
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.createOn(&d.ID, "crawled", "https://example.com/custom")
	f.createOn(&f.defaultDomain, "crawled", "https://example.com/default")

	// Enforced, which is the strongest form: no link may opt out of it.
	if _, err := f.links.SetBotBlocking(t.Context(), f.owner, true, true); err != nil {
		t.Fatalf("enforce bot blocking: %v", err)
	}

	for _, host := range []string{customHost, linkHost} {
		if got := f.statusAs(host, "/crawled", botUA); got != http.StatusForbidden {
			t.Errorf("a bot got %d from %s/crawled after blocking was enforced "+
				"instance-wide, want 403", got, host)
		}
		if got := f.statusAs(host, "/crawled", humanUA); got != http.StatusFound {
			t.Errorf("an ordinary visitor got %d from %s/crawled, want 302", got, host)
		}
	}

	// And off again, on every hostname — which also asserts the invalidation,
	// because the answer above is cached in a snapshot that carries the policy.
	if _, err := f.links.SetBotBlocking(t.Context(), f.owner, false, false); err != nil {
		t.Fatalf("turn bot blocking off: %v", err)
	}
	for _, host := range []string{customHost, linkHost} {
		if got := f.statusAs(host, "/crawled", botUA); got != http.StatusFound {
			t.Errorf("a bot got %d from %s/crawled after blocking was turned off, "+
				"want 302 — the cached snapshot still carries the old policy", got, host)
		}
	}
}

// TestAHostnameRegisteredAfterEnforcementInheritsIt is the other way the hole
// reopens: propagating the setting to the rows that exist says nothing about the
// rows registered next, and registering a hostname is a workspace's own act.
func TestAHostnameRegisteredAfterEnforcementInheritsIt(t *testing.T) {
	f := newDomains(t)
	if _, err := f.links.SetBotBlocking(t.Context(), f.owner, true, true); err != nil {
		t.Fatalf("enforce bot blocking: %v", err)
	}

	d := f.verify(f.register(customHost))
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.createOn(&d.ID, "late", "https://example.com/late")

	if got := f.statusAs(customHost, "/late", botUA); got != http.StatusForbidden {
		t.Errorf("a bot got %d from a hostname registered after blocking was "+
			"enforced, want 403", got)
	}

	// The API's own refusal reads the same row, so the two surfaces must agree
	// that this link cannot opt out.
	l := f.createOn(&d.ID, "late-two", "https://example.com/late-two")
	off := domain.BotAllow
	_, err := f.links.Update(t.Context(), f.owner, l.ID, link.UpdateInput{BotBlocking: &off})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("PATCH bot_blocking=off on an enforced custom hostname returned %v, "+
			"want a refusal", err)
	}
	if !strings.Contains(ve[0].Message, customHost) {
		t.Errorf("the refusal named %q rather than the hostname the link is served "+
			"on", ve[0].Message)
	}
}

// TestAnUnverifiedHostServesNothing is the milestone's whole security claim.
//
// A hostname somebody registered, and pointed at this instance, and does not
// currently have proof of control over. Every path on it must answer the
// operational 404 — not a link, not the dashboard, and above all not a redirect
// to the instance's own host, because a cross-host redirect reachable through
// the alias namespace is an open redirector for anybody who can create a link.
//
// The hostname is verified first and then un-verified, and that ordering is
// load-bearing rather than convenient. A hostname that was *never* verified has
// no links on it, so "nothing is served there" is true whether the gate works or
// not, and a test written that way passes on a build with no gate at all — which
// is what the first version of this test did when it was sabotaged. The state
// worth asserting is the one the grace window produces: links exist on the
// hostname, and the gate is the only thing stopping them.
func TestAnUnverifiedHostServesNothing(t *testing.T) {
	f := newDomains(t)
	d := f.verify(f.register(customHost))
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.createOn(&d.ID, "onlyhere", "https://example.com/target")
	if _, err := f.links.SetDomainRootRedirect(t.Context(), f.owner, d.ID,
		"https://example.com/home"); err != nil {
		t.Fatal(err)
	}
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Serving, so the assertions below are about the gate closing rather than
	// about a hostname that never worked.
	if resp := f.get(customHost, "/onlyhere"); resp.StatusCode != http.StatusFound {
		t.Fatalf("the verified hostname did not serve its own link (%d); the rest of "+
			"this test would prove nothing", resp.StatusCode)
	}

	// The column is cleared — by the grace window, by a rename, by an operator.
	// Whichever route, this is the state.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE domains SET verified_at = NULL WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/onlyhere", "/", "/login", "/api/v1/openapi.json", "/nosuch"} {
		t.Run(path, func(t *testing.T) {
			resp := f.get(customHost, path)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s%s = %d, want 404; a hostname without verified_at is "+
					"not a routing target, whatever DNS points at this instance",
					customHost, path, resp.StatusCode)
			}
			if loc := resp.Header.Get("Location"); loc != "" {
				t.Errorf("an unverified host redirected to %q; a cross-host redirect "+
					"driven by the alias namespace is an open redirector", loc)
			}
		})
	}

	// And a hostname nobody registered at all, which is the same answer for the
	// same reason.
	if resp := f.get("stranger.test", "/onlyhere"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown host served an alias with %d", resp.StatusCode)
	}
}

// TestVerificationIsWhatStartsServing pairs with the test above: the same
// hostname, the same request, and the only thing that changed is the column.
func TestVerificationIsWhatStartsServing(t *testing.T) {
	f := newDomains(t)
	d := f.register(customHost)

	// Refused before verification, and the refusal names the record.
	_, err := f.links.VerifyDomain(t.Context(), f.owner, d.ID)
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("verifying a hostname with no TXT record returned %v, want a validation error", err)
	}
	if !strings.Contains(ve[0].Message, domain.ChallengeRecordName(customHost)) {
		t.Errorf("the refusal did not name the record to publish: %q", ve[0].Message)
	}

	// A link cannot be created there either, so the product never mints a short
	// URL that resolves nowhere.
	if _, cerr := f.links.Create(t.Context(), f.owner, link.CreateInput{
		URL: "https://example.com/x", Alias: "early", DomainID: &d.ID,
	}); !errors.As(cerr, &ve) {
		t.Errorf("a link was created on an unverified hostname (err=%v)", cerr)
	}

	verified := f.verify(d)
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}

	l := f.createOn(&verified.ID, "promo", "https://example.com/promo")
	if want := "http://" + customHost + "/promo"; l.ShortURL != want {
		t.Errorf("ShortURL = %q, want %q; a link on a custom hostname whose short "+
			"URL names the instance's own host is a link published at the wrong "+
			"address", l.ShortURL, want)
	}

	resp := f.get(customHost, "/promo")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET %s/promo = %d, want 302", customHost, resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://example.com/promo" {
		t.Errorf("Location = %q", got)
	}
}

// TestAliasNamespacesArePerHost is the reason a hostname has exactly one owning
// workspace: uniqueness is (domain_id, alias), so two verified hostnames are two
// namespaces and the same alias on each is two links.
func TestAliasNamespacesArePerHost(t *testing.T) {
	f := newDomains(t)
	a := f.verify(f.register(customHost))
	b := f.verify(f.register(otherCustomHost))
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}

	f.createOn(&a.ID, "sale", "https://example.com/a")
	f.createOn(&b.ID, "sale", "https://example.com/b")

	for host, want := range map[string]string{
		customHost:      "https://example.com/a",
		otherCustomHost: "https://example.com/b",
	} {
		resp := f.get(host, "/sale")
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("GET %s/sale = %d, want 302", host, resp.StatusCode)
		}
		if got := resp.Header.Get("Location"); got != want {
			t.Errorf("%s/sale went to %q, want %q; the two hostnames are sharing "+
				"one alias namespace", host, got, want)
		}
	}

	// And neither alias exists on the instance's own link host.
	if resp := f.get(linkHost, "/sale"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an alias created on a custom hostname resolved on the instance's "+
			"own link host with %d", resp.StatusCode)
	}
}

// TestReservedAliasesApplyOnEveryHost.
//
// The reserved list constrains what an alias may be *called*, and it is not a
// property of the instance's own hostname: a dashboard route shadowed on a
// custom domain today is a dashboard route shadowed everywhere the day somebody
// merges the hosts back together.
func TestReservedAliasesApplyOnEveryHost(t *testing.T) {
	f := newDomains(t)
	d := f.verify(f.register(customHost))
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}

	for _, reserved := range []string{"login", "api", "healthz", "tls-check"} {
		t.Run(reserved, func(t *testing.T) {
			_, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
				URL: "https://example.com/x", Alias: reserved, DomainID: &d.ID,
			})
			var ve domain.ValidationErrors
			if !errors.As(err, &ve) {
				t.Fatalf("the alias %q was accepted on a custom hostname (err=%v)", reserved, err)
			}
		})
	}
}

// TestTheGraceWindowWarnsBeforeItStops is decision D70, end to end.
//
// Three states and the middle one is the decision: a failing hostname keeps
// serving and its owner is told, and only when the window has elapsed does
// serving actually stop. A grace period whose expiry escalates instead of acting
// would be the silent persistence this milestone forbids, reached gently.
func TestTheGraceWindowWarnsBeforeItStops(t *testing.T) {
	f := newDomains(t)
	d := f.verify(f.register(customHost))
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.createOn(&d.ID, "live", "https://example.com/live")

	// The record goes away. This is the deleted CNAME.
	f.zone.withdraw(domain.ChallengeRecordName(customHost))

	now := time.Now()
	sum, err := f.links.ReverifyDomains(t.Context(), now, 100)
	if err != nil {
		t.Fatalf("first failing pass: %v", err)
	}
	if sum.Failing != 1 || sum.Unverified != 0 {
		t.Fatalf("first failure produced %+v; a single failed DNS poll must not "+
			"stop anybody's links", sum)
	}
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if resp := f.get(customHost, "/live"); resp.StatusCode != http.StatusFound {
		t.Fatalf("the hostname stopped serving on the first failed check (%d); the "+
			"window exists because one poll is weak evidence", resp.StatusCode)
	}

	// The workspace is told, before the stop rather than only at it.
	var warned int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM notifications WHERE kind = $1`, notify.KindDomainFailing).
		Scan(&warned); err != nil {
		t.Fatal(err)
	}
	if warned == 0 {
		t.Error("nothing warned the workspace that its hostname was failing; a stop " +
			"nobody was told about is not a grace window")
	}

	// A second failure inside the window changes nothing.
	if _, err := f.links.ReverifyDomains(t.Context(), now.Add(time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if resp := f.get(customHost, "/live"); resp.StatusCode != http.StatusFound {
		t.Fatalf("serving stopped inside the grace window (%d)", resp.StatusCode)
	}

	// Past the window, it is a real stop.
	sum, err = f.links.ReverifyDomains(t.Context(), now.Add(link.DefaultVerifyGrace+time.Minute), 100)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unverified != 1 {
		t.Fatalf("the window elapsed and produced %+v; an expiry that warns again "+
			"instead of acting is the silent persistence this forbids", sum)
	}
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	resp := f.get(customHost, "/live")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET %s/live = %d after the grace window, want 404", customHost, resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("an unverified host redirected to %q", loc)
	}

	// Recorded, and attributed to the instance rather than to a person.
	var actor string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT actor_label FROM audit_logs WHERE action = 'domain.unverified' LIMIT 1`).
		Scan(&actor); err != nil {
		t.Fatalf("no domain.unverified audit record: %v", err)
	}
	if actor != "system" {
		t.Errorf("the un-verification was attributed to %q, not to the instance", actor)
	}

	// And a success at any point resets the count: republish, re-check, serving
	// resumes without waiting out anything.
	f.zone.publish(domain.ChallengeRecordName(customHost), d.Verification.RecordData)
	if _, err := f.links.ReverifyDomains(t.Context(), time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if resp := f.get(customHost, "/live"); resp.StatusCode != http.StatusFound {
		t.Errorf("republishing the record did not restore serving (%d)", resp.StatusCode)
	}
}

// TestRenamingAHostnameUnverifiesIt.
//
// The record proves control of the old name and says nothing about the new one.
// D69 deferred this bullet to M40 precisely because there was nothing to
// un-verify until now.
func TestRenamingAHostnameUnverifiesIt(t *testing.T) {
	f := newDomains(t)
	d := f.verify(f.register(customHost))
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}

	// A live link on it, so "stopped serving" is a claim with something behind
	// it rather than a 404 for an alias that never existed.
	f.createOn(&d.ID, "renamed", "https://example.com/renamed")
	if resp := f.get(customHost, "/renamed"); resp.StatusCode != http.StatusFound {
		t.Fatalf("the verified hostname did not serve its link (%d)", resp.StatusCode)
	}

	renamed, err := f.links.RenameDomain(t.Context(), f.owner, d.ID, otherCustomHost)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Verified {
		t.Fatal("a renamed hostname stayed verified; the published record proves " +
			"control of a name this row no longer has")
	}
	if renamed.Verification.RecordData == d.Verification.RecordData {
		t.Error("the challenge token survived the rename; it is published in a zone " +
			"this workspace may no longer control")
	}
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Neither name serves the link now: the old one because the row no longer
	// carries it, the new one because nothing has proved control of it.
	for _, host := range []string{customHost, otherCustomHost} {
		resp := f.get(host, "/renamed")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s still served after the rename (%d)", host, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Errorf("%s redirected to %q after the rename", host, loc)
		}
	}
}

// state reads what the row actually says.
//
// A test about a race cannot take the service's return value for it: the whole
// failure being tested is one where the service returns a domain that looks
// verified and the row underneath is a different name.
func (f *domainFixture) state(id uuid.UUID) (hostname string, verified bool) {
	f.t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT hostname, verified_at FROM domains WHERE id = $1`, id).
		Scan(&hostname, &at); err != nil {
		f.t.Fatal(err)
	}
	return hostname, at != nil
}

// checkState reads what the row says about the last *failed* check.
//
// The two columns a failure writes, and neither is reach — which is why they are
// read straight out of the table rather than off the domain the service returns:
// the sentence is rendered on the Domains page and the watermark decides the
// order of the re-verification queue, so a value either of them should not hold
// is invisible to a caller and visible to whoever owns the hostname.
func (f *domainFixture) checkState(id uuid.UUID) (sentence *string, checked *time.Time) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT verification_error, verification_checked_at FROM domains WHERE id = $1`, id).
		Scan(&sentence, &checked); err != nil {
		f.t.Fatal(err)
	}
	return sentence, checked
}

// verifiedEvents is every hostname the audit log claims was verified.
func (f *domainFixture) verifiedEvents() []string {
	f.t.Helper()
	rows, err := f.pool.Query(f.t.Context(),
		`SELECT metadata->>'hostname' FROM audit_logs WHERE action = $1`,
		audit.ActionDomainVerified)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h *string
		if err := rows.Scan(&h); err != nil {
			f.t.Fatal(err)
		}
		if h != nil {
			out = append(out, *h)
		}
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// TestARenameCannotStealAVerificationInFlight is the reopened milestone's
// security claim, on the on-demand path.
//
// The gate is *"only after `verified_at` is set"*, and what sets it used to be a
// write addressed by id alone. Between reading the row and writing that column,
// verification asks a nameserver the registrant controls about the hostname it
// read — so the registrant chooses how long that gap lasts. A rename committed
// inside it left the row carrying a name nobody had proved anything about, with
// `verified_at` filled in by a check of the *old* name.
//
// The name renamed into is the instance's own link host here, because that is
// what the finding established the reach to be: nothing refuses the instance's
// own names, and a verified row carrying one repoints every alias lookup at the
// attacker's domain. A test that renamed to a harmless third-party hostname
// would pass on a build where the gate is closed and understate what it is a
// gate on.
func TestARenameCannotStealAVerificationInFlight(t *testing.T) {
	f := newDomains(t)
	d := f.register(customHost)
	f.zone.publish(d.Verification.RecordName, d.Verification.RecordData)

	f.zone.interrupt(func() {
		if _, err := f.links.RenameDomain(t.Context(), f.owner, d.ID, linkHost); err != nil {
			t.Errorf("the rename this test races against did not happen: %v", err)
		}
	})

	out, err := f.links.VerifyDomain(t.Context(), f.owner, d.ID)
	if err == nil {
		t.Fatalf("verifying %s reported success for a row that had become %s: %+v",
			customHost, linkHost, out)
	}
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) || ve[0].Code != "conflict" {
		t.Errorf("the refusal was %v; a row that changed under the check is a conflict, "+
			"not a server error and not a DNS failure", err)
	}

	hostname, verified := f.state(d.ID)
	if hostname != linkHost {
		t.Fatalf("the row holds %s, so this test raced nothing", hostname)
	}
	if verified {
		t.Fatalf("%s is verified on the strength of a TXT record published for %s",
			linkHost, customHost)
	}
	for _, h := range f.verifiedEvents() {
		t.Errorf("the audit log records %s as verified; nothing was", h)
	}
}

// TestTheJobCannotVerifyANameItDidNotCheck is the same claim on the leader's
// path, which has the identical read-check-write shape and reaches every
// registered hostname on the instance rather than one the caller owns.
func TestTheJobCannotVerifyANameItDidNotCheck(t *testing.T) {
	f := newDomains(t)
	d := f.register(customHost)
	f.zone.publish(d.Verification.RecordName, d.Verification.RecordData)

	f.zone.interrupt(func() {
		if _, err := f.links.RenameDomain(t.Context(), f.owner, d.ID, appHost); err != nil {
			t.Errorf("the rename this test races against did not happen: %v", err)
		}
	})

	sum, err := f.links.ReverifyDomains(t.Context(), time.Now(), 100)
	if err != nil {
		t.Fatalf("the pass failed rather than declining the write: %v", err)
	}
	if sum.Verified != 0 {
		t.Errorf("the pass verified %d domains; the only check it ran was of a name the "+
			"row no longer had", sum.Verified)
	}

	hostname, verified := f.state(d.ID)
	if hostname != appHost {
		t.Fatalf("the row holds %s, so this test raced nothing", hostname)
	}
	if verified {
		t.Fatalf("%s — this instance's own dashboard host — is verified as a custom "+
			"domain on somebody else's proof", appHost)
	}
	for _, h := range f.verifiedEvents() {
		t.Errorf("the audit log records %s as verified; nothing was", h)
	}
}

// TestARenameCannotInheritAFailureInFlight is F131: the same claim on the
// failure path, which was left addressed by id when the reopening scoped the
// conditional write to the two sites that start serving.
//
// A failure grants nothing, so what a late write lands here is not reach. It is
// two lies about a name nobody checked — a `verification_error` sentence naming
// the hostname the row used to have, rendered on the Domains page beside the one
// it has now, and a `verification_checked_at` watermark that moves the row out of
// the head of the pending queue `verificationWorkList` orders NULLs first. So the
// name that has genuinely never been checked is checked later for having been
// renamed, and the page tells its owner to publish a record for a name they have
// just stopped using.
//
// Both columns are NULL after a rename, which is what makes the assertion
// possible: anything in either of them was written by the check of a different
// hostname.
func TestARenameCannotInheritAFailureInFlight(t *testing.T) {
	f := newDomains(t)
	d := f.register(customHost)
	// The challenge is deliberately not published, so the check fails.

	f.zone.interrupt(func() {
		if _, err := f.links.RenameDomain(t.Context(), f.owner, d.ID, otherCustomHost); err != nil {
			t.Errorf("the rename this test races against did not happen: %v", err)
		}
	})

	_, err := f.links.VerifyDomain(t.Context(), f.owner, d.ID)
	if err == nil {
		t.Fatal("the check reported success; nothing was published for either name")
	}
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) || ve[0].Code != "conflict" {
		t.Errorf("the refusal was %v; a row that changed under the check is a conflict, "+
			"not an unverified hostname — the caller is being told to publish a record "+
			"for a name this row no longer has", err)
	}

	hostname, _ := f.state(d.ID)
	if hostname != otherCustomHost {
		t.Fatalf("the row holds %s, so this test raced nothing", hostname)
	}
	sentence, checked := f.checkState(d.ID)
	if sentence != nil {
		t.Errorf("%s carries the sentence %q, which was written by a check of %s",
			otherCustomHost, *sentence, customHost)
	}
	if checked != nil {
		t.Errorf("%s is watermarked as checked at %s; the only lookup this test ran "+
			"asked about %s", otherCustomHost, checked.UTC(), customHost)
	}
}

// TestTheJobCannotFailANameItDidNotCheck is F131 on the leader's path, which
// reaches every registered hostname on the instance rather than one the caller
// owns — and which is the path the watermark actually matters on, because it is
// the one that decides whose turn is next.
func TestTheJobCannotFailANameItDidNotCheck(t *testing.T) {
	f := newDomains(t)
	d := f.register(customHost)

	f.zone.interrupt(func() {
		if _, err := f.links.RenameDomain(t.Context(), f.owner, d.ID, otherCustomHost); err != nil {
			t.Errorf("the rename this test races against did not happen: %v", err)
		}
	})

	sum, err := f.links.ReverifyDomains(t.Context(), time.Now(), 100)
	if err != nil {
		t.Fatalf("the pass failed rather than declining the write: %v", err)
	}
	if sum.Checked != 1 {
		t.Fatalf("the pass checked %d domains; it was meant to check exactly the one "+
			"that gets renamed", sum.Checked)
	}

	hostname, _ := f.state(d.ID)
	if hostname != otherCustomHost {
		t.Fatalf("the row holds %s, so this test raced nothing", hostname)
	}
	sentence, checked := f.checkState(d.ID)
	if sentence != nil {
		t.Errorf("%s carries the sentence %q, which was written by a check of %s",
			otherCustomHost, *sentence, customHost)
	}
	if checked != nil {
		t.Errorf("%s is watermarked as checked at %s, so it sorts behind every "+
			"genuinely unchecked registration on the next pass; the only lookup this "+
			"pass ran asked about %s", otherCustomHost, checked.UTC(), customHost)
	}
}

// TestServingHostnamesAreCheckedBeforePendingOnes is the other half of the
// reopening: the cadence, and what it is a cadence *on*.
//
// Re-verification is the only thing that ever takes a lapsed hostname out of
// service, and it walked one queue ordered by last check with NULLs first. A
// rename writes that column back to NULL, so registrations can be kept at the
// head of that queue for as long as somebody keeps renaming them — and a serving
// hostname, which always carries a watermark, sorted behind all of them.
//
// The batch here is one, which is the whole test: under a single queue the pass
// spends it on a pending row and the hostname whose DNS has gone keeps serving
// for ever, while the summary reports a healthy pass.
func TestServingHostnamesAreCheckedBeforePendingOnes(t *testing.T) {
	f := newDomains(t)
	served := f.verify(f.register(customHost))
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.createOn(&served.ID, "live", "https://example.com/live")

	// Registrations with no check against their name yet, which is what sorts
	// first under NULLS FIRST — and what a rename manufactures on demand.
	pending := make([]*link.Domain, 0, 3)
	for i := range 3 {
		pending = append(pending, f.register(fmt.Sprintf("pending%d.custom.test", i)))
	}

	// The deleted CNAME, on the hostname that is actually serving links.
	f.zone.withdraw(domain.ChallengeRecordName(customHost))

	now := time.Now()
	sum, err := f.links.ReverifyDomains(t.Context(), now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failing != 1 {
		t.Fatalf("a one-row pass produced %+v; the row it spent itself on was a "+
			"registration nobody is serving, so the hostname whose DNS has gone was "+
			"never looked at", sum)
	}

	sum, err = f.links.ReverifyDomains(t.Context(), now.Add(link.DefaultVerifyGrace+time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unverified != 1 {
		t.Fatalf("the grace window elapsed and the pass produced %+v; the hard stop is "+
			"what the cadence exists for", sum)
	}
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if resp := f.get(customHost, "/live"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET %s/live = %d after the window; serving did not stop",
			customHost, resp.StatusCode)
	}

	// And the pending class is checked, not starved and above all not reaped: a
	// hostname registered before its DNS cut-over fails every check until its
	// owner publishes, which is somebody's migration in progress rather than an
	// abandoned row.
	if _, err := f.links.ReverifyDomains(t.Context(), now, 100); err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		hostname, verified := f.state(p.ID)
		if hostname != p.Hostname || verified {
			t.Errorf("%s came back as %q verified=%v", p.Hostname, hostname, verified)
		}
		var checked *time.Time
		if err := f.pool.QueryRow(t.Context(),
			`SELECT verification_checked_at FROM domains WHERE id = $1`, p.ID).
			Scan(&checked); err != nil {
			t.Fatal(err)
		}
		if checked == nil {
			t.Errorf("%s was never checked once there was room for it", p.Hostname)
		}
	}
}

// TestAWorkspaceCannotRegisterUnboundedHostnames.
//
// Registration is the cheapest write on this surface and the most expensive one
// downstream: each row is a recurring DNS lookup the instance owes, aimed at a
// nameserver the registrant chose and able to block for the whole lookup timeout.
// Unbounded, one workspace decides how much re-verification the rest of the
// instance gets — and needs no working hostnames to do it.
func TestAWorkspaceCannotRegisterUnboundedHostnames(t *testing.T) {
	f := newDomains(t)
	for i := range domain.MaxDomainsPerWorkspace {
		f.register(fmt.Sprintf("bulk%02d.custom.test", i))
	}

	_, err := f.links.RegisterDomain(t.Context(), f.owner, "one-too-many.custom.test")
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("registering past the cap returned %v; a workspace can register "+
			"hostnames without bound", err)
	}
	if ve[0].Code != "too_many" {
		t.Errorf("the refusal was %q, which is not the cap", ve[0].Code)
	}
}

// TestTheTLSAskAnswersOnlyForVerifiedDomains.
//
// The endpoint decides which hostnames the operator's proxy will obtain
// certificates for. Answering wider would make the instance an unauthenticated
// issuance trigger for any name on the internet, which is the abuse `ask` exists
// to prevent.
func TestTheTLSAskAnswersOnlyForVerifiedDomains(t *testing.T) {
	f := newDomains(t)
	f.register(otherCustomHost)
	f.verify(f.register(customHost))
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}

	cases := map[string]int{
		customHost:      http.StatusOK,
		otherCustomHost: http.StatusNotFound,
		"stranger.test": http.StatusNotFound,
		appHost:         http.StatusNotFound,
		linkHost:        http.StatusNotFound,
		"":              http.StatusNotFound,
	}
	for name, want := range cases {
		t.Run("ask "+name, func(t *testing.T) {
			resp := f.get("anything.at.all", httpx.TLSAskPath+"?domain="+name)
			if resp.StatusCode != want {
				t.Errorf("ask for %q = %d, want %d", name, resp.StatusCode, want)
			}
		})
	}
}

// TestAVerifiedHostnameHasItsOwnRoot.
//
// The bare hostname is a URL somebody will type, and where it goes is the
// workspace's choice rather than the operator's. Answering 404 is the default
// and stays the default.
func TestAVerifiedHostnameHasItsOwnRoot(t *testing.T) {
	f := newDomains(t)
	d := f.verify(f.register(customHost))
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}

	if resp := f.get(customHost, "/"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unconfigured custom root answered %d, want 404", resp.StatusCode)
	}

	if _, err := f.links.SetDomainRootRedirect(t.Context(), f.owner, d.ID,
		"https://example.com/home"); err != nil {
		t.Fatal(err)
	}
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	resp := f.get(customHost, "/")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("the custom root answered %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://example.com/home" {
		t.Errorf("Location = %q", got)
	}

	// Recorded, like the instance-wide one it shares an action with.
	var n int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM audit_logs WHERE action = 'domain.root_redirect_changed'`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("pointing a hostname's root somewhere wrote no audit record")
	}
}

// TestDomainRootRedirectGoesThroughEveryTier is what M40 owes M30 for adding an
// eighth destination-writing surface.
//
// A root redirect is reachable with no alias and no link — only the bare
// hostname — which makes it the cleanest SSRF this product could offer if it
// skipped the tiers. The source scan in internal/link/surfaces_test.go fails the
// build if this surface ever stops going through checkDestination; this is the
// behavioural half.
func TestDomainRootRedirectGoesThroughEveryTier(t *testing.T) {
	f := newDomains(t)
	d := f.verify(f.register(customHost))

	for _, tc := range []struct{ name, url string }{
		{"the cloud metadata endpoint", "http://169.254.169.254/latest/meta-data/"},
		{"loopback", "http://127.0.0.1:8080/admin"},
		{"localhost by name", "http://localhost/admin"},
		{"a private address", "http://10.0.0.5/"},
		{"an obfuscated decimal address", "http://2130706433/"},
		{"a scheme outside the allowlist", "javascript:alert(1)"},
		{"a host on the seeded blocklist", "https://bit.ly/whatever"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.links.SetDomainRootRedirect(t.Context(), f.owner, d.ID, tc.url)
			var ve domain.ValidationErrors
			if !errors.As(err, &ve) {
				t.Fatalf("a root redirect to %s was accepted (err=%v)", tc.url, err)
			}
			if ve[0].Field != "root_redirect_url" {
				t.Errorf("the refusal was reported against %q", ve[0].Field)
			}
		})
	}

	var n int
	if err := f.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM audit_logs
		 WHERE action = 'destination.blocked'
		   AND metadata->>'surface' = 'domain.root_redirect'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("no destination.blocked record names the root-redirect surface")
	}
}

// TestTheLinksListFiltersByHostname.
//
// Once a workspace serves links on more than one hostname, "which links are on
// go.custom.test" is a question the list has to answer — and the total beside it
// has to agree, or the page reads "4 of 40" over a list of four.
func TestTheLinksListFiltersByHostname(t *testing.T) {
	f := newDomains(t)

	// Before the hostname verifies, so these land on the instance default: once
	// a workspace has its own verified hostname, a link that names no domain
	// goes there instead. That ordering is the point of the query's new
	// workspace filter, and it is asserted on its own below.
	f.createOn(nil, "default1", "https://example.com/1")
	f.createOn(nil, "default2", "https://example.com/2")

	d := f.verify(f.register(customHost))
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.createOn(&d.ID, "custom1", "https://example.com/3")

	page, err := f.links.List(t.Context(), f.owner, domain.LinkFilter{
		DomainID: &d.ID, IncludeTotal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Alias != "custom1" {
		t.Fatalf("the hostname filter returned %d links, want the one on %s", len(page.Items), customHost)
	}
	if page.Total == nil || *page.Total != 1 {
		t.Errorf("total = %v beside one link; the count query does not mirror the list", page.Total)
	}
}

// TestTheWorkspaceDefaultDomainIsTheWorkspaceOwn.
//
// The promise in GetWorkspaceDefaultDomain's name, which the query never kept:
// it read `WHERE is_default` with no workspace argument at all.
func TestTheWorkspaceDefaultDomainIsTheWorkspaceOwn(t *testing.T) {
	f := newDomains(t)

	// Before verifying anything, a link with no domain named lands on the
	// instance default, exactly as it always did.
	before := f.createOn(nil, "beforeown", "https://example.com/1")
	if !strings.HasPrefix(before.ShortURL, "http://"+linkHost+"/") {
		t.Fatalf("ShortURL = %q, want the instance's own link host", before.ShortURL)
	}

	f.verify(f.register(customHost))
	if err := f.hosts.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}

	after := f.createOn(nil, "afterown", "https://example.com/2")
	if want := "http://" + customHost + "/afterown"; after.ShortURL != want {
		t.Errorf("ShortURL = %q, want %q; a workspace that has verified its own "+
			"hostname is what registering one was for", after.ShortURL, want)
	}
}
