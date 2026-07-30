# Link Platform Project Plan

## Vision

Build a modern, self-hostable link management platform that replaces traditional URL shorteners.

A link is a programmable resource with:
- destination
- routing rules
- analytics
- metadata
- automation
- permissions
- history

The platform should support individuals, developers, businesses, and enterprises.

## Goals

Primary:
- Fast URL shortening and redirecting
- Advanced routing
- Rich analytics
- Dynamic QR codes
- Campaign management
- Automation
- API-first design
- Self-hosted deployment
- Enterprise features

Principles:
- Everything available through API
- Links are editable without changing URLs
- Privacy-conscious analytics
- Modular architecture
- Scalable from personal use to enterprise

## Users

Individual:
- Personal links
- Portfolio/social links

Creator:
- Campaigns
- QR codes
- Analytics

Business:
- Marketing links
- Teams
- Domains

Developer:
- API
- Automation
- Self hosting

Enterprise:
- SSO
- Permissions
- Compliance
- High availability

# Core Features

## Link Management

Required:
- Create/edit/delete links
- Custom aliases
- Tags
- Search
- Archive
- Restore
- Metadata

Future:
- Folders (table ships in Phase 1; API and tree UI in Phase 2)
- Bulk operations
- Templates
- Import/export
- Version history
- Scheduled changes
- Approval workflows
- Malicious Link Detection

## Redirect Engine

Support rules based on:
- Country
- Region
- City
- Language
- Browser
- OS
- Device
- Date/time
- Referrer
- Query parameters
- UTM values
- Cookies
- Returning visitors

Support:
- A/B testing
- Weighted routing
- Percentage splits
- Sequential routing
- Feature flags
- Fallback destinations

## Analytics

Track:
- Clicks
- Visitors
- Timestamp
- Country
- Region
- City
- Device
- Browser
- OS
- Referrer
- Language
- ASN
- Bot detection
- VPN/proxy detection
- Response latency

Provide:
- Dashboards
- Trends
- Campaign analytics
- Conversion tracking
- Geographic reporting
- Live activity

Phase 1 covers: clicks, unique visitors, timestamp, device, browser, OS,
referrer, language, bot detection, plus dashboards and trends.

Geographic reporting (country/region/city) is Phase 1 but optional at runtime,
because the GeoIP database cannot be redistributed in the image. Without it the
UI says so rather than showing empty charts.

ASN, VPN/proxy detection, response latency, campaign analytics, conversion
tracking, and live activity are Phase 2 or later.

Click recording is asynchronous and must never delay a redirect. Under sustained
overload events are dropped and counted rather than queued indefinitely, and the
click counter on a link is therefore approximate.

## QR Codes

Support:
- Dynamic QR codes
- Editable destinations
- Branding
- Templates
- Analytics
- Print tracking
- Resize on generation

Future:
- NFC integration

## Campaigns

Support:
- Campaign grouping
- Goals
- UTM templates
- Multiple links
- Scheduling
- Analytics

## Domains

Support:
- Custom domains
- Multiple domains
- SSL
- Verification
- Domain health monitoring

## Automation

Triggers:
- Link created
- Link clicked
- First click
- Click threshold reached
- Expiration
- Campaign completion

Actions:
- Webhook
- Email
- Slack
- Discord
- Teams
- Change destination
- Archive
- Notify user

## Collaboration

Support:
- Organizations
- Workspaces
- Teams
- Roles
- Permissions
- Activity feed
- Comments
- Audit logs

Phase 1 ships Roles and Permissions with a working evaluator, and one
auto-provisioned personal Organization and Workspace per user. There is no
sharing, no invites, and no team management yet — but every tenant-scoped row
already carries its workspace, so Phase 2 turns this on without a data
migration.

Teams, activity feed, and comments are Phase 2 or later.

## Security

Phase 1:
- Authentication (email/password, argon2id, account lockout)
- Rate limiting
- Abuse prevention (scheme allowlist, private-IP block, reserved/profanity
  alias filters, 404 probe limiting)
- Expiring links
- API keys with scopes
- Audit log table (behavior in Phase 2)

Phase 2:
- Malware scanning
- Password links
- One-time links
- Signed URLs
- Audit log behavior

