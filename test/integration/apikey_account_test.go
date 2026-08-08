//go:build integration

package integration

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/invite"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// M54 — a key belongs to an account, not to one organization.
//
// Everything here needs one person holding memberships in two organizations at
// two different roles, which is a shape no fixture in this package had: M44's
// tests bound a key to one tenant and asserted it stayed there, and that was
// the whole model. The assertions below are about what happens when it does
// not.

// joinOrganization gives an existing account an organization-wide membership in
// a **new** organization, at a named role, and returns the organization and its
// workspace.
//
// addOrgWithWorkspace next door does the same thing at the owner role only,
// which is exactly the case that cannot show what this milestone is about: an
// account-wide key intersected against owner in both tenants behaves identically
// in both, so a test built on it would pass whatever the intersection did.
func joinOrganization(
	t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, orgName, wsName, role string,
) (orgID, wsID uuid.UUID) {
	t.Helper()
	orgID = uuid.Must(uuid.NewV7())
	wsID = uuid.Must(uuid.NewV7())

	if _, err := pool.Exec(t.Context(),
		`INSERT INTO organizations (id, name, slug, is_personal) VALUES ($1, $2, $3, false)`,
		orgID, orgName, strings.ToLower(orgName)+"-"+orgID.String()[:8]); err != nil {
		t.Fatalf("create organization %s: %v", orgName, err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO workspaces (id, organization_id, name, slug) VALUES ($1, $2, $3, $4)`,
		wsID, orgID, wsName, strings.ToLower(wsName)+"-"+wsID.String()[:8]); err != nil {
		t.Fatalf("create workspace %s: %v", wsName, err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO memberships (id, user_id, organization_id, role_id, workspace_id)
		 VALUES ($1, $2, $3, (SELECT id FROM roles WHERE slug = $4 AND organization_id IS NULL), NULL)`,
		uuid.Must(uuid.NewV7()), userID, orgID, role); err != nil {
		t.Fatalf("create %s membership in %s: %v", role, orgName, err)
	}
	return orgID, wsID
}

// actIn points the account's last-used workspace at one it belongs to, which is
// the rung an account-wide key resolves on when nothing is pinned.
//
// Through the column the switcher writes, not through a parameter: what a key
// follows is the person, and this is where the person's current choice lives.
func actIn(t *testing.T, pool *pgxpool.Pool, userID, workspaceID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE users SET last_workspace_id = $2, default_workspace_id = NULL WHERE id = $1`,
		userID, workspaceID); err != nil {
		t.Fatalf("point the account at workspace %s: %v", workspaceID, err)
	}
}

// accountKeys is the two-organization fixture: owner@example.com owns Acme and
// is a **viewer** in Beta.
type accountKeys struct {
	*teamFixture
	betaOrg uuid.UUID
	betaWS  uuid.UUID
	acmeWS  uuid.UUID
}

func newAccountKeys(t *testing.T, roleInBeta string) *accountKeys {
	t.Helper()
	f := newTeamFixture(t)
	betaOrg, betaWS := joinOrganization(t, f.pool, f.owner.UserID, "Beta", "Beta Space", roleInBeta)
	return &accountKeys{
		teamFixture: f, betaOrg: betaOrg, betaWS: betaWS, acmeWS: f.owner.WorkspaceID,
	}
}

// accountWideKey mints a key with no organization of its own.
func (a *accountKeys) accountWideKey(t *testing.T, name string, scopes ...string) *auth.CreatedAPIKey {
	t.Helper()
	key, err := a.keys.Create(t.Context(), a.owner, auth.CreateAPIKeyInput{
		Name: name, Scopes: scopes, OrgWide: true,
	})
	if err != nil {
		t.Fatalf("mint the account-wide key %q: %v", name, err)
	}
	if key.OrganizationID != nil {
		t.Fatalf("a key minted without an organization is pinned to %s", key.OrganizationID)
	}
	return key
}

// pinnedKey mints a key pinned to the organization the owner is acting in,
// which is what every key issued before this milestone is.
func (a *accountKeys) pinnedKey(t *testing.T, name string, scopes ...string) *auth.CreatedAPIKey {
	t.Helper()
	org := a.owner.OrgID
	key, err := a.keys.Create(t.Context(), a.owner, auth.CreateAPIKeyInput{
		Name: name, Scopes: scopes, OrgWide: true, OrganizationID: &org,
	})
	if err != nil {
		t.Fatalf("mint the pinned key %q: %v", name, err)
	}
	if key.OrganizationID == nil || *key.OrganizationID != org {
		t.Fatalf("the pinned key came back with organization %v, want %s", key.OrganizationID, org)
	}
	return key
}

// ─── the intersection, per organization ─────────────────────────────────────

// The milestone's central claim, and the one m54.md names as having no
// precedent in the tree: the same key is more powerful in one organization than
// in another.
//
// The scopes are constant — they are stored on the row and never change — and
// what varies is the authority they are intersected against. Owner in Acme
// holds links.create; viewer in Beta does not. So the identical credential
// creates links in one tenant and is refused in the other, with nothing about
// the key having been touched in between.
func TestAnAccountWideKeyIsWeakerWhereItsOwnerIsWeaker(t *testing.T) {
	a := newAccountKeys(t, "viewer")
	key := a.accountWideKey(t, "two-tenants", "links.read", "links.create")

	actIn(t, a.pool, a.owner.UserID, a.acmeWS)
	inAcme, err := a.keys.Authenticate(t.Context(), key.Key)
	if err != nil {
		t.Fatalf("the key did not authenticate in Acme: %v", err)
	}
	if inAcme.OrgID != a.owner.OrgID {
		t.Fatalf("the key resolved into organization %s, want Acme (%s)", inAcme.OrgID, a.owner.OrgID)
	}
	if !inAcme.Can(link.PermCreate) {
		t.Errorf("the key holds no links.create in Acme, where its owner is an owner")
	}

	actIn(t, a.pool, a.owner.UserID, a.betaWS)
	inBeta, err := a.keys.Authenticate(t.Context(), key.Key)
	if err != nil {
		t.Fatalf("the key did not authenticate in Beta: %v", err)
	}
	if inBeta.OrgID != a.betaOrg {
		t.Fatalf("the key resolved into organization %s, want Beta (%s)", inBeta.OrgID, a.betaOrg)
	}
	if !inBeta.Can(link.PermRead) {
		t.Errorf("the key lost links.read in Beta, where its owner is a viewer who holds it")
	}
	if inBeta.Can(link.PermCreate) {
		t.Error("the key holds links.create in Beta, where its owner is a viewer. " +
			"The stored scopes are not the authority — they are intersected with " +
			"the role that applies where the request landed")
	}

	// The other half of the same sentence, stated by m54.md as a consequence to
	// document rather than to discover: a demotion narrows the key without the
	// key being touched. Acme's owner becomes a viewer there, and the credential
	// that could create links a moment ago cannot.
	if _, err := a.pool.Exec(t.Context(), `
		UPDATE memberships
		   SET role_id = (SELECT id FROM roles WHERE slug = 'viewer' AND organization_id IS NULL)
		 WHERE user_id = $1 AND organization_id = $2`,
		a.owner.UserID, a.owner.OrgID); err != nil {
		t.Fatal(err)
	}
	actIn(t, a.pool, a.owner.UserID, a.acmeWS)
	demoted, err := a.keys.Authenticate(t.Context(), key.Key)
	if err != nil {
		t.Fatalf("the key stopped authenticating after a demotion: %v", err)
	}
	if demoted.Can(link.PermCreate) {
		t.Error("the key kept links.create in Acme after its owner was demoted to viewer")
	}
}

// The decision m54.md calls the one most likely to be disagreed with, asserted
// so that disagreeing with it means changing a test rather than noticing a
// behaviour.
//
// The key is minted while Beta does not exist. Its reach is not a snapshot of
// the memberships that existed then, because there is no way to see such a
// snapshot and no way to correct it — so it follows the account, and somebody
// who wants the snapshot pins the key. The next test is the pinning.
func TestAnAccountWideKeyReachesAnOrganizationJoinedAfterIt(t *testing.T) {
	f := newTeamFixture(t)
	key, err := f.keys.Create(t.Context(), f.owner, auth.CreateAPIKeyInput{
		Name: "minted-first", Scopes: []string{"links.read"}, OrgWide: true,
	})
	if err != nil {
		t.Fatalf("mint the key: %v", err)
	}

	// Only now does the second organization exist.
	betaOrg, betaWS := joinOrganization(t, f.pool, f.owner.UserID, "Beta", "Beta Space", "editor")
	actIn(t, f.pool, f.owner.UserID, betaWS)

	id, err := f.keys.Authenticate(t.Context(), key.Key)
	if err != nil {
		t.Fatalf("the key did not authenticate: %v", err)
	}
	if id.OrgID != betaOrg {
		t.Errorf("the key resolved into %s, want the organization its owner joined "+
			"after it was minted (%s). Account-wide means the account's "+
			"organizations, not the ones it had on the day", id.OrgID, betaOrg)
	}
}

// The counterweight, and the reason pinning stays available: a pinned key does
// not widen when its owner joins a second organization, and no key that existed
// before this milestone changed reach on the day of the migration.
//
// Every row written before 04200 carries an organization, so "a key minted
// before the migration" and "a pinned key" are the same row. That is what makes
// this assertion the migration's as well as the model's.
func TestAPinnedKeyDoesNotFollowItsOwnerIntoASecondOrganization(t *testing.T) {
	a := newAccountKeys(t, "owner")
	key := a.pinnedKey(t, "stays-put", "links.read")

	before, err := a.keys.Authenticate(t.Context(), key.Key)
	if err != nil {
		t.Fatalf("the pinned key did not authenticate: %v", err)
	}

	// The owner moves to the other tenant. A key that followed the person
	// wholesale would move with them; this one is a snapshot by construction.
	actIn(t, a.pool, a.owner.UserID, a.betaWS)
	after, err := a.keys.Authenticate(t.Context(), key.Key)
	if err != nil {
		t.Fatalf("the pinned key stopped authenticating: %v", err)
	}
	if after.OrgID != a.owner.OrgID {
		t.Errorf("a pinned key resolved into %s after its owner moved to Beta, "+
			"want its own organization %s", after.OrgID, a.owner.OrgID)
	}
	if after.WorkspaceID != before.WorkspaceID {
		t.Errorf("a pinned key resolved into workspace %s, want the %s it resolved "+
			"into before its owner joined anything",
			after.WorkspaceID, before.WorkspaceID)
	}
}

// The refusal m54.md carries over from apikey.go's existing one: no membership
// where the request would land is ErrAPIKeyInvalid rather than an identity
// holding nothing.
//
// An account-wide key needs an **organization-wide** membership, which is the
// same bar an unpinned key has always had — MayCreateOrgWide refuses to mint one
// without it. Carrying that across the tenancy boundary is what stops a key
// minted under organization-wide authority acquiring reach into a tenant where
// its owner is scoped to a single workspace.
func TestAnAccountWideKeyNeedsAnOrganizationWideMembershipWhereItLands(t *testing.T) {
	a := newAccountKeys(t, "editor")
	key := a.accountWideKey(t, "needs-a-membership", "links.read")

	// Narrowed to the one workspace, which leaves the account a member of Beta
	// and leaves this key unable to reach it.
	if _, err := a.pool.Exec(t.Context(),
		`UPDATE memberships SET workspace_id = $2 WHERE user_id = $1 AND organization_id = $3`,
		a.owner.UserID, a.betaWS, a.betaOrg); err != nil {
		t.Fatal(err)
	}
	actIn(t, a.pool, a.owner.UserID, a.betaWS)

	id, err := a.keys.Authenticate(t.Context(), key.Key)
	if err != nil {
		t.Fatalf("the key stopped authenticating altogether: %v", err)
	}
	if id.OrgID == a.betaOrg {
		t.Error("an account-wide key landed in an organization where its owner holds " +
			"only a workspace-scoped membership. An unpinned key has always " +
			"required an organization-wide one")
	}

	// And with every organization-wide membership gone, the credential resolves
	// to no tenancy at all and is invalid rather than powerless.
	if _, err := a.pool.Exec(t.Context(),
		`UPDATE memberships SET workspace_id = $2 WHERE user_id = $1 AND organization_id = $3`,
		a.owner.UserID, a.acmeWS, a.owner.OrgID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.keys.Authenticate(t.Context(), key.Key); !errors.Is(err, auth.ErrAPIKeyInvalid) {
		t.Errorf("a key whose owner holds no organization-wide membership anywhere "+
			"authenticated: %v", err)
	}
}

// ─── D87's second axis ──────────────────────────────────────────────────────

// Rotation is authorized by the key's own token, which is only safe while a
// successor is identical or narrower. Tenancy is the axis M54 adds, and both
// directions are asserted because only one of them is a refusal.
func TestARotationMayPinAKeyButNeverUnpinOne(t *testing.T) {
	a := newAccountKeys(t, "owner")

	// Narrowing: account-wide into pinned. Allowed, and the successor carries the
	// organization the request resolved into.
	wide := a.accountWideKey(t, "narrowing", "links.read")
	actor, err := a.keys.Authenticate(t.Context(), wide.Key)
	if err != nil {
		t.Fatalf("authenticate as the account-wide key: %v", err)
	}
	narrowed, err := a.keys.Rotate(t.Context(), actor, auth.RotateAPIKeyInput{
		Reach: auth.ReachOrganization,
	})
	if err != nil {
		t.Fatalf("an account-wide key could not rotate into a pinned one: %v", err)
	}
	if narrowed.OrganizationID == nil {
		t.Fatal("the successor came back account-wide after a rotation that asked to pin it")
	}
	if *narrowed.OrganizationID != actor.OrgID {
		t.Errorf("the successor is pinned to %s, want the organization the rotation "+
			"request resolved into (%s)", *narrowed.OrganizationID, actor.OrgID)
	}

	// Widening: pinned into account-wide. Refused, and refused as a field error
	// rather than as a silent copy — an automated rotation that asked for
	// something it may not have should be told so.
	pinnedActor, err := a.keys.Authenticate(t.Context(), narrowed.Key)
	if err != nil {
		t.Fatalf("authenticate as the pinned successor: %v", err)
	}
	_, err = a.keys.Rotate(t.Context(), pinnedActor, auth.RotateAPIKeyInput{
		Reach: auth.ReachAccount,
	})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("a pinned key rotated into an account-wide one: %v", err)
	}
	if len(ve) != 1 || ve[0].Field != "reach" || ve[0].Code != "would_widen" {
		t.Errorf("the refusal was %+v, want one would_widen error on the reach field", ve)
	}
	// The row, not the error: a refusal that writes anyway passes the assertion
	// above.
	if n := a.count(t, `SELECT count(*) FROM api_keys WHERE organization_id IS NULL
	                     AND successor_id IS NULL AND revoked_at IS NULL`); n != 0 {
		t.Errorf("%d live account-wide keys exist after the refused rotation, want 0", n)
	}

	// And saying nothing copies the reach verbatim, which is what an unattended
	// rotation sends and what every rotation did before this milestone.
	quiet, err := a.keys.Rotate(t.Context(), pinnedActor, auth.RotateAPIKeyInput{})
	if err != nil {
		t.Fatalf("a rotation with no reach was refused: %v", err)
	}
	if quiet.OrganizationID == nil || *quiet.OrganizationID != *narrowed.OrganizationID {
		t.Errorf("a rotation that named no reach produced %v, want the predecessor's %s",
			quiet.OrganizationID, *narrowed.OrganizationID)
	}
}

// ─── F75, dissolved ─────────────────────────────────────────────────────────

// The finding was that revoking was owner-scoped and listing was owner-and-
// organization-scoped, so a key invisible in the list still answered 204 to a
// revoke — a weak existence oracle over guessed ids.
//
// Both statements ask the same question now. Asserted on the list rather than on
// the oracle, because the oracle was the symptom: two statements that agree
// cannot disagree about anything.
func TestRevokingAndListingReachTheSameKeys(t *testing.T) {
	a := newAccountKeys(t, "owner")

	// A key pinned to Beta, minted while acting there, is the row the old list
	// could not see from Acme.
	actIn(t, a.pool, a.owner.UserID, a.betaWS)
	inBeta := a.identity(t, "owner@example.com")
	if inBeta.OrgID != a.betaOrg {
		t.Fatalf("the premise did not hold: the owner resolved into %s, want Beta", inBeta.OrgID)
	}
	betaKey, err := a.keys.Create(t.Context(), inBeta, auth.CreateAPIKeyInput{
		Name: "betas", Scopes: []string{"links.read"},
	})
	if err != nil {
		t.Fatalf("mint a key in Beta: %v", err)
	}

	// Back in Acme, and the question is whether the two statements agree about
	// that key.
	actIn(t, a.pool, a.owner.UserID, a.acmeWS)
	fromAcme := a.identity(t, "owner@example.com")
	if fromAcme.OrgID != a.owner.OrgID {
		t.Fatalf("the premise did not hold: the owner resolved into %s, want Acme", fromAcme.OrgID)
	}

	listed, err := a.keys.List(t.Context(), fromAcme)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	var seen bool
	for _, k := range listed {
		if k.ID == betaKey.ID {
			seen = true
		}
	}
	if !seen {
		t.Error("a key the owner can revoke from here is absent from the list they " +
			"see from here. That disagreement is F75, and closing it is what makes " +
			"204-versus-404 stop answering \"my key, elsewhere\"")
	}
	if err := a.keys.Revoke(t.Context(), fromAcme, betaKey.ID); err != nil {
		t.Errorf("revoking a key the list showed was refused: %v", err)
	}
}

// ─── the administrator's arm, and the reach revoke ──────────────────────────

// m54.md's rule for revokeInOrganization: an administrator may revoke a key's
// reach into their organization, and may not revoke a key belonging to somebody
// else's account outright. The two have to be distinguishable, and here they are
// distinguished by what survives and by which record is written.
func TestAnAdministratorCutsTheirOrganizationOutOfAnAccountWideKey(t *testing.T) {
	f := newTeamFixture(t)
	bob := f.member(t, "bob-account-wide@example.com", "admin")

	// Bob also belongs to Beta, where he is an owner, and mints one key that
	// reaches both.
	betaOrg, betaWS := joinOrganization(t, f.pool, bob.UserID, "Beta", "Beta Space", "owner")
	key, err := f.keys.Create(t.Context(), bob, auth.CreateAPIKeyInput{
		Name: "bobs-account-key", Scopes: []string{"links.read"}, OrgWide: true,
	})
	if err != nil {
		t.Fatalf("mint bob's account-wide key: %v", err)
	}

	// Acme's owner cuts Acme out of it. The key is not destroyed — that is the
	// whole distinction — so the row survives with no revoked_at.
	if err := f.keys.Revoke(t.Context(), f.owner, key.ID); err != nil {
		t.Fatalf("an organization-wide owner could not cut their organization out "+
			"of an account-wide key: %v", err)
	}
	if n := f.count(t,
		`SELECT count(*) FROM api_keys WHERE id = $1 AND revoked_at IS NOT NULL`, key.ID); n != 0 {
		t.Error("an administrator destroyed a key belonging to somebody else's account. " +
			"They may cut their own organization out of its reach and no more")
	}
	if n := f.count(t, `SELECT count(*) FROM api_key_org_revocations
	                     WHERE api_key_id = $1 AND organization_id = $2`,
		key.ID, f.owner.OrgID); n != 1 {
		t.Fatalf("%d reach revocations name Acme, want 1", n)
	}

	// Distinguishable in the log, which is where an operator asks "was that key
	// stopped" afterwards and needs the answer "not everywhere".
	if n := f.count(t, `
		SELECT count(*) FROM audit_logs
		 WHERE action = 'apikey.reach_revoked' AND target_id = $1
		   AND organization_id = $2 AND metadata->>'owner_id' = $3`,
		key.ID, f.owner.OrgID, bob.UserID.String()); n != 1 {
		t.Errorf("%d apikey.reach_revoked records name the key in Acme, want 1", n)
	}
	if n := f.count(t,
		`SELECT count(*) FROM audit_logs WHERE action = 'apikey.revoked' AND target_id = $1`,
		key.ID); n != 0 {
		t.Error("a reach revocation was recorded as an outright revoke, which is the " +
			"one thing the two records must not do")
	}

	// Behaviour, not only rows: the key resolves into Beta and no longer into
	// Acme, wherever its owner happens to be acting.
	actIn(t, f.pool, bob.UserID, betaWS)
	id, err := f.keys.Authenticate(t.Context(), key.Key)
	if err != nil {
		t.Fatalf("the key stopped working everywhere after one organization cut it: %v", err)
	}
	if id.OrgID != betaOrg {
		t.Errorf("the key resolved into %s, want Beta (%s)", id.OrgID, betaOrg)
	}
	actIn(t, f.pool, bob.UserID, f.owner.WorkspaceID)
	back, err := f.keys.Authenticate(t.Context(), key.Key)
	if err != nil {
		t.Fatalf("the key stopped authenticating: %v", err)
	}
	if back.OrgID == f.owner.OrgID {
		t.Error("the key resolved back into the organization that cut it out, " +
			"because its owner pointed at a workspace there")
	}

	// Idempotent, for the reason revoking twice is: two administrators reacting
	// to one incident is the normal case.
	if err := f.keys.Revoke(t.Context(), f.owner, key.ID); err != nil {
		t.Errorf("a second reach revocation was refused: %v", err)
	}

	// And a key whose owner reaches nothing here is not found, so the endpoint
	// still discloses nothing about ids the actor may not act on.
	stranger := f.otherOrganization(t)
	theirs, err := f.keys.Create(t.Context(), stranger, auth.CreateAPIKeyInput{
		Name: "strangers", Scopes: []string{"links.read"}, OrgWide: true,
	})
	if err != nil {
		t.Fatalf("mint the stranger's key: %v", err)
	}
	if err := f.keys.Revoke(t.Context(), f.owner, theirs.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("an owner cut their organization out of a key that never reached "+
			"it: %v", err)
	}
}

// ─── D43, re-derived per organization ───────────────────────────────────────

// D43 caps the role a key-issued invitation may carry, and the cap is against
// the owner's role **there**. A key whose owner is an owner in Acme and a
// viewer in Beta must not manufacture an interactive principal in Beta at all,
// let alone one above what its owner holds.
//
// Nothing in the invitation path changed for this. What changed is that
// actor.OrgID can now differ between two requests made with one credential, so
// the check that was already per-organization has a second organization to be
// wrong in — and that is exactly the kind of thing no test would have caught
// while there was only one.
func TestAKeyIssuedInvitationIsCappedWhereTheRequestLanded(t *testing.T) {
	a := newAccountKeys(t, "viewer")
	key := a.accountWideKey(t, "inviter", "members.write", "members.read")

	// In Acme the owner holds members.write, so the key does too — capped at
	// editor by D43, which is the rule this test must not accidentally re-assert.
	actIn(t, a.pool, a.owner.UserID, a.acmeWS)
	inAcme, err := a.keys.Authenticate(t.Context(), key.Key)
	if err != nil {
		t.Fatalf("the key did not authenticate in Acme: %v", err)
	}
	if _, err := a.invites.Create(t.Context(), inAcme, invite.CreateInput{
		Email: "capped@example.com", Role: "admin",
	}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a key issued an admin invitation in Acme: %v", err)
	}
	if _, err := a.invites.Create(t.Context(), inAcme, invite.CreateInput{
		Email: "allowed@example.com", Role: "editor",
	}); err != nil {
		t.Fatalf("a key could not issue the editor invitation D43 permits: %v", err)
	}

	// In Beta the owner is a viewer and holds no members.write at all, so the
	// intersection leaves the key with none — the cap is never reached because
	// the permission is not there to cap.
	actIn(t, a.pool, a.owner.UserID, a.betaWS)
	inBeta, err := a.keys.Authenticate(t.Context(), key.Key)
	if err != nil {
		t.Fatalf("the key did not authenticate in Beta: %v", err)
	}
	if _, err := a.invites.Create(t.Context(), inBeta, invite.CreateInput{
		Email: "elsewhere@example.com", Role: "editor",
	}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a key issued an invitation into an organization where its owner is "+
			"a viewer: %v", err)
	}
	if n := a.count(t,
		`SELECT count(*) FROM invitations WHERE organization_id = $1`, a.betaOrg); n != 0 {
		t.Errorf("%d invitations were written into Beta by a key whose owner cannot "+
			"invite there", n)
	}
}

// ─── audit attribution ──────────────────────────────────────────────────────

// One key, two organizations, two differently-attributed records. The
// attribution already worked — every record carries the organization it was
// written in — and what makes it worth asserting is that the answer now varies
// per request for a single credential, which it never could before.
func TestOneKeyThroughTwoOrganizationsIsAuditedInBoth(t *testing.T) {
	a := newAccountKeys(t, "owner")
	key := a.accountWideKey(t, "auditable", "members.write", "members.read")

	for _, where := range []struct {
		ws    uuid.UUID
		org   uuid.UUID
		email string
	}{
		{a.acmeWS, a.owner.OrgID, "in-acme@example.com"},
		{a.betaWS, a.betaOrg, "in-beta@example.com"},
	} {
		actIn(t, a.pool, a.owner.UserID, where.ws)
		id, err := a.keys.Authenticate(t.Context(), key.Key)
		if err != nil {
			t.Fatalf("the key did not authenticate: %v", err)
		}
		if id.OrgID != where.org {
			t.Fatalf("the key landed in %s, want %s", id.OrgID, where.org)
		}
		// Any audited act would do; an invitation is the cheapest one a key with
		// these scopes can perform in both tenants.
		if _, err := a.invites.Create(t.Context(), id, invite.CreateInput{
			Email: where.email, Role: "editor",
		}); err != nil {
			t.Fatalf("issue an invitation in %s: %v", where.org, err)
		}
	}

	for _, want := range []struct {
		org  uuid.UUID
		name string
	}{{a.owner.OrgID, "Acme"}, {a.betaOrg, "Beta"}} {
		if n := a.count(t, `
			SELECT count(*) FROM audit_logs
			 WHERE organization_id = $1 AND actor_api_key_id = $2`,
			want.org, key.ID); n != 1 {
			t.Errorf("%d records in %s name this key, want 1. A key that acts in two "+
				"organizations has to be attributable to each", n, want.name)
		}
	}
}

// ─── the check constraint ───────────────────────────────────────────────────

// m54.md: setting both a workspace and a null organization is refused by a
// check constraint rather than by convention.
//
// Asserted against the database directly, because the point of a constraint is
// that it holds for a writer the service does not go through — a migration, a
// repair script, a future call site that forgets.
func TestAWorkspaceBoundKeyCannotBeAccountWide(t *testing.T) {
	f := newTeamFixture(t)
	_, err := f.pool.Exec(t.Context(), `
		INSERT INTO api_keys (id, user_id, organization_id, workspace_id, name, prefix, key_hash, scopes)
		VALUES ($1, $2, NULL, $3, 'impossible', 'lk_live_zzzzzzzz', '\x00', '{links.read}')`,
		uuid.Must(uuid.NewV7()), f.owner.UserID, f.owner.WorkspaceID)
	if err == nil {
		t.Fatal("a key bound to a workspace was written with no organization. " +
			"A workspace belongs to exactly one organization, so such a row has " +
			"two columns disagreeing about which tenant the key is in")
	}
	if !strings.Contains(err.Error(), "api_keys_workspace_needs_organization") {
		t.Errorf("the insert failed on %v, want the check constraint", err)
	}
}
