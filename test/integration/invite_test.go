//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/invite"
	"github.com/DevOfPie/LinkCtrl/internal/mail"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
)

// M27. Invitations: issuing, delivery, redemption, and the three things the
// milestone is really about — membership only (D6), the closed-mode ceiling
// (D7), and refusals that answer nothing (D27).

const invitePassword = "a-sufficiently-long-password"

// inviteFixture is an instance with an owner, and whichever halves of the
// mailer and the signup ceiling a test needs.
type inviteFixture struct {
	pool    *pgxpool.Pool
	auth    *auth.Service
	keys    *auth.APIKeyService
	invites *invite.Service
	notify  *notify.Service
	mail    *mail.Service
	sender  *recordingSender
	owner   *auth.Identity
	// server is nil unless a test asks for the HTTP surface.
	server *httptest.Server
}

type inviteOptions struct {
	// NewAccounts is SIGNUP_MODE being something other than `closed`.
	NewAccounts bool
	WithMailer  bool
	// TTL defaults to a week, matching the shipped default.
	TTL time.Duration
}

func newInviteFixture(t *testing.T, opts inviteOptions) *inviteFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	owner, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner", Password: invitePassword,
	})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	keySvc, err := auth.NewAPIKeyService(pool, authSvc, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatal(err)
	}

	f := &inviteFixture{
		pool: pool, auth: authSvc, keys: keySvc,
		notify: notify.NewService(pool), owner: owner,
	}

	cfg := invite.Config{
		AppURL:      "https://links.example.com",
		TTL:         opts.TTL,
		NewAccounts: opts.NewAccounts,
		Hasher:      authSvc.Hasher(),
		Audit:       audit.NewService(pool),
		Notify:      f.notify,
	}
	if cfg.TTL == 0 {
		cfg.TTL = 168 * time.Hour
	}
	if opts.WithMailer {
		f.sender = &recordingSender{}
		f.mail = newMailService(t, pool, f.sender)
		cfg.Mail = f.mail
	}

	f.invites, err = invite.NewService(pool, cfg)
	if err != nil {
		t.Fatalf("invite.NewService: %v", err)
	}
	return f
}

// serve brings up the HTTP surface, for the assertions that are about what a
// client can observe rather than what a service returns.
func (f *inviteFixture) serve(t *testing.T, mode config.SignupMode) *httptest.Server {
	t.Helper()
	if f.server != nil {
		return f.server
	}
	cfg := config.Config{
		AppEnv: config.Development, BaseURL: "http://links.test", SecureCookies: false,
	}
	cfg.Auth.SignupMode = mode
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour

	f.server = httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config: cfg, Auth: f.auth, Keys: f.keys, Invites: f.invites,
	}))
	t.Cleanup(f.server.Close)
	return f.server
}

// invited issues one invitation and returns it.
func (f *inviteFixture) invited(t *testing.T, email, role string) *invite.Created {
	t.Helper()
	c, err := f.invites.Create(t.Context(), f.owner, invite.CreateInput{Email: email, Role: role})
	if err != nil {
		t.Fatalf("create invitation for %s: %v", email, err)
	}
	return c
}

// tokenOf pulls the token back out of the copyable link, which is the only
// place it exists.
func tokenOf(t *testing.T, c *invite.Created) string {
	t.Helper()
	const prefix = "https://links.example.com/invite/"
	if len(c.URL) <= len(prefix) || c.URL[:len(prefix)] != prefix {
		t.Fatalf("invitation URL %q is not the documented shape", c.URL)
	}
	return c.URL[len(prefix):]
}

// register makes an ordinary account with its own personal organization, for
// the tests where somebody already exists before they are invited.
func (f *inviteFixture) register(t *testing.T, email string) *auth.Identity {
	t.Helper()
	id, err := f.auth.Register(t.Context(), auth.RegisterInput{
		Email: email, Name: "", Password: invitePassword,
	})
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	return id
}

func (f *inviteFixture) redeem(t *testing.T, token, email, password string) (*invite.Redeemed, error) {
	t.Helper()
	return f.invites.Redeem(t.Context(), invite.RedeemInput{
		Token: token, Email: email, Name: "", Password: password,
	})
}

// queuedMail is one outbox row, as the F32 assertions read it.
type queuedMail struct {
	subject, body, kind, status, lastError string
	attempts                               int
}

// outboxRow reads the newest queued message for an address.
func (f *inviteFixture) outboxRow(t *testing.T, recipient string) queuedMail {
	t.Helper()
	var r queuedMail
	if err := f.pool.QueryRow(t.Context(),
		`SELECT subject, body, kind, status, last_error, attempts
		   FROM mail_outbox WHERE recipient = $1
		  ORDER BY created_at DESC, id DESC LIMIT 1`, recipient).
		Scan(&r.subject, &r.body, &r.kind, &r.status, &r.lastError, &r.attempts); err != nil {
		t.Fatalf("no outbox row for %s: %v", recipient, err)
	}
	return r
}