Phase 3:
- MFA
- OAuth
- OIDC
- SSO

# API

Requirements:
- REST API
- OpenAPI documentation
- Webhooks
- API keys
- CLI support

Future:
- GraphQL
- SDKs
- Terraform provider

# Data Model

Entities:
- User
- Organization
- Workspace
- Role
- Permission
- Link
- Destination
- RoutingRule
- Campaign
- Folder
- Tag
- QRCode
- Domain
- ClickEvent
- Visitor
- Webhook
- APIKey
- AutomationRule
- AuditLog
- Notification

All 20 entities are created in Phase 1. Most carry no behavior yet — the tables
exist so later phases are additive migrations rather than rewrites. Anything
structural in a dormant table is stored as jsonb, because its real shape will
not survive contact with the feature that eventually uses it.

Tenancy chain: Organization -> Workspace -> Link. Every tenant-scoped table
carries workspace_id from Phase 1, even though Phase 1 gives each user a single
auto-provisioned personal workspace. These columns are the ones that must be
correct now; Phase 2 columns are expected to be adjusted.

Alias uniqueness is per-domain, not global, from Phase 1 onward — so Phase 2
custom domains need no data migration.

See docs/data-model.md for the ERD and a table mapping each entity to its phase
and whether behavior is implemented.

# Architecture

Services:
- Frontend
- API Gateway
- Authentication
- Link Service
- Redirect Service
- Routing Engine
- Analytics Service
- Campaign Service
- QR Service
- Automation Service
- Notification Service

These are logical services, not a deployment topology. In Phase 1 they are
internal packages inside a single binary, with package boundaries drawn along
these exact seams so later phases can extract any of them into its own process
without a rewrite. Shipping eleven containers for an MVP would make
"docker compose up" hostile to the self-hosters this platform is for.

Infrastructure:
- Database
- Cache
- Queue (Phase 1: in-process bounded channel; upgrade path is Redis Streams)
- Workers (Phase 1: in-process scheduler, leader-elected via a Postgres
  advisory lock so extra replicas do not double-run jobs)
- Object storage (unused in Phase 1)
- CDN (unused in Phase 1)

The cache is strictly optional at runtime: if it is unavailable, redirects fall
through to the database and still meet the uncached target. Nothing
correctness-critical may depend on it.

The HTTP layer is two handler trees rather than one, and the split is what the
redirect target depends on. The application tree carries session lookup and
security headers; the redirect tree carries almost nothing, because a session
check alone is a database round trip. Only client-address resolution is shared,
since analytics needs it and it costs a header read. A test wires an
authenticator that fails if called and asserts the redirect path never touches
it, so the split cannot erode into a comment.

There are two database pools for the same reason: a small dedicated pool serves
redirects, so a slow analytics query holding application connections cannot
leave a redirect waiting to acquire one.

# Technology Stack

Chosen in Phase 1 planning and corrected against what was actually built.
Recorded here because the rest of this document is written as
technology-agnostic requirements, which is no longer the whole truth.

Backend:
- Go 1.26
- net/http ServeMux, not a third-party router. Planning assumed chi; the
  standard library's routing has covered every requirement so far, including
  method and wildcard patterns, so the dependency was never added. Two
  consequences are recorded in the Decision Log: ServeMux rejects some pattern
  pairs as ambiguous where chi would silently pick one, and middleware is
  composed by hand rather than by a router API.
- sqlc + pgx/v5 (type-safe SQL, no ORM)
- goose in library mode (migrations, embedded, run at boot)
- PostgreSQL 17
- Redis 7 (cache only, no persistence)
- golang.org/x/crypto argon2id for passwords
- golang.org/x/sync singleflight to collapse cache stampedes
- google/uuid for UUIDv7

Frontend:
- Go html/template, server-rendered
- HTMX
- Tailwind CSS via the standalone CLI, version-pinned and checksum-verified in
  the Docker build, so the production image contains no Node

API:
- Spec-first OpenAPI: api/openapi.yaml is the source of truth
- oapi-codegen generates server interfaces and the client used by tests
- Swagger UI embedded in the binary, no CDN dependency

Identity:
- UUIDv7, generated in the application for index locality

