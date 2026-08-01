package team

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Member is one membership as an administrator sees it.
//
// One row per membership rather than per person, because a user may hold an
// organization-wide membership and a workspace-scoped one at the same time and
// under D31 the two add. Collapsing them would hide the second grant behind the
// first, which is the grant somebody would go looking for.
type Member struct {
	// ID is the membership's id, not the user's. Every operation here acts on a
	// membership: removing somebody from one workspace and removing them from
	// the organization are the same verb applied to different rows.
	ID     uuid.UUID `json:"id"`
	UserID uuid.UUID `json:"user_id"`
	Email  string    `json:"email"`
	Name   string    `json:"name"`
	Role   string    `json:"role"`
	// RoleRank is carried so a control can order and compare without a second
	// lookup, and so the rank rules are visible to whoever reads the response.
	RoleRank int32 `json:"role_rank"`
	// WorkspaceID is nil for a membership covering every workspace in the
	// organization, which is what registration and invitation redemption both
	// create. A set one covers exactly that workspace and adds to whatever else
	// the person holds.
	WorkspaceID   *uuid.UUID `json:"workspace_id,omitempty"`
	WorkspaceName string     `json:"workspace_name,omitempty"`
	// Manageable is whether the actor who asked may re-role or remove this row.
	// Computed against the asker rather than stored, because it is a property of
	// the request: it is what lets a page draw the controls that will work and
	// omit the ones that would answer 403.
	Manageable bool `json:"manageable"`
	// IsSelf marks the asker's own membership. An admin's own row is not
	// manageable — self is not strictly below self — and saying which row is
	// theirs is what makes that read as a rule rather than as a bug.
	IsSelf    bool      `json:"is_self"`
	CreatedAt time.Time `json:"created_at"`
}

