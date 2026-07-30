-- +goose Up
--
-- Links and everything that hangs off them.
--
-- The single most consequential file in the schema: it holds the alias unique
-- index, the denormalized hot-path column, and the search vector.

CREATE TABLE domains (
    id              uuid        PRIMARY KEY,
    -- NULL for the instance default domain, which every workspace shares.
    -- Custom domains (Phase 2) belong to an organization.
    organization_id uuid        REFERENCES organizations(id) ON DELETE CASCADE,
    hostname        text        NOT NULL,
    is_default      boolean     NOT NULL DEFAULT false,
    verified_at     timestamptz,
    ssl_status      text        NOT NULL DEFAULT 'none'
                    CHECK (ssl_status IN ('none', 'pending', 'active', 'error')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz
);

CREATE UNIQUE INDEX domains_hostname_key ON domains (lower(hostname)) WHERE deleted_at IS NULL;
-- At most one default domain.
CREATE UNIQUE INDEX domains_single_default ON domains ((is_default)) WHERE is_default;

CREATE TABLE folders (
    id           uuid        PRIMARY KEY,
    workspace_id uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    parent_id    uuid        REFERENCES folders(id) ON DELETE CASCADE,
    name         text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz
);
-- PHASE 2: table only in Phase 1. No API, no UI. Links may reference a folder
-- but nothing creates one yet.
CREATE INDEX folders_workspace_idx ON folders (workspace_id) WHERE deleted_at IS NULL;

CREATE TABLE tags (
    id           uuid        PRIMARY KEY,
    workspace_id uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         text        NOT NULL,
    color        text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX tags_workspace_name_key ON tags (workspace_id, lower(name));

CREATE TABLE links (
    id            uuid        PRIMARY KEY,
    workspace_id  uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Present in Phase 1 despite custom domains being Phase 2. It makes the
    -- unique index and the cache key correct now, so Phase 2 needs no data
    -- migration and no cache-key version bump.
    domain_id     uuid        NOT NULL REFERENCES domains(id),
    folder_id     uuid        REFERENCES folders(id) ON DELETE SET NULL,

    alias         text        NOT NULL,
    -- Denormalized from the primary destination so the hot path reads one row.
    -- Kept in sync by trigger; a consistency check in the test suite asserts
    -- it never drifts.
    primary_url   text        NOT NULL,
    primary_destination_id uuid,

    title         text        NOT NULL DEFAULT '',
    description   text        NOT NULL DEFAULT '',

    status        text        NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'archived', 'expired', 'disabled')),

    expires_at    timestamptz,
    -- PHASE 2: columns exist so the cached snapshot carries them and the API
    -- can reject them with a clear 422 rather than silently ignoring them.
    -- One-time and max-click enforcement needs a durable counter, which a
    -- cache-only Redis cannot provide.
    password_hash text,
    max_clicks    bigint,
    one_time      boolean     NOT NULL DEFAULT false,
    forward_query boolean     NOT NULL DEFAULT false,

    -- Approximate by design: updated in batches with the click events, so it
    -- lags by up to one flush interval and loses at most one batch on SIGKILL.
    -- Nothing that must be exact may read it.
    click_count   bigint      NOT NULL DEFAULT 0,
    last_click_at timestamptz,

    created_by    uuid        REFERENCES users(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    archived_at   timestamptz,
    deleted_at    timestamptz,
    -- Set when a link enters the trash; the purge job deletes the row after
    -- this passes.
    purge_after   timestamptz,

    search_vector tsvector
);

-- Alias uniqueness is per domain, not global. Written this way from Phase 1 so
-- adding custom domains later is purely additive.
CREATE UNIQUE INDEX links_domain_alias_key
    ON links (domain_id, alias) WHERE deleted_at IS NULL;

-- Keyset pagination for the dashboard list. Matches the ORDER BY exactly, so
-- the scan is index-only and stable while rows are inserted mid-scan.
CREATE INDEX links_workspace_created_idx
    ON links (workspace_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;

CREATE INDEX links_workspace_clicks_idx
    ON links (workspace_id, click_count DESC) WHERE deleted_at IS NULL;
CREATE INDEX links_folder_idx  ON links (folder_id) WHERE deleted_at IS NULL AND folder_id IS NOT NULL;
CREATE INDEX links_expiry_idx  ON links (expires_at)
    WHERE expires_at IS NOT NULL AND deleted_at IS NULL AND status = 'active';
CREATE INDEX links_purge_idx   ON links (purge_after) WHERE purge_after IS NOT NULL;
CREATE INDEX links_search_idx  ON links USING gin (search_vector);

-- Substring search. Created only when pg_trgm is available; see 00100.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm') THEN
        CREATE INDEX links_alias_trgm_idx ON links USING gin (alias gin_trgm_ops);
        CREATE INDEX links_url_trgm_idx   ON links USING gin (primary_url gin_trgm_ops);
    ELSE
        RAISE WARNING 'pg_trgm unavailable; substring search will use sequential scans.';
    END IF;
END
$$;
-- +goose StatementEnd

CREATE TABLE destinations (
    id           uuid        PRIMARY KEY,
    link_id      uuid        NOT NULL REFERENCES links(id) ON DELETE CASCADE,
    workspace_id uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    url          text        NOT NULL,
    -- Extracted at write time for the destination blocklist and for reporting;
    -- parsing a URL on the hot path would be wasted work.
    url_host     text        NOT NULL DEFAULT '',
    label        text        NOT NULL DEFAULT '',
    -- PHASE 2: weighted and geo routing. Phase 1 has exactly one destination
    -- per link, weight 100.
    weight       int         NOT NULL DEFAULT 100 CHECK (weight >= 0),
    position     int         NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz
);

CREATE INDEX destinations_link_idx ON destinations (link_id) WHERE deleted_at IS NULL;

ALTER TABLE links
    ADD CONSTRAINT links_primary_destination_fk
    FOREIGN KEY (primary_destination_id) REFERENCES destinations(id) ON DELETE SET NULL;

CREATE TABLE link_tags (
    link_id      uuid NOT NULL REFERENCES links(id) ON DELETE CASCADE,
    tag_id       uuid NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    PRIMARY KEY (link_id, tag_id)
);

CREATE INDEX link_tags_tag_idx ON link_tags (tag_id);

-- Aliases that must never be reissued.
--
-- Deleting a link frees its alias, because the unique index only covers live
-- rows. For a link that was never used that is fine and desirable. For one
-- that received traffic it is not: the alias is in the wild, on printed
-- material and in other people's bookmarks, and handing it to a different
-- destination is a redirect hijack waiting to happen. The purge job inserts
-- here before deleting any link with clicks.
CREATE TABLE reserved_aliases (
    domain_id   uuid        NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    alias       text        NOT NULL,
    reason      text        NOT NULL DEFAULT 'purged',
    reserved_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (domain_id, alias)
);

-- Keep primary_url in step with the primary destination.
--
-- Note what is deliberately NOT covered: a soft-delete of the primary
-- destination does not touch url, so it would not fire. The service layer
-- forbids soft-deleting the primary destination outright — repointing
-- primary_destination_id first is the correct operation anyway.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION sync_link_primary_url() RETURNS trigger AS $$
BEGIN
    UPDATE links l
       SET primary_url = d.url,
           updated_at  = now()
      FROM destinations d
     WHERE l.primary_destination_id = d.id
       AND d.id = NEW.id
       AND l.primary_url IS DISTINCT FROM d.url;
    RETURN NEW;
END
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER destinations_sync_primary_url
    AFTER INSERT OR UPDATE OF url ON destinations
    FOR EACH ROW EXECUTE FUNCTION sync_link_primary_url();

-- Maintain the search vector. Weighted so an alias match outranks a body
-- match, which is what a user expects when they type a code they half
-- remember.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION links_update_search_vector() RETURNS trigger AS $$
BEGIN
    NEW.search_vector :=
        setweight(to_tsvector('simple', coalesce(NEW.alias, '')), 'A') ||
        setweight(to_tsvector('english', coalesce(NEW.title, '')), 'B') ||
        setweight(to_tsvector('simple', coalesce(NEW.primary_url, '')), 'C') ||
        setweight(to_tsvector('english', coalesce(NEW.description, '')), 'D');
    RETURN NEW;
END
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER links_search_vector_trigger
    BEFORE INSERT OR UPDATE OF alias, title, primary_url, description ON links
    FOR EACH ROW EXECUTE FUNCTION links_update_search_vector();

-- +goose Down
DROP TRIGGER IF EXISTS links_search_vector_trigger ON links;
DROP FUNCTION IF EXISTS links_update_search_vector();
DROP TRIGGER IF EXISTS destinations_sync_primary_url ON destinations;
DROP FUNCTION IF EXISTS sync_link_primary_url();
DROP TABLE IF EXISTS reserved_aliases;
DROP TABLE IF EXISTS link_tags;
ALTER TABLE links DROP CONSTRAINT IF EXISTS links_primary_destination_fk;
DROP TABLE IF EXISTS destinations;
DROP TABLE IF EXISTS links;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS folders;
DROP TABLE IF EXISTS domains;
