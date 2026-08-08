-- +goose Up
--
-- An API key belongs to an account, not to one organization (M54).
--
-- `organization_id` has been NOT NULL since 00500, and every rule built on top
-- of it read "the organization this key was issued into" as a fact about the
-- credential. The owner's answer to F75 is that it is a fact about the *mint*,
-- not about the key: a key is minted by an account and reaches the
-- organizations that account belongs to, the way a personal access token does.
--
-- So the column becomes nullable and NULL acquires a meaning:
--
--   NULL      — account-wide. Every organization the owner holds an
--               organization-wide membership in, resolved per request.
--   non-NULL  — pinned. Exactly this organization, for the life of the key.
--
-- **No issued key changes reach on the day of this migration.** Dropping NOT
-- NULL writes no rows, so every existing key keeps the organization it was
-- minted into and stays pinned to it. Account-wide is a thing a key is created
-- as, or rotated into, and never a thing one becomes.
--
-- Pinning stays available on purpose. Removing it would force a key issued for
-- one tenant to widen the moment its owner joins a second, and there is no way
-- to narrow it back afterwards.

ALTER TABLE api_keys
    ALTER COLUMN organization_id DROP NOT NULL;

-- A workspace belongs to exactly one organization, so a workspace-scoped key is
-- pinned by construction. Stated as a constraint rather than left to the
-- service, because the alternative is a row whose two columns disagree about
-- which tenant the key is in and whose behaviour then depends on which one the
-- reader happens to consult.
ALTER TABLE api_keys
    ADD CONSTRAINT api_keys_workspace_needs_organization
        CHECK (workspace_id IS NULL OR organization_id IS NOT NULL);

-- Rebuilt for a nullable column. The index serves the administrator's revoke,
-- which is keyed on the organization, and an account-wide key has none to be
-- found by — so the NULLs are excluded rather than stored. On an instance where
-- account-wide becomes the common shape this is the difference between an index
-- of the rows it can answer for and an index mostly of rows it cannot.
DROP INDEX IF EXISTS api_keys_org_idx;
CREATE INDEX api_keys_org_idx ON api_keys (organization_id)
    WHERE revoked_at IS NULL AND organization_id IS NOT NULL;

-- An administrator's reach over somebody else's account-wide key.
--
-- `revokeInOrganization` exists because a key belonging to somebody who will
-- not stop it still has to be stoppable by whoever holds the incident. For a
-- pinned key that is a full revoke, and it stays one: the organization *is* the
-- key's whole reach, so cutting the reach and cutting the key are the same act.
--
-- An account-wide key is the case that has no answer yet. Revoking it outright
-- would let an administrator in one tenant destroy a credential its owner uses
-- in another, which is authority over an account they have none over. Leaving
-- it alone would mean an account-wide key is the one credential an incident
-- cannot stop. So the reach is what is revocable, one organization at a time,
-- and this table is that row.
--
-- Rows here are the *only* thing that removes an organization from an
-- account-wide key's reach. A pinned key never has one — nothing writes it for
-- a key with an organization of its own — and the resolution path skips the
-- lookup for one entirely.
CREATE TABLE api_key_org_revocations (
    api_key_id      uuid        NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    organization_id uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    revoked_at      timestamptz NOT NULL DEFAULT now(),
    -- Who did it, for the audit question this table's whole existence is about.
    -- SET NULL rather than CASCADE: the bar outlives the administrator's
    -- account, because it is a statement about the key rather than about them.
    revoked_by      uuid        REFERENCES users(id) ON DELETE SET NULL,
    PRIMARY KEY (api_key_id, organization_id)
);

-- +goose Down
DROP TABLE IF EXISTS api_key_org_revocations;
DROP INDEX IF EXISTS api_keys_org_idx;
CREATE INDEX api_keys_org_idx ON api_keys (organization_id) WHERE revoked_at IS NULL;
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_workspace_needs_organization;
-- Refuses while any account-wide key exists, which is the honest failure: this
-- direction has to decide which organization such a key retroactively belonged
-- to, and nothing here can.
ALTER TABLE api_keys
    ALTER COLUMN organization_id SET NOT NULL;
