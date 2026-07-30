-- +goose Up
--
-- Identity, tenancy and RBAC.
--
-- Tenancy chain: organization -> workspace -> everything else. Phase 1 gives
-- each user one auto-provisioned personal organization and workspace, but
-- every tenant-scoped table carries workspace_id from the start. These columns
-- are the ones that must be right now; getting them wrong is the migration
-- that cannot be done additively later.

CREATE TABLE users (
    id                uuid        PRIMARY KEY,
    email             text        NOT NULL,
    -- Comparison is always on lower(email); the generated column gives the
    -- unique index something stable to index rather than relying on every
    -- query remembering to fold case.
    email_lower       text        GENERATED ALWAYS AS (lower(email)) STORED,
    email_verified_at timestamptz,
    name              text        NOT NULL DEFAULT '',
    -- Nullable: an SSO-only user (Phase 3) has no local password, and an
    -- erased user has had theirs removed.
    password_hash     text,
    status            text        NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active', 'suspended', 'deleted')),

    -- Lockout state. Counted per account; the per-IP limit is separate and
    -- lives in Redis.
    failed_login_count int        NOT NULL DEFAULT 0,
    locked_until       timestamptz,

    -- Phase 3.
    mfa_secret        text,
    mfa_enabled_at    timestamptz,

    -- Set by the GDPR erasure routine. Distinct from deleted_at: erasure
    -- scrubs identifying fields while keeping the row so foreign keys and
    -- audit records stay intact.
    anonymized_at     timestamptz,

    last_login_at     timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz
);

CREATE UNIQUE INDEX users_email_key ON users (email_lower) WHERE deleted_at IS NULL;

CREATE TABLE organizations (
    id          uuid        PRIMARY KEY,
    name        text        NOT NULL,
    slug        text        NOT NULL,
    -- Regional storage means one instance per region, selected by this value.
    -- Row-level regional routing is explicitly not attempted.
    data_region text        NOT NULL DEFAULT 'default',
    -- True for the organization auto-created at registration. Distinguishes
    -- "your own space" from a real shared organization in Phase 2.
    is_personal boolean     NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz
);

CREATE UNIQUE INDEX organizations_slug_key ON organizations (lower(slug)) WHERE deleted_at IS NULL;

CREATE TABLE workspaces (
    id              uuid        PRIMARY KEY,
    organization_id uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            text        NOT NULL,
    slug            text        NOT NULL,

    -- Retention shorter than the instance floor is enforced by a batched
    -- delete job. Longer is ignored: partition drop is instance-wide, so a
    -- workspace cannot keep data past the instance retention.
    analytics_retention_days int,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz
);

CREATE UNIQUE INDEX workspaces_org_slug_key
    ON workspaces (organization_id, lower(slug)) WHERE deleted_at IS NULL;
CREATE INDEX workspaces_org_idx ON workspaces (organization_id);

-- RBAC. Unlike most of the dormant Phase 2 schema, these tables are live in
-- Phase 1: a real evaluator reads them. Retrofitting authorization after
-- features exist is where permission bugs come from.

CREATE TABLE roles (
    id          uuid        PRIMARY KEY,
    -- NULL for the four built-in roles; set for future per-organization
    -- custom roles.
    organization_id uuid    REFERENCES organizations(id) ON DELETE CASCADE,
    slug        text        NOT NULL,
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    is_builtin  boolean     NOT NULL DEFAULT false,
    -- Lower binds tighter. Used to resolve the effective role when a user
    -- holds more than one, which cannot happen in Phase 1 but will in Phase 2.
    rank        int         NOT NULL DEFAULT 100,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Built-in role slugs are globally unique; custom ones are unique per org.
CREATE UNIQUE INDEX roles_builtin_slug_key
    ON roles (slug) WHERE organization_id IS NULL;
CREATE UNIQUE INDEX roles_org_slug_key
    ON roles (organization_id, slug) WHERE organization_id IS NOT NULL;

CREATE TABLE permissions (
    id          uuid        PRIMARY KEY,
    -- Dotted "resource.action", e.g. links.create. Also the vocabulary used
    -- for API key scopes, so the two authorization systems cannot drift.
    slug        text        NOT NULL UNIQUE,
    description text        NOT NULL DEFAULT ''
);

CREATE TABLE role_permissions (
    role_id       uuid NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    permission_id uuid NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,
    PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE memberships (
    id              uuid        PRIMARY KEY,
    user_id         uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    organization_id uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    role_id         uuid        NOT NULL REFERENCES roles(id),
    -- NULL means the membership covers every workspace in the organization,
    -- which is what Phase 1 always creates. Phase 2 adds workspace-scoped
    -- membership without a schema change.
    workspace_id    uuid        REFERENCES workspaces(id) ON DELETE CASCADE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX memberships_user_org_workspace_key
    ON memberships (user_id, organization_id, COALESCE(workspace_id, '00000000-0000-0000-0000-000000000000'::uuid));
CREATE INDEX memberships_user_idx ON memberships (user_id);
CREATE INDEX memberships_org_idx  ON memberships (organization_id);

-- Sessions live in Postgres rather than Redis on purpose: Redis here is a
-- cache with no persistence and LRU eviction, so sessions kept there would be
-- silently evicted under memory pressure and log everyone out.
CREATE TABLE sessions (
    id              uuid        PRIMARY KEY,
    user_id         uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- SHA-256 of the cookie value. The raw token is never stored, so a
    -- database leak does not hand over live sessions.
    token_hash      bytea       NOT NULL,
    -- Anonymized before storage, same as analytics.
    ip_prefix       text,
    user_agent      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    -- Absolute deadline. Idle expiry is derived from last_seen_at at read
    -- time, so extending the idle window does not require rewriting rows.
    expires_at      timestamptz NOT NULL,
    revoked_at      timestamptz
);

CREATE UNIQUE INDEX sessions_token_key ON sessions (token_hash);
CREATE INDEX sessions_user_idx        ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_expiry_idx      ON sessions (expires_at) WHERE revoked_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS role_permissions;
DROP TABLE IF EXISTS permissions;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS workspaces;
DROP TABLE IF EXISTS organizations;
DROP TABLE IF EXISTS users;
