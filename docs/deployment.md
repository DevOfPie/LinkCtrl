# Deployment

Running LinkCtrl for real: one host, Docker Compose, a reverse proxy for TLS.
For a local trial, the quick start in the [README](../README.md) is enough — this
document is the version you follow when the links are printed on something.

Every command here has been run against the shipped compose file. Where
something is *not* implemented yet, it says so rather than describing what a
future version will do.

## What you need

| | |
| --- | --- |
| A host | 1 vCPU and 1 GB RAM is enough to start. Postgres wants the RAM; the app is idle between requests. |
| Docker Engine + Compose v2 | `docker compose version` should print v2.x |
| A domain | With an A/AAAA record pointing at the host |
| Ports 80 and 443 | For the reverse proxy. LinkCtrl itself listens on 8080 and never needs to be exposed directly. |

Postgres 17 and Redis 7 come from the compose file. Nothing else is required —
no Node, no Python, no build toolchain.

## 1. Get the code and set the secrets

```sh
git clone https://github.com/DevOfPie/LinkCtrl.git
cd LinkCtrl
cp .env.example .env
```

Generate three independent secrets:

```sh
openssl rand -base64 48   # LINKCTRL_SECRET_KEY
openssl rand -base64 48   # LINKCTRL_API_KEY_PEPPER
openssl rand -base64 32   # POSTGRES_PASSWORD
```

Edit `.env`:

```ini
LINKCTRL_BASE_URL=https://links.example.com
LINKCTRL_SECRET_KEY=<48 random bytes>
LINKCTRL_API_KEY_PEPPER=<48 random bytes>
POSTGRES_PASSWORD=<32 random bytes>

LINKCTRL_APP_ENV=production
LINKCTRL_SIGNUP_MODE=closed
LINKCTRL_TRUSTED_PROXIES=172.16.0.0/12
```

Four things about that file are worth more than a glance:

- **`LINKCTRL_BASE_URL` must be the public origin, with `https`.** It builds
  every short URL, scopes cookies, and is trusted as a CSRF origin. Getting it
  wrong produces short links that point somewhere useless.
- **`APP_ENV=production` is enforced, not decorative.** It refuses to start with
  `SECURE_COOKIES=false` or an `http://` base URL, because session cookies use
  the `__Host-` prefix and browsers silently discard those over plain HTTP.
- **`API_KEY_PEPPER` is not rotatable in place.** Every API key's hash is keyed
  with it; changing it invalidates every existing key at once.
