//go:build integration

package integration

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/invite"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/team"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// M28. Team management, workspaces and organization creation — and the three
// decisions that shape them: the rank bound (D30), the union rule for
// workspace-scoped membership (D31), and the refusal to delete a workspace that
// still holds links (D32).

const teamPassword = "a-sufficiently-long-password"

// teamFixture is an instance with an owner and the services that put people
// into it. Members are made by issuing and redeeming real invitations rather
// than by inserting rows, because that is the only way the product produces one
// and a fixture that cheats would not exercise what a member actually is.
type teamFixture struct {
	pool    *pgxpool.Pool
	auth    *auth.Service
	keys    *auth.APIKeyService
	invites *invite.Service
	links   *link.Service
	team    *team.Service
	owner   *auth.Identity
}

func newTeamFixture(t *testing.T) *teamFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	owner, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner", Password: teamPassword,
	})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	keySvc, err := auth.NewAPIKeyService(pool, authSvc, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatal(err)
	}
	inviteSvc, err := invite.NewService(pool, invite.Config{
		AppURL:      "https://links.example.com",
		TTL:         168 * time.Hour,
		NewAccounts: true,
		Hasher:      authSvc.Hasher(),
		Audit:       audit.NewService(pool),
		Notify:      notify.NewService(pool),
	})
	if err != nil {
		t.Fatal(err)
	}

	return &teamFixture{
		pool:    pool,
		auth:    authSvc,
		keys:    keySvc,
		invites: inviteSvc,
		links: link.NewService(pool, link.Config{
			Policy: link.DefaultDestinationPolicy(), BaseURL: "http://links.test",
		}),
		team:  team.NewService(pool, team.Config{Audit: audit.NewService(pool)}),
		owner: owner,
	}
}

// member invites somebody at a role, redeems it, and returns their identity.
func (f *teamFixture) member(t *testing.T, email, role string) *auth.Identity {
	t.Helper()
	created, err := f.invites.Create(t.Context(), f.owner, invite.CreateInput{Email: email, Role: role})
	if err != nil {
		t.Fatalf("invite %s as %s: %v", email, role, err)
	}
	const prefix = "https://links.example.com/invite/"
	token := created.URL[len(prefix):]
	if _, err := f.invites.Redeem(t.Context(), invite.RedeemInput{
		Token: token, Email: email, Password: teamPassword,
	}); err != nil {
		t.Fatalf("redeem for %s: %v", email, err)
	}
	id, err := f.auth.IdentityForEmail(t.Context(), email)
	if err != nil {
		t.Fatalf("identity for %s: %v", email, err)
	}
	if id.Role != role {
		t.Fatalf("%s joined as %q, want %s", email, id.Role, role)
	}
	return id
}

// identity re-reads somebody, because a role change only shows up on the next
// resolution — which is exactly the property being asserted when it is used.
func (f *teamFixture) identity(t *testing.T, email string) *auth.Identity {
	t.Helper()
	id, err := f.auth.IdentityForEmail(t.Context(), email)
	if err != nil {
		t.Fatalf("identity for %s: %v", email, err)
	}
	return id
}

