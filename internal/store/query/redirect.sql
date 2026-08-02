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
--
-- The one join this query has, and the rule above is why it reads the way it
-- does. M32.5 needs the domain's bot-blocking settings on the redirect path,
-- and the alternative — a second lookup, or a second cache with its own
-- invalidation — would put either an extra round trip or an extra staleness
-- window on the hottest path in the product. This is neither: `domain_id` is
-- the domains primary key, the table holds one row on every deployment built so
-- far, and the two booleans ride home inside the round trip that was happening
-- anyway. The cached snapshot then carries them, so a cache hit answers the
-- whole question — link policy and domain policy together — without asking
-- anything.
--
-- No `d.deleted_at IS NULL` here, deliberately. A soft-deleted domain row still
-- joins, which is exactly the behaviour this query had before the join existed;
-- adding the filter would silently turn every link on such a domain into a 404,
-- which is a change nobody asked this milestone to make.
SELECT
    l.id,
    l.workspace_id,
    l.domain_id,
    l.alias,
    l.primary_url,
    l.status,
    l.expires_at,
    l.password_hash,
    l.max_clicks,
    l.one_time,
    l.forward_query,
    l.forward_path,
    l.bot_blocking,
    d.block_bots,
    d.block_bots_enforced
FROM links l
JOIN domains d ON d.id = l.domain_id
WHERE l.domain_id = $1
  AND l.alias = $2
  AND l.deleted_at IS NULL;

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
