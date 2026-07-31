-- Analytics: salts, rollups and reads.

-- name: GetSalt :one
SELECT salt FROM analytics_salts WHERE valid_on = $1;

-- name: CreateSalt :one
-- ON CONFLICT DO NOTHING returns no row when another replica inserted first,
-- which the caller detects and re-reads. Two replicas using different salts
-- for the same day would split every visitor in two.
INSERT INTO analytics_salts (valid_on, salt, purge_at)
VALUES ($1, $2, $3)
ON CONFLICT (valid_on) DO NOTHING
RETURNING salt;

-- name: PurgeExpiredSalts :execrows
-- The de-identification step. Once the salt is gone the day's hashes cannot be
-- linked back to an address.
DELETE FROM analytics_salts WHERE purge_at < now();

-- name: RollupLinkDaily :exec
-- Recompute per-link daily totals for a window.
--
-- Idempotent by construction: it recomputes a whole day from the raw events
-- and upserts, so running it twice, or after a crash mid-run, converges to the
-- same numbers. An incremental "add what is new" design would double-count on
-- any retry.
INSERT INTO link_click_daily (link_id, workspace_id, day, clicks, unique_visitors, bot_clicks)
SELECT
    ce.link_id,
    ce.workspace_id,
    (ce.occurred_at AT TIME ZONE 'UTC')::date AS day,
    count(*) FILTER (WHERE NOT ce.is_bot)                        AS clicks,
    count(DISTINCT ce.visitor_hash) FILTER (WHERE NOT ce.is_bot) AS unique_visitors,
    count(*) FILTER (WHERE ce.is_bot)                            AS bot_clicks
FROM click_events ce
WHERE ce.occurred_at >= sqlc.arg(window_start)
  AND ce.occurred_at <  sqlc.arg(window_end)
GROUP BY ce.link_id, ce.workspace_id, 3
ON CONFLICT (link_id, day) DO UPDATE
   SET clicks = EXCLUDED.clicks,
       unique_visitors = EXCLUDED.unique_visitors,
       bot_clicks = EXCLUDED.bot_clicks;

-- name: RollupWorkspaceDaily :exec
INSERT INTO workspace_click_daily (workspace_id, day, clicks, unique_visitors, bot_clicks, active_links)
SELECT
    ce.workspace_id,
    (ce.occurred_at AT TIME ZONE 'UTC')::date,
    count(*) FILTER (WHERE NOT ce.is_bot),
    count(DISTINCT ce.visitor_hash) FILTER (WHERE NOT ce.is_bot),
    count(*) FILTER (WHERE ce.is_bot),
    count(DISTINCT ce.link_id)
FROM click_events ce
WHERE ce.occurred_at >= sqlc.arg(window_start)
  AND ce.occurred_at <  sqlc.arg(window_end)
GROUP BY ce.workspace_id, 2
ON CONFLICT (workspace_id, day) DO UPDATE
   SET clicks = EXCLUDED.clicks,
       unique_visitors = EXCLUDED.unique_visitors,
       bot_clicks = EXCLUDED.bot_clicks,
       active_links = EXCLUDED.active_links;

-- name: RollupDimensionDaily :exec
-- Every dimension in one pass over click_events.
--
-- This was six UNION ALL branches, one per dimension, reading the same rows six
-- times. Measured on the load-test dataset (5.7M events, ~830k inside the
-- recomputed window), that shape sorted 6.2M rows through an external merge that
-- spilled 471 MB of temp files, every 60 seconds. Reading once and expanding each
-- row with LATERAL VALUES lets the sort use the index's link_id ordering, so it
-- runs incrementally in memory instead — peak 152 kB per group, no temp files.
--
-- Wall clock is unchanged (~20s either way), and that is the finding rather than a
-- disappointment: the time is in the 553k upserts a whole-day recompute implies,
-- not in reading the events. See docs/slo.md. This version is kept because
-- eliminating half a gigabyte of temp I/O per run is worth having on any host
-- smaller than the one it was measured on; it is not a fix for the job's cost.
--
-- The output is identical: same grouping keys, same aggregates, same conflict
-- resolution. TestDimensionRollupMatchesAPerDimensionAggregate checks that
-- against a per-dimension aggregate written the other way round.
INSERT INTO link_dimension_daily (link_id, workspace_id, day, dimension, value, clicks, unique_visitors)
SELECT ce.link_id,
       ce.workspace_id,
       (ce.occurred_at AT TIME ZONE 'UTC')::date AS day,
       d.dimension,
       d.value,
       count(*)                        AS clicks,
       count(DISTINCT ce.visitor_hash) AS unique_visitors
  FROM click_events ce
 CROSS JOIN LATERAL (VALUES
        ('device',   coalesce(ce.device, 'unknown')),
        ('browser',  coalesce(ce.browser, 'Other')),
        ('os',       coalesce(ce.os, 'Other')),
        ('country',  coalesce(ce.country, 'unknown')),
        ('referrer', coalesce(nullif(ce.referrer_host, ''), 'direct')),
        ('language', coalesce(nullif(ce.language, ''), 'unknown'))
       ) AS d(dimension, value)
 WHERE ce.occurred_at >= sqlc.arg(window_start)
   AND ce.occurred_at <  sqlc.arg(window_end)
   AND NOT ce.is_bot
 GROUP BY ce.link_id, ce.workspace_id, day, d.dimension, d.value
