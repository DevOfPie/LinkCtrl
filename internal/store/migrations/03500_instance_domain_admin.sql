-- +goose Up
--
-- The instance default domain becomes the instance principal's (D100, F70).
--
-- `domains.write` is a *role* permission, granted to owner and admin by 00800.
-- That is right for a workspace's own registered hostname and wrong for the
-- instance default, because the default is not any tenant's: it is the hostname
-- every workspace's links are served on until it registers one of its own, and
-- `canAdminister`'s third limb answered `true` for it on the bare permission.
--
-- So on a multi-organization instance every organization's owner and admin could
-- repoint the default domain's root redirect and change its bot policy. On an
-- instance running LINKCTRL_SIGNUP_MODE=open that is one registration away: a
-- registrant is provisioned as the owner of their own organization, and owner
-- holds `domains.write`.
--
-- D38 refused to fix this because the product had no instance-level principal to
-- give it to. D98 built one, so the refusal's stated reason has stopped being
-- true, and 03400's argument applies again unchanged.
--
-- Delegability, which the inherited Permissions rule requires be recorded:
-- **neither limb of D18**. Repointing the instance root escalates nothing — it
-- grants no permission and widens no key's reach — it is reversible, and it
-- discloses nothing. The destination goes through the same validation a link's
-- does, which is what keeps a root redirect from being the cleanest SSRF in the
-- product. So it is delegable, and it is deliberately NOT added to
-- auth.NonDelegableScopes.
--
-- It is also **not** added to auth.InstanceGrantable. The principal holds it;
-- the principal may not confer it. Conferring instance-level *review* is what
-- D98 decided the principal may delegate, and widening that to "and also who
-- administers the instance's own hostname" is a separate decision nobody has
-- made. This file is where somebody would have to make it.
INSERT INTO permissions (id, slug, description) VALUES
    ('00000000-0000-4000-8000-00000000021a', 'domains.write.instance',
     'Administer the instance default domain: its root redirect and its bot policy');

-- +goose StatementBegin
DO $$
DECLARE
    perm_instance_domains uuid := '00000000-0000-4000-8000-00000000021a';
    principal             uuid;
BEGIN
    -- Granted to no role, for 03400's reason: a role grant is what F70 is.
    --
    -- It reaches a person only through instance_grants. `domains.write` itself
    -- is deliberately left alone — a workspace admin goes on administering their
    -- own registered hostnames, which is M39's whole point and is not what this
    -- finding is about.

    -- Whoever already holds the principal. Not "the earliest surviving account"
    -- as 03400 computed it: by the time this runs 03400 has already chosen, and
    -- `lctl instance principal move` may have moved it since. Re-deriving would
    -- silently undo an operator's move, which is the one thing that command
    -- exists to make possible.
    SELECT user_id INTO principal
      FROM instance_grants
     WHERE permission_id = '00000000-0000-4000-8000-000000000217'
     ORDER BY granted_at, user_id
     LIMIT 1;

    -- No principal is the fresh-database case, exactly as in 03400: the setup
    -- flow confers every scope in auth.InstancePrincipalScopes in the same
    -- transaction that creates the first account, and this permission is in that
    -- list, so a fresh instance gets it without this branch.
    IF principal IS NOT NULL THEN
        INSERT INTO instance_grants (user_id, permission_id, granted_by)
        VALUES (principal, perm_instance_domains, NULL)
        ON CONFLICT DO NOTHING;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
--
-- Dropping the permission is enough to restore the previous behaviour: the guard
-- reads this slug, so removing it makes `canAdminister`'s instance-default limb
-- fall back to `domains.write` alone, which is where it was. The grant rows go
-- with it by cascade.
DELETE FROM permissions WHERE id = '00000000-0000-4000-8000-00000000021a';
