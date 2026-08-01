-- +goose Up
--
-- `orgs.create`: the permission that lets an account provision an organization
-- of its own (M28, decision D16).
--
-- D16 says it is granted **by default to self-registered users only**, and
-- m28.md's Risks section says that must be a grant rather than a runtime check
-- against how the account was made — the latter would be a second authorization
-- axis running alongside RBAC, which is exactly what D16 chose to avoid.
--
-- The grant below is that expression, and the mechanism is worth stating
-- because it is not obvious from the row alone. Registration provisions a
-- personal organization with an **owner** membership in the same transaction as
-- the user (auth.Register); redemption of an invitation provisions a membership
-- and nothing else, at whatever role the invitation named (D6). So granting
-- this to the owner role and to nothing else means:
--
--   * the account from the setup form holds it, because registering made it an
--     owner;
--   * every account that arrived by invitation does not, because an invitation
--     defaults to viewer and is capped at the inviter's rank;
--   * an owner who deliberately hands somebody the owner role has granted it,
--     which is the "and nobody else until they grant it" half of D16.
--
-- Admin is deliberately excluded, unlike audit.read in 00900. Admins arrive by
-- invitation on a default instance, and D16's sentence is about who holds this
-- without anybody deciding they should.
--
-- Delegable to an API key (D18). Neither limb matches: it discloses nothing
-- about anyone's identity or network, and it cannot widen a key's own reach —
-- a key's permissions are its scopes intersected with its owner's role on every
-- request, so an organization created through a key leaves that key holding
-- exactly the scopes it was minted with. `NonDelegableScopes` therefore does not
-- list it. See decisions.md.

INSERT INTO permissions (id, slug, description) VALUES
    ('00000000-0000-4000-8000-000000000211', 'orgs.create',
     'Create an organization, with its first workspace and an owner membership');

-- Granted explicitly, for the reason 00800 and 00900 spell their grants out:
-- the seed migration's "owner gets everything" ran once, at its own version,
-- against the permissions that existed then. A permission added later is held by
-- nobody unless it says so here.
-- +goose StatementBegin
DO $$
DECLARE
    perm_id  uuid := '00000000-0000-4000-8000-000000000211';
    owner_id uuid := '00000000-0000-4000-8000-000000000101';
BEGIN
    INSERT INTO role_permissions (role_id, permission_id)
    VALUES (owner_id, perm_id)
    ON CONFLICT DO NOTHING;
END
$$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM role_permissions WHERE permission_id = '00000000-0000-4000-8000-000000000211';
DELETE FROM permissions WHERE slug = 'orgs.create';
