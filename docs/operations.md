# Operations

What to watch, what to alert on, and what to do when something is wrong.

## Health endpoints

Both live on the public listener and perform no session lookup, so they answer
even when the database is down.

| Endpoint | Meaning |
| --- | --- |
| `GET /healthz` | The process is alive. Use it for liveness. |
| `GET /readyz` | Dependencies are usable. Use it for load-balancer readiness. |

`/readyz` returns JSON and distinguishes degradation from failure:

```json
{
  "status": "ok",
  "version": "1.0.0",
  "dependencies": { "postgres": "ok", "redis": "ok" }
}
```

`status` is `ok`, `degraded` or `unavailable`. **Redis being down is `degraded`,
not `unavailable`** — the redirect path falls through to Postgres and still
works, so reporting unready would pull a functioning instance out of rotation
over a cache problem. Postgres being down is `unavailable`.

During shutdown, `/readyz` fails *first*, then the drain delay elapses, then the
listener closes. That ordering is what makes a rolling restart lossless; skipping
it is how clients see connection resets during every deploy.

## Metrics

Prometheus format at `GET /metrics` on `METRICS_ADDR` (`:9090`), a **separate
listener with no authentication**. Compose does not publish it. Do not proxy it.

From a Prometheus on the same compose network:

```yaml
scrape_configs:
  - job_name: linkctrl
    static_configs:
      - targets: ["app:9090"]
```

### The series that matter

| Metric | Use |
| --- | --- |
| `linkctrl_redirect_duration_seconds{outcome,cache}` | The SLO histogram. `cache` is `memory`, `redis`, `database`, `negative` or `rejected`. |
| `linkctrl_redirects_total{outcome,cache}` | Traffic and cache hit ratio. |
| `linkctrl_http_requests_total{surface,method,status}` | `surface` is `redirect`, `api`, `web`, `static` or `ops`. |
| `linkctrl_http_request_duration_seconds{surface,method}` | Outside view, including all middleware. |
| `linkctrl_analytics_queue_depth` | Leading indicator for the click pipeline. |
| `linkctrl_analytics_events_dropped_total` | Clicks discarded to protect redirect latency. |
| `linkctrl_rate_limited_total{limit}` | Requests refused by a limit: `login`, `api` or `redirect_404`. |
| `linkctrl_rate_limit_tracked_keys{limit}` | Client keys each limiter is holding. |
| `linkctrl_rate_limit_overflow_total{limit}` | Requests allowed **without being limited** because the key table was full. |
| `linkctrl_db_pool_*{pool="app"\|"redirect"}` | Saturation, per pool. |
| `linkctrl_job_runs_total{job,result}` | `ok`, `error` or `skipped`. |
| `linkctrl_job_last_success_timestamp_seconds{job}` | Staleness of each job. |
| `linkctrl_build_info{version,commit,go}` | Always 1; the labels are the point. |

Plus the standard `go_*` and `process_*` collectors.

### Queries worth saving

Cached-redirect p99, which is how the <20ms target is stated:

```promql
histogram_quantile(0.99,
  sum by (le) (rate(linkctrl_redirect_duration_seconds_bucket{cache=~"memory|redis"}[5m])))
```

Fraction of cached redirects inside the target — a ratio of bucket counts, which
is why the histogram has a boundary at exactly `0.02`:

```promql
  sum(rate(linkctrl_redirect_duration_seconds_bucket{cache=~"memory|redis",le="0.02"}[5m]))
/ sum(rate(linkctrl_redirect_duration_seconds_count{cache=~"memory|redis"}[5m]))
```

Cache hit ratio:

```promql
  sum(rate(linkctrl_redirects_total{cache=~"memory|redis"}[5m]))
/ sum(rate(linkctrl_redirects_total[5m]))
```

Redirect pool queueing — nonzero means the hot path is waiting for a connection:

```promql
rate(linkctrl_db_pool_acquire_waits_total{pool="redirect"}[5m]) > 0
```

### Alerts worth having