- **`TRUSTED_PROXIES` must list your proxy and nothing else.** It is empty by
  default, and that default is the safe one: with it set, `X-Forwarded-For` is
  believed, and anything in that list can claim any client address — which
  corrupts analytics and would defeat rate limiting if there were any. See
  [step 3](#3-put-tls-in-front).

Secrets can come from files instead of the environment, for Docker or Swarm
secrets:

```ini
LINKCTRL_API_KEY_PEPPER_FILE=/run/secrets/api_key_pepper
```

Supported for `SECRET_KEY`, `API_KEY_PEPPER`, `DATABASE_URL` and
`SMTP_PASSWORD`. Setting both the inline and `_FILE` form for the same secret is
an error rather than a silent precedence rule.

Save `.env` with **LF line endings**. A CRLF makes `POSTGRES_PASSWORD` end in an
invisible carriage return, Postgres initialises with a password nobody can type,
and every later connection fails authentication for no visible reason.

## 2. Check the configuration before starting anything

```sh
docker compose run --rm app --check-config
```

Validation is aggregated: if six values are wrong you get six messages, each
naming the variable and what to do about it. This is much cheaper than reading a
crash loop.

Then bring the stack up:

```sh
docker compose up -d --wait
```

`--wait` blocks until the healthchecks pass. The app waits for Postgres to be
*healthy*, not merely started, so a cold boot does not race `initdb`.

Migrations run in-process before the listener opens, serialised across replicas
by a Postgres session lock. There is no separate migration step, and a request
can never reach a half-migrated schema.

## 3. Put TLS in front

LinkCtrl does not terminate TLS. Run a reverse proxy; Caddy needs the least
configuration and gets certificates itself.

`/etc/caddy/Caddyfile`:

```caddyfile
links.example.com {
	encode zstd gzip

	reverse_proxy localhost:8080 {
		header_up X-Forwarded-For {remote_host}
		header_up X-Forwarded-Proto {scheme}
	}

	# LinkCtrl sets its own security headers, including HSTS in production.
	# Do not duplicate them here; duplicates are how a policy ends up with two
	# conflicting values.
}
```

With nginx, the equivalent essentials:

```nginx
location / {
	proxy_pass http://127.0.0.1:8080;
	proxy_set_header Host              $host;
	proxy_set_header X-Forwarded-For   $remote_addr;
	proxy_set_header X-Forwarded-Proto $scheme;
}
```

Then set `LINKCTRL_TRUSTED_PROXIES` to the proxy's address as LinkCtrl sees it.
If the proxy runs on the host and the app in compose, that is the Docker bridge
range (commonly `172.16.0.0/12`). If it runs as another compose service, use that
network's subnet.

To confirm it works, click a link and check that the analytics show a plausible
device rather than everything arriving from one address. Getting this wrong is
invisible in the logs and only shows up as flattened analytics.

Two things not to forward:

- **`:9090`.** The metrics listener has no authentication. It reports queue
  depths, pool saturation and traffic shape. Compose does not publish it, and
  the proxy should not reach it.
- **`/api/v1/openapi.json` if you set `LINKCTRL_DOCS_ENABLED=false`.** It is
  public by default, which is usually what you want; the switch is there for
  instances that should describe nothing.

## 4. Claim the instance

Visit `https://links.example.com`. A fresh instance redirects to a setup form
that creates the first account as an owner, then returns 404 forever after.

On a headless box, or if you would rather not use a browser:

```sh
docker compose exec -T app /lctl apikey create \
  --user you@example.com --name bootstrap --scopes links.read,links.create
```

That requires an existing user, so claim the instance through the web form or
the JSON API first:

```sh
curl -sS -X POST https://links.example.com/api/v1/auth/setup \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","name":"You","password":"a-long-passphrase"}'
```

`SIGNUP_MODE=closed` (the default) means nobody else can register. The setup
endpoint is exempt — otherwise a closed instance could never create its first
account — and it closes the moment it succeeds.

## 5. Back it up

Everything that matters is in Postgres. Redis is a cache and can be lost without
consequence; the app is a stateless image.

```sh
docker compose exec -T postgres \
  pg_dump -U linkctrl -Fc linkctrl > linkctrl-$(date -u +%Y%m%d).dump
```

Restore into an empty database:

```sh
docker compose exec -T postgres \
  pg_restore -U linkctrl -d linkctrl --clean --if-exists < linkctrl-20260730.dump
```

Two notes specific to this schema:

- `click_events`, `visitors` and `audit_logs` are month-partitioned, and
  `pg_dump -Fc` handles the partitions correctly. Do not hand-roll a per-table
  dump script that misses next month's partition.
- Analytics salts are deleted after two days *by design*. A restored backup
  cannot recompute unique visitors for days whose salts had already been purged.
  That is the privacy guarantee working, not data loss.

Verify a restore somewhere else occasionally. An untested backup is a hope.

## 6. Upgrades

```sh
git pull
docker compose pull        # or: docker compose build
docker compose up -d --wait
```

Migrations apply at boot. The compose file sets `stop_grace_period: 30s`, which
must remain longer than `SHUTDOWN_DRAIN_DELAY + SHUTDOWN_TIMEOUT` plus the final
click flush — otherwise Docker sends `SIGKILL` mid-flush and discards the
buffered clicks that graceful shutdown exists to save.

For change-controlled environments, set `LINKCTRL_MIGRATE_ON_START=false` and run
migrations deliberately:

```sh
docker compose run --rm --entrypoint /lctl app migrate up
```

Rolling back a migration is `lctl migrate down`, one step at a time. Test it on
a copy first: `down` migrations drop columns, and a rollback after real traffic
loses whatever those columns held.

## Optional: geographic analytics

Off unless you supply a database. MaxMind's licence does not allow redistributing
one in the image, which is why this is optional at runtime rather than built in —
without it the dashboard says geographic data is unavailable instead of drawing an
empty chart.

Download a **GeoLite2-Country** `.mmdb` (a free MaxMind account; the City database
also works), mount it read-only, and point the variable at it:

```yaml
services:
  app:
    volumes:
      - ./geoip/GeoLite2-Country.mmdb:/geoip/country.mmdb:ro
    environment:
      LINKCTRL_GEOIP_MMDB_PATH: /geoip/country.mmdb
```

Startup logs the database's own type and build date, so a wrong or stale file is
visible in the log rather than as thin data weeks later. An unreadable path fails
configuration validation; a file that is not a MaxMind database fails at startup
rather than resolving nothing forever.

Two things worth knowing:

- **The country is resolved at ingest**, from the address, in the same place the
  visitor hash is derived — because that is the last moment the address exists.
  There is no stored address to enrich later, by design.
- **Only the country is stored.** Region and city are in the same database and
  have columns in the schema, and are deliberately left null. Nothing in the
  product displays them, and city plus a timestamp is close to a location history.
  That is a decision, not an omission.

Updating the database is a file replacement plus a restart. Nothing is
pre-computed, so a newer database changes only future clicks; historical rows keep
the country they were resolved with.

## Hardening already in place

Worth knowing so you do not spend an afternoon re-adding it:

- The app container runs `read_only`, as a non-root user, with `cap_drop: ALL`
  and `no-new-privileges`. `/tmp` is a tmpfs.
- The runtime image is distroless: no shell, no package manager, no curl. The
  healthcheck is the binary probing itself.
- Postgres initialises with `--data-checksums` and runs with `timezone=UTC`.
  UTC is not cosmetic: partition bounds on `timestamptz` resolve against the
  session timezone, and a non-UTC session creates partitions offset by the
  UTC offset, leaving gaps that silently swallow rows.
- Redis runs with no persistence and `allkeys-lru`. Any key may vanish at any
  moment, which is exactly what the design assumes.
- Log files rotate at 10 MB × 3 per service.

## Scaling, honestly

Phase 1 is designed for one app instance, and one limitation decides it:
**cache invalidation is single-replica.** Editing a link clears the cache on the
replica that handled the edit; other replicas serve the old destination until
the entry's TTL expires (`REDIRECT_TTL`, 24h by default). Phase 2 adds pub/sub.

Until then:

- Run one `app` container. It is a Go binary serving cached redirects; that goes
  a long way.
- If you must run more, lower `REDIRECT_TTL` to bound the staleness window and
  accept that edits take up to that long to reach every replica.
- Vertical growth first: Postgres `shared_buffers` and the two pool sizes
  (`DB_MAX_CONNS`, `DB_REDIRECT_MAX_CONNS`) are the knobs that matter. Keep
  their total under the server's `max_connections`; startup warns when the sum
  approaches 100.

## When it will not start

The failures worth recognising immediately:

| Symptom | Cause |
| --- | --- |
| `configuration is invalid:` followed by a list | Exactly what it says; each line names a variable. Nothing has connected yet. |
| `password authentication failed for user "linkctrl"` | `POSTGRES_PASSWORD` changed after the volume was initialised — the database keeps the *original* password. Either restore the old value or recreate the volume. Also check for a CRLF in `.env`. |
| `BASE_URL: must use https in production` | `APP_ENV=production` with an `http://` base URL. Refused because `__Host-` cookies would be silently dropped. |
| `session timezone is not UTC` | Something overrode the Postgres timezone. Partitioning depends on UTC; fix it rather than working around it. |
| Healthcheck never passes, logs show migration waiting | Another instance holds the migration lock, or a previous crash left it. It is a session lock, so it releases when that connection dies. |
| Redirects work, dashboard is unstyled | The image was built without `make css`. Rebuild; the server also warns about this at boot. |

More, including the metrics to watch and what each alert means, in
[operations.md](operations.md).
