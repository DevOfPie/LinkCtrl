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
| Invitations | Bound to the address they name, never to whoever holds the link, so a forwarded invitation cannot add a stranger. Only `SHA-256(token)` is stored, like a session. Single-use, revocable, and expiring on a configurable window that starts at creation. The role an invitation may carry is capped at its issuer's own rank, which is why `members.write` is safe to delegate to an API key. **Every redemption failure is one answer** — unknown token, expired, revoked, spent, wrong address, wrong password, already a member, or an address with no account — with the same status, the same body and the same argon2 cost, so redemption cannot be asked whether an address is registered. Under `SIGNUP_MODE=closed` no invitation creates an account. |
| Member management | A member may re-role or remove only a membership **strictly below their own rank**, so an admin cannot reach another admin — nor themselves, which is what stops a compromised admin session stripping the peers who would have noticed. Owners are the single exception and manage every rank including each other, because an owner already holds everything and there is nothing left to escalate to; the refusal to remove or demote the **last owner** is what stops an organization being orphaned, and it locks the owner rows before counting them so two administrators acting at once cannot each remove one. Separately, nobody may *grant* a role above their own — the same ceiling an invitation carries. |
| Workspace-scoped roles | A membership naming a workspace **adds** permissions there and removes none anywhere: the evaluator takes the union of every matching membership and the lowest rank among them. There is no operation that restricts somebody to a workspace, so the feature can only ever surprise in the direction of granting more than expected. Deleting a workspace is refused while it holds any link at all, and while it is an organization's last one. |
| Organization creation | Behind a new `orgs.create` permission granted to the **owner role only**, which on a default instance means the account from the setup form and nobody else until an owner grants somebody that role. Expressed as a role grant rather than as a check on how an account was created, so there is exactly one authorization axis. |
| API keys | Only an HMAC is stored; the token is shown once. Scopes are intersected with the holder's current role on every request, so demoting a user weakens their keys immediately. `apikeys.*`, `org.delete` and `audit.read` are **not delegable** — the first two because a key that can mint keys or delete the organization makes revoking a leaked one meaningless, the third because the audit log ties a network prefix to a named person. Reading it requires a signed-in session. |
| Audit log | An event that *is* recorded carries the actor snapshotted at write time, so it stays readable after the account is deleted, and a network prefix rather than an address. Reading needs `audit.read`, held by owners and admins, and not delegable to an API key. Retention is its own setting and defaults to keeping everything, so history is never deleted by an upgrade nobody configured. **Coverage is eleven events so far** — root-redirect changes, an invitation issued, revoked or accepted, a member added, removed or re-roled, a workspace created, renamed or deleted, and an organization created — see *What is not defended*. |
| Secrets | Configuration secrets are a type that refuses to print itself through `fmt`, `slog` or `json`. A config dump or a formatted panic cannot leak the database password, the API-key pepper or the SMTP password. |
| Outbound mail | Plain text only — no HTML part, so no remote image that reports when a message was opened and no anchor text that disagrees with its link. Every interpolated value has its control and bidirectional-formatting characters removed before it reaches a template, so nothing a person typed can inject a header, forge a second message, or make an address render as one it is not. A relay that will not take STARTTLS is refused rather than downgraded to plaintext. |
| Analytics | No IP address is stored in any column of `click_events`. A visitor is `HMAC(daily salt, ip ‖ user-agent ‖ workspace)` and the salts are deleted after two days, which is the de-identification step rather than housekeeping. Session and audit rows keep a prefix only: /24 for IPv4, /48 for IPv6. |
| Errors | Internal error text never reaches a client. An unrecognised error is a flat 500; the cause is in the log, because error strings carry table names and connection strings. |
| Egress | No telemetry, no phone-home, no third-party calls in the default configuration. GeoIP is a local file. A configured SMTP relay is the one outbound connection the product can make, and it is off unless `SMTP_HOST` is set. |

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

**No malicious-destination blocking yet.** Nothing checks whether a destination is
a phishing page. `LINKCTRL_DESTINATION_BLOCKLIST` refuses host suffixes an
operator names, and that is the whole of it. On an instance where untrusted people
can create links, assume they will. Tiered blocking with an appeal path is
specified for Phase 2 in Plan.md.

**No MFA, and no email verification.** An optional SMTP mailer now exists, but
nothing yet verifies an address with it, so an account's address is unverified.
Public registration is closed by default and there is no signup page;
`SIGNUP_MODE=open` is honoured only by the JSON API. Verification gating open
signup is specified for Phase 2 in Plan.md. MFA is Phase 3.

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
an organization created. Link creation and editing, key minting and revocation,
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
