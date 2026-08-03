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
--
-- The lateral is M34's, and it obeys the same rule the domain join does: the
-- routing rules a link carries have to be in the snapshot, and the alternatives
-- are a second query on every cache miss or a second cache with its own
-- invalidation. This is neither. It is an index probe on
-- `routing_rules_link_idx` — partial on `enabled`, keyed on (link_id,
-- priority) — which finds nothing at all for the overwhelming majority of
-- links, because a link with no rules is the default and always will be.
-- Nothing about it runs on a cache *hit*: by then the rules are already inside
-- the snapshot.
--
-- The rules come back as one jsonb array, already in evaluation order, because
-- ordering them here is free and ordering them in Go would mean the sort that
-- decides which destination a visitor gets lived somewhere other than the query
-- that reads them. The keys are spelled out rather than short: this is the
-- database's own vocabulary, and the compact spelling the cached snapshot uses
-- is the Go type's business.
--
-- M36 widened the lateral to every kind and left it a single probe, which is the
-- property worth protecting: a link's match rules and its split arms live in one
-- table, on one index, and asking for them separately would double the cost of
-- the only lookup a cache miss makes. The ordering carries both vocabularies at
-- once. `rr.kind <> 'match'` sorts false before true, so M34's rules come first
-- and keep their (priority, created_at) order exactly; the arms follow in
-- `dest.position` order, which is what a rotation is explained against and what
-- the dashboard lists. `created_at` remains the last tiebreak so the order is
-- total whatever else ties.
--
-- `id` and `weight` are new here. The id is what a click is attributed to —
-- click_events.destination_id — and the weight is the arm's share; both have to
-- be in the snapshot because reading either at request time would be the query
-- this design exists to avoid.
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
    l.require_signature,
    l.forward_query,
    l.forward_path,
    l.bot_blocking,
    d.block_bots,
    d.block_bots_enforced,
    COALESCE(r.rules, '[]'::jsonb)::jsonb AS rules
FROM links l
JOIN domains d ON d.id = l.domain_id
LEFT JOIN LATERAL (
    SELECT jsonb_agg(
               jsonb_build_object(
                   'id', dest.id,
                   'url', dest.url,
                   'kind', rr.kind,
                   'weight', dest.weight,
                   'conditions', rr.conditions)
               ORDER BY (rr.kind <> 'match'), rr.priority, dest.position, rr.created_at
           ) AS rules
    FROM routing_rules rr
    JOIN destinations dest ON dest.id = rr.destination_id AND dest.deleted_at IS NULL
    WHERE rr.link_id = l.id
      AND rr.enabled
) r ON true
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
