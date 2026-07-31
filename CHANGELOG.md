# Changelog

Notable changes, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

Two things are versioned separately, and the distinction matters when deciding
whether an upgrade is safe:

- **The REST API is `/api/v1`** and is a stable contract. A breaking change there
  becomes `/api/v2`, not a major version bump here.
- **The product** is pre-1.0 while Phase 2 is outstanding. Shared workspaces,
  folders and custom domains will change the dashboard and add tables, so the
  version stays in the `0.x` range until that has settled. `0.x` here means "the
  product surface may still move", not "unfinished": everything documented as
  built is tested and exercised end to end.

The database schema only ever changes additively within a minor version, and
migrations run at boot.

## [Unreleased]

### Added

- **The audit log has behavior.** The table shipped in 0.1.0 with nothing writing
  to it; there is now a writer, a read API, and a retention policy of its own.
  Changing the link domain's root redirect is the first recorded action — the
  setting that sends every stray visitor somewhere, and the one 0.1.0 said would
  become an audit event once there was an audit log to put it in.
- `GET /api/v1/audit` lists an organization's records newest-first with keyset
  pagination, gated by a new `audit.read` permission granted to owners and
  admins. **It cannot be granted to an API key.** Reading the log is the one
  place a network prefix is tied to a named person, so it requires a signed-in
  session; a key requesting the scope is refused when it is minted.
- `LINKCTRL_AUDIT_RETENTION_DAYS`, **defaulting to `0` — keep forever.**
  Deliberately not the analytics default: an upgrade must never silently start
  deleting history an operator assumed permanent. `audit_logs` partitions are
  now dropped under this window and never under the analytics one.
