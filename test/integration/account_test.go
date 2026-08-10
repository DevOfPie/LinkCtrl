//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/account"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/instance"
	"github.com/DevOfPie/LinkCtrl/internal/invite"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/team"
)

// Account deletion and subject erasure (M52), the milestone that closes F44 —
// *there is no account deletion or erasure in this product at all, while the
// schema and four other sites describe both as behaviour that exists*.
//
// Asserted here rather than in internal/account's own package because every
// claim the milestone makes is about rows: which ones stop existing, which ones
// keep existing with the person taken out of them, and which ones a soft delete
// leaves untouched precisely because they belong to somebody else. A unit test
// with a fake store would be asserting the fake — and the specific thing most
// likely to be wrong here is a cascade that does not fire, which only a database
// can tell you.

const accountPassword = "a-sufficiently-long-password"

type accountFixture struct {
	pool     *pgxpool.Pool
	auth     *auth.Service
	keys     *auth.APIKeyService
	invites  *invite.Service
	team     *team.Service
	notify   *notify.Service
	instance *instance.Service
	svc      *account.Service
	// owner claimed the instance, so it is the instance principal (D98) and the
	// sole owner of the personal organization registration provisioned for it.
	// Both are refusals, which makes it the wrong account for most of the tests
	// below and exactly the right one for two of them.
	owner *auth.Identity
}

