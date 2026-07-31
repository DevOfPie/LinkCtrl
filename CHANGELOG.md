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

Nothing yet.

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
