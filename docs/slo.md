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

### Re-measured for M32.5 (2026-08-01)

[M32.5](build-notes/phase-details/m32.5.md) is the first milestone to put a
decision on the redirect path itself. Everything measured above was cached
lookup and response; this adds a policy check and, when blocking is on, a pass
over the user-agent string on every request.

So the run was configured to exercise the expensive branch rather than the cheap
one. `bot_blocking` was set to `on` for all 100,000 seeded links before the
warm-up, both cache tiers were emptied, and the container restarted — so every
one of the 240,001 measured requests resolved the precedence rule to "block" and
then ran `analytics.Classify` against its user agent. k6 identifies itself as
`k6/2.1.0 (https://k6.io/)`, which the classifier does not read as automated, so
each request went on to redirect: verified before the run at
`http://app:8080/ld1`, which answered **302** to a browser agent and **403** to
`Mozilla/5.0 (compatible; Googlebot/2.1)` on the same link.

| | Target | Measured |
| --- | --- | --- |
| Cached redirect, server-side | p99 < 20ms | **100% of 240,001 requests under 20ms**; 99.993% under 1ms; 99.982% under 0.5ms |
| Cached redirect, generator-side | — | p99 **1.5ms**, median 481µs, p(95) 937µs |
| Sustained rate | 2,000 rps for 2m | 2,000.14 rps, 240,001 requests, **zero failures** |
| Dataset | 100k links, 5M events | 100,000 links, 5,000,000 events |
| Cache mix | hits only | 240,001 memory, 0 redis, 0 database |
| Redirect pool | — | 0 acquire waits |
| Blocking | on for every measured link | `bot_blocking = 'on'` × 100,000 |

Taken on image `sha256:2eb5fbe5008648a…` (`linkctrl:test`, built 2026-08-01
from the M32.5 working tree at `v0.1.0-45-g2a9cde1-dirty`) on the same
Windows 11 / Docker Desktop / WSL2 host as every figure above.

**The gate is not visible in the result.** Six runs now read 100%, 100%, 100%,
99.991%, 100% and 100% under 20ms, and this one is the second-tightest at the
half-millisecond line. That is what a pure function over two struct fields and a
substring scan should cost against a 20ms budget — but "should" is what this
document exists to replace, and it is now measured with the work switched on
rather than argued from the shape of the code.

The reason there is nothing to see is the design rather than the arithmetic. The
decision reads two fields the cached snapshot already carries, because the
domain's policy is denormalized into each link's snapshot by the resolver's one
query; the alternatives — a per-request domain lookup, or a second cache with
its own invalidation — would both have shown up here.
`TestBlockingCostsNoQueryOnTheRedirectPath` asserts the same thing structurally,
by counting what reaches Postgres: one query for an uncached redirect, zero for
a cached one.

### Re-measured for M33 (2026-08-02)

[M33](build-notes/phase-details/m33.md) is the first milestone to do string
surgery on the path a visitor is redirected to. M32.5 added a decision; this
adds work — a slice out of the escaped path, a scan of its segments, a
concatenation, and a URL that has to be re-emitted without being re-encoded.

Two cached runs, on the same image, differing **only in the shape of the
request**, because that is the comparison that isolates what this milestone
added. `forward_path` was set to `true` for all 100,000 seeded links for both,
so the difference is not the setting but whether there is a path to join. Both
cache tiers were emptied and the container restarted before each.

| | Target | Bare alias (`/ld42`) | Deep link (`/ld42/deep/segments`) |
| --- | --- | --- | --- |
| Cached redirect, server-side | p99 < 20ms | **100% of 239,999 under 20ms**; 239,998 of them under 0.5ms | **100% of 240,002 under 20ms**; 240,001 of them under 0.5ms |
| Cached redirect, generator-side | — | p99 **149.76µs**, median 85.97µs, p(95) 116.12µs | p99 **162.54µs**, median 90.37µs, p(95) 122.27µs |
| Sustained rate | 2,000 rps for 2m | 2,000.0 rps, zero failures | 2,000.0 rps, zero failures |
| Cache mix | hits only | 239,999 memory, 0 redis, 0 database | 240,002 memory, 0 redis, 0 database |
| Redirect pool | — | 0 acquire waits | 0 acquire waits |

