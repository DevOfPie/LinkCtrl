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

### Changed

- **Analytics breakdowns are recomputed every fifteen minutes instead of every
  minute.** A link's click count still updates every sixty seconds. What moved is
  the expensive half — the per-country, per-device, per-browser and
  per-destination breakdowns — whose cost grows with the number of distinct
  values your traffic produces rather than with the number of links you have. At
  five and a half million clicks it was using between a sixth and a third of
  every minute; it now uses under one percent of every fifteen.

  **The visible consequence: a breakdown on a link's page can be up to fifteen
  minutes behind the click count above it.** Nothing on the page distinguishes
  the two, which is why the staleness metric above exists and why
  `docs/operations.md` carries an alert for it. This is what makes it safe to
  draw a map from those numbers at all — the alternative was a visualization
  reading a job that would eventually stop finishing inside its own interval.

  Nothing needs to be done about this on upgrade. The existing job keeps its name
  and its position, so its history carries forward; the new one starts from the
  usual two-day window.

- **Upgrading to this version empties the redirect cache.** The cached value a
  short link resolves through now carries its routing rules and its split-test
  arms, and an entry written by the previous version does not — so the cache key
  moved from `v1` to `v3` and every old entry is abandoned rather than read. The
  first request for each live alias after the upgrade reads the database;
  concurrent requests for the same one are collapsed into a single query, so a
  popular link does not become a stampede. Nothing needs to be done about this,
  and nothing is lost; expect a brief rise in database reads while traffic warms
  the cache again.

  The version moved twice inside this unreleased series — once for routing rules
  and once for split testing — but they ship together, so an upgrade from 0.1.0
  pays for one cold cache and not two. Both moved it for the same reason, and it
  is the reason the earlier additions to that value deliberately avoided one: an
  entry written by the older build carries no rules and no arms, so a link whose
  owner has since divided its traffic would go on sending all of it to one place
  for up to `REDIRECT_TTL`. That is a control the owner configured being silently
  absent, which is exactly what a cold cache is for.

  **On a multi-replica rolling upgrade there is one more consequence, and it is
  deliberate.** Cache invalidations are broadcast on a channel versioned with the
  cache key, so while old and new replicas are both running they do not hear each
  other's invalidations. Each still invalidates its own caches correctly; what
  they lose is the cross-replica broadcast, so an edit can take up to
  `REDIRECT_TTL` to reach a replica on the other version. That is the same
  degradation a Redis outage produces, it lasts only as long as the rollout, and
  it is preferred to one version applying another version's messages under
  different rules.

- **`LINKCTRL_REDIRECT_DEFAULT_STATUS` accepts `303`, and never applies to a
  password submission.** A correct link password is now answered `303` whatever
  the instance is set to. On a `307` instance it used to be answered `307`, and
  `307` forbids the browser from changing the method — so the browser re-sent the
  password it had just submitted, as a POST body, to the link's destination: a
  third-party host the link's owner chose and the operator does not control.
  `303` is the one redirect status that mandates a `GET`, so the body stops here.
  Nothing to do on upgrade, and `302` instances — the default — never had the
  problem.

- **A webhook batch is now sent all at once, so one unresponsive receiver no
  longer holds up the rest of the instance.** Deliveries were sent one after
  another on the same goroutine that runs every other scheduled job. At the
  default `LINKCTRL_WEBHOOK_TIMEOUT` of ten seconds a full batch of twenty
  therefore took up to two hundred seconds — and for that whole time nothing else
  ran: invitation and verification mail sat in the outbox, automation rules did
  not fire on their advertised minute, custom-domain re-verification did not
  happen, and the analytics rollups went stale. It needed no misconfiguration and
  no attack, only one webhook pointed at a host that accepts nothing and answers
  nothing, which any member who can register a webhook could do by accident.

  A drain now costs one attempt rather than twenty, whatever the backlog is.
  **If you operate a receiver, the visible change is that it can see up to twenty
  concurrent requests from this instance rather than a steady trickle, and they
  no longer arrive in queue order** — deduplicate on the `X-LinkCtrl-Delivery`
  header, as `docs/usage.md` has always said to. Nothing to configure, and the
  retry schedule, the attempt count and the batch size are all unchanged.

### Added

- **An API key can now replace itself, unattended.** `POST /api/v1/api-keys/rotate`,
  sent with the key's own token, returns a successor and the only copy of its
  secret that will ever exist. There is no id in the URL because there is no other
  key it could rotate: the credential in the `Authorization` header is the one
  being replaced. A signed-in session gets a `403` — a session that wants another
  key mints one, as it always could.

  **The successor is identical or narrower.** Its scopes are this key's, or a
  subset you name; a scope the key does not already hold is refused rather than
  quietly dropped. The workspace binding is copied unchanged. Nothing widens, and
  `apikeys.read`/`apikeys.write` remain permissions no key can ever hold — which
  is what makes it safe to leave rotation in a credential's hands at all. The
  successor inherits the predecessor's *lifetime*: a key created to live 30 days
  rotates into another that lives 30 days, and a key with no expiry rotates into
  one with none.

  **The old key keeps working for a bounded window and then stops.** An hour by
  default, adjustable per rotation between five minutes and a day via
  `grace_seconds`, so a rolling deployment can hold both. When the window closes
  the old key is refused on every replica immediately — the check is part of
  authenticating, not a scheduled job — and housekeeping then marks it revoked so
  the key list agrees. **A key rotates once**; asking again returns `409` rather
  than minting a second successor. Every rotation is an audit event naming the
  prefix it came from and the prefix it became.

  **What this costs, stated plainly: a leaked key can survive a rotation.**
  Whoever holds the secret can rotate it, so revoking the key you know about may
  leave somebody holding a successor you never issued. Every generation shows up
  in your key list with its own prefix, every rotation is in the audit log, and
  each old key's window is capped — that bounds it; it does not remove it. If you
  believe a key has leaked, check the list for keys you did not create before you
  revoke, and rotate nothing.

  One more thing worth knowing when you use rotation: `last_used_at` is written on
  a 30-second cadence, so a key that reads as idle may have been used up to 30
  seconds ago. That is why the grace window cannot be set below five minutes.

  Nothing needs to be done on upgrade. Existing keys are unaffected until
  something rotates one.

- **API keys can now be issued for a whole organization instead of one
  workspace.** The *Reach* choice on the key form, `"org_wide": true` on
  `POST /api/v1/api-keys`, or `--org-wide` on `lctl apikey create`. A key issued
  this way follows its owner into every workspace of the organization instead of
  acting only where it was created.

  **The default has not changed**, and every key you already have is still bound
  to one workspace. The wider option needs `apikeys.write` through an
  *organization-wide* membership: a role you hold in a single workspace issues
  keys for that workspace, which is the same rule that already governs re-roling
  members. The key list, `lctl apikey list` and the API all report which of the
  two a key is.