// scalar runs a single-value query, for the assertions that are about rows
// rather than about return values.
func (f *inviteFixture) scalar(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// ─── issuing ────────────────────────────────────────────────────────────────

// members.write, its first enforcement. An editor holds no such permission, so
// none of the three administrative operations is reachable for them.
//
// The editor is made by redeeming an invitation, which is the only way this
// milestone produces a second member — so this also proves the redeemed
// membership carries the role the invitation named rather than a default.
func TestInvitationsRequireMembersWrite(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})

	c := f.invited(t, "editor@example.com", "editor")
	if _, err := f.redeem(t, tokenOf(t, c), "editor@example.com", invitePassword); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	editor, err := f.auth.IdentityForEmail(t.Context(), "editor@example.com")
	if err != nil {
		t.Fatalf("identity for the invited editor: %v", err)
	}
	if editor.Role != "editor" {
		t.Fatalf("the redeemed membership has role %q, want editor", editor.Role)
	}
	if editor.Can(invite.PermWrite) {
		t.Fatal("an editor holds members.write; the role grants would have to have changed")
	}

	if _, err := f.invites.Create(t.Context(), editor,
		invite.CreateInput{Email: "x@example.com", Role: "viewer"}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an editor created an invitation: %v", err)
	}
	if _, err := f.invites.List(t.Context(), editor); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an editor listed invitations: %v", err)
	}
	if err := f.invites.Revoke(t.Context(), editor, c.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an editor revoked an invitation: %v", err)
	}
}

// D28: an invitation may carry any role at or below the inviter's own rank.
//
// The owner may issue all four. An admin may issue three, and asking for owner
// is refused — which is the privilege-escalation path the decision closed, since
// an admin who could invite an owner would be promoted by them an hour later.
func TestInvitationRoleCeilingIsTheInvitersOwnRank(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})

	for _, role := range []string{"owner", "admin", "editor", "viewer"} {
		if _, err := f.invites.Create(t.Context(), f.owner,
			invite.CreateInput{Email: "as-" + role + "@example.com", Role: role}); err != nil {
			t.Fatalf("an owner could not invite at %s: %v", role, err)
		}
	}

	admin := f.invited(t, "admin2@example.com", "admin")
	if _, err := f.redeem(t, tokenOf(t, admin), "admin2@example.com", invitePassword); err != nil {
		t.Fatalf("redeem admin: %v", err)
	}
	adminID, err := f.auth.IdentityForEmail(t.Context(), "admin2@example.com")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.invites.Create(t.Context(), adminID,
		invite.CreateInput{Email: "co-owner@example.com", Role: "owner"}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an admin invited an owner: %v", err)
	}
	for _, role := range []string{"admin", "editor", "viewer"} {
		if _, err := f.invites.Create(t.Context(), adminID,
			invite.CreateInput{Email: "a-" + role + "@example.com", Role: role}); err != nil {
			t.Errorf("an admin could not invite at %s: %v", role, err)
		}
	}

	// The form offers exactly what the ceiling permits, so a control cannot
	// present a choice the service will then refuse.
	roles, err := f.invites.Roles(t.Context(), adminID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if r.Slug == "owner" {
			t.Error("the role list offered an admin the owner role")
		}
	}
	if len(roles) != 3 {
		t.Errorf("an admin is offered %d roles, want admin, editor and viewer", len(roles))
	}
}

// Omitting the role admits at the least powerful one. A caller that did not
// think about it must not hand out more than they meant to.
func TestInvitationWithNoRoleIsAViewer(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})
	c := f.invited(t, "quiet@example.com", "")
	if c.Role != invite.DefaultRole {
		t.Errorf("an invitation with no role carries %q, want %q", c.Role, invite.DefaultRole)
	}
}

