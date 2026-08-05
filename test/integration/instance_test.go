//go:build integration

package integration

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/instance"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/team"
)

// The instance-level principal (M45, D98), and the three findings it closes.
//
// D38 recorded that this product had no principal, so instance-wide reach was
// guarded by permissions granted to the *owner role* — which is per-organization
// and which every self-registered account holds over its own. This file asserts
// the replacement from the outside: who reaches what, who may hand it on, and
// where an act that belongs to no tenant is recorded.

const instancePassword = "a-sufficiently-long-password"

type instanceFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	auth  *auth.Service
	audit *audit.Service
	svc   *instance.Service
	// principal is the account that claimed the instance, which is the only
	// thing that makes an account the principal.
	principal *auth.Identity
}

func newInstanceFixture(t *testing.T) *instanceFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	principal, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "founder@example.com", Name: "Founder", Password: instancePassword,
		IsFirstUser: true,
	})
	if err != nil {
		t.Fatalf("claim the instance: %v", err)
	}

	auditSvc := audit.NewService(pool)
	return &instanceFixture{
		t: t, pool: pool, auth: authSvc, audit: auditSvc,
		svc:       instance.NewService(pool, instance.Config{Audit: auditSvc}),
		principal: principal,
	}
}

// register makes an ordinary self-serve account, which provisions an
// organization and makes them its owner — the population F15 is about.
func (f *instanceFixture) register(email string) *auth.Identity {
	f.t.Helper()
	id, err := f.auth.Register(f.t.Context(), auth.RegisterInput{
		Email: email, Name: email, Password: instancePassword,
	})
	if err != nil {
		f.t.Fatalf("register %s: %v", email, err)
	}
	if id.Role != "owner" {
		f.t.Fatalf("%s registered as %q; F15 is about owners, so this fixture is "+
			"asserting nothing unless they are one", email, id.Role)
	}
	return id
}

func (f *instanceFixture) identity(email string) *auth.Identity {
	f.t.Helper()
	id, err := f.auth.IdentityForEmail(f.t.Context(), email)
	if err != nil {
		f.t.Fatalf("identity for %s: %v", email, err)
	}
	return id
}

