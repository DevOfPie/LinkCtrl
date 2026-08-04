//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/dispute"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/feed"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// Opt-in reputation feeds (M32), against a real database and a real feed.
//
// The unit tests hold the adapter's own behaviour and the structural claims —
// off means no client, a verdict cannot name a tier, nothing else in
// internal/link reaches the network. What can only be asserted here is the part
// the milestone is actually about: that a default instance sends nothing
// anywhere, that turning a feed on changes no built-in tier's answer, that a
// feed which errors is a feed that decides nothing, and that the instance owner
// can overrule a verdict and stop the sending with it.

const feedPassword = "a-sufficiently-long-password"

// thirdParty is a reputation feed under the test's control. Every request it
// receives is recorded, because "what left the box" is the measurement most of
// this file is making.
type thirdParty struct {
	mu   sync.Mutex
	sent []string
	// verdict decides the answer for a destination; the default is clean.
	verdict func(destination string) (status int, body string)
	srv     *httptest.Server
}

func newThirdParty(t *testing.T) *thirdParty {
	t.Helper()
	tp := &thirdParty{}
	tp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		tp.mu.Lock()
		tp.sent = append(tp.sent, string(body))
		verdict := tp.verdict
		tp.mu.Unlock()

		status, out := http.StatusOK, `{"blocked":false}`
		if verdict != nil {
			status, out = verdict(string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(tp.srv.Close)
	return tp
}

// received is every request body the feed was sent, oldest first.
func (tp *thirdParty) received() []string {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return append([]string(nil), tp.sent...)
}

func (tp *thirdParty) sawHost(host string) bool {
	for _, body := range tp.received() {
		if strings.Contains(body, host) {
			return true
		}
	}
	return false
}

// blocks makes the feed refuse any destination whose URL contains needle.
func (tp *thirdParty) blocks(needle string) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.verdict = func(body string) (int, string) {
		if strings.Contains(body, needle) {
			return http.StatusOK, `{"blocked":true}`
		}
		return http.StatusOK, `{"blocked":false}`
	}
}

// breaks makes every check fail, which is the state the fail-open promise is
// about.
func (tp *thirdParty) breaks() {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.verdict = func(string) (int, string) {
		return http.StatusInternalServerError, `{"error":"down"}`
	}
}

func (tp *thirdParty) client(t *testing.T) *feed.Client {
	t.Helper()
	c, err := feed.New(feed.Config{
		Name: "Example Reputation", URL: tp.srv.URL, Method: feed.MethodPOST,
		Param: "url", VerdictField: "blocked", Timeout: 2 * time.Second,
		Transport: tp.srv.Client().Transport,
	})
	if err != nil {
		t.Fatalf("build feed client: %v", err)
	}
	return c
}

// feedFixture is the destination path with a feed wired the way main.go wires
// one, plus the dispute queue that argues with it.
type feedFixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	links    *link.Service
	disputes *dispute.Service
	notify   *notify.Service
	metrics  *observability.Metrics
	owner    *auth.Identity
	ctx      context.Context
}

// newFeedFixture builds the services. checker is nil for the default instance,
// which is the case half this file is about.
func newFeedFixture(t *testing.T, checker link.FeedChecker) *feedFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	if _, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner", Password: feedPassword,
	}); err != nil {
		t.Fatalf("register owner: %v", err)
	}

	auditSvc := audit.NewService(pool)
	notifySvc := notify.NewService(pool)
	metrics := observability.NewMetrics()
	links := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: "http://lnk.test",
		SplitHosts: true, Audit: auditSvc,
		Feed: checker, FeedMetrics: metrics,
	})
	disputes, err := dispute.NewService(pool, dispute.Config{
		Judge: links, Audit: auditSvc, Notify: notifySvc,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := auth.WithClientIP(t.Context(), netip.MustParseAddr("198.51.100.9"))
	owner, err := authSvc.IdentityForEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return &feedFixture{
		t: t, pool: pool, links: links, disputes: disputes,
		notify: notifySvc, metrics: metrics, owner: owner, ctx: ctx,
	}
}

// create attempts a link and reports the reason code it was refused with, or ""
// when it was accepted.
func (f *feedFixture) create(dest, alias string) string {
	f.t.Helper()
	_, err := f.links.Create(f.ctx, f.owner, link.CreateInput{URL: dest, Alias: alias})
	if err == nil {
		return ""
	}
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		f.t.Fatalf("create %s: %v", dest, err)
	}
	if len(ve) == 0 {
		f.t.Fatalf("create %s was refused with no field errors", dest)
	}
	return ve[0].Code
}

