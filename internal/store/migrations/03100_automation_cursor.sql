-- +goose Up
--
-- The scheduler gets a cursor of its own (M43, reopened).
--
-- 02900 ordered the due-rule query by `last_fired_at` and said that doing so
-- meant *"the rules a capped run skipped are the ones with the oldest watermarks
-- next time"*. That is false in the one direction that matters, and this
-- migration is the correction.
--
-- `last_fired_at` moves only when a rule **fires**: the evaluator returns before
-- the claim whenever the match set is below the rule's threshold, which on the
-- default threshold of one is every run that matched nothing. Idle is precisely
-- what keeps a watermark old, so the hundred oldest-watermark enabled rules are a
-- fixed set and the hundred-and-first enabled rule on an instance was never
-- evaluated on any run, ever — enabled on its page, `last_fired_at` never moving,
-- no log line naming it. A hundred rules is five workspaces at the documented
-- per-workspace cap of twenty. Recorded as F83.
--
-- One column had been carrying two facts: *when did this rule last fire* and
-- *when was this rule last looked at*. They are separated here, and the ordering
-- follows the second.

ALTER TABLE automation_rules
    -- When the scheduler last looked at this rule, whether or not it fired.
    --
    -- The ordering fact and nothing else: no match window is bounded by it. Every
    -- trigger still reads `(last_fired_at, now]`, because advancing the watermark
    -- on a no-match run — the one-line alternative to this column — discards the
    -- subjects already inside the window, and `min_count` stops working the day
    -- that changes.
    --
    -- NULL means "never looked at" and sorts first, so a rule created a moment
    -- ago is evaluated on the next tick rather than waiting for a rotation. A
    -- rule that has been disabled for a month keeps its old value and therefore
    -- goes early when it is switched back on, which is the right direction for a
    -- column that answers "who has waited longest".
    ADD COLUMN last_checked_at timestamptz;

-- The due-rule index follows the ordering onto the new column.
--
-- Same name and same shape as 02900's, replaced rather than added beside it: the
-- old ordering has no reader left, and an index nothing walks is write cost on
-- every rule the scheduler touches. Existing rows all carry NULL, which the
-- partial index stores and `NULLS FIRST` reads, so the first run after this
-- migration takes the hundred lowest ids and rotation begins from there.
DROP INDEX IF EXISTS automation_rules_due_idx;
CREATE INDEX automation_rules_due_idx
    ON automation_rules (last_checked_at NULLS FIRST) WHERE enabled;

-- +goose Down
DROP INDEX IF EXISTS automation_rules_due_idx;
CREATE INDEX automation_rules_due_idx
    ON automation_rules (last_fired_at NULLS FIRST) WHERE enabled;
ALTER TABLE automation_rules DROP COLUMN IF EXISTS last_checked_at;
