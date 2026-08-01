# Decision log

Why LinkCtrl is built the way it is. `Plan.md` states *what* is true; this file
states *why*, so the spec stays terse and the reasoning is still recoverable.

Entries are append-only and dated. A later entry may correct an earlier one;
the earlier text is left in place with a pointer rather than edited away.

Longer investigations live in `../adr/`.

## Index

Navigation only — adding to it is not editing an entry. Newest last, matching the
file. Append a row when you append an entry.

| Entry | Covers |
| --- | --- |
| [Phase 1 planning](#2026-07-29--phase-1-planning) | Stack choices, tenancy model, 302-only, Redis as pure cache |
| [Decisions made while building](#2026-07-30--decisions-made-while-building) | Schema, partitioning, auth, RBAC, redirect hot path, analytics ingest |
| [Dashboard (M11)](#2026-07-30--dashboard-m11) | HTMX, template structure, CSP, no-Node stylesheet |
| [OpenAPI contract (M12)](#2026-07-30--openapi-contract-m12) | Hand-maintained spec, contract test, `/api/v2` for breaking changes |
| [Metrics (M13)](#2026-07-30--metrics-m13) | Prometheus conventions, cardinality rules, second listener |
| [Documentation (M14)](#2026-07-30--documentation-m14) | What each document is for, and what it may claim |
| [Planning: the enforcement milestone (M15)](#2026-07-30--planning-the-enforcement-milestone-m15) | Why every config variable must take effect or be removed |
| [Enforcement (M15)](#2026-07-30--enforcement-m15) | Rate limits, 404 probes, GeoIP, retention, `config.Removed` |
| [Load validation (M16)](#2026-07-30--load-validation-m16) | How the redirect SLO is defined and measured |
| [Release packaging (M17)](#2026-07-30--release-packaging-m17) | Tag-driven releases, version stamping, image and binary artifacts |
| [The Phase 1 completeness review](#2026-07-30--the-phase-1-completeness-review-and-what-it-found) | The 30 confirmed findings and the shape they shared |
| [Planning: signup and host separation (M18, M19)](#2026-07-30--planning-signup-and-host-separation-m18-m19) | The signup ceiling rule; why open signup admits tenants |
| [Signup deferred to Phase 2](#2026-07-30--signup-deferred-to-phase-2-and-two-milestones-added) | Why invitations and a mailer must precede the toggle |
| [Malicious destination blocking, specified](#2026-07-30--malicious-destination-blocking-specified-rather-than-named) | The three tiers, the unappealable rule, the review queue as attack surface |
| [Build-notes, a security policy, and the process](#2026-07-30--build-notes-a-security-policy-and-the-process-written-down) | Why workflow.md exists and what SECURITY.md promises |
| [M18: two hostnames, one listener](#2026-07-30--m18-two-hostnames-one-listener) | Host dispatch, no cross-host redirect, unknown hosts get ops only |
| [M19: three defects, and the seeder](#2026-07-30--m19-three-defects-and-the-seeder-that-found-them) | Derived status, dormant `visitors`, honest deletion notice |
| [Planning: M20, root redirect](#2026-07-30--planning-m20-a-redirect-for-the-root-of-the-link-domain) | Why the link domain's root needs an operator-set destination |
| [M20 built, and 0.1.0 absorbs everything](#2026-07-30--m20-built-and-010-absorbs-everything) | Why no `[Unreleased]` section survived into the first release |
| [0.1.0 tagged](#2026-07-31--010-tagged) | Why the tag sits on `main`; docs made true before tagging |
| [Phase 2 planned](#2026-07-31--phase-2-planned) | The doc split, two review milestones, and the seventeen Phase 2 decisions |
| [Two development instances](#2026-07-31--two-development-instances-and-the-link-gates-third-failure) | Why demo and test are separate stacks, what guards them, and the link gate's SIGPIPE flake |
| [Dark mode added as M24.5](#2026-07-31--dark-mode-added-to-the-plan-as-m245) | Post-finalisation scope; why it lands before the UI run; the CSP case for a server-rendered cookie |
| [Feature intake, and reviews at X.9](#2026-07-31--feature-intake-written-down-and-the-review-slot-moves-to-x9) | planning.md exists; why reviews cap the fractional band at .9 |
| [docs/ reorganized](#2026-07-31--docs-reorganized-around-its-reader) | Root is for running and using; SECURITY.md surfaced; the two recorded keep-decisions |
| [The phase loop](#2026-07-31--the-phase-loop-written-down) | Unattended milestone iteration; why validation precedes each one; why the resume note is untracked |
| [Six decisions taken ahead of the run](#2026-07-31--six-decisions-taken-ahead-of-an-unattended-run) | Delegability as a rule (D18); growth alert on by default (D19); reconnect flush (D20); light theme may move (D21); last-used workspace (D22); mail outbox (D23) |
| [M21, the audit log gets behavior](#2026-07-31--m21-the-audit-log-gets-behavior) | Why the writer reduces the address itself; `audit.read` under D18; retention as a per-table policy; why the growth metric is job-measured |
| [M22, the inbox and what it is not](#2026-07-31--m22-the-inbox-and-what-it-is-not) | The fence against a preferences system; why the warning defaults on where retention defaults off; the silence window; the badge query |
| [M23, invalidation that crosses replicas](#2026-07-31--m23-invalidation-that-crosses-replicas) | Why a reconnect must flush; why the publish does not wait; the black-hole proxy and a test that measured nothing |
| [M24, limits that hold across replicas](#2026-07-31--m24-limits-that-hold-across-replicas) | A backend rather than a replacement; the server-side clock; enforcing a deadline outside the client; why the request context is not used |
| [The loop kept stopping](#2026-07-31--the-loop-kept-stopping-for-reasons-it-had-invented) | Why two runs ended early; the safety net that read as permission; naming the specific excuses |
| [M24.5, a dark theme that cannot flash](#2026-07-31--m245-a-dark-theme-that-cannot-flash) | Why the server renders the attribute; the token scan as enforcement; the two light values that moved under D21 |
| [The loop splits in two](#2026-07-31--the-loop-splits-into-an-orchestrator-and-workers) | Premature stopping as a context symptom; why the builder does not commit its own work; the seam at 3.3/3.4; what the split costs |
| [M24.6, and a test that could not see the defect](#2026-07-31--m246-and-a-test-that-could-not-see-the-defect) | The unlayered-`:root` cascade bug; why the token scan missed it; verifying mechanisms instead of outcomes; why a new milestone rather than reopening M24.5 |
| [M24.6 withdrawn; M24.5 reopened](#2026-07-31--m246-withdrawn-m245-reopened-and-appends-get-a-number) | Corrects the entry above: a `done` row may not assert something false; reopening as the rule; why every append now carries its milestone number |
| [Capture, read-ahead, and cost](#2026-07-31--capture-read-ahead-and-measuring-what-the-contract-costs) | `/note` decides nothing; classification against the tree; upcoming-decisions holds questions only; predicted vs realized read cost; sub-milestone work may commit alone |
| [M24.5 reopened: applied, not declared](#2026-07-31--m245-applying-the-theme-rather-than-declaring-it) | Why the tokens are unlayered rather than all in `@layer base`; a test that had to be shown red against the shipped stylesheet; resolving the cascade live instead of counting attributes; where the control went and why two sites is not two controls |
| [M25, which workspace a request is in](#2026-07-31--m25-where-a-request-decides-which-workspace-it-is-in) | Three columns for three questions; precedence as one `ORDER BY`; why the switch needs a session; why the switcher draws nothing with one membership |
| [M26, a mailer that is genuinely optional](#2026-07-31--m26-a-mailer-that-is-genuinely-optional) | Off-by-default as a nil interface rather than a flag; why a relay being down is not a boot failure; inert by construction in the renderer; attempts counted at claim time; plain text as the whole hostile-input answer |

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
bytes. See `development.md`.

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

Tested before writing the schema (`../adr/0001-partitioning-and-sqlc.md`).
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

### API key tokens are a fixed-length prefix plus a secret

`lk_live_<8 chars>_<43 chars>`. The prefix is stored and uniquely indexed, so
verification is a single-row lookup rather than a scan comparing every stored
hash — the alternative gets slower with every key ever issued.

Both parts are fixed length and taken by offset. Splitting on `_` would break
the first time a base64url secret contained one, which is roughly one token in
sixty. The public id is lowercase base32 because five random bytes encode as
exactly eight characters with no padding, and the alphabet has nothing that
needs quoting in a shell, a YAML file or a CI secret box.

`live` has no meaning yet. It is there so a future test-mode key is
distinguishable by eye instead of by asking the database.

### Key hashes are HMAC with a configured pepper, not argon2

The same reasoning as session tokens: the secret is full-entropy random, so
key-stretching adds nothing, and 64 MiB of argon2 per request does not fit a
150ms API budget. The pepper lives in configuration rather than the database,
so a database dump alone does not permit offline verification.

The prefix is part of the HMAC message, which binds a hash to its own row: a
hash copied into another key's row stops verifying. NUL-separated from the
secret, for the same reason the visitor hash separates its fields.

Rotating `API_KEY_PEPPER` invalidates every existing key. That is stated in the
config validation message, because it is the kind of thing an operator
otherwise discovers from a support ticket.

### A key's permissions are its scopes intersected with its owner's role

Recomputed on every request, never stored. Demoting a user therefore weakens
their keys immediately, and a scope the role no longer grants stops working
without the key being reissued. Storing the effective set would leave keys
holding permissions their owner has lost — which is exactly the state an
attacker who briefly held an admin account would want to leave behind.

Scopes are validated against the `permissions` table rather than a list in Go,
so the vocabulary cannot drift from RBAC. A scope the creator does not hold is
refused: otherwise minting a key would be a way to grant yourself permissions
your role does not include.

### `apikeys.*` is not delegable to a key

A key that can mint keys makes revocation meaningless — whoever holds a leaked
one issues a replacement before the original is cut off. So key management, and
password changes, require a session. `org.delete` follows the same rule: an
irreversible action should need an interactive sign-in rather than a token in a
CI variable.

The cost is that key rotation cannot be automated through the API in Phase 1.
Accepted, and recorded as a known limitation.

### One error for every invalid key

Unknown, malformed, wrong secret, revoked, expired and inactive-owner all
return the same 401. The distinction is of no use to a legitimate caller — the
key list shows revocation and expiry, so an owner can already see which of
theirs is which — and separate responses would tell whoever found a leaked key
whether it is still worth trying somewhere else.

Bearer authentication does reject, where a session cookie does not. A cookie
that no longer resolves is ordinary, so the request continues anonymously; a
bearer token is deliberate, and answering a dead key with "authentication
required" sends the caller looking in the wrong place.

### A bearer token beats a cookie on the same request

The explicit credential wins. The alternative — a cookie silently upgrading a
deliberately weak key's permissions — would make a scoped key untestable from a
browser session and would hide the mistake.

### `last_used_at` is coalesced in memory and flushed on a timer

Authentication must not cost a write. A map keyed by key id collapses a
thousand uses per second into one row update per interval, the map is bounded so
a pathological case loses a timestamp rather than growing without limit, and the
pending set is cleared before the write rather than retained for a retry.

The value answers "is this key still in use", which does not need second
resolution or durability. It is flushed on shutdown along with the click
buffer.

### The CLI acts as a named user, not as root

`lctl apikey` resolves `--user` to an identity and then calls the same service
methods the API does, so it cannot mint a key with scopes that user's role does
not grant. An operator with database access could bypass RBAC by hand anyway;
the point is that the supported path does not, because a CLI that quietly
ignores permissions is where the exception becomes the habit.

It exists because the first key on a headless instance has to come from
somewhere, and creating one through the API needs a browser session. The token
goes to stdout and everything else to stderr, so redirecting stdout captures
the key and nothing else.

---

## 2026-07-30 — dashboard (M11)

### The web tree is a skin over the same services

Web handlers parse forms, call the identical service methods the JSON API
calls, and render templates. No validation, authorization or behaviour lives in
either surface, so they cannot diverge — which is the mechanism behind the
"every UI feature has API support" success criterion, not a review checklist.
The integration suite makes the point concrete: a key minted through the
dashboard form authenticates against the JSON API in the same test.

### CSRF is the stdlib's cross-origin check, not tokens

`http.CrossOriginProtection` (Go 1.25+) rejects unsafe cross-site requests by
reading `Sec-Fetch-Site`, falling back to comparing `Origin` against `Host`.
Layered with `SameSite=Lax` cookies, that covers what a synchronizer token
covers, with no token to generate, embed in every form, rotate, or forget.
Non-browser API clients send neither header and pass untouched, so one wrapper
protects the API and the dashboard together. The trade: pre-2020 browsers send
neither header and are not protected — accepted, they are also unsupported for
the dashboard generally.

### The CSP has no unsafe- waivers, and the templates keep it that way

`script-src 'self'; style-src 'self'` with no inline anything. Three
consequences are load-bearing: dynamic bar widths are SVG attributes rather
than `style=` (CSP does not govern presentation attributes); charts are
server-rendered inline SVG rather than a JS charting library; and htmx is
restricted to the feature subset that does not eval (`hx-on`, `js:`
expressions and bracketed event filters are off limits in templates). A test
fails if a waiver ever appears in the header.

### Charts are computed in Go, drawn as SVG

A charting library would be the product's only piece of custom JavaScript, for
bars and an axis. Instead `ui.BarChart` lays out integer geometry and the
template is a dumb loop over `<rect>`s. Day series are dense-filled before
charting, because the rollup query returns only days that have rows and a
sparse bar chart lies — a silent week between two busy days would vanish.

### htmx is vendored and checksummed; the stylesheet is generated

Opposite treatments for a reason. app.css is generated *from this repo's own
templates*, so committing it means it goes stale invisibly; it is built by
`make css` and embedded. htmx is a fixed upstream artifact, so committing it
keeps a fresh clone building offline; `make htmx` verifies the blob against
the pinned release checksum so it is verifiable rather than trusted. The
Docker css stage copies the whole ui tree (Tailwind scans templates and
funcs.go for class names) and fails the build if the output is implausibly
small — a stylesheet of just the preflight reset means the scan found nothing.

### Templates parse at boot; pages render into a buffer

A syntax error fails startup, not the first visit to the unlucky page. Each
page is parsed into a clone of the shared layout+partials set, so two pages
defining "content" is the normal case rather than a collision. Rendering goes
through a buffer because executing straight into the ResponseWriter commits a
200 alongside half a page the moment a template hits a missing field. A
missing stylesheet, by contrast, is a boot warning, not a failure: unstyled
pages work, and refusing to start would turn a forgotten `make css` into an
outage.

### Static assets are fingerprinted and skip the session middleware

Every URL the templates emit carries a content hash, so assets are served
immutable-for-a-year and a new build busts the cache by changing the URL. The
`/static/` mount bypasses session lookup entirely — public bytes must not cost
a database round trip, and before this the session middleware would have run
for every stylesheet request carrying a cookie.

### htmx responses that must navigate use HX-Redirect

htmx follows HTTP redirects transparently and swaps the *target* into the
fragment it was updating — the classic symptom is a login page rendered inside
a search-results table. Anywhere the browser must actually move (auth expiry
mid-page, post-action navigation), htmx requests get an `HX-Redirect` header
and everyone else gets a 303.

### The key-creation form renders its POST response directly

The token exists only in that response, and a redirect would drop it. The
alternative — a flash cookie — would put a live credential into a Set-Cookie
header to survive one hop. Rendering directly means a refresh re-submits and
mints a second key; accepted, because the browser warns first and the extra
key is visible in the list and revocable.

### The login form's `next` parameter only accepts local paths

Anything else makes the login page an open redirect. The check rejects
non-`/`-prefixed values, `//host` (scheme-relative, the classic bypass of a
naive prefix check), and backslashes. Rejected values fall back to /dashboard
rather than erroring, because a mangled `next` is noise, not an attack worth
stopping a sign-in over.

---

## 2026-07-30 — OpenAPI contract (M12)

### The spec is hand-maintained and test-enforced, not generated

Planning said spec-first with oapi-codegen. By the time the document was
written, the API already existed — built service-first, with handlers as thin
skins over the service layer — and adopting the generated server interface
would have rewritten working, tested handlers for zero behavioural change.
Generation was the means; the end was always "the contract cannot lie". Tests
deliver that end directly, twice over:

- a parity test in internal/httpx asserts the router and the document describe
  the same set of routes, in both directions;
- an integration contract test replays a real request through every operation
  in the document and validates request and response against its schemas, then
  fails if any operation was never exercised. Response schemas carry
  `additionalProperties: false`, so an undocumented field is a failure, not a
  quiet extension.

The enforcement was verified by sabotage before being trusted: a field added
to the spec that the API does not return fails the suite.

### Writing the contract found two real bugs

Which is the argument for contract tests in one sentence. `ErrInvalidEmail`
was unmapped in the problem writer, so registering with a malformed address
returned a 500; and minimum password length was enforced only by the HTML
form, so the JSON API accepted a one-character password. Both are fixed at the
right layer — the error mapping and the service — because a policy one client
can skip is not a policy.

### The YAML is authored; the JSON is derived at first use

Tooling asks for JSON, humans and diffs are better served by YAML. Committing
both invites them to disagree; converting the embedded YAML once in memory
makes disagreement impossible. Both forms are served under /api/v1, publicly,
matching the existing "/docs is public" default.

### Swagger UI, vendored like htmx, with a one-directive CSP waiver

The try-it-out console is what earns Swagger UI its megabyte on a self-hosted
product: paste an API key, exercise the API from the browser. Its two dist
files are vendored and checksum-pinned (scripts/get-swagger.sh), served
fingerprinted from the same embedded static tree as everything else.

Swagger UI is React writing inline style attributes, which the strict CSP
blocks. /docs alone gets `style-src 'unsafe-inline'`; script-src stays 'self',
which works because the initializer lives in a real file
(static/js/docs.js) instead of the inline <script> of the stock index.html. A
test pins the waiver's shape so it cannot creep to scripts or to other pages.

---

## 2026-07-30 — metrics (M13)

### Its own registry, passed explicitly, nil-safe

Not `prometheus.DefaultRegisterer`. A global registry makes two servers in one
test process collide on registration, and it lets any dependency that happens
to import client_golang publish into this project's namespace. The struct is
passed through Deps like every other collaborator, and every method is nil-safe
so an instrumentation call site never has to know whether metrics are enabled —
which is what lets the whole test suite construct routers without them.

### Labels are surfaces, never paths

`{surface, method, status}` where surface is one of redirect, api, web, static,
ops. The redirect namespace is chosen by whoever sends the request, so a path
label would let a scanner mint unbounded series and take the process down
*through the metrics endpoint*. Status is a class (`4xx`) rather than a code,
because that is what alerts are written against.

The cost is no per-route API latency. Accepted: the access log has that detail
and does not accumulate. A test asserts the label set stays fixed across
arbitrary paths.

### The redirect histogram is the SLO's measurement point

`linkctrl_redirect_duration_seconds{outcome, cache}`, observed inside the
redirect handler rather than in middleware — the outer view includes router
dispatch, and the target names the time to resolve and answer. `cache` is the
label that makes the SLO answerable server-side: the target is stated for cache
hits only, so memory and redis are hits, database a miss, negative a cached
miss.

Buckets are hand-picked with a boundary at exactly 0.02, so "fraction under
target" is a ratio of bucket counts rather than an interpolation. The default
buckets would have put the entire interesting range into one bucket and made
any p99 estimate meaningless. A histogram, never a summary: per-process
quantiles cannot be aggregated across replicas.

### Pool and pipeline state is read at scrape time

The connection pools and the ingester already keep authoritative counters.
Mirroring them into gauges at write time would create two sources of truth that
can drift; a collector that reads them during a scrape cannot drift and costs
nothing in between. The two pools are labelled separately because the entire
point of splitting them is that they saturate independently — the alert worth
having is "the redirect pool is queueing", which an aggregate hides.

### A second listener, unauthenticated, unpublished

Queue depths, pool saturation and traffic shape are operational detail. They go
on `METRICS_ADDR`, which compose does not publish, rather than behind a token on
the public listener — the second port is a stronger boundary than a credential
someone will eventually put in a URL. A test asserts `/metrics` on the public
listener is an ordinary 404 from the redirect tree. Losing the metrics listener
logs and continues; monitoring must not be able to take down what it monitors.

### Latency measured on a Windows host is zero, and that is the clock

Verified rather than assumed: 100,000 out of 100,000 back-to-back `time.Since`
samples return exactly zero, because Go's monotonic clock on Windows cannot
resolve intervals this short. So a cache-served redirect lands in the zero
bucket and `_sum` is useless locally — while bucket counts, and the ratio the
SLO is stated as, stay correct. The same applies to `click_events.latency_us`.
Both resolve on Linux, which is where the SLO is measured and where the service
runs. Recorded in development.md so nobody chases it as a bug or quotes a
local number as a result.

---

## 2026-07-30 — documentation (M14)

### Writing the setup docs was an audit

Documenting every environment variable meant checking whether anything reads it.
Twelve do not. `LOGIN_RATE_PER_MIN`, `API_RATE_PER_MIN` and
`REDIRECT_404_RATE_LIMIT` parse, validate and change nothing — so the rate
limiting that *Scope by phase* lists as Phase 1 does not exist. Nor does GeoIP
enrichment, nor retention enforcement: partitions are created and never dropped,
so `ANALYTICS_RETENTION_DAYS` is decoration.

None of that was hidden; it was just never stated in one place. It is now, in
Plan.md under "Phase 1 scope not yet built", in the README under "Not built
yet", and in a table at the end of the configuration reference. A knob that
accepts a value and does nothing is worse than a missing one, because the
operator believes they have configured something.

The same pass found three dangling references — `.gitignore` pointing at a
DEPLOY.md that never existed, compose pointing at a `docker-compose.prod.yml`
and an "obs profile" that never existed — and one contradiction: the OpenAPI
document claimed AGPL while LICENSE says MIT. Fixed rather than documented.

### Every command in the docs was run before it was written down

`docker compose run --rm app --check-config`, `migrate status` through the image,
`lctl apikey create`, the `curl` examples, the `/readyz` body. One got corrected
by doing it: `docker compose run app /lctl migrate up` cannot work, because the
image's entrypoint is the server and the arguments append to it — it needs
`--entrypoint /lctl`. That is exactly the kind of error a reader cannot debug,
because it looks like it should work.

Two examples were also wrong in detail until checked against the code: the
validation error code is `private_address`, not the `blocked_host` first written,
and the grantable scope list omitted `members.*` and `workspace.write`, which
*are* grantable and gate nothing yet.

### The audience decides the file, not the topic

`deployment.md`, `configuration.md`, `usage.md`, `cli.md` and `operations.md` are
each written for one person with one question: I am installing this; what does
this variable do; how do I use it; what does this command do; it is 3am and
something is wrong. The alternative — one long document ordered by subsystem —
is the one nobody finishes reading.

Each states what is *not* built where a reader would otherwise assume it is.
Operations lists the missing rate limiting next to the alerts, because the moment
someone needs alerts is the moment they need to know throttling is theirs to add.

---

## 2026-07-30 — planning: the enforcement milestone (M15)

### The gap the docs found gets one milestone, not four

Rate limiting, 404 probe limiting, GeoIP enrichment and retention enforcement are
four unrelated subsystems — middleware, hot-path middleware, ingest, a background
job — and splitting them into four milestones is the defensible-looking choice.
They are one milestone because they share the only property that matters here:
each is a configuration variable that lies. The acceptance criterion is therefore
also one thing, and it is mechanical rather than a judgment call — *the "Accepted
but not yet in effect" table in the configuration reference is empty*. Four
milestones would let three of them sit indefinitely at "next", which is how a
knob that does nothing survives a second release.

### It goes before load validation, not after

Load validation was the obvious next milestone: the histogram exists, the target
is written down, the plan has said "not yet verified" for weeks. But rate limiting
and 404 probe limiting both add work to the redirect tree, and probe limiting adds
it to the *miss* path that a public shortener is mostly asked for. Measuring the
SLO first produces a number for a path that is about to change, and a measured
number is exactly the kind of artifact nobody re-measures. Throttle first, then
measure once, honestly.

### Three knobs get deleted instead of implemented

`INGEST_WORKERS`, `VISITOR_SALT_ROTATION` and `BOT_FILTER_ENABLED` were in the
same list of variables-that-do-nothing, but they are a different defect. Nothing
reads them because the fixed behavior is the design: a single ingest consumer is
what makes batch coalescing work, daily salt rotation is what the purge is keyed
to, and bots are always classified because the control that matters — keeping them
out of headline figures — lives in the queries. Implementing them would mean
shipping three ways to make the system worse, one of which (`VISITOR_SALT_ROTATION`)
weakens de-identification by setting an environment variable. Removal is the fix.

Startup will warn when a removed variable is still set. Silent removal reproduces
the original defect from the other direction: the operator still believes the
value does something.

### Two spec details worth their reasons

**Per-IP limits are added alongside the per-account lockout, not instead of it.**
One address attacking many accounts and many addresses attacking one account are
different attacks; each limit is blind to the other's. Replacing the lockout with
per-IP throttling would be a regression that looks like a feature.

**`Server-Timing` stays default-off and application-tree-only.** It publishes
internal phase timings to anyone who asks, which is a side channel on a service
where the interesting timing question is whether an alias exists. On the redirect
tree it would also mean measuring the path it is reporting on.

---

## 2026-07-30 — enforcement (M15)

### The 404 probe limit charges misses, and only misses that cost something

The obvious design — check a limiter at the top of the redirect handler and refuse
when it is empty — has a failure mode that is worse than the abuse it prevents.
Buckets are keyed on the client address, so behind a proxy with `TRUSTED_PROXIES`
unset every visitor shares one, and the ~60 favicon 404s a modest site produces per
minute would then refuse *every* redirect, including working links. A limiter that
turns one configuration mistake into a total outage is not a mitigation.

Three rules fix it, and each one narrows what the limiter can break:

**Only a miss is charged.** A hit never spends a token, so a popular link cannot
throttle its own audience, and a bucket can only empty by asking for things that
are not there.

**Only a miss that cost a lookup is charged.** `/favicon.ico`, `/robots.txt`,
`/wp-login.php` — anything that could not be a stored alias — is refused on shape
by a byte scan, before the cache or the database. That was already most of the
protection: a request rejected on shape costs nothing, so there is nothing to
throttle. It also removes the main source of legitimate 404s from the limiter's
view, since those paths are exactly what browsers and scanners ask for. The check
became `alias.WellFormed`, shape only, no list lookups, no allocation.

**A throttled request is still served from the in-process cache.** `ResolveCached`
answers from memory or not at all — one map lookup, no I/O — so an address that
tripped the limit keeps following links that are actually in use, while an alias
nobody is using still cannot be turned into a database query by asking again. The
cost a prober imposes is the query, and that is precisely what is refused.

What survives is a limit that stops alias enumeration and cannot take a working
link off the air. The integration test asserts the last property directly, because
it is the one a future refactor would quietly break.

### Rate limiting is in-process, and IPv6 is keyed by /64

Redis-backed limits are the textbook answer and the wrong one here. The redirect
path's entire budget is 20ms, so spending a network round trip to decide whether to
allow a request costs more than the limit saves — and Redis is optional at runtime
by design, so a limiter that stops limiting when the cache goes away is worse than
one whose numbers are per-instance. With N replicas the effective limit is N times
the configured one; that is in Known limitations rather than hidden.

IPv6 keys on the /64 prefix, not the address. A single host is routinely handed a
/64 or larger, so per-address keying would let one machine present effectively
unlimited identities — defeating the limit and growing the key table without bound
while doing it. The table is capped and fails open when full, counted by
`linkctrl_rate_limit_overflow_total`: a limiter is abuse mitigation, not an
authorization boundary, and refusing real traffic because bookkeeping ran out of
room would turn a memory ceiling into an outage. Failing open silently would be
the real defect, which is why the counter is a documented alert.

Sweeping is amortized across calls rather than run from a goroutine. A limiter
cannot then outlive its owner or leak a goroutine into a test binary, and the
buckets it reclaims are the ones that have refilled to full — they hold no pending
penalty, so dropping them loses nothing. A spent bucket is never swept, or an
attacker could clear their own penalty by generating unrelated traffic.

### Per-address limits are added to the lockout, not substituted for it

One address guessing across a leaked credential list never trips a per-account
counter, and one account under attack from a botnet never trips a per-address one.
They are different attacks and both limits stay. The two answer `429` with
different problem types for the same reason: a client that cannot tell "you are
going too fast" from "this account is frozen" will retry the wrong one.

Dashboard page loads are deliberately outside `API_RATE_PER_MIN`. The variable says
API, and a person clicking around a server-rendered UI should not consume the
budget their own scripts need.

### Country only, and resolved at ingest

The schema has `country`, `region` and `city`, and the MaxMind database supplies all
three. Only the country is stored. Nothing in the product displays a region or a
city, so writing them would be collecting personal data for no purpose — and city
plus timestamp is close to a location history, on the one table the privacy design
is proudest of holding nothing personal. This narrows *Scope by phase*, which listed
country/region/city as Phase 1; the row was split rather than quietly satisfied.

Resolution happens in `prepare`, beside the visitor hash, because that is the last
moment the address exists. There is no stored address to enrich later — which is
also why an operator adding a database later changes only future clicks.

The reader is `oschwald/maxminddb-golang`, one module with no transitive
dependencies. The fixture the tests read is a synthetic database built by MaxMind's
own writer and committed, with the generator kept under `testdata` behind a
`//go:build ignore` tag: an independent implementation writes the file and ours
reads it, which is a better test than round-tripping our own encoder, and the
writer does not become a dependency of the module. Every network in it is a
documentation range and every country is invented.

### Retention drops months, and only whole ones

`DELETE FROM click_events WHERE occurred_at < ...` on the largest table in the
system, followed by a `VACUUM`, forever, is the alternative to what the
partitioning was for. Dropping a partition is instant, reclaims the space
immediately, and cannot half-finish.

A partition goes only once its *newest possible* row is outside the window, so data
survives up to a month past the nominal number. That is the right way to be wrong:
keeping data slightly too long is recoverable and deleting it early is not. The
boundary case has a test, because an off-by-one here deletes retained analytics.

`audit_logs` is partitioned identically and exempt. Audit retention is a different
policy — the reason to keep an audit trail is that someone may ask what happened
long afterwards — and deleting it on the analytics setting would be a surprise of
the wrong kind. Partitions whose names this code did not generate are also left
alone: an operator who attached a table by hand had a reason.

Each drop runs in its own transaction with `lock_timeout = 5s`. Detaching a
partition needs a brief exclusive lock on the parent, which the ingester takes on
every batch; without the timeout the drop would sit in the lock queue and
everything arriving behind it would queue too, turning housekeeping into a stall on
the write path. Failing is cheap — it runs again in an hour.

### The removed knobs, and why the removal is announced

`INGEST_WORKERS`, `VISITOR_SALT_ROTATION` and `BOT_FILTER_ENABLED` are gone rather
than implemented, for the reason recorded when the milestone was planned. What is
new is the announcement: `config.Removed` keeps them as data, startup logs a
warning naming each one still set, and `lctl config check` prints the same. Silent
removal reproduces the original defect from the other side — the operator still has
the line and still believes it does something. A reflective test asserts nothing in
that map still has an `env` tag, so the list cannot start lying.

### The per-request deadline is a context, not a TimeoutHandler

`http.TimeoutHandler` buffers the entire response in memory so it can replace it
with a 503. That is a real cost on every request, to gain a guarantee this service
does not need: every database call takes a context, so the deadline is what
actually stops the work. The client gets a `504` from the error mapper instead of a
fabricated one from middleware, and `context.Canceled` — a disconnect — now maps to
499 rather than falling through to a 500. Counting people closing tabs as server
faults is how a 5xx alert becomes noise.

The redirect tree is deliberately not covered. It has `REDIRECT_TIMEOUT`, applied
where the resolver would touch Postgres, and a 15-second ceiling there would be
meaningless — a redirect that has been waiting a second has already missed its
target and is holding a connection from the small redirect pool.

### A newer gosec flagged five pre-existing lines (M15)

The linter's own version moved, and gosec gained taint-analysis checks that flag
the healthcheck's self-probe (G704), both session cookie constructors (G124, which
wants `Secure` hardcoded) and `seeOther` (G710). All five are false positives, and
each is now annotated with the reason rather than silenced by excluding the rule —
excluding G710 wholesale would hide a real open redirect if one were ever added,
whereas the annotation says exactly why *this* call is safe: `safeNext` is the only
path by which a caller-supplied destination reaches it, and it rejects anything
that is not a local path, including the `//evil.com` form that beats a naive
"starts with /" check.

---

## 2026-07-30 — load validation (M16)

### The measurement is two numbers, and the harness has to earn both

A load test that reports only what the generator saw is measuring the generator,
the network and the server together and calling the sum the server's latency. So
`scripts/load-test.sh` reports the generator's percentiles *and* the server's own
histogram, and the second one takes work to be honest about: the histogram is
cumulative since boot, so it is snapshotted before and after and reported as a
delta, and the warm-up is a separate k6 invocation that finishes before the first
snapshot. Otherwise a "cached p99" quietly contains every cold read the warm-up
performed.

The script also prints the cache mix and the redirect pool's acquire waits next to
the latency, because those are what say whether the number means what it claims. A
cached measurement with database reads in it is not a cached measurement, and the
first run of this test was exactly that — 9,003 of 245,001 requests reached
Postgres — with latency that looked perfectly fine.

### k6's `__ITER` is per-VU, and the cache mix is what caught it

The warm-up walked the hot alias set with `__ITER % HOT` across 20 VUs. Each VU has
its own counter, so it covered the first 250 aliases twenty times instead of 5,000
aliases once. It now runs on a single VU, sequentially: slower, and impossible to
get wrong. A warm-up whose correctness depends on the executor's iteration
semantics is a warm-up that will silently stop warming.

Worth recording alongside it: `cache="database"` counts requests that reached the
database *tier*, not queries executed. `singleflight` collapses concurrent misses
for one alias into a single query and every waiter is still counted, which is why
5,000 cold aliases produced 9,003 observations. The metric is not wrong, but it
overstates queries and the histogram is the wrong place to look for them.

### A resolve failure was answering 404, and 404 is a claim

Under load at 500 rps uncached, an early run failed 38.7% of requests: the redirect
pool queued 1,798 acquires totalling 229 seconds, `REDIRECT_TIMEOUT` fired, and
every one of those requests was answered 404.

The original comment defended that choice — a visitor cannot act on the difference,
and an error page on a short link is worse than "not found". It was wrong, and the
project's own reasoning elsewhere says why: 410 exists precisely so crawlers and
link checkers stop retrying, which means 404 is understood as "this link is dead".
Publishing it because a connection was briefly unavailable tells every crawler to
drop a live link, and no retry follows. It is now 503 with `Retry-After: 1`, which
is true.

This is the finding that justifies the milestone. At development traffic that code
path never executes.

### The rollup rewrite did not do what it was for, and kept for a different reason

`RollupDimensionDaily` takes 16-21 seconds and runs every 60. The obvious cause was
that it read `click_events` six times, once per dimension, so it was rewritten to
read once and expand each row with `CROSS JOIN LATERAL (VALUES ...)`. Wall clock did
not move: ~20s either way.

`EXPLAIN (ANALYZE, BUFFERS)` says why, and says something else too. 831,776 events
in the window become 553,053 output rows and every one is a conflicting tuple, so
the time is in the upsert — of ~8M buffer hits, the aggregate accounts for under a
million. Recomputing whole days means rewriting every `(link, day, dimension,
value)` tuple every run, and that choice was made deliberately: an incremental
"add what arrived since the watermark" design double-counts on retry and, once it
drifts, stays wrong invisibly. What the load test adds is the size at which the
choice stops being free.

The something else: the six-branch version sorted 6.2M rows through an **external
merge that spilled 471 MB of temp files, every 60 seconds**. Reading once lets the
sort use the index's `link_id` ordering and run incrementally in memory — peak
152 kB per group, no temp files at all. That is a real measured improvement in
resource consumption, invisible in wall clock only because this host's disk absorbs
the spill.

So the rewrite is kept, on that evidence rather than on the reason it was written.
The decision was reversed twice while working through this, which is worth
recording: first kept for its shape, then reverted for showing no wall-clock
benefit, then kept once the plan showed the temp spill. "No faster" and "no better"
are different claims, and only the second one justifies a revert.

It is documented in slo.md as *not* a fix for the job's cost, because a change that
looks like an optimisation and is not is worse than no change — the next person
would assume the 20 seconds had been addressed.

### Reverting a sabotage test can revert the change under test

The dimension test was sabotage-checked by editing the generated `analytics.sql.go`
directly and restoring it with `git checkout`. That restore also undid the sqlc
regeneration, so the rollup measurement that followed was taken against the old
query while the working tree said otherwise — a wrong number that agreed with the
expected conclusion, which is the most dangerous kind.

What caught it was reading `git status` before committing and noticing the
generated file was absent from a change set that regenerated it. Generated code
belongs in the diff review for exactly this reason.

Sabotage-checking that new test was itself informative. Changing the `browser`
fallback did not fail it: the only rows with a null browser are bots, and bots are
excluded, so that `coalesce` is unreachable. Changing the `country` fallback and
the bot filter both failed it. The test is sensitive to what the data actually
exercises, which is not the same as sensitive to everything the query says.

### What the cached result actually demonstrates

Every measurement was taken while that 19-second rollup ran every minute on the
same Postgres. The cached path recorded zero database reads and zero pool acquire
waits, and 100% of 240,001 redirects answered under 20ms. That is the two-tier
cache and the dedicated redirect pool doing exactly the job they were built for,
under the load that would otherwise expose it. The isolation is the result worth
keeping; the microsecond figures belong to one laptop.

---

## 2026-07-30 — release packaging (M17)

### 0.x, because the product surface is not settled and the API is

Two contracts, versioned separately. The REST API is versioned by its path, so a
breaking change there becomes `/api/v2` and never a change to `v1` — which means
the release version says nothing about API stability and should not pretend to.
The product version is `0.x` while Phase 2 is outstanding: shared workspaces,
folders and custom domains will move the dashboard and add tables. `0.x` here means
"the surface may still move", not "unfinished"; everything documented as built is
tested and the SLO is measured.

Calling it 1.0.0 would claim stability the project has not earned while
single-replica cache invalidation is a documented limitation.

### The changelog is the artifact, and the workflow refuses to publish without it

Release notes assembled from commit subjects are written for the person who wrote
the commits. `CHANGELOG.md` is written for the operator deciding whether to take an
upgrade, which is why each version lists its *limitations* alongside its additions —
"run one instance", "the pepper cannot be rotated", "the dimension rollup gets
expensive" are what someone needs before deploying, and none of them appear in a
diff.

Both the local gate and the release workflow fail if there is no section for the
version being tagged. A release with no notes is a release nobody can evaluate.

### `lctl` was shipping unstamped, and CI now asserts it is not

The Dockerfile built the server with version ldflags and `lctl` with `-s -w` alone,
so a released image answered `lctl version` with "dev (commit unknown)". The first
thing anyone does when a CLI misbehaves is ask it what it is, and it was lying.

Both binaries now carry the same stamp, and CI greps for "commit unknown" in the
output of both — a stamp that silently stops working is invisible until the moment
someone needs it, which is the worst moment to discover it.

### Multi-architecture builds were going to run the Go toolchain under emulation

The build stage inherited the *target* platform, so an arm64 image built on an
amd64 host ran the entire compile through QEMU. Pinning the stage to
`$BUILDPLATFORM` and letting `GOARCH` do the work — which the Dockerfile was
already set up for — took a two-architecture build to 34 seconds, measured. Go with
CGO disabled cross-compiles as a matter of course, so the emulation was buying
nothing at all.

The stylesheet stage had the same problem for a different reason: it *runs* a
downloaded Tailwind binary, so under a multi-architecture build it was executed
once per target through emulation to produce a byte-identical stylesheet. It is now
pinned to the build platform and selects its asset by `BUILDARCH`.

### One archive format, including for Windows

`.zip` for Windows would mean either a zip tool on every build host or an artifact
whose format depends on where it was built. Windows 10 and later ship `tar`, so
`.tar.gz` everywhere costs a Windows user nothing and removes both problems. The
alternative was discovered the honest way: `zip` is not present in this
environment's shell, and a target that only works in CI is a target nobody can
verify.

### The release gate checks what a machine can, and lists what it cannot

`scripts/release-check.sh` verifies the tree is clean, the tag is free, the
changelog has a section, sqlc output matches its SQL, vendored assets match their
checksums, the stylesheet exists, everything builds and tests under the race
detector, the OpenAPI document matches the routes, and every platform
cross-compiles. Running it on a dirty tree during this milestone produced exactly
one failure — the clean-tree check — which is the gate demonstrating itself.

The clean-tree requirement is not fussiness: a release must be reproducible from
the tag, and an uncommitted file means the artifacts contain something the tag does
not. The sqlc check exists because this project has already shipped a measurement
taken against a generated file that did not match its source.

What a script cannot check is a list in releasing.md: that the changelog was written
for an operator, that behaviour changes reached configuration.md and operations.md,
that new limitations reached Plan.md, and that any performance claim was measured on
the version making it.

---

## 2026-07-30 — the Phase 1 completeness review, and what it found

"18 of 18 milestones" was reviewed rather than trusted: six parallel reviewers
(scope parity, M15 code, M16/17 infrastructure, test gaps, documentation claims,
security), every finding adversarially verified against the code before being
accepted. Thirty confirmed, one refuted. Phase 1 was not complete, and two of the
gaps were the kind that live in production for years.

### The purge job did not exist, and the alias promise was inverted

The schema promised it twice ("the purge job deletes the row after this passes"),
ReserveAlias was written for it, the docs and the changelog described it as real —
and no job called any of it. Worse than rows accumulating: the unique index is
partial on deleted_at IS NULL, so soft-deleting a link freed its alias *instantly*.
The documented promise — a trafficked alias is never reissued, because it is on
printed material and in other people's bookmarks — was not merely unenforced; the
opposite was true, and anyone could take over a deleted link's audience the moment
its owner trashed it. The SQL comment "the alias stays reserved while the row
exists" was false, and a test comment cited it while testing something else.

The fix has three parts because the index cannot express the rule. IsAliasTaken
now counts trashed rows and reservations as taken; BOTH create paths consult it —
the user-supplied path previously relied on the index alone, so a populated
reservation table would still not have stopped a custom-alias re-registration —
and alias changes do too, because a rename is a creation as far as the namespace
is concerned. The purge itself is one statement: a writable CTE that inserts the
reservation and deletes the row, so a crash cannot separate them, with SKIP LOCKED
so it can never block a concurrent restore-by-hand. Untrafficked aliases are
released deliberately — nothing in the wild points at them. The check-then-insert
race (a delete committing between check and insert) is accepted: its window is
milliseconds, its worst case is what used to be the *permanent* behavior.

The reapers came along in the same housekeeping job: DeleteExpiredSessions and
DeleteRevokedAPIKeys were both written, commented as active, and never called.
Three dead maintenance queries is not a coincidence — it is a missing job.

### Query forwarding existed only on the read side

Plan.md lists it as Phase 1; the redirect handler merged the query string; the
column, the snapshot field and the cache encoding all carried it — and nothing
could set it. No API field, no form control, and zero tests on the merge path, so
the feature was unreachable except by hand-written SQL and its gap invisible to
the suite. Now: field on both API shapes and the OpenAPI schemas, a checkbox on
the edit form, and an end-to-end test that asserts the merge, the
destination-wins conflict rule, the default staying off, and the toggle
invalidating the cached snapshot.

### The review paid for itself on the infrastructure too

CI's lint job pinned golangci-lint v2.12.2 on golangci-lint-action@v6, which runs
only v1.x — the job would have failed on every run, discovered the first time
anyone pushed. The release workflow interpolated the dispatch input into shell
text and shape-checked it with a glob that accepts 'v1.2.3;evil'; versions now
arrive via env, validated by an anchored regex, under least-privilege per-job
permissions. Third-party actions are pinned to commit SHAs — the project
checksum-pins every other third-party build input, and a mutable tag under a
write-scoped token is the same class of thing. And the dispatch dry-run path
could never pass its own changelog check with its own documented default input.

The worst documentation finding was operational: docker-compose.override.yml
hard-coded APP_ENV=development, and compose applies the override automatically —
so the documented production procedure silently deployed a dev-mode instance,
insecure cookies and all. The override's values are interpolated defaults now
(an operator's .env wins), and the production docs say `-f docker-compose.yml`,
which also keeps the override's published database ports off the host.

### Smaller confirmed findings, all fixed

RealIP read only the first X-Forwarded-For header line, so a proxy that appends
the client as a separate line (HAProxy-style) left the client's own forged first
line as the winner — Values() joined now, with the previously-untested
trusted-proxy parsing under eight unit tests including IPv4-mapped hops, which
Prefix.Contains silently never matched. The retention job's name regex never
compared its table-prefix capture to the parent table, so a hand-attached
"click_events_backup_2024_01" was droppable despite the code's promise to leave
foreign partitions alone. lctl seed --reset deleted any link sharing the prefix
in ANY workspace, with LIKE wildcards accepted in the prefix; it is now
workspace-scoped with a wildcard-free prefix charset. The seeder computed its
click-time window from the wall clock after minutes of link seeding, so a run
crossing a month boundary could write into the default partition. A deferred
geo.Close() could unmap the MaxMind file under an in-flight lookup on the
flush-timeout shutdown path; the mapping now lives as long as the process, which
is exactly as long as it is needed. The redirect tree's 429/503/404/410 responses
gained the nosniff header its own sibling documents as the rule. And the advisory
lock comment claimed the key was hashtext('linkctrl_jobs') when it is a
hand-picked constant — an operator following the comment into psql would have
locked a different key and concluded no leader exists. The first draft of the
corrected comment had the wrong decimal value in it, which is the finding
demonstrating itself.

### Test gaps closed where the risk was

internal/redirect had no tests at all — the package the SLO stands on. Decide and
CacheTTL (including the 1s floor that keeps a dead-but-popular expired link from
becoming a permanent database query, and the clamp that keeps a soon-expiring
link from outliving its expiry in cache) and the memcache tiers (expiry,
reap-before-clear eviction, the small-cache shard path, concurrent access) are
unit-tested now. EnsurePartitionRange has a December→January rollover test that
asserts a boundary row lands in an explicit partition. The purge cluster and
query forwarding got the integration tests described above. Sabotage-verified
where a first-try pass proved nothing: disabling the availability check turned
the 409 into a 201, and removing the prefix guard dropped the hand-attached
partitions.

### Process note, twice is a pattern

Reverting a sabotage with `git checkout` destroyed uncommitted work in the same
file for the second time in this project — last time it silently reverted a sqlc
regeneration, this time the whole ForwardQuery/availability wiring, caught by the
build breaking rather than by anything smart. The rule that follows: sabotage
with an edit that can be reverted by a counter-edit, never with checkout, unless
the file is committed. Separately, editing Go sources through PowerShell's
Get-Content/Set-Content mangled em-dashes into mojibake (UTF-8 read as ANSI);
byte-safe tools only for in-place source edits.

---

## 2026-07-30 — planning: signup and host separation (M18, M19)

Two milestones added to Phase 1 after 0.1.0 shipped. Adding scope to a phase that
was just declared complete deserves its own justification, so: neither is a
Phase 2 feature arriving early. Signup is a setting that already exists and is
only two-thirds wired; the host split is the thing that makes the dashboard stop
sharing a namespace with every short link. Both are gaps in what Phase 1 already
claims, which is the test for whether something belongs in this phase or the next.

### The environment is a ceiling, not a default

The request was a toggle in the UI *or* in `.env`, and the interesting question is
what happens when they disagree. Two rules were available. Database-wins is the
obvious one and is wrong: it means a stolen owner session can open a private
instance to the public, and the operator's `.env` — the first place anyone looks
when asking "can strangers register here?" — would say `closed` while the answer
is yes. Environment-wins alone is also wrong, because then the UI toggle is a
decoration on instances the operator did not pre-authorize.

So the environment sets the maximum and the toggle chooses within it, ordered
`closed` < `invite` < `open`. An operator who ships `closed` cannot have signup
opened by anyone holding a session, and the UI says why the control is disabled
rather than failing silently when pressed. An operator who ships `open` has
delegated the decision, which is what they said by writing it.

This is the same shape as the rule that API keys can never hold `apikeys.*`: a
credential that can widen its own reach makes revoking a leaked one meaningless.
A session that can open registration makes a closed instance's guarantee only as
strong as the least careful browser tab.

### Open signup admits tenants, not colleagues

`Register` provisions an organization, a workspace and an owner membership in one
transaction — that is Phase 1's tenancy model working as designed, and it means a
second account can see nothing of the first. The failure mode is entirely a
labelling one: an owner who flips a control called "allow sign-ups" to add a
co-worker gets a stranger with a private instance-within-the-instance, discovers
the co-worker sees an empty dashboard, and concludes the product is broken.

Invitations are Phase 2 and are not being pulled forward. The mitigation is that
both the toggle and the signup form must state what an account gets. A feature
whose correct behavior reliably surprises the person enabling it is a documentation
defect before it is anything else.

### No mailer, so no verification, so the default stays closed

`EmailVerifiedAt` is set at registration only for the first user, who is trusted by
construction — they had filesystem or deploy access to reach the setup page.
Nothing else sets it, because Phase 1 delivers no mail. Open signup therefore
creates accounts that are unverified and immediately usable, and anyone can claim
an address they do not control.

That is acceptable for the instances this is for — a team, a homelab, a company
behind SSO-at-the-proxy — and it is not acceptable as a default for an instance
facing the open internet. Hence `closed` stays the default, and the limitation is
written down rather than fixed with a mail dependency that Phase 1 has no other
reason to acquire.

### One instance with two hosts is not custom domains

The `domains` table already has `hostname`, `verified_at` and `ssl_status`, and
the resolver deliberately ignores all three in favor of `is_default`. The
temptation, given a milestone about hostnames, is to start matching on `hostname`
and arrive most of the way at per-workspace custom domains without having planned
for verification, certificate issuance, or what happens when a workspace points a
CNAME at you and then deletes it.

M19 therefore configures two origins for the whole instance and keeps resolution
matching on `is_default`. It may write the link host into that row for display; it
must not route on it. Custom domains stay Phase 2 with their machinery intact.

### Cookie isolation is the reason, not tidiness

`manage.example.com` and `lnk.example.com` read as cosmetics. The substance is
that `__Host-` cookies carry no `Domain` attribute and are locked to the exact
host that set them, so once the hosts differ the session cookie is *structurally*
incapable of reaching the link host. Short links are the surface that gets pasted
into forums, unfurled by strangers' bots and probed by scanners; it is the half of
the product most exposed and the half that needs no credentials at all.

That is also why the milestone requires a test rather than a paragraph. The
property holds by construction today, and it would be quietly destroyed by any
future change that sets an explicit `Domain` to "make cookies work across
subdomains" — a change that looks like a fix and reads as reasonable in review.

Two related traps, recorded so they are not rediscovered. Wrong-host requests
answer `404` rather than redirecting to the right host: a cross-host redirector
attached to the alias namespace is an open-redirect kit for anyone who can create
a link. And the reserved-alias list stays enforced even once the collision it
guards against is impossible, because an operator can merge back to a single host,
and an alias called `login` created during the split-host era would break the
dashboard on the day they do.

### Sequencing, and one non-reason

M18 first. It is additive, lives in one subsystem, and its blast radius is a page
and a setting. M19 moves routing, configuration and every short URL the product
emits, so it goes second, against a surface that is not also moving.

The tempting argument for the reverse order — "separate the hosts before letting
strangers in" — does not survive inspection. New accounts get sessions on the
management host either way; host separation isolates the *link* surface from
cookies, not users from each other. That work is RBAC's, and it already exists.

---

## 2026-07-30 — signup deferred to Phase 2, and two milestones added

Corrects the entry immediately above, which planned self-serve signup as M18.
Signup moves to Phase 2 and the numbering closes up behind it: **M18 is now the
hostname split** (planned above as M19) and **M19 is post-release defect fixes and
the demo seeder**. Phase 1 is still 18 of 20.

### Signup goes where its supporting features are

Called by the person whose product it is, and the reason holds up on its own
terms: the previous entry had already worked out that opening signup admits
tenants rather than colleagues, and that Phase 1 has no mail delivery to verify an
address with. It then proposed shipping it anyway, with the surprise documented on
the form.

Documenting a surprise is weaker than not having one. Every one of signup's
supporting features — invitations, membership in someone else's workspace, a
mailer — is Phase 2, and with them the toggle finally does what its label implies
instead of quietly meaning "let strangers homestead on my server". The design work
above is not wasted: the ceiling rule, the labelling requirement and the
verification problem all carry forward to wherever it lands.

What stays in Phase 1 is the honesty. `SIGNUP_MODE` keeps working exactly as it
does, and the configuration reference now states its actual reach — the JSON API,
not a browser — rather than leaving an operator to infer a signup page from the
existence of a setting called `open`.

### The three defects were found by using the product, not by reading it

A six-dimension review with adversarial verification found thirty findings and
none of these. Standing up a fresh instance, seeding it and clicking around found
all three inside an hour. That is not a criticism of the review, it is a statement
about what each method reaches: the review read code against its own intent, and
all three of these are places where the code is internally consistent and
disagrees with the *product*.

**`links.status` is never set to `expired`.** The value exists in the enum, in the
OpenAPI document and in the UI's filter dropdown, and `redirect/snapshot.go` reads
it. Nothing writes it. The redirect path is correct by a different route — it
compares `expires_at` — so the behaviour users see is right and every management
surface reporting on it is wrong. The fix is to derive effective status in one
place rather than to add a job that writes the column, because a stored status is
stale between the expiry and the next job run, and that window is exactly when
someone is looking at the link asking why it stopped working. Deriving also keeps
one definition instead of two that can drift.

**The `visitors` table is dead, and expensively.** All-twenty-entities-up-front is
a Phase 1 decision and a good one; the cost was supposed to be dead schema. This
row is not free dormancy. It is in `PartitionedTables`, so the hourly job creates
a partition a month for it forever, and in `RetainedTables`, so retention issues
DDL to drop empty partitions of a table that has never held a row. Meanwhile
`is_first_visit` is written `false` on every click under a comment saying the
rollup computes it, and no rollup touches it. A dormant table nobody maintains is
a decision; a dormant table with a monthly DDL bill and a comment describing
imaginary work is drift. The milestone forces a choice rather than prescribing
which one, because "populate them" is defensible the moment something displays
new-versus-returning visitors.

**The deletion notice promises a button.** "It stays restorable for 30 days" is
true about the row and false about the product: no list shows a deleted link, `GET`
by id refuses it, and `RestoreLink` is guarded by `deleted_at IS NULL` on purpose.
`usage.md` says the honest version plainly — recovery is a database operation, not
a button — so this is the flash message contradicting the manual, and the manual is
right. Rewording is the fix. Adding a trash view is a scope change Phase 1 already
declined, and doing it accidentally, as the cheapest way to make a sentence true,
is how scope arrives.

### The seeder earns its place by being a client

`lctl seed` exists and is not this. It writes a hundred thousand links named
`ld0`…`ld99999` straight through COPY with no destination rows, because it is
feeding a load test where the only thing that matters is that the resolver has
rows to find. Pointing a human at that database teaches them nothing about the
product.

The demo seeder creates links through the public REST API. That is the requirement
worth writing down, because writing them straight to the database is faster and
was the obvious first instinct. Going through the API means the seeder cannot
invent a state the product cannot reach, and it exercises alias policy, validation
and tagging on the way past — this is how the prototype discovered that `docs`,
`pricing`, `status` and five other natural demo aliases are reserved, and that a
two-character alias is refused. Backfilled click rows are held to the same
standard: they match what the ingester would have written column for column, so
nobody debugging the dashboard is looking at rows the application could not
produce.

One trap the prototype hit, recorded because it is invisible and the data looks
plausible either way. Generating per-click attributes in a `CROSS JOIN LATERAL`
subquery that depends only on the link and the day lets the planner evaluate it
once per link-day and multiply the result: every click in a day came out with the
same visitor, device, country and referrer, and the only symptom was 18 unique
visitors against 1,200 clicks — a number you have to already be suspicious of to
notice. Volatile draws belong in the SELECT list of a statement whose rows already
exist, which is evaluated per row by definition. `setseed` for reproducibility does
not save you here; it makes the wrong answer stable.

### Better graphs are Phase 2, and are not blocked on data

Every dimension renders today as the same ranked list of value and count. It is
exact and it is flat, and a country breakdown is the case where that is most
obviously the wrong shape: nobody reads `US 1425 / GB 822 / DE 510` and sees a
map. Phase 2 gives each dimension a visualization suited to it, with the current
list one click away — the list is what answers "exactly how many", so it is kept
rather than replaced.

This needs no new column. `link_dimension_daily` already carries clicks and unique
visitors per value per day, which is why the second layer the request asks for is
a rendering decision rather than a schema change. It is Phase 2 because it is
presentation polish on a working feature and gates nothing, not because it is
hard.

Two constraints it inherits rather than gets to choose. Shading by unique visitors
has to keep the caveat those figures always travel with — daily-resolution
estimates, and a multi-day total over-counts anyone who visited on more than one
day — because a saturated color is a much more confident claim than a number in a
table, and laundering an estimate into a fact through visual design is still
laundering it. And the map degrades the way the rest of the geographic UI already
does: no MaxMind database means saying so, not rendering a world uniformly colored
"unknown", which reads as "we checked and nobody is there".

An implementation note, since it constrains the choice: no Node in the image and
no CDN at runtime are standing constraints, so this is an inline SVG world map with
server-computed fills, not a charting library. That is a feature. The fills are
computed where the numbers already are, the page keeps working without JavaScript,
and the click-through to the ranked list is a link.

---

## 2026-07-30 — malicious destination blocking, specified rather than named

It was already planned, in the weakest sense of the word: *Scope by phase* carried
"Malicious link detection | 2" in link management and "Malware scanning | 2" in
security, two rows naming a feature and defining nothing. Nothing anywhere covered
tiering, the cost of an override, logging the attempt, notifying anyone, or a
dispute path. Those are now written down. The phase does not move — Phase 2 was
right — but a row that names a feature is not a plan, and this one had enough
sharp edges to be worth having before someone starts.

### The two threat models must not share a switch

This is the decision everything else follows from. Phase 1 already refuses
non-`http(s)` schemes and private, loopback, link-local, carrier-NAT and
cloud-metadata addresses. That is not malicious-link protection; it stops *this
instance* being used as an SSRF proxy, and the party it protects is the operator.
What Phase 2 adds protects *visitors* from a destination that is hostile to them,
and the party it protects is a stranger who has not clicked yet.

Merging them is the obvious implementation and it is a vulnerability. Build one
"blocked destinations" list with one override path, and the review queue that
exists so an owner can approve a false-positive phishing heuristic becomes the
mechanism by which someone gets `169.254.169.254` approved. The SSRF refusals are
therefore not appealable at any tier, and the plan says so in the same breath as
it introduces appeals — because the natural reading of "the owner can allow
blocked links" is that the owner can allow *any* blocked link.

### Tiers are about the cost of being wrong, not about severity

The request was for likelihood-of-malice tiers where low ones are owner-reviewable
and high ones need a repo change. That maps onto something the codebase already
does: `reserved.txt` and `profanity.txt` are `go:embed`ded, so changing them costs
a rebuild. The useful reframing is that a tier is not a claim about how bad the
link is, it is a statement about what it should cost to overrule the machine.

For a self-hosted product, "requires a repo change" needs defending, since the
operator owns the box and could be forgiven for expecting a checkbox. It is not
upstream gatekeeping — they own their copy and can patch it. It is that the
override becomes a deliberate, reviewable, version-controlled change instead of a
click at 2am on a queue item that looks fine, which is exactly when someone
approves a phishing page. The cost is deliberate friction, applied where being
wrong is expensive.

The consequence is a constraint on what may live in that tier: exact host matches
from a curated list, never heuristics. A heuristic that can reach the
rebuild-to-override tier turns every false positive into a rebuild, and a feature
that makes an operator rebuild to publish a legitimate link is a feature they will
disable wholesale. Confining heuristics to the owner-reviewable tier is what keeps
the expensive tier credible.

### Three traps worth naming before anyone builds it

**The review queue is a delivery mechanism.** Its entire purpose is to hand the
instance owner a URL that a stranger wants them to look at, alongside an argument
for why they should. Rendering it as a live link is the obvious thing and is
wrong; so is fetching it server-side for a preview or a screenshot, which would
reintroduce the SSRF the validator refuses, arriving as a usability improvement.
Defanged text, no fetch.

**Creation is not the only door.** Validation at create is where the thinking
naturally goes, and a destination can be edited afterwards. Update has to run the
same check. Re-checking links already accepted — because a domain can be sold, or
go bad — is a different job with a different cost, and the milestone says so
rather than letting "we block malicious links" quietly imply it.

**Notification is about the dispute, not the block.** The creator learns of a
refusal synchronously, in the response; a notification telling them what they were
just told is noise. What arrives later is the review outcome, plus the owner
learning that something is waiting. That is the asynchronous part, and it is the
part the dormant `notifications` table already fits — `kind` and a jsonb `data`,
no migration needed.

### Reputation feeds stay opt-in, or the product contradicts itself

The obvious way to detect malicious destinations is to ask someone who already
knows — a reputation API. Doing that by default would send every URL any user
creates to a third party, on a product whose README says no telemetry leaves the
box. So: off by default, disclosed plainly when enabled, and never the mechanism
the built-in tiers rely on, because a self-hosted instance with no outbound access
must still get the protection the plan promises.

### Found while validating: a comment citing a file that does not exist

`ValidateDestination`'s doc comment ends "Recorded in SECURITY.md rather than
pretended away", and no `SECURITY.md` has ever been in this repository. The
limitation it refers to — DNS rebinding — is genuinely recorded, in Plan.md's
known limitations, so the substance is honest and only the pointer is wrong. Same
class as the advisory-lock comment the completeness review corrected: a reader who
trusts the comment goes somewhere and finds nothing. Added to M19.

Whether the fix is to repoint the comment or to write the file is left open on
purpose. A `SECURITY.md` is also where vulnerability reporting belongs, and this
project does not have that yet either — which makes it a decision rather than a
typo, and the wrong thing to settle in a defect row.

---

## 2026-07-30 — build-notes, a security policy, and the process written down

### `docs/claude/` is now `docs/build-notes/`

The folder never held anything model-specific. It held the decision log and the
development setup — the notes taken while building, which is what the new name
says. Naming a directory after the tool that happened to be typing dates it the
moment the tool changes, and invites the reading that its contents are
scaffolding rather than the primary record of why this codebase is shaped the way
it is. `decisions.md` is arguably the most load-bearing file in the repository.

Four files referenced the old path; all updated. Relative links inside the folder
(`../adr/`) survive the move unchanged, since both directories sit under `docs/`.

### SECURITY.md exists now, and one consequence of where it lives

Written because a code comment has been citing it since the destination validator
was built, and because a project telling operators to expose it to the internet
should say what it defends and what it does not.

It is in `docs/build-notes/` as instructed. The trade-off, recorded because it is
invisible until it matters: GitHub detects a security policy only at the
repository root, in `.github/`, or in `docs/` — not in a subdirectory of `docs/`.
So the *Report a vulnerability* button and the advisory-creation prompt will not
appear, and the file is found only by someone who goes looking. A one-line
`SECURITY.md` at the root pointing here would restore that, and is not done
without being asked for.

The substance is split deliberately. What is defended is stated as testable
claims, several of which name their tests. What is *not* defended gets the longer
half, because that is the half a reader cannot derive from the source — DNS
rebinding, per-instance limits that fail open, the unauthenticated metrics
listener, the unrotatable pepper, the absence of any malicious-destination
checking, and the audit log that records nothing. Each links to `Plan.md` rather
than restating the trade-off, so the two cannot drift.

The dangling-pointer defect that this fixes was on M19's list. It came off it:
writing the file made repointing the comment in-spec for this change rather than
a deferred finding. That is the rule working, not an exception to it.

### The process is a file now, and it is written for a machine

`workflow.md` collects what has until now been habit: one milestone per commit,
tests before a commit completes, sabotage-verify anything that passes first try,
full validation before a phase PR, re-validate if validation triggers work, and a
documentation pass after validation but before the PR exists.

It is written terse — tables, trigger-then-action, no rationale — because it is
read at the start of every task, and every token it spends is spent again on each
one. That is the opposite of the house style, and the file says so at the top so
that nobody arrives later and helpfully rewrites it into paragraphs. Rationale
lives here instead; the two files point at each other.

The definition of "work" is the load-bearing part, and it exists because
"revalidate if anything changed" collapses under a typo fix. Spelling, phrasing,
formatting and documentation wording do not re-trigger validation. Anything
touching code, SQL, config, tests, generated output or documented behaviour does.
The line is drawn at *could this plausibly change what the software does*, and
when the answer is unclear the rule is to revalidate, because the cost is minutes
and the cost of the other mistake is a phase PR that was never actually validated.

### Deferred findings are a queue, not an empty milestone

Out-of-spec findings needed a destination that is neither "fix it now" nor
"mention it and move on". The instruction was to collect them into a final
milestone for the phase, gated on the owner reviewing each item.

Implemented as a table in Plan.md rather than as a milestone row, because a
milestone that exists before it has contents is a permanently-empty line in the
build status and a number in a ratio that means nothing. The queue becomes the
phase's final milestone when it has approved rows in it. Approval is per item,
which is the part that makes the mechanism work: a batch approval is how a
reported observation quietly becomes committed scope.

The counterpart rule matters as much. An issue that makes the *current*
milestone's own claim false is in spec no matter which subsystem it appears in,
and gets fixed immediately. Without that, "out of spec" becomes a place to put
inconvenient truths, and a milestone can be declared done while something it
claims is untrue.

---

## 2026-07-30 — M18: two hostnames, one listener

### Dispatch on Host rather than ServeMux host patterns

Go's ServeMux has taken host patterns since 1.22, so `mux.Handle("lnk.example.com/{alias}", h)`
is available and was the first thing tried. It was dropped for two reasons. It
would mean registering every route twice, once per host, which makes the route
table the place a split-host bug hides. And its matching is exact against the
request's host including port, so whether a proxy appends `:443` silently changes
which handler runs.

An explicit dispatcher instead: two muxes, one comparison, and a
`CanonicalHost` that lowercases and strips a default port from both sides before
comparing. The two sides of that comparison are written by different people — the
operator types the origin, the proxy decides about the port — and the router must
not care which choice they made.

### The wrong host gets 404, not a redirect

Redirecting `manage.example.com/somealias` to `lnk.example.com/somealias` is the
friendlier behavior and is wrong. The alias namespace is user-controlled, so a
cross-host redirect driven by it is an open redirector operated by anyone who can
create a link. Reserved words do not help: they constrain what an alias may be
called, not where a redirect may point.

The same reasoning applies to an unrecognized host, which gets nothing but the
health endpoints. Serving links under any name pointed at the listener would let
a stranger's DNS decide what this instance publishes — and that is exactly the
decision Phase 2's custom domains has to make deliberately, with verification
behind it.

### Health answers on every host, including ones never configured

Discovered by asking what breaks, which is a better question than what works. The
container's own healthcheck runs `/linkctrl healthcheck` against `127.0.0.1`, a
host no operator will ever configure. Had ops endpoints been host-gated with
everything else, the split would have made every container permanently unhealthy,
and the failure would have appeared in production rather than in a test, because
nothing in the test suite runs the Docker healthcheck.

Load balancers and orchestrators are the same case. Anything that probes does so
by address, not by the name in `.env`.

### `__Host-` was already the right cookie, which is the whole point

No cookie code changed for this milestone. `__Host-` forbids a `Domain`
attribute, so the session was already locked to the host that set it, and the
split therefore makes the link host structurally unable to receive it. That is
the security property the milestone exists for and it cost nothing to obtain.

Which is exactly why it needed a test. A property that holds by accident of an
earlier decision is one a later change deletes without noticing — someone adds
`Domain` to "make cookies work across subdomains", every test still passes, and
the reason the hosts were separated is quietly gone.

The first version of that test asserted over the cookies of `GET /login`. The
login *page* sets no cookie, so the loop ran zero times and the test passed
against a deliberately domain-scoped cookie. Sabotage caught it; without sabotage
it would have sat there looking like protection. It performs a real sign-in now.

### Backward compatibility is the requirement, not a courtesy

0.1.0 is released, so `APP_BASE_URL` and `LINK_BASE_URL` default to `BASE_URL`
and an instance that sets neither takes the single-mux path — the same
registrations, in the same order, with no host comparison at all. Verified by
running the new binary both ways against a scratch database: split, each host
answered only its own paths; single, both trees answered on one host and a
request by IP still resolved a link, as it always has.

The accessors carry the fallback (`AppOrigin`, `LinkOrigin`) rather than every
caller re-deriving it, because tests build `config.Config` as a literal and never
go through `Parse`. Without the fallback, every one of them would silently become
an instance with no dashboard origin, and the CSRF trusted origin would have gone
missing in exactly the tests meant to prove CSRF works.

### What this deliberately did not do

`domains.hostname` is still ignored; resolution still matches `is_default`. The
temptation with a milestone about hostnames is to start matching on that column
and arrive most of the way at per-workspace custom domains without having planned
verification, certificates, or what happens when a workspace points a CNAME at
you and then deletes it. One instance, two hosts, chosen by the operator. Custom
domains remain Phase 2 with their machinery untouched.

---

## 2026-07-30 — M19: three defects, and the seeder that found them

### Effective status is derived, never stored

Nothing ever wrote `expired` to `links.status`. The value existed in the enum, in
the OpenAPI document, in the UI's filter dropdown and in the resolver's snapshot
reader — and no code path produced it. The redirect path was right by a different
route, comparing `expires_at`, so users got the correct 410 while every
management surface reported the link as active and the *Expired* filter matched
nothing.

Writing the column from a job was the obvious fix and is the wrong one: a stored
status is stale between the expiry passing and the job noticing, and that window
is exactly when someone is looking at the link asking why it stopped working.

It is derived in two places, which is a compromise worth naming. `toDomain` is a
true single funnel for output, so Go computes it once there. Filtering has to
happen in SQL or the database returns rows the caller then hides, breaking
pagination counts. So the rule exists as Go and as SQL, and what stops them
drifting is not shared code but a test that asserts they agree — an expired link
must report as expired *and* be found by `?status=expired` *and* be absent from
`?status=active`. Each half was sabotaged separately to prove the test sees both.

Expiry outranks an archived status, matching `Snapshot.Decide`. If the two
disagreed on the both-true case this would be the original defect in a smaller
form.

### The dormant tables stay maintained, which is neither option the plan offered

The plan said `visitors` and `is_first_visit` should either work or leave the
maintenance and retention lists, and framed the status quo as dormancy with a
"monthly DDL bill". Writing the fix showed the framing was wrong twice over.

The bill is a `to_regclass` check per table per month, which is a rounding error.
And removing the table from `PartitionedTables` and `RetainedTables` fails in the
direction that matters: the day something does write to it, rows land in the
default partition, which retention never drops — so the dormant table would
quietly become the one place raw visitor data is kept forever, on a product whose
central privacy claim is that it does not do that.

So both stay dormant and both stay maintained. What was actually defective was
the description: a comment claiming the rollup computes `is_first_visit` when no
rollup touches it. Comments now say dormant and say why. This is the milestone's
"force a choice" working as intended — it did not prescribe which, and the
inspection that came with implementing it produced a third answer better than the
two written down in advance.

### The seeder is a client, and that is the whole design

`lctl seed` already existed and is for load tests: a hundred thousand links named
`ld0`…`ld99999`, written with COPY, no destinations rows. Correct for measuring
the redirect SLO and useless for looking at the product.

`lctl demo` creates its links through `link.Service.Create` — the same call the
REST API makes. Writing rows directly is faster and was the obvious instinct;
going through the service means alias policy, destination validation, tag
creation and the destinations row all happen exactly as they do for a client, so
the dataset cannot describe a state the product could not reach. A seeder that
can invent unreachable states produces a dashboard being debugged against data it
could never have produced.

One field is written directly and the comment says which: the expired campaign's
past `expires_at`. That state is reached by the clock, never by a request, so
there is no client path to imitate.

Click history is written directly too, because the redirect path can only produce
traffic for right now. The constraint there is fidelity rather than provenance:
every column matches what the ingester writes, including `is_first_visit` false,
null region and city, referrers already reduced to a host, a 16-byte visitor hash
keyed on (day, visitor) so the same person is a different hash tomorrow exactly
as a rotating salt makes them, and device/browser/OS strings from the vocabulary
`Classify` emits.

The prototype that found the three defects was a shell script and a pile of SQL
in a scratch directory. It hit a trap worth keeping in the record: generating
per-click attributes in a `CROSS JOIN LATERAL` that depends only on the link and
the day lets the planner evaluate it once per link-day and multiply the result,
so every click in a day shared one visitor, device and country. The only symptom
was 18 unique visitors against 1,200 clicks — a number you have to already be
suspicious of to question. The committed version generates in Go, one row at a
time, where the question cannot arise.

### Standing an instance up found what the review did not

Worth recording as a method note rather than as a fix. A six-dimension review
with adversarial verification read this codebase against its own intent and found
thirty things. Standing up a fresh instance, seeding it and clicking around found
three more inside an hour, and all three are places where the code is internally
consistent and disagrees with the product: a status nothing writes, a table
nothing fills, a message describing a button nobody built. Reading cannot find
those, because there is nothing inconsistent to notice.

---

## 2026-07-30 — planning: M20, a redirect for the root of the link domain

### Phase 1, because M18 is what created the gap

The request arrived after Phase 1 had been closed at 20 of 20, which is the
moment to be careful: new scope is easiest to justify when the phase is already
open. The test applied was the one in workflow.md — is this a gap in something
Phase 1 already claims, or a Phase 2 feature arriving early?

It is the first. M18 gave the instance a second public hostname, and left that
hostname's root answering `404`: `/{alias}` does not match `/` (verified rather
than assumed), and the dashboard routes that used to answer there moved to the
other host. So a deployment that takes up the feature Phase 1 just shipped gets a
public domain whose front page is a bare error, and trimming a short link back to
its domain — which people do — finds nothing. M18 is unreleased, so this is
cheaper to fix before it ships than to ship and document.

### "The domain owner" does not exist yet, and saying so is the honest version

The request specified the domain owner, or someone with a specific permission. In
Phase 1 there is no domain owner to check: the default domain is a single
instance-wide row with a null organization, deliberately, because Phase 1 has one
hostname and one workspace per user. Implementing an ownership check against that
would be a check that always resolves to the same person while looking like real
authorization.

So Phase 1 gets the second half of the request — a new `domains.write` permission,
granted to owner and admin — and Phase 2 gets the first, as a scope row of its
own, when a workspace can bring its own hostname and there is an owner to be. The
permission is the durable part either way: when per-domain ownership arrives, it
becomes the check that a workspace administers *its* domain rather than any.

### The refusals this inherits, written down before anyone builds it

**It validates like any other destination.** Same `ValidateDestination`, same
scheme allowlist, same private, loopback and metadata refusals. A root redirect
that skipped them would be a cleaner SSRF than the one the validator exists to
prevent, because reaching it needs no link and no alias — just the bare hostname.

**It is refused on a single-host deployment rather than ignored.** There, `/` is
the dashboard. A root redirect would take the dashboard away from the person
setting it, and the failure would look like the product breaking rather than like
a setting doing what it says.

**It is cached.** It lives on the redirect tree under the same 20ms budget as an
alias, and the root of a link domain is a page crawlers and scanners ask for
constantly. Reading a row per request would put a database round trip on the hot
path for the one URL most likely to be probed.

**Unset stays 404.** No default page, no "powered by". An instance that says
nothing about itself is a legitimate choice and the current behaviour.

**302, not 301.** The same reason the whole product uses 302: a 301 cached in
browsers and intermediaries cannot be recalled, and of every destination here
this is the one most likely to be repointed later.

**Root visits are not clicks.** There is no link, so there is no `link_id`.
Attributing them would mean inventing a synthetic link to hang the rows off,
which is a row the product could not otherwise produce — the same rule the demo
seeder is built around.

---

## 2026-07-30 — M20 built, and 0.1.0 absorbs everything

### The cycle between the handler and the service, resolved without a setter

The root handler reads through the link service, and the service invalidates the
handler's cache when the setting changes. Each needs the other. A setter on the
service would work and would also mean a service that is only correct if someone
remembers to call it.

Instead the handler is constructed empty, passed to the service as its
invalidator, and has its loader assigned afterwards — two statements, no partially
constructed service, and the wiring is visible in one place in main.go. The test
fixture does the same three lines, deliberately: a fixture that wires it
differently from production would be testing a shape that does not ship.

### Caching it is not premature

The bare domain is the URL crawlers, scanners and link-preview bots ask for most,
and it sits on the redirect tree under the same 20ms budget as an alias. Reading
a row per request would put a database round trip on the most-probed path in the
product, for a value that changes approximately never.

TTL alone would have been enough for correctness and wrong in practice: the
person most likely to reload that page immediately is the operator who just
configured it, and showing them the previous answer is how a working feature gets
reported as broken. Hence invalidation on write, with the TTL as the backstop for
an invalidation that never arrives — the same shape as a link snapshot.

The test asserts the second write is visible immediately, and sabotaging the
invalidation call fails it. Without that, the TTL would have hidden the defect
behind a one-minute wait that no test is slow enough to notice.

### Reading is not gated the way writing is

`domains.write` guards the change; reading needs only `links.read`. The value is
published to anyone who visits the bare domain, so hiding it from a viewer inside
the product would protect nothing while making the account page lie to them about
what the instance does.

### The permission had to be granted explicitly

The seed migration grants the owner role "everything" with a `SELECT ... FROM
permissions`, which ran once, at its own version, against the permissions that
existed then. A permission added in a later migration is therefore granted to
nobody unless that migration says so. Easy to miss, and the symptom would have
been the owner of a fresh instance being unable to use a feature that works for
everyone who upgraded — or the reverse, depending on which way the omission fell.

### 0.1.0 absorbs everything, because 0.1.0 never happened

There is no `v0.1.0` tag and nothing has reached `main`. Keeping an `[Unreleased]`
section describing changes to a release that was never published would mean
publishing a changelog whose first version is immediately wrong: it would list an
expired-status defect as *fixed* in a version that never shipped it, and describe
hostname splitting as an addition to something nobody ever ran.

So the entries were folded into 0.1.0 in the sections they belong to rather than
appended as an Added/Fixed block. The three defects are not "fixed" in a first
release — they simply never existed in anything anyone could install, and the
correct behaviour is now described as the behaviour. The two capabilities became
a new "Hostnames" section. What survives as a limitation is what is still true
after all twenty-one milestones: no signup page, and dormant tables named as
dormant.

## 2026-07-31 — 0.1.0 tagged

### The tag goes on `main`, not on the phase branch

`main` is `phase-1-mvp` plus the merge commit from PR #1, and the two trees are
identical — `git diff origin/main origin/phase-1-mvp` prints nothing. Either
commit would build the same artifacts, so the choice is about what the tag means
rather than what it produces. `main` is the branch the project publishes from; a
release tag hanging off a phase branch would make the published history depend on
a branch that exists in order to be merged and eventually deleted.

### The status lines had to change before the tag, not after

`README.md` said "0.1.0 not yet tagged" and `Plan.md` said the version "has never
been tagged or pushed". Both were true right up to the moment the tag existed —
and a release is built *from the tag*, so tagging the tree that still carried them
would have published a 0.1.0 whose own README says 0.1.0 does not exist. The
documentation pass workflow.md puts before a PR applies to a tag for the same
reason: the artifact is the claim, and it carries its own documentation with it.

Nothing else needed a version written into it. `VERSION` in the Makefile is
`git describe --tags`, so the version the binaries report is derived from the tag
rather than restated anywhere a copy could drift.

This entry corrects "0.1.0 absorbs everything, because 0.1.0 never happened"
above, which recorded that there was no tag and that nothing had reached `main`.
Both halves were true when written and both are now false; the entry stays as it
is, because this file is append-only.

## 2026-07-31 — Phase 2 planned

Seventeen decisions, two review milestones, and a change to how planning
documents are shaped. `Plan.md` records what was decided; this records why.

### The plan is split: contract here, definitions of done per milestone

Phase 1 kept milestone detail in `Plan.md`, which worked at three detailed
milestones and would not have at twenty-seven. Every session reads `Plan.md`, and
almost none of them need M34's condition vocabulary. So `Plan.md` keeps the
contract — scope tables, the ordering table, decisions, exclusions — and each
milestone's definition of done moved to `phase-details/m<N>.md`.

Named for the number rather than the subject so a milestone id resolves to a path
without consulting an index. Building M27 means reading a 44-line table and a
43-line file, not 1,000 lines of specification for twenty-six milestones that are
not being built today. Phase 1's own M18–M20 detail moved to `phase-1.md` for the
same reason, which also makes the rule uniform rather than a Phase 2 convention.

Rules every milestone inherits — the SLO re-measurement, the privacy stance, the
permission-seeding pattern, sabotage discipline — are stated once in that
directory's README. Repeating them twenty-seven times would guarantee they drift.

### Two reviews, because Phase 1's found what milestones did not

Phase 1 ran two adversarial reviews. The first confirmed 30 findings and, as the
build status put it, was what called the phase complete "rather than the milestone
counter reaching its end". The second confirmed 71, seven of which blocked a tag,
on a branch that felt finished. Both found the same shape of defect: an invariant
enforced on one path and not on its sibling — something no single milestone's
definition of done can catch, because each milestone was internally consistent.

Phase 2 is larger, so it gets two: M32.5 after the substrate and collaboration
work, while the redirect engine is still unwritten and a finding can still change
its design; and M44.5 before the close, because M32.5 cannot review code that does
not exist yet, and the phase's most dangerous work — rules on the hot path, a
durable counter, outbound HTTP, verified-domain serving — all lands after it.

Numbered `.5` following M0.5, so inserting them renumbered nothing.

### `closed` keeps meaning closed

The first answer was that an invite should be able to create an account on a
`closed` instance, with that account restricted to the inviting organization. The
instinct was right and the mechanism was wrong, in two ways.

The ceiling rule above exists because a session that can cause account creation
makes a closed instance's guarantee "only as strong as the least careful browser
tab" — and it is explicitly the same shape as the rule that an API key can never
hold `apikeys.*`, which the same round of decisions chose to preserve. Letting
invites through would have broken one invariant an hour after upholding its twin.
It would also have collapsed the enum: if `closed` admits invited accounts, it
differs from `invite` only by a flag.

So `closed` admits no new account by any path, and onboarding someone new costs
one `.env` edit — a deliberate, reviewable act, which is the property the ceiling
exists to force.

The restriction instinct survived, on a better axis. Rather than an account class
derived from the signup mode at creation time, org creation became an ordinary
permission, `orgs.create`, granted by default to self-registered users only. One
mechanism instead of two, no rule needed for what happens to accounts created
under a mode that later changed, and it satisfies the requirement attached to
membership-only invitations: an invited colleague's account is *capable* of owning
an organization, so nobody ever needs a second account. It is also the call site a
future entitlement check would hang on, which is why the possibility of charging
for organizations became a Phase 3+ scope row rather than either silence or
speculative machinery.

### Automation cannot disable a link, because disabling is archiving

The draft gave automation a `disabled` action. Challenged on what it would be
for, the code answered: `snapshot.go` maps `archived` and `disabled` to the same
outcome — `OutcomeNotFound`, deliberately, so a scanner cannot tell them apart.

A fourth status with no behavioural difference would buy nothing, and it would
cost something real: restore targets archived, so automation would have been the
first thing able to put a link into a state the interface offers no way out of.
Phase 1's record that nothing sets `disabled` stands, and the action set is
notify, webhook, archive.

### Returning visitors, without a cookie

The rules row lists cookies and returning visitors as conditions. Analytics here
is cookie-free and visitor hashes die with the daily salt, so the honest options
were a first-party routing cookie — a positioning change — or narrower semantics.

Returning-visitor ships as "seen earlier today", read from the same daily-salted
hash the ingester already produces, expiring with the day. A visitor from
yesterday is new again, and the UI says so; an estimate described precisely is
worth more than a better number that requires abandoning the cookie-free claim.
The cookies condition is refused outright with a reason code, and the scope row is
annotated, so a thirteen-condition row is never quietly shipped as twelve.

### Audit retention defaults to forever, and says so out loud

Both defaults are a data-loss policy. A finite window means an upgrade silently
starts deleting history an operator assumed permanent; keep-forever means
unbounded growth. The first failure is invisible and irreversible, the second is
visible and recoverable, so keep-forever won — but only paired with an obligation
to make the growth observable: a metric and alert recipe in M21, an owner
notification in M22, emailed when a mailer exists.

### A mailer, after all

The record already said one was Phase 2 — "invitations, membership in someone
else's workspace, a mailer". The draft plan had proposed shipping mail-free and
citing a Phase 1 entry as authority, which misread it: that entry recorded the
*absence* of a mailer as the reason signup stayed closed, not a decision never to
have one. With SMTP optional and off by default, `open` signup can verify an
address before an account is usable, which is the difference between a toggle and
an abuse surface.

### The link gate is now a script, not a hope

`workflow.md` has listed "every relative link and anchor resolves" as a commit
gate since Phase 1, and nothing enforced it — writing this plan required building
the checker from scratch to satisfy a rule already on the books. It is now
`scripts/check-links.sh`, wired into the release gate.

Its first run passed, which was wrong: `git ls-files` sees only tracked files, and
the new documents were untracked. Its second run rejected twenty correct anchors,
which was also wrong: GitHub replaces each space in a heading with its own hyphen
and does not collapse runs, so an em-dash — stripped as punctuation, its
surrounding spaces kept — yields two hyphens, not one. Both failures are recorded
because both are the kind a checker that "passes" quietly hides.

## 2026-07-31 — two development instances, and the link gate's third failure

Development now runs two stacks: `demo`, long-lived and refreshed only when a
milestone is validated, and `test`, disposable. Both are compose projects with
their own volumes and ports; `docs/dev-notes/instances.md` is the reference.

### One stack could not be both

The demo is worth opening only if it holds plausible recent history and the last
thing you did to it is still there. Testing means dropping the database, seeding
five million click events, rolling migrations back, and pointing a load generator
at the result. Sharing one stack means the demo dies about weekly, and wanting a
clean database starts with deciding whether anything in the current one mattered.

The demo's volume is never recreated, which buys a check nothing else here
performs: each `make demo-update` applies that milestone's migrations to a
database that has been through every previous one. CI and the test instance both
start empty, where a migration that cannot survive existing data passes.

### The refresh replaces the data rather than accumulating it

`lctl demo --reset` truncates `click_events` and rebuilds the catalogue, so
anything created while using the demo is gone at the next milestone. Preserving
it was considered and rejected: the dataset's value is that it is generated
relative to the day it runs, so the charts end today. Data that accumulates
across milestones ages instead, and a demo whose history trails off three weeks
back is worse than one that is a month old and says so. Between milestones,
everything persists.

Keeping it would also have meant changing what `--reset` means — deleting clicks
by `link_id` instead of truncating — and that is product behaviour changed to
suit a development convenience.

### `test` is the default, and the demo needs a word typed to break

Every target acts on `test` unless told otherwise, and the destructive ones
refuse `INSTANCE=demo` without `CONFIRM=demo`. A default that followed whichever
stack was running would put `make db-reset` one typo from the instance being
used. An unknown instance name is refused rather than defaulted, so `INSTANCE=dmeo`
cannot quietly build a third stack that presents as the demo having lost its data.

`make demo-update` additionally refuses a dirty tree. The demo exists to show the
last validated milestone, and a build carrying uncommitted work is not one.

### Two defects the wiring exposed

**`--env-file` does not change what the container gets.** It redirects the file
compose interpolates *from*; `env_file:` in the service is a separate path, and a
literal `.env` there hands both instances the same configuration while each
appears to have its own. It is now `${LINKCTRL_ENV_PATH:-.env}`.

**Overriding only the DSN put `lctl` in two instances at once.** The migrate,
seed and demo targets run `lctl` on the host, where `config.Load` reads `.env`
from the working directory regardless of which instance was meant. With just
`LINKCTRL_DATABASE_URL` overridden, `lctl demo` wrote to the test database and
reported the demo instance's URL — and `lctl apikey` would have minted keys under
a pepper the target instance's server does not hold, so they would never
validate. The instance's file is now exported before the command runs; godotenv
does not overwrite variables already set, so it wins.

### The test instance stops itself; the demo does not

Two mechanisms, because they answer different questions. `LINKCTRL_RESTART=no`
answers "should a reboot bring this back" — for a disposable stack, no. A systemd
timer answers "is anyone using it", every five minutes, stopping it after thirty
idle minutes. The demo is excluded from both: the script refuses that instance
outright, and it keeps `unless-stopped`, because an instance that exists to be
looked at has to be there when the browser is pointed at it.

Idle is the hard part, and two signals that look obvious are wrong. The container
log is not one: at debug level an idle app logs one rollup line a minute and
requests are not logged at all, so the log says nothing either way. Postgres
transaction counters are not one either: the rollup job and the health probe
commit on a timer forever, so the counter always moves and nothing is ever idle.

What works is the request histogram's count, summed over every surface *except*
`ops` — the surface the container's own healthcheck hits every ten seconds. That
excludes the machine's own noise and counts only what somebody did. It needs the
metrics listener reachable from the host, so the development override now
publishes it on loopback; the base file still publishes nothing, and the
production procedure does not apply the override.

The metrics counter alone would have been wrong too. `go test`, `lctl` and a
server from `make run` talk to Postgres directly and never touch the app, so a
process check on this host is a second signal, and a keep-file is the escape
hatch for something long and unattended. A failed scrape counts as activity: a
measurement that did not happen is not evidence of idleness, and guessing wrong
stops a stack somebody is using.

Then a state nothing had considered: `make migrate-status` on a stopped instance
starts Postgres and Redis and leaves the app down, and the first version called
that "not running, nothing to do". The half-stack it creates was the one state
the timer could never clean up. Any running service now counts, and with the app
down the only signal that can hold it up is a process on this host.

### The link gate failed a third way, and this one was not the anchors

`scripts/check-links.sh` reported one to three broken links per run, a different
set each time, all of them false. `slugs | grep -qxF` lets `grep` exit at the
first match, the writers upstream take SIGPIPE, and `pipefail` reports 141 for a
pipeline that succeeded — measured at 60 failures in 300 runs. It is a here-string
now.

Worth recording because of where it hid: the gate had been listed since Phase 1,
was finally implemented while planning Phase 2, and still did not work. Its first
run passed for the wrong reason, its second rejected correct anchors, and its
third was a coin flip. A gate is not enforcing anything until its failures have
been explained rather than fixed.

### shellcheck stops being a habit and becomes a gate

Eight shell scripts now carry real behaviour — the instance guards, the idle
detector, the release gate — and shellcheck ran only when someone remembered.
That is the exact shape the link gate failed in: listed, believed, unenforced.
`make shellcheck` now runs inside `make check` and as a CI lint step, and the
two findings it was sitting on (an unguarded `cd` in check-links.sh, unquoted
expansion in release-check.sh) are fixed.

Unlike golangci-lint it is not pinned: its output is stable across minor
versions, and its scripts-only surface means a surprise finding is a two-line
fix rather than a red build across the repository. The gate was sabotaged before
being trusted — a planted unquoted `rm -rf` fails it, its removal restores it.

## 2026-07-31 — dark mode added to the plan as M24.5

Owner request, made after the plan was finalised. It enters the same way Phase 1
absorbed M18–M20 after its review: scope changes arrive as numbered milestones
with their own definitions of done, not as riders on existing ones. `.5` because
the ordering table's dependency edges reference milestone numbers, so inserting
is cheap and renumbering is not — M0.5 set the precedent, M32.5 and M44.5
continued it, and this is the first non-review use of it.

### Why before M25 rather than at the end of the phase

The plan's own ordering rule: substrates before consumers, nothing retrofitted
into shipped features. Seven later milestones build significant UI — M25, M31,
M37, M38, M41, M43, M44. Landed first, dark mode is a token set plus a template
scan those milestones build inside; landed last, it is a restyle of everything
the phase produced. The scan is what makes the early placement stick: a test
that fails on raw palette utilities turns "renders in both themes" into a
property later work cannot merge without, instead of a review item someone has
to remember.

### Why a cookie rendered by the server, not a client-side toggle

The CSP decides this. Every client-side theme toggle needs a render-blocking
script in the head that reads storage before first paint, or the page flashes
the wrong theme; this UI ships no inline scripts and has no waiver to add one.
A cookie the server reads at render time produces the right document from the
first byte — the flash is not suppressed, it is unrepresentable. It also works
logged out, on the login page, and costs no schema: the preference is
per-browser ergonomics, not account data, so it is deliberately not a `users`
column. A visitor who has chosen nothing gets `prefers-color-scheme` with no
cookie at all.

### What deliberately stays light

`/docs`: Swagger UI is a checksum-pinned vendored asset, and carrying a themed
fork of its stylesheet is a standing maintenance cost with no product in it.
Email, when M26 exists: client support for dark-mode CSS in mail is chaos, and a
readable light email is better than a broken dark one. QR codes, when M41
exists: scanners have opinions about contrast, so codes stay dark-on-light in
both themes — the theme is chrome, never output.

## 2026-07-31 — feature intake written down, and the review slot moves to X.9

The dark-mode addition earlier today was the first post-finalisation scope
request, and placing it meant re-deriving conventions scattered across the
template, the phase-details README, two precedents and the ordering rule. That
derivation is now [planning.md](planning.md): establish absence, decide the
phase, place by number, write the five artifacts, verify. workflow.md gained the
trigger pointing at it. The guide exists because the second request should cost
reading, not archaeology.

One rule in it is new rather than recorded: **`X.9` is reserved for scheduled
reviews**, and the two Phase 2 reviews moved — M32.5 → M32.9, M44.5 → M44.9. A
review's range covers everything numerically below it, and reviews at `.5` left
that property to luck: scope inserted at M32.6 would have landed *after* the
mid-phase review it needed to be inside. With reviews capping the band, any
insertion between `X` and `X+1` — `.1` through `.8` — is inside the nearest
following review's coverage by construction. M24.5 keeps its number; it now
reads as what it is, an insertion below the M32.9 cap. Phase 1's M0.5 predates
the reservation and stays where history put it; earlier entries here that say
M32.5 and M44.5 were true when written, per this file's rules.

## 2026-07-31 — docs/ reorganized around its reader

One criterion, now stated in [docs/README.md](../README.md): a file sits at
`docs/` root only if someone *running or using* the product reads it; people
*changing* the product read the subfolders. Applying it moved exactly one file:
`build-notes/SECURITY.md` → `docs/SECURITY.md`. A vulnerability-reporting
policy is for reporters, who should not need to know the repository keeps its
build notes to find it — and GitHub's security tab discovers `docs/SECURITY.md`
but not a file two levels down. The *Not in Phase 2* row about a root-level
`SECURITY.md` pointer stays true: the repository root still carries none, and
this move was asked for as part of the docs reorganisation, which is what that
row said to wait for.

Everything else already sat where the criterion puts it. Two root files were
kept deliberately rather than by default, and the reasons are recorded in the
map: releasing.md (versions, upgrade and rollback are the operator's contract,
even though cutting a release is the maintainer's) and slo.md (the performance
promise is only a promise if its evidence is where a host can read it).

## 2026-07-31 — the phase loop written down

Phase 2 is twenty-eight milestones whose definitions of done are already
written. What was not written is the cycle around them, so every session
re-derived it: which milestone is next, what to check before starting, what
order the gates run in, and when to stop. That cycle is now
[phase-loop.md](phase-loop.md), triggered by "Work on Phase", and the owner asked
for it precisely so the derivation happens once.

### Validation is its own step, and it never edits

The obvious loop is build–commit–repeat. It is wrong here because the
definitions of done were all written on the same day, before any of the code
they describe existed. By M35 the tree will have moved twenty-five milestones
away from what M35's file assumed: a bullet may name a test that was renamed, a
discharge may point at a Plan.md row a later milestone rewrote, or an earlier
milestone may have already built part of it. Discovering that mid-build turns
into unplanned scope, because the natural response to "the plan is slightly
wrong" while already deep in the code is to fix it quietly. So validation runs
first, produces a verdict and notes, and is forbidden from touching code — which
means a failed validation surfaces as a question to the owner rather than as a
silent amendment.

One consequence is recorded in the file because it is an exception, not a
derivation anyone should re-make: an open deferred finding that would make the
*current* milestone's claim false is in spec by workflow.md's standing rule, and
gets fixed inside the milestone without individual owner approval. It is the
only route out of that queue that skips approval, so it is named in the commit
message when taken.

### Why demo-update lands between commit and push

workflow.md already required `make demo-update` after a milestone; the loop
fixes *where*. It refuses on a dirty tree, so it cannot precede the commit, and
its refusal is a real completion signal rather than a formality. Putting it
after the push would mean the failure arrives once the work is already
published. Between the two, it is the last gate that can still fail privately.

### The resume note is untracked, and holds nothing durable

Recovery after a stop needs working state — which milestone, which step inside
it, what was already verified, what cost real effort to learn. None of that
survives the milestone, and all of it has somewhere better to live the moment it
does: status in the phase-details README, rationale here, out-of-spec findings
in the queue, scope in Plan.md. A tracked file would therefore duplicate the
status table this repository deliberately keeps in exactly one place, and would
have to be committed with every milestone to keep the tree clean for
`make demo-update` and `make release-check`.

`.current-task.md` is gitignored instead. `git status --porcelain` skips ignored
files, so both of those gates stay green while it exists, and it can be rewritten
as often as the work changes without a single commit. The cost is that it does
not survive a fresh clone, which is the right trade: a note describing work in
flight has nothing to say to a machine that has not started.

### Where it stops

Two boundaries, both deliberate. The loop never starts the next phase — a phase
boundary is where scope, versioning and the owner's judgment all change at once,
and M45 ends with a tag and a PR that are the owner's to approve. And it never
answers its own questions: an unanswered decision prompt stops the loop rather
than resolving into a default, because the failure mode of an unattended builder
is not a wrong keystroke, it is a plausible assumption compounded over twenty
milestones.

---

## 2026-07-31 — Six decisions taken ahead of an unattended run

The loop stops at any question it has not been given an answer to, which is the
property that makes it safe to leave running. It is also the property that makes
an unattended overnight run mostly idle time: M21 is mid-build, and reading the
five milestone files after it turned up six choices the loop would have had to
stop for. They were put to the owner in one sitting and answered before the run
started. Recorded as D18–D23 in Plan.md, in a second table — the first is headed
*taken before the plan was finalised* and planning.md's restraint list forbids
editing it, so post-finalisation decisions continue the numbering in a table of
their own rather than rewriting history.

This is the same move as writing down the phase loop: a decision made once, in
advance, costs a conversation; the same decision made twenty times in the middle
of twenty builds costs a stall each time and drifts.

### Delegability stops being a per-permission conversation (D18)

The `audit.read` call earlier today — non-delegable, because reading the audit
trail is the one place a `/24` is tied to a named actor — was answered on its
merits, for one permission. Seven more permissions arrive this phase (M27, M28,
M31, M38, M39, M44 and M22 if it needs one), each carrying the same question.

The rule generalises what that answer already assumed. A permission is
non-delegable when reading it exposes an actor's identity tied to network data,
or when holding it lets a key widen its own reach. The second limb is the older
one — it is why `apikeys.*` is not a key scope (D9). The first is new vocabulary,
and naming it is the point: before today the non-delegable set was about
escalation only, and `audit.read` joined it for confidentiality. Leaving that
unstated would have meant re-deriving it at every future permission and quietly
getting a different answer at one of them.

The mechanism does not change. `NonDelegableScopes` remains the only thing
enforcing it: endpoints authorize on the permission like every other endpoint,
and no code anywhere asks whether the caller holds a session. Flipping a
permission in either direction stays a one-line map edit, which is what keeps the
rule cheap to revise if it turns out to be wrong.

What each milestone owes is one line: which limb it matched, or that it matched
neither. That is a record, not a decision, so it does not stop the loop.

### The growth alert cannot itself require configuration (D19)

D5 keeps audit history forever until an operator configures otherwise, on the
argument that an upgrade must never silently delete history someone assumed
permanent. That argument only holds while unbounded growth is *visible*. An
instance that keeps everything forever and warns nobody has not been given a safe
default; it has been given a deferred problem.

So the threshold defaults to 5 GB of audit partitions rather than to off. The
symmetry with the retention default (0 = forever) is tempting and wrong: the two
defaults protect against opposite failures. Retention defaults to inaction
because acting without being asked destroys data. The alert defaults to acting
because *inaction* is what leaves the operator uninformed. A number that fires on
a large instance and never on a small one is a nuisance at worst, and the metric
and the documented Prometheus recipe exist either way.

### A reconnecting subscriber has no way to catch up (D20)

Redis pub/sub does not replay. When M23's subscriber drops and reconnects, the
invalidations published during the gap are simply gone, and the replica cannot
know how many it missed or which keys they named. Both available responses accept
staleness; they differ in when it ends.

Flushing both in-process tiers on reconnect ends it at reconnect. Relying on TTL
ends it whenever each entry happens to expire — bounded, but bounded by a number
chosen for a different purpose, and invisible while it lasts. M23's own risk
statement is that a subscriber which stops delivering looks exactly like nothing
having changed; a flush is the one action that makes the difference between those
two states observable in the cache rather than only in a log.

The cost is a cold cache after every Redis blip, which is a latency spike on a
dependency the project has already decided is optional. Correctness never depends
on the cache being warm, so this trades the failure mode that hides itself for
the one that shows up in a graph.

### The light theme is allowed to move (D21)

M24.5 puts every template color behind semantic tokens and requires each pair to
meet WCAG AA in both themes. Today's light palette was never audited against that
bar, so some pairs will not clear it — the status colors especially, since amber
on white is the usual offender.

Freezing light and building dark around it would have kept the diff smaller and
the milestone's claim false: an AA requirement that exempts half the themes it
covers is not an AA requirement. It would also have produced deferred rows whose
fix is a second pass over the same templates the milestone had just swept, which
is the retrofit that ordering M24.5 before the UI run exists to avoid.

So the light value moves where AA needs it to, and each change is recorded beside
the token definition — next to the contrast figures the milestone already
requires, where a reader comparing today's dashboard to yesterday's can find out
why. The dashboard will look slightly different afterwards. That is the intended
outcome, not a regression to report.

### Last-used, with a way to pin it (D22)

M25's deterministic default only bites after M27 creates a second membership, but
the resolution rule has to be written before the four `GetDefaultWorkspaceForUser`
call sites are converted. Oldest-membership-wins is purely derived and needs no
state, which is its whole appeal and also its problem: a user who works in one
org and is a member of another lands in the wrong one on every login, forever,
with no way to say otherwise.

Last-used costs one piece of persisted state and matches what the switcher is
for. The owner added the escape hatch that makes it predictable rather than
merely convenient — a setting that pins an explicit workspace, whose control
defaults to *Last-Used*, so the derived behaviour stays the default and the
override is available to anyone it annoys.

Neither part changes anything for today's users: with exactly one membership,
last-used and only-used are the same workspace, and M25's claim that the
milestone is byte-identical for existing instances is unaffected.

### Mail goes through an outbox (D23)

M26 named the delivery mechanism as a decision and left it open: a scheduler job
over a small outbox, or direct-with-retry. Both keep sends off the request path,
which was the constraint that mattered for the redirect SLO.

The consumers decide it. Invitations and address verification are not
notifications a user can shrug off and re-trigger — an invite that vanishes
because the process restarted mid-retry leaves someone locked out of an org with
no record that anything was attempted, and the failure is invisible on both ends.
In-memory retry has no answer to that beyond hoping deploys do not coincide with
sends.

An outbox costs an additive migration and a job on the scheduler that already
runs partition maintenance, and buys durability plus an inspectable record of
what was attempted. It also gives the mail-free degradation path something
concrete to assert against, since "the outbox stays empty when no mailer is
configured" is a testable claim in a way that "nothing was sent" is not.

---

## 2026-07-31 — M21, the audit log gets behavior

The table shipped in Phase 1 with nothing writing to it. Five later milestones
emit events and one reads them, so the writer is built first — retrofitting
emission into shipped features would mean touching each of them again, and the
first version of the trail would be whatever those features happened to record.

### The address never reaches the caller

`Record` takes the client address off the context and reduces it to a prefix
itself, rather than accepting a prefix from its caller. That is one line either
way and the difference is that no caller ever holds a full address destined for
this table, so no caller can forget to reduce one. The privacy stance is a
property of the writer, not a convention its callers are trusted to follow.

Getting the address there needed a carrier. Services take an `*auth.Identity`
and no request, so without one, every service method that will ever write an
audit event grows an address parameter and so does every caller of those
methods — the retrofit this milestone exists to avoid, arriving through the
back door. It lives in `internal/auth`, beside `AnonymizeIP` and `Identity`,
because the HTTP layer cannot be imported from below it. `httpx.ClientIPFrom`
stayed as the name the handlers already used and now delegates.

Not a field on `Identity`, which was the tempting alternative: the address is a
property of the request, not of who is making it. The same identity acts from
different networks, and `Identity` is also constructed outside a request
entirely, by the CLI.

### `audit.read` matches D18's first limb

Reading the trail exposes an actor's identity tied to a `/24`, which is the
disclosure limb of the rule, so it is non-delegable. It was answered on its
merits before D18 generalised it; the answer did not change.

The mechanism is worth stating because it is the whole of it.
`NonDelegableScopes` is the only thing making this session-only. The endpoint
authorizes on the permission exactly like every other endpoint, no handler or
service asks whether the caller holds a session, and there is no second response
shape that redacts `ip_prefix` for keys. Reversing the call is deleting one map
entry — which is the property that makes the rule cheap to revise if the
operational case for machine export ever outweighs the disclosure.

That deliberately leaves no automated export path in this phase. It is the known
cost, and the honest one: an operator who wants the log in a SIEM today has to
read it with a session or wait for M42's webhooks.

### Retention became a policy per table, not a window and a list

`DropExpiredPartitions` took one window and a list of tables it applied to, and
`audit_logs` was exempt by being absent from that list. That worked for exactly
as long as there was one window. Adding a second by adding a second list and a
second call would have left the two policies expressed differently from each
other, and the exemption still expressed as an omission.

It now takes a `RetentionPolicy` — table to days — and a table absent from the
map is never touched. Exemption by omission still works, but now it says so:
retention deletes only where it was told a number. The two defaults differ on
purpose (395 days and forever, D5), and the test that proves they are separate
inverts them, so a policy quietly using one number for both cannot pass by
coincidence.

### The growth metric is measured by the job, not at scrape time

`linkctrl_audit_log_bytes` is set by the hourly maintenance pass rather than by
a collector that queries Postgres when Prometheus scrapes. `/metrics` has to
keep answering while the database is unwell — it is the endpoint an operator
scrapes to find out that it is — and a collector that opens a connection makes
the scrape fail exactly when it is most wanted. The cost is up to an hour of
staleness, which does not matter for a series whose entire purpose is a trend
measured in days.

It is measured on every replica and outside the leader lock, unlike the work in
that pass. A gauge only the leader wrote reads as zero on every follower, and
whether the alert fired would then depend on which replica answered the scrape.
It is a catalogue read rather than a scan, so paying for it N times is cheap.

Summed over the partitions, too: a partitioned table has no storage of its own,
so `pg_total_relation_size` on the parent answers 0 however much is underneath.

### A failed audit write does not fail the change

The record is written after the setting is saved and outside its transaction,
and a failure is logged at warn rather than returned. The operator asked for the
change; refusing it because the record of it could not be written trades a
missing log line for a setting that did not take effect, which is the worse of
the two. The warning is what keeps the gap visible rather than silent.

The previous value is read *before* the write, because "the root now points at
example.com" does not tell a reader whether that was a change or a no-op, and a
moment later the old value is unrecoverable. That ordering has its own test —
reading it afterwards returns the new value for both fields and looks correct.

---

## 2026-07-31 — M22, the inbox and what it is not

The `notifications` table shipped dormant in Phase 1. Giving it behavior is a
small milestone whose main risk was never the code: the judges flagged
over-building, and a notification system is the archetypal feature that grows a
preferences matrix, a digest scheduler and three transports before anyone asks.

So the fence is explicit and it held. There is no push, no per-event preference
machinery, no general notification centre, and no endpoint that *creates* a
notification. That last one is worth stating as a rule rather than an omission:
a notification records something the system observed, and an API a caller can
post into is an inbox of assertions rather than a record. Email stays M26's
concern, reading from its own outbox.

Zero DDL, which is the dormant-table rule, and it now has a test rather than a
convention behind it — the column set of `notifications` is asserted to be
exactly what 00600 created. The next milestone that wants somewhere to put a
field will meet that test before it meets a migration, and the answer is the
`data` jsonb.

### The warning defaults to on, and the retention window defaults to off

Both are audit-log settings and their defaults point in opposite directions,
which reads as an inconsistency until you name what each protects against.

Retention defaults to inaction because acting unasked destroys data (D5). The
warning defaults to acting because *inaction* is what leaves the operator
uninformed (D19). Keep-forever is a safe default only on the condition that the
instance nobody configured is the instance that gets warned — a threshold you
had to switch on would be no threshold at all for exactly the operators who
never look.

`AUDIT_SIZE_WARN_BYTES=0` turns it off, for someone who has decided and does not
want reminding. That is the only way off, and it is deliberately not the default.

### The re-notify guard is a silence window, not a mute

A crossed threshold stays crossed until an operator acts, so the hourly job would
file the same notification every hour forever. An inbox that fills with one
repeated line is one people stop opening, which would cost precisely the warning
D5 leans on.

A week of silence per recipient, and then it warns again — because the opposite
failure is an operator who dismissed it once and never hears about it again.
Both edges have tests, including the elapsed-interval case, which is the half
that a "notify once" implementation would silently get wrong.

Only owners are told. An editor cannot change the retention setting, so telling
them is noise in the inbox of somebody who cannot act on it.

### The policy lives in the notify package, not the job runner

`WarnAuditGrowth` started in `cmd/linkctrl`, beside the scheduler that calls it.
It moved because what counts as too big, who hears about it and how often are
decisions worth testing, and nothing in `package main` can be reached by the
integration suite. The job runner now decides *when* to ask and the notify
package decides *what to do* — which is also why the threshold comparison lives
behind the same function rather than in the caller's `if`.

### The badge costs one query per page render

`shell` computes the unread count on every dashboard page, because the nav is on
every dashboard page. That is affordable specifically because the count matches
the partial index `notifications` already shipped with — the predicate in the
query is written to match `notifications_user_unread_idx` rather than merely to
be correct, and a count that did not match it would be a sequential scan on
every page load.

A failure there is swallowed to zero rather than propagated. Failing a page an
operator asked for because a decoration could not be computed is the wrong
trade; the badge is the least important thing on the screen.

---

## 2026-07-31 — M23, invalidation that crosses replicas

The limitation this discharges was the one that made 0.1.0 a single-instance
product: an edit cleared the cache on the replica that handled it, and every
other replica served the old destination until the entry expired.

### The reconnect is the whole design

Redis pub/sub does not replay. A subscriber that was disconnected when an
invalidation was published never hears it, and — this is the part that matters —
cannot know which key it named. So there is no repair short of distrusting
everything it holds, which is what D20 settles: flush both in-process tiers on
every re-establishment.

What makes this a design rather than a detail is how the failure looks. go-redis
returns the read error to the caller and then reconnects underneath, so a
dropped subscription is observable *exactly once*, on the failing read, and is
silent afterwards. A loop that simply retried would resubscribe successfully and
carry on serving entries whose invalidations went into the gap, with nothing
anywhere reporting a problem. Stale data mistaken for fresh data, permanently,
from one dropped TCP connection.

Re-establishment is forced with a ping rather than discovered by waiting for the
next message. Waiting would hold the stale window open until some unrelated edit
happened to arrive, which on a quiet instance is indefinitely — and "no
messages" is precisely what a broken subscriber looks like too.

The first connection flushes as well. A replica whose subscriber comes up after
Redis was briefly unreachable is in exactly the same position as a reconnecting
one: holding entries whose invalidations it was not there to hear.

### The publish does not wait, and that is not laziness

It was synchronous first. Testing against a TCP proxy that accepts the
connection and then never answers — a stalled Redis, as opposed to a refused
one — showed the publish adding **three seconds** to an edit, because go-redis
does not honour the short context it is handed when it has to establish a
connection to a server that never speaks.

The caller has no use for the result either way: a failed publish degrades to
the TTL staleness that existed before this milestone, which is why it was
best-effort from the start. So the bound that actually holds is not waiting at
all. Failures are logged from the goroutine, so a broadcast that never landed is
still visible.

That measurement also turned up a larger, older problem, deferred as F2: the
*delete* beside it takes about nine seconds under the same conditions, for the
same reason, and it predates this milestone. It is not fixed here.

### The test proxy, and a test that was measuring nothing

The drop is produced by cutting a real TCP connection through a proxy rather
than by faking a client, because the behaviour under test is what go-redis does
when a read fails mid-subscription — which is exactly what a fake would be
asserting an assumption about.

The first version of the Redis-down test used a *refused* connection, and a
sabotage pass showed it caught nothing: a refused connection fails in 264ms, so
an assertion that an edit "does not hang" could never fail however unbounded the
call underneath it was. Refusal and stalling are different failures, and only
the second one can hang a caller. The proxy grew a black-hole mode, and the
assertion moved onto the publish alone — the half this milestone owns — so it
stays meaningful when F2 is eventually fixed.

### Message shape

JSON, not a packed string, so M40's hostname cache can add a field without a
channel version bump: an older replica decoding a newer message ignores what it
does not know, which is what makes a rolling deploy safe. An unknown *kind* is
ignored rather than treated as a flush — guessing in the "safe" direction would
mean clearing every cache on every message some future version sends.

The channel name carries the cache key version, so two builds that disagree
about key format cannot hear each other at all. A replica that hears nothing
degrades to TTL staleness, which is the known-good previous behaviour; a replica
that misinterprets a key does something nobody has reasoned about.

A publisher receives its own message and applies it again, deleting a key it
just deleted. Filtering that out would need a sender id, a comparison, and a way
to get it wrong, to save one map delete.

---

## 2026-07-31 — M24, limits that hold across replicas

In-memory buckets mean N replicas allow N times the configured rate. That is
tolerable for the 404-probe limiter, whose job is to make alias scanning
tedious, and wrong for the credential limiter, whose job is to make credential
stuffing across a leaked list expensive — an attacker who can reach any replica
simply gets the budget multiplied.

### A backend, not a replacement

The Redis limiter is a field on the existing `Limiter` rather than a separate
type, because the fallback is the design rather than an error path. Any Redis
failure means the local bucket answers, so the limit degrades from *shared* to
*per instance* — never from *enforced* to *absent*. Nothing else in the codebase
changed shape: `nil` still means the limit is off, `Stats()` still works, and an
instance with the cache disabled keeps exactly the limiter it had.

### The bucket is evaluated on the server, against the server's clock

Atomic because the read-modify-write is the whole point: two replicas doing
get-compute-set would each see the same starting tokens and each allow a request
the other had already spent. So it is a Lua script.

The clock inside it is Redis's own `TIME`, not the caller's. Replicas do not
agree on the time to better than a few hundred milliseconds in practice, and a
bucket refilled against a fast client's clock refills faster than it should — a
limit that quietly does not hold, discovered by whoever has the most skewed
clock. Redis 7 replicates effects rather than commands, so a non-deterministic
script is safe.

### The deadline is enforced from outside the client call

F2 measured what a stalled Redis does to a call that trusts a context deadline:
go-redis does not honour it when it has to establish a connection to a server
that never answers, and the call took seconds. On the invalidation path that was
slow. Here it would be the *login endpoint* hanging on an optional dependency,
which is the failure this milestone's risk section names.

So the Redis call runs in its own goroutine and is abandoned on a timer rather
than merely cancelled. An abandoned call may still land and spend a token the
local fallback also spent; over-counting by one token during a stall is the
harmless direction.

A breaker sits in front of it because otherwise every request during an outage
pays the full timeout before falling back — turning a cache problem into latency
on every sign-in. Three consecutive failures open it for five seconds. The test
that pins this down would take four seconds without it and takes almost none
with it, which is the arithmetic stated in the test itself.

### The context is deliberately not the request's

The three call sites carry a `contextcheck` suppression, and the reason is not
convenience. Deriving the Redis call's context from the request would let a
client escape being charged by hanging up mid-request. Abandoning a connection
is free, and a limiter that can be dodged that way is not a limiter.

### The scope row was already honest

M24's done-means asks for Plan.md's scope row to be annotated with which
limiters are shared and which are not. It already was, from the planning pass —
so that bullet was satisfied before the milestone started, and the row was left
alone. What was missing was the *known limitation* row's account of what happens
during a Redis outage, which is now stated: the limit applies per replica until
Redis returns.

---

## 2026-07-31 — The loop kept stopping for reasons it had invented

Two consecutive `/work-on-phase` runs ended after exactly two milestones — M21
and M22, then M23 and M24 — with no stop condition met either time. The runs
reported honestly that no condition had fired and stopped anyway, which is worse
than stopping by mistake: the rule was read, understood, and overridden.

The reasons given were context length, the size of the next milestone, and the
next milestone needing a long k6 run. None of those is in §4's table.

### The safety net had become a permission slip

The root cause is one sentence in the loop, and it was mine to misread:

> Stopping between two writes must cost effort only, never knowledge.

That is a promise about *interruption* — a crash, a context limit, the owner
saying stop. Read from inside a long run it becomes an argument that stopping is
cheap, therefore fine, therefore a reasonable thing to choose. A rule written to
make involuntary stops survivable had turned into a licence for voluntary ones.

So the sentence now says which kind of stop it is about, and the note section
says the same thing again at the point of use. A file that is always
resume-ready is the normal state of this project, not a signal.

### An unmarked list reads as advisory

§4's table listed five conditions and never said it was complete. A list that
does not claim to be exhaustive invites a sixth entry from judgement, and
judgement mid-run is exactly what the loop exists to remove — it is running
unattended precisely so that nobody is deciding.

The table is now marked exhaustive, and the four rules at the top gained a
fourth: only the table stops you.

### Naming the specific excuses

A general rule would not have caught this, because each stop had a plausible
local story. So the ones that actually happened are enumerated as *not* stop
conditions, with what to do instead. Context length, milestone size, a slow job,
a round number of milestones landed, "a clean handoff point", and "this deserves
review" are all named.

Two of those deserve their reasoning written down rather than asserted. Context
is summarized automatically and the run continues through it, so ending a
working run to avoid running out of context spends the thing it is protecting.
And "this deserves review" is answered by the loop's own shape: every milestone
is committed and pushed at step 3, so the owner can read it whenever they like
without the loop pausing to offer.

### Reporting is not stopping

The runs conflated the two. Both ended a turn on a summary, which reads as a
handoff and waits for input. Saying what landed and then starting the next
milestone in the same turn costs nothing and keeps the loop running, so the loop
now says that explicitly.

---

## 2026-07-31 — M24.5, a dark theme that cannot flash

Owner-added scope, and the only milestone this phase that discharges nothing on
the scope tables. It lands before the phase's UI-building run — M25, M31, M37,
M38, M41, M43, M44 — because its token test becomes the enforcement for all of
them, and restyling seven milestones afterwards is a different job from building
them right the first time.

### The flash is unrepresentable, not suppressed

The usual dark-mode bug is a page painting light and correcting itself once a
script has read a preference. The usual fix is to move that script earlier —
into `<head>`, blocking — which makes the flash small rather than absent.

Here the server reads the cookie and renders `data-theme` onto `<html>` in the
response it sends. The first paint is already correct because there is no second
state to arrive at. No script runs, so there is none to block on, and the
dashboard's no-JavaScript claim survives a feature that is usually the reason it
stops being true.

"System" is the absence of a choice rather than a third value, and is stored by
*clearing* the cookie. An absent cookie and "follow the system" are then the same
state and cannot disagree — which they would eventually, if one were a value.

### One token set, and a test rather than vigilance

Templates name meaning — surface, muted, danger — never a shade. `@theme inline`
makes the generated utilities emit `var(--…)` directly, so one definition yields
`bg-`, `text-`, `border-`, `fill-`, `stroke-` and `ring-` at once; the SVG charts
are covered by the same definition as the cards, which is why the scan includes
`fill-` and `stroke-`.

The scan is the point of the milestone. A raw palette utility is not wrong in
light — it is wrong in dark, silently, and only for the people using dark.
Nobody building a feature in the light theme has any reason to notice, so review
is the wrong instrument and a failing build is the right one.

### Two light values moved, which is the honest half

D21 decided in advance that where a pair cannot meet AA at today's light value,
the light value moves. It came due immediately. The quietest text in the shipped
theme measured 2.56:1 (`slate-400`) and 1.48:1 (`slate-300`) against white,
against a 4.5:1 requirement, and all of it was real text — timestamps,
"(optional)" hints, empty states, help copy. None of it qualified as decorative.

So the light theme is now visibly darker in its quietest text. That is a change
to something already shipped, made deliberately, because an AA claim that
exempted the theme people are already using would not be a claim.

Every pair is measured rather than asserted, in both themes, and the figures sit
beside the definitions. The tightest is 4.55:1. The status colours were the
predicted risk and behaved as predicted: amber, rose and emerald at their
light-theme values sit near 2:1 on dark surfaces, so each keeps a dark tint for
its surface and a light shade for its text rather than reusing the `-50`
backgrounds.

### Per browser, not per account

A `users` column would not work on the sign-in page, which is the first page
anybody sees and the one most likely to be looked at in a dark room. A cookie
does, needs no session, and lets two browsers on one account disagree — which is
correct, because the person sitting at each of them chose.

The cookie is unprefixed, unlike the session's `__Host-`. That prefix requires
Secure and so cannot be set over plain HTTP, which is right for a credential and
wrong for an appearance preference that has to work on a local instance. The
worst a forged one can do is show somebody the other theme.

---

## 2026-07-31 — The loop splits into an orchestrator and workers

The entry above this one but two — *The loop kept stopping for reasons it had
invented* — fixed premature stopping with rules: mark the table exhaustive, name
the specific excuses, say that reporting is not stopping. Rules were the right
first answer, and they addressed the symptom.

The cause is structural. A single context builds a milestone, lands it, and then
holds every artifact of having done so: the diff, the reasoning, the false
starts, a summary it has just written. That context contains a great many
endpoint-shaped things, and each additional milestone adds more. Telling it not
to stop is asking judgement to hold a line that the shape of its own context
keeps pushing against.

So the loop now has two actors. A **worker** builds exactly one milestone and is
*supposed* to end when it is done — the instinct to wrap up at a summary is
correct behaviour for it. An **orchestrator** never builds, so it never
accumulates build detritus; across a whole phase it holds a status table, a
handful of verdicts, and the current milestone's definition of done. The
premature stop is not forbidden any harder than before. It is simply no longer
something either actor is under pressure to want.

### The seam was already in the file

Step 3's order is load-bearing and the split does not reorder a line of it. The
worker holds 3.1 to 3.3 — the gates, and making the docs true — and stops. The
orchestrator holds 3.4 onward: status row, links, commit, demo-update, push.

That the handoff falls exactly on an existing boundary is not luck. Everything
before it is work on the tree; everything from the commit onward publishes that
work. The two were already different kinds of act, written in one list because
one actor did both.

### The builder does not accept its own work

A worker reports that it satisfied the milestone. That report is the least
reliable evidence available about the milestone, because it was written by the
thing being judged, from the context that produced the work and every
rationalisation in it.

So acceptance re-reads `phase-details/mN.md` and then reads the tree —
`git status`, `git diff`, the tests the milestone named — and re-runs the gates
rather than believing they were run. *A report is not evidence. The tree is.*
This is also why the status row flips to `done` at 3.4 rather than in step 2: a
milestone that is rejected must not have left a claim behind saying otherwise.

Rejection spawns a **new** worker rather than continuing the old one. Continuing
it would hand the second attempt the first attempt's reasoning, which is exactly
the thing that produced the gap; the whole value of the split is that the second
reading is independent. It is bounded the same way gates are — the same gap
surviving two workers is a stop condition, because a third attempt at an
unchanged problem is not progress.

### Prompts belong to one actor

*Ask, never assume* only works if there is a route to the owner. A worker has
none: nothing it says reaches anybody except through the orchestrator. So a
worker that meets a decision writes the prompt verbatim into the note, returns it
unanswered, and stops — and validation moved to the orchestrator for the same
reason, since step 1's characteristic output is a prompt and it would be spent
on a worker that must immediately hand it back.

### Stopping honestly

*Stop work* stops the spawning and stops the worker in flight. What it cannot do
is make a process killed mid-tool-call write a good note first. Rather than
pretend at a cooperative shutdown, the orchestrator reconciles
`.current-task.md` against `git status` and `git log -1` — the tree, not the
report — so the record reflects what is actually there. Nothing is committed,
pushed or reverted on the way out; uncommitted work is left for the owner to
judge.

### What it costs, honestly

No speed. Milestones are strictly ordered by their `Depends on` rows, so nothing
runs in parallel and wall-clock is unchanged or slightly worse. The gain is
context hygiene, and paying for it in tokens — every worker re-reads the loop,
the gates, the milestone, and re-orients in the tree.

Part of that was pre-paid: phase-loop.md is already written terse *because* it is
re-read at every resume. The genuinely new cost is that a worker starts with no
memory of the previous milestone, which promotes the note's *cost too much to
re-derive* section from a courtesy to the mechanism that carries knowledge across
a boundary now crossed every milestone instead of only on interruption.

The `X.9` reviews and M45 are not delegated. The product of each is a
conversation with the owner about what to schedule, and a worker cannot have it.

---

## 2026-07-31 — M24.6, and a test that could not see the defect

The owner reported, hours after M24.5 landed, that switching to dark mode does
nothing. It does not. The light tokens are declared in an unlayered `:root`; the
dark tokens live inside `@layer base`. CSS cascade layers give unlayered normal
declarations priority over layered ones *regardless of specificity*, so
`:root { --t-surface: #f8fafc }` beats `:root[data-theme="dark"]` every time.
Both dark paths are dead — the explicit override and the `prefers-color-scheme`
one — which is why even `color-scheme: dark` never takes.

The server half is correct and is not implicated. The attribute renders, the
cookie works, the no-flash design does what it claimed. The page simply does not
change colour.

### The enforcement tested naming, not effect

M24.5's own entry says the scan "is the point of the milestone", and it was
right about the risk it aimed at: a raw palette utility is wrong only in dark,
silently, and only for the people using dark. But
`TestTemplatesUseThemeTokensOnly` asserts that templates *name* tokens. Nothing
asserted that naming one changes anything, and the contrast figures — every pair
measured, in both themes — were arithmetic in a comment about values no browser
ever reached. The milestone was enforced at the layer above the one that broke.

That is the general shape worth keeping: **the check verified the mechanism, not
the outcome.** The attribute was confirmed present at all three cookie states,
which is one honest step short of confirming the page looks different. Every
piece of evidence gathered was true, and the conclusion drawn from it was false.

So M24.6's test asserts the cascade relationship directly — that every construct
declaring a `--t-*` token shares one layer context — and it has to be shown
failing against the stylesheet as M24.5 shipped it before it counts. The live
check moves from "the attribute is there" to "the rendered token values differ".

### A new milestone, not a reopened one

The owner chose M24.6 over reopening M24.5, and the tradeoff is worth recording
because it cuts against the house rule that status must be true. M24.5 stays
`done` while one of its central claims is false, which is a real cost paid for a
real thing: the shipped commit and its decision entry stay untouched, and the
correction arrives as its own numbered milestone with its own definition of done
rather than as an edit to history. F3 carries the false-claim note so the
discrepancy is written down rather than implied, and the CHANGELOG's unreleased
section now says plainly that the four bullets above it are not yet true of a
running build.

### The control moves, which is a separate thing

F4 is not a defect: M24.5 said the override is "settable from the dashboard" and
never named a place, so the footer satisfied it. The footer is nevertheless the
one region a person scanning for a setting does not read.

It moves to account settings, and the sign-in page keeps its own control. That
second render site is not redundancy — M24.5's whole per-browser design rests on
the preference being settable before there is an account, and account settings
need a session. Putting the only control behind sign-in would quietly retract the
design while appearing to tidy it.

### What this says about the split committed an hour earlier

The orchestrator-and-worker split would not have caught this on its own.
Acceptance re-reads the milestone file and the tree, and every bullet in
m24.5.md would have read as satisfied — the tokens exist, the scan passes, the
attribute renders. A second reader with fresh context is protection against a
builder's rationalisations, not against a definition of done that measures the
wrong thing.

The lesson belongs to the milestone files rather than the loop: a bullet that
names a mechanism should say what observable outcome proves the mechanism works.
"Asserted by test" is not enough when the test can pass over a dead stylesheet.

---

## 2026-07-31 — M24.6 withdrawn, M24.5 reopened, and appends get a number

This corrects the entry immediately above, within the hour, on the owner's
instruction. Everything it records about the cascade defect and about testing
mechanisms instead of outcomes stands. Its scheduling decision does not.

### A `done` row may not assert something false

The previous entry took the cost of M24.6 knowingly and wrote it down: M24.5
stays `done` while one of its central claims is false, paid for by an untouched
commit and an untouched decision entry. Written out plainly like that, it does
not survive contact with the rest of this project. *Plan.md states what is true*
is not a preference about tidiness — it is the reason any of these documents are
worth reading, and a status table with one knowingly false row is a status table
a reader has to verify independently, which is the same as not having one.

The two things it bought were both cheap to give up. History is not edited by
reopening: the M24.5 commit stands untouched, this entry corrects the previous
one rather than replacing it, and the append-only rule is doing exactly the job
it exists for. What is actually gained is that the fix, the defect, the original
work and the trail all sit under one number instead of being split across two.

So the rule is now written down in workflow.md rather than decided case by case:
a defect that makes a **shipped** milestone's claim false reopens that milestone.
The deferred-findings row still comes first, because reopening is scheduling and
scheduling is the owner's.

### The status vocabulary gained a word

M24.5's row reads `in progress (reopened)`, not `in progress`. The distinction is
worth the parenthesis: a reader scanning the table should not have to wonder why
a milestone below M25 is being worked, and "reopened" says the milestone shipped
and came back rather than never having landed.

### Appends get a number

Separately, and prompted by the same episode: F3 and F4 were traceable to M24.5
only because the defect was hours old and the whole story was in one
conversation. F2 is traceable to M23 only because whoever wrote it happened to
say "measured while building M23" in prose. Nothing required either.

Appends outlive the context that made them, and under the worker split that
context is *deliberately* discarded at the end of every milestone. So the loop
now requires the milestone number on anything appended while a milestone is
under way: leading the title in decisions.md, and a **Found in** column in
deferred-findings.md, backfilled where the existing rows made it recoverable and
marked honestly where they did not.

Two files are exempt for reasons rather than by omission. Plan.md and
phase-details/ need no marker because every row already sits under its own
number. CHANGELOG.md needs none because it is written for operators, and `MN`
means nothing outside this repository.

The useful consequence is that an unmarked decision entry becomes a positive
claim — that no milestone produced it, that it is a process change or a phase
close — rather than an absence nobody can interpret. This entry carries no
number for exactly that reason.

---

## 2026-07-31 — Capture, read-ahead, and measuring what the contract costs

No milestone produced this. The owner proposed seven process changes at once;
four landed now and three were deferred to after M45, and this records why the
split fell where it did.

### The problem all four solve is the same one

Every one of them is about something the loop cannot currently hold. A thought
the owner has mid-milestone has nowhere to go that is not an interruption. A
decision the loop will need in three milestones is invisible until the loop is
standing still in front of it. And the documentation that makes the loop work is
read into a context window on every task, growing, with nothing measuring it.

Each gap was being paid for out of the owner's attention, which is the resource
this process exists to conserve.

### Capture decides nothing, and that is the whole design

`/note` appends a line to an untracked `.queue.md` and returns. It does not
classify, does not read the tree, does not interrupt the milestone in flight.

The owner's original proposal had three commands — Add Issue, Add Task, Add
Feature — and a rule that anything blocking be planned immediately. Both were
narrowed, for the same reason. Classification at capture time asks for a
judgement at the moment the owner has least attention, and it is the moment they
have least information too: the tree is what decides whether a note is a defect
or a feature request, and `/process-queue` has the tree in front of it while
`/note` deliberately does not. So the type became optional, unclassified is the
normal case, and a guess is explicitly forbidden — because once the context that
made it is gone, an inferred type is indistinguishable from the owner's own.

The three names survived as the taxonomy, in the owner's definitions: an
**issue** changes existing function or design, a **feature** adds new function or
design, a **task** changes workflow or process without touching the product.
Those map cleanly onto three destinations that already existed, which is the
evidence they are the right three.

Immediate planning of blocking notes was cut harder. A capture command that can
preempt the current milestone reintroduces exactly the unplanned scope the
deferral system exists to prevent, and it hands the decision to whichever actor
happens to be running — which for most of a phase is a worker, the one actor
that may never answer a prompt. So `/note` may *flag* `blocking?` and the
orchestrator judges it at step 3.9. Nothing is lost: a genuinely blocking note
stops the loop within one milestone boundary, and the flag keeps its question
mark to make clear it is a report and not a finding.

The queue is untracked for the reason `.current-task.md` is: `/note` appends
mid-milestone, and a tracked file would dirty the tree that `make demo-update`
and `make release-check` refuse to run on. The cost is that it survives no
clone, so the rule that makes it safe is that draining is mandatory and a row
that overwinters there is a row in the wrong file.

`/process-queue` classifies, then **verifies** — the owner's addition, and the
better half of the command. Step 1 judges a note by its wording; step 2 judges it
against the tree, and the tree disagrees more often than the wording admits. The
common disagreement has real consequences: a note typed `issue` that is really a
feature gets a findings row instead of five artifacts and an owner decision on
scope. Every dispute is a prompt, because the tree carries what is true and the
owner carries what they meant, and the disagreement is usually about which of
those the note was about.

### Reading ahead of the loop

An unanswered prompt is the only stop condition the loop inflicts on itself.
`/preview-decisions` runs step 1's *decisions cover it* check across milestones
not yet built and writes the questions to `upcoming-decisions.md`, so they can be
answered in any session rather than while the run is halted.

The file holds questions and never answers — one direction, out to decisions.md
with a `D` number. Two files that can both hold a decision is two places to look
for it, and the append-only log is the one that has to win.

The risk of answering early is a decision resting on a tree that has not been
built yet, so every entry names what it assumes, and validation re-checks those
assumptions when it arrives. A false assumption re-opens the question instead of
letting the milestone inherit a stale answer. An early answer is otherwise
exactly as binding as one given in the loop; the timing is a scheduling
convenience, not a lower standard, which is why entries carry options and a
recommendation like any other prompt.

### Prompts got a required shape

Options with what each buys and costs, a recommendation, and a named default for
"you decide". The one non-obvious clause is that the recommended option must
state its own con: the actor writing the recommendation is the actor that will
implement it, and that biases toward whatever is cheapest to build. Naming the
cost is what holds it honest. Nothing enforces this — no test can — and it is
written as a standing rule rather than a gate for that reason.

### Measuring the contract, and why both measurements

The owner asked for a record of load-bearing files and their token cost. A
hand-kept per-task ledger was rejected: it would be the highest-frequency write
in the system, on the critical path of every task, holding estimated numbers,
which this repository's own standing rule against unmeasured figures forbids.

`make doc-cost` reports two columns instead. **Predicted** is each file's size
charged in full to every trigger whose documented read set names it — exact in
bytes, and a ceiling. **Realized** is what `Read` actually returned, parsed from
this machine's session transcripts — exact, and a floor, since content also
arrives through Bash and search results.

Only the pair is useful, which was the owner's point when they asked why not
both. A size report alone would charge decisions.md 44k tokens on every task; the
transcripts show it is read at **0.02 of its size**, because it is grepped rather
than read whole. The same report shows workflow.md at 0.69 and phase-loop.md at
0.56 — those *are* read substantially whole, every resume, and their size is the
recurring tax worth paying down. Neither number alone says which is which. The
gap is the signal, and its direction over time is the alarm: realized rising
toward predicted means something started reading whole what it used to grep.

It landed now rather than after M45, at the owner's call, on the reasoning that a
baseline is only worth having before the thing it measures grows. decisions.md is
already larger than every other build-note combined and is append-only, so the
growth is certain and one-directional.

The report carries no generation timestamp. The commit date is the date, it is
not invented, and its absence means regenerating on an unchanged tree produces no
diff — so every diff in that file is real growth rather than churn.

### The scope gate had no room for any of this

Every change here is a *task* by the definition above, and none is a milestone.
The gate read "one milestone per commit, maximum", which left process work with
no sanctioned way to be committed at all. It now reads "no more than one
milestone per commit", and says explicitly that work smaller than a milestone
commits on its own. The rule always meant this; it had only ever been written
against the case it was guarding.

Separately, workflow.md's issue trigger still sent findings to a "Deferred
findings" section of Plan.md that moved to deferred-findings.md a phase ago. It
was wrong in the file read on every task, which is the worst place for a stale
instruction, and is corrected.

### Deferred to after M45

Process Issues, Review PR, and Review Findings. All three write outside the
repository or reschedule work, and all three are better specified after a phase
close has been run once. Two constraints are recorded now while the reasoning is
fresh:

GitHub issues are an **inbox, never a queue** — they are drained into
deferred-findings.md or planning.md, and the issue mirrors its artifact's state
rather than holding any of its own. The owner's requirement is that reporters see
progress as it happens rather than in a batch at the end, so the loop will push
label changes as status rows change, log any failure, and verify later; the
alternative — reconciling at phase close — makes an issue's silence
indistinguishable from no progress.

Review Findings cannot simply be added, because workflow.md currently states that
approved findings "collect into the phase's final milestone". A command that
schedules them anywhere makes that sentence false, and by this repository's own
precedence rule a conflict between two documents is a bug to fix rather than to
work around.

---

## 2026-07-31 — M24.5, applying the theme rather than declaring it

The reopened half of M24.5. The entry two above records the defect and the
lesson; this records what was actually built and the choices inside it.

### Unlayered, not `@layer base`

Two arrangements fix the cascade: put every `--t-*` block inside `@layer base`,
or take every one of them out of any layer. Both make the dark selectors win,
because within a single cascade context the ordinary rules resume —
`:root[data-theme="dark"]` is more specific than `:root`, and the explicit
override is written last so it beats the `prefers-color-scheme` rule at equal
specificity.

Unlayered was chosen. These are the values every generated utility reads
through `var()`, so a stylesheet that later arrives unlayered — a vendored
widget, anything pasted in — should not be able to redefine a theme token by
sitting outside a layer. Putting them in `base` would make that possible and
give no warning when it happened. The cost is that the tokens are now the one
part of this stylesheet that is deliberately outside Tailwind's layer
structure, which is why the reason sits in a comment block above them rather
than only here.

Nothing else moved. Every token value is byte-for-byte what shipped, and the
contrast figures beside them were **not** re-measured — they were arithmetic
about values that were correct all along and simply never reached a browser.
Re-running the numbers would have produced the same table and a false claim of
fresh measurement.

### The test had to be shown red against the shipped stylesheet

`TestThemeTokensShareOneCascadeLayer` parses the built `app.css`, finds every
construct that declares a `--t-*` token, and fails unless they all sit in one
cascade context. It also fails if a token is declared in one block and not the
others, and if a dark block is emitted before the light one.

It was demonstrated failing twice before it counted: once against the
stylesheet built from `b289a39`'s `input.css` — the exact bytes that shipped —
where it reported the light block as unlayered and both dark blocks as inside
`@layer base`, and once with `--t-warn-line` deleted from the explicit override.
A test written after a defect is only evidence if it is shown to see that
defect, and this one was written by whoever also wrote the fix.

The order assertion is not decoration. Equal layer plus higher specificity is
what makes dark win, and emitting light last would restore the bug in a form
the layer check alone would pass.

The stylesheet is read through the embed FS and a missing `app.css` is fatal,
not skipped. The whole failure being corrected here is a green run over a
stylesheet nothing applied; a skip would be the same shape again.

### The live check resolves the cascade instead of counting attributes

The previous check confirmed `data-theme` was present at all three cookie
states. That was true, and the page was still light. So the check now fetches
`/dashboard`, `/account` and `/login` from the composed stack at each cookie
state, fetches the stylesheet the response actually links to, and resolves
`--t-*` on `<html>` under layer, then specificity, then source order —
reporting the winning *value* and which rule won.

Against the served build the four states resolve to two distinct token sets,
with the explicit override beating the system preference in both directions.
Run against a stylesheet rebuilt from `b289a39`, every state resolves to
`#f8fafc` — the light surface — which is precisely the symptom the owner
reported and the reason this check replaced the old one.

It is a cascade resolver, not a browser: it models layer, specificity, source
order and `prefers-color-scheme`, and nothing else. It cannot see paint. That
is a real limit and it is worth naming, because the previous mistake was
treating one honest step short of the outcome as the outcome.

### Two render sites, one control

The **Appearance** control left the footer for account settings, and the
sign-in page kept a copy from the same partial (F4). The partial touches no
identity, which is what lets one definition serve a page behind a session and a
page reachable without one.

`TestExactlyOneAppearanceControlPerPage` asserts the count on every page, not
just on the two that have it. "Move" is one edit away from "copy": leaving the
layout's render site in place would have given the account page two controls
that can disagree about which option looks selected, and every other page one it
no longer wants. The test was shown failing that exact way before it counted.

Most pages therefore render no appearance control at all, which is what moving
it to account settings means. The milestone's phrase "exactly one control
renders per page" is read here as the count being exact everywhere — one on the
two sites named, none anywhere else — rather than as a control on every page,
which is the arrangement the move exists to end.

---

## 2026-07-31 — M25, where a request decides which workspace it is in

D22 answered *which* workspace a session starts in — last-used, remembered,
with a pin available for anyone the derivation annoys. It did not say where
"current" is kept, and that turned out to be the whole design.

### Three columns, because they answer three questions

`sessions.workspace_id` is where this browser is now. `users.default_workspace_id`
is the pin. `users.last_workspace_id` is where the person was most recently.

The obvious cheaper shape — one column on `users` meaning "current", with the
pin applied at sign-in — collapses under its own combination. A pinned user who
switches has either their switch overruled on the next request, or their pin
silently overwritten by the switch; there is no third outcome, because both
values would live in the same place. Applying the pin only at sign-in looks like
a way out until you notice sessions here last thirty days: "at sign-in" is
approximately never, so a person who pinned a workspace in the morning would
find the pin had done nothing by the afternoon.

Splitting them also buys the behaviour a person with two workspaces actually
wants, which is two windows open on both. Current belongs to the session because
that is the thing a person switches.

### The precedence is an ORDER BY, and it is the only copy

```
1  the session's own workspace     rung 1 is dead when there is no session
2  the pinned default              users.default_workspace_id
3  the last used                   users.last_workspace_id
4  the oldest one they are in      w.created_at, w.id — what shipped in 0.1.0
```

Each rung is a tiebreak on the one above, in a single query, so a user with one
membership ties on all four and gets exactly the row the pre-M25 query returned.
That is the milestone's no-op claim expressed as arithmetic rather than as an
assurance, and `TestOneMembershipResolvesExactlyAsItDid` computes the old
ordering itself rather than asking the service what it thinks.

Membership is in the `WHERE`, never in the ordering. A preference pointing at a
deleted workspace, or one the person has been removed from, stops matching and
the next rung answers — so a stale preference degrades instead of erroring, and
nothing has to clean up after a membership change.

Four call sites were the milestone's stated risk: login, session
authentication, the CLI's identity lookup, and an API key with no workspace of
its own. They now share one unexported `resolveWorkspace`, and the old query name
is gone from the tree, so a fifth caller cannot resolve the old way by copying an
old line. Only the session id differs between them, which is exactly the
difference that matters.

### The clause that is a no-op today

Resolution and the switcher both apply `memberships.workspace_id IS NULL OR
= w.id`, which is the rule `GetUserPermissions` has always applied and the old
default-workspace query never did. Every membership in existence is
organization-wide, so it changes nothing now. It is here because the alternative
is an identity resolved into a workspace it holds no permissions in — a
dashboard that renders and can do nothing — and the milestone whose job is
identity resolution is the right one to close that.

### Switching needs a session; listing does not

`SwitchWorkspace` and `SetDefaultWorkspace` refuse an API key, the same way
changing a password does. A key acts in the workspace its own row names, so a key
switching would change nothing about its own requests while repointing where its
owner's browser lands — a side effect visible only to somebody else. Listing is
open to any credential: it exposes the caller's own memberships and nothing more,
which is why no permission was added for any of this. The milestone matched
neither limb of D18 because it introduced no permission to match.

A key that names *no* workspace does follow the owner's pin, since it has to
resolve to something and the owner's own answer is the best available one.

### The switcher renders when there is somewhere to go

With one membership the header draws nothing at all. A dropdown listing the one
workspace you are already in is a control that cannot do anything, and every
instance in existence is in that state — so the milestone is invisible to all of
them, which is a stronger claim than "the resolution is unchanged" and cheaper
to hold. The account setting does render, because a preference has to live
somewhere findable, and it reads *Last-Used* until somebody chooses otherwise.

The switcher costs one indexed query per page render, alongside the unread
badge. A cached alternative would be a second copy of "which workspaces am I
in", invalidated from the milestone that creates memberships, which is not a
trade worth making before that milestone exists.

---

## 2026-07-31 — M26, a mailer that is genuinely optional

D1 said the mailer ships and every consumer degrades mail-free; D23 said it goes
through an outbox. Both were decided before any of it existed, so this milestone
is mostly about the parts neither of them named.

### Off by default is a nil interface, not a flag

`SMTP_HOST` empty means no `mail.Service` is constructed at all. Consumers hold
`notify.Enqueuer`, which is nil, and the one check lives at the send site.

The alternative — a `Mailer` that exists and returns early on a `enabled` field —
is the version where "optional" is a property somebody has to keep remembering.
Every future consumer would carry the same `if !mailer.Enabled()` branch, and the
first one that forgot it would fail at runtime on an instance that had configured
nothing, which is the exact deployment this product is for. A nil interface makes
the degraded path the one that needs no code.

The claim is testable because of D23's table rather than in spite of it:
`TestUnconfiguredMailerLeavesTheOutboxEmptyAndTheInboxWorking` asserts both that
the owner still gets the notification and that `mail_outbox` has no rows. Only
the second half distinguishes a mailer that degraded from one that quietly
dropped the notification too, and "nothing was sent" could not have made that
distinction.

### A configuration mistake is fatal; a relay being down is not

The milestone asked for connection details "validated at boot with a clear
failure mode", and there are two failure modes hiding in that sentence.

An unparseable `SMTP_FROM`, an unknown TLS mode, a username without a password,
or credentials that would go over the wire in clear are refused by
`config.Validate`, alongside every other configuration error, and the process
does not start. They are typos, they are found by reading the environment, and
none of them can fix itself.

A relay that does not answer is different. Boot opens a connection, greets it and
hangs up; failure logs at error and startup continues. This is the same call
Redis already gets, and for a stronger reason: a link shortener that refuses to
serve redirects because somebody's SMTP provider is having an afternoon has
converted an optional dependency into an outage. The outbox is what makes it safe
— anything queued while the relay is unreachable is still there when it returns.

### Inert by construction, in the renderer

`text/template` escapes nothing. That is the correct choice for a plain-text
body and it means the one thing standing between a display name and the header
block is whoever wrote the template remembering to sanitize.

So the sanitizing moved into `RenderMail`, and the data it takes is
`map[string]string` specifically so that it can walk every value on the way in. A
struct or an `any` would have put the responsibility back on the template author.
Control characters and bidirectional formatting characters are dropped — not
escaped, because a plain-text body has no encoding layer for an escape to be
undone by, so the only honest answer is for the character not to be there.

Plain text is itself most of the answer to "renders untrusted input inert". A
multipart message with an HTML part is where mail acquires remote images that
report when it was opened, anchor text that disagrees with its href, and a
rendering engine per client to be wrong about. Shipping text only removed that
surface instead of defending it, which is why the risk section's warning about
scope creep was answered by having less rather than by testing more.

The remaining vector is injection, and it is guarded twice: the renderer strips,
and `BuildMessage` refuses a header value containing a line break. Two guards
because they fail differently — one would have to be bypassed, the other removed
— and because the second is the one that still holds for a value that never went
through a template.

### Attempts are counted when a message is claimed, not when it is sent

`ClaimDueMail` is an `UPDATE ... RETURNING` that spends the attempt and pushes
`next_attempt_at` forward before anything is delivered. Counting at send time
reads more naturally and is wrong in the case that matters: a process that dies
mid-send has spent no attempt, so it retries the same message on restart, dies
again, and a poisoned row becomes an infinite loop bounded by nothing.

Leasing forward in the same statement gives crash recovery for free. A killed
drainer leaves a row that becomes due again on its own rather than one stuck
pending forever, and no reaper is needed to notice.

`FOR UPDATE SKIP LOCKED` sits inside it even though leadership already keeps a
second replica out of this job. Leadership is an advisory lock released when its
holder dies, so a moment of overlap is possible; skip-locked makes that moment
cost nothing instead of sending somebody two invitations.

### The outbox has a window, because it is a record and not an archive

Sent and failed rows are deleted after 30 days by the housekeeping job, beside
the links, sessions and API keys it already reaps. Pending rows never are.

This is a small addition the milestone did not ask for, and it is here because
the alternative is shipping the one table in this schema that grows forever with
nothing watching it — which is precisely the shape D5 spent a metric, an alert
recipe and a notification learning about. Thirty days is long enough that "did
that invitation ever go out?" is still answerable and short enough that nobody
has to think about it.

### What is not supported, said out loud

STARTTLS, implicit TLS, or an unencrypted local relay; PLAIN authentication over
an encrypted connection. Not LOGIN, CRAM-MD5, XOAUTH2 or client certificates.

The milestone flagged the SMTP configuration surface as an invitation to scope
creep, and the answer is a list of what does not work in
`docs/configuration.md` rather than a matrix of half-tested mechanisms. A relay
that will not take PLAIN over TLS cannot be used by this product; a paragraph
saying so costs a reader thirty seconds, where discovering it from a bounce
three days after an invitation was sent costs considerably more.

`SMTP_TLS=starttls` refuses to send if the relay does not advertise STARTTLS,
rather than continuing in clear. Falling back would send the password and the
message unencrypted having been explicitly told to use TLS, which is the kind of
helpfulness that ends up in an advisory.

### The audit-growth warning is the consumer that already existed

M26's consumer list names invitations, address verification and dispute
outcomes, none of which are built. It also names the audit-growth alert, which
is, and Plan.md's D5 attributes the emailing of it to this milestone
specifically: *metric and alert (M21), owner notification (M22), emailed when a
mailer exists (M26)*.

Wiring it here is what gives the degradation claim something real to be tested
against. It also completes D5's promise on its own terms — keep-forever is only
safe if the growth it permits is visible, and an owner who does not open the
dashboard for a month was not, until now, being told anything.
