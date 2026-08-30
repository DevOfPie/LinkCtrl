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

### The load-balancer contract

Since 0.3.0 this is a promise rather than an observation. Each clause has a test
behind it, in `internal/httpx/health_test.go` and
`test/integration/failover_test.go`, so it can be shown false rather than merely
believed.

| Probe | Wire it to | Because |
| --- | --- | --- |
| Liveness — *restart this container* | `GET /healthz` | It touches **neither Postgres nor Redis**, ever. A liveness probe that followed the database would fail on every replica during one Postgres blip and the orchestrator would restart the whole deployment at once, turning a recoverable dependency failure into a far worse outage. |
| Readiness — *send it traffic* | `GET /readyz` | It reports what this replica can actually do. |
| Startup | **nothing** | See below. |

Readiness is a rule about the status **code**, not about the word in the body:

- **503 → take the replica out of rotation.** It is `draining` (shutting down,
  finish what it has) or `unavailable` (Postgres unreachable — it can resolve
  nothing).
- **200 → keep it in rotation**, including `degraded`. **A `degraded` replica is
  a serving replica.** `degraded` means Redis is unreachable; redirects fall
  through to Postgres and still meet the uncached target, so the replica works.
  An operator who wires `degraded` to *remove* takes the **entire** deployment
  out of rotation during a Redis outage — which is the exact failure this
  distinction exists to prevent.

Read the word to know how worried to be; act on the code.

