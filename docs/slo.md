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

### Re-measured for M37 (2026-08-03)

[M37](build-notes/phase-details/m37.md) does not touch the redirect path, so
there is no k6 column here. What it changes is the **rollup**, and the number
that needed re-measuring is the one Plan.md's known-limitation row carries: the
dimension breakdowns cost 16–21 seconds of a sixty-second interval, and would
eventually stop fitting inside it.

The fix is the split-cadence option Phase 1 recorded and did not take. The
per-link and per-workspace totals keep sixty seconds; the per-dimension and
per-destination breakdowns move to **fifteen minutes**
(`analytics.DimensionInterval`), each half under its own `job_state` watermark.

**Dataset.** 100,000 links and **5,735,005** click events, of which **800,909**
fall inside the two-day window a run reopens — `make seed-slo` for the five
million, then three `make load` runs of 240,001 requests each, which is how the
earlier ~830k in-window figure was reached too. A plain `make seed-slo` leaves
only ~66k events in that window and would have measured a job with a twelfth of
its documented work to do.

Taken on image `sha256:1845cdc3bcf5…` (`linkctrl:test`, built 2026-08-03 from the
M37 working tree at `v0.1.0-70-gcc551c1-dirty`), rebuilt from the tree
immediately before the runs. Each half was run three times in sequence through
`analytics.Roller` — the code the scheduler calls, not a hand-written copy of its
SQL.

| Pass | Runs | Interval | Duty cycle |
| --- | --- | --- | --- |
| Totals — `link_click_daily`, `workspace_click_daily` | **1.539s, 1.541s, 1.610s** | 60s | **2.6–2.7%** |
| Dimensions — `link_dimension_daily`, including the destination breakdown | **4.801s, 6.259s, 6.023s** | 900s | **0.53–0.70%** |
| Both, as one job on one clock (what shipped before this) | 6.34–7.87s | 60s | 10.6–13.1% |

**The dimension job no longer exceeds its interval, and the margin is the
point.** At 6.26 seconds against 900 it would have to become about **143 times
slower** before it stopped fitting. On the sixty-second clock the same job had a
margin of about 9.6×, and on the host the 16–21s figure came from it had a margin
under 3×. The narrower-window fallback recorded in Plan.md is therefore **not
taken**, and stays recorded for the day the margin is spent.

