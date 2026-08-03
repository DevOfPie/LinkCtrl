-- Links, destinations and tags.

-- name: CreateLink :one
INSERT INTO links (
    id, workspace_id, domain_id, alias, primary_url,
    title, description, status, expires_at, created_by, forward_query,
    forward_path, password_hash, max_clicks, one_time, require_signature,
    folder_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
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
       -- The gates (M35). Each is three-valued through its own pair of
       -- arguments, because "leave it alone" and "remove it" are different
       -- requests and a nullable column cannot express both with one nullable
       -- parameter. clear_password wins over password_hash for the same reason
       -- clear_expiry wins over expires_at, and the order is the same.
       password_hash = CASE WHEN sqlc.arg(clear_password)::boolean THEN NULL
                            ELSE COALESCE(sqlc.narg(password_hash), password_hash) END,
       max_clicks    = CASE WHEN sqlc.arg(clear_max_clicks)::boolean THEN NULL
                            ELSE COALESCE(sqlc.narg(max_clicks), max_clicks) END,
       one_time      = COALESCE(sqlc.narg(one_time), one_time),
       require_signature = COALESCE(sqlc.narg(require_signature), require_signature),
       -- Which folder the link is filed in (M38). Three-valued for the reason
       -- the gates above are: "leave it where it is" and "take it out of every
       -- folder" are different requests, and a nullable column cannot express
       -- both through one nullable parameter. The service has already checked
       -- that the folder belongs to this workspace — the foreign key does not,
       -- because it points at folders(id) and says nothing about tenancy.
       folder_id     = CASE WHEN sqlc.arg(clear_folder)::boolean THEN NULL
                            ELSE COALESCE(sqlc.narg(folder_id), folder_id) END,
       updated_at    = now()
 WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id) AND deleted_at IS NULL
RETURNING *;

-- name: UpdateDestinationURL :exec
-- The trigger on destinations mirrors this into links.primary_url, so the hot
-- path never joins.
--
-- Narrowed to the *primary* destination by M34, and the narrowing is the point
-- rather than tidying. Until routing rules existed a link had exactly one
-- destination row, so matching on link_id alone matched it; a rule target is a
-- second row on the same link, and this query would have rewritten every one of
-- them to the link's own URL the next time somebody edited the link. Every rule
-- on the link would silently start pointing at the same place, which is
-- indistinguishable from the rules having stopped working.
--
-- Matched through links.primary_destination_id rather than through `position =
-- 0`, because that column is what the sync trigger keys on and what the rest of
-- the schema treats as the authority. Two definitions of "the primary" is how
-- they come to disagree.
UPDATE destinations d
   SET url = $3, url_host = $4, updated_at = now()
  FROM links l
 WHERE l.id = $1
   AND l.workspace_id = $2
   AND d.id = l.primary_destination_id
   AND d.deleted_at IS NULL;

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
  -- The folder filter (M38), in two halves because "no filter" and "the links
  -- that are in no folder" are different questions and a nullable id can only
  -- ask one of them. `unfiled` is the second; without it there is no way to find
  -- the links that were never filed, which is the state every link starts in.
  --
  -- One folder, not its subtree. The number this filter returns has to be the
  -- number shown beside the folder on the tree page, and a parent that reported
  -- its descendants' links would disagree with it — and would put a recursive
  -- walk inside the dashboard's hottest query to do so.
  AND (NOT sqlc.arg(unfiled)::boolean OR l.folder_id IS NULL)
  AND (sqlc.narg(folder_id)::uuid IS NULL OR l.folder_id = sqlc.narg(folder_id)::uuid)
  -- Which hostname the link is served on (M40). One filter and no `unhosted`
  -- half, unlike the folder pair above: `links.domain_id` is NOT NULL, so there
  -- is no third state to ask about — every link is on exactly one domain, and
  -- "no filter" is the only other question there is.
  AND (sqlc.narg(domain_id)::uuid IS NULL OR l.domain_id = sqlc.narg(domain_id)::uuid)
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
                     AND lt.tag_id = ANY(sqlc.narg(tag_ids)::uuid[])))
  -- Must mirror ListLinks exactly, for the reason the tag filter above says in
  -- full: a folder-filtered page whose total counted the whole workspace would
  -- read "3 of 40 links" under a list of every link in one folder.
  AND (NOT sqlc.arg(unfiled)::boolean OR l.folder_id IS NULL)
  AND (sqlc.narg(folder_id)::uuid IS NULL OR l.folder_id = sqlc.narg(folder_id)::uuid)
  -- Must mirror ListLinks exactly, for the reason the two filters above say in
  -- full: a page filtered to one hostname whose total counted every domain
  -- would read "4 of 40 links" over a list of four.
  AND (sqlc.narg(domain_id)::uuid IS NULL OR l.domain_id = sqlc.narg(domain_id)::uuid);

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
-- The hostname a new link goes on when the caller names none.
--
-- **This is the filter the name promised and the query never had** (M40). It
-- read `WHERE is_default` with no workspace argument at all, so every workspace
-- on the instance got the same answer and the word "workspace" in the name was
-- describing an intention rather than a predicate.
--
-- A workspace's own *verified* hostname wins over the instance default, which is
-- what registering one is for; the instance default is the fallback and is what
-- every workspace without one still gets, unchanged. Organization-owned
-- hostnames sit between the two — every workspace in the organization may use
-- one, so it is more specific than the instance and less than a workspace's own.
--
-- **Verified only.** An unverified hostname is not a routing target, so putting
-- a link on it would mint a short URL that resolves nowhere; the ordering below
-- cannot reach one because the WHERE clause has already excluded it.
--
-- Ties are broken by verified_at then id, so the answer is stable: a workspace
-- that verifies a second hostname does not silently move its new links onto it.
SELECT id, hostname FROM domains
WHERE deleted_at IS NULL
  AND (
        is_default
     OR (verified_at IS NOT NULL AND workspace_id = sqlc.arg(workspace_id))
     OR (verified_at IS NOT NULL AND workspace_id IS NULL
         AND organization_id = sqlc.arg(organization_id))
      )
ORDER BY
    CASE WHEN workspace_id = sqlc.arg(workspace_id) THEN 0
         WHEN NOT is_default                        THEN 1
         ELSE 2 END,
    verified_at, id
LIMIT 1;

-- name: GetDefaultDomainSettings :one
-- One domain's settings and where its root points.
--
-- **The filter its own comment promised** (M40). It read `WHERE is_default`,
-- which was the whole truth while an instance had exactly one domain and became
-- a way of asking the wrong row the moment it had several: a verified custom
-- hostname has a root of its own, and reading its settings through a predicate
-- that can only ever return the default would answer about somebody else's
-- hostname.
--
-- Not scoped by owner, like every other statement addressed by id in this
-- schema. link.Service has already judged the actor against the row.
SELECT id, hostname, root_redirect_url, block_bots, block_bots_enforced
FROM domains
WHERE id = sqlc.arg(domain_id) AND deleted_at IS NULL;

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