// scrape reads this fixture's own metrics registry as an operator would.
func (f *feedFixture) scrape() string {
	f.t.Helper()
	srv := httptest.NewServer(f.metrics.Handler())
	defer srv.Close()
	req, err := http.NewRequestWithContext(f.ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
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

// TestNoDestinationLeavesAnInstanceWithNoFeed is the milestone's first bullet.
//
// A feed is listening and answering the whole time; what makes this a real
// assertion rather than a tautology is that the *same server* is proven
// reachable from the same process by the second half — so a silent count of
// zero cannot be a broken fixture.
//
// Every surface that judges a destination is exercised: link create, link
// update, the root-redirect setting, and filing a dispute. The last one matters
// most, because it is the one that re-judges a URL somebody has already been
// refused for, and it is the surface a reader would forget.
func TestNoDestinationLeavesAnInstanceWithNoFeed(t *testing.T) {
	tp := newThirdParty(t)

	// The default instance: no feed, and therefore no client at all.
	off := newFeedFixture(t, nil)
	if code := off.create("https://quiet.example/one", "qone"); code != "" {
		t.Fatalf("create was refused with %q on an instance with no feed", code)
	}
	created, err := off.links.Create(off.ctx, off.owner,
		link.CreateInput{URL: "https://quiet.example/two", Alias: "qtwo"})
	if err != nil {
		t.Fatal(err)
	}
	newDest := "https://quiet.example/edited"
	if _, err := off.links.Update(off.ctx, off.owner, created.ID,
		link.UpdateInput{URL: &newDest}); err != nil {
		t.Fatal(err)
	}
	root := "https://quiet.example/root"
	if _, err := off.links.SetRootRedirect(off.ctx, off.owner, root); err != nil {
		t.Fatalf("set root redirect: %v", err)
	}
	// A refusal that can be disputed, so the dispute path judges a URL too.
	if _, err := off.pool.Exec(off.ctx,
		`INSERT INTO blocked_destinations (host, source, reason) VALUES ('listed.example', 'review', 'test')`,
	); err != nil {
		t.Fatal(err)
	}
	if code := off.create("https://listed.example/x", "qthree"); code != "low_confidence.operator_blocklist" {
		t.Fatalf("refusal code = %q", code)
	}
	if _, err := off.disputes.File(off.ctx, off.owner, "https://listed.example/x"); err != nil {
		t.Fatalf("file dispute: %v", err)
	}

	if sent := tp.received(); len(sent) != 0 {
		t.Fatalf("an instance with no feed sent %d request(s) to a third party: %q",
			len(sent), sent)
	}

	// The control. Same process, same server, a feed configured: it is reached.
	// Without this the assertion above would also pass against a test server
	// that was never listening.
	on := newFeedFixture(t, tp.client(t))
	if code := on.create("https://quiet.example/one", "cone"); code != "" {
		t.Fatalf("create was refused with %q", code)
	}
	if len(tp.received()) == 0 {
		t.Fatal("with a feed configured nothing reached it; the zero above proves nothing")
	}
}

// TestEveryBuiltInTierAnswersTheSameWithAFeedOnOffOrErroring is the bullet's own
// wording, as a table.
//
// The feed is asked last and only about destinations every built-in tier already
// accepted, so this is really an assertion about ordering — and the third column
// is the one that would catch a regression: a feed that errors must not change
// an answer either, or a third party's outage would silently alter what this
// instance refuses.
//
// The erroring feed is also proof of something the other two columns cannot
// show: the unappealable and high-confidence refusals never reached it at all,
// because the feed is asked last. What that bounds is the feed and not the
// instance — the refusal emits `destination.blocked`, and a workspace with a
// webhook subscribed to it receives the refused destination, defanged.
func TestEveryBuiltInTierAnswersTheSameWithAFeedOnOffOrErroring(t *testing.T) {
	// One destination per tier, and the code each must answer with whatever the
	// feed is doing.
	cases := []struct {
		name string
		dest string
		want string
	}{
		{"unappealable: metadata address", "http://169.254.169.254/latest/", "unappealable.private_address"},
		{"unappealable: loopback", "http://127.0.0.1:8080/admin", "unappealable.private_address"},
		{"high confidence: embedded host", "https://metadata.google.internal/x", "high_confidence.embedded_host"},
		{"low confidence: shortener", "https://bit.ly/abc", "low_confidence.shortener_chain"},
		{"low confidence: operator list", "https://listed.example/x", "low_confidence.operator_blocklist"},
		{"low confidence: credentials", "https://user:pw@ok.example/x", "low_confidence.url_credentials"},
		{"low confidence: homograph", "https://xn--80ak6aa92e.com/", "low_confidence.punycode_homograph"},
		{"accepted", "https://ordinary.example/x", ""},
	}

	run := func(t *testing.T, name string, checker link.FeedChecker) map[string]string {
		t.Helper()
		f := newFeedFixture(t, checker)
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO blocked_destinations (host, source, reason) VALUES ('listed.example', 'review', 'test')`,
		); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for i, tc := range cases {
			got[tc.name] = f.create(tc.dest, alias(name, i))
		}
		return got
	}

	// A feed that refuses everything it is asked about. If any built-in answer
	// moves, the feed has been consulted somewhere it must not be.
	greedy := newThirdParty(t)
	greedy.blocks("")

	broken := newThirdParty(t)
	broken.breaks()

	results := map[string]map[string]string{
		"off":      run(t, "off", nil),
		"erroring": run(t, "erroring", broken.client(t)),
	}
	for state, got := range results {
		for _, tc := range cases {
			if got[tc.name] != tc.want {
				t.Errorf("[feed %s] %s → %q, want %q", state, tc.name, got[tc.name], tc.want)
			}
		}
	}

	// A feed that objects to everything changes exactly one answer: the
	// destination nothing built in refused. Everything else is identical,
	// because the feed never saw it.
	withGreedy := run(t, "on", greedy.client(t))
	for _, tc := range cases {
		want := tc.want
		if tc.want == "" {
			want = "low_confidence.feed_reputation"
		}
		if withGreedy[tc.name] != want {
			t.Errorf("[feed on] %s → %q, want %q", tc.name, withGreedy[tc.name], want)
		}
	}
	// The destinations the built-in tiers refused never reached the feed, which
	// is the stronger form of "the feed is not the mechanism". It bounds the
	// feed and not the instance: the same refusal emits `destination.blocked`,
	// which carries that destination, defanged, to any webhook a workspace
	// administrator subscribed to it.
	for _, host := range []string{"169.254.169.254", "127.0.0.1", "metadata.google.internal",
		"bit.ly", "listed.example", "xn--80ak6aa92e.com"} {
		if greedy.sawHost(host) {
			t.Errorf("%s was sent to the feed, but a built-in tier had already refused it; "+
				"a destination refused locally must never reach the feed", host)
		}
	}
}

// TestAFeedFailureFailsOpenAndIsCounted is the fourth bullet.
//
// Fails open because the built-in tiers had already accepted the destination,
// and a third party's outage must not decide that this instance may not create
// links. Counted because failing open is *invisible* — the product behaves
// exactly as it does with no feed at all, so an operator relying on a feed that
// quietly stopped answering would find out by noticing nothing was ever refused.
func TestAFeedFailureFailsOpenAndIsCounted(t *testing.T) {
	broken := newThirdParty(t)
	broken.breaks()
	f := newFeedFixture(t, broken.client(t))

	if code := f.create("https://ordinary.example/x", "foone"); code != "" {
		t.Fatalf("a failing feed refused the destination with %q; it must fail open", code)
	}
	if len(broken.received()) == 0 {
		t.Fatal("the feed was never asked, so nothing failed open")
	}

	scrape := f.scrape()
	want := `linkctrl_destination_feed_checks_total{result="error"} 1`
	if !strings.Contains(scrape, want) {
		t.Errorf("the failure was not counted; looked for %q in:\n%s", want, feedSeries(scrape))
	}

	// A healthy feed counts separately, so the two are distinguishable in an
	// alert. A single "checks" counter would make an outage look like traffic.
	healthy := newThirdParty(t)
	g := newFeedFixture(t, healthy.client(t))
	if code := g.create("https://ordinary.example/x", "fotwo"); code != "" {
		t.Fatalf("refused with %q", code)
	}
	if s := g.scrape(); !strings.Contains(s, `linkctrl_destination_feed_checks_total{result="clean"} 1`) {
		t.Errorf("a successful check was not counted:\n%s", feedSeries(s))
	}
}

// TestAFeedVerdictIsDisputableAndTheOwnerCanOverruleIt is the third bullet's
// second half, and the only one that spans three tables.
//
// The chain: the feed refuses, the person who was refused files a dispute (which
// re-judges, and therefore asks the feed again), the owner allows it, and the
// destination becomes creatable. The last assertion is the one that makes the
// override real rather than cosmetic — an allow that leaves the destination
// refused would be a button reporting success and doing nothing.
//
// And then the part that is not obvious: after the override, the host stops
// being sent. Overruling a feed's opinion about a host is also a decision not to
// keep asking about it, which is the only way an override could be honest on a
// verdict re-derived from a live third party on every write.
func TestAFeedVerdictIsDisputableAndTheOwnerCanOverruleIt(t *testing.T) {
	tp := newThirdParty(t)
	tp.blocks("flagged.example")
	f := newFeedFixture(t, tp.client(t))

	const dest = "https://flagged.example/promo"
	if code := f.create(dest, "fdone"); code != "low_confidence.feed_reputation" {
		t.Fatalf("refusal code = %q, want low_confidence.feed_reputation", code)
	}

	filed, err := f.disputes.File(f.ctx, f.owner, dest)
	if err != nil {
		t.Fatalf("a feed verdict could not be disputed: %v", err)
	}
	if filed.ReasonCode != "low_confidence.feed_reputation" {
		t.Errorf("dispute reason = %q", filed.ReasonCode)
	}
	if !filed.Liftable {
		t.Error("the queue reports a feed verdict as unliftable, so the owner is shown " +
			"no Allow button for the one refusal the bullet requires be overridable")
	}

	decided, err := f.disputes.Allow(f.ctx, f.owner, filed.ID)
	if err != nil {
		t.Fatalf("allow: %v", err)
	}
	if decided.Status != dispute.StatusAllowed {
		t.Fatalf("status = %q", decided.Status)
	}

	// The override is real: the same destination is now creatable.
	before := len(tp.received())
	if code := f.create(dest, "fdtwo"); code != "" {
		t.Fatalf("after the owner allowed it the destination is still refused with %q", code)
	}
	// And it is not sent any more. The lookup happens before the request, so
	// overruling the verdict stops the egress as well as the refusal.
	if after := len(tp.received()); after != before {
		t.Errorf("the feed was asked %d more time(s) about a host the owner allowed; "+
			"an override that keeps sending the host is not an override of the "+
			"disclosure, only of the answer", after-before)
	}

	// The allow deleted no blocklist row — there was none — so the audit record
	// has to say which kind of decision it was, or a reader sees an allow that
	// apparently did nothing.
	var meta string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT metadata::text FROM audit_logs WHERE action = $1 ORDER BY occurred_at DESC LIMIT 1`,
		dispute.ActionDisputeAllowed).Scan(&meta); err != nil {
		t.Fatalf("read audit record: %v", err)
	}
	if !strings.Contains(meta, "feed_verdict_overridden") {
		t.Errorf("the audit record does not say the allow overrode a feed verdict: %s", meta)
	}
	if strings.Contains(meta, "blocklist_entry_removed") {
		t.Errorf("the audit record claims a blocklist entry was removed: %s", meta)
	}

	// Narrow, deliberately. Allowing evil.example says nothing about
	// login.evil.example, so a subdomain is still asked about and still refused.
	if code := f.create("https://login.flagged.example/promo", "fdthree"); code != "low_confidence.feed_reputation" {
		t.Errorf("a subdomain of an allowed host answered %q; the override must not "+
			"widen to a label boundary nobody decided on", code)
	}
}

// TestTheDisclosureIsReadableAndAcceptsNoWrite is D40's mechanism against the
// live router.
//
// Two claims. Every signed-in account can read it — not only the owner, because
// what it describes is what happens to *their* destinations, and a disclosure
// only the person who configured it can see is not one. And no unsafe method is
// accepted on either surface: D38 removed the ability to change instance-wide
// settings from the dashboard, and the way that gets reversed is not a decision
// but a POST handler somebody adds next to a page that already looks like
// settings.
func TestTheDisclosureIsReadableAndAcceptsNoWrite(t *testing.T) {
	f := newWeb(t)
	f.claim()

	page := f.body(f.get("/feeds", nil))
	if !strings.Contains(page, "No destination leaves this instance") {
		t.Errorf("the disclosure page did not render its default state:\n%s", firstLines(page))
	}
	// The page is reachable from the chrome, or nobody finds it.
	if dash := f.body(f.get("/dashboard", nil)); !strings.Contains(dash, `href="/feeds"`) {
		t.Error("no link to /feeds in the dashboard header; a disclosure nobody can " +
			"navigate to is a file, not a page")
	}

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	} {
		for _, path := range []string{"/feeds", "/api/v1/feeds"} {
			req, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := f.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s → %d, want 405. The disclosure is read-only by D40: "+
					"reading what an instance does with your destinations needs no "+
					"principal, and changing it needs one this product does not have.",
					method, path, resp.StatusCode)
			}
		}
	}
}

// alias makes a distinct alias per case, since every create in a table shares
// one fixture.
func alias(prefix string, i int) string {
	return prefix + string(rune('a'+i))
}

func feedSeries(scrape string) string {
	var out []string
	for _, line := range strings.Split(scrape, "\n") {
		if strings.Contains(line, "destination_feed") {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return "(no linkctrl_destination_feed_checks_total series at all)"
	}
	return strings.Join(out, "\n")
}

func firstLines(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 20 {
		lines = lines[:20]
	}
	return strings.Join(lines, "\n")
}
