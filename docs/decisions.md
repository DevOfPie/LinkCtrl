# Decision log

Why LinkCtrl is built the way it is. `Plan.md` states *what* is true; this file
states *why*, so the spec stays terse and the reasoning is still recoverable.

Entries are append-only and dated. A later entry may correct an earlier one;
the earlier text is left in place with a pointer rather than edited away.

Longer investigations live in `docs/adr/`.

---

## 2026-07-29 — Phase 1 planning

Decisions made before any code existed.

### Stack

**Go, with sqlc + pgx + Postgres + Redis.** Chosen for redirect latency and
single-binary deployment, which is the shape a self-hosted product wants.
Planning also assumed chi as the router; see 2026-07-30 for why it was not
adopted.

**Server-rendered templates + HTMX rather than an SPA.** Fastest path to the
dashboard latency target, one container, no Node in production. The API/UI
parity rule is enforced by making both call the same service layer, not by
convention.

### Schema

**All 20 data-model entities created up front** rather than added per phase.
The cost is dead schema; the benefit is that later phases are additive
migrations rather than rewrites. Contained by storing anything structural in a
dormant table as jsonb, because its real shape will not survive contact with
the feature that eventually uses it.

**Tenancy columns everywhere from day one.** `workspace_id` is on every
tenant-scoped table even though Phase 1 gives each user a single
auto-provisioned workspace. These are the columns that cannot be fixed
additively later; Phase 2 columns are expected to be adjusted.

**Alias uniqueness is per-domain, not global.** Phase 2 custom domains then
need no data migration and no cache-key change.

### Scope

**RBAC implemented for real in Phase 1**, not a hardcoded owner role, even
though Organizations are Phase 2. Retrofitting authorization after features
exist is where permission bugs come from.

**Folders reduced to schema-only.** Listed as Required under Link Management
but absent from the Phase 1 roadmap; resolved in favour of the roadmap.

**Password, one-time and max-click links deferred to Phase 2.** An accurate
click counter cannot live in a cache that is allowed to evict or restart empty.
Phase 1 runs Redis as a pure cache with no persistence, so enforcing "exactly N
clicks" would either be wrong or require a durable counter store. Deferred
rather than shipped incorrectly; the columns exist and the API rejects them
with a 422.

### Behaviour

**302 redirects, never 301.** Links are editable by design, and a permanent
redirect cached in browsers and intermediaries cannot be recalled.

**Redis is cache-only with no persistence.** This is what forces the
one-time-link deferral, and it is the right trade: it keeps the operational
story simple and makes a cache outage a degradation instead of an outage.

**Analytics writes are asynchronous and may drop under sustained overload**
rather than delay a redirect. Bounded queue, counted drops, flush on shutdown.

**SLO validated at 2,000 rps / 100k links / 5M events.**

### Defaults chosen without strong constraints

Cheap to revisit before the schema is frozen: `/docs` is public; aliases are
lowercase-canonical and case-insensitive; bot clicks are recorded but excluded
from default charts; analytics retention is 395 days; query forwarding is off
and deep-link forwarding is Phase 2; new instances have signup closed.

### Development environment

Windows host with Go, Docker Desktop and a C compiler installed natively. The
working copy must not live in OneDrive: sync interferes with the Go build cache
and with `.git`, and cloud-placeholder files bind-mount into containers as zero
bytes. See `docs/development.md`.

---

## 2026-07-30 — decisions made while building

These came out of implementation. Several correct something above.

### No third-party router

Planning assumed chi. `net/http`'s ServeMux covered every requirement, so the
dependency was never added.

Two consequences are worth knowing. ServeMux refuses some pattern pairs as
ambiguous where chi would quietly choose one — `HEAD /{alias}` against
`GET /healthz` is rejected because it matches fewer methods but a more general
path — which is why the alias catch-all is registered without a method and
filters methods itself. And middleware is composed by hand, which is a few
lines rather than a framework concept.

### Partitions are never declared in SQL that sqlc reads

Tested before writing the schema (`docs/adr/0001-partitioning-and-sqlc.md`).
sqlc does not fail on a partitioned parent, which is what was expected — it
silently emits a duplicate model struct for every child partition, so generated
code would grow a dead type every month and `make generate` would produce a
diff on a schedule rather than in response to a change. Partitions are created
by application code instead.

### Both nullable and non-nullable sqlc overrides must be declared

A plain type override applies only to the NOT NULL case. Nullable columns
otherwise fall back to driver wrapper types, and those leak into the domain
layer.

### Partition bounds resolve against the session timezone

Verified empirically: identical DDL executed under UTC and under
America/New_York produced bounds four hours apart, leaving a gap that silently
routes rows to the default partition. UTC is pinned in the connection pool, the
Postgres server and the container, and startup fails if a session is not UTC.

### Two HTTP handler trees, not one

The application tree carries session lookup and security headers; the redirect
tree carries almost nothing, because a session check alone is a database round
trip and the whole response budget is 20ms. Only client-address resolution is
shared, since analytics needs it and it costs a header read.

A test wires an authenticator that fails if called and asserts the redirect
path never touches it, so the split cannot erode into a comment.

