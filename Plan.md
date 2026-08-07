# LinkCtrl — Project Plan

Scope contract and specification. States **what** is true, not why.

| | |
| --- | --- |
| Rationale for every decision | `docs/build-notes/decisions.md` |
| Investigations | `docs/adr/` |
| Dev environment | `docs/build-notes/development.md` |
| How the work is done | `docs/build-notes/workflow.md` |
| Security model and reporting | `docs/SECURITY.md` |
| Definitions of done, per milestone | `docs/build-notes/phase-details/` — one file per milestone, live phase and finished ones |
| Out-of-spec findings | `docs/build-notes/deferred-findings.md` |
| Current progress | [Build Status](#build-status) |
| Last updated | 2026-08-07 |

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
| Moving links between workspaces | 3+ — **not scheduled in Phase 3**; see [Not in Phase 3](#not-in-phase-3) |
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
| Dimension visualizations: choropleth map, richer charts, click through to the ranked list | 2 — delivered by [M37](docs/build-notes/phase-details/m37.md) |
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
| `domains.write.instance`, so the instance default domain is the principal's rather than every organization owner's | 2 |
| Per-domain ownership, so a workspace administers its own hostname | 2 |
| Audit log — table only | 1 |
| Audit log — behavior | 2 |
| In-app notifications | 2 |
| Password links, one-time links, max-click links, signed URLs | 2 |
| Malicious destination blocking: tiers, logging, notification, disputes | 2 |
| Third-party reputation and malware feeds — opt-in, off by default | 2 |
| MFA, OAuth, OIDC, SSO, SCIM | 3 for **MFA only** ([M53](docs/build-notes/phase-details/m53.md)); OAuth, OIDC, SSO and SCIM stay 3+ and unscheduled (D109) |

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

  [M32](docs/build-notes/phase-details/m32.md) shipped that exception and
  [M42](docs/build-notes/phase-details/m42.md) added a second one, so the promise
  is stated as **two named channels** rather than as one. No other part of the
  product sends a destination anywhere; the nearest thing to a third is the
  dispute-outcome email, which needs a mailer and carries the disputed *host*,
  defanged, to the address that filed the dispute. Both channels are silent on a
  fresh instance, and only one of them is the operator's to keep that way.

  **The reputation feed is the operator's.** No destination reaches it unless the
  operator sets `LINKCTRL_FEED_URL`, and the shipped default is unset. Once they
  do: the destination — and nothing else, no account, no workspace, no instance
  name — is sent to the named feed on every link create, link update,
  root-redirect change and dispute filing. Never when a visitor follows a link,
  and existing links are not re-checked in the background.

  **Outbound webhooks are a workspace administrator's**, and no operator setting
  reaches them: anybody holding `webhooks.write` — the owner and admin roles,
  never an API key — registers a URL of their own choosing and this instance POSTs
  that workspace's events to it. The five link-lifecycle events carry the link's
  destination **as typed** in `data.url`, and `destination.blocked` carries the
  attempted destination **defanged**. It reaches one workspace's own links and no
  further, and the only lever over it is who holds `webhooks.write`.

  Four bounds hold the feed whatever the operator configures. It is asked
  **last**, only about destinations every built-in tier already accepted, so no
  built-in refusal ever reaches the feed and none of them changes answer with a
  feed on, off or erroring — a bound on the feed and not on the instance, because
  that refusal is itself a `destination.blocked` event a subscribed webhook
  receives. A feed that does not answer **fails open** to those tiers and
  increments `linkctrl_destination_feed_checks_total{result="error"}`. Its
  verdicts are low-confidence: disputable, and the instance owner can overrule one
  from the review queue, which also stops that host being sent again. And the
  instance discloses the whole of the above at **`/feeds`**, to every signed-in
  user, with a feed or without one — a read-only page with no controls, because
  only the operator can change any of it (D40). **That page answers for both
  channels**, and it did not always: it read the feed setting alone, so on an
  instance with no feed it told everybody nothing left, which a webhook in their
  own workspace made false. It now reads the workspace's registrations too, and
  keeps the two claims apart — the feed's answer is instance-wide, the webhook's
  is about the workspace being viewed.

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
move: it is the operator's, in the environment, because at the time this product
had no instance-level principal for it to belong to. [M45](docs/build-notes/phase-details/m45.md)
introduced one (D98), with scopes enumerated rather than implied; whether the
signup mode should become one of them is a decision nobody has taken, and until
somebody does it stays in the environment.

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
| ↳ of that row, Phase 3 takes **high availability** ([M56](docs/build-notes/phase-details/m56.md), [M57](docs/build-notes/phase-details/m57.md)) and the erasure limb of compliance ([M52](docs/build-notes/phase-details/m52.md)). Advanced analytics and the rest of compliance stay candidates — see [phase-3-candidates.md](docs/build-notes/phase-3-candidates.md) | 3+ |
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

ERD and per-entity implementation status: [docs/data-model.md](docs/data-model.md),
written at [M45](docs/build-notes/phase-details/m45.md) after being referenced
from here and from `00600_phase2_dormant.sql` since Phase 1.

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
| Analytics queries | <2s | **met for what a reader waits on** — every dashboard query reads a rollup. The dimension *rollup itself* is 4.8-6.3s per run at 5.7M events and is a background job on a 15-minute clock since [M37](docs/build-notes/phase-details/m37.md), not a query anyone waits for; the 60-second totals pass is 1.5-1.6s |

The redirect target is defined as: **server-side p99, cache hits only, measured
from a load generator on the same Docker network, excluding client RTT and TLS,
at 2,000 rps sustained for 2 minutes, with 100k links and 5M click events
seeded.** Both the generator's number and the server histogram are reported.
The measurement, how to reproduce it and what it found: [docs/slo.md](docs/slo.md).

Measured on one developer machine, so the shape transfers and the absolute values
do not. Notably, the cached result held while the analytics rollup was recomputing
whole days from 5.7M events every 60 seconds — which is the split pool and the
two-tier cache doing what they exist for. Since [M37](docs/build-notes/phase-details/m37.md)
the expensive half of that recompute runs every 15 minutes instead, so the
measurements above were taken under a *heavier* background load than an instance
now carries.

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

**Phase 1: 21 of 21 milestones, all of them in 0.1.0, tagged `v0.1.0` on `main`
and released on 2026-07-31.**

**Phase 2: 33 milestones — 25 integers M21–M45 and 8 fractional insertions — all
of them in 0.2.0, tagged `v0.2.0` on `main` and released on 2026-08-06.** Status
per milestone lives in
[phase-details/README.md](docs/build-notes/phase-details/README.md) and nowhere
else; the plan below is the scope contract rather than a progress report.

**Phase 3: planned on 2026-08-06, unstarted.** Seventeen milestones, M46–M58,
across four work areas; the plan is [below](#phase-3-build-plan). It was planned
in full before its first milestone was built, on the owner's direction, so that
the fifteen-milestone target is enforced by arithmetic rather than discovered at
milestone twenty.

*(This paragraph read "Phase 2 is planned and unstarted, as of 2026-07-31" for
the whole of Phase 2 — a dated snapshot that nine milestones overtook on the day
it was stamped, which is why the milestone counts moved out of it and into the
one file that owns them. F37.)*

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
row. **That file is the authority on how many there are and what state each is
in**, and this sentence deliberately no longer repeats a count: it said "one
open finding, cosmetic, unreviewed" against a queue that had grown past sixty
and been triaged three times (F37).

#### Previously unassigned, now scheduled

All three items Phase 1 accepted without an owner are scheduled in Phase 2:

| Capability | Now |
| --- | --- |
| Dimension rollup cost | **Discharged by [M37](docs/build-notes/phase-details/m37.md)**: split cadence (60s totals, 15m breakdowns, a watermark each), `linkctrl_rollup_staleness_seconds` with an alert recipe, and a re-measurement at the 5.7M-event seed taken before the choropleth was allowed to read it. What remains is a lag rather than a cost, and it is in *Known limitations*. |
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
plan was finalised. **Six were inserted**: dark mode at M24.5, the dashboard
header at M26.5, the Redis stall bound at M26.6 and bot blocking at M32.5, all
2026-07-31; then organization deletion at M28.5 and the demo's own data at M33.5,
both 2026-08-01. (This said *five* and listed six from the moment the sixth was
added — F37.) The numbering rules are in
[planning.md](docs/build-notes/planning.md). One milestone per commit.

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
| D18 | Permission delegability, as a rule | 2026-07-31 | A permission is **non-delegable** to an API key when it is **escalating, irreversible, or disclosing** — when holding it lets a key widen its own reach, when exercising it cannot be undone, or when reading it exposes an actor's identity tied to network data. Everything else is delegable. Each milestone that adds a permission records which limb it matched, or that it matched neither. *(**Corrected 2026-08-05**, F12. This row named only the escalating and disclosing limbs and said "everything else is delegable", which read literally makes `org.delete` delegable — it discloses nothing and grants nothing, and has been non-delegable since Phase 1 on irreversibility. The map's own comment at `internal/auth/apikey.go` has stated the three-limb rule all along, so the code was right before this text was written. Nothing about the map changes.)* `NonDelegableScopes` is the only mechanism for whether a key may **hold** a permission; [D43](#phase-2-decisions-taken-after-the-plan-was-finalised) adds a second and narrower one for what a key may **produce** with one it holds, and the mechanisms that may branch on credential type are enumerated in [phase-details/README.md](docs/build-notes/phase-details/README.md)'s Permissions row — four since 2026-08-05, amended there rather than restated here. **Adding a scope to `NonDelegableScopes` binds keys minted and rotated afterwards; it does not disarm a key already issued.** `resolveScopes` refuses it at mint and `narrowScopes` refuses to carry it into a successor, while `Authenticate` restricts an identity to the **row's** stored scopes and never consults the map. *(**Corrected 2026-08-05**, F12: this row said flipping a permission in either direction stays a one-line map edit, which promised a disarming the code does not perform. Never exploitable — `git log -S` puts every entry in the same commit as the migration creating its permission, so no key has ever held a scope that later became non-delegable. Closing it would mean an `isNonDelegable` filter in `restrictTo`, a check on every authenticated request for a state no key has been in; left open deliberately.)* Generalises the `audit.read` call and covers M27, M28, M31, M38, M39, M44. |
| D19 | Audit-growth notification default | 2026-07-31 | On by default. A configurable size threshold over the audit partitions, defaulting to 5 GB, raises the [M22](docs/build-notes/phase-details/m22.md) owner notification. Extends D5: keep-forever is only safe if an untouched instance is warned, so the alert cannot itself require configuration. |
| D20 | Pub/sub subscriber reconnect | 2026-07-31 | A reconnecting subscriber **flushes both in-process tiers** — the alias memCache and the root-redirect cache. Invalidations published during the gap are unrecoverable, so the missed-invalidation window closes at reconnect rather than at TTL expiry. The cost is a cold cache after a Redis blip: a latency effect on an optional dependency, never a correctness one. |
| D21 | Light theme under M24.5's tokens | 2026-07-31 | One token set, correct in both themes. Where a pair cannot meet AA at today's light values, **the light value moves**, and each such change is recorded beside the token definition. M24.5 lands genuinely AA-clean rather than deferring its own contrast failures. |
| D22 | Default workspace resolution | 2026-07-31 | Last-used, remembered: the switcher persists the last selection and that becomes the default. A user setting can pin an explicit workspace instead, and its control defaults to *Last-Used*. Owner-added scope on [M25](docs/build-notes/phase-details/m25.md), 2026-07-31 — the milestone's no-op claim for today's single-membership users is unchanged. |
| D23 | Mailer delivery mechanism | 2026-07-31 | An outbox table plus a scheduler job, closing the mechanism [M26](docs/build-notes/phase-details/m26.md) deliberately left open. Queued mail survives a restart; invitations and address verification are the consumers, and losing one silently on a deploy is the failure worth an additive migration to avoid. |
| D24 | Header menu mechanism | 2026-07-31 | **The Popover API**, not `<details>`/`<summary>`. A disclosure cannot close on Escape in any browser, and [M26.5](docs/build-notes/phase-details/m26.5.md) asked for exactly that; a popover is equally declarative, needs no script and no CSP waiver, and adds outside-click dismissal. The cost is explicit: a top-layer element ignores its ancestor's containing block, so positioning is verified in a browser rather than asserted from markup, and the supported floor rises to Chrome 114 / Safari 17 / Firefox 125. Chosen over the cheaper amend-the-bullet option because this header is the idiom M28, M31, M38 and M41 will each copy. |
| D25 | Verification tooling vs the stdlib-only rule | 2026-07-31 | **Shipped code stays stdlib-only; tooling that only verifies it may use Node**, as long as Node stays out of everything except required test code. Settled when [M26.5](docs/build-notes/phase-details/m26.5.md) needed a WebKit engine to check popover positioning and the machine had no Node, no Playwright and no WebKit. The inherited *`ui` stays stdlib-only* rule governs what ships, not what measures it. |
| D26 | Bounding a stalled Redis | 2026-07-31 | **One total budget for an invalidation — `REDIS_INVALIDATE_BUDGET`, 250ms — enforced by the caller rather than per attempt.** Three retries each entitled to `REDIS_READ_TIMEOUT` multiplied a documented knob by three; the budget covers the whole loop instead. 250ms fits every retry that works today (210ms) and is 1.7% of `HTTP_REQUEST_TIMEOUT`. `MaxRetries` and `REDIS_DIAL_TIMEOUT` are left as they were: measured for [M26.6](docs/build-notes/phase-details/m26.6.md) and neither contributes to this failure, so the redirect path is untouched. The decision's original reason — *a context go-redis will not honour* — was true of the client as configured then and is not any more: M45 set `ContextTimeoutEnabled` ([F138](docs/build-notes/deferred-findings.md)), so a deadline now bounds one command. The budget still bounds the loop, which is what it was for. |
| D27 | Invite binding | 2026-07-31 | **Bound to the invited address**, not a bearer link. Redemption checks the redeeming account's address against the address invited, so a forwarded or leaked link cannot add a stranger. Accepted cost: an invite pasted into a team channel for "whoever needs this" is refused, and joining under a different address needs a re-invite. The address comparison must fail identically whether the address is unknown, already a member, or not the invited one — M27's no-enumeration bullet governs it. |
| D28 | Invite role ceiling | 2026-07-31 | **Any role at or below the inviter's own rank** (`owner` 10, `admin` 20, `editor` 30, `viewer` 40). An admin may invite an admin, editor or viewer, never an owner. Settles part of [M28](docs/build-notes/phase-details/m28.md)'s rank semantics one milestone early; if M28's rank table lands different semantics, **M27 is reopened** rather than corrected by a successor. The ceiling holds and still governs a session-issued invitation. Its final sentence — that because a key inherits its creator's rank, `members.write` matches neither limb of D18 — is **corrected by [D43](#phase-2-decisions-taken-after-the-plan-was-finalised)**: the permission does stay delegable, but the rank ceiling is not what makes it safe ([F29](docs/build-notes/deferred-findings.md)). |
| D29 | Invite lifetime | 2026-07-31 | **`LINKCTRL_INVITE_TTL`, default 168h** — a knob, matching `SESSION_ABSOLUTE_TTL`, `SESSION_IDLE_TTL` and `REDIRECT_TTL`. A constant was refused for the reason D5 refused it for audit retention: time is the one thing an operator cannot work around without a rebuild. No-expiry was refused because single-use bounds an invite to one account but nothing bounds it in time. Mail is async via D23's outbox, so the clock starts at creation, not at send. |
| D30 | Rank management bound | 2026-07-31 | **Strictly below your own rank, with owners the single exception.** An admin manages editors and viewers and never another admin; only an owner manages admins; an owner may re-role or remove another owner, bounded by the existing last-owner refusal. The exemption is where the escalation argument stops applying — an owner already holds everything — and the uniform reading would make a departed co-owner removable only by SQL. Accepted costs: any owner can remove any other owner, and a single-owner instance whose owner is away cannot manage its admins at all. The spine of [M28](docs/build-notes/phase-details/m28.md)'s rank table. |
| D31 | Workspace-scoped membership | 2026-07-31 | **Union: it adds access, never narrows it.** Permissions are the union of every matching membership and the effective role is the lowest rank among them — which is what `GetUserPermissions` and `GetUserRoleInWorkspace` already compute, so the RBAC evaluator is not touched in the milestone that also lands members, workspaces and org creation. Cost: *org admin but viewer in one workspace* is unexpressible, so M28's control must say it adds access and never imply it restricts. |
| D32 | Workspace deletion | 2026-07-31 | **Refused while the workspace holds any link**, archived ones included; the links are deleted first. Everything under a workspace cascades on delete (`00300_links.sql`) and Phase 1 has no trash/restore, so the guard goes in front. Archiving is not an escape hatch: an archived link keeps its alias and click history. Named cost, flagged by the owner for later: with no bulk delete and no cross-workspace move, links go one at a time. |
| D33 | `orgs.create` delegability | 2026-08-01 | **Delegable**, matching neither limb of D18. It discloses no identity tied to network data, and it cannot widen a key's reach: a key's permissions are its scopes intersected with its owner's role on every request, so an organization made through a key leaves that key holding exactly what it was minted with. `NonDelegableScopes` therefore does not list it, and a test asserts both the absence and a live bearer request. |
| D34 | An organization's last workspace | 2026-08-01 | **Cannot be deleted.** Every member resolves into one of an organization's workspaces to act at all, and `ResolveWorkspaceForUser` reports finding none as a broken instance — so deleting the last one would leave every member unable to authenticate, unrecoverably without SQL. The same class of guard as the last-owner refusal, and a consequence of a tree fact rather than a preference. |
| D35 | Team surfaces take no top-level nav slot | 2026-08-01 | Members, Invitations and Workspaces all hang off the identity menu. [M26.5](docs/build-notes/phase-details/m26.5.md) cut the nav to three destinations and asked the next milestone wanting a slot to argue for one; M28's argument is that all three are visited when something *changes* rather than while work is done, and that promoting one would mean choosing between three faces of one subject. `TestTopLevelNavHoldsTwoDestinations` still asserts the count exactly. *(Two, not three, since [M46](docs/build-notes/phase-details/m46.md) applied this same argument to API keys and moved it into the menu; the test was renamed with the count rather than deleted.)* |
| D36 | A member left with no organization | 2026-08-01 | **Deletion proceeds; belonging to nothing becomes a real state.** The account survives with no membership, is prompted on next sign-in to create an organization, and can take no action until it has one. Chosen over refusing (which makes a default instance's first organization effectively undeletable) and over deleting orphaned accounts (which makes one click destroy people, with no trash and an audit trail still naming them). Requires `ResolveWorkspaceForUser` to treat *no workspace* as an empty state rather than a broken instance, and requires first-organization creation to be reachable by an account holding no permissions — [M28.5](docs/build-notes/phase-details/m28.5.md) records which mechanism it used, against D16. |
| D37 | An organization holding links | 2026-08-01 | **Refuses deletion, mirroring D32.** Archived links included. An org-level cascade through the same links would make D32 bypassable by deleting one level up. Accepted cost: with no bulk delete until 2+, emptying a large organization is a link at a time. |
| D38 | Who may change the signup mode | 2026-08-01 | **The operator, and nobody else.** `LINKCTRL_SIGNUP_MODE` is the mode — no `settings` table, no `settings.write`, no runtime toggle in UI or API. [M29](docs/build-notes/phase-details/m29.md) built the toggle first and the build is what disqualified it: `settings.write` on the `owner` role does not name a small set, because registration provisions every self-registered account an organization it owns, so under an `open` ceiling every stranger who signed up could move an instance-wide setting. Binding it to a founding organization was refused as inventing an instance-level principal inside a signup milestone. The scope row moves from *switchable at runtime by an owner* to *configured by the operator*, and the runtime toggle is parked in *Not in Phase 2*. |
| D39 | Where a curated list lives | 2026-08-01 | **A list is compiled into the binary when overruling it *should* be hard, and is runtime data otherwise.** [M30](docs/build-notes/phase-details/m30.md)'s high-confidence host list stays embedded — its entries are structural claims about cloud metadata services and control planes that stay true for years. The shortener-host list moves into `blocked_destinations` as its own source: new shorteners appear constantly, and a match only raises a low-confidence flag the owner may overrule, so compiling it imposed a release cycle on data carrying no authority. |
| D40 | Where the feed opt-in is disclosed | 2026-08-01 | **A read-only instance page, plus the docs.** [M32](docs/build-notes/phase-details/m32.md)'s bullet named a settings UI that [D38](#phase-2-decisions-taken-after-the-plan-was-finalised) had deleted. The disclosure gets a dashboard home so a signed-in user can find out what the instance does with their destinations, rather than that living only in files an operator reads once. The page **has no controls and accepts no POST**, asserted by test: D38 removed the ability to *change* instance-wide settings from the dashboard because this product had no instance-level principal, and reading is not changing. (D98 later introduced one with enumerated scopes; the feed configuration is not among them, so the page is unchanged.) |
| D41 | The demo's data, and where its milestone sits | 2026-08-01 | **A milestone of its own at [M33.5](docs/build-notes/phase-details/m33.5.md)**, after the mid-phase review, seeding the Phase 2 features a visitor currently cannot see. Placed above M33 rather than in the 32 band because `X.9` reviews sit at the top of their band by design: inserting below M32.9 would add scope inside a review that already claims to cover that range. It ships a coverage test that fails when a listed feature has no seeded rows — which taxes every later milestone with a demo-visible feature, deliberately. It never enables a reputation feed and never changes `LINKCTRL_SIGNUP_MODE`. |
| D42 | Bounding a subscriber that stopped hearing | 2026-08-01 | **`LINKCTRL_REDIS_SUBSCRIBER_READ_TIMEOUT`, default 30s, and an expired read is a question rather than an answer.** `REDIS_READ_TIMEOUT` never reached the pub/sub receive path — go-redis reads it with a zero timeout under a deadline-less context — so a stalled Redis blocked the subscriber indefinitely ([F30](docs/build-notes/deferred-findings.md)). It cannot be reused either: on the hot path a timeout means the cache failed, here it usually means nobody edited a link, and at 50ms every replica would interrogate Redis twenty times a second. On expiry the subscriber pings and waits for the **reply**, which is the one thing a stalled connection cannot produce — `PubSub.Ping` is write-only, and go-redis's `Channel()` health check uses that same ping, so it is not the fix. Unanswered, it drops both in-process tiers *at the failure* rather than at the reconnect, which extends D20: against a Redis that never returns, flushing on reconnect is a flush that never happens. |
| D43 | What a key-issued invitation may carry | 2026-08-02 | **`editor` or `viewer`, never `owner` or `admin`** — an absolute bound, not one relative to the issuer. `members.write` **stays delegable**: a key may still invite collaborators. Corrects [D28](#phase-2-decisions-taken-after-the-plan-was-finalised)'s final sentence, which concluded no further bound was needed by reasoning on the *rank* axis while `NonDelegableScopes` governs the *credential-type* axis ([F29](docs/build-notes/deferred-findings.md)). The relative ceiling [M27](docs/build-notes/phase-details/m27.md)'s reopening proposed does not close it: `00700_seed.sql` grants admin every permission it seeded except `org.delete`, so one rank below an owner still reaches `apikeys.read`, `apikeys.write` and `audit.read` — three of the five scopes no key may hold — plus `members.write` to repeat the trick. Amends Phase 2's inherited Permissions rule to name a second, narrower mechanism — `NonDelegableScopes` governs what a key may *hold*, D43 governs what it may *produce*. |
| D44 | Whose membership a write is authorized by | 2026-08-02 | **The membership whose scope covers the object being written**, not the identity's union. [D31](#phase-2-decisions-taken-after-the-plan-was-finalised) answers what somebody may do in the workspace they are *acting in*, and every member write scoped by organization alone — so an organization-wide `viewer` who was granted `admin` in one workspace resolved there at rank 20 and re-roled their **own** organization-wide membership with it, in one dropdown pick ([F27](docs/build-notes/deferred-findings.md)). D31 is unchanged: the union still decides permissions, and a scoped role still only ever adds. What is bounded is the *target*: an organization-wide object — a membership with no `workspace_id`, an invitation, the organization itself — is reached only by an organization-wide membership, and both rank bounds (who may be acted on, what may be handed out) are evaluated against the rank of the membership that carried the permission there. This is the authorization side of what `members.sql` already states in SQL and `LockOrganizationOwners` already filters on: *a workspace-scoped owner membership grants ownership of one workspace, not of the organization*. Cost: one query per authorizing call site, and a second concept beside `Identity.Can` that a reader has to know which of to use. |
| D45 | What a teardown does with a trashed link's alias | 2026-08-02 | **Reserve it, in the transaction that deletes it** — not refuse the delete while trashed links exist. Deleting a workspace or an organization hard-deletes the links still in their trash, which both emptiness guards exclude on purpose, and until now that released a trafficked alias to the whole instance ([F28](docs/build-notes/deferred-findings.md)). The threshold is `PurgeExpiredLinks`': `click_count > 0` reserves, everything else is released. Refusing was the other acceptable answer and was rejected because there is no operator action that empties the trash — the refusal would hold for up to `TrashRetentionDays`, which is exactly the outcome `CountWorkspaceLinks` excludes trashed links to avoid, moved one level up. Applies to `DeleteWorkspace` as well as `DeleteOrganization`; the workspace half predates [M28.5](docs/build-notes/phase-details/m28.5.md). |
| D46 | What a trailing dot on a destination host means | 2026-08-02 | **Canonicalize it away, once, in `ValidateDestination`** — never refuse a dotted host. A trailing dot is a fully qualified name and an ordinary thing to type, and `https://example.com./` has been an accepted destination since [M30](docs/build-notes/phase-details/m30.md) shipped; it is now *stored* without the dot, so the value a visitor is handed is the value the tiers judged. Until this, the dot walked past four separate mechanisms at once — `netip.ParseAddr` refuses the dotted spelling, the numeric-obfuscation check read an empty last label as a name, the `localhost` test is an equality, and the embedded list is an exact-match map — so `http://169.254.169.254./` was accepted and stored ([F26](docs/build-notes/deferred-findings.md)). Folding at the entrance rather than inside each tier is the whole decision: two of the four places already normalized for themselves, which is how the other two came to differ. Reopened M30. |
| D47 | What a deep link the alias cannot forward gets | 2026-08-02 | **The ordinary miss** — the custom 404 page, charged to the 404-probe allowance — never the bare destination and never a quietly sanitized redirect. Three cases collapse into one answer: path forwarding off, a remainder holding a dot segment in any spelling the URL standard resolves (`..`, `%2e%2e`, `.%2e`), and a destination the joiner cannot rebuild. Falling back to the bare destination was the tempting alternative and is the worse one: it would make **every** link on the instance answer every URL beneath itself, which is the feature [M33](docs/build-notes/phase-details/m33.md) makes opt-in, handed to everybody by default. Sanitizing the dots was the other, and it sends a visitor somewhere they did not ask for while looking like it worked. Charging the probe allowance is part of the decision rather than a detail: without it, appending a slash would be a way round the 404 limit, and a refusal that cost nothing would tell a scanner which aliases exist — an alias with forwarding off and an alias that never existed answer identically and cost identically. The price is that a trailing-slash typo on a real link spends one token, which is the same price the limit already charges for mistyping the alias itself. |
| D48 | What the city lookup cost is measured against | 2026-08-02 | **A synthetic City database, built large enough to exercise the tree, and named as such wherever the number appears.** [M34](docs/build-notes/phase-details/m34.md) requires city-level rule conditions and requires their mmap lookup cost to be *measured, not assumed*, inside the 20ms budget — and no GeoLite2-City database exists on this project's machines, nor can one be committed: it is ~60MB and MaxMind's to license, which is exactly why the milestone calls it *the operator's* City database. `internal/geoip/testdata/gen_mmdb.go` already builds the country fixture with `mmdbwriter` over documentation and reserved ranges, deliberately so the fixture is not a claim about anywhere real; the city fixture extends that generator and keeps the property. **The cost, stated rather than buried:** a synthetic tree's node layout is not MaxMind's, so the figure is a representative floor and not an authoritative reading of GeoLite2-City. Every place it is written — [docs/slo.md](docs/slo.md) above all — says which database produced it and how many networks it held, so nobody can mistake it for a measurement against the real file. Owner-answered at M34's validation, chosen over supplying a real database (which stops the loop on a licensed download that no reader could reproduce), over deferring city conditions to a later milestone (a real reduction of the Rules scope row at line 92), and over shipping the measurement unverified (a `done` row asserting something untrue). |
| D49 | Routing rules mint no permission | 2026-08-02 | **Reuse the link's own permissions.** `links.read` lists rules, `links.update` writes them, and no new slug is seeded. Neither [D18](#phase-2-decisions-taken-after-the-plan-was-finalised) limb applies, because there is no permission to classify — which is the answer worth recording rather than the absence of a row. The same call [M36](docs/build-notes/phase-details/m36.md) made for split arms and [M38](docs/build-notes/phase-details/m38.md) made for folders (D67), and the one [D75](#phase-2-decisions-taken-after-the-plan-was-finalised) cites for QR codes and campaigns. |
| D50 | Who maintains the returning-visitor set | 2026-08-02 | **The redirect handler flags the click; the analytics pipeline writes the set.** The handler already holds the snapshot, so it knows whether this link carries a returning-visitor rule and carries that decision into the pipeline as `ClickEvent.TrackReturning`; the pipeline never asks. [D2](#phase-2-decisions-taken-after-the-plan-was-finalised) had fixed the semantics — seen earlier today, cookie-free, from the daily-salted visitor hash — and left open who maintains it, which is the half that decides whether the feature is affordable. The hot path may not create a salt, and the set member is eight bytes. |
| D51 | How a rule reaches its destination in the cached snapshot | 2026-08-02 | **A destination list, with rules indexing into it, and the slice order is the priority.** Two rules pointing at one destination is the ordinary case — *everyone outside the EU goes here*, written as several country rules — and a URL per rule would put that string in the payload once per rule, on a value serialized at every cache write and parsed at every miss. **No priority column is stored**: the order rules are returned in is the order they are evaluated in. |
| D52 | The cache-key bump M34 was always going to need | 2026-08-02 | **`CacheKeyVersion` moves to `v2`.** Every earlier Phase 2 snapshot field argued its way out of a bump on one ground — the stale reading is the behaviour the link already had, bot blocking off, path forwarding off, correct yesterday. A routing rule is the first field whose *absence* means something a visitor can observe that its zero value does not: an entry written by the previous build carries no rules, so a link whose owner has since routed traffic elsewhere keeps ignoring that for up to `REDIRECT_TTL`. See also [D46](#phase-2-decisions-taken-after-the-plan-was-finalised)'s neighbour rule recorded at M45: a new `omitempty` field skips the bump only when its zero value means the same thing to a visitor as the true value would. |
| D53 | A POST on the redirect tree, and the CSRF rule it waives | 2026-08-02 | **One POST route for password verification, and nothing else — with no CSRF token, deliberately.** [M35](docs/build-notes/phase-details/m35.md) requires a password challenge on a tree whose inherited rule is *no session lookup, no CSRF check, no template rendering*, and requires the owner to sign the amendment off before it lands. Signed off 2026-08-02. `methodFilter` gains `POST` on the verification route only; `tripwireAuthenticator` still fails on **any** session lookup, so the link host keeps having no session middleware; the challenge page stays template-engine-free. **Why CSRF protects nothing here:** the POST issues nothing. It verifies argon2id against Postgres and answers the redirect itself — no cookie, no unlock token, no session — and the redirect tree sets no cookie anywhere. *(The status is **303**, not the 302 this row said when it was written; amended by [D94](#phase-2-decisions-taken-after-the-plan-was-finalised) on 2026-08-04, which changes only the status and leaves the waiver's reasoning intact.)* A forged submission cannot read the cross-origin `Location`, changes no state, and at most sends a visitor to a destination whose password the attacker already had. **The waiver is justified by that, not by CSRF being unnecessary in general:** if a later milestone makes an unlock *persist*, something is issued to the browser and this decision must be revisited rather than inherited. Chosen over a stateless HMAC token, which would expire and so refuse a correct password typed after a delay, and would make the challenge HTML cache-poisonable unless every path sets `no-store` — a usability regression and a new sharp edge bought against an attack with no victim. |
| D54 | Brute-force protection on the password challenge | 2026-08-02 | **Rate-limited per alias and per address, on the shared limiter [M24](docs/build-notes/phase-details/m24.md) already built.** m35.md never named this; it is in scope because [docs/SECURITY.md](docs/SECURITY.md) already claims credential endpoints carry per-account lockout *and* per-address rate limiting, and a new credential endpoint with neither would make a shipped claim misleading — which the Docs gate treats as failing rather than as cleanup. No new mechanism is introduced. **The per-alias limb is load-bearing for [D53](#phase-2-decisions-taken-after-the-plan-was-finalised)**: without it, guesses driven through many visitors' browsers would spread across addresses and defeat a per-address limit, which is the one CSRF variant with real teeth. Accepted cost, stated: the limiter is a Redis path on a tree designed so Redis is optional, so it **fails open** when Redis is down — the only behaviour consistent with the cache-is-optional rule, and it makes this protection best-effort rather than a guarantee. Documented as such. |
| D55 | Where a split arm's weight lives | 2026-08-03 | **On `destinations.weight`, the column migration 00300 created in Phase 1 for exactly this.** A weight is a property of the place traffic goes rather than of the sentence that sends it there, so an arm keeps its weight across a URL edit, the redirect path reads both from one row, and the breakdown can put the configured weight beside the observed share without a second lookup. Chosen over a column on `routing_rules`, which would sit beside the `kind` that gives it meaning but would split an arm's identity across two rows — every read a join, every write two statements that can disagree. **Weights are relative, never percentages**, so 60/40 and 600/400 are the same test and a third arm can be added without re-balancing the first two; the percentage a person reads is computed against the *enabled* arms, because a parked arm receives nothing. |
| D56 | How a split composes with rules, and what it deliberately is not | 2026-08-03 | **Match rules first, then the split, then a fallback, then the link's own destination.** A matching rule beats a split because a rule is a statement about *who* and a split is a statement about *how many* — reversing it would let a percentage override a country rule somebody wrote on purpose. **A link's arms are all one kind**: "40% of visitors, in rotation" has no meaning, and permitting the mix would put a precedence rule on the hot path for a state nobody intended. **A kind cannot be changed on an existing arm** — converting a running weighted test into a rotation makes its own history two experiments drawn as one. **A fallback is a fourth kind, at most one per link**, standing in for the link's own destination without changing it, so switching it off is reversible. **No stickiness**: selection is per request, so the same visitor may see two arms. Each click is an independent trial and attribution comes from `click_events.destination_id`; the alternative is a cookie this redirect path refuses to set (as D2 already refused for conditions) or a per-visitor lookup on a path designed to perform none. |
| D57 | Where the sequential rotation counter lives | 2026-08-03 | **A new `rotation` column on `link_click_budget`, beside `consumed` and never sharing it.** D8's "via M35's durable counter" is reuse of the table and the mechanism — one upsert per request, serialising on the row lock, which *is* the strict global order. It is not reuse of the budget column, and the failure that forces the separation is concrete: a one-time link carrying a sequential split would have its single click spent by the rotation before the gate ran, and would answer 410 on the visit that was supposed to work. Monotonic and never reset; adding or removing an arm re-phases the rotation rather than restarting it, because there is no correct answer to "who was next" once the list changed. **A failure to advance is 503, never a guess** — an arbitrary arm would make the order approximate, which is the thing D8 named as a support ticket. **A request later refused has still spent its position**, because the arm is chosen before the deep-link join and before the gates; M35's ordering is the stronger constraint. The password challenge is the one exception (F87, M45): it is not a refusal but the first half of a visit arriving in two parts, so a request about to be challenged chooses no arm at all. |
| D58 | The shape of `click_events.destination_id` | 2026-08-03 | **Nullable, no foreign key, and appended to the COPY column list rather than inserted.** Nullable because NULL means the link's own destination — where every click before the column and every click on a split-free link goes — so a backfill would write an id nothing measured and a default would copy what `links.primary_destination_id` already says. No foreign key because the highest-write table in the system is partitioned and written by binary COPY: a reference would cost a lookup per row and make deleting an arm lock the whole click history. The consequence is reported rather than hidden — clicks against a deleted arm appear as *a destination that no longer exists*, because a running test's totals must not change when somebody tidies up. Appended last because `pgx.CopyFrom` sends by position and a list out of step with the row slice writes values into the wrong columns **silently**; appending leaves the sixteen prior positions untouched, and two tests guard it — a width check and an integration test that reads a written row back column by column. |
| D59 | A second cache-key bump inside one unreleased phase | 2026-08-03 | **`CacheKeyVersion` moves to v3.** M34 called v2 "the phase's one deliberate bump"; this is the same argument arriving again rather than that claim being dropped, and both ship in 0.2.0, so an upgrade from 0.1.0 pays for one cold cache and not two. The mechanical cause is that the cached destination list changed shape and a v2 payload does not decode into it — but that alone would only cost a discarded entry. The real cause is M34's verbatim: a v2 entry carries no split arms, so a link whose owner has since divided its traffic would keep sending all of it to one destination for up to `REDIRECT_TTL`, which is a configured control being silently absent. Two consequences of the reshape: deduplication moved from the URL to the destination id, because a merged entry would credit one arm with another's clicks; and a match rule's kind encodes as absent, so a link carrying only M34's rules is the payload size it was. |
| D60 | How the per-destination breakdown is read | 2026-08-03 | **A rollup pass of its own, not a seventh row in the dimension rollup and not a scan of `click_events`.** `analytics.Reader` reads rollups and nothing else except the bounded recent-activity feed, so the breakdown obeys that. Folding it into `RollupDimensionDaily`'s LATERAL expansion would have been one line and would have grown the sort and the upsert count by a sixth for **every click on the instance**, to serve a column that is NULL on every link running no split — against a rollup that is already this project's largest known cost and is scheduled for [M37](docs/build-notes/phase-details/m37.md). `RollupDestinationDaily` instead filters on `destination_id IS NOT NULL` over a partial index, so an instance with no split tests reads an empty index and writes nothing. The value stored is the destination **id**, resolved to a URL at read time — storing the URL would freeze it at rollup time and make an edited arm read as two. The link's own destination is the remainder, so the figures sum to the total by construction. |
| D61 | Which workspace `lctl demo` seeds into | 2026-08-03 | **The owning account's oldest live workspace, pinned before the reset runs — never the account's last-used preference.** `demoReset` scopes its `links`, `link_tags` and `destinations` deletes to the actor's workspace and everything else it removes to the organization, so an actor resolving elsewhere commits the destructive half and then collides with the catalogue it could not see: alias uniqueness is per domain, not per workspace. That is how `make demo-update` failed after M36 was committed — the reset took the accounts, the second workspace and a month of clicks, and the first catalogue link answered `409`. The preference is written by M25's switcher every time somebody clicks through the demo, and by the seeder itself while it fills the second workspace, so a run that dies in between arms the next one. `demoActor` asks *where does the demo live* instead, and the answer — the workspace the account was given when it claimed the instance — is `ResolveWorkspaceForUser`'s own final tiebreak made unconditional. An account that has **pinned** a default workspace outranks last-used and cannot be repointed; there the command refuses with the reason rather than half-resetting. Chosen over widening `demoReset` to the organization, which would stop the reset missing rows and still let the catalogue land wherever somebody last visited. |
| D62 | How the demo attributes clicks to split arms | 2026-08-03 | **Bucketed on `visitor_hash`, not on the click's id.** `attributeSplitClicks` claimed to be deterministic *"so `lctl demo --reset` twice produces the same breakdown"* and never was: `demoClicks` writes every row with a fresh `uuid.NewV7`, so the same seeded dataset hashed differently on each run and the per-day breakdown gained and lost rows between two runs — a low-frequency failure of M33.5's idempotency check, which is worse than a reliable one. The visitor hash is derived from the day and the visitor index, both from the seeded PRNG. The change also makes the demo truer: a visitor who returns the same day now sees the same arm, which is what a split test does. Weights, the 60/90ths and 30/90ths bucketing, and the deliberate gap between configured and observed share are unchanged. |
| D63 | Where the world map comes from, and under what licence | 2026-08-03 | **Natural Earth, taken as the world-atlas 110m TopoJSON and converted to SVG paths by a committed Go generator.** Natural Earth is explicitly **public domain**, no attribution required; world-atlas is the standard derivation of it and its packaging is ISC. That combination is what makes this safe to vendor, and [M37](docs/build-notes/phase-details/m37.md) calls a wrong licence a blocker rather than a cleanup. **Two artefacts, with different rules, matching idioms this repo already has.** The fetched TopoJSON is the *vendored* file: acquired by a `scripts/get-*.sh`, pinned to a version, checksummed, and verified by the `verify-assets` gate with `VERIFY_ONLY=1` so a mismatch is fatal rather than silently repaired — exactly the htmx and Swagger UI contract. The per-country SVG paths are *generated output*, committed like `sqlc`'s `dbgen`, and regenerating them on an unchanged tree must produce no diff. Conversion happens at generate time and never at request time, so `ui` stays stdlib-only and the CSP is untouched — the server renders inline SVG from Go data. Owner-answered at M37's validation. Chosen over a ready-made SVG (the ready-made ones carry attribution, share-alike or unstated licences, which is the blocker m37.md names), over deferring the choropleth to its own milestone, and over dropping the map and satisfying the row with charts alone — the last two being scope changes the owner declined. Accepted cost: the conversion is real work — winding order, the antimeridian, and a projection that has to be chosen and named — and there are now two artefacts to keep honest instead of one. |
| D64 | How much longer the dimension cadence is, and what "stale" is measured from | 2026-08-03 | **Fifteen minutes, and staleness measured from the last *success* — which needed a new `job_state.last_success_at` column.** The split-cadence option was already recorded; neither the interval nor the metric's shape was. Fifteen minutes takes a job measured at 4.8-6.3s from a 9.6x margin against its interval to about 143x, which is the difference between a fix and a postponement, while capping the visible lag at a quarter of an hour — long enough to be worth an alert, short enough that nobody watching a link's traffic thinks the breakdown is broken. Five minutes would have left a 48x margin for a lag nobody would notice; an hour would have made the breakdowns feel stale on a busy link. The metric is `linkctrl_rollup_staleness_seconds{job}`, read from `job_state` rather than from process memory, because the existing `linkctrl_job_last_success_timestamp_seconds` is set only by the replica that did the work and is cleared by a restart — on a rolling deploy it reports a stalled job as healthy. `last_run_at` could not carry it either: `RecordJobFailure` stamps that column, so a job failing every tick would publish itself as perpetually fresh. Migration 02300 adds `last_success_at`, backfilled from `last_run_at` where `last_error IS NULL`, and each half of the rollup keeps its **own** `job_state` row — sharing one would let the 60-second job advance a watermark past a day the 15-minute job had not covered, which is precisely the permanent-gap bug the watermark was introduced to fix. |
| D65 | What the choropleth is shaded by, and what it refuses to draw | 2026-08-03 | **Banded by share of the largest country's figure, five bands, with "no data" outside the ramp — and no map at all when there is no GeoIP database.** Banding by rank would colour the fifth-busiest country the same whether it sent half the traffic or four clicks; banding by share of the *total* puts everything in band one as soon as traffic spreads across forty countries, which is what a working link looks like. Five bands because a monotone ramp spanning 1.10:1 to 7.90:1 cannot hold more distinguishable steps at the size a country is drawn, and because the exact figure is always one hover or one click away — the ranked list is never replaced, only added to. A country with no clicks is `sunken` rather than the bottom band, because "nobody came from here" and "one person came from here" are different answers. With no GeoIP database the map is **not rendered**: a world uniformly in the no-data colour is a picture of nothing that looks like a picture of something. Fixing that exposed a defect this milestone's own claim depended on — the ranked list's "no GeoIP database is configured" empty state had been unreachable since it was written, because an unresolved click rolls up under the value `unknown` and the list rendered that instead, so the map could not say it "exactly as the ranked list already does". The list is now given nothing to rank when the instance cannot resolve a country. The unique-visitors layer repeats the daily-estimate caveat verbatim; the clicks layer does not, because clicks are counted rather than estimated. |
| D66 | The shape of a folder, and what deleting one means | 2026-08-03 | **Deleting a folder is a real `DELETE`, so Phase 1's two foreign keys actually fire** — `folders.parent_id ON DELETE CASCADE` takes the branch, `links.folder_id ON DELETE SET NULL` unfiles every link anywhere in it, and no link is ever removed. A soft delete would have been consistent with every other table here and would have left those links pointing at a row the tree no longer walks: intact in the table, absent from every page, which for anybody using the product is the same as losing them. `folders.deleted_at` therefore stays unwritten while every query still filters on it, so the partial indexes serve and a later reversal changes one method rather than seven queries. **Sibling names are unique case-insensitively**, enforced in the service for a readable 422 and backed by a unique index whose `COALESCE(parent_id, nil-uuid)` is what covers the roots — `NULL <> NULL` would otherwise leave the top level unconstrained. **Eight levels**, a product limit taken from the two surfaces that render it rather than from any technical bound, plus 500 folders per workspace so the tree and every folder `<select>` stay one small query. **A folder may never become its own descendant**, and the depth cap is checked against the moved *subtree* — a two-level branch dropped one short of the cap puts its child past it, which a per-folder check accepts. All three rules live in Go over one flat query rather than in recursive SQL, because they are the same walk and a recursive CTE over a table with no cycle constraint never terminates if the data ever holds one. `domain.FolderTree.MoveRefusal` states every refusal once and is read by both the writer and the page, so the tree cannot offer a destination the service refuses. The links list filters by **one folder, not its subtree**, because that number has to equal the count shown beside the folder; `?folder=none` is the separate question a nullable id cannot ask. The tree UI is click-to-move via POST forms — **no drag-and-drop**, asserted by test, because a drag target is unreachable by keyboard and needs script `ui` has no build step for. |
| D67 | Folders mint no permission | 2026-08-03 | **Reuse `links.*`; matched neither limb of [D18](#phase-2-decisions-taken-after-the-plan-was-finalised).** Reading a folder tree exposes no actor identity tied to network data, and holding folder access widens nobody's reach — a folder decides nothing about where a link points, who may follow it, or what anybody may create. So the only question left is whether the vocabulary earns two or four more entries, and it does not: reading the tree is `links.read`, creating is `links.create`, renaming and moving is `links.update`, deleting is `links.delete`. Same call [M34](docs/build-notes/phase-details/m34.md) made for routing rules and [M36](docs/build-notes/phase-details/m36.md) for split arms, for the same reason — *a folder is where a link lives, in the sense a rule is where a link points*. A `folders.*` set would need a seed migration, four grants and a delegability call, and every grant would land on exactly the roles that already hold the link permissions. No migration and no `NonDelegableScopes` entry: a viewer sees and filters by the tree, an editor organises it, and an API key holding `links.update` may move a folder. **What would change this**: a folder that carried a setting — a default destination policy, a per-folder bot rule, a grant scoped to a branch. Then holding it would widen reach and D18's second limb would apply. None exists, and none is planned in Phase 2. |
| D68 | Where a domain's owning workspace lives | 2026-08-03 | **A new nullable `domains.workspace_id`, with a CHECK expressing the three legal states** — instance default (both owner columns NULL), organization-owned (`organization_id` set), workspace-owned (both set, the workspace implying its organization). Taken before code because [M39](docs/build-notes/phase-details/m39.md) requires it and because Phase 3 inherits the shape. **Alias uniqueness is per domain, not per workspace**, and that is what decides it: one owning workspace per domain keeps the alias namespace unambiguous, where a shared hostname would have two workspaces racing for the same alias with no rule to settle it. Chosen over reusing the existing nullable `organization_id`, which is the smallest schema and the least honest fit — the scope row at line 143 promises a *workspace* administers its own hostname, so org-grain would discharge it only by rewording it. And over a `domain_workspaces` join table, which is the most flexible for Phase 3 and permits exactly the shared-namespace race above — precisely the alias-hijack surface M39 was split out to keep reviewable in isolation. Accepted cost, stated: two nullable ownership columns whose legal combinations live in a CHECK constraint rather than in the type, and Phase 3 inherits the constraint with the column. |
| D69 | What managing a domain means before anything is served | 2026-08-03 | **Update is the hostname and nothing else; the instance default's guard is unchanged; no hostname is checked against the instance's own names; no permission is minted.** A `domains` row also carries `root_redirect_url` and the two bot-blocking flags, and extending those per domain would configure how a hostname *serves* before anything serves it — [M40](docs/build-notes/phase-details/m40.md)'s work — while giving a workspace a second route to settings the instance default administers through `/api/v1/domain`. So create is register, update is rename, delete is remove, and the singular settings endpoint is untouched. Renaming is safe only because nothing is served: links, clicks and reserved aliases hang off `domain_id`, and M40 must invalidate verification on rename. **The instance default** keeps exactly the guard [M20](docs/build-notes/phase-details/phase-1.md) gave it — `domains.write`, granted by migration 00800 to the owner and admin roles alone — and is additionally refused a rename or a delete through the collection whoever asks, because its hostname is a placeholder `ResolveDefaultDomain` never reads and deleting it would take the hostname out from under every link on the instance. The residue is named rather than left to be found: `domains.write` is a *role* permission, so on a multi-organization instance any organization's owner or admin holds it; that is the behaviour before M39 and after it, and it is recorded as **F70** rather than narrowed here. **No self-host refusal.** Whether a registrant controls a hostname is DNS's question and M40's verification is the answer; a syntax check that rejected a few recognizable names would read as protection while proving nothing. Refused instead is what cannot work — a pasted URL, an IP address, a single label, a numeric TLD. **No new slug**: M39 adds a scope check to `domains.write` rather than a second kind of permission, so no seed migration, no `NonDelegableScopes` entry, and D18's delegability question does not arise. |
| D70 | What happens when a verified domain stops verifying | 2026-08-03 | **A bounded grace window, a notification on the first failure, then a real hard stop** — `verified_at` cleared, the host back to ops-only 404, invalidated across replicas through [M23](docs/build-notes/phase-details/m23.md)'s pub/sub. A single successful re-verification at any point resets the failure count. [M40](docs/build-notes/phase-details/m40.md) fixes *"never silently persists"* and cites a recorded decision that did not exist; this is it. **Why not stop on the first failure:** re-verification is a poll against DNS, so one resolver hiccup, one rate-limited query or one brief nameserver outage would take a paying customer's links down with no human in the loop. One failed check is weak evidence. **The cost, which is the security one:** for the length of the window the instance keeps serving a hostname whose DNS the workspace may no longer control, so authority is stale by exactly that much. The window is therefore bounded, short enough to state plainly in the runbook, operator-visible, and the stop at the end of it is real rather than a further warning. Rejected: degrading to *keep serving, refuse new links*, which keeps the alias namespace live on a hostname that may have been lost — the mildest possible response to the one failure mode m40.md calls its whole security story. Owner-answered at M40's validation, before code. |
| D71 | The numbers in the window, and the four questions serving raised | 2026-08-03 | **One day, checked hourly** (`DOMAIN_VERIFY_GRACE=24h`, `DOMAIN_VERIFY_INTERVAL=1h`), so a hostname must fail twenty-four consecutive checks before its links stop resolving. The window is bounded below by what a human can act on — a shorter one warns somebody at 02:00 and takes their links down before they read it — and above by what [D70](#phase-2-decisions) says it costs. The cadence is what makes the window mean anything: at an hour, a resolver blip cannot produce twenty-four consecutive failures. Both are environment variables, which is D70's second constraint. **A workspace's own verified hostname becomes the default for its new links**, which is the filter `GetWorkspaceDefaultDomain`'s name had been promising and its comment conceded; the instance default remains the fallback, ties break on `verified_at`, and the cost is that a workspace ends up with links on two hostnames because nothing rewrites a URL already published — which is part of why the links list gained a hostname filter. **A rename un-verifies and re-mints the token**, the bullet [D69](#phase-2-decisions) deferred here: the record proves control of the *old* name, and the old token is published in a zone that may be somebody else's. **The `ask` endpoint answers for verified custom hostnames only** — not the instance's own hosts, which an operator configures statically — because widening it would turn an unauthenticated endpoint into a certificate-issuance trigger for names nobody has proved they hold. `ssl_status` is kept current to the limit of what the app can know and `error` is never written. |
| D72 | The QR encoder, and what it was weighed against | 2026-08-03 | **`github.com/boombuler/barcode` v1.1.0, MIT, with no module dependencies of its own** — direct dependencies go from twelve to thirteen and the indirect set does not move. It is used for one thing, turning a string into a module matrix; `internal/qr` draws the SVG, because `qr_codes.style` has to drive the quiet zone, the colours and the size. Weighed on licence, maintenance and what each candidate drags in. **`rsc.io/qr`** is the close call and loses on maintenance alone — zero dependencies, BSD-3, a clean accessor, no release since 2018-06-05 against a frozen spec; everything else was equal, so the library that shipped a release this year is the one to be holding. **`piglig/go-qr`** is the only candidate with a built-in SVG renderer and compiles `image/png` and `compress/zlib` in for a path this product never calls, which is the shape [D11](#phase-2-decisions) exists to avoid; its SVG is also its own, and the styling here comes out of jsonb regardless. **`skip2/go-qrcode`** has no tagged release and no commit since 2020-06-17. Writing the encoder was not considered: Reed–Solomon over GF(256), version selection and eight mask patterns have a failure mode — scans on one reader, not another — that no test here would catch. What is *not* delegated is the drawing, so the SVG is parsed back into a grid and compared to the encoder's matrix, and the three finder patterns are checked against the picture's own corners without consulting the encoder at all. |
| D73 | `scan_count` is dropped rather than wired | 2026-08-03 | **Dropped, and dropped from the schema** — migration 02700 runs `ALTER TABLE qr_codes DROP COLUMN scan_count`. m41.md required this decided rather than hedged. Wiring it costs a write per scan on a path whose whole budget is 20ms, or new per-click work on the rollup [M37](docs/build-notes/phase-details/m37.md) has just fixed — which is exactly the load this same milestone refuses to add for campaign analytics, and refusing it there while accepting it here would be two answers to one question. The number would also be *worse* than the one the product has: a scan is an ordinary click carrying `?src=qr`, already counted per day, deduplicated by visitor, bot-filtered and broken down by device and country, where `scan_count` is a monotonic integer with none of that — and two numbers for one quantity disagree rather than corroborate. Nothing ever wrote it, so no history is lost. **The rule it bends is named**: *DDL is additive within a minor version* is inherited by every Phase 2 milestone and a `DROP COLUMN` is not additive; it lands before 0.2.0 on a column no released code reads or writes, and keeping it would preserve a dormant counter that reads as a supported feature. What would bring it back: a scan that is not observable as a click — a per-print code reconciled offline — of which none exists. |
| D74 | A QR code does not follow the theme | 2026-08-03 | **The picture paints its own background across the quiet zone and defaults to black on white in both themes; the frame around it is a theme token.** Two facts force it. A scanner expects dark modules on a light field — inverted codes are refused by a large share of readers — and an SVG with no background is transparent, so a code drawn in `ink` becomes a light code on a dark field the moment somebody switches theme. That is not a styling regression but a code that stops scanning, discovered by whoever printed it. The theme therefore owns everything around the drawing and nothing inside it. `TestTheQuietZoneIsEmptyAndPainted` asserts the background rect spans the whole viewBox. The style form can still produce an unscannable code, deliberately — a workspace may want a brand colour and this product does not own their contrast judgement — but it refuses the two failures that are not judgement calls: the same colour twice, which is a blank square, and anything that is not a `#rgb`/`#rrggbb` colour, which would be markup inside a document `internal/qr` generates. |
| D75 | QR codes and campaigns mint no permission | 2026-08-03 | **Reuse `links.*`; matched neither limb of [D18](#phase-2-decisions-taken-after-the-plan-was-finalised).** Reading a campaign list exposes no actor identity tied to network data and holding one widens nobody's reach; a QR code is a picture of the link's own short URL, so seeing one is seeing the link. Reading either is `links.read`, creating a campaign is `links.create`, editing one or styling a code is `links.update`, deleting one is `links.delete` — the call [M34](docs/build-notes/phase-details/m34.md) made for rules (D49), [M36](docs/build-notes/phase-details/m36.md) for arms and [M38](docs/build-notes/phase-details/m38.md) for folders ([D67](#phase-2-decisions-taken-after-the-plan-was-finalised)), for the reason D67 gives: every grant a `campaigns.*` set needed would land on exactly the roles that already hold the link permissions. No seed migration, no `NonDelegableScopes` entry, and an API key holding `links.update` may label a link or restyle its code. A viewer sees and filters by campaigns and can look at a code, which is right — they can already see the short URL it encodes. **What would change this**: a campaign that carried a setting — a default destination policy, a per-campaign bot rule, a UTM template applied at redirect time. `campaigns.settings` stays empty for that reason; the moment something reads it, this decision is reopened rather than inherited. |
| D76 | How a scan tells the analytics what it is | 2026-08-03 | **A reserved `src` query parameter, encoded inside the picture, landing in the existing `referrer` breakdown as the value `qr`.** A camera sends no `Referer`, so without it every scan arrives indistinguishable from a typed URL; the fact has to ride in the payload because there is nowhere else it can come from. **No new analytics schema**: no column, no dimension name, no rollup pass, no reader key, no template row — `link_dimension_daily` already stores `(dimension, value)` generically and already holds a non-hostname sentinel there, since the rollup writes `direct` for a click with no referrer. A `source` column would have been a seventeenth position in a positional binary COPY and a seventh row in the LATERAL expansion [D60](#phase-2-decisions) declined to grow for exactly this kind of mostly-null value. **The vocabulary is closed, and that is load-bearing**: the dimension table's primary key includes the value, so an open parameter would let anybody append `?src=` and a fresh random string to a popular link a million times and grow that table permanently — write amplification anybody can trigger. `domain.ClickSource` resolves against an allowlist of one entry and ignores everything else. **It is forwarded rather than stripped**, unlike the signature parameters (M35): a signature is a credential and leaking one hands the destination a replayable URL, while a source tag is a label. On the redirect path it costs one `strings.Contains` over the raw query, false for every request that is not a scan, with `url.ParseQuery` reached only when the substring is present — the shape `gate.StripSignature` already uses.
| D77 | How a webhook delivery is claimed | 2026-08-03 | **`FOR UPDATE SKIP LOCKED` *under* the existing leader lock, not instead of it.** [M42](docs/build-notes/phase-details/m42.md) offered the two as alternatives and that "or" is the mistake: leadership alone still duplicates, because an advisory lock drops when its holder dies mid-drain, and skip-locked alone still has every replica dialling every receiver. **Redis stays cache-only** — the queue is Postgres, which disposes of the Redis Streams upgrade path as *unexercised rather than adopted*. |
| D78 | The rebinding posture for a fetch the server makes itself | 2026-08-03 | **The address is checked, not the name, in the dialer's `Control` hook — after DNS and before `connect(2)`**, once per attempt, against the unappealable tier's own predicate. **No redirect is followed at all**, which is stronger than *none to a private address* and leaves no second hop needing a second policy. `Proxy: nil`, because `HTTP_PROXY` would defeat the check. The redirect path's accepted rebinding gap is **explicitly not inherited**: that path sends a visitor's browser, this sends the server, and the address that means nothing there is the metadata endpoint here. The feed's dialer is deliberately not shared — the feed's URL is an operator's own choice about their own network. |
| D79 | The part of a webhook that is a published interface | 2026-08-03 | Six events, a closed vocabulary, an explicit payload map rather than a marshalled struct, and the HMAC key is the secret **as displayed**. Seven attempts across 61 minutes; the attempt count is a constant while the per-attempt timeout and the retention window are settings. Documented for receivers with a worked verifier, because a signature nobody outside this repo can check is not a signature. |
| D80 | Webhooks mint two permissions, and one is not delegable | 2026-08-03 | `webhooks.read` stays delegable; **`webhooks.write` does not**, matching [D18](#phase-2-decisions-taken-after-the-plan-was-finalised)'s **second** limb — a webhook keeps delivering after the credential that created it is revoked, so a key that could create one would have reach that survives its own revocation. This is where [D75](#phase-2-decisions-taken-after-the-plan-was-finalised)'s *reuse the link permissions* reasoning stops: a QR code is a picture of a link, and a webhook is an egress channel. |
| D81 | What the webhook demo shows, and why it dials nobody | 2026-08-03 | One enabled and one paused, so the pause control is visible; `.example` hostnames, which cannot resolve for anybody; and the seeder queues **no** delivery — asserted as a coverage row rather than trusted, because a demo instance that dialled out would be the one surface where seeded data reaches somebody else's network. |
| D82 | `last_fired_at` is a watermark, and it is the loop guard | 2026-08-03 | A position, not a diagnostic. Every match query reads the half-open window `(last_fired_at, now]`, and a compare-and-set advances the column past the last subject handled **before any action runs** — so a subject is visible to a rule exactly once, however many times the scheduler ticks. It advances to the last subject *handled* rather than to `now`, so a run truncated at the per-rule cap defers its remainder instead of dropping it. A rule is armed at creation and re-armed on resume, because a NULL watermark means "everything that ever happened". Below the threshold it does not move, so matches accumulate. |
| D83 | The cascade a watermark cannot stop, and what does | 2026-08-03 | The watermark stops a rule feeding itself; it does nothing about rule A producing what rule B triggers on. `domain.TriggerReads` and `domain.ActionWrites` declare both halves at the granularity the queries filter at, and a test asserts they never intersect — a property of the vocabulary, so it holds for rules that do not exist yet. Three constraints follow: the webhook action emits only `automation.fired` (a **seventh** webhook event, and nothing triggers on it) rather than letting a rule choose; the archive writes `status` and never `expires_at`; and the link-expired query does not filter on status, which is why 02900 adds an index 00300 could not supply. |
| D84 | What bounds one evaluation run, and where the numbers live | 2026-08-03 | Four constants in `internal/domain`, with the arithmetic beside them: 100 rules a run × (1 match query + 3 actions + 25 archive statements) = **2,900 statements** worst case, against a one-minute clock and a two-minute job timeout. Expected cost is 100 indexed range scans that return nothing. Both caps log when they bite, and the API advertises the whole `evaluation` block, because a bound nobody can state is not one. `MaxAutomationMinCount` is defined *as* the match cap so a threshold can always be reached. `automation_rules` gains no column; 02900 adds three indexes. *(One column since, and the arithmetic is 2,901: `last_checked_at`, the scheduler's own cursor, added by 03100 under [D96](#phase-2-decisions-taken-after-the-plan-was-finalised) on 2026-08-04. The ordering fact and the firing watermark had been the same column, which is what left the hundred-and-first rule unevaluated; the four constants and every other number in this row are unchanged.)* |
| D85 | Automation mints two permissions, and one is not delegable | 2026-08-03 | `automation.read` stays delegable; **`automation.write` does not**, on [D18](#phase-2-decisions-taken-after-the-plan-was-finalised)'s durability limb again — one turn past [D80](#phase-2-decisions-taken-after-the-plan-was-finalised), because a webhook is a standing instruction to *report* and a rule is one to *act*, unattended, after the credential that wrote it is gone. The archive a rule performs takes **no actor**: a synthetic identity holding `links.delete` would be the scheduler manufacturing authority `internal/auth` exists to keep unmintable, so the statement is workspace-scoped instead and the firing writes an `automation.fired` audit record naming the rule. |
| D86 | What the automation demo shows, and why it changes nobody's links | 2026-08-03 | Three rules, one per trigger, one paused. **No seeded rule archives a link** — the demo is public, and that is the one action another visitor feels — asserted as a coverage row rather than trusted. The webhook action *is* seeded, precisely because [D81](#phase-2-decisions-taken-after-the-plan-was-finalised)'s `.example` hostnames dial nobody, so the page shows an automation with an outbound consequence and the instance connects to no one. The seeder fires nothing, because arming at creation outruns everything it wrote. |
| D87 | A key that replaces itself, and the leak it can carry | 2026-08-03 | [D9](#phase-2-decisions) made concrete. Rotation is a key replacing **itself**: `POST /api-keys/rotate` takes no id, scopes may only narrow against the **row's** stored set rather than the actor's current permissions, the workspace binding is copied verbatim, and a key rotates **once** — enforced by the service and by a unique index on `successor_id`, so the lineage is a chain and not a tree. The grace window defaults to an hour, floored at five minutes (ten `last_used_at` flush intervals, so "is the old key still in use" is answerable before it closes) and capped at twenty-four. The refusal is read from the row on the request path, not written by a job; housekeeping's `revoked_at` is bookkeeping so the key list agrees with the behaviour. The successor inherits the predecessor's **lifetime**, not its deadline — the one dimension rotation refreshes. Accepted trade restated in the open: a leaked key can persist across rotations, bounded by a visible list, an `apikey.rotated` audit record per generation, and the capped window. `apikeys.*` is untouched and `TestNonDelegableScopesCoverKeyManagement` passes unmodified. |
| D88 | How wide a key reaches, and why no permission was minted for it | 2026-08-03 | The workspace choice migration 00500's nullable column and `apikey.go`'s comment both promised. Opt-in, defaulting to the shipped single-workspace behaviour, and gated on `MembershipAuthority.In(nil)` rather than `Identity.Can` — under [D31](#phase-2-decisions-taken-after-the-plan-was-finalised) a workspace-scoped role answers yes to `Can(apikeys.write)`, and issuing organization-wide reach on that basis is exactly F27's shape, which [D44](#phase-2-decisions-taken-after-the-plan-was-finalised) already answers. **No new permission**: a slug is held per role and roles are granted per membership, so an `apikeys.org_scope` would have been held by the same workspace-scoped admin and enforced nothing. Matches neither limb of [D18](#phase-2-decisions-taken-after-the-plan-was-finalised), because it adds no permission to classify. |
| D89 | What the key demo shows, and why no secret is on it | 2026-08-03 | Three keys and four rows — a rotation is a row rather than an edit — covering a rotated pair, an organization-wide key and an ordinary one, bounded above because every row is a credential on a public instance. Every token is discarded: the product shows one once, and a demo that kept one would be publishing a live credential. The rotation runs through the real path, because `Rotate` refuses any actor that is not a key and a seeder writing `successor_id` directly would show a state the product might no longer produce. The seeded window is the maximum, which is a fact about `demo-update`'s cadence rather than advice; a coverage row asserts it has not already closed. The reset removes every key in the demo organization, visitors' included. |
| D90 | The tenancy bound an organization-wide key needed | 2026-08-03 | A defect M44 created by making a NULL `workspace_id` issuable at all, and fixed inside M44 because it falsified M44's own claim. `ResolveWorkspaceForUser` filters on membership, so an owner belonging to two organizations would have their organization-wide key resolve into the *other* one — and `Authenticate` takes the organization from the resolved workspace, so the key would act wholly in a tenant nobody issued it for. Fixed as an optional **bound** on the candidate set rather than a rung in the precedence: the key still follows its owner's pinned default, inside its own organization, and every other caller passes NULL and resolves exactly as before. Unreachable in any released version, because `Create` always wrote a workspace id. |
| D91 | `golang.org/x/net/idna` is added, reversing the punycode decision | 2026-08-04 | **The dependency goes in**, because F77 cannot be closed without full UTS-46 mapping and a hand-rolled separator map is a half-fix that reads as closed. Two consequences accepted with it: the stored value becomes the ToASCII form, which is [D46](#phase-2-decisions-taken-after-the-plan-was-finalised)'s own rule, and a project with almost no dependencies gains one. Answered on [M44.9](docs/build-notes/phase-details/m44.9.md)'s triage, before the reopening that uses it. |
| D92 | What a verification may write, and what a pass may be delayed by | 2026-08-04 | **The write that sets `verified_at` is predicated on the hostname and token that were checked**, at both call sites, with a zero-row branch — a transaction closes nothing here because the rename commits *between* two transactions, and `FOR UPDATE` across the DNS lookup trades a hijack for a lock convoy. The audit record names the hostname that was **checked**, so the log cannot corroborate what it exists to catch. **Re-verification is drawn in two classes, serving hostnames first**, inside the one configured batch: a rename un-verifies, so registration churn can crowd only the pending class and the D70/D71 hard stop stays reachable. **A workspace may register at most 25 hostnames** — a constant, bounding work owed to somebody else's nameserver rather than a page. The batch was not raised and no lookup concurrency was added; nothing reaps a registration, because a NULL watermark is what a live renamed row carries. |
| D93 | How a destination host is folded, and what happens to one that cannot be | 2026-08-04 | **UTS-46 ToASCII, in `canonicalHost`, at the one fold [D46](#phase-2-decisions-taken-after-the-plan-was-finalised) already put in `ValidateDestination`** — so the trailing dot and the alphabet are handled in one place and every tier still reads its host off the value that function returns. The profile is **WHATWG's `domain to ASCII` with `beStrict` false**, not `idna.Lookup`: Lookup sets `UseSTD3ASCIIRules` and `CheckHyphens` and therefore refuses `my_host.example`, `under_score.example.com` and the real `r3---sn-apo3qvuoxuxbt-j5pe.googlevideo.com`, all three of which this validator accepts today, and a canonicalizer that turns away ordinary destinations is one operators route around. An **all-ASCII host skips the mapping**, as `net/http`'s own `idnaASCII` does — with STD3 rules off the mapping's only effect on ASCII is a case fold already done, and everything else the profile would do to such a host is a rejection rather than a spelling. A host UTS-46 **cannot** map is **refused**, with the untiered `invalid` code rather than an `unappealable.*` one: passing the raw spelling through is precisely [F77](docs/build-notes/deferred-findings.md), and naming a tier would claim a judgement about the destination that nothing made. The **list entries fold identically** — `LINKCTRL_DESTINATION_BLOCKLIST` and `blocked_hosts.txt`, through the same function — because a fix that stored `xn--mnchen-3ya.example` while leaving the operator's entry as they typed it would have re-broken the same sentence it was closing. |
| D94 | Which namespace a gate is keyed on, and what a verified password is answered with | 2026-08-04 | Three corrections inside [M35](docs/build-notes/phase-details/m35.md)'s reopening, and one of them **amends [D53](#phase-2-decisions-taken-after-the-plan-was-finalised)**. **A verified password is answered `303`, unconditionally** — not `REDIRECT_DEFAULT_STATUS`, which admits `307`; RFC 9110 §15.4.8 forbids a user agent changing the method on a `307`, and the challenge form posts `password=<secret>` to the alias with no `action`, so a `307` instance had the browser re-send the password to the link's third-party destination. D53 reads *"answers the **302** itself"* and that clause is superseded here; everything else it decided — the one POST route, the CSRF waiver and the condition attached to it — is untouched, because the waiver rests on the POST **issuing nothing**, which a change of status does not disturb. `303` joins `REDIRECT_DEFAULT_STATUS`'s allowed set, since it is a status this tree now emits. **A signature and a password bucket are keyed on the domain the request arrived on**, not the one resolved at boot: alias uniqueness is `(domain_id, alias)`, so the boot constant described a different link on every request to a verified custom hostname — signature verification was 100% non-functional there, and a signature minted for one hostname opened the same alias on another. **A signed URL is minted on the link's own hostname**, through the same helper `short_url` uses, and a domain row that cannot be read is an **error** rather than a fall back to the instance's host: the default domain is shared across workspaces, so the wrong hostname can resolve a stranger's link rather than merely 404. **`HEAD` checks the click budget without spending it** — a non-consuming read plus `410`, on the HEAD branch only, so a `GET` still performs exactly the one upsert it always did. |
| D95 | What one webhook drain costs the rest of the scheduler | 2026-08-04 | **A claimed batch is dialled together and `Drain` waits for it**, so one drain costs one `WEBHOOK_TIMEOUT` rather than `DrainBatch` of them — the arithmetic that made twenty rows at the default ten seconds occupy the shared job goroutine for two hundred seconds, dropping every other job's tick, is gone. [M42](docs/build-notes/phase-details/m42.md)'s reopening on [F82](docs/build-notes/deferred-findings.md). The concurrency sits **inside** `Drain` with a `sync.WaitGroup` waited on before it returns, so no goroutine outlives the function `withLeadership` releases [D77](#phase-2-decisions-taken-after-the-plan-was-finalised)'s advisory lock on: the lock covers every dial exactly as it did when they were sequential, and D77 is untouched. **A goroutine of its own was rejected on a fact rather than a preference** — `pg_try_advisory_lock` is session-scoped, so a second goroutine holding the lock makes every other job *skip* for the same duration it used to *stall* for, which moves the cost rather than removing it. Out of scope and unchanged: the queue's instance-wide capacity. What a receiver sees: up to twenty concurrent requests, not in queue order, deduplicated on the delivery id [D79](#phase-2-decisions-taken-after-the-plan-was-finalised) already published. |
| D96 | The queue's own clock, and why the watermark could not be it | 2026-08-04 | **`automation_rules` gains `last_checked_at`** (03100), and the due query orders on it: one column had been carrying two facts — *when did this rule last fire*, which bounds every match window, and *when was it last looked at*, which decides whose turn it is. Ordering on the first meant an idle rule held the head of the queue permanently, since the watermark moves only on a firing and the evaluator returns before the claim below the threshold; the hundred oldest were a fixed set and the hundred-and-first enabled rule on an instance was **never evaluated on any run**, showing as enabled with nothing in any log naming it ([F83](docs/build-notes/deferred-findings.md), [M43](docs/build-notes/phase-details/m43.md)'s reopening). **Advancing `last_fired_at` on a no-match run was refused rather than overlooked**: it is the threshold accumulator, and moving it discards the subjects already inside the window, so `min_count` stops working. The cursor advances in **one statement a run** over the rules the pass reached — 2,900 statements becomes 2,901, and [D84](#phase-2-decisions-taken-after-the-plan-was-finalised)'s *"no new column"* clause is superseded, nothing else in it. It writes neither `last_fired_at` nor `updated_at`; it stamps rules whose evaluation **failed**, because a rule that errors every pass would otherwise park at the head forever; and it runs on `context.WithoutCancel` under a five-second bound, so a run cut off by the job timeout keeps what it looked at while the rules it never reached keep their older cursor. A new rule's cursor is NULL and sorts first, so it is evaluated on the next tick. The column is deliberately **not** surfaced on the page, the API or the domain struct. |
| D97 | The mount list is produced by registering, not written beside it | 2026-08-04 | **`appMux` records every pattern it is handed, and the root mux mounts from that record.** [F85](docs/build-notes/deferred-findings.md): eleven of [M42](docs/build-notes/phase-details/m42.md)'s and [M43](docs/build-notes/phase-details/m43.md)'s routes shipped registered, reserved, linked from the nav and documented — and unreachable on every deployment shape, because the root mux mounted from a hand-written slice and the only guard was built from that same slice, so it could only ever compare the list against itself. Adding the four missing strings fixes the instance and not the class. `dashboardPatterns` is deleted, `registerAppRoutes` is split out so the route set can be enumerated without building a router, and `maximalDeps` is filled by reflection because a literal would be a third list failing silently. The API subtree stays hand-mounted deliberately. **Applied a second time at [M45](docs/build-notes/phase-details/m45.md)**, where the metrics surface classifier stopped carrying its own copy of the dashboard paths (F16). |
| D98 | The instance-level principal D38 said did not exist | 2026-08-04 | **Introduced**, over naming a moderator in configuration, scoping the blocklist per organization, or carrying all three. [D38](#phase-2-decisions-taken-after-the-plan-was-finalised) recorded that *"the instance owner"* was not a thing the permission system could name, and three findings bottomed out there — F15, F31 and F36. Two owner-set constraints came with it: **only the instance-owner level may delegate the permission**, and **API access to disputes is read-only, because a change requires a person**. The second is built as a *split permission* with the decide half in `NonDelegableScopes` rather than as a branch on credential type, which the inherited Permissions rule forbids — a literal reading would have added an eighth such branch to the seven F104 was already filed about. A holder may not re-delegate, and the principal's scopes are enumerated rather than implied. |
| D99 | A discarded click costs the same draws as a kept one | 2026-08-04 | **Every draw for a demo click is taken before the click can be discarded**, and the seeded history ends at the top of the hour. F71 and F74: a generator consuming a variable number of draws per iteration has no stable output, so a dropped click consumed three fewer draws than a kept one and re-rolled every link and day after it — which is where the unequal, unbounded and negative deltas came from. The minute boundary was the trigger and not the cause, measured on its own at one click per thirty-seven seconds. The two changes are not alternatives; each fixes what the other leaves standing. |
| D100 | Who administers the instance default domain | 2026-08-05 | **The instance principal**, alongside the dispute queue and the instance audit log [D98](#phase-2-decisions-taken-after-the-plan-was-finalised) already put there. `domains.write` is a *role* permission, so every organization's owner and admin could repoint the default domain's root redirect and change its bot policy — one registration away on an instance running `SIGNUP_MODE=open`. [D38](#phase-2-decisions-taken-after-the-plan-was-finalised) refused this because no instance-level principal existed to name; D98 built one, so the refusal's reason has stopped being true. **The stated cost:** organization owners and admins lose a capability they hold today, and the principal's enumerated scopes grow by one. Chosen over documenting and carrying, and over naming an operator in configuration — a second mechanism for *who runs this instance* beside the one just built. Owner-answered, F70. |
| D101 | Whether a blocked bot is recorded on a link that would answer 410 or 404 | 2026-08-05 | **Recorded, in every link state.** With bot blocking on, a blocked bot wrote a click event for expired and archived links, which recorded nothing before [M32.5](docs/build-notes/phase-details/m32.5.md); no milestone claim is falsified, since m32.5.md says *"a blocked attempt is counted, not audited"* without qualification, which is why this is a decision rather than a correction. Identical traffic is now recorded identically whatever the status made the response. *Record for none* reads as the smaller change and is not: it makes the redirect path decide what to record from the response it was about to send, and leaves a blocked bot on a live link counted, so the rule gains an exception instead of losing one. **Deliberately not fixed here:** archived links still accrue visible counts from crawlers, which is `links.click_count` being rendered raw beside a human-only rollup — F24, approved separately. Owner-answered, F50. |
| D102 | What the notification inbox is scoped to | 2026-08-05 | **The reader, filtered by the workspace they are standing in, with organization-level notifications always shown.** `notifications.workspace_id` had been written since [M40](docs/build-notes/phase-details/m40.md) and read by nothing, while two comments stated it produced a per-workspace inbox — and it is F94's stated mitigation, so that row was closed believing in a containment that did not exist. The predicate is `workspace_id IS NULL OR workspace_id = @ws`; a bare `= @ws` hides every organization-level notification, since disputes and audit growth write NULL, and it must be added identically to the count and the preview or `notifications_user_unread_idx` stops serving one of them. **The cost:** a workspace-scoped notification stops appearing while its reader is elsewhere — which is the behaviour those comments have promised since M40. Owner-answered, F105. |
| D103 | Whether the dashboard requires JavaScript | 2026-08-05 | **It does, and that is now written down.** Owner-answered on [F21](docs/build-notes/deferred-findings.md): requiring JavaScript for the dashboard is reasonable. The workspace switcher's separate **Switch** button is deleted and the select switches on change, carrying the 2026-08-02 directive literally. No `<noscript>` fallback: the stance is recorded instead of defended in markup nobody reads. **No new dependency** — htmx is already served on every page from `layout.html` and already does exactly this at `links.html`, and the owner's answer restated the standing bar that packages are avoided unless necessary. The redirect tree is untouched and stays scriptless; this is a claim about the *dashboard* alone. |
| D104 | Whether README describes the released product or the current branch | 2026-08-05 | **The released product.** Owner-answered at [M45](docs/build-notes/phase-details/m45.md)'s documentation pass, reversing the convention the orchestrator adopted in prose on 2026-07-31 without asking. README is read by somebody who installed a tag, and it should be true for them. The cost is accepted and is real: the per-commit Docs gate becomes close to a no-op, so it stops catching the drift it was added for, and each phase's features land in README at its close instead of as they ship. `CHANGELOG.md`'s `[Unreleased]` section is what keeps the two tellable apart. |
| D105 | Whether the outbox's thirty-day purge stays | 2026-08-05 | **Stays.** Owner-answered at M45. It was never asked for by an [M26](docs/build-notes/phase-details/m26.md) bullet — a worker added it, the orchestrator named it strikeable, and nobody decided. Kept because the alternative is the one table in the schema growing forever with nothing watching it, and because it matches the thirty-day link purge the same reaper runs. Not made a setting: a knob for a table nobody has complained about is a knob to document, test and get wrong. F52 now depends on this path, since abandoned mail leaves by it. |

### Not in Phase 2

Every row below carries the reason it was deferred, and that reason lives here
and nowhere else. What this list does *not* say is which of them belong together:
[phase-3-candidates.md](docs/build-notes/phase-3-candidates.md) groups the ones
Phase 3 might take by **work area**, cut by which files a milestone would touch,
so more than one can be worked at a time or a blocked one has somewhere to fall
back to (owner directive, 2026-08-06). It schedules nothing and restates nothing —
a row there is a pointer back to this list plus an area.

- MFA, OAuth, OIDC, SSO, SCIM — Phase 3 by the scope table. **Partially
  scheduled 2026-08-06: MFA is [M53](docs/build-notes/phase-details/m53.md),
  TOTP only.** OAuth, OIDC, SSO and SCIM stay on this row and stay unscheduled —
  each is a separate credential model, and the row is amended rather than
  removed so their deferral keeps its reason.
- **An API key that reaches more than one organization.** Owner-directed on
  2026-08-05, after [F75](docs/build-notes/deferred-findings.md): a key should be
  minted by an *account* and reach the organizations that account belongs to,
  the way a GitHub personal access token does, rather than being issued into one
  organization. **Validated as the right answer to F75 and deliberately not
  built here.** It dissolves that row instead of patching it — revoking and
  listing would both be scoped to the owner, so the two statements agree by
  construction rather than by adding a filter that makes a key unrevokable from
  the organization somebody is signed into.

  What makes it Phase 3 work rather than a phase-close fix is that it **reverses
  recorded decisions rather than extending them**, and each one has a test and a
  reason behind it. [F103](docs/build-notes/deferred-findings.md) bounds a key's
  reads to one organization precisely because the workspace switcher exists to
  cross organizations and a key was issued for one. M44 spent an
  `organization_id` parameter on `ResolveWorkspaceForUser` so an
  organization-wide key cannot land in a tenant it was never issued for. D43
  caps the role a key-issued invitation may carry, and D87 makes rotation refuse
  a session, both of which are reasoned about one tenancy at a time. `api_keys`
  carries a non-null `organization_id`, and the audit trail records which
  organization a key acted in.

  So the work is: decide what a key's scopes mean when the owner's role differs
  between organizations (the intersection is currently taken against one role),
  decide whether a key may act in an organization the owner joined *after* the
  key was minted, re-derive D43's cap and D87's rotation rule against a
  multi-tenant credential, migrate the column, and re-open F103's bound. None of
  that is a defect being fixed; it is a tenancy model being changed, and doing it
  inside a phase close would cross both the scope contract and the phase
  boundary.

  Until it is built, F75's asymmetry stands as described in that row: revoking is
  owner-scoped and listing is owner-and-organization-scoped, which leaves a
  `204`-versus-`404` existence oracle against key ids a caller would have to
  guess. **F75 stays open** and points here, because scheduled elsewhere is not
  resolved.

  **Scheduled 2026-08-06 as [M54](docs/build-notes/phase-details/m54.md)**, which
  is where the five pieces of work this paragraph enumerates are answered one at
  a time.
- **Account deletion and erasure, of any kind, for anybody.** There is no way to
  delete an account, and no subject-erasure routine. `users` appears in none of
  the schema's fourteen `DELETE` statements, nothing writes `users.deleted_at`,
  and `users.anonymized_at` — a column carrying the comment *"set by the GDPR
  erasure routine"* since the first migration — has no writer at all.
  `destination_disputes` holds addresses too and by explicit design has no
  foreign keys and no purge. What *is* reclaimable is reclaimable: `click_events`
  and `visitors` are dropped by partition at `ANALYTICS_RETENTION_DAYS`,
  `audit_logs` is governed by its own retention setting, and invitations and
  notifications cascade from the organization or the user. **The cost is not the
  absence, it is that four other places described erasure in the present tense**
  ([F44](docs/build-notes/deferred-findings.md)) — two migration comments, the
  audit actor snapshot, and `docs/SECURITY.md` — so a compliance reader met a
  feature that was never built. Those sentences are corrected; the feature is
  recorded here rather than scheduled, and F44 stays open, because documented is
  not resolved. **Scheduled 2026-08-06 as
  [M52](docs/build-notes/phase-details/m52.md)**, which also gives `deleted_at`
  and `anonymized_at` their first writers and states in writing why `suspended`
  still has none.
- **Account recovery, of any kind, for anybody.** A forgotten password cannot be
  recovered by the person who forgot it. There is no *forgot password* route on
  any surface, no reset table, column, token or query, and no mail template for
  one — so **configuring a mailer does not change this**: the mechanism does not
  exist to be enabled, and the mailer is what everybody assumes stands in for it.
  The only password write in the product is `POST /account/password`, behind a
  session, which serves somebody who can already sign in. That leaves the
  operator, and the operator's route is editing the database and rewriting an
  argon2 hash on somebody's behalf. **One case has a command instead**, and only
  one: `lctl instance principal move` hands the instance principal to another
  account (D98, [F140](docs/build-notes/deferred-findings.md)) — which repairs
  *who administers the box*, not the password that was lost. Recorded here rather
  than scheduled, and [F141](docs/build-notes/deferred-findings.md) stays open
  for it: every part needed to build it already exists — a token-hash pattern
  used twice, an outbox, a mailer, argon2 rehashing — so the absence reads as a
  gap nobody wrote down rather than a decision anybody took, and writing it down
  is what this entry is. **Scheduled 2026-08-06 as
  [M51](docs/build-notes/phase-details/m51.md)**, first of the phase's identity
  work, and a hard prerequisite of [M53](docs/build-notes/phase-details/m53.md)
  because a second factor multiplies the lockout this row describes.
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
  **Settled for webhooks and left unexercised**: [M42](docs/build-notes/phase-details/m42.md)
  put delivery on Postgres — `webhook_deliveries`, claimed with `FOR UPDATE SKIP
  LOCKED` under the existing leader lock, the same shape the mail outbox already
  proved. Nothing in the tree is written against Streams and nothing would have to
  be undone to move later; the point is that Redis stays a cache, and a queue that
  lives in the cache is a queue a `FLUSHALL` loses. The recorder comment is trued
  up in M45.
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
  every stranger who signed up could move the toggle. Restoring it needed an
  instance-level principal this product did not have, and inventing one inside a
  signup milestone is what D38 declined to do. M45 introduced one (D98) for the
  three findings that needed it, with its scopes enumerated rather than implied —
  so this toggle is still parked, and un-parking it is a decision about widening
  that set rather than a consequence of the principal existing.

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
  **Campaign analytics stays here after [M41](docs/build-notes/phase-details/m41.md)
  shipped the campaigns themselves**, and the reason is load rather than scope: a
  per-campaign rollup is a new pass over `click_events` grouped by a column that
  is null on most links, stacked on the job M37 has just been rewritten to fit
  its interval. That fix proves itself at scale before anything is built on top
  of it. The links list filtered by campaign is what the product answers with
  meanwhile.
- **A PNG QR code.** [D11](#phase-2-decisions) is SVG only, and M41 built it that
  way: no image encoder is in the dependency set and nothing rasterises on a
  request. Adding a PNG download later is additive — one endpoint and one
  encoder — rather than a rework. **Scheduled 2026-08-06 as
  [M49](docs/build-notes/phase-details/m49.md), which reverses D11** and takes
  the additive path this row describes: `image/png` is standard library, so no
  module dependency joins the set, and the rasteriser that now runs on a request
  is bounded by a stated output cap.
- **More than one QR code per link, and per-code scan counts.** `qr_codes` holds
  one style row per link (02700's unique index), and `?src=qr` carries no code
  identity, so two printed codes for one link are indistinguishable in the
  analytics. Both follow from [D73](#phase-2-decisions-taken-after-the-plan-was-finalised)
  and [D76](#phase-2-decisions-taken-after-the-plan-was-finalised); telling them
  apart needs a per-code token in the payload, which is a design decision rather
  than a column. **Scheduled 2026-08-06 as
  [M50](docs/build-notes/phase-details/m50.md)**, which takes that decision: the
  identity travels in its own query parameter beside `src=qr` rather than inside
  the closed vocabulary, and per-code counts are read from `click_events` — D73's
  refusal of a counter column on the 20ms path is **not** reversed.

---

## Phase 3 build plan

**Seventeen milestones, M46–M58, continuing Phase 2's numbering.** Fourteen of
work, two adversarial reviews (`X.9`, as reserved), one close. The size target is
**fifteen** — *a phase stays under sixteen, insertions counted*, set by the owner
on 2026-08-06 and recorded in
[planning.md](docs/build-notes/planning.md#the-size-target-a-phase-stays-under-sixteen-milestones).
This phase is over it, knowingly and twice; the paragraph below is the record.
Phase 2 ran 33.

**The target moved twice on 2026-08-07, both times knowingly.** The phase was
planned at fifteen with every slot spent. It went to sixteen when the owner chose
to have both QR logos and M50 rather than trade one for the other, and to
seventeen when a review found the logo milestone was two — an upload surface
([M50.5](docs/build-notes/phase-details/m50.5.md)) and the compositing
([M50.6](docs/build-notes/phase-details/m50.6.md)) — and the owner split it
rather than keep one fat milestone. That is the phase-boundary conversation
[planning.md](docs/build-notes/planning.md#the-size-target-a-phase-stays-under-sixteen-milestones)
requires rather than a rule being broken — the target is owner-set and
explicitly revisitable, and the alternatives were priced before it moved.
**The standing rule is unchanged at fifteen**; Phase 3 is a recorded exception
to it, not a new ceiling for every phase after.

Four work areas, chosen 2026-08-06 (D108), cut by which files a milestone would
touch so a blocked milestone has an independent one to fall back to under
[W33](docs/build-notes/workflow-changes.md#made):

| Area | Milestones | |
| --- | --- | --- |
| **B** — Dashboard UI and UX | M46–M48 | Asked for first, and the surface every other area rebases onto |
| **F** — QR codes | M49–M50.6 | Overlaps B on the settings vocabulary, so it is an ordered run behind B rather than an independent area |
| **A** — Identity and account lifecycle | M51–M54 | Carries the two findings that make claims the tree makes today false |
| **E** — Infrastructure and resilience | M55–M57 | The only area with no edge into any other |

Ordering is B first, deliberately: a redesign landed after the milestones that
build inside it is a retrofit of everything they produced. The cost is that the
phase's first three milestones are the least specified, which is why the
walkthrough that specifies them is planning's first input (D112).

| # | Milestone | Depends on | Discharges |
| --- | --- | --- | --- |
| [M46](docs/build-notes/phase-details/m46.md) | The shell, the navigation, and the links list | — | The *workspace selector* candidate row · owner-requested scope, 2026-08-06 |
| [M47](docs/build-notes/phase-details/m47.md) | The link page, taken apart | M46 | The *"massive mess"* complaint |
| [M48](docs/build-notes/phase-details/m48.md) | On-demand panels, and what stops being buried | M47 | The *"buried deep in the page"* complaint |
| [M49](docs/build-notes/phase-details/m49.md) | QR codes sized in pixels, and a PNG to download | M48 *(ordering)* | *A PNG QR code* · the QR-vocabulary complaint · reverses D11 |
| [M50](docs/build-notes/phase-details/m50.md) | More than one QR code per link, told apart in the analytics | M49 | *More than one QR code per link, and per-code scan counts* |
| [M50.5](docs/build-notes/phase-details/m50.5.md) | The first file this product accepts | M50 | — *(owner-added scope, 2026-08-07)* |
| [M50.6](docs/build-notes/phase-details/m50.6.md) | A logo in the middle of a QR code | M50.5 | — *(owner-added scope, 2026-08-07)* |
| [M51](docs/build-notes/phase-details/m51.md) | Account recovery: a forgotten password stops being permanent | — *(after M48, ordering)* | F141 · *Account recovery, of any kind, for anybody* |
| [M51.9](docs/build-notes/phase-details/m51.9.md) | **Mid-phase adversarial review** | M46–M51 | — |
| [M52](docs/build-notes/phase-details/m52.md) | Account deletion and subject erasure | M50.5 · M51 *(ordering)* | F44 · *Account deletion and erasure* · compliance (erasure limb) |
| [M53](docs/build-notes/phase-details/m53.md) | A second factor: TOTP, enrolment, and recovery codes | M51 | The MFA limb of *MFA, OAuth, OIDC, SSO, SCIM* |
| [M54](docs/build-notes/phase-details/m54.md) | An API key belongs to an account, not to one organization | M52 | F75 · *An API key that reaches more than one organization* |
| [M55](docs/build-notes/phase-details/m55.md) | An update checker, and the fifth thing that leaves this product | — | — *(owner-added scope, 2026-08-06)* |
| [M56](docs/build-notes/phase-details/m56.md) | High availability: the failover contract | — | *Other surfaces* (high availability) |
| [M57](docs/build-notes/phase-details/m57.md) | High availability: measured, and still one container | M56 | *Other surfaces* (complete) · the single-instance constraint |
| [M57.9](docs/build-notes/phase-details/m57.9.md) | **Pre-release adversarial review** | M46–M57 | — |
| [M58](docs/build-notes/phase-details/m58.md) | Deferred findings, documentation pass, 0.3.0 | all | Phase close |

**Status per milestone lives in
[phase-details/README.md](docs/build-notes/phase-details/README.md) and nowhere
else**, which is the lesson F37 taught: a dated snapshot in this file was
overtaken on the day it was stamped.

### Phase 3 decisions

Taken 2026-08-06, at planning. The *why* for each is in
[decisions.md](docs/build-notes/decisions.md); this table is what was decided.

| # | Decision | Outcome |
| --- | --- | --- |
| D108 | Which work areas Phase 3 takes | **A, B, E and F.** C (analytics), D (redirect path) and G (commercial) stay candidates — not dropped, not re-homed. D was declined partly on cost: every row there owes the `slo.md` k6 measurement, and six rows would mean six runs. |
| D109 | Area A's scope | The two findings that falsify current claims — F44 erasure, F141 recovery — plus **MFA (TOTP only)** and the **multi-organization API key** (F75). OAuth, OIDC, SSO and SCIM stay unscheduled; each is a separate credential model, and one of them would consume the phase. |
| D110 | Area E's scope, and its constraint | The **update checker** and **high availability**. High availability must not come at the cost of single-instance installs — owner-set, and enforced by a conformance test in [M57](docs/build-notes/phase-details/m57.md) rather than by intention. No new required dependency; Postgres stays the only one. |
| D111 | Area F's scope, and D11 reversed | All three candidates, and a fourth added on 2026-08-07 — see D115. **SVG stays the render; PNG is a download.** The user-facing setting becomes an output size in pixels, and *an SVG at that size matches the PNG* is asserted by test rather than assumed — both are generated from one module matrix and one arithmetic. Sizes snap to keep modules whole, and the produced size is reported beside the requested one. |
| D112 | How the redesign is specified | A **live owner walkthrough with blind tasks**, run before any milestone is built. The alternative — the actor that will build it also specifying it, judged by nobody who uses it — is the failure the queue row asked to avoid. The cost is that planning stalls on owner time, which is why A, E and F were planned in parallel with it. |
| D113 | Version at phase end | **0.3.0.** 1.0.0 stays a later phase's promise, on D4's reasoning for 0.2.0. |
| D114 *(count superseded by D115 and D116)* | What the walkthrough changed, and what it did not | Eighteen blind tasks over two rounds (2026-08-06, 2026-08-07) specified [M46](docs/build-notes/phase-details/m46.md)–[M48](docs/build-notes/phase-details/m48.md) and produced **seven defects**, F160–F166, which are fixed at [M58](docs/build-notes/phase-details/m58.md) and cost no redesign slot. **B stays at three and the phase stays at fifteen** — owner-set 2026-08-07. Notification click-through and mark-unread fold into M48, being the same *buried* complaint and needing no schema change. **Folders path-entry, organization switching and API-key scope grouping are deferred to Phase 4** with their reasons, in [phase-3-candidates.md](docs/build-notes/phase-3-candidates.md). |
| D115 | QR logos, and the target moving | **Both** QR logos and [M50](docs/build-notes/phase-details/m50.md), rather than trading one for the other — owner-set 2026-08-07, taking the phase to sixteen. Logos are placed after M50, because a logo is per-code style and landing it first would mean retrofitting. **Split into two by D116 the same day**, so the row reads [M50.5](docs/build-notes/phase-details/m50.5.md) and [M50.6](docs/build-notes/phase-details/m50.6.md). **It is the first time this product accepts a file**, which is what makes it a milestone rather than a QR parameter: an upload surface, untrusted image decoding, a storage decision bounded by [M57](docs/build-notes/phase-details/m57.md)'s no-new-dependency test, a `docs/SECURITY.md` row, and an erasure limb [M52](docs/build-notes/phase-details/m52.md) does not currently have. |
| D116 | The logo milestone is two, and the target moves again | **Split**, owner-set 2026-08-07 after an independent review, taking the phase to **seventeen**. [M50.5](docs/build-notes/phase-details/m50.5.md) is the upload surface — endpoint, caps, re-encode, storage, removal and orphan collection, the `docs/SECURITY.md` row, and teaching the contract test multipart, which it has never done. [M50.6](docs/build-notes/phase-details/m50.6.md) is the compositing — level H, the occlusion cap, SVG/PNG parity. The seam is that M50.5 is useful with no logo ever drawn. Rejected: keeping one milestone and dropping its decode test, and paying for the split by dropping M50. The standing target stays **fifteen**; this phase is over it twice, both times recorded. |
| D117 | What the workspace switcher offers | **The workspaces you can move to, and not the one you are in.** Blind task 9 asked for the current workspace to be removed *and* could not determine which workspace it was; the selected option was the only place the answer appeared, so the two asks were incompatible until [M46](docs/build-notes/phase-details/m46.md) added a current-workspace label to the header. The select's first option is a selected, disabled placeholder — with the current entry gone, the alternative is a control displaying some other workspace's name. M25's guard is untouched: below two memberships there is still no control, and what closes that case is the label. |
| D118 | The top-level destination set, re-derived | **Two: Dashboard and Links.** API keys moved into the identity menu on the reasoning D35 already used for the team surfaces and M39, M42 and M43 used for Domains, Webhooks and Automation — a top-level slot is for where work is done, and a key is minted once. Blind task 7's first click for API keys went to the identity menu, which is the evidence D35 asked a later milestone to bring. F6 and F7's outcome is **amended, not reopened**, and `TestTopLevelNavHoldsThreeDestinations` was renamed and updated rather than deleted. |
| D119 | Which filters stay hot on the links list | **Search, and only search.** The owner asked for "1-2 hot controls" plus one control holding the rest; the criterion [M46](docs/build-notes/phase-details/m46.md) set is what the blind tasks reached for, and search is the only one of the six named in any of eighteen task notes. The second slot is left empty rather than guessed at. Search is not repeated inside the panel — two controls named `search` in one form would submit two values for one query parameter — and the panel opens by itself, server-rendered, whenever a hidden filter is set. |
| D120 | Where the link page's analytics go | **Below the configuration, on the same page** — not behind a tab and not on their own route. Both alternatives cost what [M47](docs/build-notes/phase-details/m47.md) promised not to spend: a tab needs either a query parameter this milestone said would not change or the product's first piece of client-side view state, and a route splits one object across two URLs one milestone before [M48](docs/build-notes/phase-details/m48.md) starts putting things back on this page as panels. **The three statistic tiles went down with the rest**: a tile is analytics, and a summary strip at the top would leave the reader behind exactly what they were behind. |
| D121 | The order of the link page's eight sections | **Edit, QR code, routing rules, split test, signed links, analytics, recent activity, danger zone.** Rows 1 and 2 are measured — ~35s to change a destination, ~26s to reach the QR code, from the round-one blind tasks. **No task reached for rows 3 to 8 by name**, so they are ranked by a stated secondary criterion instead of by a preference dressed as frequency: how close the section is to the question the edit form answers. [M51.9](docs/build-notes/phase-details/m51.9.md) re-runs the tasks against the built tree, which is the check on all of it. |
| D122 | The line cap on `pages/link_detail.html` | **60 lines**, against 805 before the decomposition and 50 after it. Fixed by the *shortest partial's body* — 21 lines — rather than by the page's own length, so any cap below 71 refuses even the smallest section being pasted back and 60 refuses it by eleven while leaving room for three more sections. Stated in bodies because the first attempt counted whole files and the sabotage run landed at 72 against a cap of 70, which is luck rather than enforcement. Bounds this page and no other. |
| D123 | What an on-demand panel is | **A route first, a popup second.** The contents are served at their own URL and render as an ordinary page; the popup is the Popover API applied to the same markup, so nothing is fetched and nothing is scripted. Rejected: htmx-loading the contents into an overlay, which cannot be opened without a script the CSP forbids; and a `<details>` in flow, which D120 chose on the links page for a panel that pushes content down rather than covering it — these cover, so D24's reasoning applies instead and neither decision is reversed. Two callers in [M48](docs/build-notes/phase-details/m48.md), and a test compares the two rendered panels' geometry so a third surface cannot invent its own. |
| D124 | Where the QR thumbnail sits, and where it does not | **Overruled by D126 — the owner ruled on 2026-08-07 that the picture goes in the heading row and M47's guard narrows to let it. Kept because the argument was a real one, correctly escalated, and decided the other way.** As it stood: **in the QR section, with a worded invoker in the page's heading row** — not above the edit form. The bullet asks for a small code in the *upper region*, and the heading row is the upper region, but M47's `TestTheEditControlIsReachableWithoutScrolling` refuses any `<svg>`, `<img>` or table drawn before the destination box because a picture's height cannot be read off the markup. Breaking it would reverse the measured half of D121 one milestone after it landed. So the invoker up there is a word — which the test allows, and which costs seven of its 400-character budget — and the picture is in the section D121 put second. The retrieval path is re-measured at [M51.9](docs/build-notes/phase-details/m51.9.md) rather than asserted here. |
| D125 | Where a notification leads | **One function from `kind` plus `data` to a URL, enumerated in a map.** A map rather than a switch, because "has a mapping" has to be a question code can ask: a `default:` arm answers it for kinds nobody thought about. Two kinds resolve to *nowhere* — `audit.growth`, which has no dashboard page, and `dispute.decided` upheld, whose recipient is a filer who cannot read the review queue — and the surfaces draw no control for those rather than linking to the list the reader is already on. A test reads the vocabulary out of the source with `go/ast` instead of listing it, so a kind added in a later phase fails the build. |
| D126 | What M47's fold guard refuses, now that a picture goes above the destination box | **A height class and a pixel budget, not a blanket refusal of `<svg>`.** The owner overruled D124 and required a rule rather than an exemption. Every `<svg>` before `id="url"` must carry a Tailwind height utility naming a fixed length — `h-full`, `h-screen`, `h-auto` and arbitrary values are refused as heights nothing in the markup states — and the declared heights together must stay inside 160px. The `height` attribute could not be what is read: `internal/qr` sizes the drawing from the encoded version, so it is 111px for a short URL and 123px for the demo's host, while `ui.QRThumbClass` is 96px for every link. The other eight tags are untouched. Re-measured with the thumbnail in place, in three engines: the destination box moved 327→349px and the alias's bottom edge 443→465px, leaving 335px of viewport. |

### Not in Phase 3

Deferred **out of** Phase 3 rather than never considered. Everything the phase
declined at planning time is in
[phase-3-candidates.md](docs/build-notes/phase-3-candidates.md) with its area;
this list is the shorter one — what the owner's walkthrough asked for and the
fifteen-milestone target could not pay for. Each row carries the reason it was
deferred, and that reason lives here and nowhere else.

- **Filing a link into a folder by typing its path.** Creating a folder and
  assigning links to it are separate actions in separate places, which the owner
  called *"a hassle"* on 2026-08-07 and asked to become one control: type
  `Products/Docs`, see matches along the path as you type, and create only when
  the search returns nothing. It is a real workflow complaint from using the
  product, and it is **the only area round two called irritating with no defect
  behind it** — so it is deferred knowingly rather than because it turned out to
  be a bug somebody else was already fixing. It needs a search surface over the
  folder tree that does not exist, which is why it is a milestone rather than a
  control.
- **Switching organizations anywhere except the workspace dropdown.** Round two:
  *"the workspace dropdown includes Org but I see no way of switching orgs
  outside of it."* [M46](docs/build-notes/phase-details/m46.md) names the
  current organization in the header, which closes the *where am I* half; moving
  between organizations stays where it is. The switcher is the only affordance,
  and giving organizations one of their own is a navigation question that
  belongs with whatever Phase 4 decides about the destination set.
- **Moving links between workspaces.** The *Link management* scope table put it
  at Phase 3, and that table is the authoritative one — *"where this table and
  prose elsewhere disagree, this table wins"*. Phase 3 does not build it: it
  appeared in no milestone, no candidate row and no deferral until a review
  found it on 2026-08-07, which made it the one row in this plan that was
  effectively dropped in silence. Deferred to Phase 4 by the owner the same day.

  Two reasons it is not a cheap insertion. It is a **cross-workspace** capability
  in a service layer where `actor.WorkspaceID` is a single value threaded
  through every scoped query, which is the same hard part the *All Workspaces*
  entry in [upcoming-decisions.md](docs/build-notes/upcoming-decisions.md)
  names — and that entry's recommended option is to build the two together, so
  deferring this strands that question for another phase. And a link carries
  analytics, folders, campaigns and a QR row that are all workspace-scoped, so
  *moving* one is a question about what follows it, not an `UPDATE`.
- **Grouping API-key scopes by the object they act on.** Round two: the scope
  list is *"hard to find things in and should be better organized/grouped by use
  > name > action"*, with the worked example that organizations, workspaces and
  domains belong together and links and members do not. The keys page came back
  **fine** overall and this is the one control on it that did not; it is a
  presentation change over a permission set that is stable, so deferring it
  costs nothing that compounds.

**Deferring these was the owner's choice on 2026-08-07 and the recommendation
was the other way** — trading [M50](docs/build-notes/phase-details/m50.md)'s
slot for a fourth redesign milestone. Recorded so the alternative is visible:
these three are cheap to schedule if a slot ever frees, and folders is the one
that will be missed.

---

## Known limitations

Deliberately accepted, and **not only in Phase 1** — most of the rows below rest
on Phase 2 decisions and were added as those milestones landed. The caption said
"in Phase 1" until 0.2.0, which made a reader date every row here to a phase that
produced a minority of them (F37).

| Limitation | Consequence |
| --- | --- |
| DNS rebinding not defended against | A host resolving public at creation and private at click time is not caught. Detection needs resolution on the hot path. |
| Invalidation needs Redis to cross replicas | Invalidations are broadcast on Redis pub/sub. With Redis down each replica falls back to `REDIRECT_TTL` staleness, which is correct but slower to converge; a reconnecting subscriber flushes its in-process tiers because pub/sub cannot replay what it missed. A Redis that accepts and then stalls is bounded rather than waited on: an edit spends at most `REDIS_INVALIDATE_BUDGET` (250ms) on the cache before committing anyway and logging the staleness (D26, [M26.6](docs/build-notes/phase-details/m26.6.md)). |
| Rate limits are shared only while Redis is reachable | The credential and API limits are enforced in Redis, so they hold across replicas. **On any Redis error each replica falls back to its own in-memory bucket, and the configured limit then applies per replica — N replicas allow N times it** until Redis returns. A restart also resets the local buckets. The 404-probe limiter is deliberately never shared: a Redis round trip on the redirect path would put an optional dependency on the 20ms budget. |
| Rate limits fail open | A full key table allows requests rather than refusing them, counted by `linkctrl_rate_limit_overflow_total`. A limiter is abuse mitigation, not an authorization boundary. |
| Behind a proxy, limits need `TRUSTED_PROXIES` | Otherwise every request carries the proxy's address and all traffic shares one bucket. This is a correctness requirement once a limit is on, not only an analytics one. **[M34](docs/build-notes/phase-details/m34.md) widened what it reaches**: a routing rule's country, region and city conditions and its returning-visitor condition are all derived from the client address, so on a misconfigured proxy every visitor resolves to the proxy's location and to one visitor identity — a geographic rule then matches everybody or nobody, and a returning-visitor rule matches everybody after the first request. Nothing fails; the rules quietly route on the wrong thing. |
| `links.click_count` is approximate | Written with the click rows, but an unclean shutdown loses at most one batch of both. |
| `api_keys.last_used_at` is approximate | Buffered and flushed on a 30s cadence, so an unclean shutdown loses the most recent timestamps. Authentication must not cost a write. |
| A member cannot manage their own rank, or a peer's | [M28](docs/build-notes/phase-details/m28.md) bounds management to ranks strictly below your own (D30), so an admin cannot demote themselves and cannot touch another admin — both need an owner. On a single-owner instance whose owner is unavailable, the admins cannot be changed at all, and there is no self-service way to leave an organization. |
| A workspace-scoped role cannot narrow anybody | Permissions are the union of every matching membership (D31), so *org admin, viewer in one workspace* is unexpressible. Granting a role in a workspace only ever adds to what somebody already holds. |
| A workspace-scoped role reaches no further than its workspace | D44 authorizes each write against the membership whose scope covers its target, so a workspace-scoped admin manages that workspace's memberships and cannot re-role an organization-wide one, grant themselves a second workspace, reach the organization's invitations at all — issuing, listing and revoking alike — or delete the organization — nor can a workspace-scoped owner, who owns one workspace and not the organization. The cost is that widening somebody's reach still needs an organization-wide member: there is no way to delegate *organization* administration without an organization-wide membership. |
| Emptying a workspace is one link at a time | D32 refuses to delete a workspace holding any link, archived ones included, and Phase 2 has neither bulk delete nor a cross-workspace move. Flagged by the owner as worth revisiting; bulk delete, a link move and archive-then-cascade are three separate features with three separate arguments. |
| Emptying an organization is one link at a time too | D37 refuses to delete an organization holding any link in any of its workspaces, archived ones included, for the reason D32 refuses it one level up — an org-level cascade would make the workspace rule bypassable. With no bulk delete, a large organization has no practical path that does not involve SQL. Replaces *nothing deletes an organization*, which [M28.5](docs/build-notes/phase-details/m28.5.md) ended. |
| An organization's audit trail is readable only from inside it | The records survive the organization they describe — `audit_logs.organization_id` carries no foreign key — but `GET /api/v1/audit` is scoped to the caller's own organization and nobody can be in a deleted one. So a deleted organization's trail is intact in the table and reachable only with database access. |
| An account belonging to no organization can do exactly one thing | D36 lets deletion orphan people rather than refuse on their behalf. The account survives and signs in, but every dashboard route redirects to the page offering a new organization until it has one — including *Account*, so a password cannot be changed from that state. |
| A dispute queue crosses organizations, and one named person holds the key to it | The low-confidence blocklist is instance-wide (M30), so M31's queue and its decisions are too. [M45](docs/build-notes/phase-details/m45.md) closed F15 by introducing the instance-level principal D38 said did not exist (D98): `destinations.review` and `destinations.decide` are held **instance-wide by named people and by no organization role**, conferred on the account that claimed the instance and delegable by it alone. What remains is that the reach is real and concentrated — that account, and anybody it appoints, reads every dispute filed on the instance including the address of whoever filed it, and lifts entries for everybody. Reading is delegable to an API key; deciding is not. Losing that account is the operator's to repair and nobody else's: `lctl instance principal move` hands it to another account from a shell, on the same filesystem-access trust `/setup` rests on, and it can only ever move — exactly one account holds the principal afterwards, checked before the change commits, so the delegation bound survives the repair. Finding F140. |
| Allowing a destination cannot lift a refusal no row produced | An `allow` deletes one row from `blocked_destinations` and there is no row anybody may add that permits a destination — 01500 has no allow column on purpose. So a `low_confidence.punycode_homograph` or `low_confidence.url_credentials` refusal can be filed, read and upheld, but not allowed: the queue says so and offers only *Uphold*. Overruling one of those is a change to the rule, which is a code change. |
| Only a refusal with a blocklist row behind it is bounded to one open dispute | [M45](docs/build-notes/phase-details/m45.md) keys the "already waiting for review" bound on the entry the refusal matched, so every subdomain of one listed host is one dispute and one notification instead of one each. A refusal *computed from the URL* has no entry to count, so it is bounded by the host as typed — and `low_confidence.url_credentials` fires on credentials before the host with the host itself ignored, which means distinct strings are distinct open disputes, over a browser route with no rate limiter. Narrowing the rule would change what the destination validator refuses, so that repair is a decision rather than a correction. What M45 *did* fix is the multiplier: a filing notified every organization owner on the instance, a set that grew with every registration, and it now notifies the holders of `destinations.review` — the instance principal and whoever it appointed ([D98](#phase-2-decisions-taken-after-the-plan-was-finalised)). So an unbounded number of filings is an unbounded queue for the people whose job the queue is, and not an inbox flood for everybody who ever signed up. Finding F137. |
| A destination stored by an earlier build keeps the spelling it was stored with | **Both** folds a destination host gets run when it is **written** — [D46](#phase-2-decisions-taken-after-the-plan-was-finalised)'s trailing-dot trim and [D93](#phase-2-decisions-taken-after-the-plan-was-finalised)'s UTS-46 mapping, one after the other inside `canonicalHost`, which for a destination is reached from `ValidateDestination` and nowhere else — and already-accepted links are never re-checked, which is *Not in Phase 2* above and predates both. So the residue is two spellings rather than one. `169。254。169。254` in ideographic full stops was accepted because 0.1.0 carried no mapping at all; `169.254.169.254.` was accepted because `netip.ParseAddr` refuses the dotted form while `looksNumeric` read the empty last label as evidence of a name. A link created before the fold that holds either one still holds it and still serves it, and a visitor's browser resolves both to the cloud metadata endpoint — **a stored open redirect rather than SSRF**, since nothing in this product fetches a destination server-side ([F26](docs/build-notes/deferred-findings.md)). What it takes to hold one is correspondingly narrow, and is worth stating rather than waving at: an instance created before the fold **and** then given such a host. No instance this project can account for holds one, because **no server has ever been built from 0.1.0** — but that tag and its release are public, so this is a statement about what is accountable here and not a claim about the world. Editing a link converts it. There is no sweep, and building one is the separate job that row names — a command to walk the affected population would walk an empty one, which is why [F132](docs/build-notes/deferred-findings.md) is closed as declined rather than built. |
| Changing the signup mode needs the operator | `LINKCTRL_SIGNUP_MODE` is the mode and nothing in the running instance moves it (D38), so letting somebody in means an `.env` edit and a restart — or an invitation, which needs neither. The toggle that would have avoided that was built and removed: at the time this product had no instance-level principal, so *owner-only* would have meant every account that signed up on an open instance. M45 introduced one (D98) and deliberately did not give it this scope — its scopes are enumerated, and adding one is a decision rather than a consequence. [M29](docs/build-notes/phase-details/m29.md); recorded in [docs/SECURITY.md](docs/SECURITY.md). |
| A verification link cannot be re-sent, only re-issued | Only the token's hash is stored, so the emailed link exists once. Registering the same address again supersedes the outstanding one and mails a new link, which is the recovery path; there is no "resend" that reuses the old token. |
| An invitation cannot be re-sent, only re-issued | Only the token's hash is stored, so the link exists once, in the response that created it. Losing it means revoking the invitation and issuing another — the same trade an API key makes, for the same reason. |
| An API key can replace itself, and nothing else | `apikeys.*` is still not delegable, so minting an unrelated key and revoking one both need a session — that has not changed and is what makes revocation mean anything. What [M44](docs/build-notes/phase-details/m44.md) adds under [D87](#phase-2-decisions-taken-after-the-plan-was-finalised) is the one exception that is not one: a key rotates **itself**, into a successor with identical-or-narrower scopes and the same workspace binding, once, with both secrets verifying for a bounded window (five minutes to a day, an hour by default) after which the predecessor is refused on every request and then auto-revoked. **The accepted trade is that a leaked key can persist across rotations**, because whoever holds the secret can rotate it: revoking the key its owner knows about may leave an intruder holding a successor under a prefix nobody issued. Four things bound it and one now removes it: every generation is in the owner's key list, every rotation writes an `apikey.rotated` audit record naming both prefixes, each window is capped — and somebody holding `apikeys.write` across the organization can revoke a successor by the id that record names, which is the difference between seeing the chain and ending it. A key whose owner has left the organization no longer authenticates at all, so the chain cannot outlive the membership behind it either. `last_used_at` is flushed on a 30-second cadence, so a predecessor that reads as idle may have been used up to 30 seconds ago; the window's floor is ten times that for exactly this reason. |
| A human blocked as a bot has no recourse | [M32.5](docs/build-notes/phase-details/m32.5.md) decides with `analytics.Classify`, which treats an absent user agent as automated and matches substrings including `preview`, `monitor` and `checker`. Its false-positive rate has never been measured, because until bot blocking existed nothing depended on it. A person it misjudges gets a 403 with no challenge and no appeal, and the link's owner is not told. The mitigation available without the bypass is the default: `inherit`, resolving to off, so the cost sits with whoever switches it on. The bypass is Phase 3, per *Not in Phase 2*. |
| Blocking is per link and per domain, and nothing between | The instance default domain's setting is instance-wide, like its root redirect and the low-confidence blocklist, so whoever enforces it decides for every workspace on the box. **Who that is narrowed in 0.2.0**: F70 recorded that it was the shape `domains.write` had — a role permission, so every organization's owner and admin — and [D100](#phase-2-decisions-taken-after-the-plan-was-finalised) moved both settings to `domains.write.instance`, which reaches a person only through the instance principal D98 built. What is unchanged is the *granularity*: there is still no per-workspace level between the link and the domain, and a workspace serving its links on its own registered hostname administers that hostname's policy rather than this one. There is also no way to see how many refusals a setting is producing beyond `linkctrl_redirects_total{outcome="blocked_bot"}` and the bot column of a link's own statistics. |
| A domain-level blocking change sweeps the Redis keyspace | The redirect path reads the domain's policy from each link's cached snapshot, which is what keeps the decision free of I/O — so changing it invalidates every one of those snapshots. Each replica clears its own memory tier by prefix, and whoever made the change walks Redis with `SCAN`/`UNLINK` inside a five-second budget. On an instance with a very large keyspace the walk can exhaust that budget, which is logged: the affected links then keep applying the previous policy until `REDIRECT_TTL` expires them. It runs only when somebody submits the form, and never on the redirect path. |
| A routing rule is only as good as what the instance can resolve | [M34](docs/build-notes/phase-details/m34.md)'s geographic conditions need `LINKCTRL_GEOIP_MMDB_PATH`, and region and city need a *City* database rather than the Country one — without either, those conditions never match and the visitor falls through to the link's own destination. The returning-visitor condition needs Redis; without it every visitor reads as new. Neither is an error and neither is announced at request time: the rule form says so where somebody writes one, and the redirect path degrades rather than refusing. The failure mode this leaves is a rule that quietly never fires on an instance whose operator has not supplied the file. |
| A gated link costs a write, and only a gated link does | [M35](docs/build-notes/phase-details/m35.md)'s one-time and max-click gates consume a durable Postgres counter on every redirect, because `links.click_count` is approximate and Redis cannot be trusted to hold a budget. So a click-limited link pays a synchronous write per visit where an ordinary link pays none, and the ceiling on how fast such a link can be followed is Postgres rather than the cache. Measured separately in [docs/slo.md](docs/slo.md) rather than folded into the cached figure, because the two are different paths. |
| A link password is remembered nowhere, so it is typed every time | D53's CSRF waiver rests on the verification POST issuing nothing to the browser — no cookie, no unlock token, no session. The consequence is the visitor's: every visit to a password link is another challenge, and there is no "remember this" because building one would void the reasoning the amendment was signed off under. |
| A password link's rate limit can be held empty by a stranger | [D54](#phase-2-decisions-taken-after-the-plan-was-finalised) keys the limit on the **alias** as well as the address, deliberately: guesses driven through many visitors' browsers spread across as many addresses as there are visitors and would never trip a per-address bucket. The cost D54 stated was fail-open when Redis is down; the one it did not is this — anybody may empty a link's alias bucket with wrong guesses and lock out its intended audience, at roughly one request every three seconds against the shipped `LINK_PASSWORD_RATE_LIMIT` of 20. No fix is available that keeps the guarantee: dropping the alias limb, or keying it on address-plus-alias, reopens the distributed-guessing hole D54 exists to close, and D53's CSRF waiver rests on D54 holding. **A correct password refunds the alias token since 0.2.0** ([F115](docs/build-notes/deferred-findings.md)), which removes the other half — a link with more legitimate visitors than its limit used to throttle its own audience with no attacker present — and leaves this one. |
| Guessing a link password is limited best-effort, not bounded | `LINKCTRL_LINK_PASSWORD_RATE_LIMIT` is enforced per address *and* per alias through the shared Redis limiter (D54). **On any Redis error each replica falls back to its own in-memory bucket**, so N replicas allow N times the configured number until Redis returns, and a restart resets the local buckets. That is the same trade every shared limit makes, and it is the reason a link password should be treated as a speed bump rather than as an access control. |
| A signed URL is revoked by clearing a column, and not before the caches expire | Signatures expire and there is no revocation endpoint: [M35](docs/build-notes/phase-details/m35.md) built expiry, not rotation. Invalidating every outstanding signature for a workspace means setting `workspaces.signing_secret` to NULL by hand, and each replica keeps honouring the old key until its in-process copy expires — up to `gate.DefaultSecretTTL`, one minute. Anybody holding a signed URL can follow the link until then. |
| Raising a click ceiling re-opens a spent link | `link_click_budget.consumed` is monotonic and is compared against the link's *current* limit, so a link that answered 410 at five clicks starts redirecting again the moment the ceiling moves to six. That is what somebody raising a limit is asking for; there is no way to say "five more from now" without also resetting the counter, which nothing exposes. |
| A sequential split costs a write, and only a sequential split does | [M36](docs/build-notes/phase-details/m36.md) keeps the rotation in the same durable Postgres counter M35's click budget uses, because D8 chose strict global order and an in-process counter gives each replica a rotation of its own. So a link with sequential arms pays a synchronous write per visit — measured at a 3.1ms median against 87µs for a weighted split on the same run ([docs/slo.md](docs/slo.md)) — and 0.495% of those requests exceeded the 20ms budget. A weighted split costs nothing measurable, and a link with no split is unchanged. |
| A split test does not remember a visitor | [M36](docs/build-notes/phase-details/m36.md) chooses an arm per request, so the same person following a link twice may see two arms and a conversion cannot be traced back to the arm that first showed it. That is what a link-level test is here: each click is an independent trial, attribution comes from `click_events.destination_id`, and the alternative is a cookie this product refuses to set or a per-visitor lookup on the redirect path. Somebody who needs per-visitor consistency does not have it and cannot build it from what is exposed. |
| A sequential rotation advances even when the request is then refused | The arm is chosen where the destination is decided, which is before the deep-link join and before the gates — so a visitor who gets a 404 for an unforwardable path, or submits a wrong password, has still spent a position. The distribution is unaffected, because whether a gate passes is independent of which arm came up; what is affected is the claim "the *n*th visitor saw the *n*th arm", which holds for requests that reached destination selection rather than for requests that were served. **The password challenge is not one of these** (F87, M45): it is a visit arriving in two parts rather than a refusal, so a request about to be challenged chooses no arm, and one visit advances the rotation once. |
| Removing a split arm leaves its clicks pointing at nothing | `click_events.destination_id` carries no foreign key — the highest-write table in the system is partitioned and a reference would put a lookup on every COPY row — so deleting an arm leaves rows naming an id that no longer resolves. The breakdown reports them as *a destination that no longer exists* rather than dropping them, which is deliberate: a running test's totals must not change because somebody tidied up. There is no way to relabel them afterwards. |
| A link's split arms are all one kind, and a kind cannot be changed | "40% of visitors, in rotation" has no meaning, so [M36](docs/build-notes/phase-details/m36.md) refuses the mix outright rather than inventing a precedence rule for a state nobody meant to create — and it offers no way to convert a running weighted test into a sequential one, because that changes what the test's own history means. Switching costs removing every arm and adding them again, which loses the association between the old clicks and the new arms. |
| A custom domain is served only while its DNS record is there | [M40](docs/build-notes/phase-details/m40.md) verifies a hostname by reading back a TXT record, and re-reads it hourly. A hostname whose record disappears keeps serving for `DOMAIN_VERIFY_GRACE` — one day by default — with the owning workspace notified at the first failure, and then stops being served on every replica. There is no setting that makes a hostname serve forever on the strength of a check that once passed, and `DOMAIN_VERIFY_INTERVAL=0` switches re-verification off entirely rather than making the window longer. Renaming a hostname un-verifies it, and a rename that lands while a check is in flight makes that check verify nothing. A pass checks serving hostnames before registered-but-unserved ones, and a workspace registers **at most 25** — both bound what can delay the stop, since the pass is the only thing that ever performs one ([D92](#phase-2-decisions-taken-after-the-plan-was-finalised)). |
| LinkCtrl obtains no certificates | TLS for a custom hostname is the operator's reverse proxy's, per [D3](#phase-2-decisions). The application never speaks ACME: it answers Caddy's on-demand `ask` at `/tls-check` for verified hostnames only, and `ssl_status` says whether that ask will be answered rather than anything about the certificate. A proxy that cannot ask needs each custom hostname configured by hand. |
| A hostname cannot be shared, and its owner is a workspace | Alias uniqueness is per domain, so one hostname is one alias namespace: exactly one workspace owns it (D68) and a second registration of the same name is refused instance-wide, whoever asks. There is no way to give two workspaces the same hostname and no way to hand one over — removing it and registering it again is the whole of a transfer. **And the claim never lapses**: nothing reaps a registration that fails its DNS check or is never attempted, so a name registered and abandoned is held until its registrant deletes the row, which only they can do ([F113](docs/build-notes/deferred-findings.md)). A reaper is not the fix it looks like — verification deliberately keeps polling a hostname registered *before* its DNS cut-over, which is the case a reaper would delete, and it could not read a NULL check timestamp as abandonment because a rename writes one onto a live row. |
| A returning visitor is a visitor from earlier today, and nothing longer | D2's within-day semantics are the whole answer rather than an approximation of a durable one, and the day ends at midnight UTC. A visitor from yesterday is new again, because a durable answer needs a cookie or a per-visitor identifier kept across days and this product keeps neither — the salt that keys the day's hashes is deleted on a schedule, which is what makes them unlinkable. Stated in the rule form, the API document and `docs/configuration.md` rather than left to be discovered. |
| The city lookup cost is measured against a synthetic database | M34 requires city-level rule conditions and requires their mmap lookup cost to be measured rather than assumed. There is no GeoLite2-City on this project's machines and one cannot be committed, so the figure in [docs/slo.md](docs/slo.md) was taken against `internal/geoip/testdata/city-test.mmdb` — 33,000 synthetic networks over documentation, reserved and private ranges, 469,455 nodes, 2.8MB. The real file is roughly twenty times that and does not fit in a CPU cache, so the number is a **representative floor**, not a reading of what an operator will deploy. D48 chose this over the alternatives and this row is the residue it refused to pretend away. |
| A QR scan is a click labelled `qr`, and nothing more precise | [M41](docs/build-notes/phase-details/m41.md) attributes scans through the existing `referrer` breakdown ([D76](#phase-2-decisions-taken-after-the-plan-was-finalised)): a code encodes `<short url>?src=qr` and the value lands in `referrer_host`. So a scan is countable, and three things are not. **A scan and a click on a pasted copy of the same URL are indistinguishable** — anybody can type `?src=qr` — which is why the value is a label rather than evidence. **Two codes for one link cannot be told apart**, because the parameter carries no code identity and `qr_codes` holds one style row per link. And **the vocabulary is closed to one value**: `?src=` anything else attributes nothing, deliberately, because `link_dimension_daily` keys on the value and an open parameter is unbounded write amplification anybody can trigger. Adding a second source is a code change with a test asserting the size. |
| A campaign has no analytics of its own | [M41](docs/build-notes/phase-details/m41.md) ships the label, the CRUD and the filter, and no aggregate: there is no per-campaign click series, no rollup table and no `campaign` dimension. Campaign rollups would stack a new pass on the job [M37](docs/build-notes/phase-details/m37.md) has just fixed, and that fix is to prove itself at scale first — so *Not in Phase 2* keeps it. What the product answers with is the links list filtered by campaign, which is exact and per link rather than aggregated. |
| A campaign's dates describe the work and enforce nothing | `starts_at` and `ends_at` are read by no redirect: a link in a campaign that ended yesterday redirects exactly as it did the day before. Expiry is a property of the link, and a second weaker one on the campaign would give two answers to "why did this stop" — and would put a second table on the redirect path to find out. The campaigns page says so beside the schedule column. |
| A webhook delivery is abandoned after an hour, and nothing recovers it | [M42](docs/build-notes/phase-details/m42.md) retries seven times across 61 minutes and then marks the delivery `abandoned`. There is no replay endpoint and no dead-letter drain: a receiver that was down for longer than the window has lost those events, and the delivery log is where it finds out which. Retrying forever was the alternative, and it means a dead endpoint is dialled on every tick until a human notices. |
| A webhook that has stopped working is invisible outside its own log | Nothing a workspace can otherwise see changes when deliveries fail, so `/webhooks` carries the delivery log and there is no notification, no bell entry and no email. `linkctrl_webhook_deliveries_total{outcome="abandoned"}` is the operator's signal; the workspace's is the page. |
| The webhook address check is only as good as the network it dials from | Delivery refuses a socket to a private, loopback or link-local address, checked at connect rather than at registration — which closes DNS rebinding *for this path*. Behind an egress proxy that resolves names itself, every address is the proxy's and the check says nothing, so the control has to be in the proxy. Stated in `docs/SECURITY.md` under operator responsibilities. |
| An API key cannot create a webhook | `webhooks.write` is in `NonDelegableScopes` (D18's second limb): a webhook keeps delivering after the credential that created it is revoked, so registering one takes a signed-in person. Reading webhooks and their delivery log is delegable, so an integration can watch itself. |
| The delivery queue is instance-wide, arrival-ordered, and one tenant can fill it | `ListPendingDeliveries` selects `status='pending' AND next_attempt_at <= now()` with no workspace term, twenty rows on a thirty-second tick — forty a minute for the whole instance. A workspace may hold twenty subscriptions, so one link write there produces twenty rows and two link writes a minute saturate the instance; nothing caps pending rows, and `WEBHOOK_RETENTION_DAYS` prunes only finished ones. Nothing is lost and nothing is disclosed — another tenant's deliveries arrive late, behind everything queued before them. The queue file argues the missing tenant term was deliberate, because a drainer that filtered by tenant would deliver in tenant order; that is true, and arrival order starves the same way. [F90](docs/build-notes/deferred-findings.md) is **carried out of Phase 2 deliberately**, because every mechanism that fixes it is a design choice rather than a repair: `DISTINCT ON (workspace_id)` cannot sit under `FOR UPDATE SKIP LOCKED`, and a per-workspace pending cap silently drops events this project has already said nothing recovers. |
| Analytics drops under overload | Bounded queue; drops counted and alertable. Backpressure would slow redirects. |
| A dimension breakdown can be a quarter of an hour behind the totals beside it | [M37](docs/build-notes/phase-details/m37.md) discharged *the dimension rollup grows with traffic* by taking the split-cadence option: the breakdowns recompute every 15 minutes while the per-link and per-workspace totals stay on 60 seconds. The cost is a real one and it is on the page — a link's country, device and per-destination breakdowns can lag its click count by up to fifteen minutes, and nothing on the page says which of the two you are looking at. `linkctrl_rollup_staleness_seconds` is what makes the lag observable, with an alert recipe in [docs/operations.md](docs/operations.md#alerts-worth-having). Nothing about the query got cheaper: it is 4.8-6.3s per run at 5.7M events, 289k upserts, and the recorded fallback if that stops fitting 15 minutes is to narrow the recomputed window. Measured in [docs/slo.md](docs/slo.md#re-measured-for-m37-2026-08-03). |
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
