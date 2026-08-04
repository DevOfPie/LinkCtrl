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
| `linkctrl_job_last_success_timestamp_seconds{job}` | When each job last succeeded, **as this replica remembers it**. Absent on a replica that has not run the job, and cleared by a restart. |
| `linkctrl_rollup_staleness_seconds{job}` | Seconds since each job last succeeded, read from `job_state` and therefore the same on every replica and unaffected by a restart. **This is the one to alert on.** A job that has never succeeded has no series at all. |
| `linkctrl_build_info{version,commit,go}` | Always 1; the labels are the point. |
| `linkctrl_destination_feed_checks_total{result}` | Third-party reputation checks: `clean`, `malicious`, `error`, `skipped`. **Absent entirely unless `LINKCTRL_FEED_URL` is set**, which makes the series itself the answer to "is this instance sending destinations anywhere". |
| `linkctrl_webhook_deliveries_total{outcome,status}` | Outbound webhook deliveries. `outcome` is `delivered`, `retry` or `abandoned`; `status` is the HTTP class — `2xx`, `3xx`, `4xx`, `5xx` — or **`none`** when there was no response at all: a refused connection, a timeout, or this instance declining to open the socket because the name resolved to a private address. Deliberately not labelled by URL: registrations are chosen by users, so a label per endpoint is a series count anybody with a workspace could grow. |
| `linkctrl_automation_firings_total{trigger,outcome}` | Automation rules that fired (M43). `trigger` is one of the three names in the closed vocabulary; `outcome` is `fired`, or `partial` when at least one action failed. Counted once per **firing**, not per subject and not per evaluation — a rule that matched forty links did one thing, and a counter that ticked for every run would measure the scheduler rather than the automation. Deliberately not labelled by rule: rules are named by users, so a label per rule is a series count anybody with a workspace could grow. Which rule fired is in that workspace's audit log, as `automation.fired`. |

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
| Redirect errors | `rate(linkctrl_redirects_total{outcome="error"}[5m]) > 0` | Resolution is failing; visitors get `503 Retry-After: 1` for links that exist. Usually the redirect pool or Postgres. |
| Redirect pool starved | `linkctrl_db_pool_acquire_waits_total{pool="redirect"}` increasing | The split pool is not absorbing load; the hot path is queueing. |
| Rollup stalled | `linkctrl_rollup_staleness_seconds{job="analytics_rollup"} > 600` | The dashboard's headline numbers are going stale. The job runs every 60s, so ten minutes is ten missed runs — comfortably past a transient failure and well short of a working instance. |
| Breakdowns stalled | `linkctrl_rollup_staleness_seconds{job="analytics_dimension_rollup"} > 3600` | The device, browser, country and per-destination breakdowns are going stale, including the choropleth. The threshold is **four missed runs, not ten**: this job is allowed to be fifteen minutes behind by design, so a ten-minute alert would fire on a healthy instance and a one-hour one is the first figure that cannot. Raise it if you lengthen `analytics.DimensionInterval`; leaving it while shortening the interval only makes the alert slower. |
| A job that has stopped reporting at all | `absent(linkctrl_rollup_staleness_seconds{job="analytics_dimension_rollup"})` | The two above cannot fire on a series that does not exist, and a job that has never succeeded has no series — which is what a fresh instance looks like for its first few seconds and what a permanently broken one looks like forever. Give it a `for: 15m` so the first case does not page you. |
| Job stalled, per replica | `time() - linkctrl_job_last_success_timestamp_seconds{job="rollup"} > 600` | The process-local view. Useful for "did *this* replica ever hold leadership", useless as a staleness alert: it is absent on every follower and cleared by a restart, so on more than one replica the answer depends on which one was scraped. |
| Job erroring | `rate(linkctrl_job_runs_total{result="error"}[15m]) > 0` | |
| Limiter stopped limiting | `rate(linkctrl_rate_limit_overflow_total[15m]) > 0` | The key table filled, so requests are being allowed uncounted. The design fails open deliberately — a limiter must not become an outage — which is exactly why this needs an alert rather than a log line. |
| Credential limit stopped being shared | `rate(linkctrl_rate_limited_total{limit="login"}[5m])` unchanged while Redis is unhealthy, plus the log line below | The limit fell back to per-replica buckets, so N replicas now allow N times it. Correct by design — it never refuses because Redis is unwell — but the effective limit is looser until Redis returns. |
| Redirects being throttled | `rate(linkctrl_rate_limited_total{limit="redirect_404"}[5m]) > 1` | Either someone is scanning for aliases, or `TRUSTED_PROXIES` is wrong and every visitor shares one bucket. Check which before tuning the limit. |
| Webhooks being abandoned | `rate(linkctrl_webhook_deliveries_total{outcome="abandoned"}[1h]) > 0` | A workspace's receiver has been failing for over an hour and events are being dropped. Nothing recovers an abandoned delivery. Which webhook it was is in that workspace's own delivery log, at `/webhooks`, rather than in a metric label. |
| Webhooks reaching nothing | `rate(linkctrl_webhook_deliveries_total{status="none"}[15m]) > 0` | No response at all. Usually somebody's endpoint is down; if it is *every* webhook at once, suspect this instance's egress or DNS rather than the receivers. |
| Automation actions failing | `rate(linkctrl_automation_firings_total{outcome="partial"}[1h]) > 0` | A rule fired and at least one of its actions did not complete. The firing itself is not retried — the watermark has already moved — so the subjects it covered are not seen again. Which rule, and what it was trying to do, is in that workspace's audit log at `automation.fired`. |
| Reputation feed not answering | `rate(linkctrl_destination_feed_checks_total{result="error"}[15m]) > 0` | Only if you configured one. A feed check fails **open**: the destination is accepted and the built-in tiers decide, so the product behaves exactly as it does with no feed at all. That is deliberate — a third party's outage must not stop you making links — and it is also why a feed that silently stopped working is invisible anywhere but here. |
| 5xx on any surface | `rate(linkctrl_http_requests_total{status="5xx"}[5m]) > 0` | |
| Rows in a default partition | see [below](#partitions) | Silent data misrouting; next month's partition will fail to attach. |

One caveat on local numbers: **on a Windows host, latency measures as exactly
zero** because Go's monotonic clock there cannot resolve intervals this short.
Bucket counts stay correct; `_sum` and averages do not. Linux is unaffected. See
[development.md](build-notes/development.md#latency-measured-on-a-windows-host-is-not-a-latency-measurement).

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
| `rollup` | 60s | Recomputes the per-link and per-workspace daily totals from raw events and upserts. Whole days, never incremental — an "add what arrived since the watermark" design double-counts on retry and, once it drifts, stays wrong invisibly. |
| `dimension-rollup` | 15m | The same recompute for the per-dimension and per-destination breakdowns, on a longer clock because it costs far more: its upsert count is bounded by the distinct `(link, day, dimension, value)` tuples the day's clicks imply, where the totals are bounded by the links that were clicked. Measured at the SLO dataset it is 4.8–6.3s against a totals pass of ~1.5s ([slo.md](slo.md#re-measured-for-m37-2026-08-03)). **The visible consequence is that a breakdown on a link's page can be up to fifteen minutes behind the totals above it**, which is what the staleness alert below is for. Its own row in `job_state`, so a totals run cannot advance a watermark the breakdowns have not reached. |
| `mail` | 30s | Drains `mail_outbox`. Does not run at all with no mailer configured, because nothing is ever queued then. Five attempts per message, backing off 1m to 16m; a message that never gets through is marked `failed` and kept with the relay's error. Faster than the hourly jobs because an invitation is something a person is waiting for. See [configuration.md](configuration.md#mail). |
| `partitions` | 1h | Creates monthly partitions two months ahead. |
| `retention` | 1h | Drops monthly partitions that are entirely outside their table's window — `ANALYTICS_RETENTION_DAYS` for `click_events` and `visitors`, `AUDIT_RETENTION_DAYS` for `audit_logs`. Runs after `partitions`, so a run can never drop what it just created. With the audit default of `0` it drops no audit partition at all. |
| `salt-purge` | 1h | Deletes analytics salts older than two days. **This is the de-identification step, not housekeeping**: once a salt is gone, that day's visitor hashes cannot be linked to an address by anyone. |
| `audit-growth-warning` | 1h | Notifies every organization owner once `audit_logs` passes `AUDIT_SIZE_WARN_BYTES`, at most weekly each. Skipped entirely when the threshold is `0`. |
| `partition-check` | 1h | Warns if rows landed in a default partition. |
| `domain-verification` | 1h | Re-reads every registered hostname's DNS TXT challenge. A hostname that starts passing begins being served without anybody pressing anything; one that stops passing keeps serving for `DOMAIN_VERIFY_GRACE` — a day by default — with the owning workspace notified at the **first** failure, and then stops being served on every replica. **This is the only job whose failure takes something offline**, which is why the window is long and why the warning comes early; the runbook is in [deployment.md](deployment.md#custom-domains). A pass that changed anything logs `custom domain verification` with the counts. **Serving hostnames are checked before registered-but-unserved ones**, so the stop above can only ever be delayed by other serving hostnames — never by a pile of registrations, which anybody can create and whose place in the queue a rename resets. A domain renamed while its check was in flight logs `a domain changed while its verification was in flight` and verifies nothing; one line is an owner renaming at an unlucky moment, a stream of them is somebody trying to have a check of one name land on another. `DOMAIN_VERIFY_INTERVAL=0` switches it off, which also removes the only thing that would ever stop serving a hostname whose record has gone. |
| `housekeeping` | 1h | The reapers. Hard-deletes links whose 30-day trash window has passed — reserving any alias that ever received traffic in the same statement, so it is never reissued — and deletes sessions, revoked API keys and finished outbox rows past their retention. Each purged link is logged by alias; that log line is the only record of the deletion. |

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

The `retention` job runs hourly and drops whole monthly partitions. Dropping a
partition is instant and reclaims the space, which is the whole reason these
tables are partitioned this way — a `DELETE` across the largest table in the
system followed by a `VACUUM` is the alternative, on a schedule, forever.

Each table answers to its own window:

| Table | Window | Default |
| --- | --- | --- |
| `click_events`, `visitors` | `ANALYTICS_RETENTION_DAYS` | 395 days |
| `audit_logs` | `AUDIT_RETENTION_DAYS` | `0` — **keep forever** |

Two windows rather than one because the right default differs. Analytics is
high-volume and its value decays; an audit trail is read precisely because
somebody is asking about something that happened a long time ago. Deleting the
second on the first's setting would be a surprise of the wrong kind, so
`audit_logs` had no policy at all until it had one of its own.

Four properties follow from doing it by the month:

- **Data survives up to a month past the window.** A partition goes only once its
  newest possible row is already outside the window, so nothing inside it is ever
  deleted early. Keeping data slightly too long is recoverable; deleting it early
  is not.
- **Rollups survive.** They live in their own unpartitioned tables, so charts keep
  working after the raw events are gone.
- **A window of `0` drops nothing**, and so does a table with no configured
  window at all. Retention deletes only where it was told a number.
- **Only partitions this software created** are considered — the `_YYYY_MM` naming
  is the test. A table you attached by hand is left alone.

### Audit log growth

`AUDIT_RETENTION_DAYS` defaults to `0`, so an instance nobody configures keeps
every audit record forever. That is the safe default only for as long as the
growth it permits is visible, which is what this series is for:

```
linkctrl_audit_log_bytes
```

On-disk bytes across every `audit_logs` partition, indexes included. Refreshed
by the hourly maintenance pass on **every** replica, not only the job leader, so
it does not matter which one a scrape reaches.

Two alerts, because size and growth fail differently. The first matches the
5 GB threshold the in-app notification uses, so Prometheus and the dashboard do
not disagree about when this is a problem:

```yaml
- alert: AuditLogLarge
  # The same 5 GB the in-app owner notification fires at.
  expr: linkctrl_audit_log_bytes > 5e9
  for: 1h
  labels: { severity: warning }
  annotations:
    summary: "audit_logs has passed 5 GB"
    description: >-
      Currently {{ $value | humanize1024 }}B. Set
      LINKCTRL_AUDIT_RETENTION_DAYS, or confirm the disk is sized for it.

- alert: AuditLogGrowthUnbounded
  # Projects a fortnight ahead from the last week, so an instance heading for
  # trouble is flagged before it arrives rather than after.
  expr: predict_linear(linkctrl_audit_log_bytes[7d], 14 * 86400) > 5e9
  for: 6h
  labels: { severity: warning }
  annotations:
    summary: "audit_logs is on track to pass 5 GB within a fortnight"
    description: >-
      Projected {{ $value | humanize1024 }}B. Growth this steady usually
      means retention was never configured.
```

Tune `5e9` to the volume you actually have — the number that matters is your
own. An instance with a year of history and no growth is fine at any size; one
doubling monthly is not fine at any size.

The owner-facing notification for the same threshold is **on by default** rather
than opt-in, and is emailed as well once a [mailer](configuration.md#mail) is
configured. The metric and this alert recipe stay the primary mechanism: they
reach whoever watches Prometheus, where the notification reaches whoever signs
in.

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
| An edit did not take effect | Invalidation is broadcast over Redis pub/sub, so check Redis first: with it down, each replica falls back to `REDIRECT_TTL` staleness. The subscriber logs `lost its connection` or `went silent and redis did not answer a probe`, then `reconnected` — a replica showing either of the first two without the third is not hearing invalidations. |
| Analytics stopped updating | The `rollup` job. Check `linkctrl_job_last_success_timestamp_seconds{job="rollup"}` and the logs for `job failed`. |
| A custom domain stopped resolving | Its DNS challenge has been failing for the whole grace window. The domain's row on the workspace's Domains page names what the last check found; `audit_logs` holds `domain.unverified` with the time. Republish the TXT record and re-check from the page — serving resumes at once. |
| Clicks missing entirely | `linkctrl_analytics_events_dropped_total` climbing, or an unclean shutdown lost a batch. `click_count` on a link is approximate for the same reason. |
| Everything looks like one visitor | `TRUSTED_PROXIES` wrong, so every request appears to come from the proxy. Visitor hashing includes the address. |
| Visitors getting 429 on links that exist | Same cause, now with teeth: with one shared address, everyone's 404s come out of one bucket. A throttled address can still follow links already in the in-process cache, so the symptom is partial and looks random. Fix `TRUSTED_PROXIES`; raising the limit only moves the threshold. |
| `429` right after a deploy | The credential and API limits live in Redis and survive a restart. The 404-probe limiter is per instance and in memory, so that one resets. Buckets refill continuously rather than on a window boundary, so a client is never waiting for a reset. |
| Dashboard unstyled | Image built without `make css`. The server warns at boot. Rebuild. |
| `/docs` renders as plain text | Its CSP relaxes `style-src` only; a proxy overriding `Content-Security-Policy` breaks it. Stop overriding it — LinkCtrl sets its own security headers. |
| API keys all rejected after a config change | `API_KEY_PEPPER` changed. Every hash is keyed with it. Restore the old value or reissue every key. |
| Login always fails, no obvious reason | A CRLF in `.env` gave Postgres or a secret a trailing carriage return. Also check whether the account is locked: five failures triggers a 15-minute lockout, and **the response does not say so** — every sign-in refusal is the same 401, deliberately, because a distinct answer for a lockout tells a stranger which addresses are registered. `SELECT email, locked_until FROM users WHERE locked_until > now();` is how you find out, and `UPDATE users SET locked_until = NULL WHERE email_lower = '…';` is how you lift one early. |
| Cannot claim a fresh instance | `/setup` is single-use and returns 404 once a user exists. Invite the person instead, or set `SIGNUP_MODE` and restart. |
| `/signup` answers 403 with `SIGNUP_MODE=open` | No `LINKCTRL_SMTP_HOST`. Public registration confirms the address by email before the account exists, so with no relay the effective mode is `invite`. The boot log says so; set a relay and restart. |

## What is not here yet

Deliberate gaps, so they are not discovered during an incident:

- **Region and city are never stored**, even with a GeoIP database configured.
  Country only, deliberately.
- **Invalidation needs Redis to cross replicas.** With Redis down, each replica
  falls back to `REDIRECT_TTL` staleness — correct, just slower to converge.
  See [below](#cache-invalidation-across-replicas).
- **The dimension rollup is expensive and gets worse.** It recomputes whole days
  every 60 seconds; at 5.7M click events that measured 16–21 seconds per run, and
  it will eventually exceed its own interval. Redirects are unaffected — the
  measured SLO held throughout — but dashboards go stale when it falls behind.
  Watch `linkctrl_job_last_success_timestamp_seconds{job="rollup"}`. Details and
  the `EXPLAIN` output: [slo.md](slo.md).

The redirect SLO itself is measured and met: [slo.md](slo.md).

## Cache invalidation across replicas

Each replica keeps its own in-process cache in front of Redis. Editing a link
clears the editing replica's copy and deletes the Redis entry, but the other
replicas hold copies only they can reach — so invalidations are broadcast on a
Redis pub/sub channel and every replica clears its own tiers when it hears one.

The subscriber runs in its own goroutine and never touches the request path. It
only ever deletes from the in-process tiers, which is why invalidation traffic
does not show up in the redirect latency.

**With Redis down, this degrades rather than breaks.** Redirects still resolve,
from Postgres. Edits still apply, and still clear the replica that made them.
What is lost is the broadcast, so other replicas serve their cached copy until
it expires — the behaviour every deployment had before this existed. Nothing
errors, and nothing is served incorrectly for longer than `REDIRECT_TTL`.

The interesting case is a subscriber that stops hearing. Redis pub/sub does not
replay, so invalidations published while a replica was not listening are gone,
and that replica cannot know which keys they named. **It flushes both in-process
tiers the moment it stops trusting the subscription, and again when it
reconnects**, which ends the stale window at the failure instead of at each
entry's TTL. The cost is a cold cache for a moment after a Redis blip — latency
on an optional dependency, rather than serving a destination the owner already
changed.

A connection that *dies* announces itself: the read fails, and the subscriber
handles it. A connection that stalls — held open by a wedged Redis, a proxy or a
sidecar, with bytes going nowhere — announces nothing, and silence is also what
a channel nobody has published on looks like. So the subscriber bounds its read
with `REDIS_SUBSCRIBER_READ_TIMEOUT` (30s) and, when one expires, pings and
waits for the *reply* rather than assuming either answer. A stalled connection
cannot produce the reply, which is what separates the two. At most two of those
intervals pass before a stalled replica stops serving what it can no longer
vouch for.

Three log lines matter:

| Line | Means |
| --- | --- |
| `cache invalidation subscriber lost its connection` | This replica is not hearing invalidations. Expect `REDIRECT_TTL` staleness until it recovers. |
| `cache invalidation subscriber went silent and redis did not answer a probe` | The same, for a Redis that is holding the connection open and not answering. The in-process caches have been dropped rather than served as current. |
| `cache invalidation subscriber reconnected; in-process caches flushed` | Recovered, and it distrusted everything it held. Normal after a Redis restart. |
| `rate limiting fell back to per-instance buckets` | The credential or API limit is no longer shared across replicas. Logged once when it starts, not per request. |

A replica logging either of the first two without the third is the one to look
at: it is serving from a cache nothing is invalidating, from Postgres rather
than from memory, until Redis answers again.

Full list: [Plan.md](../Plan.md#phase-1-scope-not-yet-built).
