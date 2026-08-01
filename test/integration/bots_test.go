//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// Bot blocking (M32.5).
//
// The fixture here wires the redirect tree, the API and the dashboard against
// one database, because this milestone's claims span all three: the gate runs on
// the redirect path, the refusal of a link-level override is asserted at two
// surfaces, and the invalidation a domain change forces is only observable by
// asking the redirect path again afterwards.
//
// It is a fixture of its own rather than an extension of newRedirect. That file
// holds the tripwire the inherited redirect rule names, and this milestone's
// bullet says those tests pass unmodified — so it is left byte for byte alone.

const botUA = "Mozilla/5.0 (compatible; ExampleBot/2.1; +https://example.invalid/bot)"
const humanUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0 Safari/537.36"

type botFixture struct {
	t        *testing.T
	server   *httptest.Server
	client   *http.Client
	pool     *pgxpool.Pool
	links    *link.Service
	resolver *redirect.Resolver
	ingester *analytics.Ingester
	domainID uuid.UUID
}

func newBots(t *testing.T) *botFixture { return newBotsOn(t, newDB(t), true) }

// newBotsOn is newBots against a caller-supplied pool, and with click recording
// optional.
//
// The switch exists for the query-count test and nothing else. The ingester
// writes on its own goroutine, on its own schedule, and it did so before this
// milestone existed — so a tracer counting everything that reaches Postgres
// would be counting the flush interval rather than the redirect. That claim is
// about what resolving a redirect costs; what recording costs is asserted
// separately, by TestABlockedAttemptIsCountedNotAudited.
func newBotsOn(t *testing.T, pool *pgxpool.Pool, recordClicks bool) *botFixture {
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

	// No Redis, like newRedirect: the memory tier plus Postgres is the degraded
	// path a cache outage produces, and every claim here has to hold on it.
	resolver := redirect.NewResolver(pool, nil, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute,
	})
	linkSvc := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: cfg.BaseURL,
		Cache: resolver, Audit: audit.NewService(pool),
	})

	// A real ingester, because "counted, not audited" is only checkable against
	// the row a refusal produces. Flushed fast so the tests do not sleep.
	ingester := newIngester(t, pool, analytics.IngestConfig{
		BatchSize: 1, FlushInterval: 20 * time.Millisecond,
	})

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
		recorder = botRecorder{ingester}
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
			Recorder: recorder,
		},
		Web: &httpx.Web{
			UI: renderer, Config: cfg, Auth: authSvc,
			Links: linkSvc, Stats: stats,
		},
	}))
	t.Cleanup(srv.Close)

	jar, _ := newCookieJar()
	return &botFixture{
		t: t, server: srv, pool: pool, links: linkSvc, resolver: resolver,
		ingester: ingester, domainID: dom.ID,
		client: &http.Client{
			Jar: jar,
			// Redirects are assertions here; following them would hide a wrong
			// Location behind a plausible 200.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// botRecorder is cmd/linkctrl's clickRecorder, which is not importable from a
// main package. Kept to the same shape deliberately: a refusal that recorded a
// different event here than in production would prove nothing about production.
type botRecorder struct{ ingester *analytics.Ingester }

func (b botRecorder) Record(ev httpx.ClickEvent) {
	addr, _ := netip.ParseAddr(ev.IP)
	b.ingester.Record(analytics.Event{
		LinkID: ev.LinkID, WorkspaceID: ev.WorkspaceID, OccurredAt: ev.OccurredAt,
		IP: addr, UserAgent: ev.UserAgent, Referrer: ev.Referrer,
		Language: ev.Language, LatencyUS: ev.LatencyUS,
	})
}

// --- driving the fixture -----------------------------------------------------

func (f *botFixture) claim() {
	f.t.Helper()
	resp := f.postForm("/setup", url.Values{
		"name": {"Owner"}, "email": {"owner@example.com"}, "password": {webPassword},
	}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		f.t.Fatalf("setup returned %d", resp.StatusCode)
	}
}

func (f *botFixture) postForm(path string, vals url.Values, hdr map[string]string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost,
		f.server.URL+path, strings.NewReader(vals.Encode()))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func (f *botFixture) getAs(path, ua string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.server.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("User-Agent", ua)
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

// createLink makes a link by hand, so these tests do not depend on the create
// API's shape. The returned alias is what the redirect path is asked for.
func (f *botFixture) createLink(alias, dest string) uuid.UUID {
	f.t.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := f.pool.Exec(f.t.Context(), `
		INSERT INTO links (id, workspace_id, domain_id, alias, primary_url, status)
		VALUES ($1, (SELECT id FROM workspaces ORDER BY created_at LIMIT 1), $2, $3, $4, 'active')`,
		id, f.domainID, alias, dest); err != nil {
		f.t.Fatal(err)
	}
	f.resolver.InvalidateAlias(f.t.Context(), f.domainID, alias)
	return id
}

func (f *botFixture) setLinkPolicy(id uuid.UUID, policy string) {
	f.t.Helper()
	var alias string
	if err := f.pool.QueryRow(f.t.Context(),
		`UPDATE links SET bot_blocking = $2 WHERE id = $1 RETURNING alias`,
		id, policy).Scan(&alias); err != nil {
		f.t.Fatal(err)
	}
	f.resolver.InvalidateAlias(f.t.Context(), f.domainID, alias)
}

func (f *botFixture) setDomainPolicy(block, enforced bool) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.t.Context(),
		`UPDATE domains SET block_bots = $1, block_bots_enforced = $2 WHERE id = $3`,
		block, enforced, f.domainID); err != nil {
		f.t.Fatal(err)
	}
	f.resolver.InvalidateDomain(f.t.Context(), f.domainID)
}

func (f *botFixture) statusOf(path, ua string) int {
	f.t.Helper()
	resp := f.getAs(path, ua)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// --- the gate ----------------------------------------------------------------

// TestBotBlockingRefusesBotsAndNobodyElse walks the matrix the redirect path
// actually sees.
//
// The false-positive risk is the whole risk of this feature, so the assertion
// that matters most is the boring one: a browser user agent redirects in every
// row. A gate that refused everybody would satisfy every other test in this
// file.
func TestBotBlockingRefusesBotsAndNobodyElse(t *testing.T) {
	f := newBots(t)
	f.claim()

	tests := []struct {
		name       string
		linkPolicy string
		domBlock   bool
		domEnforce bool
		wantBot    int
		wantHuman  int
	}{
		{"default instance", "inherit", false, false, http.StatusFound, http.StatusFound},
		{"link blocks", "on", false, false, http.StatusForbidden, http.StatusFound},
		{"link allows", "off", false, false, http.StatusFound, http.StatusFound},
		{"domain blocks, link inherits", "inherit", true, false, http.StatusForbidden, http.StatusFound},
		{"domain blocks, link opts out", "off", true, false, http.StatusFound, http.StatusFound},
		{"domain enforces, link opts out", "off", true, true, http.StatusForbidden, http.StatusFound},
		{"domain enforces, link inherits", "inherit", true, true, http.StatusForbidden, http.StatusFound},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			alias := "gate" + string(rune('a'+i))
			id := f.createLink(alias, "https://example.com/"+alias)
			f.setLinkPolicy(id, tc.linkPolicy)
			f.setDomainPolicy(tc.domBlock, tc.domEnforce)

			if got := f.statusOf("/"+alias, botUA); got != tc.wantBot {
				t.Errorf("a bot got %d, want %d", got, tc.wantBot)
			}
			if got := f.statusOf("/"+alias, humanUA); got != tc.wantHuman {
				t.Errorf("a browser got %d, want %d — a gate that refuses people "+
					"is worse than no gate, and there is no way past it until Phase 3",
					got, tc.wantHuman)
			}
		})
	}
}