Operations:
- log/slog structured logging, Prometheus metrics
- Docker + Docker Compose; Caddy for TLS in production
- k6 for load validation

# Deployment

Required:
- Docker
- Docker Compose
- Linux support

The bare compose file is the "try it on localhost" path. Caddy is the documented
default for any real deployment: session cookies use the __Host- prefix, which
browsers only accept over HTTPS, and automatic ACME removes the single most
common self-hosting failure.

Migrations run in-process at boot, before the listener opens, serialized across
replicas by a Postgres session lock. An operator who needs migrations gated by
change control can disable that and run them explicitly.

Future:
- Kubernetes
- Cloud deployments
- Multi-region

# Performance Targets

Redirect:
- <20ms cached
- <100ms uncached

API:
- <150ms typical response

Dashboard:
- <250ms load

Analytics:
- <2s queries

A latency target without a measurement definition cannot be passed or failed.
The redirect target is therefore defined as:

- server-side p99, not average
- for cache-hit redirects (the uncached target covers misses)
- measured from a load generator on the same Docker network
- excluding client round-trip time and TLS handshake
- at 2,000 requests/second sustained for 2 minutes
- with 100,000 links and 5,000,000 click events seeded

Both the load generator's number and the server's own histogram are reported,
and the gap between them is the network and proxy overhead an operator needs to
know when putting a reverse proxy or CDN in front. See docs/slo.md.

Measured so far — none of these is the SLO, and the target is not yet claimed
as met:

- Cached redirect, in-process including a loopback HTTP client: ~270us average.
  Enough to show nothing queries per request; not a p99, and not under load.
- Cached redirect through the container on a Windows host: ~13ms. This is
  dominated by Docker Desktop's WSL2 network bridge and is not a useful signal;
  it is recorded only so nobody later mistakes it for a regression.
- Cold start from an empty volume to serving, including migrations and
  partition creation: ~12s.

The redirect target is verified or not verified by the load run described
above. Until that runs on Linux at the stated rate and dataset size, the
<20ms figure is a design target rather than a measurement.

# Privacy

Support:
- GDPR
- CCPA
- Cookie-free analytics
- IP anonymization
- Data retention policies
- Regional storage

Method, so these are designs rather than intentions:

- The click event table has no IP column at all. Not a truncated IP, not a
  hashed IP stored alongside the row — no column.
- Visitor identity is HMAC(daily salt, ip || user agent || workspace). HMAC
  rather than a hash of salt||data, which is length-extendable, and the fields
  are NUL-separated so a crafted user agent cannot shift the boundary to
  collide with a different address.
- The salt rotates daily and is deleted two days later. Deleting it is the
  de-identification step, not housekeeping: once it is gone, the hashes cannot
  be linked back to an IP even by someone holding the original addresses. Two
  days is the minimum that lets a day be rolled up and then finalized.
- The workspace is part of the hashed message, so the same visitor produces a
  different hash in each workspace, and two workspaces' analytics cannot be
  joined to follow one person across both. The salt itself is per-day and
  shared; the non-correlation comes from the message, not the key.
- Consequence worth stating plainly: the largest table in the system contains no
  personal data, so it is out of scope for subject-access and erasure requests
  entirely.
- Default analytics retention is 395 days (13 months, so year-over-year
  comparison works), configurable, enforced by dropping whole monthly partitions
  rather than deleting rows.
- Regional storage means one instance per region, selected by an organization's
  data_region. Row-level regional routing is not attempted.

Honest limitation: cookie-free unique-visitor counts are estimates at daily
resolution. Carrier NAT collapses many real people behind one address into a
single visitor; a person moving from WiFi to cellular counts as two. This is
surfaced in the UI rather than presented as an exact figure. See docs/privacy.md.

# Build Status

As of 2026-07-30. Updated as milestones land, so this document says what is
true rather than what was intended.

Working and verified against a real Postgres and Redis:

- Configuration, structured logging, health endpoints, drain-aware shutdown
- Full 31-table schema, migrations run at boot, monthly partitions maintained
- Authentication: argon2id passwords, server-side sessions, account lockout
- RBAC: four roles with a working evaluator, enforced in the service layer
- Link CRUD, custom aliases, tags, search, archive/restore, soft delete
- REST API with problem+json errors and keyset pagination
- Redirect hot path with a two-tier cache and negative caching
- Analytics: cookie-free ingest, daily rollups, read API
- Background jobs elected by a Postgres advisory lock