// identityIn resolves somebody in a named workspace, by pinning it as their
// default and letting the ordinary resolution path find it.
//
// Through the real precedence rather than by constructing an Identity, because
// what is being tested is what the evaluator computes for a person in a
// workspace — and building the identity by hand would test the test.
func (f *teamFixture) identityIn(t *testing.T, email string, workspaceID uuid.UUID) *auth.Identity {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE users SET default_workspace_id = $2 WHERE email_lower = lower($1)`,
		email, workspaceID); err != nil {
		t.Fatalf("pin workspace for %s: %v", email, err)
	}
	id := f.identity(t, email)
	if id.WorkspaceID != workspaceID {
		t.Fatalf("%s resolved into workspace %s, want %s", email, id.WorkspaceID, workspaceID)
	}
	return id
}

// membershipOf finds somebody's organization-wide membership id in the owner's
// organization, which is what every member operation acts on.
func (f *teamFixture) membershipOf(t *testing.T, actor *auth.Identity, email string) uuid.UUID {
	t.Helper()
	members, err := f.team.Members(t.Context(), actor)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	for _, m := range members {
		if m.Email == email && m.WorkspaceID == nil {
			return m.ID
		}
	}
	t.Fatalf("%s has no organization-wide membership", email)
	return uuid.Nil
}

func (f *teamFixture) count(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// ─── the rank table ─────────────────────────────────────────────────────────

// The table m28.md required be written into it before any code, asserted row by
// row. D30 is its spine, and the two consequences the milestone file calls out —
// an admin cannot demote themselves, and "strictly below" is evaluated on rank
// rather than identity — are the last two cases.
func TestRankTableIsWhatWasWrittenDown(t *testing.T) {
	f := newTeamFixture(t)

	admin := f.member(t, "admin@example.com", "admin")
	f.member(t, "admin2@example.com", "admin")
	f.member(t, "editor@example.com", "editor")
	f.member(t, "viewer@example.com", "viewer")
	f.member(t, "co-owner@example.com", "owner")

	// Row 3 and 4: an editor and a viewer hold no members.write, so neither can
	// reach any of it — and an editor cannot even read the list, because
	// members.read is granted to owner and admin only.
	for _, who := range []string{"editor@example.com", "viewer@example.com"} {
		id := f.identity(t, who)
		if _, err := f.team.Members(t.Context(), id); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("%s listed members: %v", who, err)
		}
		if err := f.team.Remove(t.Context(), id,
			f.membershipOf(t, f.owner, "viewer@example.com")); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("%s removed a member: %v", who, err)
		}
	}

	// Row 2: an admin manages editors and viewers.
	for _, who := range []string{"editor@example.com", "viewer@example.com"} {
		if err := f.team.ChangeRole(t.Context(), admin, f.membershipOf(t, admin, who), "viewer"); err != nil {
			t.Errorf("an admin could not re-role %s: %v", who, err)
		}
	}

	// …and never another admin, nor an owner, nor themselves. Three refusals
	// with one cause: none of those ranks is strictly below an admin's.
	for _, who := range []string{"admin2@example.com", "owner@example.com", "admin@example.com"} {
		id := f.membershipOf(t, admin, who)
		if err := f.team.ChangeRole(t.Context(), admin, id, "viewer"); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("an admin re-roled %s: %v", who, err)
		}
		if err := f.team.Remove(t.Context(), admin, id); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("an admin removed %s: %v", who, err)
		}
	}

	// Row 1: an owner manages every rank, its own included — and that is
	// evaluated on rank, not identity, so a second owner is managed by the first
	// without either being special.
	if err := f.team.ChangeRole(t.Context(), f.owner,
		f.membershipOf(t, f.owner, "co-owner@example.com"), "editor"); err != nil {
		t.Errorf("an owner could not demote another owner: %v", err)
	}
	if err := f.team.ChangeRole(t.Context(), f.owner,
		f.membershipOf(t, f.owner, "admin2@example.com"), "viewer"); err != nil {
		t.Errorf("an owner could not demote an admin: %v", err)
	}

	// The ceiling on what may be granted is separate from who may be managed:
	// "nobody grants a role binding tighter than their own". An admin may mint
	// another admin — the same ceiling an invitation carries — and may not mint
	// an owner.
	editorMembership := f.membershipOf(t, admin, "editor@example.com")
	if err := f.team.ChangeRole(t.Context(), admin, editorMembership, "owner"); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an admin granted the owner role: %v", err)
	}
	if err := f.team.ChangeRole(t.Context(), admin, editorMembership, "admin"); err != nil {
		t.Errorf("an admin could not grant the admin role: %v", err)
	}
	// And having done so, they can no longer manage them, which follows from the
	// first rule rather than contradicting the second.
	if err := f.team.Remove(t.Context(), admin, editorMembership); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an admin removed the peer they had just created: %v", err)
	}
}

// The refusal that stops an organization being orphaned, on both paths.
//
// A second owner makes both operations succeed, which is what distinguishes
// this from an implementation that simply refuses to touch owners.
func TestTheLastOwnerCannotBeRemovedOrDemoted(t *testing.T) {
	f := newTeamFixture(t)
	own := f.membershipOf(t, f.owner, "owner@example.com")

	if err := f.team.ChangeRole(t.Context(), f.owner, own, "admin"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("the last owner demoted themselves: %v", err)
	}
	if err := f.team.Remove(t.Context(), f.owner, own); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("the last owner removed themselves: %v", err)
	}
	if n := f.count(t, `
		SELECT count(*) FROM memberships m JOIN roles r ON r.id = m.role_id
		 WHERE m.organization_id = $1 AND r.slug = 'owner'`, f.owner.OrgID); n != 1 {
		t.Fatalf("the organization has %d owners after two refusals, want 1", n)
	}

	// With a second owner, both become possible — and the second one is then the
	// last, so it is refused in turn.
	f.member(t, "second@example.com", "owner")
	if err := f.team.ChangeRole(t.Context(), f.owner, own, "admin"); err != nil {
		t.Fatalf("an owner could not step down while another owner existed: %v", err)
	}

	second := f.identity(t, "second@example.com")
	if err := f.team.Remove(t.Context(), second,
		f.membershipOf(t, second, "second@example.com")); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("the remaining owner removed themselves: %v", err)
	}
}

// ─── workspace-scoped membership (D31) ──────────────────────────────────────

// The writer the COALESCE uniqueness index in 00200 has been waiting for since
// Phase 1, and the union rule it makes observable.
//
// A workspace-scoped membership **adds**: the same person is an admin in the
// workspace it names and stays a viewer everywhere else. Nothing here narrows,
// which is the surprise D31 chose to have — in the direction of granting more
// than expected rather than less.
func TestWorkspaceScopedMembershipAddsAndNeverNarrows(t *testing.T) {
	f := newTeamFixture(t)
	viewer := f.member(t, "scoped@example.com", "viewer")

	marketing, err := f.team.CreateWorkspace(t.Context(), f.owner, "Marketing")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// Before the grant, the second workspace is reachable — the org-wide
	// membership covers every workspace — but only as a viewer.
	if id := f.identityIn(t, "scoped@example.com", marketing.ID); id.Role != "viewer" {
		t.Fatalf("before the grant they are %q in Marketing, want viewer", id.Role)
	}

	granted, err := f.team.Grant(t.Context(), f.owner, team.GrantInput{
		UserID: viewer.UserID, WorkspaceID: marketing.ID, Role: "admin",
	})
	if err != nil {
		t.Fatalf("grant workspace access: %v", err)
	}
	if granted.WorkspaceID == nil || *granted.WorkspaceID != marketing.ID {
		t.Fatalf("the grant is not workspace-scoped: %+v", granted)
	}
	if n := f.count(t,
		`SELECT count(*) FROM memberships WHERE user_id = $1 AND workspace_id = $2`,
		viewer.UserID, marketing.ID); n != 1 {
		t.Fatal("no workspace-scoped membership row was written")
	}

	// In the named workspace: the union. The effective role is the lowest rank
	// among the matching memberships, and the permissions are the sum.
	inMarketing := f.identityIn(t, "scoped@example.com", marketing.ID)
	if inMarketing.Role != "admin" {
		t.Errorf("in Marketing they are %q, want admin", inMarketing.Role)
	}
	if !inMarketing.Can("members.write") {
		t.Error("the workspace-scoped admin holds no members.write there; the union did not apply")
	}

	// Everywhere else: unchanged. This is the half that makes it "adds, never
	// narrows" rather than "moves".
	elsewhere := f.identityIn(t, "scoped@example.com", f.owner.WorkspaceID)
	if elsewhere.Role != "viewer" {
		t.Errorf("outside Marketing they are %q, want viewer", elsewhere.Role)
	}
	if elsewhere.Can("members.write") {
		t.Error("the workspace grant leaked members.write into another workspace")
	}

	// One grant per workspace, which is what the COALESCE index makes
	// unrepresentable rather than unlikely.
	if _, err := f.team.Grant(t.Context(), f.owner, team.GrantInput{
		UserID: viewer.UserID, WorkspaceID: marketing.ID, Role: "editor",
	}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a second grant for the same workspace answered %v, want a validation error", err)
	}

	// Withdrawing it takes back exactly what it added.
	members, err := f.team.Members(t.Context(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	var scoped uuid.UUID
	for _, m := range members {
		if m.WorkspaceID != nil && m.UserID == viewer.UserID {
			scoped = m.ID
		}
	}
	if scoped == uuid.Nil {
		t.Fatal("the member list does not show the workspace-scoped membership")
	}
	if err := f.team.Remove(t.Context(), f.owner, scoped); err != nil {
		t.Fatalf("remove the workspace grant: %v", err)
	}
	if id := f.identityIn(t, "scoped@example.com", marketing.ID); id.Role != "viewer" {
		t.Errorf("after withdrawal they are %q in Marketing, want viewer again", id.Role)
	}
	if n := f.count(t, `SELECT count(*) FROM memberships WHERE user_id = $1`, viewer.UserID); n != 1 {
		t.Errorf("the person holds %d memberships after withdrawal, want their org-wide one", n)
	}
}

// A grant is for somebody already in the organization. A stranger is invited,
// not granted, and a workspace in another tenant is not a workspace at all.
func TestGrantRefusesNonMembersAndForeignWorkspaces(t *testing.T) {
	f := newTeamFixture(t)
	marketing, err := f.team.CreateWorkspace(t.Context(), f.owner, "Marketing")
	if err != nil {
		t.Fatal(err)
	}

	// Somebody with their own account and their own organization, and no
	// membership here.
	stranger, err := f.auth.Register(t.Context(), auth.RegisterInput{
		Email: "stranger@example.com", Password: teamPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.team.Grant(t.Context(), f.owner, team.GrantInput{
		UserID: stranger.UserID, WorkspaceID: marketing.ID, Role: "editor",
	}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a non-member was granted workspace access: %v", err)
	}

	// And the stranger's own workspace cannot be named from here: it belongs to
	// another organization, so it is not a workspace this actor can see.
	member := f.member(t, "inside@example.com", "editor")
	if _, err := f.team.Grant(t.Context(), f.owner, team.GrantInput{
		UserID: member.UserID, WorkspaceID: stranger.WorkspaceID, Role: "editor",
	}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a workspace from another organization was granted: %v", err)
	}
	if n := f.count(t,
		`SELECT count(*) FROM memberships WHERE organization_id = $1`,
		stranger.OrgID); n != 1 {
		t.Error("a refused grant wrote into another organization")
	}

	// Somebody left holding only a workspace-scoped membership is still a
	// member, and can still be given a second workspace.
	//
	// This is the dead end the "any membership counts" rule exists to avoid.
	// Re-inviting them is refused as already-a-member, so if a grant also
	// required an organization-wide membership, a person narrowed to one
	// workspace could never be widened again by any route the product has.
	if _, err := f.team.Grant(t.Context(), f.owner, team.GrantInput{
		UserID: member.UserID, WorkspaceID: marketing.ID, Role: "editor",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.team.Remove(t.Context(), f.owner,
		f.membershipOf(t, f.owner, "inside@example.com")); err != nil {
		t.Fatalf("remove the organization-wide membership: %v", err)
	}
	second, err := f.team.CreateWorkspace(t.Context(), f.owner, "Support")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.team.Grant(t.Context(), f.owner, team.GrantInput{
		UserID: member.UserID, WorkspaceID: second.ID, Role: "editor",
	}); err != nil {
		t.Errorf("a workspace-scoped member could not be given a second workspace: %v", err)
	}
}

// ─── workspaces (D15, D32) ──────────────────────────────────────────────────

// Create, rename and delete, and the permission that gates all three.
func TestWorkspaceLifecycleIsPermissionGated(t *testing.T) {
	f := newTeamFixture(t)
	editor := f.member(t, "editor@example.com", "editor")

	if _, err := f.team.CreateWorkspace(t.Context(), editor, "Sneaky"); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an editor created a workspace: %v", err)
	}

	ws, err := f.team.CreateWorkspace(t.Context(), f.owner, "Marketing")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ws.Slug != "marketing" {
		t.Errorf("slug is %q, want it derived from the name", ws.Slug)
	}
	// The name is unique per organization, and the slug is what enforces it.
	if _, err := f.team.CreateWorkspace(t.Context(), f.owner, "marketing"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a duplicate workspace name answered %v, want a validation error", err)
	}
	if _, err := f.team.CreateWorkspace(t.Context(), f.owner, "  "); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a blank workspace name answered %v, want a validation error", err)
	}

	if _, err := f.team.RenameWorkspace(t.Context(), editor, ws.ID, "Nope"); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an editor renamed a workspace: %v", err)
	}
	renamed, err := f.team.RenameWorkspace(t.Context(), f.owner, ws.ID, "Growth")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Name != "Growth" || renamed.Slug != "growth" {
		t.Errorf("rename produced %q/%q, want Growth/growth", renamed.Name, renamed.Slug)
	}

	if err := f.team.DeleteWorkspace(t.Context(), editor, ws.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an editor deleted a workspace: %v", err)
	}
	if err := f.team.DeleteWorkspace(t.Context(), f.owner, ws.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM workspaces WHERE id = $1`, ws.ID); n != 0 {
		t.Error("the workspace row survived its deletion")
	}

	// An id from another organization is not-found, the same answer one that
	// never existed gets, so ids cannot be probed.
	other, err := f.auth.Register(t.Context(), auth.RegisterInput{
		Email: "other@example.com", Password: teamPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.team.DeleteWorkspace(t.Context(), f.owner, other.WorkspaceID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deleting another organization's workspace answered %v, want not-found", err)
	}
}

// D32: while a workspace holds any link at all, deleting it is refused, and the
// links must be deleted first.
//
// Archiving is deliberately not an escape hatch — an archived link keeps its
// alias and its click history, so cascading it away with the workspace would be
// silent data loss dressed as tidying up.
func TestAWorkspaceHoldingLinksRefusesToBeDeleted(t *testing.T) {
	f := newTeamFixture(t)
	ws, err := f.team.CreateWorkspace(t.Context(), f.owner, "Campaigns")
	if err != nil {
		t.Fatal(err)
	}
	inWorkspace := f.identityIn(t, "owner@example.com", ws.ID)

	created, err := f.links.Create(t.Context(), inWorkspace, link.CreateInput{
		URL: "https://example.com/one", Alias: "one",
	})
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	if err := f.team.DeleteWorkspace(t.Context(), inWorkspace, ws.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a workspace holding a link was deleted: %v", err)
	}

	// Archiving does not get around it.
	if _, err := f.links.Archive(t.Context(), inWorkspace, created.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := f.team.DeleteWorkspace(t.Context(), inWorkspace, ws.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("archiving a link let the workspace be deleted: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM links WHERE id = $1`, created.ID); n != 1 {
		t.Fatal("a refused deletion cascaded the link away anyway")
	}

	// Deleting the link is the way through, which is the cost the decision names
	// out loud: one link at a time, because Phase 2 has no bulk delete.
	if err := f.links.Delete(t.Context(), inWorkspace, created.ID); err != nil {
		t.Fatalf("delete link: %v", err)
	}
	if err := f.team.DeleteWorkspace(t.Context(), inWorkspace, ws.ID); err != nil {
		t.Fatalf("an emptied workspace still refused deletion: %v", err)
	}
}

// The organization's last workspace is refused too.
//
// Everybody in an organization resolves into one of its workspaces to act at
// all, and ResolveWorkspaceForUser reports finding none as a broken instance —
// so deleting the last one would leave every member unable to sign in, with no
// route back that does not involve SQL.
func TestTheLastWorkspaceOfAnOrganizationCannotBeDeleted(t *testing.T) {
	f := newTeamFixture(t)

	if err := f.team.DeleteWorkspace(t.Context(), f.owner, f.owner.WorkspaceID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("the only workspace was deleted: %v", err)
	}

	// With a second one it becomes possible, and everybody still resolves
	// somewhere afterwards — which is the property the refusal protects.
	if _, err := f.team.CreateWorkspace(t.Context(), f.owner, "Second"); err != nil {
		t.Fatal(err)
	}
	if err := f.team.DeleteWorkspace(t.Context(), f.owner, f.owner.WorkspaceID); err != nil {
		t.Fatalf("delete with a spare workspace: %v", err)
	}
	if _, err := f.auth.IdentityForEmail(t.Context(), "owner@example.com"); err != nil {
		t.Fatalf("the owner resolves nowhere after deleting the workspace they were in: %v", err)
	}
}

// ─── organizations (D16) ────────────────────────────────────────────────────

// D16, expressed as a grant rather than as a check on how an account was made.
//
// The self-registered owner holds orgs.create because registering made them an
// owner. Everybody who arrived by invitation holds it only if somebody
// deliberately gave them the owner role — which is the "and nobody else until
// they grant it" half of the decision.
func TestOrgsCreateReachesSelfRegisteredAccountsOnly(t *testing.T) {
	f := newTeamFixture(t)

	if !f.owner.Can(team.PermOrgsCreate) {
		t.Fatal("the account from the setup form does not hold orgs.create")
	}
	for _, role := range []string{"admin", "editor", "viewer"} {
		id := f.member(t, role+"@example.com", role)
		if id.Can(team.PermOrgsCreate) {
			t.Errorf("an invited %s holds orgs.create", role)
		}
		if _, err := f.team.CreateOrganization(t.Context(), id, "Theirs"); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("an invited %s created an organization: %v", role, err)
		}
	}

	// The grant is a role grant, so promoting somebody to owner is what hands it
	// over — the D6 promise that an invited account can own an organization
	// later without needing a second account.
	invited := f.member(t, "future@example.com", "viewer")
	if err := f.team.ChangeRole(t.Context(), f.owner,
		f.membershipOf(t, f.owner, "future@example.com"), "owner"); err != nil {
		t.Fatal(err)
	}
	promoted := f.identity(t, "future@example.com")
	if !promoted.Can(team.PermOrgsCreate) {
		t.Fatal("an invited account promoted to owner still cannot create an organization")
	}
	if _, err := f.team.CreateOrganization(t.Context(), promoted, "Theirs"); err != nil {
		t.Fatalf("the promoted account could not create an organization: %v", err)
	}
	_ = invited

	// The permission is seeded and granted to exactly one role, which is the
	// migration's whole claim.
	if n := f.count(t, `
		SELECT count(*) FROM role_permissions rp
		  JOIN permissions p ON p.id = rp.permission_id
		  JOIN roles r ON r.id = rp.role_id
		 WHERE p.slug = 'orgs.create' AND r.slug <> 'owner'`); n != 0 {
		t.Errorf("orgs.create is granted to %d roles besides owner", n)
	}
}

// Creating an organization provisions it with a workspace and an owner
// membership, in one transaction, reusing the Register path.
func TestCreatingAnOrganizationProvisionsItWhole(t *testing.T) {
	f := newTeamFixture(t)

	org, err := f.team.CreateOrganization(t.Context(), f.owner, "Acme")
	if err != nil {
		t.Fatalf("create organization: %v", err)
	}
	if org.IsPersonal {
		t.Error("a deliberately created organization is marked personal")
	}
	if n := f.count(t,
		`SELECT count(*) FROM workspaces WHERE organization_id = $1`, org.ID); n != 1 {
		t.Errorf("the new organization has %d workspaces, want the one it was provisioned with", n)
	}
	if n := f.count(t, `
		SELECT count(*) FROM memberships m JOIN roles r ON r.id = m.role_id
		 WHERE m.organization_id = $1 AND m.user_id = $2
		   AND r.slug = 'owner' AND m.workspace_id IS NULL`,
		org.ID, f.owner.UserID); n != 1 {
		t.Error("the creator is not the new organization's organization-wide owner")
	}

	// And they can act in it: the identity resolves, with an owner's
	// permissions, which is what "provisioned whole" has to mean.
	acting := f.identityIn(t, "owner@example.com", org.WorkspaceID)
	if acting.OrgID != org.ID || acting.Role != "owner" {
		t.Errorf("in the new organization they are %q of %s, want owner of %s",
			acting.Role, acting.OrgID, org.ID)
	}

	// Nothing about the personal organization changed, so this is an addition
	// rather than a move.
	if n := f.count(t,
		`SELECT count(*) FROM memberships WHERE user_id = $1`, f.owner.UserID); n != 2 {
		t.Errorf("the creator holds %d memberships, want their own plus the new one", n)
	}

	if _, err := f.team.CreateOrganization(t.Context(), f.owner, "   "); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a blank organization name answered %v, want a validation error", err)
	}

	// Two organizations of the same name, back to back.
	//
	// Names are not unique and slugs are, so the slug carries a suffix from the
	// id — and the suffix has to be the random half of the UUIDv7 rather than
	// its leading timestamp, whose top 32 bits are identical for everything
	// created within the same 65-second window. That is not a rare race: it is
	// every pair of same-named organizations made in the same minute, which on
	// this path means somebody retrying.
	for i := range 2 {
		if _, err := f.team.CreateOrganization(t.Context(), f.owner, "Acme"); err != nil {
			t.Fatalf("organization %d named Acme: %v", i+1, err)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM organizations WHERE name = 'Acme'`); n != 3 {
		t.Errorf("%d organizations are named Acme, want 3", n)
	}
}

// D18's delegability conclusion for orgs.create, tested rather than only
// recorded: neither limb matches, so the map does not list it and a key holding
// it works.
//
// Both halves are asserted, because the map is the only mechanism that makes a
// scope session-only — its absence is the decision, and a live request through
// the bearer path is what proves the decision is in effect.
func TestOrgsCreateIsDelegableToAnAPIKey(t *testing.T) {
	f := newTeamFixture(t)
	if _, blocked := auth.NonDelegableScopes[team.PermOrgsCreate]; blocked {
		t.Fatal("orgs.create is in NonDelegableScopes; M28 concluded it stays delegable")
	}

	key, err := f.keys.Create(t.Context(), f.owner, auth.CreateAPIKeyInput{
		Name: "provisioner", Scopes: []string{team.PermOrgsCreate},
	})
	if err != nil {
		t.Fatalf("mint a key with orgs.create: %v", err)
	}

	cfg := config.Config{AppEnv: config.Development, BaseURL: "http://links.test"}
	cfg.Auth.SignupMode = config.SignupInvite
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour
	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config: cfg, Auth: f.auth, Keys: f.keys, Team: f.team,
	}))
	t.Cleanup(srv.Close)

	status, body := postJSONAs(t, srv, "/api/v1/organizations", key.Key,
		map[string]string{"name": "By key"})
	if status != http.StatusCreated {
		t.Fatalf("an API key holding orgs.create could not create one: %d\n%s", status, body)
	}
	if n := f.count(t, `SELECT count(*) FROM organizations WHERE name = 'By key'`); n != 1 {
		t.Error("the organization was not written")
	}

	// The key's reach did not widen: its scopes are what they were minted with,
	// intersected with its owner's role on every request, so it still cannot do
	// anything else in the organization it just made.
	status, _ = postJSONAs(t, srv, "/api/v1/members", key.Key, map[string]string{
		"user_id": f.owner.UserID.String(), "workspace_id": f.owner.WorkspaceID.String(),
		"role": "admin",
	})
	if status != http.StatusForbidden {
		t.Errorf("the key reached members.write with %d; its scopes should not have widened", status)
	}
}

// ─── audit ──────────────────────────────────────────────────────────────────

// Member added, removed and re-roled, workspace created, renamed and deleted,
// organization created — every change this milestone makes is an audit event
// (M21).
func TestTeamChangesAreAudited(t *testing.T) {
	f := newTeamFixture(t)
	member := f.member(t, "audited@example.com", "editor")

	ws, err := f.team.CreateWorkspace(t.Context(), f.owner, "Audited")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.team.RenameWorkspace(t.Context(), f.owner, ws.ID, "Audited Two"); err != nil {
		t.Fatal(err)
	}
	granted, err := f.team.Grant(t.Context(), f.owner, team.GrantInput{
		UserID: member.UserID, WorkspaceID: ws.ID, Role: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.team.ChangeRole(t.Context(), f.owner,
		f.membershipOf(t, f.owner, "audited@example.com"), "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := f.team.Remove(t.Context(), f.owner, granted.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.team.DeleteWorkspace(t.Context(), f.owner, ws.ID); err != nil {
		t.Fatal(err)
	}
	org, err := f.team.CreateOrganization(t.Context(), f.owner, "Audited Org")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []struct {
		action, targetType string
		org                uuid.UUID
	}{
		{audit.ActionMemberAdded, "membership", f.owner.OrgID},
		{audit.ActionMemberRoleChanged, "membership", f.owner.OrgID},
		{audit.ActionMemberRemoved, "membership", f.owner.OrgID},
		{audit.ActionWorkspaceCreated, "workspace", f.owner.OrgID},
		{audit.ActionWorkspaceRenamed, "workspace", f.owner.OrgID},
		{audit.ActionWorkspaceDeleted, "workspace", f.owner.OrgID},
		// Recorded against the organization it created, which is where somebody
		// reading that organization's log would look for how it began.
		{audit.ActionOrganizationCreated, "organization", org.ID},
	} {
		n := f.count(t, `
			SELECT count(*) FROM audit_logs
			 WHERE action = $1 AND target_type = $2 AND organization_id = $3
			   AND actor_label = 'owner@example.com'`,
			want.action, want.targetType, want.org)
		if n != 1 {
			t.Errorf("%s recorded %d times, want 1", want.action, n)
		}
	}

	// The deletion record carries the name, because the row it names is gone and
	// this is the only remaining trace of what was there.
	if n := f.count(t, `
		SELECT count(*) FROM audit_logs
		 WHERE action = $1 AND metadata->>'name' = 'Audited Two'`,
		audit.ActionWorkspaceDeleted); n != 1 {
		t.Error("the workspace deletion record does not name what was deleted")
	}
	// The re-role record carries both ends, which is what makes "who gave this
	// person admin" answerable from the log alone.
	if n := f.count(t, `
		SELECT count(*) FROM audit_logs
		 WHERE action = $1 AND metadata->>'from' = 'editor' AND metadata->>'to' = 'viewer'`,
		audit.ActionMemberRoleChanged); n != 1 {
		t.Error("the role-change record does not say what the role changed from and to")
	}
}

// ─── organization deletion (M28.5: D36, D37) ────────────────────────────────

// otherOrganization puts a second organization on the instance, so the
// last-organization refusal is not the one under test.
//
// A separate registered account rather than a second organization for the
// owner: the refusals below are about the *instance* having one left, and the
// clearest way to say "there is another" is that somebody else has one.
func (f *teamFixture) otherOrganization(t *testing.T) *auth.Identity {
	t.Helper()
	id, err := f.auth.Register(t.Context(), auth.RegisterInput{
		Email: "elsewhere@example.com", Password: teamPassword,
	})
	if err != nil {
		t.Fatalf("register the second organization's owner: %v", err)
	}
	return id
}

// org.delete, enforced for the first time since Phase 1 seeded it.
//
// Held by the owner role alone, and M28's ceiling already forbids an admin
// acquiring it — granting the owner role requires being an owner — so the set of
// accounts that can reach this is exactly the set an owner chose.
func TestDeletingAnOrganizationRequiresOrgDelete(t *testing.T) {
	f := newTeamFixture(t)
	other := f.otherOrganization(t)

	for _, role := range []string{"admin", "editor", "viewer"} {
		id := f.member(t, role+"@example.com", role)
		if id.Can(team.PermOrgDelete) {
			t.Errorf("an invited %s holds %s", role, team.PermOrgDelete)
		}
		if err := f.team.DeleteOrganization(t.Context(), id, id.OrgID); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("an %s deleted the organization: %v", role, err)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM organizations WHERE id = $1`, f.owner.OrgID); n != 1 {
		t.Fatal("a refused deletion removed the organization anyway")
	}

	// Another organization's id is not-found rather than forbidden, the same
	// answer one that never existed gets, so an id cannot be probed — and a
	// mistyped one deletes nothing rather than the wrong thing.
	if err := f.team.DeleteOrganization(t.Context(), f.owner, other.OrgID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deleting another organization by id answered %v, want not-found", err)
	}
	if err := f.team.DeleteOrganization(t.Context(), f.owner, uuid.Must(uuid.NewV7())); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deleting an id from nowhere answered %v, want not-found", err)
	}
	if n := f.count(t, `SELECT count(*) FROM organizations WHERE id = $1`, other.OrgID); n != 1 {
		t.Fatal("naming another organization's id deleted it")
	}

	if err := f.team.DeleteOrganization(t.Context(), f.owner, f.owner.OrgID); err != nil {
		t.Fatalf("the owner could not delete their own organization: %v", err)
	}
	// The row, not the return value. A deletion that reports success and leaves
	// the organization standing passes every assertion above it.
	if n := f.count(t, `SELECT count(*) FROM organizations WHERE id = $1`, f.owner.OrgID); n != 0 {
		t.Error("the organization survived a deletion that reported success")
	}
}

// D37: an organization holding any link refuses deletion, mirroring D32 one
// level up so that deleting above a workspace is not a way around it.
//
// Archived links count for the same reason they do at the workspace level — an
// archived link keeps its alias and its click history — and the link lives in a
// *second* workspace here, because the guard has to look across all of them
// rather than at the one the actor happens to be in.
func TestAnOrganizationHoldingLinksRefusesToBeDeleted(t *testing.T) {
	f := newTeamFixture(t)
	f.otherOrganization(t)

	ws, err := f.team.CreateWorkspace(t.Context(), f.owner, "Campaigns")
	if err != nil {
		t.Fatal(err)
	}
	inWorkspace := f.identityIn(t, "owner@example.com", ws.ID)
	created, err := f.links.Create(t.Context(), inWorkspace, link.CreateInput{
		URL: "https://example.com/one", Alias: "one",
	})
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	// Acting from the organization's *first* workspace, which holds nothing: the
	// refusal is about the organization, not about where the caller is standing.
	fromDefault := f.identityIn(t, "owner@example.com", f.owner.WorkspaceID)
	if err := f.team.DeleteOrganization(t.Context(), fromDefault, f.owner.OrgID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("an organization holding a link in another workspace was deleted: %v", err)
	}

	// Archiving is not a way around it, exactly as at the workspace level.
	if _, err := f.links.Archive(t.Context(), inWorkspace, created.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := f.team.DeleteOrganization(t.Context(), fromDefault, f.owner.OrgID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("archiving the link let the organization be deleted: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM links WHERE id = $1`, created.ID); n != 1 {
		t.Fatal("a refused deletion cascaded the link away anyway")
	}

	// Deleting the link is the way through, and it is the cost D37 states out
	// loud: with no bulk delete, that is one link at a time.
	if err := f.links.Delete(t.Context(), inWorkspace, created.ID); err != nil {
		t.Fatalf("delete link: %v", err)
	}
	if err := f.team.DeleteOrganization(t.Context(), fromDefault, f.owner.OrgID); err != nil {
		t.Fatalf("an emptied organization still refused deletion: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM workspaces WHERE organization_id = $1`, f.owner.OrgID); n != 0 {
		t.Error("the organization's workspaces survived its deletion")
	}
}

// The instance's last organization is refused, on the reasoning that refuses the
// last owner and the last workspace: the state is unreachable by any other route
// and unrecoverable without SQL.
func TestTheLastOrganizationOnTheInstanceCannotBeDeleted(t *testing.T) {
	f := newTeamFixture(t)

	if err := f.team.DeleteOrganization(t.Context(), f.owner, f.owner.OrgID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("the instance's only organization was deleted: %v", err)
	}

	// With a second one it becomes possible, and the second is then the last, so
	// it is refused in turn — which is what distinguishes a rule from a refusal
	// to touch organizations at all.
	second, err := f.team.CreateOrganization(t.Context(), f.owner, "Second")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.team.DeleteOrganization(t.Context(), f.owner, f.owner.OrgID); err != nil {
		t.Fatalf("delete with a spare organization: %v", err)
	}
	inSecond := f.identityIn(t, "owner@example.com", second.WorkspaceID)
	if err := f.team.DeleteOrganization(t.Context(), inSecond, second.ID); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("the remaining organization was deleted: %v", err)
	}
}

// D36, the answer that makes this milestone large: deletion proceeds even when
// it leaves somebody belonging to nothing.
//
// The account survives whole — it is not deleted, not suspended, not anonymized
// — and the session path treats *no workspace* as an empty state rather than as
// the broken instance it called it before M28.5. Both halves are asserted,
// because an account that survives but cannot authenticate is the same outcome
// as one that was deleted.
func TestDeletingAnOrganizationLeavesItsMembersBelongingToNothing(t *testing.T) {
	f := newTeamFixture(t)
	f.otherOrganization(t)
	orphan := f.member(t, "orphan@example.com", "editor")

	// Something to sign in with afterwards, established before the deletion so
	// the assertion is about the session surviving rather than about a new one.
	if _, err := f.auth.Login(t.Context(), auth.LoginInput{
		Email: "orphan@example.com", Password: teamPassword,
	}); err != nil {
		t.Fatalf("sign in before the deletion: %v", err)
	}

	if err := f.team.DeleteOrganization(t.Context(), f.owner, f.owner.OrgID); err != nil {
		t.Fatalf("delete organization: %v", err)
	}

	// The account is untouched. Nothing deletes an account as a side effect of
	// deleting an organization.
	if n := f.count(t,
		`SELECT count(*) FROM users WHERE id = $1 AND status = 'active' AND deleted_at IS NULL`,
		orphan.UserID); n != 1 {
		t.Fatal("the orphaned account did not survive the organization")
	}
	if n := f.count(t, `SELECT count(*) FROM memberships WHERE user_id = $1`, orphan.UserID); n != 0 {
		t.Fatalf("the orphaned account holds %d memberships, want none", n)
	}

	// It resolves, rather than erroring. This is the line every authenticated
	// request in the product passes through, which is why it is asserted here
	// and not left to a rendered page.
	id, err := f.auth.IdentityForEmail(t.Context(), "orphan@example.com")
	if err != nil {
		t.Fatalf("an account belonging to nothing failed to resolve: %v", err)
	}
	if id.OrgID != uuid.Nil || id.WorkspaceID != uuid.Nil {
		t.Errorf("the orphaned identity carries org %s / workspace %s, want neither",
			id.OrgID, id.WorkspaceID)
	}
	if id.HasOrganization() {
		t.Error("HasOrganization is true for an account with no membership")
	}
	if len(id.Permissions()) != 0 {
		t.Errorf("the orphaned identity holds %v; belonging to nothing must grant nothing",
			id.Permissions())
	}
	if id.Role != "" || id.RoleRank != auth.NoRoleRank {
		t.Errorf("the orphaned identity is %q at rank %d, want no role at NoRoleRank",
			id.Role, id.RoleRank)
	}

	// And signing in works, which is the half that would otherwise turn one
	// owner's teardown into an authentication outage for the person it orphaned.
	res, err := f.auth.Login(t.Context(), auth.LoginInput{
		Email: "orphan@example.com", Password: teamPassword,
	})
	if err != nil {
		t.Fatalf("an account belonging to nothing could not sign in: %v", err)
	}
	if res.Identity.HasOrganization() {
		t.Error("signing in conjured an organization for an account with no membership")
	}
	authed, err := f.auth.Authenticate(t.Context(), res.Token)
	if err != nil {
		t.Fatalf("the session of an account belonging to nothing does not authenticate: %v", err)
	}
	if authed.HasOrganization() || len(authed.Permissions()) != 0 {
		t.Error("the authenticated orphan carries tenancy it should not have")
	}

	// A service call refuses on the permission it always checked. There is no
	// second authorization path for this state, which is the point.
	if _, err := f.team.Members(t.Context(), authed); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an account belonging to nothing listed members: %v", err)
	}
	if _, err := f.links.List(t.Context(), authed, domain.LinkFilter{}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an account belonging to nothing listed links: %v", err)
	}
}

// The `orgs.create` seam, and the boundary either side of it.
//
// An account with no membership holds no role and therefore no orgs.create, so
// without a second door the product's own prompt would lead to a 403. The
// mechanism is a membership count read at that one call site — a check on
// present state, which is what D16 distinguished from a check on how an account
// was made. What makes it a seam rather than a hole is the boundary: the moment
// an account holds any membership at all, only the permission answers.
func TestFirstOrganizationCreationIsReachableFromBelongingToNothing(t *testing.T) {
	f := newTeamFixture(t)
	f.otherOrganization(t)

	// The boundary, established first: somebody who *is* in an organization and
	// lacks orgs.create is refused, count or no count.
	viewer := f.member(t, "viewer@example.com", "viewer")
	if viewer.Can(team.PermOrgsCreate) {
		t.Fatal("an invited viewer holds orgs.create")
	}
	if _, err := f.team.CreateOrganization(t.Context(), viewer, "Not theirs"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer with a membership created an organization: %v", err)
	}

	if err := f.team.DeleteOrganization(t.Context(), f.owner, f.owner.OrgID); err != nil {
		t.Fatalf("delete organization: %v", err)
	}

	orphan := f.identity(t, "viewer@example.com")
	if orphan.Can(team.PermOrgsCreate) {
		t.Fatal("an account with no membership holds orgs.create; the seam is " +
			"supposed to be a check at the call site, not a synthesized grant")
	}
	created, err := f.team.CreateOrganization(t.Context(), orphan, "Theirs at last")
	if err != nil {
		t.Fatalf("an account belonging to nothing could not create its first organization: %v", err)
	}

	// Provisioned whole, and the permission takes over from here: they are now an
	// owner, so the count is never consulted again.
	acting := f.identityIn(t, "viewer@example.com", created.WorkspaceID)
	if acting.Role != "owner" || !acting.Can(team.PermOrgsCreate) {
		t.Fatalf("the first organization's creator is %q holding %v, want an owner "+
			"with orgs.create", acting.Role, acting.Permissions())
	}
	if n := f.count(t, `SELECT count(*) FROM memberships WHERE user_id = $1`, orphan.UserID); n != 1 {
		t.Errorf("the account holds %d memberships, want the one it just made", n)
	}
}

// The audit trail outlives the organization it describes.
//
// `audit_logs.organization_id` carries no foreign key, which is what makes this
// true rather than anything the deletion does — so it is asserted rather than
// reviewed, because a later migration adding that key would erase the record of
// every teardown and nothing else would notice.
func TestTheAuditTrailSurvivesTheOrganizationItDescribes(t *testing.T) {
	f := newTeamFixture(t)
	other := f.otherOrganization(t)

	// Some history to outlive the organization, written the way it normally is.
	ws, err := f.team.CreateWorkspace(t.Context(), f.owner, "Doomed")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.team.DeleteWorkspace(t.Context(), f.owner, ws.ID); err != nil {
		t.Fatal(err)
	}
	before := f.count(t, `SELECT count(*) FROM audit_logs WHERE organization_id = $1`, f.owner.OrgID)
	if before == 0 {
		t.Fatal("nothing was recorded before the deletion; the test would prove nothing")
	}

	if err := f.team.DeleteOrganization(t.Context(), f.owner, f.owner.OrgID); err != nil {
		t.Fatalf("delete organization: %v", err)
	}

	if n := f.count(t, `SELECT count(*) FROM organizations WHERE id = $1`, f.owner.OrgID); n != 0 {
		t.Fatal("the organization row survived its deletion")
	}
	if n := f.count(t, `SELECT count(*) FROM audit_logs WHERE organization_id = $1`, f.owner.OrgID); n != before+1 {
		t.Errorf("the deleted organization has %d audit records, want the %d it had "+
			"plus the deletion; a tenancy teardown must not erase its own record",
			n, before)
	}

	// The deletion record names what was deleted, because the row that held the
	// name is gone and this is the only remaining trace of it.
	if n := f.count(t, `
		SELECT count(*) FROM audit_logs
		 WHERE action = $1 AND target_type = 'organization' AND target_id = $2
		   AND organization_id = $2 AND actor_label = 'owner@example.com'
		   AND metadata->>'name' = 'Owner'`,
		audit.ActionOrganizationDeleted, f.owner.OrgID); n != 1 {
		t.Error("the organization deletion record is missing, or does not name what was deleted")
	}

	// And nothing was recorded against the organization that was not deleted.
	if n := f.count(t, `SELECT count(*) FROM audit_logs WHERE organization_id = $1 AND action = $2`,
		other.OrgID, audit.ActionOrganizationDeleted); n != 0 {
		t.Error("the deletion was recorded against an organization that still exists")
	}
}

// What the cascade takes, and what it leaves.
//
// Asserted table by table rather than summarized, because this is the first
// operation in the product that deletes rows Phase 1's schema only ever
// inserted: what the foreign keys actually do on delete is a fact about the tree
// and not an assumption to build on.
func TestDeletingAnOrganizationTakesItsTenancyAndNothingElse(t *testing.T) {
	f := newTeamFixture(t)
	other := f.otherOrganization(t)
	member := f.member(t, "member@example.com", "editor")

	if _, err := f.team.CreateWorkspace(t.Context(), f.owner, "Second"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.invites.Create(t.Context(), f.owner,
		invite.CreateInput{Email: "pending@example.com", Role: "viewer"}); err != nil {
		t.Fatal(err)
	}
	key, err := f.keys.Create(t.Context(), f.owner, auth.CreateAPIKeyInput{
		Name: "doomed", Scopes: []string{"links.read"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := f.team.DeleteOrganization(t.Context(), f.owner, f.owner.OrgID); err != nil {
		t.Fatalf("delete organization: %v", err)
	}

	for _, gone := range []struct {
		what, sql string
	}{
		{"workspaces", `SELECT count(*) FROM workspaces WHERE organization_id = $1`},
		{"memberships", `SELECT count(*) FROM memberships WHERE organization_id = $1`},
		{"invitations", `SELECT count(*) FROM invitations WHERE organization_id = $1`},
		{"api keys", `SELECT count(*) FROM api_keys WHERE organization_id = $1`},
	} {
		if n := f.count(t, gone.sql, f.owner.OrgID); n != 0 {
			t.Errorf("%d %s survived the organization", n, gone.what)
		}
	}

	// The key is dead as a credential too, not merely as a row.
	if _, err := f.keys.Authenticate(t.Context(), key.Key); err == nil {
		t.Error("an API key issued in a deleted organization still authenticates")
	}

	// The accounts survive, both of them, and so does the instance's default
	// domain — it belongs to no organization, so the aliases reserved against it
	// are untouched by any organization being deleted.
	for _, email := range []string{"owner@example.com", "member@example.com"} {
		if n := f.count(t, `SELECT count(*) FROM users WHERE email_lower = lower($1)`, email); n != 1 {
			t.Errorf("the account %s did not survive", email)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM domains WHERE organization_id IS NULL`); n != 1 {
		t.Error("the instance default domain was taken with the organization")
	}
	_ = member

	// And the other organization is entirely untouched, which is what makes the
	// cascade a tenancy boundary rather than a blast radius.
	if n := f.count(t, `SELECT count(*) FROM memberships WHERE organization_id = $1`, other.OrgID); n != 1 {
		t.Error("deleting one organization reached into another")
	}
	if _, err := f.auth.IdentityForEmail(t.Context(), "elsewhere@example.com"); err != nil {
		t.Errorf("the other organization's owner stopped resolving: %v", err)
	}
}

// ─── the dashboard, for an account that belongs to nothing ──────────────────

// browser brings up the full HTML surface and drives it the way one does: a
// cookie jar, form posts, and no transparent redirect following, because the
// redirects are the assertions.
func (f *teamFixture) browser(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	cfg := config.Config{
		AppEnv: config.Development, BaseURL: "http://links.test", SecureCookies: false,
	}
	cfg.Auth.SignupMode = config.SignupInvite
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour

	renderer, err := ui.New()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	notifySvc := notify.NewService(f.pool)
	stats := analytics.NewReader(f.pool)
	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config: cfg, Auth: f.auth, Keys: f.keys, Links: f.links, Stats: stats,
		Notify: notifySvc, Invites: f.invites, Team: f.team,
		Web: &httpx.Web{
			UI: renderer, Config: cfg, Auth: f.auth, Keys: f.keys,
			Links: f.links, Stats: stats, Notify: notifySvc,
			Invites: f.invites, Team: f.team,
		},
	}))
	t.Cleanup(srv.Close)

	jar, err := newCookieJar()
	if err != nil {
		t.Fatal(err)
	}
	return srv, &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// D36's dashboard half: the account is prompted to create an organization, and
// can take no other action until it has one.
//
// Asserted across every dashboard route the router mounts rather than on the one
// page that does the prompting, because the failure this guards against is a
// page that renders for a workspace which does not exist — and that is a
// different page every time somebody adds one. The list below is the whole
// signed-in surface; a route added without RequireOrganization fails here.
func TestAnAccountThatBelongsToNothingIsPromptedForAnOrganization(t *testing.T) {
	f := newTeamFixture(t)
	f.otherOrganization(t)
	f.member(t, "orphan@example.com", "editor")
	if err := f.team.DeleteOrganization(t.Context(), f.owner, f.owner.OrgID); err != nil {
		t.Fatalf("delete organization: %v", err)
	}

	srv, client := f.browser(t)
	send := func(method, path string, form url.Values) *http.Response {
		t.Helper()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	read := func(resp *http.Response) string {
		t.Helper()
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	redirectsToPrompt := func(method, path string, form url.Values) {
		t.Helper()
		resp := send(method, path, form)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("%s %s = %d, want 303 to the organization prompt", method, path, resp.StatusCode)
			return
		}
		if loc := resp.Header.Get("Location"); loc != "/organizations/new" {
			t.Errorf("%s %s redirected to %q, want /organizations/new", method, path, loc)
		}
	}

	// Signing in works, and lands where anybody else would — which is what makes
	// the next assertion about the destination rather than about the sign-in.
	login := send(http.MethodPost, "/login", url.Values{
		"email": {"orphan@example.com"}, "password": {teamPassword},
	})
	defer func() { _ = login.Body.Close() }()
	if login.StatusCode != http.StatusSeeOther {
		t.Fatalf("an account belonging to nothing could not sign in: %d\n%s",
			login.StatusCode, read(login))
	}

	// Every signed-in surface, refused. Reads and writes both: a page that
	// rendered would be rendering a workspace that does not exist, and a write
	// that reached its service would be refused there with a 403 page instead of
	// the offer this state is supposed to end in.
	for _, path := range []string{
		"/dashboard", "/links", "/links/" + uuid.Must(uuid.NewV7()).String(),
		"/keys", "/account", "/notifications", "/members", "/invites", "/workspaces",
	} {
		redirectsToPrompt(http.MethodGet, path, nil)
	}
	for _, post := range []struct {
		path string
		form url.Values
	}{
		{"/links", url.Values{"url": {"https://example.com"}}},
		{"/keys", url.Values{"name": {"nope"}}},
		{"/members", url.Values{"role": {"admin"}}},
		{"/invites", url.Values{"email": {"someone@example.com"}}},
		{"/workspaces", url.Values{"name": {"Nope"}}},
		{"/account/password", url.Values{"current_password": {teamPassword}}},
		{"/workspace/switch", url.Values{"workspace_id": {uuid.Must(uuid.NewV7()).String()}}},
	} {
		redirectsToPrompt(http.MethodPost, post.path, post.form)
	}

	// The one page that does render, and the one action it offers.
	prompt := send(http.MethodGet, "/organizations/new", nil)
	if prompt.StatusCode != http.StatusOK {
		t.Fatalf("the organization prompt returned %d", prompt.StatusCode)
	}
	page := read(prompt)
	for _, want := range []string{
		"You are not in an organization",
		`action="/organizations"`,
		`action="/logout"`, // the other thing they must always be able to do
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the prompt page is missing %q", want)
		}
	}

	// A refusal comes back on the prompt rather than on a page this reader
	// cannot load: re-rendering the workspaces page would need workspace.read,
	// which an account with no membership does not hold.
	blank := send(http.MethodPost, "/organizations", url.Values{"name": {"  "}})
	if blank.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a blank organization name returned %d, want 422", blank.StatusCode)
	}
	if body := read(blank); !strings.Contains(body, "You are not in an organization") {
		t.Error("the refusal did not come back on the prompt page")
	}

	// And creating one ends the state.
	created := send(http.MethodPost, "/organizations", url.Values{"name": {"Fresh"}})
	defer func() { _ = created.Body.Close() }()
	if created.StatusCode != http.StatusSeeOther {
		t.Fatalf("creating the first organization returned %d\n%s",
			created.StatusCode, read(created))
	}
	if loc := created.Header.Get("Location"); loc != "/workspaces?done=organization" {
		t.Errorf("after creating an organization the browser went to %q", loc)
	}
	dash := send(http.MethodGet, "/dashboard", nil)
	defer func() { _ = dash.Body.Close() }()
	if dash.StatusCode != http.StatusOK {
		t.Errorf("the dashboard is still refused after creating an organization: %d", dash.StatusCode)
	}
	// The prompt turns away somebody who now belongs somewhere, so it cannot
	// become a second, permanent route to the same form.
	again := send(http.MethodGet, "/organizations/new", nil)
	defer func() { _ = again.Body.Close() }()
	if loc := again.Header.Get("Location"); again.StatusCode != http.StatusSeeOther || loc != "/workspaces" {
		t.Errorf("the prompt answered a member with %d to %q, want 303 to /workspaces",
			again.StatusCode, loc)
	}
}

// The dashboard's own path to deletion, and the confirmation it is behind.
func TestTheDashboardDeletesAnOrganization(t *testing.T) {
	f := newTeamFixture(t)
	f.otherOrganization(t)

	srv, client := f.browser(t)
	form := func(path string, vals url.Values) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			srv.URL+path, strings.NewReader(vals.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	login := form("/login", url.Values{
		"email": {"owner@example.com"}, "password": {teamPassword},
	})
	_ = login.Body.Close()

	// The control is on the workspaces page, names what it will destroy, and
	// says the refusal before it happens rather than after.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/workspaces", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	if !strings.Contains(page, "/organizations/"+f.owner.OrgID.String()+"/delete") {
		t.Fatal("the workspaces page offers no way to delete the organization")
	}
	if !strings.Contains(page, "hx-confirm=") || !strings.Contains(page, "cannot be undone") {
		t.Error("the deletion control is not behind a confirmation that says it is irreversible")
	}

	deleted := form("/organizations/"+f.owner.OrgID.String()+"/delete", nil)
	defer func() { _ = deleted.Body.Close() }()
	if deleted.StatusCode != http.StatusSeeOther {
		t.Fatalf("deleting the organization from the dashboard returned %d", deleted.StatusCode)
	}
	if n := f.count(t, `SELECT count(*) FROM organizations WHERE id = $1`, f.owner.OrgID); n != 0 {
		t.Error("the organization survived the form post")
	}
	// The owner belonged to nothing else, so the browser lands on the prompt by
	// the ordinary route rather than by anything the handler worked out.
	next, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		srv.URL+deleted.Header.Get("Location"), nil)
	if err != nil {
		t.Fatal(err)
	}
	followed, err := client.Do(next)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = followed.Body.Close() }()
	if loc := followed.Header.Get("Location"); loc != "/organizations/new" {
		t.Errorf("after deleting their only organization the owner went to %q", loc)
	}
}
