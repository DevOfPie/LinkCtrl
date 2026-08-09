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

Generate two independent secrets:

```sh
openssl rand -base64 48   # LINKCTRL_API_KEY_PEPPER
openssl rand -base64 32   # POSTGRES_PASSWORD
```

Edit `.env`:

```ini
LINKCTRL_BASE_URL=https://links.example.com
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
  corrupts analytics and defeats the per-address rate limits. See
  [step 3](#3-put-tls-in-front).

One more file matters here: **`docker-compose.override.yml` is applied
automatically** whenever `docker compose` runs in the checkout, and it carries
development conveniences — published database ports among them. Its values are
written as defaults, so your `.env` settings above win either way, but for
production run compose with the base file only:

```sh
docker compose -f docker-compose.yml up -d --wait
```

That skips the override entirely, which is also what keeps Postgres and Redis
off the host's network interfaces. (An earlier version of the override
hard-coded `APP_ENV: development`, which silently overrode the `.env` above and
deployed dev mode; the values are defaults now precisely so forgetting `-f`
cannot do that again.)

Secrets can come from files instead of the environment, for Docker or Swarm
secrets:

```ini
LINKCTRL_API_KEY_PEPPER_FILE=/run/secrets/api_key_pepper
```

Supported for `API_KEY_PEPPER` and `DATABASE_URL`. Setting both the inline and
`_FILE` form for the same secret is an error rather than a silent precedence
rule.

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

Then bring the stack up (base file only, per step 1):

```sh
docker compose -f docker-compose.yml up -d --wait
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

To serve the dashboard and short links on separate hostnames, give both names to
the same backend and set the two origins in `.env`:

```caddyfile
manage.example.com, lnk.example.com {
	encode zstd gzip

	reverse_proxy localhost:8080 {
		header_up X-Forwarded-For {remote_host}
		header_up X-Forwarded-Proto {scheme}
	}
}
```

```sh
LINKCTRL_BASE_URL=https://manage.example.com
LINKCTRL_APP_BASE_URL=https://manage.example.com
LINKCTRL_LINK_BASE_URL=https://lnk.example.com
```

Compose loads `.env` into the app service wholesale, so these take effect on the
next `docker compose up -d`. Confirm it before trusting the split: the link host
must answer `/login` with a 404. If it serves the sign-in page, the two-hostname
configuration did not reach the process and sessions are still being minted on
the host that serves other people's destinations.

```sh
curl -so /dev/null -w '%{http_code}\n' https://lnk.example.com/login    # want 404
```