// TestABlockedBotLearnsNothingAboutTheLink is the enumeration claim.
//
// A crawler refused on an active link, an expired one and an archived one must
// receive the same bytes, or being blocked tells it which short codes are real
// and which of those are still live — which is exactly what a shortener must not
// confirm. Compared byte for byte, headers included, because a difference in
// Cache-Control or Content-Length is as good an oracle as a difference in the
// body.
func TestABlockedBotLearnsNothingAboutTheLink(t *testing.T) {
	f := newBots(t)
	f.claim()
	f.setDomainPolicy(true, false)

	active := f.createLink("blkactive", "https://example.com/active")
	expired := f.createLink("blkexpired", "https://example.com/expired")
	archived := f.createLink("blkarchived", "https://example.com/archived")
	_ = active

	if _, err := f.pool.Exec(t.Context(),
		`UPDATE links SET expires_at = now() - interval '1 hour' WHERE id = $1`, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE links SET status = 'archived' WHERE id = $1`, archived); err != nil {
		t.Fatal(err)
	}
	f.setDomainPolicy(true, false) // clears every cached snapshot after the edits

	// The states really are different when the client is not a bot: without
	// this, three identical 403s would also be produced by a handler that
	// refused everything.
	for alias, want := range map[string]int{
		"blkactive": http.StatusFound, "blkexpired": http.StatusGone,
		"blkarchived": http.StatusNotFound,
	} {
		if got := f.statusOf("/"+alias, humanUA); got != want {
			t.Fatalf("/%s answered a browser %d, want %d — the three links are not "+
				"in the three states this test needs", alias, got, want)
		}
	}

	first := f.captureAs("/blkactive", botUA)
	for _, alias := range []string{"blkexpired", "blkarchived"} {
		got := f.captureAs("/"+alias, botUA)
		if got != first {
			t.Errorf("a blocked bot can tell /blkactive from /%s:\n--- active ---\n%s\n"+
				"--- %s ---\n%s\nBeing refused must reveal no more than a 404 does.",
				alias, first, alias, got)
		}
	}

	if !strings.Contains(first, "403") {
		t.Errorf("the refusal is not a 403:\n%s", first)
	}
	// The body must name neither the destination nor the alias. A refusal that
	// echoed either would confirm both that the code is real and where it goes.
	for _, leak := range []string{"example.com", "blkactive", "blkexpired", "blkarchived"} {
		if strings.Contains(first, leak) {
			t.Errorf("the refusal names %q, which turns it into a confirmation "+
				"oracle:\n%s", leak, first)
		}
	}
}

// captureAs renders a response as comparable text: status line, the headers that
// are not per-connection, then the body.
//
// Date and Content-Length-adjacent transport headers are excluded by naming the
// ones that are compared rather than by filtering, so a header added later is
// compared by default instead of silently dropped.
func (f *botFixture) captureAs(path, ua string) string {
	f.t.Helper()
	resp := f.getAs(path, ua)
	defer func() { _ = resp.Body.Close() }()

	var b strings.Builder
	b.WriteString(resp.Status + "\n")
	for _, h := range []string{
		"Content-Type", "Content-Length", "Cache-Control", "X-Robots-Tag",
		"X-Content-Type-Options", "Location", "Retry-After",
	} {
		b.WriteString(h + ": " + resp.Header.Get(h) + "\n")
	}
	b.WriteString("\n")
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	b.Write(body[:n])
	return b.String()
}

// TestABlockedAttemptIsCountedNotAudited is the growth argument M21's threshold
// exists for, made concrete.
//
// A crawler that finds a blocked link asks for it thousands of times. Each one
// must produce a click event — the traffic is real and its owner should see it —
// and none may produce an audit row, because the audit log is for administrative
// change and a table that grows with crawler traffic stops being readable.
func TestABlockedAttemptIsCountedNotAudited(t *testing.T) {
	f := newBots(t)
	f.claim()
	id := f.createLink("counted", "https://example.com/counted")
	f.setLinkPolicy(id, "on")

	var auditBefore int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_logs`).Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}

	const attempts = 12
	for range attempts {
		if got := f.statusOf("/counted", botUA); got != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", got)
		}
	}
	if err := f.ingester.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	var clicks, bots int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*), count(*) FILTER (WHERE is_bot) FROM click_events WHERE link_id = $1`,
		id).Scan(&clicks, &bots); err != nil {
		t.Fatal(err)
	}
	if clicks != attempts {
		t.Errorf("%d click events for %d refusals; a blocked attempt is traffic and "+
			"has to be visible as traffic", clicks, attempts)
	}
	if bots != attempts {
		t.Errorf("%d of %d recorded clicks carry is_bot; the gate and the recorder "+
			"disagree about who is a bot, which is what calling one Classify was "+
			"meant to prevent", bots, attempts)
	}

	var auditAfter int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_logs`).Scan(&auditAfter); err != nil {
		t.Fatal(err)
	}
	if auditAfter != auditBefore {
		t.Errorf("%d audit rows were written by %d refusals; a crawler would turn "+
			"the audit log into a traffic log", auditAfter-auditBefore, attempts)
	}
}

