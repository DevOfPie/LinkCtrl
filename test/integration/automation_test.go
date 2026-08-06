//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/automation"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/webhook"
)

// Automation rules end to end (M43).
//
// **The test that carries this milestone is TestARuleFiresOnceForOneSubject.**
// m43.md asks for a rule that cannot trigger itself or loop, and the guard is
// the watermark: `last_fired_at` is advanced past the last subject handled,
// inside a compare-and-set, before any action runs. Break that and the rule
// fires on the same link on every tick forever, which is the runaway an
// automation product exists to not have.
//
// It is written so that removing the guard is *visible*, which is the failure
// mode a loop test has: a rule that never fires also never loops, so "fired at
// most once" passes on a rule that is completely broken. Every assertion here
// therefore checks the exact count on the first run and again after the third —
// one, then still one — and TestARuleFiresAgainForANewSubject holds the other
// end, that the watermark advances to the subject rather than to infinity.

// automationFixture is the whole product under one clock.
//
// The clock is injectable because every claim here is about a half-open window:
// a test has to be able to put a link's expiry on either side of a watermark
// without sleeping for a minute.
type automationFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	links *link.Service
	rules *automation.Service
	hooks *webhook.Service
	notes *notify.Service
	owner *auth.Identity
	obs   *firingObserver

	// offset is how far ahead of the wall clock this fixture's evaluator is.
	//
	// An offset rather than a fixed instant, and that is not a detail. The
	// evaluator's clock is injectable but internal/link's is not — a rule is
	// armed with time.Now() inside CreateAutomationRule — so a fixture holding a
	// frozen instant would be comparing two clocks with no relationship, and
	// every window assertion would depend on how long argon2 took. With an
	// offset, "a rule armed a moment ago" is always exactly `offset` behind the
	// evaluator, whatever the machine was doing.
	mu     sync.Mutex
	offset time.Duration
}

// firingObserver records the metric labels, so the cardinality claim is
// asserted rather than described.
type firingObserver struct {
	mu     sync.Mutex
	counts map[[2]string]int
}

func (f *firingObserver) ObserveAutomationFiring(trigger, outcome string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts == nil {
		f.counts = map[[2]string]int{}
	}
	f.counts[[2]string{trigger, outcome}]++
}

func (f *firingObserver) get(trigger, outcome string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[[2]string{trigger, outcome}]
}

func newAutomation(t *testing.T) *automationFixture {
	t.Helper()
	pool := newDB(t)

	f := &automationFixture{t: t, pool: pool, obs: &firingObserver{}}

	// No transport: nothing in this file delivers a webhook, and nothing should.
	// The claim being made about the webhook action is that it *queues* one
	// event per firing, which is a row in webhook_deliveries; whether that row
	// reaches a receiver is M42's business and M42's test file.
	f.hooks = webhook.NewService(pool, webhook.Config{Logger: quietLogger()})
	f.notes = notify.NewService(pool)
	f.links = link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: "http://links.test",
		Audit: audit.NewService(pool), Events: f.hooks, Log: quietLogger(),
	})
	f.rules = automation.NewService(pool, automation.Config{
		Links: f.links, Notifier: f.notes, Events: f.hooks,
		Audit: audit.NewService(pool), Logger: quietLogger(), Observer: f.obs,
		Now: f.clock,
	})

	authSvc := auth.NewService(pool, auth.ServiceConfig{Params: fastParams})
	owner, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner",
		Password: webPassword, IsFirstUser: true,
	})
	if err != nil {
		t.Fatalf("claim the instance: %v", err)
	}
	f.owner = owner
	return f
}

func (f *automationFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return time.Now().UTC().Add(f.offset)
}

// advance moves the scheduler's clock forward relative to the writing side.
//
// Every test calls it between arming a rule and creating the subject the rule is
// supposed to see, because that gap is exactly what the watermark measures: a
// rule armed at the wall clock only matches events after it.
func (f *automationFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offset += d
}

// evaluate runs one scheduler pass.
func (f *automationFixture) evaluate() {
	f.t.Helper()
	if err := f.rules.Evaluate(f.t.Context()); err != nil {
		f.t.Fatalf("evaluate: %v", err)
	}
}

func (f *automationFixture) rule(name, trigger string, minCount int, actions ...string) *domain.AutomationRule {
	f.t.Helper()
	rule, err := f.links.CreateAutomationRule(f.t.Context(), f.owner, link.CreateAutomationRuleInput{
		Name: name, Trigger: trigger, Actions: actions, Enabled: true,
		TriggerConfig: domain.AutomationTriggerConfig{MinCount: minCount},
	})
	if err != nil {
		f.t.Fatalf("create rule %q: %v", name, err)
	}
	return rule
}

// expiredLink writes a link whose expiry is `ago` before the fixture clock.
//
// The expiry is set with a statement rather than through the service, because
// what this file needs is a link that has *already* expired and the service
// refuses a expiry in the past — correctly, since a link created already-dead is
// a mistake rather than a feature.
func (f *automationFixture) expiredLink(alias string, ago time.Duration) uuid.UUID {
	f.t.Helper()
	created, err := f.links.Create(f.t.Context(), f.owner, link.CreateInput{
		URL: "https://example.com/" + alias, Alias: alias,
	})
	if err != nil {
		f.t.Fatalf("create link %s: %v", alias, err)
	}
	return f.setExpiry(created.ID, alias, f.clock().Add(-ago))
}

