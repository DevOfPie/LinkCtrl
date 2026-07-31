# Configuration

Every setting is an environment variable prefixed `LINKCTRL_`. The `POSTGRES_*`
variables are the exception: they are consumed by the Postgres container itself
and deliberately sit outside the prefix.

`.env.example` is the annotated copy to start from. This document is the complete
reference: every variable listed here takes effect. Three that used to be
documented as accepted-but-inert no longer exist — see
[Removed](#removed-variables).

Under the shipped `docker-compose.yml` that holds without qualification: the app
service loads `.env` wholesale via `env_file`, so anything set there reaches the
process. Only the values compose itself owns — the service hostnames in
`DATABASE_URL` and `REDIS_URL`, the ports inside the container — are pinned in
the compose file and cannot be overridden from `.env`. (This used to be a fixed
fifteen-variable list, which meant most of this document was read from `.env`
for interpolation and then quietly dropped. A test in `internal/config` now
fails if the compose file goes back to hand-picking.)

Validate before deploying:

```sh
docker compose run --rm app --check-config     # in compose
lctl config check                              # anywhere else
```

Validation is aggregated — every problem in one run, each message naming the
variable and what to do about it.

## Required

No defaults. The process refuses to start without them.

| Variable | Notes |
| --- | --- |
| `LINKCTRL_BASE_URL` | Public origin, e.g. `https://links.example.com`. Builds every short URL, scopes cookies, and is trusted as a CSRF origin. No path, query or fragment. Must be `https` when `APP_ENV=production`. Also the default for the two variables below. |
| `LINKCTRL_API_KEY_PEPPER` | ≥32 bytes. `openssl rand -base64 48`. Keys the HMAC that protects every API key hash, so **changing it invalidates every existing API key**. Not rotatable in place. |
| `LINKCTRL_DATABASE_URL` | pgx-compatible DSN. Compose builds it from the `POSTGRES_*` variables. |

`API_KEY_PEPPER` and `DATABASE_URL` also accept a `_FILE` suffix pointing at a
file, for mounted secrets. Setting both forms of the same secret is an error, not
a precedence rule.

## Core

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_APP_ENV` | `production` | `development` or `production`. Production refuses insecure cookies and an http base URL. Only `development` reads a `.env` file, and only when the variable is already set in the environment — a stray `.env` on a production host cannot change how the service runs. |
| `LINKCTRL_APP_BASE_URL` | `BASE_URL` | Origin serving the dashboard, the API and `/docs`. Set it, with the next variable, to put management and short links on separate hostnames. |
| `LINKCTRL_LINK_BASE_URL` | `BASE_URL` | Origin serving short links. Every `short_url` the product emits is built from this. |
| `LINKCTRL_HTTP_ADDR` | `:8080` | Public listener. One listener regardless: the split is by `Host` header, not by port. |
| `LINKCTRL_METRICS_ADDR` | `:9090` | Prometheus listener, unauthenticated. Do not expose it. |
| `LINKCTRL_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `LINKCTRL_LOG_FORMAT` | `json` | `json` or `text`. |
| `LINKCTRL_SECURE_COOKIES` | `true` | `false` drops the `__Host-` cookie prefix for plain-HTTP development. Refused in production. |
| `LINKCTRL_DOCS_ENABLED` | `true` | Serves `/docs` and the OpenAPI document. Public when on. |
| `LINKCTRL_MIGRATE_ON_START` | `true` | `false` for change-controlled deploys; run `lctl migrate up` yourself. |
| `LINKCTRL_TRUSTED_PROXIES` | *(empty)* | Comma-separated CIDRs. **Empty means `X-Forwarded-For` is ignored**, which is the safe default: anything listed here can claim any client address. Set it to your proxy and nothing more. |
| `LINKCTRL_HTTP_READ_HEADER_TIMEOUT` | `5s` | Slowloris guard. |
| `LINKCTRL_HTTP_WRITE_TIMEOUT` | `30s` | Socket-level backstop; the connection is closed regardless of what the handler is doing. |
| `LINKCTRL_HTTP_REQUEST_TIMEOUT` | `15s` | Context deadline on the application tree. Queries abort and the client gets `504`. Not applied to the redirect tree, which has `REDIRECT_TIMEOUT` instead. `0` disables it. |
| `LINKCTRL_SERVER_TIMING` | `false` | Emits a `Server-Timing` header on the application tree, measuring server time to headers. Off by default because it publishes internal timings to anyone who asks — on a service where "does this alias exist" is the interesting question, a timing difference is an answer. |

### Two hostnames

Leave `APP_BASE_URL` and `LINK_BASE_URL` unset and the instance behaves exactly
as it always has: one origin, both trees, told apart by path.

Set them to different hosts and each answers only its own paths. The dashboard
host stops resolving aliases; the link host stops serving the dashboard, the API
and the static assets. Both keep answering `/healthz` and `/readyz`, as does any
other hostname pointed at the listener — probes come from load balancers and
container runtimes that do not know the operator's names.

```sh
LINKCTRL_BASE_URL=https://manage.example.com
LINKCTRL_APP_BASE_URL=https://manage.example.com
LINKCTRL_LINK_BASE_URL=https://lnk.example.com
```

Worth understanding before choosing:

- **Session cookies cannot reach the link host.** They carry the `__Host-`
  prefix, which forbids a `Domain` attribute, so the browser locks them to the
  host that set them. This is the reason to do it: short links are the surface
  that gets pasted into forums and probed by strangers, and it needs no
  credentials at all.
- **A request to the wrong host is `404`, never a redirect.** A cross-host
  redirect reachable through the alias namespace would be an open redirector for
  anyone able to create a link.
- **Both names must resolve to this listener** and, behind a proxy, both need a
  certificate. There is still one listener and one process.
- **Existing short links keep working only if the old host stays the link host.**
  Moving links to a new hostname breaks every URL already printed or bookmarked;
  the product cannot fix that, because the old host is what people have.
- **Reserved aliases stay reserved** even though nothing on the link host could
  collide with them. An instance can be merged back onto one host later, and an
  alias called `login` created in the meantime would break the dashboard.
- **The link host's root answers `404` until you point it somewhere.** Someone
  who trims a short link back to the bare domain lands there. Set a destination
  on the *Account* page or with `PATCH /api/v1/domain`; it needs the
  `domains.write` permission, which the owner and admin roles hold. The
  destination is validated exactly as a link's is, and clearing it restores the
  `404`. There is no default page: an instance that says nothing about itself is
  a legitimate choice.

## Database

Two pools. The redirect pool is separate so a slow analytics query on the
application pool cannot leave a redirect waiting to acquire a connection.

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_DB_MAX_CONNS` | `20` | Application pool ceiling. |
| `LINKCTRL_DB_MIN_CONNS` | `2` | Kept warm. |
| `LINKCTRL_DB_REDIRECT_MAX_CONNS` | `6` | Redirect pool ceiling. |
| `LINKCTRL_DB_MAX_CONN_LIFETIME` | `1h` | |
| `LINKCTRL_DB_MAX_CONN_IDLE_TIME` | `15m` | |
| `LINKCTRL_DB_CONNECT_TIMEOUT` | `10s` | |

Startup **refuses** when `DB_MAX_CONNS + DB_REDIRECT_MAX_CONNS` exceeds 90,
which approaches Postgres's default `max_connections` of 100. It is a validation
error, not a warning: a pool that cannot get a connection fails requests, and
finding that out at startup is cheaper than finding it out under load. Raise
`max_connections` on the server before raising the pools past it.

## Cache

Redis is a cache and nothing else — no persistence, LRU eviction, any key may
vanish at any moment. Losing it makes redirects slower, never wrong.

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_CACHE_ENABLED` | `true` | `false` skips Redis entirely and serves from the in-process cache plus Postgres. |
| `LINKCTRL_REDIS_URL` | `redis://redis:6379/0` | |
| `LINKCTRL_REDIS_DIAL_TIMEOUT` | `1s` | |
| `LINKCTRL_REDIS_READ_TIMEOUT` | `50ms` | Deliberately short. A slow cache must not become slow redirects; the resolver falls through to Postgres. |
| `LINKCTRL_REDIS_POOL_SIZE` | `50` | |

Redis being unavailable at startup is a warning, not a failure.

## Redirects

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_REDIRECT_TTL` | `24h` | How long a resolved link stays cached. Clamped down automatically for links that expire sooner. Also the staleness window if you run more than one replica — see [deployment.md](deployment.md#scaling-honestly). |
| `LINKCTRL_REDIRECT_NEGATIVE_TTL` | `60s` | Caching of unknown aliases, which is most of what a public shortener is asked for. Capped at 5m by validation, and cleared when a matching link is created, so a probed-then-created alias is never stuck as a 404. |
| `LINKCTRL_REDIRECT_DEFAULT_STATUS` | `302` | `302` or `307` only. `301`/`308` are refused: a permanent redirect cached in browsers and intermediaries cannot be recalled, and links here are editable by design. |
| `LINKCTRL_REDIRECT_LOG_SAMPLE` | `0` | Log one in N successful redirects; `0` disables. At 2,000 rps, logging every redirect produces more bytes than the redirects. |
| `LINKCTRL_REDIRECT_TIMEOUT` | `250ms` | Bounds the Postgres fallback. A query still running after this has already missed the uncached target and is holding a connection from the small redirect pool while requests queue behind it. |
| `LINKCTRL_REDIRECT_404_RATE_LIMIT` | `60` | Misses per address per minute. See [Rate limits](#rate-limits). `0` disables. |

## Aliases and destinations

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_ALIAS_LENGTH` | `7` | Generated alias length, 4–12. |
| `LINKCTRL_ALIAS_MIN_USER_LENGTH` | `3` | Floor for custom aliases. |
| `LINKCTRL_ALIAS_RESERVED_EXTRA` | *(empty)* | Comma-separated words to refuse, merged with the built-in list. Additions extend it and cannot shrink it: every route the server registers is on the built-in list, and an alias shadowing one would take a working page out of service. |
| `LINKCTRL_ALIAS_PROFANITY_FILTER` | `true` | `false` switches off the built-in profanity list. Reserved words are unaffected — one is taste, the other is routing correctness. |
| `LINKCTRL_DESTINATION_SCHEMES` | `http,https` | Allowlist. Validation refuses anything else, so `javascript:`, `data:` and whatever the next browser ships are excluded by default rather than by blocklist. |
| `LINKCTRL_DESTINATION_MAX_LENGTH` | `2048` | |
| `LINKCTRL_DESTINATION_BLOCK_PRIVATE_IPS` | `true` | Refuses private, loopback, link-local, carrier-NAT and cloud-metadata addresses. Turning this off lets a short link point at `169.254.169.254`, which makes the shortener a tool for having someone else's browser probe their own network. |
| `LINKCTRL_DESTINATION_BLOCKLIST` | *(empty)* | Comma-separated host suffixes to refuse. |

## Authentication

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_SIGNUP_MODE` | `closed` | `closed`, `invite` or `open`. `invite` behaves as closed until Phase 2. The first-run setup endpoint works regardless, then closes permanently. Read only by `POST /api/v1/auth/register`: there is no signup page, so `open` admits API clients and not browsers, and a registration creates a new isolated organization and workspace rather than adding a member to yours. A signup page waits for Phase 2, where invitations make the second half of that sentence stop being a surprise. |
| `LINKCTRL_SESSION_ABSOLUTE_TTL` | `720h` | Hard deadline from creation. |
| `LINKCTRL_SESSION_IDLE_TTL` | `168h` | Maximum gap between requests. Must not exceed the absolute TTL. Enforced at read time, so a change takes effect immediately. |
| `LINKCTRL_LOGIN_LOCKOUT_THRESHOLD` | `5` | Failed attempts before a 15-minute per-account lockout. |
| `LINKCTRL_ARGON2_MEMORY_KIB` | `65536` | 64 MiB. Floor of 19456 enforced (RFC 9106); lowering this is the easiest way to silently weaken password storage. |
| `LINKCTRL_ARGON2_ITERATIONS` | `3` | Minimum 2. |
| `LINKCTRL_ARGON2_PARALLELISM` | `2` | |

Parameters are recorded in each hash, so raising them does not lock anyone out —
existing hashes verify against their own recorded cost and are upgraded on the
next successful login.

## Rate limits

Three limits, all per client address, all per instance, and `0` disables any of
them. A negative value is refused rather than read as "unlimited".

| Variable | Default | Applies to |
| --- | --- | --- |
| `LINKCTRL_LOGIN_RATE_PER_MIN` | `10` | The endpoints that verify a credential: login, register, first-run setup, password change — both the API and the dashboard forms, sharing one budget so alternating between them gains nothing. |
| `LINKCTRL_API_RATE_PER_MIN` | `600` | Everything under `/api/v1`. Dashboard pages, static assets and the health endpoints are not counted. |
| `LINKCTRL_REDIRECT_404_RATE_LIMIT` | `60` | Misses on the redirect path, and misses only. |

Four properties are worth knowing before you tune them.

**Per address means per address as the server sees it.** Behind a reverse proxy
with `TRUSTED_PROXIES` empty, every request appears to come from the proxy, so all
your traffic shares one bucket and the limit applies to the whole world at once.
Setting `TRUSTED_PROXIES` is a correctness requirement once a limit is on, not
just an analytics nicety.

**IPv6 is keyed by /64, not by address.** A single host is routinely handed a /64,
so per-address keying would let one machine present unlimited identities.

**Per instance.** The limiter is in memory: no Redis round trip on the redirect
path, and no dependency on a cache that is optional by design. With N replicas the
effective limit is N times the number.

**The 404 limit charges misses only, and never a request that costs nothing.**
A working link never spends anything, so a popular link cannot throttle its own
audience. Paths that could not be an alias — `/favicon.ico`, `/robots.txt`,
`/wp-login.php` — are refused on shape before any lookup and are not charged
either. And while an address *is* throttled, links already in the in-process cache
still resolve; only a lookup it would have to pay for is refused. That is what
keeps a tripped limit from turning into an outage.

Refusals answer `429` with `Retry-After`, as a problem document on the API and as
a page on the dashboard. `linkctrl_rate_limited_total{limit}` counts them, and
`linkctrl_rate_limit_overflow_total` is the one to alert on: it means a limiter's
key table filled and it has stopped limiting.

## Analytics

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_INGEST_QUEUE_SIZE` | `16384` | Bounded buffer. When full, clicks are **dropped** rather than delaying a redirect. Drops are counted and alertable. |
| `LINKCTRL_INGEST_BATCH_SIZE` | `500` | Must not exceed the queue size. |
| `LINKCTRL_INGEST_FLUSH_INTERVAL` | `250ms` | |
| `LINKCTRL_ANALYTICS_RETENTION_DAYS` | `395` | 13 months. Enforced hourly by dropping monthly partitions of `click_events` and `visitors`, and only once a partition's newest possible row is outside the window — so raw data survives up to a month longer than the number says. Rollups are separate tables and are never dropped, so charts outlive the events. `audit_logs` is partitioned too and deliberately exempt: audit retention is a different policy. `0` keeps everything. |
| `LINKCTRL_GEOIP_MMDB_PATH` | *(empty)* | A MaxMind DB file. Resolves a country at ingest, from the address, before it is discarded — nothing is stored to resolve later. Empty disables geographic reporting and the dashboard says so. See [deployment.md](deployment.md#optional-geographic-analytics). |

Country is the only geographic field stored. Region and city are in the same
database and in the schema, and are deliberately left null: nothing in the product
shows them, and city plus timestamp is close to a location history.

## Shutdown

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_SHUTDOWN_DRAIN_DELAY` | `5s` | Readiness fails first, then this delay, so a load balancer deregisters the instance before the listener closes. |
| `LINKCTRL_SHUTDOWN_TIMEOUT` | `15s` | Bounds in-flight requests plus the final click flush. |

Their sum must stay under the container's stop grace period (30s in the shipped
compose file), or Docker sends `SIGKILL` mid-flush and loses the buffered clicks
graceful shutdown exists to save. Validation refuses a sum over 25s.

## Postgres container

Read by the Postgres image, not by LinkCtrl.

| Variable | Default | Notes |
| --- | --- | --- |
| `POSTGRES_PASSWORD` | *(required)* | Baked into the data volume on first start. Changing it later does **not** change the database's password. |
| `POSTGRES_USER` | `linkctrl` | |
| `POSTGRES_DB` | `linkctrl` | |
| `REDIS_MAXMEMORY` | `256mb` | |
| `LINKCTRL_HTTP_PORT` | `8080` | Host port compose publishes. |
| `POSTGRES_PORT` / `REDIS_PORT` | `55432` / `56379` | Published by the *development* override only, on non-default ports so they cannot collide with a local install. |
| `LINKCTRL_IMAGE` | `ghcr.io/devofpie/linkctrl` | Image repository, paired with `LINKCTRL_TAG`. Point it at a local name to run an image you built rather than one from the registry. |
| `LINKCTRL_ENV_PATH` | `.env` | Which file compose loads into the container. `--env-file` alone does not change this: it redirects the file compose *interpolates* from, while the container still receives whatever this names. Running two instances side by side needs both. |
| `LINKCTRL_RESTART` | `unless-stopped` | Restart policy for all three services. `no` for a stack that should stay down once stopped. |
| `LINKCTRL_METRICS_PORT` | `9090` | Host port for the metrics listener, bound to `127.0.0.1` and published by the *development* override only. The base file publishes nothing here; the endpoint is unauthenticated. |

## Removed variables

These existed, did nothing, and are gone. The fixed behaviour was the design
rather than a default waiting to be overridden, so the variable went instead of
the behaviour becoming configurable. Startup warns if one is still set in your
`.env`, and `lctl config check` reports it — a stale line is worth saying out loud
and is not a reason to refuse to boot.

| Variable | Behaviour that is now fixed |
| --- | --- |
| `LINKCTRL_INGEST_WORKERS` | One ingest consumer, which is what makes batch coalescing work. |
| `LINKCTRL_VISITOR_SALT_ROTATION` | One UTC day, the period the purge window de-identifies against. |
| `LINKCTRL_BOT_FILTER_ENABLED` | Bots are always classified and recorded; headline figures exclude them in the queries. |
