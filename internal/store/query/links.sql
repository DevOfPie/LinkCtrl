-- Links, destinations and tags.

-- name: CreateLink :one
INSERT INTO links (
    id, workspace_id, domain_id, alias, primary_url,
    title, description, status, expires_at, created_by, forward_query,
    forward_path
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: CreateDestination :one
INSERT INTO destinations (id, link_id, workspace_id, url, url_host, position)
VALUES ($1, $2, $3, $4, $5, 0)
RETURNING *;

-- name: SetPrimaryDestination :exec
UPDATE links SET primary_destination_id = $2, updated_at = now() WHERE id = $1;

-- name: GetLink :one
-- Workspace-scoped by design. Passing the workspace here rather than checking
-- it after the fetch makes cross-tenant reads impossible to write by accident:
-- the wrong workspace returns no rows rather than a row the caller must
-- remember to reject.
SELECT * FROM links
WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL;

-- name: GetLinkByAlias :one
SELECT * FROM links
WHERE domain_id = $1 AND alias = $2 AND deleted_at IS NULL;

-- name: UpdateLink :one
-- COALESCE with sqlc.narg gives partial update: a NULL argument leaves the
-- column alone, so PATCH semantics need no dynamic SQL.
UPDATE links
   SET title         = COALESCE(sqlc.narg(title), title),
       description   = COALESCE(sqlc.narg(description), description),
       expires_at    = CASE WHEN sqlc.arg(clear_expiry)::boolean THEN NULL
                            ELSE COALESCE(sqlc.narg(expires_at), expires_at) END,
       alias         = COALESCE(sqlc.narg(alias), alias),
       forward_query = COALESCE(sqlc.narg(forward_query), forward_query),
       forward_path  = COALESCE(sqlc.narg(forward_path), forward_path),
       bot_blocking  = COALESCE(sqlc.narg(bot_blocking), bot_blocking),
       updated_at    = now()
 WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id) AND deleted_at IS NULL
RETURNING *;

-- name: UpdateDestinationURL :exec
-- The trigger on destinations mirrors this into links.primary_url, so the hot
-- path never joins.
UPDATE destinations
   SET url = $3, url_host = $4, updated_at = now()
 WHERE link_id = $1 AND workspace_id = $2 AND deleted_at IS NULL;

-- name: ArchiveLink :one
UPDATE links
   SET status = 'archived', archived_at = now(), updated_at = now()
 WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: RestoreLink :one
UPDATE links
   SET status = 'active', archived_at = NULL, updated_at = now()
 WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteLink :one
-- Soft delete with a purge deadline rather than an immediate DELETE. Restoring
-- a link someone deleted by accident is a common request, and the alias stays
-- reserved while the row exists.
UPDATE links
   SET deleted_at = now(),
       purge_after = now() + make_interval(days => sqlc.arg(retention_days)::int),
       updated_at = now()
 WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id) AND deleted_at IS NULL
RETURNING id, alias, domain_id, click_count;

-- name: ListLinks :many
-- Keyset pagination over (created_at, id).
--
-- The cursor is a composite so ordering is total: created_at alone is not
-- unique, and a tie at the page boundary would drop or duplicate rows.
-- Comparing the pair with row-value syntax lets the composite index serve it
-- directly.
--
-- Sorting is a CASE rather than three separate queries because sqlc has no
-- dynamic SQL. If plan stability becomes a problem this splits into
-- ListLinksNewest/Oldest/Clicks; measure before doing that.
-- The two tag aggregates are paired positionally by the caller, so they must
-- agree on their order — and on the table they read. Aggregating names from a
-- join and ids from link_tags alone, each sorted by its own column, produced
-- arrays in different orders whenever a link's tags sorted differently by name
-- than by id, and every tag came back carrying another tag's name. One
-- subquery, one ORDER BY, both columns.
SELECT
    l.*,
    COALESCE(tg.names, ARRAY[]::text[])::text[] AS tag_names,
    COALESCE(tg.ids, ARRAY[]::text[])::text[]   AS tag_ids