- **Automation rules: standing instructions the scheduler carries out.** Write a
  rule at `/automation` or through `/api/v1/automation` — *when this happens in
  this workspace, do these things*. Three triggers: a link reaching its expiry, a
  link's click budget running out, and somebody in the workspace being refused a
  destination. Three actions: an in-app notification to the organization's owners,
  an `automation.fired` webhook to whoever subscribed to it, and archiving the
  links that matched. A rule may hold at most three actions and a workspace at
  most twenty rules.

  **Evaluation runs on the scheduler under the leader lock and never on a
  request.** There is no endpoint that evaluates a rule and no button that does;
  a link write, a redirect and an API call all leave rules alone until the next
  scheduler tick, which is every minute. That is enforced by the import graph
  rather than promised: no package on a request path can reach the evaluator.

  **A rule acts on what happens after it exists.** Creating one arms it at that
  instant and resuming a paused one re-arms it, so a rule written this afternoon
  does not fire for every link that expired last year, and a rule paused for a
  month does not deliver a month of backlog when you switch it back on.
  `last_fired_at` is what does this: it is the point a rule has already seen up
  to, so a rule sees each subject exactly once and cannot re-fire on its own
  effect. The page calls it "last fired or armed", because it is both.

  **A rule cannot set another rule off.** No action produces anything any trigger
  watches for, and that is asserted rather than assumed — which is why the webhook
  action always emits `automation.fired` rather than an event you choose, and why
  archiving a link never touches its expiry.

  A threshold is available on every trigger: *fire after N*, with nothing
  discarded while the count is unmet. One run considers at most 100 rules and 25
  subjects per rule, logs when either cap bites, and defers the remainder to the
  next run rather than dropping it; `/api/v1/automation` reports all of it.

  **Which 100 rotates.** An instance may hold more enabled rules than one run
  considers, and the run takes the ones it has looked at least recently — not the
  ones that fired least recently, which is a different set and one that does not
  drain: a rule whose trigger has not matched has not fired, so ordering on
  firings would park every idle rule at the front and leave anything past the
  hundredth unevaluated for good. The scheduler keeps its own timestamp for this,
  separate from the `last_fired_at` a rule shows you.

  Every rule change is an audit event, and so is every firing — the firing record
  names the rule rather than a person, which is what makes an automated archive
  answerable. `linkctrl_automation_firings_total{trigger,outcome}` counts them.

  **Two new permissions**, `automation.read` and `automation.write`, granted to
  the owner and admin roles. `automation.write` cannot be held by an API key: a
  rule keeps acting after the credential that created it is revoked, and it can
  archive links, so a key that could write one would have reach that survives its
  own revocation.

  Nothing needs to be done on upgrade, and there is nothing to configure. No rule
  exists until somebody writes one, and an instance where nobody does pays one
  indexed query a minute that returns nothing.

- **Webhooks: a workspace can have this instance POST its events somewhere.**
  Register a URL at `/webhooks` or through `/api/v1/webhooks`, choose from seven
  events — `link.created`, `link.updated`, `link.archived`, `link.restored`,
  `link.deleted` and `destination.blocked` — and each one arrives as a signed
  JSON POST. The vocabulary is closed: an unknown event name is refused rather
  than ignored, so a subscription that would never fire is not something you can
  create by typo.

  **Payloads are signed with HMAC-SHA256** using a per-webhook secret that is
  shown exactly once, on the response that mints it, and is not stored anywhere it
  can be read back. The scheme is written out for receivers in `docs/usage.md`,
  worked example included; the short version is that the key is the secret string
  as displayed and the message is the timestamp, a dot, and the raw body. Rotating
  a secret has no overlap window — the old one stops verifying immediately, which
  is what somebody rotating a leaked secret actually wants.

  **Delivery is a Postgres queue, not a call on the request path.** A link write
  queues one row per subscribed webhook and returns; the scheduler drains it every
  thirty seconds under the same leader lock the mail outbox uses. A failure is
  retried with a doubling backoff — 1m, 2m, 4m, 8m, 16m, 30m — for seven attempts
  spanning 61 minutes, then abandoned. Every attempt is recorded with its status,
  attempt count and response code, and that log is on the page behind each
  registration's **Deliveries** button and at
  `GET /api/v1/webhooks/{id}/deliveries`. Finished rows are pruned by age;
  `LINKCTRL_WEBHOOK_RETENTION_DAYS` is the window and there is no "keep forever"
  setting for it, because this table grows by a row per link write per webhook.

  **Your endpoint has to be publicly routable, and this is enforced twice.** The
  URL goes through the same destination checks a link's does when you register it,
  and the address the hostname actually resolves to is checked again at the moment
  the connection is opened — so a name that later points at a private address is
  not delivered to either. No redirect is followed at all: a receiver answering
  `302` is pointing this server at a URL nobody registered, and the `3xx` is
  recorded instead of chased. This is deliberately stricter than where a *link*
  may point, because a link sends a visitor's browser somewhere and a webhook
  sends the server; `docs/SECURITY.md` says so at length.

  **Two new permissions**, `webhooks.read` and `webhooks.write`, granted to the
  owner and admin roles. `webhooks.write` cannot be held by an API key: a webhook
  keeps delivering after the credential that created it is revoked, so creating
  one takes a signed-in person. Reading and inspecting deliveries work with a key.
  Up to twenty webhooks fit in a workspace.

  Nothing needs to be done on upgrade. No webhook exists until somebody registers
  one, and an instance where nobody does never opens an outbound connection for
  this feature. `LINKCTRL_WEBHOOK_TIMEOUT` and `LINKCTRL_WEBHOOK_RETENTION_DAYS`
  both have working defaults.

- **Every link now has a QR code, and scanning one is counted.** `GET
  /api/v1/links/{id}/qr.svg` draws it; `GET /api/v1/links/{id}/qr` says what it
  encodes and how it is drawn; `PUT` the same path stores a style — foreground,
  background, error-correction level, quiet zone and module size — and `DELETE`
  puts it back to plain black on white. The link's own page shows the code inline
  with the same controls and a download beside it.

  **SVG only**, by decision: the output is vector text, so no image encoder is in
  this program's dependency set and nothing rasterises on a request. A code
  prints at any size. There is no PNG download and no way to have two codes for
  one link.

  **The code paints its own white background and stays black on white in both
  themes.** A QR code inverted onto a dark field is refused by a large share of
  scanners, and a transparent one inverts itself the moment somebody switches to
  dark mode — so the picture does not follow the theme and the frame around it
  does. The style form will accept a low-contrast pair if that is what your brand
  wants; it refuses the same colour twice and anything that is not a hex colour.

  **A scan is an ordinary click, labelled `qr`.** The code encodes your short URL
  with `?src=qr` on it, because a camera sends no referrer and there is nowhere
  else the fact could come from. Scans then appear in the Referrers breakdown as
  `qr`, beside the `direct` you already had, with no new table, column or
  dimension — so they are deduplicated by visitor, filtered for bots and broken
  down by device and country like everything else. Two things follow and are
  worth knowing: anybody who types `?src=qr` by hand is counted as a scan, and
  two printed codes for one link cannot be told apart. **Any other `?src=` value
  is ignored**, deliberately — the analytics table keys on that value, so an open
  parameter would let anybody grow it without bound.

  `?src=qr` is **forwarded** to your destination when a link has query forwarding
  on, unlike the signature parameters, which are stripped. A signature is a
  credential; a source label is not.

  Nothing to do on upgrade. Codes are generated on demand and no link needs a row
  until somebody restyles its code.

- **Campaigns: label your links with the work they belong to, and filter by it.**
  `GET|POST /api/v1/campaigns`, `GET|PATCH|DELETE /api/v1/campaigns/{id}`, and a
  Campaigns page beside Folders. A link carries a campaign through `campaign_id`
  on create and update, and `GET /api/v1/links?campaign={id}` — or
  `?campaign=none` — filters the list, exactly as the folder filter does.

  A slug is derived from the name if you do not give one and is unique per
  workspace, because it is what a filter URL names. Start and end dates
  **describe** the campaign and enforce nothing: a link in a campaign that ended
  last month redirects exactly as it did before, because expiry is a property of
  the link. **Deleting a campaign keeps every link it held** — they stop carrying
  it, and none is deleted, archived or moved.

  A link can carry a folder and a campaign at the same time; they answer
  different questions. Campaigns are guarded by the link permissions you already
  have rather than by new ones, so a viewer sees and filters by them and an
  editor manages them.

  **There is no per-campaign analytics** — no click total, chart or export. That
  is a later phase, because computing one means a new pass over every click
  event, stacked on the rollup job this release has just rewritten.

