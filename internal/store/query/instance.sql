-- Instance-level grants: what a person may do to the instance rather than to a
-- tenant (D98). Every statement here joins `permissions` on the slug instead of
-- taking a permission id, so the slugs stay the vocabulary the Go code speaks
-- and no caller has to carry a uuid literal around.

-- name: ListInstanceGrants :many
-- Every instance-level permission one person holds.
--
-- Read on every identity resolution, beside GetUserPermissions, which is why it
-- is keyed on the user alone and returns slugs: the caller folds it into the
-- same set and Identity.Can cannot tell the two sources apart. It deliberately
-- does not join workspaces or memberships — an instance grant is not reached
-- through a tenancy, and an account that belongs to no organization at all (D36)
-- keeps whatever it holds here.
SELECT p.slug
  FROM instance_grants ig
  JOIN permissions p ON p.id = ig.permission_id
 WHERE ig.user_id = @user_id
 ORDER BY p.slug;

-- name: ListInstanceGrantHolders :many
-- Who holds one instance-level permission, with enough of the account to name
-- them on the page that confers it.
--
-- Soft-deleted accounts are filtered rather than shown as inert: a grant to an
-- account that cannot authenticate is not reach, and listing it invites somebody
-- to revoke a row that was already doing nothing.
SELECT u.id, u.email, u.name, ig.granted_at, ig.granted_by
  FROM instance_grants ig
  JOIN permissions p ON p.id = ig.permission_id
  JOIN users u       ON u.id = ig.user_id
 WHERE p.slug = @permission
   AND u.deleted_at IS NULL
 ORDER BY u.email;

-- name: GrantInstancePermission :execrows
-- Confer one instance-level permission on one account.
--
-- Idempotent. Re-conferring what somebody already holds is the ordinary result
-- of two administrators doing the same obvious thing, and it must not turn into
-- an error that reads like a refusal; the original granted_by and granted_at
-- stand, because the first grant is the one that happened.
--
-- It returns the row count for a reason that has nothing to do with idempotence:
-- the SELECT finds no row for a slug that does not exist, so a typo would confer
-- nothing and report success. On the setup path that means an instance that has
-- been claimed and has nobody who can administer it, which is the one outcome
-- this whole table exists to prevent. The caller distinguishes 0 from 1 by
-- reading the count against a permission it already knows the account did not
-- hold; see internal/auth's grantInstancePrincipal.
INSERT INTO instance_grants (user_id, permission_id, granted_by)
SELECT @user_id, p.id, sqlc.narg(granted_by)::uuid
  FROM permissions p
 WHERE p.slug = @permission
ON CONFLICT DO NOTHING;

-- name: RevokeInstancePermission :execrows
-- Withdraw one instance-level permission from one account.
--
-- Returns the row count so the caller can tell "withdrawn" from "they never held
-- it" without a read first. Which permissions may travel this path at all is
-- decided in Go, not here: instance.admin is deliberately not one of them.
DELETE FROM instance_grants ig
 USING permissions p
 WHERE p.id = ig.permission_id
   AND ig.user_id = @user_id
   AND p.slug = @permission;
