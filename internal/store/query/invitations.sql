-- Invitations: issuing, listing, revoking and redeeming (M27).

-- name: CreateInvitation :one
INSERT INTO invitations (
    id, organization_id, email, role_id, token_hash, invited_by, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: RevokeLapsedInvitation :execrows
-- Clears an expired invite out of the outstanding slot so a replacement can be
-- issued.
--
-- The partial unique index cannot exclude expired rows — `now()` is not
-- immutable, so Postgres will not index on it — which means an invite that
-- lapsed still occupies the address. This runs immediately before the insert,
-- in the same transaction, and touches nothing that is still redeemable.
UPDATE invitations
   SET revoked_at = now()
 WHERE organization_id = $1
   AND email_lower = lower(sqlc.arg(email)::text)
   AND revoked_at IS NULL
   AND redeemed_at IS NULL
   AND expires_at <= now();

-- name: ListInvitations :many
-- The administrator's list, newest first.
--
-- No pagination and no cursor. An organization's outstanding invitations are a
-- handful of rows by construction, and redeemed ones stop accumulating the
-- moment people join; the link list's machinery here would be a page that
-- cannot fill.
--
-- The inviter is joined as a label rather than an id, for the same reason the
-- audit log stores one: the row has to stay readable after that account is
-- gone, and the LEFT JOIN is what lets it.
SELECT
    i.id, i.email, i.created_at, i.expires_at, i.revoked_at, i.redeemed_at,
    r.slug AS role_slug,
    r.rank AS role_rank,
    coalesce(u.email, '') AS invited_by_label
FROM invitations i
JOIN roles r      ON r.id = i.role_id
LEFT JOIN users u ON u.id = i.invited_by
WHERE i.organization_id = $1
ORDER BY i.created_at DESC, i.id DESC;

-- name: RevokeInvitation :execrows
-- Scoped by organization as well as id, so an id from another organization is
-- indistinguishable from one that does not exist: both change zero rows.
--
-- Already-revoked and already-redeemed are excluded rather than tolerated. A
-- redeemed invite has produced a member, and reporting "revoked" for it would
-- claim something the tree does not support.
UPDATE invitations
   SET revoked_at = now()
 WHERE id = $1
   AND organization_id = $2
   AND revoked_at IS NULL
   AND redeemed_at IS NULL;

-- name: GetInvitationByTokenHash :one
-- Redemption's only lookup, and the row it locks.
--
-- FOR UPDATE OF i serializes two redemptions of the same token: the second
-- blocks until the first commits and then reads redeemed_at set, so single-use
-- is enforced by the database rather than by a check-then-act in Go. The joined
-- tables are not locked — they are read for their labels, and locking a role
-- row would block every other invite that names it.
--
-- Expiry and revocation are deliberately NOT in the WHERE clause. The caller
-- has to tell "no such token" from "expired" apart to decide what to log, and
-- it answers all of them identically to the person redeeming (decision D27).
SELECT
    i.id, i.organization_id, i.email, i.email_lower, i.role_id,
    i.invited_by, i.created_at, i.expires_at, i.revoked_at, i.redeemed_at,
    r.slug AS role_slug,
    o.name AS organization_name
FROM invitations i
JOIN roles r         ON r.id = i.role_id
JOIN organizations o ON o.id = i.organization_id
WHERE i.token_hash = $1
  AND o.deleted_at IS NULL
FOR UPDATE OF i;

-- name: PeekInvitationByTokenHash :one
-- The same lookup without the lock, for rendering the redemption page.
--
-- A GET must not take a row lock: the page is served to anybody holding the
-- link, and a locking read there would let a stranger hold a write lock on the
-- row by opening a page.
SELECT
    i.id, i.organization_id, i.email, i.email_lower, i.role_id,
    i.invited_by, i.created_at, i.expires_at, i.revoked_at, i.redeemed_at,
    r.slug AS role_slug,
    o.name AS organization_name
FROM invitations i
JOIN roles r         ON r.id = i.role_id
JOIN organizations o ON o.id = i.organization_id
WHERE i.token_hash = $1
  AND o.deleted_at IS NULL;

-- name: MarkInvitationRedeemed :execrows
-- The single-use write. Conditional on the invite still being redeemable, so
-- even without the lock above this could not be spent twice.
UPDATE invitations
   SET redeemed_at = now(),
       redeemed_by = $2
 WHERE id = $1
   AND revoked_at IS NULL
   AND redeemed_at IS NULL
   AND expires_at > now();

-- name: CountMembershipsForEmail :one
-- Whether the person at this address is already in this organization.
--
-- Asked at creation, where the actor holds members.write on the organization
-- and could read its member list anyway, so answering it discloses nothing they
-- could not already see. Redemption asks the same question of a user id it has
-- already resolved, and answers it with the same generic refusal as every other
-- failure.
SELECT count(*) FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.organization_id = $1
  AND u.email_lower = lower(sqlc.arg(email)::text)
  AND u.deleted_at IS NULL;

-- name: CountMembershipsForUser :one
SELECT count(*) FROM memberships
WHERE organization_id = $1 AND user_id = $2;

-- name: ListBuiltinRoles :many
-- The four seeded roles, most powerful first. Feeds the invite form's role
-- choices, filtered by the inviter's own rank in the service (decision D28).
SELECT id, slug, name, description, rank
FROM roles
WHERE organization_id IS NULL AND is_builtin
ORDER BY rank;

-- name: GetOrganizationName :one
-- What to call the organization in an invitation. A primary-key lookup on a
-- path that is already writing a row, rather than carrying a name on every
-- identity for the one surface that needs it.
SELECT name FROM organizations WHERE id = $1 AND deleted_at IS NULL;

-- name: GetBuiltinRoleBySlug :one
SELECT id, slug, name, rank
FROM roles
WHERE slug = $1 AND organization_id IS NULL;
