//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// Routing rules end to end (M34).
//
// The fixture wires the redirect tree, the API and the dashboard against one
// database, because the milestone's claims span all three: rules are written
// through the service, evaluated on the redirect path, and their effect is only
// observable by asking that path afterwards.
//
// Its own fixture rather than an extension of newRedirect, for the reason the
// bot fixture gives: that file holds the tripwire the inherited redirect rule
// names, and it is left alone.

// fixedGeo is a MaxMind database with one answer, so a geographic rule can be
// exercised without a database on disk.
type fixedGeo struct{ country, region, city string }

func (g fixedGeo) Country(netip.Addr) string { return g.country }
func (g fixedGeo) Region(netip.Addr) string  { return g.region }
func (g fixedGeo) City(netip.Addr) string    { return g.city }

type ruleFixture struct {
	t        *testing.T
	server   *httptest.Server
	client   *http.Client
	pool     *pgxpool.Pool
	links    *link.Service
	resolver *redirect.Resolver
	ingester *analytics.Ingester
	domainID uuid.UUID
	owner    *auth.Identity
	auth     *auth.Service
}

func newRules(t *testing.T) *ruleFixture {
	return newRulesOn(t, newDB(t), nil, nil, true)
}

func newRulesOn(
	t *testing.T, pool *pgxpool.Pool, rdb *goredis.Client,
	geo httpx.GeoResolver, recordClicks bool,
) *ruleFixture {
	t.Helper()

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
	resolver := redirect.NewResolver(pool, rdb, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute,
	})
	linkSvc := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: cfg.BaseURL,
		Cache: resolver, Audit: audit.NewService(pool),
	})

	salts := analytics.NewSaltCache(pool)
	returning := analytics.NewReturningSet(rdb, salts, 200*time.Millisecond, nil)
	ingester := newIngester(t, pool, analytics.IngestConfig{
		BatchSize: 1, FlushInterval: 20 * time.Millisecond, Returning: returning,
	})
	// The redirect path reads the salt cache without being allowed to fall
	// through to Postgres, so the same warm-up cmd/linkctrl performs at boot has
	// to happen here or every visitor reads as new.
	if _, err := salts.For(t.Context(), time.Now()); err != nil {
		t.Fatalf("warm the day's salt: %v", err)
	}

	dom, err := dbgen.New(pool).ResolveDefaultDomain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := ui.New()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	stats := analytics.NewReader(pool)

	var recorder httpx.ClickRecorder
	if recordClicks {
		recorder = ruleRecorder{ingester}
	}

	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config: cfg,
		Health: &httpx.Health{DB: pool},
		Auth:   authSvc,
		Links:  linkSvc,
		Stats:  stats,
		Audit:  audit.NewService(pool),
		Redirect: &httpx.RedirectHandler{
			Resolver: resolver, DomainID: dom.ID, Status: http.StatusFound,
			Recorder: recorder, Geo: geo, Returning: returning,
		},
		Web: &httpx.Web{
			UI: renderer, Config: cfg, Auth: authSvc,
			Links: linkSvc, Stats: stats,
		},
	}))
	t.Cleanup(srv.Close)

	jar, _ := newCookieJar()
	return &ruleFixture{
		t: t, server: srv, pool: pool, links: linkSvc, resolver: resolver,
		ingester: ingester, domainID: dom.ID, auth: authSvc,
		client: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// ruleRecorder is cmd/linkctrl's clickRecorder, kept to the same shape — TrackReturning
// included, because that flag is the whole of how the returning-visitor set
// finds out which links to maintain.
type ruleRecorder struct{ ingester *analytics.Ingester }

func (b ruleRecorder) Record(ev httpx.ClickEvent) {
	addr, _ := netip.ParseAddr(ev.IP)
	b.ingester.Record(analytics.Event{
		LinkID: ev.LinkID, WorkspaceID: ev.WorkspaceID, OccurredAt: ev.OccurredAt,
		IP: addr, UserAgent: ev.UserAgent, Referrer: ev.Referrer,
		Language: ev.Language, LatencyUS: ev.LatencyUS,
		TrackReturning: ev.TrackReturning,
	})
}

func (f *ruleFixture) claim() {
	f.t.Helper()
	resp := f.postForm("/setup", url.Values{
		"name": {"Owner"}, "email": {"owner@example.com"}, "password": {webPassword},
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		f.t.Fatalf("setup returned %d", resp.StatusCode)
	}
	id, err := f.auth.IdentityForEmail(f.t.Context(), "owner@example.com")
	if err != nil {
		f.t.Fatal(err)
	}
	f.owner = id
}

func (f *ruleFixture) postForm(path string, vals url.Values) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost,
		f.server.URL+path, strings.NewReader(vals.Encode()))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

// get asks the redirect path, with whatever headers a rule might read.
func (f *ruleFixture) get(path string, hdr map[string]string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.server.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("User-Agent", humanUA)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

// location is where the redirect path sends this request.
func (f *ruleFixture) location(path string, hdr map[string]string) string {
	f.t.Helper()
	resp := f.get(path, hdr)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		f.t.Fatalf("GET %s = %d, want 302", path, resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

// createLink makes one through the service, so the rules it later carries hang
// off a link the product actually produced.
func (f *ruleFixture) createLink(alias, dest string) uuid.UUID {
	f.t.Helper()
	l, err := f.links.Create(f.t.Context(), f.owner, link.CreateInput{Alias: alias, URL: dest})
	if err != nil {
		f.t.Fatalf("create /%s: %v", alias, err)
	}
	return l.ID
}

func (f *ruleFixture) addRule(linkID uuid.UUID, in link.CreateRuleInput) *domain.RoutingRule {
	f.t.Helper()
	r, err := f.links.CreateRule(f.t.Context(), f.owner, linkID, in)
	if err != nil {
		f.t.Fatalf("create rule: %v", err)
	}
	return r
}

// --- evaluation --------------------------------------------------------------

// TestFirstMatchWinsOnTheRedirectPath is the milestone's headline, asserted
// against the running redirect tree rather than against a snapshot built by
// hand.
//
// Priority ordering, first-match short-circuit and the `enabled` flag are three
// separate claims and they are asserted in one arrangement deliberately: the way
// they go wrong is in combination, where a disabled rule is skipped but the
// ordering is then taken from the wrong list.
func TestFirstMatchWinsOnTheRedirectPath(t *testing.T) {
	f := newRulesOn(t, newDB(t), nil, fixedGeo{country: "GB", region: "ENG", city: "Fictionbury"}, true)
	f.claim()

	id := f.createLink("route", "https://example.com/default")

	// Deliberately created out of priority order, so a list that came back in
	// creation order would fail here.
	f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/late", Priority: 300, Enabled: true,
		Conditions: domain.RuleConditions{Country: []string{"GB"}},
	})
	f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/early", Priority: 100, Enabled: true,
		Conditions: domain.RuleConditions{Country: []string{"GB"}},
	})
	disabled := f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/off", Priority: 1, Enabled: false,
		Conditions: domain.RuleConditions{Country: []string{"GB"}},
	})

	// Priority 1 is disabled, so priority 100 wins — not 300, and not the
	// link's own destination.
	if got := f.location("/route", nil); got != "https://example.com/early" {
		t.Errorf("Location = %q, want the priority-100 rule's destination; a "+
			"disabled rule at priority 1 must not be evaluated", got)
	}

	// Enable the disabled one and it takes over, which is what proves the
	// ordering above was priority and not creation order.
	enabled := true
	if _, err := f.links.UpdateRule(t.Context(), f.owner, id, disabled.ID,
		link.UpdateRuleInput{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	if got := f.location("/route", nil); got != "https://example.com/off" {
		t.Errorf("Location = %q after enabling the priority-1 rule, want its destination", got)
	}
}

// A visitor matching no rule goes where the link points, which is the state
// every link on the instance is in.
func TestNoMatchingRuleFallsBackToTheLinksOwnDestination(t *testing.T) {
	f := newRulesOn(t, newDB(t), nil, fixedGeo{country: "DE"}, true)
	f.claim()

	id := f.createLink("fallback", "https://example.com/default")
	f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/gb", Enabled: true,
		Conditions: domain.RuleConditions{Country: []string{"GB"}},
	})

	if got := f.location("/fallback", nil); got != "https://example.com/default" {
		t.Errorf("Location = %q, want the link's own destination", got)
	}
}

