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
  are the bottom tier, meant to be changed without one.
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
  destination your own rules refuse is never sent anywhere, and those rules
  answer identically with a feed on, off, or failing. A refusal it produces is
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
  owner. Because the ceiling is the issuer's own rank, `members.write` is safe to
  grant to an API key, and it is grantable.
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
  subscriber that loses its connection **flushes its in-process caches when it
  reconnects**, because Redis pub/sub does not replay and a replica cannot know
  which invalidations it missed. The cost is a cold cache after a Redis blip;
  the alternative is serving a destination the owner already changed.
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
