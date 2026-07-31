# LinkCtrl — Project Plan

Scope contract and specification. States **what** is true, not why.

| | |
| --- | --- |
| Rationale for every decision | `docs/build-notes/decisions.md` |
| Investigations | `docs/adr/` |
| Dev environment | `docs/build-notes/development.md` |
| How the work is done | `docs/build-notes/workflow.md` |
| Security model and reporting | `docs/build-notes/SECURITY.md` |
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
| Rate limiting: shared across replicas | 2 |
| Abuse prevention: scheme allowlist, private-IP block, reserved/profanity alias filters, 404 probe limiting | 1 |
| `domains.write` permission, for settings that affect every link at once | 1 |
| Per-domain ownership, so a workspace administers its own hostname | 2 |
| Audit log — table only | 1 |
| Audit log — behavior | 2 |
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

Logging a blocked attempt is what finally gives `audit_logs` a reason to have
behavior, which is why the two rows sit together. `ip_prefix` rather than an
address, matching the rest of the privacy stance, and the attempted URL is stored
as evidence and treated as hostile input everywhere it is displayed.

Sequenced within Phase 2: blocking and logging first, which are useful on their
own; disputes and review after, since an appeal path is meaningless before
anything is being refused.

### Collaboration

| Capability | Phase |
| --- | --- |
| Roles and permissions with evaluator | 1 |
| One auto-provisioned personal org + workspace per user | 1 |
| Organizations: sharing, invites, team management | 2 |
| Self-serve signup, switchable at runtime by an owner | 2 |
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

As of 2026-07-31. 21 of 21 milestones, all of them in 0.1.0, tagged `v0.1.0` on
`main`.

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
three are done; their definitions of done are kept because they are what the
implementations are held to.

| Milestone | State |
| --- | --- |
| **M18 — separate management and link hostnames** | **Done.** `APP_BASE_URL` and `LINK_BASE_URL` both default to `BASE_URL`, so an existing single-host deployment is unaffected; set to different hosts, the router dispatches on `Host` and each tree answers only its own paths. A wrong-host request is `404`, never a cross-host redirect. `short_url` is built from the link origin, the CSRF trusted origin follows the dashboard host, and `/healthz` and `/readyz` answer on every hostname including ones never configured, because probes do not know the operator's names. Reserved aliases stay enforced on both hosts. |
| **M19 — post-release defect fixes, and a demo seeder** | **Done.** Effective status is derived rather than stored, so an expired link reports as expired everywhere and `?status=expired` matches it. `visitors` and `is_first_visit` are documented as dormant instead of described as working, and stay under partition maintenance and retention so the day something writes to them the guarantees already apply. The deletion notice says what recovery is. `lctl demo` / `make demo` fills an instance with a workspace worth looking at. |
| **M20 — root redirect on the link domain** | **Done.** Every requirement below holds, verified live and under test. |

**M20 in detail.** M18 gave the instance a second public hostname and left its
root a bare `404`: `/{alias}` does not match `/`, and the dashboard routes that
used to answer there moved to the other host. Anyone who trimmed a short link
back to the domain, or typed it out of curiosity, got nothing. Every commercial
shortener points that page somewhere, and the operator is the only party who
knows where.

An operator can now set a destination for `https://lnk.example.com/`, and every
row below holds:

| Requirement | Why it is stated |
| --- | --- |
| Stored on the domain row, as an additive column | It is a property of the hostname, not of a workspace or a link. The `domains` table already exists and already has the row this describes. |
| Guarded by a new `domains.write` permission, granted to owner and admin | Phase 1 has no per-domain owner to check: the default domain is instance-wide with a null organization, so "the domain owner" resolves to whoever administers the instance. The permission is the honest form of that, and it becomes a real ownership check in Phase 2 when a workspace can bring its own hostname. |
| Only in effect when the hosts are actually separate | On a single-host deployment `/` is the dashboard, and a root redirect there would take the dashboard away from the person who set it. Refused rather than silently ignored. |
| Destination validated by `ValidateDestination`, unchanged | Same scheme allowlist and same private, loopback and metadata refusals as any other destination. A root redirect that skipped them would be a cleaner SSRF than the one the validator exists to stop, because it needs no link and no alias. |
| `302`, like every other redirect here | Same reason: a `301` cached in browsers and intermediaries cannot be recalled, and this is the destination most likely to be changed later. |
| Unset stays `404` | The current behaviour, and the one that reveals nothing. No default marketing page, no "powered by" — an instance that says nothing is a valid choice. |
| Resolved from cache, never a query per request | It sits on the redirect tree under the same 20ms budget as an alias, and the root of a link domain is a page crawlers and scanners hit often. Cached with invalidation on change, the way a link snapshot already is. |
| Not counted as a click | There is no link, so there is no `link_id` to attribute it to. Root visits stay out of `click_events` rather than acquiring a synthetic link to hang off. |
| Changing it is an audit-log event once the audit log has behavior | It is a setting that redirects every stray visitor to the whole domain, which is exactly the class of change worth being able to ask about afterwards. Phase 2, noted here so it is not rediscovered. |

