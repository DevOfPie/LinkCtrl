//go:build integration

package integration

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/gate"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// M66 end to end: an add-on inside the redirect path, driven through the router a
// visitor reaches, against a real wasm module and a real database.
//
// It lives here rather than in internal/httpx because every assertion in it is
// about a *row*. What a veto costs is whether a one-time link is still there
// afterwards; what a rewrite produces is a Location built from a destination the
// link service stored; what a killed module costs is whether the redirect
// happened at all. None of those can be asserted against a stub without the test
// asserting its own opinion of what the answer should be — and the whole of this
// milestone's risk is in the *ordering* between the extension point and the gates,
// which only a real gate can show.
//
// Redis is off throughout, like the gate fixture's, and for the same reason: the
// click budget the veto must not spend is in Postgres and nothing about that may
// depend on a cache being there.

type inlineFixture struct {
	t       *testing.T
	server  *httptest.Server
	client  *http.Client
	pool    *pgxpool.Pool
	host    *addon.Host
	links   *link.Service
	owner   *auth.Identity
	metrics *observability.Metrics
	log     *logSink
}

// newInlineAddon installs one redirect fixture with the grants given and builds
// the redirect tree around it.
//
// module is `redirect` for the behaving one and `slow` for the module that hangs;
// deadline of zero takes the shipped default.
func newInlineAddon(t *testing.T, module string, deadline time.Duration,
	perms ...string,
) *inlineFixture {
	t.Helper()
	pool := newDB(t)

	root := t.TempDir()
	code := addonFixture(t, module)
	installAddon(t, root, module, code, perms, nil)

	sink := &logSink{}
	metrics := observability.NewMetrics()
	host, err := addon.Open(t.Context(), addon.Options{
		Dir:            root,
		Logger:         slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Metrics:        metrics,
		DB:             pool,
		DSN:            dsnFor(dbNameOf(t, pool)),
		InlineDeadline: deadline,
	})
	if err != nil {
		t.Fatalf("the %s fixture did not load: %v", module, err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })

	cfg := config.Config{BaseURL: "http://links.test"}
	authSvc := auth.NewService(pool, auth.ServiceConfig{Params: fastParams})
	resolver := redirect.NewResolver(pool, nil, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute,
	})
	gateSvc := gate.NewService(pool, gate.Config{Hasher: authSvc.Hasher()})
	linkSvc := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: cfg.BaseURL, Cache: resolver,
		Hasher: authSvc.Hasher(), Gates: gateSvc,
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
			Resolver: resolver, DomainID: dom.ID, Metrics: metrics,
			Gates: gateSvc, Addons: host,
		},
	}))
	t.Cleanup(srv.Close)

	// Seeded through the service rather than over HTTP: what is under test is the
	// redirect tree, and signing a browser in to create a link would add a session
	// lookup to a fixture whose whole subject is the path that does not make one.
	owner, err := authSvc.Register(context.Background(), auth.RegisterInput{
		Email: "owner@example.com", Password: "a-sufficiently-long-password", IsFirstUser: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	return &inlineFixture{
		t: t, server: srv, pool: pool, host: host, links: linkSvc, owner: owner,
		metrics: metrics, log: sink,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// seed creates a link with the alias the fixture branches on. The alias is the
// whole of how the host-side test tells the module what to do — see the fixture's
// own doc comment — so it is chosen rather than generated.
func (f *inlineFixture) seed(alias, destination string, opts ...func(*link.CreateInput)) uuid.UUID {
	f.t.Helper()
	in := link.CreateInput{Alias: alias, URL: destination}
	for _, o := range opts {
		o(&in)
	}
	created, err := f.links.Create(f.t.Context(), f.owner, in)
	if err != nil {
		f.t.Fatalf("seeding %q: %v", alias, err)
	}
	return created.ID
}

func (f *inlineFixture) visit(path string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.server.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (f *inlineFixture) scrape() string {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// --- the three answers a visitor can get -------------------------------------

// An inline module that says nothing changes nothing, which is the case every
// redirect on an instance with an observing-only add-on takes and is the baseline
// the two below are read against.
func TestAnInlineAddonThatAllowsChangesNothing(t *testing.T) {
	f := newInlineAddon(t, "redirect", 0, addon.PermissionRedirectInline)
	f.seed("quiet", "https://shop.example.test/landing")

	resp := f.visit("/quiet")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("the redirect answered %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://shop.example.test/landing" {
		t.Errorf("Location is %q, want the destination unchanged", got)
	}
	// The module ran, so the line above is a module allowing rather than a module
	// that was never called.
	if logs := f.log.String(); !strings.Contains(logs, "redirect: alias=quiet") {
		t.Fatalf("the module was not called\n%s", logs)
	}
	series := `linkctrl_addon_redirect_duration_seconds_count{addon="redirect",class="inline"}`
	if !strings.Contains(f.scrape(), series) {
		t.Errorf("the scrape does not carry %s", series)
	}
}

// A veto, which is the refusal m66.md sends to the gate-refusal path.
func TestAVetoedRedirectIsRefusedAndTellsTheVisitorNothing(t *testing.T) {
	f := newInlineAddon(t, "redirect", 0, addon.PermissionRedirectInline)
	f.seed("veto", "https://shop.example.test/secret-landing")

	resp := f.visit("/veto")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a vetoed redirect answered %d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "" {
		t.Errorf("a vetoed redirect carried a Location: %q", got)
	}
	body := readBody(t, resp)
	// A refusal that echoed the alias or the destination would make this a
	// confirmation oracle, which is the reason the blocked-bot page is fixed bytes
	// and is the reason a veto reuses it.
	for _, leak := range []string{"veto", "shop.example.test", "secret-landing", "redirect"} {
		if strings.Contains(body, leak) {
			t.Errorf("the refusal page names %q; it names no alias, no destination and no add-on", leak)
		}
	}
	// Its own outcome, which m66.md's last risk asks for by name: a new refusal
	// source a visitor meets has to be tellable apart from the ones that existed.
	if want := `linkctrl_redirects_total{cache="database",outcome="vetoed"}`; !strings.Contains(f.scrape(), want) {
		t.Errorf("the scrape does not carry %s\n%s", want, seriesLike(f.scrape(), "linkctrl_redirects_total"))
	}
}

// The rewrite, end to end: what reaches the browser is the destination with the
// module's query and the same everything else.
func TestARewritingAddonChangesTheQueryAndNothingElse(t *testing.T) {
	f := newInlineAddon(t, "redirect", 0,
		addon.PermissionRedirectInline, addon.PermissionRewriteQuery)
	f.seed("strip", "https://shop.example.test/item/42?utm_source=ads&id=7")

	resp := f.visit("/strip")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("the redirect answered %d, want 302", resp.StatusCode)
	}
	if want := "https://shop.example.test/item/42?id=7"; resp.Header.Get("Location") != want {
		t.Errorf("Location is %q, want %q", resp.Header.Get("Location"), want)
	}
}

// The same module without the second grant. **This is D317's whole point** and it
// is asserted through the visitor rather than through a status the module saw: an
// add-on that declared *run on the redirect path* has not declared *and edit where
// the visitor goes*, and the browser is where that difference shows.
func TestARewriteWithoutItsGrantLeavesTheVisitorWhereTheLinkPointed(t *testing.T) {
	f := newInlineAddon(t, "redirect", 0, addon.PermissionRedirectInline)
	const dest = "https://shop.example.test/item/42?utm_source=ads&id=7"
	f.seed("strip", dest)

	resp := f.visit("/strip")
	if got := resp.Header.Get("Location"); got != dest {
		t.Errorf("Location is %q, want the untouched destination %q", got, dest)
	}
}

// --- the placement, which is the milestone's riskiest line --------------------

// The extension point runs **before** the gates, and this is the failure the
// ordering exists to prevent: a veto after a one-time link's single click had been
// spent would refuse the visitor *and* retire the link, so an add-on could take
// somebody's links down by refusing traffic to them.
//
// Asserted by asking the link again with the add-on gone — if the budget had been
// spent, the second visit would be a 410 whatever answers it.
func TestAVetoDoesNotSpendAOneTimeLinksClick(t *testing.T) {
	f := newInlineAddon(t, "redirect", 0, addon.PermissionRedirectInline)
	f.seed("veto", "https://shop.example.test/one-time", func(in *link.CreateInput) {
		in.OneTime = true
	})

	if resp := f.visit("/veto"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the vetoed visit answered %d, want 403", resp.StatusCode)
	}
	// The budget, read from the source of truth rather than inferred from a second
	// visit that the same add-on would veto again.
	var consumed int64
	err := f.pool.QueryRow(f.t.Context(),
		`SELECT coalesce(sum(consumed), 0) FROM link_click_budget b
		   JOIN links l ON l.id = b.link_id WHERE l.alias = 'veto'`).Scan(&consumed)
	if err != nil {
		t.Fatalf("reading the click budget: %v", err)
	}
	if consumed != 0 {
		t.Errorf("a vetoed redirect spent %d of a one-time link's single click; the "+
			"extension point runs before the gates precisely so an add-on cannot "+
			"retire somebody's link by refusing traffic to it", consumed)
	}
}

// --- the deadline, through the visitor ----------------------------------------

// The availability half of the owner's boundary, seen from the browser: a module
// that never returns costs the visitor the deadline and **not** the redirect.
func TestAnOverrunningAddonDoesNotCostTheVisitorTheirRedirect(t *testing.T) {
	f := newInlineAddon(t, "slow", 100*time.Millisecond, addon.PermissionRedirectInline)
	f.seed("hang", "https://shop.example.test/still-works")

	start := time.Now()
	resp := f.visit("/hang")
	took := time.Since(start)

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("a redirect behind a hung add-on answered %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://shop.example.test/still-works" {
		t.Errorf("Location is %q, want the destination the link points at", got)
	}
	if took > 10*time.Second {
		t.Fatalf("the redirect took %s, so nothing bounded the add-on", took)
	}
	if want := `linkctrl_addon_redirect_kills_total{addon="slow"} 1`; !strings.Contains(f.scrape(), want) {
		t.Errorf("the scrape does not carry %s\n%s", want,
			seriesLike(f.scrape(), "linkctrl_addon_redirect_kills_total"))
	}
	if logs := f.log.String(); !strings.Contains(logs, "overran its deadline") {
		t.Errorf("the kill was not logged where an operator would read it\n%s", logs)
	}
}

// --- the two curves, which is what per-module attribution means ---------------

// **Core's histogram excludes what the add-on cost**, and this is the assertion
// m66.md's attribution bullet reduces to: *core p99 and each add-on's p99 are
// different curves an operator can read apart*.
//
// It is a separate test from the one above and not an extra check inside it,
// because it is a separate claim. That one says the visitor still got their
// redirect; this one says the operator can tell whose fault the wait was. A host
// that satisfied the first and failed this would be one where
// linkctrl_redirect_duration_seconds *enclosed* the invocation — the two series
// rising together, neither of them core's, and an operator with an inline add-on
// installed having no baseline left to compare against.
//
// Driven with the module that never returns and a deadline of 200 ms, so the gap
// being asserted is two orders of magnitude rather than a margin: the visitor
// waits the deadline, and core's own work on a cache-served redirect is
// microseconds. The bound below is 50 ms — a quarter of the deadline — so the
// test fails on a nesting handler and does not fail on a slow machine.
func TestCoresLatencyExcludesWhatAnInlineAddonHeldTheRedirectFor(t *testing.T) {
	const deadline = 200 * time.Millisecond
	f := newInlineAddon(t, "slow", deadline, addon.PermissionRedirectInline)
	f.seed("attributed", "https://shop.example.test/attributed")

	start := time.Now()
	resp := f.visit("/attributed")
	waited := time.Since(start)

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("the redirect answered %d, want 302", resp.StatusCode)
	}
	if waited < deadline {
		t.Fatalf("the visitor waited %s, less than the %s deadline, so the module was "+
			"not on the path and this test asserted nothing", waited, deadline)
	}

	body := f.scrape()
	core := histogramSum(t, body, "linkctrl_redirect_duration_seconds_sum")
	if core >= deadline.Seconds()/4 {
		t.Errorf("linkctrl_redirect_duration_seconds totals %.4fs for one redirect the "+
			"visitor waited %s for; core's histogram is enclosing the add-on's time "+
			"rather than excluding it\n%s", core, waited,
			seriesLike(body, "linkctrl_redirect_duration_seconds_sum"))
	}
	// The other half: the time is not simply lost. A killed invocation is on the
	// kill counter by name, which is where an operator reads what the add-on cost
	// them, and the histogram is deliberately empty for it — see the series' own
	// comment in internal/observability/metrics.go.
	if want := `linkctrl_addon_redirect_kills_total{addon="slow"} 1`; !strings.Contains(body, want) {
		t.Errorf("the scrape does not carry %s\n%s", want,
			seriesLike(body, "linkctrl_addon_redirect_kills_total"))
	}
}

// histogramSum adds up every `_sum` sample of one histogram in a scrape,
// whatever labels the samples carry.
//
// Summed across label sets rather than matched against one, because which cache
// tier a redirect was served from is not this test's subject and pinning it would
// make the assertion fail for the wrong reason the first time the resolver's
// warm-up changed. The fixture serves exactly one redirect, so the total is that
// redirect's reading.
func histogramSum(t *testing.T, body, name string) float64 {
	t.Helper()
	var total float64
	var found bool
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+"{") && line != name {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			t.Fatalf("%s carries %q, which is not a number", name, fields[1])
		}
		total += v
		found = true
	}
	if !found {
		t.Fatalf("the scrape carries no %s at all, so this assertion would pass on a "+
			"host that recorded nothing\n%s", name, seriesLike(body, name))
	}
	return total
}

// --- the observe class, through the pipeline ----------------------------------

// m66.md's first bullet, wired the way an instance wires it: an observing add-on
// is fed from the click pipeline, after the response and after the click is
// durable.
//
// It is here rather than in internal/analytics because the record an add-on
// receives is built from fields the pipeline derives — the visitor hash and the
// country — from an address it discards in the same statement, and a unit test
// would have to build them itself and would then be asserting its own arithmetic.
func TestAnObservingAddonIsFedFromTheClickPipeline(t *testing.T) {
	f := newInlineAddon(t, "redirect", 0, addon.PermissionRedirectObserve)
	if got := f.host.ObservingAddons(); len(got) != 1 {
		t.Fatalf("the observe class carries %v", got)
	}
	// The pipeline, wired to the host exactly as cmd/linkctrl wires it.
	ing := newIngester(t, f.pool, analytics.IngestConfig{Observer: f.host})
	id := f.seed("watched", "https://shop.example.test/watched")

	ing.Record(analytics.Event{
		LinkID: id, WorkspaceID: f.owner.WorkspaceID, OccurredAt: time.Now(),
		IP:        netip.MustParseAddr("203.0.113.9"),
		UserAgent: "Mozilla/5.0 (X11; Linux x86_64) Gecko/20100101 Firefox/128.0",
		Language:  "en-GB",
	})
	if err := ing.Close(f.t.Context()); err != nil {
		t.Fatalf("flushing the click: %v", err)
	}

	logs := waitForLog(t, f.log, "redirect: observed_link=")
	if !strings.Contains(logs, "redirect: observed_browser=") {
		t.Errorf("the observing add-on was handed no derived fields\n%s", logs)
	}
}

// waitForLog blocks until the sink holds the marker. The observe class is
// asynchronous by construction, so a test of it either waits or asserts nothing.
func waitForLog(t *testing.T, sink *logSink, marker string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if logs := sink.String(); strings.Contains(logs, marker) {
			return logs
		}
		if time.Now().After(deadline) {
			t.Fatalf("the observing add-on never reported %q\n%s", marker, sink.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
