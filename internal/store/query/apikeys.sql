-- API keys and the permission vocabulary their scopes are drawn from.

-- name: CreateAPIKey :one
INSERT INTO api_keys (
    id, user_id, organization_id, workspace_id,
    name, prefix, key_hash, scopes, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetAPIKeyByPrefix :one
-- The verification lookup, on the unique prefix index, joined with the user so
-- authentication is one round trip. Revoked and expired keys are returned
-- rather than filtered out: the caller distinguishes them so the response can
-- say which it was, and a deleted user's key resolves to no row at all.
--
-- grace_expires_at comes back for the same reason revoked_at does: a rotated
-- predecessor stops verifying when its window closes, and that refusal is
-- decided here rather than by the housekeeping job that later writes revoked_at.
--
-- owner_is_member is the membership the key leans on, asked here so that
-- authentication stays one round trip. A key acts *as its owner*, and an owner
-- with no membership covering the key's scope has no authority for it to act
-- with — so the credential is invalid rather than merely powerless. The
-- predicate matches GetUserPermissions exactly: an organization-wide membership
-- covers every workspace, a workspace-scoped one covers its own, and an
-- organization-wide key is covered only by an organization-wide membership,
-- because NULL = NULL is not true.
--
-- Returned as a column rather than joined into the WHERE clause for the reason
-- revoked and expired keys are returned rather than filtered: the caller decides
-- what each state means, and a refusal a reader can see beside the others is
-- worth more than a row that silently fails to exist.
SELECT
    k.id, k.user_id, k.organization_id, k.workspace_id,
    k.key_hash, k.scopes, k.expires_at, k.revoked_at, k.grace_expires_at,
    u.email, u.name AS user_name, u.status,
    EXISTS (
        SELECT 1 FROM memberships m
         WHERE m.user_id = k.user_id
           AND m.organization_id = k.organization_id
           AND (m.workspace_id IS NULL OR m.workspace_id = k.workspace_id)
    ) AS owner_is_member
FROM api_keys k
JOIN users u ON u.id = k.user_id
WHERE k.prefix = $1
  AND u.deleted_at IS NULL;

-- name: ListAPIKeysForUser :many
-- Revoked keys are included. "Which keys existed and when were they revoked"
-- is the question asked after an incident, so they are listed until the reaper
-- removes them.
--
-- workspace_id is selected because NULL is a state the owner chose (M44): a key
-- bound to one workspace and a key valid across the organization look identical
-- without it.
SELECT id, name, prefix, scopes, workspace_id, last_used_at, expires_at,
       revoked_at, rotated_at, grace_expires_at, successor_id, created_at
FROM api_keys
WHERE user_id = $1
  AND organization_id = $2
ORDER BY created_at DESC;

-- name: GetAPIKeyForRotation :one
-- The predecessor, locked, so two rotations of one key serialize instead of
-- racing.
--
-- FOR UPDATE rather than an optimistic conditional write: the loser of a race
-- should be told "this key has already been rotated" by the check that follows,
-- which is a sentence somebody can act on, not have its transaction rolled back
-- by a unique-index violation on a column it never named.
--
-- owner_is_member and owner_status come back for the reason revoked_at does:
-- they are re-read here, under the lock, so a membership removed or an account
-- deactivated between authentication and this statement wins. Without them a
-- removal racing a rotation leaves behind a successor the removal was meant to
-- kill, on a chain that can rotate again. A soft-deleted account reads
-- 'deleted' rather than NULL, so the one comparison at the call site covers it
-- and no branch depends on a scan of an absent row.
SELECT k.id, k.user_id, k.organization_id, k.workspace_id, k.name, k.prefix,
       k.scopes, k.expires_at, k.revoked_at, k.rotated_at, k.grace_expires_at,
       k.successor_id, k.created_at,
       EXISTS (
           SELECT 1 FROM memberships m
            WHERE m.user_id = k.user_id
              AND m.organization_id = k.organization_id
              AND (m.workspace_id IS NULL OR m.workspace_id = k.workspace_id)
       ) AS owner_is_member,
       COALESCE((SELECT u.status FROM users u
                  WHERE u.id = k.user_id AND u.deleted_at IS NULL), 'deleted') AS owner_status
FROM api_keys k
WHERE k.id = $1
FOR UPDATE;

-- name: MarkAPIKeyRotated :execrows
-- Closes the predecessor: names its successor and sets the far edge of the
-- grace window.
--
-- `successor_id IS NULL` in the WHERE clause is belt to the FOR UPDATE braces.
-- The lock is what serializes; this is what makes the second writer a no-op
-- rather than a silent overwrite if the lock is ever dropped from the read.
UPDATE api_keys
   SET rotated_at = now(),
       grace_expires_at = $2,
       successor_id = $3
 WHERE id = $1
   AND successor_id IS NULL;

-- name: RevokeLapsedAPIKeyGraces :execrows
-- Auto-revocation, from housekeeping.
--
-- revoked_at is set to the moment the window closed rather than to now(), so
-- the list says when the key stopped working instead of when the job noticed.
-- Nothing depends on this running: authentication already refuses a key past
-- grace_expires_at. What this buys is a key list that agrees with the behaviour.
UPDATE api_keys
   SET revoked_at = grace_expires_at
 WHERE revoked_at IS NULL
   AND grace_expires_at IS NOT NULL
   AND grace_expires_at <= now();

-- name: RevokeAPIKey :execrows
-- Idempotent: revoking an already-revoked key keeps the original timestamp and
-- still reports one row, so a repeated call is a success rather than a 404
-- while a genuinely unknown id is still distinguishable.
UPDATE api_keys
   SET revoked_at = COALESCE(revoked_at, now())
 WHERE id = $1
   AND user_id = $2;

-- name: RevokeAPIKeyInOrganization :one
-- The administrator's revoke, keyed on the organization instead of on the
-- owner.
--
-- RevokeAPIKey above is a person disabling their own credential and is the
-- normal path. This one exists because there was otherwise no path at all: a key
-- belonging to somebody else could be *seen* — the rotation records are
-- organization-scoped — and not stopped, so an administrator holding an incident
-- had to wait for its owner. Scoped by organization rather than by workspace
-- because a key is issued into an organization and its id is what an audit
-- record hands the reader.
--
-- Returns the owner and the prefix rather than a row count, because this write
-- is audited and the record has to name whose credential was stopped. No row
-- means an id from another organization or none at all, and both answer the same
-- way at the call site.
UPDATE api_keys
   SET revoked_at = COALESCE(revoked_at, now())
 WHERE id = $1
   AND organization_id = $2
RETURNING user_id, prefix;

-- name: TouchAPIKeys :exec
-- Batch write of last_used_at, from the coalescing tracker rather than from the
-- request path: authenticating a key must not cost a synchronous write.
--
-- GREATEST guards against a late batch moving the timestamp backwards, which
-- two processes flushing out of order would otherwise do.
UPDATE api_keys AS k
   SET last_used_at = GREATEST(COALESCE(k.last_used_at, to_timestamp(0)), v.used_at)
  FROM (
      SELECT unnest(sqlc.arg(ids)::uuid[])          AS id,
             unnest(sqlc.arg(used_at)::timestamptz[]) AS used_at
  ) AS v
 WHERE k.id = v.id;

-- name: DeleteRevokedAPIKeys :execrows
-- Reaper. Kept long enough to be visible in the key list after revocation, and
-- long enough for the audit question above to be answerable.
DELETE FROM api_keys
WHERE revoked_at IS NOT NULL
  AND revoked_at < now() - interval '90 days';

-- name: ListPermissionSlugs :many
-- The scope vocabulary. Scopes are validated against the permissions table
-- rather than a list in Go, so RBAC and API keys cannot drift apart.
SELECT slug FROM permissions ORDER BY slug;
