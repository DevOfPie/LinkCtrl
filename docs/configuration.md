# Configuration

Every setting is an environment variable prefixed `LINKCTRL_`. The `POSTGRES_*`
variables are the exception: they are consumed by the Postgres container itself
and deliberately sit outside the prefix.

`.env.example` is the annotated copy to start from. This document is the complete
reference, including which variables **currently do nothing** — a knob that
accepts a value and has no effect is worse than a missing one, so they are marked
rather than quietly listed.

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
| `LINKCTRL_BASE_URL` | Public origin, e.g. `https://links.example.com`. Builds every short URL, scopes cookies, and is trusted as a CSRF origin. No path, query or fragment. Must be `https` when `APP_ENV=production`. |
| `LINKCTRL_SECRET_KEY` | ≥32 bytes. `openssl rand -base64 48`. |
| `LINKCTRL_API_KEY_PEPPER` | ≥32 bytes. Keys the HMAC that protects every API key hash, so **changing it invalidates every existing API key**. Not rotatable in place. |
| `LINKCTRL_DATABASE_URL` | pgx-compatible DSN. Compose builds it from the `POSTGRES_*` variables. |

`SECRET_KEY`, `API_KEY_PEPPER`, `DATABASE_URL` and `SMTP_PASSWORD` also accept a
`_FILE` suffix pointing at a file, for mounted secrets. Setting both forms of the
same secret is an error, not a precedence rule.

## Core

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_APP_ENV` | `production` | `development` or `production`. Production refuses insecure cookies and an http base URL. Only `development` reads a `.env` file, and only when the variable is already set in the environment — a stray `.env` on a production host cannot change how the service runs. |
| `LINKCTRL_HTTP_ADDR` | `:8080` | Public listener. |
| `LINKCTRL_METRICS_ADDR` | `:9090` | Prometheus listener, unauthenticated. Do not expose it. |
| `LINKCTRL_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `LINKCTRL_LOG_FORMAT` | `json` | `json` or `text`. |
| `LINKCTRL_SECURE_COOKIES` | `true` | `false` drops the `__Host-` cookie prefix for plain-HTTP development. Refused in production. |
| `LINKCTRL_DOCS_ENABLED` | `true` | Serves `/docs` and the OpenAPI document. Public when on. |
| `LINKCTRL_MIGRATE_ON_START` | `true` | `false` for change-controlled deploys; run `lctl migrate up` yourself. |
| `LINKCTRL_TRUSTED_PROXIES` | *(empty)* | Comma-separated CIDRs. **Empty means `X-Forwarded-For` is ignored**, which is the safe default: anything listed here can claim any client address. Set it to your proxy and nothing more. |
| `LINKCTRL_HTTP_READ_HEADER_TIMEOUT` | `5s` | Slowloris guard. |
| `LINKCTRL_HTTP_WRITE_TIMEOUT` | `30s` | |

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

Startup warns when `DB_MAX_CONNS + DB_REDIRECT_MAX_CONNS` approaches Postgres's
default `max_connections` of 100.

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

