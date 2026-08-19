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

Five variables accept a `_FILE` suffix pointing at a file, for mounted secrets:
`API_KEY_PEPPER` and `DATABASE_URL` here, `MFA_SECRET_KEY` below, and
`SMTP_PASSWORD` and `FEED_AUTH_TOKEN` in their own sections. Setting both forms
of the same secret is an error, not a precedence rule. (This said two until
0.2.0, while the loader accepted four and the two missing ones were documented
correctly in their own rows — a summary that had fallen behind the table it
summarises, F45.)

## Two-factor authentication

One variable, and it is optional. Leaving it unset is a supported instance: the
second factor is simply not offered, which is what every instance was before
0.3.0.

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_MFA_SECRET_KEY` | *(unset)* | ≥32 bytes. `openssl rand -base64 48`. Encrypts each account's TOTP secret at rest. Unset means nobody can enrol and the account page says so. |

**Losing it locks every enrolled account out of the second factor, and no
further.** The secret cannot be decrypted, so no authenticator code can be
verified and an enrolled account stops at the code prompt. What still works is a
**recovery code** — those are SHA-256 hashes and this key is not involved in
them. The route back is: sign in with a recovery code, turn the second factor off
with another one, then enrol again. An account that has neither is the operator's
to repair, which is the same last resort a forgotten password has.

**It is deliberately not `API_KEY_PEPPER`, and must not be set to the same
value.** The pepper is bound to retained API-key rows and rotating it invalidates
every issued key. Sharing one value would mean rotating an API-key secret also
locked every account out of its authenticator — two credential lifecycles with
nothing to do with each other, given one lifetime.

Like the pepper, it is not rotatable in place: there is no re-encrypting sweep.
Changing it has exactly the effect of losing it.

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
  on the *Account* page or with `PATCH /api/v1/domain`; **since 0.2.0 it needs
  the `domains.write.instance` permission, which only the instance principal
  holds** — this hostname is not any tenant's, it is the one every workspace's
  links are served on until it registers its own, and until 0.2.0 every
  organization's owner and admin could repoint it. The
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
| `LINKCTRL_REDIS_READ_TIMEOUT` | `50ms` | Deliberately short. A slow cache must not become slow redirects; the resolver falls through to Postgres. It is also a redirect's whole patience per Redis call, and a stalled cache makes a redirect pay it rather than wait indefinitely. |
| `LINKCTRL_REDIS_INVALIDATE_BUDGET` | `250ms` | The most an edit will wait for the cache to be told a link changed — the total across all three attempts, not each. Raising `REDIS_READ_TIMEOUT` therefore no longer multiplies into edit latency. Spending the budget is not an error: the edit is already committed, the failure is logged, and the stale entry expires with `REDIRECT_TTL`. |
| `LINKCTRL_REDIS_SUBSCRIBER_READ_TIMEOUT` | `30s` | How long the cache-invalidation subscriber accepts silence before it makes Redis prove the subscription still delivers. Not `REDIS_READ_TIMEOUT`, and it must not be set anywhere near it: on the hot path a timeout means the cache failed, while here it usually means nobody has edited a link. It bounds the *staleness* a stalled Redis can cause — at most twice this value passes before the replica drops what it can no longer vouch for — and it costs one `PING` round trip per interval on an instance quiet enough to reach it. |
| `LINKCTRL_REDIS_POOL_SIZE` | `50` | |

Redis being unavailable at startup is a warning, not a failure.

A Redis that *accepts* a connection and then never answers is the failure worth
knowing about, because it is the one that can hold a caller. `REDIS_READ_TIMEOUT`
is the ceiling on every Redis command the service issues, whatever deadline the
code around it asks for. Measured, against a proxy that accepts and stays
silent: an edit costs at most `REDIS_INVALIDATE_BUDGET` and then commits anyway,
and a redirect costs one read timeout per Redis call and then falls through to
Postgres. A cold one measured 108ms before 0.2.0, because it spent the timeout
twice — once on the lookup that never answered and once on the write that would
have repopulated the cache. It now spends it once: a lookup that fails for any
reason other than the key being absent suppresses that write for one resolve,
since a server that will not answer a read will not usefully answer the write
either.

**A redirect answered from memory usually costs nothing, and there is one
exception.** A link carrying a *returning visitor* routing condition asks Redis
whether this visitor has been seen before, on every redirect, whatever tier
answered it — the answer decides which destination is served, so it cannot be
deferred or skipped. With Redis stalled that is one read timeout on a request
that would otherwise have touched nothing. It applies only to links with that
condition; every other link is answered from memory without a Redis call.

All these figures hold for a connection that was established and then went quiet
mid-command. None depends on Redis answering.

A caller asking for *less* than `REDIS_READ_TIMEOUT` gets what it asked for.
That has been true only since 0.2.0: before it, go-redis was not configured to
apply a caller's deadline to the socket at all, so the read timeout was the only
bound and a shorter deadline meant nothing. The knob's meaning is unchanged —
it is still the most any one Redis command may cost — and no default moved.

The invalidation subscriber is the exception, and it needs its own variable
because of it. `REDIS_READ_TIMEOUT` does not reach the pub/sub receive path at
all: go-redis reads it with a zero timeout and no context deadline, which is an
infinite wait, so a stalled Redis left the subscriber blocked with no error, no
log line and no reconnection. It now reads with
`REDIS_SUBSCRIBER_READ_TIMEOUT`, and because a quiet channel and a dead one look
identical from there, an expired read is not a failure by itself — the
subscriber pings and waits for the *reply*, which is the part a stalled
connection cannot produce. Unanswered, it logs, drops both in-process caches
rather than serving them as current, and reconnects.

## Redirects

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_REDIRECT_TTL` | `24h` | How long a resolved link stays cached. Clamped down automatically for links that expire sooner. Also the staleness window if you run more than one replica — see [deployment.md](deployment.md#scaling-honestly). |
| `LINKCTRL_REDIRECT_NEGATIVE_TTL` | `60s` | Caching of unknown aliases, which is most of what a public shortener is asked for. Capped at 5m by validation, and cleared when a matching link is created, so a probed-then-created alias is never stuck as a 404. |
| `LINKCTRL_REDIRECT_DEFAULT_STATUS` | `302` | `302`, `303` or `307`. `301`/`308` are refused: a permanent redirect cached in browsers and intermediaries cannot be recalled, and links here are editable by design. **It does not apply to a password submission**, which is answered `303` whatever this is set to — `307` preserves the method, so the browser would re-send the password body to the link's destination. |
| `LINKCTRL_REDIRECT_LOG_SAMPLE` | `0` | Log one in N successful redirects; `0` disables. At 2,000 rps, logging every redirect produces more bytes than the redirects. |
| `LINKCTRL_REDIRECT_TIMEOUT` | `250ms` | Bounds every database call the redirect path makes: the Postgres fallback for an uncached alias, and each of the gate queries — password verification, the signing-key read, the click-budget write and a sequential split's rotation. A query still running after this has already missed the uncached target and is holding a connection while requests queue behind it. `LINKCTRL_HTTP_REQUEST_TIMEOUT` does **not** cover this tree, deliberately: it buffers the response, which would break the `Location` header and swallow the password page. |
| `LINKCTRL_REDIRECT_404_RATE_LIMIT` | `60` | Misses per address per minute. See [Rate limits](#rate-limits). `0` disables. |
| `LINKCTRL_LINK_PASSWORD_RATE_LIMIT` | `20` | Guesses at a password-protected link per minute, charged **per address and per alias**. See [Rate limits](#rate-limits). `0` disables it, which on a public instance leaves a link password worth only as much as the wordlist somebody is willing to run. |

## Custom domains

Verification of a workspace's own hostname, and what happens when it stops
verifying. The operational half — the Caddy `ask` block, what to do when a
customer's links stop working — is the custom-domain runbook in
[deployment.md](deployment.md#custom-domains).

**These two numbers decide when somebody's links go dark.** They are here rather
than in the source because the trade is an operator's to make: the window is how
long this instance keeps serving a hostname whose DNS its owner may no longer
control, and the cadence is how much evidence there is before the window expires.

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_DOMAIN_VERIFY_INTERVAL` | `1h` | How often every registered hostname's DNS challenge is re-checked. One replica at a time. It is also how often **each** replica re-reads the verified-hostname set it serves from — that half takes no leadership, because the set is in-process and one replica reloading does nothing for the others. `0` switches both off, leaving verification on-demand only, a hostname that stops verifying served indefinitely, and a replica that missed an invalidation serving a stale set until it restarts. |
| `LINKCTRL_DOMAIN_VERIFY_GRACE` | `24h` | How long a **serving** hostname keeps serving after its first failed check. The owning workspace is notified at that first failure, not only at the end. A successful check at any point clears the clock. When the window elapses the hostname stops being served on every replica, which is a stop rather than another warning. Zero or unset takes the default; there is no "no window". |
| `LINKCTRL_DOMAIN_VERIFY_DNS_TIMEOUT` | `5s` | Bounds one TXT lookup, so a nameserver that accepts a query and never answers costs this rather than the whole pass. |
| `LINKCTRL_DOMAIN_VERIFY_BATCH` | `500` | How many hostnames one pass checks in total, least recently checked first **within each of two classes**: the hostnames this instance is serving are drawn first and take what they need, and registrations that are not yet served take what is left. A bound rather than a limit anybody is expected to reach, and raising it is not a way to make a pass reach more of the second class — a longer queue of lookups that can each block for the timeout above is the same starvation with more rows in it. |

At the defaults a hostname must fail **twenty-four consecutive hourly checks**
before its links stop resolving. Shortening the window narrows how long a lost
hostname keeps being served and widens the chance that a resolver outage takes a
working one down; lengthening it does the reverse. Nothing else in this file has
that shape, which is why the reasoning is stated rather than left to the default.

This is the only outbound network traffic LinkCtrl sends on its own behalf other
than an opt-in [reputation feed](#reputation-feeds) and [mail](#mail): TXT queries
for a fixed label under hostnames somebody registered here. No destination is ever
looked up.

## Aliases and destinations

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_ALIAS_LENGTH` | `7` | Generated alias length, 4–12. |
| `LINKCTRL_ALIAS_MIN_USER_LENGTH` | `3` | Floor for custom aliases. |
| `LINKCTRL_ALIAS_RESERVED_EXTRA` | *(empty)* | Comma-separated words to refuse, merged with the built-in list. Additions extend it and cannot shrink it: every route the server registers is on the built-in list, and an alias shadowing one would take a working page out of service. |
| `LINKCTRL_ALIAS_PROFANITY_FILTER` | `true` | `false` switches off the built-in profanity list. Reserved words are unaffected — one is taste, the other is routing correctness. |
| `LINKCTRL_DESTINATION_SCHEMES` | `http,https` | Allowlist. It may **narrow** the built-in pair and may never widen it: naming anything other than `http` or `https` is refused at startup. `javascript:`, `data:` and whatever the next browser ships are excluded by default rather than by blocklist, and there is no setting that adds one back. Set it to `https` alone if you want TLS-only destinations. |
| `LINKCTRL_DESTINATION_MAX_LENGTH` | `2048` | |
| `LINKCTRL_DESTINATION_BLOCKLIST` | *(empty)* | Comma-separated hosts to refuse, matched on a label boundary — `evil.example` also refuses `login.evil.example` and does not refuse `notevil.example`. Since 0.2.0 these seed the runtime blocklist table at every boot rather than being held in memory: entries you remove from this variable are retired on the next restart, and rows from any other source are never touched by it. Each entry is folded exactly the way a destination is — lowercased, a trailing dot dropped, and a non-ASCII name converted to its `xn--` form — so `münchen.example` here refuses a link typed the same way. An entry that cannot be folded into a name at all stops the instance from starting, rather than becoming a row that refuses nothing and says nothing about it. **Write bare hostnames, not URLs or wildcards.** `https://evil.example` and `*.evil.example` are the shape of a valid host as far as the fold is concerned, so they are accepted, stored, and shown in your blocklist — and they match nothing, because matching is done against the labels of a real hostname and neither of those can ever be one. Subdomains are already covered by the bare name, so a wildcard buys nothing even when it works. IP literals are fine and do what you expect. |

The `blocked_destinations` table this variable feeds also arrives with about
twenty known URL-shortener hosts, seeded by the migration that creates it and
marked `source = 'shortener'`. They are there because a short link pointing at
another short link hides the real destination from everyone in the chain; the
refusal is low confidence and appealable. There is no variable for them and none
is planned — the row *is* the setting:

```sql
DELETE FROM blocked_destinations WHERE host = 'bit.ly';
```

That is permanent. Nothing re-asserts those rows, so a restart does not bring
one back, and no rebuild is involved either way. Adding a shortener the list
does not know about is the same statement with `INSERT`.

Since 0.2.0 the same deletion is a button. A person refused by any
`low_confidence.*` rule can ask for a review, and whoever holds
`destinations.decide` decides it at `/disputes`; allowing removes the row. That
permission is held instance-wide by the account that claimed the instance and by
anybody it appointed, never by an organization role and never by an API key. That
path deliberately will not touch a `source = 'env'` row — this variable owns
those, and boot would put one back — so an entry you listed here is retired by
editing the variable and restarting, exactly as before.

`LINKCTRL_DESTINATION_BLOCK_PRIVATE_IPS` **was removed in 0.2.0.** Private,
loopback, link-local, carrier-NAT and cloud-metadata addresses are now refused
unconditionally, because that refusal protects the visitor whose browser would do
the fetching and they are not the party who was turning it off. A stale line in
your `.env` is reported at startup and otherwise ignored. To point links at an
intranet, give the host a name that resolves there — hostnames are not checked
against these ranges, and that limitation is
[documented](SECURITY.md), not a loophole to rely on.

## Reputation feeds

**This is the only setting in LinkCtrl that sends your users' data to somebody
else. It is off by default. Read this section before switching it on.**

Every other check this product makes on a destination is local: a host list
compiled into the binary, the `blocked_destinations` table above, and rules that
inspect the URL's own text. A reputation feed cannot work that way. Answering
*is this destination malicious* means **sending the destination to a third
party's server**, and that is a deliberate exception to this project's promise
that no destination leaves the box uninvited — which is why turning it on costs
you a named feed rather than a boolean.

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_FEED_URL` | *(empty)* | The endpoint, and the switch. Empty means no feed, no client, and no code path that sends a destination anywhere. **`https` only**: destinations are already going to somebody else's server and must not travel in clear as well. **No credential in the URL**: a username or password (`https://key:secret@feed.example/`) is refused at boot — Go would send it as Basic auth, so it would work, and `/feeds` shows every signed-in user this endpoint. Put it in `FEED_AUTH_TOKEN`. The scheme, host and path are what `/feeds` prints; a query string and a fragment are not, so a key in **the path** is the one place this cannot protect you. |
| `LINKCTRL_FEED_NAME` | *(empty)* | Who the destinations go to, in words — "Google Safe Browsing", "urlscan.io". **Required once `FEED_URL` is set.** It is what `/feeds` prints, and an instance may not send destinations somewhere it cannot name. |
| `LINKCTRL_FEED_METHOD` | `POST` | `POST` or `GET`. `POST` keeps the destination out of the feed's access-log query string. |
| `LINKCTRL_FEED_PARAM` | `url` | The field carrying the destination: a JSON key on `POST`, a query parameter on `GET`. |
| `LINKCTRL_FEED_VERDICT_FIELD` | `blocked` | Dotted path into the JSON response holding the verdict — `data.malicious` reads `{"data":{"malicious":true}}`. `true`, a non-zero number, or one of `yes`/`malicious`/`blocked`/`phishing`/`malware` means refuse; `false`, `no`, `clean`, `ok`, `harmless` and empty mean accept. Anything else is counted as an error and fails open. |
| `LINKCTRL_FEED_AUTH_HEADER` | `Authorization` | Sent only when a token is set. |
| `LINKCTRL_FEED_AUTH_TOKEN` | *(empty)* | The credential, verbatim — include `Bearer ` yourself if the feed wants it. `LINKCTRL_FEED_AUTH_TOKEN_FILE` works too, for mounted secrets. Never printed: it is redacted in logs, unset from the process environment once parsed, and it is a header rather than part of the URL, so nothing on `/feeds` or in a transport error can echo it. This is where a feed credential goes; `FEED_URL` refuses one. |
| `LINKCTRL_FEED_TIMEOUT` | `2s` | Bounds one check end to end. Spent inside a form submission somebody is waiting on, so keep it small; validation refuses anything above `HTTP_REQUEST_TIMEOUT`. |

One generic HTTP adapter, not a list of integrations. Point it at any endpoint
that takes a URL and answers JSON. Which feeds get first-class support is a
product decision nobody has made, and shipping a named integration would be
making it.

### What is sent, and when

The destination URL, and nothing else. No account, no address, no workspace name,
no name for your instance — the request carries the URL and your own credential
for that feed.

It is sent when a destination is **checked**, which is four moments: creating a
link, editing one, setting the link domain's root redirect, and asking for a
refusal to be reviewed. Nothing is sent when a visitor follows a link, and
existing links are never re-checked in the background.

### What the answer can and cannot do

- **Asked last.** The feed only ever sees a destination every built-in tier has
  already accepted. A private address, a host on the compiled list, a listed
  host, a homograph — none of them ever reaches the feed, and none of them
  changes answer with a feed on, off or failing. That is a bound on the feed and
  not on the instance: the refusal is itself a `destination.blocked` event, and a
  workspace that has subscribed a webhook to it receives the refused destination,
  defanged.
- **Low confidence.** A refusal it produces is `low_confidence.feed_reputation`:
  disputable like any other, and the instance owner can overrule it from
  `/disputes`. Overruling also stops that host being sent again, so an override
  ends both the refusal and the egress. It is scoped to the exact host —
  allowing `evil.example` says nothing about `login.evil.example`.
- **Fails open.** A timeout, a `500`, or a response this adapter cannot read
  accepts the destination and increments
  `linkctrl_destination_feed_checks_total{result="error"}`. A third party's
  outage must not decide that your instance may not create links — but it also
  means a feed that quietly stopped working is invisible in the product's
  behaviour, so alert on that counter if you are relying on one.

### The disclosure

Once a feed is configured, the instance says so at **`/feeds`** — to every
signed-in user, gated on no permission, because what it describes is what happens
to their own destinations. The same disclosure is on `GET /api/v1/feeds`. It
names the feed, states what is sent and when, and says that only the operator can
change it. With no feed configured the page still answers, and says nothing
leaves.

The page is **read-only and accepts no `POST`**, and that is asserted by test
rather than left as a convention (decision D40). There is no setting there and
there will not be one: this file is the only switch.

## Webhooks

Outbound delivery of a workspace's events. **There is no switch here**: a webhook
is registered by a workspace rather than enabled by you, and these two numbers
apply the moment somebody registers one. An instance where nobody does never opens
an outbound connection for this feature.

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_WEBHOOK_TIMEOUT` | `10s` | Bounds one delivery attempt end to end — connect, write, read. A batch of twenty is dialled together on a thirty-second tick, so a drain costs one of these rather than twenty; this is also how long a drain occupies the scheduler, which is why validation refuses anything above `1m`. |
| `LINKCTRL_WEBHOOK_RETENTION_DAYS` | `30` | How long a delivered or abandoned delivery row is kept. **Must be at least 1.** Zero means "keep forever" elsewhere in this file — `AUDIT_RETENTION_DAYS` — and is refused here, because this table grows by one row per link write per enabled webhook and nobody would choose that on purpose. |

**The attempt count is not configurable.** Seven attempts spanning 61 minutes,
with a doubling backoff capped at thirty minutes. It is left as a constant
deliberately: unlike the two above, changing it changes what somebody *else's*
receiver experiences — how late a delivery may arrive — which is a contract with a
party who does not read this instance's environment.

The scheme a receiver verifies with, the event vocabulary, and the delivery log
are all in [usage.md](usage.md#webhooks). What the address checks do and do not
cover — in particular on a deployment with an egress proxy — is in
[SECURITY.md](SECURITY.md).

## Update check

Once somebody administering this instance has been asked and said yes, it asks
GitHub once a day whether a newer LinkCtrl has been released and, if there is
one, puts a notification in the instance principal's inbox. It is **the only
scheduled work in this product that opens a socket to a host outside your
deployment**, which is why it is a job family of its own rather than a step in
the hourly maintenance pass: what this instance connects to, and when, is one
thing to read rather than several.

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_UPDATE_CHECK` | `true` | Whether this instance may perform the check at all. `false` disables it outright: no client is built, no work is scheduled, and the prompts below are replaced by a line saying so. `true` is permission, not instruction — somebody here still has to be asked and say yes. |

### What the request carries, enumerated

Not summarized, because a summary is what you would have to trust:

- **This server's source address.** A property of opening a socket, not
  something the product chooses to send.
- **The version it is running**, in the `User-Agent`, as `LinkCtrl/0.3.0`. No
  platform, no Go version, no hostname.

And nothing else. No instance identifier, no deployment size, no link counts, no
account, no configuration, no request body, no query string, no cookie and no
credential. The response is read for a version number and discarded — nothing
else from it is stored anywhere.

The destination is a constant in the source, not a setting. There is no variable
that points the check somewhere else, deliberately: the value of a disclosed,
auditable daily request is lost the moment it can be redirected.

A test in `internal/update` compares the outgoing request against an exact
expected form — method, URL, body and every header — so a field added to it
fails the build rather than shipping quietly.

### Two switches, and either one is enough

The variable above is the **deployment's** answer, and it only ever says no. The
other is the **operator's**, and it has three states rather than two: yes, no,
and *nobody has been asked yet*. The check runs only when the variable allows it
**and** somebody has said yes, so:

- `LINKCTRL_UPDATE_CHECK=false` wins over any answer given in a browser. That is
  deliberate — a deployment with no egress should not depend on somebody in the
  dashboard knowing why.
- Answering *no* stops the check on an instance whose configuration never
  mentioned it.
- An instance where the question is still open **does not check.** Unanswered is
  off.

### Where the question is asked

| The instance | Asked | Until then |
| --- | --- | --- |
| Fresh | On the setup page that claims it, with the box ticked | It has no accounts, so nothing is running |
| **Upgraded** into 0.3.0 | On the dashboard, at the first sign-in by an account holding `instance.admin` | The check is **off** |
| Claimed through `POST /api/v1/auth/setup` without the `update_check` field | As an upgraded one: on the dashboard | The check is **off** |

It is asked once. There is no settings page and no way to change the answer from
a browser afterwards — the control that remains is `LINKCTRL_UPDATE_CHECK`,
which is where a deployment's decisions belong and where somebody with shell
access can always find it.

**The bound this puts on the feature is real and is stated rather than left to
be discovered: an instance nobody signs into stays quiet forever.** No
administrator, no question, no answer, no check — on the very instance a release
notification would be most use to. That is the price of not answering on an
operator's behalf, and if it applies to you, watch the repository instead. See
[deployment.md](deployment.md#air-gapped-and-egress-restricted-deployments).

### What failure looks like

A check that cannot complete is logged at debug and does nothing else. It never
fails a startup, never delays a shutdown, never reaches a user, and is never
retried — the day's attempt is recorded before the request is made, so a
failure costs one attempt rather than one per tick.

**No notification is not evidence of being up to date.** GitHub's
unauthenticated API is rate-limited per source address, so a deployment behind a
large NAT can be throttled into checking successfully rarely or never, and the
symptom is silence. If being current matters, watch the releases rather than
this.

Air-gapped and egress-restricted deployments want `false`; see
[deployment.md](deployment.md#air-gapped-and-egress-restricted-deployments).

## Authentication

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_SIGNUP_MODE` | `closed` | `closed`, `invite` or `open`, and **this variable is the only way to set it** — there is no runtime toggle, so changing how the instance admits accounts is an edit here and a restart (decision D38). **`closed` admits no new account by any path, invitations included** — an invitation can then only add somebody who already has an account. `invite` and `open` both let an invitation create the account it names; `open` additionally opens the `/signup` form and `POST /api/v1/auth/register` to anybody, and a registration there creates a new isolated organization and workspace rather than adding a member to yours. **`open` needs a mailer**: registration confirms the address by email before the account exists, so with no `LINKCTRL_SMTP_HOST` the effective mode is `invite` whatever this says, `/signup` answers 403, and the server says so in one line at boot. The first-run setup endpoint works regardless, then closes permanently. |
| `LINKCTRL_INVITE_TTL` | `168h` | How long an invitation stays redeemable. The clock starts when it is created, not when the mail goes out — delivery is asynchronous through the outbox — so a slow relay spends this window. It must be positive: expiry cannot be switched off, and revoking is how an invitation is ended early. |
| `LINKCTRL_SESSION_ABSOLUTE_TTL` | `720h` | Hard deadline from creation. |
| `LINKCTRL_SESSION_IDLE_TTL` | `168h` | Maximum gap between requests. Must not exceed the absolute TTL. Enforced at read time, so a change takes effect immediately. |
| `LINKCTRL_LOGIN_LOCKOUT_THRESHOLD` | `5` | Failed attempts before a 15-minute per-account lockout. Counted across **both** places an account's password is checked — signing in, and redeeming an invitation into an account that already exists — because one lockout an operator configured should not leave the second door unbolted. `0` disables it. |
| `LINKCTRL_ARGON2_MEMORY_KIB` | `65536` | 64 MiB. Floor of 19456 enforced (RFC 9106); lowering this is the easiest way to silently weaken password storage. |
| `LINKCTRL_ARGON2_ITERATIONS` | `3` | Minimum 2. |
| `LINKCTRL_ARGON2_PARALLELISM` | `2` | |

Parameters are recorded in each hash, so raising them does not lock anyone out —
existing hashes verify against their own recorded cost and are upgraded on the
next successful login.

## Rate limits

Three limits, all keyed on the client address, and `0` disables any of them. A
negative value is refused rather than read as "unlimited".

**Two of the three are shared across replicas and one is not**, which is the
thing to know before running more than one. The credential and API limits go
through Redis, so the configured number is the number however many replicas are
running; the 404-probe limit is deliberately in memory only, because it guards
the redirect path and a network round trip there would cost more than the limit
saves. See *Per instance, unless the limit is shared* below.

| Variable | Default | Applies to |
| --- | --- | --- |
| `LINKCTRL_LOGIN_RATE_PER_MIN` | `10` | The endpoints that verify a credential: login, register, first-run setup, password change — both the API and the dashboard forms, sharing one budget so alternating between them gains nothing. |
| `LINKCTRL_API_RATE_PER_MIN` | `600` | Everything under `/api/v1`. Dashboard pages, static assets and the health endpoints are not counted. |
| `LINKCTRL_REDIRECT_404_RATE_LIMIT` | `60` | Misses on the redirect path, and misses only. |
| `LINKCTRL_LINK_PASSWORD_RATE_LIMIT` | `20` | Submissions to a password-protected link, charged per address **and** per alias. |
| `LINKCTRL_UPLOAD_RATE_PER_MIN` | `30` | Uploading a QR code's logo — the only thing this product accepts a file for, at three addresses: the API's two logo `PUT`s and the dashboard's upload form on the QR tab. **On top of the API limit, not instead of it** — an API upload spends a token from both buckets — and **one bucket across all three**, so alternating between the dashboard and the API does not double the budget. |

Five properties are worth knowing before you tune them.

**Per address means per address as the server sees it.** Behind a reverse proxy
with `TRUSTED_PROXIES` empty, every request appears to come from the proxy, so all
your traffic shares one bucket and the limit applies to the whole world at once.
Setting `TRUSTED_PROXIES` is a correctness requirement once a limit is on, not
just an analytics nicety.

**IPv6 is keyed by /64, not by address.** A single host is routinely handed a
whole /64, so per-address keying would let one machine present unlimited
identities. The /64 is a floor and not a customer: a site delegated a /56 or a
/48 still gets one bucket per /64 inside it, so the limit applies that many times
over to one subscriber. That is deliberate — keying any coarser would let one
abusive host throttle its neighbours.

**Per instance, unless the limit is shared.** The limiter is in memory, so the
404-probe limit costs no Redis round trip on the redirect path and depends on
nothing optional; with N replicas its effective limit is N times the number. The
credential, API and link-password limits are backed by Redis instead — see the
degradation note below.

**The link-password limit has two keys, not one.** Every submission spends a
token from the address's bucket *and* from the alias's, and either being empty
refuses the request. The second key is the one that matters: guesses driven
through many visitors' browsers spread across as many addresses as there are
visitors, and a per-address bucket would never see them. It is charged only on a
**submitted** password — visiting a password link and being shown the challenge
costs nothing — and there is no lockout to go with it, because there is no
account to lock.

**The upload limit exists because an upload is not an API call.** Everything
else under `/api/v1` is a JSON body capped at 256 KiB and parsed by the standard
library; an upload is up to a megabyte handed to an image decoder, so what a
request costs is decided by its content rather than by its shape.
`API_RATE_PER_MIN`'s 600 was chosen about the first kind, and 600 megabyte
uploads a minute is a bandwidth and decoder budget nobody set by tuning a number
about JSON. It is shared through Redis like the credential limit, so it means one
thing across replicas rather than N.

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

**The credential, API and link-password limits are shared across replicas**
through Redis, so the configured rate is the whole instance's rate rather than
each replica's. The 404-probe limiter is not, and never will be: a Redis round
trip on the redirect path would put an optional dependency inside the 20ms
budget. The link-password limit is on that path too and is shared anyway,
because it is charged only when somebody submits a password — never on a visit —
and because it is the only thing standing between a link password and a
wordlist, so an attacker must not multiply their budget by the replica count.

Sharing degrades rather than fails. On any Redis error — unreachable, stalled,
or slower than `REDIS_READ_TIMEOUT` — the limiter falls back to the per-replica
bucket it always had, so the limit is still enforced, just once per replica. It
never starts refusing requests because Redis is unwell; a limiter is abuse
mitigation, not an authorization boundary, and converting a cache outage into a
sign-in outage would be the wrong trade. The fallback is logged once when it
starts, not per request.

That degradation is why the link-password limit is documented as **best-effort
rather than a guarantee**: while Redis is unavailable, N replicas allow N times
the configured number of guesses, and a restart refills the local buckets.

## Analytics

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_INGEST_QUEUE_SIZE` | `16384` | Bounded buffer. When full, clicks are **dropped** rather than delaying a redirect. Drops are counted and alertable. |
| `LINKCTRL_INGEST_BATCH_SIZE` | `500` | Must not exceed the queue size. |
| `LINKCTRL_INGEST_FLUSH_INTERVAL` | `250ms` | |
| `LINKCTRL_ANALYTICS_RETENTION_DAYS` | `395` | 13 months. Enforced hourly by dropping monthly partitions of `click_events` and `visitors`, and only once a partition's newest possible row is outside the window — so raw data survives up to a month longer than the number says. Rollups are separate tables and are never dropped, so charts outlive the events. `audit_logs` has its own window and is not covered by this one. `0` keeps everything. |
| `LINKCTRL_GEOIP_MMDB_PATH` | *(empty)* | A MaxMind DB file. Resolves a country at ingest, from the address, before it is discarded — nothing is stored to resolve later. Also resolves a region and a city on the redirect path, transiently, for links whose routing rules ask; those two need a **City** database. Empty stops both: no new click resolves a country, and no geographic routing condition can match. It does **not** hide country history already resolved — since 0.3.0 the dashboard draws the map and the ranked list from the rows, so the *geographic data is unavailable* sentence appears only when nothing resolved **for that link in the window on screen** and nothing could resolve. The rule form still asks about this variable, because whether a routing rule can ever match is genuinely a question about the database. See [deployment.md](deployment.md#optional-geographic-analytics). |

**Country is the only geographic field stored, and that has not changed.**
Region and city are in the same database and in the schema, and are deliberately
left null: nothing in the product displays them, and city plus timestamp is
close to a location history. `click_events.region` and `click_events.city`
staying null is asserted by test rather than promised here.

What changed with routing rules is that region and city are now *resolvable* as
well as unstored. A rule may match on either, in which case the value is looked
up for the length of one redirect and discarded. The distinction is the whole of
the position: a value that exists for the microseconds it takes to decide where
to send somebody is not the same thing as a value in a row, because a row is
what gets reported on, exported and joined against.

Two consequences for an operator:

- **Which database file matters now.** GeoLite2-Country carries no subdivisions
  and no city names, so on one of those a region or city condition simply never
  matches — the same answer an address in nobody's range gets. Analytics is
  unaffected either way. GeoLite2-City works for all three.
- **A geographic condition on an instance with no database never matches.** It is
  not an error and nothing refuses the rule; the visitor falls through to the
  link's own destination. The rule form on the link's page says so where somebody
  is writing one.

### Returning-visitor routing needs Redis

A routing rule can ask whether a visitor has been seen on that link **earlier
today**, where the day ends at midnight UTC. A visitor from yesterday is new
again, and that is the whole of the semantics rather than an approximation of a
longer-lived one — a durable answer would need a cookie or a per-person
identifier kept across days, and this product keeps neither.

It is computed from the same daily-salted visitor hash the analytics use, so it
carries no address and becomes unlinkable when the salt is purged. The set lives
in Redis under `lc:rv:`, is written by the click pipeline rather than on the
redirect path, is maintained only for links that actually carry such a rule —
and only by visits that were actually served: a refused request is counted as a
click but never marks the visitor as seen. It expires with the day it
describes.

**With `LINKCTRL_REDIS_URL` unset or Redis unreachable, every visitor reads as
new**: a rule looking for a returning visitor never fires, and one looking for a
new visitor fires for everybody. That is the same "cache is optional" degradation
the rest of the redirect path takes, and it is stated in the rule form too.

## Audit log

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_AUDIT_RETENTION_DAYS` | `0` | **Keep forever.** Enforced by the same hourly pass and the same whole-month rule as analytics retention, under a separate window: `audit_logs` partitions are dropped only once their newest possible row is outside *this* number. `0` deletes nothing, ever. |

The default is the opposite of the analytics one, and that is deliberate. Both
settings are a data-loss policy, and they fail in opposite directions: a finite
default means an upgrade silently begins deleting history an operator assumed
permanent, while keeping everything means growth nobody bounded. The first
failure is invisible and irreversible; the second is visible and recoverable.

Visible is a claim that has to be paid for, so it is:
`linkctrl_audit_log_bytes` reports the on-disk size of every `audit_logs`
partition, refreshed hourly on every replica. The alert recipe is in
[operations.md](operations.md#audit-log-growth).

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_AUDIT_SIZE_WARN_BYTES` | `5368709120` | 5 GiB. Once the audit partitions pass this, **the instance principal** gets an in-app notification, at most one a week — and the same warning by email if a [mailer](#mail) is configured. **On by default**, unlike the retention window. `0` disables it. |

The asymmetry is deliberate. Retention defaults to inaction because acting
unasked destroys data; the warning defaults to acting because inaction is what
leaves an operator uninformed. Keep-forever is a safe default only if the
instance nobody configured is the one that gets warned.

Reading the log needs the `audit.read` permission, held by owners and admins.
It cannot be granted to an API key — see [SECURITY.md](SECURITY.md).

## Mail

**Off by default.** Leave `SMTP_HOST` empty and the instance behaves exactly as
it does with this section deleted: notifications are delivered in the dashboard
and nowhere else, nothing is queued, and no outbound connection is ever made.

**One feature does not degrade — it refuses.** Account recovery is delivered by
mail and has no second channel, so with no relay there is no *forgot your
password?* link on the sign-in page, `/forgot` says the instance cannot send
mail, and `POST /api/v1/auth/forgot` answers `503`. The server says so once at
boot, beside the sign-up warning. On such an instance a forgotten password is
still the operator's to fix, with database access.

**Turning it off again is not the same as never turning it on.** Messages queued
while a relay was configured stay in the outbox, and with no relay there is
nothing to deliver them: the drain does not run. The scheduler abandons them
once they are more than **30 days** old — the same fixed window the outbox keeps
finished rows for, not a configurable one. They are marked `failed` with the
reason rather than deleted, so the record of what was attempted survives that
window and is then purged like any other finished row, and a warning names the
count. Put `SMTP_HOST` back inside the 30 days and the queue delivers instead.

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_SMTP_HOST` | *(empty)* | **The switch.** Empty means no mailer. |
| `LINKCTRL_SMTP_PORT` | `587` | `465` for implicit TLS, `25` for a local relay. |
| `LINKCTRL_SMTP_TLS` | `starttls` | `starttls`, `tls` or `none`. `starttls` **refuses to send** if the relay does not offer STARTTLS, rather than falling back to plaintext. |
| `LINKCTRL_SMTP_FROM` | *(empty)* | Required once a host is set. A bare address or `LinkCtrl <links@example.com>`. Parsed at boot, not at the first send. |
| `LINKCTRL_SMTP_USERNAME` | *(empty)* | PLAIN authentication. Set both this and the password, or neither. |
| `LINKCTRL_SMTP_PASSWORD` | *(empty)* | Also accepts a `_FILE` suffix, for mounted secrets. Held as a secret that refuses to print itself. |
| `LINKCTRL_SMTP_TIMEOUT` | `10s` | Bounds one delivery attempt end to end: dial, handshake, `DATA`. A batch of twenty is handed over together on a thirty-second tick, so a drain costs one of these rather than twenty; this is also how long a drain occupies the scheduler, which every other background job shares. The batch opens up to twenty sessions to this one relay — a relay that caps concurrent connections lower refuses the extra ones, and those messages retry with backoff. **It also bounds the reachability probe at boot, which since 0.3.0 runs off the startup path**: until then the probe was called inline and an unreachable relay held the whole server dark for the length of this timeout at every start — measured at 10.05s on the default, and long enough at a raised one that `docker compose up --wait` gave up ([F173](build-notes/deferred-findings.md)). Setting a long timeout no longer delays a boot; the `smtp relay reachable` line simply arrives after `http server listening` rather than before it. |

### What is supported, and what is not

The surface is deliberately small, because TLS modes and auth mechanisms are
where a mail configuration turns into a compatibility matrix.

**Supported:** STARTTLS on submission, implicit TLS, or an unencrypted
connection to a relay that needs no credentials; PLAIN authentication, over an
encrypted connection only.

**Not supported:** LOGIN, CRAM-MD5, XOAUTH2, client certificates, and any relay
that requires one of them. There is no fallback and no negotiation — a relay
that will not take PLAIN over TLS cannot be used, and finding that out from this
paragraph is better than finding it out from a bounce.

Credentials over an unencrypted connection are refused by validation rather than
warned about. Go's SMTP client refuses to send PLAIN in clear too, so accepting
the combination here would only move the failure to the first send.

### How mail is delivered

Nothing sends on the request path. A message is rendered, written to the
`mail_outbox` table, and delivered by the `mail` job on the scheduler — the same
scheduler that maintains partitions. Three consequences worth knowing:

- **Mail queued before a restart is still delivered after it.** That is the
  reason for the table. An invitation lost to a deploy landing mid-retry would
  be invisible on both ends: nobody receives it, and nobody knows one was
  attempted.
- **Delivery is not instant.** The job runs every 30 seconds, and once at
  startup.
- **Retry is bounded**: five attempts, backing off 1m, 2m, 4m, 8m, 16m. After
  the fifth the row is marked `failed` and kept, with the relay's error in
  `last_error`. Sent and failed rows are deleted 30 days later; pending rows
  never are.
- **A finished row keeps the record and not the message.** `body` is emptied in
  the same statement that marks a message sent or failed, and the database
  refuses to hold a finished row that still has one. Two of the four templates
  carry a single-use token — an invitation and an address verification — so a
  kept body would be a redeemable credential sitting in the table for the
  retention window above. Everything the query below reads is untouched; if you
  want to know what a message *said*, read the template.

To see what happened to a message:

```sh
docker compose exec -T postgres psql -U linkctrl -d linkctrl -c \
  "SELECT created_at, recipient, kind, status, attempts, last_error FROM mail_outbox ORDER BY created_at DESC LIMIT 20;"
```

Message bodies are **plain text only**. There is no HTML part, which is what
removes remote images that report when a message was opened and anchor text that
disagrees with its link. Every value interpolated into a message has its control
and bidirectional-formatting characters removed first, so nothing a person typed
can become a header or a second message.

### What sends mail today

Four things, and every one of them degrades to a mail-free behaviour rather than
failing:

- **The audit-growth warning.** Once `audit_logs` passes
  `AUDIT_SIZE_WARN_BYTES`, the instance principal gets the in-app
  notification, and the same warning by email.
- **Invitations.** With no mailer the invitation is still created and the link
  is shown to whoever made it, to be passed on by hand.
- **Address verification**, which is what `SIGNUP_MODE=open` needs. Without a
  mailer the effective mode drops to `invite`.
- **Dispute outcomes.** Whoever asked for a blocked destination to be reviewed
  is told what was decided.

In-app delivery is the baseline in every case and does not depend on the mailer;
the email is the addition. The dispute outcome is the one that most repays
configuring a relay, because it is the only message addressed to somebody who
did not choose to be an administrator — a person who filed a dispute may not open
the dashboard again for a week, and the outcome is the thing they are waiting
for.

### Boot behaviour

Startup opens a connection to the relay, greets it, and hangs up. Success is
logged; failure is logged as an **error and the process continues**. A relay
being unreachable is not a reason for a link shortener to stop serving
redirects, and anything queued meanwhile is retried from the outbox.

A *configuration* mistake is still fatal, as every other one is: an unparseable
`SMTP_FROM`, an unknown TLS mode, a username without a password, or credentials
that would go over the wire in clear all refuse to boot.

## Add-ons

**Off unless you set a directory, and off is exact.** With `LINKCTRL_ADDONS_DIR`
empty no WASM runtime is constructed, no goroutine is started, no route is
mounted, no table is created and no metric series is published. That is asserted
by tests rather than asserted here, because "off costs nothing" is the kind of
sentence that stops being true one release after somebody writes it.

| Variable | Default | Notes |
| --- | --- | --- |
| `LINKCTRL_ADDONS_DIR` | *(empty — no add-ons)* | A directory holding one subdirectory per add-on. Validated at startup: a path that is not there, or is a file rather than a directory, refuses to boot rather than being read as "no add-ons". **On compose, add the mount before you set this.** The shipped `docker-compose.yml` has no `/addons` volume, so a container path set here is not readable inside the container and the refusal above takes the instance down — add the `volumes:` snippet from [the trust boundary](#the-trust-boundary) below in the same change. `lctl` reads the same `.env`, so it stops starting too, exactly as `GEOIP_MMDB_PATH` does. |

### The layout

One directory per add-on, **named for the add-on**, holding a manifest and the
module it describes:

```
/var/lib/linkctrl/addons/
  oidc/
    addon.json
    oidc.wasm
```

The directory name and the manifest's `name` must match. That is what lets a
metric label exist for an add-on whose manifest will not parse, and what stops
two directories claiming one identity — which would be two add-ons contending for
one Postgres schema.

### The manifest

`addon.json`, and every field is required unless marked otherwise. Unknown fields
are **refused**, not ignored: `schema_version` is checked for equality, so a field
this host does not know means a manifest written for a schema this host does not
implement.

```json
{
  "schema_version": 1,
  "name": "clickstats",
  "version": "0.4.0",
  "abi_version": 1,
  "module": "clickstats.wasm",
  "sha256": "5f2b…64 lowercase hex characters…",
  "failure_class": "degrade",
  "permissions": ["storage.own_schema", "redirect.observe"],
  "settings": [
    {"name": "retention_days", "type": "text",   "default": "30"},
    {"name": "api_token",      "type": "secret"},
    {"name": "granularity",    "type": "select", "options": ["hour", "day"], "default": "day"},
    {"name": "count_bots",     "type": "toggle", "default": "false"}
  ]
}
```

| Field | Notes |
| --- | --- |
| `schema_version` | Must be `1`. Not "at least 1" — a newer manifest is refused rather than partially understood. |
| `name` | 2–31 characters, lowercase letters, digits and underscores, starting with a letter. It has to be three things at once: a metric label, a directory name, and a Postgres schema name. |
| `version` | The add-on's own version. Its author's business; this only refuses what would make a log line unreadable. |
| `abi_version` | Which host ABI the module was built against. Read and stored now; enforced when the ABI exists. |
| `module` | The `.wasm` file, as a **bare filename** inside this directory. A separator or a `..` is refused rather than cleaned. |
| `sha256` | The digest of that file, 64 lowercase hex characters — what `sha256sum` prints. Verified before the module is compiled; a mismatch means it is never parsed, let alone run. |
| `failure_class` | `required` or `degrade`. No default. |
| `permissions` | Optional. Dotted lowercase names, as in `links.read`. Read and stored; enforcement arrives with the permission model. |
| `settings` | Optional. Each has a `name`, a `type` of `text`, `secret`, `select` or `toggle`, and an optional `default`. A `select` carries at least two `options` and its default must be one of them; a `toggle`'s default is `"true"` or `"false"`; a **`secret` may not carry a default**, because a default secret is one every installation shares. |

### What happens when an add-on will not load

**The add-on decides, not you.** A manifest saying `required` stops the instance
with the reason on stderr; one saying `degrade` logs an error, increments
`linkctrl_addon_loads_total`, and the instance serves without it.

**Both of those are about a module that *fails*.** A module that never returns —
one whose package initialization spins, since initialization runs while the module
is being instantiated — hangs the boot instead, and `degrade` does not rescue it:
nothing has classified the failure yet, so no listener comes up and no metric is
written. There is **no load-time deadline** and the process still answers SIGTERM.
Filed as F267 in the build notes; until it is fixed, an add-on you did not write is
an add-on that can stop this instance from starting whatever class it declares.

So does a manifest this host cannot parse — it stops the instance. There is no
class to honour in that case, and guessing the forgiving one would boot an
instance with an authentication add-on silently missing.

A file that is not a directory is ignored with a warning, so a `README` you left
in there is not an outage.

### The trust boundary

**A module in this directory is code this instance executes.** Who may write to
the directory is the whole of the boundary; the digest in the manifest protects
against the file changing after it was published, and against nothing else — a
manifest and a module replaced together verify perfectly. Own the directory, and
mount it read-only:

```yaml
services:
  app:
    volumes:
      - /var/lib/linkctrl/addons:/addons:ro
```

What a loaded module is handed is covered in [SECURITY.md](SECURITY.md).

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

These existed, did nothing, and are gone. **The table is the set startup warns
about**, and it said three of the five until 0.2.0 — `SECRET_KEY` appeared in no
markdown file in the repository at all, which is the one whose name most invites
somebody to set it (F45). The fixed behaviour was the design
rather than a default waiting to be overridden, so the variable went instead of
the behaviour becoming configurable. Startup warns if one is still set in your
`.env`, and `lctl config check` reports it — a stale line is worth saying out loud
and is not a reason to refuse to boot.

| Variable | Behaviour that is now fixed |
| --- | --- |
| `LINKCTRL_INGEST_WORKERS` | One ingest consumer, which is what makes batch coalescing work. |
| `LINKCTRL_VISITOR_SALT_ROTATION` | One UTC day, the period the purge window de-identifies against. |
| `LINKCTRL_BOT_FILTER_ENABLED` | Bots are always classified and recorded; headline figures exclude them in the queries. |
| `LINKCTRL_SECRET_KEY` | Nothing was ever keyed by it. Sessions use random 32-byte tokens stored as SHA-256, CSRF is origin-based, and API keys use `API_KEY_PEPPER` — rotating this changed nothing, which is the opposite of what a variable with that name promises. |
| `LINKCTRL_DESTINATION_BLOCK_PRIVATE_IPS` | Private, loopback, link-local, carrier-NAT and cloud-metadata addresses are refused unconditionally since M30. It was an off switch on the one tier that must not have one: the person it protects is the visitor whose browser would do the fetching, and they are not the person who would be turning it off. Point links at an intranet with a hostname that resolves there, not with a literal address. |
