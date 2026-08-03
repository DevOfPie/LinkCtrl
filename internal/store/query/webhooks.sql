-- Webhooks and their delivery queue (M42).
--
-- Two halves that never meet in one statement. The registration half is
-- workspace-scoped and reached from the dashboard and the API; the delivery half
-- is scoped to nothing but time and is reached only by the scheduler. Every
-- query below belongs to exactly one of them, and the delivery half deliberately
-- carries no workspace parameter — a drainer that filtered by tenant would
-- deliver in tenant order, and the queue is fair or it is a queue somebody's
-- backlog can starve.

-- name: CreateWebhook :one
INSERT INTO webhooks (id, workspace_id, url, secret, events, description, enabled)
VALUES (@id, @workspace_id, @url, @secret, @events, @description, @enabled)
RETURNING id, workspace_id, url, events, description, enabled, created_at, updated_at;

-- name: ListWebhooks :many
--
-- The secret is not selected. It is written once and read only by the signer, so
-- nothing that renders a page or answers the API can leak it by accident.
SELECT id, workspace_id, url, events, description, enabled, created_at, updated_at
  FROM webhooks
 WHERE workspace_id = @workspace_id
 ORDER BY created_at, id;

-- name: GetWebhook :one
SELECT id, workspace_id, url, events, description, enabled, created_at, updated_at
  FROM webhooks
 WHERE id = @id AND workspace_id = @workspace_id;

-- name: CountWebhooks :one
SELECT count(*) FROM webhooks WHERE workspace_id = @workspace_id;

-- name: UpdateWebhook :one
--
-- Partial: a NULL parameter leaves its column alone. Same shape as every other
-- update in this schema, so "absent" and "empty" stay different — an empty
-- events array is a real request to subscribe to nothing.
UPDATE webhooks
   SET url         = coalesce(sqlc.narg(url), url),
       events      = coalesce(sqlc.narg(events)::text[], events),
       description = coalesce(sqlc.narg(description), description),
       enabled     = coalesce(sqlc.narg(enabled), enabled),
       updated_at  = now()
 WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id)
RETURNING id, workspace_id, url, events, description, enabled, created_at, updated_at;

-- name: RotateWebhookSecret :exec
UPDATE webhooks
   SET secret = @secret, updated_at = now()
 WHERE id = @id AND workspace_id = @workspace_id;

-- name: DeleteWebhook :execrows
--
-- The deliveries go with it, by the ON DELETE CASCADE 00600 declared. A webhook
-- that has been removed and a delivery log that still names it would be a record
-- of where events *used to* go, which is a different and less useful thing than
-- the record of where they go.
DELETE FROM webhooks WHERE id = @id AND workspace_id = @workspace_id;

-- --- the queue ---------------------------------------------------------------

-- name: EnqueueWebhookDeliveries :execrows
--
-- One statement fans one event out to every webhook that asked for it.
--
-- This runs inside a link write, so its cost on a workspace with no webhooks has
-- to be one indexed lookup that returns nothing — which is what the partial index
-- `webhooks_workspace_idx ... WHERE enabled` (00600) makes it.
--
-- The payload is rendered by the caller and stored rendered, for the reason the
-- mail outbox stores its body rendered (D23): a change to what an event looks
-- like must not rewrite an event that was already queued, and a row has to stay
-- readable after the code that produced it is gone.
--
-- gen_random_uuid() rather than a v7 generated in Go, because the number of rows
-- is not known until the SELECT runs. Nothing orders deliveries by id — the queue
-- orders by next_attempt_at — so the time-sortability v7 buys is not spent here.
INSERT INTO webhook_deliveries (id, webhook_id, event, payload)
SELECT gen_random_uuid(), w.id, sqlc.arg(event), sqlc.arg(payload)
  FROM webhooks w
 WHERE w.workspace_id = sqlc.arg(workspace_id)
   AND w.enabled
   AND sqlc.arg(event) = ANY(w.events);

