-- Blocked-attempt disputes (M31).
--
-- Read and written on the management path only. Like the blocklist it argues
-- with, nothing here is reachable from the redirect tree: a dispute is about
-- what may be *stored*, and a link that was refused at creation never became a
-- row for a visitor to resolve.

-- name: InsertDestinationDispute :one
-- Files one dispute.
--
-- Two unique partial indexes decide whether this is a duplicate, and both do it
-- in the database rather than in a check-then-insert two requests can both pass.
-- 01600's is on (host) WHERE status = 'open' — one open dispute per host as
-- typed. 03300's is on (blocked_host) WHERE status = 'open' AND blocked_host
-- <> '' — one open dispute per *blocklist row*, so a caller cannot put the same
-- decision in front of the owner once per subdomain of it.
--
-- @blocked_host is the row the refusal matched, which is routinely a parent of
-- @host. Empty when the rule is computed from the URL rather than held on the
-- list, and the second index skips those: every one of them would carry the same
-- key, and one open homograph dispute must not lock out every other.
INSERT INTO destination_disputes (
    id, host, blocked_host, url_defanged, reason_code,
    organization_id, workspace_id, created_by, created_by_label
) VALUES (
    @id, lower(sqlc.arg(host)::text), lower(sqlc.arg(blocked_host)::text),
    @url_defanged, @reason_code,
    @organization_id, @workspace_id, @created_by, @created_by_label
)
RETURNING *;

-- name: GetDestinationDispute :one
SELECT d.*, b.source AS entry_source
  FROM destination_disputes d
  LEFT JOIN blocked_destinations b ON b.host = d.blocked_host
 WHERE d.id = @id;

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
--
-- The LEFT JOIN carries the blocklist entry's **source**, which is what decides
-- whether an allow can do anything (F42). `liftableRules` says the *rule* is
-- list-backed; it does not say the entry behind this particular refusal is one
-- a decision may delete. An `env`-sourced entry comes from
-- LINKCTRL_DESTINATION_BLOCKLIST and is rewritten at every boot, so removing it
-- would be undone by the next restart and `entryToLift` refuses — while the page
-- drew the Allow button from the rule alone and the operator found out by
-- clicking. LEFT, because a refusal computed from the URL has no entry at all
-- and must stay in the queue.
SELECT d.*, b.source AS entry_source
  FROM destination_disputes d
  LEFT JOIN blocked_destinations b ON b.host = d.blocked_host
 WHERE (NOT @open_only::boolean OR d.status = 'open')
   AND (
        sqlc.narg('cursor_created')::timestamptz IS NULL
     OR (d.created_at, d.id) < (sqlc.narg('cursor_created')::timestamptz, sqlc.narg('cursor_id')::uuid)
   )
 ORDER BY d.created_at DESC, d.id DESC
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

-- name: GetBlockedDestination :one
-- Reads one entry by its exact host.
--
-- The decision path's read, and deliberately not MatchBlockedDestination: that
-- one walks a host and every parent of it and answers with the longest match,
-- which is the right question when *judging* a destination and the wrong one
-- when acting on a dispute. The dispute already names the row it is about —
-- destination_disputes.blocked_host, written when it was filed — so the only
-- thing left to ask is whether that row is still there and who owns it (03300).
--
-- No rows means the entry has gone since the dispute was filed, which the caller
-- reports rather than papering over: an allow that deleted nothing must not be
-- recorded as one that did.
SELECT host, source, reason FROM blocked_destinations
 WHERE host = lower(sqlc.arg(host)::text);

-- name: DeleteBlockedDestination :execrows
-- Removes one host from the low-confidence runtime list.
--
-- The only deletion in this program that is not a reconciliation, and the only
-- one an `allow` decision performs. Scoped to an exact host — the row the
-- dispute recorded when it was filed and the queue displayed on the button — so
-- a decision about 'login.evil.example' cannot take 'evil.example' off the list
-- by accident. It can take it off deliberately, and routinely does: 'evil.example'
-- is what refused 'login.evil.example', and lifting anything else would leave the
-- destination refused. What 03300 changed is that the owner is now told which of
-- the two they are deciding about, and that the answer cannot move between the
-- filing and the click.
--
-- It cannot reach the other two tiers, and there is nothing to scope against
-- them: the embedded list is a compiled file and the unappealable tier has no
-- row anywhere. That is the structural half of "decisions act only on the
-- runtime low-confidence list".
DELETE FROM blocked_destinations WHERE host = lower(sqlc.arg(host)::text);