// Both delivery paths, which is the milestone's own wording. The copyable link
// exists either way; the mail is the addition, and its absence is the default
// instance rather than a failure.
func TestInvitationDeliveryWithAndWithoutAMailer(t *testing.T) {
	t.Run("with a relay configured", func(t *testing.T) {
		f := newInviteFixture(t, inviteOptions{NewAccounts: true, WithMailer: true})
		c := f.invited(t, "mailed@example.com", "editor")

		if !c.Emailed {
			t.Error("emailed = false with a relay configured")
		}
		if c.URL == "" {
			t.Error("no copyable link was returned; it must exist on every path")
		}
		if n := f.scalar(t, `SELECT count(*) FROM mail_outbox WHERE kind = 'invitation'`); n != 1 {
			t.Fatalf("outbox holds %d invitation mails, want 1", n)
		}
		if err := f.mail.Drain(t.Context()); err != nil {
			t.Fatalf("drain: %v", err)
		}
		sent := f.sender.delivered()
		if len(sent) != 1 {
			t.Fatalf("delivered %d messages, want 1", len(sent))
		}
		if sent[0].To != "mailed@example.com" {
			t.Errorf("delivered to %q", sent[0].To)
		}
		// The link is what the mail is for. A message that arrives without it
		// is a notification, not an invitation.
		if !strings.Contains(sent[0].Body, c.URL) {
			t.Errorf("the mail does not carry the invitation link:\n%s", sent[0].Body)
		}
	})

	t.Run("and the delivered row keeps no token", func(t *testing.T) {
		// Finding F32, at the milestone whose migration makes the claim: the
		// invitation *table* stores only SHA-256(token), but until this release
		// the outbox held the same token in clear, rendered into the message
		// body, for the thirty days a finished row is kept. A token read out of
		// that column by SQL alone hashed and redeemed, up to owner.
		//
		// Two halves, and the first is what makes the second mean anything. The
		// pending row genuinely does carry the token — it has to, it is the
		// message waiting to be sent — so this asserts the exposure exists and
		// then asserts it ends at delivery rather than at retention.
		f := newInviteFixture(t, inviteOptions{NewAccounts: true, WithMailer: true})
		c := f.invited(t, "scrubbed@example.com", "editor")
		token := tokenOf(t, c)

		queued := f.outboxRow(t, "scrubbed@example.com")
		if queued.status != "pending" {
			t.Fatalf("a freshly queued row is %q", queued.status)
		}
		if !strings.Contains(queued.body, token) {
			t.Fatalf("the queued mail does not carry the token, so this test is " +
				"asserting nothing; the leak it closes was reading it from here")
		}

		if err := f.mail.Drain(t.Context()); err != nil {
			t.Fatalf("drain: %v", err)
		}

		sent := f.outboxRow(t, "scrubbed@example.com")
		if sent.status != "sent" {
			t.Fatalf("after a successful drain the row is %q", sent.status)
		}
		// Every column the row still has, not only the one the finding named. A
		// scrub that moved the token into last_error would pass a body-only
		// check and leak exactly as much.
		for column, value := range map[string]string{
			"body": sent.body, "subject": sent.subject, "last_error": sent.lastError,
		} {
			if strings.Contains(value, token) {
				t.Errorf("mail_outbox.%s still holds the invitation token after "+
					"delivery: %q", column, value)
			}
		}
		if sent.body != "" {
			t.Errorf("body = %q after delivery, want it emptied", sent.body)
		}
		// What the outbox exists to tell an operator is untouched. The record of
		// what was attempted is recipient, kind, attempts and last_error — 01100
		// says so — and none of those is the message.
		if sent.kind != "invitation" || sent.subject == "" || sent.attempts != 1 {
			t.Errorf("the record of the attempt was damaged: %+v", sent)
		}

		// And the mail was really sent before the row was emptied, so this is a
		// scrub and not a silent drop.
		delivered := f.sender.delivered()
		if len(delivered) != 1 || !strings.Contains(delivered[0].Body, c.URL) {
			t.Fatalf("the relay received %d messages, and the link did not survive "+
				"to the wire", len(delivered))
		}
		// The invitation itself is untouched: scrubbing the copy must not spend
		// the credential the recipient is holding.
		if _, err := f.redeem(t, token, "scrubbed@example.com", invitePassword); err != nil {
			t.Fatalf("the delivered invitation no longer redeems: %v", err)
		}
	})

	t.Run("with no relay", func(t *testing.T) {
		f := newInviteFixture(t, inviteOptions{NewAccounts: true})
		c := f.invited(t, "copied@example.com", "editor")

		if c.Emailed {
			t.Error("emailed = true on an instance with no relay")
		}
		if c.URL == "" {
			t.Fatal("no copyable link; on a mail-free instance it is the only delivery path")
		}
		if n := f.scalar(t, `SELECT count(*) FROM mail_outbox`); n != 0 {
			t.Errorf("the outbox holds %d rows on a mail-free instance, want 0", n)
		}
		// And it works: the link is the whole mechanism, so it has to redeem.
		if _, err := f.redeem(t, tokenOf(t, c), "copied@example.com", invitePassword); err != nil {
			t.Fatalf("the copyable link did not redeem: %v", err)
		}
	})
}

// ─── redemption ─────────────────────────────────────────────────────────────

