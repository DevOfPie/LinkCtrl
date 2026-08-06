//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

const wsPassword = "a-sufficiently-long-password"

// addOrgWithWorkspace gives a user a second membership, which is the state
// nothing in the product can produce yet — M27 and M28 are what create one.
//
// Written straight to the database on purpose. The alternative is waiting for
// those milestones to test this one, and the switcher's whole reason to exist
// now is that identity resolution should be settled *before* a feature starts
// creating memberships.
func addOrgWithWorkspace(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, orgName, wsName string) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	orgID := uuid.Must(uuid.NewV7())
	wsID := uuid.Must(uuid.NewV7())

	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug, is_personal) VALUES ($1, $2, $3, false)`,
		orgID, orgName, strings.ToLower(orgName)+"-"+orgID.String()[:8]); err != nil {
		t.Fatalf("create organization: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, organization_id, name, slug) VALUES ($1, $2, $3, $4)`,
		wsID, orgID, wsName, strings.ToLower(wsName)); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memberships (id, user_id, organization_id, role_id, workspace_id)
		 VALUES ($1, $2, $3, (SELECT id FROM roles WHERE slug = 'owner' AND organization_id IS NULL), NULL)`,
		uuid.Must(uuid.NewV7()), userID, orgID); err != nil {
		t.Fatalf("create membership: %v", err)
	}
	return wsID
}

// TestOneMembershipResolvesExactlyAsItDid is the claim the whole milestone
// rests on: every instance running today has one membership per user, so all of
// this must be a no-op for them.
//
// Asserted against the ordering the old query used — oldest workspace first —
// rather than against "the workspace login returned", which would pass even if
// every path had quietly changed together. All four resolution call sites are
// walked, because a missed one would keep working today and only diverge once
// somebody held two memberships, which is a different milestone's blame radius.
func TestOneMembershipResolvesExactlyAsItDid(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	id, err := svc.Register(ctx, auth.RegisterInput{
		Email: "solo@example.com", Password: wsPassword,
	})
	if err != nil {
		t.Fatal(err)
	}

	// What the pre-M25 query would have returned, computed here rather than
	// trusted from the service.
	var want uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT w.id
		FROM workspaces w
		JOIN memberships m ON m.organization_id = w.organization_id
		WHERE m.user_id = $1 AND w.deleted_at IS NULL
		ORDER BY w.created_at, w.id
		LIMIT 1`, id.UserID).Scan(&want); err != nil {
		t.Fatal(err)
	}

	// 1. Login.
	res, err := svc.Login(ctx, auth.LoginInput{Email: "solo@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}
	if res.Identity.WorkspaceID != want {
		t.Errorf("login resolved workspace %s, want %s", res.Identity.WorkspaceID, want)
	}

	// 2. Session authentication.
	authed, err := svc.Authenticate(ctx, res.Token)
	if err != nil {
		t.Fatal(err)
	}
	if authed.WorkspaceID != want {
		t.Errorf("session resolved workspace %s, want %s", authed.WorkspaceID, want)
	}

	// 3. The CLI's identity lookup.
	cli, err := svc.IdentityForEmail(ctx, "solo@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if cli.WorkspaceID != want {
		t.Errorf("IdentityForEmail resolved workspace %s, want %s", cli.WorkspaceID, want)
	}

	// 4. An API key that names no workspace of its own.
	keys, err := auth.NewAPIKeyService(pool, svc, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatal(err)
	}
	created, err := keys.Create(ctx, res.Identity, auth.CreateAPIKeyInput{
		Name: "no-workspace", Scopes: []string{"links.read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE api_keys SET workspace_id = NULL WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	keyIdentity, err := keys.Authenticate(ctx, created.Key)
	if err != nil {
		t.Fatal(err)
	}
	if keyIdentity.WorkspaceID != want {
		t.Errorf("api key resolved workspace %s, want %s", keyIdentity.WorkspaceID, want)
	}

	// And the switcher itself has nothing to offer, which is what keeps the
	// dashboard unchanged for these accounts.
	list, err := svc.Workspaces(ctx, res.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].Current || list[0].Default {
		t.Errorf("single-membership switcher = %+v; want one entry, current, unpinned", list)
	}
}

// TestSwitchingMovesThisSessionAndIsRemembered covers both halves of what a
// switch means: the browser that asked moves now, and the next sign-in starts
// there. A browser that did *not* ask stays put, which is why the current
// workspace is session state rather than a column on the user.
func TestSwitchingMovesThisSessionAndIsRemembered(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	id, err := svc.Register(ctx, auth.RegisterInput{
		Email: "two@example.com", Password: wsPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := id.WorkspaceID
	second := addOrgWithWorkspace(t, pool, id.UserID, "Acme", "Marketing")

	login := func() *auth.LoginResult {
		t.Helper()
		res, err := svc.Login(ctx, auth.LoginInput{Email: "two@example.com", Password: wsPassword})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	identityFor := func(token string) *auth.Identity {
		t.Helper()
		i, err := svc.Authenticate(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}

	browserA := login()
	browserB := login()
	if browserA.Identity.WorkspaceID != first {
		t.Fatalf("a fresh account did not start in its own workspace: %s", browserA.Identity.WorkspaceID)
	}

	if err := svc.SwitchWorkspace(ctx, browserA.Identity, second); err != nil {
		t.Fatalf("switch: %v", err)
	}

	if got := identityFor(browserA.Token).WorkspaceID; got != second {
		t.Errorf("after switching, the session is in %s, want %s", got, second)
	}
	if got := identityFor(browserB.Token).WorkspaceID; got != first {
		t.Errorf("switching in one browser moved another (%s); the current workspace "+
			"is per session, or two windows can never look at two workspaces", got)
	}

	// The next sign-in starts where the switch left off.
	if got := login().Identity.WorkspaceID; got != second {
		t.Errorf("a new session started in %s, want the last-used %s", got, second)
	}

	// And the switcher reports it.
	list, err := svc.Workspaces(ctx, identityFor(browserA.Token))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("switcher lists %d workspaces, want 2", len(list))
	}
	for _, w := range list {
		if (w.ID == second) != w.Current {
			t.Errorf("workspace %s (%s) has current=%v", w.Name, w.ID, w.Current)
		}
		if w.Default {
			t.Errorf("workspace %s is marked as the pinned default with nothing pinned", w.Name)
		}
	}
}

// TestPinnedDefaultOutranksLastUsed is the owner-added half of D22.
//
// The pin decides where a sign-in *starts*; it must not freeze the switcher,
// or pinning would be a way to lock yourself out of your other workspaces.
func TestPinnedDefaultOutranksLastUsed(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	id, err := svc.Register(ctx, auth.RegisterInput{
		Email: "pinned@example.com", Password: wsPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := id.WorkspaceID
	second := addOrgWithWorkspace(t, pool, id.UserID, "Acme", "Marketing")

	login := func() *auth.LoginResult {
		t.Helper()
		res, err := svc.Login(ctx, auth.LoginInput{Email: "pinned@example.com", Password: wsPassword})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// Last-used is the second workspace.
	session := login()
	if err := svc.SwitchWorkspace(ctx, session.Identity, second); err != nil {
		t.Fatal(err)
	}
	if got := login().Identity.WorkspaceID; got != second {
		t.Fatalf("last-used is not in force before the pin: %s", got)
	}

	// Pin the first one. New sessions start there, last-used notwithstanding.
	if err := svc.SetDefaultWorkspace(ctx, session.Identity, &first); err != nil {
		t.Fatalf("pin: %v", err)
	}
	pinnedSession := login()
	if pinnedSession.Identity.WorkspaceID != first {
		t.Errorf("a pinned account signed in to %s, want the pinned %s",
			pinnedSession.Identity.WorkspaceID, first)
	}

	// Switching still works while pinned, and still only moves this session.
	if err := svc.SwitchWorkspace(ctx, pinnedSession.Identity, second); err != nil {
		t.Fatalf("switch while pinned: %v", err)
	}
	moved, err := svc.Authenticate(ctx, pinnedSession.Token)
	if err != nil {
		t.Fatal(err)
	}
	if moved.WorkspaceID != second {
		t.Errorf("a pinned account could not switch: still in %s", moved.WorkspaceID)
	}
	if got := login().Identity.WorkspaceID; got != first {
		t.Errorf("after switching, a new session started in %s; the pin must outrank "+
			"the switch it just recorded", got)
	}

	// The way back: clearing the pin returns the account to last-used.
	if err := svc.SetDefaultWorkspace(ctx, moved, nil); err != nil {
		t.Fatalf("clear pin: %v", err)
	}
	if got := login().Identity.WorkspaceID; got != second {
		t.Errorf("after clearing the pin a new session started in %s, want last-used %s", got, second)
	}
	list, err := svc.Workspaces(ctx, moved)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list {
		if w.Default {
			t.Errorf("workspace %s still reports itself as the pinned default", w.Name)
		}
	}
}

// TestSwitchingToSomebodyElsesWorkspaceIsNotFound: membership is checked in the
// statement that writes, so there is no window between the check and the write,
// and a foreign id is indistinguishable from one that does not exist.
func TestSwitchingToSomebodyElsesWorkspaceIsNotFound(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	alice, err := svc.Register(ctx, auth.RegisterInput{Email: "alice-ws@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := svc.Register(ctx, auth.RegisterInput{Email: "bob-ws@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.Login(ctx, auth.LoginInput{Email: "alice-ws@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}

	for name, target := range map[string]uuid.UUID{
		"another user's workspace":        bob.WorkspaceID,
		"a workspace that does not exist": uuid.Must(uuid.NewV7()),
	} {
		t.Run(name, func(t *testing.T) {
			if err := svc.SwitchWorkspace(ctx, res.Identity, target); !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("switch = %v, want ErrNotFound", err)
			}
			if err := svc.SetDefaultWorkspace(ctx, res.Identity, &target); !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("pin = %v, want ErrNotFound", err)
			}
		})
	}

	// Nothing moved, in either direction.
	after, err := svc.Authenticate(ctx, res.Token)
	if err != nil {
		t.Fatal(err)
	}
	if after.WorkspaceID != alice.WorkspaceID {
		t.Errorf("a refused switch moved the session to %s", after.WorkspaceID)
	}
	var last, pinned *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT last_workspace_id, default_workspace_id FROM users WHERE id = $1`,
		alice.UserID).Scan(&last, &pinned); err != nil {
		t.Fatal(err)
	}
	if last != nil || pinned != nil {
		t.Errorf("a refused switch wrote a preference: last=%v pinned=%v", last, pinned)
	}
}

// TestAWorkspaceScopedMembershipDoesNotReachItsSiblings pins the rule that
// resolution and the switcher share with the permission evaluator: a membership
// naming a workspace covers that workspace and no other.
//
// Nothing creates a scoped membership yet — the column has been there since
// Phase 1 and every row is organization-wide — which is exactly why it is worth
// asserting now. Resolving into a workspace the user holds no permissions in
// would produce an identity that can see a page and do nothing on it.
func TestAWorkspaceScopedMembershipDoesNotReachItsSiblings(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	id, err := svc.Register(ctx, auth.RegisterInput{Email: "scoped@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}
	mine := addOrgWithWorkspace(t, pool, id.UserID, "Acme", "Mine")

	// A sibling workspace in the same organization, and a membership narrowed
	// to the first one.
	var sibling uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspaces (id, organization_id, name, slug)
		SELECT $1, organization_id, 'Theirs', 'theirs' FROM workspaces WHERE id = $2
		RETURNING id`, uuid.Must(uuid.NewV7()), mine).Scan(&sibling); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE memberships SET workspace_id = $1
		 WHERE user_id = $2
		   AND organization_id = (SELECT organization_id FROM workspaces WHERE id = $1)`,
		mine, id.UserID); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Login(ctx, auth.LoginInput{Email: "scoped@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}
	list, err := svc.Workspaces(ctx, res.Identity)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list {
		if w.ID == sibling {
			t.Errorf("the switcher offers %s, a workspace the membership does not cover", w.Name)
		}
	}
	if err := svc.SwitchWorkspace(ctx, res.Identity, sibling); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("switching into an uncovered sibling = %v, want ErrNotFound", err)
	}
}

// TestASoftDeletedOrganizationIsNeitherListedNorResolvedInto pins the two
// halves of one scoping rule to each other.
//
// ListWorkspacesForUser has always filtered the organization's deleted_at as
// well as the workspace's; ResolveWorkspaceForUser filtered only the
// workspace's. A workspace under a soft-deleted organization could therefore be
// resolved *into* and never *listed*, which is the worst shape the disagreement
// has: the switcher marks nothing selected because the current workspace is not
// in the list it was given, so a browser shows the first entry while the session
// acts somewhere else entirely.
//
// Nothing in the tree soft-deletes an organization — DeleteOrganization is a
// hard DELETE — so the column is set here directly. That is the point rather
// than a shortcut: the two queries agree today by accident, and this test is
// what makes them agree on purpose (F25).
func TestASoftDeletedOrganizationIsNeitherListedNorResolvedInto(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	first, err := svc.Register(ctx, auth.RegisterInput{Email: "softdel@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}
	second := addOrgWithWorkspace(t, pool, first.UserID, "Beta", "Beta")

	// Pinned, so the precedence prefers it over the workspace registration
	// made. Without this the fallback would be indistinguishable from the
	// ordering it was always going to produce.
	res, err := svc.Login(ctx, auth.LoginInput{Email: "softdel@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetDefaultWorkspace(ctx, res.Identity, &second); err != nil {
		t.Fatal(err)
	}
	before, err := svc.Login(ctx, auth.LoginInput{Email: "softdel@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}
	if before.Identity.WorkspaceID != second {
		t.Fatalf("baseline: pinned workspace did not win, got %s want %s",
			before.Identity.WorkspaceID, second)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE organizations SET deleted_at = now()
		 WHERE id = (SELECT organization_id FROM workspaces WHERE id = $1)`,
		second); err != nil {
		t.Fatal(err)
	}

	after, err := svc.Login(ctx, auth.LoginInput{Email: "softdel@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}
	if after.Identity.WorkspaceID == second {
		t.Errorf("resolved into a workspace under a soft-deleted organization (%s)", second)
	}
	if after.Identity.WorkspaceID != first.WorkspaceID {
		t.Errorf("resolution landed at %s, want the surviving workspace %s",
			after.Identity.WorkspaceID, first.WorkspaceID)
	}

	// The half that was already right, asserted beside the half that was not —
	// so a later change breaking either one fails here rather than producing
	// the split again.
	list, err := svc.Workspaces(ctx, after.Identity)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list {
		if w.ID == second {
			t.Errorf("the switcher offers %s, which belongs to a soft-deleted organization", w.Name)
		}
	}
}

// addWorkspace gives an organization a second workspace, without giving the user
// a second organization.
//
// addOrgWithWorkspace is the other shape and they are not interchangeable, which
// M44 is what made true. An organization-wide key reaches its own organization's
// workspaces, so a test about *where a loose key lands* needs a second workspace
// the key legitimately covers; a test about the tenancy bound needs a second
// organization. Before M44 nothing could tell them apart, because nothing issued
// a key with no workspace of its own.
func addWorkspace(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	wsID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, organization_id, name, slug) VALUES ($1, $2, $3, $4)`,
		wsID, orgID, name, strings.ToLower(name)+"-"+wsID.String()[:8]); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	return wsID
}

// TestAPIKeysFollowTheirOwnerButCannotSwitch covers the fourth call site and
// the one restriction on it.
func TestAPIKeysFollowTheirOwnerButCannotSwitch(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	keys, err := auth.NewAPIKeyService(pool, svc, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatal(err)
	}

	id, err := svc.Register(ctx, auth.RegisterInput{Email: "keyed@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.Login(ctx, auth.LoginInput{Email: "keyed@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}

	// A second workspace **in the same organization**, which is where a key with
	// no workspace of its own may land. The membership registration created is
	// organization-wide, so it covers this one too.
	//
	// This read `addOrgWithWorkspace(…, "Acme", "Marketing")` until M44, and the
	// difference is not cosmetic: that gave the owner a second *organization* and
	// then asserted the key followed a pin into it. Nothing could produce such a
	// key at the time — `Create` always wrote a workspace id, and the NULL below
	// is written by hand — so the assertion was about a state the product did not
	// have. M44 makes it producible, and D90 bounds it to the key's own
	// organization; the cross-organization case is asserted a few lines down,
	// where it belongs.
	second := addWorkspace(t, pool, res.Identity.OrgID, "Marketing")

	bound, err := keys.Create(ctx, res.Identity, auth.CreateAPIKeyInput{
		Name: "bound", Scopes: []string{"links.read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	loose, err := keys.Create(ctx, res.Identity, auth.CreateAPIKeyInput{
		Name: "loose", Scopes: []string{"links.read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE api_keys SET workspace_id = NULL WHERE id = $1`, loose.ID); err != nil {
		t.Fatal(err)
	}

	// The owner pins the second workspace.
	if err := svc.SetDefaultWorkspace(ctx, res.Identity, &second); err != nil {
		t.Fatal(err)
	}

	looseIdentity, err := keys.Authenticate(ctx, loose.Key)
	if err != nil {
		t.Fatal(err)
	}
	if looseIdentity.WorkspaceID != second {
		t.Errorf("a key with no workspace of its own resolved to %s, want the owner's "+
			"pinned %s", looseIdentity.WorkspaceID, second)
	}

	// And the bound the pin does not cross (D90). The owner joins a second
	// organization and pins a workspace there; the key stays where it was issued,
	// because organization-wide means every workspace in *its* organization and a
	// pin is a property of the person rather than of the tenancy.
	elsewhere := addOrgWithWorkspace(t, pool, id.UserID, "Acme", "Their Space")
	if _, err := pool.Exec(ctx,
		`UPDATE users SET default_workspace_id = $1, last_workspace_id = $1 WHERE id = $2`,
		elsewhere, id.UserID); err != nil {
		t.Fatal(err)
	}
	crossed, err := keys.Authenticate(ctx, loose.Key)
	if err != nil {
		t.Fatal(err)
	}
	if crossed.WorkspaceID == elsewhere {
		t.Errorf("a key issued in one organization resolved into workspace %s, which "+
			"belongs to another one its owner joined; it would then act wholly in a "+
			"tenant nobody issued it for", crossed.WorkspaceID)
	}
	if crossed.OrgID != res.Identity.OrgID {
		t.Errorf("the key reported organization %s, want the one it was issued in (%s)",
			crossed.OrgID, res.Identity.OrgID)
	}

	boundIdentity, err := keys.Authenticate(ctx, bound.Key)
	if err != nil {
		t.Fatal(err)
	}
	if boundIdentity.WorkspaceID != res.Identity.WorkspaceID {
		t.Errorf("a key naming its own workspace was moved by its owner's preference: %s",
			boundIdentity.WorkspaceID)
	}

	// A key cannot repoint where its owner's browser lands.
	if err := svc.SwitchWorkspace(ctx, looseIdentity, res.Identity.WorkspaceID); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("switch with an API key = %v, want ErrForbidden", err)
	}
	if err := svc.SetDefaultWorkspace(ctx, looseIdentity, nil); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("pin with an API key = %v, want ErrForbidden", err)
	}
}

// TestAKeyIsToldOnlyAboutItsOwnOrganization asserts the read half of the bound
// M44 spent an organization_id parameter to put on the write half.
//
// The switcher's job is to cross organizations, so a browser sees every one its
// owner belongs to and must keep doing so. A key was never issued for the
// others: it cannot act there, and until F103 it could still read their names,
// slugs and identifiers out of GET /api/v1/workspaces. The two halves are
// asserted together here because the fix is a filter on credential type, and a
// filter that took the session with it would be a worse defect than the
// disclosure it closed.
func TestAKeyIsToldOnlyAboutItsOwnOrganization(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	keys, err := auth.NewAPIKeyService(pool, svc, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatal(err)
	}

	id, err := svc.Register(ctx, auth.RegisterInput{Email: "twoorgs@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Login(ctx, auth.LoginInput{Email: "twoorgs@example.com", Password: wsPassword})
	if err != nil {
		t.Fatal(err)
	}

	// The key is issued here, before the second organization exists, so nothing
	// about its creation could have named the tenant it must not see.
	key, err := keys.Create(ctx, res.Identity, auth.CreateAPIKeyInput{
		Name: "reader", Scopes: []string{"links.read"},
	})
	if err != nil {
		t.Fatal(err)
	}

	addOrgWithWorkspace(t, pool, id.UserID, "Elsewhere Ltd", "Their Space")

	names := func(list []auth.Workspace) map[string]bool {
		seen := make(map[string]bool, len(list))
		for _, w := range list {
			seen[w.Name] = true
		}
		return seen
	}

	session, err := svc.Workspaces(ctx, res.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(session); !got["Their Space"] {
		t.Errorf("the owner's own session was not offered the second organization's "+
			"workspace; the switcher exists to cross organizations and has stopped "+
			"being able to (saw %v)", got)
	}

	keyed, err := keys.Authenticate(ctx, key.Key)
	if err != nil {
		t.Fatal(err)
	}
	list, err := svc.Workspaces(ctx, keyed)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(list); got["Their Space"] {
		t.Errorf("a key issued in one organization was told the name of a workspace in "+
			"another one its owner belongs to, which it cannot act in (saw %v)", got)
	}
	for _, w := range list {
		if w.OrganizationID != keyed.OrgID {
			t.Errorf("workspace %q reported organization %s, want the key's own %s",
				w.Name, w.OrganizationID, keyed.OrgID)
		}
	}
	if len(list) == 0 {
		t.Error("the key was told about no workspace at all, which is the filter " +
			"having removed its own organization along with the other one")
	}
}

// TestWebWorkspaceSwitcher drives the dashboard the way a person does: the
// header control appears once there is somewhere to go, moves the browser, and
// leaves them on the page they were reading.
func TestWebWorkspaceSwitcher(t *testing.T) {
	f := newWeb(t)
	f.claim()

	// One membership: no control anywhere, which is every instance today.
	if page := f.body(f.get("/dashboard", nil)); strings.Contains(page, `action="/workspace/switch"`) {
		t.Error("a single-membership dashboard renders a switcher")
	}

	var userID uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT id FROM users WHERE email_lower = 'owner@example.com'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	second := addOrgWithWorkspace(t, f.pool, userID, "Acme", "Marketing")

	page := f.body(f.get("/links", nil))
	if !strings.Contains(page, `action="/workspace/switch"`) {
		t.Fatal("with two memberships the header still has no switcher")
	}
	if !strings.Contains(page, second.String()) {
		t.Error("the switcher does not offer the second workspace")
	}

	// The control is one step, not two (D103, F21). Picking a workspace used to
	// do nothing until Switch was pressed, and any navigation discarded the
	// choice silently — a control that looks like it works and does not.
	//
	// Asserted on the markup, because the behaviour past this point is htmx's
	// and the browser's rather than ours: what this tree is responsible for is
	// emitting a select that submits on change and no second control to press.
	switcher := page[strings.Index(page, `action="/workspace/switch"`):]
	switcher = switcher[:strings.Index(switcher, "</form>")]
	for _, want := range []string{`hx-post="/workspace/switch"`, `hx-trigger="change"`, `hx-include="closest form"`} {
		if !strings.Contains(switcher, want) {
			t.Errorf("the switcher select is missing %s, so picking a workspace does "+
				"nothing until something else is pressed", want)
		}
	}
	if strings.Contains(switcher, "<button") {
		t.Error("the switcher still renders a button; the directive was that a " +
			"separate button to switch cannot stay, and a redundant control is the " +
			"affordance problem F21 is about")
	}
	// hx-include carries this, and without it the switch loses the page it was
	// made from and lands on the dashboard instead.
	if !strings.Contains(switcher, `name="next"`) {
		t.Error("the switcher no longer carries the path it was submitted from")
	}

	// Switching returns to the page it was posted from.
	f.wantRedirect(f.postForm("/workspace/switch", url.Values{
		"workspace_id": {second.String()}, "next": {"/links"},
	}, nil), "/links")

	// And the account page now shows the switched-to workspace as current.
	account := f.body(f.get("/account", nil))
	if !strings.Contains(account, second.String()+`" selected`) {
		t.Error("after switching, the header control does not show the new workspace as current")
	}

	// The default-workspace control pins it.
	if !strings.Contains(account, `action="/workspace/default"`) {
		t.Fatal("the account page has no default-workspace control")
	}
	f.wantRedirect(f.postForm("/workspace/default", url.Values{
		"workspace_id": {second.String()},
	}, nil), "/account?workspace=1")

	var pinned *uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT default_workspace_id FROM users WHERE id = $1`, userID).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if pinned == nil || *pinned != second {
		t.Errorf("the pin stored %v, want %s", pinned, second)
	}

	// Last-Used is reachable again: an empty value is the option, not a missing
	// field.
	f.wantRedirect(f.postForm("/workspace/default", url.Values{
		"workspace_id": {""},
	}, nil), "/account?workspace=1")
	if err := f.pool.QueryRow(t.Context(),
		`SELECT default_workspace_id FROM users WHERE id = $1`, userID).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if pinned != nil {
		t.Errorf("choosing Last-Used left the pin at %s", *pinned)
	}

	// A workspace the user has nothing to do with is refused as not-found.
	other := uuid.Must(uuid.NewV7())
	resp := f.postForm("/workspace/switch", url.Values{
		"workspace_id": {other.String()}, "next": {"/links"},
	}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("switching to a foreign workspace returned %d, want 404", resp.StatusCode)
	}
	_ = f.body(resp)
}