FROM links l
LEFT JOIN LATERAL (
    SELECT array_agg(t.name ORDER BY t.name, t.id)     AS names,
           array_agg(t.id::text ORDER BY t.name, t.id) AS ids
      FROM link_tags lt JOIN tags t ON t.id = lt.tag_id
     WHERE lt.link_id = l.id
) tg ON true
WHERE l.workspace_id = sqlc.arg(workspace_id)
  AND l.deleted_at IS NULL
  -- Filtered on effective status, not the stored column: nothing ever writes
  -- 'expired', so `?status=expired` matched no row while expired links listed
  -- themselves as active. Must stay identical to domain.EffectiveStatus and to
  -- Snapshot.Decide, expiry outranking an archived status in all three.
  AND (sqlc.narg(status)::text IS NULL
       OR CASE WHEN l.expires_at IS NOT NULL AND l.expires_at <= now()
               THEN 'expired' ELSE l.status END = sqlc.narg(status)::text)
  -- Full-text first, then trigram substring. websearch_to_tsquery returns an
  -- empty query for input like "and" or "!!", which matches nothing, so the
  -- caller treats a blank search as no filter rather than as zero results.
  AND (sqlc.narg(search)::text IS NULL
       -- A degenerate query means "no filter", not "match nothing".
       -- websearch_to_tsquery returns an empty tsquery for a stopword ("and")
       -- or pure punctuation ("!!"), and without this branch a user typing
       -- either gets an empty list and concludes their links have vanished.
       OR websearch_to_tsquery('english', sqlc.narg(search)::text) = ''::tsquery
       OR l.search_vector @@ websearch_to_tsquery('english', sqlc.narg(search)::text)
       OR l.alias ILIKE '%' || sqlc.narg(search)::text || '%'
       OR l.primary_url ILIKE '%' || sqlc.narg(search)::text || '%')
  AND (sqlc.narg(tag_ids)::uuid[] IS NULL
       OR EXISTS (SELECT 1 FROM link_tags lt
                   WHERE lt.link_id = l.id
                     AND lt.tag_id = ANY(sqlc.narg(tag_ids)::uuid[])))
  -- Keyset pagination only works if the predicate compares the same tuple the
  -- ORDER BY sorts on. It did not: every sort filtered on (created_at, id)
  -- while 'clicks' ordered by click_count, so page 2 dropped rows that belonged
  -- on it and repeated rows already shown. Each branch below pairs with the
  -- correspondingly-named ORDER BY key, and the id tiebreaker matches its
  -- direction — 'oldest' ascends, so its tiebreaker ascends too.
  AND (
        sqlc.narg(cursor_id)::uuid IS NULL
        OR (sqlc.arg(sort)::text = 'oldest'
              AND (l.created_at, l.id) > (sqlc.narg(cursor_created)::timestamptz, sqlc.narg(cursor_id)::uuid))
        OR (sqlc.arg(sort)::text = 'clicks'
              AND (l.click_count, l.id) < (sqlc.narg(cursor_clicks)::bigint, sqlc.narg(cursor_id)::uuid))
        OR (sqlc.arg(sort)::text NOT IN ('oldest','clicks')
              AND (l.created_at, l.id) < (sqlc.narg(cursor_created)::timestamptz, sqlc.narg(cursor_id)::uuid))
      )
ORDER BY
    CASE WHEN sqlc.arg(sort)::text = 'oldest' THEN l.created_at END ASC,
    CASE WHEN sqlc.arg(sort)::text = 'clicks' THEN l.click_count END DESC,
    CASE WHEN sqlc.arg(sort)::text NOT IN ('oldest','clicks') THEN l.created_at END DESC,
    -- Ascending tiebreaker for the ascending sort. For the others this key is
    -- NULL on every row, so it ties and the DESC key below decides.
    CASE WHEN sqlc.arg(sort)::text = 'oldest' THEN l.id END ASC,
    l.id DESC
LIMIT sqlc.arg(page_limit);

-- name: CountLinks :one
-- Only issued when the caller explicitly asks for a total, because counting
-- costs a scan the common page load should not pay for.
SELECT count(*) FROM links l
WHERE l.workspace_id = sqlc.arg(workspace_id)
  AND l.deleted_at IS NULL
  -- Filtered on effective status, not the stored column: nothing ever writes
  -- 'expired', so `?status=expired` matched no row while expired links listed
  -- themselves as active. Must stay identical to domain.EffectiveStatus and to
  -- Snapshot.Decide, expiry outranking an archived status in all three.
  AND (sqlc.narg(status)::text IS NULL
       OR CASE WHEN l.expires_at IS NOT NULL AND l.expires_at <= now()
               THEN 'expired' ELSE l.status END = sqlc.narg(status)::text)
  AND (sqlc.narg(search)::text IS NULL
       -- A degenerate query means "no filter", not "match nothing".
       -- websearch_to_tsquery returns an empty tsquery for a stopword ("and")
       -- or pure punctuation ("!!"), and without this branch a user typing
       -- either gets an empty list and concludes their links have vanished.
       OR websearch_to_tsquery('english', sqlc.narg(search)::text) = ''::tsquery
       OR l.search_vector @@ websearch_to_tsquery('english', sqlc.narg(search)::text)
       OR l.alias ILIKE '%' || sqlc.narg(search)::text || '%'
       OR l.primary_url ILIKE '%' || sqlc.narg(search)::text || '%')
  -- Must mirror ListLinks exactly. Without this branch a tag-filtered page
  -- reported the workspace's whole link count as its total, so "8 of 100" was
  -- shown for a filter matching 8 of 8.
  AND (sqlc.narg(tag_ids)::uuid[] IS NULL
       OR EXISTS (SELECT 1 FROM link_tags lt
                   WHERE lt.link_id = l.id
                     AND lt.tag_id = ANY(sqlc.narg(tag_ids)::uuid[])));

-- name: GetLinkTags :many
SELECT t.id, t.name, t.color
FROM link_tags lt JOIN tags t ON t.id = lt.tag_id
WHERE lt.link_id = $1
ORDER BY t.name;

