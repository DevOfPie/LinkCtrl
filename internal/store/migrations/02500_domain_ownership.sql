-- +goose Up
--
-- A domain gets an owning workspace (M39).
--
-- `domains.organization_id` has been nullable since 00300, with a comment saying
-- custom domains "belong to an organization". Plan.md's security scope row
-- promises something finer — *a workspace administers its own hostname* — and
-- those are different grains. This migration adds the finer one beside the
-- coarser one rather than reinterpreting it. Decision D68, taken before any code
-- because Phase 3 inherits the shape.
--
-- **Exactly one owning workspace per domain, and that is the whole argument.**
-- Alias uniqueness is per domain: `links_domain_alias_key` makes a hostname one
-- alias namespace. A `domain_workspaces` join table would be the shape Phase 3
-- finds easiest to extend and would let two workspaces contend for the same
-- alias on the same hostname with no rule to settle it — which is the
-- alias-hijack surface M39 was split out of M40 to keep reviewable in isolation.
-- A column cannot express that state, which is why it is the right one.
--
-- Nothing is served on a hostname registered here. M40 adds verification and
-- serving; until then `verified_at` stays NULL, no router looks a hostname up,
-- and an unknown Host still gets the operational 404.

ALTER TABLE domains
    -- NULL for the instance default and for an organization-owned domain.
    -- CASCADE rather than SET NULL: a deleted workspace's hostname has no owner
    -- left, and leaving the row behind with both owner columns NULL would
    -- silently promote it to the instance default state.
    ADD COLUMN workspace_id uuid REFERENCES workspaces(id) ON DELETE CASCADE;

-- The three legal states, enumerated rather than reduced.
--
-- `workspace_id IS NULL OR organization_id IS NOT NULL` says the same thing in
-- one line and says nothing about what the three states *are*. D68's stated cost
-- is that a reader has to consult this constraint to know which pairs are legal,
-- so the constraint is written to answer that question directly.
--
-- The fourth combination — a workspace with no organization — is the one that
-- must not exist: a workspace always belongs to an organization, so a domain
-- naming one implies the other. There is no composite foreign key holding the
-- pair *consistent* (that would need a unique index on `workspaces (id,
-- organization_id)` for a guarantee only one writer can breach); instead
-- link.Service writes both from the same resolved identity, which is where the
-- pair comes from in the first place.
ALTER TABLE domains
    ADD CONSTRAINT domains_ownership_states CHECK (
        -- The instance default, which every workspace shares.
        (organization_id IS NULL     AND workspace_id IS NULL)
        -- Organization-owned: every workspace in the organization.
     OR (organization_id IS NOT NULL AND workspace_id IS NULL)
        -- Workspace-owned: one workspace, and it implies its organization.
     OR (organization_id IS NOT NULL AND workspace_id IS NOT NULL)
    );

-- The domains page lists one workspace's own hostnames, which nothing indexed.
-- Partial on deleted_at like every other list in this schema, so a soft-deleted
-- hostname costs the scan nothing.
CREATE INDEX domains_workspace_idx ON domains (workspace_id) WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS domains_workspace_idx;
ALTER TABLE domains DROP CONSTRAINT IF EXISTS domains_ownership_states;
ALTER TABLE domains DROP COLUMN IF EXISTS workspace_id;
