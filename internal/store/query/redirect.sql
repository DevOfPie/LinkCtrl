-- The redirect hot path.
--
-- Everything here runs under a 20ms budget on the dedicated redirect pool.
-- Keep the query set small, index-covered, and free of joins that are not
-- strictly required.

-- name: ResolveAliasForRedirect :one
-- Single-row lookup on links_domain_alias_key.
--
-- primary_url is read from the denormalized column rather than joined from
-- destinations: the join would double the row fetches on the hottest query in
-- the system to retrieve a value a trigger already keeps in step.
--
-- Status and expiry are returned rather than filtered, so the handler can
-- distinguish 404 (unknown or archived) from 410 (expired) and can cache a
-- negative result. Filtering here would make every non-serving state look
-- identical.
SELECT
    id,
    workspace_id,
    domain_id,
    alias,
    primary_url,
    status,
    expires_at,
    password_hash,
    max_clicks,
    one_time,
    forward_query
FROM links
WHERE domain_id = $1
  AND alias = $2
  AND deleted_at IS NULL;

-- name: ResolveDefaultDomain :one
-- Read once at boot and cached. The default domain is matched on the flag
-- rather than on a hostname string, so it never has to agree with
-- LINKCTRL_BASE_URL.
SELECT id, hostname
FROM domains
WHERE is_default AND deleted_at IS NULL;

-- name: ResolveDomainByHostname :one
-- PHASE 2: custom domains. Present now because the cache key is already
-- host-scoped, so enabling it later needs no key change.
SELECT id, organization_id, hostname
FROM domains
WHERE lower(hostname) = lower($1)
  AND deleted_at IS NULL;

-- name: IsAliasTaken :one
-- Consulted by BOTH create paths — generated aliases before insert, and
-- user-supplied aliases as validation — and by alias changes.
--
-- No deleted_at filter on the links branch, deliberately: a soft-deleted row
-- holds its alias for the whole trash window, so a link deleted by accident can
-- be restored under its own name. The partial unique index cannot enforce that
-- (it ignores trashed rows), so this check is the enforcement and the index
-- remains the guarantee against live-row races only.
SELECT EXISTS (
    SELECT 1 FROM links l
    WHERE l.domain_id = $1 AND l.alias = $2
    UNION ALL
    -- Aliases of purged links that had traffic. Reissuing one would silently
    -- redirect somebody else's printed QR code to a new destination.
    SELECT 1 FROM reserved_aliases ra
    WHERE ra.domain_id = $1 AND ra.alias = $2
);