func newAccounts(t *testing.T) *accountFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	// IsFirstUser, so the instance is *claimed*: Register confers every scope in
	// auth.InstancePrincipalScopes in the same transaction. Without it there is
	// no principal on this instance and the refusal that names one could not be
	// provoked at all.
	owner, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner",
		Password: accountPassword, IsFirstUser: true,
	})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}

	auditSvc := audit.NewService(pool)
	keySvc, err := auth.NewAPIKeyService(pool, authSvc, auth.APIKeyConfig{
		Pepper: testPepper, Auditor: auditSvc,
	})
	if err != nil {
		t.Fatal(err)
	}
	inviteSvc, err := invite.NewService(pool, invite.Config{
		AppURL:      "https://links.example.com",
		TTL:         168 * time.Hour,
		NewAccounts: true,
		Hasher:      authSvc.Hasher(),
		Audit:       auditSvc,
		Notify:      notify.NewService(pool),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := account.NewService(pool, account.Config{Auth: authSvc, Audit: auditSvc})
	if err != nil {
		t.Fatal(err)
	}

	return &accountFixture{
		pool: pool, auth: authSvc, keys: keySvc, invites: inviteSvc,
		team:     team.NewService(pool, team.Config{Audit: auditSvc}),
		notify:   notify.NewService(pool),
		instance: instance.NewService(pool, instance.Config{Audit: auditSvc}),
		svc:      svc, owner: owner,
	}
}

// joined admits somebody by invitation and returns their identity.
//
// **Redemption, not registration, and the difference is the whole reason this
// helper exists.** Redemption creates a membership and nothing else (D6): no
// personal organization, so the account is the sole owner of nothing and the
// deletion under test is not refused before it starts. `auth.Register` would
// provision an organization this account owns alone, which is a different test —
// and is the one two functions below.
func (f *accountFixture) joined(t *testing.T, email, role string) *auth.Identity {
	t.Helper()
	created, err := f.invites.Create(t.Context(), f.owner, invite.CreateInput{Email: email, Role: role})
	if err != nil {
		t.Fatalf("invite %s as %s: %v", email, role, err)
	}
	const prefix = "https://links.example.com/invite/"
	if _, err := f.invites.Redeem(t.Context(), invite.RedeemInput{
		Token: strings.TrimPrefix(created.URL, prefix), Email: email, Password: accountPassword,
	}); err != nil {
		t.Fatalf("redeem for %s: %v", email, err)
	}
	id, err := f.auth.IdentityForEmail(t.Context(), email)
	if err != nil {
		t.Fatalf("identity for %s: %v", email, err)
	}
	return id
}

// count is one number out of the database, for the before-and-after assertions.
func (f *accountFixture) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// ─── What deletion removes ───────────────────────────────────────────────────

// The four tables m52.md enumerates, plus the two its enumeration missed, all
// counted before and after for one account holding at least one of each.
//
// **Counted rather than trusted, because none of it happens by cascade.** All
// six declare `ON DELETE CASCADE` against `users` and every one of those clauses
// fires on `DELETE`; the account row is kept, so the deletion path removes them
// with statements of its own. That is the single most plausible way this
// milestone could be wrong — write the soft delete, assume the schema does the
// rest, and ship an account whose sessions still resolve.
func TestDeletingAnAccountRemovesEverythingThatGrantsAccess(t *testing.T) {
	f := newAccounts(t)
	ctx := t.Context()
	member := f.joined(t, "member@example.com", "admin")

	// A session, from the login path rather than an insert, so the row is the
	// shape the product makes.
	if _, err := f.auth.Login(ctx, auth.LoginInput{
		Email: "member@example.com", Password: accountPassword,
	}); err != nil {
		t.Fatalf("sign in as the member: %v", err)
	}
	// A key, from the service that mints one.
	if _, err := f.keys.Create(ctx, member, auth.CreateAPIKeyInput{
		Name: "ci", Scopes: []string{"links.read"},
	}); err != nil {
		t.Fatalf("create a key for the member: %v", err)
	}
	// A notification, an outstanding reset token, and an instance-level grant.
	// The reset row is written directly: recovery.Service needs a mailer to
	// issue one, and what is under test is that the row goes, not how it arrived.
	if err := f.notify.Notify(ctx, member.UserID, notify.Event{
		Kind: "audit.growth", Title: "Something happened",
	}); err != nil {
		t.Fatalf("notify the member: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO password_resets (id, user_id, token_hash, expires_at)
		 VALUES ($1, $2, $3, now() + interval '1 hour')`,
		uuid.Must(uuid.NewV7()), member.UserID, []byte("not-a-real-token-hash")); err != nil {
		t.Fatalf("write an outstanding reset for the member: %v", err)
	}
	if _, err := f.instance.GrantReviewer(ctx, f.owner, "member@example.com"); err != nil {
		t.Fatalf("make the member a dispute reviewer: %v", err)
	}

	for _, tc := range []struct{ what, query string }{
		{"memberships", `SELECT count(*) FROM memberships WHERE user_id = $1`},
		{"sessions", `SELECT count(*) FROM sessions WHERE user_id = $1`},
		{"api_keys", `SELECT count(*) FROM api_keys WHERE user_id = $1`},
		{"notifications", `SELECT count(*) FROM notifications WHERE user_id = $1`},
		{"password_resets", `SELECT count(*) FROM password_resets WHERE user_id = $1`},
		{"instance_grants", `SELECT count(*) FROM instance_grants WHERE user_id = $1`},
	} {
		if n := f.count(t, tc.query, member.UserID); n == 0 {
			t.Fatalf("the account holds no %s before deletion, so the assertion "+
				"below would pass without the deletion doing anything", tc.what)
		}
	}

	if err := f.svc.Delete(ctx, member, accountPassword); err != nil {
		t.Fatalf("delete the member's account: %v", err)
	}

	for _, tc := range []struct{ what, query string }{
		{"memberships", `SELECT count(*) FROM memberships WHERE user_id = $1`},
		{"sessions", `SELECT count(*) FROM sessions WHERE user_id = $1`},
		{"api_keys", `SELECT count(*) FROM api_keys WHERE user_id = $1`},
		{"notifications", `SELECT count(*) FROM notifications WHERE user_id = $1`},
		{"password_resets", `SELECT count(*) FROM password_resets WHERE user_id = $1`},
		{"instance_grants", `SELECT count(*) FROM instance_grants WHERE user_id = $1`},
	} {
		if n := f.count(t, tc.query, member.UserID); n != 0 {
			t.Errorf("%s still holds %d row(s) for the deleted account. A soft "+
				"delete fires no foreign key, so every one of these needs a "+
				"statement in DeleteAccountDependents", tc.what, n)
		}
	}

	// The row itself survives, scrubbed later. That is the difference between
	// deleted_at and anonymized_at and the fact the whole tombstone rests on.
	var status string
	var deletedAt, anonymizedAt *time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT status, deleted_at, anonymized_at FROM users WHERE id = $1`,
		member.UserID).Scan(&status, &deletedAt, &anonymizedAt); err != nil {
		t.Fatalf("the account row is gone; erasure would have nothing to mark: %v", err)
	}
	if status != "deleted" || deletedAt == nil {
		t.Errorf("account row reads status=%q deleted_at=%v; both are written together "+
			"or the row disagrees with itself", status, deletedAt)
	}
	if anonymizedAt != nil {
		t.Errorf("anonymized_at is already set at deletion time (%v). It marks the "+
			"row the erasure sweep has scrubbed, and the gap between the two is "+
			"the sweep's lag", anonymizedAt)
	}
}

// The partial index `users_email_key` has been shaped for this since the first
// migration and nothing had ever exercised it: the uniqueness it enforces is on
// `deleted_at IS NULL`, so a deleted account releases its address.
func TestADeletedAddressCanBeRegisteredAgain(t *testing.T) {
	f := newAccounts(t)
	ctx := t.Context()
	member := f.joined(t, "member@example.com", "admin")

	if _, err := f.auth.Register(ctx, auth.RegisterInput{
		Email: "member@example.com", Name: "Impostor", Password: accountPassword,
	}); !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("registering the live address answered %v, want ErrEmailTaken; "+
			"the assertion after the deletion would prove nothing", err)
	}

	if err := f.svc.Delete(ctx, member, accountPassword); err != nil {
		t.Fatalf("delete the member's account: %v", err)
	}

	reused, err := f.auth.Register(ctx, auth.RegisterInput{
		Email: "member@example.com", Name: "Somebody Else", Password: accountPassword,
	})
	if err != nil {
		t.Fatalf("the address is still taken after its account was deleted: %v", err)
	}
	if reused.UserID == member.UserID {
		t.Error("re-registering returned the same account id, so the address was " +
			"reattached rather than reused. They are different people")
	}
}

// ─── What deletion refuses ───────────────────────────────────────────────────

// Deleting the account that administers the box leaves no path back that does
// not involve SQL, and the refusal names the one command that repairs it.
func TestAccountDeletionRefusesTheInstancePrincipal(t *testing.T) {
	f := newAccounts(t)

	err := f.svc.Delete(t.Context(), f.owner, accountPassword)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("deleting the instance principal answered %v, want a conflict", err)
	}
	if !strings.Contains(err.Error(), "lctl instance principal move") {
		t.Errorf("the refusal is %q and does not name the route back. The whole "+
			"reason this is refused is that there is one", err)
	}

	// Refused *before* anything is written, not part-way through.
	if n := f.count(t,
		`SELECT count(*) FROM users WHERE id = $1 AND deleted_at IS NULL`,
		f.owner.UserID); n != 1 {
		t.Error("the principal's account was marked deleted by a call that refused")
	}
}

