-- Routing rules (M34).
--
-- Every query here filters on kind = 'match'. That is not defensive coding: the
-- column's CHECK also permits weighted, sequential and fallback, which are
-- M36's, and a query that read every kind would start behaving differently the
-- day those rows first exist — silently, on the redirect path. The filter means
-- M36 has to write its own reads, which is the correct amount of work for a
-- milestone that adds a new evaluation model.
--
-- Rule *targets* are ordinary `destinations` rows. A rule therefore costs two
-- writes and the destination is what carries the URL, its host, and the M30
-- tier check the service applied before either row existed.

-- name: CreateRoutingRule :one
INSERT INTO routing_rules (
    id, link_id, workspace_id, destination_id, priority, conditions, kind, enabled
) VALUES ($1, $2, $3, $4, $5, $6, 'match', $7)
RETURNING *;

-- name: ListRoutingRules :many
-- The management list: every rule on a link, enabled or not, in the order the
-- redirect path would evaluate them.
--
-- Ordered by (priority, created_at) rather than by priority alone. Priority is
-- not unique, and two rules that tie have to be evaluated in a defined order or
-- the same request resolves differently on two replicas. Creation order is the
-- tiebreak because it is the only one a person can predict from the list they
-- are looking at.
--
-- Workspace-scoped in the WHERE like every other management read, so a rule
-- belonging to another tenant returns no rows rather than a row the caller has
-- to remember to reject.
SELECT rr.id, rr.link_id, rr.workspace_id, rr.destination_id, rr.priority,
       rr.conditions, rr.enabled, rr.created_at, rr.updated_at,
       d.url::text AS url
FROM routing_rules rr
JOIN destinations d ON d.id = rr.destination_id
WHERE rr.link_id = $1
  AND rr.workspace_id = $2
  AND rr.kind = 'match'
  AND d.deleted_at IS NULL
ORDER BY rr.priority, rr.created_at;

-- name: GetRoutingRule :one
SELECT rr.id, rr.link_id, rr.workspace_id, rr.destination_id, rr.priority,
       rr.conditions, rr.enabled, rr.created_at, rr.updated_at,
       d.url::text AS url
FROM routing_rules rr
JOIN destinations d ON d.id = rr.destination_id
WHERE rr.id = $1
  AND rr.workspace_id = $2
  AND rr.kind = 'match'
  AND d.deleted_at IS NULL;

-- name: CountRoutingRules :one
-- Read before an insert, to enforce the per-link ceiling. Counts every kind,
-- not only 'match': the ceiling exists because the whole list travels inside
-- the cached snapshot and is walked in order on the redirect path, and M36's
-- rows will be in that list too.
SELECT count(*) FROM routing_rules WHERE link_id = $1;

-- name: UpdateRoutingRule :one
-- Partial update, same COALESCE-with-narg shape as UpdateLink. The destination
-- is not here: changing where a rule points is a write to its `destinations`
-- row, so that the URL, its host and the tier check that accepted it stay in
-- one place.
UPDATE routing_rules
   SET priority   = COALESCE(sqlc.narg(priority), priority),
       conditions = COALESCE(sqlc.narg(conditions), conditions),
       enabled    = COALESCE(sqlc.narg(enabled), enabled),
       updated_at = now()
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND kind = 'match'
RETURNING *;

-- name: DeleteRoutingRule :one
-- Returns the destination so the caller can remove it in the same transaction.
-- A rule target left behind would be an orphan row nothing reads and nothing
-- can reach, accumulating one per deleted rule for the life of the link.
DELETE FROM routing_rules
WHERE id = $1 AND workspace_id = $2 AND kind = 'match'
RETURNING destination_id;

-- name: CreateRuleDestination :one
-- A rule's target. Position is above zero so it can never be mistaken for the
-- link's own destination, which Phase 1 put at position 0 and which
-- `links.primary_destination_id` points at.
INSERT INTO destinations (id, link_id, workspace_id, url, url_host, position)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: NextRuleDestinationPosition :one
-- The next free position above the primary. COALESCE so the first rule on a
-- link lands at 1 rather than at NULL.
SELECT COALESCE(max(position), 0) + 1 FROM destinations
WHERE link_id = $1 AND deleted_at IS NULL;

-- name: UpdateRuleDestinationURL :exec
-- Scoped to one destination id, unlike UpdateDestinationURL. A rule target and
-- the link's own destination are two rows on the same link now, and the wrong
-- one of these two queries would move both.
UPDATE destinations
   SET url = $2, url_host = $3, updated_at = now()
 WHERE id = $1 AND workspace_id = $4 AND deleted_at IS NULL;

