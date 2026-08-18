-- Instance-level settings: the answers an operator gives about the box rather
-- than about a tenant in it (M55). One row, guaranteed by 04300's primary key,
-- so every statement here is written against `id` and returns or touches
-- exactly one.
--
-- **The column is nullable and NULL means unanswered** (D164). Three statements
-- rather than two because that third state has to be readable — the prompt an
-- upgraded instance gets at its first administrative sign-in is drawn from
-- exactly one fact, *has anybody answered this yet*, and there is nowhere else
-- to read it from. A `GetInstanceSettings` returning the whole row was written,
-- generated unused into the Querier interface, and removed: nothing renders
-- these settings, and a read of everything is the shape a settings API grows out
-- of before anything has asked for one.
--
-- Two statements write the answer, and the difference between them is which
-- state they are allowed to leave. Setup may rewrite, because nothing has been
-- committed to until the instance is claimed; the principal answers once.

-- name: SetUpdateCheckEnabled :execrows
-- Record the operator's answer to the first-run prompt, at setup (D149).
--
-- **Unconditional, which is deliberate and is the difference from
-- AnswerUpdateCheck below.** The instance is unclaimed — `SetUpdateCheckAtSetup`
-- has just counted the users to be sure of it — so an answer already sitting in
-- the row is a previous setup attempt whose `Register` failed, and the operator
-- retrying with the box unticked must be able to replace a yes with a no. A
-- conditional write here would make the first attempt's answer the permanent one.
--
-- The row count is the check: the row is inserted by migration 04300, so zero
-- means the settings row is missing and the answer went nowhere. The setup path
-- reads it rather than assuming, because the failure it guards against is an
-- operator declining the update check and being checked on anyway.
UPDATE instance_settings
   SET update_check_enabled = @enabled::boolean,
       updated_at = now()
 WHERE id;

-- name: AnswerUpdateCheck :execrows
-- Record the answer given at the first administrative sign-in after an upgrade
-- (D164).
--
-- **Conditional on the question still being open**, so this is a first answer
-- and never a change of one. Two things fall out of that and both are the point:
-- two browser tabs racing produce one answer and one no-op rather than
-- last-write-wins, and the route cannot become the instance-settings page D161
-- refused to build — there is no second answer to give through it.
--
-- Row count 1 means this caller's answer is the one that landed. Zero means the
-- question was already answered, by them or by somebody else holding
-- `instance.admin`, and the caller's own read has already established that the
-- row exists.
UPDATE instance_settings
   SET update_check_enabled = @enabled::boolean,
       updated_at = now()
 WHERE id
   AND update_check_enabled IS NULL;

-- name: UpdateCheckAnswered :one
-- Has anybody answered the update-check question on this instance?
--
-- The whole of what the prompt needs, and deliberately not the value: whether
-- the check is *on* is decided inside ClaimUpdateCheck, and a second reader of
-- that fact is a second place for it to be got wrong. This answers only *is the
-- question still open*, which is what decides whether an administrator is asked.
SELECT (update_check_enabled IS NOT NULL)::boolean AS answered
  FROM instance_settings WHERE id;

-- name: ClaimUpdateCheck :execrows
-- Take the day's update check, if it is available to take.
--
-- **The daily bound is this statement, not a ticker.** One UPDATE decides
-- whether the check may run and records that it did, so the bound is a property
-- of the instance rather than of one process's uptime: a replica restarted
-- every ten minutes reads a row that says the check already happened and
-- declines, where a bare timer would ask GitHub on every boot.
--
-- It writes the timestamp *before* the request rather than after it, which is
-- what makes a failure cost one attempt instead of one per tick. The milestone
-- forbids a retry storm and this is where that is enforced; a check that fails
-- waits out the same day a check that succeeded does.
--
-- **`IS TRUE` rather than a bare test, because the column has three states**
-- (D164). A bare `AND update_check_enabled` would already decline on NULL —
-- unknown is not true — so the behaviour is the same and the spelling is not:
-- *off while unanswered* is a decision this statement enforces, and a reader
-- should not have to recover it from SQL's three-valued logic to be sure it was
-- meant.
--
-- Row count 1 means the caller holds the check. Zero means the operator turned
-- it off, or has not been asked yet, or somebody has already run it today, and
-- the caller does not need to know which — all three are "do nothing".
UPDATE instance_settings
   SET update_checked_at = @at::timestamptz,
       updated_at = now()
 WHERE id
   AND update_check_enabled IS TRUE
   AND (update_checked_at IS NULL OR update_checked_at <= @not_since::timestamptz);