**Wired that way, a rolling deploy costs nothing, and that is measured.** Three
replicas behind a balancer polling `/readyz` every second and deregistering
after two failures, every replica destroyed and rebuilt while 2,000 requests a
second went through it: **zero requests failed, zero retried, cached p99 295µs,
the whole replacement in 35 seconds** — 11 to 12 seconds per replica. The run,
the two columns it has, and what the numbers cannot show are in
[slo.md](slo.md#measured-during-a-rolling-deploy-for-m57-2026-08-09). The
balancer's configuration is in `test/ha/haproxy.cfg`, which is the contract
above written out in one vendor's syntax and is worth reading beside it.

**There is no startup probe, and that is a choice rather than an omission.**
Migrations run before the listener binds, so *not yet ready* and *not yet
listening* are the same observable state — a startup probe would be a second
name for a connection refused. Configure your orchestrator's start period
instead, and give it room for migrations on a large `audit_logs`.

### Sizing the drain delay

`LINKCTRL_SHUTDOWN_DRAIN_DELAY` is how long a replica keeps serving *after* its
readiness starts failing. It has to outlast your load balancer noticing:

```
DRAIN_DELAY  >  health-check interval  ×  unhealthy threshold
```

Take the interval and the threshold from your own load balancer, then add one
interval of slack for the check that was already in flight when readiness
flipped. A worked example, for a balancer polling every 5s and deregistering
after 2 consecutive failures: 5 × 2 = 10s of detection, plus 5s of slack, so
**15s** is the smallest defensible value.

No number is recommended here, because the number belongs to your load balancer
and not to this product. In particular the shipped default of **5s is not a
recommendation** — it is what a deployment with no load balancer in front of it
needs, and behind one that polls every 5s it is too short by half. What a delay
that is too short costs is connection resets on every deploy: the listener
closes while the balancer still believes the replica is healthy.

**What a delay that is too short costs, as a number.** The inequality was tested
from both sides on the same three-replica deployment, at 2,000 rps, replacing
every replica. Satisfied — 5s of drain against 1s × 2 of detection — **zero
requests failed and zero were retried**. Removed entirely, by killing each
replica with SIGKILL instead of stopping it, **905 requests of 239,833 were
retried, four drew a response error, the slowest took a full second, and the
generator went from 3 concurrent connections to 212 to hold the rate.** Still
zero failures, and the credit for that belongs to the balancer's `retries` and
`redispatch` rather than to this product: a balancer without them answers 503 to
all 905. The measurement is
[here](slo.md#measured-during-a-rolling-deploy-for-m57-2026-08-09).

`LINKCTRL_SHUTDOWN_TIMEOUT` is the separate budget for finishing in-flight
requests *after* the listener closes, and buffered click events are flushed
after that. **The two are spent in sequence and startup refuses a pair that
exceeds 25 seconds**, because compose's `stop_grace_period` is 30 and a SIGKILL
mid-flush loses the click events the flush exists to save. So raising the drain
delay is a trade against the request budget, not a free knob: the worked example
above leaves 10s for in-flight requests, and a deployment that needs both
numbers larger has to raise `stop_grace_period` first.

## Running more than one replica

Nothing here needs a coordinator, an external lock service, or a second
Postgres. Running one container is unaffected by all of it: with one replica the
only *replica* competing for an advisory lock is that replica. An installed add-on
is the one other session that can hold one — the keys are constants in a public
repository and `pg_advisory_lock` is executable by any role — so the host releases
every advisory lock an add-on's connection holds before that connection is reused,
which bounds the hold to the add-on's own call. `docs/SECURITY.md` states the
residual: one statement, five seconds at most, and a job that finds the lock taken
skips its tick exactly as a follower does.

### What happens when a replica dies

| Work | On a replica that is killed | Bound |
| --- | --- | --- |
| **Scheduled jobs** | A Postgres advisory lock is released the instant the session holding it ends — killed or not. Nothing detects the death; the next follower to tick simply finds the lock free, unless an installed add-on's own statement is holding it at that moment, which is bounded by the paragraph above. | One tick of that family. The fastest family ticks every 60s; the hourly ones every hour. |
| **Webhook deliveries** | Claimed with `FOR UPDATE SKIP LOCKED` under a **60-second lease**, in the same statement that spends the attempt. A replica killed after claiming leaves a row that comes back on its own rather than one stuck pending. | 60s, then another replica delivers it. |
| **Mail outbox** | The same claim, the same lease, the same recovery. | 60s. |
| **Buffered click events** | Lost. The queue is in-process and bounded by design (D77); a graceful shutdown flushes it, a kill does not. | Whatever was in the queue — `linkctrl_analytics_queue_depth` is the number. |

Stated as a single promise: **a replica killed without draining loses at most
its buffered click events, and nothing else.**

Delivery is therefore **at-least-once, not exactly-once**, and the window is
worth naming: a replica killed *after* a webhook request left the socket but
*before* the row was marked delivered sends it again from somewhere else. Every
delivery carries a stable `X-LinkCtrl-Delivery` uuid for exactly this — dedupe
on it in the receiver. Making the click queue durable would mean a work queue in
Redis, which would make Redis required, which is a trade this product has not
made.

### Can two replicas lead the same job at once

Not because of a deploy, and the distinction is the whole answer.

**A rolling deploy does not produce two leaders.** Every binary from 0.2.0 on
asks for the same per-family advisory keys, so the old replica and the new one
contend for one lock and `pg_try_advisory_lock` gives it to exactly one of them.
The only binary that ever behaved otherwise is 0.1.x, which took a single key
for every job — and running more than one replica was not a supported
configuration until 0.3.0, so no upgrade path a deployment is allowed to take
puts one in a rolling deploy. `TestAReleasedFamilyKeepsItsAdvisoryKey` is what
keeps that true: renumbering a family that has already shipped would re-open the
window one family at a time, and it fails when anybody does.

**A leader that loses its database connection while still working does.** The
lock is held on a connection of its own; the pass works on another. A terminated
backend, a dropped network or a pause long enough to break keepalives releases
the lock while the pass keeps running, and the next follower to tick takes it.
Nothing bounds that window, because nothing detects it — which is the same
property that makes failover need no coordinator.

**So the window is real and it costs duplicate effort rather than duplicate
effects.** Every pass is written to survive being run twice: the mail and webhook
drains claim rows with `FOR UPDATE SKIP LOCKED`, the rollups recompute whole
days idempotently, partition creation is `IF NOT EXISTS`, the automation
watermark is compare-and-set, and the daily update check is one `UPDATE` that a
second replica cannot match. **There was one exception until 0.3.0**, and it was
a logging defect rather than a data one: custom-domain re-verification decided
whether to write an audit record from a read taken before its own write, so two
leaders could record one verification twice. It was
[F180](build-notes/deferred-findings.md#closed), **closed at 0.3.0** together
with [F185](build-notes/deferred-findings.md#closed), the same defect at the
manual *Check DNS* button twelve lines away: both now decide from what the write
returned, so the leader that did not move the row records nothing.

### Which jobs run on every replica

Two, and both are observations rather than work. Everything else in the
[background jobs](#background-jobs) table is leader-elected, so on a healthy
multi-replica deployment most of its runs are `skipped` — that is the normal
reading, not a fault.

| Pass | Why it takes no lock |
| --- | --- |
| Job-staleness reporting (rides the `rollup` family) | It publishes `linkctrl_rollup_staleness_seconds` by reading `job_state`. A follower that never published it would report nothing, and whether an alert fired would depend on which replica Prometheus happened to reach. |
| `host-reload` | The verified-hostname set is in-process and every replica holds its own, so a reload the leader performs does nothing for the other three. |

The list is enforced rather than described: `TestOnlyTheDocumentedJobsRunOnEveryReplica`
in `cmd/linkctrl` fails when a pass in `jobs.go` stops taking leadership without
this table changing with it.

### What to alert on across replicas

**`linkctrl_rollup_staleness_seconds`**, and not a per-replica *job skipped*
count. It is read from `job_state`, so every replica publishes the same answer
about the deployment as a whole, and a leader that died is visible as the number
climbing rather than as a series disappearing. A skipped count says only that
*this* replica is not the leader, which on a healthy deployment is what most
replicas are most of the time. The thresholds are in
[Alerts worth having](#alerts-worth-having).

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
| `linkctrl_redirect_duration_seconds{outcome,cache}` | The SLO histogram. `cache` is `memory`, `redis`, `database`, `negative`, `rejected` or **`none`**. `none` is the one worth knowing: it is emitted when resolution *failed* — no tier answered, because Postgres did not — so it is the only series carrying `outcome="error"`, which is the one the alert table below tells you to watch. It was undocumented until 0.2.0, where this row said five values ([F45](build-notes/deferred-findings.md)). **Since M66 it is core's work and not the visitor's wait**: what an inline add-on held the redirect for is subtracted before this is observed, so that this curve and `linkctrl_addon_redirect_duration_seconds` below can disagree and an operator can tell whose latency moved. What the visitor actually waited is `linkctrl_http_request_duration_seconds{surface="redirect"}`, which subtracts nothing. |
| `linkctrl_redirects_total{outcome,cache}` | Traffic and cache hit ratio. `outcome` is `redirect`, `gone`, `not_found`, `error`, `blocked_bot`, `method_not_allowed`, `throttled`, a gate refusal — `unsigned`, `password_required`, `password_wrong`, `spent` — or **`vetoed`** — which is the newest and the one worth knowing about, because it is a refusal *an add-on* decided. A module holding `redirect.inline` said no to a redirect this instance would otherwise have served, and the visitor got the same fixed 403 page a blocked bot gets. **Nothing else on this instance can produce it**: zero forever until an operator installs an inline add-on, and any movement is that add-on refusing traffic. The add-on is named in the log line rather than in a label, for the reason no series here is labelled by add-on name from the redirect path — one series per module per outcome is a cardinality an operator does not control. A vetoed request records **no click**, like the other gate refusals, so a link whose traffic an add-on is refusing shows the drop in its own analytics. |
| `linkctrl_http_requests_total{surface,method,status}` | `surface` is `redirect`, `api`, `web`, `static` or `ops`. `web` is derived at boot from the routes the dashboard actually registers, so it does not fall behind them; before 0.2.0 it came from a hand-written list and nine dashboard routes were counted as `redirect`. |
| `linkctrl_http_request_duration_seconds{surface,method}` | Outside view, including all middleware. |
| `linkctrl_analytics_queue_depth` | Leading indicator for the click pipeline. |
| `linkctrl_analytics_events_dropped_total` | Clicks discarded to protect redirect latency. |
| `linkctrl_rate_limited_total{limit}` | Requests refused by a limit, or add-on work a concurrency bound would not admit: `login`, `api`, `redirect_404`, `link_password`, `upload`, and — since add-ons reached the redirect path — `addon_inline` and `addon_observe`. **The last two are not rate limits and no visitor is refused by one.** They count add-on invocations skipped because all sixteen instance slots were busy: `addon_inline` is a redirect served *without* the add-on rather than queued behind it, and `addon_observe` is an observation dropped after waiting out `LINKCTRL_ADDON_INSTANTIATE_DEADLINE` — **500ms by default, not the 25ms inline deadline**, because what it is waiting for is a slot to start a module in rather than a module's own work. A sustained `addon_inline` rate means an inline add-on is not keeping up with this instance's redirect rate, and it is the series that says so before latency does. **`login` is not only the credential endpoints.** Since add-ons could mint sessions it also counts requests to the routes an installed add-on serves — the whole of `/addons/<name>/` for every add-on, not only one holding `session.mint` — because a limit keyed on a manifest's permissions moves when the manifest does ([configuration.md](configuration.md#rate-limits)). So a rise in this series with `limit="login"` is *either* credential traffic *or* somebody's add-on being visited, and what tells them apart is `linkctrl_http_requests_total{surface="web"}` against the add-on's own paths. A path under `/addons/` that names no installed add-on is a 404 and is **not** counted here, so an alias scanner probing that prefix does not move this series. Never `blocked_audit` — that limiter refuses no request. It bounds how often a destination refusal writes an audit row, so an exhausted budget skips the row rather than the request, and there is nothing here to count. |
| `linkctrl_rate_limit_tracked_keys{limit}` | Client keys each limiter is holding **in this replica's own table**. Every limiter appears here, `blocked_audit` included despite its absence from the row above. For the Redis-shared limits — `login`, `api`, `link_password`, `blocked_audit` and `upload` — that table is only written when the shared limiter did not answer, so a healthy shared limit reads **zero** here — which is indistinguishable from no traffic, and is why the next row exists. |
| `linkctrl_rate_limit_fallback_total{limit}` | Decisions made from this replica's own buckets because the shared limiter did not answer — whether Redis was asked and failed, or the circuit breaker declined to ask at all. **This is the series that says a shared limit has stopped being shared**: any movement means the configured number is being enforced per replica rather than across them. It counts every locally decided request, so during an outage it moves with traffic, and its rate against the request rate says how much of the load is running on per-replica numbers. Always zero for the 404-probe limiter, which is deliberately not shared. |
| `linkctrl_rate_limit_overflow_total{limit}` | Requests allowed **without being limited** because the key table was full. |
| `linkctrl_db_pool_*{pool="app"\|"redirect"}` | Saturation, per pool. |
| `linkctrl_job_runs_total{job,result}` | `ok`, `error` or `skipped`. |
| `linkctrl_job_last_success_timestamp_seconds{job}` | When each job last succeeded, **as this replica remembers it**. Absent on a replica that has not run the job, and cleared by a restart. |
| `linkctrl_rollup_staleness_seconds{job}` | Seconds since each job last succeeded, read from `job_state` and therefore the same on every replica and unaffected by a restart. **This is the one to alert on.** A job that has never succeeded has no series at all. |
| `linkctrl_build_info{version,commit,go}` | Always 1; the labels are the point. |
| `linkctrl_destination_feed_checks_total{result}` | Third-party reputation checks: `clean`, `malicious`, `error`, `skipped`. **Absent entirely unless `LINKCTRL_FEED_URL` is set**, which makes the series itself the answer to "is this instance sending destinations anywhere". |
| `linkctrl_webhook_deliveries_total{outcome,status}` | Outbound webhook deliveries. `outcome` is `delivered`, `retry` or `abandoned`; `status` is the HTTP class — `2xx`, `3xx`, `4xx`, `5xx` — or **`none`** when there was no response at all: a refused connection, a timeout, or this instance declining to open the socket because the name resolved to a private address. Deliberately not labelled by URL: registrations are chosen by users, so a label per endpoint is a series count anybody with a workspace could grow. |
| `linkctrl_automation_firings_total{trigger,outcome}` | Automation rules that fired (M43). `trigger` is one of the three names in the closed vocabulary; `outcome` is `fired`, or `partial` when at least one action failed. Counted once per **firing**, not per subject and not per evaluation — a rule that matched forty links did one thing, and a counter that ticked for every run would measure the scheduler rather than the automation. Deliberately not labelled by rule: rules are named by users, so a label per rule is a series count anybody with a workspace could grow. Which rule fired is in that workspace's audit log, as `automation.fired`. |
| `linkctrl_addon_loads_total{addon,outcome}` | Add-on load attempts: one per add-on per boot, **and one more each time an add-on is installed over the API**, because an install runs the same load. So this counter moving on an instance nobody restarted is somebody installing an add-on, and the boot log's equivalent line says the same thing at `warn`. `outcome` is `loaded`, `manifest_invalid`, `abi_unsupported`, `checksum_mismatch`, `module_unreadable`, `instantiate_failed`, `storage_failed`, `name_collision` or `load_timeout`. **`storage_failed` is the one whose fix is in the database rather than in the directory**: the add-on asked for a schema of its own and something about it did not work — a privilege your database user does not hold, a migration its author wrote wrongly, DDL that reached outside the schema it owns, or a manifest that does not describe the `.sql` files beside it. The boot log names which. **`name_collision` is the one that is about two add-ons rather than one**: their names stand in a `name + "_"` prefix relation — `oidc` and `oidc_x` — so the cookie prefixes and `LINKCTRL_ADDON_` variables derived from the two overlap, and **both** are refused rather than one of them winning. Rename either directory and its manifest with it. **`abi_unsupported` is the one whose fix is a version rather than a file**: the manifest is well-formed and names an ABI generation this build does not serve, so either LinkCtrl or the add-on is the wrong one and the boot log names both numbers — see [addon-abi.md](addon-abi.md). **`load_timeout` is the one where nothing is wrong with the add-on's files at all**: the *module* did not finish compiling or starting within 30 seconds, so the host gave up on it and moved on. A module that spins in its package initialization never returns and never traps, and before this outcome existed it stopped boot indefinitely with nothing said about it; the budget is per add-on, so one that hangs does not spend anybody else's, and boot pays it once per add-on that hangs. The budget covers the module's own execution and nothing else — an add-on's migrations wait on the database for as long as this product's own do, and a slow one is `storage_failed` or nothing, never this. Compiling and starting one add-on takes well under a second on ordinary hardware — a hundred-and-fortieth of the budget — so this is a module that is not going to finish rather than a machine that is slow, and it is the add-on's author who has to fix it. **Absent entirely unless `LINKCTRL_ADDONS_DIR` is set**, so the series itself answers "is this instance running any add-ons". The refusals are counted and not only logged, because an operator whose add-on is quietly not there needs something a scrape can see. `addon` is the add-on's own name, except on a refusal that never got as far as reading one: there the label is the directory, and a directory whose name is not a usable add-on name is published as **`<invalid>`** rather than verbatim — so if you see that, the fault is the directory's name and the boot log names it in full. |
| `linkctrl_addon_info{addon,version,abi_version,failure_class,permissions}` | Always 1 per add-on that **is loaded right now**, which since M67 is not the same as *loaded at boot*: an install over the API adds a series without a restart and a removal **deletes** one, so this is a live count rather than a boot-time reading; the labels are the point, as with `linkctrl_build_info`. An add-on that was refused has no series here — this is what the instance is running, not what it was asked to run — so comparing this against the directory's contents is how you notice one missing. `permissions` is the sorted, comma-separated list of grants the module **holds**, which is not always what its manifest asked for: a permission this build publishes and grants to nobody is declarable, is not held, and is absent here. **There is none today**, and `redirect.inline` was the last — declarable and ungrantable from M62 until M66 turned it on. The boot log names the difference. Sorted so that a manifest listing the same grants in another order does not change the series' identity. `failure_class` is the class this instance **applies**, which is likewise not always the one the manifest declared: an add-on holding `session.mint` reads `required` whatever it asked for, and an operator's `LINKCTRL_ADDON_<NAME>_FAILURE_CLASS` outranks both. Read this label rather than the manifest when you want to know whether a failed load would have stopped the instance. |
| `linkctrl_addon_refusals_total{addon,permission}` | ABI calls an add-on made and did not have the permission for. **Non-zero means a module is asking for something its manifest does not declare**, which is either an add-on shipped with an incomplete manifest or one whose author expected a capability this build does not grant — either way it is the add-on's author's to fix, and the add-on's own log lines at debug name the function. Bounded at one series per add-on per **permission that gates a function** — eight today of a nine-token vocabulary, because `redirect.rewrite_query` gates no *function*: what it permits is one field of one record `redirect_answer_write` already carries, so the check for it lives inside that function and writes this counter from there ([addon-abi.md](addon-abi.md)). The bound is the gating set rather than the vocabulary: the label is read from the function's own requirement, so a token no function names can never appear. It counts *undeclared* calls only: a call refused because this build has not implemented the function yet is not counted, since probing for a capability is what the ABI invites, and neither is a settings key outside the add-on's own manifest. |
| `linkctrl_addon_schema_bytes{addon}` | On-disk size of one add-on's own Postgres schema: **every relation in it that has storage** — tables, sequences, materialized views — with their indexes and TOAST. It sums the kinds it does *not* exclude rather than a list of the kinds somebody thought of, and that is a correction: until 2026-08-19 it summed ordinary and materialized tables only, so a **sequence** — 8192 bytes in the schema from the moment it is created, and outside `pg_total_relation_size` of the table that owns it — was invisible. 24,000 of them, 188 MB of `pg_database_size`, and this gauge read **0** throughout. It was never only an adversary's case either: the host's own `goose_db_version` table in that schema declares an identity column, so every storage add-on has always held at least 8192 bytes this number reported as nothing. One series per add-on that declared `storage.own_schema`; absent for one that did not, and absent entirely on an instance with no add-ons. **Nothing caps it** — that is the same answer the audit log gets, so an add-on that writes a row per redirect is visible here. It is not *everything* an add-on can fill: a **large object** belongs to no schema, so it is absent from this number by construction, and `linkctrl_addon_large_objects{addon}` below is the other half of the *stored* data. **Neither gauge covers disk an add-on's session holds transiently**, and one case is measured: a `WITH HOLD` cursor materialized at commit keeps a temporary file in the cluster's `pgsql_tmp` until the connection ends — 553 MB for one cursor inside the add-on's five-second statement timeout, with both gauges reading zero throughout. It is not stored data and it is freed with the backend, so nothing here reports it; the only bound is `ALTER ROLE addon_<name> SET temp_file_limit`, which needs a superuser, after which the cursor fails with *temporary file size exceeds temp_file_limit* and the add-on cannot raise or reset it — measured both ways. Measured hourly by the maintenance pass on **every** replica rather than only the leader, for the reason `linkctrl_audit_log_bytes` is: a gauge the followers never set reads as zero and your alert would depend on which replica answered the scrape. It is catalogue arithmetic, not a scan. Removing an add-on removes its series and not its data — the schema stays, the boot log names it as an orphan, and **since 0.4.0 the Add-on manager lists it** with its size and offers to drop it, which is where the number goes when the series stops. |
| `linkctrl_addon_large_objects{addon}` | How many Postgres **large objects** an add-on's database role owns, and it should be **0** for every add-on forever. Nothing in LinkCtrl creates one; the ABI gives an add-on no way to ask for one; and a large object is in no schema, so `linkctrl_addon_schema_bytes` cannot see it. A role confined to its own schema can still create one — `EXECUTE` on `lo_from_bytea` belongs to `PUBLIC` and Postgres has no per-role deny — and 40 MB in a single statement was measured. A count rather than a size, because the bytes live in `pg_largeobject`, which only a superuser may read: as superuser, `SELECT count(*), pg_size_pretty(sum(length(data))) FROM pg_largeobject` is the size. **Any value above zero is an add-on writing outside its schema**, the add-on is refused at its next load, and the purge below is what removes the data. Measured hourly, on every replica, like the gauge above. |
| `linkctrl_addon_redirect_duration_seconds{addon,class}` | What one add-on cost one redirect, by add-on and by class (`inline` or `observe`). **Separate from `linkctrl_redirect_duration_seconds` on purpose**: the published figure in [slo.md](slo.md) is *core*, with no inline add-on on the path, and this is the curve that says whose latency the difference is. That is the whole reason it exists — an operator chasing a slow instance can take the number to the right team rather than to this product. It times the *invocation* — instantiating the module and calling it — and not the redirect around it, and **the separation is enforced from both ends**: the redirect handler subtracts the whole extension point before it observes `linkctrl_redirect_duration_seconds`, so that series is core's own work whether or not an add-on is installed. The two therefore sit beside each other rather than one enclosing the other, and they do not sum to what the visitor waited — a killed invocation costs the deadline and is deliberately absent from this histogram. What the visitor waited is `linkctrl_http_request_duration_seconds`. **An invocation that never ran is absent rather than zero**: a skipped one is on `linkctrl_rate_limited_total{limit="addon_inline"}` and a killed one is on the counter below, and a bucket of zeroes would drag the p99 towards a latency nobody experienced. Absent entirely for an add-on that declared neither redirect class, and for an instance with no add-ons. The `observe` series is off the request path by construction and says nothing about what a visitor waited for. |
| `linkctrl_addon_redirect_kills_total{addon,step}` | Invocations the host stopped waiting for, and **`step` says whose bound was missed**. `step="call"` is the add-on: it held a redirect past `LINKCTRL_ADDON_INLINE_DEADLINE`, the runtime closed it, and **the redirect completed without it**. This is the number the availability half of the add-on boundary rests on — an add-on's latency is its own and this instance's availability is not — so any sustained rate there is an add-on to go and fix, with the module named in the label and a warning line per kill naming the deadline it overran. `step="instantiate"` is **not** the add-on: this instance could not *start* the module inside `LINKCTRL_ADDON_INSTANTIATE_DEADLINE`, so the add-on's own code never ran, and the fix is a wider bound, a less busy instance or faster hardware rather than a word with the publisher. The two shared one number until 0.5.0's development and were indistinguishable, which is how a build shipped where every invocation died at `instantiate` and looked exactly like a set of add-ons that had declined to act. It counts overruns and nothing else: a module that was skipped because every instance slot was busy is on `linkctrl_rate_limited_total{limit="addon_inline"}`, and one that trapped is in the log. Zero forever on an instance with nothing holding a redirect grant. |
| `linkctrl_addon_fetch_duration_seconds{addon}` | How long one add-on's **outbound** request took (0.4.0), by add-on. A different path from the two rows above and a different scale: a redirect invocation is priced in milliseconds against a deadline this product sets, and this is priced against a server somebody else runs, bounded by `LINKCTRL_ADDON_FETCH_TIMEOUT`. **Only a request this host attempted is here.** A refusal this host decided itself — no origin configured, an origin nobody named, a malformed request, an address the policy will not dial — is microseconds of parsing and is on the counter below instead, for the reason a killed redirect invocation is absent from the histogram above. `dns_failed` is the one refusal that *is* timed, because a name that will not resolve is the world answering slowly rather than this host declining. Absent entirely for an add-on that did not declare `network.fetch`, and for one that declared it and has never been pointed anywhere. |
| `linkctrl_addon_fetch_total{addon,outcome}` | Every outbound request an add-on made and what became of it (0.4.0). **This is the row to alert on**, because most of the vocabulary is something an operator can act on. `unconfigured` means nobody has named an origin for that add-on, so it reaches nothing — the add-on is installed and inert, and the fix is a field on its page in the Add-on manager. `origin_refused` means it asked for an origin you did not name, which is either a provider whose token endpoint lives on a second hostname or an add-on reaching somewhere it should not; either way it is worth looking at rather than raising a bound. `address_refused` is this host refusing to dial an address outside globally-routable unicast space — loopback, link-local, the cloud metadata service, unique-local, a private range, or any range nobody has thought about, because the rule is an allowlist and refusal is what happens by default. A non-zero rate usually means an add-on is pointed at somewhere inside your perimeter; it can also mean this host has refused an origin that was perfectly legitimate, which an allowlist will eventually do to IPv6 space allocated after this release. **Grep the log for `address_rule=` to tell the two apart** — every refusal writes one line naming the address and the rule that refused it, and `address_rule=outside-routable-space` on an origin you meant to allow is the second case and worth reporting. **A refused URL install writes the same key and is not in this counter**, which is per add-on and cannot name one that was never installed: the line reads *this host refused to dial an address a URL install resolved to* and carries the origin, the address and the rule, so one grep covers both doors. `class_refused` is an add-on trying to fetch from an invocation that may not: an observing redirect handler, or its own start-up. The **inline** redirect class never produces this row — the ABI refuses that call before the fetch machinery is reached, and deliberately counts nothing on the redirect hot path — so an inline module reaching outward shows up in the debug log and nowhere else. **The three refusals this host decides for itself write no warning at all**: `unconfigured`, `origin_refused` and `class_refused` are things a module can produce as fast as it likes on a page anybody can reach, so their lines are at `debug` and this counter is where you watch them — which is why the alert below is written against the label rather than against a log. `address_refused` is the one that keeps its warning, because `address_rule=` names something no counter carries. `redirect_refused`, `too_large`, `timeout`, `dns_failed`, `connect_failed` and `invalid_request` are the rest; `ok` says a response arrived and says nothing about its status code. `outcome` is a **closed** eleven-word vocabulary — the same words the add-on itself branches on — so this is bounded at eleven series per add-on that has actually made a request. |

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
| A shared limit stopped being shared | `rate(linkctrl_rate_limit_fallback_total[5m]) > 0` | The shared limiter is not answering, so each replica is enforcing the configured number on its own and N replicas allow N times it — on the password-guess limiter, that is N times the wordlist budget. Deliberately label-free: the one limiter that is never shared, `redirect_404`, never moves this series, so a filter would add nothing today and would go stale tomorrow — this expression previously matched `login\|api`, and a Redis outage could not page for `link_password`, the limiter whose silent degradation an operator otherwise learns about from a guessed password. A rate rather than a threshold on the value: the counter is monotonic, so `> 0` on the raw number latches forever after one transient blip. This row previously told you to watch a rate that "stays unchanged", which is a description rather than an expression ([F102](build-notes/deferred-findings.md)). |
| Redirects being throttled | `rate(linkctrl_rate_limited_total{limit="redirect_404"}[5m]) > 1` | Either someone is scanning for aliases, or `TRUSTED_PROXIES` is wrong and every visitor shares one bucket. Check which before tuning the limit. |
| Webhooks being abandoned | `rate(linkctrl_webhook_deliveries_total{outcome="abandoned"}[1h]) > 0` | A workspace's receiver has been failing for over an hour and events are being dropped. Nothing recovers an abandoned delivery. Which webhook it was is in that workspace's own delivery log, at `/webhooks`, rather than in a metric label. |
| Webhooks reaching nothing | `rate(linkctrl_webhook_deliveries_total{status="none"}[15m]) > 0` | No response at all. Usually somebody's endpoint is down; if it is *every* webhook at once, suspect this instance's egress or DNS rather than the receivers. |
| Automation actions failing | `rate(linkctrl_automation_firings_total{outcome="partial"}[1h]) > 0` | A rule fired and at least one of its actions did not complete. The firing itself is not retried — the watermark has already moved — so the subjects it covered are not seen again. Which rule, and what it was trying to do, is in that workspace's audit log at `automation.fired`. |
| Reputation feed not answering | `rate(linkctrl_destination_feed_checks_total{result="error"}[15m]) > 0` | Only if you configured one. A feed check fails **open**: the destination is accepted and the built-in tiers decide, so the product behaves exactly as it does with no feed at all. That is deliberate — a third party's outage must not stop you making links — and it is also why a feed that silently stopped working is invisible anywhere but here. |
| 5xx on any surface | `rate(linkctrl_http_requests_total{status="5xx"}[5m]) > 0` | |
| Rows in a default partition | see [below](#partitions) | Silent data misrouting; next month's partition will fail to attach. |
| An add-on is not loaded | `(count(linkctrl_addon_info) or vector(0)) < <however many are installed right now>` | A `degrade`-class add-on refused to load and the instance is serving without it. **`or vector(0)` is load-bearing, not defensive**: a refused add-on publishes no `linkctrl_addon_info` series at all, so if every add-on you installed is `degrade`-class and every one of them refuses, a bare `count()` returns an empty vector, the comparison yields nothing, and the alert is silent in exactly the case it is written for — [F102](build-notes/deferred-findings.md) was this same shape in this same table. Which one, and why, is in `linkctrl_addon_loads_total{outcome!="loaded"} > 0`, which names the add-on and stays above zero for the life of the process; alert on that instead if you would rather not maintain a count by hand, and keep this one as well if you also want to hear about an add-on that has gone missing from the directory entirely, which produces no load attempt and therefore no counter. A `required`-class add-on cannot produce either — the instance would not be up. |
| An add-on is asking for what it did not declare | `rate(linkctrl_addon_refusals_total[1h]) > 0` | The module is calling a host function whose permission is not in its manifest, and the host is refusing it. Not a fault of yours: report it to whoever publishes the add-on, with the permission from the label and the function from the instance's debug log. Worth alerting on because the module may be degrading silently — the refusal is a status its author has to have handled. |
| An add-on owns a large object | `linkctrl_addon_large_objects > 0` | Zero is the only correct value; see the metric's own row. It means the add-on wrote data outside its schema, that data is invisible to the size gauge, and the add-on will be refused at its next load until an operator purges it. Nothing about it is normal, so alert on any nonzero value rather than on a rate. |
| An add-on's schema is growing | `rate(linkctrl_addon_schema_bytes[24h]) > 0` on a module you did not expect to grow, or an absolute ceiling of your own | Nothing caps an add-on's schema, so this is the only thing that will tell you it is growing. **Alert on the filesystem as well**: this gauge and `linkctrl_addon_large_objects` between them cover data an add-on has *stored* — every catalogued relation in the schema since the 2026-08-19 correction, sequences included — and a session holding a `WITH HOLD` cursor fills `pgsql_tmp` without moving either, see this gauge's own row. Pick the shape by what the add-on is: a configuration store should be flat and a rate above zero is news, while an analytics add-on grows by design and only an absolute threshold means anything. There is no product-side remedy — the add-on's author decides what it keeps — so the action is theirs to take or yours to uninstall. |
| An add-on is holding up redirects | `rate(linkctrl_addon_redirect_kills_total{step="call"}[15m]) > 0` | A module on the redirect path is not returning inside its deadline, and every kill is a visitor who waited it out before getting their redirect. Nothing is broken — the redirect happened — but the latency somebody is measuring on this instance is the add-on's, and the published figure in [slo.md](slo.md) does not describe it. Take it to whoever publishes the add-on, with `linkctrl_addon_redirect_duration_seconds{class="inline"}` beside `linkctrl_redirect_duration_seconds` as the evidence of whose it is. Raising `LINKCTRL_ADDON_INLINE_DEADLINE` makes the kills stop and the waiting longer; it is a way of deciding how much of somebody else's latency you will absorb, not a fix. |
| An add-on cannot reach where it was pointed | `rate(linkctrl_addon_fetch_total{outcome!="ok"}[15m]) > 0` | Something an add-on tried to fetch was refused. Read the `outcome` label before doing anything: `unconfigured` is an add-on nobody has named an origin for and is fixed on its page in the Add-on manager; `origin_refused` is an origin you did not name, and adding it is a decision about egress rather than a configuration chore; `address_refused` is this host refusing to dial outside globally-routable unicast space and usually means an add-on is pointed inside your perimeter, which is worth understanding before it is worth fixing — `grep address_rule=` in the log names the address and the rule, and is what separates that from this host refusing an origin that was legitimate. `timeout` and `connect_failed` on an origin you meant to allow are the provider or the network rather than the add-on. Raising `LINKCTRL_ADDON_FETCH_TIMEOUT` or `LINKCTRL_ADDON_FETCH_MAX_BYTES` addresses only the last two outcomes and addresses none of the first three. |
| This instance cannot start add-ons | `rate(linkctrl_addon_redirect_kills_total{step="instantiate"}[15m]) > 0` | **Your add-ons are not running.** This instance could not instantiate the module inside `LINKCTRL_ADDON_INSTANTIATE_DEADLINE`, so the redirect was served as though nothing were installed — no observation, no veto, no rewrite — and the only outward sign is this counter and a warning line naming the variable. Nothing is down and nothing will look wrong: that is why it is worth an alert rather than a glance. Starting a module costs what the hardware and the load make it cost, so read it beside CPU saturation and beside `linkctrl_rate_limited_total{limit="addon_inline"}`. Widen the bound if the instance is simply slower than the default assumes; if the rate is constant at every load, the module is too large to be starting per redirect and the publisher needs to know. |
| Add-on invocations are being skipped | `rate(linkctrl_rate_limited_total{limit="addon_inline"}[15m]) > 0` | Every instance slot was busy, so redirects were served **with the add-on skipped**. That is the host protecting itself and it is why availability survives a module that hangs, but it also means the add-on is not doing whatever you installed it to do on those requests. Read it beside the kill counter: both high together is one module hanging, and skips alone with no kills is an instance genuinely at capacity for add-on work. |

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

An in-process scheduler, one goroutine per job family, each family
leader-elected by its own Postgres advisory lock — so a long dimension pass
delays nothing but itself (D107). Every job re-checks its lock, so leadership
moves between runs without coordination, and a crashed holder releases it
automatically. On a multi-instance deployment most runs are `skipped` — that is
healthy, and a follower reporting no skips for a family's jobs is a follower
whose goroutine for that family has stopped. `domain-verification` and
`host-reload` share one goroutine in that order, so a replica reloads the set
its own pass just wrote rather than racing it.

| Job | Every | Does |
| --- | --- | --- |
| `rollup` | 60s | Recomputes the per-link and per-workspace daily totals from raw events and upserts. Whole days, never incremental — an "add what arrived since the watermark" design double-counts on retry and, once it drifts, stays wrong invisibly. |
| `dimension-rollup` | 15m | The same recompute for the per-dimension and per-destination breakdowns, on a longer clock because it costs far more: its upsert count is bounded by the distinct `(link, day, dimension, value)` tuples the day's clicks imply, where the totals are bounded by the links that were clicked. Measured at the SLO dataset it is 4.8–6.3s against a totals pass of ~1.5s ([slo.md](slo.md#re-measured-for-m37-2026-08-03)). **The visible consequence is that a breakdown on a link's page can be up to fifteen minutes behind the totals above it**, which is what the staleness alert below is for. Its own row in `job_state`, so a totals run cannot advance a watermark the breakdowns have not reached. |
| `mail` | 30s | Drains `mail_outbox`. Does not run at all with no mailer configured. That does not mean the table is empty: an instance that *had* a relay and had it cleared keeps everything queued before the change, and the hourly purge abandons those past 30 days rather than holding them forever. Five attempts per message, backing off 1m, 2m, 4m and 8m between them — about fifteen minutes end to end, not the *1m to 16m* this row said until 0.2.0: the fifth attempt is the last, so the delay after it is never waited and `BackoffMax` is unreachable at the shipped attempt count ([F45](build-notes/deferred-findings.md)). A message that never gets through is marked `failed` and kept with the relay's error. Faster than the hourly jobs because an invitation is something a person is waiting for. See [configuration.md](configuration.md#mail). |
| `partitions` | 1h | Creates monthly partitions two months ahead. |
| `retention` | 1h | Drops monthly partitions that are entirely outside their table's window — `ANALYTICS_RETENTION_DAYS` for `click_events` and `visitors`, `AUDIT_RETENTION_DAYS` for `audit_logs`. Runs after `partitions`, so a run can never drop what it just created. With the audit default of `0` it drops no audit partition at all. |
| `salt-purge` | 1h | Deletes analytics salts older than two days. **This is the de-identification step, not housekeeping**: once a salt is gone, that day's visitor hashes cannot be linked to an address by anyone. |
| `audit-growth-warning` | 1h | Notifies **the instance principal** once `audit_logs` passes `AUDIT_SIZE_WARN_BYTES`, at most weekly. Skipped entirely when the threshold is `0`, and silent on an instance whose principal grant has been revoked — the table is instance-wide and the only thing that bounds it is `AUDIT_RETENTION_DAYS`, an environment variable, so there is nobody else the warning could usefully reach. Until 0.2.0 it went to every organization's owners, which under open signup meant every account on the instance, weekly, carrying a number none of them could act on. |
| `partition-check` | 1h | Warns if rows landed in a default partition. |
| `signup-purge` | 1h | Deletes pending registrations whose verification window has lapsed, and consumed ones past their short retention. Runs only where self-serve sign-up is possible; it exports `linkctrl_job_runs_total` like every other job and was the one `withLeadership` job with no row in this table until 0.2.0 ([F45](build-notes/deferred-findings.md)), so an operator reading it for the set of jobs to watch was one short. |
| `domain-verification` | 1h | Re-reads every registered hostname's DNS TXT challenge. A hostname that starts passing begins being served without anybody pressing anything; one that stops passing keeps serving for `DOMAIN_VERIFY_GRACE` — a day by default — with the owning workspace notified at the **first** failure, and then stops being served on every replica. **This is the only job whose failure takes something offline**, which is why the window is long and why the warning comes early; the runbook is in [deployment.md](deployment.md#custom-domains). A pass that changed anything logs `custom domain verification` with the counts. **Serving hostnames are checked before registered-but-unserved ones**, so the stop above can only ever be delayed by other serving hostnames — never by a pile of registrations, which anybody can create and whose place in the queue a rename resets. A domain renamed while its check was in flight logs `a domain changed while its verification was in flight` and verifies nothing; one line is an owner renaming at an unlucky moment, a stream of them is somebody trying to have a check of one name land on another. `DOMAIN_VERIFY_INTERVAL=0` switches it off, which also removes the only thing that would ever stop serving a hostname whose record has gone. |
| `host-reload` | `DOMAIN_VERIFY_INTERVAL` | Re-reads this replica's verified-hostname set from Postgres. **The only job in this table that does not take leadership, and that is the point**: the set is in-process and every replica holds its own, so a reload the leader performs does nothing for the other three. (The other per-replica pass is job-staleness reporting, which is not a family of its own and so has no row here — both are in [which jobs run on every replica](#which-jobs-run-on-every-replica).) It is the backstop Redis pub/sub cannot be — pub/sub is at-most-once, so a published invalidation that is simply lost while the subscription stays healthy is never noticed, and an instance with no Redis has no subscriber at all. Before this, such a replica served whatever it last knew until it restarted. One query against a table of tens of rows; a failure is logged and the replica keeps the set it has. `DOMAIN_VERIFY_INTERVAL=0` switches it off with the pass. |
| `update-check` | 1h | Asks GitHub whether a newer LinkCtrl has been released, and notifies **the instance principal** if there is one. **The only job here whose work leaves your deployment**, which is why it is a family of its own and not a step in the hourly pass — a later "just one more thing" added to `housekeeping` must not be able to become undisclosed egress. The ticker is hourly and the period is daily: the bound is a row in `instance_settings`, not the clock, so a replica restarted every ten minutes still asks once. A failure is one debug line and no retry until the next day. Off entirely with `LINKCTRL_UPDATE_CHECK=false`; off for an instance whose operator declined; and off for one where **nobody has been asked yet**, which is what an upgraded instance is until an administrator signs in — the pass runs, claims nothing and returns. What the request carries is enumerated in [configuration.md](configuration.md#update-check) and counted in [SECURITY.md](SECURITY.md)'s egress row. |
| `housekeeping` | 1h | The reapers, plus the one pass that reaps nothing. Hard-deletes links whose 30-day trash window has passed — reserving any alias that ever received traffic in the same statement, so it is never reissued — and deletes sessions, revoked API keys, spent and lapsed password-reset tokens, and finished outbox rows past their retention. Each purged link is logged by alias; that log line is the only record of the deletion. **Since 0.3.0 it also erases the residue of deleted accounts**, which is the opposite shape: the rows are kept and the person is taken out of them, because the audit log and the dispute queue are records of what happened. This is what bounds the gap between an account being deleted and its name disappearing from those tables at **one hour** — access ends inside the deleting transaction and does not wait for this. Logged as `deleted accounts erased` with a count and no identifier. |

All of them run once at startup rather than waiting a full interval, so a fresh
instance has current numbers — except `host-reload`, whose boot pass is process
start-up itself loading the set.

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
  # The same threshold the in-app owner notification fires at, spelled exactly.
  # LINKCTRL_AUDIT_SIZE_WARN_BYTES defaults to 5368709120 — five *gibibytes* —
  # and `5e9` is five gigabytes, seven per cent lower, so the two disagreed about
  # when this is a problem while the comment claimed they were the same (F45).
  # Change both if you change the variable.
  expr: linkctrl_audit_log_bytes > 5368709120
  for: 1h
  labels: { severity: warning }
  annotations:
    summary: "audit_logs has passed 5 GiB"
    description: >-
      Currently {{ $value | humanize1024 }}B. Set
      LINKCTRL_AUDIT_RETENTION_DAYS, or confirm the disk is sized for it.

- alert: AuditLogGrowthUnbounded
  # Projects a fortnight ahead from the last week, so an instance heading for
  # trouble is flagged before it arrives rather than after.
  expr: predict_linear(linkctrl_audit_log_bytes[7d], 14 * 86400) > 5368709120
  for: 6h
  labels: { severity: warning }
  annotations:
    summary: "audit_logs is on track to pass 5 GiB within a fortnight"
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

## Add-ons

Only relevant with `LINKCTRL_ADDONS_DIR` set. An instance without it constructs no
WASM runtime, creates no schema and publishes none of the series above.

### What an add-on's own pages answer when they cannot

An add-on holding `routes.own_prefix` serves `/addons/<name>/…` on the dashboard.
Four failures reach a reader as a page, and each names a different fix:

| Status | Means | Where to look |
| --- | --- | --- |
| **404** | No add-on of that name is loaded, or the one loaded did not declare `routes.own_prefix`. The two are deliberately one answer — which add-ons an instance runs is not something an anonymous visitor is told | The boot log's `add-on loaded` lines, and the manifest's `permissions` |
| **503** | Sixteen add-on invocations are already in flight across the instance and this one waited out the request timeout. Each holds a module instance — a page request's is built for it and thrown away, a redirect's comes from the pool `LINKCTRL_ADDON_POOL_SIZE` bounds — so this is capacity rather than an error — and the sixteen are shared with the redirect path, so an inline add-on under load can be what filled them — **and it runs the other way too**: an add-on page that makes an outbound request holds its slot for up to `LINKCTRL_ADDON_ROUTE_DEADLINE` (10s), so sixteen concurrent sign-ins through an add-on hold all sixteen for seconds, and inline redirect add-ons are skipped for the duration. An add-on holding both `routes.own_prefix` and `network.fetch` — which is the OIDC add-on's shape — is the case to watch | `linkctrl_http_requests_total` for the shape of the traffic; if it is not a burst, the add-on is slow and its own log will say why |
| **502** | The module answered nothing usable: it exports no handler, it wrote no response, it trapped, or it refused its own route | The instance log — the host writes the add-on's name and the reason, and nothing of it reaches the page |
| **413** | The request is larger than an add-on may be handed. The bound is on the *record* the host builds, not on the body alone: the ABI carries one value at up to 64 KiB, and the body shares that with the method, path, query, content type, language and declared cookies — a body that is not UTF-8 is base64 first, and a control character costs six bytes inside the encoding. So there is no single body size to quote, and the module never ran | Whatever is posting to the add-on |

**A 502 naming a missing export is a packaging fault, not an outage**: the add-on
loaded, so the module is intact and signed for — it simply exports no
`linkctrl_http_handle`, which is its author's to add. The instance is otherwise
healthy and every other page works.

### A required add-on that will not load holds the instance down

This is designed behaviour and it has a cost worth knowing before you meet it. An
add-on declares its own failure class, and `required` means *the instance is not
this product without me*. So a `required` add-on that fails to load is an exit with
the reason, before the listener opens — which is right for an authentication
provider, because the alternative is an instance that boots with sign-in silently
missing, and wrong to discover during an incident.

**One case is not the add-on's to declare.** An add-on whose manifest lists
`session.mint` is `required` whatever its `failure_class` says, because a module
that decides who is signed in cannot know whether this instance has another way
in — and the failure mode of guessing wrong is the instance above, booted with
sign-in missing. Its manifest's class is ignored, not merged. The operator's
override below is the only thing that changes it, which is the point: the
decision belongs to whoever knows what else this instance has.

Since add-on migrations run at boot, a **bad add-on release can hold an instance
down without any of your configuration changing**. The DDL is the add-on author's;
what you control is which version is in the directory.

What it looks like: the process exits, the last log line names the add-on, the
outcome and the reason, and `linkctrl_addon_loads_total{addon,outcome}` never gets
scraped because nothing is serving. Read the exit, not the metrics.

Recovery, in the order that costs least:

1. **Roll the add-on back.** Put the previous version's directory back and restart.
   Its migrations are versioned in a goose table inside its own schema, so a
   version already applied is not re-applied and a rollback of the module alone is
   safe unless the add-on's own release notes say otherwise. Nothing rolls DDL
   *back* — `down` migrations are not run at load — so a release that added a
   column leaves the column.
2. **Remove the directory and restart.** The instance comes up without the add-on,
   its schema and its data untouched. Whatever the add-on did for you stops; if
   that was sign-in, make sure you have another way in first.

   **If the instance is still up, `DELETE /api/v1/addons/{name}` does this without
   the restart** and is the same outcome: the add-on unloads, its directory comes
   out of the add-ons directory, and its schema and data stay. It needs the
   directory mounted writable and `addons.manage`, which is the instance
   principal's — see [configuration.md](configuration.md#installing-without-a-restart).
   It is listed second here rather than first because the failure this section is
   about is a **boot** failure, and an instance that will not start has no API to
   call. Removing a `required` add-on this way cannot brick the next boot: the
   directory is out of the discovery set before anything is unloaded.
3. **Change the failure class.** Editing `failure_class` to `degrade` in a manifest
   you did not write is a decision, not a fix: it converts "the instance stops" into
   "the instance serves with this missing", which for an authentication add-on is
   the outcome the class exists to prevent. **And for an authentication add-on the
   edit does nothing at all** — an add-on declaring `session.mint` is `required`
   however its manifest reads, so the only thing that degrades one is
   `LINKCTRL_ADDON_<NAME>_FAILURE_CLASS=degrade` in the environment. That is
   deliberate: an environment variable is a thing an operator sets on purpose and
   a deployment records, where an edit to somebody else's manifest is neither.
   Setting it to anything that is not `required` or `degrade` stops the instance
   rather than picking one, because the variable that decides whether this add-on
   may be skipped is the one that could not be read. Nothing stops you editing the
   manifest or records that you did: the manifest is the trust root and is not
   itself hashed — the digests in it
   are of the `.wasm` and of each `.sql`, and editing a field beside them
   invalidates none of them. So the only account of the change is yours. Prefer 1
   or 2. What the host *does* guarantee about that file is that it reads it the way
   you do: a key must be spelled exactly as [configuration.md](configuration.md)
   documents it and must appear once, so a manifest cannot say `"permissions": []`
   to your eye and mean something else to the host.

`name_collision` as the outcome needs none of the three: nothing is wrong with
either module. Two installed add-ons' names stand in a `name + "_"` prefix
relation — `oidc` and `oidc_x` — so the cookie prefixes and the
`LINKCTRL_ADDON_` variables derived from them overlap, and both are refused
rather than one of them silently getting the other's. The boot log names the
pair. Rename one directory **and** the `name` in its manifest, which must match
it; if either add-on is `required` the instance does not start until you do.

`load_timeout` as the outcome means the host waited 30 seconds for that add-on's
*module* to compile or to start, and gave up. Nothing is malformed: the manifest
parsed, the digest matched, and the module is *running* — it just never finished
starting, which a module can do by looping in its package initialization.
Compiling and starting an ordinary add-on takes well under a second, so this is
never a machine being slow. There is nothing to fix on your side beyond the three
options above — roll it back, remove it, or accept it as `degrade` — and the
add-on's author is who has to fix it.

**The budget covers the module and not the database.** An add-on's migrations wait
up to five minutes for the migration lock, exactly as this product's own
migrations do, so a replica starting while another is mid-migration waits rather
than failing into a restart loop — and a long `CREATE INDEX` on an upgrade is not
cut off at 30 seconds. A migration that fails is `storage_failed`, never this.

**Boot pays the budget once per add-on that hangs**, and in the contrived case
twice: compiling and starting are two steps and each gets its own 30 seconds. A
module that loops in its package initialization — the ordinary shape of this —
spends one. Three hanging add-ons delay the listener by 90 seconds, not 30, and
that is worth knowing before
`docker compose up --wait`: the shipped healthcheck allows a 30-second start
period and five attempts 10 seconds apart, so a bring-up with three of them
hanging is reported as failed even though the instance comes up behind it. Either
take the hanging add-ons out of the directory or raise `start_period`. The budget
is per add-on rather than shared on purpose — one shared budget, once spent by the
first hang, is spent for every add-on behind it, which turns one `degrade`
add-on's hang into a `required` add-on's refusal to boot.

`storage_failed` as the outcome narrows it further: the module is fine and the
database is the problem. Six causes, most likely first — **not** in the order the
host reaches them, since it reads and verifies the migration files before it
creates anything:

- **Your database user cannot create a role.** The host makes one role per add-on,
  and that role is the confinement. `CREATEROLE` or superuser is required, and
  password authentication has to be available for the new role — the host connects
  *as* it. A deployment authenticating by `peer` or by a cloud IAM token cannot
  offer that and such an add-on will not load there.
- **The manifest does not describe the `.sql` files beside it.** A file it does not
  list, or one whose bytes it describes wrongly, refuses the add-on before any
  schema exists. Report it to whoever published the add-on; do not edit the
  manifest to match, which is deleting the check rather than passing it.
- **The DDL reached outside the add-on's own schema.** Postgres refuses it and the
  host also asks the catalogue afterwards whether anything landed elsewhere. This
  one is an add-on bug and possibly worse; it is the add-on's author's to answer
  for. The refusal reads `it owns <what>, which is not in addon_<name>` and names
  the object, whether it is a table in another schema, a large object or a
  temporary relation. The purge below is what removes what it names.
- **The add-on gave its own schema away.** The refusal reads `it has granted
  <privilege> on <what> to <whom>`, and `PUBLIC` is a possible *whom*. An add-on
  owns its schema, so it can grant on it, and another add-on can then read those
  tables — the host cannot prevent that and does not pretend to; it reports it at
  the next load and declines to run the module. Usually an add-on bug or a
  deliberate integration its author did not document, and the operator's part is one
  `REVOKE` of what the message names. It reads the same way if *you* granted it: a
  reporting role with `SELECT` on an add-on's schema stops that add-on from loading,
  which is a cost of the check stated rather than hidden.
- **The add-on's tables are owned by somebody else, and you restored a backup.**
  The refusal reads `it does not own <what>, which is in addon_<name>`. This is not
  an add-on bug: `pg_dump` carries no roles, so a restore into a cluster whose roles
  were not restored first leaves an add-on's tables owned by LinkCtrl's own user.
  Fix it by restoring the roles and re-owning the tables — the procedure and the
  reason are in [deployment.md](deployment.md#5-back-it-up) — not by dropping the
  schema, which drops the add-on's data.
- **The add-on parked a Postgres setting on its own role.** The refusal reads
  `it has parked <parameter> in database <name>`, or `in every database`. Any role
  may set a user-settable parameter on itself and Postgres offers no way to forbid
  it, so the host clears them at every load instead — including the per-database
  ones, which are a second set the role can write for *any* database and which
  outlived every boot until this release. This message means one survived that,
  which is the host's own repair having failed rather than an ordinary add-on
  fault: the load resets before it checks, so in normal operation you will never
  see it. Report it, and clear it by hand — one statement per line the refusal
  printed, each naming that line's own scope:

  ```sql
  ALTER ROLE addon_<name> IN DATABASE <database> RESET ALL;  -- in database <name>
  ALTER ROLE addon_<name> RESET ALL;                         -- in every database
  ```

  `<name>` is the add-on's and `<database>` is the database the refusal named —
  two different placeholders, and the second is not necessarily the database
  LinkCtrl runs in, because an add-on can park a setting for any database in the
  cluster, including one it cannot connect to. The second statement also clears
  the search path LinkCtrl pinned on the role; the next load puts it back, and
  putting it back by hand is `ALTER ROLE addon_<name> SET search_path =
  addon_<name>`.

### Removing an add-on leaves its schema

Deleting a module's directory does not delete its data. The next boot warns:

```
add-on schemas with no loaded module; their data is still on disk and nothing here deletes it
```

**Its role stays too, and so does anything the add-on parked on that role.**
Removing an add-on never removes its role — the `DROP ROLE` below is the only one
there is, and it is yours to run — so a Postgres setting the add-on set on itself
sits in the cluster after the module is gone, with no load left that would ever
clear it. LinkCtrl clears those settings at every load of that add-on and nowhere
else, and it does not sweep roles no add-on claims: it cannot tell a role it made
from one you made that happens to be named the same way, and mutating a
cluster-shared catalogue on the strength of a name is worse than the leftover.

**What is left is inert, and that is measured rather than assumed.** A session
default is read only by a session that **logs in** as the role: on Postgres 17.10,
`SET ROLE` and `SET SESSION AUTHORIZATION` both leave `work_mem` at the cluster
default of 4 MB, and only a login reads back the parked value — `source = user`
for the cluster-wide row, `source = database user` for a per-database one. Nothing logs in as an add-on's role once its module is gone, so the row
costs nothing while it sits there. Re-install the add-on and its next load clears
every scope before its pool opens a connection.

If you want it gone anyway, one statement per scope, against any database in the
cluster:

```sql
SELECT coalesce(d.datname, '(every database)') AS scope, s.setconfig
  FROM pg_db_role_setting s
  LEFT JOIN pg_database d ON d.oid = s.setdatabase
  JOIN pg_roles r ON r.oid = s.setrole
 WHERE r.rolname = 'addon_<name>';

ALTER ROLE addon_<name> IN DATABASE <database> RESET ALL;  -- one per named scope
ALTER ROLE addon_<name> RESET ALL;                         -- the (every database) row
```

The last statement also drops the search path LinkCtrl pinned on the role. That is
harmless while the add-on is uninstalled, and the next load puts it back; the purge
below drops the role outright and makes the question moot.

Leaving the schema is deliberate. Purging one destroys rows nothing else can
recreate, and doing it because a file went missing would make an accidentally
unmounted volume into data loss. The schema is `addon_<name>`, it still costs disk,
and it still holds whatever the add-on stored.

**Since 0.4.0 the Add-on manager lists it and offers to drop it**, which is the
route to prefer: `/instance/addons` names every `addon_*` schema no installed
module owns, with its size measured at the moment you are looking, and each row
carries its own purge behind a confirmation. What that does is one statement —
`DROP SCHEMA addon_<name> CASCADE` — and it is recorded in the instance-wide audit
log as `addon.data_purged` with the size. Nothing measures the schema after the
drop, so that number survives only where the purge wrote it: the audit row, which
is the durable one, the `200` body the API answers a purge with, and the server
log line beside it.

**It deliberately stops there**, and the two statements after it in the block
below are why this section keeps them: the page does not drop the role and does
not drop what the role owns outside the schema. Dropping a role fails while it
owns anything anywhere, so a page that tried would succeed or fail depending on
state you cannot see from it; and a large object is exactly that state. So use the
page for the schema, and the block below when you also want the role gone — or
when `linkctrl_addon_large_objects` was ever nonzero for that add-on.

To purge one by hand, having decided that is what you want:

```sql
DROP SCHEMA addon_<name> CASCADE;
DROP OWNED BY addon_<name>;
DROP ROLE addon_<name>;
DELETE FROM addon_settings WHERE addon = '<name>';
```

Back up first — this is not recoverable — and do it with the add-on's directory
already removed, or the next boot recreates the schema empty and re-runs its
migrations.

**The first three in that order, and the fourth is separate.** `DROP SCHEMA … CASCADE` does not touch a
**large object**, because a large object belongs to no schema; `DROP ROLE` then
fails with *cannot be dropped because some objects depend on it*, whose DETAIL
names the large object, and the disk stays allocated with no schema left to
attribute it to. `DROP OWNED BY` is the statement that drops one. The middle line
is a no-op for an add-on that behaved, which is every add-on that has never made
`linkctrl_addon_large_objects` nonzero, and it costs nothing to run anyway.

**The fourth statement is the one no page offers**, and it is here because there
is nowhere else. `addon_settings` holds what an operator typed into the Add-on
manager's detail page for this add-on — a stored `secret` among them — keyed on
the add-on's **name** the way `addon_identity_links` is, so whatever is installed
under the name next reads those values through `config_get`. The purge counts them
at the point of decision and deletes none, and the manager cannot delete them
either: a settings write is refused for a name that is not loaded. Run it when the
name is being retired, or when the add-on you are replacing held a credential the
replacement should not have. Leave it alone when you are re-installing the same
add-on, which is exactly the case the rows are kept for.


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
| The instance exits at boot naming an add-on | A `required`-class add-on will not load. The exit line names the outcome; `storage_failed` means the database rather than the directory. See [Add-ons](#add-ons) for the recovery order. |
| An add-on's tables are missing after an upgrade | Its migrations did not run, and if the add-on is `degrade`-class the instance came up anyway. `linkctrl_addon_loads_total{outcome="storage_failed"}` names it and the boot log says why. |
| Cannot claim a fresh instance | `/setup` is single-use and returns 404 once a user exists. Invite the person instead, or set `SIGNUP_MODE` and restart. |
| `/signup` answers 403 with `SIGNUP_MODE=open` | No `LINKCTRL_SMTP_HOST`. Public registration confirms the address by email before the account exists, so with no relay the effective mode is `invite`. The boot log says so; set a relay and restart. |
| Nobody can see `/disputes` after upgrading to 0.2.0 | The dispute queue moved off the owner role and onto the instance-level principal. The upgrade conferred it on the **earliest surviving account**, which on any instance that went through `/setup` is the setup account — sign in as that account and appoint whoever should review, at the bottom of `/disputes`. If that account is gone or was never yours, see *Moving the instance principal* below. |

### Moving the instance principal

The account that claimed the instance holds `instance.admin`, and no page, API
route or key confers it: a principal that could mint another principal would
defeat the bound that makes delegation safe. Somebody holding it appoints
reviewers, and reviewers appoint nobody.

That leaves one case — the founding account is gone, or was never the operator's,
or its password is lost on an instance with no mailer, where the product's own
password reset has nothing to send with. It is the operator's to fix, because
they have the box and the person who lost the account does not:

```sh
$ lctl instance principal show
ID                                    EMAIL                NAME
019fb19b-6fa9-7932-9de0-81810c2db7b2  founder@example.com  Founder

$ lctl instance principal move --to you@example.com
the instance principal is you@example.com
taken from founder@example.com
```

**It moves and never adds.** Exactly one account holds the principal afterwards,
checked before the change commits, so the set of people who can appoint reviewers
cannot grow — which is why this is a command an operator runs and not a control
in the dashboard. Reviewers already appointed keep the queue; nothing inside any
organization changes. On an instance running with `APP_ENV=production` it asks for
`--force`, exactly as `lctl seed` and `lctl demo` do.

The account has to exist already: this appoints, it does not register anybody and
it does not change a password. The change takes effect on that account's next
request, nothing is cached, and it is written to the instance-wide audit log with
the actor recorded as `system`, because nobody signed in to make it.


## What is not here yet

Deliberate gaps, so they are not discovered during an incident:

- **Region and city are never stored**, even with a GeoIP database configured.
  Country only, deliberately.
- **Invalidation needs Redis to cross replicas.** With Redis down, each replica
  falls back to `REDIRECT_TTL` staleness — correct, just slower to converge.
  See [below](#cache-invalidation-across-replicas).
- **A killed replica loses its buffered click events.** The queue is in-process
  and bounded on purpose; a graceful shutdown flushes it and a `SIGKILL` does
  not. Making it durable means a work queue in Redis, and that would make Redis
  required. Everything else in flight survives — see
  [what happens when a replica dies](#what-happens-when-a-replica-dies).
- **The dimension rollup is expensive and gets worse.** It recomputes whole days
  every fifteen minutes; at 5.7M click events that measured 4.8–6.3 seconds per
  run. It ran on a 60-second clock at 16–21 seconds per run until
  [M37](build-notes/phase-details/m37.md) split the cadences, and this paragraph
  described that until M69.9 counted it. Redirects are unaffected — the
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
| `rate limiting fell back to per-instance buckets` | A shared limit — named in the line's `limit` field — is no longer shared across replicas. Logged once when it starts, not per request. |

A replica logging either of the first two without the third is the one to look
at: it is serving from a cache nothing is invalidating, from Postgres rather
than from memory, until Redis answers again.

Full list: [Plan.md](../Plan.md#phase-1-scope-not-yet-built).