// expiredLinkAt writes a link whose expiry is an exact instant, for the tests
// that have to place a subject between two watermarks rather than merely before
// the current one.
func (f *automationFixture) expiredLinkAt(alias string, at time.Time) uuid.UUID {
	f.t.Helper()
	created, err := f.links.Create(f.t.Context(), f.owner, link.CreateInput{
		URL: "https://example.com/" + alias, Alias: alias,
	})
	if err != nil {
		f.t.Fatalf("create link %s: %v", alias, err)
	}
	return f.setExpiry(created.ID, alias, at)
}

func (f *automationFixture) setExpiry(id uuid.UUID, alias string, at time.Time) uuid.UUID {
	f.t.Helper()
	if _, err := f.pool.Exec(f.t.Context(),
		`UPDATE links SET expires_at = $2 WHERE id = $1`, id, at); err != nil {
		f.t.Fatalf("expire link %s: %v", alias, err)
	}
	return id
}

// counts reads what a firing leaves behind, in one place so every test asserts
// the same three things.
type firingCounts struct {
	Notifications int
	Deliveries    int
	AuditRecords  int
}

func (f *automationFixture) counts() firingCounts {
	f.t.Helper()
	var c firingCounts
	row := f.pool.QueryRow(f.t.Context(), `
		SELECT (SELECT count(*) FROM notifications WHERE kind = $1),
		       (SELECT count(*) FROM webhook_deliveries WHERE event = $2),
		       (SELECT count(*) FROM audit_logs WHERE action = $3)`,
		notify.KindAutomationFired, domain.EventAutomationFired, audit.ActionAutomationFired)
	if err := row.Scan(&c.Notifications, &c.Deliveries, &c.AuditRecords); err != nil {
		f.t.Fatalf("read firing counts: %v", err)
	}
	return c
}

func (f *automationFixture) linkStatus(id uuid.UUID) string {
	f.t.Helper()
	var status string
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT status FROM links WHERE id = $1`, id).Scan(&status); err != nil {
		f.t.Fatalf("read link status: %v", err)
	}
	return status
}

func (f *automationFixture) expiry(id uuid.UUID) time.Time {
	f.t.Helper()
	var at time.Time
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT expires_at FROM links WHERE id = $1`, id).Scan(&at); err != nil {
		f.t.Fatalf("read link expiry: %v", err)
	}
	return at.UTC()
}

