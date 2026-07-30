-- +goose Up
--
-- Baseline rows every instance needs.
--
-- Fixed UUIDs rather than generated ones: these are referenced by the
-- application and by tests, and a stable identity makes both simpler. They are
-- v4-shaped constants, not UUIDv7, because they are not time-ordered rows.
-- Blocks: ...d0xx domains, ...01xx roles, ...02xx permissions.

-- The default domain. Its hostname is a placeholder; the resolver matches on
-- is_default rather than on the hostname string, so this value never has to
-- agree with LINKCTRL_BASE_URL. Phase 2 adds real hostnames alongside it.
INSERT INTO domains (id, organization_id, hostname, is_default, verified_at, ssl_status)
VALUES ('00000000-0000-4000-8000-0000000000d1', NULL, 'default', true, now(), 'active');

-- Built-in roles. rank orders them when a user holds more than one, which
-- cannot happen in Phase 1 but will once invitations exist.
INSERT INTO roles (id, organization_id, slug, name, description, is_builtin, rank) VALUES
    ('00000000-0000-4000-8000-000000000101', NULL, 'owner',  'Owner',
     'Full control, including billing and deletion of the organization.', true, 10),
    ('00000000-0000-4000-8000-000000000102', NULL, 'admin',  'Admin',
     'Manage links, members and settings. Cannot delete the organization.', true, 20),
    ('00000000-0000-4000-8000-000000000103', NULL, 'editor', 'Editor',
     'Create and edit links. Cannot manage members or settings.', true, 30),
    ('00000000-0000-4000-8000-000000000104', NULL, 'viewer', 'Viewer',
     'Read-only access to links and analytics.', true, 40);

-- Permissions. The slug vocabulary is shared with API key scopes so the two
-- authorization systems cannot drift.
INSERT INTO permissions (id, slug, description) VALUES
    ('00000000-0000-4000-8000-000000000201', 'links.read',      'View links'),
    ('00000000-0000-4000-8000-000000000202', 'links.create',    'Create links'),
    ('00000000-0000-4000-8000-000000000203', 'links.update',    'Edit links'),
    ('00000000-0000-4000-8000-000000000204', 'links.delete',    'Delete and archive links'),
    ('00000000-0000-4000-8000-000000000205', 'tags.read',       'View tags'),
    ('00000000-0000-4000-8000-000000000206', 'tags.write',      'Create and edit tags'),
    ('00000000-0000-4000-8000-000000000207', 'analytics.read',  'View analytics'),
    ('00000000-0000-4000-8000-000000000208', 'apikeys.read',    'View API keys'),
    ('00000000-0000-4000-8000-000000000209', 'apikeys.write',   'Create and revoke API keys'),
    ('00000000-0000-4000-8000-00000000020a', 'members.read',    'View members'),
    ('00000000-0000-4000-8000-00000000020b', 'members.write',   'Invite and remove members'),
    ('00000000-0000-4000-8000-00000000020c', 'workspace.read',  'View workspace settings'),
    ('00000000-0000-4000-8000-00000000020d', 'workspace.write', 'Change workspace settings'),
    ('00000000-0000-4000-8000-00000000020e', 'org.delete',      'Delete the organization');

-- Role grants.
-- +goose StatementBegin
DO $$
DECLARE
    owner_id  uuid := '00000000-0000-4000-8000-000000000101';
    admin_id  uuid := '00000000-0000-4000-8000-000000000102';
    editor_id uuid := '00000000-0000-4000-8000-000000000103';
    viewer_id uuid := '00000000-0000-4000-8000-000000000104';
BEGIN
    -- Owner: everything.
    INSERT INTO role_permissions (role_id, permission_id)
    SELECT owner_id, id FROM permissions;

    -- Admin: everything except deleting the organization.
    INSERT INTO role_permissions (role_id, permission_id)
    SELECT admin_id, id FROM permissions WHERE slug <> 'org.delete';

    -- Editor: link and tag work, plus reading analytics. Deliberately no key
    -- management: an editor who can mint API keys can grant themselves scopes
    -- beyond their own role, which defeats the point of having roles.
    INSERT INTO role_permissions (role_id, permission_id)
    SELECT editor_id, id FROM permissions WHERE slug IN (
        'links.read', 'links.create', 'links.update', 'links.delete',
        'tags.read', 'tags.write', 'analytics.read', 'workspace.read'
    );

    -- Viewer: read only.
    INSERT INTO role_permissions (role_id, permission_id)
    SELECT viewer_id, id FROM permissions WHERE slug IN (
        'links.read', 'tags.read', 'analytics.read', 'workspace.read'
    );
END
$$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM role_permissions;
DELETE FROM permissions;
DELETE FROM roles WHERE is_builtin;
DELETE FROM domains WHERE id = '00000000-0000-4000-8000-0000000000d1';
