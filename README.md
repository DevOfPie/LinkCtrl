# LinkCtrl

Self-hostable link management. A short link here is a resource you can edit,
measure, script and revoke — not a row you create once and hope about.

Runs as one Go binary with Postgres and Redis beside it. No Node in the image,
no SaaS dependency, no telemetry leaving the box.

> **Status: Phase 1, near complete.** Everything below is built, tested and
> exercised end to end. What is *not* built is listed plainly in
> [Not built yet](#not-built-yet) — including a few settings that currently
> accept a value and do nothing. Check that list before deploying anything you
> care about.

---

## Why it exists

Most shorteners make you choose between a hosted product that owns your click
data and a weekend script with no analytics. LinkCtrl aims at the third option:

- **Links stay editable.** The destination changes; the short URL does not.
  Redirects are always 302, because a 301 cached in browsers and intermediaries
  cannot be recalled.
- **Analytics that cannot identify anybody.** No IP address is stored in any
  column. Visitors are counted with a daily-rotating HMAC that is deleted after
  two days — after which those counts cannot be linked to an address by anyone,
  including you. See [Privacy](#privacy).
- **Everything the dashboard does, the API does.** Both call the same service
  layer, and a contract test replays every documented operation against a live
  server to keep it that way.
- **Fast on the path that matters.** A two-tier cache in front of a dedicated
  connection pool, on an HTTP tree that carries no session lookup, no CSRF
  check and no templates.

## Quick start

Docker and Docker Compose are the only prerequisites.

```sh
git clone https://github.com/DevOfPie/LinkCtrl.git
cd LinkCtrl
cp .env.example .env
```

Fill in the three secrets in `.env` (`openssl rand -base64 48` for each):

```sh
LINKCTRL_BASE_URL=http://localhost:8080
LINKCTRL_SECRET_KEY=…
LINKCTRL_API_KEY_PEPPER=…
POSTGRES_PASSWORD=…
```

Then:

```sh
docker compose up -d --wait
```

Open <http://localhost:8080>. The first visit lands on a setup form that creates
the owner account and then disappears permanently. Migrations run at boot, so
there is no separate install step.

For a real deployment — TLS, a reverse proxy, backups, upgrades — follow
[docs/deployment.md](docs/deployment.md) instead. It is a different set of
answers, not the same ones with a domain name.

## What you get

| | |
| --- | --- |
| **Links** | Create, edit, archive, soft-delete with a 30-day window. Custom or generated aliases, tags, titles, expiry (410 past it). Full-text and substring search, cursor pagination. |
| **Redirects** | In-process cache → Redis → Postgres, with negative caching for the unknown aliases a public shortener is mostly asked for. Redis is optional: lose it and redirects get slower, not wrong. |
| **Analytics** | Clicks, estimated unique visitors, bots, device, browser, OS, language, referrer host, and country with an optional GeoIP database. Daily rollups, server-rendered charts, a bounded recent-activity feed, retention enforced by dropping whole months. |
| **Auth** | Email/password with argon2id, server-side sessions in `__Host-` cookies, per-account lockout and per-address rate limiting, real RBAC with four built-in roles and a working permission evaluator. |
| **Abuse limits** | Per-address limits on credential endpoints, the API, and 404 probing. The last charges misses only, so a working link is never throttled by anyone's scanning. |
| **API keys** | `lk_live_…` bearer tokens, scoped to permissions you hold, intersected with your current role on every request. Revocable, with usage timestamps. |
| **Dashboard** | Server-rendered HTML with htmx. Works without JavaScript; no build step at runtime. |
| **API** | REST with RFC 9457 problem responses, an OpenAPI 3 document, and Swagger UI at `/docs`. |
| **Operations** | `/healthz`, `/readyz`, Prometheus metrics on a separate unpublished port, structured JSON logs, graceful shutdown that flushes buffered clicks. |
| **CLI** | `lctl` for config validation, migrations, partitions and API keys — including the first key on a headless box. |

## Documentation

| Guide | For |
| --- | --- |
| [docs/deployment.md](docs/deployment.md) | Running it for real: TLS, reverse proxy, secrets, backups, upgrades, GeoIP |
| [docs/configuration.md](docs/configuration.md) | Every environment variable, its default, and what it actually affects |
| [docs/usage.md](docs/usage.md) | Using the dashboard and the API, with worked `curl` examples |
| [docs/cli.md](docs/cli.md) | `lctl` command reference |
| [docs/operations.md](docs/operations.md) | Runbook: what to watch, what to alert on, what to do when it breaks |
| [docs/claude/development.md](docs/claude/development.md) | Working on LinkCtrl itself |
| [Plan.md](Plan.md) | Scope contract: what is in Phase 1, what is deferred, what is measured |
| [docs/claude/decisions.md](docs/claude/decisions.md) | Why it is built this way. Every non-obvious choice, with its trade-off |

## Privacy

This is a design constraint, not a settings page.

- `click_events` has **no address column of any kind**. There is nothing to
  leak, subpoena or accidentally log.
- A visitor is `HMAC(daily salt, ip ‖ 0 ‖ user-agent ‖ 0 ‖ workspace)`,
  truncated to 16 bytes. The workspace is inside the message, so two workspaces
  on one instance cannot join their analytics to follow one person.
- Salts are **deleted after two days**. That deletion is the de-identification
  step, not housekeeping.
- Referrers are reduced to a host at ingest; query strings — which routinely
  carry session tokens and search terms — are discarded, not stored and cleaned
  up later.
- Session and audit records keep an address *prefix* only: /24 for IPv4, /48 for
  IPv6.

The consequence worth stating plainly: the largest table in the system holds no
personal data, which puts it outside the scope of subject-access and erasure
requests. Unique-visitor counts are therefore estimates at daily resolution, and
every API response that includes them says so.

## Not built yet

Phase 1 is not finished:

- **The redirect latency target is unverified.** <20ms p99 for cached redirects
  is a design target with a histogram ready to measure it, not a result. The load
  test is the next milestone.
- **Single-instance cache invalidation.** Editing a link clears the cache on the
  replica that served the edit; others wait out the TTL. Run one app instance
  until Phase 2 adds pub/sub.
- **Region and city are never stored.** With a GeoIP database configured, a
  country is resolved at ingest; region and city are available from the same file
  and deliberately left null. Nothing shows them, and city plus a timestamp is
  close to a location history.
- **No audit log behaviour, no folders, no custom domains, no QR codes, no
  password/one-time links.** The tables exist; the features are Phase 2.

Rate limiting, geographic analytics and retention enforcement used to be listed
here as settings that accepted a value and did nothing. They are implemented now,
and three variables that were never going to be implemented — an ingest worker
count, a salt rotation period, a bot-filter switch — were removed instead, because
in each case the fixed behaviour was the design. Startup warns if you still have
one set.

The full list, with consequences, is in
[Plan.md](Plan.md#phase-1-scope-not-yet-built) and
[Known limitations](Plan.md#known-limitations).

## Contributing

[docs/claude/development.md](docs/claude/development.md) covers the toolchain, the test
strategy and the platform quirks worth knowing (particularly if you develop on
Windows). In short:

```sh
make assets            # build the stylesheet, verify vendored JS
make test              # unit tests, race detector on
make test-integration  # needs `docker compose up -d`
make lint
```

New behaviour is expected to come with a test that fails without it, and any
non-obvious decision with an entry in `docs/claude/decisions.md`.

## License

MIT — see [LICENSE](LICENSE).