// watermark reads both halves of a rule's resume position: the instant, and the
// id of the boundary subject. The subject half is nil until the rule first
// fires, and nil again after a re-arm — nil means "the instant is fully spent",
// which is how rows without a recorded boundary keep the strict `>` semantics
// they were written under.
func (f *automationFixture) watermark(id uuid.UUID) (time.Time, *uuid.UUID) {
	f.t.Helper()
	var at *time.Time
	var subject *uuid.UUID
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT last_fired_at, last_fired_subject_id FROM automation_rules WHERE id = $1`,
		id).Scan(&at, &subject); err != nil {
		f.t.Fatalf("read watermark: %v", err)
	}
	if at == nil {
		f.t.Fatal("the watermark is NULL; every write path is supposed to arm a rule, " +
			"and a NULL watermark means a rule that will fire for the whole history " +
			"of the workspace")
	}
	return at.UTC(), subject
}

// subscribe registers a webhook for every event, directly.
//
// Directly, because CreateWebhook refuses a URL that resolves to loopback and
// there is nowhere else for a test URL to point. Nothing here delivers anything
// — see the fixture — so the row exists only to make the fan-out query match.
func (f *automationFixture) subscribe() {
	f.t.Helper()
	if _, err := f.pool.Exec(f.t.Context(), `
		INSERT INTO webhooks (id, workspace_id, url, secret, events, description, enabled)
		VALUES ($1, $2, 'https://receiver.example/hook', $3, $4::text[], 'automation test', true)`,
		uuid.Must(uuid.NewV7()), f.owner.WorkspaceID, []byte("not-a-real-secret"),
		domain.WebhookEvents); err != nil {
		f.t.Fatalf("subscribe: %v", err)
	}
}

// TestARuleFiresOnceForOneSubject is the milestone's second hard claim.
//
// One link, expired. One rule, with all three actions. Three evaluations. The
// assertion is *exactly one* of everything after the first run and *still
// exactly one* after the third — not "at most one", because a rule that never
// fires satisfies "at most" and is what a broken guard looks like from the
// outside.
//
// The three counts are asserted rather than one, because they fail differently:
// a second notification is an inbox somebody stops reading, a second delivery is
// an event a receiver double-counts, and a second archive would be silent
// (archiving an archived link changes nothing) which is exactly why the audit
// record is counted instead of the status.
func TestARuleFiresOnceForOneSubject(t *testing.T) {
	f := newAutomation(t)
	f.subscribe()

	// The rule is armed first and the subject appears afterwards, which is the
	// only order that fires anything: a rule acts on what happens after it
	// exists, and TestARuleActsOnlyOnWhatHappensAfterItExists is the assertion
	// that this is a property rather than an accident of this fixture.
	rule := f.rule("archive what expired", domain.TriggerLinkExpired, 1,
		domain.ActionNotify, domain.ActionWebhook, domain.ActionArchiveLink)
	f.advance(time.Minute)
	linkID := f.expiredLink("expired-once", time.Second)

	f.evaluate()
	first := f.counts()
	if first != (firingCounts{Notifications: 1, Deliveries: 1, AuditRecords: 1}) {
		t.Fatalf("after one evaluation: %+v, want exactly one of each. A zero here is "+
			"the rule not firing at all, which would make every 'fires once' "+
			"assertion below vacuous.", first)
	}
	if got := f.linkStatus(linkID); got != string(domain.StatusArchived) {
		t.Fatalf("link status is %q, want archived: the archive_link action did not run", got)
	}

	// The watermark moved to the subject's own position — its expiry and its id,
	// not the clock. That is what makes a truncated run pick up its remainder
	// instead of skipping it, tied subjects included.
	got, gotSubject := f.watermark(rule.ID)
	if want := f.expiry(linkID); !got.Equal(want) {
		t.Errorf("watermark is %s, want the subject's expiry %s", got, want)
	}
	if gotSubject == nil || *gotSubject != linkID {
		t.Errorf("watermark subject is %v, want the link %s: without the id half, a "+
			"capped run that stops between two links sharing one expiry cannot say "+
			"which of them it handled", gotSubject, linkID)
	}

	// Two more passes, with the clock moving. Nothing new has happened, so
	// nothing new may be produced.
	f.advance(time.Minute)
	f.evaluate()
	f.advance(time.Minute)
	f.evaluate()

	if again := f.counts(); again != first {
		t.Errorf("after three evaluations: %+v, want %+v. The rule fired again for a "+
			"subject it had already handled — this is the runaway m43.md's "+
			"'a rule cannot trigger itself' names, and the guard is the watermark "+
			"advance in ClaimAutomationRule.", again, first)
	}
	if got := f.obs.get(domain.TriggerLinkExpired, "fired"); got != 1 {
		t.Errorf("the firing metric counted %d, want 1", got)
	}
}

// TestArchivingDoesNotRearmTheTrigger is the cascade guard, at the one place in
// this tree where the cycle is actually reachable.
//
// "link expired -> archive link" is a rule whose action writes the same table
// its trigger reads. The reason it is not a loop is that the two read *different
// columns*: the trigger reads `expires_at` and the archive writes `status`, and
// domain/automation.go declares them as separate sources for exactly this
// reason. An archive that moved the expiry — which is a perfectly natural way to
// implement one — would put the link back inside the next window, forever.
func TestArchivingDoesNotRearmTheTrigger(t *testing.T) {
	f := newAutomation(t)
	f.rule("archive what expired", domain.TriggerLinkExpired, 1, domain.ActionArchiveLink)
	f.advance(time.Minute)
	linkID := f.expiredLink("expired-archive", time.Second)
	before := f.expiry(linkID)

	f.evaluate()

	if got := f.linkStatus(linkID); got != string(domain.StatusArchived) {
		t.Fatalf("link status is %q, want archived", got)
	}
	if after := f.expiry(linkID); !after.Equal(before) {
		t.Fatalf("the archive moved expires_at from %s to %s. The link-expired "+
			"trigger reads that column, so the link is now inside the next "+
			"window and the rule will archive it again on every tick.", before, after)
	}
}

// TestARuleFiresAgainForANewSubject is the other end of the watermark, and it is
// what stops TestARuleFiresOnceForOneSubject passing for the wrong reason.
//
// A guard that simply never let a rule fire twice — a boolean, say — would pass
// that test and would also break the feature: the second link to expire would be
// ignored forever. The watermark is a position, not a latch, and this asserts it
// moves rather than closes.
func TestARuleFiresAgainForANewSubject(t *testing.T) {
	f := newAutomation(t)
	f.rule("tell me what expired", domain.TriggerLinkExpired, 1, domain.ActionNotify)
	f.advance(time.Minute)
	f.expiredLink("first-to-expire", time.Second)

	f.evaluate()
	if got := f.counts().Notifications; got != 1 {
		t.Fatalf("after the first subject: %d notifications, want 1", got)
	}

	// A second link expires after the first firing.
	f.advance(time.Minute)
	f.expiredLink("second-to-expire", time.Second)

	f.evaluate()
	if got := f.counts().Notifications; got != 2 {
		t.Fatalf("after a second subject: %d notifications, want 2. The watermark is "+
			"behaving as a latch rather than a position — a rule that fires once "+
			"and then never again is not a rule.", got)
	}
}

// TestARuleActsOnlyOnWhatHappensAfterItExists is the arming property.
//
// A rule created this afternoon must not fire for every link that expired last
// year. `last_fired_at` is set at creation for that reason, and this is what
// says so: the link expired before the rule existed, and nothing happens.
func TestARuleActsOnlyOnWhatHappensAfterItExists(t *testing.T) {
	f := newAutomation(t)
	f.expiredLink("expired-long-ago", 30*24*time.Hour)

	f.advance(time.Minute)
	f.rule("tell me what expires", domain.TriggerLinkExpired, 1, domain.ActionNotify)

	f.evaluate()
	if got := f.counts().Notifications; got != 0 {
		t.Fatalf("%d notifications for a link that expired before the rule existed, "+
			"want 0. Creating a rule is supposed to arm it at the current instant, "+
			"not hand it the whole history of the workspace.", got)
	}
}

// TestResumingARuleRearmsIt is the same property for the pause switch.
//
// A rule switched off for a month and switched back on must not fire for the
// month it missed. The re-arm is inside UpdateAutomationRule's statement, on the
// disabled-to-enabled transition only.
func TestResumingARuleRearmsIt(t *testing.T) {
	f := newAutomation(t)
	rule := f.rule("tell me what expires", domain.TriggerLinkExpired, 1, domain.ActionNotify)

	armed, _ := f.watermark(rule.ID)

	// Paused, then a link expires while it is off. The expiry is placed a
	// microsecond after the original arming instant rather than "a bit ago",
	// because what this test needs is a subject strictly between the two
	// watermarks: visible to the rule as it was armed, invisible to the rule as
	// it is re-armed. Anything vaguer would pass whether the re-arm happened or
	// not.
	off := false
	if _, err := f.links.UpdateAutomationRule(t.Context(), f.owner, rule.ID,
		link.UpdateAutomationRuleInput{Enabled: &off}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	f.expiredLinkAt("expired-while-paused", armed.Add(time.Microsecond))

	on := true
	if _, err := f.links.UpdateAutomationRule(t.Context(), f.owner, rule.ID,
		link.UpdateAutomationRuleInput{Enabled: &on}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	resumed, resumedSubject := f.watermark(rule.ID)
	if !resumed.After(armed) {
		t.Fatalf("the watermark did not move on resume: %s then %s", armed, resumed)
	}
	if resumedSubject != nil {
		t.Fatalf("the subject half of the watermark survived the re-arm: %s. The pair "+
			"describes one position, and a stale id beside a fresh instant admits "+
			"subjects at exactly the arming instant whose ids sort above it.",
			resumedSubject)
	}
	f.advance(time.Minute)

	f.evaluate()
	if got := f.counts().Notifications; got != 0 {
		t.Fatalf("%d notifications after resuming, want 0. Switching a rule back on "+
			"is supposed to re-arm it, or every pause becomes a backlog somebody "+
			"gets delivered in one go.", got)
	}
}

// TestATruncatedRunPicksUpTheRemainder is the per-run bound, and the assertion
// that the bound loses nothing.
//
// More subjects than one run may handle. The first run handles exactly the cap,
// the second handles the rest — because the watermark advances to the last
// subject *handled* rather than to the clock. Advancing to `now` would be the
// natural implementation and would silently drop everything after the cap.
func TestATruncatedRunPicksUpTheRemainder(t *testing.T) {
	f := newAutomation(t)
	const extra = 3
	total := domain.AutomationMatchesPerRule + extra

	f.rule("archive the backlog", domain.TriggerLinkExpired, 1, domain.ActionArchiveLink)
	f.advance(time.Minute)

	// Distinct expiries, so "the last one handled" is a well-defined position
	// even for a timestamp alone. The tied case — where it is not — is
	// TestATiedBacklogStraddlingTheCapAllFiresExactlyOnce.
	for i := range total {
		f.expiredLink(fmt.Sprintf("bulk-%02d", i), time.Duration(total-i)*time.Second)
	}

	f.evaluate()
	if got := f.archivedCount(); got != domain.AutomationMatchesPerRule {
		t.Fatalf("first run archived %d, want the cap of %d",
			got, domain.AutomationMatchesPerRule)
	}

	f.advance(time.Minute)
	f.evaluate()
	if got := f.archivedCount(); got != total {
		t.Fatalf("after two runs %d of %d links are archived. The remainder past the "+
			"per-run cap was dropped rather than deferred, which is what advancing "+
			"the watermark to now() instead of to the last subject handled does.",
			got, total)
	}
}

func (f *automationFixture) archivedCount() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT count(*) FROM links WHERE workspace_id = $1 AND status = 'archived'`,
		f.owner.WorkspaceID).Scan(&n); err != nil {
		f.t.Fatalf("count archived links: %v", err)
	}
	return n
}

