-- The low-confidence destination blocklist (M30).
--
-- Read on the management path only. The redirect tree never touches this table:
-- blocking decides what may be stored, not what may be served, and putting a
-- query here on the hot path would buy nothing a link that was refused at
-- creation does not already have.

-- name: MatchBlockedDestination :one
-- Whether any of a host's label-boundary suffixes is on the list.
--
-- The caller passes the full host and every parent of it — a.b.example becomes
-- {a.b.example, b.example, example} — so the label-boundary rule is enforced by
-- what is asked for rather than by a pattern match, and the whole question is
-- one index probe. Longest first in the caller, and ORDER BY length here, so a
-- specific entry wins over the parent it sits under and the reason an operator
-- reads is the one they wrote for that host.
--
-- The source comes back because it decides which rule the refusal reports: a
-- seeded shortener says shortener_chain and everything else says
-- operator_blocklist. One row rather than one query per source — a host that is
-- both listed by the operator and a known shortener is one refusal, and the
-- more specific entry is the longer one, which this already returns.
SELECT host, source, reason FROM blocked_destinations
 WHERE host = ANY(sqlc.arg(candidates)::text[])
 ORDER BY length(host) DESC
 LIMIT 1;

-- name: UpsertEnvBlockedDestination :exec
-- Writes one entry from LINKCTRL_DESTINATION_BLOCKLIST at boot.
--
-- ON CONFLICT DO UPDATE rather than DO NOTHING: an operator who moves a host
-- into their environment expects the environment to own it from then on, and a
-- row left claiming it came from a review would send M31 looking for a review
-- that never happened. created_at is left alone, because the entry is the same
-- entry it was before the restart.
INSERT INTO blocked_destinations (host, source, reason)
VALUES (lower(sqlc.arg(host)::text), 'env', sqlc.arg(reason)::text)
ON CONFLICT (host) DO UPDATE
   SET source = 'env', reason = EXCLUDED.reason;

-- name: DeleteStaleEnvBlockedDestinations :execrows
-- Retires environment entries the operator has since removed.
--
-- Scoped to source = 'env' and nothing else. A restart must never delete what an
-- owner decided in the review queue, nor the shortener hosts seeded by
-- migration, which is the one way a boot-time reconciliation could quietly undo
-- a decision somebody made.
DELETE FROM blocked_destinations
 WHERE source = 'env'
   AND NOT (host = ANY(sqlc.arg(keep)::text[]));
