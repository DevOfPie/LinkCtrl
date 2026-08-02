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
| Moving links between workspaces | 3 |
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
| Low confidence | Heuristics: punycode homographs, credentials in the URL, shortener chains — *freshly registered domains excluded, D13* — and a runtime-mutable Postgres blocklist | The instance owner, from the review queue |

Only part of this is new. `LINKCTRL_DESTINATION_BLOCKLIST` already refuses host
suffixes an operator names, and it is the shape the low-confidence tier grows out
of — what it lacks is a reason attached to the refusal, a record that the attempt
happened, and any way to change it short of a restart. M30 gave it all three: the
variable now seeds a `blocked_destinations` table at boot and is reconciled
against it, so the entries gained a tier, a reason code and an audit trail
without the operator changing anything.

M30 also took two override switches away, because a tier documented as having
none must not have any. `LINKCTRL_DESTINATION_BLOCK_PRIVATE_IPS` is removed —
private and metadata addresses are refused unconditionally — and
`LINKCTRL_DESTINATION_SCHEMES` is confined at startup to a subset of
`http,https`, so it can narrow the allowlist and never widen it. Freshly
registered domains is the one listed heuristic that did not ship (D13).

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

  [M32](docs/build-notes/phase-details/m32.md) shipped that exception, so the
  promise is now stated precisely rather than left to be read as absolute. What
  is true of every instance: **no destination is sent anywhere unless the
  operator sets `LINKCTRL_FEED_URL`, and the shipped default is unset.** What is
  true once they do: the destination — and nothing else, no account, no
  workspace, no instance name — is sent to the named feed on every link create,
  link update, root-redirect change and dispute filing. Never when a visitor
  follows a link, and existing links are not re-checked in the background.

  Four bounds hold whatever the operator configures. The feed is asked **last**,
  only about destinations every built-in tier already accepted, so no built-in
  refusal is ever sent out and none of them changes answer with a feed on, off or
  erroring. A feed that does not answer **fails open** to those tiers and
  increments `linkctrl_destination_feed_checks_total{result="error"}`. Its
  verdicts are low-confidence: disputable, and the instance owner can overrule one
  from the review queue, which also stops that host being sent again. And the
  instance discloses the whole of the above at **`/feeds`**, to every signed-in
  user, in both states — a read-only page with no controls, because only the
  operator can change any of it (D40).

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
| Self-serve signup, configured by the operator | 2 |
| Optional SMTP mailer, for invitations and address verification | 2 |
| Activity feed, comments | 2+ |

Signup sits in Phase 2 next to invitations because of the second row, not because
it is hard. Registration provisions a new organization and workspace and makes
the registrant its owner, so opening signup on Phase 1's tenancy model admits
*tenants*, not colleagues: a new account sees nothing of an existing workspace and
cannot be given access to one. Switching on "allow sign-ups" to add a
co-worker would get a stranger with their own private instance-within-the-instance
— the feature working exactly as designed and being useless for the thing it was
turned on to do. It also has no email delivery to verify an address against.
Shipping it before invitations means shipping the surprise; shipping it after
means the setting does what its label says.

`LINKCTRL_SIGNUP_MODE` stayed in Phase 1 as it was, honest about its values and
narrow in its reach: read only by `POST /api/v1/auth/register`, so `open`
admitted API clients and not browsers. [M29](docs/build-notes/phase-details/m29.md)
built the rest and left the variable as the mode (D38). A browser form exists,
`open` requires a mailer because registration confirms the address before the
account is created, and the shipped default stays `closed`. Who sets it did not
move: it is the operator's, in the environment, because this product has no
instance-level principal for it to belong to.

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

33 tables: the 31 Phase 1 ones, plus `mail_outbox` ([M26](docs/build-notes/phase-details/m26.md), D23)
and `invitations` ([M27](docs/build-notes/phase-details/m27.md)). Neither is a new
entity — one is a delivery queue, the other a grant with a lifetime — and both are
typed rather than dormant jsonb because the feature reading them shipped with them.

ERD and per-entity implementation status: `docs/data-model.md`
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

