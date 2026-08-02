-- Membership management: who is in an organization, at what rank, and where
-- that rank reaches (M28).
--
-- The rows these statements read and write are the ones 00200 has carried since
-- Phase 1. Nothing here is new schema; what is new is that a person can change
-- them, which is why every write is scoped by organization_id as well as by id.

-- name: ListMembers :many
-- Every membership in an organization, most powerful first.
--
-- One row per membership, not per user. A user holding an organization-wide
-- membership and a workspace-scoped one appears twice, and that is the shape the
-- page has to show: under D31 the two rows *add*, and collapsing them into one
-- would hide the second grant behind the first.
--
-- The workspace name is joined rather than looked up per row, and is NULL for an
-- organization-wide membership — which is what the absence of a workspace_id
-- means, and the distinction the list is read for.
--
-- Not paginated. An organization's membership is a handful of rows by
-- construction, exactly as its invitations are.
SELECT
    m.id,
    m.user_id,
    m.workspace_id,
    m.created_at,
    u.email,
    u.name,
    u.status,
    r.slug AS role_slug,
    r.name AS role_name,
    r.rank AS role_rank,
    w.name AS workspace_name
FROM memberships m
JOIN users u        ON u.id = m.user_id
JOIN roles r        ON r.id = m.role_id
LEFT JOIN workspaces w ON w.id = m.workspace_id AND w.deleted_at IS NULL
WHERE m.organization_id = $1
  AND u.deleted_at IS NULL
ORDER BY r.rank, u.email, m.workspace_id NULLS FIRST, m.id;

-- name: GetMembership :one
-- One membership, scoped by organization so an id from elsewhere is
-- indistinguishable from one that never existed.
--
-- FOR UPDATE OF m: every caller is about to re-role or delete this row, and the
-- rank check that decides whether they may is read from it. Without the lock,
-- two administrators acting at once could each read a state the other is
-- changing — the check-then-act that the last-owner refusal exists to prevent.
SELECT
    m.id,
    m.user_id,
    m.organization_id,
    m.workspace_id,
    u.email,
    u.name,
    r.slug AS role_slug,
    r.rank AS role_rank
FROM memberships m
JOIN users u ON u.id = m.user_id
JOIN roles r ON r.id = m.role_id
WHERE m.id = $1
  AND m.organization_id = $2
FOR UPDATE OF m;

-- name: LockOrganizationOwners :many
-- The organization's owner memberships, locked.
--
-- Organization-wide only: a workspace-scoped owner membership grants ownership
-- of one workspace, not of the organization, so counting it would let the last
-- real owner be removed while a workspace-scoped row stood in for them.
--
-- Rows rather than a count, because a count cannot be locked. Taken before any
-- removal or demotion of an owner, so two concurrent administrators cannot each
-- observe two owners and each remove one.
SELECT m.id
FROM memberships m
JOIN roles r ON r.id = m.role_id
WHERE m.organization_id = $1
  AND m.workspace_id IS NULL
  AND r.slug = 'owner'
  AND r.organization_id IS NULL
FOR UPDATE OF m;

-- name: ListMembershipAuthority :many
-- Every membership one user holds in an organization, with the rank it carries
-- and whether the role behind it grants a named permission.
--
-- This is the authorization side of the sentence LockOrganizationOwners states
-- just above: **a workspace-scoped membership grants authority over its own
-- workspace, not over the organization.** The evaluator answers a different
-- question — what may this person do in the workspace they are *acting in* —
-- by taking the union of every matching membership and the lowest rank among
-- them (D31), which is right for an object that lives in a workspace and wrong
-- for one that spans the organization. A workspace-scoped admin resolved into
-- their own workspace otherwise carries rank 20 against an organization-wide
-- membership their membership does not reach at all, and F27 walked exactly
-- that: one dropdown on /members turned them into an organization-wide admin.
--
-- So the rows come back unfolded, one per membership, and the caller keeps the
-- ones that reach the scope of the object being written. A membership scoped to
-- a deleted workspace reaches nothing, matching GetUserPermissions.
--
-- The permission is a parameter rather than a join in Go because the answer is
-- per role, not per membership: two memberships at the same role give the same
-- answer, and asking the database means the grant is read from
-- role_permissions — the same table the evaluator reads — rather than from a
-- second list of which roles hold what.
SELECT
    m.id,
    m.workspace_id,
    r.slug AS role_slug,
    r.rank AS role_rank,
    EXISTS (
        SELECT 1
        FROM role_permissions rp
        JOIN permissions p ON p.id = rp.permission_id
        WHERE rp.role_id = m.role_id
          AND p.slug = @permission
    ) AS grants_permission
FROM memberships m
JOIN roles r ON r.id = m.role_id
LEFT JOIN workspaces w ON w.id = m.workspace_id
WHERE m.user_id = @user_id
  AND m.organization_id = @organization_id
  AND (m.workspace_id IS NULL OR w.deleted_at IS NULL)
ORDER BY r.rank, m.workspace_id NULLS FIRST;

-- name: UpdateMembershipRole :execrows
-- Scoped by organization as well as id, so the authorization decision the
-- service made cannot be applied to a row in another tenant.
UPDATE memberships
   SET role_id = $3,
       updated_at = now()
 WHERE id = $1
   AND organization_id = $2;

-- name: DeleteMembership :execrows
DELETE FROM memberships
 WHERE id = $1
   AND organization_id = $2;

-- name: GetOrganizationMember :one
-- Whether a user is in an organization at all, for the grant path:
-- workspace-scoped access is given to somebody who is already a member, and
-- this is what establishes that.
--
-- **Any** membership counts, organization-wide or workspace-scoped. Requiring
-- an organization-wide one would be a dead end: somebody left holding only a
-- workspace-scoped membership could never be given a second workspace, because
-- re-inviting them is refused as already-a-member. Under D31 every grant adds,
-- so widening a scoped member to a second workspace is the same kind of act as
-- the first grant was.
--
-- The organization-wide row wins the tiebreak so the label and role this
-- returns are the person's broadest, which is what a control naming them should
-- show.
SELECT
    m.id,
    m.user_id,
    u.email,
    u.name,
    r.slug AS role_slug,
    r.rank AS role_rank
FROM memberships m
JOIN users u ON u.id = m.user_id
JOIN roles r ON r.id = m.role_id
WHERE m.organization_id = $1
  AND m.user_id = $2
  AND u.deleted_at IS NULL
ORDER BY m.workspace_id NULLS FIRST, r.rank
LIMIT 1;

-- name: CountWorkspaceLinks :one
-- What D32 refuses a workspace deletion on.
--
-- Soft-deleted links are excluded on purpose. `links`, `tags` and `folders` all
-- cascade from `workspaces`, so the guard is in front of a real cascade — but a
-- link the owner already deleted is one they cannot delete again, and counting
-- it would leave the workspace undeletable until the purge job ran, with nothing
-- the person could do about it. Archived links **are** counted: an archived link
-- keeps its alias and its click history, so cascading it away would be silent
-- data loss dressed as tidying up.
SELECT count(*) FROM links
WHERE workspace_id = $1 AND deleted_at IS NULL;

-- name: CountOrganizationWorkspaces :one
-- Whether this is the organization's last workspace.
--
-- Deleting it would leave every member of the organization resolving into no
-- workspace at all, which `ResolveWorkspaceForUser` reports as a broken instance
-- rather than as an empty state — so the account could not authenticate. Guarded
-- for the same reason the last owner is.
SELECT count(*) FROM workspaces
WHERE organization_id = $1 AND deleted_at IS NULL;
