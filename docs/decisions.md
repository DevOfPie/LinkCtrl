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
runs. Recorded in docs/development.md so nobody chases it as a bug or quotes a
local number as a result.
