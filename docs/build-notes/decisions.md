# Decision log

Why LinkCtrl is built the way it is. `Plan.md` states *what* is true; this file
states *why*, so the spec stays terse and the reasoning is still recoverable.

Entries are append-only and dated. A later entry may correct an earlier one;
the earlier text is left in place with a pointer rather than edited away.

Longer investigations live in `../adr/`.

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