func (f *instanceFixture) count(sql string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.pool.QueryRow(f.t.Context(), sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// TestNoOrganizationRoleCarriesInstanceWideReach is the root of F15, asserted
// against the schema rather than against an identity.
//
// Every instance-wide permission has to be held by named people. A role grant is
// the defect itself: `owner` is per-organization, registration provisions every
// account one it owns, and `LINKCTRL_SIGNUP_MODE=open` therefore turns any role
// grant here into "everybody who signed up".
//
// It reads role_permissions directly because that is where a future migration
// would put one back, and the failure this guards against is a later milestone
// re-granting one of these the way 01600 granted destinations.review.
func TestNoOrganizationRoleCarriesInstanceWideReach(t *testing.T) {
	f := newInstanceFixture(t)

	for _, slug := range auth.InstancePrincipalScopes {
		n := f.count(`SELECT count(*) FROM role_permissions rp
		                JOIN permissions p ON p.id = rp.permission_id
		               WHERE p.slug = $1`, slug)
		if n != 0 {
			t.Errorf("%s is granted to %d role(s). Roles are per-organization and "+
				"registration provisions every account one it owns, so a role grant "+
				"here hands instance-wide reach to every registrant — which is F15.",
				slug, n)
		}
	}

	// And the permissions themselves exist, so the loop above cannot pass by
	// asserting nothing about slugs the migration never inserted.
	for _, slug := range auth.InstancePrincipalScopes {
		if n := f.count(`SELECT count(*) FROM permissions WHERE slug = $1`, slug); n != 1 {
			t.Errorf("permission %s exists %d times, want exactly one", slug, n)
		}
	}
}

// TestClaimingTheInstanceConfersThePrincipalAndRegisteringDoesNot.
//
// Where the principal comes from, both halves. The setup flow confers it in the
// transaction that creates the account, because that is the one place in this
// product where "this account is the instance's" is established rather than
// assumed — it holds an advisory lock and re-counts users, so exactly one
// account can ever take it.
//
// Ordinary registration confers nothing, and that is the half worth asserting:
// conferring there would rebuild F15 in one line, and on an open instance a
// stranger would become the moderator of every organization's destinations by
// filling in a form.
func TestClaimingTheInstanceConfersThePrincipalAndRegisteringDoesNot(t *testing.T) {
	f := newInstanceFixture(t)

	for _, slug := range auth.InstancePrincipalScopes {
		if !f.principal.Can(slug) {
			t.Errorf("the account that claimed the instance does not hold %s", slug)
		}
	}

	stranger := f.register("stranger@example.com")
	for _, slug := range auth.InstancePrincipalScopes {
		if stranger.Can(slug) {
			t.Errorf("a self-registered owner holds %s. That is F15 rebuilt: "+
				"registration provisions an organization and makes the registrant "+
				"its owner, so on an open instance this is one form away.", slug)
		}
	}

	// A second claim is refused, so the principal cannot be acquired by racing
	// the setup page on an instance that is already up.
	if _, err := f.auth.Register(t.Context(), auth.RegisterInput{
		Email: "second-founder@example.com", Name: "Second", Password: instancePassword,
		IsFirstUser: true,
	}); err == nil {
		t.Fatal("a second account claimed the instance")
	}
	if n := f.count(`SELECT count(*) FROM instance_grants ig
	                   JOIN permissions p ON p.id = ig.permission_id
	                  WHERE p.slug = $1`, auth.PermInstanceAdmin); n != 1 {
		t.Errorf("%d accounts hold instance.admin, want exactly the one that "+
			"claimed the instance", n)
	}
}

// TestThePrincipalDelegatesReviewAndADelegateCannotDelegateOnwards is D98's
// delegation bound, which is the constraint that came with the owner's choice:
// *"only the instance-owner level may delegate"*.
//
// Stated as: the principal may grant instance-level review, and a holder of it
// may not. Without the second half the permission spreads without bound — the
// first delegatee appoints the next — and the property the constraint exists to
// protect is gone in two hops.
//
// The mechanism is structural rather than a check: instance.admin is not in
// auth.InstanceGrantable, so there is no path by which a grant produces another
// grantor. This drives the real service, so it asserts the outcome that
// structure is supposed to produce.
func TestThePrincipalDelegatesReviewAndADelegateCannotDelegateOnwards(t *testing.T) {
	f := newInstanceFixture(t)
	f.register("reviewer@example.com")
	f.register("outsider@example.com")

	if _, err := f.svc.GrantReviewer(t.Context(), f.principal, "reviewer@example.com"); err != nil {
		t.Fatalf("the principal could not appoint a reviewer: %v", err)
	}

	reviewer := f.identity("reviewer@example.com")
	for _, slug := range []string{auth.PermDestinationsReview, auth.PermDestinationsDecide} {
		if !reviewer.Can(slug) {
			t.Errorf("the appointed reviewer does not hold %s", slug)
		}
	}

	// The bound, in both directions.
	if reviewer.Can(auth.PermInstanceAdmin) {
		t.Fatal("an appointed reviewer holds instance.admin. The grant conferred the " +
			"ability to appoint the next reviewer, which is exactly the unbounded " +
			"spread D98's second constraint exists to prevent.")
	}
	if _, err := f.svc.GrantReviewer(t.Context(), reviewer, "outsider@example.com"); !errors.Is(
		err, domain.ErrForbidden) {
		t.Errorf("a reviewer appointing another reviewer returned %v, want forbidden", err)
	}
	if _, err := f.svc.Reviewers(t.Context(), reviewer); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a reviewer reading the roster returned %v, want forbidden. Who else "+
			"administers the instance is the principal's business; a reviewer needs "+
			"the queue rather than the roster.", err)
	}

	// The audit surface is the principal's alone. D98 enumerates the scopes and
	// gives the instance log to the principal, not to "instance-level review".
	if reviewer.Can(audit.PermReadInstance) {
		t.Error("an appointed reviewer holds audit.read.instance. That permission " +
			"ties an ip_prefix to a named actor, and D98 confers it on the " +
			"principal rather than on the delegated review.")
	}

	// Withdrawal, and what it cannot reach.
	if err := f.svc.RevokeReviewer(t.Context(), f.principal, reviewer.UserID); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	after := f.identity("reviewer@example.com")
	for _, slug := range []string{auth.PermDestinationsReview, auth.PermDestinationsDecide} {
		if after.Can(slug) {
			t.Errorf("a withdrawn reviewer still holds %s", slug)
		}
	}
	if after.Role != "owner" {
		t.Errorf("withdrawing instance review changed their organization role to %q; "+
			"it must touch nothing inside any organization", after.Role)
	}

	// The principal cannot be withdrawn through the surface that appoints, so
	// the instance can never be left with nobody able to appoint anybody.
	if err := f.svc.RevokeReviewer(t.Context(), f.principal, f.principal.UserID); err != nil {
		t.Fatalf("withdrawing from the principal: %v", err)
	}
	if !f.identity("founder@example.com").Can(auth.PermInstanceAdmin) {
		t.Fatal("the principal withdrew instance.admin from itself. Nothing could " +
			"then appoint anybody, and the dispute queue would be stranded exactly " +
			"as it was before the principal existed.")
	}
}

