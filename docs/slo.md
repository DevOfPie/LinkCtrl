# The redirect SLO, measured

The redirect latency target is the one number this project makes a promise about.
This document is that promise, the measurement, and what the measurement found.

## What the target says

From [Plan.md](../Plan.md#performance-targets), unchanged since it was written:

> **Server-side p99, cache hits only, measured from a load generator on the same
> Docker network, excluding client RTT and TLS, at 2,000 rps sustained for 2
> minutes, with 100k links and 5M click events seeded.** Both the generator's
> number and the server histogram are reported.

Each clause is doing work. Cache hits only, because an uncached redirect has a
different budget and mixing them produces a number that describes neither. On the
same Docker network, because measuring through a published port on Docker Desktop
measures the WSL2 bridge — this project has recorded ~13ms that way for an
operation the server completes in microseconds. Seeded, because index depth,
planner choices and cache behaviour all change with size, and a load test against
an empty database measures an empty database.

## Result

**Met, with two orders of magnitude of margin.**

| | Target | Measured |
| --- | --- | --- |
| Cached redirect, server-side | p99 < 20ms | **100% of 240,001 requests under 20ms**; 99.999% under 1ms; 99.997% under 0.5ms |
| Cached redirect, generator-side | — | p99 **961µs** (three runs: 961µs, 1.08ms, 1.18ms) |
| Sustained rate | 2,000 rps for 2m | 2,000.08 rps, 240,001 requests, **zero failures** |
| Dataset | 100k links, 5M events | 100,000 links, 5.7M events |
| Cache mix | hits only | 240,001 memory, 0 redis, 0 database |
| Redirect pool | — | 0 acquire waits |

Three consecutive runs of the same configuration produced generator p99s of 961µs,
1.08ms and 1.18ms, and 100% under 20ms server-side every time. The spread is the
honest precision of this measurement; a single figure quoted to three digits would
not be.

### Re-measured for M23 (2026-07-31)

[M23](build-notes/phase-details/m23.md) added a Redis pub/sub subscriber that
clears the in-process cache when another replica publishes an invalidation, so
the measurement was repeated on an image built from that code.

| | Target | Measured |
| --- | --- | --- |
| Cached redirect, server-side | p99 < 20ms | **99.991% of 239,193 requests under 20ms**; 99.93% under 10ms; 99.81% under 0.5ms |
| Cached redirect, generator-side | — | median 674µs, p(95) 3.34ms |
| Sustained rate | 2,000 rps for 2m | 1,993.4 rps, 239,193 requests, zero failures |
| Cache mix | hits only | 239,193 memory, 0 redis, 0 database |
| Redirect pool | — | 0 acquire waits |

**The target is met with the same order of margin**: p99 is under 500µs, against
a 20ms budget.

The honest difference from the runs above is the tail. Twenty-one requests
(0.009%) landed over 20ms where the earlier runs had none. That is not a claim
that the subscriber cost anything — it is one run on a developer laptop against
three, the generator also dropped 808 iterations trying to hold the rate, and
this document already says the spread between identical runs is the real
precision of the method. It is recorded rather than smoothed over because a
later regression should be compared against what was actually observed.

What the run does establish is that the subscriber is off the request path: the
cache mix is 100% memory, so every measured request was answered without
touching Redis or Postgres, and the pool waited zero times.

### Re-measured for M24 (2026-07-31)

[M24](build-notes/phase-details/m24.md) put the credential and API limits in
Redis. The 404-probe limiter deliberately stayed in process, so the redirect
path gained one nil comparison and nothing else, but the rule is to re-measure
whenever that path is touched at all.

**100% of 240,010 requests under 20ms**; 99.98% under 0.5ms, 99.999% under 5ms.
2,000 rps sustained, zero failures, 240,010 memory hits with no Redis or
database reads, zero pool acquire waits.

This is also the cleanest evidence about the M23 tail above: the same seeded
dataset and the same machine, with one more change layered on, produced no
requests over 20ms at all. Five runs now read 100%, 100%, 100%, 99.991% and 100%,
which is what run-to-run spread looks like rather than a regression.

The uncached path, which the plan targets at <100ms:

| | Target | Measured |
| --- | --- | --- |
| Uncached redirect, generator-side | p99 < 100ms | p99 **1.92ms**, median 684µs, at 500 rps for 1m |
| Cache mix | mostly misses | 21,107 database, 7,454 redis, 1,440 memory |

Both numbers were taken on one developer machine — Windows 11, Docker Desktop on
WSL2, Postgres and Redis in containers beside the application. **This is a
verification that the design meets its target, not a capacity statement for a
server.** A production host with real storage should do better; the shape of the
result, not its absolute value, is what transfers.

## Reproducing it

```sh
docker compose up -d --wait      # must be built from the code under test
make seed-slo                    # 100k links, 5M click events, ~90s
make load                        # cached, 2,000 rps, 2 minutes
make load-uncached               # spread across the whole dataset
```

`scripts/load-test.sh` reports both halves of the measurement. The server's
histogram is cumulative since boot, so it is sampled before and after and
reported as a delta; the warm-up is a separate k6 invocation that finishes before
the first snapshot, so the delta covers exactly the measured window. Without that,
a "cached" measurement quietly includes every cold read the warm-up performed.

The script also prints the cache mix and the redirect pool's acquire waits. Read
them before believing the latency: a cached measurement with database reads in it
is not a cached measurement, and a starved pool is the difference between "the
query was slow" and "the request never got a connection".

## What the measurement found

A load test earns its cost in findings, not in numbers that confirm what you
already believed. Four.

### The two-tier cache and the split pool do the job they were built for

Every one of these measurements was taken while the analytics rollup was
recomputing whole days from 5.7M events — a 19-second query, every 60 seconds, on
the same Postgres. The cached path did not notice: zero database reads, zero pool
waits, 100% under 20ms. The dedicated redirect pool and the in-process cache exist
precisely so that heavy analytics cannot reach the hot path, and under load that
separation held.

### The dimension rollup is the real bottleneck, and it is not the scan

`RollupDimensionDaily` takes **16–21 seconds and runs every 60 seconds** on this
dataset. That is the dominant load on the database, and it grows with traffic
until the job cannot finish inside its own interval.

The obvious cause was the scan count: the query read `click_events` six times,
once per dimension. It was rewritten to read once and expand each row into its six
dimension rows with `CROSS JOIN LATERAL (VALUES ...)`, and the wall clock did not
move — ~20s either way. That is the finding, not a disappointment.

`EXPLAIN (ANALYZE, BUFFERS)` says where the time actually goes. Six-branch version:

```
Insert on link_dimension_daily
  Conflicting Tuples: 553053
  Buffers: shared hit=7798814 read=216135, temp read=117836 written=118007
  ->  GroupAggregate (actual rows=553053)
        ->  Sort (actual rows=6219888)
              Sort Method: external merge  Disk: 471344kB
```

One-pass version:

```
Insert on link_dimension_daily
  Conflicting Tuples: 553053
  Buffers: shared hit=8448446 read=105257 dirtied=22378
  ->  GroupAggregate (actual rows=553053)
        ->  Incremental Sort (actual rows=4990656)
              Presorted Key: ce.link_id
              Sort Method: quicksort  Average Memory: 30kB  Peak Memory: 152kB
```

Two things follow. **The time is in the upsert**: 553,053 output rows, every one a
conflicting tuple, and of ~8M buffer hits the aggregate accounts for under a
million. Re-deriving and rewriting every `(link, day, dimension, value)` tuple on
every run is inherent to recomputing whole days — a deliberate choice, because an
incremental "add what arrived since the watermark" design double-counts on retry
and, once it drifts, stays wrong invisibly.

**And the six-branch version was spilling 471 MB of temp files per run.** Reading
once lets the sort use the index's `link_id` ordering and run incrementally in
memory instead. Wall clock is identical because the disk on this host absorbs the
spill; on a smaller or busier one it would not, and half a gigabyte of temp I/O
every 60 seconds is worth removing on that evidence alone. So the rewrite is kept —
but as a resource fix, not a solution to the job's cost. Nobody should read it as
having addressed the 20 seconds.

The actual fix is narrowing the recompute window, running the dimension rollup on a
longer cadence than the per-link and per-workspace totals, or accepting the cost
with an alert on job staleness. That decision is recorded as an open item in
[Plan.md](../Plan.md#known-limitations) rather than made here.

### A failed resolve was answering 404, which is a lie

At 500 rps uncached, an early run failed 38.7% of requests: the redirect pool
queued 1,798 acquires totalling 229 seconds, the 250ms `REDIRECT_TIMEOUT` fired,
and every one of those requests was answered **404**.

A 404 is a claim that the link does not exist. It is the same signal this project
deliberately distinguishes from 410 so that crawlers and link checkers stop
retrying — so answering it for a transient timeout tells every one of them to drop
a live link. Resolution failures now answer **503 with `Retry-After: 1`**, which is
true and retryable. The load test is the only reason this was ever visible: at
low traffic that code path never runs.

### k6's `__ITER` is per-VU, which made the first "cached" run 3.7% database reads

The warm-up walked the hot alias set with `__ITER % HOT` across 20 VUs. Each VU has
its own iteration counter, so the warm-up covered the first 250 aliases twenty
times instead of 5,000 aliases once, and the measured window then read ~4,750 of
them from Postgres. The cache mix is what exposed it — the latency looked fine.
Warm-up now runs on a single VU, sequentially, because a warm-up that is
impossible to get wrong is worth more than one that is fast.

The same run showed 9,003 database-tier observations for ~5,000 distinct cold
aliases, which is not a contradiction: `singleflight` collapses concurrent misses
for the same alias into one query, and every waiter is still recorded as having
been served by the database tier. The `cache="database"` counter measures requests
that reached that tier, not queries executed.

## Caveats worth carrying

- **k6 occasionally reported a negative minimum duration** (`min=-1692950ns`) in
  runs on this host. A generator artefact of clock adjustment inside the VM, not a
  server measurement; percentiles are unaffected and the server histogram agrees
  with them.
- **The seeded dataset is not production data.** Links carry their destination URL
  directly with no `destinations` row, and click events carry random visitor hashes
  rather than real HMACs. Resolution is byte-for-byte the same path; the editing
  surface and visitor-uniqueness maths are not exercised. `lctl seed --help` says
  so too.
- **Rate limits are per instance and in memory**, so a load generator hammering
  one address is throttled by `REDIRECT_404_RATE_LIMIT` only if it asks for
  aliases that do not exist. Every alias in the load test exists, and hits are
  never charged — which is why 240,001 requests from one address pass cleanly. A
  load test that used random *nonexistent* aliases would be measuring the limiter.