Not yet built: API keys and scopes, the dashboard UI, the OpenAPI document and
its docs page, Prometheus metrics, the load run that validates the redirect
target, and release packaging.

Test coverage at this point: 47 integration tests against a real database plus
unit and fuzz tests, all under the race detector.

## Known limitations, deliberately accepted

Recorded here rather than left to be discovered:

- Destination validation blocks literal private addresses but does not defend
  against DNS rebinding, where a hostname resolves public at creation and
  private when a visitor follows the link. Catching that needs resolution on
  the hot path, which the latency target cannot absorb, or an egress policy
  outside this process.
- Cache invalidation clears Redis and the invalidating process's own memory
  tier. A second replica keeps its copy until the entry expires, so an edit can
  take up to the cache TTL to be visible everywhere. Phase 1 targets
  single-replica deployments; Phase 2 adds pub/sub invalidation.
- links.click_count is approximate. It is written in the same transaction as
  the click rows, so the two never disagree, but an unclean shutdown loses at
  most one batch of both. Nothing requiring an exact count may read it.
- Analytics events are dropped rather than queued when the buffer is full.
  Drops are counted and alertable; the alternative is applying backpressure to
  a redirect, which trades a complete record for a slow site.
- Unique-visitor figures are estimates at daily resolution, for the reasons in
  the Privacy section. The API returns that caveat alongside the numbers.

# Roadmap

## Phase 1 MVP

Implement:
- Authentication (email/password, server-side sessions, argon2id)
- RBAC (roles, permissions, working permission evaluator)
- Link CRUD
- Custom aliases
- Expiring links (expires_at enforced, 410 Gone past expiry)
- Redirect service
- Tags
- Search
- Basic analytics (cookie-free)
- REST API + OpenAPI documentation
- API keys with scopes
- Docker deployment
- Operator CLI (lctl)

Create schema only, no behavior:
- Folders
- All remaining entities from the Data Model (see the note in that section)

Explicitly deferred to Phase 2:
- Password-protected links
- One-time links and max-click links

  Reason: an accurate click counter cannot live in a cache that is allowed to
  evict or restart empty. Phase 1 runs Redis as a pure cache with no
  persistence, so enforcing "exactly N clicks" would either be wrong or would
  require a durable counter store. Deferred rather than shipped incorrectly.
  The columns exist and the API rejects setting them with a 422.

## Phase 2

Add:
- Custom domains
- QR codes
- Routing rules
- Organizations (multi-user workspaces, invites)
- Campaigns
- Webhooks
- Automation
- Audit log behavior (the table ships in Phase 1)
- Folders API and tree UI
- Password links, one-time links, max-click links
- Deep-link and query forwarding
- Malicious link detection (Safe Browsing)

## Phase 3

Add:
- SSO
- SCIM
- Advanced analytics
- Compliance features
- High availability

## Phase 4

Add:
- AI optimization
- Smart routing
- Predictive analytics
- Plugin system

# Non Goals

Do not initially build:
- CRM
- Email marketing platform
- Website builder
- Advertising system
- Full CMS

# Success Criteria

The project succeeds when:
- It replaces common URL shorteners
- It supports advanced routing and analytics
- Every UI feature has API support
- It can run self-hosted or cloud hosted
- It scales from personal use to enterprise
- New features can be added without architectural rewrites

# Core Rule

Links are programmable, observable, secure resources.

# Decision Log

## 2026-07-29 — Phase 1 planning

Stack:
- Go, with sqlc + pgx + Postgres + Redis. Chosen for redirect latency and
  single-binary deployment, which is the shape a self-hosted product wants.
  (Planning also assumed chi; see the 2026-07-30 entry for why it was not
  adopted.)
- Server-rendered templates + HTMX rather than a separate SPA. Fastest path to
  the dashboard target, one container, no Node in production. The API/UI parity
  rule is enforced by making both call the same service layer, not by convention.
