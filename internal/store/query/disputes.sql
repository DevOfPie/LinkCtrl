-- Blocked-attempt disputes (M31).
--
-- Read and written on the management path only. Like the blocklist it argues
-- with, nothing here is reachable from the redirect tree: a dispute is about
-- what may be *stored*, and a link that was refused at creation never became a
-- row for a visitor to resolve.

-- name: InsertDestinationDispute :one
-- Files one dispute.
--
-- The unique partial index on (host) WHERE status = 'open' is what makes a
-- second open dispute about the same host a constraint violation rather than a
-- duplicate row, so the "already under review" answer is decided by the database
-- and not by a check-then-insert that two requests can both pass.
INSERT INTO destination_disputes (
    id, host, url_defanged, reason_code,
    organization_id, workspace_id, created_by, created_by_label
) VALUES (
    @id, lower(sqlc.arg(host)::text), @url_defanged, @reason_code,
    @organization_id, @workspace_id, @created_by, @created_by_label
)
RETURNING *;

-- name: GetDestinationDispute :one
SELECT * FROM destination_disputes WHERE id = @id;

-- name: ListDestinationDisputes :many
-- The queue, newest first, keyset on (created_at, id).
--
-- Instance-wide rather than scoped to the reader's organization, because the
-- list a decision acts on is instance-wide (01500) and a queue narrower than the
-- authority it exercises would hide rows the reader is nonetheless deciding for.
-- The permission is what bounds who sees it.
--
-- @open_only lets the page show the work and the archive from one query, which
-- is the same shape ListNotifications' unread filter has.
SELECT * FROM destination_disputes
 WHERE (NOT @open_only::boolean OR status = 'open')
   AND (
        sqlc.narg('cursor_created')::timestamptz IS NULL
     OR (created_at, id) < (sqlc.narg('cursor_created')::timestamptz, sqlc.narg('cursor_id')::uuid)
   )
 ORDER BY created_at DESC, id DESC
 LIMIT @page_limit;

-- name: CountOpenDestinationDisputes :one
-- What the queue's heading says there is to do. Served by the partial unique
-- index, whose predicate this matches exactly.
SELECT count(*) FROM destination_disputes WHERE status = 'open';

-- name: HostHasAllowedDispute :one
-- Whether the instance owner has allowed this host (M32).
--
-- Read at exactly one call site — internal/link's feed step — and that
-- confinement is the whole safety argument. It suppresses the third-party
-- reputation feed for a host the owner already decided about, which is what
-- makes a feed verdict owner-overridable without 01500 growing the allow column
-- it deliberately does not have.
--
-- It cannot widen anything else. The three tiers above the feed have all
-- returned by the time this runs, and M31 refuses to file a dispute about any
-- refusal but a low-confidence one, so no row here can carry an unappealable or
-- embedded-tier reason code to be read as permission.
--
-- Equality rather than the blocklist's candidate walk: allowing 'evil.example'
-- says nothing about 'login.evil.example', and 01700's partial index matches
-- this predicate exactly.
SELECT EXISTS (
    SELECT 1 FROM destination_disputes
     WHERE host = lower(sqlc.arg(host)::text)
       AND status = 'allowed'
);

-- name: DecideDestinationDispute :one
-- Records a decision, and only on a dispute nobody has decided yet.
--
-- The `status = 'open'` predicate is the concurrency control: two owners
-- clicking allow and uphold on the same row produce one decision and one
-- no-rows, rather than a last-writer-wins that leaves the audit record and the
-- blocklist disagreeing about what happened.
UPDATE destination_disputes
   SET status = @status,
       decided_by = @decided_by,
       decided_by_label = @decided_by_label,
       decided_at = now()
 WHERE id = @id AND status = 'open'
RETURNING *;

-- name: DeleteBlockedDestination :execrows
-- Removes one host from the low-confidence runtime list.
--
-- The only deletion in this program that is not a reconciliation, and the only
-- one an `allow` decision performs. Scoped to an exact host — the row that was
-- matched, which the caller has already read — so a decision about
-- 'login.evil.example' cannot take 'evil.example' off the list by accident.
--
-- It cannot reach the other two tiers, and there is nothing to scope against
-- them: the embedded list is a compiled file and the unappealable tier has no
-- row anywhere. That is the structural half of "decisions act only on the
-- runtime low-confidence list".
DELETE FROM blocked_destinations WHERE host = lower(sqlc.arg(host)::text);
