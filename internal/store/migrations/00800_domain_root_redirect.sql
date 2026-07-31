-- +goose Up
--
-- A destination for the root of the link domain, and the permission that guards
-- it.
--
-- Only meaningful once the dashboard and short links are on separate hosts: on a
-- single host "/" is the dashboard, and the application refuses to set this
-- there rather than taking the dashboard away from whoever set it.

ALTER TABLE domains
    -- NULL means "answer 404", which is the behaviour before this column
    -- existed and the one that reveals nothing about the instance.
    ADD COLUMN root_redirect_url text;

-- The permission is new rather than reusing workspace.write, because this is not
-- a workspace setting: one hostname serves every workspace on the instance, so
-- changing it redirects every stray visitor to all of them at once.
INSERT INTO permissions (id, slug, description) VALUES
    ('00000000-0000-4000-8000-00000000020f', 'domains.write',
     'Change domain settings, including where the link domain root redirects');

-- Granted explicitly. The seed migration's "owner gets everything" ran once, at
-- its own version, against the permissions that existed then — a permission
-- added later is granted by nobody unless it says so here.
-- +goose StatementBegin
DO $$
DECLARE
    perm_id  uuid := '00000000-0000-4000-8000-00000000020f';
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
DELETE FROM role_permissions WHERE permission_id = '00000000-0000-4000-8000-00000000020f';
DELETE FROM permissions WHERE slug = 'domains.write';
ALTER TABLE domains DROP COLUMN root_redirect_url;