- All 20 data-model entities created up front rather than added per phase. Cost
  is dead schema; benefit is that later phases are additive. Contained by using
  jsonb for structure in dormant tables.
- RBAC implemented for real in Phase 1 rather than a hardcoded owner role, even
  though Organizations are Phase 2. Retrofitting authorization after features
  exist is where permission bugs come from.
- Folders reduced to schema-only. Listed as Required under Link Management but
  absent from the Phase 1 roadmap; resolved in favor of the roadmap.
- Password and one-time links deferred. See the reason in the Roadmap section.
- 302 redirects, never 301. Links are editable by design, and a permanent
  redirect cached in browsers and intermediaries cannot be recalled.
- Redis is cache-only with no persistence. This is what forces the one-time-link
  deferral, and it is the right trade: it keeps the operational story simple and
  makes a cache outage a degradation instead of an outage.
- Analytics writes are asynchronous and may drop under sustained overload rather
  than delay a redirect. Bounded queue, counted drops, flush on shutdown.
- SLO validated at 2,000 rps / 100k links / 5M events.

## 2026-07-30 — decisions made while building

These came out of implementation rather than planning. Several correct or
replace something in the entry above.

- No third-party router. Planning assumed chi; net/http's ServeMux covered
  every requirement, so the dependency was never added. Two consequences worth
  knowing: ServeMux refuses some pattern pairs as ambiguous where chi would
  quietly choose one, which is how the alias catch-all ended up registered
  without a method and filtering methods itself; and middleware is composed by
  hand, which is a few lines rather than a framework concept.

- Partitions are never declared in the SQL that sqlc reads. Testing this before
  writing the schema (docs/adr/0001) found that sqlc does not fail on a
  partitioned parent, as expected — it silently emits a duplicate model struct
  for every child partition, so generated code would grow a dead type every
  month. Partitions are created by application code instead.

- Both nullable and non-nullable forms of every sqlc type override must be
  declared, or nullable columns fall back to driver wrapper types and those
  leak into the domain layer.

- Partition bounds resolve against the session timezone at DDL time. Verified
  empirically: identical DDL under UTC and a US timezone produced bounds four
  hours apart, leaving a gap that silently routes rows to the default
  partition. UTC is now pinned in the connection pool, the server and the
  container, and startup fails if a session is not UTC.

- Sessions are stored in Postgres, not Redis. Redis here has no persistence and
  evicts under memory pressure, so sessions kept there would log everyone out
  at an arbitrary moment.

- The JSON authentication API ships before the HTML forms. "Every UI feature
  has API support" is a success criterion, and building the form first then
  retrofitting an endpoint is precisely how that gets broken.

- Rollups recompute whole days and upsert rather than accumulating
  incrementally. An incremental design double-counts on any retry and, once it
  drifts, stays wrong invisibly.

- User-agent classification is hand-written rather than a library. The question
  is coarse — phone or desktop, roughly which browser — and a few dozen ordered
  substring checks answer it on a path that runs for every click. Accuracy on
  unusual agents is traded away deliberately.

- The alias alphabet keeps the digits 0 and 1 because the letters o, l and i
  are excluded, which is what brings it to exactly 32 characters and makes
  random generation unbiased. Aliases are lowercase-canonical, so /GitHub and
  /github are the same link.

- Profanity filtering matches short terms as whole tokens and only unambiguous
  ones as substrings. Naive substring matching rejected "therapist",
  "raccoon" and "fire-retardant" during development.

- Deleted links are kept for 30 days and restorable. An alias belonging to a
  link that received traffic is reserved permanently on purge, because it
  exists on printed material and in other people's bookmarks; reissuing it
  would redirect someone else's audience.

Defaults chosen without strong constraints, cheap to revisit before the schema
is frozen:
- /docs is public
- aliases are lowercase-canonical and case-insensitive
- bot clicks are recorded but excluded from default charts
- analytics retention 395 days
- query forwarding off by default; deep-link forwarding is Phase 2
- new instances have signup closed

Development environment:
- Windows host with Go, Docker Desktop, and Node installed natively.
- The working copy must not live in OneDrive. Sync interferes with the Go build
  cache and git internals, and bind-mounted cloud placeholder files read as
  empty inside containers.