// --- no new I/O --------------------------------------------------------------

// redirectQueries counts everything that reaches Postgres.
//
// Everything, not just queries mentioning links: the claim is that the decision
// costs no round trip, and a lookup of the domain written as its own statement
// would be exactly the regression a table-name filter would miss.
type redirectQueries struct {
	mu  sync.Mutex
	n   int
	sql []string
}

func (c *redirectQueries) TraceQueryStart(
	ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData,
) context.Context {
	c.mu.Lock()
	c.n++
	c.sql = append(c.sql, data.SQL)
	c.mu.Unlock()
	return ctx
}

func (c *redirectQueries) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *redirectQueries) reset() {
	c.mu.Lock()
	c.n, c.sql = 0, nil
	c.mu.Unlock()
}

func (c *redirectQueries) seen() (int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n, append([]string(nil), c.sql...)
}

// TestBlockingCostsNoQueryOnTheRedirectPath is the structural half of "no new
// I/O", and it is the constraint that decided this milestone's design.
//
// Blocking needs the domain's settings, and the obvious implementations both
// cost a round trip: look the domain up per request, or keep a second cache with
// its own invalidation and its own staleness window. Neither is affordable on
// the one path this project makes a latency promise about. So the settings ride
// home inside the query the resolver was already issuing, and this counts what
// actually reaches Postgres to say so.
//
// Two numbers, and both matter. A cached redirect issues nothing at all — with
// blocking on, which is the case that could have gained a lookup. An uncached
// one issues exactly one query, the same one it issued before this milestone
// added a join to it.
func TestBlockingCostsNoQueryOnTheRedirectPath(t *testing.T) {
	counter := &redirectQueries{}
	f := newBotsOn(t, newTracedDB(t, counter), false)
	f.claim()

	id := f.createLink("noio", "https://example.com/noio")
	f.setLinkPolicy(id, "on")
	f.setDomainPolicy(true, true)

	// Cold: the resolver has nothing cached, so this is the one query.
	counter.reset()
	if got := f.statusOf("/noio", botUA); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", got)
	}
	n, stmts := counter.seen()
	if n != 1 {
		t.Errorf("an uncached redirect issued %d queries, want 1:\n%s", n,
			strings.Join(stmts, "\n---\n"))
	}

	// Warm: everything the decision needs is in the snapshot.
	counter.reset()
	for range 10 {
		if got := f.statusOf("/noio", botUA); got != http.StatusForbidden {
			t.Fatalf("status = %d on a warm cache, want 403", got)
		}
		if got := f.statusOf("/noio", humanUA); got != http.StatusFound {
			t.Fatalf("status = %d for a browser on a warm cache, want 302", got)
		}
	}
	if n, stmts := counter.seen(); n != 0 {
		t.Errorf("20 cached redirects issued %d queries, want 0 — the bot decision "+
			"must read only what the cached snapshot already carries:\n%s",
			n, strings.Join(stmts, "\n---\n"))
	}
}