// TestTheOperatorCanMoveThePrincipalAndTheSetNeverGrows is F140.
//
// D98 leaves the principal conferrable at setup and nowhere else, `instance.admin`
// out of `InstanceGrantable` so no holder can mint another, and nothing in the
// product deleting a `users` row. What that leaves reachable is an operator losing
// the *account*: a forgotten password with no mailer, or a colleague who has left.
// F141 established there is no account recovery in this product at all, so the
// principal's password and the principal are one thing to lose, and the only
// repair was `psql`.
//
// The claim under test is that the repair is now a command **and is still a
// move**. The set of accounts that may appoint reviewers is one before and one
// after; anything else would defeat the bound in the same breath as closing the
// gap, since a second principal appoints a third.
//
// It drives the service rather than the CLI because the CLI is a flag parser over
// this call — what it adds is the production guard, which is `lctl seed`'s and
// asserted where that one is.
func TestTheOperatorCanMoveThePrincipalAndTheSetNeverGrows(t *testing.T) {
	f := newInstanceFixture(t)
	f.register("successor@example.com")
	f.register("reviewer@example.com")

	// A reviewer the outgoing principal appointed. What happens to them is half
	// the claim: they were given the queue, not the box, and a handover of the box
	// must not take the queue off them.
	if _, err := f.svc.GrantReviewer(t.Context(), f.principal, "reviewer@example.com"); err != nil {
		t.Fatalf("appoint a reviewer: %v", err)
	}

	before, err := f.svc.Principals(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].Email != "founder@example.com" {
		t.Fatalf("the instance starts with %d principals, want the founder alone; "+
			"this test asserts nothing about a move otherwise", len(before))
	}

	moved, err := f.svc.MovePrincipal(t.Context(), "successor@example.com")
	if err != nil {
		t.Fatalf("move the principal: %v", err)
	}
	if moved.To.Email != "successor@example.com" {
		t.Errorf("the move reports %s as the new principal", moved.To.Email)
	}
	if len(moved.From) != 1 || moved.From[0].Email != "founder@example.com" {
		t.Errorf("the move reports %v as having lost it; who lost it is the question "+
			"an operator asks afterwards", moved.From)
	}

	successor := f.identity("successor@example.com")
	for _, slug := range auth.InstancePrincipalScopes {
		if !successor.Can(slug) {
			t.Errorf("the new principal does not hold %s. The principal is the four "+
				"together — handing over three leaves somebody who can appoint "+
				"reviewers and cannot see the queue they are appointed to.", slug)
		}
	}
	founder := f.identity("founder@example.com")
	for _, slug := range auth.InstancePrincipalScopes {
		if founder.Can(slug) {
			t.Errorf("the outgoing principal still holds %s. A move that only adds is "+
				"the operation D98 forbids: two accounts able to appoint reviewers is "+
				"the unbounded spread the delegation bound exists to stop.", slug)
		}
	}

	// The bound, read back out of the table rather than off the return value.
	holders := f.count(
		`SELECT count(*) FROM instance_grants ig JOIN permissions p ON p.id = ig.permission_id
		  WHERE p.slug = $1`, auth.PermInstanceAdmin)
	if holders != 1 {
		t.Fatalf("%d accounts hold %s after a move, want exactly 1",
			holders, auth.PermInstanceAdmin)
	}

	reviewer := f.identity("reviewer@example.com")
	if !reviewer.Can(auth.PermDestinationsReview) || !reviewer.Can(auth.PermDestinationsDecide) {
		t.Error("the appointed reviewer lost the queue when the box changed hands. " +
			"Moving the principal is a handover of who appoints, not a purge of who " +
			"was appointed.")
	}
	if founder.Role != "owner" {
		t.Errorf("the outgoing principal's organization role became %q; a move must "+
			"touch nothing inside any organization", founder.Role)
	}

	// The record. Instance-wide because it decides who administers the box, and
	// actor `system` because nobody signed in to do it — the authority was the
	// shell, and naming either end of the transfer as the actor would put an
	// address in the column that answers *who did this* for somebody who did not.
	var label string
	var actorID, orgID *uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT actor_label, actor_user_id, organization_id FROM audit_logs
		  WHERE action = $1`, instance.ActionPrincipalMoved).
		Scan(&label, &actorID, &orgID); err != nil {
		t.Fatalf("no audit record for the move: %v. Going through the service rather "+
			"than the database is half the point of building the command.", err)
	}
	if label != "system" || actorID != nil {
		t.Errorf("the record names actor %q/%v; nobody in the product performed this",
			label, actorID)
	}
	if orgID != nil {
		t.Error("the move is filed under an organization. It decides who administers " +
			"every organization on the instance and belongs to none of them.")
	}
}

// TestMovingThePrincipalIsIdempotentAndRefusesAnAddressNobodyHas.
//
// Both halves are about an operator who is repairing something at a shell with
// incomplete information, which is the only situation this command is ever used
// in. Re-running it must not read as a refusal, and a typo must not leave the
// instance in a state somebody has to diagnose.
func TestMovingThePrincipalIsIdempotentAndRefusesAnAddressNobodyHas(t *testing.T) {
	f := newInstanceFixture(t)

	if _, err := f.svc.MovePrincipal(t.Context(), "founder@example.com"); err != nil {
		t.Fatalf("moving the principal onto the account that already holds it: %v. "+
			"Two administrators doing the same obvious thing must not produce "+
			"something that reads like a refusal.", err)
	}
	if !f.identity("founder@example.com").Can(auth.PermInstanceAdmin) {
		t.Fatal("re-running the move took the principal off the account it was " +
			"moving it onto")
	}

	var ve domain.ValidationErrors
	_, err := f.svc.MovePrincipal(t.Context(), "nobody@example.com")
	if !errors.As(err, &ve) || ve[0].Code != "unknown" {
		t.Errorf("moving onto an address nobody has returned %v; it must be a "+
			"validation refusal naming the field, not a server error", err)
	}
	holders := f.count(
		`SELECT count(*) FROM instance_grants ig JOIN permissions p ON p.id = ig.permission_id
		  WHERE p.slug = $1`, auth.PermInstanceAdmin)
	if holders != 1 {
		t.Errorf("%d accounts hold %s after a refused move, want the 1 that held it "+
			"before", holders, auth.PermInstanceAdmin)
	}
	if n := f.count(`SELECT count(*) FROM audit_logs WHERE action = $1`,
		instance.ActionPrincipalMoved); n != 1 {
		t.Errorf("%d audit records for two calls, one of which was refused and one of "+
			"which changed nothing observable; want the 1 the successful call wrote", n)
	}
}

// TestAnInstanceGrantSurvivesLosingEveryOrganization.
//
// D36 made "belongs to nothing" a state a signed-in account can be in, and every
// membership-derived permission is gone there by construction. An instance grant
// is held over the box rather than through a tenancy, so it must not be — the
// alternative is that one tenancy teardown silently strands the dispute queue,
// which is a sharper version of the finding the principal exists to close.
func TestAnInstanceGrantSurvivesLosingEveryOrganization(t *testing.T) {
	f := newInstanceFixture(t)

	if _, err := f.pool.Exec(t.Context(),
		`DELETE FROM memberships WHERE user_id = $1`, f.principal.UserID); err != nil {
		t.Fatalf("strip memberships: %v", err)
	}

	orphan := f.identity("founder@example.com")
	if orphan.HasOrganization() {
		t.Fatal("the account still belongs to an organization; this test asserts " +
			"nothing unless it does not")
	}
	if orphan.Can("links.create") {
		t.Fatal("a membership-derived permission survived losing every membership; " +
			"the fixture is not in the state it claims to be in")
	}
	for _, slug := range auth.InstancePrincipalScopes {
		if !orphan.Can(slug) {
			t.Errorf("%s was lost with the last membership. An instance grant is "+
				"not reached through a tenancy, so a tenancy teardown must not "+
				"revoke it.", slug)
		}
	}
}

// TestAnInstanceWideActIsRecordedAgainstNoTenant is F36.
//
// An act that changes every organization was recorded in exactly one of them —
// whichever the actor happened to be standing in — where the tenants it changed
// could not see it and the tenant it landed in had no claim to it. Both limbs
// the finding names are asserted: the default domain's settings and a dispute
// decision.
//
// The column was always nullable and the product already used a NULL
// organization to mean instance-wide for the default `domains` row; what was
// missing was somebody who could read the result, which is what D98 supplies.
func TestAnInstanceWideActIsRecordedAgainstNoTenant(t *testing.T) {
	f := newInstanceFixture(t)

	links := link.NewService(f.pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: "http://lnk.test",
		SplitHosts: true, Audit: f.audit,
	})
	if _, err := links.SetBotBlocking(t.Context(), f.principal, true, true); err != nil {
		t.Fatalf("set bot blocking: %v", err)
	}

	orgRows := f.count(
		`SELECT count(*) FROM audit_logs
		  WHERE action = 'domain.bot_blocking_changed' AND organization_id IS NOT NULL`)
	if orgRows != 0 {
		t.Errorf("%d instance-wide bot-blocking changes are filed under an "+
			"organization. The setting reaches every domain row on the box, so it "+
			"governs every link in every organization and belongs to none of them.",
			orgRows)
	}
	instanceRows := f.count(
		`SELECT count(*) FROM audit_logs
		  WHERE action = 'domain.bot_blocking_changed' AND organization_id IS NULL`)
	if instanceRows != 1 {
		t.Fatalf("%d instance-wide bot-blocking records, want 1. Marking the event "+
			"must move it rather than drop it — a repair that deleted the record "+
			"would be worse than the misattribution.", instanceRows)
	}

	// The actor is still recorded in full. What moved is the tenancy, not the
	// accountability: "who did it" is the question this table answers.
	var label string
	var actorID *uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT actor_label, actor_user_id FROM audit_logs
		  WHERE action = 'domain.bot_blocking_changed'`).Scan(&label, &actorID); err != nil {
		t.Fatal(err)
	}
	if label != "founder@example.com" || actorID == nil || *actorID != f.principal.UserID {
		t.Errorf("the instance-wide record names actor %q/%v, want the account that "+
			"made the change", label, actorID)
	}

	// And it is readable, by the principal and by nobody else.
	page, err := f.audit.ListInstance(t.Context(), f.principal, audit.Filter{})
	if err != nil {
		t.Fatalf("ListInstance for the principal: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Action != "domain.bot_blocking_changed" {
		t.Fatalf("the instance audit surface holds %d rows, want the one that was "+
			"written", len(page.Items))
	}

	stranger := f.register("stranger@example.com")
	if _, err := f.audit.ListInstance(t.Context(), stranger, audit.Filter{}); !errors.Is(
		err, domain.ErrForbidden) {
		t.Errorf("ListInstance for a self-registered owner returned %v, want forbidden", err)
	}
	// The organization log no longer carries it, which is the other half of the
	// repair: it used to be there, under a tenant with no claim to it.
	orgPage, err := f.audit.List(t.Context(), f.principal, audit.Filter{})
	if err != nil {
		t.Fatalf("List for the principal: %v", err)
	}
	for _, e := range orgPage.Items {
		if e.Action == "domain.bot_blocking_changed" {
			t.Error("an instance-wide act is still in an organization's audit log")
		}
	}
}