// TestATiedBacklogStraddlingTheCapAllFiresExactlyOnce is
// TestATruncatedRunPicksUpTheRemainder's sibling, and the sibling exists because
// that test cannot fail for the defect this one holds down: with distinct
// expiries, "the last one handled" is a well-defined instant and a
// timestamp-only watermark resumes correctly. Identical expiries are the broken
// case — bulk-created links share one expires_at, so the per-run cap lands
// *inside* a tie group, and a watermark that records only the instant reopens
// the next window strictly after it. The tied remainder was not deferred; it
// fell out of every future window, silently. The id half of the watermark
// (03600) is what re-enters the tie group, and "exactly once" is asserted from
// both directions: every link ends up archived, and the firings' own records
// sum to the backlog with nothing counted twice.
func TestATiedBacklogStraddlingTheCapAllFiresExactlyOnce(t *testing.T) {
	f := newAutomation(t)
	const extra = 3
	total := domain.AutomationMatchesPerRule + extra

	rule := f.rule("archive the tied backlog", domain.TriggerLinkExpired, 1,
		domain.ActionArchiveLink)
	f.advance(time.Minute)

	// One shared instant for every expiry. The first fetch is therefore a single
	// tie group larger than the cap — the degenerate case included: a batch
	// whose every subject carries the boundary timestamp can only ever advance
	// if the watermark records which subject it stopped at.
	at := f.clock().Add(-time.Second)
	ids := make([]uuid.UUID, 0, total)
	for i := range total {
		ids = append(ids, f.expiredLinkAt(fmt.Sprintf("tied-%02d", i), at))
	}

	f.evaluate()
	if got := f.archivedCount(); got != domain.AutomationMatchesPerRule {
		t.Fatalf("first run archived %d, want the cap of %d",
			got, domain.AutomationMatchesPerRule)
	}

	// The watermark stopped inside the tie group: the instant is the shared
	// expiry, and the subject is the last link the capped fetch reached in
	// (expires_at, id) order — id order alone, here, since the timestamps tie.
	sorted := append([]uuid.UUID(nil), ids...)
	slices.SortFunc(sorted, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	boundary := sorted[domain.AutomationMatchesPerRule-1]
	ts, subject := f.watermark(rule.ID)
	if want := f.expiry(ids[0]); !ts.Equal(want) {
		t.Errorf("watermark instant is %s, want the shared expiry %s", ts, want)
	}
	if subject == nil || *subject != boundary {
		t.Errorf("watermark subject is %v, want %s, the last link inside the cap. "+
			"Without it the next window opens after the shared instant and the tied "+
			"remainder is unreachable.", subject, boundary)
	}

	f.advance(time.Minute)
	f.evaluate()
	if got := f.archivedCount(); got != total {
		t.Fatalf("after two runs %d of %d links are archived. The remainder of a tie "+
			"group split by the per-run cap was dropped rather than deferred — the "+
			"watermark recorded the boundary instant but not the boundary subject, "+
			"so the next window opened past every link still sharing that expiry.",
			got, total)
	}

	// A third run does nothing, and the firing records prove exactly-once from
	// the matching side: the archive is idempotent, so the archived count alone
	// cannot see a subject matched twice — the sum of each firing's own matched
	// figure can.
	f.advance(time.Minute)
	f.evaluate()
	if got := f.matchedTotal(); got != total {
		t.Errorf("the firings matched %d subjects in total, want exactly %d: more is a "+
			"tied subject seen twice across the boundary, fewer is one dropped by it",
			got, total)
	}
}

// matchedTotal sums the `matched` figure over every firing record, which is the
// one place a re-matched subject is visible after idempotent actions.
func (f *automationFixture) matchedTotal() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.t.Context(), `
		SELECT coalesce(sum((metadata->>'matched')::int), 0) FROM audit_logs
		 WHERE action = $1`, audit.ActionAutomationFired).Scan(&n); err != nil {
		f.t.Fatalf("sum matched subjects: %v", err)
	}
	return n
}