// --- invalidation ------------------------------------------------------------

// TestADomainChangeReachesEveryCachedLink is the expensive case the design
// bought with the join.
//
// Putting the domain's policy inside each link's snapshot is what makes the
// redirect path free; the bill is that changing the policy invalidates every one
// of those snapshots. A test that flipped the setting on a cold cache would pass
// on an implementation that invalidates nothing, so every link here is warmed
// first.
func TestADomainChangeReachesEveryCachedLink(t *testing.T) {
	f := newBots(t)
	f.claim()

	const n = 25
	aliases := make([]string, 0, n)
	for i := range n {
		alias := "dom" + string(rune('a'+i/5)) + string(rune('a'+i%5))
		f.createLink(alias, "https://example.com/"+alias)
		aliases = append(aliases, alias)
		// Warm, as a bot, so every snapshot is cached carrying "do not block".
		if got := f.statusOf("/"+alias, botUA); got != http.StatusFound {
			t.Fatalf("/%s = %d before blocking was on, want 302", alias, got)
		}
	}

	// Through the service, not by hand: the invalidation is the service's job
	// and a direct UPDATE would prove nothing about it.
	f.postFormOK("/account/bots", url.Values{"block_bots": {"1"}})

	for _, alias := range aliases {
		if got := f.statusOf("/"+alias, botUA); got != http.StatusForbidden {
			t.Errorf("/%s = %d after the domain switched blocking on, want 403; its "+
				"cached snapshot still carries the previous policy", alias, got)
		}
	}

	// And back, because an invalidation that only fires in the direction that
	// adds refusals would leave links refusing traffic after the setting was
	// turned off — the worse of the two failures.
	f.postFormOK("/account/bots", url.Values{})
	for _, alias := range aliases {
		if got := f.statusOf("/"+alias, botUA); got != http.StatusFound {
			t.Errorf("/%s = %d after blocking was switched off, want 302", alias, got)
		}
	}
}