33 milestones, M21–M45, continuing Phase 1's numbering. Fractional numbers
insert without renumbering the work either side (Phase 1's M0.5 precedent):
`X.9` is reserved for scheduled reviews, `X.1`–`X.8` for scope added after the
plan was finalised — so far five: dark mode at M24.5, the dashboard header at
M26.5, the Redis stall bound at M26.6 and bot blocking at M32.5, all
2026-07-31; then organization deletion at M28.5 and the demo's own data at M33.5, both
2026-08-01. The numbering
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
| [M26.6](docs/build-notes/phase-details/m26.6.md) | Bounded Redis failure, when the server never answers | — *(before M32.5, M34, M40)* | — *(owner-approved finding F2, 2026-07-31)* |
| [M27](docs/build-notes/phase-details/m27.md) | Organizations: invitations and joining | M21 M22 M25 M26 | Organizations row (invites) |
| [M28](docs/build-notes/phase-details/m28.md) | Team management, workspaces, org creation | M27 | Organizations row (complete) · workspace and org creation |
| [M28.5](docs/build-notes/phase-details/m28.5.md) | Organization deletion and tenancy teardown | M28 | — *(owner-added scope, 2026-08-01)* |
| [M29](docs/build-notes/phase-details/m29.md) | Self-serve signup, configured by the operator | M26 M27 | Self-serve signup |
| [M30](docs/build-notes/phase-details/m30.md) | Destination blocking: tiers and logging | M21 | Malicious destination blocking (tiers, logging) |
| [M31](docs/build-notes/phase-details/m31.md) | Blocked-attempt disputes and owner review | M30 M22 | Disputes with owner review |
| [M32](docs/build-notes/phase-details/m32.md) | Opt-in reputation and malware feeds | M30 M31 | Third-party feeds |
| [M32.5](docs/build-notes/phase-details/m32.5.md) | Bot blocking, per domain and per link | — *(before M33, M34)* | — *(owner-added scope, 2026-07-31)* |
| [M32.9](docs/build-notes/phase-details/m32.9.md) | **Mid-phase adversarial review** | M21–M32.5 | — |
| [M33](docs/build-notes/phase-details/m33.md) | Deep-link path forwarding | — *(before M34)* | Deep-link path forwarding |
| [M33.5](docs/build-notes/phase-details/m33.5.md) | A demo that shows the phase, not just its links | M32.9 | — *(owner-added scope, 2026-08-01)* |
| [M34](docs/build-notes/phase-details/m34.md) | Routing rules: conditions, first-match evaluation | M23 M30 M33 | Rules row · region/city decision |
| [M35](docs/build-notes/phase-details/m35.md) | Gated links: password, signed, one-time, max-click | M34 *(ordering)* | Password/one-time/max-click/signed |
| [M36](docs/build-notes/phase-details/m36.md) | Split testing: weighted, sequential, fallback, flags | M34 M35 M30 | A/B testing row |
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
| D18 | Permission delegability, as a rule | 2026-07-31 | A permission is **non-delegable** to an API key when reading it exposes an actor's identity tied to network data, **or** when holding it lets a key widen its own reach. Everything else is delegable. `NonDelegableScopes` is the only mechanism for whether a key may **hold** a permission; [D43](#phase-2-decisions-taken-after-the-plan-was-finalised) adds a second and narrower one for what a key may **produce** with one it holds, and anything branching on credential type outside those two is still a defect. Each milestone that adds a permission records which limb of the rule it matched, or that it matched neither. Generalises the `audit.read` call and covers M27, M28, M31, M38, M39, M44. |
| D19 | Audit-growth notification default | 2026-07-31 | On by default. A configurable size threshold over the audit partitions, defaulting to 5 GB, raises the [M22](docs/build-notes/phase-details/m22.md) owner notification. Extends D5: keep-forever is only safe if an untouched instance is warned, so the alert cannot itself require configuration. |
| D20 | Pub/sub subscriber reconnect | 2026-07-31 | A reconnecting subscriber **flushes both in-process tiers** — the alias memCache and the root-redirect cache. Invalidations published during the gap are unrecoverable, so the missed-invalidation window closes at reconnect rather than at TTL expiry. The cost is a cold cache after a Redis blip: a latency effect on an optional dependency, never a correctness one. |
| D21 | Light theme under M24.5's tokens | 2026-07-31 | One token set, correct in both themes. Where a pair cannot meet AA at today's light values, **the light value moves**, and each such change is recorded beside the token definition. M24.5 lands genuinely AA-clean rather than deferring its own contrast failures. |
| D22 | Default workspace resolution | 2026-07-31 | Last-used, remembered: the switcher persists the last selection and that becomes the default. A user setting can pin an explicit workspace instead, and its control defaults to *Last-Used*. Owner-added scope on [M25](docs/build-notes/phase-details/m25.md), 2026-07-31 — the milestone's no-op claim for today's single-membership users is unchanged. |
| D23 | Mailer delivery mechanism | 2026-07-31 | An outbox table plus a scheduler job, closing the mechanism [M26](docs/build-notes/phase-details/m26.md) deliberately left open. Queued mail survives a restart; invitations and address verification are the consumers, and losing one silently on a deploy is the failure worth an additive migration to avoid. |
| D24 | Header menu mechanism | 2026-07-31 | **The Popover API**, not `<details>`/`<summary>`. A disclosure cannot close on Escape in any browser, and [M26.5](docs/build-notes/phase-details/m26.5.md) asked for exactly that; a popover is equally declarative, needs no script and no CSP waiver, and adds outside-click dismissal. The cost is explicit: a top-layer element ignores its ancestor's containing block, so positioning is verified in a browser rather than asserted from markup, and the supported floor rises to Chrome 114 / Safari 17 / Firefox 125. Chosen over the cheaper amend-the-bullet option because this header is the idiom M28, M31, M38 and M41 will each copy. |
| D25 | Verification tooling vs the stdlib-only rule | 2026-07-31 | **Shipped code stays stdlib-only; tooling that only verifies it may use Node**, as long as Node stays out of everything except required test code. Settled when [M26.5](docs/build-notes/phase-details/m26.5.md) needed a WebKit engine to check popover positioning and the machine had no Node, no Playwright and no WebKit. The inherited *`ui` stays stdlib-only* rule governs what ships, not what measures it. |
| D26 | Bounding a stalled Redis | 2026-07-31 | **One total budget for an invalidation — `REDIS_INVALIDATE_BUDGET`, 250ms — enforced by the caller rather than by a context go-redis will not honour.** A stalled read costs the client's `REDIS_READ_TIMEOUT` whatever deadline it was handed, so three retries multiplied a documented knob by three; the budget covers the whole loop instead. 250ms fits every retry that works today (210ms) and is 1.7% of `HTTP_REQUEST_TIMEOUT`. `MaxRetries` and `REDIS_DIAL_TIMEOUT` are left as they were: measured for [M26.6](docs/build-notes/phase-details/m26.6.md) and neither contributes to this failure, so the redirect path is untouched. |
| D27 | Invite binding | 2026-07-31 | **Bound to the invited address**, not a bearer link. Redemption checks the redeeming account's address against the address invited, so a forwarded or leaked link cannot add a stranger. Accepted cost: an invite pasted into a team channel for "whoever needs this" is refused, and joining under a different address needs a re-invite. The address comparison must fail identically whether the address is unknown, already a member, or not the invited one — M27's no-enumeration bullet governs it. |
| D28 | Invite role ceiling | 2026-07-31 | **Any role at or below the inviter's own rank** (`owner` 10, `admin` 20, `editor` 30, `viewer` 40). An admin may invite an admin, editor or viewer, never an owner. Settles part of [M28](docs/build-notes/phase-details/m28.md)'s rank semantics one milestone early; if M28's rank table lands different semantics, **M27 is reopened** rather than corrected by a successor. The ceiling holds and still governs a session-issued invitation. Its final sentence — that because a key inherits its creator's rank, `members.write` matches neither limb of D18 — is **corrected by [D43](#phase-2-decisions-taken-after-the-plan-was-finalised)**: the permission does stay delegable, but the rank ceiling is not what makes it safe ([F29](docs/build-notes/deferred-findings.md)). |
| D29 | Invite lifetime | 2026-07-31 | **`LINKCTRL_INVITE_TTL`, default 168h** — a knob, matching `SESSION_ABSOLUTE_TTL`, `SESSION_IDLE_TTL` and `REDIRECT_TTL`. A constant was refused for the reason D5 refused it for audit retention: time is the one thing an operator cannot work around without a rebuild. No-expiry was refused because single-use bounds an invite to one account but nothing bounds it in time. Mail is async via D23's outbox, so the clock starts at creation, not at send. |
| D30 | Rank management bound | 2026-07-31 | **Strictly below your own rank, with owners the single exception.** An admin manages editors and viewers and never another admin; only an owner manages admins; an owner may re-role or remove another owner, bounded by the existing last-owner refusal. The exemption is where the escalation argument stops applying — an owner already holds everything — and the uniform reading would make a departed co-owner removable only by SQL. Accepted costs: any owner can remove any other owner, and a single-owner instance whose owner is away cannot manage its admins at all. The spine of [M28](docs/build-notes/phase-details/m28.md)'s rank table. |
| D31 | Workspace-scoped membership | 2026-07-31 | **Union: it adds access, never narrows it.** Permissions are the union of every matching membership and the effective role is the lowest rank among them — which is what `GetUserPermissions` and `GetUserRoleInWorkspace` already compute, so the RBAC evaluator is not touched in the milestone that also lands members, workspaces and org creation. Cost: *org admin but viewer in one workspace* is unexpressible, so M28's control must say it adds access and never imply it restricts. |
| D32 | Workspace deletion | 2026-07-31 | **Refused while the workspace holds any link**, archived ones included; the links are deleted first. Everything under a workspace cascades on delete (`00300_links.sql`) and Phase 1 has no trash/restore, so the guard goes in front. Archiving is not an escape hatch: an archived link keeps its alias and click history. Named cost, flagged by the owner for later: with no bulk delete and no cross-workspace move, links go one at a time. |
| D33 | `orgs.create` delegability | 2026-08-01 | **Delegable**, matching neither limb of D18. It discloses no identity tied to network data, and it cannot widen a key's reach: a key's permissions are its scopes intersected with its owner's role on every request, so an organization made through a key leaves that key holding exactly what it was minted with. `NonDelegableScopes` therefore does not list it, and a test asserts both the absence and a live bearer request. |
| D34 | An organization's last workspace | 2026-08-01 | **Cannot be deleted.** Every member resolves into one of an organization's workspaces to act at all, and `ResolveWorkspaceForUser` reports finding none as a broken instance — so deleting the last one would leave every member unable to authenticate, unrecoverably without SQL. The same class of guard as the last-owner refusal, and a consequence of a tree fact rather than a preference. |
| D35 | Team surfaces take no top-level nav slot | 2026-08-01 | Members, Invitations and Workspaces all hang off the identity menu. [M26.5](docs/build-notes/phase-details/m26.5.md) cut the nav to three destinations and asked the next milestone wanting a slot to argue for one; M28's argument is that all three are visited when something *changes* rather than while work is done, and that promoting one would mean choosing between three faces of one subject. `TestTopLevelNavHoldsThreeDestinations` still asserts the count exactly. |
| D36 | A member left with no organization | 2026-08-01 | **Deletion proceeds; belonging to nothing becomes a real state.** The account survives with no membership, is prompted on next sign-in to create an organization, and can take no action until it has one. Chosen over refusing (which makes a default instance's first organization effectively undeletable) and over deleting orphaned accounts (which makes one click destroy people, with no trash and an audit trail still naming them). Requires `ResolveWorkspaceForUser` to treat *no workspace* as an empty state rather than a broken instance, and requires first-organization creation to be reachable by an account holding no permissions — [M28.5](docs/build-notes/phase-details/m28.5.md) records which mechanism it used, against D16. |
| D37 | An organization holding links | 2026-08-01 | **Refuses deletion, mirroring D32.** Archived links included. An org-level cascade through the same links would make D32 bypassable by deleting one level up. Accepted cost: with no bulk delete until 2+, emptying a large organization is a link at a time. |
| D38 | Who may change the signup mode | 2026-08-01 | **The operator, and nobody else.** `LINKCTRL_SIGNUP_MODE` is the mode — no `settings` table, no `settings.write`, no runtime toggle in UI or API. [M29](docs/build-notes/phase-details/m29.md) built the toggle first and the build is what disqualified it: `settings.write` on the `owner` role does not name a small set, because registration provisions every self-registered account an organization it owns, so under an `open` ceiling every stranger who signed up could move an instance-wide setting. Binding it to a founding organization was refused as inventing an instance-level principal inside a signup milestone. The scope row moves from *switchable at runtime by an owner* to *configured by the operator*, and the runtime toggle is parked in *Not in Phase 2*. |
| D39 | Where a curated list lives | 2026-08-01 | **A list is compiled into the binary when overruling it *should* be hard, and is runtime data otherwise.** [M30](docs/build-notes/phase-details/m30.md)'s high-confidence host list stays embedded — its entries are structural claims about cloud metadata services and control planes that stay true for years. The shortener-host list moves into `blocked_destinations` as its own source: new shorteners appear constantly, and a match only raises a low-confidence flag the owner may overrule, so compiling it imposed a release cycle on data carrying no authority. |
| D40 | Where the feed opt-in is disclosed | 2026-08-01 | **A read-only instance page, plus the docs.** [M32](docs/build-notes/phase-details/m32.md)'s bullet named a settings UI that [D38](#phase-2-decisions-taken-after-the-plan-was-finalised) had deleted. The disclosure gets a dashboard home so a signed-in user can find out what the instance does with their destinations, rather than that living only in files an operator reads once. The page **has no controls and accepts no POST**, asserted by test: D38 removed the ability to *change* instance-wide settings from the dashboard because this product has no instance-level principal, and reading is not changing. |
| D41 | The demo's data, and where its milestone sits | 2026-08-01 | **A milestone of its own at [M33.5](docs/build-notes/phase-details/m33.5.md)**, after the mid-phase review, seeding the Phase 2 features a visitor currently cannot see. Placed above M33 rather than in the 32 band because `X.9` reviews sit at the top of their band by design: inserting below M32.9 would add scope inside a review that already claims to cover that range. It ships a coverage test that fails when a listed feature has no seeded rows — which taxes every later milestone with a demo-visible feature, deliberately. It never enables a reputation feed and never changes `LINKCTRL_SIGNUP_MODE`. |
| D42 | Bounding a subscriber that stopped hearing | 2026-08-01 | **`LINKCTRL_REDIS_SUBSCRIBER_READ_TIMEOUT`, default 30s, and an expired read is a question rather than an answer.** `REDIS_READ_TIMEOUT` never reached the pub/sub receive path — go-redis reads it with a zero timeout under a deadline-less context — so a stalled Redis blocked the subscriber indefinitely ([F30](docs/build-notes/deferred-findings.md)). It cannot be reused either: on the hot path a timeout means the cache failed, here it usually means nobody edited a link, and at 50ms every replica would interrogate Redis twenty times a second. On expiry the subscriber pings and waits for the **reply**, which is the one thing a stalled connection cannot produce — `PubSub.Ping` is write-only, and go-redis's `Channel()` health check uses that same ping, so it is not the fix. Unanswered, it drops both in-process tiers *at the failure* rather than at the reconnect, which extends D20: against a Redis that never returns, flushing on reconnect is a flush that never happens. |
| D43 | What a key-issued invitation may carry | 2026-08-02 | **`editor` or `viewer`, never `owner` or `admin`** — an absolute bound, not one relative to the issuer. `members.write` **stays delegable**: a key may still invite collaborators. Corrects [D28](#phase-2-decisions-taken-after-the-plan-was-finalised)'s final sentence, which concluded no further bound was needed by reasoning on the *rank* axis while `NonDelegableScopes` governs the *credential-type* axis ([F29](docs/build-notes/deferred-findings.md)). The relative ceiling [M27](docs/build-notes/phase-details/m27.md)'s reopening proposed does not close it: `00700_seed.sql` grants admin every permission it seeded except `org.delete`, so one rank below an owner still reaches `apikeys.read`, `apikeys.write` and `audit.read` — three of the five scopes no key may hold — plus `members.write` to repeat the trick. Amends Phase 2's inherited Permissions rule to name a second, narrower mechanism — `NonDelegableScopes` governs what a key may *hold*, D43 governs what it may *produce*. |
| D44 | Whose membership a write is authorized by | 2026-08-02 | **The membership whose scope covers the object being written**, not the identity's union. [D31](#phase-2-decisions-taken-after-the-plan-was-finalised) answers what somebody may do in the workspace they are *acting in*, and every member write scoped by organization alone — so an organization-wide `viewer` who was granted `admin` in one workspace resolved there at rank 20 and re-roled their **own** organization-wide membership with it, in one dropdown pick ([F27](docs/build-notes/deferred-findings.md)). D31 is unchanged: the union still decides permissions, and a scoped role still only ever adds. What is bounded is the *target*: an organization-wide object — a membership with no `workspace_id`, an invitation, the organization itself — is reached only by an organization-wide membership, and both rank bounds (who may be acted on, what may be handed out) are evaluated against the rank of the membership that carried the permission there. This is the authorization side of what `members.sql` already states in SQL and `LockOrganizationOwners` already filters on: *a workspace-scoped owner membership grants ownership of one workspace, not of the organization*. Cost: one query per authorizing call site, and a second concept beside `Identity.Can` that a reader has to know which of to use. |
| D45 | What a teardown does with a trashed link's alias | 2026-08-02 | **Reserve it, in the transaction that deletes it** — not refuse the delete while trashed links exist. Deleting a workspace or an organization hard-deletes the links still in their trash, which both emptiness guards exclude on purpose, and until now that released a trafficked alias to the whole instance ([F28](docs/build-notes/deferred-findings.md)). The threshold is `PurgeExpiredLinks`': `click_count > 0` reserves, everything else is released. Refusing was the other acceptable answer and was rejected because there is no operator action that empties the trash — the refusal would hold for up to `TrashRetentionDays`, which is exactly the outcome `CountWorkspaceLinks` excludes trashed links to avoid, moved one level up. Applies to `DeleteWorkspace` as well as `DeleteOrganization`; the workspace half predates [M28.5](docs/build-notes/phase-details/m28.5.md). |
| D46 | What a trailing dot on a destination host means | 2026-08-02 | **Canonicalize it away, once, in `ValidateDestination`** — never refuse a dotted host. A trailing dot is a fully qualified name and an ordinary thing to type, and `https://example.com./` has been an accepted destination since [M30](docs/build-notes/phase-details/m30.md) shipped; it is now *stored* without the dot, so the value a visitor is handed is the value the tiers judged. Until this, the dot walked past four separate mechanisms at once — `netip.ParseAddr` refuses the dotted spelling, the numeric-obfuscation check read an empty last label as a name, the `localhost` test is an equality, and the embedded list is an exact-match map — so `http://169.254.169.254./` was accepted and stored ([F26](docs/build-notes/deferred-findings.md)). Folding at the entrance rather than inside each tier is the whole decision: two of the four places already normalized for themselves, which is how the other two came to differ. Reopened M30. |
| D47 | What a deep link the alias cannot forward gets | 2026-08-02 | **The ordinary miss** — the custom 404 page, charged to the 404-probe allowance — never the bare destination and never a quietly sanitized redirect. Three cases collapse into one answer: path forwarding off, a remainder holding a dot segment in any spelling the URL standard resolves (`..`, `%2e%2e`, `.%2e`), and a destination the joiner cannot rebuild. Falling back to the bare destination was the tempting alternative and is the worse one: it would make **every** link on the instance answer every URL beneath itself, which is the feature [M33](docs/build-notes/phase-details/m33.md) makes opt-in, handed to everybody by default. Sanitizing the dots was the other, and it sends a visitor somewhere they did not ask for while looking like it worked. Charging the probe allowance is part of the decision rather than a detail: without it, appending a slash would be a way round the 404 limit, and a refusal that cost nothing would tell a scanner which aliases exist — an alias with forwarding off and an alias that never existed answer identically and cost identically. The price is that a trailing-slash typo on a real link spends one token, which is the same price the limit already charges for mistyping the alias itself. |

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
- **Redis resilience beyond a bounded failure** — a circuit breaker that tracks
  health across requests, and durable invalidation via an outbox or retry queue.
  [M26.6](docs/build-notes/phase-details/m26.6.md) bounds the worst case, which
  makes both unnecessary rather than merely unaffordable; a failed invalidation
  still logs and still expires by TTL, and changing what it *means* is a
  delivery-guarantee feature rather than a timeout fix.
- **Notification severity, grouping and filtering** — [M22](docs/build-notes/phase-details/m22.md)'s
  model has no severity column and nothing ranks kinds.
  [M26.5](docs/build-notes/phase-details/m26.5.md)'s bell shows the most recent
  unread for that reason. Adding the concept is a schema change and wants its own
  milestone rather than riding on a layout one.
- **A runtime signup toggle changeable from the dashboard** — moved out of
  [M29](docs/build-notes/phase-details/m29.md) on 2026-08-01, per D38, and the
  scope row above reworded from *switchable at runtime by an owner* to
  *configured by the operator*. It was built and then removed rather than never
  attempted: the build is what showed "owner-only" does not name a small set,
  because every self-registered account owns the organization registration
  gives it, so on the one ceiling the feature existed to enable — `open` —
  every stranger who signed up could move the toggle. Restoring it needs an
  instance-level principal this product does not have, and inventing one inside
  a signup milestone is what D38 declined to do.

- **Mobile navigation rework** — the header hides the signed-in address below
  `sm` and will continue to. A responsive nav is larger than the header
  milestone that would otherwise absorb it.
- **Per-bot allowlists, and improving bot classification** —
  [M32.5](docs/build-notes/phase-details/m32.5.md) blocks all-or-nothing on one
  boolean. Letting an unfurler through while refusing a scraper needs a taxonomy
  `analytics.Classify` does not have, and editing its marker list would silently
  move every existing analytics figure at the same time. Both are separate
  changes with their own false-positive arguments.
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
| Invalidation needs Redis to cross replicas | Invalidations are broadcast on Redis pub/sub. With Redis down each replica falls back to `REDIRECT_TTL` staleness, which is correct but slower to converge; a reconnecting subscriber flushes its in-process tiers because pub/sub cannot replay what it missed. A Redis that accepts and then stalls is bounded rather than waited on: an edit spends at most `REDIS_INVALIDATE_BUDGET` (250ms) on the cache before committing anyway and logging the staleness (D26, [M26.6](docs/build-notes/phase-details/m26.6.md)). |
| Rate limits are shared only while Redis is reachable | The credential and API limits are enforced in Redis, so they hold across replicas. **On any Redis error each replica falls back to its own in-memory bucket, and the configured limit then applies per replica — N replicas allow N times it** until Redis returns. A restart also resets the local buckets. The 404-probe limiter is deliberately never shared: a Redis round trip on the redirect path would put an optional dependency on the 20ms budget. |
| Rate limits fail open | A full key table allows requests rather than refusing them, counted by `linkctrl_rate_limit_overflow_total`. A limiter is abuse mitigation, not an authorization boundary. |
| Behind a proxy, limits need `TRUSTED_PROXIES` | Otherwise every request carries the proxy's address and all traffic shares one bucket. This is a correctness requirement once a limit is on, not only an analytics one. |
| `links.click_count` is approximate | Written with the click rows, but an unclean shutdown loses at most one batch of both. |
| `api_keys.last_used_at` is approximate | Buffered and flushed on a 30s cadence, so an unclean shutdown loses the most recent timestamps. Authentication must not cost a write. |
| A member cannot manage their own rank, or a peer's | [M28](docs/build-notes/phase-details/m28.md) bounds management to ranks strictly below your own (D30), so an admin cannot demote themselves and cannot touch another admin — both need an owner. On a single-owner instance whose owner is unavailable, the admins cannot be changed at all, and there is no self-service way to leave an organization. |
| A workspace-scoped role cannot narrow anybody | Permissions are the union of every matching membership (D31), so *org admin, viewer in one workspace* is unexpressible. Granting a role in a workspace only ever adds to what somebody already holds. |
| A workspace-scoped role reaches no further than its workspace | D44 authorizes each write against the membership whose scope covers its target, so a workspace-scoped admin manages that workspace's memberships and cannot re-role an organization-wide one, grant themselves a second workspace, invite anybody, or delete the organization — nor can a workspace-scoped owner, who owns one workspace and not the organization. The cost is that widening somebody's reach still needs an organization-wide member: there is no way to delegate *organization* administration without an organization-wide membership. |
| Emptying a workspace is one link at a time | D32 refuses to delete a workspace holding any link, archived ones included, and Phase 2 has neither bulk delete nor a cross-workspace move. Flagged by the owner as worth revisiting; bulk delete, a link move and archive-then-cascade are three separate features with three separate arguments. |
| Emptying an organization is one link at a time too | D37 refuses to delete an organization holding any link in any of its workspaces, archived ones included, for the reason D32 refuses it one level up — an org-level cascade would make the workspace rule bypassable. With no bulk delete, a large organization has no practical path that does not involve SQL. Replaces *nothing deletes an organization*, which [M28.5](docs/build-notes/phase-details/m28.5.md) ended. |
| An organization's audit trail is readable only from inside it | The records survive the organization they describe — `audit_logs.organization_id` carries no foreign key — but `GET /api/v1/audit` is scoped to the caller's own organization and nobody can be in a deleted one. So a deleted organization's trail is intact in the table and reachable only with database access. |
| An account belonging to no organization can do exactly one thing | D36 lets deletion orphan people rather than refuse on their behalf. The account survives and signs in, but every dashboard route redirects to the page offering a new organization until it has one — including *Account*, so a password cannot be changed from that state. |
| A dispute queue crosses organizations, and every owner holds the key to it | The low-confidence blocklist is instance-wide (M30), so M31's queue and its decisions are too: `destinations.review` is granted to the **owner** role, which means the owner of *any* organization on the instance sees every dispute filed on it and can lift an entry for everyone. On an instance with `LINKCTRL_SIGNUP_MODE=open` that set includes anybody who signed up, because registration provisions an organization and makes the registrant its owner. It is the shape `domains.write` already has and one degree wider; the underlying cause is that this product has no instance-level principal (D38). Recorded as finding F15. |
| Allowing a destination cannot lift a refusal no row produced | An `allow` deletes one row from `blocked_destinations` and there is no row anybody may add that permits a destination — 01500 has no allow column on purpose. So a `low_confidence.punycode_homograph` or `low_confidence.url_credentials` refusal can be filed, read and upheld, but not allowed: the queue says so and offers only *Uphold*. Overruling one of those is a change to the rule, which is a code change. |
| Changing the signup mode needs the operator | `LINKCTRL_SIGNUP_MODE` is the mode and nothing in the running instance moves it (D38), so letting somebody in means an `.env` edit and a restart — or an invitation, which needs neither. The toggle that would have avoided that was built and removed: this product has no instance-level principal, so *owner-only* would have meant every account that signed up on an open instance. [M29](docs/build-notes/phase-details/m29.md); recorded in [docs/SECURITY.md](docs/SECURITY.md). |
| A verification link cannot be re-sent, only re-issued | Only the token's hash is stored, so the emailed link exists once. Registering the same address again supersedes the outstanding one and mails a new link, which is the recovery path; there is no "resend" that reuses the old token. |
| An invitation cannot be re-sent, only re-issued | Only the token's hash is stored, so the link exists once, in the response that created it. Losing it means revoking the invitation and issuing another — the same trade an API key makes, for the same reason. |
| API keys cannot manage API keys | `apikeys.*` is not delegable, so minting and revoking need a session. Rotation is scheduled as self-rotation-only: [M44](docs/build-notes/phase-details/m44.md). |
| A human blocked as a bot has no recourse | [M32.5](docs/build-notes/phase-details/m32.5.md) decides with `analytics.Classify`, which treats an absent user agent as automated and matches substrings including `preview`, `monitor` and `checker`. Its false-positive rate has never been measured, because until bot blocking existed nothing depended on it. A person it misjudges gets a 403 with no challenge and no appeal, and the link's owner is not told. The mitigation available without the bypass is the default: `inherit`, resolving to off, so the cost sits with whoever switches it on. The bypass is Phase 3, per *Not in Phase 2*. |
| Blocking is per link and per domain, and nothing between | The domain's setting is instance-wide, like the root redirect and the low-confidence blocklist, so an owner who enforces it decides for every workspace on the box — the shape `domains.write` already has (F15). There is no per-workspace level, and no way to see how many refusals a setting is producing beyond `linkctrl_redirects_total{outcome="blocked_bot"}` and the bot column of a link's own statistics. |
| A domain-level blocking change sweeps the Redis keyspace | The redirect path reads the domain's policy from each link's cached snapshot, which is what keeps the decision free of I/O — so changing it invalidates every one of those snapshots. Each replica clears its own memory tier by prefix, and whoever made the change walks Redis with `SCAN`/`UNLINK` inside a five-second budget. On an instance with a very large keyspace the walk can exhaust that budget, which is logged: the affected links then keep applying the previous policy until `REDIRECT_TTL` expires them. It runs only when somebody submits the form, and never on the redirect path. |
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