// D6: redemption creates a membership and nothing else.
//
// The invited person is a colleague in an organization that already exists.
// Provisioning them a personal organization and workspace of their own — which
// is what registration does — would make them a tenant, and the "one personal
// org per user" invariant is deliberately broken here.
func TestRedemptionCreatesMembershipOnly(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})

	orgsBefore := f.scalar(t, `SELECT count(*) FROM organizations`)
	wsBefore := f.scalar(t, `SELECT count(*) FROM workspaces`)

	c := f.invited(t, "colleague@example.com", "editor")
	out, err := f.redeem(t, tokenOf(t, c), "colleague@example.com", invitePassword)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if !out.Created {
		t.Error("account_created = false, but no account existed at that address")
	}
	if out.OrganizationID != f.owner.OrgID {
		t.Errorf("joined organization %s, want the inviter's %s", out.OrganizationID, f.owner.OrgID)
	}

	if n := f.scalar(t, `SELECT count(*) FROM organizations`); n != orgsBefore {
		t.Errorf("organizations went from %d to %d; redemption provisioned one", orgsBefore, n)
	}
	if n := f.scalar(t, `SELECT count(*) FROM workspaces`); n != wsBefore {
		t.Errorf("workspaces went from %d to %d; redemption provisioned one", wsBefore, n)
	}
	if n := f.scalar(t,
		`SELECT count(*) FROM memberships WHERE user_id = $1`, out.UserID); n != 1 {
		t.Errorf("the invited account holds %d memberships, want exactly 1", n)
	}
	if n := f.scalar(t,
		`SELECT count(*) FROM memberships WHERE user_id = $1 AND organization_id = $2`,
		out.UserID, f.owner.OrgID); n != 1 {
		t.Error("the one membership is not in the inviting organization")
	}
	// The invariant D6 deliberately breaks, asserted as the thing it is rather
	// than left implicit: this account belongs to somebody else's personal
	// organization and has none of its own. Registration's "one personal org per
	// user" does not hold for an invited account, and that is the decision.
	if n := f.scalar(t, `
		SELECT count(*) FROM memberships m JOIN organizations o ON o.id = m.organization_id
		 WHERE m.user_id = $1 AND o.is_personal AND o.id = $2`,
		out.UserID, f.owner.OrgID); n != 1 {
		t.Error("the invited account is not in the inviter's organization")
	}
	// The membership covers the whole organization, which is the shape
	// registration creates and the one the evaluator already resolves.
	if n := f.scalar(t,
		`SELECT count(*) FROM memberships WHERE user_id = $1 AND workspace_id IS NULL`,
		out.UserID); n != 1 {
		t.Error("the membership is workspace-scoped; M27 issues organization-wide ones")
	}

	// And they can act: the identity resolves into the inviter's workspace
	// rather than into nothing, which is what a user with no workspace would be.
	id, err := f.auth.IdentityForEmail(t.Context(), "colleague@example.com")
	if err != nil {
		t.Fatalf("the invited account resolves to no identity: %v", err)
	}
	if id.OrgID != f.owner.OrgID || id.WorkspaceID != f.owner.WorkspaceID {
		t.Errorf("the invited account acts in org %s workspace %s, want the inviter's %s / %s",
			id.OrgID, id.WorkspaceID, f.owner.OrgID, f.owner.WorkspaceID)
	}
}

// The requirement D6 attached to membership-only: the invited account is an
// ordinary account, so it can be made an owner later without a second one.
//
// Asserted as what it is — nothing about the account is second-class — rather
// than by exercising `orgs.create`, which M28 introduces along with the org
// creation it guards. What M27 owes is that the account it makes is reachable
// by an ordinary role grant, and that is what this checks.
func TestInvitedAccountStaysCapableOfOwningAnOrganization(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})

	c := f.invited(t, "future-owner@example.com", "viewer")
	out, err := f.redeem(t, tokenOf(t, c), "future-owner@example.com", invitePassword)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	// An ordinary user row: active, with a password of its own, not marked or
	// flagged as invite-only in any way that a later grant would have to
	// special-case.
	var status string
	var hasPassword bool
	if err := f.pool.QueryRow(t.Context(),
		`SELECT status, password_hash IS NOT NULL FROM users WHERE id = $1`,
		out.UserID).Scan(&status, &hasPassword); err != nil {
		t.Fatal(err)
	}
	if status != "active" || !hasPassword {
		t.Errorf("the invited account is status=%q hasPassword=%v; not an ordinary account",
			status, hasPassword)
	}

	// A later role grant reaches it through the same RBAC path as anyone else's.
	// Promoting it to owner is exactly the shape M28's org creation needs, and
	// it needs no second account to do it.
	if _, err := f.pool.Exec(t.Context(), `
		UPDATE memberships SET role_id = (SELECT id FROM roles WHERE slug = 'owner' AND organization_id IS NULL)
		 WHERE user_id = $1`, out.UserID); err != nil {
		t.Fatal(err)
	}
	id, err := f.auth.IdentityForEmail(t.Context(), "future-owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if id.Role != "owner" {
		t.Fatalf("after the grant the invited account is %q, want owner", id.Role)
	}
	if !id.Can("org.delete") {
		t.Error("the promoted account holds no owner-only permission; the grant did not reach it")
	}
}