| Alert | Condition | Why |
| --- | --- | --- |
| Clicks being dropped | `rate(linkctrl_analytics_events_dropped_total[5m]) > 0` | Analytics is losing data to protect latency. Bounded queue working as designed — but you want to know. |
| Queue climbing | `linkctrl_analytics_queue_depth > 8000` for 5m | The database is falling behind. Fires minutes before drops start. |
| Redirect errors | `rate(linkctrl_redirects_total{outcome="error"}[5m]) > 0` | Resolution is failing; visitors get 404s for links that exist. |
| Redirect pool starved | `linkctrl_db_pool_acquire_waits_total{pool="redirect"}` increasing | The split pool is not absorbing load; the hot path is queueing. |
| Job stalled | `time() - linkctrl_job_last_success_timestamp_seconds{job="rollup"} > 600` | Dashboards are going stale. |
| Job erroring | `rate(linkctrl_job_runs_total{result="error"}[15m]) > 0` | |
| Limiter stopped limiting | `rate(linkctrl_rate_limit_overflow_total[15m]) > 0` | The key table filled, so requests are being allowed uncounted. The design fails open deliberately — a limiter must not become an outage — which is exactly why this needs an alert rather than a log line. |
| Redirects being throttled | `rate(linkctrl_rate_limited_total{limit="redirect_404"}[5m]) > 1` | Either someone is scanning for aliases, or `TRUSTED_PROXIES` is wrong and every visitor shares one bucket. Check which before tuning the limit. |
| 5xx on any surface | `rate(linkctrl_http_requests_total{status="5xx"}[5m]) > 0` | |
| Rows in a default partition | see [below](#partitions) | Silent data misrouting; next month's partition will fail to attach. |

One caveat on local numbers: **on a Windows host, latency measures as exactly
zero** because Go's monotonic clock there cannot resolve intervals this short.
Bucket counts stay correct; `_sum` and averages do not. Linux is unaffected. See
[development.md](claude/development.md#latency-measured-on-a-windows-host-is-not-a-latency-measurement).

## Logs

Structured `log/slog`, JSON by default (`LOG_FORMAT=text` for humans). Every
record carries the service name and version.

Successful redirects are **not** logged by default — at 2,000 rps that produces
more bytes than the redirects themselves. Set `REDIRECT_LOG_SAMPLE=1000` to log
one in a thousand while investigating.

Errors log the real cause and return nothing about it: an unrecognised error
becomes a flat 500 to the client, because internal error strings carry table
names, query fragments and connection strings. If a client reports a 500, the
answer is in the log, not in their response body.

Secrets cannot be logged. Configuration secrets are a type that refuses to print
itself through `fmt`, `slog` or `json`, so a config dump or a formatted panic
cannot leak the database password or the API-key pepper.

## Background jobs

An in-process scheduler, leader-elected by a Postgres advisory lock. Every job
re-checks the lock, so leadership moves between runs without coordination, and a
crashed holder releases it automatically. On a multi-instance deployment most
runs are `skipped` — that is healthy, and a follower reporting no skips at all is
a follower whose scheduler has stopped.

| Job | Every | Does |
| --- | --- | --- |
| `rollup` | 60s | Recomputes recent days from raw events and upserts. Whole days, never incremental — an "add what arrived since the watermark" design double-counts on retry and, once it drifts, stays wrong invisibly. |
| `partitions` | 1h | Creates monthly partitions two months ahead. |
| `retention` | 1h | Drops monthly partitions of `click_events` and `visitors` that are entirely outside `ANALYTICS_RETENTION_DAYS`. Runs after `partitions`, so a run can never drop what it just created. |
| `salt-purge` | 1h | Deletes analytics salts older than two days. **This is the de-identification step, not housekeeping**: once a salt is gone, that day's visitor hashes cannot be linked to an address by anyone. |
| `partition-check` | 1h | Warns if rows landed in a default partition. |

All of them run once at startup rather than waiting a full interval, so a fresh
instance has current numbers.

## Partitions

`click_events`, `visitors` and `audit_logs` are RANGE-partitioned by month,
created by application code. Rows in a *default* partition mean something arrived
outside every explicit range — and attaching the partition that should have held
them will then fail.

```sh
docker compose exec -T postgres psql -U linkctrl -d linkctrl -c \
  "SELECT count(*) FROM click_events_default;"
```

The `partition-check` job logs a warning for this hourly. If it fires, the usual
cause is a non-UTC session at DDL time: partition bounds on `timestamptz` resolve
against the session timezone, so a non-UTC session creates bounds offset by the
UTC offset and leaves a gap. UTC is pinned in the pool, the server and the
container, and startup fails if a session is not UTC — so this should be
impossible, and worth investigating rather than patching around.

### Retention

`ANALYTICS_RETENTION_DAYS` (default 395) is enforced hourly by the `retention`
job, which drops whole monthly partitions of `click_events` and `visitors`.
Dropping a partition is instant and reclaims the space, which is the whole reason
these tables are partitioned this way — a `DELETE` across the largest table in the
system followed by a `VACUUM` is the alternative, on a schedule, forever.

Four properties follow from doing it by the month:

- **Data survives up to a month past the window.** A partition goes only once its
  newest possible row is already outside the window, so nothing inside it is ever
  deleted early. Keeping data slightly too long is recoverable; deleting it early
  is not.
- **Rollups survive.** They live in their own unpartitioned tables, so charts keep
  working after the raw events are gone.
- **`audit_logs` is exempt** even though it is partitioned identically. Audit
  retention is a different policy from analytics retention, and deleting an audit
  trail on the analytics setting would be a surprise of the wrong kind.
- **Only partitions this software created** are considered — the `_YYYY_MM` naming
  is the test. A table you attached by hand is left alone.

Each drop is logged at info with the partition name. That log line is the only
record that irreversible deletion happened, so keep it.

To drop a month early, do it yourself; the job will not object:

```sh
docker compose exec -T postgres psql -U linkctrl -d linkctrl -c \
  "DROP TABLE click_events_2025_06;"
```

If the job reports errors, the usual cause is the five-second lock timeout it
sets: detaching a partition needs a brief exclusive lock on the parent, and the
click ingester holds one on every batch. Failing is cheap — it runs again in an
hour — and the timeout exists so a housekeeping job can never stall the write
path while it waits.

## Troubleshooting

| Symptom | Likely cause and fix |
| --- | --- |
| Redirects 404 for a link that exists | Cached negative entry, or the link is archived/expired. Check the link's status; negative caching is bounded by `REDIRECT_NEGATIVE_TTL` (60s). |
| An edit did not take effect | More than one app replica. Cache invalidation is single-replica in Phase 1; others wait out `REDIRECT_TTL`. Run one instance. |
| Analytics stopped updating | The `rollup` job. Check `linkctrl_job_last_success_timestamp_seconds{job="rollup"}` and the logs for `job failed`. |
| Clicks missing entirely | `linkctrl_analytics_events_dropped_total` climbing, or an unclean shutdown lost a batch. `click_count` on a link is approximate for the same reason. |
| Everything looks like one visitor | `TRUSTED_PROXIES` wrong, so every request appears to come from the proxy. Visitor hashing includes the address. |
| Visitors getting 429 on links that exist | Same cause, now with teeth: with one shared address, everyone's 404s come out of one bucket. A throttled address can still follow links already in the in-process cache, so the symptom is partial and looks random. Fix `TRUSTED_PROXIES`; raising the limit only moves the threshold. |
| `429` right after a deploy | Rate limits are per instance and in memory, so a restart resets every bucket. Buckets refill continuously rather than on a window boundary, so a client is never waiting for a reset. |
| Dashboard unstyled | Image built without `make css`. The server warns at boot. Rebuild. |
| `/docs` renders as plain text | Its CSP relaxes `style-src` only; a proxy overriding `Content-Security-Policy` breaks it. Stop overriding it — LinkCtrl sets its own security headers. |
| API keys all rejected after a config change | `API_KEY_PEPPER` changed. Every hash is keyed with it. Restore the old value or reissue every key. |
| Login always fails, no obvious reason | A CRLF in `.env` gave Postgres or a secret a trailing carriage return. Also check whether the account is locked — five failures triggers a 15-minute lockout. |
| Cannot claim a fresh instance | `/setup` is single-use and returns 404 once a user exists. Create further users with `SIGNUP_MODE=open` (temporarily) or directly in the database. |

## What is not here yet

Deliberate gaps, so they are not discovered during an incident:

- **No audit log behaviour.** The table exists and stays empty. Phase 2.
- **Region and city are never stored**, even with a GeoIP database configured.
  Country only, deliberately.
- **The redirect SLO is unverified.** The histogram is in place; the load test
  that turns the target into a measured number has not been run. It is the next
  milestone, and it was sequenced after throttling so it measures the path that
  actually ships.
- **Cache invalidation is single-replica.** Run one app instance until Phase 2
  adds pub/sub.

Full list: [Plan.md](../Plan.md#phase-1-scope-not-yet-built).