// TestAThresholdAccumulatesRatherThanDiscards is the one config key.
//
// Below the threshold the watermark does not move, so the subjects that did not
// reach it are still there next run. A threshold that discarded what it counted
// would be one nobody could reason about — "tell me after five" would mean
// "never" on an instance where they arrive one at a time.
func TestAThresholdAccumulatesRatherThanDiscards(t *testing.T) {
	f := newAutomation(t)
	f.rule("tell me after three", domain.TriggerLinkExpired, 3, domain.ActionNotify)

	f.advance(time.Minute)
	f.expiredLink("threshold-1", time.Second)
	f.evaluate()
	if got := f.counts().Notifications; got != 0 {
		t.Fatalf("%d notifications at one of three, want 0", got)
	}

	f.advance(time.Minute)
	f.expiredLink("threshold-2", time.Second)
	f.evaluate()
	if got := f.counts().Notifications; got != 0 {
		t.Fatalf("%d notifications at two of three, want 0", got)
	}

	f.advance(time.Minute)
	f.expiredLink("threshold-3", time.Second)
	f.evaluate()
	if got := f.counts().Notifications; got != 1 {
		t.Fatalf("%d notifications at three of three, want 1. The first two subjects "+
			"were discarded by the runs that did not fire, so the threshold can "+
			"never be reached one subject at a time.", got)
	}
}