func (f *botFixture) postFormOK(path string, vals url.Values) {
	f.t.Helper()
	resp := f.postForm(path, vals, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		f.t.Fatalf("POST %s returned %d, want 303", path, resp.StatusCode)
	}
}

// --- the override, at both surfaces ------------------------------------------

// TestAnEnforcingDomainRefusesALinkLevelOptOut asserts the same rule at the two
// places a link owner can reach it.
//
// The rule is that an enforcing domain wins, and domain.BlocksBots already makes
// it win unconditionally. So the refusal is not about correctness of the
// redirect — it is about not telling somebody their change was saved when it
// changes nothing. Both surfaces have to say so, because a dashboard that
// silently posts a value the API would refuse is the same lie in a nicer form.
func TestAnEnforcingDomainRefusesALinkLevelOptOut(t *testing.T) {
	f := newBots(t)
	f.claim()
	id := f.createLink("enforced", "https://example.com/enforced")
	f.postFormOK("/account/bots", url.Values{
		"block_bots": {"1"}, "block_bots_enforced": {"1"},
	})

	// The API.
	resp := f.patchJSON("/api/v1/links/"+id.String(), `{"bot_blocking":"off"}`)
	body := readBody(f.t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("PATCH bot_blocking=off returned %d, want 422:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "domain_enforced") {
		t.Errorf("the refusal does not name why:\n%s", body)
	}

	// Inherit is still accepted: it resolves to blocking, so it overrides
	// nothing and refusing it would be refusing agreement.
	resp = f.patchJSON("/api/v1/links/"+id.String(), `{"bot_blocking":"inherit"}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("PATCH bot_blocking=inherit returned %d under an enforcing domain, "+
			"want 200:\n%s", resp.StatusCode, readBody(f.t, resp))
	} else {
		_ = resp.Body.Close()
	}

	// The dashboard.
	page := f.getBody("/links/" + id.String())
	control := sliceAround(page, `name="bot_blocking"`, 400)
	if control == "" {
		t.Fatal("the link detail page has no bot_blocking control at all")
	}
	if !strings.Contains(control, "disabled") {
		t.Errorf("the control is editable while the domain enforces, so the form "+
			"posts a value the service refuses:\n%s", control)
	}
	if !strings.Contains(page, "enforced for every link") {
		t.Error("the page does not say why the control cannot be changed")
	}

	// And it is editable when the domain merely blocks, or the assertion above
	// would pass on a control that is always disabled.
	f.postFormOK("/account/bots", url.Values{"block_bots": {"1"}})
	control = sliceAround(f.getBody("/links/"+id.String()), `name="bot_blocking"`, 400)
	if strings.Contains(control, "disabled") {
		t.Errorf("the control is disabled under a domain that only blocks by "+
			"default; a link may opt out of that:\n%s", control)
	}
}

// TestBotBlockingChangesAreAudited covers the administrative half.
//
// Two actions rather than one, because they are two grants: a link owner with
// links.update changed one link, and somebody with domains.write changed every
// link on the instance at once. An operator asking who did the second is not
// asking the same question as who did the first.
func TestBotBlockingChangesAreAudited(t *testing.T) {
	f := newBots(t)
	f.claim()
	id := f.createLink("audited", "https://example.com/audited")

	resp := f.patchJSON("/api/v1/links/"+id.String(), `{"bot_blocking":"on"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH returned %d:\n%s", resp.StatusCode, readBody(f.t, resp))
	}
	_ = resp.Body.Close()

	f.postFormOK("/account/bots", url.Values{
		"block_bots": {"1"}, "block_bots_enforced": {"1"},
	})

	for action, wantMeta := range map[string]string{
		audit.ActionLinkBotBlockingChanged:   `"to": "on"`,
		audit.ActionDomainBotBlockingChanged: `"to": "enforced"`,
	} {
		var metadata string
		if err := f.pool.QueryRow(t.Context(),
			`SELECT metadata::text FROM audit_logs WHERE action = $1`, action).
			Scan(&metadata); err != nil {
			t.Fatalf("no %s audit record: %v", action, err)
		}
		if !strings.Contains(strings.ReplaceAll(metadata, " ", ""),
			strings.ReplaceAll(wantMeta, " ", "")) {
			t.Errorf("%s metadata = %s, want it to carry %s", action, metadata, wantMeta)
		}
	}

	// Re-sending the value it already has writes nothing. The dashboard form
	// posts every field on every save, so a log that recorded no-ops would fill
	// with entries saying nothing changed.
	var before int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM audit_logs WHERE action = $1`,
		audit.ActionLinkBotBlockingChanged).Scan(&before); err != nil {
		t.Fatal(err)
	}
	resp = f.patchJSON("/api/v1/links/"+id.String(), `{"bot_blocking":"on"}`)
	_ = resp.Body.Close()
	var after int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM audit_logs WHERE action = $1`,
		audit.ActionLinkBotBlockingChanged).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("re-sending the current value wrote %d more audit rows", after-before)
	}
}

