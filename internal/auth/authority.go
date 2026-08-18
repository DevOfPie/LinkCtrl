package auth

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Authority is what one actor may exercise over one object: whether they hold a
// permission in that object's scope at all, and the rank of the membership that
// carried it there.
//
// It is the companion to Identity.Can, and the two answer deliberately
// different questions. Can answers *what may this person do in the workspace
// they are acting in*, from the union of every membership matching it and the
// lowest rank among them (D31). That is the right answer for an object that
// lives in a workspace — a link, a tag, a key — and the wrong one for an object
// that spans the organization, because the union silently lends the reach of one
// membership to the authority of another.
//
// M28's reopening is the reason this type exists. An actor holding an
// organization-wide `viewer` row and a workspace-scoped `admin` row resolves,
// inside that workspace, as an admin at rank 20 — and every member write then
// scoped by `actor.OrgID` alone, so `mayManage` compared that borrowed rank
// against their **own organization-wide membership** and answered yes. One
// dropdown on /members made them an organization-wide admin (F27).
//
// The rule this restores is the one `LockOrganizationOwners` already states in
// SQL: a workspace-scoped membership grants authority over its own workspace,
// not over the organization.
type Authority struct {
	// Granted is whether any membership reaching the scope grants the
	// permission. False is the whole refusal — no rank comparison follows.
	Granted bool
	// Rank is the lowest rank among the memberships that both reach the scope
	// **and** grant the permission: the authority actually being carried, which
	// is what a rank bound must be evaluated against. NoRoleRank when none does,
	// so an ungranted Authority outranks nothing.
	Rank int32
	// Role is the slug behind Rank, for refusals that name the rule rather than
	// the person. Empty when nothing was granted.
	Role string
}

// MembershipAuthority answers Authority per scope, from one load of an actor's
// memberships in one organization.
//
// Loaded once and folded per scope rather than queried per object, because the
// member list asks the same question for every row it draws a control on and a
// query per row is a query per row. An organization's memberships are a handful
// by construction — the same reason ListMembers is not paginated.
type MembershipAuthority struct {
	permission string
	rows       []dbgen.ListMembershipAuthorityRow
}

// LoadMembershipAuthority reads an actor's memberships in one organization,
// with the rank and the permission grant each carries.
//
// The queries handle is a parameter so a caller inside a transaction passes its
// own: the authority a write is authorized by must be read under the same lock
// the write takes, or it is a check-then-act.
func LoadMembershipAuthority(
	ctx context.Context, q *dbgen.Queries, userID, orgID uuid.UUID, permission string,
) (*MembershipAuthority, error) {
	rows, err := q.ListMembershipAuthority(ctx, dbgen.ListMembershipAuthorityParams{
		UserID: userID, OrganizationID: orgID, Permission: permission,
	})
	if err != nil {
		return nil, fmt.Errorf("load %s authority: %w", permission, err)
	}
	return &MembershipAuthority{permission: permission, rows: rows}, nil
}

// LoadMemberships is the same load with the permission question left out: which
// scopes does this actor hold a membership in at all, never mind what it grants.
//
// It exists for M54. An account-wide API key's authority has to be established
// in an organization the caller is *not* acting in — an administrator cutting
// their tenant out of somebody else's key has to know whether the key reaches
// it — and the question there is membership, not permission. Passing a
// permission slug would be noise a later reader would try to interpret; the
// empty string matches no row in `permissions`, so every GrantsPermission comes
// back false and Reaches below is the only honest thing to ask of the result.
func LoadMemberships(
	ctx context.Context, q *dbgen.Queries, userID, orgID uuid.UUID,
) (*MembershipAuthority, error) {
	return LoadMembershipAuthority(ctx, q, userID, orgID, "")
}

// Reaches reports whether any membership covers the scope, ignoring what it
// grants. In's question minus the permission.
//
// A nil workspaceID is the organization-wide scope, and only an
// organization-wide membership reaches it — the same asymmetry In relies on,
// and the reason this is the right test for an unpinned key. Such a key has
// always required an organization-wide membership (GetAPIKeyByPrefix's
// predicate refuses one covered by a workspace-scoped row), so asking whether
// it reaches a second organization is asking exactly this.
//
// A nil receiver reaches nothing, for the reason In grants nothing.
func (m *MembershipAuthority) Reaches(workspaceID *uuid.UUID) bool {
	if m == nil {
		return false
	}
	for _, row := range m.rows {
		if row.WorkspaceID == nil {
			return true
		}
		if workspaceID != nil && *row.WorkspaceID == *workspaceID {
			return true
		}
	}
	return false
}

// In answers for one scope.
//
// A nil workspaceID is the **organization-wide** scope, which only an
// organization-wide membership reaches — that asymmetry is the entire point,
// and it is why this is not simply GetUserPermissions with a different
// signature. A set one is that workspace, which an organization-wide membership
// reaches as well, because such a membership covers every workspace in the
// organization.
//
// A nil receiver answers ungranted, so a caller that skipped the load because
// the actor holds nothing cannot accidentally read authority out of it.
func (m *MembershipAuthority) In(workspaceID *uuid.UUID) Authority {
	out := Authority{Rank: NoRoleRank}
	if m == nil {
		return out
	}
	for _, row := range m.rows {
		if !row.GrantsPermission {
			continue
		}
		if row.WorkspaceID != nil {
			if workspaceID == nil || *row.WorkspaceID != *workspaceID {
				continue
			}
		}
		if out.Granted && row.RoleRank >= out.Rank {
			continue
		}
		out.Granted, out.Rank, out.Role = true, row.RoleRank, row.RoleSlug
	}
	return out
}

// Scopes is the same answer In gives, turned inside out: instead of "may this
// actor exercise the permission over that object", it is "which scopes may they
// exercise it over at all".
//
// orgWide true means an organization-wide membership grants it, which reaches
// every workspace in the organization — the workspace list is then redundant and
// the caller should ignore it. Otherwise the list is exactly the workspaces
// whose own membership grants it, and it may be empty.
//
// It exists because a *read* has no single object to ask In about. F31 is that
// gap: ListAuditLogs was scoped by the actor's organization alone, so a
// workspace-scoped admin read the rows of workspaces they hold no membership in.
// Answering that per row would be a query per row; answering it as a predicate
// needs the set, and this is the set.
//
// A nil receiver answers "nothing, nowhere", so a caller that skipped the load
// cannot read authority out of it.
func (m *MembershipAuthority) Scopes() (orgWide bool, workspaceIDs []uuid.UUID) {
	if m == nil {
		return false, nil
	}
	for _, row := range m.rows {
		if !row.GrantsPermission {
			continue
		}
		if row.WorkspaceID == nil {
			orgWide = true
			continue
		}
		workspaceIDs = append(workspaceIDs, *row.WorkspaceID)
	}
	return orgWide, workspaceIDs
}

// Permission is the permission this was loaded for, so a refusal can name it
// without the caller carrying the slug alongside.
func (m *MembershipAuthority) Permission() string {
	if m == nil {
		return ""
	}
	return m.permission
}
