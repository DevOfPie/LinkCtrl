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
| Add-on runtime | `wazero` — WASM, pure Go, no cgo, so the binary stays `CGO_ENABLED=0` |
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
image. **What the UI draws follows the data rather than the setting**, narrowed
at [M58](docs/build-notes/phase-details/m58.md) by
[F160](docs/build-notes/deferred-findings.md#closed) — this sentence read
*without it the UI states that geographic data is unavailable* until then, and
an instance holding resolved country history whose database was removed met that
sentence over rows that were present. A link with countries **in the window on
screen** gets the map and the ranked list either way; the unavailable sentence is
reached only when nothing resolved in that window and nothing can resolve, which
is the one state where it is the whole truth, and it is still that rather than an
empty chart. The bound is the link and the window, which is narrower than
*history*: a link whose countries all fall outside the selected window meets the
sentence until the window is widened
([F195](docs/build-notes/deferred-findings.md#open)).
[D65](#phase-2-decisions-taken-after-the-plan-was-finalised) carries the
reasoning. The country is resolved at ingest, from the address, in
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
| MFA, OAuth, OIDC, SSO, SCIM | **MFA built in 3** ([M53](docs/build-notes/phase-details/m53.md)) — TOTP only, off until `LINKCTRL_MFA_SECRET_KEY` is set. **OIDC is Phase 4's, as a first-party add-on rather than in core** ([M69](docs/build-notes/phase-details/m69.md), D211). OAuth, SSO and SCIM stay unscheduled (D109) |

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
| Add-on support: a WASM host, a published ABI and SDK, an add-on manager | 4 — the phase's spine; see [Phase 4 build plan](#phase-4-build-plan) |
| AI optimization, smart routing, predictive analytics | future — this row read `4` until Phase 4 was planned (2026-08-18, D211); the phase took its *plugin system* limb as the spine and moved the rest out rather than carrying them as quiet scope |
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
- **Multi-replica operation adds no component.** Failover of scheduled work is a
  Postgres advisory lock being released when its holder's session ends, so it is
  the absence of a mechanism rather than one; failover of in-flight work is a
  60-second claim lease on the same Postgres. No coordinator, no external lock
  service, no second Postgres, no session affinity, and a one-container
  deployment is unaffected by all of it ([M56](docs/build-notes/phase-details/m56.md),
  D110). The one thing a kill loses is the in-process click queue, which is the
  *Queue* row above and the reason its upgrade path is named there.
- **A single container is a tested configuration, not merely an unaffected
  one.** The release image is started on a network carrying only Postgres and
  the whole surface is driven over HTTP — redirect, dashboard, API, jobs,
  invalidation, rate limiting — in CI on every push
  ([M57](docs/build-notes/phase-details/m57.md), `scripts/single-instance-check.sh`).
  The required dependency set is Postgres, and a change that adds to it fails
  the build.
- **A rolling deploy of every replica drops nothing, measured rather than
  claimed**: 240,002 requests at 2,000 rps through a load balancer while all
  three replicas were destroyed and rebuilt, zero failed, zero retried, cached
  p99 295µs (M57,
  [docs/slo.md](docs/slo.md#measured-during-a-rolling-deploy-for-m57-2026-08-09)).
  It holds because `SHUTDOWN_DRAIN_DELAY` outlasts the balancer's detection; the
  same run with the drain removed retried 905 of 239,833.
- **Two replicas cannot both lead one job family because of a deploy.** Every
  binary from 0.2.0 on takes the same per-family advisory keys, so a rolling
  deploy has the two contending for one lock rather than holding one each, and
  `TestAReleasedFamilyKeepsItsAdvisoryKey` freezes the released assignments so a
  rename cannot re-open it (M57, D168). The residual window is a leader losing
  its lock connection while still working, which no deploy causes and every pass
  is written to survive.
- **An add-on reaches this product through an enumerated set of imports and
  nothing else.** No socket, no file, no shared table, no environment, so the whole
  of what an add-on can do is one list — **including, from 0.4.0, reaching the
  network, which is a function on that list and not a hole in this sentence**: a
  module still opens nothing itself, and `network.fetch` buys it a host function
  that dials an origin *the operator named in a setting*, with the manifest unable
  to name one and both redirect classes refused outright
  ([M68.5](docs/build-notes/phase-details/m68.5.md), D367). The list is
  `internal/addon/abi`, published as
  [docs/addon-abi.md](docs/addon-abi.md) and as a generated SDK an add-on's own
  repository imports. The host owns the definition, and the host module the runtime
  registers is derived from the same list rather than restating it, so host and
  guest cannot disagree about a signature ([M61](docs/build-notes/phase-details/m61.md)).
  **What a module may call out of that list is what its manifest declared**, against
  a closed nine-token vocabulary the host resolves at load and checks on every call,
  refusing anything else with a distinguishable status and a counter per add-on and
  permission ([M62](docs/build-notes/phase-details/m62.md)). The check sits in the
  host's dispatch rather than in each function, and it runs before the host says
  whether it implements the function at all, so a module that declared nothing
  cannot enumerate what a build can do. Running inside the redirect path is a
  separate declaration, and editing where a visitor is sent is a third one on top
  of it ([M66](docs/build-notes/phase-details/m66.md)) — an inline invocation also
  reaches only a redirect-safe subset of the list, whatever its manifest declared,
  so there is no storage, no request, no session and no template on the hot path. **The four functions that cost nothing
  are not four the host trusts**: writing to the log is ungated on purpose, so
  the host neutralizes what a module wrote, which is where a module that declared
  nothing would otherwise be able to forge a record that reads as this product's own.
  **The neutralization is the logger's and not a rule each call site follows**, since
  that rule was enumerated wrongly twice — most recently missing the line naming a
  migration as it is applied, which another package writes on the path that runs when
  nothing is wrong. The 4 KiB bound is the log line's alone: the aggregated reason a
  manifest was refused reaches an operator whole. What survives that boundary is
  the set of graphic characters, in any script, and what does not is escaped — a
  default-deny, because a list of invisible characters is behind the next Unicode
  revision the day it is written. Graphic characters the host treats as invisible are
  escaped too — 268 of them: Unicode's `Default_Ignorable_Code_Point`, **derived** rather
  than asked of the residue property Go ships under a nearly identical name, plus
  `U+2800 BRAILLE PATTERN BLANK`, which the property does not carry and which is the
  one blank that is not whitespace. **The claim is that property and not *invisible***,
  which is not a property anybody publishes: eight combining marks Unicode annotates as
  not visibly rendered stay outside it, and what bounds them is that an add-on may post
  to the log and cannot read it back. The derivation reaches 398 members the residue property never had,
  and the 260 of them a reader could ever have seen are the variation selectors: a
  module that declared nothing used those to carry a secret out of an
  ordinary-looking line; **they are deleted rather than escaped**, unconditionally and
  with no exemption for the emoji case, because a selector has no appearance of its
  own and no property says which bases a renderer will vary. The one graphic character
  escaped anyway is the backslash, so that a reader can tell a literal `\n` from an
  escaped newline and a module cannot spell the mark on a truncated line; the carve-out
  running the other way is Unicode's prepended concatenation marks, read from the
  property rather than copied out of it, for the same reason the escape set is not a
  list (M62, D240, D241, D242, D243, D244, D283).
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
| The add-on boundary | The stance holds at the **ABI** rather than by reviewing add-on code, which this project cannot do for a module it did not write ([M61](docs/build-notes/phase-details/m61.md)). No host function hands a module a client address in any form, and the record carrying redirect data is bound to what `click_events` may carry — country-level, prefix-derived, with region and city refused although the columns exist. An add-on cannot store what it is never handed, and a test over the ABI surface reads the column list out of the migration rather than trusting a copy of it. |
| Regional storage | One instance per region via `organizations.data_region`; no row-level routing |

Consequence: the largest table holds no personal data and is out of scope for
subject-access and erasure requests.

Unique-visitor counts are estimates at daily resolution. The API returns that
caveat with the data.

---

## Build status

Phases 1, 2 and 3 are released: `v0.1.0` on 2026-07-31 (21 milestones),
`v0.2.0` on 2026-08-06 (33 milestones), `v0.3.0` tagged 2026-08-18 (23
milestones). Status per milestone lives in
[phase-details/](docs/build-notes/phase-details/) and nowhere else: the live
phase in its [README](docs/build-notes/phase-details/README.md), each released
phase in its own `phase-N.md`.

### Phase 1 scope not yet built

Phase 1's per-area completion table, its verification note, and this
section's record of what it deferred are archived at
[phase-details/phase-1.md#build-status-detail](docs/build-notes/phase-details/phase-1.md#build-status-detail).

---

## Phase 2 build plan

The 33-milestone table (M21–M45) and its ordering are archived at
[phase-details/phase-2.md#phase-2-build-plan](docs/build-notes/phase-details/phase-2.md#phase-2-build-plan).

### Phase 2 decisions

The decisions behind Phase 2's plan are archived at
[phase-details/phase-2.md#phase-2-decisions](docs/build-notes/phase-details/phase-2.md#phase-2-decisions).

### Phase 2 decisions taken after the plan was finalised

The decisions taken while Phase 2 was being built are archived at
[phase-details/phase-2.md#phase-2-decisions-taken-after-the-plan-was-finalised](docs/build-notes/phase-details/phase-2.md#phase-2-decisions-taken-after-the-plan-was-finalised).

### Not in Phase 2

The full deferral reasons are archived at
[phase-details/phase-2.md#not-in-phase-2](docs/build-notes/phase-details/phase-2.md#not-in-phase-2).
What of it is still actually parked, tracked by work area, is
[phase-3-candidates.md](docs/build-notes/phase-3-candidates.md)'s [*What Phase
3 shipped, and what is still on this list*](docs/build-notes/phase-3-candidates.md#what-phase-3-shipped-and-what-is-still-on-this-list)
and [phase-4-candidates.md](docs/build-notes/phase-4-candidates.md).

---

## Phase 3 build plan

The 23-milestone table (M46–M58) and its ordering are archived at
[phase-details/phase-3.md#phase-3-build-plan](docs/build-notes/phase-details/phase-3.md#phase-3-build-plan).

### Phase 3 decisions

The decisions behind Phase 3's plan are archived at
[phase-details/phase-3.md#phase-3-decisions](docs/build-notes/phase-details/phase-3.md#phase-3-decisions).

### Not in Phase 3

The full deferral reasons are archived at
[phase-details/phase-3.md#not-in-phase-3](docs/build-notes/phase-details/phase-3.md#not-in-phase-3).
What of it is still actually parked, tracked by work area, is
[phase-3-candidates.md](docs/build-notes/phase-3-candidates.md)'s [*What Phase
3 shipped, and what is still on this list*](docs/build-notes/phase-3-candidates.md#what-phase-3-shipped-and-what-is-still-on-this-list)
and [phase-4-candidates.md](docs/build-notes/phase-4-candidates.md).

---

## Phase 4 build plan

**Fourteen milestones planned, M59–M70, continuing Phase 3's numbering:**
eleven integers of work, two adversarial reviews (`X.9`, as reserved), and one
close. 11 + 2 + 1 = 14. One of the eleven arrived at the plan's own review —
the manager milestone was found to be two, an upload-and-unload lifecycle and
the page that drives it, and was split before anything was built against it
(the [M50.5/M50.6 precedent](docs/build-notes/planning.md#the-size-target-a-phase-stays-under-sixteen-milestones)).
Planned under the size rule as Phase 4's planning
recorded it: **plan to fifteen, cap raised — once, deliberately — to
eighteen** (owner-set 2026-08-18,
[phase-4-candidates.md](docs/build-notes/phase-4-candidates.md#the-phases-shape)).
One planned slot was deliberately unspent: an ABI is the kind of artifact
insertions come from, and [M69](docs/build-notes/phase-details/m69.md) is
designed to surface what the foundation got wrong. **It was spent on
2026-08-23**, before M69 ran, on
[M66.5](docs/build-notes/phase-details/m66.5.md) — measuring what M66's inline
class costs a visitor when nothing is pooled produced a milestone the build
turned out to need, and the owner chose to spend the reserve on it knowing what
the reserve was being held for. The phase is therefore at its planning target
with no slack: an insertion M69 produces is a conversation about the cap of
eighteen, not a slot. **That conversation happened on 2026-08-26**, when M69's
validation produced two insertions before the milestone started —
[M68.5](docs/build-notes/phase-details/m68.5.md) and
[M68.6](docs/build-notes/phase-details/m68.6.md), taking the phase to seventeen
— and the owner chose to proceed with one slot left rather than move the cap or
close the phase early. So the sentence above has been honoured rather than
overtaken: what it promised was a decision, and the decision is
[D366](docs/build-notes/decisions.md#2026-08-26--the-conversation-planmd-promised-and-what-it-decided).
Recorded in
[D333](docs/build-notes/decisions.md#2026-08-23--m665-added-pooling-because-a-well-behaved-add-on-cost-4489ms)
rather than left as a sentence describing a reserve that no longer exists.

The phase's shape, its owner-set answers and their dates are
[phase-4-candidates.md](docs/build-notes/phase-4-candidates.md)'s record and
are not restated here: **add-on support is the spine**, the OIDC add-on —
built in `DevOfPie/LinkCtrl-OIDC`, consuming only the published SDK — is the
foundation's acceptance test, and the redirect promise is rescoped to core in
the same milestone that lets an add-on onto that path.

Ordering is substrates before consumers, in one dependency chain rather than
Phase 3's independent areas: host, then contract, then enforcement, then
capabilities in rising order of what a defect in each would cost — storage,
pages, sessions, the redirect path — then the surfaces that show it and the
consumer that proves it.

| # | Milestone | Depends on | Discharges |
| --- | --- | --- | --- |
| [M59](docs/build-notes/phase-details/m59.md) | Process debt: the gates that were not watching | — | [F248](docs/build-notes/deferred-findings.md#closed) · [F253](docs/build-notes/deferred-findings.md#closed) · [F254](docs/build-notes/deferred-findings.md#closed) · [F255](docs/build-notes/deferred-findings.md#closed) |
| [M60](docs/build-notes/phase-details/m60.md) | The host: a module loads, or is refused | M59 *(ordering)* | Opens the *Add-on support* scope row · owed-work #5 (single-instance gate case) |
| [M61](docs/build-notes/phase-details/m61.md) | The ABI: what an add-on may import, written down and versioned | M60 | Owed-work #2 (deprecation policy) · the host-function question |
| [M62](docs/build-notes/phase-details/m62.md) | Declared permissions: an add-on gets what it named and nothing else | M61 | The enforcement answer · the permission-expression question |
| [M63](docs/build-notes/phase-details/m63.md) | An add-on's tables: a schema of its own, migrated by the host | M62 | The schema-per-add-on answer · the DDL-additiveness collision |
| [M64](docs/build-notes/phase-details/m64.md) | An add-on reaches the page: routes, templates, config | M62 · M63 *(ordering)* | Reach: routes, templates, config |
| [M64.9](docs/build-notes/phase-details/m64.9.md) | **Mid-phase adversarial review** | M59–M64 | — |
| [M65](docs/build-notes/phase-details/m65.md) | The authentication hook: a session minted on an add-on's word | M61 · M62 · M64 | Reach: the session hook, last limb of *everything OIDC needs* |
| [M66](docs/build-notes/phase-details/m66.md) | Add-ons on the redirect path: two classes, a deadline, and a promise rescoped | M60 · M62 | The redirect answer and its three requirements · owed-work #1 (core-only SLO claim) · the deadline question |
| [M66.5](docs/build-notes/phase-details/m66.5.md) | Instances are reused, so a visitor stops paying for a cold start | M66 · M60 *(ordering)* | Owner-added scope 2026-08-23 — reverses D319, which declined pooling before an add-on had been measured under load |
| [M67](docs/build-notes/phase-details/m67.md) | Runtime lifecycle: an add-on arrives and leaves without a reboot | M60 · M62 · M63 · M66.5 | The install/remove halves of the manager answer · split from the surface at the plan's review |
| [M68](docs/build-notes/phase-details/m68.md) | The Add-on manager | M63 · M66 · M67 · M64 *(ordering)* | The manager answer's visible half: listing, per-module performance, orphaned data, the purge choice |
| [M68.5](docs/build-notes/phase-details/m68.5.md) | An add-on reaches outward, and only where the operator pointed it | M61 · M62 · M64 · M68 *(ordering)* | [F334](docs/build-notes/deferred-findings.md#closed) — the gap M69's validation found; owner-answered scope 2026-08-25 |
| [M68.6](docs/build-notes/phase-details/m68.6.md) | A module arrives from a URL, because that was always the intention | M67 · M68.5 · M68 *(ordering)* | Owner-added scope 2026-08-25 — corrects M67's *never a fetch*, which no decision backed |
| [M69](docs/build-notes/phase-details/m69.md) | The OIDC add-on: the foundation's acceptance test | M61 · M63 · M64 · M65 · **M68.5** · M68 *(ordering)* | The OIDC limb of *MFA, OAuth, OIDC, SSO, SCIM* · the acceptance test · owed-work #4 (the add-on repo's LICENSE, checked as a precondition) |
| [M69.9](docs/build-notes/phase-details/m69.9.md) | **Pre-release adversarial review** | everything below it | — |
| [M70](docs/build-notes/phase-details/m70.md) | Deferred findings, documentation pass, 0.4.0 | all | Phase close · owed-work #3 (the 1.0 sentence) |

### Phase 4 decisions

The planning conversation's answers — all owner-set 2026-08-18 — live in
[phase-4-candidates.md](docs/build-notes/phase-4-candidates.md) and receive `D`
numbers as the milestones that rest on them land, per
[upcoming-decisions.md](docs/build-notes/upcoming-decisions.md)'s convention.
The plan itself is
[D211](docs/build-notes/decisions.md#2026-08-18--phase-4-planned-the-spine-and-the-fourteen-slots).

### Not in Phase 4

[phase-4-candidates.md](docs/build-notes/phase-4-candidates.md#what-is-not-in-phase-4)
carries the list and its reasons; nothing is restated here. The two rows this
plan moved are recorded in the scope tables where they sat: AI optimization,
smart routing and predictive analytics left the Phase 4 row above, and OAuth,
SSO and SCIM stay unscheduled while OIDC ships as an add-on.

---

## Known limitations

Deliberately accepted, and **not only in Phase 1** — most of the rows below rest
on Phase 2 decisions and were added as those milestones landed. The caption said
"in Phase 1" until 0.2.0, which made a reader date every row here to a phase that
produced a minority of them (F37).

| Limitation | Consequence |
| --- | --- |
| A default instance still has no account recovery, and says so | [M51](docs/build-notes/phase-details/m51.md) built the mechanism and it is delivered by mail, so `SMTP_HOST` unset means there is no route back into an account whose password was lost. The product **refuses out loud** rather than degrading (D143): the sign-in page draws no link, `/forgot` answers with the reason instead of a form, and the API answers `503 no-mailer`. The operator's route is unchanged and is what the refusal names — setting the hash directly, or `lctl instance principal move` for the one account that administers the box. This is the only consumer of the mailer whose absence is a refusal; every other one has a second channel. |
| Erasure leaves a queued message and an outstanding offer | [M52](docs/build-notes/phase-details/m52.md)'s hourly sweep replaces `audit_logs.actor_label` and both of `destination_disputes`'s label columns with a constant tombstone — **the dispute pair in one statement, since [F187](docs/build-notes/deferred-findings.md#closed) reopened M52 late in 0.3.0**: they were two, so a dispute filed *and* decided by accounts erased in the same batch kept one of its labels, and [D175](#phase-3-decisions) reopened M52 rather than carrying it because the shape predates the sweep's later widening. **[M58](docs/build-notes/phase-details/m58.md) widened it to four more things**, closing [F177](docs/build-notes/deferred-findings.md#closed), [F181](docs/build-notes/deferred-findings.md#closed), [F189](docs/build-notes/deferred-findings.md#closed) and [F188](docs/build-notes/deferred-findings.md#closed): the `"email"` key inside `audit_logs.metadata` — **seven** writers, counted against the tree at M58 where this row said six — the `"from"` **array** beside it, which is the one writer that stored a list where the others store a scalar; the address on the invitations the erased account redeemed, which `/invites` rendered in full on an ordinary dashboard page behind no `audit.read` and no retention window; and the notification the *inviter* received when the account accepted their invitation, in the title as well as the detail, since the title is the field a reader is shown ([D176](#phase-3-decisions)). That last one is a row belonging to a different reader, so removing the erased account's own notifications never came near it and nothing sweeps notifications by age. What is left is deliberate. An **outstanding** invitation addressed to the same text is untouched, because it is an offer to an address rather than a record of a person and the address became reusable the moment the account was deleted. **`mail_outbox.recipient`** survives on messages not yet purged; it is bounded by the mail retention schedule, and on an instance whose relay is down it can outlive the account. The surviving `user_id` is the design rather than a residue — the row is kept so the audit trail stays readable, which is why the claim is that the residue identifies nobody *from inside this instance* rather than that it is anonymous. `docs/SECURITY.md` states all of it where a compliance reader will meet it. |
| Nothing can *require* a second factor, and the key that protects one is not rotatable | [M53](docs/build-notes/phase-details/m53.md) builds TOTP for anybody who wants it, and stops there. There is no organization-level *require MFA for all members* policy: it needs a permission of its own, an enforcement point on every session resolution, and an answer for members who cannot enrol — a policy feature wearing an authentication milestone's clothes, named in m53.md so a later reader knows it was considered. So an administrator who needs a second factor across a team has to ask rather than enforce. Separately, `LINKCTRL_MFA_SECRET_KEY` has no re-encrypting sweep, so changing it has exactly the effect of losing it: every enrolled account falls back to a recovery code, then to the operator. That is the second piece of configuration in this product whose loss destroys something that cannot be recomputed, and it is counted as a new class of operator mistake rather than as a defect — the consequence is bounded and `docs/configuration.md` states the chain beside the variable. |
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
| A human blocked as a bot has no recourse | [M32.5](docs/build-notes/phase-details/m32.5.md) decides with `analytics.Classify`, which treats an absent user agent as automated and matches substrings including `preview`, `monitor` and `checker`. Its false-positive rate has never been measured, because until bot blocking existed nothing depended on it. A person it misjudges gets a 403 with no challenge and no appeal, and the link's owner is not told. The mitigation available without the bypass is the default: `inherit`, resolving to off, so the cost sits with whoever switches it on. **The bypass is not scheduled.** This row read *"The bypass is Phase 3, per Not in Phase 2"* until M58; Phase 3 declined the redirect-path work area outright (D108), so the promise loses its phase number rather than moving to the next one — a limitation with no date is honest where one carrying the next phase's number is a promise nobody made. |
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
| A replica killed without draining loses its buffered click events | The one thing the failover contract does **not** recover, and it is stated as a bound rather than left to be discovered: everything else in flight survives, because scheduled work is leader-elected on an advisory lock that releases when its holder dies and both queues claim under a 60-second lease. The click queue is neither — it is in-process and bounded by decision (D77), so a graceful shutdown flushes it and a `SIGKILL` does not. How much is lost is `linkctrl_analytics_queue_depth` at the moment of the kill. The fix is a durable work queue, which is *Redis Streams as a work queue* — a Phase 3 candidate that was not taken, and taking it would make Redis required and break the constraint in D110. [M56](docs/build-notes/phase-details/m56.md); the contract is in [docs/operations.md](docs/operations.md#what-happens-when-a-replica-dies). |
| A dimension breakdown can be a quarter of an hour behind the totals beside it | [M37](docs/build-notes/phase-details/m37.md) discharged *the dimension rollup grows with traffic* by taking the split-cadence option: the breakdowns recompute every 15 minutes while the per-link and per-workspace totals stay on 60 seconds. The cost is a real one and it is on the page — a link's country, device and per-destination breakdowns can lag its click count by up to fifteen minutes, and nothing on the page says which of the two you are looking at. `linkctrl_rollup_staleness_seconds` is what makes the lag observable, with an alert recipe in [docs/operations.md](docs/operations.md#alerts-worth-having). Nothing about the query got cheaper: it is 4.8-6.3s per run at 5.7M events, 289k upserts, and the recorded fallback if that stops fitting 15 minutes is to narrow the recomputed window. Measured in [docs/slo.md](docs/slo.md#re-measured-for-m37-2026-08-03). |
| The add-on ABI is `0.x`, and one of its seventeen functions still refuses | [M61](docs/build-notes/phase-details/m61.md) publishes the whole contract and implements three functions of it: logging, reading an add-on's own declared settings, and reporting the host's ABI version; [M63](docs/build-notes/phase-details/m63.md) added the two storage functions, [M64](docs/build-notes/phase-details/m64.md) added the request, the response and the session read, and [M65](docs/build-notes/phase-details/m65.md) added the mint, the identity link, the clock and the random source, and [M66](docs/build-notes/phase-details/m66.md) added the redirect limb — the observation read it turned on, and the decision read and answer write an inline module answers through, and [M68.5](docs/build-notes/phase-details/m68.5.md) added the outbound fetch, which is the seventeenth function and the sixteenth live one — so sixteen are live. Template rendering from a module's own files is declared — its name fixed, its signature fixed enough to compile against — and its call answers a refusal until the milestone behind it lands. That is deliberate, because the add-on repository compiles against this boundary from its first commit and cannot wait six milestones for a header file. The cost is stated rather than hidden: **the ABI promises no stability while it is `0.x`**, the signature of a refused function may still move as the behaviour is built — no version number moves with it, and a module built against the older SDK then fails to instantiate rather than misbehaving, which [docs/addon-abi.md](docs/addon-abi.md) states as the rule's one cost — and a generation can be retired on the minimum window — two minor releases and 90 days — and nothing longer. `1.0` is what would mean the contract has settled, and it is a release's statement to make. |
| An add-on's schema needs a database role, and some deployments cannot make one | [M63](docs/build-notes/phase-details/m63.md) confines an add-on to its own Postgres schema with a **login role** rather than with a search path, because a search path is never consulted for a qualified name and confines nothing, and `SET ROLE` on the application's own connection was measured against Postgres 17 and escapes twice — a single `DO` block resetting the role, and `SET SESSION AUTHORIZATION`, which is checked against the session user and so succeeds whenever the application connects as a superuser. So the host creates one role per add-on and opens a connection as it. The cost is a **new operator requirement**: the application's database user needs `CREATEROLE`, and the add-on's role needs to be able to authenticate with a password. A managed Postgres where LinkCtrl connects as a restricted user, or a deployment authenticating by `peer` or a cloud IAM token, cannot satisfy that and such an add-on will not load — explicitly, naming the reason. There is deliberately no fallback, because the only weaker mechanism is not a boundary. |
| An add-on can read the catalogue of tables it cannot read a row of | The confining role still reaches `pg_catalog` and `information_schema`, which Postgres does not make revocable, so a module holding `storage.own_schema` can enumerate every schema, table and column on the instance. It cannot read a byte of one, and the adversarial suite for that boundary reaches for the product's tables eleven ways from inside a guest. What leaks is a schema map — the same map anybody with a clone of this repository has — and closing it would mean a database per add-on or a filtering proxy, which was not worth the operational surface. |
| An add-on's pages are text the dashboard wraps, and it may not ship markup, a stylesheet or an asset | [M64](docs/build-notes/phase-details/m64.md) gives an add-on `/addons/<name>/` on the dashboard host and renders what it answers through this product's own page template, escaped. That is the whole of how it reaches the page: the content types a module may name for itself are `text/plain` and `application/json`, `text/html` is refused, and there is no path by which a module's bytes become markup — which is what keeps the Content-Security-Policy byte-identical to what it was before add-ons could draw anything, and what makes *an add-on cannot inject a script tag* a property of the shape rather than of a filter (D259). The cost is that an add-on's page is plain: no layout of its own, no stylesheet, no font, no image. `template_render` is declared for the shape that would change it and is **still refused**, which is [F283](docs/build-notes/deferred-findings.md#open) — a contract question rather than a defect, since the milestone that backs the function answered its purpose a different way. Serving no add-on asset is also the answer to a narrower question M24.5 would otherwise have left open: its template scan walks the *embedded* templates, so a stylesheet an add-on shipped would be the first CSS this product serves unscanned, and none is served (D264). Those pages are also reachable **without signing in**, because an add-on that authenticates somebody is answering a request from a person who has no session yet (D261), and a module holding the routes grant can redirect a visitor anywhere — never permanently, which the host enforces. Sixteen add-on invocations run at once across the instance, each bounded at 8 MiB of guest memory, because a request gets an instance of the module to itself (D260, D288) — and since M66 a redirect-path invocation and an out-of-band observation draw on the same sixteen — which is also why a module cannot keep a flow's state in memory between two requests and has to keep it in the schema it owns. Since [M66.5](docs/build-notes/phase-details/m66.5.md) a redirect-path instance is **kept** afterwards rather than destroyed, because building one cost the visitor 11.05ms on a path whose target is 20ms (D335); what stops reuse handing one visitor's residue to the next is that the host restores the module's memory to what its package initialization left, so the sentence above holds by enforcement rather than by the instance being gone. Eight kept warm is what `LINKCTRL_ADDON_POOL_SIZE` bounds. The three bounds add into the 192 MiB ceiling `docs/deployment.md` sizes a host by; until M64 was reopened only the first existed, and the 2.4 MB a fixture measures was being quoted as though it were the second. A module's cookies are bounded the same way and for the same reason (D287): the host carries the whole set inside one cookie of its own, so an add-on occupies two slots of a visitor's cookie store however many cookies it sets and however often it is visited, and cannot fill that store until the browser evicts this product's session cookie. |
| Nothing caps an add-on's schema, and removing an add-on does not remove its data | The same answer the audit log gets, for the same reason: `linkctrl_addon_schema_bytes{addon}` makes the *stored* growth visible and there is no quota, so an add-on that writes a row per redirect is a disk the operator agreed to when they installed it. **That gauge summed a list of relation kinds until D254** — ordinary and materialized tables — so a **sequence** in the add-on's own schema was 8192 bytes it reported as nothing, 24,000 of them being 188 MB of `pg_database_size` under a gauge reading 0; and the host's own migration table in that schema carries an identity column, so the shortfall was never conditional on an add-on misbehaving. It now excludes the kinds already counted inside another relation rather than listing the kinds that count, which is the same argument the confinement post-condition below makes and is why the two are stated together. A schema is not everything a confined role can fill — it can create a **large object**, which belongs to no schema and which that gauge cannot see, so `linkctrl_addon_large_objects{addon}` publishes the count, an add-on owning one is refused at its next load, and the purge in [docs/operations.md](docs/operations.md) carries the `DROP OWNED BY` that removes it; closing the capability outright needs ownership of a `pg_catalog` function and was measured to be a silent no-op for the database user this product documents (D248). A **temporary table** is the same shape and is narrowed rather than only accounted for: installing a storage add-on revokes `TEMPORARY` on the database from `PUBLIC` and grants it back to the application, which refuses every spelling of it — but only where the application owns the database, and no dump carries it, so what actually holds is the load's post-condition, which since D251 asks Postgres what the role owns instead of checking a list of the places it might have used. The cost is that another application sharing this database loses temporary tables, stated in [docs/deployment.md](docs/deployment.md). A third narrowing joins those two and is the only one in the family conditional on neither superuser nor database ownership: the confining role may set any user-settable parameter on its own role — `work_mem = '4GB'` accepted, inherited by every connection the add-on's pool opens afterwards — so every load clears the role's settings before re-pinning its search path (D253) — **in every database, which took a reopening**: `RESET ALL` clears the cluster-wide defaults and not the per-database rows the same role writes with `ALTER ROLE … IN DATABASE`, those survived every load for a phase, and scoping a second reset to the current database is evaded by naming another one, so the load reads the databases out of `pg_db_role_setting` instead of naming any (F288, D279) — **per add-on and at that add-on's load only**, because nothing sweeps roles no add-on claims: a name beginning `addon_` is not evidence this product created the role, and the membership row that looked like evidence is written automatically for a `CREATEROLE` creator, measured (D282). What that leaves is a setting parked on the role of an add-on since uninstalled, which no load will clear and which is inert because only a login reads one and nothing logs in as that role once the module is gone; [docs/operations.md](docs/operations.md) carries the statement an operator can type. **What the post-condition and the gauges cover is every kind of object Postgres catalogues, which is not every way an add-on can use disk**: a `WITH HOLD` cursor holds a temporary *file* for the life of its session, in neither catalogue and under no gauge, transient rather than stored, and bounded only by a `temp_file_limit` a superuser must set — [F279](docs/build-notes/deferred-findings.md#open). Deleting a module's directory leaves its schema, its tables and its rows, and so does removing it through [M67](docs/build-notes/phase-details/m67.md)'s API — which names the orphan in its own answer, so the choice is offered at the moment somebody made it rather than discovered later; the next boot enumerates `addon_*` schemas nothing claims and warns, its size gauges stop rather than freezing at their last reading, and nothing deletes one — a purge is an operator's explicit act, and since [M68](docs/build-notes/phase-details/m68.md) the Add-on manager is where it is offered: every orphaned schema is a row on `/instance/addons` with its size measured at the moment of the prompt rather than read from the gauge, and the purge itself is `DROP SCHEMA … CASCADE` audited as `addon.data_purged`. **The drop is the schema and nothing else**, which the confirmation states rather than leaving to be found: the `addon_<name>` login role stays with its password, so re-installing under that name works as it did and so this operation does not depend on the role owning nothing; any large objects that role owns are outside every schema and survive, which is what the `DROP OWNED BY` in [docs/operations.md](docs/operations.md) is still for; any `addon_identity_links` rows written under that name stay, which is [F330](docs/build-notes/deferred-findings.md#open); and the **stored settings** an operator typed on the manager's own detail page stay too, keyed on the name the same way and deleted by nothing in this product, which is [F332](docs/build-notes/deferred-findings.md#open). **Four things, not three** — the count moved when M68 added the fourth, and the confirmation, the API document and this row say four together or the enumeration is decoration. Backup and restore beyond what `pg_dump` of a schema already is was not built, stated in [docs/data-model.md](docs/data-model.md) — with one requirement that is not optional: `pg_dump` carries **no roles**, so a backup of an instance with a storage add-on is two files and the roles are restored first, or the add-on's tables come back owned by the application and the add-on is refused on its own rows. [docs/deployment.md](docs/deployment.md) carries both commands and the order. |
| A bad add-on release can hold a `required` instance down | Host-run migrations are third-party DDL, applied before the listener opens, and M60's class rule says a `required` add-on that will not load stops the instance. Both are the design — the alternative is an instance serving with an authentication provider's tables missing — and together they mean an operator whose configuration did not change can be held down by somebody else's release. The manifest digests bound *what* runs to what the add-on's author published, and the schema boundary bounds where it can reach; neither bounds whether it works. [docs/operations.md](docs/operations.md#add-ons) has the recovery order. |
| The host offers cross-add-on data access no vocabulary at all, and an add-on can still give its own data away | Nothing this product offers lets two add-ons share a table: there is no permission for it, no host function, and no way to ask. Deliberate rather than unfinished ([M63](docs/build-notes/phase-details/m63.md)): two add-ons that want to share data are one add-on, and anything else is a decision nobody has needed yet. **What that does not mean is that the reach is impossible**, and D255 corrected four documents that said it was: an add-on owns its schema, so `GRANT USAGE ON SCHEMA … TO PUBLIC` and a `GRANT SELECT` beside it are two ordinary statements through the write path, after which another add-on — whose role name `pg_roles` will tell it — reads and writes those tables. Measured on Postgres 17.10. The harm is bounded to what the granting add-on chose to give, which is its own data, so the answer is a load-time narrowing rather than a redesign: the load post-condition reads `pg_namespace.nspacl` and `pg_class.relacl` and refuses an add-on that has granted anything on its schema to any grantee but its own role, until an operator revokes. The DDL-additiveness answer rests on that — *the only reader is one the add-on created itself*, which is the add-on author's own act and visible to the host — rather than on a reader being impossible. |
| Installing an add-on at runtime reaches one replica, and needs the add-ons directory writable | [M67](docs/build-notes/phase-details/m67.md) makes arrival and departure runtime acts: a module is uploaded, verified, started and removed without restarting the instance, under `addons.manage` — an instance-level scope held by the account that administers the box and delegable to no API key, because a key that could install an add-on would carry whatever that add-on's manifest declares, past every scope the key was issued with. **The add-ons directory stays the only store**, so the boot-directory route and the API are one lifecycle with one answer to what is installed (D338); a second store in Postgres would have reached every replica and was refused because the first time the two disagreed, the disagreement would be a module running that nothing lists. What that costs is three things, each a property of where the directory is mounted rather than a defect waiting on a fix: an install reaches **the replica that served the request** and no other; a container filesystem that is not a volume loses it on the next deploy; and `:ro` — which [docs/configuration.md](docs/configuration.md) still recommends for an instance whose add-ons are placed by hand — refuses an install with a `503` that says so. Two narrower bounds sit beside them. An add-on whose manifest declares `.sql` **migration files** ships files that are not part of the upload, so it is refused with a message naming the other route rather than failing on a missing path ([F328](docs/build-notes/deferred-findings.md#open)) — an add-on that owns a schema and creates its tables from its own code installs fine. And there is **no upgrade-in-place**: `rename(2)` onto a non-empty directory fails, which is what keeps activation to a single atomic step, so replacing an add-on is a removal and an install. Removal completes the invocations already inside the module, bounded at five seconds and interrupting past it (D340), and takes the directory out of the discovery set *before* anything is unloaded — which is what makes removing a `required` add-on unable to leave an instance that will not start. |
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