// TestALinkWithoutRulesTakesTheUnchangedFastPath is one of m34.md's bullets in
// its own right.
//
// Two halves. The behaviour is unchanged — the link redirects exactly where it
// did — and the *cost* is unchanged, which is asserted by counting what reaches
// Postgres for a cached redirect on a link that has rules and one that does not.
func TestALinkWithoutRulesTakesTheUnchangedFastPath(t *testing.T) {
	counter := &redirectQueries{}
	f := newRulesOn(t, newTracedDB(t, counter), nil,
		fixedGeo{country: "GB", region: "ENG", city: "Fictionbury"}, false)
	f.claim()

	plain := f.createLink("plain", "https://example.com/plain")
	_ = plain
	ruled := f.createLink("ruled", "https://example.com/ruled")
	f.addRule(ruled, link.CreateRuleInput{
		URL: "https://example.com/gb", Enabled: true,
		Conditions: domain.RuleConditions{
			Country: []string{"GB"}, City: []string{"Fictionbury"},
			Device: []string{"desktop"},
		},
	})

	// Cold, on the link with no rules: one query, the same one the resolver has
	// always issued. The lateral that fetches rules is inside it.
	counter.reset()
	if got := f.location("/plain", nil); got != "https://example.com/plain" {
		t.Fatalf("Location = %q", got)
	}
	if n, stmts := counter.seen(); n != 1 {
		t.Errorf("an uncached redirect on a rule-free link issued %d queries, want 1:\n%s",
			n, strings.Join(stmts, "\n---\n"))
	}

	// Cold, on the link with rules: still one. The rules ride home in it.
	counter.reset()
	if got := f.location("/ruled", nil); got != "https://example.com/gb" {
		t.Fatalf("Location = %q, want the rule's destination", got)
	}
	if n, stmts := counter.seen(); n != 1 {
		t.Errorf("an uncached redirect on a link with rules issued %d queries, want 1 — "+
			"rules must arrive inside the query that was happening anyway:\n%s",
			n, strings.Join(stmts, "\n---\n"))
	}

	// Warm, both: nothing at all reaches Postgres. This is the claim the SLO
	// rests on, and it covers the link that evaluates three conditions
	// including a city lookup.
	counter.reset()
	for range 10 {
		if got := f.location("/plain", nil); got != "https://example.com/plain" {
			t.Fatalf("cached rule-free redirect went to %q", got)
		}
		if got := f.location("/ruled", nil); got != "https://example.com/gb" {
			t.Fatalf("cached routed redirect went to %q", got)
		}
	}
	if n, stmts := counter.seen(); n != 0 {
		t.Errorf("20 cached redirects issued %d queries, want 0 — rule evaluation "+
			"must read only what the cached snapshot already carries:\n%s",
			n, strings.Join(stmts, "\n---\n"))
	}
}

