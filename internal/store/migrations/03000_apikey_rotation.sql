-- +goose Up
--
-- API keys rotate themselves, and choose how wide they reach (M44).
--
-- Two features, one migration, because both are columns on `api_keys` and
-- neither is a table. Rotation adds three; the scope choice adds none at all —
-- `workspace_id` has been nullable since 00500, with a comment saying NULL means
-- "valid across the organization's workspaces", and nothing has ever written
-- one. M44 is where that column starts being used rather than described.

ALTER TABLE api_keys
    -- When this key was rotated. Set on the **predecessor**, never on the
    -- successor: rotation is something that happens to the key being replaced.
    ADD COLUMN rotated_at       timestamptz,

    -- When the predecessor stops verifying. The grace window's far edge, and
    -- the authoritative one: authentication reads this column directly rather
    -- than waiting for the sweeper below to write `revoked_at`, so a key stops
    -- working at the instant it was told it would even if the housekeeping job
    -- is not running at all. The sweeper exists to make the *list* honest, not
    -- to make the refusal happen.
    ADD COLUMN grace_expires_at timestamptz,

    -- The successor this key was rotated into.
    --
    -- ON DELETE SET NULL rather than CASCADE: the reaper removes long-revoked
    -- keys, and a predecessor kept for the "which keys existed" question must
    -- not be deleted because the key that replaced it was.
    ADD COLUMN successor_id     uuid REFERENCES api_keys(id) ON DELETE SET NULL;

-- A key is not its own successor. Cheap, and it forecloses the one shape that
-- would make the chain below a loop.
ALTER TABLE api_keys
    ADD CONSTRAINT api_keys_successor_not_self CHECK (successor_id IS DISTINCT FROM id);

-- One predecessor per successor, enforced here rather than only in Go.
--
-- This is what makes a rotation chain a chain. The service refuses to rotate a
-- key that already has a successor, which bounds fan-out at the application; this
-- index bounds fan-*in* in the database, so the two together mean each key has at
-- most one predecessor and at most one successor. Without it, a race between two
-- rotations of the same key would be caught by the service's own check and by
-- nothing else — and "a leaked key can persist across rotations" (D9) is an
-- accepted trade only while the persistence is a line, not a tree.
CREATE UNIQUE INDEX api_keys_successor_key
    ON api_keys (successor_id) WHERE successor_id IS NOT NULL;

-- The sweep's index. Partial on the rows the sweep can act on, which on any
-- real instance is a handful: keys mid-grace, not yet revoked.
CREATE INDEX api_keys_grace_idx
    ON api_keys (grace_expires_at)
    WHERE grace_expires_at IS NOT NULL AND revoked_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS api_keys_grace_idx;
DROP INDEX IF EXISTS api_keys_successor_key;
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_successor_not_self;
ALTER TABLE api_keys
    DROP COLUMN IF EXISTS successor_id,
    DROP COLUMN IF EXISTS grace_expires_at,
    DROP COLUMN IF EXISTS rotated_at;