// D7: the environment ceiling is absolute, and this is its matrix.
//
// Under `closed`, an existing account joins and an unknown address is refused —
// and, crucially, the refusal is the same one every other failure produces, so
// the pair does not answer "was that address already registered".
func TestClosedModeAdmitsExistingAccountsAndNoOthers(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: false})

	// Existing account: succeeds. The membership is added to the account they
	// already had rather than to a new one.
	existing := f.register(t, "already@example.com")
	c := f.invited(t, "already@example.com", "editor")
	out, err := f.redeem(t, tokenOf(t, c), "already@example.com", invitePassword)
	if err != nil {
		t.Fatalf("an existing account could not redeem under closed: %v", err)
	}
	if out.Created {
		t.Error("account_created = true for an account that already existed")
	}
	if out.UserID != existing.UserID {
		t.Errorf("redemption produced user %s, want the existing %s", out.UserID, existing.UserID)
	}

	// Unknown address: refused, and no account is left behind.
	usersBefore := f.scalar(t, `SELECT count(*) FROM users`)
	stranger := f.invited(t, "stranger@example.com", "editor")
	_, err = f.redeem(t, tokenOf(t, stranger), "stranger@example.com", invitePassword)
	if !errors.Is(err, invite.ErrNotRedeemable) {
		t.Fatalf("a new account was admitted under closed: %v", err)
	}
	if n := f.scalar(t, `SELECT count(*) FROM users`); n != usersBefore {
		t.Errorf("users went from %d to %d under closed; an invitation created an account",
			usersBefore, n)
	}
	// The invitation is untouched, so lifting the ceiling later makes it work
	// rather than having silently spent it.
	if n := f.scalar(t,
		`SELECT count(*) FROM invitations WHERE id = $1 AND redeemed_at IS NULL`,
		stranger.ID); n != 1 {
		t.Error("a refused redemption spent the invitation")
	}
}

// The other half of the closed-mode matrix: under `invite`, the same unknown
// address is admitted. Without this, the test above would also pass on an
// implementation that refuses everybody.
func TestInviteModeAdmitsANewAccount(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})
	c := f.invited(t, "stranger@example.com", "editor")
	out, err := f.redeem(t, tokenOf(t, c), "stranger@example.com", invitePassword)
	if err != nil {
		t.Fatalf("a new registrant was refused under invite mode: %v", err)
	}
	if !out.Created {
		t.Error("account_created = false, but no account existed")
	}
}

// D27: the invitation is bound to the address it names, not to whoever holds
// the link.
func TestInvitationIsBoundToTheInvitedAddress(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})
	f.register(t, "someone-else@example.com")

	c := f.invited(t, "intended@example.com", "editor")
	// A real account, a real password, a real link — and still refused, because
	// the link was not issued to them. This is the forwarded-invitation case.
	if _, err := f.redeem(t, tokenOf(t, c), "someone-else@example.com", invitePassword); !errors.Is(err, invite.ErrNotRedeemable) {
		t.Fatalf("a forwarded invitation admitted a different address: %v", err)
	}
	if n := f.scalar(t, `SELECT count(*) FROM memberships WHERE organization_id = $1`,
		f.owner.OrgID); n != 1 {
		t.Errorf("the organization has %d memberships, want only the owner's", n)
	}
	// Case does not matter: the comparison is against the generated lowercase
	// column on both sides.
	if _, err := f.redeem(t, tokenOf(t, c), "INTENDED@Example.COM", invitePassword); err != nil {
		t.Fatalf("the invited address was refused because of its case: %v", err)
	}
}

// Single-use, enforced by the database rather than by a check in Go.
func TestInvitationIsSingleUse(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})
	c := f.invited(t, "once@example.com", "editor")
	token := tokenOf(t, c)

	out, err := f.redeem(t, token, "once@example.com", invitePassword)
	if err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	// The row itself records who spent it and when, which is the half of
	// single-use that survives the membership index also refusing a duplicate.
	var spentAt time.Time
	var spentBy uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT redeemed_at, redeemed_by FROM invitations WHERE id = $1`,
		c.ID).Scan(&spentAt, &spentBy); err != nil {
		t.Fatalf("the invitation was not marked redeemed: %v", err)
	}
	if spentBy != out.UserID {
		t.Errorf("redeemed_by = %s, want the account that joined (%s)", spentBy, out.UserID)
	}

	if _, err := f.redeem(t, token, "once@example.com", invitePassword); !errors.Is(err, invite.ErrNotRedeemable) {
		t.Fatalf("the invitation redeemed twice: %v", err)
	}
	if n := f.scalar(t,
		`SELECT count(*) FROM memberships WHERE organization_id = $1`, f.owner.OrgID); n != 2 {
		t.Errorf("the organization has %d memberships, want the owner plus one", n)
	}
	// And the second attempt changed nothing about the row. A re-stamped
	// redeemed_at would mean the spend was not conditional, so "who used this,
	// and when" would answer with the last attempt rather than the one that
	// worked.
	var afterAt time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT redeemed_at FROM invitations WHERE id = $1`, c.ID).Scan(&afterAt); err != nil {
		t.Fatal(err)
	}
	if !afterAt.Equal(spentAt) {
		t.Errorf("redeemed_at moved from %s to %s; the spend is not conditional", spentAt, afterAt)
	}
}