-- name: ClaimDueWebhookDeliveries :many
--
-- One batch of due deliveries, claimed rather than merely selected — the exact
-- shape ClaimDueMail uses, and for the same two reasons:
--
--   * The UPDATE spends the attempt and leases the row forward before anything
--     is sent, so a process killed mid-delivery leaves a row that comes back on
--     its own instead of one stuck pending.
--   * A crash loop is bounded. Counting the attempt at send time would let a
--     process that dies mid-send retry the same delivery forever.
--
-- FOR UPDATE SKIP LOCKED inside the subquery is the claim mechanism this
-- milestone had to choose (see decisions.md). Leadership already keeps a second
-- replica out of the job, but leadership is an advisory lock released when its
-- holder dies, so a moment of overlap is possible; skip-locked makes that moment
-- cost nothing rather than deliver the same event twice.
--
-- The webhook's URL and secret are joined in here rather than fetched per row:
-- the drainer needs both for every claimed delivery, and N+1 round trips to
-- assemble a batch of network calls is the wrong shape.
UPDATE webhook_deliveries d
   SET attempts = d.attempts + 1,
       next_attempt_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::int)
  FROM webhooks w
 WHERE w.id = d.webhook_id
   AND d.id IN (
       SELECT id FROM webhook_deliveries
        WHERE status = 'pending'
          AND next_attempt_at <= now()
        ORDER BY next_attempt_at
        LIMIT sqlc.arg(batch_size)
          FOR UPDATE SKIP LOCKED
 )
RETURNING d.id, d.webhook_id, d.event, d.payload, d.attempts, w.url, w.secret;

-- name: MarkWebhookDelivered :exec
--
-- attempts is not touched: the claim already spent it.
UPDATE webhook_deliveries
   SET status = 'delivered', response_code = @response_code,
       next_attempt_at = NULL, completed_at = now()
 WHERE id = @id;

-- name: MarkWebhookRetry :exec
--
-- A failure that will be tried again. Replaces the lease the claim set with the
-- real backoff for this attempt, and does not touch attempts.
--
-- response_code is nullable and stays NULL when there was no response at all,
-- which is what tells a refused connection apart from a receiver answering 500.
UPDATE webhook_deliveries
   SET next_attempt_at = now() + make_interval(secs => sqlc.arg(backoff_seconds)::int),
       response_code = sqlc.narg(response_code),
       last_error = sqlc.arg(last_error)
 WHERE id = sqlc.arg(id);

-- name: MarkWebhookAbandoned :exec
--
-- Retry exhausted. Terminal, and deliberately not deleted: a row saying what was
-- attempted, how many times, and what the receiver said is the whole reason this
-- is a table rather than an in-memory retry loop.
UPDATE webhook_deliveries
   SET status = 'abandoned', response_code = sqlc.narg(response_code),
       last_error = sqlc.arg(last_error), next_attempt_at = NULL,
       completed_at = now()
 WHERE id = sqlc.arg(id);

-- name: ListWebhookDeliveries :many
--
-- One webhook's recent attempts, newest first, for the panel and the API. Bounded
-- by the caller: this is a log, and a page that renders all of it renders a log.
SELECT d.id, d.event, d.status, d.attempts, d.response_code, d.last_error,
       d.next_attempt_at, d.created_at, d.completed_at
  FROM webhook_deliveries d
  JOIN webhooks w ON w.id = d.webhook_id
 WHERE d.webhook_id = @webhook_id AND w.workspace_id = @workspace_id
 ORDER BY d.created_at DESC, d.id DESC
 LIMIT sqlc.arg(row_limit);

-- name: CountPendingWebhookDeliveries :one
SELECT count(*) FROM webhook_deliveries WHERE status = 'pending';

-- name: PurgeFinishedWebhookDeliveries :execrows
--
-- Delivered and abandoned rows past the retention window. The delivery log is a
-- record of what was attempted, not an archive; without this it is a table that
-- grows forever with one row per link write per webhook, which is the shape D5
-- and M21 exist to stop repeating.
DELETE FROM webhook_deliveries
 WHERE status <> 'pending'
   AND created_at < now() - make_interval(days => sqlc.arg(max_age_days)::int);