- **A workspace can serve its short links on a hostname of its own, once it has
  proved it controls the name.** Registering a hostname is not enough and never
  becomes enough: until a DNS TXT record published in your zone has been read
  back by this instance, a request arriving for the hostname gets the same `404`
  it got before you registered it. That gap is the point of the feature rather
  than a step on the way to it — without it, anybody who pointed a hostname at
  this address could serve short links on it.

  The Domains page shows the record to publish and checks it on demand.
  `POST /api/v1/domains/{id}/verify` is the same check. Once it passes, links can
  be created on the hostname (`domain_id` on create, or leave it out and get your
  workspace's own hostname), `short_url` is built from it, and the bare hostname
  can redirect wherever you like via
  `PUT /api/v1/domains/{id}/root-redirect`. The links list gained a hostname
  filter, and `GET /api/v1/links?domain=` is its API form.

  **Every registered hostname is re-checked hourly, and the check has teeth.**
  The first failure notifies the owning workspace and changes nothing else — one
  failed DNS poll is weak evidence, and taking a customer's links down over it
  would make an availability feature into an availability incident. After **24
  hours** of continuous failure the hostname stops being served on every replica
  at once, with a second notification and an audit record. Both numbers are
  operator configuration (`LINKCTRL_DOMAIN_VERIFY_INTERVAL`,
  `LINKCTRL_DOMAIN_VERIFY_GRACE`); the runbook in `docs/deployment.md` states
  them and what changing them trades. **Renaming a hostname un-verifies it**: the
  record you published proves control of the old name — and a rename that lands
  while a check is running makes that check verify nothing and say so, because it
  proved control of a name the row no longer carries.

  Two bounds keep that stop reachable, and both are about the same thing: the
  hourly pass is the only mechanism that ever takes a lapsed hostname out of
  service, so nothing anybody can create at will may be allowed to delay it. A
  pass checks the hostnames you are **serving** before the ones merely registered.
  And **a workspace may register at most twenty-five hostnames** — every
  registration is a recurring DNS lookup this instance owes to a nameserver
  somebody else runs, whether or not the name resolves.

  **TLS stays your reverse proxy's.** This application never speaks ACME — no
  certificate authority is contacted and no account key is held. It answers
  Caddy's on-demand `ask` at `/tls-check`, and answers it **only** for hostnames
  that have verified; a wider answer would make the instance an unauthenticated
  certificate-issuance trigger for any name on the internet. The Caddyfile block
  is in `docs/deployment.md`. Keep the endpoint on the loopback address.

  Two operational notes for anyone already running this. The links a workspace
  created before verifying a hostname **do not move** — nothing rewrites a URL
  somebody has already published — so a workspace ends up with links on two
  hostnames, which is what the new filter is for. And `/tls-check` is now a
  reserved alias: an existing link with that alias is unaffected, but a new one
  cannot take the name.

- **A workspace can register a domain of its own.** Registering records that a
  hostname belongs to your workspace; the paragraph above is what makes it serve
  anything.

  A **Domains** page in the header's identity menu registers, renames and removes
  them, behind the `domains.write` permission that owner and admin already hold.
  The same operations are on the API: `GET/POST /api/v1/domains`, `PATCH` and
  `DELETE /api/v1/domains/{id}`. The existing `/api/v1/domain` — singular, the
  instance default's root redirect and bot policy — is unchanged and is a
  different resource.

  **A hostname belongs to exactly one workspace, instance-wide.** A hostname is
  one alias namespace, so it cannot be shared: registering a name somebody else
  already registered is refused, and the refusal names the hostname rather than
  its owner. `domains.write` now checks ownership as well as permission — an
  admin manages their own workspace's hostnames and receives `403` on another
  workspace's, which is also absent from their list. The instance's default
  domain stays the operator's and is not renamed or removed from this page.

  Registering, renaming and removing a domain are written to the audit log.

  Nothing to do on upgrade. The migration adds one nullable column and a
  constraint; no existing row changes, and an instance where nobody registers a
  hostname behaves exactly as it did.

- **Folders.** Links can be filed into a tree of folders, up to eight levels
  deep, with a page at **Folders** — linked from the top of the links list — that
  creates, renames, moves and deletes them. The links list gains a folder filter,
  including a **No folder** option for everything that was never filed, and a
  link's own page gains a folder select. Everything the page does is also on the
  API: `GET/POST /api/v1/folders`, `PATCH` and `DELETE /api/v1/folders/{id}`,
  `POST /api/v1/folders/{id}/move`, `folder_id` on a link, and `?folder=` on the
  link list.

  **Deleting a folder never deletes a link.** It removes the folder and the
  folders inside it; every link anywhere in that branch stays exactly where it
  was and becomes unfiled, which the **No folder** filter then finds. There is no
  undo for the folder itself — what is lost is a name and a shape.

  Moving is two clicks rather than a drag: **Move** on a folder, then **Move
  here** on the destination. Only destinations that would actually be accepted
  offer the button, and a folder can never be moved into itself or into anything
  inside it. The page works with JavaScript switched off and is operable from a
  keyboard; there is no drag-and-drop, deliberately.

  Two folders in the same place may not share a name, and the comparison ignores
  case. Nothing needs to be done on upgrade — the `folders` table has existed
  since 0.1.0 with nothing able to write to it, so every existing link starts
  unfiled and behaves exactly as it did.

- **A world map on a link's page, and rings on the breakdowns beside it.** The
  country breakdown is now also a choropleth: countries shaded by their share of
  the link's clicks, five bands, every shape carrying its exact figure. A toggle
  switches the shading to unique visitors, and when it does, the page repeats the
  sentence that says those are privacy-preserving estimates at daily resolution —
  the same one the API returns — because a map is a good deal more persuasive
  than a table and an estimate should not become a fact by being drawn.

  The ranked list is not replaced. It stays a click away from the map, and it is
  still the view that answers "exactly how many". The other breakdowns — devices,
  browsers, operating systems, referrers, languages — each gain a ring showing
  whether the traffic is one value or five, next to the numbers rather than
  instead of them.

  **With no GeoIP database configured the map is not drawn at all**, and says so.
  A world coloured entirely in the no-data shade is a picture of nothing that
  looks like a picture of something.

  No JavaScript, no CDN, no build step: the map is inline SVG computed on the
  server from country outlines compiled into the binary. Those outlines come from
  **Natural Earth**, which is public domain, packaged as world-atlas, which is
  ISC — vendored, version-pinned and checksummed like the two other third-party
  assets in the tree, with `make verify-assets` failing rather than silently
  repairing a mismatch. Nothing about the deployment changes and the Content
  Security Policy is untouched.

- **A staleness metric for the background rollups**,
  `linkctrl_rollup_staleness_seconds{job}` — seconds since each job last
  succeeded, read from the database rather than from process memory, so every
  replica reports the same number and a restart does not make a stalled job look
  healthy. Alert recipes are in `docs/operations.md`. The metric that existed
  before it, `linkctrl_job_last_success_timestamp_seconds`, is unchanged and is
  still the per-replica view; it is the wrong one to alert on and now says so.

- **A link can ask for something before it redirects.** Four gates, each off
  unless you switch it on, each set on the link's own page or over the API.
  Nothing changes for a link that uses none of them.
  - **A password.** The visitor gets a small challenge page and types it in;
    getting it right answers the redirect. It is stored as an argon2id hash with
    the same cost parameters an account password gets, and neither the password
    nor its hash is ever returned by the API. **Nothing is remembered in the
    visitor's browser** — no cookie, no token, no session — so coming back later
    means typing it again. That is deliberate rather than unfinished: it is what
    keeps the short-link host free of the session and CSRF machinery the
    dashboard needs, and it is why the challenge could be added to that host at
    all. Guesses are rate-limited per address *and* per link.
  - **A single use.** The first visit redirects, the second answers 410 Gone.
  - **A click ceiling.** The same thing with a bigger number. The count is exact
    and lives in the database — it is **not** the approximate click total shown
    on the link's page, which is written in batches and can lose one. A HEAD
    request never spends a click, so a link checker cannot use up a one-time
    link by asking whether it is alive — and it is refused all the same once
    there is nothing left to spend, so a spent link answers 410 to HEAD as well
    as to GET. Raising a ceiling on a link that has already stopped starts it
    working again.
  - **A signature.** The plain short URL is refused with 403 and only a signed,
    unexpired one works. Signed URLs are minted from the link's page or with
    `POST /api/v1/links/{id}/sign`, carry an expiry of up to thirty days, and the
    expiry is inside the signature so it cannot be extended by editing the URL.
    The signature parameters are stripped before a link's query forwarding
    passes anything to the destination, so whoever runs the destination never
    receives a URL they could replay. A signed URL names the hostname the link
    is published under — your own verified domain, when the link is on one — and
    works only there: the hostname is inside the signature, so the same alias on
    another of your hostnames is a different link and will not accept it.

- **A link can divide its traffic between several destinations.** A link now
  carries a split test: a set of *arms*, each one a destination of its own, and
  every visitor no routing rule claimed goes to one of them. Managed on the
  link's own page and over the API at `/api/v1/links/{id}/split`. A link with no
  arms is unchanged, which is every link that exists today.
  - **Weighted, for percentage splits.** Each arm has a weight and receives that
    share of the traffic. Weights are relative rather than percentages, so 60/40
    and 600/400 are the same test and adding a third arm does not mean editing
    the first two. The page shows each arm's computed share beside its weight.
  - **Sequential, for a strict rotation.** Arms are visited in turn — first
    visitor to the first arm, second to the second, and round again — and the
    order is **global**: it is kept in the database rather than in a process, so
    it holds across every replica and across restarts. That costs a database
    write on every visit to such a link, and only to such a link. "Approximately
    sequential" would have been free and would have been a support ticket.
  - **A fallback destination.** One per link, used when no rule matched and no
    arm was chosen. It stands in for the link's own destination without changing
    it, so switching the fallback off puts the link back exactly where it was.
  - **Switching an arm off is the feature flag.** One click, no deletion, and
    the remaining arms re-share its traffic on the next request. Clicks already
    recorded against the parked arm are kept. Setting an arm's weight to zero
    does the same thing while leaving it in the weighted list.
  - **There is no stickiness, deliberately.** Somebody following the same link
    twice may see two different arms. Each click is an independent trial, and
    which arm converted is answered by the per-destination breakdown rather than
    by remembering people — which would mean either a cookie this product does
    not set or a per-visitor lookup on the redirect path.
- **Clicks record which destination served them.** `click_events` gains a
  `destination_id`, and the link's page and `GET /api/v1/links/{id}/stats` gain a
  per-destination breakdown: clicks, unique visitors and share, beside each arm's
  configured weight. A split with no attribution is a coin flip with extra steps.
  The breakdown reads the same daily rollup every other breakdown does, so it
  costs nothing to a link that has none, and an unattributed click means the
  link's own destination. Clicks recorded against an arm that has since been
  deleted are still reported, as a destination that no longer exists — a running
  test's totals must not change because somebody tidied up.
- **A split arm's destination goes through the same checks as a link's.** The
  same tiers, the same refusals, the same audit record naming the split-variant
  surface. An arm receiving 5% of the traffic is still somewhere a browser is
  sent.
- **A link can send different visitors to different destinations.** A link now
  carries an ordered list of routing rules: each one has a condition set and a
  destination, they are checked from the lowest priority number upwards, and
  **the first rule that matches wins** — the rest are not consulted. A visitor
  matching none goes to the link's own destination, which is what every link
  does today and will keep doing. Rules are managed on the link's own page and
  over the API at `/api/v1/links/{id}/rules`, and a rule can be switched off
  without being deleted.
- **Twelve conditions, and any combination of them.** Country, region, city,
  language, browser, operating system, device, date and time, referrer host,
  query parameters, UTM parameters, and whether the visitor has been seen
  earlier today. Every condition set on a rule must hold and any listed value
  matches, so a rule narrows as you add to it. Comparison is
  case-insensitive, and a condition tested against something the request does
  not have — no referrer, no GeoIP database — does not match, so an
  unresolvable request falls through rather than being routed on a blank.
- **Date and time conditions are evaluated when the visitor arrives**, never
  when the link was cached, so a window opens and closes on time even on a hot
  link. A window is a set of weekdays, a `HH:MM`–`HH:MM` span, or both, in an
  IANA timezone — `Europe/London`, not an offset, because an offset is wrong for
  half the year. A span whose end is earlier than its start runs overnight. The
  timezone database is compiled into the binary, so a rule evaluates identically
  whatever base image the server was built on.
- **Region and city can be routed on, and are still never stored.**
  `LINKCTRL_GEOIP_MMDB_PATH` pointed at a MaxMind **City** database makes region
  and city conditions work; the country database that was enough for analytics
  carries neither. The values are resolved for the length of one redirect and
  discarded — `click_events.region` and `click_events.city` stay null, asserted
  by test, and no page shows them. A geographic condition on an instance with no
  database simply never matches.
- **"Returning visitor" means seen earlier today, and the day ends at midnight
  UTC.** A visitor from yesterday is new again. That is the whole feature rather
  than an approximation of a longer-lived one: a durable answer needs a cookie or
  a per-person identifier kept across days, and this product keeps neither. It is
  computed from the same daily-salted, address-free visitor hash the analytics
  already use, held in Redis, maintained by the click pipeline rather than on the
  redirect path, and expiring with the day. **With no Redis configured, every
  visitor reads as new.**
- **A rule's destination goes through the same checks as a link's.** Private and
  loopback addresses, the cloud metadata endpoint, obfuscated numeric hosts,
  schemes outside `http`/`https`, the operator's blocklist and any enabled
  reputation feed all apply, and a refusal is recorded in the audit log naming
  the routing-rule surface. A rule reached only by mobile visitors in one country
  is still somewhere a browser is sent.
- **There is no cookies condition, and that is a decision rather than an
  omission.** The redirect path sets no cookie and reads none; adding one would
  mean storing a per-visitor identifier the rest of the product deliberately does
  not keep. Sending one to the API is refused with the code
  `cookies_not_supported`, and the rule-list endpoint advertises the refusal
  beside the twelve conditions that are supported.
- **A link without rules costs exactly what it did before.** Its cached value is
  byte-for-byte what it was, rule evaluation never begins, and no geographic or
  Redis lookup happens. For a link *with* rules, evaluation reads only the
  request and that cached value — no database query per request, and the one
  network call the returning-visitor condition needs is reached only by a
  request that has already satisfied everything else on the rule. Re-measured on
  the built image; see [docs/slo.md](docs/slo.md).
- **`lctl demo` fills an instance with the whole product, not just its links.**
  It was written when a workspace and twenty links were all there was, and it
  had not grown since. It now seeds a second workspace with links of its own, two
  more accounts and their memberships, an outstanding invitation and two
  redeemed ones, an inbox with unread items, an audit trail spanning eight
  actions and two people, three refused destinations with a dispute about each —
  one open, one allowed, one upheld — and bot blocking switched on for exactly
  one link. Everything is created through the same service calls the dashboard
  and the API make, so nothing in it describes a state the product could not
  reach.
- **The demo seeder needs no mailer, enables no reputation feed, and does not
  touch `LINKCTRL_SIGNUP_MODE`.** The outstanding invitation is delivered by the
  copyable link, which is how a default instance delivers one; no destination
  leaves the box; and the extra accounts are written directly, so a `closed`
  instance stays closed while it runs. All three are asserted by test rather than
  documented and hoped for.
- **`lctl demo --reset` puts an instance back the way it found it**, seeded
  accounts and organizations included, so running it twice produces the same
  demo. It always writes into the owning account's oldest workspace rather than
  into whichever one the workspace switcher was last left on, so what a reset
  removes and where the next run writes are the same answer — on a long-lived
  demo instance they were not, and a reset could remove the accounts and the
  click history while leaving the links it could not see. A full run takes a
  little under two seconds against a local Postgres.
- **A link can forward the path a visitor arrived with, not just the query
  string.** `forward_path` is per link and off by default, in the API and as a
  checkbox on the link's edit page. With it on, `/{alias}/api/quickstart`
  reaches the destination's own `/api/quickstart`, so one short link can stand
  in for a whole documentation tree. Both halves may be on at once: the path is
  joined first, then the query is merged onto the result.
- **With path forwarding off, anything after the alias is a `404`.** Not a
  redirect to the bare destination — that would make every link on the instance
  answer every URL beneath itself. It is the custom 404 page, and it spends the
  same 404-probe allowance an unknown alias does, so the refusal cannot be used
  to find out which aliases exist.
- **Forwarding cannot move the destination's origin.** The visitor's segments
  are appended to the destination's path and nothing touches its scheme, host,
  query or fragment; an encoded `?` or `#` stays a path byte rather than
  becoming a separator; and `..` segments are refused rather than resolved, in
  every spelling the URL standard treats as one. A property test asserts those
  invariants over generated input. Measured on the built image with forwarding
  on for all 100,000 seeded links and every request carrying extra path
  segments: 100% of 240,002 requests under 20ms at 2,000 rps, generator p99
  163µs, in [docs/slo.md](docs/slo.md).
- **Automated clients can be refused, per link or for the whole link domain.**
  A link's setting is *inherit*, *block* or *allow*; *inherit* is the default and
  takes the domain's answer, which on a fresh instance is allow — so nothing
  changes for anybody who does not switch it on. An operator with `domains.write`
  can turn it on for the domain, and can additionally **enforce** it, which
  overrules links set to allow. A link-level *allow* under an enforcing domain is
  refused at the API and the control is disabled in the dashboard, rather than
  being stored and quietly ignored. Both settings are on
  `PATCH /api/v1/domain` and `PATCH /api/v1/links/{id}`, and both changes are
  audited.
- **A blocked client gets `403` and learns nothing.** The body is a fixed page
  built into the binary, naming no alias and no destination, and the refusal
  happens before the link's state is looked at — so a live link, an expired one
  and an archived one return byte-identical responses. Being blocked reveals no
  more than the `404` an unknown alias already returns.
- **A refusal is counted, not logged.** It is a click event with `is_bot` already
  true, and increments `linkctrl_redirects_total{outcome="blocked_bot"}`. It is
  deliberately kept out of the audit log: a crawler hitting one blocked link
  thousands of times a day would otherwise fill the table whose growth this
  product alerts on.
- **The redirect path costs nothing extra for it.** The decision reads two fields
  the cached snapshot already carries, so a cached redirect still issues zero
  queries — asserted by a test that counts what reaches Postgres. Measured on the
  built image with blocking switched on for all 100,000 seeded links: 100% of
  240,001 requests under 20ms at 2,000 rps, generator p99 1.5ms, in
  [docs/slo.md](docs/slo.md).
- **Detection is a heuristic and there is no way past it.** The same user-agent
  classifier the analytics use decides — it treats a missing user agent as
  automated and matches substrings including `preview`, `monitor` and `checker`,
  and its false-positive rate has never been measured. A person it misjudges gets
  the 403 with no challenge and no appeal, and the link's owner is not told. That
  is why the default is off; a bypass is a separate, later piece of work.
- **A hostile destination is refused before a link is made, in three tiers that
  differ in what it costs to overrule them.** A refusal's `422` carries a reason
  code naming both — `unappealable.private_address`,
  `high_confidence.embedded_host`, `low_confidence.punycode_homograph` — so a
  caller can tell "this will never be allowed" from "ask the instance owner".
  Non-`http(s)` schemes and private, loopback, link-local, carrier-NAT and
  cloud-metadata addresses are the unappealable tier and have no off switch of
  any kind. A curated list compiled into the binary is the middle tier; changing
  it costs a rebuild, on purpose. Heuristics and a `blocked_destinations` table
  are the bottom tier, meant to be changed without one. The host is canonicalized
  once, before any tier sees it: lowercased, a trailing dot folded away, and a
  host written outside ASCII converted to the same `xn--` form a browser resolves
  it to. So `169.254.169.254.`, `169。254。169。254` with ideographic full stops,
  `１６９.２５４.１６９.２５４` in fullwidth digits and `169.254.169.254` are one address to
  all three tiers, and what gets stored is what they judged. **Both foldings
  convert rather than refuse**, because a trailing dot is a fully qualified name
  and `müller.de` is an ordinary one: `https://example.com./x` is stored as
  `https://example.com/x`, and `https://müller.de/preise` as
  `https://xn--mller-kva.de/preise`. **This changes what is stored for a
  destination whose host is not ASCII** — links created before this version keep
  the spelling they were created with, since accepted destinations are never
  re-checked, and re-saving one converts it. A host that is not a usable name in
  any spelling — a right-to-left override, a broken `xn--` label — is refused as
  malformed, with the untiered code `invalid`.
- **Three low-confidence rules, all of them appealable.** A host spelled to
  imitate another in punycode (`аpple.com` with a Cyrillic а), credentials before
  the host (`https://paypal.com@evil.example/`), and a destination that is itself
  a public short link. Each will sometimes be wrong, which is why they are the
  tier the instance owner overrules. The first two are computed; the third is a
  list of about twenty known shortener hosts that the schema seeds into
  `blocked_destinations`, so **removing one is a `DELETE` and not a rebuild**,
  and it stays removed.
- **The check runs everywhere a destination is written**: creating a link,
  editing one, and setting the link domain's root redirect. It does **not** run
  on the redirect path — a link accepted before its host was listed keeps
  working, because re-checking accepted links is a separate job and reading a
  blocklist on the hot path is not something this product does.
- **Every refusal is in the audit log**, as `destination.blocked`, with the tier,
  the rule, which surface it came from, an `ip_prefix` — never an address — and
  the attempted URL kept as evidence. That URL is stored defanged
  (`https[:]//evil[.]example/...`, anything HTML-active percent-escaped), so
  nothing that displays the log can render it as markup or as a link somebody
  clicks. Note that anyone who can create a link can now add audit rows;
  `LINKCTRL_AUDIT_RETENTION_DAYS` is what bounds that.
- **A low-confidence refusal can be appealed, and the instance owner decides.**
  Somebody who was refused asks for a review — from the link form, or
  `POST /api/v1/disputes` — and the request appears in a queue at `/disputes`
  with a notification to the owner. Allowing it deletes the blocklist entry and
  the destination becomes usable immediately; upholding it leaves the refusal
  standing. Either way the person who asked is notified, and either way it is an
  audit event (`dispute.allowed`, `dispute.upheld`).
- **The other two tiers have no appeal path at all.** A private address, a
  forbidden scheme or a host on the curated compiled list answers `422` with
  `not_disputable` and never reaches the queue. The party those refusals protect
  is the visitor, not the person appealing, and an owner who could approve
  `169.254.169.254` on request would have turned the queue into the SSRF the
  validator exists to prevent.
- **The queue is built as an attack surface, because it is one.** It shows an
  instance owner a URL a stranger chose. Every destination in it is defanged in
  the database and in the API, is never rendered as a link or in anything a
  browser resolves, and **the server never fetches it** — no preview, no
  screenshot, no favicon, no liveness check. A test parses the feature's source
  and fails on any outbound-HTTP symbol, so that stays true.
- **`destinations.review`**, a new permission, granted to the **owner** role
  only and never to an API key. Admins do not hold it: it decides what every
  workspace on the instance may link to, which is wider than the one
  organization an admin administers. A key cannot hold it because allowing a
  destination would let that same key point links there.
- Two refusals `allow` gives instead of pretending to work: a punycode or
  credentials refusal has no blocklist row to delete (the queue marks it and
  offers only *Uphold*), and an entry that came from
  `LINKCTRL_DESTINATION_BLOCKLIST` would come back at the next restart — take it
  out of the environment instead.
- **An optional third-party reputation feed, off by default, and the disclosure
  that comes with it.** Every other check LinkCtrl makes on a destination is
  local. Setting `LINKCTRL_FEED_URL` adds one that is not: the destination
  somebody typed is **sent to a server you name** each time a link is created or
  edited, the root redirect is set, or a refusal is disputed. Nothing is sent on
  the redirect path and existing links are never re-checked. The destination is
  the entire payload — no account, no address, no workspace, no name for your
  instance. `LINKCTRL_FEED_NAME` is required alongside the URL, because an
  instance may not send destinations somewhere it cannot name, and the endpoint
  must be `https`.
- **The instance discloses it, at `/feeds` and `GET /api/v1/feeds`**, to every
  signed-in account rather than to administrators only — what it describes is
  what happens to their own destinations. It names the feed, states what is sent
  and when, and says that only the operator can change it; with no feed
  configured it says plainly that nothing leaves. The page is read-only and
  accepts no `POST`, asserted by test: this product has no instance-level
  principal, so instance-wide settings are not changed from the dashboard.
- **A feed is a low-confidence signal and never a dependency.** It is asked
  **last**, only about destinations every built-in check already accepted — so a
  destination your own rules refuse never reaches the feed, and those rules
  answer identically with a feed on, off, or failing. That is a bound on the
  feed and not on the instance: the refusal is itself a `destination.blocked`
  event, and a workspace that has subscribed a webhook to it receives the
  refused destination, defanged. A refusal it produces is
  `low_confidence.feed_reputation`, appealable like any other, and an owner
  allowing it also stops that host being sent again. A feed that times out,
  errors, or answers something unreadable accepts the destination and increments
  `linkctrl_destination_feed_checks_total{result="error"}` — alert on it, because
  a feed that quietly stopped working is otherwise indistinguishable from no
  feed.
- **Dispute outcomes are emailed** when a mailer is configured. The dashboard
  notification is unchanged and does not depend on one; the email is the
  addition, and it is the only message this product sends to somebody who did not
  choose to be an administrator.


### Removed

- **`qr_codes.scan_count`.** A dormant column that nothing has ever incremented
  since it was created, dropped rather than wired: incrementing it would have
  cost a database write on the redirect path, and the number it produced would
  have disagreed with the click figures beside it — those exclude bots and
  deduplicate visitors, and a raw counter does neither. A QR scan is now counted
  as a click labelled `qr`, which is strictly more than the counter would have
  said. No instance has a non-zero value in it, so nothing is lost on upgrade.

  This is the one non-additive schema change in this release, and it is stated
  here rather than left to be found in a migration.

### Changed

- **`LINKCTRL_DESTINATION_BLOCKLIST` now seeds a database table** instead of
  being held in memory. It keeps working and still matches on a label boundary.
  What changed is that its entries gained a reason code and an audit trail, and
  that they are *reconciled* at every boot: a host you remove from the variable
  is retired on the next restart. Entries the instance owner adds directly are
  never touched by that reconciliation.
- **Destination validation error codes are now `<tier>.<rule>`.** `private_address`
  became `unappealable.private_address`, and the old `host_blocked` became
  `low_confidence.operator_blocklist`. **A client branching on these codes needs
  updating.** Codes for malformed input — `required`, `too_long`, `invalid`,
  `no_scheme`, `no_host` — are unchanged.
- **`LINKCTRL_DESTINATION_SCHEMES` may only narrow the allowlist.** Naming
  anything other than `http` or `https` is now refused at startup. Setting it to
  `https` alone still works and is the reason the variable exists.

### Removed

- **`LINKCTRL_DESTINATION_BLOCK_PRIVATE_IPS`.** Private, loopback, link-local,
  carrier-NAT and cloud-metadata addresses are refused unconditionally. The
  variable was an off switch on a protection whose beneficiary is the visitor
  whose browser would do the fetching, and the visitor was not the one turning it
  off. A stale line in your `.env` is reported at startup and otherwise ignored.
  **If you relied on it to point links at an intranet, use a hostname that
  resolves there** — hostnames are not checked against these ranges.

- **Anybody can sign themselves up, if the operator switches it on.** There is a
  `/signup` page, and `POST /api/v1/auth/register` honours the same setting.
  `LINKCTRL_SIGNUP_MODE` chooses between closed, invitation-only and open, and it
  is the only way to choose: there is no toggle in the dashboard and no endpoint
  that changes it.
- **A self-registered account gets an organization and workspace of its own**,
  and is their owner. It does not join yours — an invitation is what does that,
  and it gives membership and nothing else. The sign-up form says so in words,
  because switching sign-ups on to add a colleague and getting a stranger with a
  private instance-within-the-instance is exactly the surprise this ordering
  exists to avoid.
- **Open registration confirms the address before the account exists.** The form
  answers `202`, writes a pending row and mails a single-use link that lapses
  after a day; the user, organization and workspace are created when the link is
  followed. So `open` requires a configured mailer, and with none the instance
  stays invitation-only — `/signup` refuses on the way in rather than after a
  password has been typed. Registering the same address again replaces the
  outstanding link, which is what to do when the mail does not arrive.
- **You can manage the people already in your organization.** *Members*, in the
  menu behind your address in the header, lists every membership with its role
  and lets you change or end one. `GET /api/v1/members`, `PATCH` and `DELETE
  /api/v1/members/{id}` do the same. The list needs `members.read` and changing
  anything needs `members.write` — both held by owners and admins, and this is
  the first enforcement of `members.read` anywhere.
- **You can only manage roles below your own.** An admin manages editors and
  viewers, and never another admin — nor themselves, so an admin who wants to
  step down asks an owner. Owners manage every role including each other,
  because an owner already holds everything and refusing would only mean a
  departed co-owner could be removed by nothing but SQL. **The last owner of an
  organization cannot be removed or demoted**, by anybody, including themselves.
  Separately, nobody can hand out a role above their own — the same ceiling an
  invitation carries. The cost is worth knowing before you rely on it: one owner
  and a few admins, with the owner away, means the admins cannot be changed at
  all.
- **You can give somebody a role in one workspace.** It **adds** that role there
  on top of whatever they already hold, and takes nothing away anywhere. There
  is no way to *restrict* somebody to a workspace — "org admin, viewer in
  finance" is not expressible — so the feature can only ever grant more than you
  expected, never less. `POST /api/v1/members` issues one; removing it is the
  same call that removes any membership.
- **A role given in one workspace reaches that workspace only.** Somebody who is
  an admin in one workspace manages the memberships scoped to it, and cannot
  re-role or remove an organization-wide membership, grant themselves into a
  second workspace, issue an invitation, or delete the organization — each of
  those is authorized against a membership that covers the whole organization.
  So owning one workspace is not owning the organization, however completely you
  own the workspace. The member list draws only the controls that will work, and
  the workspace picker on it offers only the workspaces you may grant in.
- **Workspaces can be created, renamed and deleted.** *Workspaces*, in the same
  menu, and `POST /api/v1/workspaces`, `PATCH` and `DELETE
  /api/v1/workspaces/{id}`. Until now registration provisioned one and there was
  no way to add another.
- **Deleting a workspace is refused while it still holds any link**, archived
  ones included. Links, tags and folders all cascade from a workspace and there
  is no trash to restore them from, so deleting one with links in it would be a
  redirect outage for every alias it held, and an archived link keeps its alias
  and its click history. The cost is stated rather than hidden: there is no bulk
  delete and no way to move a link between workspaces, so emptying a workspace
  is one link at a time. An organization's **last** workspace is refused too —
  everybody in an organization has to resolve into one to sign in at all.
- **You can create an organization of your own**, provisioned with a workspace
  and an owner membership in one transaction — the same path registration takes,
  not a second implementation of it. `POST /api/v1/organizations`, and a form on
  the workspaces page that moves you into the new organization once it exists.
- **A new `orgs.create` permission**, granted to the `owner` role and to nothing
  else. On a default instance — signup closed, everybody else arriving by
  invitation — that means the account from the setup form and nobody else, until
  an owner deliberately makes somebody else an owner. It is a role grant rather
  than a check on how an account was created, so there is one authorization axis
  rather than two. It is delegable to an API key: a key's scopes are intersected
  with its owner's role on every request, so an organization made through one
  leaves that key holding exactly what it was minted with.
- **An organization can be deleted.** `org.delete` has existed since the first
  release, held by owners and gating nothing; this is the operation behind it.
  *Workspaces*, in the menu behind your address, carries the control, and
  `DELETE /api/v1/organizations/{id}` does the same. It removes every workspace,
  membership, outstanding invitation and API key in the organization, in one
  transaction. **There is no undo, no trash and no export**, and the id in the
  path has to be the organization you are acting in — anything else is a `404`,
  so a mistyped id deletes nothing. Unlike every other permission added this
  cycle, `org.delete` is not grantable to an API key: an irreversible action
  belongs behind an interactive sign-in.
- **Deleting an organization is refused while it still holds any link**, archived
  ones included, and while it is the only organization on the instance. The first
  is the workspace rule one level up — without it, deleting an organization would
  be a way around the refusal that protects a workspace's links — and it carries
  the same cost: no bulk delete, so emptying a large organization is a link at a
  time. The second is the same reasoning that refuses the last owner and the last
  workspace.
- **A deletion still reserves the aliases of trashed links that had traffic.**
  Neither refusal counts links already in the trash — one you have deleted is one
  you cannot delete again, so counting them would make a workspace undeletable
  until the 30-day window ran out, with nothing you could do about it. Those
  links are destroyed with the workspace or organization, and any alias among
  them that ever received a click is reserved permanently, exactly as the purge
  job and a rename already reserve one. An alias that never received a click is
  released and can be registered again. Without this, tidying up inside the trash
  window handed a live audience's alias back to the whole instance.
- **Belonging to no organization is now a state the product handles.** Deleting
  an organization is *not* refused because somebody would be left with no other
  one. Their account survives untouched, they can still sign in, and they are
  offered an organization of their own instead of an error — until they take it,
  every other page redirects them to that offer. This was the alternative to
  refusing a deletion whenever anyone would be orphaned, which would make the
  first organization on a default instance effectively permanent, and to deleting
  those accounts, which would make one click destroy people.
- **Eight more audit events**: a member added to a workspace, removed or
  re-roled; a workspace created, renamed or deleted; an organization created or
  deleted. A workspace or organization deletion records the name, because the row
  it names is gone and the record is the only trace left. **An organization's
  audit trail survives the organization**, deletion record included.
- **You can invite somebody into your organization.** *Invitations*, in the menu
  behind your address in the header, issues a single-use link that adds one
  person as one role. Emailed when a mailer is configured, and copyable either
  way — on an instance with no relay, which is the default, that link is the
  whole delivery mechanism. `POST`, `GET` and `DELETE /api/v1/invitations` do the
  same, behind the `members.write` permission that owners and admins hold. This
  is that permission's first enforcement.
- **An invitation is tied to the address it names, not to whoever holds it.**
  Accepting requires entering the address it was sent to, so a link forwarded
  into a group chat cannot add somebody nobody chose. The cost is stated plainly:
  pasting one into a channel for "whoever needs this" is now a thing the product
  refuses, and joining under a different address needs a new invitation.
- **The role an invitation carries is capped at the issuer's own.** An owner can
  invite an owner; an admin can invite an admin, editor or viewer, and never an
  owner. `members.write` is grantable to an API key, so automation can bring
  collaborators in — but **an invitation issued with a key carries `editor` or
  `viewer` and nothing above**, whatever rank created the key. Redeeming one
  produces an interactive account that revoking the key does not revoke, so
  without that second bound a key holding one delegable scope could mint an
  account holding the scopes no key may hold. Automation that issues `admin`
  invitations through a key now gets a `403` when it asks, rather than an
  account with more authority than the key that made it.
- `LINKCTRL_INVITE_TTL`, defaulting to `168h`. The clock starts when the
  invitation is created rather than when the mail leaves, because delivery is
  asynchronous through the outbox — so a slow relay spends the window, which is
  why it is tunable. Expiry cannot be switched off; revoking is how an invitation
  ends early.
- **Accepting an invitation adds a membership and nothing else.** No personal
  organization or workspace is provisioned for the person joining: they are a
  colleague in an organization that already exists, not a tenant of their own.
  The account is otherwise ordinary. This is also the first way an account can
  hold two memberships, so it is the first time the workspace switcher in the
  header has anything to switch between.
- **`LINKCTRL_SIGNUP_MODE=closed` now means closed to invitations too.** Under
  `closed` an invitation may only add somebody who already has an account;
  `invite` and `open` let it create one. `invite` previously behaved exactly as
  `closed`, and the configuration reference said so — that sentence is gone
  because it is no longer true.
- **Every way accepting can fail answers identically.** An unknown token, an
  expired or revoked or already-used one, the wrong address, the wrong password,
  somebody who is already a member, or an address with no account on a closed
  instance: one status code, one body, and the same password-hashing cost spent
  on each. Anything else would let whoever holds a link ask the server whether a
  given address has an account here.
- Issued, revoked and accepted are audit events, and accepting notifies whoever
  sent the invitation in their dashboard inbox.

- **The dashboard header has an identity menu and a notification bell.** Your
  email address was inert text with Account and Sign out scattered around it, and
  Notifications was a nav link whose badge could say how many but never what.
  The address is now the control: Account and Sign out live under it, and the top
  level is down to Dashboard, Links and API keys. The bell keeps the unread count
  and, opened, previews the newest few unread notifications with a **View all**
  link. `/notifications` is unchanged and is still the full surface.
- Both menus are built on the Popover API. They open from a keyboard, close on
  Escape or a click outside, and need no JavaScript — no page in the dashboard
  runs an inline handler. They are **not** ARIA menus: a screen reader announces
  a button and a group rather than the `role="menu"` pattern, which is the
  honest trade for needing no script.
- **The dashboard now needs a browser from mid-2023 or later**: Chrome 114,
  Safari 17, Firefox 125. That is what the Popover API costs. In anything older
  the two panels render as ordinary blocks in the header — untidy, but Account,
  Sign out and the notifications link all stay on the page.
- **The bell costs no extra database work.** The header already ran one query per
  page render for the unread count; it now returns the count and the preview
  together, so a page still issues exactly one — asserted by a test that counts
  what reaches Postgres.

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
  An **Appearance** control in account settings overrides that with System,
  Light or Dark, and the sign-in page carries the same control — the preference
  is per browser, so it has to be settable before you have an account to sign
  in to.
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
  subscriber that stops hearing invalidations **flushes its in-process caches
  then, and again when it reconnects**, because Redis pub/sub does not replay
  and a replica cannot know which invalidations it missed. The cost is a cold
  cache after a Redis blip; the alternative is serving a destination the owner
  already changed.
- **A Redis that stalls counts as one that stopped hearing.** A connection held
  open with nothing coming down it looks exactly like a channel nobody has
  published on, so `LINKCTRL_REDIS_SUBSCRIBER_READ_TIMEOUT` (30s) bounds how
  long a replica accepts silence before it pings and waits for the reply — the
  part a stalled connection cannot produce. Unanswered, it says so in the log
  and drops what it can no longer vouch for. Without this a wedged Redis, proxy
  or sidecar left a replica serving pre-edit destinations for the whole
  `REDIRECT_TTL` with nothing reporting a problem.
- **Notifications, in the dashboard.** An unread count in the header, a
  notifications page and `GET /api/v1/notifications` with mark-read and
  mark-all-read. (The count began as a nav link and is now the bell described
  above; both shipped unreleased, so this describes where it ended up.) Your own
  inbox
  only — there is no permission for reading somebody else's, because there is no
  reason for one — and no endpoint that creates a notification: they record what
  the system observed, not what a caller asserts.
- The first thing that raises one is the audit-log size threshold above.
  `LINKCTRL_AUDIT_SIZE_WARN_BYTES` defaults to 5 GiB and is **on by default**,
  which is deliberately the opposite of the retention default: keeping
  everything forever is only safe if the instance nobody configured is the one
  that gets warned. Owners are told, at most once a week each, and only owners —
  nobody else can change the setting.
- **A workspace switcher, in the dashboard and at
  `GET /api/v1/workspaces`.** Which workspace a request acts in is now resolved
  in one place for every credential, with a switch that moves the browser that
  asked and is remembered for the next sign-in. `POST
  /api/v1/workspaces/{id}/switch` and `PUT /api/v1/workspaces/default` are the
  API half; both need a signed-in session, because a key acts in the workspace
  its own row names and must not repoint where its owner lands.
- **Where a new sign-in starts** follows the workspace you used last. An
  *Account* setting pins one instead, and offers **Last-Used** as its first
  option — the derived behaviour stays the default, and the override is there
  for anyone it annoys. The pin applies to sessions started after it; switching
  from the header still moves the browser you are in.
- The header control **draws nothing while you have one workspace**, which is
  every account on every instance today: nothing creates a second membership
  yet. This release is the groundwork that lands before invitations do, so the
  identity resolution underneath is settled before a feature starts producing
  memberships.
- **An optional SMTP mailer, off by default.** Set `LINKCTRL_SMTP_HOST` and the
  instance can send mail; leave it empty — which is the default — and nothing
  changes at all: no outbound connection, nothing queued, and every feature that
  could email behaves exactly as it does today. It lands before the features
  that need it so email is part of each one from its first commit rather than
  retrofitted.
- **Sends never happen on the request path.** A message is written to a new
  `mail_outbox` table and delivered by a job on the scheduler that already
  maintains partitions, so mail queued before a restart is still delivered
  after it. Retry is bounded — five attempts backing off 1m to 16m — and a
  message that never got through is kept, with the relay's error, rather than
  disappearing.
- **The audit-growth warning is the first thing that emails.** Owners already
  got it in the dashboard; with a mailer configured they get it by email too,
  under the same once-a-week guard. The in-app notification remains the
  baseline and does not depend on the mailer.
- Messages are **plain text only**. No HTML part means no remote image that
  reports when a message was opened and no anchor text that disagrees with its
  link, and every value put into a message has its control and
  bidirectional-formatting characters stripped first — so nothing anyone types
  can inject a header or forge a second message.
- The supported configuration is deliberately narrow: STARTTLS, implicit TLS, or
  an unencrypted local relay, with PLAIN authentication over an encrypted
  connection. LOGIN, CRAM-MD5, XOAUTH2 and client certificates are not
  supported, and `SMTP_TLS=starttls` **refuses to send** rather than falling back
  to plaintext against a relay that does not offer it.
- `LINKCTRL_SMTP_PASSWORD` **exists again.** It was accepted and ignored before
  0.1.0, then removed because there was no mail feature to authenticate to.
  There is one now.

### Fixed

- **The webhooks and automation pages returned "Link not found".** Both are
  linked from the sidebar, both are documented in `docs/usage.md`, and both
  answered 404 for everybody on every deployment shape, as did all nine of their
  forms. The handlers were there and the API worked throughout; what was missing
  was the entry that attaches those paths to the dashboard, so the requests fell
  through to the short-link handler and were answered as if somebody had followed
  a link that does not exist. Nothing needs to be done on upgrade, and no data
  was affected — the pages simply start working.

  The list those entries lived in is gone. The dashboard's paths are now produced
  by registering the pages themselves, so a page cannot be added and left
  unreachable, and the test that guards the reserved-word list reads the same
  registrations instead of reading that list back to itself.

- **Two organizations with the same name, created within about a minute of each
  other, failed with an internal error.** The suffix that makes an organization
  slug unique was taken from the front of its UUIDv7 — which is the timestamp,
  and whose leading characters are identical for everything created inside the
  same 65-second window. It now comes from the random half of the id. This was
  reachable before only by two accounts registering with the same display name;
  creating organizations by hand is what makes it ordinary.

### Notes for operators

- **`LINKCTRL_SIGNUP_MODE` keeps its meaning and gains a browser.** It was read
  only by `POST /api/v1/auth/register`; it now governs the `/signup` page as
  well. It is still set in the environment and nowhere else — no runtime toggle
  was added, deliberately, because an instance-wide setting held on the owner
  role would be held by everyone who signed up on an open instance.
- **`open` needs `LINKCTRL_SMTP_HOST`.** Public registration confirms the address
  before creating the account, so with no relay the effective mode is
  invitation-only whatever the variable says. `/signup` answers 403 and the
  server states the derivation once at boot.
- **`POST /api/v1/auth/register` answers `202`, not `201`,** and no longer
  returns a user id. Nothing exists when it returns; the account is created by
  the emailed link. A client that expected `201` and a `user_id` needs changing.
- One additive table, `pending_registrations` — the waiting room for an address
  that has not been proven yet. Lapsed and spent rows are swept hourly. No new
  permission.
- Every record stores a network prefix — /24 for IPv4, /48 for IPv6 — and never
  an address, matching what sessions already did. The actor's label is
  snapshotted when the event is written, so a record stays readable after the
  account it names is deleted.
- Nothing is recorded retroactively. The log starts at the upgrade.
- Notifications added no database columns. The table shipped in 0.1.0 and
  per-kind detail goes in its `data` jsonb, so this upgrade is additive in the
  ordinary way and needs no backfill.
- There is no push, and no per-event preference machinery. In-app is the
  baseline; email is an addition an operator switches on.
- The switcher adds three nullable columns —
  `users.default_workspace_id`, `users.last_workspace_id` and
  `sessions.workspace_id` — all NULL after the migration, which is why
  resolution falls through to the workspace every account already resolved to.
  Additive, no backfill.
- Nothing about tenancy has changed yet: one personal organization and one
  workspace are still provisioned per account, and no product surface creates a
  second membership. What changed is that when one exists, it will be reachable.
- The mailer adds one table, `mail_outbox`, and changes nothing else. An
  instance that never sets `LINKCTRL_SMTP_HOST` will never write a row to it.
- **If you configure a mailer, own the sending domain's DNS.** SPF, DKIM and
  DMARC are set where the domain lives, not here. Nothing signs outbound mail.
- Queued messages are stored rendered, so anyone who can read the database can
  read them. Sent and failed rows are deleted after 30 days; pending rows are
  never deleted by age.
- **An edit made while Redis has stopped answering now has a stated bound.**
  `LINKCTRL_REDIS_INVALIDATE_BUDGET`, default `250ms`, is the most an edit will
  wait for the cache to be told a link changed — the total across all three
  attempts rather than each. Previously each attempt got its own
  `REDIS_READ_TIMEOUT`, so raising that knob multiplied it by three into the
  latency of a form submission. Nothing about a healthy or briefly stalled cache
  changes: every retry that used to succeed still fits inside the budget.
- The bound covers a Redis that accepts a connection and never answers, and one
  that answered and then went quiet mid-command. Both are bounded because the
  caller stops waiting — a stalled read ignores the deadline the client is
  given, which is the part that made this worth fixing rather than tuning.
- **A bounded failure is still a failure to invalidate.** The edit is committed
  either way, the failure is logged, and the stale cache entry expires with
  `REDIRECT_TTL` exactly as before. Redirects are untouched, and were measured
  rather than assumed: nothing on that path retries, so a stalled cache costs a
  redirect one `REDIS_READ_TIMEOUT` per call and then Postgres answers it. A
  redirect served from memory costs nothing; a cold one measured 108ms, since it
  pays the timeout on the lookup and again on the write that would have
  refilled the cache.

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
