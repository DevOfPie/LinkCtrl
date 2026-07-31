-- +goose Up

CREATE TABLE api_keys (
    id              uuid        PRIMARY KEY,
    user_id         uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    organization_id uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    -- NULL means the key is valid across the organization's workspaces.
    workspace_id    uuid        REFERENCES workspaces(id) ON DELETE CASCADE,

    name            text        NOT NULL,

    -- The public, non-secret part: "lk_live_<prefix>". Indexed so verification
    -- is a single-row lookup instead of scanning and comparing every key.
    prefix          text        NOT NULL,
    -- HMAC-SHA256(pepper, secret). Deliberately not argon2: the key is
    -- full-entropy random, so key-stretching buys nothing, and 64 MiB of
    -- argon2 per API request would be untenable on a path with a 150ms budget.
    -- The pepper lives in configuration, not the database, so a database dump
    -- alone does not permit offline verification.
    key_hash        bytea       NOT NULL,

    -- Subset of permissions.slug. Sharing the vocabulary with RBAC keeps the
    -- two authorization systems from drifting apart.
    scopes          text[]      NOT NULL DEFAULT '{}',

    -- Written through the async pipeline so authentication costs no
    -- synchronous write on the request path.
    last_used_at    timestamptz,
    expires_at      timestamptz,
    revoked_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX api_keys_prefix_key ON api_keys (prefix);
CREATE INDEX api_keys_user_idx ON api_keys (user_id) WHERE revoked_at IS NULL;
CREATE INDEX api_keys_org_idx  ON api_keys (organization_id) WHERE revoked_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS api_keys;
