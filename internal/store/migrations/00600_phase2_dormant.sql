-- +goose Up
--
-- PHASE 2 / PHASE 3: dormant tables.
--
-- These complete the 20-entity model from Plan.md. They are created now so
-- later phases are additive migrations rather than rewrites, but nothing reads
-- or writes them in Phase 1.
--
-- Rule for this file: anything structural is jsonb. The real shape of a
-- webhook payload filter or an automation trigger will not survive contact
-- with the feature that eventually uses it, and a jsonb blob evolves with an
-- UPDATE rather than an ALTER TABLE. If you find yourself adding a fifth typed
-- column to a table in this file, put it in the jsonb instead.
--
-- docs/data-model.md carries the entity -> phase -> behavior-implemented
-- table, so nobody has to read this comment to find out what is live.

CREATE TABLE routing_rules (
    id           uuid        PRIMARY KEY,
    link_id      uuid        NOT NULL REFERENCES links(id) ON DELETE CASCADE,
    workspace_id uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    destination_id uuid      REFERENCES destinations(id) ON DELETE CASCADE,
    -- Lower wins. First match short-circuits.
    priority     int         NOT NULL DEFAULT 100,
    -- { "country": ["GB","IE"], "device": ["mobile"], "time": {...} }
    conditions   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    kind         text        NOT NULL DEFAULT 'match'
                 CHECK (kind IN ('match', 'weighted', 'sequential', 'fallback')),
    enabled      boolean     NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX routing_rules_link_idx ON routing_rules (link_id, priority) WHERE enabled;

CREATE TABLE campaigns (
    id           uuid        PRIMARY KEY,
    workspace_id uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         text        NOT NULL,
    slug         text        NOT NULL,
    description  text        NOT NULL DEFAULT '',
    -- utm defaults, goals, scheduling
    settings     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    starts_at    timestamptz,
    ends_at      timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz
);
CREATE UNIQUE INDEX campaigns_workspace_slug_key
    ON campaigns (workspace_id, lower(slug)) WHERE deleted_at IS NULL;

ALTER TABLE links ADD COLUMN campaign_id uuid REFERENCES campaigns(id) ON DELETE SET NULL;
CREATE INDEX links_campaign_idx ON links (campaign_id) WHERE campaign_id IS NOT NULL;

CREATE TABLE qr_codes (
    id           uuid        PRIMARY KEY,
    link_id      uuid        NOT NULL REFERENCES links(id) ON DELETE CASCADE,
    workspace_id uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- What this blob actually holds, as of M50.6 (0.3.0): `qr.Style` has five
    -- fields — foreground, background, error-correction level, margin, scale.
    --
    -- This line read "colours, logo reference, error-correction level, margin,
    -- shape" from the first migration until M58. Two of those five were never
    -- built into the blob and one still is not. **The logo is a `bytea` column**
    -- (`logo`, 03800_qr_code_logo.sql, D134), not a reference in here — bytes
    -- were chosen over a reference precisely so the row and the picture cannot
    -- disagree. **Shape is not built and nothing schedules it**: the renderer
    -- draws square modules only, and area F closed with 0.3.0.
    style        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- Nothing increments this in Phase 1. See docs/data-model.md before
    -- writing a feature against it.
    scan_count   bigint      NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX qr_codes_link_idx ON qr_codes (link_id);

CREATE TABLE webhooks (
    id              uuid        PRIMARY KEY,
    workspace_id    uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    url             text        NOT NULL,
    secret          bytea,
    -- Event names and any filtering, as jsonb rather than a typed array: the
    -- filter shape is the part most likely to change.
    subscription    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    enabled         boolean     NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhooks_workspace_idx ON webhooks (workspace_id) WHERE enabled;

CREATE TABLE webhook_deliveries (
    id           uuid        PRIMARY KEY,
    webhook_id   uuid        NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    event        text        NOT NULL,
    payload      jsonb       NOT NULL,
    status       text        NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'delivered', 'failed', 'abandoned')),
    attempts     int         NOT NULL DEFAULT 0,
    response_code int,
    next_attempt_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);
CREATE INDEX webhook_deliveries_pending_idx
    ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';

CREATE TABLE automation_rules (
    id           uuid        PRIMARY KEY,
    workspace_id uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         text        NOT NULL,
    trigger      text        NOT NULL,
    -- Thresholds, schedules, filters.
    trigger_config jsonb     NOT NULL DEFAULT '{}'::jsonb,
    -- Ordered list of actions with their parameters.
    actions      jsonb       NOT NULL DEFAULT '[]'::jsonb,
    enabled      boolean     NOT NULL DEFAULT true,
    last_fired_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX automation_rules_workspace_idx
    ON automation_rules (workspace_id, trigger) WHERE enabled;

CREATE TABLE notifications (
    id           uuid        PRIMARY KEY,
    user_id      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id uuid        REFERENCES workspaces(id) ON DELETE CASCADE,
    kind         text        NOT NULL,
    title        text        NOT NULL,
    body         text        NOT NULL DEFAULT '',
    data         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    read_at      timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notifications_user_unread_idx
    ON notifications (user_id, created_at DESC) WHERE read_at IS NULL;

-- Audit log. The table ships in Phase 1 (Plan.md lists it under Phase 1
-- Security) but nothing writes to it until Phase 2.
--
-- Partitioned like the other high-volume append-only tables, and for the same
-- reason: retention by DROP TABLE is O(1) where DELETE is not. No PARTITION OF
-- here, per the rule at the top of 00400.
--
-- actor_user_id has no foreign key on purpose: an audit record must survive
-- the deletion of the user it refers to, and actor_label is a snapshot so the
-- record stays readable. Erasure overwrites the label rather than deleting the
-- row, since audit records of actions are retained under legitimate interest —
-- and since M52 that is what happens rather than what was designed for. The
-- sweep sets actor_label to a constant tombstone and leaves actor_user_id alone
-- (D148), so one erased actor's entries stay correlated by the id
-- audit_logs_actor_idx already keys on and nothing is derived from anything.
--
-- Written in the present tense here from the first migration until 0.2.0, when
-- no such routine existed at all (F44) — one of the five sites that made an
-- absent feature read as a shipped one. The tense is the same now and the
-- sentence is finally true; the history stays because a reader meeting it in an
-- old checkout needs to know which of the two applied.
CREATE TABLE audit_logs (
    id            uuid        NOT NULL,
    occurred_at   timestamptz NOT NULL,
    organization_id uuid,
    workspace_id  uuid,
    actor_user_id uuid,
    actor_label   text        NOT NULL DEFAULT '',
    actor_api_key_id uuid,
    action        text        NOT NULL,
    target_type   text,
    target_id     uuid,
    metadata      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    ip_prefix     text,
    PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

CREATE INDEX audit_logs_org_time_idx ON audit_logs (organization_id, occurred_at DESC);
CREATE INDEX audit_logs_actor_idx    ON audit_logs (actor_user_id, occurred_at DESC);

-- +goose Down
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS automation_rules;
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhooks;
DROP TABLE IF EXISTS qr_codes;
DROP INDEX IF EXISTS links_campaign_idx;
ALTER TABLE links DROP COLUMN IF EXISTS campaign_id;
DROP TABLE IF EXISTS campaigns;
DROP TABLE IF EXISTS routing_rules;
