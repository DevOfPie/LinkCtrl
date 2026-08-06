package team

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// maxOrganizationName bounds a name the same way a workspace's is bounded.
const maxOrganizationName = 80

// Organization is a newly created organization and the workspace it was
// provisioned with.
//
// The workspace is part of the answer rather than a detail: an organization
// with nothing to work in is not usable, so the call that makes one makes both,
// and the response says where to go.
type Organization struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Slug string    `json:"slug"`
	// IsPersonal is false for everything created here. The flag marks the
	// organization registration provisions alongside an account — "your own
	// space" — and an organization somebody deliberately created to share is not
	// that, whatever they end up using it for.
	IsPersonal    bool      `json:"is_personal"`
	WorkspaceID   uuid.UUID `json:"workspace_id"`
	WorkspaceName string    `json:"workspace_name"`
	CreatedAt     time.Time `json:"created_at"`
}

// CreateOrganization provisions an organization, its first workspace and an
// owner membership for the caller, in one transaction.
//
// The provisioning is auth.ProvisionOrganization — literally the function
// registration calls — rather than a second implementation of the same four
// writes. That is deliberate: the tenancy invariants (an organization always has
// a workspace, and always has an owner, both written in the transaction that
// created it) are the kind that hold until somebody writes them out a second
// time slightly differently.
//
// Gated on orgs.create (D16), which on a default instance is held by the account
// from the setup form and nobody else — see 01300_orgs_create.sql for why that
// is a role grant rather than a check against how the account was made. The
// permission is also the call site a future entitlement check would hang on
// (Phase 3+); nothing here is billing-shaped, and the point of naming it now is
// that the check has somewhere to go without a schema change.
//
// The caller becomes the owner. Not a parameter, and not settable: an
// organization created on somebody else's behalf would be an organization
// nobody asked for, and the account that holds orgs.create is the one that
// wanted it.
//
// # The first-organization seam (D36, recorded against D16)
//
// An account that belongs to no organization holds no role, therefore holds no
// permissions, therefore does not hold orgs.create. Since M28.5 that state is
// reachable — deleting an organization leaves its members with one fewer, and
// possibly none — and the product's answer to it is to offer that account an
// organization of its own. Without a second door, the offer would lead straight
// to a 403.
//
// **The mechanism is a membership count, read inside this transaction, at this
// one call site.** It is written here rather than folded into the permission
// evaluator on purpose. An identity that synthesised orgs.create for itself
// would carry that grant into every Can() call and every affordance the
// templates draw from one, and the blast radius of getting it wrong would be the
// whole authorization surface; a count read where it is used authorizes exactly
// this operation and is findable by grepping for the permission.
//
// **Why this is not a second authorization axis.** D16 made orgs.create a grant
// rather than a check on how an account was made, because a provenance test —
// "did this account self-register?" — is a parallel authorization system that
// RBAC cannot see, cannot audit and cannot revoke. A membership count is not
// that. It is a check on *present state*, it is monotone in the direction that
// closes rather than opens (the moment the account has any membership at all,
// only the permission answers), and it cannot escalate: an account with no
// memberships can reach exactly one operation, whose entire effect is to give
// that account an owner membership — which is where the permission takes over.
// The zero-membership account is therefore not a role beside RBAC; it is the
// empty case RBAC has no row for.
//
// Read inside the transaction because everything this call does is, and not
// because that serializes it. A count cannot be locked — the check-then-act
// organizations.sql warns about in its own preamble, and which LockOrganizations
// avoids by selecting the rows a decision is made on FOR UPDATE. At read
// committed each statement takes its own snapshot, so a redemption committing
// after this read is invisible to it and two calls racing can both see zero.
// The race is left open rather than closed, on the paragraph above: the one
// operation a zero-membership account can reach gives that account an owner
// membership, so losing it costs one more organization the account legitimately
// owns and nothing else.
//
// **No credential-type check, and that is deliberate rather than an omission.**
// An API key used to be able to walk through this door: its owner could be
// removed from the organization, the key would still authenticate, and the count
// it then read was zero — so a key scoped to links.read alone could create an
// organization and own it, which is a bypass of orgs.create rather than ordinary
// use of it. The answer is not a requireSessionActor here. Branching on
// credential type outside NonDelegableScopes and D43 is itself a defect, and one
// more branch would leave the *state* the door opens on intact. The state is
// what was wrong: an authenticated key now always has a live membership in the
// organization it was issued into, because Authenticate refuses one whose owner
// does not, so the count a key reads here is never zero. A key holding
// orgs.create still creates organizations, which is what D36 and its test say it
// may do.
func (s *Service) CreateOrganization(
	ctx context.Context, actor *auth.Identity, name string,
) (*Organization, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, domain.ValidationErrors{{
			Field: "name", Code: "required", Message: "give the organization a name",
		}}
	}
	if len(name) > maxOrganizationName {
		return nil, domain.ValidationErrors{{
			Field: "name", Code: "too_long",
			Message: fmt.Sprintf("an organization name is at most %d characters", maxOrganizationName),
		}}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if !actor.Can(PermOrgsCreate) {
		memberships, cerr := q.CountUserMemberships(ctx, actor.UserID)
		if cerr != nil {
			return nil, fmt.Errorf("count memberships: %w", cerr)
		}
		if memberships > 0 {
			return nil, fmt.Errorf("%w: creating an organization requires %s",
				domain.ErrForbidden, PermOrgsCreate)
		}
	}

	org, ws, err := auth.ProvisionOrganization(ctx, q, actor.UserID, name, false)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Recorded against the new organization rather than the caller's current
	// one. The audit list is scoped by the actor's organization, so this record
	// lands where the new organization's own history begins — which is where
	// somebody reading that organization's log would look for how it started.
	s.record(ctx, &auth.Identity{
		UserID:      actor.UserID,
		Email:       actor.Email,
		Name:        actor.Name,
		OrgID:       org.ID,
		WorkspaceID: ws.ID,
		APIKeyID:    actor.APIKeyID,
	}, audit.Event{
		Action:     audit.ActionOrganizationCreated,
		TargetType: "organization",
		TargetID:   &org.ID,
		Metadata: map[string]any{
			"name":      org.Name,
			"slug":      org.Slug,
			"workspace": ws.Name,
		},
	})

	return &Organization{
		ID:            org.ID,
		Name:          org.Name,
		Slug:          org.Slug,
		IsPersonal:    org.IsPersonal,
		WorkspaceID:   ws.ID,
		WorkspaceName: ws.Name,
		CreatedAt:     org.CreatedAt,
	}, nil
}

