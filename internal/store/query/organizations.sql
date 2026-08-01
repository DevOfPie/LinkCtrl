-- Organization teardown (M28.5).
--
-- Every statement here is either a guard or the delete it guards. The delete is
-- one line; the guards are the milestone.
--
-- **Locking, and why each of these returns rows rather than a count.** A count
-- cannot be locked, so a guard written as `SELECT count(*)` is a check-then-act:
-- two administrators acting at once each read a state the other is changing.
-- The pattern is `LockOrganizationOwners`' — select the rows the decision is
-- made on `FOR UPDATE`, count them in Go, and let the second transaction block
-- until the first commits and then re-read what it left behind. Postgres also
-- gives this a second effect that is the point of it here: inserting a row that
-- references a locked parent takes `FOR KEY SHARE` on that parent, which
-- conflicts with `FOR UPDATE` — so a locked organization cannot acquire a new
-- workspace, and a locked workspace cannot acquire a new link, while the guard
-- is deciding.

-- name: LockOrganizations :many
-- Every live organization on the instance, locked, for the refusal that stops
-- the last one being deleted.
--
-- Instance-wide rather than scoped, because that is what the rule is about: an
-- instance with no organization has no path back that does not involve SQL, the
-- same argument that refuses the last owner and the last workspace.
--
-- `ORDER BY id` is load-bearing rather than cosmetic. Two concurrent deletions
-- of different organizations both take this lock, and taking a set of row locks
-- in a different order in each transaction is a deadlock; a fixed order makes it
-- a wait instead. It is also why this runs *before* the target row is read —
-- the target is inside this set, so it is already locked by the time anything
-- else touches it.
SELECT id FROM organizations
WHERE deleted_at IS NULL
ORDER BY id
FOR UPDATE;

-- name: GetOrganization :one
-- The organization being deleted, read after LockOrganizations has locked it.
--
-- Its name and slug are read here because the audit record has to carry them:
-- once the row is gone that record is the only remaining trace of what was
-- deleted, exactly as it is for a workspace.
SELECT * FROM organizations
WHERE id = $1 AND deleted_at IS NULL;

-- name: LockOrganizationWorkspaces :many
-- The organization's workspaces, locked, so no link can be created in one while
-- the link guard below is counting.
--
-- Ordered for the same deadlock reason as LockOrganizations.
SELECT id FROM workspaces
WHERE organization_id = $1 AND deleted_at IS NULL
ORDER BY id
FOR UPDATE;

-- name: CountOrganizationLinks :one
-- What D37 refuses an organization deletion on, and it is deliberately the same
-- shape as CountWorkspaceLinks one level up.
--
-- Archived links count, soft-deleted ones do not — the reasoning is D32's and is
-- written out there. What is new here is *why the org level asks the question at
-- all*: cascading through these links would make D32 bypassable by deleting one
-- level up, which turns a rule into a speed bump. So the organization refuses on
-- exactly the rows its workspaces would refuse on.
SELECT count(*) FROM links l
JOIN workspaces w ON w.id = l.workspace_id
WHERE w.organization_id = $1
  AND w.deleted_at IS NULL
  AND l.deleted_at IS NULL;

-- name: DeleteOrganization :execrows
-- A real delete, not a soft one, and everything in 00200/00300/00500/01200 that
-- references it cascades: workspaces and everything under them, memberships,
-- invitations, API keys, and any custom domain.
--
-- Two things deliberately do not. `audit_logs.organization_id` carries no
-- foreign key, so the trail this organization wrote survives the row it
-- describes — a tenancy teardown that erased its own record is the one shape an
-- audit log must not have. And the instance default domain has
-- `organization_id IS NULL`, so it and the `reserved_aliases` rows keyed to it
-- are untouched.
DELETE FROM organizations
 WHERE id = $1 AND deleted_at IS NULL;

-- name: CountUserMemberships :one
-- Whether an account belongs to anything at all.
--
-- Read by exactly one caller: the first-organization path (D36). An account with
-- no membership holds no role and therefore no `orgs.create`, so without this
-- the prompt to create an organization would lead somewhere it is refused. This
-- is a check on **present state** — how many memberships exist right now — and
-- not on how the account was made, which is the distinction D16 was drawing when
-- it made the permission a grant rather than a provenance test.
SELECT count(*) FROM memberships WHERE user_id = $1;