-- name: ReserveAlias :exec
-- Called before purging a link that has clicks. The alias is in the wild — on
-- printed material and in other people's bookmarks — so handing it to a new
-- destination would be a redirect hijack.
INSERT INTO reserved_aliases (domain_id, alias, reason)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: PurgeExpiredLinks :many
-- The end of the trash window: hard-delete links whose purge_after has passed.
--
-- One statement, so the reservation and the deletion cannot be separated by a
-- crash: an alias that ever received traffic is written to reserved_aliases in
-- the same command that removes its row, and ON CONFLICT makes a retried run
-- converge rather than fail. Aliases that never received a click are released —
-- deliberately, per the reserved_aliases rationale: nothing in the wild points
-- at them, so permanent reservation would only bleed the namespace.
--
-- SKIP LOCKED so the purge can never block, or be blocked by, a concurrent
-- restore-by-hand of the same row; a skipped row is caught on the next run.
-- Destinations and link_tags follow by ON DELETE CASCADE. click_events rows
-- carry no FK (partitioned) and are dropped by analytics retention instead.
WITH doomed AS (
    SELECT id, domain_id, alias, click_count
      FROM links
     WHERE deleted_at IS NOT NULL
       AND purge_after IS NOT NULL
       AND purge_after < now()
     ORDER BY purge_after
     LIMIT sqlc.arg(batch_size)::int
       FOR UPDATE SKIP LOCKED
),
reserve AS (
    INSERT INTO reserved_aliases (domain_id, alias, reason)
    SELECT domain_id, alias, 'purged with traffic'
      FROM doomed
     WHERE click_count > 0
    ON CONFLICT DO NOTHING
)
DELETE FROM links l
 USING doomed d
 WHERE l.id = d.id
RETURNING l.alias, (d.click_count > 0)::boolean AS reserved;

-- --- tags -------------------------------------------------------------------

-- name: CreateTag :one
INSERT INTO tags (id, workspace_id, name, color)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetTagByName :one
SELECT * FROM tags WHERE workspace_id = $1 AND lower(name) = lower($2);

-- name: ListTags :many
-- Counts l.id, not lt.link_id. The join onto links is what excludes trashed
-- links, but counting the link_tags column ignored it: a LEFT JOIN keeps the
-- link_tags row when its link is soft-deleted, so the count included trashed
-- links for the whole 30-day window and the tag list disagreed with the link
-- list it filters.
SELECT t.*, count(l.id) AS link_count
FROM tags t
LEFT JOIN link_tags lt ON lt.tag_id = t.id
LEFT JOIN links l ON l.id = lt.link_id AND l.deleted_at IS NULL
WHERE t.workspace_id = $1
GROUP BY t.id
ORDER BY t.name;

-- name: DeleteTag :execrows
DELETE FROM tags WHERE id = $1 AND workspace_id = $2;

-- name: AttachTag :exec
INSERT INTO link_tags (link_id, tag_id, workspace_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: DetachAllTags :exec
DELETE FROM link_tags WHERE link_id = $1;

-- name: GetWorkspaceDefaultDomain :one
SELECT id, hostname FROM domains WHERE is_default AND deleted_at IS NULL;

-- name: GetDefaultDomainSettings :one
-- The instance's link domain and where its root points. Phase 1 has exactly one
-- default domain; Phase 2 gives a workspace its own and this gains a filter.
SELECT id, hostname, root_redirect_url, block_bots, block_bots_enforced
FROM domains
WHERE is_default AND deleted_at IS NULL;

-- name: GetDomainBotSettings :one
-- One domain's bot policy, by id.
--
-- Read on the management path only, and only when a link's own setting is being
-- changed: the service has to know whether the domain enforces before it can
-- tell the caller their `off` will not be honoured. The redirect path never
-- runs this — it gets the same two columns from ResolveAliasForRedirect's join,
-- which is the whole reason that join exists.
SELECT id, hostname, block_bots, block_bots_enforced
FROM domains
WHERE id = $1 AND deleted_at IS NULL;

-- name: SetDefaultDomainRootRedirect :one
-- NULL clears it, which restores the 404 the root answered before anyone set
-- anything.
UPDATE domains
   SET root_redirect_url = sqlc.narg(root_redirect_url), updated_at = now()
 WHERE is_default AND deleted_at IS NULL
RETURNING id, hostname, root_redirect_url, block_bots, block_bots_enforced;

-- name: SetDefaultDomainBotBlocking :one
-- Both switches at once, because they are one setting with two halves and the
-- CHECK in 01800 refuses the combination that writing them separately would
-- pass through on the way.
UPDATE domains
   SET block_bots          = sqlc.arg(block_bots)::boolean,
       block_bots_enforced = sqlc.arg(block_bots_enforced)::boolean,
       updated_at          = now()
 WHERE is_default AND deleted_at IS NULL
RETURNING id, hostname, root_redirect_url, block_bots, block_bots_enforced;
