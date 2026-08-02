-- +goose Up
--
-- Routing rules wake up (M34).
--
-- The table itself was created dormant in 00600, with the shape it needed to
-- have: a link, a workspace, a destination, a priority, a jsonb bag of
-- conditions, a kind and an enabled flag. Nothing here changes any of that,
-- which is the point of having created it then — this migration adds only what
-- becomes true the moment something reads and writes the rows.
--
-- Three additions, and each of them is a rule the application would otherwise
-- have to be trusted to keep.

-- A match rule without a destination is not a rule; it is a condition with
-- nowhere to send anybody.
--
-- Written as a CHECK rather than as NOT NULL on the column, because the other
-- three kinds the column's own CHECK permits — weighted, sequential, fallback —
-- are M36's, and at least one of them will legitimately have no single
-- destination of its own. Constraining the kind this milestone actually ships
-- leaves that door open without leaving this one unlocked.
--
-- Safe to add unvalidated-free: the table is dormant on every deployment that
-- exists, so there are no rows for it to reject.
ALTER TABLE routing_rules
    ADD CONSTRAINT routing_rules_match_needs_destination
    CHECK (kind <> 'match' OR destination_id IS NOT NULL);

-- The management list reads every rule, including the disabled ones.
--
-- 00600's `routing_rules_link_idx` is partial on `enabled`, which is exactly
-- right for the redirect path and useless for the dashboard: a rule somebody
-- switched off still has to appear in the list they switched it off from. Two
-- indexes rather than one unpartitioned index, because the hot path's is the
-- one that must stay small — it is probed on every uncached resolve, for every
-- link, including the overwhelming majority that have no rules at all.
--
-- created_at is in the key because priority is not unique. Two rules at the same
-- priority have to evaluate in a defined order or the same request gets
-- different destinations on different replicas, and creation order is the only
-- tiebreak a person can predict.
CREATE INDEX routing_rules_link_all_idx
    ON routing_rules (link_id, priority, created_at);

-- Rule targets are ordinary destination rows, which is what the dormant
-- `destination_id` foreign key already said. What was not true until now is that
-- a link can have more than one.
--
-- Phase 1 gave every link exactly one destination at position 0, and
-- `links.primary_destination_id` points at it. That stays true: a rule's target
-- is created at a position above zero and is never the primary, so the trigger
-- that mirrors the primary destination's URL into `links.primary_url` — scoped
-- to `primary_destination_id` since 00300 — cannot be moved by one. The index
-- below is what makes "this link's rule targets" a cheap read.
CREATE INDEX destinations_link_position_idx
    ON destinations (link_id, position) WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS destinations_link_position_idx;
DROP INDEX IF EXISTS routing_rules_link_all_idx;
ALTER TABLE routing_rules DROP CONSTRAINT IF EXISTS routing_rules_match_needs_destination;