// M28.5 refuses removing an organization's last owner. This is the same rule
// from the other side, and without it the rule is bypassable by leaving through
// a different door.
func TestAccountDeletionRefusesTheSoleOwnerOfASurvivingOrganization(t *testing.T) {
	f := newAccounts(t)
	ctx := t.Context()

	// Registered rather than invited, so registration provisions the personal
	// organization this account owns alone. Not the first user, so not the
	// principal — otherwise the refusal under test is indistinguishable from the
	// one above.
	solo, err := f.auth.Register(ctx, auth.RegisterInput{
		Email: "solo@example.com", Name: "Solo", Password: accountPassword,
	})
	if err != nil {
		t.Fatalf("register solo: %v", err)
	}

	err = f.svc.Delete(ctx, solo, accountPassword)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("deleting a sole owner answered %v, want a conflict", err)
	}
	var soleOwner *account.SoleOwnerError
	if !errors.As(err, &soleOwner) {
		t.Fatalf("the refusal is %v, which does not carry the organizations "+
			"blocking it. Which ones is the only actionable part of it", err)
	}
	if len(soleOwner.Organizations) != 1 {
		t.Errorf("the refusal names %v; the account owns exactly one organization alone",
			soleOwner.Organizations)
	}

	// A second owner, and the same call goes through. Nothing about the account
	// changed — only the organization's owner count — which is what makes this
	// a rule about the organization rather than a property of the person.
	second := f.joined(t, "second@example.com", "viewer")
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO memberships (id, user_id, organization_id, role_id)
		 SELECT $1, $2, m.organization_id, m.role_id
		   FROM memberships m
		  WHERE m.user_id = $3 AND m.workspace_id IS NULL`,
		uuid.Must(uuid.NewV7()), second.UserID, solo.UserID); err != nil {
		t.Fatalf("make somebody else an owner of solo's organization: %v", err)
	}
	if err := f.svc.Delete(ctx, solo, accountPassword); err != nil {
		t.Fatalf("deleting a co-owner was still refused: %v", err)
	}
}

// D36 made belonging to nothing a state a signed-in account can legitimately be
// in, and handing over every organization you own alone is how somebody arrives
// at it. Refusing here would mean the only way to leave is to be the last person
// out.
func TestAccountDeletionAllowsAnAccountThatBelongsToNothing(t *testing.T) {
	f := newAccounts(t)
	ctx := t.Context()
	member := f.joined(t, "member@example.com", "viewer")

	if _, err := f.pool.Exec(ctx, `DELETE FROM memberships WHERE user_id = $1`,
		member.UserID); err != nil {
		t.Fatalf("strip the member's membership: %v", err)
	}
	orphan, err := f.auth.IdentityForEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatalf("resolve the orphaned account: %v", err)
	}
	if orphan.HasOrganization() {
		t.Fatal("the account still belongs to an organization, so this is not the state under test")
	}

	if err := f.svc.Delete(ctx, orphan, accountPassword); err != nil {
		t.Fatalf("deleting an account that belongs to nothing was refused: %v", err)
	}
}

// D87's limb: the subject is the person, not the credential. A leaked key must
// not be able to delete the account that owns it — and the confirmation is a
// password, which a key does not have.
func TestAccountDeletionRefusesAnAPIKeyAndAWrongPassword(t *testing.T) {
	f := newAccounts(t)
	ctx := t.Context()
	member := f.joined(t, "member@example.com", "admin")

	created, err := f.keys.Create(ctx, member, auth.CreateAPIKeyInput{
		Name: "ci", Scopes: []string{"links.read"},
	})
	if err != nil {
		t.Fatalf("create a key: %v", err)
	}
	asKey, err := f.keys.Authenticate(ctx, created.Key)
	if err != nil {
		t.Fatalf("authenticate with the key: %v", err)
	}
	if err := f.svc.Delete(ctx, asKey, accountPassword); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("deleting with an API key answered %v, want forbidden", err)
	}

	if err := f.svc.Delete(ctx, member, "not-the-right-password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("deleting with the wrong password answered %v, want invalid credentials", err)
	}
	if n := f.count(t,
		`SELECT count(*) FROM users WHERE id = $1 AND deleted_at IS NULL`,
		member.UserID); n != 1 {
		t.Error("a refused deletion still marked the account deleted")
	}
}

// ─── What survives, and what erasure does to it ──────────────────────────────

// erase runs the sweep the hourly pass runs.
func (f *accountFixture) erase(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	n, err := f.svc.ErasePending(ctx, 1000)
	if err != nil {
		t.Fatalf("erasure pass: %v", err)
	}
	return n
}

// The audit trail keeps its rows and loses the person, which is the whole design
// `audit_logs.actor_user_id` has carried no foreign key for since Phase 1.
//
// Two things are asserted together because they are in tension: the address has
// to be gone, and the entries have to stay correlated to each other. D148 settles
// how — a constant label, with the ids surviving, so correlation is
// `audit_logs_actor_idx` and nothing is derived from anything.
func TestErasureTombstonesTheActorAndKeepsTheEntriesCorrelated(t *testing.T) {
	f := newAccounts(t)
	ctx := t.Context()
	member := f.joined(t, "member@example.com", "admin")

	// Two records by this actor, so "still correlate" has something to be true
	// of. Both through the service, so they are the rows the product writes.
	for _, email := range []string{"one@example.com", "two@example.com"} {
		if _, err := f.invites.Create(ctx, member, invite.CreateInput{Email: email, Role: "viewer"}); err != nil {
			t.Fatalf("invite %s: %v", email, err)
		}
	}
	before := f.count(t, `SELECT count(*) FROM audit_logs WHERE actor_user_id = $1`, member.UserID)
	if before < 2 {
		t.Fatalf("the actor wrote %d audit records; the correlation assertion needs at least two", before)
	}

	if err := f.svc.Delete(ctx, member, accountPassword); err != nil {
		t.Fatalf("delete the member's account: %v", err)
	}
	if n := f.erase(t, ctx); n != 1 {
		t.Fatalf("the erasure pass took %d accounts, want 1", n)
	}

	after := f.count(t, `SELECT count(*) FROM audit_logs WHERE actor_user_id = $1`, member.UserID)
	if after != before+1 {
		t.Errorf("the actor has %d records after erasure and had %d before, plus the "+
			"account.deleted record written by the deletion itself. An audit trail "+
			"that loses rows with its actor is not an audit trail", after, before)
	}

	// The address is gone from every one of them.
	if n := f.count(t,
		`SELECT count(*) FROM audit_logs WHERE actor_user_id = $1 AND actor_label <> $2`,
		member.UserID, account.TombstoneLabel); n != 0 {
		t.Errorf("%d of the erased actor's records still carry a label that is not "+
			"the tombstone", n)
	}
	if n := f.count(t,
		`SELECT count(*) FROM audit_logs WHERE actor_label = 'member@example.com'`); n != 0 {
		t.Errorf("%d audit records still carry the erased address as their actor label", n)
	}

	// And the account row survives, scrubbed. `anonymized_at` marks it; the
	// address, the name and the password are gone from it.
	var email, name, status string
	var hash *string
	var anonymizedAt *time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT email, name, status, password_hash, anonymized_at FROM users WHERE id = $1`,
		member.UserID).Scan(&email, &name, &status, &hash, &anonymizedAt); err != nil {
		t.Fatalf("the erased account row is gone, so every id pointing at it now dangles: %v", err)
	}
	if email != "" || name != "" || hash != nil {
		t.Errorf("the erased row still reads email=%q name=%q password_hash set=%v",
			email, name, hash != nil)
	}
	if anonymizedAt == nil {
		t.Error("the row was scrubbed and anonymized_at was not set, so the sweep " +
			"will find it again on every run")
	}
	if status != "deleted" {
		t.Errorf("the erased row reads status=%q, want deleted", status)
	}
}

