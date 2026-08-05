//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/dispute"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/instance"
	"github.com/DevOfPie/LinkCtrl/internal/invite"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
)

// Blocked-attempt disputes and owner review (M31), against a real database.
//
// The unit tests in internal/dispute hold the tier gate and the two structural
// claims — nothing fetches, nothing holds a client. internal/ui holds the
// rendering. What can only be asserted here is the part that spans tables: that
// an allow actually removes the row that was refusing the destination, that the
// destination becomes creatable afterwards, that both decisions reach the audit
// log, and that the two notifications land in the right inboxes.

const disputePassword = "a-sufficiently-long-password"

type disputeFixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	auth     *auth.Service
	links    *link.Service
	invites  *invite.Service
	notify   *notify.Service
	disputes *dispute.Service
	owner    *auth.Identity
	ctx      context.Context
	// sender is non-nil only on a fixture built with a mailer, which is the
	// difference D1's addendum turns on: in-app delivery is the baseline and
	// the email is the addition, so most of this file runs without one.
	sender *recordingSender
}

func newDispute(t *testing.T) *disputeFixture { return newDisputeWith(t, false) }

// newDisputeWithMail is newDispute on an instance that has an SMTP relay.
func newDisputeWithMail(t *testing.T) *disputeFixture { return newDisputeWith(t, true) }