// TestARuleBeyondTheRunCapIsStillEvaluated is the reopening's claim, and F83's
// refutation.
//
// One run considers domain.AutomationRulesPerRun rules and an instance may hold
// more, so *which* hundred is a fairness question rather than a detail. It used
// to be answered by `last_fired_at`, which moves only when a rule **fires** — and
// idle is exactly what keeps a watermark old, so the hundred oldest were a fixed
// set and the hundred-and-first enabled rule was never evaluated on any run,
// ever. The cursor is its own column now, advanced for every rule a run reached.
//
// The shape carries the claim. The rule under test is the last by id, the only
// one with a subject, and the first run is asserted to produce **nothing**:
// without that assertion the test would also pass on a tree where the cap never
// bit, which is the one thing it exists to rule out.
func TestARuleBeyondTheRunCapIsStillEvaluated(t *testing.T) {
	f := newAutomation(t)

	// A hundred rules with nothing to match, in a workspace holding no links.
	fillers, fillersArmed := f.fillerRules(domain.AutomationRulesPerRun)

	// The rule under test, in the fixture's own workspace, where the subject is.
	// Its id comes from CreateAutomationRule and is a v7, so it sorts after every
	// filler id and is exactly one place past the cap.
	rule := f.rule("the hundred and first", domain.TriggerLinkExpired, 1, domain.ActionNotify)
	armed, _ := f.watermark(rule.ID)
	f.advance(time.Minute)
	f.expiredLink("subject-past-the-cap", time.Second)

	f.evaluate()
	if got := f.counts().Notifications; got != 0 {
		t.Fatalf("%d notifications on the first run, want 0. The rule under test was "+
			"reached, so the per-run cap is not biting and every assertion below "+
			"would pass without the thing they are here to check.", got)
	}

	// The fillers matched nothing, and the two columns record different things
	// about that: all of them were *looked at*, and none of them *fired*.
	if got := f.checkedCount(fillers); got != len(fillers) {
		t.Fatalf("%d of %d rules carry a cursor after a run that looked at all of "+
			"them, want all. A rule the scheduler evaluated and did not stamp keeps "+
			"its place at the head of the queue forever.", got, len(fillers))
	}
	if got := f.firedSince(fillers, fillersArmed); got != 0 {
		t.Fatalf("%d rules that matched nothing had their watermark moved, want 0. "+
			"`last_fired_at` is the threshold accumulator: advancing it on a "+
			"no-match run discards the subjects already inside the window, and "+
			"min_count stops working the day that happens.", got)
	}

	// Nothing new has happened between the two runs. The only difference is
	// whose turn it is.
	f.advance(time.Minute)
	f.evaluate()

	if got := f.counts().Notifications; got != 1 {
		t.Fatalf("%d notifications after a second run, want 1. The rule past the cap "+
			"was not reached on that run either, which is F83: the hundred rules "+
			"ahead of it hold their places permanently, because a rule that matches "+
			"nothing never moves the column the queue is ordered by.", got)
	}
	if got, _ := f.watermark(rule.ID); !got.After(armed) {
		t.Errorf("the rule fired and its watermark is still %s", got)
	}
}

// fillerRules writes n enabled rules with nothing to match, in a workspace of
// their own, and returns their ids and the instant they were armed at.
//
// Written with statements rather than through the service, for two reasons that
// both matter. The service caps a workspace at MaxAutomationRulesPerWorkspace
// and the bound under test is the instance-wide one; and the ids are *chosen*,
// because the due query breaks ties on id and the claim being tested is about
// which rules sit at the front of the queue. A generated id would leave that to
// chance.
func (f *automationFixture) fillerRules(n int) ([]uuid.UUID, time.Time) {
	f.t.Helper()

	var ws uuid.UUID
	if err := f.pool.QueryRow(f.t.Context(), `
		INSERT INTO workspaces (id, organization_id, name, slug)
		VALUES ($1, $2, 'Filler', 'filler') RETURNING id`,
		uuid.Must(uuid.NewV7()), f.owner.OrgID).Scan(&ws); err != nil {
		f.t.Fatalf("create the filler workspace: %v", err)
	}

	armed := f.clock()
	rows, err := f.pool.Query(f.t.Context(), `
		INSERT INTO automation_rules
		       (id, workspace_id, name, trigger, trigger_config, actions, enabled, last_fired_at)
		SELECT ('00000000-0000-7000-8000-' || lpad(i::text, 12, '0'))::uuid, $1,
		       'filler ' || i, $2, '{"min_count":1}'::jsonb, '["notify"]'::jsonb, true, $3
		  FROM generate_series(1, $4) AS i
		RETURNING id`, ws, domain.TriggerLinkExpired, armed, n)
	if err != nil {
		f.t.Fatalf("write the filler rules: %v", err)
	}
	defer rows.Close()

	ids := make([]uuid.UUID, 0, n)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			f.t.Fatalf("read a filler rule id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("read the filler rule ids: %v", err)
	}
	if len(ids) != n {
		f.t.Fatalf("wrote %d filler rules, want %d", len(ids), n)
	}
	return ids, armed
}

// checkedCount is how many of these rules the scheduler has stamped as looked at.
func (f *automationFixture) checkedCount(ids []uuid.UUID) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.t.Context(), `
		SELECT count(*) FROM automation_rules
		 WHERE id = ANY($1::uuid[]) AND last_checked_at IS NOT NULL`, ids).Scan(&n); err != nil {
		f.t.Fatalf("count checked rules: %v", err)
	}
	return n
}

// firedSince is how many of these rules moved their watermark past `at`.
//
// Strictly after, rather than "not equal to the value written", so the assertion
// does not turn on how a timestamp round-trips through the driver.
func (f *automationFixture) firedSince(ids []uuid.UUID, at time.Time) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.t.Context(), `
		SELECT count(*) FROM automation_rules
		 WHERE id = ANY($1::uuid[]) AND last_fired_at > $2`, ids, at).Scan(&n); err != nil {
		f.t.Fatalf("count fired rules: %v", err)
	}
	return n
}