// TestEnforcingWithoutBlockingIsRefused covers the state the schema will not
// hold. The CHECK would refuse it too, as a 500 naming a constraint; the
// service refuses it first, naming the box to tick.
func TestEnforcingWithoutBlockingIsRefused(t *testing.T) {
	f := newBots(t)
	f.claim()

	resp := f.postForm("/account/bots", url.Values{"block_bots_enforced": {"1"}}, nil)
	body := readBody(f.t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "requires turning it on") {
		t.Errorf("the refusal does not say what to do:\n%s", sliceAround(body, "block_bots_enforced", 400))
	}

	var block, enforced bool
	if err := f.pool.QueryRow(t.Context(),
		`SELECT block_bots, block_bots_enforced FROM domains WHERE id = $1`, f.domainID).
		Scan(&block, &enforced); err != nil {
		t.Fatal(err)
	}
	if block || enforced {
		t.Errorf("the refused submission still changed the stored settings to (%v, %v)",
			block, enforced)
	}
}

// --- small helpers -----------------------------------------------------------

func (f *botFixture) patchJSON(path, body string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPatch,
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

func (f *botFixture) getBody(path string) string {
	f.t.Helper()
	resp := f.getAs(path, humanUA)
	return readBody(f.t, resp)
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b := make([]byte, 256*1024)
	n, _ := resp.Body.Read(b)
	total := n
	for n > 0 && total < len(b) {
		n, _ = resp.Body.Read(b[total:])
		total += n
	}
	return string(b[:total])
}

// sliceAround returns the window of s around the first occurrence of needle, so
// a failure prints the control rather than the whole page.
func sliceAround(s, needle string, width int) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return ""
	}
	start := max(i-width, 0)
	end := min(i+width, len(s))
	return s[start:end]
}
