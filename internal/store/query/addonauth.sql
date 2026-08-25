-- name: ResolveAddonIdentityLink :one
-- The only statement in this product that turns an add-on's assertion into an
-- account, and the whole of what "linking is explicit" enforces.
--
-- Three columns in the predicate and no fourth. There is deliberately no variant
-- of this keyed on the email address an assertion carries: that is the
-- account-takeover shape m65.md names, and the way it stays absent is that the
-- statement which would perform it does not exist.
--
-- The user's own row comes back with it, so the caller decides about status and
-- lockout from one read rather than from a second lookup that could disagree with
-- this one about which account it is talking about.
SELECT l.id,
       l.user_id,
       u.email,
       u.name,
       u.status,
       u.locked_until,
       u.mfa_enabled_at
  FROM addon_identity_links l
  JOIN users u ON u.id = l.user_id
 WHERE l.addon = @addon
   AND l.issuer = @issuer
   AND l.subject = @subject
   AND u.deleted_at IS NULL;

-- name: TouchAddonIdentityLink :exec
-- Record that this link minted a session. Best-effort at the call site: a session
-- that exists and a timestamp that did not move is a worse outcome than the
-- reverse, so the caller logs a failure here rather than failing the sign-in.
UPDATE addon_identity_links SET last_used_at = now() WHERE id = @id;

-- name: CreateAddonIdentityLink :one
-- Connect a provider to the account of the person who is signed in.
--
-- **The only writer, and it takes a user id the caller resolved from a session.**
-- That is the deliberate half of the linking flow: nothing an add-on asserts
-- reaches this statement, so an add-on cannot create the mapping it will later be
-- believed on. `ON CONFLICT DO NOTHING` on the unique key makes a second attempt
-- at the same connection idempotent rather than an error page; a conflict with a
-- *different* account returns no row, and the caller reports that the subject is
-- already connected somewhere else rather than moving it.
INSERT INTO addon_identity_links (id, user_id, addon, issuer, subject)
VALUES (@id, @user_id, @addon, @issuer, @subject)
ON CONFLICT (addon, issuer, subject) DO NOTHING
RETURNING id, user_id, addon, issuer, subject, created_at, last_used_at;

-- name: CountAddonIdentityLinks :one
-- How many account mappings were written under one add-on's name.
--
-- M68's, and it exists for the confirmation rather than for a management surface.
-- A purge is `DROP SCHEMA … CASCADE` and deletes no row here, so the links stay
-- and are inherited by name — the whole of F330's shape — and the confirmation is
-- the point of decision where an operator can still act on that. Naming them
-- without a number would be a warning nobody could size; this is the number.
--
-- Keyed on the add-on's name because the table is: `addon` is the manifest name,
-- not a foreign key to anything, which is exactly why the inheritance exists.
SELECT count(*) FROM addon_identity_links WHERE addon = @addon;

-- Reading and *removing* a link still have no statement here, deliberately. M65
-- builds the table, the flow that writes it and the refusals that read it; a
-- management surface is nobody's yet, and a query kept alive by nothing but
-- sqlc's generator is a query nobody has ever run against this schema.
