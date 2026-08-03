-- +goose Up
--
-- Split testing wakes up (M36).
--
-- The three remaining rule kinds — weighted, sequential, fallback — have been
-- permitted by `routing_rules.kind`'s CHECK since 00600 and refused by every
-- query since 02000. Nothing here creates a table for them, because a variant is
-- a rule row pointing at a `destinations` row, exactly as a match rule's target
-- is: same two writes, same M30 tier check, same invalidation. What this
-- migration adds is what becomes true the moment a variant is chosen rather than
-- matched — a rotation counter, a place to record which destination a click went
-- to, and the constraint that says every kind now needs somewhere to send
-- somebody.
--
-- `destinations.weight` is deliberately absent from this file. It has existed
-- since 00300, NOT NULL DEFAULT 100 CHECK (weight >= 0), with a comment naming
-- Phase 2 weighted routing as the reason. M36 is that reason arriving; the
-- decision to keep weights on the destination rather than on the rule is
-- recorded in decisions.md and needs no DDL.

-- Every kind needs a destination, not only 'match'.
--
-- 02000 constrained the one kind it shipped and said why: "at least one of
-- [weighted, sequential, fallback] will legitimately have no single destination
-- of its own". That turned out to be wrong, and this is where it is corrected
-- rather than left as a door nothing walks through. A weighted variant is one
-- destination with a weight; a sequential variant is one destination with a
-- position in the rotation; a fallback is one destination used when nothing else
-- applies. There is no kind that is a group, because the *link* is the group.
--
-- The weaker constraint is dropped rather than kept alongside: it is implied by
-- this one in every row it could ever reject, and a redundant CHECK is a second
-- statement of the same rule that a later edit can make disagree with the first.
ALTER TABLE routing_rules DROP CONSTRAINT routing_rules_match_needs_destination;
ALTER TABLE routing_rules
    ADD CONSTRAINT routing_rules_needs_destination
    CHECK (destination_id IS NOT NULL);

-- The sequential rotation counter (D8: strict global order).
--
-- On `link_click_budget` because that table is already the answer to "a durable
-- counter the redirect path consumes transactionally, which Redis cannot hold" —
-- 02100 says so, and says this milestone would reuse it. Reuse is the table and
-- the mechanism.
--
-- It is **not** the `consumed` column, and the separation is the whole reason
-- this column exists. `consumed` is a budget: M35's ConsumeClickBudget refuses to
-- go past the link's limit and the caller answers 410. A rotation is not spent,
-- it only advances. Sharing one column would mean a one-time link carrying a
-- sequential split destroyed itself on its first visit — the rotation would take
-- the single click the gate was holding, and the gate would then find nothing
-- left. Two numbers that mean different things get two columns.
--
-- Monotonic and never reset, like `consumed`. The variant is chosen by position
-- modulo the number of enabled variants, so adding or removing one re-phases the
-- rotation rather than restarting it; that is the honest behaviour, because
-- there is no correct answer to "who was next" when the list changes underneath.
ALTER TABLE link_click_budget
    ADD COLUMN rotation bigint NOT NULL DEFAULT 0 CHECK (rotation >= 0);

-- Which destination a click was sent to.
--
-- Additive and nullable, and NULL is load-bearing rather than a gap: it means
-- the link's own destination — the one `links.primary_destination_id` points at
-- and `links.primary_url` mirrors. Every click recorded before this column
-- existed went there, so backfilling would be writing an id nothing measured,
-- and every click on a link with no rules still goes there, so a default would
-- be a per-row copy of a fact the link already carries.
--
-- **No foreign key.** click_events is the highest-write table in the system and
-- is partitioned; a reference would put a lookup on every COPY row and would
-- make deleting a rule's destination take a lock against the whole click
-- history. A destination that is deleted leaves rows pointing at an id that no
-- longer resolves, and the breakdown reads those as "a destination that no
-- longer exists" rather than losing the clicks — which is the correct thing to
-- show somebody who has just removed a variant from a running test.
ALTER TABLE click_events ADD COLUMN destination_id uuid;

-- The breakdown's rollup reads this and nothing else reads it.
--
-- Partial, because on any instance that runs no split test every row has a NULL
-- here and the index is empty: the per-destination rollup pass then costs an
-- index scan over nothing instead of a second sequential pass over the window.
-- Created on the partitioned parent, so every existing partition gets it and
-- every partition `internal/store/partitions.go` creates later inherits it.
CREATE INDEX click_events_destination_idx
    ON click_events (link_id, occurred_at)
    WHERE destination_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS click_events_destination_idx;
ALTER TABLE click_events DROP COLUMN IF EXISTS destination_id;
ALTER TABLE link_click_budget DROP COLUMN IF EXISTS rotation;
ALTER TABLE routing_rules DROP CONSTRAINT IF EXISTS routing_rules_needs_destination;
ALTER TABLE routing_rules
    ADD CONSTRAINT routing_rules_match_needs_destination
    CHECK (kind <> 'match' OR destination_id IS NOT NULL);