// TestTheMaxClicksTriggerFires covers M35's half of the vocabulary.
//
// The subject is `link_click_budget.exhausted_at`, which the gate stamps in the
// same transaction that spends the last click — not `links.click_count`, which
// is the approximate counter the analytics pipeline writes afterwards.
func TestTheMaxClicksTriggerFires(t *testing.T) {
	f := newAutomation(t)
	linkID := f.expiredLink("budget-spent", time.Hour)

	f.advance(time.Minute)
	f.rule("archive what ran out", domain.TriggerLinkMaxClicks, 1,
		domain.ActionNotify, domain.ActionArchiveLink)

	// The budget row as the gate writes it when the last click is spent.
	if _, err := f.pool.Exec(t.Context(), `
		INSERT INTO link_click_budget (link_id, workspace_id, consumed, exhausted_at)
		VALUES ($1, $2, 5, $3)`, linkID, f.owner.WorkspaceID, f.clock()); err != nil {
		t.Fatalf("exhaust the budget: %v", err)
	}
	f.advance(time.Minute)

	f.evaluate()
	if got := f.counts().Notifications; got != 1 {
		t.Fatalf("%d notifications, want 1", got)
	}
	if got := f.linkStatus(linkID); got != string(domain.StatusArchived) {
		t.Fatalf("link status is %q, want archived", got)
	}

	f.advance(time.Minute)
	f.evaluate()
	if got := f.counts().Notifications; got != 1 {
		t.Errorf("%d notifications after a second run, want 1", got)
	}
}

// TestTheBlockedTriggerFires covers M30's half.
//
// The subject is the audit record, because that is the only durable trace a
// refusal leaves. The label travels **defanged**, exactly as the audit record
// stores it: a notification or a webhook payload piped into a chat room must not
// hand somebody a live link to the thing that was refused.
func TestTheBlockedTriggerFires(t *testing.T) {
	f := newAutomation(t)
	f.rule("tell me about refusals", domain.TriggerDestinationBlocked, 1, domain.ActionNotify)
	f.advance(time.Minute)

	// A destination the unappealable tier refuses, through the real service, so
	// the audit record is the one the product writes rather than one this test
	// invented.
	if _, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
		URL: "http://169.254.169.254/latest/meta-data/", Alias: "metadata",
	}); err == nil {
		t.Fatal("the metadata address was accepted; this test needs it refused")
	}
	f.advance(time.Minute)

	f.evaluate()
	if got := f.counts().Notifications; got != 1 {
		t.Fatalf("%d notifications, want 1", got)
	}

	var body string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT body FROM notifications WHERE kind = $1`, notify.KindAutomationFired).
		Scan(&body); err != nil {
		t.Fatalf("read the notification: %v", err)
	}
	// The defanged spelling internal/link.Defang produces: the scheme's colon
	// and every dot bracketed, so nothing in the sentence is a clickable URL.
	if !strings.Contains(body, "[:]") || !strings.Contains(body, "[.]") {
		t.Errorf("the notification body is %q and does not carry a defanged URL. "+
			"A refusal reported with a live link is a link somebody clicks.", body)
	}
	if strings.Contains(body, "http://169.254.169.254") {
		t.Errorf("the notification body carries the live URL: %q", body)
	}

	f.advance(time.Minute)
	f.evaluate()
	if got := f.counts().Notifications; got != 1 {
		t.Errorf("%d notifications after a second run, want 1", got)
	}
}

// TestNothingButTheSchedulerFiresARule is the first hard claim, from the inside.
//
// The import-graph test in internal/automation says no request-path package can
// reach the evaluator. This says the same thing behaviourally: a workspace full
// of matching subjects and an enabled rule, and then every kind of write a
// request makes — creating, editing, archiving, restoring, listing — and nothing
// fires until the scheduler's own entry point is called.
func TestNothingButTheSchedulerFiresARule(t *testing.T) {
	f := newAutomation(t)
	f.subscribe()
	f.rule("tell me what expires", domain.TriggerLinkExpired, 1, domain.ActionNotify)
	f.advance(time.Minute)

	linkID := f.expiredLink("subject-waiting", time.Second)
	f.advance(time.Minute)

	// Everything a request does.
	created, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
		URL: "https://example.com/other", Alias: "other",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	title := "renamed"
	if _, err := f.links.Update(t.Context(), f.owner, created.ID,
		link.UpdateInput{Title: &title}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := f.links.Archive(t.Context(), f.owner, created.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := f.links.Restore(t.Context(), f.owner, created.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := f.links.AutomationRules(t.Context(), f.owner); err != nil {
		t.Fatalf("list rules: %v", err)
	}

	if got := f.counts(); got != (firingCounts{}) {
		t.Fatalf("%+v after link writes and a rule listing, want nothing. Evaluation "+
			"is supposed to happen on the leader-elected scheduler and nowhere "+
			"else; something on the request path has started firing rules.", got)
	}

	f.evaluate()
	if got := f.counts().Notifications; got != 1 {
		t.Fatalf("%d notifications after the scheduler ran, want 1 — the subject at "+
			"%s was there the whole time", got, linkID)
	}
}

// TestRuleChangesAreAuditEvents is m43.md's last bullet.
func TestRuleChangesAreAuditEvents(t *testing.T) {
	f := newAutomation(t)
	rule := f.rule("audited", domain.TriggerLinkExpired, 1, domain.ActionNotify)

	name := "audited, renamed"
	if _, err := f.links.UpdateAutomationRule(t.Context(), f.owner, rule.ID,
		link.UpdateAutomationRuleInput{Name: &name}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := f.links.DeleteAutomationRule(t.Context(), f.owner, rule.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	for _, action := range []string{
		audit.ActionAutomationRuleCreated,
		audit.ActionAutomationRuleUpdated,
		audit.ActionAutomationRuleDeleted,
	} {
		var n int
		if err := f.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM audit_logs WHERE action = $1 AND target_id = $2`,
			action, rule.ID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", action, err)
		}
		if n != 1 {
			t.Errorf("%d audit records for %s, want 1. A standing instruction that "+
				"can archive links and leaves no trace of who wrote it is one "+
				"nobody can answer for.", n, action)
		}
	}
}

