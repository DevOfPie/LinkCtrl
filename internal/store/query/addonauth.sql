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

-- name: ListAddonIdentityLinksForUser :many
-- Every provider one account has connected, newest first.
--
-- **M70's, and it is what F315 was waiting for.** M65 wrote this table, the flow
-- that fills it and the refusals that read it, and deliberately shipped no way to
-- see or sever a row — so somebody who connected a provider was connected to it
-- for the life of the account, and deleting the whole account was the only thing
-- that reliably removed one. A link admits somebody with no password and no
-- second factor of this product's, which is why account deletion already takes
-- these rows; the missing half was undoing one on purpose.
--
-- No subject column. The subject is the provider's identifier for a person and
-- nothing on either surface needs it: what a reader chooses between is *which
-- add-on, which issuer, and when it was last used*, and putting an opaque
-- external id on a page invites somebody to treat it as one of ours.
SELECT id, addon, issuer, created_at, last_used_at
  FROM addon_identity_links
 WHERE user_id = @user_id
 ORDER BY created_at DESC, id;

-- name: ListAddonIdentityLinksForAddon :many
-- Every account one add-on has connected, newest first, with the person named.
--
-- The operator's half of the same question, and it carries the email because the
-- operator is deciding about *accounts* — an add-on's row means nothing to them
-- without knowing whose it is. The person's own list above deliberately carries
-- no such column: it is already their account.
SELECT l.id, l.issuer, l.created_at, l.last_used_at, l.user_id, u.email, u.name
  FROM addon_identity_links l
  JOIN users u ON u.id = l.user_id
 WHERE l.addon = @addon
   AND u.deleted_at IS NULL
 ORDER BY l.created_at DESC, l.id;

-- name: DeleteAddonIdentityLink :one
-- Sever one link, returning what was severed so the caller can record it.
--
-- **The user id is in the predicate and is not optional**, which is what makes
-- one statement serve both surfaces without a second one that could disagree
-- about ownership: a person passes their own, and the operator's path resolves
-- the row's owner first and passes that. An id alone would let a mistyped
-- identifier remove somebody else's credential.
--
-- Returning rather than :exec, because what is deleted is what the audit record
-- has to name and reading it back afterwards is impossible.
DELETE FROM addon_identity_links
 WHERE id = @id AND user_id = @user_id
RETURNING id, user_id, addon, issuer, created_at, last_used_at;