-- name: DeleteRuleDestination :exec
-- A hard delete, not the soft delete `deleted_at` exists for. Nothing reports
-- on a rule target and nothing can restore a deleted rule, so a tombstone here
-- would be a row that only ever grows the table.
-- The NOT EXISTS is a guard rather than a branch anything takes: only rule
-- targets reach this query. It is here because the one row this must never
-- delete is the link's own destination, and the cost of being wrong about that
-- is a link that redirects nowhere.
DELETE FROM destinations d
WHERE d.id = $1 AND d.workspace_id = $2
  AND NOT EXISTS (SELECT 1 FROM links l WHERE l.primary_destination_id = d.id);

-- --- split testing (M36) -----------------------------------------------------
--
-- A variant is a rule row of kind weighted, sequential or fallback, pointing at
-- its own `destinations` row exactly as a match rule's target does. So the
-- writes below are the same two writes, and the M30 tier check the service
-- applied before either row existed is the same check.
--
-- Every query here excludes `match` for the reason every query above requires
-- it: the two management surfaces address disjoint sets of rows, so a rule id
-- handed to the wrong endpoint finds nothing rather than editing a rule of a
-- kind the caller was not looking at.

-- name: CreateVariantRule :one
-- Priority is fixed at the column default and never read for a variant: arms are
-- chosen, not matched in order, and a priority on one would be a number the
-- dashboard shows and nothing obeys. Creation order is what orders a rotation,
-- which is what ListVariantRules sorts by.
INSERT INTO routing_rules (
    id, link_id, workspace_id, destination_id, conditions, kind, enabled
) VALUES ($1, $2, $3, $4, '{}'::jsonb, sqlc.arg(kind), $5)
RETURNING *;

-- name: ListVariantRules :many
-- A link's whole split, including the disabled arms and the fallback, in
-- rotation order.
--
-- Ordered by the destination's position rather than by created_at: position is
-- assigned once, from NextRuleDestinationPosition, and is the only ordering that
-- a person reading the list can predict and that a rotation can be explained
-- against. Two arms created in the same transaction cannot tie on it.
SELECT rr.id, rr.link_id, rr.workspace_id, rr.destination_id, rr.kind,
       rr.enabled, rr.created_at, rr.updated_at,
       d.url::text AS url, d.weight, d.position
FROM routing_rules rr
JOIN destinations d ON d.id = rr.destination_id
WHERE rr.link_id = $1
  AND rr.workspace_id = $2
  AND rr.kind <> 'match'
  AND d.deleted_at IS NULL
ORDER BY d.position;

-- name: GetVariantRule :one
SELECT rr.id, rr.link_id, rr.workspace_id, rr.destination_id, rr.kind,
       rr.enabled, rr.created_at, rr.updated_at,
       d.url::text AS url, d.weight, d.position
FROM routing_rules rr
JOIN destinations d ON d.id = rr.destination_id
WHERE rr.id = $1
  AND rr.workspace_id = $2
  AND rr.kind <> 'match'
  AND d.deleted_at IS NULL;

-- name: UpdateVariantRule :one
-- Only the enabled flag, because that is the only thing on the rule row a
-- variant has. A weight is a write to the destination; a kind is not editable at
-- all, since changing one arm's kind would mix two kinds on a link and the
-- service refuses that outright.
UPDATE routing_rules
   SET enabled    = COALESCE(sqlc.narg(enabled), enabled),
       updated_at = now()
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND kind <> 'match'
RETURNING *;

-- name: DeleteVariantRule :one
DELETE FROM routing_rules
WHERE id = $1 AND workspace_id = $2 AND kind <> 'match'
RETURNING destination_id;

-- name: CreateVariantDestination :one
-- A variant's target, with its weight. The same row CreateRuleDestination
-- writes, plus the one column that makes it an arm rather than a target: 00300
-- has carried `weight` since Phase 1 with a comment naming this milestone.
INSERT INTO destinations (id, link_id, workspace_id, url, url_host, position, weight)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: UpdateVariantDestination :exec
-- The URL, the weight, or both. Scoped to one destination id for the reason
-- UpdateRuleDestinationURL is: a link's own destination and its arms are rows in
-- the same table on the same link.
UPDATE destinations
   SET url        = COALESCE(sqlc.narg(url), url),
       url_host   = COALESCE(sqlc.narg(url_host), url_host),
       weight     = COALESCE(sqlc.narg(weight), weight),
       updated_at = now()
 WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id)
   AND deleted_at IS NULL;

-- name: ListLinkDestinations :many
-- Every destination a link has, for the per-destination breakdown to name.
--
-- Includes the link's own destination at position 0, because a click recorded
-- before the link had a split — or on a link whose split was later removed —
-- carries a NULL destination_id and is attributed to exactly that row. A
-- breakdown that could not name it would be a chart with an unlabelled bar
-- holding most of the traffic.
SELECT d.id, d.url::text AS url, d.weight, d.position,
       (d.id = l.primary_destination_id) AS is_primary
FROM destinations d
JOIN links l ON l.id = d.link_id
WHERE d.link_id = $1
  AND d.workspace_id = $2
  AND d.deleted_at IS NULL
ORDER BY d.position;
