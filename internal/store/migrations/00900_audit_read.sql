-- +goose Up
--
-- The permission that guards reading the audit log.
--
-- The table has shipped since 00600 with nothing writing to it. This migration
-- is the vocabulary half of giving it behavior: a permission distinct from
-- workspace.read because the audit log spans a workspace's whole history and
-- names who did what, which is a different question from "what are the current
-- settings".
--
-- It is not delegable to an API key. NonDelegableScopes in internal/auth
-- enforces that at mint time, and it is the only place that does, so the
-- decision can be reversed by deleting one map entry if the operational case
-- for machine export ever outweighs it. See decisions.md.

INSERT INTO permissions (id, slug, description) VALUES
    ('00000000-0000-4000-8000-000000000210', 'audit.read',
     'Read the audit log: who changed what, and when');

-- Granted explicitly, for the same reason 00800 spells its grants out: the seed
-- migration's "owner gets everything" ran once, at its own version, against the
-- permissions that existed then. A permission added later is held by nobody
-- unless it says so here.
--
-- Owner and admin only. An editor can change links, and an audit log the
-- subject can read is still an audit log — but the reason to have one is to let
-- whoever is accountable for the workspace review actions they did not take,
-- and that set is the two administrative roles.
-- +goose StatementBegin
DO $$
DECLARE
    perm_id  uuid := '00000000-0000-4000-8000-000000000210';
    owner_id uuid := '00000000-0000-4000-8000-000000000101';
    admin_id uuid := '00000000-0000-4000-8000-000000000102';
BEGIN
    INSERT INTO role_permissions (role_id, permission_id)
    VALUES (owner_id, perm_id), (admin_id, perm_id)
    ON CONFLICT DO NOTHING;
END
$$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM role_permissions WHERE permission_id = '00000000-0000-4000-8000-000000000210';
DELETE FROM permissions WHERE slug = 'audit.read';