// Revoked and expired invitations are both refused, and D29's TTL is what makes
// the second one true.
func TestRevokedAndExpiredInvitationsAreRefused(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})

	revoked := f.invited(t, "revoked@example.com", "editor")
	if err := f.invites.Revoke(t.Context(), f.owner, revoked.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := f.redeem(t, tokenOf(t, revoked), "revoked@example.com", invitePassword); !errors.Is(err, invite.ErrNotRedeemable) {
		t.Errorf("a revoked invitation redeemed: %v", err)
	}
	// Revoking twice is not-found rather than a second success, and neither is
	// revoking somebody else's.
	if err := f.invites.Revoke(t.Context(), f.owner, revoked.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("re-revoking answered %v, want not-found", err)
	}
	if err := f.invites.Revoke(t.Context(), f.owner, uuid.Must(uuid.NewV7())); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("revoking an unknown id answered %v, want not-found", err)
	}

	// The TTL is the knob D29 chose, so a short one has to actually shorten the
	// window rather than being decoration on the row.
	short := newInviteFixture(t, inviteOptions{NewAccounts: true, TTL: time.Millisecond})
	lapsed := short.invited(t, "lapsed@example.com", "editor")
	if !lapsed.ExpiresAt.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("INVITE_TTL was ignored: expires_at is %s", lapsed.ExpiresAt)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := short.redeem(t, tokenOf(t, lapsed), "lapsed@example.com", invitePassword); !errors.Is(err, invite.ErrNotRedeemable) {
		t.Errorf("an expired invitation redeemed: %v", err)
	}
	// And it is re-issuable: a lapsed invitation must not occupy the address
	// forever, or a colleague who was slow can never be invited again.
	if _, err := short.invites.Create(t.Context(), short.owner,
		invite.CreateInput{Email: "lapsed@example.com", Role: "editor"}); err != nil {
		t.Errorf("re-inviting after a lapse was refused: %v", err)
	}
}

// At most one outstanding invitation per address, so revoking the one an
// administrator can see cannot leave another one live.
func TestOnlyOneOutstandingInvitationPerAddress(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})
	f.invited(t, "twice@example.com", "editor")

	_, err := f.invites.Create(t.Context(), f.owner,
		invite.CreateInput{Email: "twice@example.com", Role: "viewer"})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("a second outstanding invitation was issued: %v", err)
	}
	// Inviting somebody who is already in the organization is refused too,
	// which is a fact about the actor's own organization and safe to tell them.
	c := f.invited(t, "member@example.com", "editor")
	if _, err := f.redeem(t, tokenOf(t, c), "member@example.com", invitePassword); err != nil {
		t.Fatal(err)
	}
	if _, err := f.invites.Create(t.Context(), f.owner,
		invite.CreateInput{Email: "member@example.com", Role: "editor"}); !errors.As(err, &ve) {
		t.Errorf("inviting an existing member answered %v, want a validation error", err)
	}
}

// ─── the thing the milestone is most careful about ──────────────────────────

// Every redemption failure is indistinguishable, over HTTP, byte for byte.
//
// This is M27's no-enumeration bullet and D27's hazard in one assertion. The
// pair that matters is the third and fourth case: an address with no account and
// an address with one produce the same answer, so redemption cannot be used to
// ask whether somebody is registered. The rest are here because a status code
// that differed for any of them would reopen the same oracle from another angle.
func TestRedemptionRefusalsAreIndistinguishable(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})
	srv := f.serve(t, config.SignupInvite)
	f.register(t, "has-an-account@example.com")

	live := f.invited(t, "target@example.com", "editor")
	existing := f.invited(t, "has-an-account@example.com", "editor")
	revoked := f.invited(t, "revoked@example.com", "editor")
	if err := f.invites.Revoke(t.Context(), f.owner, revoked.ID); err != nil {
		t.Fatal(err)
	}
	spent := f.invited(t, "spent@example.com", "editor")
	if _, err := f.redeem(t, tokenOf(t, spent), "spent@example.com", invitePassword); err != nil {
		t.Fatal(err)
	}

	// Somebody who was invited and then joined by another route — which is what
	// M28's member management will be. Reaching the already-a-member branch
	// needs it, and that branch has to answer like every other one.
	member := f.register(t, "joined-elsewhere@example.com")
	alreadyIn := f.invited(t, "joined-elsewhere@example.com", "editor")
	if _, err := f.pool.Exec(t.Context(), `
		INSERT INTO memberships (id, user_id, organization_id, role_id)
		VALUES (gen_random_uuid(), $1, $2,
		        (SELECT id FROM roles WHERE slug = 'viewer' AND organization_id IS NULL))`,
		member.UserID, f.owner.OrgID); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name              string
		token, email, pwd string
	}{
		{"a token that was never issued", "0123456789012345678901234567890123456789012", "target@example.com", invitePassword},
		{"the right token, the wrong password", tokenOf(t, existing), "has-an-account@example.com", "the-wrong-password-x"},
		// The pair this test exists for. One address has no account and one
		// does; neither is the invited one, and the two answers must not differ
		// by a byte.
		{"an address with no account", tokenOf(t, live), "nobody@example.com", invitePassword},
		{"an address that does have an account", tokenOf(t, live), "has-an-account@example.com", invitePassword},
		{"the inviter's own address", tokenOf(t, live), "owner@example.com", invitePassword},
		{"somebody who is already a member", tokenOf(t, alreadyIn), "joined-elsewhere@example.com", invitePassword},
		{"a revoked invitation", tokenOf(t, revoked), "revoked@example.com", invitePassword},
		{"an invitation already used", tokenOf(t, spent), "spent@example.com", invitePassword},
	}

	var first string
	for i, tc := range cases {
		status, body := postJSON(t, srv, "/api/v1/invitations/redeem", map[string]string{
			"token": tc.token, "email": tc.email, "password": tc.pwd,
		})
		if status != http.StatusNotFound {
			t.Fatalf("%s answered %d, want 404\n%s", tc.name, status, body)
		}
		if i == 0 {
			first = body
			continue
		}
		if body != first {
			t.Errorf("%s answered a different body from the first case:\n got: %s\nwant: %s",
				tc.name, body, first)
		}
	}

	// Nothing was created by any of it: the owner, the one real redemption, and
	// the membership this test inserted by hand.
	if n := f.scalar(t, `SELECT count(*) FROM memberships WHERE organization_id = $1`,
		f.owner.OrgID); n != 3 {
		t.Errorf("the organization has %d memberships, want 3; a refusal admitted somebody", n)
	}
}