## Aliases and destinations

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_ALIAS_LENGTH` | `7` | Generated alias length, 4–12. |
| `LINKCTRL_ALIAS_MIN_USER_LENGTH` | `3` | Floor for custom aliases. |
| `LINKCTRL_DESTINATION_SCHEMES` | `http,https` | Allowlist. Validation refuses anything else, so `javascript:`, `data:` and whatever the next browser ships are excluded by default rather than by blocklist. |
| `LINKCTRL_DESTINATION_MAX_LENGTH` | `2048` | |
| `LINKCTRL_DESTINATION_BLOCK_PRIVATE_IPS` | `true` | Refuses private, loopback, link-local, carrier-NAT and cloud-metadata addresses. Turning this off lets a short link point at `169.254.169.254`, which makes the shortener a tool for having someone else's browser probe their own network. |
| `LINKCTRL_DESTINATION_BLOCKLIST` | *(empty)* | Comma-separated host suffixes to refuse. |

## Authentication

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_SIGNUP_MODE` | `closed` | `closed`, `invite` or `open`. `invite` behaves as closed until Phase 2. The first-run setup endpoint works regardless, then closes permanently. |
| `LINKCTRL_SESSION_ABSOLUTE_TTL` | `720h` | Hard deadline from creation. |
| `LINKCTRL_SESSION_IDLE_TTL` | `168h` | Maximum gap between requests. Must not exceed the absolute TTL. Enforced at read time, so a change takes effect immediately. |
| `LINKCTRL_LOGIN_LOCKOUT_THRESHOLD` | `5` | Failed attempts before a 15-minute per-account lockout. This *is* enforced. |
| `LINKCTRL_ARGON2_MEMORY_KIB` | `65536` | 64 MiB. Floor of 19456 enforced (RFC 9106); lowering this is the easiest way to silently weaken password storage. |
| `LINKCTRL_ARGON2_ITERATIONS` | `3` | Minimum 2. |
| `LINKCTRL_ARGON2_PARALLELISM` | `2` | |

Parameters are recorded in each hash, so raising them does not lock anyone out —
existing hashes verify against their own recorded cost and are upgraded on the
next successful login.

## Analytics

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_INGEST_QUEUE_SIZE` | `16384` | Bounded buffer. When full, clicks are **dropped** rather than delaying a redirect. Drops are counted and alertable. |
| `LINKCTRL_INGEST_BATCH_SIZE` | `500` | Must not exceed the queue size. |
| `LINKCTRL_INGEST_FLUSH_INTERVAL` | `250ms` | |

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

## Accepted but not yet in effect

These parse and validate. Nothing reads them. Setting one changes no behaviour,
and they are listed so that is discovered here rather than in production.

The next milestone empties this section. Each variable below either starts taking
effect or is removed — which one it is, is decided, and is the split here.

### Will start taking effect

| Variable | What is missing |
| --- | --- |
| `LINKCTRL_LOGIN_RATE_PER_MIN` | Per-IP rate limiting is not implemented. Per-account lockout is. |
| `LINKCTRL_API_RATE_PER_MIN` | Same. Rate-limit at the reverse proxy for now. |
| `LINKCTRL_REDIRECT_404_RATE_LIMIT` | 404 probe limiting is not implemented. |
| `LINKCTRL_GEOIP_MMDB_PATH` | Validated (a missing file fails startup) but never read; country breakdowns stay empty and the dashboard says so. |
| `LINKCTRL_ANALYTICS_RETENTION_DAYS` | Partitions are created, never dropped. Click data grows until you drop partitions yourself. |
| `LINKCTRL_HTTP_REQUEST_TIMEOUT` | No per-request deadline is applied. |
| `LINKCTRL_REDIRECT_TIMEOUT` | The redirect path uses the request context, not this. |
| `LINKCTRL_SERVER_TIMING` | No `Server-Timing` header is emitted. |
| `LINKCTRL_ALIAS_RESERVED_EXTRA` | The built-in reserved list applies; extras are not merged in. |
| `LINKCTRL_ALIAS_PROFANITY_FILTER` | The built-in filter always applies; this cannot switch it off. |

### Will be removed

Do not set these. The fixed behaviour is the design, not a limitation waiting to
be lifted, so the variable is going rather than the behaviour changing. Startup
will warn if one is still set.

| Variable | Fixed behaviour that stays |
| --- | --- |
| `LINKCTRL_INGEST_WORKERS` | One ingest worker. A single consumer is what makes batch coalescing work. |
| `LINKCTRL_VISITOR_SALT_ROTATION` | One UTC day. The purge is keyed to it, and a longer period would weaken de-identification. |
| `LINKCTRL_BOT_FILTER_ENABLED` | Bots are always classified and recorded; charts already exclude them from headline figures. |

Tracked in [Plan.md](../Plan.md#phase-1-scope-not-yet-built).
