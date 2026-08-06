# LinkCtrl

Self-hostable link management. A short link here is a resource you can edit,
measure, script and revoke — not a row you create once and hope about.

Runs as one Go binary with Postgres and Redis beside it. No Node in the image,
no SaaS dependency, no telemetry leaving the box.

> **Status: 0.2.0.** Everything described below is built, tested and exercised
> end to end, and the redirect latency target is measured rather than
> aspirational. This page describes the released product — see
> [CHANGELOG.md](CHANGELOG.md) for what each version shipped, and its
> `[Unreleased]` section for anything on `main` that no tag carries yet.
> [Not built yet](#not-built-yet) is the list of what is still missing from
> either, and it is the one to read before deploying anything you care about.
>
> *(This block said "Phase 1 complete, released as 0.1.0" while the table below
> described a dozen Phase 2 features in the present tense, and pointed at a
> changelog section that states their absence — a reader who followed the Quick
> start and pinned the tag as instructed got an instance with none of what this
> page had just sold them.)*

---

## Why it exists

Most shorteners make you choose between a hosted product that owns your click
data and a weekend script with no analytics. LinkCtrl aims at the third option:

- **Links stay editable.** The destination changes; the short URL does not.
  Redirects are never permanent — 302 by default — because a 301 cached in
  browsers and intermediaries cannot be recalled.
- **Analytics that cannot identify anybody.** No IP address is stored in any
  column. Visitors are counted with a daily-rotating HMAC that is deleted after
  two days — after which those counts cannot be linked to an address by anyone,
  including you. See [Privacy](#privacy).
- **Everything the dashboard does, the API does.** Both call the same service
  layer, and a contract test replays every documented operation against a live
  server to keep it that way.
- **Fast on the path that matters, and measured.** A two-tier cache in front of a
  dedicated connection pool, on an HTTP tree that carries no session lookup, no
  CSRF check and no templates. Every one of 240,001 cached redirects answered in
  under 20ms at 2,000 rps, with 100k links and 5.7M click events in the database
  and the analytics rollup running throughout — [docs/slo.md](docs/slo.md).

## Quick start

Docker and Docker Compose are the only prerequisites.

```sh
git clone https://github.com/DevOfPie/LinkCtrl.git
cd LinkCtrl
cp .env.example .env
```

Fill in the two secrets in `.env` (`openssl rand -base64 48` for each):

```sh
LINKCTRL_BASE_URL=http://localhost:8080
LINKCTRL_API_KEY_PEPPER=…
POSTGRES_PASSWORD=…
```

Then:

```sh
docker compose up -d --wait
```

That runs `latest`. For anything you care about, pin a version — set
`LINKCTRL_TAG` in `.env` to the release you mean — so that a later `pull` is a
decision rather than a surprise. **The latest tag is `0.1.0`, which is Phase 1
only**: pinning it gets you a working shortener without the organizations,
custom domains, routing rules, split tests, gated links, webhooks or automation
this page describes. Releases also publish static binaries for linux, macOS and Windows if you
would rather not use Docker; see [docs/releasing.md](docs/releasing.md).

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
| **Redirects** | In-process cache → Redis → Postgres, with negative caching for the unknown aliases a public shortener is mostly asked for. Redis is optional: lose it and redirects get slower, not wrong. Per link and off by default, a visitor's query string and the path segments after the alias can be forwarded onto the destination — so one short link can stand in for a whole documentation tree. With path forwarding off, anything under an alias is the same `404` an unknown alias gets. |
| **Routing rules** | Send different visitors to different destinations from one link. Rules are checked lowest priority number first and **the first match wins**; anyone matching none falls through to the link's split test, its fallback, or the link's own destination. Twelve conditions — country, region, city, language, browser, OS, device, date and time, referrer host, query parameters, UTM parameters, and whether somebody was seen on that link earlier today — combined with AND, any listed value matching. Time windows are evaluated when the visitor arrives, in a real IANA timezone, so a window opens and closes on time even on a hot link. Every rule destination goes through the same tier checks a link's own does. Region and city are resolved for the redirect and never stored. There is no cookies condition, deliberately, and asking for one is refused by name. |
| **Split testing** | Divide one link's traffic between several destinations. **Weighted** arms take a share each — weights are relative, so 60/40 and 600/400 are the same test — or **sequential** arms are visited strictly in turn, in an order kept in the database so it holds across every replica and every restart. A **fallback** destination catches whoever no rule and no arm claimed, standing in for the link's own without changing it. Switching an arm off is one click and the rest re-share its traffic, which is what feature-flagging a destination looks like here. Every click records which destination served it, and the link's page shows clicks, visitors and share per arm beside its configured weight — a split with no attribution is a coin flip with extra steps. |
| **Folders** | File links into a tree, up to eight levels deep. Create, rename, move and delete from a page of its own; filter the link list by a folder, or by **No folder** for everything never filed. Moving is two clicks — **Move**, then **Move here** on a destination — and only destinations that would be accepted offer the button, because a folder can never be moved into itself or into anything inside it. Works with JavaScript off and from a keyboard; there is no drag-and-drop, deliberately. **Deleting a folder deletes the folders inside it and no links at all**: everything filed anywhere in the branch survives as unfiled. Two folders in the same place cannot share a name, ignoring case. |
| **QR codes** | Every link has one, drawn as **SVG** by a pure-Go encoder — no PNG, no image encoder, nothing rasterised on a request, and a code that prints at any size. `GET /api/v1/links/{id}/qr.svg` is the picture and the link's page shows it inline with a download beside it. Restyle it — foreground, background, error-correction level, quiet zone, module size — and the style is stored per link; a style changes the drawing and never what the code says. **The code carries its own white background and stays black on white in both themes**, because an inverted QR code is refused by a large share of scanners. **Scans are counted as ordinary clicks**: the code encodes your short URL with `?src=qr`, which a camera cannot add for you, and they appear in the Referrers breakdown as `qr`. |
| **Webhooks** | Register a URL and this instance POSTs a workspace's events to it, signed with HMAC-SHA256 and a per-webhook secret shown once. Seven events — a link's five lifecycle changes, a refused destination, and an automation rule that fired — and the vocabulary is closed, so a subscription that would never fire is not something you can create by typo. Delivery is a **Postgres queue**, not a call on the request path: a link write queues a row and returns, the scheduler drains it under a leader lock, and a failure is retried with a doubling backoff for seven attempts across an hour before it is abandoned. Every attempt is recorded — status, count, response code — and the log is on the page behind each registration. **The URL goes through the same destination checks a link does, and the address it resolves to is checked again at connect time**, because a webhook makes *this server* fetch something rather than a visitor's browser. No redirect is followed. |
| **Automation** | Standing instructions: *when this happens in this workspace, do these things*. Three triggers — a link expiring, a click budget running out, a destination somebody was refused — and three actions: notify the owners, emit an `automation.fired` webhook, or archive the matched links. **Evaluation runs on the scheduler under a leader lock and never on a request**; there is no run-now button and no endpoint that could be one. A rule is armed when you create it and re-armed when you unpause it, so it acts on what happens next rather than on your back catalogue — and `last_fired_at` is a **watermark**, not a note: a rule sees each subject exactly once, so nothing loops. One run reads at most 100 rules and 25 subjects a rule, and a rule that matches more picks up the rest next time rather than dropping it. The hundred are the hundred the scheduler has looked at least recently — not the ones that fired least recently, which would leave every idle rule at the front — so an instance holding more rules than that evaluates all of them, in rotation. Twenty rules fit in a workspace; every change is audited, and so is every firing. |
| **Campaigns** | Label links with the body of work they belong to, and filter the list by it. Create, edit and delete from a page of its own; a slug is derived from the name and is unique per workspace, because it is what a filter URL names. Start and end dates **describe** the campaign and enforce nothing — a link in a finished campaign still redirects, because expiry belongs to the link. **Deleting a campaign keeps every link it held**, unlabelled. A link can carry a folder and a campaign at once: one is where it lives, the other is what it is for. There is no per-campaign analytics — that is a later phase. |
| **Domains** | A workspace registers a hostname of its own, proves it controls it, and serves its links there: register, verify, rename, remove, from a page and from the API, behind `domains.write`. **A hostname belongs to exactly one workspace** — it is one alias namespace, so it cannot be shared and a second registration of the same name is refused instance-wide. An admin manages their own workspace's hostnames and gets `403` on anybody else's; the instance's default domain stays the operator's. Every administrative change is audited. |
| **Custom domain verification** | A **DNS TXT record** is what turns a registration into a served hostname, and nothing else does. Until the check passes, a `Host` header naming the hostname gets the operational `404` whatever DNS points at this instance — which is what stops a stranger registering your name and serving links on it. Verified hostnames are re-checked hourly; a hostname that stops passing keeps working for **24 hours** while its owner is notified, and then stops being served on every replica at once. Both numbers are yours to set. Renaming a hostname un-verifies it, because the record you published proves control of the old name. Links can then be created on your own hostname, with `short_url` built from it, and its bare domain can redirect wherever you like. **TLS stays your proxy's**: LinkCtrl never speaks ACME, and answers Caddy's on-demand `ask` for verified hostnames only. |
| **Analytics** | Clicks, estimated unique visitors, bots, device, browser, OS, language, referrer host, and country with an optional GeoIP database. Daily rollups, server-rendered charts — including a world choropleth and a share ring per breakdown, both computed in Go and drawn as inline SVG, no JavaScript and no CDN — a bounded recent-activity feed, retention enforced by dropping whole months. Totals are recomputed every minute and the per-dimension breakdowns every fifteen, so a breakdown can lag the click count above it by up to that; `linkctrl_rollup_staleness_seconds` says by how much. |
| **Auth** | Email/password with argon2id, server-side sessions in `__Host-` cookies, per-account lockout and per-address rate limiting, real RBAC with four built-in roles and a working permission evaluator. |
| **Abuse limits** | Per-address limits on credential endpoints, the API, and 404 probing. The last charges misses only, so a working link is never throttled by anyone's scanning. |
| **Bot blocking** | Refuse automated clients on a link, or on the whole link domain, with `403` and a body naming nothing — identical whether the link is live, expired or archived, so being blocked reveals no more than a `404` would. Off by default and inherited from the domain; an operator with `domains.write` may enforce it so no link can opt out. Detection is the same user-agent heuristic the click statistics use, and **there is no challenge or appeal**: a person it misjudges cannot get through. Refusals are counted as bot clicks and on `linkctrl_redirects_total{outcome="blocked_bot"}`, never written to the audit log — a crawler would fill it. |
| **API keys** | `lk_live_…` bearer tokens, scoped to permissions you hold, intersected with your current role on every request — and dead the moment their owner stops holding a membership that covers them, so removing somebody stops the credentials they leave behind. Revocable by their owner, or by anybody holding `apikeys.write` across the organization, which is the answer to a key that has to be stopped and an owner who will not stop it. Usage timestamps. Issued for one workspace by default, or for the whole organization if your own membership reaches that far. **A key can replace itself** — one call with its own token returns a successor with the same reach or less, and the old secret keeps working for a bounded window before it stops. Nobody has to be signed in, which is the point; the cost is that a leaked key can rotate itself too, so every generation is listed and audited and [SECURITY.md](docs/SECURITY.md) says what to do about a key you did not create. |
| **Audit log** | Events recorded with the actor snapshotted at write time and a network prefix rather than an address, readable at `GET /api/v1/audit` behind a non-delegable permission. Retention is its own setting and defaults to keeping everything, so growth is reported rather than trimmed silently. **Thirty-two actions are recorded**, which is every administrative change this product makes: the root redirect and bot policy of a domain, the invitation lifecycle, member and workspace changes, the organization lifecycle, refused destinations and the disputes that follow them, domain registration and verification, API key rotation, automation firings, and the instance-level acts that belong to no tenant. This list said seven categories and *the rest arrive with the Phase 2 features that produce them* until 0.2.0, by which point they had arrived — a reader sizing audit coverage from the front page was out by half. A bot being refused is not among them — that is traffic, and it is counted rather than logged. An organization's records outlive the organization, so a teardown does not erase its own trail. |
| **Notifications** | An in-app inbox for things the instance wanted you to know about — the audit log outgrowing its threshold is the first — with mark-read. A bell in the header carries the count and previews the newest few, so answering "what is it" costs nothing; the full page is one click on. Emailed as well when a mailer is configured. |
| **Mail** | Optional SMTP, off unless `SMTP_HOST` is set. Queued in an outbox and delivered by the scheduler, so a message survives a restart; plain text only, and every consumer works unchanged with no mailer at all. |
| **Invitations** | Bring somebody into your organization with a single-use, revocable, expiring link. It is tied to the address you send it to, so forwarding it cannot add a stranger, and the role it carries is capped at your own — at `editor` when an API key issued it, because redeeming one produces an account that outlives the key. Emailed when a mailer is configured, copyable either way. While sign-ups are `closed` an invitation may only add an account that already exists. |
| **Sign-ups** | A public `/signup` form and `POST /api/v1/auth/register`, admitting people according to `LINKCTRL_SIGNUP_MODE` — closed, invitation-only or open. The operator sets it and nothing in the running instance changes it; the shipped default is `closed`. Open registration confirms the address by email before the account exists, so it needs a mailer — with none the instance stays invitation-only and says so at boot. A self-registered account gets an organization and workspace of its own; an invited one gets membership and nothing else. |
| **Members** | A member list with role changes and removal, behind `members.read` and `members.write`. You manage only roles below your own — an admin manages editors and viewers, an owner manages everyone including other owners — and the last owner of an organization cannot be removed or demoted. A role assigned with an API key is capped at `editor`, for the reason an invitation issued with one is: the account it produces outlives the key. Giving somebody a role in one workspace *adds* it there and takes nothing away anywhere — and reaches that workspace only: organization-wide memberships, invitations and the organization itself need a membership that covers the organization. |
| **Workspaces** | Every request resolves to exactly one, and a switcher moves the browser you are in without moving the others. Which workspace a new session starts in follows the one you used last, or a pin you set. Create, rename and delete them within an organization; deleting one is refused while it still holds any link, archived ones included, because everything in it cascades and there is no trash. |
| **Organizations** | Create one of your own, provisioned with a workspace and an owner membership in a single transaction, behind a new `orgs.create` permission held by the owner role. On a default instance that means the account from the setup form and nobody else, until an owner grants it. Delete one behind `org.delete`, which owners alone hold and no API key may: it removes every workspace, membership, invitation and key in one transaction, is refused while any link remains or while it is the instance's last, and leaves the audit trail behind. Somebody left belonging to nothing keeps their account and is offered an organization of their own. |
| **Dashboard** | Server-rendered HTML with htmx. Works without JavaScript; no build step at runtime — the header's menus are popovers, so the browser opens them, closes them on Escape and needs no script to do it. Needs a browser from mid-2023 (Chrome 114, Safari 17, Firefox 125) for that. Light and dark, following the operating system unless overridden per browser — the server renders the theme into the page, so there is no flash of the wrong one. |
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
| [docs/slo.md](docs/slo.md) | The redirect latency target, how it was measured, and what the measurement found |
| [docs/releasing.md](docs/releasing.md) | What a version number means, how a release is cut, how to upgrade and roll back |
| [CHANGELOG.md](CHANGELOG.md) | What changed, and what each version's limitations are |
| [docs/SECURITY.md](docs/SECURITY.md) | The security model, what it does not defend, and how to report a vulnerability |
| [docs/build-notes/development.md](docs/build-notes/development.md) | Working on LinkCtrl itself |
| [docs/build-notes/README.md](docs/build-notes/README.md) | How this project is built, and why the method looks like that — start here to review the process rather than the product |
| [docs/build-notes/workflow.md](docs/build-notes/workflow.md) | How work is done here: gates, commit rules, what happens when a defect turns up |
| [Plan.md](Plan.md) | Scope contract: what is in Phase 1, what is deferred, what is measured |
| [docs/build-notes/decisions.md](docs/build-notes/decisions.md) | Why it is built this way. Every non-obvious choice, with its trade-off |

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

**Two things can send a destination out of the box, and only one of them is the
operator's to switch off.** Every check LinkCtrl makes on a destination is
local — a host list compiled into the binary, a list in your own database, and
rules that read the URL's own text — so neither of the two below is a check. No
other part of the product sends a destination anywhere; the nearest thing to a
third is the dispute-outcome email, which needs a mailer and carries the disputed
host, defanged, to whoever filed the dispute.

**A reputation feed is the operator's.** Setting `LINKCTRL_FEED_URL` means the
destination somebody typed is **sent to a server you named** each time a link is
created or edited, the root redirect is set, or a refusal is disputed. It is
unset by default. When it is set, the instance says so at `/feeds` to every
signed-in user — which feed, what is sent, when, and that only you can change
it — and the same disclosure is on `GET /api/v1/feeds`. A feed that does not
answer is ignored rather than trusted, so the built-in checks behave identically
with one on, off, or failing.

**A webhook is a workspace administrator's**, and no setting of yours reaches it:
somebody holding `webhooks.write` — the owner and admin roles, never an API
key — registers a URL and this instance POSTs that workspace's events to it. The
five link-lifecycle events carry the link's destination as typed, and
`destination.blocked` carries the attempted destination defanged — so a
destination your own rules *refused* leaves this way, even though it never
reaches a feed. It reaches one workspace's own links and no further, and who
holds `webhooks.write` is the whole of the control over it. **`/feeds` answers
for this channel too**, per workspace: anybody signed in is told whether anything
their workspace registered receives the destinations they type, and how many do —
never to what address, which stays behind `webhooks.read`. See
[docs/configuration.md](docs/configuration.md#reputation-feeds) and
[docs/SECURITY.md](docs/SECURITY.md).

## Not built yet

Known limitations and deferred work, so nobody discovers them in production:

- **Rate limits are shared only while Redis is reachable.** The credential and
  API limits are enforced in Redis and hold across replicas; on any Redis error
  each replica falls back to its own bucket, so the limit then applies per
  replica until Redis returns. The 404-probe limiter is per instance by design.
- **Cross-replica cache invalidation needs Redis.** Edits are broadcast on a
  Redis pub/sub channel, so every replica clears its cache. With Redis down each
  replica falls back to waiting out `REDIRECT_TTL`, which is correct but slower
  to converge. A Redis that accepts connections and then stops answering is
  bounded rather than waited on: an edit spends at most
  `REDIS_INVALIDATE_BUDGET` on the cache, then commits anyway and logs that the
  previous destination may be served until the entry expires, and a replica
  whose subscription stops delivering notices within
  `REDIS_SUBSCRIBER_READ_TIMEOUT` and drops what it can no longer vouch for
  rather than serving it as current.
- **The analytics dimension rollup gets expensive with traffic.** It recomputes
  whole days every 60 seconds, which measured 16–21 seconds at 5.7M click events
  and will eventually exceed its own interval. Redirects are unaffected — that is
  what the dedicated pool is for — but dashboards go stale.
- **Region and city are never stored.** With a GeoIP database configured, a
  country is resolved at ingest. Region and city are resolved too — for the
  length of one redirect, when a link's routing rules ask about them — and then
  discarded: both columns stay null, asserted by test, and nothing displays
  them. City plus a timestamp is close to a location history, and a value that
  exists for microseconds is not the same as a value in a row.
- **Routing rules degrade quietly when the instance cannot answer them.** A
  country, region or city condition needs `LINKCTRL_GEOIP_MMDB_PATH`, and region
  and city need a *City* database rather than the Country one. A
  returning-visitor condition needs Redis. Without them the condition simply
  never matches and the visitor goes to the link's own destination — no error,
  and nothing at request time saying why. The rule form says so where a rule is
  written.
- **"Returning visitor" means earlier today and nothing longer.** The day ends
  at midnight UTC and yesterday's visitor is new again, because a durable answer
  needs a cookie or a per-person identifier kept across days and this product
  keeps neither.
- **A custom domain is served only while its DNS record is there.** Verification
  is a TXT record this instance re-reads every hour. If it disappears, the
  hostname keeps working for 24 hours and then stops being served — links on it
  answer `404` until the record comes back. That window is a deliberate trade and
  it is configurable, but there is no setting that makes a hostname serve
  forever on the strength of a check that once passed.
- **LinkCtrl gets no certificates.** TLS for a custom hostname is your reverse
  proxy's job. LinkCtrl answers Caddy's on-demand `ask` for hostnames that have
  verified and refuses everything else; if you use a proxy that cannot ask, you
  configure each custom hostname there by hand.
- **A QR code is SVG only, one per link, and it does not follow your theme.**
  There is no PNG download — the code is vector text, which prints at any size —
  and there is no way to have two codes for one link. The picture paints its own
  white background across its quiet zone rather than going transparent: a QR
  code inverted onto a dark page is refused by a large share of scanners, so the
  drawing stays black on white in both themes and the frame around it is what
  the theme colours. You can restyle it — colours, error correction, quiet zone,
  module size — and the form will let you choose a low-contrast pair if that is
  what your brand wants; it refuses only the two that are certainly broken, the
  same colour twice and anything that is not a hex colour.
- **A scan is a click labelled `qr`, and anybody can type that label.** The code
  encodes your short URL with `?src=qr` on it, because a camera sends no
  referrer and the fact has to travel in the picture. Scans then show up in the
  Referrers breakdown as `qr` beside `direct`. Two consequences worth knowing:
  somebody who types `?src=qr` by hand is counted as a scan, and two printed
  codes for one link cannot be told apart. Any other `?src=` value is ignored
  entirely — the parameter accepts one word on purpose, because the analytics
  table keys on the value and an open one would let anybody grow it without
  bound.
- **A campaign is a label, not a report or a schedule.** It groups links so the
  list can be filtered by it. There is no per-campaign click total, chart or
  export — that is a later phase, because computing one means a new pass over
  every click. Its start and end dates describe the work and enforce nothing: a
  link in a campaign that ended last month still redirects, because expiry
  belongs to the link. Deleting a campaign keeps every link it held; they simply
  stop carrying it.
- **A folder is only a place to put a link.** It carries no settings, grants
  nobody access to anything, and changes nothing about where a link sends
  somebody. Filtering the link list by a folder shows the links filed *directly*
  in it — a parent does not gather up its children's — and deleting a folder is
  final: there is no trash for one, though the links inside it survive as
  unfiled.
- **A gated link is not an authenticated one.** A password, a signature, a
  single use or a click ceiling can be put in front of a link, and each of them
  restricts *the short link* rather than the destination — anybody who reaches
  the destination another way is unaffected, and a visitor who has passed a gate
  once can share the URL they were sent to. A link password is remembered
  nowhere, so it is typed on every visit; guesses are rate-limited per address
  and per link, but that limit falls back to per-replica buckets when Redis is
  unavailable, which makes it a speed bump rather than a bound. Signed URLs
  expire and there is no revocation button: invalidating a workspace's
  outstanding signatures means clearing `workspaces.signing_secret` by hand, and
  each replica keeps honouring the old key for up to a minute after that.
- **A split test does not remember anybody.** Which arm a visitor gets is decided
  per request, so the same person following the link twice may see two. That is
  the whole feature rather than a stage of one: each click is an independent
  trial and which arm converted is answered by the per-destination breakdown,
  because remembering people would need a cookie this redirect path does not set.
  A sequential rotation costs a database write on every visit to such a link —
  strict order across replicas is what the write buys, and only links that ask
  for it pay. Removing an arm keeps its clicks, reported as a destination that no
  longer exists.
- **Routing rules match, in order, and there is no cookies condition.** There
  will not be one; the refusal has a reason code rather than being an absence.
- **Hostile destinations are refused in tiers, and only one tier is absolute.**
  Non-`http(s)` schemes and private, loopback, link-local, carrier-NAT and
  cloud-metadata addresses are refused with no way to switch it off. Above that
  sit a small curated list compiled into the binary and a runtime blocklist —
  the hosts you list, the known URL shorteners the schema ships with, and two
  heuristics for punycode homographs and credentials before the host — which
  will sometimes be wrong. A low-confidence refusal can be appealed: whoever was
  refused asks for a review, and the instance owner allows or upholds it from
  `/disputes`. The other two tiers have no appeal path at all, and a punycode or
  credentials refusal can be upheld but not allowed, because it is computed from
  the URL rather than held as a list entry. Nothing decides whether a destination
  is a phishing page. Blocking runs when a link is created or edited, never on
  the redirect path: a link accepted before its host was blocked keeps working.
- **The review queue is instance-wide, and so is the permission** — but it is
  no longer held by a role. The account that claimed the instance reads the queue
  and decides what is in it, and may appoint other accounts to do the same;
  nobody it appoints can appoint anybody else. Reading the queue is delegable to
  an API key and deciding is not, so an integration can watch the queue and a
  person has to act on it. Before 0.2.0 this was granted to the **owner** role of
  any organization, which on an instance with open sign-ups meant anybody who
  registered.
- **Open sign-ups have no CAPTCHA.** Email confirmation before the account
  exists, and the shared sign-in rate limit, are the whole defence. On a public
  instance that is the largest abuse surface there is, which is why the shipped
  default is `closed`.
- **The signup mode is the operator's alone.** There is no runtime toggle: it is
  an `.env` edit and a restart. An owner who wants to let somebody in without
  one sends an invitation.
- **There is no way to delete an account, and no erasure routine.** Nothing in
  the product removes a user or scrubs their details, and a schema column named
  for a GDPR erasure routine has never had one behind it. Analytics is the
  exception and always was: click events and visitor rows are dropped wholesale
  on a retention schedule and hold no addresses in the first place. If you are
  deploying this somewhere that has to answer erasure requests, that is yours to
  do with database access.
- **There is no account recovery, and a mailer does not add one.** If somebody
  forgets their password, they cannot get back in by themselves: there is no
  *forgot password* link, no reset email, and nothing to switch on — the
  mechanism does not exist, so configuring SMTP changes nothing here. Changing a
  password requires being signed in already. The way back is you, with database
  access, replacing the stored hash on their behalf. The one exception is the
  account that administers the instance itself: `lctl instance principal move`
  hands that role to another account from a shell, which repairs who administers
  the box rather than the password that was lost.
- **You can only manage members below your own role.** An admin manages editors
  and viewers, never another admin and never themselves — so an admin who wants
  to step down asks an owner. Owners are the exception and manage every role
  including each other, bounded by the refusal to remove or demote the last
  owner. The practical consequence on a small instance: one owner, a few admins,
  and the owner unavailable means the admins cannot be changed at all.
- **A workspace-scoped role only ever adds, and only where it was given.**
  Giving somebody a role in one workspace grants it there on top of what they
  already hold; there is no way to *restrict* somebody to a workspace. "Org
  admin, viewer in finance" is not expressible. The authority it adds stops at
  that workspace: somebody who is an admin in one workspace manages that
  workspace's memberships, and cannot change organization-wide memberships,
  reach the organization's invitations at all — issue, list or revoke — or
  delete the organization; those need a membership that covers the
  organization. So an owner of one workspace is not an owner of the
  organization, however completely they own that workspace. It cuts the other
  way too: they are told about *their* workspace — a custom domain failing, an
  automation rule firing — and not about anybody else's.
- **Emptying a workspace is one link at a time.** A workspace holding any link —
  archived ones included — refuses to be deleted, because links, tags and folders
  cascade from it and there is no trash to restore them from. There is no bulk
  delete and no way to move a link between workspaces, so a workspace with fifty
  links takes fifty deletions.
- **Emptying an organization is one link at a time too.** Deleting an
  organization is refused while any of its workspaces still holds a link, for the
  reason deleting a workspace is: otherwise deleting one level up would be a way
  around the rule. The instance's last organization cannot be deleted at all.
- **An account can end up belonging to nothing.** Deleting an organization is not
  refused because somebody would be left with no other one — their account
  survives and they are offered an organization of their own — but until they
  take it, every page redirects them to that offer, *Account* included.
- **A deleted organization's audit trail is readable only from the database.**
  The records survive, but the audit API is scoped to the organization you are
  in, and nobody can be in one that is gone.
- **A member cannot leave an organization themselves.** Removal is done by
  somebody who holds `members.write` and outranks them.


The full list, with consequences, is in
[Plan.md](Plan.md#phase-1-scope-not-yet-built) and
[Known limitations](Plan.md#known-limitations).

## Contributing

[docs/build-notes/development.md](docs/build-notes/development.md) covers the toolchain, the test
strategy and the platform quirks worth knowing (particularly if you develop on
Windows). In short:

```sh
make assets            # build the stylesheet, verify vendored JS
make verify-assets     # fail if a vendored asset does not match its pinned checksum
make mapgen            # regenerate the world-map SVG paths from the vendored TopoJSON
make test              # unit tests, race detector on
make test-integration  # needs `docker compose up -d`
make lint
```

New behaviour is expected to come with a test that fails without it, and any
non-obvious decision with an entry in `docs/build-notes/decisions.md`. The gates a
change has to clear before it is committed, and what happens to a defect found
along the way, are in
[docs/build-notes/workflow.md](docs/build-notes/workflow.md).

Security issues do not go in an issue or a pull request — see
[SECURITY.md](docs/SECURITY.md).

## License

MIT — see [LICENSE](LICENSE).
