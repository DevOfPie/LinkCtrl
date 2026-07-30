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
-- One statement per dimension via UNION ALL, rather than eight round trips.
INSERT INTO link_dimension_daily (link_id, workspace_id, day, dimension, value, clicks, unique_visitors)
SELECT link_id, workspace_id, day, dimension, value,
       count(*)                     AS clicks,
       count(DISTINCT visitor_hash) AS unique_visitors
FROM (
    SELECT ce.link_id, ce.workspace_id, (ce.occurred_at AT TIME ZONE 'UTC')::date AS day,
           'device' AS dimension, coalesce(ce.device, 'unknown') AS value, ce.visitor_hash
      FROM click_events ce
     WHERE ce.occurred_at >= sqlc.arg(window_start) AND ce.occurred_at < sqlc.arg(window_end) AND NOT ce.is_bot
    UNION ALL
    SELECT ce.link_id, ce.workspace_id, (ce.occurred_at AT TIME ZONE 'UTC')::date,
           'browser', coalesce(ce.browser, 'Other'), ce.visitor_hash
      FROM click_events ce
     WHERE ce.occurred_at >= sqlc.arg(window_start) AND ce.occurred_at < sqlc.arg(window_end) AND NOT ce.is_bot
    UNION ALL
    SELECT ce.link_id, ce.workspace_id, (ce.occurred_at AT TIME ZONE 'UTC')::date,
           'os', coalesce(ce.os, 'Other'), ce.visitor_hash
      FROM click_events ce
     WHERE ce.occurred_at >= sqlc.arg(window_start) AND ce.occurred_at < sqlc.arg(window_end) AND NOT ce.is_bot
    UNION ALL
    SELECT ce.link_id, ce.workspace_id, (ce.occurred_at AT TIME ZONE 'UTC')::date,
           'country', coalesce(ce.country, 'unknown'), ce.visitor_hash
      FROM click_events ce
     WHERE ce.occurred_at >= sqlc.arg(window_start) AND ce.occurred_at < sqlc.arg(window_end) AND NOT ce.is_bot
    UNION ALL
    SELECT ce.link_id, ce.workspace_id, (ce.occurred_at AT TIME ZONE 'UTC')::date,
           'referrer', coalesce(nullif(ce.referrer_host, ''), 'direct'), ce.visitor_hash
      FROM click_events ce
     WHERE ce.occurred_at >= sqlc.arg(window_start) AND ce.occurred_at < sqlc.arg(window_end) AND NOT ce.is_bot
    UNION ALL
    SELECT ce.link_id, ce.workspace_id, (ce.occurred_at AT TIME ZONE 'UTC')::date,
           'language', coalesce(nullif(ce.language, ''), 'unknown'), ce.visitor_hash
      FROM click_events ce
     WHERE ce.occurred_at >= sqlc.arg(window_start) AND ce.occurred_at < sqlc.arg(window_end) AND NOT ce.is_bot
) d
GROUP BY link_id, workspace_id, day, dimension, value
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
