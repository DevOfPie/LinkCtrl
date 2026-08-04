-- +goose Up
--
-- Analytics.
--
-- The privacy guarantee is structural, not procedural: click_events has no IP
-- column at all. Not a truncated IP, not a hashed IP stored on the row — no
-- column. Visitor identity is a salted hash whose salt is deleted on a
-- schedule, and deleting the salt is the de-identification step. The
-- consequence is that the largest table in the system holds no personal data
-- and is out of scope for subject-access and erasure requests entirely.
--
-- NOTE: no CREATE TABLE ... PARTITION OF appears in this file, by rule.
-- sqlc emits a duplicate junk model for every child partition it sees, so
-- partitions would add a dead struct to generated code every month. They are
-- created by a Go migration and by the partition_maintain job instead.
-- See docs/adr/0001-partitioning-and-sqlc.md.

-- Rotating salts for visitor hashing.
CREATE TABLE analytics_salts (
    -- The UTC day the salt is valid for.
    valid_on   date        PRIMARY KEY,
    salt       bytea       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- After this, the row is deleted. That deletion is what makes the day's
    -- visitor hashes irreversible: without the salt they cannot be linked back
    -- to an IP even given the original addresses.
    purge_at   timestamptz NOT NULL
);

CREATE INDEX analytics_salts_purge_idx ON analytics_salts (purge_at);

CREATE TABLE click_events (
    id            uuid        NOT NULL,
    link_id       uuid        NOT NULL,
    workspace_id  uuid        NOT NULL,
    occurred_at   timestamptz NOT NULL,

    -- HMAC(salt_of_the_day, ip || user_agent || workspace_id). The workspace is
    -- in the message, not in the key: analytics_salts is one row per day, shared
    -- by every workspace on the instance. That is still what stops the same
    -- person being correlated across two of them — the derived hashes differ, so
    -- one workspace's analytics cannot be joined against another's — while the
    -- salt does the other job, making a day's hashes irreversible once purged.
    visitor_hash  bytea,
    -- Dormant. Intended as "first time this visitor_hash was seen for this link
    -- today", for Phase 2's new-versus-returning split. Nothing computes it and
    -- nothing reads it, so it is always false; deriving it at ingest would cost
    -- a lookup per click for a number no surface shows.
    is_first_visit boolean    NOT NULL DEFAULT false,

    country       text,
    region        text,
    city          text,
    device        text,
    browser       text,
    os            text,
    language      text,
    -- Host only. The full referrer URL frequently carries query parameters
    -- with personal data, so it is discarded at the edge rather than stored
    -- and cleaned later.
    referrer_host text,
    is_bot        boolean     NOT NULL DEFAULT false,
    -- Server-side handling time, for the latency SLO.
    latency_us    int,

    -- The partition key must be part of the primary key.
    PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

CREATE INDEX click_events_link_time_idx  ON click_events (link_id, occurred_at DESC);
CREATE INDEX click_events_ws_time_idx    ON click_events (workspace_id, occurred_at DESC);

-- Daily unique-visitor tracking, also partitioned. Separate from click_events
-- so the uniqueness constraint does not sit on the highest-write table.
CREATE TABLE visitors (
    visitor_hash bytea       NOT NULL,
    link_id      uuid        NOT NULL,
    workspace_id uuid        NOT NULL,
    seen_on      date        NOT NULL,
    first_seen_at timestamptz NOT NULL,
    occurred_at  timestamptz NOT NULL,
    PRIMARY KEY (visitor_hash, link_id, seen_on, occurred_at)
) PARTITION BY RANGE (occurred_at);

-- Pre-aggregated rollups. Dashboards read these, never the raw events, which
-- is what keeps analytics queries under the 2s target as click_events grows
-- into the tens of millions.

CREATE TABLE link_click_daily (
    link_id      uuid   NOT NULL,
    workspace_id uuid   NOT NULL,
    day          date   NOT NULL,
    clicks       bigint NOT NULL DEFAULT 0,
    unique_visitors bigint NOT NULL DEFAULT 0,
    bot_clicks   bigint NOT NULL DEFAULT 0,
    -- Set once the day is closed and recomputed; until then the row is a
    -- running estimate that the rollup job keeps refreshing.
    finalized_at timestamptz,
    PRIMARY KEY (link_id, day)
);

CREATE INDEX link_click_daily_ws_day_idx ON link_click_daily (workspace_id, day DESC);

CREATE TABLE link_dimension_daily (
    link_id      uuid   NOT NULL,
    workspace_id uuid   NOT NULL,
    day          date   NOT NULL,
    -- country | region | city | device | browser | os | referrer_host | language
    dimension    text   NOT NULL,
    value        text   NOT NULL,
    clicks       bigint NOT NULL DEFAULT 0,
    unique_visitors bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (link_id, day, dimension, value)
);

CREATE INDEX link_dimension_daily_ws_idx
    ON link_dimension_daily (workspace_id, day DESC, dimension);

CREATE TABLE workspace_click_daily (
    workspace_id uuid   NOT NULL,
    day          date   NOT NULL,
    clicks       bigint NOT NULL DEFAULT 0,
    unique_visitors bigint NOT NULL DEFAULT 0,
    bot_clicks   bigint NOT NULL DEFAULT 0,
    active_links bigint NOT NULL DEFAULT 0,
    finalized_at timestamptz,
    PRIMARY KEY (workspace_id, day)
);

-- Bookkeeping for the scheduler: last run and watermark per job.
CREATE TABLE job_state (
    job         text        PRIMARY KEY,
    last_run_at timestamptz,
    watermark   timestamptz,
    last_error  text,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS job_state;
DROP TABLE IF EXISTS workspace_click_daily;
DROP TABLE IF EXISTS link_dimension_daily;
DROP TABLE IF EXISTS link_click_daily;
DROP TABLE IF EXISTS visitors;
DROP TABLE IF EXISTS click_events;
DROP TABLE IF EXISTS analytics_salts;
