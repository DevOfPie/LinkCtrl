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

# Technology Stack

Chosen in Phase 1 planning. Recorded here because the rest of this document is
written as technology-agnostic requirements, which is no longer the whole truth.

Backend:
- Go 1.24
- chi (router)
- sqlc + pgx/v5 (type-safe SQL, no ORM)
- goose in library mode (migrations, embedded, run at boot)
- PostgreSQL 17
- Redis 7 (cache only, no persistence)

Frontend:
- Go html/template, server-rendered
- HTMX
- Tailwind CSS via the standalone CLI, so the production image contains no Node

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
- Visitor identity is a salted hash of IP + user agent. The salt rotates daily
  and old salts are deleted. Deleting the salt is the de-identification step:
  once it is gone, the hashes cannot be linked back to an IP even with the
  original data in hand.
- The workspace is part of the hashed message, so the same visitor produces a
  different hash in each workspace and two workspaces. analytics cannot be
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
- Go, with chi + sqlc + pgx + Postgres + Redis. Chosen for redirect latency and
  single-binary deployment, which is the shape a self-hosted product wants.
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
