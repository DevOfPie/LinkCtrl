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
SELECT
    k.id, k.user_id, k.organization_id, k.workspace_id,
    k.key_hash, k.scopes, k.expires_at, k.revoked_at, k.grace_expires_at,
    u.email, u.name AS user_name, u.status
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
SELECT id, user_id, organization_id, workspace_id, name, prefix, scopes,
       expires_at, revoked_at, rotated_at, grace_expires_at, successor_id,
       created_at
FROM api_keys
WHERE id = $1
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