// TestAFiringNamesTheRuleAndNotAPerson checks the one audit record in this log
// with no human in it.
func TestAFiringNamesTheRuleAndNotAPerson(t *testing.T) {
	f := newAutomation(t)
	rule := f.rule("the night shift", domain.TriggerLinkExpired, 1, domain.ActionNotify)
	f.advance(time.Minute)
	f.expiredLink("audited-firing", time.Second)

	f.evaluate()

	var label string
	var actorID *uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `
		SELECT actor_label, actor_user_id FROM audit_logs
		 WHERE action = $1 AND target_id = $2`,
		audit.ActionAutomationFired, rule.ID).Scan(&label, &actorID); err != nil {
		t.Fatalf("read the firing record: %v", err)
	}
	if want := "automation:the night shift"; label != want {
		t.Errorf("actor label is %q, want %q", label, want)
	}
	if actorID != nil {
		t.Errorf("the firing record names user %s. Nobody took this action — a "+
			"scheduler run attributed to a person is an audit log that lies about "+
			"who did what.", actorID)
	}
}

// TestARuleCannotReachAnotherWorkspace is tenancy, on a surface that acts
// unattended.
//
// The rule's own workspace has a matching link and the other workspace has one
// too, both expired inside the same window. The rule must archive exactly one of
// them — and asserting that it archived *its own* is what stops this passing on
// a rule that archived nothing at all, which is what a tenancy test written only
// as "the other one is untouched" would do.
//
// **It asserts the match set as well as the effect**, and that is not belt and
// braces: two statements are workspace-scoped here, the match query and the
// archive, and each covers the other. A test that read only the link statuses
// passes with either one removed, which is a test that cannot see a tenancy
// guard disappear. The firing's own record says how many subjects it matched, so
// the match query's scope is asserted where it is actually made.
func TestARuleCannotReachAnotherWorkspace(t *testing.T) {
	f := newAutomation(t)
	f.rule("mine", domain.TriggerLinkExpired, 1, domain.ActionArchiveLink)
	f.advance(time.Minute)

	mine := f.expiredLink("mine-expired", time.Second)

	// A second workspace in the same organization, with an expired link in it.
	// Written with statements because the service acts as the identity it is
	// given, and this test needs a link the rule's identity could not have made.
	var otherWS uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `
		INSERT INTO workspaces (id, organization_id, name, slug)
		VALUES ($1, $2, 'Other', 'other') RETURNING id`,
		uuid.Must(uuid.NewV7()), f.owner.OrgID).Scan(&otherWS); err != nil {
		t.Fatalf("create the other workspace: %v", err)
	}
	var otherLink uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `
		INSERT INTO links (id, workspace_id, domain_id, alias, primary_url, expires_at)
		SELECT $1, $2, l.domain_id, 'elsewhere', 'https://example.com/elsewhere', $3
		  FROM links l WHERE l.id = $4
		RETURNING id`,
		uuid.Must(uuid.NewV7()), otherWS, f.clock().Add(-time.Second), mine).
		Scan(&otherLink); err != nil {
		t.Fatalf("create the other workspace's link: %v", err)
	}
	f.advance(time.Second)

	f.evaluate()
	if got := f.linkStatus(mine); got != string(domain.StatusArchived) {
		t.Fatalf("the rule's own link is %q, want archived. Without this the "+
			"assertion below passes on a rule that archived nothing.", got)
	}
	if got := f.linkStatus(otherLink); got == string(domain.StatusArchived) {
		t.Fatal("a rule in one workspace archived another workspace's link")
	}

	var matched int
	if err := f.pool.QueryRow(t.Context(), `
		SELECT (metadata->>'matched')::int FROM audit_logs
		 WHERE action = $1`, audit.ActionAutomationFired).Scan(&matched); err != nil {
		t.Fatalf("read the firing record: %v", err)
	}
	if matched != 1 {
		t.Fatalf("the firing matched %d subjects, want 1. The match query saw a link "+
			"outside the rule's workspace; the archive happened to refuse it, but "+
			"a trigger that reads another tenant's rows is already a leak — the "+
			"notification and the webhook payload carry the subject labels.", matched)
	}
}