// The two refusals that are *not* generic, because they are about the values
// the caller sent rather than about anybody's account.
func TestRedemptionStillReportsMalformedInput(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})
	c := f.invited(t, "fine@example.com", "editor")

	if _, err := f.redeem(t, tokenOf(t, c), "not-an-address", invitePassword); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a malformed address answered %v, want a validation error", err)
	}
	if _, err := f.redeem(t, tokenOf(t, c), "fine@example.com", "short"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a short password answered %v, want a validation error", err)
	}
	// The length check runs before anything is looked up, so it cannot be the
	// thing that reveals whether an account exists: an unknown address with a
	// short password answers the same way.
	if _, err := f.redeem(t, tokenOf(t, c), "unknown@example.com", "short"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a short password for an unknown address answered %v, want the same validation error", err)
	}
}

// ─── audit, notification, delegability ──────────────────────────────────────

// Issued, revoked and redeemed are all audit events (M21), and the redeemed one
// is recorded against the person who joined.
func TestInvitationLifecycleIsAudited(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})

	joined := f.invited(t, "joiner@example.com", "editor")
	out, err := f.redeem(t, tokenOf(t, joined), "joiner@example.com", invitePassword)
	if err != nil {
		t.Fatal(err)
	}
	dropped := f.invited(t, "dropped@example.com", "viewer")
	if err := f.invites.Revoke(t.Context(), f.owner, dropped.ID); err != nil {
		t.Fatal(err)
	}

	for _, want := range []struct {
		action, actor string
		target        uuid.UUID
	}{
		{audit.ActionInvitationCreated, "owner@example.com", joined.ID},
		{audit.ActionInvitationRedeemed, "joiner@example.com", joined.ID},
		{audit.ActionInvitationCreated, "owner@example.com", dropped.ID},
		{audit.ActionInvitationRevoked, "owner@example.com", dropped.ID},
	} {
		n := f.scalar(t, `
			SELECT count(*) FROM audit_logs
			 WHERE action = $1 AND actor_label = $2 AND target_id = $3
			   AND target_type = 'invitation' AND organization_id = $4`,
			want.action, want.actor, want.target, f.owner.OrgID)
		if n != 1 {
			t.Errorf("%s by %s recorded %d times, want 1", want.action, want.actor, n)
		}
	}

	// The created record carries the address and the role, which is what makes
	// "invited alice@…, alice@… joined" readable as one story months later.
	if n := f.scalar(t, `
		SELECT count(*) FROM audit_logs
		 WHERE action = $1 AND metadata->>'email' = 'joiner@example.com'
		   AND metadata->>'role' = 'editor'`, audit.ActionInvitationCreated); n != 1 {
		t.Error("the created event does not record the address and role invited")
	}
	// A token must never reach a table that is read back verbatim.
	if n := f.scalar(t,
		`SELECT count(*) FROM audit_logs WHERE metadata::text LIKE '%' || $1 || '%'`,
		tokenOf(t, joined)); n != 0 {
		t.Error("an invitation token was written into the audit log")
	}
	_ = out
}