- `linkctrl_audit_log_bytes`, the on-disk size of every `audit_logs` partition,
  refreshed hourly on every replica. Keeping everything forever is only a safe
  default if the growth it permits is visible; the Prometheus alert recipe is in
  [docs/operations.md](docs/operations.md#audit-log-growth).
- **The dashboard has a dark theme.** With nothing chosen it follows
  `prefers-color-scheme`, with no cookie, no account and no JavaScript involved.
  An **Appearance** control at the foot of every page — the sign-in page
  included — overrides that with System, Light or Dark.
- The choice is stored per browser rather than on the account, so it works
  before you sign in and two browsers on one account may disagree. Deliberate.
- **There is no flash of the wrong theme.** The server reads the cookie and
  renders the theme onto `<html>`, so the first response is already correct.
  Nothing corrects it afterwards because there is nothing to correct.
- **Two light-theme colours changed.** The quietest text — timestamps,
  "(optional)" hints, empty states — was `slate-400` and `slate-300`, which
  measure 2.56:1 and 1.48:1 against white and fail WCAG AA. Both are now darker.
  An accessibility claim that exempted the theme already shipped would not have
  been one; the contrast figures for every token pair, in both themes, are
  recorded beside the definitions in `internal/ui/static/css/input.css`.
- **Known limitation, unreleased:** the dark theme does not currently apply. Its
  light tokens are declared unlayered and its dark tokens inside `@layer base`,
  and unlayered declarations win regardless of specificity, so both the explicit
  override and the `prefers-color-scheme` path lose to the light values. The
  server side is correct — the attribute renders — but the page does not change.
  M24.6 fixes the cascade and moves the control out of the footer; the four
  bullets above describe the intended behaviour and are not yet true of a running
  build. Tracked as F3 and F4 in
  [docs/build-notes/deferred-findings.md](docs/build-notes/deferred-findings.md).
- Not themed, deliberately: `/docs`, whose Swagger UI is vendored and
  checksum-pinned.
- **Credential and API rate limits are shared across replicas.** They are
  enforced in Redis, so the configured rate is the instance's rate rather than
  each replica's — an attacker spreading a credential-stuffing run across
  replicas no longer gets the limit multiplied by however many are behind the
  load balancer.
- On any Redis error the limiter falls back to the per-replica bucket it always
  had. It still limits, just once per replica, and it never starts refusing
  requests because Redis is unwell: a limiter is abuse mitigation, not an
  authorization boundary. The fallback is logged once when it begins.
- **The 404-probe limiter is deliberately not shared**, and will not be. A Redis
  round trip on the redirect path would put an optional dependency inside the
  20ms budget.
- **Cache invalidation now crosses replicas.** Editing a link on one instance
  clears every instance's cache, over a Redis pub/sub channel, instead of only
  the one that handled the edit. Running more than one app replica no longer
  means an edit takes up to `REDIRECT_TTL` to become visible everywhere — the
  limitation 0.1.0 shipped with, and the reason it told you to run one instance.
- With Redis down this degrades rather than breaks: redirects still resolve from
  Postgres, edits still apply and still clear the replica that made them, and
  the other replicas fall back to the TTL staleness they had before. A
  subscriber that loses its connection **flushes its in-process caches when it
  reconnects**, because Redis pub/sub does not replay and a replica cannot know
  which invalidations it missed. The cost is a cold cache after a Redis blip;
  the alternative is serving a destination the owner already changed.
- **Notifications, in the dashboard.** A nav badge, a notifications page and
  `GET /api/v1/notifications` with mark-read and mark-all-read. Your own inbox
  only — there is no permission for reading somebody else's, because there is no
  reason for one — and no endpoint that creates a notification: they record what
  the system observed, not what a caller asserts.
- The first thing that raises one is the audit-log size threshold above.
  `LINKCTRL_AUDIT_SIZE_WARN_BYTES` defaults to 5 GiB and is **on by default**,
  which is deliberately the opposite of the retention default: keeping
  everything forever is only safe if the instance nobody configured is the one
  that gets warned. Owners are told, at most once a week each, and only owners —
  nobody else can change the setting.

### Notes for operators

- Every record stores a network prefix — /24 for IPv4, /48 for IPv6 — and never
  an address, matching what sessions already did. The actor's label is
  snapshotted when the event is written, so a record stays readable after the
  account it names is deleted.
- Nothing is recorded retroactively. The log starts at the upgrade.
- Notifications added no database columns. The table shipped in 0.1.0 and
  per-kind detail goes in its `data` jsonb, so this upgrade is additive in the
  ordinary way and needs no backfill.
- There is no email yet, and no push. In-app only until a mailer exists.

## [0.1.0] - 2026-07-31

First release, and all of Phase 1's twenty-one milestones: a self-hostable link
manager where a short link is an editable, measurable, scriptable resource.

### Links

- Create, edit, archive and soft-delete links. A trashed link holds its alias for
  30 days; the hourly purge then deletes it, permanently reserving any alias that
  ever received traffic. There is no trash view in this release, so recovery
  inside that window is a database operation and the interface says so rather
  than implying a button. Editing a destination never changes the short URL —
  the reason redirects are always 302.
- **Renaming a link reserves its old alias** on the same rule the purge uses: if
  it ever received a click it can never be reissued, to anyone. Abandoning an
  alias is not the same as freeing it — the old one is still on printed material
  and in other people's bookmarks, and handing it to a different destination is
  a redirect hijack.
- Custom or generated aliases, lowercase-canonical and case-insensitive. Dots are
  refused outright, which removes the "is `logo.png` an alias or an asset?" class
  of problem rather than pattern-matching for it.
- Tags, titles, descriptions and expiry. An expired link answers `410 Gone`, not
  `404`, so crawlers and link checkers stop retrying, and it reports as expired
  in the dashboard and the API too — the status is derived from the expiry
  wherever it is shown or filtered, never written to a column that would be stale
  between the deadline passing and something noticing.
- Per-link query forwarding, off by default: the visitor's query string is merged
  into the destination, whose own parameters win on conflict.
- Full-text and substring search, filtering by status, sorting, and cursor
  pagination. Offsets are not offered: they re-scan skipped rows and silently
  duplicate or drop entries when links are created mid-page.
- An alias that has ever received traffic is never reissued after purge.

### Redirects

- In-process cache, then Redis, then Postgres, with negative caching for the
  unknown aliases a public shortener is mostly asked for.
- Redis is optional at runtime. Losing it makes redirects slower, never wrong.
- A dedicated connection pool for the redirect path, so a slow analytics query
  cannot leave a redirect waiting for a connection.
- **Measured**: 100% of 240,001 cached redirects answered under 20ms at a sustained
  2,000 rps, with 100k links and 5.7M click events in the database and the
  analytics rollup running throughout. See [docs/slo.md](docs/slo.md).

### Hostnames

- **The dashboard and short links can be served on separate hostnames**, via
  `LINKCTRL_APP_BASE_URL` and `LINKCTRL_LINK_BASE_URL`. Both default to
  `LINKCTRL_BASE_URL`, so a single-host deployment needs no configuration at all.

  Set to different hosts, each answers only its own paths: the dashboard host
  stops resolving aliases, the link host stops serving the dashboard, the API and
  the static assets. A request to the wrong host is `404`, never a redirect to
  the right one — a cross-host redirect reachable through the alias namespace
  would be an open redirector for anyone able to create a link.

  The point is the session cookie. It carries the `__Host-` prefix, which forbids
  a `Domain` attribute, so once the hosts differ a browser will not send it to
  the host serving short links — the half of the product that gets pasted into
  forums and probed by strangers, and the half that needs no credentials at all.

  `/healthz` and `/readyz` answer on every hostname, including ones never
  configured: probes come from load balancers and container runtimes that do not
  know the operator's names. Still one listener and one process.

- **The link domain's root can be pointed somewhere**, for the visitor who trims
  a short link back to the bare domain. Unset it answers `404`, and there is no
  default page — an instance that says nothing about itself is a legitimate
  choice. Setting it needs the `domains.write` permission, held by owner and
  admin, because this is not one link but where every stray visitor to the whole
  domain ends up. The destination is validated exactly as a link's is, which
  matters most here: reaching it needs no link and no alias. Cached and
  invalidated on change, so it costs no query per request and takes effect
  immediately.

### Analytics

- Clicks, estimated unique visitors, bots, device, browser, OS, language and
  referrer host, from daily rollups rather than raw events.
- Country, with an operator-supplied MaxMind database. Region and city are
  resolvable from the same file and deliberately not stored.
- Retention enforced by dropping whole monthly partitions, which is instant and
  reclaims the space. Rollups survive, so charts outlive the raw events.
- **No IP address is stored in any column.** A visitor is
  `HMAC(daily salt, ip ‖ user-agent ‖ workspace)`, and the salts are deleted after
  two days — that deletion is the de-identification step, not housekeeping.
  Referrers are reduced to a host at ingest.
- Unique-visitor figures are therefore estimates at daily resolution, and every
  API response carrying them says so.

### Authentication and authorization

- Email and password with argon2id, server-side sessions in `__Host-` cookies,
  and per-account lockout.
- Real RBAC with four built-in roles and a working permission evaluator, not a
  hardcoded owner check.
- API keys as `lk_live_…` bearer tokens, scoped to permissions the owner holds and
  intersected with their current role on every request. Only the HMAC is stored.
- Keys can never hold `apikeys.*` or `org.delete`: a key that can mint keys makes
  revoking a leaked one meaningless.

### Abuse limits

- Per-address rate limits on the credential endpoints and on `/api/v1`, answering
  `429` with `Retry-After`. Added alongside the per-account lockout rather than
  instead of it — one address guessing across a leaked list never trips a
  per-account counter.
- 404 probe limiting on the redirect path, charging misses only. A working link is
  never throttled by anyone's scanning, paths that could not be an alias are
  refused on shape without a lookup, and a throttled address still resolves links
  already in the cache.
- Destination validation by allowlist: `http` and `https` only, with private,
  loopback, link-local, carrier-NAT and cloud-metadata addresses refused.

### Interfaces

- Server-rendered dashboard with htmx. Works without JavaScript; no build step at
  runtime and no Node in the image.
- REST API with RFC 9457 problem responses, a hand-maintained OpenAPI 3 document,
  and Swagger UI at `/docs`. A contract test replays every documented operation
  against a live server, so the document cannot drift from the implementation.
- `lctl` for configuration checks, migrations, partitions, API keys and load-test
  seeding — including minting the first key on a headless host.
- `lctl demo` fills an instance with a workspace worth looking at: around twenty
  links with titles, tags and destinations, a month of click history with weekday
  seasonality and a launch spike, and every status the dashboard can render. Its
  links are created through the same service call the REST API uses, so the
  dataset cannot describe a state the product could not reach.

### Operations

- `/healthz` and `/readyz`, the latter distinguishing degradation from failure:
  Redis down is `degraded`, because the service still works.
- Prometheus metrics on a separate, unpublished listener.
- Structured JSON logs. Secrets are a type that refuses to print itself, so a
  config dump or a formatted panic cannot leak the database password.
- Graceful shutdown that fails readiness first, drains, then flushes buffered
  clicks.
- Migrations run in-process at boot, serialized across replicas by a Postgres
  session lock, and disableable for change-controlled deployments.

### Known limitations

Stated because they are the things worth knowing before deploying, and they are
all in [Plan.md](Plan.md#known-limitations) with their consequences:

- **Run one application instance.** Cache invalidation reaches the replica that
  served the edit; others wait out the TTL. Phase 2 adds pub/sub.
- **Rate limits are per instance** and reset on restart.
- **The analytics dimension rollup is expensive** and gets worse with traffic: 16-21
  seconds per 60-second run at 5.7M events. Redirects are unaffected; dashboards go
  stale if it falls behind.
- **Behind a reverse proxy, `LINKCTRL_TRUSTED_PROXIES` must be set**, or every
  request appears to come from the proxy and all traffic shares one rate-limit
  bucket.
- **`LINKCTRL_API_KEY_PEPPER` cannot be rotated in place.** Changing it invalidates
  every existing key.
- No audit log behaviour, folders API, per-workspace custom domains, QR codes, or
  password/one-time links. The tables exist; the features are Phase 2. The
  `visitors` table and `click_events.is_first_visit` are dormant in the same way:
  nothing writes or reads them, and both stay under partition maintenance and
  retention so the guarantees apply the day something does.
- No signup page. `LINKCTRL_SIGNUP_MODE=open` is honoured by the JSON API only,
  and a registration creates a new isolated workspace rather than adding a member
  to yours. Invitations, and a signup form worth having, are Phase 2.

[Unreleased]: https://github.com/DevOfPie/LinkCtrl/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/DevOfPie/LinkCtrl/releases/tag/v0.1.0
