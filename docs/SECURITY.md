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
| Destination validation | Scheme **allowlist** (`http`, `https`), so a scheme shipped by a future browser is refused by default rather than added to a blocklist later. Private, loopback, link-local, unique-local, carrier-NAT and cloud-metadata addresses are refused, which is what stops the instance being used as an SSRF proxy toward `169.254.169.254`. The host is canonicalized once before any of that is checked — lowercased, a trailing dot folded away, and a host written outside ASCII put through **UTS-46 ToASCII**, the same conversion a browser applies before it resolves one — so a name cannot be spelled past a check that would otherwise refuse it, and the destination stored is the one that was judged. That last conversion is what makes the sentence true rather than nearly true: until 0.2.0 the claim held only for hosts spelled in ASCII, and `169。254。169。254` with ideographic full stops, `１６９.２５４.１６９.２５４` in fullwidth digits, `ｌｏｃａｌｈｏｓｔ`, `metadata。google。internal` and `127。0。0。1` were each accepted and stored while the ASCII spelling of the same address was refused. **The conversion is a conversion, not a refusal** — `müller.de` is stored as `xn--mller-kva.de` and reaches every tier as that, because refusing internationalized names would close the hole by breaking the product; only a host UTS-46 cannot map at all, such as one carrying a right-to-left override, is refused, and it is refused as malformed rather than by a tier. The operator's own `LINKCTRL_DESTINATION_BLOCKLIST` entries go through the identical fold, so a listed name and a typed destination cannot land in different alphabets and miss each other. Control characters are rejected before parsing, so a newline cannot reach a `Location` header and split a response. |
| Alias namespace | Every dashboard route is a reserved word, enforced by a test that walks the registered routes. A link cannot be created that shadows `/login` or `/docs`. Registering the deep-link pattern `/{alias}/{rest...}` does not widen that: it is the most general pattern in the tree, so every route with a fixed prefix still wins on `ServeMux` specificity, asserted for `/api/v1/…` by `TestRoutesAreNotShadowedByTheCatchAll`. |
| Deep-link path forwarding | Off per link, and off for every link that existed before it. With it on, **the origin cannot move**: the forwarded segments are appended to the destination's path and nothing touches its scheme or host, which is what rules out the reference-resolution bug where a visitor's `//evil.example` becomes a different site. A property test asserts that invariant over generated remainders, along with the destination's query and fragment surviving byte for byte — an encoded `?` or `#` is a path byte and cannot become a separator. Dot segments are **refused rather than resolved**, in every spelling the URL standard counts as one (`..`, `%2e%2e`, `.%2e`), so a forwarded path cannot walk out of the subtree the owner pointed at. With forwarding off, anything under an alias is the same `404` an unknown alias gets and spends the same probe allowance, so the refusal is not an existence oracle. |
| Passwords | argon2id, with a memory floor of 19456 KiB enforced in config validation (RFC 9106). Lowering it below the floor is refused rather than warned about. |
| Sessions | Server-side, opaque, in `__Host-` prefixed cookies — no `Domain` attribute, so the cookie is locked to the exact host that set it. Both an idle and an absolute TTL, enforced at read time so a shortened TTL takes effect immediately. |
| Credential endpoints | Per-account lockout **and** per-address rate limiting, because one address guessing across a leaked list never trips a per-account counter, and many addresses attacking one account never trip a per-address one. **Every sign-in failure is one answer** — unknown address, wrong password, suspended account, and an account locked out by repeated failures all come back with the same status, the same problem type, the same body and the same argon2 cost, so neither what came back nor how long it took says whether an address has an account here. The lockout is the case that had to be folded in rather than added: it used to answer `429 account-locked` where an unregistered address answered `401 invalid-credentials`, and five wrong passwords fit inside `LOGIN_RATE_PER_MIN`, so anybody with a list of addresses could ask — unauthenticated, on the shipped `closed` default, and at the cost of locking whoever they asked about. The refusal names the lockout in words *whether or not one is in force*, which is what keeps the uniform answer from costing a real user the explanation for their own; an operator who needs to know which account is locked reads `users.locked_until`. **Redeeming an invitation is a credential endpoint too**, because it verifies the password of an account that already exists: failures there count toward the same threshold and an account already locked is refused there as well, which it was not until M45 (F51). Its refusal is `ErrNotRedeemable` like every other redemption failure, so the lockout is not observable through it — the uniform answer D27 requires is unchanged, and the invitation is not spent by a refusal. The **link-password challenge** is a credential endpoint too and carries the same posture in the shape available to it: there is no account to lock, so the second limb is the *link* rather than the account — `LINKCTRL_LINK_PASSWORD_RATE_LIMIT` is charged per address **and** per alias on every submission (D54). The per-alias limb is the one that matters: guesses driven through many visitors' browsers spread across as many addresses as there are visitors and would never trip a per-address bucket. **It is best-effort and not a guarantee**, stated in those words: it runs on the shared Redis limiter, and on any Redis error each replica falls back to its own in-memory buckets, so the configured number then applies per replica rather than across them. |
| Account recovery | **A single-use token, mailed, hashed like a session token, and worth what a password is worth.** 32 bytes from `crypto/rand`, stored only as its SHA-256 in `password_resets`, unique-indexed, one hour, and a request supersedes whatever was outstanding so there is never more than one live link per account. **The response is byte-identical whether or not the address has an account**, and pays the same argon2 hash either way — a dummy verification spent before the lookup, so closing the oracle in the body does not leave it open to a stopwatch. The answer goes to the address by mail: an address that cannot be recovered receives a message saying no link was created, which is the stance registration already takes. **Four refusals are one answer** — spent, lapsed, an account whose status is not `active`, and an account with no password — answered `404` and never `410`, because `410` concedes that a token existed and a distinct refusal for a suspended account tells whoever holds the link what state it is in. A password below the floor is refused **before** the token is looked at, so a short password cannot be used to tell a real token from a guess. **A completed reset revokes every session on the account and spends every other outstanding token**, and starts no session of its own; **API keys are deliberately untouched**, because a key is a separate credential with its own rotation story and revoking them would make a recovery an outage. The new hash goes through `auth.WritePassword`, the same function `POST /auth/password` reaches, which also clears the lockout that the guessing may have set. The reset is audited as `password.reset` with the account as actor, a network prefix and no address, and **no organization** — there is no session and therefore no tenant. The routes share the login limiter with `POST /login`, so recovery and credential stuffing draw on one budget. **The mail body carries a live token**, which is why `mail_outbox`'s `status = 'pending' OR body = ''` constraint is load-bearing here and asserted by a test that drives a reset mail to `sent` and reads the row back. **With no `SMTP_HOST` there is no recovery at all**, and the product says so rather than degrading — see *What is not defended*. |
| Invitations | Bound to the address they name, never to whoever holds the link, so a forwarded invitation cannot add a stranger. Only `SHA-256(token)` is stored, like a session. Single-use, revocable, and expiring on a configurable window that starts at creation. The role an invitation may carry is capped at its issuer's own rank, and an invitation issued with an **API key** may carry no more than `editor` whatever rank created the key. That second bound is what makes `members.write` safe to delegate: redemption produces an interactive account rather than a credential, one that revoking the key does not revoke, so without it a key holding a single delegable scope could mint an account reaching `apikeys.*`, `audit.read`, `org.delete` and `destinations.review`. The same cap applies to **assigning** a role, not only to issuing an invitation — promoting somebody already in the organization produces the same interactive principal one axis over. **Issuing, listing, choosing a role for and revoking an invitation all require `members.write` from an organization-wide membership**: an invitation admits somebody to the whole organization, so a role scoped to one workspace neither reads the list of pending invitees nor revokes one. **Every redemption failure is one answer** — unknown token, expired, revoked, spent, wrong address, wrong password, already a member, or an address with no account — with the same status, the same body, and argon2 work spent before each of them returns, so redemption cannot be asked whether an address is registered. The *cost* is equal across every failure reachable without the password: the four that cost twice as much are reached only after a correct password for the invited address was verified, or by the losing half of two simultaneous redemptions, and somebody in that position can sign in as the account and read its memberships directly. Under `SIGNUP_MODE=closed` no invitation creates an account. |
| Member management | A member may re-role or remove only a membership **strictly below their own rank**, so an admin cannot reach another admin — nor themselves, which is what stops a compromised admin session stripping the peers who would have noticed. Owners are the single exception and manage every rank including each other, because an owner already holds everything and there is nothing left to escalate to; the refusal to remove or demote the **last owner** is what stops an organization being orphaned, and it locks the owner rows before counting them so two administrators acting at once cannot each remove one. Separately, nobody may *grant* a role above their own — the same ceiling an invitation carries — and nobody may assign `admin` or `owner` **with an API key**, whatever the rank of the account that created it, for the reason an invitation carries that cap: the result is an interactive account holding scopes no key may hold, and revoking the key does not take it back. |
| Workspace-scoped roles | A membership naming a workspace **adds** permissions there and removes none anywhere: the evaluator takes the union of every matching membership and the lowest rank among them. There is no operation that restricts somebody to a workspace, so the feature can only ever surprise in the direction of granting more than expected. **What it adds reaches that workspace and no further.** Every write is authorized against the membership whose scope covers the object being changed, so an organization-wide membership, an invitation, a **new workspace**, or the organization itself is reachable only from an organization-wide membership — a workspace-scoped admin manages memberships in their own workspace, and a workspace-scoped **owner** owns one workspace rather than the organization, which is what `org.delete` and the last-owner count both read. Before that bound existed, somebody holding an organization-wide `viewer` row and a workspace-scoped `admin` row resolved as an admin inside that workspace and could re-role their **own** organization-wide membership with it. Deleting a workspace is refused while it holds any link at all, and while it is an organization's last one. |
| Organization creation | Behind a new `orgs.create` permission granted to the **owner role only**, which on a default instance means the account from the setup form and nobody else until an owner grants somebody that role. Expressed as a role grant rather than as a check on how an account was created, so there is exactly one authorization axis. The single exemption is an account holding **no membership at all**, which can create its first organization — a check on present state, at one call site, whose entire effect is to give that account the owner membership `orgs.create` then decides from. |
| Organization deletion | Behind `org.delete`, seeded since the first release, held by the **owner role only** and **never delegable to an API key**: an irreversible action belongs behind an interactive sign-in. The id in the path must be the organization the caller is acting in, so an id cannot be probed and a mistyped one deletes nothing. Refused while any workspace still holds a link — the workspace-level rule one level up, so deleting above it is not a way around it — and while it is the instance's only organization. Both guards lock the rows they count before counting them, so two administrators acting at once cannot each pass a check the other invalidates. The whole teardown is one transaction; a partially deleted organization would leave members resolving into workspaces that no longer exist. |
| Two-factor authentication | **TOTP (RFC 6238) only, off by default, and unavailable at all until an operator sets `LINKCTRL_MFA_SECRET_KEY`.** No WebAuthn, no passkeys, no SMS, no push — each is a separate credential model with its own recovery story, and SMS is worse than what it replaces. The secret is 160 bits from `crypto/rand`, **encrypted at rest** under AES-256-GCM rather than hashed, because verifying a time-based code means recomputing it; the key is its own variable and deliberately not `API_KEY_PEPPER`, so rotating an API-key secret cannot lock every account out of its authenticator. **Nothing is written until a code from that secret verifies** — an enrolment somebody abandons leaves the account byte-for-byte as it was, and `mfa_secret` and `mfa_enabled_at` are set by one statement, so half-enrolled is not a state this product has. **The interposition is between the password and the session:** a right password against an enrolled account mints no session token at all, only a single-use pending row that lives five minutes; the API answers `401` with an `mfa-required` problem, so a client that has never heard of this refuses to proceed rather than believing it signed in. **Failed codes charge the account's own lockout counter**, the same counter and the same policy failed passwords use — a second factor with a budget of its own would hand somebody who already had the password an unlimited supply of guesses, which is why a right password no longer clears the counter for an enrolled account. **Replay is refused:** the accepted step is recorded and a code from that step or earlier is rejected inside its own window, so a code read over a shoulder is spent. Clock tolerance is **one step either side — ninety seconds in total** — and no more. **Ten single-use recovery codes** are issued at enrolment, stored as SHA-256 like every other opaque token here, shown once and regenerable, which voids the previous set in full. Spending one is audited and notifies the account, because it means either the phone is gone or somebody else has it. **A password reset does not bypass the second factor** — proving control of a mailbox is one factor, and letting it stand in for the other makes this worth exactly the mailbox — so a recovered account lands at the code prompt, and an account that has lost both the password and the phone is recovered by a recovery code. Disabling needs the password **and** a code (authenticator or recovery), is refused to an API key, and clears the secret and every code in one transaction. **Losing `MFA_SECRET_KEY` is bounded and stated:** enrolled accounts cannot use an authenticator code, they sign in with a recovery code, and an account with neither is the operator's to repair. There is **no organization-level *require MFA* policy** — see *What is not defended*. |
| Account deletion and subject erasure | **`DELETE /api/v1/account`, and a form on the account page, acting on the calling account and nothing else.** There is no administrative delete-somebody-else: who may end another person's account is a permission-model question and this feature does not answer it. Confirmation is the account's own password, re-verified, and an **API key is refused** — the subject of the operation is the person, not the credential. Two refusals, each naming its remedy: the account that administers the instance (move the principal first, with `lctl instance principal move`) and the sole owner of an organization that still exists (the last-owner rule met from the other side, so it cannot be stepped around by leaving). Being left with no organization at all is **not** refused; it is the ordinary way to arrive here. **One transaction ends access**: sessions, API keys, memberships, notifications, outstanding password-reset tokens and instance-level grants are removed, `deleted_at` and `status = 'deleted'` are written, and the address is released for a new account by the partial unique index that has been shaped for it since the first migration. **Erasure is separate and it lags.** The tables that deliberately carry no foreign key to the account — the audit log and destination disputes, which exist so a record outlives its subject — keep their rows; an hourly pass replaces the actor snapshot in all three columns (`actor_label`, `created_by_label`, `decided_by_label`) with the fixed string `deleted account` and clears the account row's address, name and password hash, stamping `anonymized_at`. **Both dispute labels are written by one statement, and that is a correctness requirement rather than tidiness** ([F187](build-notes/deferred-findings.md#closed)): they were two until late in 0.3.0, and a dispute filed *and* decided by accounts the same batch erases kept one of its two addresses, because Postgres applies one of two statements writing a single row and silently drops the other. It never reached a release, and the batch that produces it is ordinary — one account that filed a dispute and then ruled on it, or two that leave within the same hour. **Since 0.3.0 the same pass reaches four things beyond the snapshot**, which is what M58 closed: the `"email"` key inside `audit_logs.metadata`, wherever it matches the erased address case-folded — seven writers, spanning the invitation lifecycle, the three membership actions and the two instance-level ones — the `"from"` **array** beside it on `instance.principal_moved`, which is the one writer that stored a list where the others store a scalar; `invitations.email` on the invitations this account **redeemed**, which an ordinary dashboard page rendered in full behind no `audit.read` and no retention window; and the address in the **notification the inviter received** when this account accepted their invitation, in the detail and in the sentence alike — a row belonging to a different reader, which deleting the account never touched and which no retention window expires. The metadata scrub edits records whose *actor* is still here, because the erased person is their subject; that was weighed and taken, on the reasoning that an erasure reaching the label and stopping at the detail one column over has not erased anybody. An **outstanding** invitation to the same address is deliberately untouched — it is an offer to an address, which became reusable at deletion, and blanking it would break redemption for whoever takes it next. **The bound is one hour**, and it is the scheduler's cadence rather than a promise: access ends inside the deleting transaction and only the residue waits. **The residue identifies nobody from inside this instance — that is the claim, and it is deliberately not "anonymous".** The label is a constant, so it distinguishes nobody; `actor_user_id` survives, which is what keeps one erased actor's entries correlated with each other and is therefore pseudonymous data. Anybody holding an *external* id-to-person mapping re-identifies the actor. That cost was chosen (D148) over a random per-account token with the ids nulled, which is the stronger claim and stays available: this migrates to that, where that could not migrate back. What erasure still leaves — one queued message, one outstanding offer — is in *What is not defended*. **Reusing a deleted address is correct and will read as a bug**: a new account can be created at an address that appears in old audit entries under a tombstone, and they are different people — the ids differ, and nothing about the old account is inherited. |
| API keys | Only an HMAC is stored; the token is shown once. Scopes are intersected with the holder's current role on every request, so demoting a user weakens their keys immediately, and a key whose owner holds no membership covering its scope **does not authenticate at all** — removing somebody from an organization stops their credentials into it, rather than leaving one that resolves with the tenancy attached and an empty permission set. Rotation re-reads the same membership under its own lock, so a removal landing mid-rotation wins. An administrator holding `apikeys.write` from an organization-wide membership can stop somebody else's key, which is the answer to a credential whose owner will not stop it, and **since 0.3.0 what that does depends on the key**: one pinned to their organization is revoked outright, because their organization is all it reached, while an account-wide one has their organization cut out of its reach and keeps working for its owner elsewhere — authority over an account nobody granted them is the thing that refusal exists to withhold. The two are separate audit actions, `apikey.revoked` and `apikey.reach_revoked`, because *was that key stopped* now has two answers and an operator must not have to open the metadata to tell which. **A reach revocation survives rotation, and that is load-bearing rather than tidy.** Rotation authenticates with the key's own token and no session (D87), so without this the holder of a barred credential simply replaces it and is back in the tenant they were cut out of — acting and reading — which is exactly the case the mechanism exists for: the credential that is not in legitimate hands. The successor inherits the predecessor's bars inside the rotation transaction, carrying the original `revoked_at` and `revoked_by` rather than restamping them: rotating does not move the moment an administrator acted, and attributing the bar to whoever rotated would name a credential's holder as the author of a bar against itself. A successor **pinned** to one organization carries none, by construction — a pinned key's reach is the organization it names, it can only have been pinned into one it was never barred from, and a row nothing reads would put *cut out of Acme* on a key pinned to Beta. Revoking your own key is audited as neither, because you are the record. **Since 0.3.0 the owner of a narrowed key is told, on their own key list, which organizations it has been cut out of** ([F178](build-notes/deferred-findings.md#closed)) — the audit record that says *why* is written in the administrator's organization, which the owner may hold no `audit.read` in, so without this the credential simply stopped working in one tenant with no page saying so. **And the narrowing bounds what the key may read, not only what it may do**: `GET /api/v1/workspaces` no longer lists a barred organization's name, slug or workspace ids to the key it was barred from ([F183](build-notes/deferred-findings.md#closed)), because the bound applied was *pinned or not* and the keys a reach revocation applies to are exactly the ones that are not pinned. **Nine permissions are not delegable to a key at all**: `apikeys.read`, `apikeys.write`, `org.delete`, `audit.read`, `webhooks.write`, `automation.write`, `instance.admin`, `destinations.decide` and `audit.read.instance`. Each matches a limb of D18 — escalating, irreversible, or disclosing. A key that can mint keys or delete an organization makes revoking a leaked one meaningless; a webhook or an automation rule keeps running after the credential that created it is revoked; the audit log and its instance-wide surface tie a network prefix to a named person; deciding a disputed destination lifts an entry from the instance-wide blocklist, which widens what the deciding key may itself point at; and `instance.admin` confers instance-level review, so a key holding it could grow the set that may delegate. This list read *three* slugs until 0.2.0 and the map held nine ([F45](build-notes/deferred-findings.md)); `TestDocumentedNonDelegableScopesMatchTheMap` now fails if the two drift again. Reading it requires a signed-in session. A key issued for a single workspace acts only there; either wider reach is opt-in and needs `apikeys.write` held through an organization-wide membership, so a role scoped to one workspace cannot issue a credential reaching the rest. **Since 0.3.0 a key belongs to an account rather than to a tenant**, and there are three reaches: pinned to a workspace, pinned to an organization, or account-wide. An account-wide key resolves into one of the organizations its owner holds an **organization-wide** membership in — the same bar an unpinned key always had, carried across the tenancy boundary so a key cannot acquire reach where its owner is scoped to a single workspace — and its permissions are its scopes intersected with the owner's role *there*, so the same credential is more powerful in one organization than another and a demotion narrows it in one and nowhere else. It also reaches an organization its owner joins **after** it was minted, which is what account-wide means; pinning is how somebody asks for a snapshot instead, and pinning is irreversible because a rotation may narrow a key's reach and never widen it. **Every key issued before 0.3.0 is pinned and stayed pinned** — the migration changed no row's reach. A pinned key reaches its **own** organization's workspaces and no others, even when its owner belongs to several and has pinned a default elsewhere. **Since 0.2.0 that bound is on what a pinned key reads as well as on what it does**: `GET /api/v1/workspaces` answered with one lists its own organization only, where the same call from a browser lists every organization the person belongs to, because crossing them is what the switcher is for. Until then a key returned the names, slugs and identifiers of tenancies it could not touch. **That bound is the pinned one and it does not apply to an account-wide key**, and the premise is why: it rested on a key being issued for one organization, and the tenants such a key reads about are the tenants it works in. Which is not the same as an account-wide key being unbounded — a reach revocation cuts an organization out of exactly that set, so it is one the key no longer works in, and `GET /api/v1/workspaces` stops listing it. One predicate answers both, `keyReaches`: a pinned key reaches the organization it was issued for, an account-wide one reaches every organization its owner does less the ones it has been barred from. **Self-rotation is the one exception to *a key cannot manage keys*, and it is deliberately not an exception to the rule behind it** — see *A key can replace itself, and what that costs* below. |
| Audit log | An event that *is* recorded carries the actor snapshotted at write time, so it stays readable after the account is deleted, and a network prefix rather than an address. **Since 0.3.0 that is a lifecycle rather than a design note**: accounts can be deleted, and the hourly erasure pass replaces an erased actor's snapshot with the constant `deleted account` while leaving `actor_user_id` alone — see *Account deletion and subject erasure* in this table for what that does and does not claim. Until 0.3.0 nothing in this product deleted a user and the snapshot protected against a deletion that could not happen. Reading needs `audit.read`, held by owners and admins, and not delegable to an API key. **What that read returns is bounded by the reader's own authority since 0.2.0**: an organization-wide membership reads the whole organization, as it always did, and a membership scoped to one workspace reads that workspace's records rather than every workspace in the organization — which is what it used to do, including workspaces where the reader held no membership at all. Retention is its own setting and defaults to keeping everything, so history is never deleted by an upgrade nobody configured. **Coverage is thirty-nine actions**, which is every administrative change this product makes: a domain's root redirect and bot policy, the invitation lifecycle, member and workspace changes, the organization lifecycle, a refused destination and the dispute lifecycle that follows it, domain registration, renaming, removal and verification, API key rotation, automation firings, a password recovered from a mailbox, an account deleted by the person who owned it, the four second-factor lifecycle events — enrolled, disabled, a recovery code spent, the codes regenerated — an administrator cutting their organization out of an account-wide key's reach, and the instance-level acts that belong to no tenant — see *What is not defended*. **The number has been wrong twice**, which is why it is now checked rather than restated: it read twelve until M32.5 while omitting `destination.blocked`, and eighteen until 0.2.0 while the vocabulary had grown past it ([F45](build-notes/deferred-findings.md)). Both times a hand-maintained number sat beside a list nothing checked, and the mechanical reason a careful count still came up short was that two of the actions — `dispute.allowed` and `dispute.upheld` — were declared in `internal/dispute`, so anything enumerating the vocabulary from `internal/audit` was silently missing them (F18). Since 0.2.0 there is one home and one list: `audit.AllActions`, with a test that parses the source and fails if a constant is declared without appearing in it. The count above is that list's length. What is **not** recorded is a bot actually being refused: that is traffic, counted as a bot click, and a crawler would otherwise write thousands of rows a day into this table. **Nor is every repeat of the same refusal, since 0.2.0**: `destination.blocked` is the only action here recording something that did not happen, so it is the only one with no successful state change bounding how often somebody can provoke it, and an ordinary member could otherwise loop a refused destination and write a row per request. It is bounded per actor **and per reason code** — ten a minute — which is what keeps it a bound rather than a way to bury evidence: a refusal code nobody has provoked before is always recorded, however hard another is being hammered, and the destination is refused identically whether or not its row is written. An organization's records carry no foreign key to it, so deleting one leaves its trail, deletion record included, intact in the table; the read API is scoped to the caller's organization, so afterwards that trail is reachable only with database access. **Acts that belong to no organization have their own surface**, `GET /api/v1/instance/audit`, behind `audit.read.instance`, held by the instance principal alone and not delegable: the default domain's root redirect and bot policy, every dispute decision, and every change to who reviews them all govern every organization on the instance, and until 0.2.0 each was filed under whichever organization the person happened to be acting in — visible to a tenant with no claim to it, and invisible to every tenant it changed. |
| Secrets | Configuration secrets are a type that refuses to print itself through `fmt`, `slog` or `json`. A config dump or a formatted panic cannot leak the database password, the API-key pepper or the SMTP password. |
| Outbound mail | Plain text only — no HTML part, so no remote image that reports when a message was opened and no anchor text that disagrees with its link. Every interpolated value has its control and bidirectional-formatting characters removed before it reaches a template, so nothing a person typed can inject a header, forge a second message, or make an address render as one it is not. A relay that will not take STARTTLS is refused rather than downgraded to plaintext. |
| Analytics | No IP address is stored in any column of `click_events`. A visitor is `HMAC(daily salt, ip ‖ user-agent ‖ workspace)` and the salts are deleted after two days, which is the de-identification step rather than housekeeping. Session and audit rows keep a prefix only: /24 for IPv4, /48 for IPv6. |
| Bot blocking | Off by default and inherited from the domain, so installing this product refuses nobody. When it is on, a refused client gets **403 with a fixed page built into the binary** — no alias, no destination, no template execution on a tree that has never rendered one — and the gate runs *before* the link's state is evaluated, so the same bytes come back whether the link is live, expired or archived. Blocking therefore tells a crawler nothing the existing `404` does not. Refusals are counted (a click event with `is_bot`, and `linkctrl_redirects_total{outcome="blocked_bot"}`) and deliberately **not audited**: a crawler asking ten thousand times would otherwise write ten thousand audit rows into the table whose growth is alerted on. Enforcement is behind `domains.write` and reaches every link on the instance — **including links served on a verified custom hostname**, which it did not until 0.2.0: the setting was written to the default domain's row alone while a link is served under the policy of the domain it is on, so every link on a custom hostname was unprotected whatever the operator set, and a workspace opened that gap for itself by registering a hostname. The setting now reaches every registered hostname and a hostname registered afterwards inherits it. There is still **no per-hostname bot policy**, deliberately: `domains.write` is a role permission that reaches every organization's owner and admin, so a per-hostname setting under it would let a workspace switch off an enforcement the operator set. Changing one link needs `links.update`. Both changes are audited. Detection is a heuristic and there is **no bypass** — see *What is not defended*. |
| Domain ownership | `domains.write` is an **ownership** check rather than a flat permission: a workspace admin registers, renames and removes their own workspace's hostnames and gets `403` — not `404` — on another workspace's, because being told a hostname is not yours is the true answer and does not invite guessing at ids. Another workspace's hostname is **absent from the list**, not present and unmanageable; the registration is the only thing there is to disclose. **A hostname belongs to exactly one workspace**, enforced instance-wide by `domains_hostname_key` on `lower(hostname)`: a hostname is one alias namespace, so sharing one would mean two workspaces racing for the same alias with nothing to arbitrate. The conflict message names the hostname and never its owner, so registration cannot be used to ask what a neighbour has registered. The instance's **default domain** is not administered here at all. It cannot be renamed or deleted by anybody — the collection refuses that whoever asks, and refuses it before looking at permissions, because it is a fact about the row rather than about the caller — and **since 0.2.0 its root redirect and bot policy need `domains.write.instance`, which reaches a person only through the instance principal**. Until then they needed `domains.write`, which the owner and admin *roles* hold: the default domain is the hostname every workspace's links are served on until it registers its own, so on an instance with more than one organization every owner and admin could repoint it, and under `SIGNUP_MODE=open` one registration was enough. That gap is the reason D38 said an instance-level principal was missing; D98 built one and D100 moved this to it. `domains.write` itself is unchanged and still an ownership check over a workspace's own hostnames. **Nothing is served on a registered hostname until it is verified** — see the row below. Nothing checks that a registrant controls the name at registration, deliberately: that is DNS's question and verification's answer, and a syntax check rejecting a few recognizable names would read as protection while proving nothing. Registering, renaming and removing are audited. |
| Custom-domain verification | **The gap between registered and verified is an alias-namespace hijack, and one column closes it.** A `Host` header reaches the redirect tree only if `domains.verified_at` is set; an unverified or unknown host gets the operational `404`, and never a redirect to the instance's own hostname. **The header is folded before it is matched, and in 0.2.0 it was not folded enough**: a trailing dot — the fully qualified spelling, which SNI does not carry, so an HTTPS handshake completes without it — and an explicit non-default port both missed the verified set. On a split-hostname deployment that failed closed, at the operational `404`. On a **single-hostname** deployment it failed open, because the fall-through there is the instance's own tree: a customer's verified hostname was answered with the dashboard, the API, and short links belonging to the default domain. Both spellings now fold. A non-default port stays significant when matching the instance's *own* hostnames, because a deployment may serve the dashboard and short links on one name and two ports — a cross-host redirect reachable through the alias namespace would be an open redirector for anybody who can create a link. Verification is a **DNS TXT challenge**: sixteen bytes from `crypto/rand`, hex-encoded, published at `_linkctrl-challenge.<hostname>`, under a label a hostname cannot contain so nothing registered here can ever be another name's challenge. The token is unguessable because a predictable one could be published *before* registering, and the record is the whole proof. Hostname-to-domain resolution on the redirect path reads an **in-process set of the verified domains only**, reloaded on a Redis broadcast, so no replica goes on serving a hostname another has just unverified — and an unknown `Host` costs a map lookup rather than a query. A **rename un-verifies**: the published record proves control of the old name and says nothing about the new one, and a fresh token is minted because the old value is published in a zone this workspace may no longer control. Alias uniqueness stays `(domain_id, alias)`, so two verified hostnames are two namespaces; **reserved aliases apply on every host**, because a dashboard route shadowed on a custom domain today is one shadowed everywhere the day the hosts are merged. **The write that sets `verified_at` is predicated on the hostname and the token the check was actually made against**, so a check whose row is renamed while the DNS lookup is in flight affects zero rows and verifies nothing — the registrant runs the nameserver and therefore chooses how long that gap lasts, and without the predicate a check of a name they hold could have landed on a name they do not, this instance's own hosts included. Verifying and un-verifying are audited, the second attributed to `system` when a job did it, and the verification record names the hostname that was **checked** rather than whatever the row said afterwards. |
| Custom-domain re-verification | Re-checked hourly. The **first** failed check notifies the owning workspace and **keeps serving** — a poll against somebody else's nameserver is weak evidence, and building an outage trigger out of one failed query turns an availability feature into an availability incident. **Twenty-four hours** of continuous failure stops serving: `verified_at` cleared, the hostname back to the operational `404` on every replica, a second notification, and an audit record. **On every replica** is carried by two mechanisms rather than one, and it used to be carried by the weaker alone: the invalidation is published over Redis pub/sub, which is at-most-once, so a message lost while the subscription stays healthy is never noticed and an instance with no Redis has no subscriber to lose it. Each replica therefore also re-reads the set on `DOMAIN_VERIFY_INTERVAL`, which bounds how long one that missed a message keeps serving a hostname whose verification is gone (F73). Both numbers are operator configuration (`DOMAIN_VERIFY_INTERVAL`, `DOMAIN_VERIFY_GRACE`), and the cost of the window is stated rather than hidden — for its length this instance serves a hostname whose DNS its owner may already have lost. A successful check at any point clears the clock. **A pass checks serving hostnames before registered-but-unserved ones**, and that ordering is a security property rather than a scheduling preference: what a starved pass loses is not throughput but the stop itself, and registrations — which anybody can create, and whose place in the queue a rename resets to the front — must never be able to delay it. A workspace may register at most **twenty-five** hostnames for the same reason: each is a recurring lookup this instance owes to a nameserver the registrant chose, so an unbounded surface would let one tenant decide how much re-verification everybody else gets. |
| Outbound webhooks | **The one place this product connects to an address a user chose, and it is checked twice.** A webhook URL goes through the *same* tier check every link destination goes through — `Service.checkDestination`, so the scheme allowlist, the unappealable private and obfuscated-literal refusals, the instance blocklist, the heuristics and the opt-in feed all apply, and a refusal is recorded as `destination.blocked` naming the `webhook.url` surface. That check is necessary and **not sufficient**, because a name that resolves publicly at registration can resolve to `169.254.169.254` an hour later. So **the delivery client checks the resolved address at connect time**: the dialer's `Control` hook runs after DNS has answered and before `connect(2)`, on the address the socket is about to open, using the same predicate the unappealable tier uses — there is no window between the check and the syscall for a second answer to arrive in, and every address in a multi-record set is checked because the hook runs per attempt. **No redirect is followed at all**, not merely none to a private address: a receiver answering `302` is pointing this process at a URL nobody registered, so there is no second hop needing a second policy, and the `3xx` is recorded on the delivery. The transport reads no proxy from the environment, because `HTTP_PROXY` would make every address look like the proxy's. The signing secret is 32 bytes from `crypto/rand`, returned once and never readable again; rotation has no overlap window, deliberately, because a window is time a compromised receiver keeps verifying. `webhooks.write` is **not delegable to an API key** — a webhook keeps delivering after the key that created it is revoked, which is reach the credential retains once it is gone — while `webhooks.read` is. Registering, editing, rotating and removing are audited, with the URL stored **defanged**. **What one workspace's unresponsive receiver costs the rest of the instance is bounded to a single attempt**: a claimed batch is dialled together rather than one row after another, so a drain occupies the scheduler for one `LINKCTRL_WEBHOOK_TIMEOUT` however large the backlog is, and a target that accepts connections and never answers cannot hold invitation mail, automation or domain re-verification behind it. That bounds the *time* a drain takes; it is not a statement about how much of the queue one workspace can occupy. |
| Automation rules | **A standing instruction that acts unattended is a credential's reach outliving the credential**, which is what makes `automation.write` non-delegable to an API key — one turn past `webhooks.write`, because a webhook reports and a rule *acts*: it can archive links on a schedule nobody is watching. `automation.read` stays delegable; reading the list escalates nothing. Evaluation runs **only** on the leader-elected scheduler: there is no endpoint that runs a rule, and no package on a request path can reach the evaluator at all — asserted by a test over the import graph, not by convention — so a rule cannot be used to move work onto somebody else's request. Every match query is scoped to the rule's own workspace and so is the archive, and the firing's own audit record carries the match count, so a trigger that saw another tenant's rows is visible rather than merely ineffective. A firing writes an `automation.fired` audit record whose actor is the **rule**, never a person: an automated archive with no trace beside it is indistinguishable from a bug. The archive an automation performs takes **no actor** — a synthetic identity holding `links.delete` would be the scheduler manufacturing authority that `internal/auth` keeps unmintable — and the subject labels a rule reports carry an attempted URL **defanged**, exactly as the audit record stores it. One run is bounded from four sides (100 rules, 25 subjects a rule, 3 actions a rule, 20 rules a workspace), so a workspace cannot make the scheduler's work unbounded, and a rule cannot fire twice for one subject or set another rule off — see `docs/usage.md`. |
| On-demand TLS | The application **never speaks ACME**: no certificate authority is contacted, no account key is held, and no certificate is stored. Its whole part is one unauthenticated endpoint the operator's proxy consults during a handshake, which answers `200` **only for a verified custom hostname** — a wider answer would make the instance an unauthenticated certificate-issuance trigger for any name on the internet, which is the abuse the `ask` mechanism exists to prevent. It reads the same in-process set the router does, performs at most one write per verification, and discloses only whether a name is already being served publicly. It is not for the proxy to expose. |
| Link passwords | Hashed with argon2id at the **same cost parameters an account password gets**, because the hash lands in the same database dump and a cheaper rule for links would be a claim that guessing one matters less. Twelve characters minimum, the account floor. **The hash is never in the cache**: the value the redirect path caches carries a bare `has_password` boolean, so whatever can read Redis cannot walk away with an offline cracking target for every password link on the instance — verification reads the hash from Postgres, on the submit path only, and `TestCachedSnapshotCarriesNoPasswordHash` asserts the serialized payload. Neither the password nor its hash is returned by any API. Verification **issues nothing to the browser** — no cookie, no unlock token, no session — which is what lets a POST exist on a host that has no session middleware and no CSRF check (D53); a forged cross-site submission therefore changes no state, cannot read the opaque `Location` it receives, and at most sends somebody to a destination whose password the forger already knew. That waiver is conditional on the "issues nothing" property and is revisited rather than inherited if anything is ever made to persist. A correct password is answered **`303`, unconditionally** — never the instance's configured redirect status, which may be `307`: a method-preserving redirect would have the browser re-send `password=<secret>` as a POST body to the link's destination, a third-party host the operator does not control. |
| Signed links | HMAC-SHA256 over a version tag, the domain id, the canonical alias and the expiry, keyed by a **per-workspace** secret of 32 bytes from `crypto/rand`, minted lazily on first use and never returned by any API. The expiry is **inside** the MAC, so whoever holds the URL cannot extend it, and the domain id is inside it so a signature minted for one hostname does not verify on another — the id the verifier uses is **the domain the request arrived on**, so a workspace serving the same alias on its own verified hostname and on the instance default has two links and two signatures, and neither opens the other. The minted URL names that same hostname, and a domain whose row cannot be read is an error rather than a fallback to the instance's own host: the default domain is shared across workspaces, so a signed URL on the wrong host can resolve a stranger's link. Refusal is one 403 page for all four causes — no signature, a wrong one, an expired one, a workspace with no key — so it cannot be used as an oracle. The signature parameters are stripped before a link's query forwarding reaches the destination, so the operator of the destination never receives a URL they could replay. Minting requires `links.update`, not `links.read`: a signature is what makes a gated link followable. **Revocation is a column, not a button** — see *What is not defended*. |
| Click budgets | One-time and max-click links are enforced by a **durable Postgres counter consumed in a single statement**, so two concurrent requests for the last click of a one-time link serialize on the row lock and exactly one is redirected. Not `links.click_count`, which is approximate and written asynchronously — gating on it would make a lossy counter an authorization boundary. Not Redis either: the cache is optional by design, and a budget that vanishes with it re-opens every spent link at once. A database that cannot answer produces 503 rather than 410, because 410 is a durable claim crawlers act on and a blip must not retire a live link. HEAD never spends a click, and is refused all the same once there is none left to spend: it is answered from a **non-consuming read** of the same counter, so a link checker can neither destroy a one-time link by asking about it nor be handed the destination of one that is already gone. |
| Errors | Internal error text never reaches a client. An unrecognised error is a flat 500; the cause is in the log, because error strings carry table names and connection strings. |
| Routing rules | A rule's destination is judged by **every tier a link's own destination is**, through the same single door — `Service.checkDestination` — and a source scan fails the build if a new destination-writing surface reaches the validator or the tier judge past it. That check exists because the failure it prevents is quiet: a rule reached only by mobile visitors in one country is still somewhere a browser is sent, and a rule pointing at `169.254.169.254` is the same SSRF as a link pointing at it. Refusals are audited under the surface `link.routing_rule`, so an operator tuning the heuristics sees them alongside every other refusal. Rules are guarded by `links.read` and `links.update` and mint no permission of their own — a routing rule *is* where a link points, and the narrower operation must not need more authority than repointing the link outright. Evaluation cannot resurrect a link: rules run **after** the link's state is decided, so an expired, archived or disabled link is not served by a rule. Region and city are resolved for the length of one redirect and never written; `click_events.region` and `city` stay null, asserted by test. |
| Split testing | A split arm's destination goes through the **same single door** every other destination-writing surface does, and the same source scan fails the build if it stops — an arm receiving 5% of the traffic is still somewhere a browser is sent, and the tiers do not price refusals by traffic share. Refusals are audited under the surface `link.split_variant`. Arms mint no permission of their own: `links.read` to see a split, `links.update` to change one, because an arm *is* where a link points. Selection cannot resurrect a link — it runs after the link's state is decided, like rule evaluation. **A sequential split writes to the database on every visit**, which is a cost an unauthenticated visitor can drive; it is bounded by the same 404-probe and per-address limits every redirect is, lands only on links whose owner asked for it, and a failure answers 503 rather than choosing an arm, so a database under pressure degrades to unavailable rather than to a silently approximate rotation. **Nothing about a visitor is stored to keep them on one arm** — there is no cookie, no identifier and no per-visitor row, which is also why per-visitor consistency is not offered. |
| Folders | Workspace-scoped like everything else, and the scoping is the query rather than a check after it: `GetFolder` and `ListFolders` take the workspace as an argument, so a folder in another one returns no rows instead of a row somebody has to remember to reject. That matters because `links.folder_id` has a foreign key to `folders(id)` and **says nothing about tenancy** — without the lookup, a caller could file their own link into another workspace's folder and inflate a count its readers cannot explain. Folders mint no permission: `links.read` to see the tree, `links.create`, `links.update` and `links.delete` to change it, because a folder is where a link lives in the sense a rule is where it points. They reach the redirect path not at all — `folder_id` is not in the cached snapshot, nothing about filing a link changes where it sends anybody, and a folder carries no setting that could. **Deleting a folder deletes no link**: the schema's `ON DELETE SET NULL` unfiles them and `ON DELETE CASCADE` takes the branch, so tidying a tree cannot destroy content, and an integration test breaks the build if that ever stops being true. |
| QR codes | The drawing is built from integers and from colours already parsed as `#rgb`/`#rrggbb`, and it carries **no title, no `aria-label` and no metadata naming the destination** — so nothing a workspace controls reaches the markup, which is what makes it safe to inline into a dashboard page as trusted HTML. A colour that is not a hex colour is refused rather than escaped, because `red`, `rgb(1,2,3)` and `url(#x)` are all valid paint values and none of them is something this product will write into an attribute. The endpoint is `links.read` — a code is a picture of the link's own short URL, so seeing one is seeing the link — and styling is `links.update`. The response carries `X-Content-Type-Options: nosniff` and a `private` cache policy, because it is workspace data behind an authenticated request. A link in another workspace is a `404`. **Since M49 one of the two picture endpoints rasterises**, which decision D11 had refused: `qr.png` allocates a bitmap on a request, so the size is bounded rather than trusted. Output stops at **2048 pixels**, and the image is two-colour paletted at one byte per pixel — so the largest buffer a request can cause is **4,194,304 bytes**, stated as a number because "bounded" without one is not a bound. *(A code carrying a logo cannot be paletted and is four bytes a pixel, so its figure is 16,777,216 — see the **Uploaded content** row below, which is where that bound and the two on the logo itself are stated together.)* A size above the cap is refused rather than clamped, and a style stored before the cap existed that would draw larger is refused too. The encoder is `image/png` from the Go standard library: no module joined the dependency set for it, asserted by a test on `go.mod`'s require block. `qr.png` also carries `Content-Disposition: attachment` with a **constant** filename — the link's alias is put in the dashboard's `download` attribute instead, so no workspace-controlled string is written into a response header. Neither endpoint is on the redirect path, so no SLO re-verification is owed. |
| Uploaded content | **This product accepts exactly one kind of file — a QR code's logo — and refuses everything else about it.** **PNG and JPEG only, chosen by sniffing the leading bytes**: neither the filename nor the part's declared `Content-Type` is read, asserted by a source scan over the handler as well as by behaviour, so a decoder cannot be selected from outside. **An SVG is refused by name**, because an SVG is a document that carries script, external references and an entity resolver, and accepting one would mean sanitising markup this product then serves; PNG and JPEG decode to pixels, which is a smaller thing to be wrong about. **Every bound is a number, and they are enforced in the order that makes them bounds.** A decompression bomb is a small file declaring an enormous image, so the request body is capped first at **1,048,576 bytes** by `http.MaxBytesReader` — the whole multipart envelope, not just the part — and then `image.DecodeConfig` reads the *header only* and the dimension cap is enforced there: **1024 pixels a side**. Only then is anything decoded. A test feeds a file that passes the first cap and fails the second and asserts the allocation never happened, measured rather than argued. **The figures in this sentence changed at M50.5's 2026-08-12 reopening (F214), and they changed upward.** There were two header caps — the side, and 262,144 pixels in total — and an 813×813 upload passed one and failed the other, which is a refusal naming two numbers with no verdict in it and nothing the reader can do. The area figure stopped refusing anything: it is now the size a stored logo is **resized down to**, keeping its aspect ratio, with both sizes reported to whoever uploaded it. So the side cap is the only header refusal and it is what bounds the decode, at 1024 × 1024 = **1,048,576 pixels**. **A decoded pixel is eight bytes and not four**, which every earlier statement of these figures on this page got wrong in the same way: four bytes is `image.NRGBA`, which is what this product *normalizes* to, while `image/png` hands back `image.NRGBA64` or `image.RGBA64` — eight bytes a pixel — for any bit-depth-16 file, and such a file is ordinary rather than exotic. A 1024×1024 16-bit PNG of one flat colour is about ten kilobytes on the wire, so it clears the body cap by two orders of magnitude and the side cap exactly. Refusing bit depth 16 to keep the smaller figure true was considered and rejected: it would add a refusal to the change whose purpose is to stop refusing what can be adapted, and it would turn away a valid PNG for a property its author cannot see. So the decode bound is 1,048,576 × 8 = **8,388,608 bytes**, four times what the shipped area cap admitted, and it is **measured against a real decode** rather than multiplied out — a test builds that exact file, puts it through the decoder and compares the buffer that comes back. The **image buffers** one upload holds are the upload itself — live across the decode, and inside the same 1,048,576-byte body cap — plus the decoded source, the resampler's own `NRGBA` copy of it and the resampled destination: 1,048,576 + 8,388,608 + 4,194,304 + 1,048,576 = **14,680,064 bytes, 14 MiB exactly**, against 4 MiB before. **The encoder is bounded separately and by the standard library rather than by these caps**, which is why it is stated rather than folded in: the `bytes.Buffer` the PNG is written into grows by doubling, so at its last growth two arrays are live, under 3,145,728 bytes together; and flate's window, its hash tables and the per-scanline buffers are a fixed cost of one `png.Encode`, measured at about 850,000 bytes and the same whatever the image. Under 4 MiB together, so **under 18 MiB in flight per upload**. The handler's own read buffer doubles the same way and does not raise that figure — it reaches its largest size before anything is decoded, and what it hands on is the first term above. That is what accepting a large image instead of refusing it costs, every term is a constant or a measurement rather than an argument, and it is why uploads have a rate limit bucket of their own. **What is stored is a PNG this product re-encoded from the decoded pixels**, never the received bytes — which kills polyglot files, strips every kind of metadata, and means what is served later is bytes this instance produced. The re-encoding is bounded too, at **1,060,000 bytes**, and that bound is checked rather than derived, so a row can never exceed it. **Nothing about what is stored derives from user input.** Under D134 a logo is a `bytea` column on `qr_codes`, so there is no path and no object key to escape: the row is addressed by a uuid and a slug this server generated. **Decoding happens off the redirect path** — it is an authenticated `links.update` write on the dashboard tree, and no SLO re-verification is owed. The decoders are `image/png` and `image/jpeg` from the Go standard library, so no module joined the dependency set. Uploads carry a rate limit of their own (`UPLOAD_RATE_PER_MIN`, default thirty a minute, shared through Redis) **in addition to** the API limit, because an upload's cost is set by its content where a JSON body's is set by its shape. **Three addresses accept a file and all three share one bucket** — the two API `PUT`s (the default code's shorthand and the named-code collection) and the dashboard's upload form on the QR tab *(the popup panel that first carried it folded into the tab at M48's 2026-08-11 reopening; the address and its bucket are unchanged)* — so alternating between the API and a browser does not double the budget, which is the reasoning `login` has carried since it was first shared between the two surfaces. **The accepted cost, stated rather than discovered**: binary lives in the database row and therefore in every `pg_dump`, bounded at 1,060,000 bytes a code and about 20 MiB for a link at the twenty-code cap. **Since M50.6 the stored image is also drawn into the code**, and three things follow. It travels inside the SVG as a **base64 `data:` URI** — the alphabet is `A-Za-z0-9+/=`, which cannot express a quote or an angle bracket, so workspace bytes inside a drawing this product writes still cannot close an attribute or open a tag; the alternative, an endpoint serving the bytes and a reference to it, does not exist and is not wanted. The page's `img-src 'self' data:` is what permits the embedded form, and **a test now pins that directive exactly** rather than checking it contains a substring — before this the CSP tests covered `script-src` and the absence of `unsafe-` only, so widening it to `blob:` or a host would have passed everything. And **the rasteriser's allocation figure changes when a code carries a logo**: the composited image is four bytes a pixel rather than the two-colour paletted one byte, so the largest buffer a request can cause becomes 2048 × 2048 × 4 = **16,777,216 bytes** in place of 4,194,304, with the resampled logo bounded separately at 512 × 512 × 4 = 1,048,576 — the same 262,144 pixels a stored logo is fitted to, which the drawing path re-checks on the way in because that path decodes a *stored* image and a row past it could only have been written by hand. **M50.6's 2026-08-12 reopening put a third buffer beside those two**, and it is stated because the sentence above was complete only while the box was small: the occluded square grew from a fifth of the code's width to **three tenths**, so at the 2048-pixel cap the box is 614 pixels against the 512 the raster is clamped to, and the drawing path now scales that clamped raster **up** to the rectangle the box needs. That one is bounded by the box rather than by the clamp — 614 × 614 × 4 = **1,507,984 bytes**, which is `MaxSize` × the fraction, squared, times four — so the three together are under 19,400,000 and the figure that bounds a request is still the picture's 16,777,216. None of the three is on the redirect path. |
| QR code attribution | `?qrc=` is read on every redirect that already carries a recognised `?src=`, and says which of a link's QR codes was scanned. It has the same primary key underneath it as `?src=` and therefore needs the same bound, but a different kind of one: a code's identity is workspace data, so it cannot be an allowlist. **The value must be one the link itself issued** — checked against the slugs carried in the redirect snapshot, which is data already resolved rather than a query — and anything else is recorded as the link's default code and never stored. So the set of values a visitor can write into `link_dimension_daily` is bounded by `domain.MaxQRCodesPerLink` per link, and every one of them was put there by the workspace. Length and alphabet are checked before the membership scan, so a large value is refused by a comparison. Like `?src=` it is **not** stripped before query forwarding, it is **not evidence**, and nothing authorizes on it. |
| Click-source attribution | `?src=` is read on every redirect and resolved against a **closed vocabulary of one value**. That bound is a defence rather than tidiness: `link_dimension_daily`'s primary key includes the value, so a source anybody could choose would be a row anybody could create — an unauthenticated visitor appending a fresh random `?src=` to a popular link would grow this project's largest table without limit. An unrecognised value attributes nothing and is otherwise ignored. The parameter is **not** stripped before query forwarding, unlike the signature parameters above, and the difference is the point: a signature is a credential and leaking one hands the destination a replayable URL, while a source label is not. It follows that the label is **not evidence** — anybody can type `?src=qr` — and nothing authorizes on it. |
| Campaigns | Workspace-scoped in the query rather than checked after it, exactly as folders are, and for the same reason: `links.campaign_id` has a foreign key to `campaigns(id)` that says nothing about tenancy, so without the lookup a caller could label their link with another workspace's campaign and inflate a count its readers cannot explain. They mint no permission — `links.read` to see the list, `links.create`/`update`/`delete` to change it — and reach the redirect path not at all: `campaign_id` is not in the cached snapshot, and the schedule is **descriptive**, consulted by nothing at redirect time, so no campaign setting can decide where a link sends anybody. **Deleting a campaign deletes no link**: the delete is soft and the unlabelling runs in the same transaction, asserted by an integration test rather than left to a cascade that never fires. |
| Egress | No telemetry, and no third-party calls that were not agreed to by somebody administering this instance. GeoIP is a local file. **This row said *no phone-home in the default configuration* until 0.3.0, and that sentence is edited rather than qualified** — the update check below is a phone-home by any honest reading, it is offered by default (D149), and it does nothing at all until an operator has been asked and said yes (D164). **Five** connections leave this product, enumerated below rather than counted, because this row said *two* until M45 and both of the missing ones were shipped features. What is being counted is *a socket this process opens to a host outside its own deployment*, and the five divide by who decides them. **The operator's three. Two are off until they configure one:** an SMTP relay (`SMTP_HOST`), and a reputation feed (`FEED_URL`) which sends the destinations your users type to a third party — the consequential one, and the one that discloses itself, see *Reputation feeds* below. **The third is the only one an instance turns on by agreeing to a question rather than by being configured:** a daily `GET` to GitHub's releases API asking whether a newer LinkCtrl exists. It carries this server's source address and the running version in the `User-Agent`, and **nothing else** — no instance identifier, no deployment size, no link counts, no configuration, no request body, no query string, no credential; the response is read for a version and discarded. **It is off until somebody is asked and says yes** — on the setup form for a fresh instance, on the dashboard at the first sign-in by an account holding `instance.admin` for one that was upgraded — so an instance nobody administers never makes the request at all. And it is refused three ways, any of which is enough: `LINKCTRL_UPDATE_CHECK=false` on the deployment, *no* at whichever prompt was shown, or simply never answering. The destination is a compile-time constant, not a setting, so there is no value anywhere that redirects it somewhere else. A test compares the outgoing request against an exact expected form and fails when a field is added to it. *No notification is not evidence of up to date*: a throttled or blocked check does nothing and says so only at debug. **A workspace's two, and no operator setting turns either off:** a webhook delivery to a URL an owner or administrator registered, carrying that workspace's link events and the destinations in them (*Outbound webhooks* above); and a DNS `TXT` query for the challenge label under each hostname a workspace has registered, made when it is registered and hourly thereafter (*Custom-domain re-verification* above). **The DNS query is the weakest of the five and is counted anyway.** The socket goes to *this host's own resolver*, the one the operator configured; it is the query rather than the connection that reaches a nameserver the registrant chose, and what that nameserver observes is this instance asking about a name it was given, on a clock. Excluding it would rest the number on which end of a resolver you stand at — and a count arrived at that way is the count that was wrong before. It is also why *Outbound webhooks* can still say webhooks are the one place this product connects to an **address** a user chose: they are, and neither DNS nor the update check is a counter-example to it — the update check's destination is a constant in this repository's source. **Not counted, so the five can be checked:** Postgres and Redis, which are the deployment rather than somewhere outside it, and `linkctrl healthcheck`, which dials this process's own listener. Nothing else opens a socket outwards, and a source scan over the package that judges destinations fails on any outbound-HTTP or name-resolution symbol, so a later "just check the host resolves" cannot become undisclosed egress. That scan is untouched by the update check, which lives in `internal/update` and is not visible to it, so nothing was added to its allowlist to let this land. **Since 0.3.0 a second scan covers the authentication path** — `internal/auth` and `internal/httpx`, in the same shape and with the same symbol list. It exists because the second factor is exactly the kind of feature that arrives with a verification service attached: TOTP is arithmetic over a clock and a shared secret, so there is nobody to ask, and the count above did not move when it landed. The two scans are separate because the first reads its own directory and could not have seen either of these packages, which is a gap the milestone found in its own draft rather than in the tree. |
| Add-on loading | **Off unless `LINKCTRL_ADDONS_DIR` is set**, and off means no WASM runtime is constructed at all. When it is set, every module is verified against the `sha256` in its manifest **before it is compiled**, so a module whose bytes are not the bytes the manifest describes is never parsed, let alone instantiated. The manifest's `module` field must be a bare filename and a separator or a `..` is **refused rather than cleaned**, so a manifest cannot name a path outside its own directory. Unknown manifest fields are refused, because `schema_version` is checked for equality and a field this host does not know means a file written for a schema it does not implement. **The manifest is the trust root and nothing hashes it**, so the host is held to reading it the way a person does: a key must be spelled exactly as documented and must appear once, at every level of the document. That is a boundary rather than strictness, and it was measured — a JSON parser keeps the **last** occurrence of a repeated key and binds a case-variant spelling to the field anyway, so `{"permissions": ["session.mint"], "permissions": []}` loaded with no grants and the same pair in the other order loaded with all of them, while `"SCHEMA_VERSION": 7` beside `"schema_version": 1` loaded as schema 1. A publisher could write a file whose readable text said one thing to a reviewer and another to the host; both spellings are now refused and the error names the documented one. A module is instantiated with **no capability granted**: no filesystem is preopened, no environment or arguments are passed, its stdout and stderr are discarded, and the clock and random source it sees are the runtime's fake ones rather than this machine's. The imports it may resolve are the **published ABI** and WASI preview 1, whose capabilities the host leaves empty — no preopened file, no environment, no arguments, discarded output, no socket — apart from the fake clock and random source named below, which is the row below. What an add-on will not load means is the **add-on's** declaration and not the operator's: `required` stops the instance with the reason, `degrade` lets it serve without the module, and a manifest that cannot be parsed stops the instance too — there is no class to honour, and assuming the forgiving one would boot an instance with an authentication add-on silently missing. **A module that never finishes loading is a failure like any other**, which it was not until this was measured: package initialization runs during instantiation, so a module that loops there returns nothing, and with no deadline on the context the load ran under the instance never reached its listener — no log line, no metric, no error, and an operator's only remedy a second signal. Compiling and starting each add-on's module are now bounded at 30 seconds **each**, so one add-on's guest execution is at most 60 seconds and an instance's bring-up window is that times the number of add-ons that hang; expiry is counted as `linkctrl_addon_loads_total{outcome="load_timeout"}` and named in the log, and the class the manifest declared decides what happens next. Starting is interrupted wherever it has got to; **compiling is checked between the module's functions**, so a single function that is pathologically slow to compile overshoots by however long that one function takes — the bound is real and its granularity is stated rather than rounded up. The budget is **per add-on** rather than for the directory, because a shared one, once spent, would refuse every add-on behind the slow one — turning a `degrade` module's hang into a `required` module's refusal to boot. It bounds the add-on's own code and not the host's work with the database: an add-on's migrations wait on the migration lock for as long as this product's own do, which is what stops the bound turning a replica that arrives mid-migration into a crash loop. |
| Add-on ABI | **An add-on reaches this product through an enumerated set of imports and through nothing else** — no socket, no file, no shared table, no environment — so the whole of what an add-on can do is one list, in `internal/addon/abi`, published as [addon-abi.md](addon-abi.md) and as a generated SDK. Eight of those functions do anything today; the rest are declared and answer a refusal, so the contract is complete before the behaviour is. **No function hands a module a client's address in any form.** That is a property of the surface rather than a promise about add-on code: the record carrying redirect data is bound to what `click_events` may carry — country-level, and no address column exists to carry — and a test reads the column list out of the migration to prove the bound, because this project cannot review the source of an add-on it did not write. An add-on cannot store what it is never handed. Region and city are refused as well, though the table has both columns: they resolve transiently and are never stored, and a module with a schema of its own is exactly where they would stop being transient. **No function hands a module a credential of this instance's either**, and that is the same kind of property: sessions here are server-side and opaque, so the `Cookie` header *is* the session, and an add-on serving a route is handed only the cookies whose names begin with one of the `cookie_prefixes` its manifest declares — each of which must begin with the add-on's own name, so no prefix an add-on may declare reaches `linkctrl_session` in either spelling. It bounds what an add-on may *set* as well, because a cookie it may not read is one it must not overwrite. **Beginning with its own name does not on its own keep one add-on out of another's namespace**, and that was measured rather than reasoned about: add-on `oidc` may declare the prefix `oidc_x`, which begins with `oidc_` and is a prefix of every prefix add-on `oidc_x` is allowed to declare, so with both installed the first read and overwrote the second's state cookie. The same relation makes `LINKCTRL_ADDON_OIDC_X_KEY` two add-ons' setting at once. So the rule has a second half at load: two installed add-ons whose names stand in a `name + "_"` prefix relation are **both** refused — neither is awarded the other's namespace and neither wins by loading first — counted as `linkctrl_addon_loads_total{outcome="name_collision"}`, with the pair named in the boot log and the instance stopping if either is `required`. The route prefix and the Postgres schema were never exposed to it: each uses the whole name as one segment, with nothing joined onto it. The one payload the host itself composes — what comes back when a module's authentication claim is accepted — is enumerated for the same reason a list of forbidden names is not enough on its own: it carries when the session expires and whether a second factor is still owed, and no token, no cookie and no row of the sessions table, so the release that implements it cannot quietly widen it without the change being visible on the published surface. An add-on that genuinely needs some other cookie of the host's cannot have it, which is accepted: the alternative is a trust model in which an installed add-on can impersonate any signed-in user, and that would have to be stated here rather than implied. A single value crossing into the host is bounded at 64 KiB, so a length is not a way to make this process allocate a gigabyte, and a module's only route to the operator's log is the `log` function, attributed to the add-on. What the ABI does **not** hand over is also worth knowing: no real clock and no real random source, because those were never granted and no function asks for them yet. |
| Add-on permissions | **What an add-on may call is what its manifest declared, and the host enforces it rather than trusting the module.** Every function in the ABI names the permission it costs; the host resolves a manifest's declarations against a **closed** seven-token vocabulary at load and refuses any call whose grant is not held, with a status the module can branch on and a `linkctrl_addon_refusals_total{addon,permission}` counter an operator can alert on. The check is in the host's dispatch and not in a function's body, so a capability cannot arrive with its check somewhere else, and it runs **before** the host says whether it implements the function at all — an add-on that declared nothing cannot use the ABI's availability status to enumerate what this build can do. A token outside the vocabulary refuses the add-on at load, for the reason an unknown manifest field does. A token the vocabulary carries and this build grants to nobody — `redirect.inline`, until an add-on may run inside the redirect path — is declarable, is never held, and every call behind it is refused; the boot log names it and `linkctrl_addon_info` does not, because that label carries what a module holds rather than what it asked for. Two functions cost nothing and are ungated deliberately: reporting the ABI version, and writing a line to the log, which is the one capability granted on purpose. **Ungated is not trusted**: because every loaded module can reach the log, including one that declared nothing, the host neutralizes the message before the line is written and bounds it at 4 KiB, so a module can neither forge a record that reads as this product's own nor arrange bytes in an operator's log to be overlooked. **The neutralization is stated as what survives, not as a list of what is caught** — a **graphic** character reaches the line as itself, meaning every letter, mark, digit, punctuation mark, symbol and space in every script — with the one exception named below — and everything else becomes its escape: a newline, a control character, an ANSI escape, every format and bidirectional control, every unassigned or private-use code point, and the few characters that are graphic by category and render as nothing. That inversion is deliberate and it replaced an enumeration (D242). No release ever carried the enumerated form this replaced, and it was already incomplete when written — it missed `U+061C ARABIC LETTER MARK`, an invisible bidirectional control from the same Unicode revision as the isolates it did cover, along with the interlinear annotation controls that hide the run they wrap; a list of invisible characters is behind the next revision by construction, so it cannot support a claim like this one. **The exceptions now run the other way and are named**: Unicode's `Prepended_Concatenation_Mark` property — `U+0600`–`U+0605`, `U+06DD`, `U+070F`, `U+0890`, `U+0891`, `U+08E2`, `U+110BD` and `U+110CD` today, the Arabic, Syriac and Kaithi signs that scope the digits following them — is left alone, because a boundary that mangles Arabic is a worse defect than the one it prevents. **It is read from Unicode's own property and not transcribed from it** (D243): a hand-copied form, also never released, carried eleven of the property's thirteen members, so `U+0890 ARABIC POUND MARK` and `U+0891 ARABIC PIASTRE MARK` were escaped from the day it was written — the same staleness the enumeration had, in the allowlist that replaced it. **One graphic character is escaped the other way**: a backslash is doubled (D244), because the two characters `\` and `n` otherwise reached the line as the bytes an escaped newline reached it as, and because that is what makes the truncation mark `…\(truncated)` something a module cannot spell — a log read as evidence may not leave a reader unable to tell what was written from what the host did to it. **What the inversion costs** is legibility rather than safety, and it is stated because it is real: a code point assigned by a Unicode revision newer than the host's Go toolchain is not a graphic character to that host, so it is escaped until the host is rebuilt. The set is asserted from Unicode's own categories by test rather than from a second copy of the list. It is done at the host function rather than in the logger because there is no second boundary on that path, and it refuses nothing: a message that needed neutralizing is still logged. **A grant is held, never inferred** — nothing about a module's name, its code, its other declarations or its install order widens it — and it is conferred by the operator's act of installing a module whose manifest asks for it, not by a role. That makes it a parallel to `NonDelegableScopes` rather than a use of it: the two are separate mechanisms over separate principals, and the reasoning is in [build-notes/decisions.md](build-notes/decisions.md). |
| Add-on storage | **An add-on that asked for tables gets a Postgres schema of its own and a database role that reaches none of this product's data.** *Reaches nothing else* is what this row said until the two paragraphs at its end, which are there because both were measured rather than reasoned about: a confined role can still create a **large object** and can still take one of this product's **advisory locks**, and neither is a read of a product row. The schema is `addon_<name>` and the role has the same name, both derived from the add-on's name rather than recorded anywhere, so two add-ons cannot contend for one schema — the directory *is* the name, and a manifest that disagrees with its directory is refused. **The boundary is privileges, not the search path**, and the distinction is the whole of it: a search path decides where an *unqualified* name resolves and is never consulted for `public.links`, so it confines nothing on its own. The role holds no privilege on any table outside its own schema, so a schema-qualified read of this product's tables is refused by Postgres. **Privileges only bind if the session is that role**, which is why the host opens a second connection pool per add-on, authenticated as the add-on's role with a credential generated at every boot and stored nowhere. **A second replica booting rotates that credential**, since each mints its own, so a replica whose credential has been rotated out from under it mints a new one on its next connection and logs that it did — without which every replica but the last to boot would lose add-on storage quietly, the failure being a connection refusal and not a privilege one. `SET ROLE` on the application's own connection was tried, measured against Postgres 17 and rejected as a boundary: a single `DO $$ BEGIN EXECUTE 'RESET ROLE'; … END $$` escapes it, and `SET SESSION AUTHORIZATION` is checked against the *session* user rather than the current role, so it succeeds whenever the application connects as a superuser — which the shipped compose file does. Authenticated as the role, both are refused. One statement per call, because the host parses through Postgres's extended protocol; a read runs in a `READ ONLY` transaction, so the ABI's read function cannot write; the search path and a five-second statement timeout are pinned per transaction, so nothing a previous statement left on a pooled connection changes what the next one means. **Migrations are the host's and are third-party DDL the operator did not write**, and two things bound that together: the manifest names every `.sql` file with its digest, so the DDL that runs is the *add-on author's* rather than whatever is on disk — a file the manifest does not list refuses the add-on, and so does one whose bytes disagree — and the migrations are applied *as the add-on's own role*, so DDL naming another schema is refused by the database and a `SECURITY DEFINER` function the DDL creates is owned by a role that can reach nothing. The host then asks Postgres itself three questions and refuses the add-on if any answer is not empty: what does this role own that is not in its own schema, what is in its schema that this role does not own, and what has it granted on that schema to anybody but itself. The first two are each a set difference over the catalogue Postgres's own `DROP` consults — `pg_shdepend` for `DROP OWNED BY`, `pg_depend` for `DROP SCHEMA` — and asking a shape rather than checking a list of places is deliberate: three earlier versions of this check enumerated the places an add-on might own something and each was missing one, the last being a temporary relation that the exclusion added for TOAST also hid. **The third question is there because an add-on can give its own schema away and this document once said it could not**: a grant is not an object, so neither catalogue above records one, and two ordinary statements from the confined role — `GRANT USAGE ON SCHEMA … TO PUBLIC` and a `GRANT SELECT` beside it — let another add-on read those tables while both set differences stayed empty (measured; D255). Nothing prevents it, because ownership carries the right to grant and Postgres offers no way to withhold that from an owner; what the host does is read `pg_namespace.nspacl` and `pg_class.relacl` at every load, name any grantee that is not the add-on's own role, and refuse to run the module until it is revoked. The harm is bounded to what the granting add-on chose to give, which is its own data. It cuts the other way too: a grant *you* made — a reporting role with `SELECT` on an add-on's schema — stops that add-on loading, and [operations.md](operations.md) carries the remedy. The second question exists because `pg_dump` carries no roles, so a restore into a cluster whose roles were not restored separately leaves an add-on's tables owned by the application, which no amount of asking what the *role* owns will find. An add-on's migration state is a goose table inside its own schema, so a re-load applies nothing twice and an add-on's data has no half in a table this product owns. **Nothing caps how large that schema may grow** — the same answer the audit log gets — and `linkctrl_addon_schema_bytes{addon}` is what makes the *stored* growth visible. **That gauge kept a denylist of its own until 2026-08-19**, which is worth stating here because it is the same defect as the enumeration above, left standing one function away from where the argument against it was made: it summed ordinary and materialized tables, so a **sequence** — `relkind 'S'`, in the add-on's own schema, 8192 bytes from creation, and outside `pg_total_relation_size` of the table that owns it — was not counted. 24,000 of them moved `pg_database_size` by 188 MB with the gauge reading 0. It now sums every relation kind in the schema except the ones already counted inside another, measured as an identity against a second decomposition of the same bytes. Nor was it only an adversary's case: the migration table the host itself creates in that schema declares an identity column, so every storage add-on has always held 8192 bytes this number reported as nothing. **What the post-condition and the two gauges between them cover is every kind of object Postgres catalogues, which is not the same as every way an add-on can use disk**, and the qualifier is stated because the argument for asking a shape rather than keeping a list would otherwise be overclaimed: a `WITH HOLD` cursor materialized at commit holds a temporary **file** for the life of the session — 553 MB measured for one cursor inside the five-second statement timeout — and it is an object in neither catalogue, so the post-condition is empty and both gauges read zero while it sits on disk. It is transient rather than stored data, which is why nothing here reports it and why it is a residual rather than a hole in the confinement; the bound that would close it, `temp_file_limit` on the add-on's role, needs a superuser to set, which is the same conditionality as the two narrowings below. **Two things the role can put outside its schema are accounted for rather than prevented, and it is the post-condition above that accounts for them** — a list of them here would be the same denylist the check stopped keeping. A **large object** is the first: `EXECUTE` on `lo_from_bytea` belongs to `PUBLIC`, Postgres has no per-role deny, and revoking it needs ownership of a `pg_catalog` function — which the application's role does not have, and where the role is not superuser the `REVOKE` is a **silent no-op** rather than an error, so shipping one would be a control that does nothing on the deployments this product documents. 40 MB in a single statement was measured, from the write function only; the read function's `READ ONLY` transaction refuses it. What holds instead: `linkctrl_addon_large_objects{addon}` publishes the count, the load post-condition refuses an add-on that owns one, and `docs/operations.md`'s purge carries the `DROP OWNED BY` that drops it — the schema drop alone does not, and `DROP ROLE` then fails. A **temporary table** is the second, and it is *narrowed* rather than only accounted for: `PUBLIC` holds `TEMPORARY` on a database by default, a pooled connection is not a fresh session so the relation survives across calls, and 51 MB went into one inside a single five-second statement. Installing a storage add-on now revokes `TEMPORARY` on the database from `PUBLIC` and grants it back to the application, after which `CREATE TEMP TABLE`, `CREATE TABLE pg_temp.x`, `CREATE TEMPORARY TABLE … AS`, `CREATE UNLOGGED TABLE pg_temp.x` and `SELECT … INTO TEMP` are each refused — measured, all five. **That revoke is a narrowing and not a boundary**, for three measured reasons: it does nothing but print a warning unless the application owns the database, which this document asks for `CREATEROLE` and not ownership; no dump carries a database privilege, so it is gone after any restore; and it changes the database for every role, so an operator sharing this database with another application that uses temporary tables loses them — [deployment.md](deployment.md) states that. A per-role revoke is not available, because the privilege arrives through `PUBLIC` and Postgres has no per-role deny. **A third narrowing is the only one in this family conditional on neither superuser nor database ownership, and it is why the family is worth naming as one**: the confining role may `ALTER ROLE CURRENT_USER SET` any user-settable parameter on itself — `work_mem = '4GB'` was accepted, landed in `rolconfig`, and a fresh connection read it back, so one read inside the five-second timeout peaked a backend at 1.37 GB resident against 31 MB for the same query at the 4 MB default, which spills to disk instead — and there is no per-role deny for that either. So every load now clears the role's settings before pinning its search path, which needs only the `CREATEROLE` this document already asks for and survives a restore, unlike the two above. It is not an escalation route: `NOLOGIN`, `CONNECTION LIMIT 0` and `temp_file_limit` are each refused to the role, and the two settings that would otherwise matter — the search path and the statement timeout — are overridden per transaction by the host. One thing the role *can* change about itself is its **password**, which Postgres allows every role and forbids to nobody: it is accepted through the write path, it outlives the process in `pg_authid`, and [F280](build-notes/deferred-findings.md#open) is what it is worth. So the post-condition is what holds here too, and it reports a temporary relation whether the revoke took or not. **The role can also take one of this product's job advisory locks**, the keys being constants in a public repository and `pg_advisory_lock` being executable by any role — the product's own migrations use the same functions, so revoking that family is not available either. A session-level lock survives the rollback both storage paths perform, so the host releases every advisory lock a connection holds before that connection is reused, synchronously. The residual is one statement's worth: an add-on can hold a job's lock for up to the five-second statement timeout and retake it, which makes a job skip ticks — the same outcome as a follower losing an election, and the same class of nuisance as an add-on holding its four connections, which it may also do. |
| Add-on pages | **An add-on holding `routes.own_prefix` answers requests under `/addons/<name>/` on the dashboard host, and it does not write the HTML.** That is the load-bearing claim of this row and it is a property of the shape rather than of a filter: what a module returns is **text**, the content types it may name for itself are a closed pair — `text/plain` and `application/json`, neither of which a browser executes and both served with `nosniff` — and the default is that LinkCtrl wraps that text in its own page template, where it is escaped like every other value on every other page. There is no path by which a module's bytes reach a browser as markup, so `text/html` is **refused** at the moment the module writes it rather than sanitized afterwards, and the `Content-Security-Policy` is byte-identical to what every other page carries: a test holds the served header against a literal copy of the policy as it stood before add-ons could draw a page, because comparing it against the constant would agree with whatever the constant says. A module that answers with a script tag, an inline handler or an external reference puts the characters of one on the screen, and that is asserted against a real module which tries all three. The prefix is on the **application tree only** — the link host serves short links and nothing else, and the redirect tree's tripwire tests pass unmodified — and the prefix itself is in the reserved-word list, so no alias can shadow an add-on's pages. **Those pages are reachable without a session, deliberately**, because an add-on that authenticates somebody is answering a request from a person who has none yet; what it may learn about who *is* signed in is the separate `session.context` grant, whose record carries an account, an email, a display name, a workspace, an organization and a role, and carries no cookie, no token and no session identifier — so an add-on can draw a page for the person in front of it and cannot act as them anywhere else. **Three consequences are accepted rather than prevented, and they are stated because the directory is a trust boundary and this is part of what installing a module costs.** An add-on holding this grant can redirect a visitor **anywhere**, since sending somebody to an identity provider is the whole point of one; the host enforces that the redirect is never permanent, that a `Location` carrying a control character or a scheme-relative `//host` is refused, that a bare 3xx with no location is refused, and that a redirect carries no body and no content type. An add-on's route runs code on an **unauthenticated** surface, which is why the number of add-on requests in flight is bounded at sixteen across the instance and a further one waits on the request's own deadline: each holds about 2.4 MB of guest memory, because a request gets its own instance of the module, and unbounded that is memory exhaustion with no session to rate-limit against. **A request that would not fit the boundary is refused as the client's error, not the add-on's**: the host encodes the request record before it instantiates anything and answers **413** when that record exceeds the ABI's single-value bound of 64 KiB, so a large POST cannot reach a module that would then be unable to answer about it — which would have read in the log as the add-on failing. The ceiling on a body is therefore a function of its bytes rather than a number an operator can configure: the envelope sits beside it in the record, a body that is not UTF-8 is base64 first, and a control character costs six bytes inside the encoding. And a cookie an add-on sets is bounded the way one it reads is — the declared prefixes — with the path scoped to the add-on's own prefix and `Secure`, `HttpOnly` and `SameSite` applied by the host, so a module cannot opt out of them or widen its namespace to the origin. **An add-on's configured settings are read from the environment** as `LINKCTRL_ADDON_<NAME>_<SETTING>`, only for the settings its manifest declares, and every value is held in the type that refuses to print itself — whatever the manifest called the setting — so an add-on's client secret cannot reach a log through a line about the add-on. The boot log says how many settings are configured and never which. |

## What is not defended

The list that matters. Each of these is a decision with a consequence, not an
oversight, and each is also in [Plan.md](../Plan.md#known-limitations) with the
trade-off that produced it.

**What erasure still leaves is one queued message and one outstanding offer.**
Two residues that this document described until 0.3.0 are gone, and they are
named because a reader who checked this page against an older release should be
able to tell which applied when.

Since 0.3.0 the sweep scrubs six things, not two: the *actor* snapshot
(`audit_logs.actor_label`, and the two label columns in `destination_disputes`,
both of them, whichever accounts a batch happens to hold), the
`"email"` key inside `audit_logs.metadata` — **seven writers, counted against the
tree rather than recalled**, covering the invitation lifecycle, the three
membership actions and the two instance-level ones —
([F177](build-notes/deferred-findings.md#closed)), the `"from"` array beside it
([F189](build-notes/deferred-findings.md#closed)), the address on the
invitations the erased account **redeemed**
([F181](build-notes/deferred-findings.md#closed)), and the inviter's own
notification ([F188](build-notes/deferred-findings.md#closed)). The invitation
one mattered most and was the smallest change: `/invites` lists every invitation
an organization ever issued, redeemed included, so an account deleted, erased and
tombstoned everywhere else was still named in full on an ordinary dashboard page
— reachable by whatever role sees invitations rather than by `audit.read`, and
expired by no setting at all.

The last two are the shapes the first count missed rather than tables it did not
know about, and they are named separately because each needed a different
predicate. `instance.principal_moved` writes the outgoing principals' addresses
as a jsonb **array**, and a scalar comparison reaches nothing inside a list; the
array is rewritten element by element rather than dropped, because how many
principals the role moved away from is part of what the record says. The
notification is a different **table** — `notifications`, written to whoever sent
the invitation this account accepted — so deleting the erased account's own
notifications never came near it, and nothing sweeps notifications by age. Both
its detail and its title carry the address, and both are scrubbed: the title is
what a reader actually sees on `/notifications`, so reaching the jsonb key alone
would have left the sentence naming the person in full.

Two things are deliberately not scrubbed, and both are choices rather than gaps:

- **An outstanding invitation addressed to the same text.** It is an offer to an
  address, not a record of a person; the address became reusable the moment the
  account was deleted, and blanking it would break the redemption comparison for
  whoever takes it next.
- **`mail_outbox.recipient`**, on messages that have not yet been purged. It is
  bounded — `PurgeFinishedMail` removes finished rows on the retention schedule
  — and it is the queue rather than a record, but on an instance whose relay is
  down it can outlive the account.

The surviving `user_id` is unchanged and is the point of the design: erasure keeps
the row so the audit trail stays readable, which is why this page says the residue
identifies nobody *from inside this instance* rather than that it is anonymous.
The scrub is value-matched, so it costs a sequential scan of a partitioned table
once per batch — and only when there is a batch, which is what keeps an idle pass
free.

**No account recovery on an instance with no mailer.** Recovery exists as of
0.3.0 and is delivered by email — see *Account recovery* in the table above — so
with `SMTP_HOST` unset there is no route back into an account whose password was
lost. **The product refuses out loud rather than degrading**, which is the one
place it breaks the pattern every other mailer consumer follows: the sign-in page
draws no *forgot your password?* link, `/forgot` answers with the reason in place
of a form, and the API answers `503 no-mailer` naming the operator's route. That
is deliberate, because the mail *is* the mechanism here — an invitation still has
a copyable link and a notification is still in the inbox, while a reset request
that succeeded into a void would leave somebody waiting for a message nobody
queued.

`SMTP_HOST` unset is the shipped default, so this is the state a fresh instance
is in. What is left for it is what was there before recovery existed: the
operator, with database access, rewriting an argon2 hash — and, for the one
account that administers the box, `lctl instance principal move`.

Two costs of the mechanism itself are accepted rather than defended. It **mails
addresses that never registered**, which is what an attacker gets for free from
the identical-response stance; the login limiter is the only thing bounding a
sweep, and it is shared with sign-in. And a **pending** outbox row holds a live
reset token in clear until it is delivered — bounded by the one-hour token
lifetime rather than by the outbox's retention window, which is the same
qualifier invitations and verification links carry.

**DNS rebinding, on the redirect path.** Destination validation refuses private
address *literals*, but a hostname that resolves to a public address when the link
is created can resolve to a private one when a visitor follows it. Catching that
needs resolution on the redirect hot path, which the latency target cannot afford,
or an egress policy outside this process. If the instance runs somewhere with
reachable internal services, put the egress control in the network, not here.

**This is a statement about where a *visitor's browser* is sent, and it does not
extend to fetches this server makes itself.** The two are not the same question:
a redirect leaves the visitor's network and its response never touches this
instance, while a webhook leaves the instance's own network from inside whatever
that network can reach. `169.254.169.254` means something very different in each
case. The posture for server-initiated fetches is in *What is defended*, above,
and it is stated there rather than inherited from this paragraph.

**A human blocked as a bot cannot get through.** Bot blocking decides with the
same coarse user-agent heuristic the click statistics use: it treats a missing
user agent as automated and matches substrings including `preview`, `monitor`
and `checker`. Those were written to bucket a number, and their false-positive
rate has never been measured because until now nothing depended on it. There is
no challenge page, no allowlist and no appeal — a misclassified person gets a
403, and the link's owner is not told it happened. **No bypass is built and none
is scheduled.** This paragraph promised one as a Phase 3 milestone until 0.3.0;
Phase 3 declined the redirect-path work area, so the promise loses its phase
number rather than moving to the next one. What made it a milestone rather than
a bullet is unchanged and is why it keeps being deferred: a challenge is a
rendered, stateful, interactive surface on the one tree this product keeps free
of session lookups and templates. Until it exists, switching blocking on is a
decision to accept
that cost, which is why the default is off and why enforcing it for a whole
domain is behind a separate permission.

**Rate limits fail open, and only the 404-probe limiter is still per
instance.** The credential, API, link-password and blocked-audit limits are
shared across replicas through Redis since 0.2.0,
so the configured number is the number whatever the replica count — until Redis
stops answering, at which point each replica falls back to its own in-memory
bucket and the limit becomes per instance again, which is the fail-open direction
the cache-is-optional rule requires. The **404-probe** limiter is per instance
permanently and deliberately: it guards the redirect path, where a Redis round
trip would cost more than the limit saves. For anything per-instance, N replicas
allow N times the configured limit and a restart resets them. When the
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

**With add-ons installed, that endpoint also names them.**
`linkctrl_addon_info{addon,version,abi_version,failure_class,permissions}`
publishes each loaded add-on's name, version and the capabilities it holds, and
`linkctrl_addon_loads_total` names the ones that were refused. That is an
inventory, and the `permissions` label makes it a sharper one: it tells a reader
which extensions this instance runs, at which versions, and which of them can mint
a session or reach the redirect path — which is the list somebody looking for a
known vulnerability wants. It is deliberate — an operator whose add-on is quietly
not there needs a series that says so — and it is one more reason the paragraph
above is a requirement rather than advice.

**An add-on with a schema can read this product's catalogue, and not its rows.**
The role confining an add-on to its own schema still reaches `pg_catalog` and
`information_schema`, because Postgres does not make those revocable. So a module
that asked for tables can enumerate every schema, table, column, index and
constraint this instance has, including the names of tables it may not read a byte
of. It cannot read a byte of one: privileges refuse a qualified read, and the
adversarial test suite for that boundary reaches for the product's tables eleven
ways. The exposure is a schema map rather than data, it is the same map anybody
with a copy of this repository has, and the alternative — a separate database per
add-on, or a per-add-on connection through a proxy that filters the catalogue — was
not worth the operational surface. It is stated because "confined to its own
schema" would otherwise read as more than it is.

**Each add-on's database credential is generated at boot and may reach the
database's own log.** The host issues a fresh password for an add-on's role every
time it loads one and stores it nowhere, so there is no secret for an operator to
manage and nothing survives the process. The statement that sets it is
`ALTER ROLE … PASSWORD '…'`, and a Postgres configured with `log_statement = ddl`
or `all` writes that statement — password included — into its own log. The
credential is replaced at the next restart, so the window is bounded; if your
database log is readable by people your add-on's data is not, set `log_statement`
accordingly. **What it is worth is more than that add-on's schema**, and the
earlier wording understated it: a session authenticated as an add-on's role also
holds `EXECUTE` on the advisory-lock functions, which Postgres grants to `PUBLIC`
and this product cannot revoke, and the lock keys are compile-time constants in a
public repository. The host releases a lock the add-on took before it reuses the
connection, which bounds the hold to one statement **for connections through the
host's own pool** and governs nothing about a session opened from outside it. An
add-on can also set a password it knows on its own role — Postgres lets any role
change its own, and that one persists in `pg_authid` across shutdown until the next
load re-mints it. Both need network reach to Postgres with password authentication,
which the documented deployment does not give;
[F280](build-notes/deferred-findings.md#open) carries it.

**An add-on's migration failure stops a `required` instance, and that is the
design.** A bad add-on release can hold an instance down: migrations run before
the listener opens, and a `required`-class add-on whose DDL will not apply is an
exit with the reason rather than a boot without the module. The alternative is an
instance serving with an authentication provider's tables missing. Recovery is an
operator's: remove the module's directory, or change nothing and roll the add-on
back — [operations.md](operations.md) has the surface.

**Removing an add-on does not remove its data.** A module whose directory is
deleted leaves its schema, its tables and its rows exactly where they were, and
the next boot names them: the host enumerates `addon_*` schemas nothing claims and
warns. Nothing in this release deletes one, deliberately — a purge is an
operator's explicit act — so an add-on you uninstalled is still holding disk, and
still holding whatever it stored, until you purge it yourself. Dropping the schema
is not the whole of that: a large object the add-on created belongs to no schema,
so `docs/operations.md`'s purge is three statements and the middle one is
`DROP OWNED BY`, without which `DROP ROLE` fails and the disk stays allocated.

**A restore has to carry roles.** `pg_dump` carries none — that is
`pg_dumpall --roles-only` — so restoring into a cluster whose roles were not
restored separately leaves every add-on's tables owned by the application while the
next boot repairs only the *schema*'s owner. The add-on's own role is then refused
on its own rows: a `required` add-on stops the instance and a `degrade` one serves
with every storage call failing. The load post-condition asks that direction, so the
failure names the tables instead of arriving from inside a migration, and
[deployment.md](deployment.md) carries the roles dump and the restore order.

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
notification to the reviewers, who allow it (the blocklist entry is deleted) or
uphold it. Both decisions are audit events and both notify the person who asked.

**A filing notifies the reviewers and nobody else, and that is the bound on it.**
Filing costs only the permission that would have let you create the link, the
route carries no rate limiter, and a refusal computed from the URL rather than
matched against a list row is bounded by the string typed — so the number of
filings one account can produce is not usefully limited. What is limited is the
cost of each: before 0.2.0 every filing wrote one notification per organization
owner on the instance, which on an open-signup instance is a number a stranger
grows by registering. Since 0.2.0 it is the holders of `destinations.review`, a
set the instance principal chose. Neither rate-limiting the route nor capping the
filer would have touched that, because the multiplier was the recipient list.

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
- **The queue names the entry the decision acts on, and it cannot move.** The
  runtime list matches on label boundaries, so a refusal at `login.evil.example`
  comes from the row that says `evil.example` — and that row is what allowing
  deletes, for every workspace on the instance. Each dispute records the entry
  when it is filed and the page renders it beside the host, so the string you
  read is the string the button removes and a row added while the dispute waits
  cannot retarget your decision. Disputes filed before 0.2.0 carry no entry and
  refuse to be allowed at all.
- **The unappealable and high-confidence tiers have no dispute path**, in either
  direction: filing one answers `422 not_disputable`, and a decision can only
  ever delete a `blocked_destinations` row.

**Who can review, and the reach that comes with it.** The queue is instance-wide,
because the blocklist it argues with is: a decision here changes what every
workspace on the instance may link to. Since 0.2.0 the permissions that reach it
are held **instance-wide by named people and by no organization role**.

`destinations.review` reads the queue; `destinations.decide` acts on one. Both
are conferred on the account that claimed the instance through
`POST /api/v1/auth/setup`, which is the only account in this product with a claim
to the box rather than to a tenant. That account holds `instance.admin` and may
confer instance-level review on other accounts, at `/disputes` or through
`POST /api/v1/instance/reviewers`. **A reviewer it appoints cannot appoint
anybody else**, and cannot read the roster: `instance.admin` is not among the
scopes a grant confers, so the set of people who may delegate cannot grow.

Nothing in the product moves `instance.admin`, and that leaves the operator with
the one case the product cannot answer: the founding account is unreachable — a
colleague who has left, or a lost password on an instance with no mailer, where
the reset above has nothing to send with. `lctl instance principal move` is the
answer, and it is a command
rather than a page because its authority is filesystem access to the box, which
is the same claim `/setup` already rests on. **It moves the principal and cannot
add one**: exactly one account holds it afterwards, checked before the change
commits, so the bound above survives the repair. It writes an instance-wide audit
record whose actor is `system`, because nobody signed in to make it. Anyone who
can run it could already have edited the database directly; what changes is that
the edit is now recorded and cannot produce a second principal by accident.

`destinations.decide` and `instance.admin` are kept off every API key by
`auth.NonDelegableScopes` — a key that can allow a destination could then point
links at it, and a key that can appoint a reviewer widens its reach by
manufacturing somebody else's. `destinations.review` **is** delegable: reading
the queue discloses who filed a dispute and a defanged host, never an address or
a network prefix, and escalates nothing. That split is what "read it with a
token, change it with a person" is built out of; there is no check anywhere on
what kind of credential is calling.

*Before 0.2.0* `destinations.review` was granted to the **owner role**, which is
per-organization — so the owner of *any* organization on the instance saw every
dispute filed on it, including the address of whoever filed it, and could lift an
entry for everybody. With `LINKCTRL_SIGNUP_MODE=open` that was one registration
away. **Upgrading moves it**: the migration removes the role grant and confers
the instance permissions on the earliest surviving account, which on any instance
that went through setup is the setup account. If that is not the operator's
account any more, see [operations.md](operations.md).

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
third party.** `LINKCTRL_FEED_URL` is unset by default; setting it is the
exception, and it is one you are making on behalf of people who are not you. What
then leaves is the destination URL and nothing else — no account, no address, no
workspace, no instance name — over `https`, to the endpoint you named, when a
link is created or edited, when the root redirect is set, and when a refusal is
disputed. Never on the redirect path, and existing links are not re-checked.

**A feed is not the only way a destination leaves this instance — it is the only
way you decide.** Outbound webhooks are the second channel, and they are a
workspace's feature rather than an operator's: no environment variable turns them
off, and anybody holding `webhooks.write` — the owner and admin roles, never an
API key — registers a URL of their own choosing and receives their workspace's
events at it. `link.created`, `link.updated`, `link.archived`, `link.restored`
and `link.deleted` carry the link's destination **as typed**, in `data.url`;
`destination.blocked` carries the attempted destination **defanged**; and
`automation.fired` carries, as a subject label, an alias or a defanged host and
never a destination in full. That is all seven events, so the list is what leaves
and not an example of it. The reach is one workspace's own links and no further,
the URL itself is checked twice before anything is sent to it (*Outbound
webhooks*, above), and every registration is audited — but the lever you hold
over it is who has `webhooks.write`, not a setting. Size that the way you would size the feed: an
admin registering a webhook is deciding, on behalf of everybody who creates a
link in that workspace, that their destinations go to a host of that admin's
choosing.

Four bounds hold the feed. It is asked **last**, so a destination any built-in
tier refuses never reaches it and no built-in answer changes with a feed on, off
or erroring — which bounds the feed and not the instance, because that refusal is
itself a `destination.blocked` event, and a workspace with a webhook subscribed
to it receives the refused destination, defanged. A feed that does not answer
**fails open** and increments
`linkctrl_destination_feed_checks_total{result="error"}` — which means a feed
that silently stopped working looks exactly like no feed at all, so alert on that
counter if you depend on one. Its verdicts are **low confidence**: disputable,
and an owner overruling one from `/disputes` also stops that host being sent
again. And the instance **discloses it** at `/feeds` and `GET /api/v1/feeds`, to
every signed-in account rather than to administrators only, on or off — a
read-only page with no controls, because only you can change any of this and a
disclosure that could be edited from the dashboard would be a settings page this
product has no principal for (decision D40). Feed responses are treated as
hostile input: bounded in size, redirects not followed, and an unreadable verdict
counted as an error rather than guessed at.

**That page answers for the webhook channel too, and it did not always.** Until
M45 it read the feed setting alone, so on an instance with no feed it told every
signed-in account, in a green panel, that no destination left — which a webhook
registered in their own workspace made false. It now reads the workspace's
registrations as well and states each channel's answer in all four combinations:
the feed's is instance-wide and the webhook's is scoped to the workspace being
viewed, and neither may be read as the other. What it publishes about the second
channel is *whether* something enabled there is subscribed to an event carrying a
destination, and how many — never a URL, because who a workspace posts to is
behind `webhooks.read` and this page is behind nothing at all.

**Your feed credential belongs in `FEED_AUTH_TOKEN`, and a URL carrying one is
refused at boot.** That variable is a secret: redacted wherever it is printed,
removed from the process environment after parsing, and readable from a mounted
file. A credential written into `FEED_URL` instead would get none of that — Go
turns `https://key:secret@feed.example/` into a Basic auth header, so it works
silently — and `/feeds` shows every signed-in account where destinations go.
Configuration validation therefore refuses a `FEED_URL` with a username or
password in it, beside the `https` check. What the disclosure prints is built up
from the scheme, host and path rather than cut down from the whole URL, so a
query string and a fragment never appear either. **A credential written into the
path is the one this cannot see**: it is indistinguishable from a path, nothing
refuses it, and `/feeds` will print it.

Transport failures name the same redacted endpoint. A feed that times out — the
common case on a two-second budget — used to be logged with the full configured
URL, API key and all, and on `FEED_METHOD=GET` with the user's destination
appended to it. The error now carries what `/feeds` carries and the cause, so a
log stream shipped elsewhere is not where the credential ends up.

**Blocked attempts are recorded, and the attempted URL is hostile input.** Every
refusal writes a `destination.blocked` audit event carrying the tier, the rule,
the surface and an `ip_prefix` — never an address. The attempted URL is stored as
evidence in a defanged form (`https[:]//evil[.]example/...`, everything
HTML-active percent-escaped), so nothing that renders the audit log can turn it
into markup or into a link somebody follows by reflex. Anyone who can create a
link can therefore add rows to `audit_logs`; `LINKCTRL_AUDIT_RETENTION_DAYS` and
the size warning are what bound that.

**Addresses are verified only on the self-serve path.** Public registration
confirms the address before the account exists — the form writes a pending row
and mails a single-use link, and the user, organization and workspace are created
when it is followed — so `open` requires a configured mailer and drops to
invitation-only without one. Every other path leaves the address unverified: the
first-run setup account is trusted by construction, and an invited one proves
receipt of a link rather than readership of an inbox.

*This paragraph opened with "No MFA" until 0.3.0, and that is no longer true —
see* Two-factor authentication *above. What is still absent is narrower and is
below.*

**Nobody can be *required* to have a second factor.** There is no
organization-level *require MFA for all members* policy, and its absence is a
decision rather than a gap in the implementation: it needs a permission of its
own, an enforcement point on every session resolution, and an answer for members
who cannot enrol — a policy feature wearing an authentication feature's clothes.
So a second factor is each account's own choice, and an administrator who needs
one across a team has to ask rather than enforce. **The second factor is also
only as good as the instance's key management**: `LINKCTRL_MFA_SECRET_KEY` is not
rotatable in place and there is no re-encrypting sweep, so changing it has
exactly the effect of losing it. That consequence is bounded — accounts fall back
to recovery codes and then to the operator — and it is a new class of operator
mistake, counted as one.

**Who may sign up is the operator's setting and nobody else's.**
`LINKCTRL_SIGNUP_MODE` is the mode, there is no runtime toggle, and no session
or API call changes it — which is deliberate rather than unfinished. When that
was decided this product had no instance-level principal: an account is an owner
*of an organization*, and a self-registered one owns the organization it was
given, so any permission gating this on the owner role would have been held by
every stranger who signed up on an open instance. Decision D38 removed the toggle
instead of narrowing it. 0.2.0 introduces a principal for the dispute queue and
the instance audit log (D98), and its scopes are enumerated rather than implied —
the signup mode is deliberately not one of them, so changing the mode still
requires whoever can edit the environment and restart the process.

**Registration cannot be asked whether an address already has an account, since
0.2.0.** It could until then: a taken address answered `409` and a free one
`202`, on an endpoint that is unauthenticated whenever the effective mode is
`open`, so a leaked address list could be tested for membership against the
instance. Both answers are now the same status, the same body and the same
argon2 cost — the hash is computed before the address is looked up, so the
timing does not restore the difference the status code stopped carrying — and
the person who owns the address is told by mail that somebody tried to register
it. Nothing is written for a taken address either, so a stranger cannot
invalidate the real owner's outstanding verification link by typing their
address into the form. This is the same stance redemption has held since M27
(D27); the two surfaces used to disagree.

**Open sign-ups have no CAPTCHA and no proof-of-work.** Registration shares the
sign-in rate limit per address, and confirming an address by email is what stops
an account existing for one nobody controls. Neither stops a distributed run from
filling the pending table or sending mail to addresses that did not ask for it.
Leave `LINKCTRL_SIGNUP_MODE` at `closed` or `invite` unless public sign-up is
something the instance actually wants.

**Queued mail is readable by anyone who can read the database, and two of the
messages carry a credential until they are delivered.** The outbox stores each
message rendered — recipient, subject and body — so it survives a restart and so
an operator can see what was attempted. Two of the four templates contain a
single-use token in that body, because that is how an invitation and an address
verification reach the person they are for; the `invitations` and
`pending_registrations` rows themselves keep only `SHA-256(token)`.

**A row loses its body the moment it stops needing it.** Marking a message sent
or failed empties `body` in the same statement, and the database refuses to hold
a finished row that still has one, so the exposure is the delivery rather than
the 30-day retention window that keeps the record of the attempt. What remains
for an operator — recipient, subject, kind, attempts, `last_error` — is what the
outbox exists to show.

What that leaves, stated rather than implied: a message that has **not** been
delivered still holds its token, because it is the message about to be sent. On
an instance whose relay is down, an invitation's token is readable from the
database for as long as that invitation is redeemable — `LINKCTRL_INVITE_TTL`,
seven days by default, and 24 hours for an address verification. Shortening
those shortens this. Nothing that must never touch storage should go in a mail.

**What an unresponsive relay costs the rest of the instance is bounded to a
single attempt.** A claimed batch is handed to the relay together rather than one
message after another, so a drain occupies the scheduler for one
`LINKCTRL_SMTP_TIMEOUT` however large the outbox is, and a relay that accepts
connections and never answers cannot hold webhook delivery, automation or domain
re-verification behind it. The cost of that shape is stated rather than implied:
up to twenty sessions are opened to the one relay the operator named, and a relay
that caps concurrent connections below that refuses the extra ones — a refusal is
a spent attempt that retries with backoff, so nothing is lost, but an outbox held
continuously above the cap for five attempts abandons the overflow.

**Mail is not signed, and delivery is only as trustworthy as the relay.** There
is no DKIM signing and no SPF or DMARC alignment done here — those belong to the
relay and the sending domain's DNS, and an instance that points `SMTP_HOST` at a
relay it does not control has handed that relay every message it sends. PLAIN is
the only supported authentication mechanism; it is refused over an unencrypted
connection, so the password is never sent in clear, but it is a reusable
credential in the environment like any other.

**The audit log works, and what it does not cover is worth knowing.** The writer,
the read API, retention and the growth metric all exist.

*This paragraph was wrong in both directions until 0.2.0* ([F17](build-notes/deferred-findings.md)),
which is why it now points at a list rather than restating one. It named five
kinds of event when the vocabulary had grown to **thirty-two**, and it said key
revocation was untrailed when `apikey.revoked` had been recorded since M44 — so
a security-conscious reader met both an under-count and a claim that something
was unaudited when it was. The authoritative list is `audit.AllActions` in
`internal/audit`, a test fails if a constant is declared without appearing in it,
and the coverage row above states its length.

**What is genuinely not trailed**, checked against that list rather than
remembered:

- **Link creation, editing and deletion.** The highest-volume writes in the
  product, and the ones an operator most often wants a history of.
- **Signing in**, successfully or otherwise. Failed attempts move
  `users.failed_login_count` and `users.locked_until`, which is the only record.
- **Minting an API key.** Rotating one and revoking one *are* recorded; creating
  one is not, so a key's first appearance in the trail is its rotation or its
  end.
- **Redeeming an invitation creates a membership**, and the redemption is
  recorded — but the account that may have been created alongside it is not, and
  neither is self-serve registration.

Do not treat a quiet audit log as evidence that nothing happened.

**No malware scanning of destinations, no rate limit on redirect volume per
link.** A popular link and a link being used for amplification look the same.

**A gate restricts the short link, not the destination.** A password, a
signature, a single use or a click ceiling all decide whether *this alias*
redirects. None of them protects the destination: anybody who reaches it another
way is unaffected, and a visitor who has passed a gate once holds the destination
URL and can pass it on. Treat these as a way to control distribution of a link,
never as access control over what it points at.

**A link password is remembered nowhere, and that is the trade D53 bought.**
Verification issues nothing to the browser, so every visit is another challenge.
The cost is a visitor typing it repeatedly; the benefit is that the short-link
host still has no session middleware, no CSRF middleware and no cookie of any
kind. Anything added later that makes an unlock persist across clicks reopens
that decision rather than inheriting it.

**Guessing a link password is limited, not bounded.** The limit is charged per
address and per alias, but it is enforced through Redis and falls back to
per-replica in-memory buckets on any Redis error — so N replicas allow N times
the configured number until Redis returns, and a restart resets the local
buckets. There is no lockout, because there is no account to lock. A link
password should be chosen as though somebody will get several attempts a minute
indefinitely.

**Signed links are revoked by clearing a column, and not immediately.** There is
no revocation endpoint: signatures expire, and that is the mechanism. To
invalidate every outstanding signature a workspace has issued:

```sql
UPDATE workspaces SET signing_secret = NULL WHERE id = '<workspace uuid>';
```

The next signing request mints a new key. Each replica keeps honouring the old
one until its in-process copy expires — up to one minute — so anybody holding a
signed URL can still follow the link during that window.

**Raising a click ceiling re-opens a spent link.** The counter is monotonic and
is compared against the link's current limit, so moving a ceiling from five to
six makes a link that was answering 410 redirect again. There is no way to grant
"five more from here".

**A key can replace itself, and what that costs: a leaked key can survive being
revoked.** `POST /api/v1/api-keys/rotate`, sent with a key's own token, mints that
key's successor. The design keeps the rule that makes revocation meaningful — a
key still cannot mint an unrelated key, cannot grant itself a scope it does not
hold, cannot move to another workspace, and cannot rotate twice — and it still
cannot hold `apikeys.read` or `apikeys.write`, which is the whole reason handing
rotation to a credential is defensible. What it cannot prevent is the obvious
consequence: **whoever holds a stolen secret can rotate it too.** They end up
holding a successor under a prefix you never issued, and revoking the key you know
about does not touch it.

Three things bound that, and none of them removes it:

- **Every generation is in your key list**, with its own prefix, its reach and its
  state. A rotation appears as a new row, not as an edit to an old one.
- **Every rotation writes an `apikey.rotated` audit record** naming the prefix it
  came from and the prefix it became, so a chain is walkable from any generation
  by anybody with `audit.read`.
- **Each predecessor's grace window is capped at a day.** The old secret verifies
  for that window and then stops, on every replica, as part of authenticating
  rather than by a scheduled job.

So: **if you believe a key has leaked, read the key list before you revoke
anything, and look for keys you did not create.** Revoking the one you recognise
and stopping there is the mistake this design makes possible. Rotating it is
worse — it hands the same holder a fresh secret. If you cannot account for every
key on the list, revoke all of them.

One operational note that is easy to get wrong: `last_used_at` is written on a
30-second cadence, so a key that reads as unused may have been used within the
last half minute. Do not treat an idle-looking predecessor as proof that a
rotation has finished propagating. The grace window cannot be set below five
minutes for exactly this reason.

## Operator responsibilities

The product cannot do these for you, and getting them wrong undoes the section
above.

- **Terminate TLS in front of it.** LinkCtrl does not. Session cookies carry
  `Secure` in production and are useless over plaintext. See
  [deployment.md](deployment.md).
- **Keep `:9090` unreachable.**
- **Keep `/tls-check` reachable only by your proxy**, on the loopback address.
  It is consulted during a TLS handshake so it cannot be authenticated; publishing
  it lets anybody enumerate which custom hostnames this instance serves.
- **Decide the custom-domain grace window deliberately.** `DOMAIN_VERIFY_GRACE`
  is how long this instance keeps serving a hostname whose DNS its owner may no
  longer control. The default is one day. Setting `DOMAIN_VERIFY_INTERVAL=0`
  switches **two** jobs off, not one: re-verification, which makes that window
  unbounded, and each replica's periodic re-read of the verified-hostname set it
  serves from. The second is the backstop for a pub/sub invalidation that was
  simply lost, so with it off a replica that missed one keeps serving a hostname
  whose verification is already gone until something restarts it.
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
- **Know what the webhook address check cannot see.** Delivery refuses a socket
  to a private, loopback or link-local address, checked at connect. If you deploy
  behind an egress proxy that resolves names itself, every address LinkCtrl sees
  is the proxy's and that check then says nothing — the control has to be in the
  proxy. The same applies to any sidecar or service mesh that intercepts outbound
  connections.
- **Own the add-ons directory, and mount it read-only.** A module in
  `LINKCTRL_ADDONS_DIR` is code this instance executes in its own process. **Who
  may write to that directory is the whole of the trust boundary**, and the
  digest in a manifest does not narrow it: it protects against the module
  changing after the manifest was written, and a manifest and a module replaced
  together verify perfectly. So the checksum answers *did this file change*, and
  only you answer *should this code be here*. An add-on you did not build is a
  supply chain you have taken on, on the same terms as the image itself. **The
  same is true of the DDL beside it.** An add-on may ship a `migrations/`
  directory, the host runs it at boot, and every file in it is named in the
  manifest with its own digest — so DDL added to an installed add-on without
  editing the manifest refuses the add-on rather than running. That closes the
  set; it does not widen the boundary, which is still who may write there.
- **Read the `permissions` in a manifest before you install it.** They are the
  whole of what the module may do and the host enforces them, so they are also the
  most compact statement of what you are agreeing to: `session.mint` means the
  add-on decides who is signed in, and `storage.own_schema` means it owns tables in
  your database. The Add-on manager will show them; until it does, they are in
  `addon.json`, in the boot log, and on
  `linkctrl_addon_info{permissions=…}`. What they do **not** bound is CPU and wall
  clock, and only one half of that is answered: the add-on's *module* gets 30
  seconds to compile and 30 seconds to start, after which it is refused as
  `load_timeout` and the class it declared decides what that costs. Its
  **migrations** are outside that budget on purpose — they run in the same
  before-listener window, under a Postgres session lock that waits up to five
  minutes for another replica, and no statement timeout applies to them (F274), so
  a migration that never returns still holds the boot open.
- **An add-on that asks for `storage.own_schema` needs your database user to hold
  `CREATEROLE`**, because the host creates a role per add-on and that role is what
  confines it. Superuser satisfies it and so does an explicit `CREATEROLE` grant;
  the shipped compose file gives you the former. Password authentication has to be
  available for that role as well, since the host opens a connection *as* it — a
  deployment where LinkCtrl authenticates by `peer` or by a cloud IAM token cannot
  offer one, and such an add-on will not load. The refusal is explicit and names
  the reason. There is deliberately no fallback: the only weaker mechanism is
  `SET ROLE` on the application's own connection, which is not a boundary, and an
  instance quietly running an unconfined add-on is worse than one that says it
  cannot run it at all.
- **Watch each add-on's schema, because nothing caps it.**
  `linkctrl_addon_schema_bytes{addon}` is the only bound there is — the same answer
  the audit log gets, and for the same reason. An add-on that writes a row per
  redirect is a disk you agreed to when you installed it. **Watch
  `linkctrl_addon_large_objects{addon}` beside it**, which should be zero for every
  add-on forever: a large object is in no schema, so the gauge above cannot see one,
  and any other value means an add-on wrote data outside its schema. **Both gauges
  measure data an add-on has *stored* — every catalogued relation in the schema,
  sequences included since the 2026-08-19 correction — and one kind of disk use is
  outside them**:
  a session holding a `WITH HOLD` cursor keeps a temporary file in the cluster's
  `pgsql_tmp` until the connection ends, and neither gauge moves while it does —
  measured at 553 MB for one cursor inside the add-on's five-second statement
  timeout. It is transient rather than stored, and it is bounded only by
  `temp_file_limit`, which needs a superuser to set on the add-on's role. So alert
  on the filesystem as well, not only on these two.
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
| Link passwords | argon2id, the same parameters and the same 12-character floor as an account password, per-password salt |
| Signed links | `HMAC-SHA256(workspace secret, "lc1" ‖ 0x0A ‖ domain uuid ‖ 0x0A ‖ alias ‖ 0x0A ‖ expiry)`, base64url unpadded in `?sig=`, with the expiry beside it in `?exp=`. The secret is 32 bytes from `crypto/rand` |
| Webhook signatures | `HMAC-SHA256(secret-as-hex-string, "<unix seconds>" ‖ "." ‖ raw body)`, lowercase hex in `X-LinkCtrl-Signature: v1=…`. The secret is 32 bytes from `crypto/rand`; the **hex string as shown to the receiver** is the key rather than the decoded bytes, so a receiver copies it and uses it with no encoding step to get wrong. The timestamp is in the signed message so a receiver can reject replays |
| CSRF | Go's `net/http` cross-origin protection — `Sec-Fetch-Site` and `Origin` checking on unsafe methods, with `BASE_URL` added as a trusted origin for proxied deployments |

The visitor hash is truncated deliberately: 16 bytes is far beyond collision
relevance for a day's traffic, and the shorter value is the one that never needs
to be reversible.