**M19 in detail.** The three defects, each with what "fixed" means:

| Defect | Fix |
| --- | --- |
| `links.status` is never set to `expired` | Only `active` and `archived` are ever written. The redirect path is correct — it reads `expires_at` and answers `410` — but the management surface reports an expired link as `"status":"active"`, and the UI's *Expired* filter is an option that can never match a row. [operations.md](docs/operations.md#troubleshooting) tells an operator diagnosing an unexpected `410` to check the link's status, which will tell them the link is fine. Fixed by deriving effective status in one place that both the list and the resolver agree with, rather than by adding a job to write a column that is stale the moment it is written. `disabled` is in the same enum and the same position; it is out of scope only because nothing offers to set it either. |
| The `visitors` table is dead | Nothing writes it and nothing reads it, and `click_events.is_first_visit` is the same one column down: always `false`, under a comment claiming the rollup computes it, which no rollup does. The milestone forced a choice; the choice taken was neither of the two it offered. Both stay dormant and both stay under partition maintenance and retention, because the alternative fails in the direction that matters — a table dropped from those lists that later acquires a writer puts its rows in the default partition, which retention never drops, making the dormant table the one place raw visitor data is kept forever. What was actually wrong was the description, so the comments now say dormant instead of describing work that does not happen. |
| The deletion notice promises a button that does not exist | The UI says "Link deleted. It stays restorable for 30 days." [usage.md](docs/usage.md) is straight about the truth — "recovery inside the 30 days is a database operation, not a button" — and `RestoreLink` is guarded by `deleted_at IS NULL`, so it refuses soft-deleted rows by design. Fixed by making the notice say what recovery is. Adding a trash view instead would be a scope change, and Phase 1 already decided against it. |

**The seeder.** `lctl seed` exists and is for load testing: a hundred thousand
links called `ld0`…`ld99999`, no destination rows, random visitor hashes. It is
the wrong shape for looking at the product. `make demo` fills an empty instance
with something worth a screenshot — a workspace of plausible links, thirty days
of history with weekday seasonality and a launch spike, and every status the UI
can render, including an archived link, an expired campaign and one in the trash.
Two requirements make it worth committing rather than keeping as a snippet: links
are created through the same service call the REST API makes, so seeding runs the
same validation and alias policy a client does and cannot invent states the
product cannot reach; and
backfilled click rows match what the ingester would have written column for
column — no IP anywhere, referrers already reduced to a host, `is_first_visit`
false, and the exact device and browser strings `Classify` emits. A seeder that
writes rows the application could not have produced tests nothing and teaches the
reader something false.

#### Deferred findings

Where out-of-spec issues go the moment they are found, instead of being fixed on
the spot or forgotten. Anything discovered while a milestone is in flight that is
not required by that milestone lands here as one row: what, where, the evidence
it is real, and a suspected severity.

**Nothing here is scheduled work.** The owner reviews each row and approves it
individually; approved rows become the phase's final milestone. An unreviewed row
is a report, not a commitment — which is what keeps "I noticed something" from
turning into unplanned scope.

Issues that make the *current* milestone's own claim false are in spec by
definition and get fixed immediately, whatever subsystem they turn up in. The test
is the claim, not the file.

| Finding | Where | Evidence | Severity | Reviewed |
| --- | --- | --- | --- | --- |
| *(empty)* | | | | |

The process around this — when to defer, what counts as work, what has to pass
before a phase PR — is in [workflow.md](docs/build-notes/workflow.md).

#### Accepted, unassigned

None of it blocking a release:

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