**Thirteen microseconds of difference is not a finding.** The gap between the two
columns at p99 is comfortably inside the run-to-run spread this document has
already recorded between identical configurations — an earlier pair of runs on a
previous build of the same code read 144µs and 155µs, so the difference moved by
more between builds than it does between request shapes. It is quoted only to say
that the joiner did not cost something visible.

What the pair does establish is that the work stayed off the parts of the path
that could have been expensive: the cache mix is 100% memory in both, so no
request in either run touched Redis or Postgres, and the pool waited zero times.
Joining a path reads the request and the snapshot the resolver had already
produced, and asks nothing.

The uncached path, run with the same deep-link request shape:

| | Target | Measured |
| --- | --- | --- |
| Uncached redirect, generator-side | p99 < 100ms | p99 **421.47µs**, at 500 rps for 1m, zero failures |
| Cache mix | mostly misses | 24,528 database, 3,867 redis, 1,606 memory |
| Redirect pool | — | 5 acquire waits, 0.021s total |

Verified live on the same image before the measured runs, so the branch really
was the one being timed: `/ld1` answered `302` to `https://example.com/seed/1`,
`/ld1/deep/segments` answered `302` to `https://example.com/seed/1/deep/segments`,
and `/ld1/%2e%2e/%2e%2e/admin` answered **404** — the traversal refusal, on the
running server rather than only in a unit test.

Taken on image `sha256:99dcdebbdf08722ec…` (`linkctrl:test`, built 2026-08-02
from the M33 working tree at `v0.1.0-65-g6f95079-dirty`) on the same
Windows 11 / Docker Desktop / WSL2 host as every figure above. The image was
rebuilt and every figure retaken after a late refactor of the handler, because a
number measured on a binary that is not the one being committed is not a
measurement of it. Eight cached runs now read 100%, 100%, 100%, 99.991%, 100%,
100%, 100% and 100% under 20ms.

The `SUFFIX` environment variable in the reproduction recipe below is what makes
this repeatable. Without it the generator can only ask for bare aliases, and a
"re-measured for M33" section would have been measuring the path this milestone
did not change.

### Re-measured for M34 (2026-08-02)

[M34](build-notes/phase-details/m34.md) is the phase's largest redirect-path
change. Everything before it added a decision (M32.5) or string surgery (M33);
this adds an ordered walk of a rule list, user-agent classification, clock
evaluation against a real timezone, an optional MaxMind lookup, and — for the
returning-visitor condition — **one Redis round trip on the request path**. That
last one is the first optional dependency this path has ever consulted per
request, and it is the reason this section reports a difference where the
previous four reported none.

Two cached runs on the same image, differing **only in whether the links carry
rules**, because that is the comparison that isolates what this milestone added.
Both cache tiers were emptied and the container restarted before each.

**The ruled configuration is deliberately the expensive one.** All 5,000 hot
aliases were given three rules apiece, arranged so that no request
short-circuits early:

| Priority | Conditions | What it costs, per request |
| --- | --- | --- |
| 5 | `device: [mobile]` | a `Classify` pass over the user agent, then a miss |
| 10 | `device: [unknown]`, `browser: [Other]`, `os: [Other]`, a seven-day 00:00–23:59 window in `Europe/London`, `city: [Nowhere]` | passes all four cheap tests, evaluates the window against the request's own clock, reaches the geographic test and misses |
| 20 | `returning: true` | **a Redis `SISMEMBER`** — and it *matches*, so the request is routed to the rule's destination rather than the link's |

That the third rule matched is not incidental: it means every one of the 240,000
measured requests performed the Redis lookup and got `true`, which is the worst
case rather than a sampled one. Verified on the running server before the runs —
`/ld0` answered `https://example.com/seed/0` on a first visit and
`https://example.com/seed/ld0/returning` on the next, once the click pipeline
had written the visitor into the day's set. Afterwards, Redis held 5,000
`lc:rv:` sets, each with a TTL of about six and a half hours, which is the end of
the UTC day plus an hour.

