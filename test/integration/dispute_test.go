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
}

func newDispute(t *testing.T) *disputeFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	owner, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner", Password: disputePassword,
	})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}

	auditSvc := audit.NewService(pool)
	notifySvc := notify.NewService(pool)
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
	}
	// Re-read, so the owner carries destinations.review from the migration's
	// grant rather than from whatever Register happened to compute.
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
// Reading it and deciding in it both require destinations.review, which the
// migration grants to the owner role alone — so an editor, who can file, cannot
// review, and an admin who arrived by invitation cannot either.
func TestTheQueueIsGatedOnTheNewPermission(t *testing.T) {
	f := newDispute(t)
	editor := f.editor("editor@example.com")
	admin := f.editor("admin@example.com")
	f.blockHost("evil.example", link.SourceReview)

	if editor.Can(dispute.PermReview) {
		t.Error("an editor holds destinations.review")
	}
	if admin.Can(dispute.PermReview) {
		t.Error("somebody who arrived by invitation holds destinations.review")
	}
	if !f.owner.Can(dispute.PermReview) {
		t.Fatal("the owner does not hold destinations.review; the seed migration's grant " +
			"did not reach the role")
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

// TestTheReviewPermissionIsNotDelegable.
//
// D18's escalating limb: a key that can allow a destination can then point links
// at it. auth.NonDelegableScopes is the one place that enforces it, so this
// asserts the mechanism rather than a handler branch.
func TestTheReviewPermissionIsNotDelegable(t *testing.T) {
	if _, blocked := auth.NonDelegableScopes[dispute.PermReview]; !blocked {
		t.Fatalf("%s is delegable to an API key. Allowing a destination deletes a row "+
			"from the instance-wide blocklist, after which the key that deleted it may "+
			"point links there — a credential widening its own reach (D18).",
			dispute.PermReview)
	}

	f := newDispute(t)
	keys, err := auth.NewAPIKeyService(f.pool, f.auth, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Create(f.ctx, f.owner, auth.CreateAPIKeyInput{
		Name: "review-bot", Scopes: []string{dispute.PermReview},
	}); err == nil {
		t.Fatal("a key was minted holding destinations.review")
	}
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