// DeleteOrganization removes an organization and everything the schema hangs
// off it, and it is the first operation `org.delete` has ever gated.
//
// The permission has been seeded and held by owners since Phase 1 with nothing
// behind it. M28's rank rules already forbid an admin acquiring it: granting the
// owner role requires being an owner (resolveRole's ceiling), so the set of
// accounts that can reach this is exactly the set an owner chose.
//
// # Which organization
//
// The one the caller is acting in, named by id. An id that is not the caller's
// current organization is not-found — the same answer one that never existed
// gets, so ids cannot be probed, and consistent with every other read in this
// package. The path parameter is therefore a confirmation rather than a
// selector, which is the right shape for an irreversible operation: pasting the
// wrong id deletes nothing.
//
// # What it refuses, and why each refusal is a rule rather than a check
//
// **The instance's last organization.** An instance with no organization has no
// path back that does not involve SQL — the same argument that refuses the last
// owner and the last workspace, one level up.
//
// **An organization still holding any link** (D37), archived ones included. D32
// refuses this for a workspace; an organization-level cascade through the same
// links would make that rule bypassable by deleting one level up. The cost is
// stated rather than hidden: with no bulk delete until Phase 2+, emptying a
// large organization is a link at a time.
//
// Both guards lock the rows they count before counting them, so two
// administrators acting at once cannot each pass a check the other invalidates.
// The lock on the organization rows also blocks a workspace being created in one
// while this decides, and the lock on the workspaces blocks a link being created
// in one — see the notes in organizations.sql.
//
// # What it does not refuse on
//
// **Members left with no organization at all** (D36). Deletion proceeds; the
// accounts survive with no membership, and the session path treats that as an
// empty state rather than a broken instance. That is the expensive answer and it
// is most of this milestone; auth.ErrNoWorkspace is where it lands.
//
// # What survives
//
// Two things, enumerated rather than counted. This paragraph opened with *the
// audit trail, and nothing else* and then described a second survivor two
// sentences later, which is how it also failed to notice a third (F106).
//
// The audit trail. `audit_logs.organization_id` carries no foreign key, so every
// record this organization wrote outlives it, including the
// `organization.deleted` record emitted below — whose metadata carries the name
// and slug precisely because the row that held them is gone.
//
// The aliases of trashed links that received traffic, in `reserved_aliases`.
// The link guard does not decide this, which is what an earlier version of this
// comment got wrong (F28): it counts live links, and excludes soft-deleted ones
// on purpose, so an organization can reach this line still holding trashed links
// for the rest of their trash window. The cascade hard-deletes them, and the
// purge job — the only other writer of `reserved_aliases` — never sees them. So
// the reservation is made here, in this transaction, at PurgeExpiredLinks'
// threshold; an alias that never received a click is released, because nothing
// in the wild points at it.
//
// **The analytics rollups no longer survive, and used to.** `link_click_daily`,
// `link_dimension_daily` and `workspace_click_daily` carry `workspace_id` with
// no foreign key, so nothing cascaded them and they outlived the tenancy they
// described. Not a disclosure — every reader scopes to a live workspace, so the
// rows were unreachable rather than exposed — but stale aggregate data with no
// owner, and a sentence above that was not true. `DeleteOrganizationRollups`
// takes them in this transaction, before the cascade removes the workspaces
// that are the only way to name them.
//
// **It preserves the aliases on the shared default domain, and only those**
// (F118). `reserved_aliases` is keyed to `domain_id` with `ON DELETE CASCADE`,
// and a workspace's own registered hostname cascades from the workspace, which
// cascades from here — so for a link on a custom hostname the reservation
// inserted a line above is removed by the cascade of this same statement. The
// inserts are wasted rather than wrong.
//
// **Nothing is done about that, and the reasons are worth having in one place.**
// The exposure F28 is about is the shared default domain, whose
// `organization_id` is NULL and which this teardown does not touch, so the path
// that matters is intact. For a custom hostname the domain row is destroyed
// too, and re-serving one of its aliases would require re-registering the
// hostname *and* passing the TXT check — at which point whoever did that
// controls the name anyway and the reservation was never what protected
// anybody. And every available repair is worse: `RESTRICT` makes organization
// deletion fail outright, `SET NULL` is impossible because `domain_id` is half
// the primary key, and re-keying reservations by hostname would reserve aliases
// on a name whose next owner proved control of it.
//
// Nothing else is preserved. Holding anything else back would be keeping rows
// nobody can reach on behalf of an organization nobody can enter.
func (s *Service) DeleteOrganization(ctx context.Context, actor *auth.Identity, id uuid.UUID) error {
	if !actor.Can(PermOrgDelete) {
		return fmt.Errorf("%w: deleting an organization requires %s",
			domain.ErrForbidden, PermOrgDelete)
	}
	if id != actor.OrgID {
		return domain.ErrNotFound
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// The permission has to come from an organization-wide membership (D44).
	// `resolveRole` lets an owner grant **owner** scoped to a single workspace —
	// a supported path, and one the role control offers — and that membership
	// resolves inside its workspace holding every permission the owner role has,
	// org.delete included. Deleting the organization with it would be the
	// clearest possible violation of the sentence `members.sql` already states:
	// a workspace-scoped owner membership grants ownership of one workspace, not
	// of the organization (F27).
	authority, err := auth.LoadMembershipAuthority(ctx, q, actor.UserID, actor.OrgID, PermOrgDelete)
	if err != nil {
		return err
	}
	if !authority.In(nil).Granted {
		return fmt.Errorf(
			"%w: deleting an organization requires %s from an organization-wide membership; "+
				"owning one workspace is not owning the organization",
			domain.ErrForbidden, PermOrgDelete)
	}

	// Then, and in a fixed order, because the target is inside this set: after
	// this line the organization being deleted is locked, and so is every other
	// one the count is about.
	organizations, err := q.LockOrganizations(ctx)
	if err != nil {
		return fmt.Errorf("lock organizations: %w", err)
	}
	if len(organizations) <= 1 {
		return fmt.Errorf(
			"%w: this is the only organization on this instance, and an instance "+
				"without one cannot be used or repaired from the dashboard; create "+
				"another first",
			domain.ErrConflict)
	}

	org, err := q.GetOrganization(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("look up organization: %w", err)
	}

	if _, err := q.LockOrganizationWorkspaces(ctx, org.ID); err != nil {
		return fmt.Errorf("lock workspaces: %w", err)
	}
	links, err := q.CountOrganizationLinks(ctx, org.ID)
	if err != nil {
		return fmt.Errorf("count links: %w", err)
	}
	if links > 0 {
		return fmt.Errorf(
			"%w: %s still holds %d link(s) across its workspaces, archived ones "+
				"included; delete them first",
			domain.ErrConflict, org.Name, links)
	}

	// The same reservation DeleteWorkspace makes, for the same reason and at the
	// same threshold, one level up: the link guard counts live links only, so the
	// cascade may still take trashed ones and their aliases with them (F28).
	if err := q.ReserveOrganizationTraffickedAliases(ctx, org.ID); err != nil {
		return fmt.Errorf("reserve trafficked aliases: %w", err)
	}

	// The analytics rollups, which nothing cascades. They carry workspace_id
	// with no foreign key, so they used to outlive the tenancy they describe —
	// while the doc above said the audit trail was all that survived (F106).
	// Before the delete, because the workspaces are what name these rows and the
	// cascade takes the workspaces with it.

	// The analytics rollups, which nothing cascades. They carry workspace_id
	// with no foreign key, so they used to outlive the tenancy they describe —
	// while the doc above said the audit trail was all that survived (F106).
	// Before the delete, because the workspaces are what name these rows and the
	// cascade takes the workspaces with it.
	if _, err := q.DeleteOrganizationRollups(ctx, org.ID); err != nil {
		return fmt.Errorf("delete analytics rollups: %w", err)
	}

	n, err := q.DeleteOrganization(ctx, org.ID)
	if err != nil {
		return fmt.Errorf("delete organization: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Recorded against the organization that is gone, which is where its own
	// history already lives, and written after the commit like every other audit
	// emission in this tree. The name is in the metadata for the reason a deleted
	// workspace's is: this record is the only remaining trace of what was there.
	s.record(ctx, actor, audit.Event{
		Action:     audit.ActionOrganizationDeleted,
		TargetType: "organization",
		TargetID:   &org.ID,
		Metadata: map[string]any{
			"name":        org.Name,
			"slug":        org.Slug,
			"is_personal": org.IsPersonal,
		},
	})
	return nil
}