`EXPLAIN (ANALYZE, BUFFERS)` on the dimension upsert, so the shape can be
compared with the one recorded under
[the rollup finding](#the-dimension-rollup-is-the-real-bottleneck-and-it-is-not-the-scan):

```
Insert on link_dimension_daily (actual time=6731.809..6731.810)
  Conflicting Tuples: 289272
  Buffers: shared hit=5180765 read=19129 dirtied=14612 written=7537
  ->  GroupAggregate (actual rows=289272)
        ->  Incremental Sort (actual rows=4707636)
              Presorted Key: ce.link_id
              Sort Method: quicksort  Peak Memory: 140kB
Execution Time: 6787.653 ms
```

Still no temp files, still in memory, and still dominated by the upsert: 289,272
conflicting tuples out of 5.18M buffer hits, with the aggregate accounting for
797k of them. **Nothing about the query got cheaper** — that was the M34-era
finding and it stands. What changed is how often it runs.

**On comparing this with the 16–21 seconds.** Do not, directly. That figure was
taken on the Windows 11 / Docker Desktop / WSL2 machine that produced every
measurement from the first through M34; the host moved to Linux with native
Docker at [M35](#re-measured-for-m35-2026-08-03) and has not moved since. The
comparison that carries this milestone's claim is the **third row of the table
against the first two**, which are the same job, on the same host, on the same
image, minutes apart. On this host the combined job costs 6.34–7.87s of every
sixty seconds; split, the sixty-second half costs 1.54–1.61s and the expensive
half costs 4.80–6.26s of every fifteen minutes.

**What the split costs, stated rather than buried.** A breakdown on the link
detail page can now be up to fifteen minutes behind the totals on the same page.
That is why M37 also adds `linkctrl_rollup_staleness_seconds`, read from
`job_state.last_success_at` rather than from process memory, with an alert recipe
in [operations.md](operations.md#alerts-worth-having): a job allowed to be a
quarter of an hour late needs a number that says how late it actually is.

### Re-measured for M40 (2026-08-03)

[M40](build-notes/phase-details/m40.md) touches the redirect path, so this is a
k6 run rather than a note. What it adds in front of every request is a **host
lookup**: the router matches the `Host` header against an in-process map of
verified custom hostnames before dispatching, and a request that matches carries
its domain id through the context to the resolver.

**The number to check is that nothing moved**, because the default link host is
the one the SLO is defined against and it takes the *miss* branch of that lookup —
one read lock and one map read, no allocation, no query.

| | M40, cached |
| --- | --- |
| Requests | **240,001** at 2,000 rps for 2m |
| Under 20ms | **100.000%** (240,001 of 240,001) |
| Mean | **87.44µs** |
| Median | 83.65µs |
| p90 / p95 | 105.05µs / 113.30µs |
| Max | 6.84ms |
| Cache mix | memory 240,001 · redis 0 · database 0 |
| Redirect pool waits | 0 |

Taken on image `sha256:2daa92fa3903c6…` (`linkctrl:test`, built 2026-08-03 from
the M40 working tree at `v0.1.0-73-g19fa007-dirty`), rebuilt and recreated from
the tree immediately before the run, against a freshly seeded `make seed-slo`
dataset of 100,000 links and 5,000,000 click events. Same host as
[M35](#re-measured-for-m35-2026-08-03) onward.

**87.44µs against M36's 97.89µs** on the same host and the same dataset size.
That is not an improvement caused by this milestone and should not be read as
one — the click table was freshly seeded here and had been grown by three load
runs when M36's figure was taken. What the comparison supports is the only claim
being made: adding a host lookup to the front of the redirect path did not move
the distribution.

**No custom hostname was in the map during this run**, and that is deliberate
rather than an omission. The SLO is defined against the instance's own link host,
which is the path every deployment uses and the one a regression would matter
most on; a run against a verified custom hostname would additionally pay one
`context.WithValue` and one request clone, which is a cost paid only by traffic
to custom domains. It is worth stating rather than measuring here: that
allocation is on the custom-domain branch and nowhere else, and the seventeen
cached runs this document now records were all taken on the branch that does not
take it.

Seventeen cached runs now read 100%, 100%, 100%, 99.991%, 100%, 100%, 100%, 100%,
100%, 100%, 100%, 100%, 99.743%, 100%, 100%, 99.505% and **100%** under 20ms.

### Re-measured for M41 (2026-08-03)

[M41](build-notes/phase-details/m41.md) touches the redirect path, so this is a
k6 run rather than a note. What it adds is **one read of the query string**:
`clickSource` looks for the reserved `src` parameter, which is what a QR code
encodes so a scan can be told apart from a typed URL
([D76](../Plan.md#phase-2-decisions-taken-after-the-plan-was-finalised)).

**Two request shapes, because this milestone has two branches and the
interesting one is not the default.** A request with no query pays a length check and a
`strings.Contains` over a string already in hand; only a request that could carry
the parameter reaches `url.ParseQuery`, which allocates. So the second shape puts
`?src=qr` on every request — exactly what a scan looks like — and it is the one
that measures the cost being introduced.

| | No query (the default path) | `?src=qr` (a scan) |
| --- | --- | --- |
| Requests | **240,000** at 2,000 rps for 2m | **240,001** at 2,000 rps for 2m |
| Under 20ms | **100.000%** (240,000 of 240,000) | **100.000%** (240,001 of 240,001) |
| Mean | **87.04µs** | **86.58µs** |
| Median | 82.99µs | 82.73µs |
| p90 / p95 | 104.12µs / 112.09µs | 103.39µs / 111.48µs |
| Max | 6.97ms | 8.26ms |
| Cache mix | memory 240,000 · redis 0 · database 0 | memory 240,001 · redis 0 · database 0 |
| Redirect pool waits | 0 | 0 |

Taken on image `sha256:bae11abd25c1a4…` (`linkctrl:test`, built 2026-08-03 from
the M41 working tree at `v0.1.0-74-ge4c5979-dirty`), rebuilt and recreated from
the tree immediately before the run, against a freshly seeded `make seed-slo`
dataset of 100,000 links and 5,000,000 click events. Same host as
[M35](#re-measured-for-m35-2026-08-03) onward.

**87.04µs against M40's 87.44µs**, on the same host, the same dataset size and a
dataset seeded the same way. Nothing moved, which is the only claim being made.

**And the scan path is not slower than the default one** — 86.58µs against
87.04µs, a difference smaller than the spread between two runs of the same
configuration this document already records. That is not a claim that parsing a
query is free; it is the honest reading that the parse is too small to see
against a redirect that already costs 87µs, and that the two figures are the same
number measured twice.

**What was actually run, so the table is not read as more than it is**: three k6
runs, one without a query and two with `?src=qr`. The figures in the `?src=qr`
column are the *second* of those two; the first was let through with only its
histogram captured — 240,001 requests, 100% under 20ms, memory-only — and its
mean was not recorded, so it is counted below and quoted nowhere.

`SUFFIX` is what produced the `?src=qr` runs — it appends to the measured URL, so
`SUFFIX='?src=qr' make load` is a query rather than the path segments
[M33](build-notes/phase-details/m33.md) used it for. The cache key is the alias,
so the query changes nothing about which tier answers, and the mix confirms it:
240,001 memory, zero elsewhere.

Twenty cached runs now read 100%, 100%, 100%, 99.991%, 100%, 100%, 100%, 100%,
100%, 100%, 100%, 100%, 99.743%, 100%, 100%, 99.505%, 100%, **100%**, **100%**
and **100%** under 20ms.

### Re-measured for M35's reopening (2026-08-04)

[M35](build-notes/phase-details/m35.md) was reopened on
[M44.9](build-notes/phase-details/m44.9.md)'s triage for four defects in the
gates, and three of the four are on the redirect path — so the inherited rule
applies and this is a k6 run rather than a note. **Both columns were re-taken**,
not only the SLO's own, because the milestone under repair is the one whose claim
is about gates.

**What actually changed on each path, so the numbers are read against something.**
On the **ungated** path: one method comparison. `h.status()` became
`h.redirectStatus(r)`, which tests `r.Method == http.MethodPost` before returning
the configured status, because a verified link password must be answered `303`
and not a `307` that would have the browser re-send the password body to the
destination. Nothing else — every other change is behind `Snapshot.Gated()`, which
is false for every link on a default instance. On the **gated** path: `passGates`
takes the request's domain id as an argument, a value `ServeHTTP` already held in
a local, so the signature and the password bucket stop keying on the constant
resolved at boot. No query was added, and none was removed.

| | Target | **Ungated** | **Every link click-limited** |
| --- | --- | --- | --- |
| Cached redirect, server-side | p99 < 20ms | **100% of 239,999 under 20ms**; **100% under 0.5ms** | 99.405% of 239,903 under 20ms; 99.028% under 10ms; 92.078% under 5ms; 37.232% under 2.5ms; **0% under 1ms** |
| Cached redirect, generator-side | — | avg **92.76µs**, median 87.91µs, p(90) 109.48µs, p(95) 118.41µs, max 7.64ms, min 38.76µs | avg **3.52ms**, median 3.12ms, p(90) 4.78ms, p(95) 5.58ms, max 94.52ms |
| Sustained rate | 2,000 rps for 2m | 2,000.0 rps, 239,999 requests, zero failures | 1,999.1 rps, 239,903 requests, zero failures, 99 dropped iterations |
| Dataset | 100k links, 5M events | 100,000 links, 5,000,000 events, freshly seeded | same |
| Cache mix | hits only | 239,999 memory, 0 redis, 0 database | 239,903 memory, 0 redis, 0 database |
| Redirect pool | — | 0 acquire waits | 0 acquire waits |

**The ungated column is the SLO and it did not move.** 92.76µs against
[M41](#re-measured-for-m41-2026-08-03)'s 87.04µs and
[M40](#re-measured-for-m40-2026-08-03)'s 87.44µs, on the same host and a dataset
seeded the same way — a few microseconds, which is inside the run-to-run spread
this document has recorded between identical configurations since its first
section. 100% of requests under half a millisecond against a 20ms budget, which
is where it has been for every ungated run on this host.

**The gated column is the same durable-counter cost, measured a third time.**
99.405% under 20ms here, against 99.743% for
[M35](#re-measured-for-m35-2026-08-03)'s own gated run and 99.505% for
[M36](#re-measured-for-m36-2026-08-03)'s sequential column — the same table, the
same `INSERT … ON CONFLICT DO UPDATE`, the same 5,000 hot aliases taking about
forty-eight updates each inside two minutes. Three measurements of one mechanism
landing between 99.4% and 99.75% is the spread of that mechanism, not a
regression in it, and the median is 3.12ms against M35's 3.38ms and M36's 3.09ms.
It is also, again, the one column that could not hold the offered rate: 99
dropped iterations and 1,999.1 rps.

#### What this run did **not** measure, and why

**The HEAD branch, which is the only new I/O in the reopening.** A HEAD to a link
that carries a click budget now performs one primary-key read of
`link_click_budget` — `gate.Service.Budget`, non-consuming — so that an exhausted
link answers 410 instead of publishing its destination to whoever asks with the
cheaper method. k6 sends GET, and the load script's checks are `is 302` and
`has Location`, so nothing above exercises it. It is stated rather than measured
because the shape is not in doubt and the comparison that would matter is not
available: the branch replaces *no query at all* with one indexed read, on a
method that previously did no database work on this path, and there is no
configuration in which it is slower than the `Consume` upsert the same link's GET
already pays — which the right-hand column above prices at 3.12ms. **A GET is
unchanged**, and that is the part worth checking rather than asserting: no read
was put in front of the upsert, so the gated column is comparable with the two
earlier runs of the same configuration, and it is.

**The custom-domain half of the signature fix.** Verifying it needs a verified
custom hostname, which needs DNS this instance cannot publish for itself; it is
covered by `TestASignedLinkWorksOnACustomHostname` and
`TestASignatureDoesNotCrossHostnames` instead. What the load run does establish
about it is that threading the domain id through `passGates` cost nothing
visible, which is what passing a value already in hand should cost.

**The password gate**, for the reason M35's own section gives: it is an argon2id
derivation on a POST that k6 cannot drive against 5,000 aliases without 5,000
passwords.

Verified live on the same image, before this section was written, because a fix
this document reports on should be shown working on the binary the figures came
from rather than only in a test process:

- `/f78live`, a one-time link, answered **302** with
  `https://example.com/secret-destination` to the first GET, **410** to the
  second, and **410 with no `Location`** to three successive `HEAD` requests. That
  third answer is the whole of F78 — it was 302 with the destination, three times
  over, on the build this reopening replaces.
- `/f81live`, a password link, answered **200** to a GET, **200** to a wrong
  password, and **303** to the right one — on an instance configured
  `REDIRECT_DEFAULT_STATUS=302`, which is what makes it evidence that the status
  is unconditional rather than inherited.
- `/f80live`, a signature-gated link, answered **403** unsigned and **302** to the
  URL `POST /api/v1/links/{id}/sign` minted, which named this instance's own link
  host — the correct hostname for a link on the default domain, and the control
  for the custom-domain case the tests cover.

Taken on image `sha256:c7441cad0036…` (`linkctrl:test`, built 2026-08-04 from the
M35-reopening working tree at `v0.1.0-83-g98cc6de-dirty`), rebuilt and recreated
from the tree immediately before the runs, against a freshly seeded
`make seed-slo` dataset. Both cache tiers were emptied and the container
restarted before each column. Same host as
[M35](#re-measured-for-m35-2026-08-03) onward.

Twenty-two cached runs now read 100%, 100%, 100%, 99.991%, 100%, 100%, 100%,
100%, 100%, 100%, 100%, 100%, 99.743%, 100%, 100%, 99.505%, 100%, 100%, 100%,
100%, **100%** and **99.405%** under 20ms — the last of those being the gated
configuration, which is a different path rather than a regression in this one.

### Re-measured for M45's redirect findings (2026-08-04)

Four findings from [M45](build-notes/phase-details/m45.md)'s triage — F87, F96,
F88 and F89 — and three of them touch the redirect path, so the inherited rule
applies and this is a k6 run rather than a note. **Both columns were re-taken**,
because the one change that adds instructions to a hot call adds them to a gated
one.

**What actually changed on each path, so the numbers are read against
something.** On the **ungated** path: nothing at all. `challengePending` is one
boolean expression and it is only reached from inside `split`, which is only
reached by a link that carries arms; `config.CanonicalHost` gained a
`TrimSuffix`, and it runs once per request in the host router rather than per
alias. On the **gated** path: every `internal/gate` query is now wrapped in
`context.WithTimeout` at `REDIRECT_TIMEOUT` — one allocation and one timer per
call, on calls that were already making a database round trip. No query was added
and none was removed.

| | Target | **Ungated** | **Every link click-limited** |
| --- | --- | --- | --- |
| Cached redirect, server-side | p99 < 20ms | **100% of 239,999 under 20ms**; 239,998 of them under 0.5ms | 99.826% of 240,001 under 20ms; 99.513% under 10ms; 92.895% under 5ms; 39.350% under 2.5ms; **0% under 1ms** |
| Cached redirect, generator-side | — | avg **87.33µs**, median 83.64µs, p(90) 104.93µs, p(95) 113.17µs, max 6.49ms, min 37.17µs | avg **3.24ms**, median 3.11ms, p(90) 4.70ms, p(95) 5.55ms, max 48.51ms, min 1.14ms |
| Sustained rate | 2,000 rps for 2m | 2,000.0 rps, 239,999 requests, zero failures, no dropped iterations | 1,999.9 rps, 240,001 requests, zero failures, no dropped iterations |
| Dataset | 100k links, 5M events | 100,000 links, 5,000,000 events, freshly seeded | same |
| Cache mix | hits only | 239,999 memory, 0 redis, 0 database | 240,001 memory, 0 redis, 0 database |
| Redirect pool | — | 0 acquire waits | 0 acquire waits |

**The ungated column is the SLO and it did not move.** 87.33µs against
[M41](#re-measured-for-m41-2026-08-03)'s 87.04µs,
[M40](#re-measured-for-m40-2026-08-03)'s 87.44µs and
[M35's reopening](#re-measured-for-m35s-reopening-2026-08-04)'s 92.76µs, on the
same host and a dataset seeded the same way — inside the run-to-run spread this
document has recorded between identical configurations since its first section.

**The gated column is the fourth measurement of the durable-counter cost, and it
is the best of the four.** 99.826% under 20ms here, against 99.743% for
[M35](#re-measured-for-m35-2026-08-03)'s own gated run, 99.505% for
[M36](#re-measured-for-m36-2026-08-03)'s sequential column and 99.405% for M35's
reopening — the same table, the same `INSERT … ON CONFLICT DO UPDATE`, the same
5,000 hot aliases. The median is 3.11ms against 3.38ms, 3.09ms and 3.12ms. It is
also the first gated run to hold the offered rate: zero dropped iterations
against the reopening's 99. **That is not evidence the timeout made anything
faster** — four measurements of one mechanism landing between 99.4% and 99.83% is
the spread of that mechanism — and the honest reading is that a per-call
`WithTimeout` on a query already costing three milliseconds does not show up.

#### What this run verified live, and what it could not

**F96's bound, on the built image, because a deadline that has never been reached
is a deadline nobody has seen work.** `ld1` was given a click budget, then
`link_click_budget` was locked `ACCESS EXCLUSIVE` in a transaction held open for
twelve seconds. During the lock the link answered **503 in 251ms**, twice, against
`REDIRECT_TIMEOUT=250ms`; unblocked it answers 302 in 2–9ms. On the build this
replaces, that request would have waited out the lock holding a connection from
the pool. The 503 rather than a 410 is `budgetGate`'s deliberate direction: a
database that cannot answer must not retire a live link.

**F87, on the same image.** `/f87live` — a two-arm sequential split behind a
password — answered **200** to the first GET, **303 to `…/arm1`** to the POST that
verified the password, **200** to the second GET and **303 to `…/arm2`** to its
POST, with `link_click_budget.rotation` at **2** after two visits. Every one of
those four values is what the fix produced: before it, both visits were served
`arm2`, `arm1` was unreachable at any point in the link's life, and the rotation
read 4.

**F88 and F89's custom-domain halves were not verified live**, for the reason
M35's reopening gives about its own: both need a *verified* custom hostname, which
needs a DNS TXT record this instance cannot publish for itself. They are covered
by `TestAHostSpellingThatDoesNotFoldStillReachesItsOwnHostname` — which builds a
**single-host** fixture for the purpose, because that is the deployment where the
failure is open rather than closed — `TestAConfiguredHostnameFoldsItsTrailingDot`,
`TestBotBlockingReachesEveryHostname` and
`TestAHostnameRegisteredAfterEnforcementInheritsIt`, all sabotage-verified.

**The password gate itself**, for the reason M35's own section gives: it is an
argon2id derivation on a POST that k6 cannot drive against 5,000 aliases without
5,000 passwords. F87's live check above is one visit, not a load measurement, and
what it establishes is the ordering rather than the cost.

Taken on image `sha256:8a1188da3a43…` (`linkctrl:test`, built 2026-08-04 from the
M45 group-5 working tree at `v0.1.0-95-g2e0a661-dirty`), rebuilt and recreated
from the tree immediately before the runs, against a freshly seeded
`make seed-slo` dataset. Both cache tiers were emptied and the container restarted
before each column. Same host as
[M35](#re-measured-for-m35-2026-08-03) onward.

Twenty-four cached runs now read 100%, 100%, 100%, 99.991%, 100%, 100%, 100%,
100%, 100%, 100%, 100%, 100%, 99.743%, 100%, 100%, 99.505%, 100%, 100%, 100%,
100%, 100%, 99.405%, **100%** and **99.826%** under 20ms — the last of those being
the gated configuration, which is a different path rather than a regression in
this one.

### Re-measured for M45's Redis client change (2026-08-05)

[F138](build-notes/deferred-findings.md#closed): `internal/platform/redis.Open`
now sets `ContextTimeoutEnabled`. That is the client the resolver holds, so the
inherited rule applies and this is a k6 run rather than a note.

**What changed on the path, so the numbers are read against something.** Nothing
executes differently in the healthy case. `Options.RedisTimeout` *is*
`REDIS_READ_TIMEOUT`, so `fromRedis` and `store` already asked their calls for
exactly the client's own ceiling; go-redis takes the minimum of the two, and the
minimum of a number and itself is that number. No allocation, no branch and no
call was added anywhere on the redirect path — the diff on this path is one
field on an options struct at boot. The one case that does change is a request
context with **less** than `REDIS_READ_TIMEOUT` left, which under load never
happens: at 2,000 rps the whole request is 87µs of a 250ms budget.

**The uncached column is the one that matters here**, because the cached column
does not touch Redis at all — its own cache-mix line says `0 redis`. Both are
taken anyway, because the SLO is stated on the cached path and a change to the
client is exactly the kind that could reach it by accident.

| | Target | Measured |
| --- | --- | --- |
| Cached redirect, server-side | p99 < 20ms | **100% of 240,001 under 20ms**; 100% under 0.5ms |
| Cached redirect, generator-side | — | avg **87.42µs**, median 83.27µs, p(90) 104.59µs, p(95) 113.02µs, max 6.12ms, min 37.40µs |
| Sustained rate | 2,000 rps for 2m | 2,000.00 rps, 240,001 requests, zero failures, no dropped iterations |
| Dataset | 100k links, 5M events | 100,000 links, 5,000,000 events, freshly seeded |
| Cache mix | hits only | 240,001 memory, 0 redis, 0 database |
| Redirect pool | — | 0 acquire waits |

The uncached path, which is where the Redis client is actually exercised:

| | Target | Measured |
| --- | --- | --- |
| Uncached redirect, generator-side | p99 < 100ms | avg **287.71µs**, median 295.78µs, p(90) 346.99µs, p(95) 366.28µs, max 8.91ms, at 500 rps for 1m, zero failures |
| Cache mix | mostly misses | 25,946 database, 2,646 redis, 1,409 memory |
| Redirect pool | — | 2 acquire waits, 0.015s total |

**The cached column did not move.** 87.42µs against
[M45's redirect findings](#re-measured-for-m45s-redirect-findings-2026-08-04)'s
87.33µs, [M41](#re-measured-for-m41-2026-08-03)'s 87.04µs and
[M40](#re-measured-for-m40-2026-08-03)'s 87.44µs, on the same host and a dataset
seeded the same way — a 0.1% spread across four measurements, which is the
run-to-run precision this document has recorded since its first section. The
uncached column is the second-fastest recorded, behind
[M33](#re-measured-for-m33-2026-08-02)'s 421.47µs p99 and well inside the 100ms
target; it is not comparable to it directly, because the summary k6 prints now
reports p(95) rather than p99.

#### What this run verified live, and what it could not

**The stall bound, on the built image, because the whole finding is about what
happens when Redis stops answering.** `docker pause` on the Redis container is
the exact failure shape: connections stay established and nothing is answered,
which is what a refused connection is not. Three cold aliases each answered
**302 in 102ms** while it was paused, against 4.95ms warm and 1.55ms after
unpausing. 102ms is the documented cost of a cold resolve against a stalled
Redis — the read timeout spent twice, once on the lookup that never answers and
once on the `Set` that would have repopulated the cache, plus the Postgres query
— and it is unchanged, which is the point: the ceiling is still
`REDIS_READ_TIMEOUT` and the option did not lower it.

**The half that changed could not be shown here**, and saying which half is the
honest version of "the numbers did not move". A request with less than 50ms of
its budget left is not a state a healthy instance enters, and manufacturing one
under k6 would measure the manufacture. It is asserted instead by
`TestAContextDeadlineBoundsARedisCall`, against a listener that accepts and never
answers: 50ms asked for, 50ms spent, where the same call cost 400.85ms with the
option removed.

Taken on image `sha256:99ca625a3b8c…` (`linkctrl:test`, built 2026-08-04 23:54
UTC from the M45 F137/F138 working tree at `v0.1.0-103-g58b2042-dirty`), rebuilt
and recreated from the tree immediately before the runs, against a freshly seeded
`make seed-slo` dataset. Both cache tiers were emptied and the container restarted
before each column and before the live check. Same host as
[M35](#re-measured-for-m35-2026-08-03) onward.

Twenty-five cached runs now read 100%, 100%, 100%, 99.991%, 100%, 100%, 100%,
100%, 100%, 100%, 100%, 100%, 99.743%, 100%, 100%, 99.505%, 100%, 100%, 100%,
100%, 100%, 99.405%, 100%, 99.826% and **100%** under 20ms.

### Re-measured for M50 (2026-08-07)

[M50](build-notes/phase-details/m50.md) touches the redirect path, so the
inherited rule applies and this is a k6 run rather than a note. It is the first
of the phase's three.

**What it adds, stated as work rather than as a feature.** A link may now carry
several QR codes, each printing an identity in its payload as `&qrc=<slug>`
beside `?src=qr`. The redirect has to say which code a scan came from, and it has
to do so without trusting the value: `link_dimension_daily`'s primary key
includes what gets stored, so a code parameter a visitor could choose would be an
unbounded write anybody could trigger — the same hole `?src=`'s closed vocabulary
was built to shut, one parameter to the left. The bound is resolution against the
codes the link actually has, and the whole design question was whether that
resolution could be answered from data the resolver already holds.

It can. The link's slugs ride home in `ResolveAliasForRedirect`'s round trip, in
a second lateral on `qr_codes_link_idx`, and the snapshot carries them. So what
runs per request is: the `strings.Contains` test M41 added, which is false for
nearly every request on the instance; then, only for a request that carries a
recognised source, one more `Values.Get` and a scan of a slice bounded by
`domain.MaxQRCodesPerLink`. No query, no allocation beyond the map M41's parse
already builds, and nothing at all on a request without `src=`.

**Three request shapes, because there are three branches and the third is the one
this milestone introduced.** The first is the default path, unchanged since M41.
The second is M41's scan — `?src=qr`, no code — which is what every picture this
product printed before today carries. The third is M50's: a slug that has to be
found in the snapshot before anything is recorded.

| | No query (the default path) | `?src=qr` (M41's scan) | `?src=qr&qrc=sloqrcod` (M50's scan) |
| --- | --- | --- | --- |
| Requests | **240,002** at 2,000 rps for 2m | **240,001** at 2,000 rps for 2m | **240,002** at 2,000 rps for 2m |
| Under 20ms | **100.000%** | **100.000%** | **100.000%** |
| Under 0.5ms | 100.000% | 100.000% | 100.000% |
| Mean | **93.67µs** | **93.86µs** | **93.08µs** |
| Median | 88.84µs | 89.44µs | 88.84µs |
| p(90) / p(95) | 111.65µs / 120.81µs | 111.64µs / 120.80µs | 110.82µs / 119.39µs |
| Max | 6.53ms | 5.00ms | 3.84ms |
| Checks | 100.00%, 480,004 of 480,004 | 100.00%, 480,002 of 480,002 | 100.00%, 480,004 of 480,004 |
| Cache mix | memory 240,002 · redis 0 · database 0 | memory 240,001 · redis 0 · database 0 | memory 240,002 · redis 0 · database 0 |
| Redirect pool waits | 0 | 0 | 0 |

**Nothing moved, and the third column is the claim.** 93.08µs for the per-code
scan against 93.67µs for a request that does no attribution at all — the
resolving branch measured *faster* than the branch that skips it, which is not a
finding about resolution being free. It is the honest reading that a slice scan
over one short string is far below the resolution of a measurement whose
run-to-run spread this document already records at several microseconds, and that
the three figures are one number measured three times.

**The measurement was verified to be of the branch it claims**, which matters
more here than the microseconds. A `qrc` the snapshot does not recognise falls
through to the default code and costs almost exactly what column two costs, so a
run where the resolution silently failed would have produced this same table. It
did not: the 100,000 seeded links were each given a code with the slug
`sloqrcod` before the third run, and the click events written during it read
**240,003 rows stored as `qr:sloqrcod`** against 240,001 as the bare `qr` from
the second run. Every request in the third column resolved a slug and stored the
code's identity.

The 93.67µs baseline against M45's 87.42µs is host drift rather than this
milestone: all three columns here were taken minutes apart on one image, and it
is the *comparison between them* this section is for. Absolute figures across
sections of this document have moved by more than this between builds that
changed nothing on the path.

Taken on image `sha256:8f653b58cdd31075eefe7d83798f7be05fb6f18d75cde431bff0a6556be97ead`
(`linkctrl:test`, built 2026-08-07 from the M50 working tree at
`v0.2.0-27-gfc8702f-dirty`), rebuilt and recreated from the tree immediately
before the runs, against a freshly seeded `make seed-slo` dataset of 100,000
links and 5,000,000 click events. Both cache tiers were emptied and the container
restarted between the second and third columns, because the third needs snapshots
written after the `qr_codes` rows existed — without that the run would have
measured cached entries carrying no slugs, which is the silent failure described
above. Same host as [M35](#re-measured-for-m35-2026-08-03) onward.

Twenty-eight cached runs now read 100%, 100%, 100%, 99.991%, 100%, 100%, 100%,
100%, 100%, 100%, 100%, 100%, 99.743%, 100%, 100%, 99.505%, 100%, 100%, 100%,
100%, 100%, 99.405%, 100%, 99.826%, 100%, **100%**, **100%** and **100%** under
20ms.

### Measured during a rolling deploy, for M57 (2026-08-09)

[M57](build-notes/phase-details/m57.md) does not change the redirect path. It
measures what that path does while the processes serving it are **replaced one
at a time underneath live traffic**, which is the one thing every figure above
was taken with carefully held still.

It is the second of the phase's three, after
[M50](#re-measured-for-m50-2026-08-07)'s and before
[M57.9](build-notes/phase-details/m57.9.md)'s re-measurement on the final build.

Every earlier section is a single container serving 240,000 requests. This is
**three replicas behind a load balancer**, with each replica destroyed and
rebuilt during the measured window. The harness is `scripts/rolling-deploy.sh`,
`test/ha/compose.yml` and `test/ha/haproxy.cfg`, and none of it ships: it exists
so the contract in [operations.md](operations.md#the-load-balancer-contract) can
be checked against a running thing rather than argued from the shape of the
code.

**Two columns, because the drain delay has to be worth something.** *Deploy* is
`docker compose up -d --force-recreate`: SIGTERM, `/readyz` turns 503, the drain
delay elapses, then the listener closes. *Kill* is SIGKILL — no drain, no
readiness change, the listener simply stops existing, which is a crash, an OOM
kill or a yanked cable. Running only the first would produce a number without
saying what it bought.

| | Target | **Rolling deploy** | **Rolling kill** |
| --- | --- | --- | --- |
| **Requests failed** | — | **0** of 240,002 | **0** of 239,833 |
| **Requests retried** | — | **0** | **905** redispatched to another replica |
| Cached p99, generator-side | — | **295.24µs** | **313.16µs** |
| Cached, server-side | p99 < 20ms | **100% of 236,575 under 20ms**; 100% under 5ms; 99.987% under 0.5ms | **100% of 239,470 under 20ms**; 100% under 2.5ms; 99.983% under 0.5ms |
| Mean / median | — | 154.63µs / 141.51µs | **2.72ms** / 141.82µs |
| Max | — | 7.91ms | **1.00s** |
| Sustained rate | 2,000 rps for 2m | 2,000.00 rps, zero dropped iterations | 1,998.6 rps, **168 dropped iterations** |
| Checks (`is 302`, `has Location`) | 100% | 100.00%, 480,004 of 480,004 | 100.00%, 479,666 of 479,666 |
| Cache mix | hits only | 221,575 memory · 15,000 redis · **0 database** | 224,470 memory · 15,000 redis · **0 database** |
| Response errors at the balancer | — | 0 | 4 |
| Concurrency the generator needed | — | max **3** VUs | max **212** VUs |
| Each replica replaced in | — | 12s, 12s, 11s | 7s, 6s, 7s |
| Whole replacement | — | **35s** | **20s** |

**The deploy column is the headline and it is a zero.** Every one of three
replicas was destroyed and rebuilt while 2,000 requests a second went through
the balancer, and not one request failed, not one was retried, and the cached
p99 was 295µs against a 20ms budget. The generator never needed more than three
concurrent connections, which is the same thing said from the other side: no
request ever waited.

**The reason is the drain delay, and the kill column is what it costs to not
have one.** The arithmetic
[operations.md](operations.md#sizing-the-drain-delay) asks an operator to
satisfy is `DRAIN_DELAY > interval × threshold`; here that is 5s against
`inter 1s fall 2`, so the balancer has three seconds of margin to stop routing
before the listener closes. Take the drain away and the same replacement costs
**905 redispatched requests, four response errors, a worst case of a full
second** — `timeout connect 1s`, a request dialling an address that no longer
exists — and a generator that had to spin from 3 to 212 concurrent connections
to hold the rate, dropping 168 iterations anyway.

**Nothing failed even in the kill column, and the credit does not go to this
product.** `retries 3` with `option redispatch` is what turned every one of
those 905 into latency instead of an error. A balancer configured without them
would have answered 503 to all of them. That is worth stating plainly because it
is the difference between what the product guarantees and what the deployment
around it does: the product's contribution is that `/readyz` tells the truth
early enough to be acted on, and acting on it is the balancer's.

**The cache mix is 15,000 redis hits in both columns, and that is not a
coincidence.** It is exactly 3 × 5,000 hot aliases: each replacement replica
started with an empty in-process tier and read each hot alias from the shared
tier once. **Postgres was never reached** — zero database-tier observations in
either run — which is the shared tier doing the job it exists for, in the one
situation that produces a guaranteed cold cache on purpose.

#### What the server-side column can and cannot be

A rolling deploy destroys two thirds of the counters mid-run, so the
before-and-after delta every section above takes is not available here. Each
replica is instead scraped when the window opens and again immediately before it
is replaced, and its replacement is scraped at the end from a counter that
started at zero; the three contributions are summed.

**That sum undercounts, always in the same direction, and by an amount worth
reading.** 236,575 against the generator's 240,002 in the deploy column — 3,427
requests — and 239,470 against 239,833 in the kill column, 363. The gap is the
traffic a replica served between its final scrape and actually stopping, which
is why the graceful column's gap is nearly ten times the killed one's: **that
difference is the drain, measured by accident.**

#### The two-leader window, sampled

`pg_locks` was polled every **100ms** for the length of each run — 1,482 samples
each — counting distinct sessions holding each advisory key. Four keys were seen
held during the deploy run (rollup, dimension-rollup, webhooks, domains) and
three during the kill run; **the most holders any sample found on any key was
one, and no sample found two.**

Read that as corroboration and not as the argument, for two reasons. Sampling at
100ms cannot see a window shorter than the gap between samples, and most
families hold their key for a single indexed query — which is why the four
observed are the four with work long enough to be caught. What actually closes
the deploy-shaped window is that from 0.2.0 on every binary asks for the same
per-family keys, so `pg_try_advisory_lock` excludes the second leader; the
decision log's M57 entry has the whole argument, and
`TestAReleasedFamilyKeepsItsAdvisoryKey` is what keeps it true.

Taken on image `sha256:465e33d27b653f8e1119692915766527f553ecbb28b1cdd85d57689f007370db`
(`linkctrl:test`, built 2026-08-09 from the M57 working tree at
`v0.2.0-41-gadcdf02-dirty`), rebuilt from the tree by the harness immediately
before the runs, against a freshly seeded `make seed-slo` dataset of 100,000
links and 5,023,001 click events. Every replica's in-process tier was warmed
individually before the window opened — round-robin warm-up through the balancer
would have left two thirds of each replica's hot set cold and measured that
instead. HAProxy 3.1, `balance roundrobin`, `option httpchk GET /readyz`,
`inter 1s fall 2 rise 1`. Same host as
[M35](#re-measured-for-m35-2026-08-03) onward.

Thirty cached runs now read 100%, 100%, 100%, 99.991%, 100%, 100%, 100%, 100%,
100%, 100%, 100%, 100%, 99.743%, 100%, 100%, 99.505%, 100%, 100%, 100%, 100%,
100%, 99.405%, 100%, 99.826%, 100%, 100%, 100%, 100%, **100%** and **100%**
under 20ms — the last two being the rolling deploy and the rolling kill, which
are the only two taken while the processes serving them were being replaced.

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

The per-code scan (M50) needs codes to resolve against, and `make seed-slo`
writes none — so they are inserted the same way, and the flush and the restart
are load-bearing for the same reason the `forward_path` recipe's are: the slugs
travel inside the cached snapshot, so entries written before the rows existed
carry none and the run measures the unrecognised-value branch while reporting the
recognised one.

```sh
docker compose exec postgres psql -U linkctrl -d linkctrl -c \
  "INSERT INTO qr_codes (id, link_id, workspace_id, slug, label, style)
   SELECT gen_random_uuid(), l.id, l.workspace_id, 'sloqrcod', 'SLO print run', '{}'::jsonb
     FROM links l WHERE l.alias LIKE 'ld%'"
docker compose exec redis redis-cli FLUSHALL
docker compose restart app
SUFFIX='?src=qr&qrc=sloqrcod' make load
```

Check afterwards that the run measured what it claims, because the failure is
silent — an unresolved slug is attributed to the default code and costs the same:

```sh
docker compose exec postgres psql -U linkctrl -d linkctrl -c \
  "SELECT referrer_host, count(*) FROM click_events
    WHERE occurred_at > now() - interval '10 minutes' GROUP BY 1 ORDER BY 2 DESC"
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

### Reproducing the rolling-deploy measurement

A different script, because it is a different question: not *how fast is a
redirect* but *what does replacing every process serving them cost*.

```sh
make seed-slo                             # the same dataset every column uses
scripts/rolling-deploy.sh deploy 2000 2m  # SIGTERM, drain, replace
scripts/rolling-deploy.sh kill   2000 2m  # SIGKILL, no drain
```

It builds the image from the working tree itself rather than trusting the one
that is running, brings up three replicas and a load balancer as an overlay on
the instance's own Postgres and Redis, warms each replica's in-process tier
separately, starts the generator, and only then begins replacing replicas. The
overlay stops the single-replica `app` first: a fourth replica against the same
database would hold job leadership for the whole run and is not what is being
measured.

Read three things out of the report before the latency.

**The cache mix.** A column with database-tier observations in it is measuring
cold resolution rather than a deploy — it would mean the shared tier did not
absorb the replacement replicas' cold reads, which is a finding rather than a
number to quote.

**The load-balancer counters.** `wretr`/`wredis` are what "requests retried"
means here, taken from the balancer rather than inferred from the generator;
`hrsp_5xx` is what "requests failed" means. A run reporting zero of both while
the generator reports failures is a run where the generator could not reach the
balancer at all.

**The advisory-lock poll**, whose sampling interval is printed with it. It can
only see a window longer than one sample, so a clean result is corroboration
rather than proof — see the [M57 section](#measured-during-a-rolling-deploy-for-m57-2026-08-09).

The load balancer's own configuration is the measurement's most load-bearing
input and is deliberately in the repository rather than in this document:
`test/ha/haproxy.cfg` satisfies `DRAIN_DELAY > interval × threshold` on purpose,
and changing either number changes what the deploy column means.

### Re-measured for M45's redirect-path batch (2026-08-05)

Six findings fixed in [M45](build-notes/phase-details/m45.md) touch the redirect
path, so the inherited rule applies and this is a k6 run rather than a note. They
were batched deliberately and measured once, because one run answers for all of
them and six runs would answer the same question six times.

**What changed on the path, so the number is read against something.** F48 moved
the root-redirect cache refill to the redirect pool and gave it a timeout and a
single-flight; F101 added a deadline to one detached Redis delete; F9 suppresses
the repopulating `Set` for one uncached resolve after a failed lookup; F65 routed
two log calls through the nil-tolerant accessor; F41 changed a comment only; F50
added one `record` call on the `410` branch; F64 sets one header in a wrapper
around the mux; F100 reads the split rotation instead of advancing it **on HEAD
only**; F115 adds one limiter call after a *correct* link password; F116 adds a
length and a segment-count test to the deep-link joiner. Nothing was added to the
ungated `GET` of an uncached-then-cached alias, which is what this column
measures.

| Column | Result |
| --- | --- |
| Cached, 2,000/s for 2 minutes | **240,000 redirects, 100% under 0.5ms** |
| Cache mix | 240,000 memory, 0 redis, 0 database, 0 negative |
| Redirect pool acquire waits | 0 |

**The dataset is smaller than the runs above and that is stated rather than
glossed**: 100,000 links and 200,000 click events, against the 100k/5M this
document's earlier columns used. The click volume feeds the analytics pipeline
and the rollups, not the cache-hit path being measured here, and the cache mix
confirms what was exercised — every one of the 240,000 was a memory hit. It is
the right measurement for *did these ten changes slow the cached redirect*, and
it is not a re-measurement of the rollup columns, which nothing in this batch
touches.

Twenty-one cached runs now read 100%, 100%, 100%, 99.991%, 100%, 100%, 100%,
100%, 100%, 100%, 100%, 100%, 99.743%, 100%, 100%, 99.505%, 100%, 100%, 100%,
100% and **100%** under 20ms.

### Where the checks stop, measured rather than assumed (2026-08-05)

Every column above is a **pass at a fixed size**, because the size is whoever
seeded the database's choice. None of them is a bound. `scripts/slo-breaking-point.sh`
asks the other question: seed, measure, check, multiply, and stop at the first
documented claim that breaks.

It checks five things, each named against the document it comes from — the
cached-redirect SLO as a fraction under 20ms, analytics drops, and both rollups
against the alert thresholds in [operations.md](operations.md).

**Run of 2026-08-05, ×4 from 50,000 links, two clicks per link:**

| Links | Clicks | Cached under 20ms | Drops | Totals rollup | Dimension rollup |
| --- | --- | --- | --- | --- | --- |
| 50,000 | 100,000 | 100% | 0 | fresh | fresh |
| 200,000 | 400,000 | 100% | 0 | fresh | fresh |
| 800,000 | 1,600,000 | 100% | 0 | fresh | fresh |
| **3,200,000** | **6,400,000** | **100%** | **0** | 0.004s | **661s** |

**No check failed.** Read the margins rather than the verdict, which is the whole
reason the script prints them: the redirect columns are flat, because a cache hit
does not care how many rows it did not read, and the number that *moves* with
size is the dimension rollup's staleness — 661 seconds at 3.2M links, against its
own 15-minute cadence and a 1-hour alert threshold.

That is the bottleneck this document already names, arrived at from the other
direction. The rollup columns above measure its cost per run; this measures how
far behind it falls, and says the edge is there rather than on the redirect path.

**What this is not.** It is one machine, one disk, and whatever else was running.
The transferable part is the *shape* — which check gives first, and that it is not
the one the SLO is about — not the numbers.

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
- **The 404-probe limit is per instance and in memory**, so a load generator hammering
  one address is throttled by `REDIRECT_404_RATE_LIMIT` only if it asks for
  aliases that do not exist. Every alias in the load test exists, and hits are
  never charged — which is why 240,001 requests from one address pass cleanly. A
  load test that used random *nonexistent* aliases would be measuring the limiter.
