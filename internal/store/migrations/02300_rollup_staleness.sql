-- +goose Up
--
-- The dimension rollup gets its own clock, and staleness gets a number (M37).
--
-- M37 splits `analytics_rollup` into two jobs. The per-link and per-workspace
-- totals keep their sixty-second cadence; the per-dimension breakdowns move to a
-- longer one, because at the SLO dataset the dimension pass costs 16-21 seconds
-- of a sixty-second interval and the cost is the ~553k upserts a whole-day
-- recompute implies rather than the scan. Two jobs means two rows in job_state,
-- and no DDL is needed for that — `job` is a text primary key.
--
-- What does need DDL is the staleness metric. Splitting the cadence makes
-- "how far behind are the breakdowns?" a question an operator has to be able to
-- answer, and `last_run_at` cannot answer it: RecordJobFailure stamps
-- last_run_at on a run that failed, so a job failing every tick would report
-- itself perpetually fresh. `last_success_at` is only ever written by the
-- watermark advance, which happens after the rollup returns without error.
--
-- Backfilled from last_run_at for rows whose last run did not fail. That is not
-- a guess: SetJobWatermark clears last_error, so `last_error IS NULL` on an
-- existing row means the last thing that touched it was a success at
-- last_run_at. Rows whose last run failed are left NULL, which reads as "has
-- never been observed to succeed" — the honest answer, and the one the alert
-- recipe in docs/operations.md treats as stale.
ALTER TABLE job_state ADD COLUMN last_success_at timestamptz;

UPDATE job_state SET last_success_at = last_run_at WHERE last_error IS NULL;

-- +goose Down
ALTER TABLE job_state DROP COLUMN IF EXISTS last_success_at;