func newDisputeWith(t *testing.T, withMailer bool) *disputeFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	// IsFirstUser, because since D98 that is what makes an account the instance
	// principal: the queue's permissions are held instance-wide by named people
	// and by no organization role, so an owner registered any other way holds
	// nothing here — which is F15 being closed rather than a fixture detail.
	owner, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner", Password: disputePassword,
		IsFirstUser: true,
	})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}

	auditSvc := audit.NewService(pool)
	notifySvc := notify.NewService(pool)
	var sender *recordingSender
	if withMailer {
		sender = &recordingSender{}
		notifySvc = notifySvc.WithMail(newMailService(t, pool, sender), "https://links.example.com")
	}
	links := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: "http://lnk.test",
		SplitHosts: true, Audit: auditSvc,
	})
	inviteSvc, err := invite.NewService(pool, invite.Config{
		AppURL: "https://app.example", TTL: 168 * time.Hour, NewAccounts: true,
		Hasher: authSvc.Hasher(), Audit: auditSvc, Notify: notifySvc,
	})
	if err != nil {
		t.Fatal(err)
	}
	disputes, err := dispute.NewService(pool, dispute.Config{
		Judge: links, Audit: auditSvc, Notify: notifySvc,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A client address, so a decision's audit record can be checked for the
	// prefix the privacy stance requires and the address it must never hold.
	ctx := auth.WithClientIP(t.Context(), netip.MustParseAddr("198.51.100.9"))

	f := &disputeFixture{
		t: t, pool: pool, auth: authSvc, links: links, invites: inviteSvc,
		notify: notifySvc, disputes: disputes, owner: owner, ctx: ctx,
		sender: sender,
	}
	// Re-read, so the owner carries the instance grants conferred when it
	// claimed the instance rather than whatever Register happened to compute.
	f.owner = f.identity("owner@example.com")
	return f
}

func (f *disputeFixture) identity(email string) *auth.Identity {
	f.t.Helper()
	id, err := f.auth.IdentityForEmail(f.ctx, email)
	if err != nil {
		f.t.Fatalf("identity for %s: %v", email, err)
	}
	return id
}

// register makes an ordinary self-serve account, which provisions an
// organization and makes them its owner.
//
// That population is the whole of F137's multiplier: before M45 a filing wrote
// one notification per organization owner on the instance, so every one of these
// accounts made a stranger's next dispute cost one more inbox row.
func (f *disputeFixture) register(email string) *auth.Identity {
	f.t.Helper()
	id, err := f.auth.Register(f.ctx, auth.RegisterInput{
		Email: email, Name: email, Password: disputePassword,
	})
	if err != nil {
		f.t.Fatalf("register %s: %v", email, err)
	}
	if id.Role != "owner" {
		f.t.Fatalf("%s registered as %q; this fixture asserts nothing about the "+
			"owner fan-out unless they are an owner", email, id.Role)
	}
	return id
}

// editor is somebody who can create links and review nothing.
func (f *disputeFixture) editor(email string) *auth.Identity {
	f.t.Helper()
	created, err := f.invites.Create(f.ctx, f.owner, invite.CreateInput{Email: email, Role: "editor"})
	if err != nil {
		f.t.Fatalf("invite %s: %v", email, err)
	}
	const prefix = "https://app.example/invite/"
	if _, err := f.invites.Redeem(f.ctx, invite.RedeemInput{
		Token: created.URL[len(prefix):], Email: email, Password: disputePassword,
	}); err != nil {
		f.t.Fatalf("redeem for %s: %v", email, err)
	}
	return f.identity(email)
}

func (f *disputeFixture) blockHost(host, source string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO blocked_destinations (host, source, reason) VALUES ($1, $2, 'test')`,
		host, source); err != nil {
		f.t.Fatalf("block %q: %v", host, err)
	}
}

func (f *disputeFixture) blockedHosts() map[string]string {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT host, source FROM blocked_destinations`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var host, source string
		if err := rows.Scan(&host, &source); err != nil {
			f.t.Fatal(err)
		}
		out[host] = source
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// filedNotifications counts every dispute.filed row on the instance, whoever it
// belongs to. It is the fan-out itself rather than one inbox's share of it.
func (f *disputeFixture) filedNotifications() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM notifications WHERE kind = $1`, dispute.KindFiled).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *disputeFixture) openDisputeCount() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM destination_disputes WHERE status = 'open'`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

type disputeAuditEvent struct {
	Action   string
	IPPrefix *string
	Meta     map[string]any
}

func (f *disputeFixture) decisionEvents() []disputeAuditEvent {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `
		SELECT action, ip_prefix, metadata FROM audit_logs
		 WHERE action = ANY($1) ORDER BY occurred_at, id`,
		[]string{dispute.ActionDisputeAllowed, dispute.ActionDisputeUpheld})
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()

	var out []disputeAuditEvent
	for rows.Next() {
		var e disputeAuditEvent
		var raw []byte
		if err := rows.Scan(&e.Action, &e.IPPrefix, &raw); err != nil {
			f.t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &e.Meta); err != nil {
			f.t.Fatalf("audit metadata is not JSON: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// inbox reads one user's notifications of a kind.
func (f *disputeFixture) inbox(userID uuid.UUID, kind string) []notify.Notification {
	f.t.Helper()
	page, err := f.notify.List(f.ctx, &auth.Identity{UserID: userID}, notify.Filter{Limit: 50})
	if err != nil {
		f.t.Fatalf("list notifications: %v", err)
	}
	var out []notify.Notification
	for _, n := range page.Items {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

func disputeCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got none")
	}
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) || len(ve) == 0 {
		t.Fatalf("error %T is not a field-level refusal: %v", err, err)
	}
	return ve[0].Code
}

// TestOnlyTheAppealableTierReachesTheQueue is the milestone's first bullet
// against a live evaluator rather than a stub.
//
// The stubbed version in internal/dispute proves the gate rejects a verdict it
// is handed. This proves the verdicts themselves: that a metadata address, a
// forbidden scheme and an embedded-list host really do come back from the tier
// evaluator as tiers with no dispute path, and that the two runtime-list rules
// really do come back as one that has.
func TestOnlyTheAppealableTierReachesTheQueue(t *testing.T) {
	cases := []struct {
		name, url, want string
	}{
		{"cloud metadata address", "http://169.254.169.254/latest/meta-data/", dispute.CodeNotDisputable},
		{"private address", "http://10.0.0.5/admin", dispute.CodeNotDisputable},
		{"forbidden scheme", "ftp://files.example/x", dispute.CodeNotDisputable},
		{"embedded list", "https://metadata.google.internal/computeMetadata/", dispute.CodeNotDisputable},
		{"nothing refuses it", "https://good.example/", dispute.CodeNotBlocked},
		{"operator blocklist", "https://blocked.example/x", ""},
		{"seeded shortener", "https://bit.ly/abc", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDispute(t)
			f.blockHost("blocked.example", link.SourceReview)

			d, err := f.disputes.File(f.ctx, f.owner, tc.url)
			if tc.want != "" {
				if got := disputeCode(t, err); got != tc.want {
					t.Fatalf("File refused with %q, want %q", got, tc.want)
				}
				var n int
				if err := f.pool.QueryRow(f.ctx,
					`SELECT count(*) FROM destination_disputes`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Errorf("a refusal that has no appeal path wrote %d dispute rows; the "+
						"queue must never hold one", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("File: %v", err)
			}
			if d.ReasonCode[:len("low_confidence")] != "low_confidence" {
				t.Errorf("filed dispute carries %q; only the low-confidence tier can reach "+
					"the queue", d.ReasonCode)
			}
		})
	}
}

// TestTheStoredDestinationIsInert covers the column rather than the rendering.
//
// Defanging happens once, on the way in, exactly as M30 does it for the audit
// metadata. That is what makes the property survive a consumer written later:
// the value in the row cannot be rendered live, so nothing downstream has to
// remember to escape it.
func TestTheStoredDestinationIsInert(t *testing.T) {
	f := newDispute(t)
	f.blockHost("evil.example", link.SourceReview)

	const attempted = `https://evil.example/promo?next=https://bank.example&x=<script>`
	d, err := f.disputes.File(f.ctx, f.owner, attempted)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	var storedURL, storedHost string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT url_defanged, host FROM destination_disputes WHERE id = $1`,
		d.ID).Scan(&storedURL, &storedHost); err != nil {
		t.Fatal(err)
	}

	if storedURL == attempted {
		t.Fatal("the attempted URL is stored verbatim; every consumer of this row would " +
			"then have to remember to defang it, and one of them will not")
	}
	for _, live := range []string{"://", "evil.example", "<script>"} {
		if contains(storedURL, live) {
			t.Errorf("url_defanged still contains %q: %s", live, storedURL)
		}
	}
	// The embedded second URL matters as much as the first. An open-redirect
	// payload carries one in its query string, and neutralizing only the leading
	// scheme would leave a followable link inside the record of a refusal.
	if contains(storedURL, "bank.example") {
		t.Errorf("url_defanged leaves the embedded destination live: %s", storedURL)
	}

	// The host is stored plainly, because it is the key a decision acts on — and
	// handed out defanged, because every reader gets the inert form.
	if storedHost != "evil.example" {
		t.Errorf("host = %q, want the plain host the blocklist matches on", storedHost)
	}
	if d.Host != "evil[.]example" {
		t.Errorf("Host = %q, want the defanged form", d.Host)
	}
}

// TestAllowingLiftsTheEntryAndOpensTheDestination is the milestone's fourth
// bullet end to end: the decision acts on the runtime list, is an audit event,
// and notifies the person who asked.
func TestAllowingLiftsTheEntryAndOpensTheDestination(t *testing.T) {
	f := newDispute(t)
	editor := f.editor("editor@example.com")
	f.blockHost("evil.example", link.SourceReview)

	// The refusal that starts it, from the surface a person actually uses.
	if _, err := f.links.Create(f.ctx, editor, link.CreateInput{
		URL: "https://login.evil.example/x",
	}); err == nil {
		t.Fatal("the blocked destination was accepted")
	}

	d, err := f.disputes.File(f.ctx, editor, "https://login.evil.example/x")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !d.Liftable {
		t.Fatal("an operator-blocklist refusal reports itself unliftable")
	}

	// The owner hears that something is waiting.
	if filed := f.inbox(f.owner.UserID, dispute.KindFiled); len(filed) != 1 {
		t.Fatalf("the owner has %d %s notifications, want 1", len(filed), dispute.KindFiled)
	} else if contains(filed[0].Body, "://") {
		t.Errorf("the notification body carries a live URL: %s", filed[0].Body)
	}

	decided, err := f.disputes.Allow(f.ctx, f.owner, d.ID)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if decided.Status != dispute.StatusAllowed {
		t.Errorf("status = %q, want %q", decided.Status, dispute.StatusAllowed)
	}

	// The row that refused it is gone — the one it matched, not the one that was
	// typed. "login.evil.example" was refused by the "evil.example" entry.
	if _, still := f.blockedHosts()["evil.example"]; still {
		t.Error("allowing the dispute left the blocklist entry in place")
	}

	// And the destination is now creatable, which is the only proof that the
	// decision reached the thing doing the refusing.
	if _, err := f.links.Create(f.ctx, editor, link.CreateInput{
		URL: "https://login.evil.example/x",
	}); err != nil {
		t.Fatalf("the destination is still refused after being allowed: %v", err)
	}

	// The decision is an audit event, carrying what the dispute row does not:
	// which blocklist entry disappeared.
	events := f.decisionEvents()
	if len(events) != 1 {
		t.Fatalf("got %d decision events, want 1", len(events))
	}
	e := events[0]
	if e.Action != dispute.ActionDisputeAllowed {
		t.Errorf("action = %q, want %q", e.Action, dispute.ActionDisputeAllowed)
	}
	if got := e.Meta["blocklist_entry_removed"]; got != "evil[.]example" {
		t.Errorf("blocklist_entry_removed = %v, want the defanged entry that was lifted", got)
	}
	if e.IPPrefix == nil || *e.IPPrefix != "198.51.100.0/24" {
		t.Errorf("ip_prefix = %v, want the /24 the privacy stance requires", e.IPPrefix)
	}
	for key, v := range e.Meta {
		if s, ok := v.(string); ok && contains(s, "://") {
			t.Errorf("audit metadata %q carries a live URL: %s", key, s)
		}
	}

	// And the person who asked is told.
	told := f.inbox(editor.UserID, dispute.KindDecided)
	if len(told) != 1 {
		t.Fatalf("the filer has %d %s notifications, want 1", len(told), dispute.KindDecided)
	}
	if got := told[0].Data["status"]; got != dispute.StatusAllowed {
		t.Errorf("notification says status %v, want %q", got, dispute.StatusAllowed)
	}
}

// TestTheEntryLiftedIsTheOneRecordedAtFiling is F33's decision-integrity half.
//
// The runtime list answers with the *longest* matching entry, so which row
// refuses a destination is a function of what the list holds at the moment the
// question is asked. Until M45 that question was asked twice — once when the
// dispute was filed and again when somebody clicked Allow — and only the first
// answer was ever shown to anybody.
//
// This is the gap between the two. A more specific entry lands while the dispute
// sits in the queue; the owner reads a dispute that says one entry and clicks a
// button that used to delete the other. Nothing in the old queue could have told
// them, because the dispute row carried no field naming what would go.
func TestTheEntryLiftedIsTheOneRecordedAtFiling(t *testing.T) {
	f := newDispute(t)
	editor := f.editor("editor@example.com")
	f.blockHost("evil.example", link.SourceReview)

	d, err := f.disputes.File(f.ctx, editor, "https://login.evil.example/x")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	// What the filer typed, and what actually refused them. Different values, and
	// both on the row, because the queue renders the second on the Allow control.
	if d.Host != "login[.]evil[.]example" {
		t.Errorf("host_defanged = %q, want the host that was typed", d.Host)
	}
	if d.BlockedHost != "evil[.]example" {
		t.Fatalf("blocked_host_defanged = %q, want the entry that refused it. Without "+
			"it the queue shows one host while Allow acts on another.", d.BlockedHost)
	}

	// The list moves while the dispute waits. A longer entry now matches the same
	// destination, and a decision re-derived here would delete this one instead.
	f.blockHost("login.evil.example", link.SourceReview)

	if _, err := f.disputes.Allow(f.ctx, f.owner, d.ID); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	blocked := f.blockedHosts()
	if _, still := blocked["evil.example"]; still {
		t.Error("the entry the dispute recorded is still listed; the decision acted " +
			"on something else")
	}
	if _, gone := blocked["login.evil.example"]; !gone {
		t.Error("allowing deleted an entry the dispute never named. The owner was " +
			"shown one blocklist entry and a different one was removed — which is " +
			"the whole defect, whichever of the two happens to be broader.")
	}
	// And the audit record names the entry that actually went, so the log agrees
	// with what the queue said.
	events := f.decisionEvents()
	if len(events) != 1 {
		t.Fatalf("got %d decision events, want 1", len(events))
	}
	if got := events[0].Meta["blocklist_entry_removed"]; got != "evil[.]example" {
		t.Errorf("blocklist_entry_removed = %v, want the entry the dispute recorded", got)
	}
}

// TestOneOpenDisputePerBlocklistEntry is F33's flood half, and the claim
// 01600's index comment used to make and could not keep.
//
// One blocked row is one decision. Keyed on the typed host, that row admitted a
// fresh open dispute — and, at the time, a notification to every owner on the
// instance — for every distinct subdomain somebody could type, which is
// unbounded. Keyed on the entry, the second appeal is the same appeal. The
// recipient half is F137 and is asserted by
// TestAFilingReachesTheReviewersAndNobodyElse.
func TestOneOpenDisputePerBlocklistEntry(t *testing.T) {
	f := newDispute(t)
	editor := f.editor("editor@example.com")
	f.blockHost("evil.example", link.SourceReview)

	first, err := f.disputes.File(f.ctx, editor, "https://login.evil.example/x")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if _, err := f.disputes.File(f.ctx, editor, "https://mail.evil.example/y"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("a second open dispute about the same blocklist entry returned %v, "+
			"want a conflict. Every subdomain of a listed host is the same decision "+
			"and the same notification to the same reviewers.", err)
	}
	if n := f.openDisputeCount(); n != 1 {
		t.Errorf("%d open disputes, want 1", n)
	}

	// A decided dispute frees the entry, exactly as 01600's partial predicate
	// intends: an entry upheld today and argued about again next month is a new
	// question.
	if _, err := f.disputes.Uphold(f.ctx, f.owner, first.ID); err != nil {
		t.Fatalf("Uphold: %v", err)
	}
	if _, err := f.disputes.File(f.ctx, editor, "https://mail.evil.example/y"); err != nil {
		t.Errorf("an entry whose dispute was decided cannot be disputed again: %v", err)
	}

	// A rule with no row behind it is bounded by the host and by nothing else,
	// and that is a real limit rather than a claim: every one of them stores the
	// same empty entry, so a shared bound would make one open homograph dispute
	// lock out every other destination on the instance.
	if _, err := f.disputes.File(f.ctx, editor, "https://a@one.example/x"); err != nil {
		t.Fatalf("File a credentials refusal: %v", err)
	}
	if _, err := f.disputes.File(f.ctx, editor, "https://a@two.example/x"); err != nil {
		t.Errorf("a second computed-rule dispute about a different host returned %v; "+
			"they carry no blocklist entry and must not share one bound", err)
	}
}

// TestAFilingReachesTheReviewersAndNobodyElse is F137.
//
// F33 keyed the one-open-dispute bound on the blocklist entry that refused the
// destination, which makes every subdomain of a listed host one decision. A
// refusal computed from the URL has no entry to key on — `url_credentials` fires
// on any host carrying userinfo — so those stay bounded by the string that was
// typed, which is not a bound. That half was left open deliberately: sharing a
// key across them would let one open homograph dispute lock out every unrelated
// destination on the instance, and TestOneOpenDisputePerBlocklistEntry asserts
// it still does not.
//
// So what was left to bound was never the number of filings. It was what each
// one costs, and until M45 each one cost **one inbox row per organization owner
// on the instance** — a number a stranger grows by registering, and which the
// filer neither controls nor is charged for. Rate-limiting the route or capping
// the filer leaves that multiplier exactly where it is.
//
// D98 replaced the guess with an answer: the people who can act on a dispute are
// the holders of destinations.review, and this asserts a filing reaches them and
// stops there. The two strangers below are owners of their own organizations,
// which is precisely the population the old fan-out swept up.
func TestAFilingReachesTheReviewersAndNobodyElse(t *testing.T) {
	f := newDispute(t)
	editor := f.editor("editor@example.com")
	// Two more organizations, each with an owner who has nothing to do with the
	// instance's dispute queue.
	first := f.register("first-org@example.com")
	second := f.register("second-org@example.com")

	// The F137 shape: a rule computed from the URL, so no blocklist entry exists
	// and no index bounds how many of these one filer may open.
	if _, err := f.disputes.File(f.ctx, editor, "https://a@one.example/x"); err != nil {
		t.Fatalf("File: %v", err)
	}

	// One filing, one notification. Three organization owners exist.
	if n := f.filedNotifications(); n != 1 {
		t.Errorf("one filing wrote %d %s notifications on an instance with three "+
			"organization owners, want 1. The recipient set is the multiplier: a "+
			"filer who cannot be bounded must not be amplified by how many people "+
			"have signed up", n, dispute.KindFiled)
	}
	if got := len(f.inbox(f.owner.UserID, dispute.KindFiled)); got != 1 {
		t.Errorf("the reviewer has %d %s notifications, want 1", got, dispute.KindFiled)
	}
	for _, stranger := range []*auth.Identity{first, second} {
		if got := len(f.inbox(stranger.UserID, dispute.KindFiled)); got != 0 {
			t.Errorf("%s owns an organization and reviews nothing, but has %d %s "+
				"notifications", stranger.Email, got, dispute.KindFiled)
		}
	}

	// And the set is the grant table read live, not a shape fixed at boot:
	// appointing a reviewer adds them to it, and only them.
	svc := instance.NewService(f.pool, instance.Config{})
	if _, err := svc.GrantReviewer(f.ctx, f.owner, first.Email); err != nil {
		t.Fatalf("GrantReviewer: %v", err)
	}
	if _, err := f.disputes.File(f.ctx, editor, "https://a@two.example/x"); err != nil {
		t.Fatalf("File a second computed-rule dispute: %v", err)
	}
	if got := len(f.inbox(first.UserID, dispute.KindFiled)); got != 1 {
		t.Errorf("a newly appointed reviewer has %d %s notifications from the "+
			"filing that followed their appointment, want 1", got, dispute.KindFiled)
	}
	if got := len(f.inbox(second.UserID, dispute.KindFiled)); got != 0 {
		t.Errorf("an owner who was not appointed has %d %s notifications, want 0",
			got, dispute.KindFiled)
	}
	if n := f.filedNotifications(); n != 3 {
		t.Errorf("%d %s notifications after two filings and two reviewers, want 3 "+
			"(one reviewer for the first, two for the second)", n, dispute.KindFiled)
	}
}

// TestUpholdingChangesNoListAndIsStillRecorded.
func TestUpholdingChangesNoListAndIsStillRecorded(t *testing.T) {
	f := newDispute(t)
	editor := f.editor("editor@example.com")
	f.blockHost("evil.example", link.SourceReview)

	d, err := f.disputes.File(f.ctx, editor, "https://evil.example/x")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	before := f.blockedHosts()

	if _, err := f.disputes.Uphold(f.ctx, f.owner, d.ID); err != nil {
		t.Fatalf("Uphold: %v", err)
	}

	if after := f.blockedHosts(); len(after) != len(before) {
		t.Errorf("upholding changed the blocklist: %d rows before, %d after",
			len(before), len(after))
	}
	if _, still := f.blockedHosts()["evil.example"]; !still {
		t.Error("upholding removed the entry it was meant to leave standing")
	}
	events := f.decisionEvents()
	if len(events) != 1 || events[0].Action != dispute.ActionDisputeUpheld {
		t.Fatalf("got %v, want one %s event", events, dispute.ActionDisputeUpheld)
	}
	if told := f.inbox(editor.UserID, dispute.KindDecided); len(told) != 1 {
		t.Errorf("the filer has %d decision notifications, want 1", len(told))
	}

	// A decided dispute is decided. The second caller loses, rather than
	// overwriting a decision somebody already acted on.
	if _, err := f.disputes.Allow(f.ctx, f.owner, d.ID); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("deciding twice returned %v, want a conflict", err)
	}
}

// TestAllowRefusesWhatItCannotActuallyLift.
//
// Two refusals, both of which would otherwise be decisions that quietly do
// nothing: a rule computed from the URL has no row to delete, and an entry the
// environment owns comes back at the next restart.
func TestAllowRefusesWhatItCannotActuallyLift(t *testing.T) {
	t.Run("a computed rule has no entry", func(t *testing.T) {
		f := newDispute(t)
		// Credentials before the host: a heuristic, evaluated every time, backed
		// by no row anywhere.
		d, err := f.disputes.File(f.ctx, f.owner, "https://paypal.com@evil.example/signin")
		if err != nil {
			t.Fatalf("File: %v", err)
		}
		if d.Liftable {
			t.Error("a heuristic refusal reports itself liftable; the queue would draw a " +
				"button that can only fail")
		}
		if _, err := f.disputes.Allow(f.ctx, f.owner, d.ID); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("Allow returned %v, want a conflict", err)
		}
		// Refused before anything was written: the dispute is still open, so the
		// owner can still uphold it.
		var status string
		if err := f.pool.QueryRow(f.ctx,
			`SELECT status FROM destination_disputes WHERE id = $1`, d.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != dispute.StatusOpen {
			t.Errorf("status = %q after a refused decision, want it still open", status)
		}
	})

	t.Run("a dispute filed before the entry was recorded", func(t *testing.T) {
		f := newDispute(t)
		f.blockHost("evil.example", link.SourceReview)

		// Written directly, because no build produces one any more: this is the
		// shape 03300 inherited — a list-backed dispute carrying no entry, filed
		// by a build that stored only the typed host.
		id := uuid.Must(uuid.NewV7())
		if _, err := f.pool.Exec(f.ctx, `
			INSERT INTO destination_disputes
			       (id, host, blocked_host, url_defanged, reason_code, created_by_label)
			VALUES ($1, 'login.evil.example', '', 'https[:]//login[.]evil[.]example/x',
			        'low_confidence.operator_blocklist', 'editor@example.com')`,
			id); err != nil {
			t.Fatal(err)
		}

		_, err := f.disputes.Allow(f.ctx, f.owner, id)
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("Allow returned %v, want a conflict", err)
		}
		// And the refusal has to say which conflict it is. The generic one —
		// "nothing on the blocklist refuses that host any more" — is false here:
		// evil.example is still listed and still refusing. An owner told that
		// would go looking for an entry that is in front of them.
		if !contains(err.Error(), "before the blocklist entry was recorded") {
			t.Errorf("refusal reads %q. A dispute that names no entry and a "+
				"destination nothing refuses any more are different states and "+
				"cannot share a sentence.", err)
		}
		if _, still := f.blockedHosts()["evil.example"]; !still {
			t.Error("allowing a dispute that names no entry deleted one anyway. " +
				"Re-deriving it is exactly the behaviour the column replaced, and " +
				"doing it once for old rows keeps the defect alive on every " +
				"instance that has one.")
		}
		// Still open, so the owner can uphold it and close the queue.
		var status string
		if err := f.pool.QueryRow(f.ctx,
			`SELECT status FROM destination_disputes WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != dispute.StatusOpen {
			t.Errorf("status = %q after a refused decision, want it still open", status)
		}
	})

	t.Run("an environment entry belongs to the environment", func(t *testing.T) {
		f := newDispute(t)
		if err := f.links.SeedBlocklist(f.ctx, []string{"env-blocked.example"}); err != nil {
			t.Fatalf("seed blocklist: %v", err)
		}

		d, err := f.disputes.File(f.ctx, f.owner, "https://env-blocked.example/x")
		if err != nil {
			t.Fatalf("File: %v", err)
		}
		if _, err := f.disputes.Allow(f.ctx, f.owner, d.ID); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("Allow returned %v, want a conflict naming the environment variable", err)
		}
		if _, still := f.blockedHosts()["env-blocked.example"]; !still {
			t.Error("the environment's entry was deleted; the next boot would put it back, " +
				"so the decision would have reverted itself")
		}
	})
}

// TestTheQueueIsGatedOnTheNewPermission.
//
// Reading the queue requires destinations.review and deciding in it requires
// destinations.decide, and **no organization role grants either** since D98:
// they are instance-level grants held by the account that claimed the instance
// and by whoever it appoints. So an editor cannot review, an admin who arrived
// by invitation cannot, and — the whole of F15 — neither can the owner of any
// other organization on the instance.
func TestTheQueueIsGatedOnTheNewPermission(t *testing.T) {
	f := newDispute(t)
	editor := f.editor("editor@example.com")
	admin := f.editor("admin@example.com")
	f.blockHost("evil.example", link.SourceReview)

	for _, perm := range []string{dispute.PermReview, dispute.PermDecide} {
		if editor.Can(perm) {
			t.Errorf("an editor holds %s", perm)
		}
		if admin.Can(perm) {
			t.Errorf("somebody who arrived by invitation holds %s", perm)
		}
		if !f.owner.Can(perm) {
			t.Fatalf("the account that claimed the instance does not hold %s; the "+
				"setup flow's grant did not reach it", perm)
		}
	}

	// F15 itself, and the reason this milestone exists: a second organization's
	// owner is an owner in full, and reaches nothing here. Before D98 they read
	// every dispute on the instance and could lift a blocklist entry for
	// everybody, one registration away on an open instance.
	other, err := f.auth.Register(f.ctx, auth.RegisterInput{
		Email: "stranger@example.com", Name: "Stranger", Password: disputePassword,
	})
	if err != nil {
		t.Fatalf("register a second organization's owner: %v", err)
	}
	if other.Role != "owner" {
		t.Fatalf("the second account is %q, not an owner; this test proves nothing "+
			"unless it is one", other.Role)
	}
	for _, perm := range []string{dispute.PermReview, dispute.PermDecide} {
		if other.Can(perm) {
			t.Errorf("the owner of a second organization holds %s. That is F15: the "+
				"queue and its decisions are instance-wide, so a role grant hands "+
				"every registrant on an open instance the moderation of every other "+
				"organization's destinations.", perm)
		}
	}
	if _, err := f.disputes.List(f.ctx, other, dispute.Filter{}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("List for a second organization's owner returned %v, want forbidden", err)
	}

	d, err := f.disputes.File(f.ctx, editor, "https://evil.example/x")
	if err != nil {
		t.Fatalf("an account that can create links cannot file a dispute: %v", err)
	}

	if _, err := f.disputes.List(f.ctx, editor, dispute.Filter{}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("List for an editor returned %v, want forbidden", err)
	}
	if _, err := f.disputes.CountOpen(f.ctx, editor); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("CountOpen for an editor returned %v, want forbidden", err)
	}
	if _, err := f.disputes.Allow(f.ctx, editor, d.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("Allow for an editor returned %v, want forbidden", err)
	}
	if _, err := f.disputes.Uphold(f.ctx, editor, d.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("Uphold for an editor returned %v, want forbidden", err)
	}

	// And the owner can see it — the same row, from the queue.
	page, err := f.disputes.List(f.ctx, f.owner, dispute.Filter{OpenOnly: true})
	if err != nil {
		t.Fatalf("List for the owner: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != d.ID {
		t.Fatalf("the queue holds %d disputes, want the one that was filed", len(page.Items))
	}
	if page.Items[0].FiledBy != "editor@example.com" {
		t.Errorf("filed_by = %q, want the address of whoever filed it", page.Items[0].FiledBy)
	}
}

// TestOneOpenDisputePerHost bounds what a caller can put in front of the owner.
//
// The unique partial index decides it, not a check-then-insert, so two
// concurrent requests cannot both pass. A decided dispute does not block a later
// one: a host upheld today and argued about again next month is a new question.
func TestOneOpenDisputePerHost(t *testing.T) {
	f := newDispute(t)
	f.blockHost("evil.example", link.SourceReview)

	first, err := f.disputes.File(f.ctx, f.owner, "https://evil.example/one")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if _, err := f.disputes.File(f.ctx, f.owner, "https://evil.example/two"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("a second open dispute about the same host returned %v, want a conflict", err)
	}

	if _, err := f.disputes.Uphold(f.ctx, f.owner, first.ID); err != nil {
		t.Fatalf("Uphold: %v", err)
	}
	if _, err := f.disputes.File(f.ctx, f.owner, "https://evil.example/three"); err != nil {
		t.Errorf("a host whose dispute was decided cannot be disputed again: %v", err)
	}
}

// TestTheDisputePermissionSplitsIntoAReadAKeyMayHoldAndADecisionItMayNot is
// D98's second constraint — "API access is read-only for disputes; a change
// requires a person" — asserted as what it is actually built out of.
//
// It is deliberately **not** a branch on credential type. The inherited
// Permissions rule says anything branching on credential type outside
// NonDelegableScopes and D43 is a defect, so the constraint is implemented as
// the map: destinations.review leaves it and destinations.decide enters it. The
// two halves of this test are therefore the map itself and a key minted through
// the real service, because the map is the whole mechanism and a handler check
// would be the defect.
func TestTheDisputePermissionSplitsIntoAReadAKeyMayHoldAndADecisionItMayNot(t *testing.T) {
	if _, blocked := auth.NonDelegableScopes[dispute.PermDecide]; !blocked {
		t.Fatalf("%s is delegable to an API key. Allowing a destination deletes a row "+
			"from the instance-wide blocklist, after which the key that deleted it may "+
			"point links there — a credential widening its own reach (D18).",
			dispute.PermDecide)
	}
	if _, blocked := auth.NonDelegableScopes[dispute.PermReview]; blocked {
		t.Fatalf("%s is in NonDelegableScopes. D98 split the permission so that a key "+
			"may read the queue and may not act on it; putting the reading half back "+
			"makes the split decorative.", dispute.PermReview)
	}

	f := newDispute(t)
	keys, err := auth.NewAPIKeyService(f.pool, f.auth, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Create(f.ctx, f.owner, auth.CreateAPIKeyInput{
		Name: "decide-bot", Scopes: []string{dispute.PermDecide},
	}); err == nil {
		t.Fatal("a key was minted holding destinations.decide")
	}

	created, err := keys.Create(f.ctx, f.owner, auth.CreateAPIKeyInput{
		Name: "review-bot", Scopes: []string{dispute.PermReview},
	})
	if err != nil {
		t.Fatalf("a key holding only the reading half was refused: %v", err)
	}

	// The key as a request actually presents it, so what is asserted is the
	// identity the service authorizes rather than the row that was written.
	bot, err := keys.Authenticate(f.ctx, created.Key)
	if err != nil {
		t.Fatalf("authenticate the review key: %v", err)
	}
	if !bot.IsAPIKey() {
		t.Fatal("the resolved identity is not an API key; this test would then be " +
			"asserting nothing about delegation")
	}

	f.blockHost("evil.example", link.SourceReview)
	d, err := f.disputes.File(f.ctx, f.owner, "https://evil.example/x")
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if _, err := f.disputes.List(f.ctx, bot, dispute.Filter{}); err != nil {
		t.Errorf("a key holding destinations.review cannot read the queue: %v", err)
	}
	if _, err := f.disputes.Allow(f.ctx, bot, d.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("Allow for a key returned %v, want forbidden", err)
	}
	if _, err := f.disputes.Uphold(f.ctx, bot, d.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("Uphold for a key returned %v, want forbidden", err)
	}
}

// TestTheOutcomeIsEmailedOnlyWhenAMailerExists is D1's addendum, both halves.
//
// The claim is an ordering, not a feature flag: in-app delivery is the baseline
// and the email is the addition. So the same decision is made twice, on two
// instances that differ only in whether an SMTP relay is configured, and the
// inbox row has to be identical on both while the outbox is empty on one.
//
// This is the only notification in the feature addressed to somebody who did not
// choose to be an administrator, and the outcome is the one thing they are
// actually waiting for — a person who filed a dispute may not open the dashboard
// again for a week.
func TestTheOutcomeIsEmailedOnlyWhenAMailerExists(t *testing.T) {
	decide := func(f *disputeFixture) notify.Notification {
		f.t.Helper()
		f.blockHost("mailed.example", link.SourceReview)
		editor := f.editor("editor@example.com")
		filed, err := f.disputes.File(f.ctx, editor, "https://mailed.example/x")
		if err != nil {
			t.Fatalf("file: %v", err)
		}
		if _, err := f.disputes.Uphold(f.ctx, f.owner, filed.ID); err != nil {
			t.Fatalf("uphold: %v", err)
		}
		got := f.inbox(editor.UserID, dispute.KindDecided)
		if len(got) != 1 {
			t.Fatalf("the filer has %d %s notifications, want 1", len(got), dispute.KindDecided)
		}
		return got[0]
	}

	silent := newDispute(t)
	baseline := decide(silent)
	if rows := mailOutboxRows(t, silent.pool); len(rows) != 0 {
		t.Errorf("an instance with no mailer queued %d message(s); the mailer is "+
			"optional and nothing may depend on it", len(rows))
	}

	mailed := newDisputeWithMail(t)
	withMail := decide(mailed)

	// The baseline is unchanged by the addition. If these ever diverge, the
	// in-app notification has started depending on the mailer.
	if withMail.Title != baseline.Title || withMail.Body != baseline.Body {
		t.Errorf("the dashboard notification differs when a mailer exists:\n with: %q / %q\n"+
			"without: %q / %q", withMail.Title, withMail.Body, baseline.Title, baseline.Body)
	}

	rows := mailOutboxRows(t, mailed.pool)
	if len(rows) != 1 {
		t.Fatalf("outbox has %d rows, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.recipient != "editor@example.com" {
		t.Errorf("recipient = %q, want the person who filed it", got.recipient)
	}
	if got.kind != notify.MailDisputeDecided {
		t.Errorf("kind = %q, want %q", got.kind, notify.MailDisputeDecided)
	}
	// The host reaches the message defanged. It is a string a stranger chose,
	// being mailed to somebody who did not ask for it, so it must arrive as
	// something no client will make clickable.
	if !contains(got.body, "mailed[.]example") {
		t.Errorf("the mail does not carry the defanged host:\n%s", got.body)
	}
	if contains(got.body, "https://mailed.example") {
		t.Errorf("the mail carries a live URL:\n%s", got.body)
	}
}

// mailOutboxRows reads the queued mail. A local reader rather than the mail
// fixture's, because that one hangs off a fixture this file does not build.
type mailOutboxRow struct{ recipient, kind, subject, body string }

func mailOutboxRows(t *testing.T, pool *pgxpool.Pool) []mailOutboxRow {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`SELECT recipient, kind, subject, body FROM mail_outbox ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []mailOutboxRow
	for rows.Next() {
		var r mailOutboxRow
		if err := rows.Scan(&r.recipient, &r.kind, &r.subject, &r.body); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
