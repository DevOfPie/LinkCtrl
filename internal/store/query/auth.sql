-- Users, sessions and tenancy provisioning.

-- name: CountUsers :one
-- Drives the first-run setup flow: /setup exists only while this is zero.
SELECT count(*) FROM users WHERE deleted_at IS NULL;

-- name: GetUserByEmail :one
-- Comparison is on the generated email_lower column, so callers cannot
-- accidentally do a case-sensitive lookup and create a duplicate account.
SELECT * FROM users
WHERE email_lower = lower(sqlc.arg(email)::text)
  AND deleted_at IS NULL;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1 AND deleted_at IS NULL;

-- name: CreateUser :one
INSERT INTO users (id, email, name, password_hash, status, email_verified_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpdateUserPassword :exec
UPDATE users
   SET password_hash = $2,
       failed_login_count = 0,
       locked_until = NULL,
       updated_at = now()
 WHERE id = $1;

-- name: LockFirstUserSetup :exec
-- Serializes the setup flow's count-then-create.
--
-- Both setup surfaces read CountUsers and then, in a separate transaction,
-- register the first user. Nothing held the gap, and the gap is wide: the
-- argon2 hash runs for ~100ms before the transaction even begins. On a fresh
-- closed instance, setup is unauthenticated and only login-rate-limited, so an
-- attacker polling it could have their CountUsers land in the window while the
-- real operator was hashing, and both would be created as "the first user" —
-- each with their own organization, on an instance the operator believes only
-- they can reach.
--
-- Transaction-scoped, so it releases on commit or rollback with nothing to
-- clean up. The key is the ASCII bytes "lcsetup\0" as a literal, NOT a hash of
-- anything; to inspect it from psql use the value directly:
--
--     SELECT pg_advisory_xact_lock(7810213058373316608);
SELECT pg_advisory_xact_lock(7810213058373316608);

-- name: RecordSuccessfulLogin :exec
UPDATE users
   SET last_login_at = now(),
       failed_login_count = 0,
       locked_until = NULL,
       updated_at = now()
 WHERE id = $1;

-- name: RecordFailedLogin :one
-- Returns the new count so the caller can apply the lockout policy without a
-- second round trip and without a read-modify-write race between two
-- concurrent attempts.
--
-- An elapsed lockout starts the count over. Incrementing unconditionally meant
-- the counter only ever went down on a successful sign-in or a password change,
-- so once an account had been locked it sat at the threshold forever: the user
-- waited out the window, got one attempt, and a single wrong guess re-locked
-- them for the full duration. The lockout became permanent for anyone who could
-- not remember their password on the first try — which is the population it
-- applies to.
UPDATE users
   SET failed_login_count = CASE
           WHEN locked_until IS NOT NULL AND locked_until <= now() THEN 1
           ELSE failed_login_count + 1
       END,
       -- Seconds rather than an interval literal: an interval parameter maps
       -- to pgtype.Interval, which would leak a driver type into the service
       -- layer for no benefit.
       locked_until = CASE
           WHEN sqlc.arg(threshold)::int <= 0 THEN locked_until
           WHEN locked_until IS NOT NULL AND locked_until <= now()
           THEN CASE
               WHEN 1 >= sqlc.arg(threshold)::int
               THEN now() + make_interval(secs => sqlc.arg(lockout_seconds)::int)
               ELSE NULL
           END
           WHEN failed_login_count + 1 >= sqlc.arg(threshold)::int
           THEN now() + make_interval(secs => sqlc.arg(lockout_seconds)::int)
           ELSE locked_until
       END,
       updated_at = now()
 WHERE id = $1
RETURNING failed_login_count, locked_until;

-- name: CreateOrganization :one
INSERT INTO organizations (id, name, slug, is_personal)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: CreateWorkspace :one
INSERT INTO workspaces (id, organization_id, name, slug)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: CreateMembership :one
INSERT INTO memberships (id, user_id, organization_id, role_id, workspace_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetRoleBySlug :one
SELECT * FROM roles WHERE slug = $1 AND organization_id IS NULL;

-- name: GetDefaultWorkspaceForUser :one
-- The workspace a user lands in with no explicit selection. Ordered so the
-- result is deterministic rather than whatever the planner returns first.
SELECT w.*
FROM workspaces w
JOIN memberships m ON m.organization_id = w.organization_id
WHERE m.user_id = $1
  AND w.deleted_at IS NULL
ORDER BY w.created_at, w.id
LIMIT 1;

-- name: CreateSession :one
INSERT INTO sessions (id, user_id, token_hash, ip_prefix, user_agent, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetSessionByTokenHash :one
-- Joined with the user so validating a session is one round trip on a path
-- that runs for every authenticated request. Filters revoked and deleted here
-- rather than in Go, so a revoked session cannot be resurrected by a caller
-- that forgets to check.
SELECT
    s.id, s.user_id, s.created_at, s.last_seen_at, s.expires_at,
    u.email, u.name, u.status, u.password_hash
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1
  AND s.revoked_at IS NULL
  AND u.deleted_at IS NULL;

-- name: TouchSession :exec
-- Idle expiry is measured from last_seen_at. Updated at most once a minute by
-- the caller, because writing on every request would turn a read-mostly path
-- into a write on the hottest authenticated query.
UPDATE sessions SET last_seen_at = now() WHERE id = $1;

-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeAllUserSessions :exec
-- Used on password change. Anyone who had the old password must be logged out,
-- which is the entire point of changing it.
-- keep_session is optional: pass NULL to revoke everything, or the current
-- session's id to leave the browser the user is changing their password in
-- still signed in.
UPDATE sessions
   SET revoked_at = now()
 WHERE user_id = $1
   AND revoked_at IS NULL
   AND (sqlc.narg(keep_session)::uuid IS NULL OR id <> sqlc.narg(keep_session)::uuid);

-- name: ListUserSessions :many
SELECT id, ip_prefix, user_agent, created_at, last_seen_at, expires_at
FROM sessions
WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()
ORDER BY last_seen_at DESC;

-- name: DeleteExpiredSessions :execrows
-- Reaper. Revoked rows are kept briefly so "sign out everywhere" is visible in
-- the session list before it disappears.
DELETE FROM sessions
WHERE expires_at < now() - interval '7 days'
   OR (revoked_at IS NOT NULL AND revoked_at < now() - interval '7 days');

-- name: ListUsers :many
SELECT id, email, name, status, last_login_at, created_at
FROM users
WHERE deleted_at IS NULL
ORDER BY created_at;

-- name: GetUserPermissions :many
-- The RBAC evaluator's source of truth. Returns every permission a user holds
-- in a workspace, via their organization membership and its role.
--
-- A NULL memberships.workspace_id means the membership covers every workspace
-- in the organization, which is what Phase 1 always creates.
SELECT DISTINCT p.slug
FROM memberships m
JOIN workspaces w  ON w.organization_id = m.organization_id
JOIN role_permissions rp ON rp.role_id = m.role_id
JOIN permissions p ON p.id = rp.permission_id
WHERE m.user_id = $1
  AND w.id = $2
  AND w.deleted_at IS NULL
  AND (m.workspace_id IS NULL OR m.workspace_id = w.id)
ORDER BY p.slug;

-- name: GetUserRoleInWorkspace :one
SELECT r.slug, r.name, r.rank
FROM memberships m
JOIN workspaces w ON w.organization_id = m.organization_id
JOIN roles r ON r.id = m.role_id
WHERE m.user_id = $1
  AND w.id = $2
  AND (m.workspace_id IS NULL OR m.workspace_id = w.id)
ORDER BY r.rank
LIMIT 1;