| | Target | **Links with three rules** | **The same links, rules removed** |
| --- | --- | --- | --- |
| Cached redirect, server-side | p99 < 20ms | **100% of 240,002 under 20ms**; 99.934% under 0.5ms | **100% of 240,000 under 20ms**; **100% under 0.5ms** |
| Cached redirect, generator-side | — | p99 **299.92µs**, median 132.56µs, p(95) 174.57µs | p99 **136.94µs**, median 82.24µs, p(95) 111.84µs |
| Sustained rate | 2,000 rps for 2m | 2,000.0 rps, zero failures | 2,000.0 rps, zero failures |
| Dataset | 100k links, 5M events | 100,000 links, 5,000,000 events | same |
| Cache mix | hits only | 240,002 memory, 0 redis, 0 database | 240,000 memory, 0 redis, 0 database |
| Redirect pool | — | 0 acquire waits | 0 acquire waits |

**The difference is real this time, and it is the round trip.** About 163µs at
p99 and about 50µs at the median, against a 20ms budget — two orders of
magnitude of margin, and the first time a Phase 2 milestone's added work has been
visible in this document at all. It is reported rather than smoothed over
because the shape is what matters: the ruled column's tail is where a Redis that
slows down would show up first, and a later regression should be compared
against a number that was actually observed.

The right way to read the two columns is that **they describe two different
links, not two different builds**. A link with no rules is the second column: its
cached payload is byte-for-byte what it was before this milestone, rule
evaluation never begins, and neither Redis nor the MaxMind reader is consulted.
That is the *unchanged fast path* m34.md asks to be proved, and the second column
is the proof — 100% of its requests under half a millisecond, which is the
tightest cached figure in this document. An earlier run of the same
configuration read 99.919% under 0.5ms with p(95) at 181.76µs, so the spread
between identical ruled runs is a few tens of microseconds.

A first cached run of the ruled configuration, before the p99 line was captured,
read 100% of 240,003 requests under 20ms with p(95) 181.76µs — quoted so the
ruled column is two runs rather than one.

The uncached path, with the rules back in place, so the new `LEFT JOIN LATERAL`
that fetches them is exercised on every resolve:

| | Target | Measured |
| --- | --- | --- |
| Uncached redirect, generator-side | p99 < 100ms | p99 **481.48µs**, median 286.97µs, at 500 rps for 1m, zero failures |
| Cache mix | mostly misses | 25,948 database, 2,648 redis, 1,405 memory |
| Redirect pool | — | 4 acquire waits, 0.020s total |

The lateral is not visible there either, which is the claim it was designed
around: rules arrive inside the query the resolver was already issuing, through
the partial index on `(link_id, priority) WHERE enabled` that has existed since
the table was created dormant. `TestALinkWithoutRulesTakesTheUnchangedFastPath`
asserts the same thing structurally, by counting what reaches Postgres — one
query for an uncached redirect whether or not the link has rules, and zero for a
cached one.

Taken on image `sha256:9f55c12dc58e99…` (`linkctrl:test`, built 2026-08-02 from
the M34 working tree at `v0.1.0-67-g3ffebb0-dirty`) on the same Windows 11 /
Docker Desktop / WSL2 host as every figure above. Eleven cached runs now read
100%, 100%, 100%, 99.991%, 100%, 100%, 100%, 100%, 100%, 100% and 100% under
20ms.

#### What this run did **not** measure, and the number that covers it

**No MaxMind database was mounted.** The priority-10 rule's `city` condition
therefore resolved to "no answer" without a lookup, so the figures above include
rule evaluation, the user-agent classifier, the clock and the Redis round trip —
and **not** the cost of an mmap walk. That cost is measured separately, and this
is where D48 lands.

`TestCityLookupCostFitsTheRedirectBudget` in `internal/geoip` times `City()` and
`Region()` against **`internal/geoip/testdata/city-test.mmdb`**, which is
**synthetic**: 33,000 networks scattered across reserved IPv4 space
(`240.0.0.0/4` at /32 depth, `100.64.0.0/10` at /24), unique-local IPv6
(`fc00::/7` at /48) and the documentation ranges, plus a handful of named
entries. **469,455 nodes, 2,847,886 bytes.** Every network in it is
documentation, reserved or private space and every place name is invented, so
nothing in it is a claim about a real location.

