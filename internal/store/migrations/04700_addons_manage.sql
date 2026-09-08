-- +goose Up
--
-- Installing and removing add-ons becomes the instance principal's (M67).
--
-- 03400 built the instance-level principal and 03500 applied its argument to the
-- instance default domain. This is the same argument at its sharpest, and it is
-- the reason the permission exists *before* the surface that uses it: an add-on
-- is a WebAssembly module this server executes, installed once for the whole box,
-- and no organization owns one. There is no tenant-shaped reading of "may install
-- code into the process" to give a role.
--
-- A role grant would therefore be F15's shape with arbitrary code at the end of
-- it. Registration provisions every self-registered account the owner role of its
-- own organization (M27/M28), so on an instance running
-- LINKCTRL_SIGNUP_MODE=open, "an owner may install an add-on" means "anybody with
-- an email address may install an add-on". That is not a narrower version of the
-- finding; it is the finding with the consequence changed from reading a dispute
-- queue to running code.
--
-- Delegability, which the inherited Permissions rule requires be recorded:
-- **D18's second limb, in its widest form**, and it is in auth.NonDelegableScopes
-- accordingly. Holding it lets a credential widen its own reach past any scope it
-- was issued with — not by conferring another permission, which is what
-- instance.admin does, but by adding code whose own manifest declares
-- permissions this key never had. A key that can install an add-on holding
-- `session.mint` can decide who is signed in. The first limb does not apply:
-- installing discloses nothing about an actor and touches no network data.
--
-- It is also **not** in auth.InstanceGrantable. The principal holds it and the
-- principal may not confer it, for a reason neither 03400 nor 03500 had: a
-- delegatee would not need the principal again for anything, because the module
-- they install can carry whatever reach they wanted.
INSERT INTO permissions (id, slug, description) VALUES
    ('00000000-0000-4000-8000-00000000021b', 'addons.manage',
     'Install and remove add-ons on this instance');

-- +goose StatementBegin
DO $$
DECLARE
    perm_addons uuid := '00000000-0000-4000-8000-00000000021b';
    principal   uuid;
BEGIN
    -- Granted to no role, for 03400's reason and 03500's: a role grant is the
    -- finding rather than an implementation of it. It reaches a person only
    -- through instance_grants.

    -- Whoever already holds the principal, exactly as 03500 computes it and not
    -- as 03400 did. By the time this runs the principal has been chosen, and
    -- `lctl instance principal move` may have moved it since; re-deriving "the
    -- earliest surviving account" would silently undo an operator's move.
    SELECT user_id INTO principal
      FROM instance_grants
     WHERE permission_id = '00000000-0000-4000-8000-000000000217'
     ORDER BY granted_at, user_id
     LIMIT 1;

    -- No principal is the fresh-database case: the setup flow confers every scope
    -- in auth.InstancePrincipalScopes in the same transaction that creates the
    -- first account, and this permission is in that list, so a fresh instance
    -- gets it without this branch.
    IF principal IS NOT NULL THEN
        INSERT INTO instance_grants (user_id, permission_id, granted_by)
        VALUES (principal, perm_addons, NULL)
        ON CONFLICT DO NOTHING;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
--
-- Dropping the permission is enough: nothing reads the slug except the lifecycle
-- API's own guard, which then refuses every caller, and the grant rows go with it
-- by cascade. An instance rolled back past this keeps every add-on already on
-- disk — removing the permission removes the ability to change the set, not the
-- set.
DELETE FROM permissions WHERE id = '00000000-0000-4000-8000-00000000021b';