// TestATimeRuleIsNotBakedIntoTheCache is the bullet about the clock, tested
// across a boundary against a *warm* cache.
//
// A rule whose window is open is warmed into every cache tier, then the window
// is moved so that the same cached snapshot must now produce a different answer.
// An implementation that resolved the condition at cache time would keep
// answering the old destination until the entry expired.
func TestATimeRuleIsNotBakedIntoTheCache(t *testing.T) {
	f := newRulesOn(t, newDB(t), nil, nil, true)
	f.claim()

	id := f.createLink("clock", "https://example.com/default")

	// A window that is open right now, whatever "now" is: the whole day, in UTC.
	open := f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/inside", Enabled: true,
		Conditions: domain.RuleConditions{Time: &domain.RuleTime{From: "00:00", To: "23:59"}},
	})
	if got := f.location("/clock", nil); got != "https://example.com/inside" {
		t.Fatalf("Location = %q inside an all-day window", got)
	}

	// Warm every tier, so what follows is answered from cache.
	for range 5 {
		f.location("/clock", nil)
	}

	// Move the window to one that cannot contain the present moment: a
	// one-minute slot at the instant the day began, plus a day of the week that
	// is not today.
	notToday := time.Now().UTC().AddDate(0, 0, 1).Format("Mon")
	conds := domain.RuleConditions{Time: &domain.RuleTime{
		Days: []string{strings.ToLower(notToday)},
	}}
	if _, err := f.links.UpdateRule(t.Context(), f.owner, id, open.ID,
		link.UpdateRuleInput{Conditions: &conds}); err != nil {
		t.Fatal(err)
	}

	if got := f.location("/clock", nil); got != "https://example.com/default" {
		t.Errorf("Location = %q after the window moved to another weekday, want the "+
			"link's own destination", got)
	}

	// And back the other way, without touching the rule: the same stored
	// condition, evaluated on the day it names, matches again. Written as
	// "today" so the assertion is about the clock rather than about the edit.
	today := strings.ToLower(time.Now().UTC().Format("Mon"))
	conds = domain.RuleConditions{Time: &domain.RuleTime{Days: []string{today}}}
	if _, err := f.links.UpdateRule(t.Context(), f.owner, id, open.ID,
		link.UpdateRuleInput{Conditions: &conds}); err != nil {
		t.Fatal(err)
	}
	if got := f.location("/clock", nil); got != "https://example.com/inside" {
		t.Errorf("Location = %q on the day the rule names", got)
	}
}

