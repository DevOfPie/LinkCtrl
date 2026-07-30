# LinkCtrl — Project Plan

Scope contract and specification. States **what** is true, not why.

| | |
| --- | --- |
| Rationale for every decision | `docs/claude/decisions.md` |
| Investigations | `docs/adr/` |
| Dev environment | `docs/claude/development.md` |
| Current progress | [Build Status](#build-status) |
| Last updated | 2026-07-30 |

**Core rule:** links are programmable, observable, secure resources.

## Vision

Self-hostable link management platform replacing traditional URL shorteners. A
link is a programmable resource with a destination, routing rules, analytics,
metadata, automation, permissions and history.

Serves individuals, creators, businesses, developers and enterprises.

## Principles

1. Everything available through the API. Every UI feature has API support.
2. Links are editable without changing their URL.
3. Privacy-conscious analytics.
4. Modular architecture; features added without architectural rewrites.
5. Scales from personal use to enterprise.

---

## Stack

| Layer | Choice |
| --- | --- |
| Language | Go 1.26 |
| Router | `net/http` ServeMux (no third-party router) |
| Database | PostgreSQL 17, `sqlc` + `pgx/v5`, no ORM |
| Migrations | `goose` in library mode, embedded, run at boot |
| Cache | Redis 7, cache-only, no persistence |
| Passwords | `golang.org/x/crypto` argon2id |
| IDs | UUIDv7, application-generated |
| Frontend | Go `html/template`, HTMX, Tailwind standalone CLI (no Node in image) |
| API contract | Hand-maintained OpenAPI 3, contract-tested against the implementation; Swagger UI embedded |
| Observability | `log/slog`, Prometheus |
| GeoIP | MaxMind DB reader, optional at runtime; database supplied by the operator |
| Rate limiting | In-process token buckets, no external dependency |
| Deployment | Docker + Compose; Caddy for TLS |
| Load testing | k6 |

---

## Scope by phase

Authoritative. Where this table and prose elsewhere disagree, this table wins.

### Link management

| Capability | Phase |
| --- | --- |
| Create / edit / delete links | 1 |
| Custom aliases | 1 |
| Tags | 1 |
| Search | 1 |
| Archive / restore | 1 |
| Soft delete (30-day recovery) | 1 |
| Metadata (title, description) | 1 |
| Expiring links | 1 |
| Folders — schema only, no API or UI | 1 |
| Folders — API and tree UI | 2 |
| Bulk operations, templates, import/export | 2+ |
| Version history, scheduled changes, approval workflows | 3+ |
| Malicious link detection | 2 |

### Redirect engine

| Capability | Phase |
| --- | --- |
| Alias resolution, 302 redirect | 1 |
| Expiry enforcement (410 Gone) | 1 |
| Two-tier cache with negative caching | 1 |
| Query forwarding (default off) | 1 |
| Deep-link path forwarding | 2 |
| Rules: country, region, city, language, browser, OS, device, date/time, referrer, query params, UTM, cookies, returning visitors | 2 |
| A/B testing, weighted routing, percentage splits, sequential routing, feature flags, fallback destinations | 2 |

### Analytics

| Capability | Phase |
| --- | --- |
| Clicks, unique visitors, timestamp | 1 |
| Device, browser, OS, referrer, language | 1 |
| Bot detection | 1 |
| Dashboards, trends | 1 |
| Geographic (country) — optional at runtime | 1 |
| Geographic (region/city) — resolvable, deliberately not stored | 2 |
| ASN, VPN/proxy detection, response latency | 2+ |
| Campaign analytics, conversion tracking, live activity | 2+ |

GeoIP is optional because the MaxMind database cannot be redistributed in the
image. Without it the UI states that geographic data is unavailable rather than
rendering empty charts. The country is resolved at ingest, from the address, in
the same place the visitor hash is derived — there is no stored address to enrich
afterwards.

### Security

| Capability | Phase |
| --- | --- |
| Email/password auth (argon2id), account lockout | 1 |
| Server-side sessions, `__Host-` cookies | 1 |
| RBAC: roles, permissions, working evaluator | 1 |
| API keys with scopes | 1 |
| Rate limiting: per address, in-process, on credentials and the API | 1 |
| Rate limiting: shared across replicas | 2 |
| Abuse prevention: scheme allowlist, private-IP block, reserved/profanity alias filters, 404 probe limiting | 1 |
| Audit log — table only | 1 |
| Audit log — behavior | 2 |
| Password links, one-time links, max-click links, signed URLs | 2 |
| Malware scanning | 2 |
| MFA, OAuth, OIDC, SSO, SCIM | 3 |

### Collaboration

| Capability | Phase |
| --- | --- |
| Roles and permissions with evaluator | 1 |
| One auto-provisioned personal org + workspace per user | 1 |
| Organizations: sharing, invites, team management | 2 |
| Activity feed, comments | 2+ |

### Other surfaces

| Capability | Phase |
| --- | --- |
| REST API, OpenAPI docs, CLI (`lctl`) | 1 |
| Docker / Compose / Linux deployment | 1 |
| Project documentation: README, setup, configuration, usage, operations | 1 |
| Custom domains, QR codes, campaigns, webhooks, automation | 2 |
| Advanced analytics, compliance features, high availability | 3 |
| AI optimization, smart routing, predictive analytics, plugin system | 4 |
| GraphQL, SDKs, Terraform provider | future |
| Kubernetes, cloud deployments, multi-region | future |
| NFC integration | future |

### Non-goals

CRM · email marketing · website builder · advertising system · full CMS.

---

## Data model

20 entities, all created in Phase 1; most carry no behavior yet.

`User` · `Organization` · `Workspace` · `Role` · `Permission` · `Link` ·
`Destination` · `RoutingRule` · `Campaign` · `Folder` · `Tag` · `QRCode` ·
`Domain` · `ClickEvent` · `Visitor` · `Webhook` · `APIKey` · `AutomationRule` ·
`AuditLog` · `Notification`

31 tables. ERD and per-entity implementation status: `docs/data-model.md`
*(not yet written)*.

Rules:

- Tenancy chain is Organization → Workspace → Link. Every tenant-scoped table
  carries `workspace_id`.
- Alias uniqueness is `(domain_id, alias)`, not global.
- Dormant tables store anything structural as `jsonb`.
- `click_events`, `visitors` and `audit_logs` are RANGE-partitioned by month.
  Partitions are created by application code, never declared in SQL.
- All timestamps are `timestamptz`; every session runs in UTC.

---

## Architecture

Plan-level services — Frontend, API Gateway, Authentication, Link, Redirect,
Routing Engine, Analytics, Campaign, QR, Automation, Notification — are
**logical**, not a deployment topology. Phase 1 implements them as internal
packages in a single binary, with boundaries on those seams.

| Infrastructure | Phase 1 form |
| --- | --- |
| Database | PostgreSQL, two connection pools (application + dedicated redirect) |
| Cache | Redis, strictly optional at runtime |
| Queue | In-process bounded channel; upgrade path is Redis Streams |
| Workers | In-process scheduler, leader-elected by Postgres advisory lock |
| Object storage | Unused |
| CDN | Unused |

Invariants:

- The cache is optional. If unavailable, redirects fall through to Postgres and
  still meet the uncached target. Nothing correctness-critical depends on it.
- The HTTP layer is two handler trees. The redirect tree carries no session
  lookup, CSRF check or template rendering. Enforced by test.
- The redirect pool is separate from the application pool.
- Migrations run in-process at boot, before the listener opens, serialized
  across replicas by a Postgres session lock. Disableable for change-controlled
  deployments.

---

## Performance targets

| Surface | Target | Status |
| --- | --- | --- |
| Redirect, cached | <20ms | **met**: 100% of 240,001 requests under 20ms, generator p99 1.18ms |
| Redirect, uncached | <100ms | **met**: generator p99 1.92ms at 500 rps |
| API | <150ms typical | not yet measured |
| Dashboard | <250ms load | not yet measured |
| Analytics queries | <2s | not met for the dimension rollup: 16-21s per run at 5.7M events |

The redirect target is defined as: **server-side p99, cache hits only, measured
from a load generator on the same Docker network, excluding client RTT and TLS,
at 2,000 rps sustained for 2 minutes, with 100k links and 5M click events
seeded.** Both the generator's number and the server histogram are reported.
The measurement, how to reproduce it and what it found: [docs/slo.md](docs/slo.md).

Measured on one developer machine, so the shape transfers and the absolute values
do not. Notably, the cached result held while the analytics rollup was recomputing
whole days from 5.7M events every 60 seconds — which is the split pool and the
two-tier cache doing what they exist for.

Other measurements, none of them the SLO:

| Measurement | Value | Note |
| --- | --- | --- |
| Cached redirect, in-process incl. loopback client | ~270µs avg | shows nothing queries per request |
| Cached redirect through container, Windows host | ~13ms | Docker Desktop WSL2 bridge; not a useful signal |
| Cold start to serving, incl. migrations | ~12s | from empty volume |
| Seeding 100k links and 5M click events | ~85s | `lctl seed`, via COPY |

The server-side histogram the SLO calls for now exists:
`linkctrl_redirect_duration_seconds{cache,outcome}`, with a bucket boundary at
the 20ms target so "fraction under SLO" is a ratio of bucket counts. It is
scraped from a second listener on `METRICS_ADDR`, which compose does not
publish.

---

## Privacy

Requirements: GDPR · CCPA · cookie-free analytics · IP anonymization · data
retention policies · regional storage.

Implementation:

| Rule | Detail |
| --- | --- |
| No IP stored | `click_events` has no address column of any kind |
| Visitor identity | `HMAC(daily salt, ip ‖ 0 ‖ user-agent ‖ 0 ‖ workspace)`, truncated to 16 bytes |
| Cross-workspace | Workspace is in the message, so hashes differ per workspace |
| Salt lifetime | Rotates daily, deleted after 2 days — deletion is the de-identification step |
| Referrers | Host only; query strings discarded at ingest |
| Language | Primary subtag only (`en`, not `en-GB`) |
| Session/audit IPs | Prefix only: /24 IPv4, /48 IPv6 |
| Analytics retention | 395 days default, enforced hourly by dropping monthly partitions of `click_events` and `visitors`; a partition goes only once its newest possible row is outside the window, so data survives up to a month longer. `audit_logs` is exempt. |
| Geographic detail | Country only. Region and city are resolvable and deliberately not stored. |
| Regional storage | One instance per region via `organizations.data_region`; no row-level routing |

Consequence: the largest table holds no personal data and is out of scope for
subject-access and erasure requests.

Unique-visitor counts are estimates at daily resolution. The API returns that
caveat with the data.

---

## Build status

As of 2026-07-30. 18 of 18 milestones, then re-reviewed: a six-dimension audit
with adversarial verification confirmed 30 findings — among them a missing purge
job that inverted the alias-reservation promise, and query forwarding with no
write surface — all fixed the same day. Phase 1 is complete because the review
says so, not because the milestone counter reached its end.

| Area | State |
| --- | --- |
| Config, logging, health, graceful shutdown | done, verified |
| Schema, migrations, partitioning | done, verified |
| Authentication and sessions | done, verified |
| RBAC evaluator | done, verified |
| Link CRUD, aliases, tags, search | done, verified |
| REST API (links, tags, auth, stats) | done, verified |
| Redirect hot path and caching | done, verified |
| Analytics ingest, rollups, read API | done, verified |
| Background jobs | done, verified |
| API keys and scopes | done, verified |
| Dashboard UI | done, verified |
| OpenAPI document and `/docs` | done, verified |
| Prometheus metrics | done, verified |
| Documentation: README, setup, configuration, usage, operations | done |
| Enforcement: rate limits, 404 probe limits, GeoIP, retention | done, verified |
| Load validation of the redirect target | done, target met — [docs/slo.md](docs/slo.md) |
| Release packaging | done, verified — [docs/releasing.md](docs/releasing.md) |

Verification: 92 integration tests against real Postgres and Redis — including
a contract test that replays every OpenAPI operation against the live server —
plus unit, property and fuzz tests. All run under the race detector, and all of it
runs in CI alongside a two-architecture container build.

### Phase 1 scope not yet built

Every configuration variable now either takes effect or no longer exists, which
was the enforcement milestone's definition of done, and the redirect SLO is
measured. What remains, none of it blocking a release:

| Capability | State |
| --- | --- |
| Dimension rollup cost | The job recomputes whole days every 60s and takes 16-21s at 5.7M events, because 553k `(link, day, dimension, value)` tuples are re-upserted per run. Measured, not fixed: see [docs/slo.md](docs/slo.md#the-dimension-rollup-is-the-real-bottleneck-and-it-is-not-the-scan). The options are a narrower window, a longer cadence for dimensions than for totals, or accepting it with an alert. |
| Audit log behavior | Table only, by design — Phase 1 scope says table, Phase 2 says behavior. |
| Geographic region and city | Resolvable from the same database as the country and deliberately not stored. Nothing displays them, and city plus a timestamp approaches a location history. Storing them needs a UI and a reason, which makes it a Phase 2 decision. |

The last row narrows *Scope by phase*, which lists geographic analytics as
country/region/city in Phase 1. Country is delivered; the other two are
reclassified rather than quietly skipped.

---

## Known limitations

Deliberately accepted in Phase 1.

| Limitation | Consequence |
| --- | --- |
| DNS rebinding not defended against | A host resolving public at creation and private at click time is not caught. Detection needs resolution on the hot path. |
| Cache invalidation is single-replica | A second replica keeps its copy until TTL. Phase 2 adds pub/sub. |
| Rate limits are per instance | In-memory buckets, so N replicas allow N times the configured limit, and a restart resets them. Redis-backed limits would add a network round trip to the redirect path and make an optional dependency load-bearing. |
| Rate limits fail open | A full key table allows requests rather than refusing them, counted by `linkctrl_rate_limit_overflow_total`. A limiter is abuse mitigation, not an authorization boundary. |
| Behind a proxy, limits need `TRUSTED_PROXIES` | Otherwise every request carries the proxy's address and all traffic shares one bucket. This is a correctness requirement once a limit is on, not only an analytics one. |
| `links.click_count` is approximate | Written with the click rows, but an unclean shutdown loses at most one batch of both. |
| `api_keys.last_used_at` is approximate | Buffered and flushed on a 30s cadence, so an unclean shutdown loses the most recent timestamps. Authentication must not cost a write. |
| API keys cannot manage API keys | `apikeys.*` is not delegable, so minting and revoking need a session. Automating key rotation is Phase 2 work. |
| Analytics drops under overload | Bounded queue; drops counted and alertable. Backpressure would slow redirects. |
| The dimension rollup grows with traffic | 16-21s per 60s run at 5.7M events, and the cost is the 553k upserts a whole-day recompute implies rather than the scan. It will exceed its own interval as data grows. Measured in [docs/slo.md](docs/slo.md); the cached redirect path is unaffected, which is what the split pool is for. |
| Unique visitors are estimates | Carrier NAT merges people; network switches split one. Daily resolution. |
| Multi-day unique totals over-count | Sum of daily figures; exact values unrecoverable once salts are purged. |

---

## Success criteria

1. Replaces common URL shorteners.
2. Supports advanced routing and analytics.
3. Every UI feature has API support.
4. Runs self-hosted or cloud-hosted.
5. Scales from personal use to enterprise.
6. New features added without architectural rewrites.