// TestAWorkspaceScopedAdminReadsOnlyTheirOwnWorkspacesAuditRows is F31.
//
// M21 scoped the audit log by organization and argued for it in the query: a log
// that narrowed itself to the workspace the reader happens to be *acting in*
// would hide exactly the actions worth reviewing. That argument still holds, and
// it was written when audit.read could only be held organization-wide. M28 made
// a membership scopable to one workspace, and nothing revisited the read.
//
// The repair is D44's rule reaching a read: authority is per scope, and only an
// organization-wide membership reaches the organization-wide scope. So an
// org-wide reader is unaffected, and a workspace-scoped one sees their own
// workspaces.
//
// The fixture is the finding's own: an account whose only membership is
// `role=admin, workspace_id=A`, which resolves audit.read in A and nothing in C,
// and which read C's rows anyway.
func TestAWorkspaceScopedAdminReadsOnlyTheirOwnWorkspacesAuditRows(t *testing.T) {
	f := newTeamFixture(t)
	auditSvc := audit.NewService(f.pool)
	ctx := t.Context()

	second, err := f.team.CreateWorkspace(ctx, f.owner, "Second")
	if err != nil {
		t.Fatalf("create the second workspace: %v", err)
	}

	// Alice arrives by invitation — which is the only way this product makes a
	// member — and is then granted into the owner's first workspace and stripped
	// of the organization-wide row the invitation created.
	alice := f.member(t, "alice@example.com", "admin")
	if _, err := f.team.Grant(ctx, f.owner, team.GrantInput{
		UserID: alice.UserID, WorkspaceID: f.owner.WorkspaceID, Role: "admin",
	}); err != nil {
		t.Fatalf("grant alice into the first workspace: %v", err)
	}
	if err := f.team.Remove(ctx, f.owner, f.membershipOf(t, f.owner, "alice@example.com")); err != nil {
		t.Fatalf("remove alice's organization-wide membership: %v", err)
	}

	// One record in each workspace, written through the real recorder so the
	// tenancy columns come from an identity rather than from an INSERT.
	if err := auditSvc.Record(ctx, f.owner, audit.Event{
		Action: "link.created", TargetType: "link",
	}); err != nil {
		t.Fatalf("record in the first workspace: %v", err)
	}
	inSecond := f.identityIn(t, "owner@example.com", second.ID)
	if err := auditSvc.Record(ctx, inSecond, audit.Event{
		Action: "workspace.renamed", TargetType: "workspace",
	}); err != nil {
		t.Fatalf("record in the second workspace: %v", err)
	}

	// The controls, first, or the assertion below passes for the wrong reason.
	//
	// Alice must hold exactly one membership and it must be scoped to the first
	// workspace — the shape F31 describes, and the one that used to read the
	// whole organization. She holds nothing in the second workspace at all: she
	// cannot even resolve into it, because resolution requires a membership that
	// reaches it, which is the same predicate the permission query applies.
	if n := f.count(t, `SELECT count(*) FROM memberships
	                     WHERE user_id = $1 AND workspace_id IS NULL`,
		alice.UserID); n != 0 {
		t.Fatalf("alice holds %d organization-wide membership(s); F31 is about "+
			"somebody who holds none", n)
	}
	if n := f.count(t, `SELECT count(*) FROM memberships
	                     WHERE user_id = $1 AND workspace_id = $2`,
		alice.UserID, f.owner.WorkspaceID); n != 1 {
		t.Fatalf("alice holds %d workspace-scoped membership(s) in the first "+
			"workspace, want 1", n)
	}
	alice = f.identityIn(t, "alice@example.com", f.owner.WorkspaceID)
	if !alice.Can(audit.PermRead) {
		t.Fatal("alice does not hold audit.read in the workspace she was granted " +
			"into; this test would then be asserting nothing")
	}

	page, err := auditSvc.List(ctx, alice, audit.Filter{})
	if err != nil {
		t.Fatalf("List for a workspace-scoped admin: %v", err)
	}
	for _, e := range page.Items {
		if e.Action == "workspace.renamed" {
			t.Error("a workspace-scoped admin read a record from a workspace they " +
				"hold no membership in. What is disclosed is an ip_prefix bound to " +
				"a named actor and a per-action timeline, for workspaces they do " +
				"not administer (F31).")
		}
	}
	if len(page.Items) == 0 {
		t.Error("a workspace-scoped admin reads nothing at all. The repair narrows " +
			"the log to the scopes their own authority covers; narrowing it to " +
			"nothing would be a different defect.")
	}

	// The organization-wide reader is untouched, which is the half that keeps
	// M21's argument true: an audit log narrowed to where somebody is standing
	// would hide the actions worth reviewing.
	ownerPage, err := auditSvc.List(ctx, f.owner, audit.Filter{})
	if err != nil {
		t.Fatalf("List for the owner: %v", err)
	}
	var sawSecond bool
	for _, e := range ownerPage.Items {
		if e.Action == "workspace.renamed" {
			sawSecond = true
		}
	}
	if !sawSecond {
		t.Error("an organization-wide reader lost sight of another workspace's " +
			"records. Only a workspace-scoped membership is bounded here.")
	}
}