// Members lists the organization's memberships, most powerful first.
//
// members.read, enforced here for the first time anywhere: the permission has
// been seeded since Phase 1 with nothing consulting it. Editors and viewers hold
// it, so everybody in an organization can see who else is in it — which is the
// point of belonging to one — while changing anything needs members.write.
func (s *Service) Members(ctx context.Context, actor *auth.Identity) ([]Member, error) {
	if !actor.Can(PermMembersRead) {
		return nil, fmt.Errorf("%w: listing members requires %s", domain.ErrForbidden, PermMembersRead)
	}
	rows, err := s.q.ListMembers(ctx, actor.OrgID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	ownerRank, err := s.ownerRank(ctx, s.q)
	if err != nil {
		return nil, err
	}

	writes := actor.Can(PermMembersWrite)
	out := make([]Member, 0, len(rows))
	for _, r := range rows {
		m := Member{
			ID:          r.ID,
			UserID:      r.UserID,
			Email:       r.Email,
			Name:        r.Name,
			Role:        r.RoleSlug,
			RoleRank:    r.RoleRank,
			WorkspaceID: r.WorkspaceID,
			IsSelf:      r.UserID == actor.UserID,
			CreatedAt:   r.CreatedAt,
			Manageable:  writes && mayManage(actor.RoleRank, r.RoleRank, ownerRank),
		}
		if r.WorkspaceName != nil {
			m.WorkspaceName = *r.WorkspaceName
		}
		out = append(out, m)
	}
	return out, nil
}

// ChangeRole re-roles one membership.
//
// Two bounds, and they are different questions. The membership must be one this
// actor may manage at all — strictly below their own rank, owners excepted
// (D30) — and the role being granted must be at or below the actor's own, which
// is m28.md's "nobody grants a role binding tighter than their own" and the same
// ceiling an invitation carries (D28).
//
// So an admin may promote an editor to admin and then find they can no longer
// manage them. That is not an oversight: the ceiling is about what authority may
// be handed out, and strictly-below is about who may be acted on, and an admin
// who mints a peer has done something an invitation already let them do.
func (s *Service) ChangeRole(
	ctx context.Context, actor *auth.Identity, membershipID uuid.UUID, roleSlug string,
) error {
	if !actor.Can(PermMembersWrite) {
		return fmt.Errorf("%w: changing a role requires %s", domain.ErrForbidden, PermMembersWrite)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	role, err := s.resolveRole(ctx, q, actor, roleSlug)
	if err != nil {
		return err
	}

	member, err := s.manageable(ctx, q, actor, membershipID)
	if err != nil {
		return err
	}
	if member.RoleSlug == role.Slug {
		// Nothing to do, and saying so beats writing an audit record claiming a
		// change that did not happen.
		return nil
	}

	// The last-owner refusal, on the demotion path. Reading the count inside
	// this transaction, after the owner rows are locked, is what makes it a rule
	// rather than a race: two administrators demoting the two remaining owners
	// at once would otherwise each see two.
	if member.RoleSlug == "owner" && role.Slug != "owner" {
		if err := s.refuseLastOwner(ctx, q, actor.OrgID, membershipID, "demoted"); err != nil {
			return err
		}
	}

	n, err := q.UpdateMembershipRole(ctx, dbgen.UpdateMembershipRoleParams{
		ID: membershipID, OrganizationID: actor.OrgID, RoleID: role.ID,
	})
	if err != nil {
		return fmt.Errorf("change role: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	s.record(ctx, actor, audit.Event{
		Action:     audit.ActionMemberRoleChanged,
		TargetType: "membership",
		TargetID:   &membershipID,
		Metadata: map[string]any{
			"email":    member.Email,
			"user_id":  member.UserID.String(),
			"from":     member.RoleSlug,
			"to":       role.Slug,
			"scope":    scopeLabel(member.WorkspaceID),
			"is_self":  member.UserID == actor.UserID,
			"org_wide": member.WorkspaceID == nil,
		},
	})
	return nil
}

// Remove ends one membership.
//
// The membership, not the account. Somebody removed from an organization keeps
// their user row, their password and every other membership they hold — which is
// what makes removal reversible by re-inviting rather than by restoring a
// backup, and what D6's membership-only redemption was shaped for.
//
// Removing a workspace-scoped membership is the same call: it withdraws the
// access that row added and leaves the organization-wide one alone.
func (s *Service) Remove(ctx context.Context, actor *auth.Identity, membershipID uuid.UUID) error {
	if !actor.Can(PermMembersWrite) {
		return fmt.Errorf("%w: removing a member requires %s", domain.ErrForbidden, PermMembersWrite)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	member, err := s.manageable(ctx, q, actor, membershipID)
	if err != nil {
		return err
	}
	if member.RoleSlug == "owner" && member.WorkspaceID == nil {
		if err := s.refuseLastOwner(ctx, q, actor.OrgID, membershipID, "removed"); err != nil {
			return err
		}
	}

	n, err := q.DeleteMembership(ctx, dbgen.DeleteMembershipParams{
		ID: membershipID, OrganizationID: actor.OrgID,
	})
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	s.record(ctx, actor, audit.Event{
		Action:     audit.ActionMemberRemoved,
		TargetType: "membership",
		TargetID:   &membershipID,
		Metadata: map[string]any{
			"email":   member.Email,
			"user_id": member.UserID.String(),
			"role":    member.RoleSlug,
			"scope":   scopeLabel(member.WorkspaceID),
		},
	})
	return nil
}

// GrantInput describes workspace-scoped access being given to somebody who is
// already in the organization.
type GrantInput struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
	Role        string
}

// Grant issues a workspace-scoped membership.
//
// This is the writer the COALESCE uniqueness index in 00200 has been waiting
// for since Phase 1: `(user_id, organization_id, coalesce(workspace_id, …))`
// permits exactly one organization-wide membership and one per workspace, and
// until now nothing created the second kind.
//
// **It adds and never narrows** (D31). Permissions resolve as the union of every
// matching membership and the effective role is the lowest rank among them, so
// an org-wide editor granted admin in one workspace is an admin there and an
// editor everywhere else. The reverse — org admin, viewer in one workspace — is
// not expressible, and every control that offers this says so.
//
// The person must already be a member of the organization — by any membership,
// organization-wide or scoped to one workspace. Somebody with none is invited
// rather than granted, because a grant is not a way into an organization and
// making it one would be a second admission path beside the one D27 bound to an
// address.
func (s *Service) Grant(ctx context.Context, actor *auth.Identity, in GrantInput) (*Member, error) {
	if !actor.Can(PermMembersWrite) {
		return nil, fmt.Errorf("%w: granting workspace access requires %s",
			domain.ErrForbidden, PermMembersWrite)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	role, err := s.resolveRole(ctx, q, actor, in.Role)
	if err != nil {
		return nil, err
	}

	// The workspace must be one in this organization. Scoped by organization
	// rather than checked afterwards, so an id from another tenant is
	// indistinguishable from one that does not exist.
	ws, err := q.GetWorkspaceInOrganization(ctx, dbgen.GetWorkspaceInOrganizationParams{
		ID: in.WorkspaceID, OrganizationID: actor.OrgID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ValidationErrors{{
				Field: "workspace_id", Code: "invalid",
				Message: "that is not a workspace in this organization",
			}}
		}
		return nil, fmt.Errorf("look up workspace: %w", err)
	}

	existing, err := q.GetOrganizationMember(ctx, dbgen.GetOrganizationMemberParams{
		OrganizationID: actor.OrgID, UserID: in.UserID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ValidationErrors{{
				Field: "user_id", Code: "not_a_member",
				Message: "that person is not in this organization; invite them first",
			}}
		}
		return nil, fmt.Errorf("look up member: %w", err)
	}

	wsID := ws.ID
	row, err := q.CreateMembership(ctx, dbgen.CreateMembershipParams{
		ID:             uuid.Must(uuid.NewV7()),
		UserID:         in.UserID,
		OrganizationID: actor.OrgID,
		RoleID:         role.ID,
		WorkspaceID:    &wsID,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, domain.ValidationErrors{{
				Field: "workspace_id", Code: "already_granted",
				Message: "that person already has a role in that workspace; remove it first to change it",
			}}
		}
		return nil, fmt.Errorf("grant workspace access: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.record(ctx, actor, audit.Event{
		Action:     audit.ActionMemberAdded,
		TargetType: "membership",
		TargetID:   &row.ID,
		Metadata: map[string]any{
			"email":     existing.Email,
			"user_id":   in.UserID.String(),
			"role":      role.Slug,
			"workspace": ws.Name,
			"scope":     "workspace",
		},
	})

	return &Member{
		ID:            row.ID,
		UserID:        in.UserID,
		Email:         existing.Email,
		Name:          existing.Name,
		Role:          role.Slug,
		RoleRank:      role.Rank,
		WorkspaceID:   &wsID,
		WorkspaceName: ws.Name,
		Manageable:    true,
		IsSelf:        in.UserID == actor.UserID,
		CreatedAt:     row.CreatedAt,
	}, nil
}

// manageable loads a membership and applies D30 to it, or refuses.
//
// The load takes a row lock, so the rank the decision is made on is the rank the
// write acts on. Not-found for an id in another organization, the same answer as
// one that never existed, so ids cannot be probed.
func (s *Service) manageable(
	ctx context.Context, q *dbgen.Queries, actor *auth.Identity, membershipID uuid.UUID,
) (dbgen.GetMembershipRow, error) {
	member, err := q.GetMembership(ctx, dbgen.GetMembershipParams{
		ID: membershipID, OrganizationID: actor.OrgID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return member, domain.ErrNotFound
		}
		return member, fmt.Errorf("look up membership: %w", err)
	}

	ownerRank, err := s.ownerRank(ctx, q)
	if err != nil {
		return member, err
	}
	if !mayManage(actor.RoleRank, member.RoleRank, ownerRank) {
		// The message names the rule rather than the person, because the caller
		// is authenticated and telling them why is more use than a bare refusal.
		// It says "at or above" because that is what the rank comparison found;
		// an actor's own membership lands here too, which is the answer to "why
		// can I not demote myself".
		return member, fmt.Errorf(
			"%w: you can only change members below your own role (%s), and %s is not",
			domain.ErrForbidden, actor.Role, member.RoleSlug)
	}
	return member, nil
}

// refuseLastOwner blocks the change that would leave an organization with none.
//
// The owner rows are locked before they are counted, so this is a rule rather
// than a check-then-act: a second administrator demoting the other owner blocks
// here until this transaction ends, then reads the count it left behind.
//
// Organization-wide owners only. A workspace-scoped owner membership is
// ownership of one workspace, and counting it would let the real last owner go.
func (s *Service) refuseLastOwner(
	ctx context.Context, q *dbgen.Queries, orgID, membershipID uuid.UUID, verb string,
) error {
	owners, err := q.LockOrganizationOwners(ctx, orgID)
	if err != nil {
		return fmt.Errorf("count owners: %w", err)
	}
	others := 0
	for _, id := range owners {
		if id != membershipID {
			others++
		}
	}
	if others == 0 {
		return fmt.Errorf(
			"%w: the last owner of an organization cannot be %s; make somebody else an owner first",
			domain.ErrConflict, verb)
	}
	return nil
}

// scopeLabel is what an audit record calls a membership's reach. A word rather
// than a nullable id, because the record is read by a person asking "did this
// take their access away everywhere, or in one place".
func scopeLabel(workspaceID *uuid.UUID) string {
	if workspaceID == nil {
		return "organization"
	}
	return "workspace:" + workspaceID.String()
}
