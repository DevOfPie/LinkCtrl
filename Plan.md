# LinkCtrl — Project Plan

Scope contract and specification. States **what** is true, not why.

| | |
| --- | --- |
| Rationale for every decision | `docs/build-notes/decisions.md` |
| Investigations | `docs/adr/` |
| Dev environment | `docs/build-notes/development.md` |
| How the work is done | `docs/build-notes/workflow.md` |
| Security model and reporting | `docs/SECURITY.md` |
| Phase 2 definitions of done | `docs/build-notes/phase-details/` — one file per milestone |
| Out-of-spec findings | `docs/build-notes/deferred-findings.md` |
| Current progress | [Build Status](#build-status) |
| Last updated | 2026-07-31 |

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
| Malicious destination blocking, tiered by confidence | 2 |
| Blocked-attempt disputes, with owner review | 2 |

### Redirect engine

| Capability | Phase |
| --- | --- |
| Alias resolution, 302 redirect | 1 |
| Expiry enforcement (410 Gone) | 1 |
| Two-tier cache with negative caching | 1 |
| Query forwarding (default off) | 1 |
| Root redirect on the link domain, when it is a separate host | 1 |
| Deep-link path forwarding | 2 |
| Rules: country, region, city, language, browser, OS, device, date/time, referrer, query params, UTM, returning visitors (within-day). Cookies refused — see M34 | 2 |
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
| Dimension visualizations: choropleth map, richer charts, click through to the ranked list | 2 |
| ASN, VPN/proxy detection, response latency | 2+ |
| Campaign analytics, conversion tracking, live activity | 2+ |

GeoIP is optional because the MaxMind database cannot be redistributed in the
image. Without it the UI states that geographic data is unavailable rather than
rendering empty charts. The country is resolved at ingest, from the address, in
the same place the visitor hash is derived — there is no stored address to enrich
afterwards.

Phase 1 renders every dimension as the same ranked list of value and count, which
is exact, comparable across dimensions and completely flat: nobody looks at
`US 1425 / GB 822 / DE 510` and sees a map. Phase 2 gives each dimension the
visualization it deserves — a choropleth for country, shaded by share of clicks,
with a second layer for unique visitors — and keeps today's ranked list one click
away, because the list is the one that answers "exactly how many". The data is
already there: `link_dimension_daily` carries clicks and unique visitors per
value per day, so this is presentation work and needs no new column.

Two constraints it inherits. The unique-visitor layer carries the same caveat the
figures always do — daily-resolution estimates, and a multi-day total over-counts
anyone who visited on more than one day — so shading by it without repeating the
caveat would launder an estimate into a fact. And the map has to degrade the way
the rest of the geographic UI already does: no MaxMind database means it says so,
rather than rendering a world uniformly colored "unknown".

### Security

| Capability | Phase |
| --- | --- |
| Email/password auth (argon2id), account lockout | 1 |
| Server-side sessions, `__Host-` cookies | 1 |
| RBAC: roles, permissions, working evaluator | 1 |
| API keys with scopes | 1 |
| Rate limiting: per address, in-process, on credentials and the API | 1 |
| Rate limiting: shared across replicas — credentials and API only; the 404-probe limiter stays per-instance | 2 |
| Abuse prevention: scheme allowlist, private-IP block, reserved/profanity alias filters, 404 probe limiting | 1 |
| `domains.write` permission, for settings that affect every link at once | 1 |
| Per-domain ownership, so a workspace administers its own hostname | 2 |
| Audit log — table only | 1 |
| Audit log — behavior | 2 |
| In-app notifications | 2 |
| Password links, one-time links, max-click links, signed URLs | 2 |
| Malicious destination blocking: tiers, logging, notification, disputes | 2 |
| Third-party reputation and malware feeds — opt-in, off by default | 2 |
| MFA, OAuth, OIDC, SSO, SCIM | 3 |

Destination blocking is two threat models wearing one name, and the *Abuse
prevention* row above is the other half. What Phase 1 already refuses — non-`http(s)`
schemes, private, loopback, link-local, carrier-NAT and cloud-metadata addresses —
protects *this instance* from being used as an SSRF proxy. What Phase 2 adds
protects *visitors* from a destination that is hostile to them. They are not the
same policy and must not share an override switch: the Phase 1 refusals stay
unappealable at every tier, because the party they protect is not the party
appealing. An owner who could approve `169.254.169.254` on request would have
turned the review queue into the SSRF the validator exists to prevent.

Blocking is tiered by confidence, and the tiers differ in what it costs to
overrule them:

| Tier | Example | Overruled by |
| --- | --- | --- |
| Unappealable | Private and metadata addresses, non-`http(s)` schemes | Nothing. Not a moderation decision. |
| High confidence | Exact host matches from a curated embedded list | Editing the embedded list and rebuilding |
| Low confidence | Heuristics: punycode homographs, credentials in the URL, shortener chains, freshly registered domains | The instance owner, from the review queue |

Only part of this is new. `LINKCTRL_DESTINATION_BLOCKLIST` already refuses host
suffixes an operator names, and it is the shape the low-confidence tier grows out
of — what it lacks is a reason attached to the refusal, a record that the attempt
happened, and any way to change it short of a restart.

The middle tier costing a rebuild is the same mechanism `reserved.txt` and
`profanity.txt` already use, and it is not upstream gatekeeping — an operator owns
their copy and can patch it. It makes the dangerous override a deliberate,
reviewable, version-controlled change instead of a click at 2am. The price is that
a false positive there is expensive, which is why that tier is confined to exact
matches: heuristics never promote into it, or every false positive becomes a
rebuild and the feature becomes something operators route around.

Four constraints the implementation inherits rather than chooses:

- **Creation is not the only door.** A destination can be edited after the fact,
  so the check runs on update too. Re-checking links that were already accepted is
  a separate job and a separate decision, not something this quietly implies.
- **The review queue is an attack surface.** It exists to hand an instance owner a
  URL a stranger wants them to look at. The destination is rendered defanged and
  never as a live link, and the server never fetches it for a preview or a
  screenshot — a fetch would be exactly the SSRF the validator exists to refuse,
  arriving as a convenience feature.
- **Notification is about the dispute, not the refusal.** The creator already
  learns of a block synchronously, in the response. What needs delivering later is
  the review outcome, and to the owner, the fact that something is waiting. Both
  fit the dormant `notifications` table as it stands.
- **No destination leaves the box uninvited.** Reputation feeds mean sending
  someone's URLs to a third party, which is the opposite of what this project
  promises. Off by default, disclosed plainly when on, and never the mechanism the
  built-in tiers depend on.

Blocked attempts are recorded through the audit writer [M21](docs/build-notes/phase-details/m21.md)
builds, which is why the two rows sit together — the writer exists before the
features that emit into it, rather than being retrofitted afterwards.
`ip_prefix` rather than an address, matching the rest of the privacy stance, and
the attempted URL is stored as evidence and treated as hostile input everywhere
it is displayed.

Sequenced within Phase 2: blocking and logging first, which are useful on their
own; disputes and review after, since an appeal path is meaningless before
anything is being refused.

### Collaboration

| Capability | Phase |
| --- | --- |
| Roles and permissions with evaluator | 1 |
| One auto-provisioned personal org + workspace per user | 1 |
| Organizations: sharing, invites, team management | 2 |
| Workspace creation, and organization creation from an existing account | 2 |
| Self-serve signup, switchable at runtime by an owner | 2 |
| Optional SMTP mailer, for invitations and address verification | 2 |
| Activity feed, comments | 2+ |

Signup sits in Phase 2 next to invitations because of the second row, not because
it is hard. Registration provisions a new organization and workspace and makes
the registrant its owner, so opening signup on Phase 1's tenancy model admits
*tenants*, not colleagues: a new account sees nothing of an existing workspace and
cannot be given access to one. An owner switching on "allow sign-ups" to add a
co-worker would get a stranger with their own private instance-within-the-instance
— the feature working exactly as designed and being useless for the thing it was
turned on to do. It also has no email delivery to verify an address against.
Shipping it before invitations means shipping the surprise; shipping it after
means the toggle does what its label says.

`LINKCTRL_SIGNUP_MODE` stays in Phase 1 as it is, honest about its values and
narrow in its reach: it is read only by `POST /api/v1/auth/register`, so `open`
admits API clients and not browsers. The configuration reference says so.

### Other surfaces

| Capability | Phase |
| --- | --- |
| REST API, OpenAPI docs, CLI (`lctl`) | 1 |
| Docker / Compose / Linux deployment | 1 |
| Project documentation: README, setup, configuration, usage, operations | 1 |
| Separate management and link hostnames, instance-wide | 1 |
| Custom domains, per workspace and per link | 2 |
| QR codes, campaigns, webhooks, automation | 2 |
| Advanced analytics, compliance features, high availability | 3 |
| Entitlements or billing for organization creation | 3+ |
| AI optimization, smart routing, predictive analytics, plugin system | 4 |
| GraphQL, SDKs, Terraform provider | future |
| Kubernetes, cloud deployments, multi-region | future |
| NFC integration | future |

The two domain rows are separate features that sound like one. Phase 1 gives the
*instance* two hostnames — one for the dashboard and API, one for short links,
`manage.example.com` and `lnk.example.com` — chosen by the operator in
configuration and identical for every workspace. Phase 2 lets a *workspace* bring
its own hostname per link, which needs the verification and certificate machinery
the `domains` table already has columns for. Doing the first does not imply the
second, and the first is what makes the dashboard stop sharing a namespace with
the alias space.

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
*(not yet written — scheduled in [M45](docs/build-notes/phase-details/m45.md))*.

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
| Analytics retention | 395 days default, enforced hourly by dropping monthly partitions of `click_events` and `visitors`; a partition goes only once its newest possible row is outside the window, so data survives up to a month longer. |
| Audit retention | Its own window, `AUDIT_RETENTION_DAYS`, defaulting to 0 — keep forever. Never governed by the analytics number: an upgrade must not silently delete history assumed permanent. Growth is made visible instead, by `linkctrl_audit_log_bytes`. |
| Geographic detail | Country only. Region and city are resolvable and deliberately not stored. |
| Regional storage | One instance per region via `organizations.data_region`; no row-level routing |

Consequence: the largest table holds no personal data and is out of scope for
subject-access and erasure requests.

Unique-visitor counts are estimates at daily resolution. The API returns that
caveat with the data.

---

## Build status

As of 2026-07-31. 21 of 21 milestones, all of them in 0.1.0, tagged `v0.1.0` on
`main` and released. Phase 2 is planned and unstarted:
[Phase 2 build plan](#phase-2-build-plan).

The first eighteen were then re-reviewed: a six-dimension audit with adversarial
verification confirmed 30 findings — among them a missing purge job that
inverted the alias-reservation promise, and query forwarding with no write
surface — all fixed the same day, and it was the review that called them complete
rather than the milestone counter reaching its end. Scope then grew three times.
Separate hostnames and a round of defect fixes were asked for; M20 followed
directly from the first of those, because giving the instance a second public
hostname left that hostname's root answering `404`, and only the operator knows
where it should point instead.

The second of those exists because a fresh instance was stood up and used, which
found three defects the review had not — all of them places where the code is
internally consistent and disagrees with the product. Reading code against its own
intent and using the thing reach different bugs, and only one of those had been
done.

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
| Separate management and link hostnames | done, verified |
| Post-release defect fixes, and a demo seeder | done, verified |
| Root redirect on the link domain | done, verified |

Verification: 103 integration tests against real Postgres and Redis — including
a contract test that replays every OpenAPI operation against the live server —
plus unit, property and fuzz tests. All run under the race detector, and all of it
runs in CI alongside a two-architecture container build.

### Phase 1 scope not yet built

Every configuration variable either takes effect or no longer exists, which was
the enforcement milestone's definition of done, and the redirect SLO is measured.
Nothing in Phase 1 is outstanding: what follows is the record of the three
milestones added after the completeness review, then the two lists that hold work
which is deliberately not scheduled.

#### Added after the review, and built

The scope Phase 1 grew after 0.1.0's first eighteen milestones were reviewed. All
three are done. Their definitions of done — M18's hostname split, M20's root
redirect requirement table, M19's three defects, and the demo seeder — are in
[phase-details/phase-1.md](docs/build-notes/phase-details/phase-1.md), kept
because they are what the implementations are still held to.

#### Deferred findings

Moved to [deferred-findings.md](docs/build-notes/deferred-findings.md), which
carries the queue, the rules for what lands in it, and the review state of each
row. One open finding, cosmetic, unreviewed.

#### Previously unassigned, now scheduled

All three items Phase 1 accepted without an owner are scheduled in Phase 2:

| Capability | Now |
| --- | --- |
| Dimension rollup cost | [M37](docs/build-notes/phase-details/m37.md) — split cadence, staleness metric, re-measured at the 5.7M-event seed before the choropleth reads it. |
| Audit log behavior | [M21](docs/build-notes/phase-details/m21.md) — writer, read API, and its own retention window. |
| Geographic region and city | [M34](docs/build-notes/phase-details/m34.md) — resolved transiently for routing, never stored. `click_events.region` and `city` stay null, asserted by test. |

That last row narrows *Scope by phase*, which lists geographic analytics as
country/region/city in Phase 1. Country is delivered; the other two are
reclassified rather than quietly skipped.

---

## Phase 2 build plan

30 milestones, M21–M45, continuing Phase 1's numbering. Fractional numbers
insert without renumbering the work either side (Phase 1's M0.5 precedent):
`X.9` is reserved for scheduled reviews, `X.1`–`X.8` for scope added after the
plan was finalised — so far three, all 2026-07-31: the dashboard header at
M26.5, dark mode at M24.5 and bot blocking at M32.5. The numbering
rules are in [planning.md](docs/build-notes/planning.md). One milestone per
commit.

**Definitions of done live in
[`docs/build-notes/phase-details/`](docs/build-notes/phase-details/), one file per
milestone.** Read `m27.md` to build M27, and nothing else — the files do not
restate each other, and the rules every milestone inherits are stated once in that
directory's [README](docs/build-notes/phase-details/README.md).

Ordering is strict enablement: substrates land before their consumers, so event
emission, invalidation and delivery are never retrofitted into shipped features.
The cache key bumps exactly once (M34) and the durable counter is built exactly
once (M35).

| # | Milestone | Depends on | Discharges |
| --- | --- | --- | --- |
| [M21](docs/build-notes/phase-details/m21.md) | Audit log: behavior, retention, growth alerting | — | Audit log behavior · M20's root-redirect audit promise |
| [M22](docs/build-notes/phase-details/m22.md) | Notifications: in-app behavior | — | Blocking row's *notification* leg |
| [M23](docs/build-notes/phase-details/m23.md) | Cross-replica cache invalidation (pub/sub) | — | Known limitation: single-replica invalidation |
| [M24](docs/build-notes/phase-details/m24.md) | Shared rate limits (credentials and API) | — | Rate limiting shared across replicas |
| [M24.5](docs/build-notes/phase-details/m24.5.md) | Dark mode: theme tokens, system default, override | — *(before M25)* | — *(owner-added scope, 2026-07-31)* |
| [M25](docs/build-notes/phase-details/m25.md) | Workspace and organization switcher | — | Groundwork for M27/M28 |
| [M26](docs/build-notes/phase-details/m26.md) | Mailer: optional SMTP delivery | — | Optional SMTP mailer |
| [M26.5](docs/build-notes/phase-details/m26.5.md) | Dashboard header: identity menu and notification bell | — *(before M27)* | — *(owner-added scope, 2026-07-31)* |
| [M27](docs/build-notes/phase-details/m27.md) | Organizations: invitations and joining | M21 M22 M25 M26 | Organizations row (invites) |
| [M28](docs/build-notes/phase-details/m28.md) | Team management, workspaces, org creation | M27 | Organizations row (complete) · workspace and org creation |
| [M29](docs/build-notes/phase-details/m29.md) | Self-serve signup, switchable at runtime | M26 M27 | Self-serve signup |
| [M30](docs/build-notes/phase-details/m30.md) | Destination blocking: tiers and logging | M21 | Malicious destination blocking (tiers, logging) |
| [M31](docs/build-notes/phase-details/m31.md) | Blocked-attempt disputes and owner review | M30 M22 | Disputes with owner review |
| [M32](docs/build-notes/phase-details/m32.md) | Opt-in reputation and malware feeds | M30 M31 | Third-party feeds |
| [M32.5](docs/build-notes/phase-details/m32.5.md) | Bot blocking, per domain and per link | — *(before M33, M34)* | — *(owner-added scope, 2026-07-31)* |
| [M32.9](docs/build-notes/phase-details/m32.9.md) | **Mid-phase adversarial review** | M21–M32.5 | — |
| [M33](docs/build-notes/phase-details/m33.md) | Deep-link path forwarding | — *(before M34)* | Deep-link path forwarding |
| [M34](docs/build-notes/phase-details/m34.md) | Routing rules: conditions, first-match evaluation | M23 M30 M33 | Rules row · region/city decision |
| [M35](docs/build-notes/phase-details/m35.md) | Gated links: password, signed, one-time, max-click | M34 *(ordering)* | Password/one-time/max-click/signed |
| [M36](docs/build-notes/phase-details/m36.md) | Split testing: weighted, sequential, fallback, flags | M34 M35 | A/B testing row |
| [M37](docs/build-notes/phase-details/m37.md) | Dimension visualizations, rollup cadence first | — | Dimension visualizations · rollup cost |
| [M38](docs/build-notes/phase-details/m38.md) | Folders: API and tree UI | — | Folders row |
| [M39](docs/build-notes/phase-details/m39.md) | Per-domain ownership | M21 | Per-domain ownership |
| [M40](docs/build-notes/phase-details/m40.md) | Custom domains: verification and serving | M39 M23 | Custom domains row |
| [M41](docs/build-notes/phase-details/m41.md) | QR codes and campaigns | — | Other surfaces (QR, campaigns) |
| [M42](docs/build-notes/phase-details/m42.md) | Webhooks | M30 | Other surfaces (webhooks) |
| [M43](docs/build-notes/phase-details/m43.md) | Automation rules | M22 M35 M42 | Other surfaces (complete) |
| [M44](docs/build-notes/phase-details/m44.md) | API keys: rotation and scope choice | M21 | Known limitation: key rotation |
| [M44.9](docs/build-notes/phase-details/m44.9.md) | **Pre-release adversarial review** | M21–M44 | — |
| [M45](docs/build-notes/phase-details/m45.md) | Deferred findings, documentation pass, 0.2.0 | all | Phase close · `docs/data-model.md` |

### Phase 2 decisions

Taken 2026-07-31, before the plan was finalised. The *why* for each is in
decisions.md; this table is what was decided.

| # | Decision | Outcome |
| --- | --- | --- |
| D1 | Mailer | Ships. Optional SMTP (M26), off by default; emailed invites, address verification gating `open` signup, emailed dispute outcomes. Every consumer degrades mail-free. |
| D2 | Cookie / returning-visitor conditions | Returning-visitor ships with **within-day** semantics via the daily-salted visitor hash, cookie-free. The cookies condition is refused; the scope row is annotated. |
| D3 | Custom-domain TLS | Operator-managed Caddy on-demand TLS. The app tracks `ssl_status` only and never speaks ACME. |
| D4 | Version at phase end | 0.2.0. 1.0.0 stays a later phase's promise. |
| D5 | Audit retention default | Keep forever until configured, so an upgrade never silently deletes history. Growth is made observable instead: metric and alert (M21), owner notification (M22), emailed when a mailer exists (M26). |
| D6 | Invite-path provisioning | Membership only — no auto-provisioned personal org. The account stays capable of owning an org later, so nobody needs a second account (D16). |
| D7 | Signup ceiling vs invites | `closed` admits **no new account by any path**, invites included. The environment ceiling stays absolute, matching the recorded rule that no session can open a closed instance. Onboarding under `closed` costs one `.env` edit. |
| D8 | Sequential routing | Strict global order, via M35's durable counter. The write cost lands only on links using `sequential`. |
| D9 | API key rotation | Self-rotation into an identical-or-narrower successor with a bounded grace window. `apikeys.*` never becomes a key scope. Accepted trade: a leaked key can persist across rotations. |
| D10 | `disabled` automation action | **Not built.** `archived` and `disabled` are the same outcome on the redirect path, and `disabled` has no restore affordance — automation would strand links in a state the UI cannot leave. Action set is notify / webhook / archive. |
| D11 | QR output | SVG only. No image encoder in the dependency set. |
| D12 | New-vs-returning analytics split | `visitors` and `is_first_visit` stay dormant. No scope row asks for it; M45 trues up the comments that imply otherwise. |
| D13 | Freshly-registered-domains heuristic | Excluded from M30 — it needs a domain-age source, meaning egress. Noted as what M32's opt-in feed path can supply. |
| D14 | Alias-rename 409 | Stays absolute. No self-service release path in Phase 2. |
| D15 | Workspace creation | Included in M28. Workspace-scoped roles otherwise have no second object to scope to. |
| D16 | `orgs.create` | A permission, granted by default to self-registered users only. On a default instance that means the owner. It is also the call site a future entitlement check would hang on. |
| D17 | Billing groundwork | None in Phase 2. Recorded as a Phase 3+ scope row so the intent is written down rather than living in a conversation. |

D11, D13 and D14 were recorded from recommendation rather than chosen explicitly;
they are the cheapest to revisit.

### Phase 2 decisions taken after the plan was finalised

The table above is a record of what was decided before the plan closed and is not
edited. Decisions taken afterwards are appended here, keeping the same numbering.
The *why* for each is in decisions.md.

| # | Decision | Taken | Outcome |
| --- | --- | --- | --- |
| D18 | Permission delegability, as a rule | 2026-07-31 | A permission is **non-delegable** to an API key when reading it exposes an actor's identity tied to network data, **or** when holding it lets a key widen its own reach. Everything else is delegable. `NonDelegableScopes` is the only mechanism; no endpoint branches on "is this a session". Each milestone that adds a permission records which limb of the rule it matched, or that it matched neither. Generalises the `audit.read` call and covers M27, M28, M31, M38, M39, M44. |
| D19 | Audit-growth notification default | 2026-07-31 | On by default. A configurable size threshold over the audit partitions, defaulting to 5 GB, raises the [M22](docs/build-notes/phase-details/m22.md) owner notification. Extends D5: keep-forever is only safe if an untouched instance is warned, so the alert cannot itself require configuration. |
| D20 | Pub/sub subscriber reconnect | 2026-07-31 | A reconnecting subscriber **flushes both in-process tiers** — the alias memCache and the root-redirect cache. Invalidations published during the gap are unrecoverable, so the missed-invalidation window closes at reconnect rather than at TTL expiry. The cost is a cold cache after a Redis blip: a latency effect on an optional dependency, never a correctness one. |
| D21 | Light theme under M24.5's tokens | 2026-07-31 | One token set, correct in both themes. Where a pair cannot meet AA at today's light values, **the light value moves**, and each such change is recorded beside the token definition. M24.5 lands genuinely AA-clean rather than deferring its own contrast failures. |
| D22 | Default workspace resolution | 2026-07-31 | Last-used, remembered: the switcher persists the last selection and that becomes the default. A user setting can pin an explicit workspace instead, and its control defaults to *Last-Used*. Owner-added scope on [M25](docs/build-notes/phase-details/m25.md), 2026-07-31 — the milestone's no-op claim for today's single-membership users is unchanged. |
| D23 | Mailer delivery mechanism | 2026-07-31 | An outbox table plus a scheduler job, closing the mechanism [M26](docs/build-notes/phase-details/m26.md) deliberately left open. Queued mail survives a restart; invitations and address verification are the consumers, and losing one silently on a deploy is the failure worth an additive migration to avoid. |

### Not in Phase 2

- MFA, OAuth, OIDC, SSO, SCIM — Phase 3 by the scope table.
- Version history, scheduled changes, approval workflows — 3+.
- Re-checking already-accepted links against new blocklist tiers — a separate job
  and a separate decision.
- **A human check or dispute path for a blocked bot** — the second half of the
  request that produced [M32.5](docs/build-notes/phase-details/m32.5.md), parked
  by the owner on 2026-07-31. A challenge is a rendered, stateful, interactive
  surface, and the redirect tree is the one place this product keeps free of
  session lookups and template rendering. It is a milestone of its own, not a
  bullet on the blocking one. Until it exists, a misclassified human gets a 403
  with no recourse, which is why blocking defaults to off.
- Redis Streams as a work queue, for webhooks **or** the analytics recorder.
  Webhook delivery rides Postgres (M42); the recorder comment is trued up in M45.
- Sharing the 404-probe limiter across replicas — a network round trip on the 20ms
  path, and an optional dependency made load-bearing.
- Storing region or city.
- The cookies routing condition (D2), and the new-vs-returning analytics split (D12).
- `links.status = 'disabled'` gaining a writer (D10).
- Trash/restore UI — Phase 1 decided against it.
- Root-level `SECURITY.md` pointer — not done without being asked, and it has not
  been asked for.
- Bulk operations, ASN/VPN detection, campaign analytics, activity feed and
  comments — all "2+" rows, all deferred. Their substrates (folders, campaigns,
  the fixed rollup) ship this phase, so a Phase 3 version is better informed.

---

## Known limitations

Deliberately accepted in Phase 1.

| Limitation | Consequence |
| --- | --- |
| DNS rebinding not defended against | A host resolving public at creation and private at click time is not caught. Detection needs resolution on the hot path. |
| Invalidation needs Redis to cross replicas | Invalidations are broadcast on Redis pub/sub. With Redis down each replica falls back to `REDIRECT_TTL` staleness, which is correct but slower to converge; a reconnecting subscriber flushes its in-process tiers because pub/sub cannot replay what it missed. |
| Rate limits are shared only while Redis is reachable | The credential and API limits are enforced in Redis, so they hold across replicas. **On any Redis error each replica falls back to its own in-memory bucket, and the configured limit then applies per replica — N replicas allow N times it** until Redis returns. A restart also resets the local buckets. The 404-probe limiter is deliberately never shared: a Redis round trip on the redirect path would put an optional dependency on the 20ms budget. |
| Rate limits fail open | A full key table allows requests rather than refusing them, counted by `linkctrl_rate_limit_overflow_total`. A limiter is abuse mitigation, not an authorization boundary. |
| Behind a proxy, limits need `TRUSTED_PROXIES` | Otherwise every request carries the proxy's address and all traffic shares one bucket. This is a correctness requirement once a limit is on, not only an analytics one. |
| `links.click_count` is approximate | Written with the click rows, but an unclean shutdown loses at most one batch of both. |
| `api_keys.last_used_at` is approximate | Buffered and flushed on a 30s cadence, so an unclean shutdown loses the most recent timestamps. Authentication must not cost a write. |
| API keys cannot manage API keys | `apikeys.*` is not delegable, so minting and revoking need a session. Rotation is scheduled as self-rotation-only: [M44](docs/build-notes/phase-details/m44.md). |
| Analytics drops under overload | Bounded queue; drops counted and alertable. Backpressure would slow redirects. |
| The dimension rollup grows with traffic | 16-21s per 60s run at 5.7M events, and the cost is the 553k upserts a whole-day recompute implies rather than the scan. It will exceed its own interval as data grows. Measured in [docs/slo.md](docs/slo.md); the cached redirect path is unaffected, which is what the split pool is for. Scheduled: [M37](docs/build-notes/phase-details/m37.md). |
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