// Both address snapshots in `destination_disputes`, not one.
//
// The table nobody remembers: no foreign key, no purge, no retention setting,
// and an explicit design note saying it must outlive the account. It carries
// `created_by_label` for whoever filed a dispute and `decided_by_label` for
// whoever decided it, and an account is as identifiable as the moderator of a
// dispute as it is as the filer of one.
func TestErasureScrubsBothDisputeLabels(t *testing.T) {
	f := newAccounts(t)
	ctx := t.Context()
	filer := f.joined(t, "filer@example.com", "editor")
	decider := f.joined(t, "decider@example.com", "admin")

	// Written directly. Reaching these columns through the dispute service means
	// a blocked destination, a refused link write and a review decision, none of
	// which this milestone is about — and the claim under test is about the two
	// columns, which a row is a row either way.
	for _, d := range []struct {
		host              string
		createdBy         uuid.UUID
		createdByLabel    string
		decidedBy         uuid.UUID
		decidedByLabelSet string
	}{
		{"filed.example", filer.UserID, "filer@example.com", decider.UserID, "decider@example.com"},
	} {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO destination_disputes
			   (id, host, url_defanged, reason_code, status,
			    created_by, created_by_label, decided_by, decided_by_label, decided_at)
			 VALUES ($1, $2, 'hxxp://filed[.]example/', 'shortener.known', 'upheld',
			         $3, $4, $5, $6, now())`,
			uuid.Must(uuid.NewV7()), d.host,
			d.createdBy, d.createdByLabel, d.decidedBy, d.decidedByLabelSet); err != nil {
			t.Fatalf("write a decided dispute: %v", err)
		}
	}

	// Only the filer leaves. The decider stays, and their label has to stay with
	// them — an erasure that scrubbed both would be destroying a live person's
	// record, which is the direction this could fail in that nobody would notice.
	if err := f.svc.Delete(ctx, filer, accountPassword); err != nil {
		t.Fatalf("delete the filer's account: %v", err)
	}
	if n := f.erase(t, ctx); n != 1 {
		t.Fatalf("the erasure pass took %d accounts, want 1", n)
	}

	var createdLabel, decidedLabel, status string
	var createdBy, decidedBy *uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT created_by_label, decided_by_label, status, created_by, decided_by
		   FROM destination_disputes WHERE host = 'filed.example'`).
		Scan(&createdLabel, &decidedLabel, &status, &createdBy, &decidedBy); err != nil {
		t.Fatalf("the dispute is gone; it must outlive the account (01600:53-63): %v", err)
	}
	if createdLabel != account.TombstoneLabel {
		t.Errorf("created_by_label reads %q after the filer was erased", createdLabel)
	}
	if decidedLabel != "decider@example.com" {
		t.Errorf("decided_by_label reads %q; the decider is still here and their "+
			"record is not the filer's to erase", decidedLabel)
	}
	if status != "upheld" {
		t.Errorf("the dispute's outcome reads %q; erasure scrubs the person, not the decision", status)
	}
	if createdBy == nil || *createdBy != filer.UserID {
		t.Errorf("created_by is %v; D148 keeps the ids so the rows stay correlatable", createdBy)
	}

	// The other direction, with the roles reversed: the decider leaves and the
	// filer's label survives. Stated as its own assertion because one statement
	// scrubbing both columns and one scrubbing the wrong one look identical from
	// the half above.
	if err := f.svc.Delete(ctx, decider, accountPassword); err != nil {
		t.Fatalf("delete the decider's account: %v", err)
	}
	if n := f.erase(t, ctx); n != 1 {
		t.Fatalf("the second erasure pass took %d accounts, want 1", n)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT created_by_label, decided_by_label FROM destination_disputes WHERE host = 'filed.example'`).
		Scan(&createdLabel, &decidedLabel); err != nil {
		t.Fatalf("read the dispute again: %v", err)
	}
	if decidedLabel != account.TombstoneLabel {
		t.Errorf("decided_by_label reads %q after the decider was erased", decidedLabel)
	}
}

// F177 and F181 together, because they are one address surviving in two places
// the sweep did not reach and one pass reaches both.
//
// F181 is the one that needs no permission at all: `/invites` lists every
// invitation an organization ever issued, redeemed ones included, and renders
// the address each was sent to. So an account deleted, erased, tombstoned in the
// audit log and emptied in `users` was still named in full on an ordinary
// dashboard page, with nothing to expire it. F177 is the same address one column
// over from the label the sweep already scrubbed — inside `audit_logs.metadata`,
// where seven writers put it and where it usually describes the *subject* of
// somebody else's action rather than the actor.
//
// Two accounts, both admitted the same way, and only one leaves. Every
// assertion below has its mirror on the account that stayed, because "the
// address is gone" and "every address is gone" are indistinguishable from one
// side and the second is the failure nothing else here would catch.
func TestErasureReachesAuditMetadataAndRedeemedInvitations(t *testing.T) {
	f := newAccounts(t)
	ctx := t.Context()
	const leaving, staying = "member@example.com", "stayer@example.com"

	// Redemption, so each account arrives with the pair of records the finding
	// was reproduced on: the administrator's `invitation.created`, whose actor is
	// still here, and their own `invitation.redeemed`.
	member := f.joined(t, leaving, "admin")
	stayer := f.joined(t, staying, "admin")
	_ = stayer

	// The shape `invite.go:437` writes when an administrator types the address in
	// a case the account did not register in — the reason the match below folds
	// case on both sides rather than comparing the stored strings. Written
	// directly, because it cannot be provoked through the product from here: the
	// address is already a member of this organization, so a second invitation to
	// it is refused before any record is written.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO audit_logs (id, occurred_at, organization_id, action, metadata)
		VALUES ($1, now(), $2, 'invitation.created', $3::jsonb)`,
		uuid.Must(uuid.NewV7()), f.owner.OrgID,
		`{"email": "MEMBER@Example.COM", "role": "viewer"}`); err != nil {
		t.Fatalf("write the mixed-case metadata record: %v", err)
	}

	// Deleted first, then invited again. An **outstanding** invitation to the
	// same address is deliberately not scrubbed: it is an offer to an address
	// rather than a record of a person, the address became reusable the moment
	// the account was deleted, and blanking it would break the redemption
	// comparison for whoever takes it next. Ordered this way so the pass has both
	// kinds of row in front of it and has to tell them apart.
	if err := f.svc.Delete(ctx, member, accountPassword); err != nil {
		t.Fatalf("delete the member's account: %v", err)
	}
	outstanding, err := f.invites.Create(ctx, f.owner, invite.CreateInput{
		Email: leaving, Role: "viewer",
	})
	if err != nil {
		t.Fatalf("re-invite the freed address: %v", err)
	}

	carrying := f.count(t,
		`SELECT count(*) FROM audit_logs WHERE lower(metadata->>'email') = $1`, leaving)
	if carrying < 3 {
		t.Fatalf("%d audit records carry the departing address in their metadata; "+
			"the assertion needs the administrator's invitation.created, the "+
			"account's own invitation.redeemed and the mixed-case one", carrying)
	}
	stayerCarrying := f.count(t,
		`SELECT count(*) FROM audit_logs WHERE lower(metadata->>'email') = $1`, staying)
	if stayerCarrying < 2 {
		t.Fatalf("%d audit records carry the staying account's address; the "+
			"over-reach assertion needs at least two", stayerCarrying)
	}

	if n := f.erase(t, ctx); n != 1 {
		t.Fatalf("the erasure pass took %d accounts, want 1", n)
	}

	// ── F177: the metadata ────────────────────────────────────────────────────

	if n := f.count(t,
		`SELECT count(*) FROM audit_logs WHERE lower(metadata->>'email') = $1`,
		leaving); n != 0 {
		t.Errorf("%d audit records still carry the erased address inside metadata, "+
			"where a reader with audit.read finds it beside a tombstoned label", n)
	}
	if n := f.count(t,
		`SELECT count(*) FROM audit_logs WHERE metadata->>'email' = $1`,
		account.TombstoneLabel); n != carrying {
		t.Errorf("%d metadata blobs carry the tombstone and %d carried the address; "+
			"a key that was dropped rather than overwritten loses the fact that "+
			"there was an address there at all", n, carrying)
	}
	if n := f.count(t,
		`SELECT count(*) FROM audit_logs WHERE lower(metadata->>'email') = $1`,
		staying); n != stayerCarrying {
		t.Errorf("the staying account's address is in %d metadata blobs and was in "+
			"%d. Erasing one person's records must not reach another's", n, stayerCarrying)
	}

	// The record keeps everything else it said. Scrubbing the address out of an
	// audit entry is defensible; emptying the entry is not.
	var role string
	var accountCreated bool
	if err := f.pool.QueryRow(ctx, `
		SELECT metadata->>'role', (metadata->>'account_created')::bool
		  FROM audit_logs
		 WHERE action = 'invitation.redeemed' AND metadata->>'email' = $1`,
		account.TombstoneLabel).Scan(&role, &accountCreated); err != nil {
		t.Fatalf("the redemption record lost its other keys: %v", err)
	}
	if role != "admin" || !accountCreated {
		t.Errorf("the redemption record reads role=%q account_created=%v after "+
			"erasure; only the address was the person's", role, accountCreated)
	}

	// ── F181: the invitation ──────────────────────────────────────────────────

	var redeemedEmail string
	var redeemedAt *time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT email, redeemed_at FROM invitations WHERE redeemed_by = $1`,
		member.UserID).Scan(&redeemedEmail, &redeemedAt); err != nil {
		t.Fatalf("the redeemed invitation is gone; erasure scrubs the row rather "+
			"than deleting it, because it is the organization's record: %v", err)
	}
	if redeemedEmail != "" {
		t.Errorf("the invitation the erased account joined by still reads %q under "+
			"Address on /invites", redeemedEmail)
	}
	if redeemedAt == nil {
		t.Error("the invitation lost redeemed_at, so the row no longer says a person " +
			"joined by it")
	}

	// The outstanding one keeps the address, and keeps working. This is the half
	// of the decision that a scrub matched on the address rather than on
	// redeemed_by would have got wrong, silently, by breaking redemption for
	// whoever takes the freed address next.
	var outstandingEmail string
	if err := f.pool.QueryRow(ctx,
		`SELECT email FROM invitations WHERE id = $1`, outstanding.ID).
		Scan(&outstandingEmail); err != nil {
		t.Fatalf("read the outstanding invitation: %v", err)
	}
	if outstandingEmail != leaving {
		t.Errorf("the outstanding invitation reads %q; it is an offer to an address "+
			"that no account holds, and blanking it makes it unredeemable", outstandingEmail)
	}
	const prefix = "https://links.example.com/invite/"
	if _, err := f.invites.Redeem(ctx, invite.RedeemInput{
		Token: strings.TrimPrefix(outstanding.URL, prefix),
		Email: leaving, Password: accountPassword,
	}); err != nil {
		t.Errorf("the freed address could not redeem the invitation waiting for it: %v", err)
	}

	// And the account that stayed keeps its own.
	var stayerEmail string
	if err := f.pool.QueryRow(ctx,
		`SELECT email FROM invitations WHERE redeemed_by = $1`, stayer.UserID).
		Scan(&stayerEmail); err != nil {
		t.Fatalf("read the staying account's invitation: %v", err)
	}
	if stayerEmail != staying {
		t.Errorf("the staying account's invitation reads %q; one erasure blanked "+
			"somebody else's row", stayerEmail)
	}
}

// F188 and F189: the two sites F177's count did not reach, and neither is a
// scalar `"email"` key in `audit_logs`.
//
// **Both are records *about* the erased person held by somebody else**, which is
// the class F177 established is in scope, at the two shapes its predicate cannot
// match. F189 is an address inside a jsonb *array*, where a scalar comparison
// finds nothing. F188 is an address in a different table altogether — the
// inviter's own notification, which deleting the erased account never touched
// because the row belongs to the reader rather than to its subject, and which
// nothing expires because notifications are swept by neither age nor actor.
//
// The subject of each is chosen so that only the new scrub can clean it: the
// principal-move record's scalar `"email"` is the account that *stays*, so a
// green run here cannot be F177's work doing the job, and the same record is
// written with no actor at all, so it is not M52's label scrub either.
func TestErasureReachesTheAddressInAnArrayAndInSomebodyElsesNotification(t *testing.T) {
	f := newAccounts(t)
	ctx := t.Context()
	const leaving, staying = "member@example.com", "stayer@example.com"

	member := f.joined(t, leaving, "admin")
	stayer := f.joined(t, staying, "admin")

	// Out to the departing account and back again, so the *second* record's
	// `"from"` array names them while its `"email"` names the owner. One address
	// in each half of one metadata blob, and only one of them is the erased
	// account's.
	if _, err := f.instance.MovePrincipal(ctx, leaving); err != nil {
		t.Fatalf("move the instance principal to the departing account: %v", err)
	}
	if _, err := f.instance.MovePrincipal(ctx, "owner@example.com"); err != nil {
		t.Fatalf("move the instance principal back: %v", err)
	}

	const inArray = `
		SELECT count(*) FROM audit_logs
		 WHERE action = 'instance.principal_moved' AND metadata->'from' ? $1`
	if n := f.count(t, inArray, leaving); n != 1 {
		t.Fatalf("%d principal-move records carry the departing address in their "+
			"`from` array, want 1; the array assertion below would prove nothing", n)
	}
	// The notification the redemption wrote to the *inviter*, which is the owner.
	const inNotification = `
		SELECT count(*) FROM notifications WHERE data->>'email' = $1`
	if n := f.count(t, inNotification, leaving); n != 1 {
		t.Fatalf("%d notifications carry the departing address, want 1; the invitation "+
			"the account joined by is meant to have told the person who sent it", n)
	}
	var titleBefore string
	if err := f.pool.QueryRow(ctx,
		`SELECT title FROM notifications WHERE data->>'email' = $1`, leaving).
		Scan(&titleBefore); err != nil {
		t.Fatalf("read the inviter's notification: %v", err)
	}
	if !strings.Contains(titleBefore, leaving) {
		t.Fatalf("the inviter's notification reads %q, and the address is not in it; "+
			"the title assertion below would prove nothing", titleBefore)
	}

	if err := f.svc.Delete(ctx, member, accountPassword); err != nil {
		t.Fatalf("delete the member's account: %v", err)
	}
	if n := f.erase(t, ctx); n != 1 {
		t.Fatalf("the erasure pass took %d accounts, want 1", n)
	}

	// ── F189: the address in the array ────────────────────────────────────────

	if n := f.count(t, inArray, leaving); n != 0 {
		t.Errorf("%d principal-move records still name the erased account in their "+
			"`from` array. The scrub matches `metadata->>'email'`, which is a scalar "+
			"comparison and reaches nothing inside a list", n)
	}
	if n := f.count(t, inArray, account.TombstoneLabel); n != 1 {
		t.Errorf("%d principal-move records carry the tombstone in `from`, want 1. The "+
			"array says how many principals the role moved away from, so an element "+
			"dropped rather than overwritten loses that count", n)
	}
	var from []string
	var movedEmail string
	if err := f.pool.QueryRow(ctx, `
		SELECT array(SELECT jsonb_array_elements_text(metadata->'from')), metadata->>'email'
		  FROM audit_logs
		 WHERE action = 'instance.principal_moved' AND metadata->'from' ? $1`,
		account.TombstoneLabel).Scan(&from, &movedEmail); err != nil {
		t.Fatalf("read the scrubbed principal-move record: %v", err)
	}
	if len(from) != 1 {
		t.Errorf("the `from` array holds %d entries after erasure (%v), want 1", len(from), from)
	}
	// The other half of the same blob is somebody who is still here, and it is
	// what makes this record's scrub a scrub rather than a blanking.
	if movedEmail != "owner@example.com" {
		t.Errorf("the record's `email` key reads %q after erasing somebody else; the "+
			"account the principal moved *to* is still on this instance", movedEmail)
	}

	// ── F188: the address in somebody else's notification ─────────────────────

	if n := f.count(t, inNotification, leaving); n != 0 {
		t.Errorf("%d notifications still carry the erased address in their detail. "+
			"Deleting an account removes that account's own notifications; this row is "+
			"the inviter's, and nothing else reaches it", n)
	}
	var title, body, role, invitation string
	var kind string
	if err := f.pool.QueryRow(ctx, `
		SELECT title, body, kind, data->>'role', data->>'invitation'
		  FROM notifications WHERE data->>'email' = $1`,
		account.TombstoneLabel).Scan(&title, &body, &kind, &role, &invitation); err != nil {
		t.Fatalf("the inviter's notification is gone; erasure scrubs the row rather "+
			"than deleting somebody else's inbox: %v", err)
	}
	if strings.Contains(title, leaving) {
		t.Errorf("the notification's title still reads %q. The detail is scrubbed and "+
			"the sentence beside it is what the inviter actually reads on "+
			"/notifications", title)
	}
	if !strings.Contains(title, account.TombstoneLabel) {
		t.Errorf("the title reads %q; the address was replaced by something other than "+
			"the tombstone, so the sentence no longer says who accepted", title)
	}
	if role != "admin" || invitation == "" || kind == "" || body == "" {
		t.Errorf("the notification reads kind=%q role=%q invitation=%q body=%q after "+
			"erasure; only the address was the person's", kind, role, invitation, body)
	}

	// ── Neither reaches the account that stayed ───────────────────────────────

	if n := f.count(t, inNotification, staying); n != 1 {
		t.Errorf("the staying account is named in %d notifications, want 1. Erasing "+
			"one person's records must not reach another's", n)
	}
	var stayerTitle string
	if err := f.pool.QueryRow(ctx,
		`SELECT title FROM notifications WHERE data->>'email' = $1`, staying).
		Scan(&stayerTitle); err != nil {
		t.Fatalf("read the staying account's notification: %v", err)
	}
	if !strings.Contains(stayerTitle, staying) {
		t.Errorf("the staying account's notification reads %q; one erasure rewrote "+
			"somebody else's sentence", stayerTitle)
	}
	_ = stayer
}

// The two-leader window during a rolling deploy is a stated property of this
// scheduler, so the sweep has to be safe to run twice. Asserted by running it
// twice and diffing, which is what m52.md asks for by name.
func TestTheErasurePassIsIdempotent(t *testing.T) {
	f := newAccounts(t)
	ctx := t.Context()
	member := f.joined(t, "member@example.com", "admin")
	if _, err := f.invites.Create(ctx, member, invite.CreateInput{
		Email: "someone@example.com", Role: "viewer",
	}); err != nil {
		t.Fatalf("write an audit record as the member: %v", err)
	}
	if err := f.svc.Delete(ctx, member, accountPassword); err != nil {
		t.Fatalf("delete the member's account: %v", err)
	}

	if n := f.erase(t, ctx); n != 1 {
		t.Fatalf("the first pass took %d accounts, want 1", n)
	}
	before := f.snapshot(t, ctx)

	if n := f.erase(t, ctx); n != 0 {
		t.Errorf("the second pass took %d accounts. The row it would take again "+
			"carries anonymized_at, so the batch predicate is not reading it", n)
	}
	if after := f.snapshot(t, ctx); after != before {
		t.Errorf("running the pass twice changed the tree.\n before: %s\n  after: %s", before, after)
	}
}

// snapshot is everything the erasure pass can touch, rendered so two runs can be
// compared as one string. `updated_at` is in it deliberately: a second pass that
// rewrote the same values would still move it, and "wrote nothing" is the claim
// rather than "wrote the same thing".
//
// The metadata and invitation lines are here because M58 gave the pass two more
// things to write. Neither has an `updated_at` to betray a rewrite, so the values
// are the whole check — which is why both are listed for *every* row rather than
// only the tombstoned ones: a second pass reaching a row it should not have is
// the failure, and filtering to the rows it already changed would hide it.
func (f *accountFixture) snapshot(t *testing.T, ctx context.Context) string {
	t.Helper()
	var out string
	if err := f.pool.QueryRow(ctx, `
		SELECT coalesce(string_agg(line, E'\n' ORDER BY line), '')
		  FROM (
		    SELECT format('user %s %s %s %s %s', id, email, name, anonymized_at, updated_at) AS line
		      FROM users WHERE deleted_at IS NOT NULL
		    UNION ALL
		    SELECT format('audit %s %s', id, actor_label)
		      FROM audit_logs WHERE actor_label = $1
		    UNION ALL
		    SELECT format('audit-meta %s %s', id, metadata->>'email')
		      FROM audit_logs WHERE metadata ? 'email'
		    UNION ALL
		    SELECT format('invitation %s %s %s', id, email, redeemed_at)
		      FROM invitations
		    UNION ALL
		    SELECT format('dispute %s %s %s', id, created_by_label, decided_by_label)
		      FROM destination_disputes
		  ) rows`, account.TombstoneLabel).Scan(&out); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return out
}

// ─── What erasure must not reach ─────────────────────────────────────────────

// An uploaded logo belongs to a workspace, never to a person, and the sole-owner
// refusal means every organization a deletable account belongs to outlives it.
//
// **The claim worth protecting is that erasure does not over-reach**, which is
// the direction this could have failed in — an earlier draft of m52.md said
// deletion removes every logo belonging to the account's organizations, and that
// bullet's test could not have been written: a logo needs an organization, a
// deletable account is not that organization's sole owner, so another owner
// exists and the bytes have to survive.
func TestDeletingAnAccountReachesNoQRLogo(t *testing.T) {
	f := newAccounts(t)
	ctx := t.Context()
	member := f.joined(t, "member@example.com", "admin")

	// A link the member created, in the owner's organization — so the
	// organization is co-inhabited in the sense that matters here: it survives
	// the member leaving, and so must everything hanging off it.
	//
	// Through the link service, because `links` carries columns and defaults an
	// INSERT written here would have to reproduce, and a fixture that reproduces
	// them is a fixture that can be wrong about the rows under test. The logo is
	// written directly: M50.5 owns how bytes get into the column, and what is
	// asserted below is only that nothing takes them out.
	links := link.NewService(f.pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: "http://links.test",
	})
	created, err := links.Create(ctx, member, link.CreateInput{
		URL: "https://example.com/kept", Alias: "kept",
	})
	if err != nil {
		t.Fatalf("create a link as the member: %v", err)
	}
	linkID := created.ID
	logo := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO qr_codes (id, link_id, workspace_id, logo) VALUES ($1, $2, $3, $4)`,
		uuid.Must(uuid.NewV7()), linkID, member.WorkspaceID, logo); err != nil {
		t.Fatalf("attach a logo: %v", err)
	}

	if err := f.svc.Delete(ctx, member, accountPassword); err != nil {
		t.Fatalf("delete the member's account: %v", err)
	}
	f.erase(t, ctx)

	var stored []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT logo FROM qr_codes WHERE link_id = $1`, linkID).Scan(&stored); err != nil {
		t.Fatalf("the QR code is gone with the account that made its link: %v", err)
	}
	if len(stored) != len(logo) {
		t.Errorf("the logo is %d bytes after the account was deleted, and was %d. "+
			"Logos hang off qr_codes → links → workspaces, never off users, and "+
			"the organization outlives the account by construction",
			len(stored), len(logo))
	}
	if n := f.count(t, `SELECT count(*) FROM links WHERE id = $1`, linkID); n != 1 {
		t.Error("the link went with the account that created it. It belongs to the " +
			"workspace; links.created_by is ON DELETE SET NULL and would not have " +
			"taken it even under a hard delete")
	}
}

// ─── The value nobody writes ─────────────────────────────────────────────────

// `status = 'suspended'` stays without a writer, and this is the half of that
// claim a database can answer: the enum still admits it.
//
// The other half — that nothing writes it — is a fact about the source and is
// asserted by TestNothingWritesTheSuspendedStatus in internal/account, because a
// runtime test can only ever show that nothing wrote it during that test.
func TestTheSuspendedStatusIsStillAdmitted(t *testing.T) {
	f := newAccounts(t)
	ctx := t.Context()

	id := uuid.Must(uuid.NewV7())
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO users (id, email, name, status) VALUES ($1, 'suspended@example.com', 'S', 'suspended')`,
		id); err != nil {
		t.Fatalf("the CHECK constraint no longer admits 'suspended': %v. M52 says "+
			"in writing that the value stays available and unused; removing it is "+
			"a schema change with its own reasoning", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO users (id, email, name, status) VALUES ($1, 'nonsense@example.com', 'N', 'retired')`,
		uuid.Must(uuid.NewV7())); err == nil {
		t.Error("the CHECK constraint admitted 'retired', so the assertion above " +
			"proves nothing about the enum")
	}
}