One listener and one process either way — the routing is by `Host`. Caddy will
get a certificate for both names. The dashboard host then refuses to resolve
aliases and the link host refuses everything except links and the health
endpoints, so a session cookie is never sent to the host serving other people's
destinations. See [configuration.md](configuration.md#two-hostnames) for what
changes and what does not.

**Do not switch the link host on an instance that already has traffic.** Every
short URL already printed, bookmarked or embedded names the old host, and nothing
in the product can rewrite somebody else's copy.

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

## Custom domains

A workspace can register a hostname of its own and serve its short links on it.
Two things have to be true before that happens, and this section is both of them.

### What the application does, and what it does not

LinkCtrl **never speaks ACME**. It obtains no certificates, contacts no
certificate authority, and holds no account key. Certificates for custom
hostnames are your proxy's, exactly as the certificates for your own hostnames
are. All this application does is answer one question — *should you get a
certificate for this name?* — and it answers yes only for hostnames a workspace
has proved it controls.

What it does do is **verify control by DNS**, on a cadence, and stop serving a
hostname whose proof has gone away.

### The customer's half

A workspace registers `go.customer.example` on the Domains page. LinkCtrl shows
them one TXT record. They publish it, and point the hostname at this instance:

```
_linkctrl-challenge.go.customer.example.  IN  TXT  "b7f0…"      # from the page
go.customer.example.                      IN  CNAME  lnk.example.com.
```

Then they press **Check DNS**. Until that check passes, a request arriving with
`Host: go.customer.example` gets the operational 404 and nothing else — no link,
no dashboard, and never a redirect to your own hostname. **That gap is the whole
point**: without it, anybody who pointed a hostname at your address could serve
short links on it.

Afterwards, the header is matched by the hostname alone: `go.customer.example`,
`go.customer.example.` and `go.customer.example:8443` all name the same verified
hostname and are served identically. That is not true of `BASE_URL`,
`APP_BASE_URL` and `LINK_BASE_URL` — a non-default port there is part of the
hostname you configured, so a deployment may serve the dashboard and short links
on one name and two ports and they stay two trees.

### Your half: on-demand TLS

Caddy asks LinkCtrl before obtaining a certificate for a name it was not
configured with. Add the `ask` endpoint and an on-demand site block:

```caddyfile
{
	on_demand_tls {
		# LinkCtrl answers 200 for a verified custom hostname and 404 for
		# everything else. Without this, Caddy would obtain a certificate for
		# any name pointed at this address — which is the abuse `ask` exists to
		# prevent, and the reason this endpoint answers for verified domains
		# only.
		ask http://localhost:8080/tls-check
	}
}

# Your own hostnames, configured statically, exactly as before.
manage.example.com, lnk.example.com {
	encode zstd gzip
	reverse_proxy localhost:8080 {
		header_up X-Forwarded-For {remote_host}
		header_up X-Forwarded-Proto {scheme}
	}
}

# Everything else: certificates on demand, subject to the ask above.
https:// {
	tls {
		on_demand
	}
	encode zstd gzip
	reverse_proxy localhost:8080 {
		header_up X-Forwarded-For {remote_host}
		header_up X-Forwarded-Proto {scheme}
	}
}
```

The `ask` endpoint is unauthenticated by necessity — it is consulted during a TLS
handshake, before any application request exists. It reads an in-process map,
performs at most one write per verification, and discloses only whether a name is
already being served publicly. **Do not expose it through the proxy**: Caddy
reaches it on the loopback address, and nothing else needs to.

Confirm it before trusting the setup:

```sh
curl -so /dev/null -w '%{http_code}\n' 'http://localhost:8080/tls-check?domain=go.customer.example'
# 200 once verified, 404 before that and for any name this instance does not serve
```

`ssl_status` on the domain row says what this instance knows, which is not much
and is not meant to be: `none` before verification, `pending` once it will answer
the ask, `active` once the ask has been answered. The certificate itself is your
proxy's and its state lives there.

### Re-verification, and what happens when a record is deleted

**Every registered hostname is re-checked hourly.** The check runs on one replica
at a time — its own Postgres advisory lock; each job family holds one — while
*serving* the hostnames is decided by an in-process set on every replica, kept in
step through Redis pub/sub. That is why an un-verification takes effect
everywhere within moments rather than on whichever replica happened to run the
job.

When a check fails:

| When | What happens |
| --- | --- |
| The first failed check | The domain is marked failing and **the owning workspace's owners are notified**, with the time serving stops. **Links keep working.** |
| Any successful check | The failing state is cleared and the clock is reset. Nothing was interrupted. |
| **24 hours** of continuous failure | **Serving stops.** `verified_at` is cleared, the hostname goes back to the operational 404 on every replica, the workspace is notified again, and a `domain.unverified` record is written to the audit log. |

**One day, checked hourly** — so a hostname must fail twenty-four consecutive
checks before its links go dark. That is deliberate on both sides. A single
failed DNS poll is weak evidence: one resolver hiccup or a brief nameserver
outage would otherwise take a paying customer's links down with no human in the
loop. And the window is a real deadline rather than a warning that repeats: for
as long as it runs, this instance is serving a hostname whose DNS its owner may
already have lost, and a grace period that never expires is not a grace period.

Both numbers are yours to change:

```sh
LINKCTRL_DOMAIN_VERIFY_INTERVAL=1h    # how often every registered hostname is re-checked
LINKCTRL_DOMAIN_VERIFY_GRACE=24h      # how long a failing hostname keeps serving
```

Setting the interval to `0` switches the periodic pass off entirely and leaves
verification on-demand only — which also means a hostname that stops verifying
keeps serving indefinitely. Do that only if something else is watching.

**A pass checks the hostnames you are serving first.**
`LINKCTRL_DOMAIN_VERIFY_BATCH` bounds how many hostnames one pass looks at;
serving hostnames are drawn first and take what they need of it, and
registrations that are not yet served take what is left. The stop above can
therefore be delayed only by other serving hostnames, never by a pile of
registrations — anybody with an account can create those, and renaming one puts
it back at the front of the queue. Raising the batch does not help with a slow
nameserver and makes this worse: it is more lookups inside the same pass, each
able to block for `LINKCTRL_DOMAIN_VERIFY_DNS_TIMEOUT`.

**A workspace may register at most twenty-five hostnames.** Every registration is
a recurring DNS lookup this instance owes, aimed at a nameserver the registrant
chose, so the cap bounds the work one workspace can create for everyone else. It
is a constant rather than a setting, and a workspace that reaches it is told to
remove a hostname it no longer serves.

Two changes take effect immediately and do not wait for the window, because
neither is a failed check:

- **Renaming a hostname un-verifies it.** The published record proves control of
  the old name and says nothing about the new one. A fresh token is minted, and
  the domain serves nothing until the new record is published and checked. A
  rename that lands *while* a check is in flight makes that check verify nothing
  and say so — it proved control of a name the row no longer carries, and the
  remedy is to check again.
- **Removing a hostname** stops it being served everywhere at once. Removal is
  refused while any link is on the domain, because every one of them would stop
  resolving.

### If a customer says their links stopped working

1. Read the domain's row on their Domains page. It names the record it expected
   and what the last check actually found.
2. Check it yourself: `dig +short TXT _linkctrl-challenge.go.customer.example`.
3. If the record is there and correct, re-check from the page — verification
   resumes serving immediately, with no waiting period in either direction.
4. `docker compose logs app | grep 'custom domain verification'` shows what each
   hourly pass concluded, and the audit log holds `domain.verified` and
   `domain.unverified` with timestamps.

## 4. Claim the instance

Visit `https://links.example.com`. A fresh instance redirects to a setup form
that creates the first account as an owner, then returns 404 forever after.

The form asks one question besides your name, address and password: **whether
this instance may check for new LinkCtrl releases.** It is ticked by default,
and what it does is one `GET` a day carrying this server's address and the
version it runs and nothing else — the full enumeration is beside the control
and in [configuration.md](configuration.md#update-check). Answering *no* here is
recorded on the instance; `LINKCTRL_UPDATE_CHECK=false` in your environment is
the same answer given from the deployment's side, and it overrides this one.

**Upgrading an existing instance instead?** It has no first run left to be asked
at, so the question is put on the dashboard to the first administrator who signs
in after the upgrade, and **the check does nothing until they answer it**.

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

`"update_check": false` in that body is the API's half of the same question, and
`true` is the other half. **Omitting it answers nothing**, which leaves the
check off and the question waiting on the dashboard for the first administrator
who signs in — a client that has never heard of the field cannot agree to an
outbound connection on your behalf by staying silent.

`SIGNUP_MODE=closed` (the default) means nobody else can register, and nothing
inside the running instance changes that — there is no runtime toggle, so
leaving it at `closed` settles the question until somebody edits this file. The
setup endpoint is exempt, otherwise a closed instance could never create its
first account, and it closes the moment it succeeds.

To let people in later, set `SIGNUP_MODE` here and restart. `open` additionally
needs `LINKCTRL_SMTP_HOST`, since public registration confirms the address by
email before creating the account; without a relay the instance stays at
invitation-only and says so in the log at boot.

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

**Pin a version.** `LINKCTRL_TAG` selects the image tag, and it defaults to
`latest`, which means a `docker compose pull` can change what you are running
without you choosing to:

```sh
# In .env
LINKCTRL_TAG=0.1.0
```

```sh
docker compose -f docker-compose.yml pull app
docker compose -f docker-compose.yml up -d --wait
```

For a deployment that must be reproducible, pin the digest instead — every release
publishes one, and unlike a tag it cannot be repointed:

```yaml
services:
  app:
    image: ghcr.io/devofpie/linkctrl@sha256:…
```

Read [CHANGELOG.md](../CHANGELOG.md) before upgrading. It is written for exactly
this decision, and it lists the limitations of each version rather than only its
additions.

Migrations apply at boot. The compose file sets `stop_grace_period: 30s`, which
must remain longer than `SHUTDOWN_DRAIN_DELAY + SHUTDOWN_TIMEOUT` plus the final
click flush — otherwise Docker sends `SIGKILL` mid-flush and discards the
buffered clicks that graceful shutdown exists to save.

The schema only changes additively within a minor version, so rolling back inside
a series is safe: point `LINKCTRL_TAG` at the previous version and bring it up
again. Across a minor version it may not be — see
[releasing.md](releasing.md#rolling-back).

### Without Docker

Every release also publishes static binaries — linux amd64/arm64, macOS
amd64/arm64, and Windows amd64 — with a `SHA256SUMS` file:

```sh
tar xzf linkctrl_0.1.0_linux_amd64.tar.gz
sha256sum -c SHA256SUMS --ignore-missing
./linkctrl version
```

The archive carries `linkctrl`, `lctl`, `LICENSE`, `README.md`, `CHANGELOG.md` and
`.env.example`. There is no installer and no service file: the binary reads its
configuration from the environment and needs Postgres reachable. `linkctrl
--check-config` validates a configuration before you wire up a unit file.

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
- **If you point `LINKCTRL_REDIS_URL` at a managed Redis, turn persistence off
  there too.** The shipped compose file is memory-only and this is only a
  question once you move off it. The shared rate limiter keys on the client
  address — the full address for IPv4, a `/64` for IPv6 — for the length of the
  limiter's window, 120 seconds by default. That is not a column and no address
  is stored in Postgres, which is the product's stance; it does mean a managed
  service with RDB or AOF snapshots enabled by default writes client addresses
  to disk on somebody else's infrastructure, on a retention schedule you did not
  choose. Anonymising the IPv4 side is not an option the product can take: a
  `/24` key would let one host exhaust the budget of 255 neighbours.
- Log files rotate at 10 MB × 3 per service.

## Air-gapped and egress-restricted deployments

One thing in a default 0.3.0 instance reaches the public internet on a schedule:
the daily release check. Set

```sh
LINKCTRL_UPDATE_CHECK=false
```

and restart. Nothing else in this product opens a socket outwards unless you
configure it (`SMTP_HOST`, `FEED_URL`) or a workspace registers a webhook or a
custom domain; the full accounting is the *Egress* row of
[SECURITY.md](SECURITY.md).

**What happens if you leave it on with no route out.** Nothing breaks. The check
runs on the scheduler, times out after ten seconds, writes one line at debug
level and does not retry until the next day. It cannot fail a startup, delay a
shutdown, or surface to a user. What it does cost you is the attempt: an egress
policy that logs or alerts on denied outbound connections will see one a day
from the leader replica, to `api.github.com` on 443, and somebody will
eventually have to explain it. Turning it off is cheaper than explaining it
annually.

**How loud it is, exactly.** At `LOG_LEVEL=info` — the default — you will see
nothing at all. At `debug` you get one `update check did not complete` line per
day. There is no metric, no alert and no notification for a check that fails,
because a failed check is a question that went unanswered rather than a fault.

**And the converse.** *No notification* is not evidence of being up to date. A
blocked check and a check that found nothing look identical from the dashboard.
If knowing about releases matters on a restricted network, watch the repository
rather than this instance.

**The same is true of an instance nobody signs into, and for a different
reason.** The check is off until an operator answers the question — on the setup
form for a fresh instance, on the dashboard at the first administrative sign-in
for an upgraded one — and an instance that is deployed, left running and never
signed into is never asked, so it never checks and never tells anybody a release
exists. That is the case a release notification would be most use for, and it is
the price of not answering on an operator's behalf: an upgrade cannot consent for
you. If you run instances like that, watch the repository, or claim them and
answer the question once.

## Scaling, honestly

More than one `app` container works. Each replica keeps its own in-process cache
in front of Redis, and invalidations are broadcast on a Redis pub/sub channel, so
an edit on one replica clears every replica's copy rather than only the one that
handled it. That was the limitation that made 0.1.0 a single-instance product.

What to know before running several:

- **Redis stops being only a cache.** It is still optional for correctness — with
  it down, redirects resolve from Postgres and edits still apply — but it is what
  carries invalidations between replicas. Without it, each replica serves its own
  cached copy until `REDIRECT_TTL` expires, which is the 0.1.0 behaviour.
- **A replica that loses the subscription flushes its caches, then flushes again
  when it reconnects.** Pub/sub does not replay, so it cannot know what it
  missed. Expect a brief cold cache after a Redis restart, on every replica at
  once. This covers a Redis that stalls as well as one that goes away: the
  subscriber bounds its read with `REDIS_SUBSCRIBER_READ_TIMEOUT` and makes
  Redis answer a probe before it accepts silence as *nothing has changed*.
- **The credential and API rate limits are shared across replicas; the
  404-probe limit is not.** Since 0.2.0 the first two go through Redis, so the
  configured number holds however many replicas are running — and falls back to
  per-replica buckets whenever Redis does not answer, which is the fail-open
  direction the cache-is-optional rule requires. The 404-probe limiter stays per
  instance permanently, because it guards the redirect path: N replicas allow
  roughly N times its configured limit.
- Vertical growth first: Postgres `shared_buffers` and the two pool sizes
  (`DB_MAX_CONNS`, `DB_REDIRECT_MAX_CONNS`) are the knobs that matter. Keep
  their total under the server's `max_connections`; startup refuses to run when
  the sum exceeds 90, so raise `max_connections` on Postgres first.

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
