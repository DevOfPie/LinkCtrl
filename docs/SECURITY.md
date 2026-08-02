# Security

The security model, the parts of it that are deliberately incomplete, and how to
report something.

This file is the destination for code comments that say "recorded rather than
pretended away". If a defence has a known hole, the hole is described here with
its consequence, not left for a reader to infer from its absence.

## Reporting a vulnerability

**Do not open a public issue.** Use GitHub's private vulnerability reporting on
[the repository](https://github.com/DevOfPie/LinkCtrl/security/advisories/new),
which opens a private advisory visible only to the maintainers.

No private email channel is published, deliberately: an address in a file is a
channel nobody monitors reliably, and a private advisory is tracked, attributable
and hard to lose.

Useful in a report, roughly in order of value: the version or commit, what an
attacker gains, the smallest sequence of requests that demonstrates it, and
whether it needs an authenticated session, an API key, or neither.

Expect an acknowledgement within a week. This is a self-hosted project with no
paid support: there is no bounty, and no coordinated-release calendar beyond
agreeing a date if one is warranted.

## Supported versions

Pre-1.0. Only the latest release gets fixes; there are no maintenance branches.
The database schema only ever changes additively within a minor version, so
upgrading forward is the supported remediation for everything.

## What is defended

Stated as claims, because each one is testable and several have tests naming them.

| Area | Claim |
| --- | --- |
| Destination validation | Scheme **allowlist** (`http`, `https`), so a scheme shipped by a future browser is refused by default rather than added to a blocklist later. Private, loopback, link-local, unique-local, carrier-NAT and cloud-metadata addresses are refused, which is what stops the instance being used as an SSRF proxy toward `169.254.169.254`. Control characters are rejected before parsing, so a newline cannot reach a `Location` header and split a response. |
| Alias namespace | Every dashboard route is a reserved word, enforced by a test that walks the registered routes. A link cannot be created that shadows `/login` or `/docs`. |
| Passwords | argon2id, with a memory floor of 19456 KiB enforced in config validation (RFC 9106). Lowering it below the floor is refused rather than warned about. |
| Sessions | Server-side, opaque, in `__Host-` prefixed cookies — no `Domain` attribute, so the cookie is locked to the exact host that set it. Both an idle and an absolute TTL, enforced at read time so a shortened TTL takes effect immediately. |
| Credential endpoints | Per-account lockout **and** per-address rate limiting, because one address guessing across a leaked list never trips a per-account counter, and many addresses attacking one account never trip a per-address one. |
| Invitations | Bound to the address they name, never to whoever holds the link, so a forwarded invitation cannot add a stranger. Only `SHA-256(token)` is stored, like a session. Single-use, revocable, and expiring on a configurable window that starts at creation. The role an invitation may carry is capped at its issuer's own rank, and an invitation issued with an **API key** may carry no more than `editor` whatever rank created the key. That second bound is what makes `members.write` safe to delegate: redemption produces an interactive account rather than a credential, one that revoking the key does not revoke, so without it a key holding a single delegable scope could mint an account reaching `apikeys.*`, `audit.read`, `org.delete` and `destinations.review`. **Every redemption failure is one answer** — unknown token, expired, revoked, spent, wrong address, wrong password, already a member, or an address with no account — with the same status, the same body and the same argon2 cost, so redemption cannot be asked whether an address is registered. Under `SIGNUP_MODE=closed` no invitation creates an account. |
| Member management | A member may re-role or remove only a membership **strictly below their own rank**, so an admin cannot reach another admin — nor themselves, which is what stops a compromised admin session stripping the peers who would have noticed. Owners are the single exception and manage every rank including each other, because an owner already holds everything and there is nothing left to escalate to; the refusal to remove or demote the **last owner** is what stops an organization being orphaned, and it locks the owner rows before counting them so two administrators acting at once cannot each remove one. Separately, nobody may *grant* a role above their own — the same ceiling an invitation carries. |
| Workspace-scoped roles | A membership naming a workspace **adds** permissions there and removes none anywhere: the evaluator takes the union of every matching membership and the lowest rank among them. There is no operation that restricts somebody to a workspace, so the feature can only ever surprise in the direction of granting more than expected. Deleting a workspace is refused while it holds any link at all, and while it is an organization's last one. |
| Organization creation | Behind a new `orgs.create` permission granted to the **owner role only**, which on a default instance means the account from the setup form and nobody else until an owner grants somebody that role. Expressed as a role grant rather than as a check on how an account was created, so there is exactly one authorization axis. The single exemption is an account holding **no membership at all**, which can create its first organization — a check on present state, at one call site, whose entire effect is to give that account the owner membership `orgs.create` then decides from. |
| Organization deletion | Behind `org.delete`, seeded since the first release, held by the **owner role only** and **never delegable to an API key**: an irreversible action belongs behind an interactive sign-in. The id in the path must be the organization the caller is acting in, so an id cannot be probed and a mistyped one deletes nothing. Refused while any workspace still holds a link — the workspace-level rule one level up, so deleting above it is not a way around it — and while it is the instance's only organization. Both guards lock the rows they count before counting them, so two administrators acting at once cannot each pass a check the other invalidates. The whole teardown is one transaction; a partially deleted organization would leave members resolving into workspaces that no longer exist. |
| API keys | Only an HMAC is stored; the token is shown once. Scopes are intersected with the holder's current role on every request, so demoting a user weakens their keys immediately. `apikeys.*`, `org.delete` and `audit.read` are **not delegable** — the first two because a key that can mint keys or delete the organization makes revoking a leaked one meaningless, the third because the audit log ties a network prefix to a named person. Reading it requires a signed-in session. |
| Audit log | An event that *is* recorded carries the actor snapshotted at write time, so it stays readable after the account is deleted, and a network prefix rather than an address. Reading needs `audit.read`, held by owners and admins, and not delegable to an API key. Retention is its own setting and defaults to keeping everything, so history is never deleted by an upgrade nobody configured. **Coverage is fifteen events so far** — root-redirect changes, an invitation issued, revoked or accepted, a member added, removed or re-roled, a workspace created, renamed or deleted, an organization created or deleted, a destination refused, and bot blocking changed on a link or on the domain — see *What is not defended*. (The count read twelve until M32.5 and had omitted `destination.blocked`; the sentence was being edited anyway and a number known to be wrong is not worth preserving.) What is **not** recorded is a bot actually being refused: that is traffic, counted as a bot click, and a crawler would otherwise write thousands of rows a day into this table. An organization's records carry no foreign key to it, so deleting one leaves its trail, deletion record included, intact in the table; the read API is scoped to the caller's organization, so afterwards that trail is reachable only with database access. |
| Secrets | Configuration secrets are a type that refuses to print itself through `fmt`, `slog` or `json`. A config dump or a formatted panic cannot leak the database password, the API-key pepper or the SMTP password. |
| Outbound mail | Plain text only — no HTML part, so no remote image that reports when a message was opened and no anchor text that disagrees with its link. Every interpolated value has its control and bidirectional-formatting characters removed before it reaches a template, so nothing a person typed can inject a header, forge a second message, or make an address render as one it is not. A relay that will not take STARTTLS is refused rather than downgraded to plaintext. |
| Analytics | No IP address is stored in any column of `click_events`. A visitor is `HMAC(daily salt, ip ‖ user-agent ‖ workspace)` and the salts are deleted after two days, which is the de-identification step rather than housekeeping. Session and audit rows keep a prefix only: /24 for IPv4, /48 for IPv6. |
| Bot blocking | Off by default and inherited from the domain, so installing this product refuses nobody. When it is on, a refused client gets **403 with a fixed page built into the binary** — no alias, no destination, no template execution on a tree that has never rendered one — and the gate runs *before* the link's state is evaluated, so the same bytes come back whether the link is live, expired or archived. Blocking therefore tells a crawler nothing the existing `404` does not. Refusals are counted (a click event with `is_bot`, and `linkctrl_redirects_total{outcome="blocked_bot"}`) and deliberately **not audited**: a crawler asking ten thousand times would otherwise write ten thousand audit rows into the table whose growth is alerted on. Enforcement is behind `domains.write` and reaches every link on the instance; changing one link needs `links.update`. Both changes are audited. Detection is a heuristic and there is **no bypass** — see *What is not defended*. |
| Errors | Internal error text never reaches a client. An unrecognised error is a flat 500; the cause is in the log, because error strings carry table names and connection strings. |
| Egress | No telemetry, no phone-home, no third-party calls in the default configuration. GeoIP is a local file. **Two** outbound connections are possible and both are off until an operator configures them: an SMTP relay (`SMTP_HOST`), and a reputation feed (`FEED_URL`) which sends the destinations your users type to a third party. The feed is the consequential one and it discloses itself — see *Reputation feeds* below. Nothing else in the product opens a socket outwards, and a source scan over the package that judges destinations fails on any outbound-HTTP or name-resolution symbol, so a later "just check the host resolves" cannot become undisclosed egress. |

## What is not defended

The list that matters. Each of these is a decision with a consequence, not an
oversight, and each is also in [Plan.md](../Plan.md#known-limitations) with the
trade-off that produced it.

**DNS rebinding.** Destination validation refuses private address *literals*, but
a hostname that resolves to a public address when the link is created can resolve
to a private one when a visitor follows it. Catching that needs resolution on the
redirect hot path, which the latency target cannot afford, or an egress policy
outside this process. If the instance runs somewhere with reachable internal
services, put the egress control in the network, not here.

**A human blocked as a bot cannot get through.** Bot blocking decides with the
same coarse user-agent heuristic the click statistics use: it treats a missing
user agent as automated and matches substrings including `preview`, `monitor`
and `checker`. Those were written to bucket a number, and their false-positive
rate has never been measured because until now nothing depended on it. There is
no challenge page, no allowlist and no appeal — a misclassified person gets a
403, and the link's owner is not told it happened. The bypass is a Phase 3
milestone rather than a bullet, because a challenge is a rendered, stateful,
interactive surface on the one tree this product keeps free of session lookups
and templates. Until it exists, switching blocking on is a decision to accept
that cost, which is why the default is off and why enforcing it for a whole
domain is behind a separate permission.

**Rate limits are per instance and fail open.** In-memory token buckets, so N
replicas allow N times the configured limit and a restart resets them. When the
key table fills, requests are allowed rather than refused, counted by
`linkctrl_rate_limit_overflow_total` — a limiter is abuse mitigation, not an
authorization boundary, and turning it into an outage would be worse. Alert on
that counter.

**Behind a proxy, `TRUSTED_PROXIES` must be set.** Otherwise every request appears
to come from the proxy, all traffic shares one rate-limit bucket, and the limits
stop working in a way that is invisible in the logs. This is a correctness
requirement once any limit is enabled, not just an analytics nicety.

**The metrics listener has no authentication.** `METRICS_ADDR` (`:9090`) exposes
queue depths, pool saturation and traffic shape to anyone who can reach it.
Compose does not publish it. Do not proxy it, and do not put it on a public
interface.

**`API_KEY_PEPPER` cannot be rotated in place.** Changing it invalidates every
existing key, because the stored HMACs were computed with the old value. There is
no dual-read window.

**Destination blocking is tiered, and one tier is deliberately absolute.** A
destination is refused by one of three tiers, and the reason code in the 422 says
which: `unappealable.*`, `high_confidence.*` or `low_confidence.*`.

| Tier | What it refuses | Overruled by |
| --- | --- | --- |
| Unappealable | Non-`http(s)` schemes; private, loopback, link-local, carrier-NAT and cloud-metadata addresses | **Nothing.** No configuration, no list entry, no review |
| High confidence | Exact hosts on the list compiled into the binary (`internal/link/blocked_hosts.txt`) | Editing that file and rebuilding |
| Low confidence | Two heuristics — punycode homographs and credentials before the host — plus the `blocked_destinations` table, which holds what you listed in `LINKCTRL_DESTINATION_BLOCKLIST` and the known URL shorteners the schema ships with | The instance owner, from the review queue at `/disputes` |

Only one of those lists is compiled in, and the rule is what it costs to be
wrong: a list is compiled when overruling it *should* be hard. The embedded file
makes structural claims about metadata services and control planes, which stay
true for years. The shortener hosts are a `blocked_destinations` row each,
seeded once by the migration that creates the table and never re-asserted, so
removing one is permanent and needs neither a rebuild nor a restart.

The unappealable tier has no off switch and that is the point: it protects the
visitor whose browser would do the fetching, and the visitor is not the party who
would be appealing. An operator who could approve `169.254.169.254` on request
would have turned any review path into the SSRF the validator exists to prevent.

The check runs on the management path — creating a link, editing one, and setting
the domain's root redirect — and never on the redirect path. **Links accepted
before a host was blocked keep redirecting**: re-checking what was already
accepted is a separate job, and reading a blocklist on the hot path is not
something this product does.

**A low-confidence refusal can be appealed, and the queue is built as an attack
surface.** Whoever was refused asks for a review — from the link form, or
`POST /api/v1/disputes` — and the request appears at `/disputes` with a
notification to the owner, who allows it (the blocklist entry is deleted) or
upholds it. Both decisions are audit events and both notify the person who asked.

The queue's whole job is to show an administrator a URL a stranger chose, so:

- **Nothing fetches it.** No preview, no screenshot, no favicon, no liveness
  check — anywhere behind that page. A fetch would be exactly the SSRF the
  address refusals exist to prevent, arriving as a convenience feature, and a
  test parses the feature's source and fails on any outbound-HTTP symbol so it
  stays that way.
- **The destination is defanged in the database and in the API**, and is never
  rendered as a link or in anything a browser resolves. There is no un-defanged
  form obtainable through the API.
- **The filer supplies no free text.** A dispute is a host and nothing else, so
  the stranger-controlled surface is one field wide and that field is inert.
- **The unappealable and high-confidence tiers have no dispute path**, in either
  direction: filing one answers `422 not_disputable`, and a decision can only
  ever delete a `blocked_destinations` row.

**Who can review, and the reach that comes with it.** `destinations.review` is
granted to the **owner** role and to nothing else — admins do not hold it — and
`auth.NonDelegableScopes` keeps it off every API key, because a key that can
allow a destination could then point links at it.

It is nonetheless wider than one organization, and you should size that before
opening sign-ups. The blocklist is instance-wide, so the queue and its decisions
are too: the owner of *any* organization on the instance sees every dispute filed
on it — including the address of whoever filed it — and can lift an entry for
everybody. With `LINKCTRL_SIGNUP_MODE=open` that is one registration away, since
registering provisions an organization and makes the registrant its owner. This
is the same shape `domains.write` has and one degree wider; the cause is that
this product has no instance-level principal, which is also why the signup mode
lives in the environment. **Keep sign-ups closed, or run one organization, if
that reach matters to you.**

**What tiered blocking still does not do.** Nothing *in the default
configuration* decides whether a destination is a phishing page. The
high-confidence list ships with infrastructure hosts, not reputation data,
because a list that costs a rebuild to change is the wrong instrument for data
that changes weekly. A refusal from one of the two heuristics can be upheld but
not allowed: it is computed from the URL every time rather than held as a list
row, so there is nothing to delete and — deliberately — no row anybody can add
that permits a destination. Overruling one of those is a code change. On an
instance where untrusted people can create links, assume they will.

**Reputation feeds, if you switch one on, send your users' destinations to a
third party.** `LINKCTRL_FEED_URL` is unset by default and that is the whole of
the promise that no destination leaves the box; setting it is the exception, and
it is one you are making on behalf of people who are not you. What then leaves is
the destination URL and nothing else — no account, no address, no workspace, no
instance name — over `https`, to the endpoint you named, when a link is created
or edited, when the root redirect is set, and when a refusal is disputed. Never
on the redirect path, and existing links are not re-checked.

Four bounds hold. The feed is asked **last**, so a destination any built-in tier
refuses is never sent anywhere and no built-in answer changes with a feed on, off
or erroring. A feed that does not answer **fails open** and increments
`linkctrl_destination_feed_checks_total{result="error"}` — which means a feed
that silently stopped working looks exactly like no feed at all, so alert on that
counter if you depend on one. Its verdicts are **low confidence**: disputable,
and an owner overruling one from `/disputes` also stops that host being sent
again. And the instance **discloses it** at `/feeds` and `GET /api/v1/feeds`, to
every signed-in account rather than to administrators only, in both states — a
read-only page with no controls, because only you can change any of this and a
disclosure that could be edited from the dashboard would be a settings page this
product has no principal for (decision D40). Feed responses are treated as
hostile input: bounded in size, redirects not followed, and an unreadable verdict
counted as an error rather than guessed at.

**Blocked attempts are recorded, and the attempted URL is hostile input.** Every
refusal writes a `destination.blocked` audit event carrying the tier, the rule,
the surface and an `ip_prefix` — never an address. The attempted URL is stored as
evidence in a defanged form (`https[:]//evil[.]example/...`, everything
HTML-active percent-escaped), so nothing that renders the audit log can turn it
into markup or into a link somebody follows by reflex. Anyone who can create a
link can therefore add rows to `audit_logs`; `LINKCTRL_AUDIT_RETENTION_DAYS` and
the size warning are what bound that.

**No MFA. Addresses are verified only on the self-serve path.** Public
registration confirms the address before the account exists — the form writes a
pending row and mails a single-use link, and the user, organization and workspace
are created when it is followed — so `open` requires a configured mailer and
drops to invitation-only without one. Every other path leaves the address
unverified: the first-run setup account is trusted by construction, and an
invited one proves receipt of a link rather than readership of an inbox. MFA is
Phase 3.

**Who may sign up is the operator's setting and nobody else's.**
`LINKCTRL_SIGNUP_MODE` is the mode, there is no runtime toggle, and no session
or API call changes it — which is deliberate rather than unfinished. This
product has no instance-level principal: an account is an owner *of an
organization*, and a self-registered one owns the organization it was given, so
any permission gating this on the owner role would have been held by every
stranger who signed up on an open instance. Decision D38 removed the toggle
instead of narrowing it. Changing the mode requires whoever can edit the
environment and restart the process.

**Open sign-ups have no CAPTCHA and no proof-of-work.** Registration shares the
sign-in rate limit per address, and confirming an address by email is what stops
an account existing for one nobody controls. Neither stops a distributed run from
filling the pending table or sending mail to addresses that did not ask for it.
Leave `LINKCTRL_SIGNUP_MODE` at `closed` or `invite` unless public sign-up is
something the instance actually wants.

**Queued mail is readable by anyone who can read the database.** The outbox
stores each message rendered — recipient, subject and body — so it survives a
restart and so an operator can see what was attempted. Sent and failed rows are
kept for 30 days. Nothing that must never touch storage should go in a mail; the
first mail this ships, the audit-growth warning, contains no secret.

**Mail is not signed, and delivery is only as trustworthy as the relay.** There
is no DKIM signing and no SPF or DMARC alignment done here — those belong to the
relay and the sending domain's DNS, and an instance that points `SMTP_HOST` at a
relay it does not control has handed that relay every message it sends. PLAIN is
the only supported authentication mechanism; it is refused over an unencrypted
connection, so the password is never sent in clear, but it is a reusable
credential in the environment like any other.

**The audit log works, but plenty still does not write to it.** The writer, the
read API, retention and the growth metric all exist. The events recorded today
are **changing a domain's root redirect**, the **invitation lifecycle** —
issued, revoked, accepted — the **membership changes** M28 added: a member added
to a workspace, removed or re-roled, a workspace created, renamed or deleted, and
an organization created or deleted. Link creation and editing, key minting and revocation,
and sign-in are *not* trailed — each arrives with the Phase 2 milestone that owns
it. Do not treat a quiet audit log as evidence that nothing happened.

**No malware scanning of destinations, no rate limit on redirect volume per
link.** A popular link and a link being used for amplification look the same.

## Operator responsibilities

The product cannot do these for you, and getting them wrong undoes the section
above.

- **Terminate TLS in front of it.** LinkCtrl does not. Session cookies carry
  `Secure` in production and are useless over plaintext. See
  [deployment.md](deployment.md).
- **Keep `:9090` unreachable.**
- **Set `TRUSTED_PROXIES`** to the proxy's address as LinkCtrl sees it, and only
  that. An over-broad value lets a client forge `X-Forwarded-For` and evade
  per-address limits.
- **Generate the secret.** `LINKCTRL_API_KEY_PEPPER`
  with `openssl rand -base64 48`, stored outside the repository. The values in
  `.env.example` are examples, and an instance running with them is compromised by
  anyone who has read the file.
- **If you configure a mailer, own the sending domain's DNS.** SPF, DKIM and
  DMARC are set where the domain lives, not here, and mail from a domain with
  none of them is mail that gets filed as spam or forged by somebody else.
- **Back up before upgrading**, and test the restore. Migrations run at boot and
  `down` migrations drop columns.

## Cryptography

No custom primitives, and nothing here is novel.

| Use | Primitive |
| --- | --- |
| Password storage | argon2id, 64 MiB / 3 iterations / parallelism 2 by default, per-password salt |
| Session tokens | 32 bytes from `crypto/rand`, base64url; only `SHA-256(token)` is stored. SHA-256 rather than argon2 is correct here — the token is full-entropy random, so stretching adds nothing, and this runs on every request |
| API keys | `lk_live_<prefix>_<secret>`; only `HMAC-SHA256(pepper, secret)` is stored |
| Invitation tokens | 32 bytes from `crypto/rand`, base64url; only `SHA-256(token)` is stored. The same construction as a session token, from the same function |
| Visitor identity | `HMAC-SHA256(daily salt, ip ‖ 0 ‖ user-agent ‖ 0 ‖ workspace)`, truncated to 16 bytes |
| CSRF | Go's `net/http` cross-origin protection — `Sec-Fetch-Site` and `Origin` checking on unsafe methods, with `BASE_URL` added as a trusted origin for proxied deployments |

The visitor hash is truncated deliberately: 16 bytes is far beyond collision
relevance for a day's traffic, and the shorter value is the one that never needs
to be reversible.