Measured on this host: **City() 80ns per lookup, Region() 82ns**, over 20,000
lookups spread across all three address families after warming the mapping.

Against a 20ms budget, and against the ~146µs a whole cached redirect took
above, an 80ns lookup is not a cost this path can feel. **But the database it
was measured against is not the one an operator will deploy.** GeoLite2-City is
roughly twenty times this file and does not fit in a CPU cache, so its tree walk
will fault more often. The honest statement of *measured, not assumed* here is
**measured against a stated database**, and the residue — the cost against a real
GeoLite2-City — remains unmeasured and is recorded as such in
[Plan.md](../Plan.md#known-limitations) rather than reported as a number. D48 is
the decision that chose this over the alternatives; there is no GeoLite2-City on
this project's machines and one cannot be committed.

### Re-measured for M35 (2026-08-03)

[M35](build-notes/phase-details/m35.md) puts four gates in front of a link, and
one of them does something no earlier milestone did: **a synchronous Postgres
write on the redirect path**. So this section reports two numbers rather than
one, and the milestone asks for exactly that split — the SLO's claim is about
**ungated** links, and gated-path latency is measured and documented separately
because the extra write is paid only by links that asked for it.

Two cached runs on the same image, differing **only in whether the seeded links
carry a click budget**. Both cache tiers were emptied and the container
restarted before each.

| | Target | **Ungated** | **Every link click-limited** |
| --- | --- | --- | --- |
| Cached redirect, server-side | p99 < 20ms | **100% of 240,001 under 20ms**; **100% under 0.5ms** | 99.743% of 240,002 under 20ms; 99.480% under 10ms; 91.175% under 5ms; 34.281% under 2.5ms; **0% under 1ms** |
| Cached redirect, generator-side | — | avg **88.44µs**, median 84.25µs, p(95) 113.31µs, max 8.22ms | avg **3.38ms**, median 3.15ms, p(95) 5.61ms, max 43.72ms |
| Sustained rate | 2,000 rps for 2m | 2,000.0 rps, 240,001 requests, zero failures | 2,000.0 rps, 240,002 requests, zero failures |
| Dataset | 100k links, 5M events | 100,000 links, 5,000,000 events | same |
| Cache mix | hits only | 240,001 memory, 0 redis, 0 database | 240,002 memory, 0 redis, 0 database |
| Redirect pool | — | 0 acquire waits | 0 acquire waits |

**The ungated column is the SLO and it is met with the largest margin this
document has recorded**: 100% of requests under half a millisecond, against a
20ms budget. That is the claim m35.md asks to be re-verified, and the reason it
holds is structural rather than lucky. `Snapshot.Gated()` is one boolean
expression over fields the resolver already produced, and it is false for every
link on a default instance — so an ungated link reaches none of the gate code,
performs no extra query and writes nothing.

**The gated column is the honest cost, and it is not small.** About 3.3ms at the
median against 84µs ungated: **roughly forty times slower**, and 0.257% of
requests — 616 of 240,002 — landed over the 20ms line. This is the first
configuration in this document that would fail the SLO if the SLO were stated
over it, which is precisely why it is stated over cached *redirects* on a
default instance and why this milestone was required to measure the gated path
separately rather than fold it into one figure.

The cause is not in doubt and needs no profile. Every one of those 240,002
requests performed a synchronous `INSERT … ON CONFLICT DO UPDATE` against
`link_click_budget`, and the 5,000 hot aliases means 5,000 rows taking about 48
updates each inside two minutes. The measurement afterwards: 5,000 rows,
245,003 total clicks consumed, a maximum of 76 on one row. Concurrent requests
for the same alias serialise on that row lock, which is exactly the property
that makes a one-time link one-time — the latency is the guarantee being paid
for, not overhead beside it.

Read the two columns as **two different links, not two different builds**. A
link with no gate is the first column and its cached payload is byte-for-byte
what it was before this milestone existed.

#### What this run did **not** measure

**The password gate.** Verifying one is an argon2id derivation, deliberately
~50ms of CPU at the configured parameters, plus a Postgres read — and it happens
on a POST that k6 cannot drive against 5,000 distinct aliases without 5,000
distinct passwords. Its cost is the argon2 cost, which config validation already
floors at RFC 9106's 19456 KiB, and it is bounded by the hasher's own semaphore
and by `LINKCTRL_LINK_PASSWORD_RATE_LIMIT` rather than by anything measured
here. A *visit* to a password link — the challenge page — is embedded bytes and
no query, so it is cheaper than a redirect.

**The signature gate.** It is an HMAC-SHA256 over four short strings against a
key already in the process, and the workspace secret is cached in process for a
minute — so at steady state it costs no I/O at all. It was not run because the
load generator appends a fixed path suffix and a signature is per alias, so
5,000 hot aliases would need 5,000 distinct query strings the script has no way
to produce. What is worth saying rather than guessing is that it performs no
write and, after the first request per workspace, no read.

Taken on image `sha256:54db9f1d227d1e…` (`linkctrl:test`, built 2026-08-03 from
the M35 working tree at `v0.1.0-68-g5409e94-dirty`).

**This is a different host from every figure above it**, and that is worth
stating rather than leaving to be inferred: the runs from the first measurement
through M34 were taken on Windows 11 with Docker Desktop on WSL2, and these two
were taken on Linux with Docker running natively. The ungated column is the
tightest cached figure in this document, and some of that is the host rather
than the code. **The comparison that carries the milestone's claim is between
the two columns**, which were run minutes apart on the same machine and the same
image — not between the ungated column and M34's. Anybody reading a
milestone-over-milestone trend out of this section is reading a change of
hardware.

Thirteen cached runs now read 100%, 100%, 100%,
99.991%, 100%, 100%, 100%, 100%, 100%, 100%, 100%, 100% and 99.743% under 20ms
— the last of those being the gated configuration, which is a different path
rather than a regression in this one.

### Re-measured for M36 (2026-08-03)

[M36](build-notes/phase-details/m36.md) divides a link's traffic between several
destinations, and the two kinds it ships cost wildly different amounts. So this
section reports **three** columns rather than one: a link with no split, the same
links carrying a **weighted** split, and the same links carrying a **sequential**
one. The split is the point — a weighted arm is arithmetic over the cached
snapshot and a sequential arm is a synchronous Postgres write, and averaging them
into one figure would hide the only number an operator needs.

Three cached runs on the same image, differing **only** in what the seeded links
carry. Both cache tiers were emptied and the container restarted before each,
because a snapshot written under one configuration carries it.

They were run in the order sequential, weighted, then no split, each run adding
its own 240,000 clicks to the table — so the **baseline column ran against the
largest dataset of the three**, which is the direction that cannot flatter it.
The columns are presented in the order they are easiest to read rather than the
order they were taken.

| | Target | **No split** | **Weighted, 3 arms** | **Sequential, 3 arms** |
| --- | --- | --- | --- | --- |
| Cached redirect, server-side | p99 < 20ms | **100% of 240,001 under 20ms**; **100% under 0.5ms** | **100% of 240,001 under 20ms**; **100% under 0.5ms** | 99.505% of 239,984 under 20ms; 98.684% under 10ms; 92.141% under 5ms; 38.761% under 2.5ms; **0% under 1ms** |
| Cached redirect, generator-side | — | avg **97.89µs**, median 90.74µs, p(95) 125.23µs, max 10.93ms | avg **91.96µs**, median 86.67µs, p(95) 117.47µs, max 6.81ms | avg **3.38ms**, median 3.09ms, p(95) 5.60ms, max 59.27ms |
| Sustained rate | 2,000 rps for 2m | 2,000.0 rps, 240,001 requests, zero failures | 2,000.0 rps, 240,001 requests, zero failures | 1,999.8 rps, 239,984 requests, zero failures, 17 dropped iterations |
| Dataset | 100k links, 5M events | 100,000 links; the click table grew from 6.2M to 7.2M rows across the three runs, having been carried past the seeded 5M by the load runs themselves | same, 15,000 arms on the 5,000 hot aliases | same |
| Cache mix | hits only | 240,001 memory, 0 redis, 0 database | 240,001 memory, 0 redis, 0 database | 239,984 memory, 0 redis, 0 database |
| Redirect pool | — | 0 acquire waits | 0 acquire waits | 0 acquire waits |

**The weighted column is indistinguishable from no split at all**, and it is
slightly faster than the baseline — 91.96µs against 97.89µs at the mean — which
is run-to-run noise rather than an improvement and should be read as "the same
number twice". That is the claim m36.md makes and it is structural: a weighted
arm is chosen by summing three integers already inside the cached snapshot,
drawing one random number below the total, and walking. No query, no lock, no
shared state — `math/rand/v2`'s top-level source is per-P and takes no mutex, so
100 concurrent VUs contend for nothing.

**The sequential column is the cost D8 accepted, measured.** About 3.1ms at the
median against 87µs weighted: **roughly thirty-six times slower**, and 0.495% of
requests — 1,187 of 239,984 — landed over the 20ms line. Every one of those
requests performed a synchronous `INSERT … ON CONFLICT DO UPDATE` against
`link_click_budget`, and 5,000 hot aliases means 5,000 rows taking about 48
updates each inside two minutes; the run advanced the rotation counters 239,984
times across those rows. Concurrent requests for the same alias serialise on that
row lock, and that serialisation **is** the strict global order the decision
asked for — the latency is the guarantee being paid for, not overhead beside it.

It is also the one column that could not hold the offered rate: k6 dropped 17
iterations and landed at 1,999.8 rps rather than 2,000.0. That is the same thing
the latency says, seen from the generator's side.

This is the same shape and very nearly the same magnitude as [M35](#re-measured-for-m35-2026-08-03)'s
gated column (3.38ms median, 0.257% over 20ms), which is what it should be: it is
the same table, the same upsert, the same row lock, in a different column. Two
features, one durable-counter cost, paid only by the links that ask for it.

**A link that asks for neither pays nothing**, and the first column is the SLO's
own claim re-verified on this build: 100% of cached redirects under half a
millisecond against a 20ms budget. `Snapshot.Split()` returns on a length check
over a slice that is nil for every link on a default instance.

#### What changed under the first column, and why it still reads the same

M36 bumped `CacheKeyVersion` to v3 and changed the cached destination list from
`[]string` to a list of objects carrying an id and a weight. Neither is visible
here, and the reason is that both fields are `omitempty`: a link with no rules
and no split encodes to exactly the bytes it encoded to before, which
`TestALinkWithoutArmsEncodesAsItDidBefore` asserts on the payload rather than
inferring from the latency.

The bump itself costs a cold cache once, on upgrade. It does not appear in this
measurement because every run above flushed both tiers deliberately, which is
what all sixteen cached runs in this document have done.

#### What this run did **not** measure

**A fallback.** It is one more entry in the same slice and one more string
comparison in the same walk, reached only when no rule matched and no arm was
chosen. There is no configuration in which it costs more than the weighted
column, so a fourth run would have measured the same number a fourth time.

**The interaction between a sequential split and a click budget.** They are two
upserts against the same row in the same statement-per-request shape, so a link
carrying both pays the sequential column's cost twice. Nothing was run for it
because the arithmetic is not in doubt and the combination is rare; what *is*
asserted, in `TestSequentialAndAClickBudgetDoNotShareACounter`, is that the two
counters are separate columns — sharing one would have made a one-time link with
a rotation dead on its first visit.

Taken on image `sha256:210c29aba796ac…` (`linkctrl:test`, built 2026-08-03 from
the M36 working tree at `v0.1.0-69-g541b568-dirty`). **The image was rebuilt from
the tree immediately before these runs and the three runs share it**, which is
the whole reason the figures below the table can be compared with each other —
an earlier draft of this section was taken on an image that predated a change to
`internal/redirect/snapshot.go` and has been replaced rather than amended.

**Same host as [M35](#re-measured-for-m35-2026-08-03) and a different one from
everything above it** — Linux with Docker running natively, rather than the
Windows 11 / Docker Desktop / WSL2 machine that produced the runs from the first
measurement through M34. The comparison that carries this milestone's claim is
**between these three columns**, which were run minutes apart on the same machine
and the same image. The first column is also directly comparable to M35's ungated
column (97.89µs against 88.44µs at the mean, same host, an hour apart on a click
table that these runs have since grown), and that comparison is offered because
it is like-for-like; nothing here should be read against M34's figures or
anything earlier.

Sixteen cached runs now read 100%, 100%, 100%, 99.991%, 100%, 100%, 100%, 100%,
100%, 100%, 100%, 100%, 99.743%, 100%, 100% and 99.505% under 20ms — the last of
those being the sequential configuration and the one before the gated one, both
different paths rather than regressions in this one.

## Reproducing it

```sh
docker compose up -d --wait      # must be built from the code under test
make seed-slo                    # 100k links, 5M click events, ~90s
make load                        # cached, 2,000 rps, 2 minutes
make load-uncached               # spread across the whole dataset
```

`make seed-slo` needs a workspace to seed into, so the instance has to be
claimed first — `POST /api/v1/auth/setup` — which a fresh `make rebuild` undoes.

`SUFFIX` appends path segments to every measured request, for exercising
deep-link forwarding (M33). It is empty by default, so every measurement above
that predates it was taken with the request shape it describes:

```sh
docker compose exec postgres psql -U linkctrl -d linkctrl \
  -c "UPDATE links SET forward_path = true WHERE alias LIKE 'ld%'"
docker compose exec redis redis-cli FLUSHALL
docker compose restart app       # the in-process tier is not flushed by FLUSHALL
SUFFIX=/deep/segments make load
```

The update and the restart are not optional decoration. Without the column set
every request answers 404, and k6's `is 302` check fails the run rather than
quietly measuring the miss path; without the flush and the restart the warm-up
would populate the tiers from snapshots written before the column changed.

The click budget (M35) is switched on with SQL, and the ceiling is set far
higher than the run can reach so that no request is refused rather than timed:

```sh
docker compose exec postgres psql -U linkctrl -d linkctrl \
  -c "UPDATE links SET max_clicks = 100000000 WHERE alias LIKE 'ld%'"
docker compose exec redis redis-cli FLUSHALL
docker compose restart app
make load
```

A smaller ceiling would measure the 410 path, which costs a failed predicate and
no write — the opposite of what the gated column is for. The comparison run is
the identical aliases with `max_clicks` set back to NULL, and the same flush and
restart apply for the same reason: the snapshots written while the column was
set carry the gate.

Routing rules (M34) are seeded with SQL rather than with a flag, because what
the measurement needs is a *specific arrangement* of rules — one that no request
short-circuits early — rather than "some rules". The three used above are listed
in that section's table; they are inserted for `ld0`–`ld4999`, each with a
`destinations` row of its own at `position = 1`, and the same flush and restart
apply for the same reason. The comparison run is the identical aliases with
`DELETE FROM routing_rules` and the rule destinations removed, so the only thing
that changes between the two columns is whether the links carry rules.

Split testing (M36) is seeded with SQL for the reason routing rules are: what
the measurement needs is a *specific arrangement*, not "some arms". Three arms
per alias, weighted 60/30/10, on `ld0`–`ld4999`, each a `destinations` row above
position 0 with a `routing_rules` row of the matching kind pointing at it:

```sh
docker compose exec postgres psql -U linkctrl -d linkctrl <<'EOF'
INSERT INTO destinations (id, link_id, workspace_id, url, url_host, position, weight)
SELECT gen_random_uuid(), l.id, l.workspace_id,
       'https://slo.example.com/arm' || a.n || '/' || l.alias,
       'slo.example.com', a.n, a.w
  FROM links l
 CROSS JOIN (VALUES (1, 60), (2, 30), (3, 10)) AS a(n, w)
 WHERE l.alias ~ '^ld[0-9]{1,4}$' AND substring(l.alias from 3)::int < 5000;

INSERT INTO routing_rules (id, link_id, workspace_id, destination_id, priority, conditions, kind, enabled)
SELECT gen_random_uuid(), d.link_id, d.workspace_id, d.id, 100, '{}'::jsonb, 'weighted', true
  FROM destinations d WHERE d.position > 0 AND d.url_host = 'slo.example.com';
EOF
docker compose exec redis redis-cli FLUSHALL
docker compose restart app
make load
```

The sequential column is the identical rows with `UPDATE routing_rules SET kind =
'sequential'`, and the same flush and restart apply for the same reason: the
snapshots written under the previous kind carry it. The comparison run is the
same aliases with both inserts undone.

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