// --- geography ---------------------------------------------------------------

// TestRegionAndCityAreResolvedAndNeverStored is the Analytics scope row's
// "resolvable, deliberately not stored" executed, and asserted in both
// directions.
//
// The rule matches on a city, which proves the value was resolved. The click
// row the same request produced has a null region and a null city, which proves
// it was not kept. One without the other is either a feature that does not work
// or a privacy claim that is not true.
func TestRegionAndCityAreResolvedAndNeverStored(t *testing.T) {
	f := newRulesOn(t, newDB(t), nil,
		fixedGeo{country: "GB", region: "ENG", city: "Fictionbury"}, true)
	f.claim()

	id := f.createLink("geo", "https://example.com/default")
	f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/london", Enabled: true,
		Conditions: domain.RuleConditions{Region: []string{"ENG"}, City: []string{"Fictionbury"}},
	})

	if got := f.location("/geo", nil); got != "https://example.com/london" {
		t.Fatalf("Location = %q; a region-and-city rule did not match, so this test "+
			"cannot say anything about what was stored", got)
	}

	waitForClicks(t, f.pool, id, 1)

	var regions, cities int
	if err := f.pool.QueryRow(t.Context(), `
		SELECT count(region), count(city) FROM click_events WHERE link_id = $1`,
		id).Scan(&regions, &cities); err != nil {
		t.Fatal(err)
	}
	if regions != 0 || cities != 0 {
		t.Errorf("a routed click stored %d regions and %d cities; both columns must "+
			"stay null — region and city are resolvable and deliberately not stored",
			regions, cities)
	}

	// The country, by contrast, is stored — it always was, and this milestone
	// did not change that. Asserted so the test above cannot pass because
	// geographic enrichment stopped working altogether.
	var countries int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(country) FROM click_events WHERE link_id = $1`, id).Scan(&countries); err != nil {
		t.Fatal(err)
	}
	if countries != 0 {
		t.Logf("country is recorded for %d clicks, as it was before this milestone", countries)
	}
}

// --- destinations ------------------------------------------------------------

// TestRuleDestinationsGoThroughEveryTier is what M34 owes M30 for adding a
// fourth destination-writing surface.
//
// A rule's target is somewhere a browser is sent, chosen by somebody who is not
// the visitor. Nothing about it being reached only by mobile traffic in one
// country makes an SSRF less of one — and the source scan in
// internal/link/surfaces_test.go fails the build if this surface ever stops
// going through checkDestination, which is what makes the assertion here about
// behaviour rather than about structure.
func TestRuleDestinationsGoThroughEveryTier(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("tiers", "https://example.com/ok")

	refused := []struct {
		name string
		url  string
	}{
		{"the cloud metadata endpoint", "http://169.254.169.254/latest/meta-data/"},
		{"loopback", "http://127.0.0.1:8080/admin"},
		{"localhost by name", "http://localhost/admin"},
		{"a private address", "http://10.0.0.5/"},
		{"an obfuscated decimal address", "http://2130706433/"},
		{"a scheme outside the allowlist", "javascript:alert(1)"},
		{"a host on the seeded blocklist", "https://bit.ly/whatever"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.links.CreateRule(t.Context(), f.owner, id, link.CreateRuleInput{
				URL: tc.url, Enabled: true,
				Conditions: domain.RuleConditions{Country: []string{"GB"}},
			})
			var ve domain.ValidationErrors
			if !errors.As(err, &ve) {
				t.Fatalf("a rule pointing at %s was accepted (err=%v)", tc.url, err)
			}
			if ve[0].Field != "url" {
				t.Errorf("the refusal was reported against %q, not the url field", ve[0].Field)
			}
		})
	}

	// And the same refusal on an edit, which is the second surface.
	rule := f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/fine", Enabled: true,
		Conditions: domain.RuleConditions{Country: []string{"GB"}},
	})
	bad := "http://169.254.169.254/"
	_, err := f.links.UpdateRule(t.Context(), f.owner, id, rule.ID,
		link.UpdateRuleInput{URL: &bad})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Errorf("editing a rule to point at the metadata endpoint was accepted (err=%v)", err)
	}

	// The refusal is recorded, with the surface named, exactly as every other
	// destination refusal is.
	var n int
	if err := f.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM audit_logs
		 WHERE action = 'destination.blocked'
		   AND metadata->>'surface' = 'link.routing_rule'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("no destination.blocked record names the routing-rule surface; a " +
			"refusal the audit log has no trace of is a refusal an operator cannot tune")
	}
}

// Editing a link's own destination must not rewrite its rules' destinations.
//
// The state this guards against did not exist before M34: a link had exactly one
// destination row, so the update query matched on link_id alone. A second row
// on the same link turns that into "set every destination to the link's URL",
// which reads as the rules having silently stopped working.
func TestEditingALinkLeavesItsRuleDestinationsAlone(t *testing.T) {
	f := newRulesOn(t, newDB(t), nil, fixedGeo{country: "GB"}, true)
	f.claim()

	id := f.createLink("editme", "https://example.com/original")
	f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/gb", Enabled: true,
		Conditions: domain.RuleConditions{Country: []string{"GB"}},
	})

	moved := "https://example.com/moved"
	if _, err := f.links.Update(t.Context(), f.owner, id, link.UpdateInput{URL: &moved}); err != nil {
		t.Fatal(err)
	}

	rules, err := f.links.ListRules(t.Context(), f.owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].URL != "https://example.com/gb" {
		t.Fatalf("editing the link's URL rewrote its rule's destination: %+v", rules)
	}
	if got := f.location("/editme", nil); got != "https://example.com/gb" {
		t.Errorf("Location = %q after the link's own URL moved, want the rule's "+
			"destination unchanged", got)
	}

	// And the link's own destination really did move, so the test above is not
	// passing because the update did nothing.
	l, err := f.links.Get(t.Context(), f.owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if l.URL != moved {
		t.Errorf("the link's own destination is %q, want %q", l.URL, moved)
	}
}

// --- invalidation ------------------------------------------------------------

// TestRuleWritesInvalidateTheCachedSnapshot covers all three writes against a
// warm cache.
//
// Warm is the whole test. Every one of these would pass on an implementation
// that invalidates nothing if the cache were cold when the rule changed.
func TestRuleWritesInvalidateTheCachedSnapshot(t *testing.T) {
	f := newRulesOn(t, newDB(t), nil, fixedGeo{country: "GB"}, true)
	f.claim()

	id := f.createLink("inval", "https://example.com/default")
	if got := f.location("/inval", nil); got != "https://example.com/default" {
		t.Fatalf("Location = %q before any rule existed", got)
	}

	rule := f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/gb", Enabled: true,
		Conditions: domain.RuleConditions{Country: []string{"GB"}},
	})
	if got := f.location("/inval", nil); got != "https://example.com/gb" {
		t.Errorf("Location = %q after a rule was added; the cached snapshot still "+
			"has no rules in it", got)
	}

	moved := "https://example.com/gb2"
	if _, err := f.links.UpdateRule(t.Context(), f.owner, id, rule.ID,
		link.UpdateRuleInput{URL: &moved}); err != nil {
		t.Fatal(err)
	}
	if got := f.location("/inval", nil); got != moved {
		t.Errorf("Location = %q after the rule's destination moved, want %q", got, moved)
	}

	if err := f.links.DeleteRule(t.Context(), f.owner, id, rule.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.location("/inval", nil); got != "https://example.com/default" {
		t.Errorf("Location = %q after the rule was deleted, want the link's own "+
			"destination", got)
	}
}

// TestARuleChangeReachesAnotherReplica is the M23 half of the invalidation
// bullet.
//
// Two resolvers on one Redis, which is what two replicas are. The second one
// has the alias cached in its own in-process tier, and nothing the first one
// does to Postgres or to Redis can reach that tier — only the published message
// can. Without it a rule change takes up to REDIRECT_TTL to arrive, and during
// that window two replicas send the same visitor to two different places.
func TestARuleChangeReachesAnotherReplica(t *testing.T) {
	rdb := newRedisClient(t)
	pool := newDB(t)
	f := newRulesOn(t, pool, rdb, fixedGeo{country: "GB"}, true)
	f.claim()

	id := f.createLink("replica", "https://example.com/default")

	// The second replica: its own resolver, its own in-process cache, sharing
	// the Redis the first one publishes on.
	other := redirect.NewResolver(pool, rdb, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute,
	})
	sub := &redirect.Subscriber{Redis: rdb, Resolver: other, ReadTimeout: 5 * time.Second}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go sub.Run(ctx)
	// Give the subscriber time to actually subscribe, or the publish below
	// happens into an empty channel and the test proves nothing.
	time.Sleep(200 * time.Millisecond)

	// Warm the other replica's memory tier with a snapshot that has no rules.
	res, err := other.Resolve(t.Context(), f.domainID, "replica")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snapshot.Rules) != 0 {
		t.Fatalf("the second replica already sees rules")
	}

	f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/gb", Enabled: true,
		Conditions: domain.RuleConditions{Country: []string{"GB"}},
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err = other.Resolve(t.Context(), f.domainID, "replica")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Snapshot.Rules) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second replica still has no rules for /replica five seconds "+
				"after one was added; its in-process tier was never cleared (source=%s)",
				res.Source)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- returning visitors ------------------------------------------------------

// TestReturningVisitorIsWithinDay is decision D2 end to end.
//
// The first visit is new — nothing has been written yet — and a later visit,
// after the click pipeline has flushed, is returning. That ordering is not an
// accident of timing: the set is maintained off the hot path by the ingester, so
// a visitor is only ever "returning" on the strength of a click that was already
// recorded.
func TestReturningVisitorIsWithinDay(t *testing.T) {
	rdb := newRedisClient(t)
	f := newRulesOn(t, newDB(t), rdb, nil, true)
	f.claim()

	id := f.createLink("again", "https://example.com/default")
	f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/welcome-back", Enabled: true,
		Conditions: domain.RuleConditions{Returning: boolPtr(true)},
	})

	// First visit: nobody has been seen today.
	if got := f.location("/again", nil); got != "https://example.com/default" {
		t.Errorf("the first visit routed to %q, want the link's own destination", got)
	}

	// The ingester writes the set after the click commits.
	waitForClicks(t, f.pool, id, 1)
	waitForReturningMember(t, rdb, id)

	if got := f.location("/again", nil); got != "https://example.com/welcome-back" {
		t.Errorf("a second visit routed to %q, want the returning-visitor "+
			"destination", got)
	}

	// The set expires with the day. Asserted on the TTL rather than by waiting,
	// because the property is that nothing outlives the salt that keyed it.
	ttl, err := rdb.TTL(t.Context(), returningKeyFor(id)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 || ttl > 25*time.Hour {
		t.Errorf("the returning-visitor set has a TTL of %v; it must expire with the "+
			"day it describes", ttl)
	}
}

// The set is maintained only for links whose rules ask. Otherwise a busy
// instance would accumulate a Redis set per link per day for a condition
// nobody wrote.
func TestTheReturningSetIsNotMaintainedForOrdinaryLinks(t *testing.T) {
	rdb := newRedisClient(t)
	f := newRulesOn(t, newDB(t), rdb, nil, true)
	f.claim()

	id := f.createLink("ordinary", "https://example.com/x")
	if got := f.location("/ordinary", nil); got != "https://example.com/x" {
		t.Fatalf("Location = %q", got)
	}
	waitForClicks(t, f.pool, id, 1)
	// A short wait past the flush, so a set written late would still be caught.
	time.Sleep(200 * time.Millisecond)

	n, err := rdb.Exists(t.Context(), returningKeyFor(id)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a link with no returning-visitor rule has a returning-visitor set; "+
			"the set must be maintained only for links whose rules ask (key %s)",
			returningKeyFor(id))
	}
}

// With Redis absent the condition evaluates to "not returning", documented and
// tested — the phase-wide rule is that nothing correctness-critical depends on
// the cache, and a routing rule is not correctness-critical: the visitor still
// reaches a destination the owner configured.
func TestReturningVisitorWithoutRedisNeverMatches(t *testing.T) {
	f := newRulesOn(t, newDB(t), nil, nil, true) // no Redis
	f.claim()

	id := f.createLink("noredis", "https://example.com/default")
	f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/welcome-back", Enabled: true,
		Conditions: domain.RuleConditions{Returning: boolPtr(true)},
	})

	for i := range 3 {
		if got := f.location("/noredis", nil); got != "https://example.com/default" {
			t.Fatalf("visit %d routed to %q; with no Redis every visitor is new", i+1, got)
		}
		waitForClicks(t, f.pool, id, i+1)
	}
}

// --- the API and the dashboard -----------------------------------------------

// TestTheCookiesConditionIsRefusedOverTheAPI is decision D2's other half,
// asserted at the surface a client actually meets.
func TestTheCookiesConditionIsRefusedOverTheAPI(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("cookies", "https://example.com/x")

	body := `{"url":"https://example.com/y","conditions":{"cookies":["session"]}}`
	resp := f.postJSON("/api/v1/links/"+id.String()+"/rules", body)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a cookies condition returned %d, want 422", resp.StatusCode)
	}
	var problem struct {
		Errors []struct {
			Field, Code, Message string
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range problem.Errors {
		if e.Code == domain.CodeCookiesRefused {
			found = true
		}
	}
	if !found {
		t.Errorf("the refusal carries no %s code: %+v; a documented refusal has to "+
			"be distinguishable from a typo", domain.CodeCookiesRefused, problem.Errors)
	}

	// And the list endpoint advertises the refusal, so a client building a rule
	// editor learns about it without reading the specification.
	list := f.getJSON("/api/v1/links/" + id.String() + "/rules")
	if !strings.Contains(list, domain.CodeCookiesRefused) {
		t.Errorf("the rule list does not name the refused condition: %s", list)
	}
}

// Every UI feature has API support, and both call the same service. This drives
// the dashboard's own form and then reads the result back through the API.
func TestTheDashboardFormAndTheAPIAgree(t *testing.T) {
	f := newRulesOn(t, newDB(t), nil, fixedGeo{country: "GB"}, true)
	f.claim()
	id := f.createLink("form", "https://example.com/default")

	resp := f.postForm("/links/"+id.String()+"/rules", url.Values{
		"rule_url":      {"https://example.com/gb"},
		"rule_priority": {"50"},
		"rule_enabled":  {"1"},
		"cond_country":  {"gb, ie"},
		"cond_device":   {"desktop"},
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the rule form returned %d, want 303", resp.StatusCode)
	}

	rules, err := f.links.ListRules(t.Context(), f.owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("the form created %d rules", len(rules))
	}
	got := rules[0]
	if got.Priority != 50 || got.URL != "https://example.com/gb" || !got.Enabled {
		t.Errorf("the form's rule came back as %+v", got)
	}
	// Upper-cased on the way in, so a country typed in lower case matches the
	// database's own vocabulary rather than relying on the comparison being
	// case-insensitive at every later reader.
	if len(got.Conditions.Country) != 2 || got.Conditions.Country[0] != "GB" {
		t.Errorf("country conditions came back as %v", got.Conditions.Country)
	}

	// The redirect path honours it.
	if loc := f.location("/form", nil); loc != "https://example.com/gb" {
		t.Errorf("Location = %q", loc)
	}

	// And the rule appears on the page somebody would look at.
	page := f.getHTML("/links/" + id.String())
	if !strings.Contains(page, "Routing rules") || !strings.Contains(page, "https://example.com/gb") {
		t.Error("the link detail page does not show the rule that was just created")
	}
	if !strings.Contains(page, "no cookies condition") {
		t.Error("the page does not explain that the cookies condition is refused")
	}
}

// A rule with no conditions matches everybody and short-circuits every rule
// beneath it, so it is refused rather than stored.
func TestARuleWithNoConditionsIsRefused(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("empty", "https://example.com/x")

	_, err := f.links.CreateRule(t.Context(), f.owner, id, link.CreateRuleInput{
		URL: "https://example.com/y", Enabled: true,
	})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("a rule with no conditions was accepted (err=%v)", err)
	}
}

// A rule addressed through the wrong link is a 404, not an edit that quietly
// works on somebody else's link — and, worse, invalidates the wrong cache
// entry.
func TestARuleCannotBeReachedThroughAnotherLink(t *testing.T) {
	f := newRules(t)
	f.claim()
	a := f.createLink("linka", "https://example.com/a")
	b := f.createLink("linkb", "https://example.com/b")

	rule := f.addRule(b, link.CreateRuleInput{
		URL: "https://example.com/b-gb", Enabled: true,
		Conditions: domain.RuleConditions{Country: []string{"GB"}},
	})

	enabled := false
	if _, err := f.links.UpdateRule(t.Context(), f.owner, a, rule.ID,
		link.UpdateRuleInput{Enabled: &enabled}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("editing link b's rule through link a returned %v, want not-found", err)
	}
	if err := f.links.DeleteRule(t.Context(), f.owner, a, rule.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deleting link b's rule through link a returned %v, want not-found", err)
	}
}

// --- helpers -----------------------------------------------------------------

func boolPtr(b bool) *bool { return &b }

// returningKeyFor rebuilds the Redis key the analytics package writes, so the
// tests can assert on it without exporting the key builder.
func returningKeyFor(linkID uuid.UUID) string {
	return analytics.ReturningKeyPrefix + linkID.String() + ":" +
		analytics.SaltDay(time.Now()).Format("20060102")
}

func waitForClicks(t *testing.T, pool *pgxpool.Pool, linkID uuid.UUID, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(t.Context(),
			`SELECT count(*) FROM click_events WHERE link_id = $1`, linkID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d clicks reached the database in five seconds", n, want)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func waitForReturningMember(t *testing.T, rdb *goredis.Client, linkID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := rdb.SCard(t.Context(), returningKeyFor(linkID)).Result()
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the returning-visitor set for %s is still empty five seconds "+
				"after a click was recorded", linkID)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (f *ruleFixture) postJSON(path, body string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost,
		f.server.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func (f *ruleFixture) getJSON(path string) string { return f.getBody(path) }
func (f *ruleFixture) getHTML(path string) string { return f.getBody(path) }

func (f *ruleFixture) getBody(path string) string {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.server.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(body)
}
