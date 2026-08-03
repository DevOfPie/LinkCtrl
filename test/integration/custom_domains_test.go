//go:build integration

package integration

import (
	"context"
	"errors"
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
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
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
}

func newStubZone() *stubZone { return &stubZone{records: map[string]string{}} }

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
	z.mu.Unlock()
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
}

func newDomains(t *testing.T) *domainFixture {
	t.Helper()
	pool := newDB(t)
	cfg := splitConfig(t)
	zone := newStubZone()

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: cfg.Auth.SessionAbsoluteTTL, Idle: cfg.Auth.SessionIdleTTL},
	})
	resolver := redirect.NewResolver(pool, nil, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute,
	})
	hosts := redirect.NewHostCache(pool, nil)

	rootRedirect := &httpx.RootRedirect{Status: http.StatusFound}
	linkSvc := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: cfg.LinkOrigin(), Cache: resolver,
		SplitHosts: true, RootCache: rootRedirect,
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
		hosts: hosts, zone: zone,
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