// An accepted invitation notifies the inviter, in-app (M22).
func TestAcceptedInvitationNotifiesTheInviter(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})
	c := f.invited(t, "accepter@example.com", "editor")
	if _, err := f.redeem(t, tokenOf(t, c), "accepter@example.com", invitePassword); err != nil {
		t.Fatal(err)
	}

	unread, err := f.notify.Unread(t.Context(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	if unread != 1 {
		t.Fatalf("the inviter has %d unread notifications, want 1", unread)
	}
	if n := f.scalar(t, `
		SELECT count(*) FROM notifications
		 WHERE user_id = $1 AND kind = $2 AND title LIKE '%accepter@example.com%'`,
		f.owner.UserID, notify.KindInviteAccepted); n != 1 {
		t.Error("no invite.accepted notification names who joined")
	}
	// The person who joined is not told about their own action.
	joined, err := f.auth.IdentityForEmail(t.Context(), "accepter@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := f.notify.Unread(t.Context(), joined); n != 0 {
		t.Errorf("the new member has %d notifications about their own join", n)
	}
}

// D28's delegability conclusion, tested rather than only recorded: a key
// holding members.write can issue an invitation.
//
// Both halves are asserted. The map is the only mechanism that makes a scope
// session-only, so its absence is the decision; and a live request through the
// bearer path is what proves the decision is in effect rather than merely
// written down.
func TestMembersWriteIsDelegableToAnAPIKey(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})
	if _, blocked := auth.NonDelegableScopes[invite.PermWrite]; blocked {
		t.Fatal("members.write is in NonDelegableScopes; D28 concluded it stays delegable")
	}

	key, err := f.keys.Create(t.Context(), f.owner, auth.CreateAPIKeyInput{
		Name: "inviter", Scopes: []string{invite.PermWrite},
	})
	if err != nil {
		t.Fatalf("mint a key with members.write: %v", err)
	}

	srv := f.serve(t, config.SignupInvite)
	status, body := postJSONAs(t, srv, "/api/v1/invitations", key.Key, map[string]string{
		"email": "by-key@example.com", "role": "viewer",
	})
	if status != http.StatusCreated {
		t.Fatalf("an API key holding members.write could not invite: %d\n%s", status, body)
	}

	// The capability itself, unchanged: a key may still bring collaborators in.
	// What bounds it is not the D28 ceiling — that ceiling is the *creator's*
	// rank and it turned out to bound nothing worth bounding (F29) — but D43's
	// absolute cap, which the test below walks the whole chain to hold.
	if n := f.scalar(t,
		`SELECT count(*) FROM invitations WHERE email = 'by-key@example.com'`); n != 1 {
		t.Error("the invitation was not written")
	}
}

// F29's chain, walked end to end: a key holding one delegable scope must not be
// able to manufacture an interactive principal holding scopes no key may hold.
// D43 caps a key-issued invitation at editor, absolutely rather than relative to
// whoever created the key.
//
// Every link is attempted at every built-in role — mint the key, invite, read
// the raw link out of the 201 the key itself received, redeem it, resolve the
// account that produced — and the assertion is on what the chain *produces*
// rather than on where it stops. So this cannot pass because a refusal's
// wording or status changed, and it stays red against a bound one rank lower:
// admin holds apikeys.write, audit.read and members.write.
func TestAKeyIssuedInvitationCannotReachANonDelegableScope(t *testing.T) {
	f := newInviteFixture(t, inviteOptions{NewAccounts: true})
	srv := f.serve(t, config.SignupInvite)

	key, err := f.keys.Create(t.Context(), f.owner, auth.CreateAPIKeyInput{
		Name: "inviter", Scopes: []string{invite.PermWrite},
	})
	if err != nil {
		t.Fatalf("mint a key with members.write: %v", err)
	}

	issued := 0
	for _, role := range []string{"owner", "admin", "editor", "viewer"} {
		email := "chain-" + role + "@example.com"
		status, body := postJSONAs(t, srv, "/api/v1/invitations", key.Key, map[string]string{
			"email": email, "role": role,
		})
		if status != http.StatusCreated {
			// The chain is closed at its first link for this role, so there is
			// no link to redeem and nothing was created to check.
			continue
		}
		issued++

		var created invite.Created
		if err := json.Unmarshal([]byte(body), &created); err != nil {
			t.Fatalf("read the invitation out of its own 201: %v\n%s", err, body)
		}
		if _, err := f.redeem(t, tokenOf(t, &created), email, invitePassword); err != nil {
			t.Fatalf("a key-issued %s invitation was not redeemable: %v", role, err)
		}
		joined, err := f.auth.IdentityForEmail(t.Context(), email)
		if err != nil {
			t.Fatalf("identity for the account the key produced: %v", err)
		}
		if joined.IsAPIKey() {
			t.Fatal("the redeemed principal is a key, so the escalation under test does not exist here")
		}
		for scope := range auth.NonDelegableScopes {
			if joined.Can(scope) {
				t.Errorf("a key scoped to %s alone invited at %s, and the account it minted holds %s — "+
					"a scope no key may hold, reachable now with no key at all", invite.PermWrite, role, scope)
			}
		}
	}

	// The capability D28 was right to want, kept. A fix that refused every
	// key-issued invitation would satisfy the loop above and take it away.
	if issued == 0 {
		t.Error("the key issued no invitation at any role; under D43 members.write stays delegable")
	}
}

// ─── small helpers ──────────────────────────────────────────────────────────

func postJSON(t *testing.T, srv *httptest.Server, path string, body map[string]string) (int, string) {
	t.Helper()
	return postJSONAs(t, srv, path, "", body)
}

func postJSONAs(t *testing.T, srv *httptest.Server, path, bearer string, body map[string]string) (int, string) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}
