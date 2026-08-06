-- The workspace switcher: what a user may act in, and what they have chosen.
--
-- Resolution itself is in auth.sql, because it is identity, not a feature.
-- These are the reads and writes the switcher and its account setting need.

-- name: ListWorkspacesForUser :many
-- Every workspace a user may act in, with the organization it belongs to.
--
-- DISTINCT because a user can hold both an organization-wide membership and a
-- workspace-scoped one in the same organization, which the unique index
-- permits; without it the switcher would list a workspace twice.
--
-- is_default is carried here rather than fetched separately so a page render
-- costs one query: the nav switcher needs the list, the account setting needs
-- the list plus which entry is pinned, and neither should cost two round trips.
SELECT DISTINCT
    w.id,
    w.name,
    w.slug,
    w.organization_id,
    o.name AS organization_name,
    o.is_personal,
    (w.id IS NOT DISTINCT FROM u.default_workspace_id) AS is_default
FROM workspaces w
JOIN organizations o ON o.id = w.organization_id
JOIN memberships m   ON m.organization_id = w.organization_id
JOIN users u         ON u.id = m.user_id
WHERE m.user_id = $1
  AND w.deleted_at IS NULL
  AND o.deleted_at IS NULL
  AND (m.workspace_id IS NULL OR m.workspace_id = w.id)
ORDER BY o.name, w.name, w.id;

-- name: SetLastWorkspaceForUser :execrows
-- Remembers a selection, and refuses one the user is not entitled to.
--
-- The membership check is in the statement rather than in a preceding SELECT so
-- there is no window between the two. Zero rows means "not yours or not there",
-- which the caller reports as not-found: a workspace id must not be probeable
-- for existence.
UPDATE users
   SET last_workspace_id = sqlc.arg(workspace_id)::uuid,
       updated_at = now()
 WHERE users.id = sqlc.arg(user_id)
   AND EXISTS (
       SELECT 1
       FROM workspaces w
       JOIN memberships m ON m.organization_id = w.organization_id
       WHERE w.id = sqlc.arg(workspace_id)::uuid
         AND m.user_id = sqlc.arg(user_id)
         AND w.deleted_at IS NULL
         AND (m.workspace_id IS NULL OR m.workspace_id = w.id));

-- name: SetDefaultWorkspaceForUser :execrows
-- Pins a workspace as where new sessions start, or clears the pin.
--
-- NULL is a real value here and means "follow last-used", which is the default
-- the control offers. Clearing therefore needs no membership check; setting
-- needs the same one as above.
UPDATE users
   SET default_workspace_id = sqlc.narg(workspace_id),
       updated_at = now()
 WHERE users.id = sqlc.arg(user_id)
   AND (sqlc.narg(workspace_id)::uuid IS NULL OR EXISTS (
       SELECT 1
       FROM workspaces w
       JOIN memberships m ON m.organization_id = w.organization_id
       WHERE w.id = sqlc.narg(workspace_id)::uuid
         AND m.user_id = sqlc.arg(user_id)
         AND w.deleted_at IS NULL
         AND (m.workspace_id IS NULL OR m.workspace_id = w.id)));

-- name: GetWorkspaceInOrganization :one
-- One workspace, scoped by organization so an id belonging to another tenant is
-- indistinguishable from one that does not exist.
--
-- FOR UPDATE: both callers — rename and delete — are about to write this row or
-- the rows that cascade from it, and delete reads a link count that must not
-- change underneath the decision.
SELECT * FROM workspaces
WHERE id = $1 AND organization_id = $2 AND deleted_at IS NULL
FOR UPDATE;

-- name: RenameWorkspace :one
-- Name and slug move together. The slug is derived from the name by the caller
-- rather than kept as a separate field somebody can edit into disagreement with
-- it, and the partial unique index on (organization_id, lower(slug)) is what
-- refuses a collision.
UPDATE workspaces
   SET name = $3,
       slug = $4,
       updated_at = now()
 WHERE id = $1 AND organization_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: ReserveWorkspaceTraffickedAliases :exec
-- Run immediately before DeleteWorkspace, in the same transaction, because the
-- cascade below is the third way a link lets go of its alias and it was the one
-- that let go for free (F28).
--
-- CountWorkspaceLinks excludes soft-deleted links deliberately — counting them
-- would leave a workspace undeletable until the purge job ran — so a workspace
-- reaching the delete may still hold trashed links, for up to the trash window.
-- The cascade hard-deletes them without the purge job ever seeing them, and
-- `IsAliasTaken` then stops finding the row: an alias that was on printed
-- material yesterday is claimable by anyone on the instance today.
--
-- `click_count > 0` is PurgeExpiredLinks' threshold, and the rename path's, and
-- it is the same threshold on purpose: three paths that release an alias must
-- not hold three opinions about what "in the wild" means. Aliases that never
-- received a click are released, per the reserved_aliases rationale.
--
-- No deleted_at predicate, and FOR UPDATE, for one reason between them. The
-- guard above has already established there are no live links, so this selects
-- exactly the trashed ones — unless a row stopped being trashed after the count
-- ran, which today takes a hand-written UPDATE because nothing in the product
-- un-trashes a link (`RestoreLink` restores an *archived* one and requires
-- `deleted_at IS NULL`). The statement follows what the cascade will take
-- rather than what the guard counted, so such a row is reserved rather than
-- skipped, and the lock makes it wait rather than slip between the two.
WITH doomed AS (
    SELECT domain_id, alias
      FROM links
     WHERE workspace_id = $1
       AND click_count > 0
     ORDER BY id
       FOR UPDATE
)
INSERT INTO reserved_aliases (domain_id, alias, reason)
SELECT domain_id, alias, 'workspace deleted with traffic'
  FROM doomed
ON CONFLICT DO NOTHING;

-- name: DeleteWorkspace :execrows
-- A real delete, not a soft one, and that is the decision D32 guards.
--
-- `links`, `tags` and `folders` cascade from here (00300_links.sql). Soft
-- deleting instead would leave those rows behind and their aliases still
-- serving redirects out of a workspace the dashboard says is gone, which is a
-- worse outcome than the cascade — so the guard goes in front of the delete and
-- the delete is honest about what it does.
DELETE FROM workspaces
 WHERE id = $1 AND organization_id = $2 AND deleted_at IS NULL;

-- name: SetSessionWorkspace :execrows
-- Moves one session, and only the session that asked.
--
-- Scoped by user_id as well as id so a session id from elsewhere cannot be
-- repointed, and revoked sessions are excluded because moving one would be
-- writing to a credential that no longer authenticates.
UPDATE sessions
   SET workspace_id = sqlc.arg(workspace_id)::uuid
 WHERE sessions.id = sqlc.arg(session_id)
   AND sessions.user_id = sqlc.arg(user_id)
   AND revoked_at IS NULL;
