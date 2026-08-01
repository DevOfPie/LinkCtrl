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
// Read inside the transaction rather than before it so the count and the writes
// that invalidate it cannot be separated by a concurrent redemption.
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
// The audit trail, and nothing else. `audit_logs.organization_id` carries no
// foreign key, so every record this organization wrote outlives it, including
// the `organization.deleted` record emitted below — whose metadata carries the
// name and slug precisely because the row that held them is gone.
//
// Nothing else is preserved, and the reason is that the link guard already
// decided it. There is no alias left to reserve: an organization that reaches
// this line holds no links, so every alias it ever had was released by the link
// deletions that had to happen first, and the ones that had received traffic are
// already in `reserved_aliases` where the purge job put them. Holding anything
// else back would be preserving rows nobody can reach on behalf of an
// organization nobody can enter.
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

	// First, and in a fixed order, because the target is inside this set: after
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