### Two database pools

A small dedicated pool serves redirects, so a slow analytics query holding
application connections cannot leave a redirect waiting to acquire one.

### Sessions in Postgres, not Redis

Redis here has no persistence and evicts under memory pressure, so sessions
kept there would log everyone out at an arbitrary moment. Only the SHA-256 of
the token is stored, so a database leak does not hand over live sessions —
SHA-256 rather than argon2 because the token is full-entropy random, so
stretching adds nothing on a path that runs for every request.

### Login failures are indistinguishable

Unknown account, wrong password and SSO-only account all return the same error,
and a failed lookup still performs a dummy verification so response time does
not reveal whether an address is registered.

A malformed stored hash is deliberately *not* reported as a credential
mismatch: collapsing it would show the user a login failure while a corrupt row
goes uninvestigated.

### The JSON authentication API ships before the HTML forms

"Every UI feature has API support" is a success criterion. Building the form
first and retrofitting an endpoint is precisely how that gets broken.

### Destination validation is an allowlist

A blocklist of dangerous schemes is a game you lose: `javascript:`, `data:`,
`vbscript:`, `file:`, `intent:`, and whatever the next browser ships.
Permitting only http and https means a new scheme is refused by default.

Private, loopback, link-local, carrier-NAT and cloud-metadata addresses are
refused too, because a short link pointing at `169.254.169.254` turns the
shortener into a tool for making someone else's browser probe their own
network. IPv4-mapped IPv6 is folded before the check, or `::ffff:10.0.0.1`
slips past every IPv4 rule.

### Rollups recompute rather than accumulate

Each run derives whole days from the raw events and upserts. An incremental
"add what arrived since the watermark" design double-counts on any retry and,
once it drifts, stays wrong invisibly.

### Visitor hashing specifics

`HMAC(daily salt, ip || 0 || user-agent || 0 || workspace)`.

HMAC rather than hashing `salt||data`, which is length-extendable. The
workspace is in the message, not the key, so two workspaces on one instance
cannot join their analytics to follow one person across both — this corrects
an earlier claim that the salts themselves were per-workspace. NUL separators
so a crafted user agent cannot shift the field boundary to collide with a
different address. IPv4-mapped IPv6 folded, or one person arriving over each
stack counts twice. Days are UTC, because a local boundary would rotate at a
different instant per deployment and split a visitor across a daylight-saving
change.

Salts are deleted after two days. That deletion is the de-identification step,
not housekeeping. Two days is the minimum that lets a day be rolled up and then
finalized.

### User-agent classification is hand-written

The question is coarse — phone or desktop, roughly which browser — and a few
dozen ordered substring checks answer it on a path that runs for every click.
Accuracy on unusual agents is traded away deliberately, and stated in the UI
rather than implied away.

Check order is load-bearing: Edge and Opera both claim Chrome, Chrome claims
Safari, iOS agents contain "mac os x", and Android agents contain "linux".

Unfurlers (Slack, Discord, WhatsApp, Twitter) are classified as bots, because a
link pasted into a chat is not a visit.

### Referrers are reduced to a host at the edge

Full referrer URLs routinely carry session tokens and search terms in the query
string, so the rest is discarded on the way in rather than stored and cleaned
up later.

### Alias alphabet

32 characters. The digits 0 and 1 are kept *because* the letters o, l and i are
excluded, which is what brings the alphabet to exactly 32 and makes reducing a
random byte modulo its length unbiased. A test fails if the length changes.

Aliases are lowercase-canonical, so `/GitHub` and `/github` are the same link.
Dots are rejected outright rather than pattern-matched afterwards, which
removes the whole "alias looks like logo.png" class of confusion with asset
routes.

### Profanity filtering is two-tier

Short terms match as whole separator-delimited tokens; only terms whose
appearance is essentially never innocent match as substrings. Naive substring
matching rejected "therapist", "raccoon", "tycoon" and "fire-retardant" during
development.

### Deleted links are recoverable, purged aliases are not reissued

Soft delete with a 30-day window. An alias belonging to a link that received
traffic is reserved permanently on purge, because it exists on printed material
and in other people's bookmarks; reissuing it would redirect someone else's
audience.

### Negative caching requires invalidation on create

Unknown aliases are the most common request a public shortener receives, so
caching misses matters. But an alias probed before it exists would then stay
404 for the whole negative TTL, and a newly created link would look broken.
Create clears the negative entry.

### Cache TTL is clamped to link expiry

Caching a link for 24 hours when it expires in five minutes would keep serving
it for hours past its deadline.

### Errors expose nothing unmapped

Service errors map to problem+json in exactly one place. An unrecognised error
becomes a flat 500 with the detail logged rather than returned, because
internal error strings carry table names, query fragments and connection
strings.

Unknown JSON request fields are rejected rather than ignored: a misspelled
field silently dropped means the caller believes they set something they did
not.

### The click-event adapter lives in the composition root

`httpx` imports `analytics` for the reader, so putting the adapter in
`analytics` would create a cycle. It is pure wiring, so the composition root is
where it belongs.