ON CONFLICT (link_id, day, dimension, value) DO UPDATE
   SET clicks = EXCLUDED.clicks,
       unique_visitors = EXCLUDED.unique_visitors;

-- name: GetLinkStats :many
-- Reads the rollup, never the raw events. This is what keeps analytics under
-- the 2s target as click_events grows into the tens of millions.
SELECT day, clicks, unique_visitors, bot_clicks
FROM link_click_daily
WHERE link_id = $1
  AND day >= sqlc.arg(from_day)::date
  AND day <= sqlc.arg(to_day)::date
ORDER BY day;

-- name: GetLinkDimensions :many
SELECT value, sum(clicks)::bigint AS clicks, sum(unique_visitors)::bigint AS unique_visitors
FROM link_dimension_daily
WHERE link_id = $1
  AND dimension = $2
  AND day >= sqlc.arg(from_day)::date
  AND day <= sqlc.arg(to_day)::date
GROUP BY value
ORDER BY clicks DESC, value
LIMIT sqlc.arg(row_limit);

-- name: GetWorkspaceStats :many
SELECT day, clicks, unique_visitors, bot_clicks, active_links
FROM workspace_click_daily
WHERE workspace_id = $1
  AND day >= sqlc.arg(from_day)::date
  AND day <= sqlc.arg(to_day)::date
ORDER BY day;

-- name: GetWorkspaceTotals :one
-- Summing daily uniques over-counts anyone visiting on more than one day.
-- Reported as "unique visitors per day, summed" in the UI rather than
-- presented as a distinct-person count, because the exact figure cannot be
-- recovered once the salts are purged. That is the intended trade.
SELECT
    coalesce(sum(clicks), 0)::bigint          AS clicks,
    coalesce(sum(unique_visitors), 0)::bigint AS unique_visitors,
    coalesce(sum(bot_clicks), 0)::bigint      AS bot_clicks
FROM workspace_click_daily
WHERE workspace_id = $1
  AND day >= sqlc.arg(from_day)::date
  AND day <= sqlc.arg(to_day)::date;

-- name: GetRecentClicks :many
-- The live-activity feed. Bounded and index-backed on (link_id, occurred_at).
SELECT occurred_at, device, browser, os, country, referrer_host, is_bot
FROM click_events
WHERE link_id = $1
ORDER BY occurred_at DESC
LIMIT sqlc.arg(row_limit);

-- name: CountClickEvents :one
SELECT count(*) FROM click_events WHERE workspace_id = $1;

-- --- job bookkeeping ---------------------------------------------------------

-- name: GetJobWatermark :one
-- The point a job is known to have completed through. Rollups recompute rather
-- than accumulate, so this is not a correctness dependency for a run that
-- happens on schedule — it exists for the run that does not. Without it,
-- RunRecent covered a fixed yesterday-and-today window, and any downtime that
-- spanned a UTC day left that day with no rollup and nothing to notice it: the
-- raw events were still there, but nothing ever aggregated them again.
SELECT watermark FROM job_state WHERE job = $1;

-- name: SetJobWatermark :exec
INSERT INTO job_state (job, last_run_at, watermark, last_error, updated_at)
VALUES (sqlc.arg(job), now(), sqlc.arg(watermark), NULL, now())
ON CONFLICT (job) DO UPDATE
   SET last_run_at = now(),
       watermark   = EXCLUDED.watermark,
       last_error  = NULL,
       updated_at  = now();

-- name: RecordJobFailure :exec
-- Keeps the watermark where it was: a failed run has not covered its window,
-- and advancing past it would turn one bad run into permanent gaps.
INSERT INTO job_state (job, last_run_at, last_error, updated_at)
VALUES (sqlc.arg(job), now(), sqlc.arg(last_error), now())
ON CONFLICT (job) DO UPDATE
   SET last_run_at = now(),
       last_error  = EXCLUDED.last_error,
       updated_at  = now();
