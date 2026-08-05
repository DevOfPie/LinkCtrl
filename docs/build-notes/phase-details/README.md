# Phase details

One file per milestone, named for its number: `m21.md` … `m45.md`, plus the
fractional insertions — `X.9` is reserved for scheduled reviews, `X.1`–`X.8` for
added scope; the rules are in [planning.md](../planning.md). Read exactly the one you are building — that is what this
split is for. Nothing here restates another file.

[Plan.md](../../../Plan.md) holds the scope contract and the ordering table.
This directory holds the definitions of done those rows point at.

**Status lives here**, and only here. Plan.md's Build status table is per-area,
which suited Phase 1's twenty-one milestones summarized by subsystem and does not
scale to twenty-five tracked individually. Update the row when a milestone starts
and when it lands.

A milestone that shipped and came back reads `in progress (reopened)`. The
parenthesis earns its place: without it, a milestone below the one being built
looks like an ordering mistake. Reopening is what happens when a shipped
milestone's claim turns out false — the rule, and why it beats a successor
milestone, is in [workflow.md](../workflow.md).

| # | Milestone | Depends on | Status |
| --- | --- | --- | --- |
| [M21](m21.md) | Audit log: behavior, retention, growth alerting | — | done |
| [M22](m22.md) | Notifications: in-app behavior | — | done |
| [M23](m23.md) | Cross-replica cache invalidation (pub/sub) | — | done |
| [M24](m24.md) | Shared rate limits (credentials and API) | — | done |
| [M24.5](m24.5.md) | Dark mode: theme tokens, system default, override | — (before M25) | done |
| [M25](m25.md) | Workspace and organization switcher | — | done |
| [M26](m26.md) | Mailer: optional SMTP delivery | — | done |
| [M26.5](m26.5.md) | Dashboard header: identity menu and notification bell | — (before M27) | done |
| [M26.6](m26.6.md) | Bounded Redis failure, when the server never answers | — (before M32.5, M34, M40) | done |
| [M27](m27.md) | Organizations: invitations and joining | M21 M22 M25 M26 | done |
| [M28](m28.md) | Team management, workspaces, org creation | M27 | done |
| [M28.5](m28.5.md) | Organization deletion and tenancy teardown | M28 | done |
| [M29](m29.md) | Self-serve signup, configured by the operator | M26 M27 | done |
| [M30](m30.md) | Destination blocking: tiers and logging | M21 | done |
| [M31](m31.md) | Blocked-attempt disputes and owner review | M30 M22 | done |
| [M32](m32.md) | Opt-in reputation and malware feeds | M30 M31 | done |
| [M32.5](m32.5.md) | Bot blocking, per domain and per link | — (before M33, M34) | done |
| [M32.9](m32.9.md) | **Mid-phase adversarial review** | M21–M32.5 | done |
| [M33](m33.md) | Deep-link path forwarding | — (before M34) | done |
| [M33.5](m33.5.md) | A demo that shows the phase, not just its links | M32.9 | done |
| [M34](m34.md) | Routing rules: conditions, first-match evaluation | M23 M30 M33 | done |
| [M35](m35.md) | Gated links: password, signed, one-time, max-click | M34 (ordering) | done |
| [M36](m36.md) | Split testing: weighted, sequential, fallback, flags | M34 M35 M30 | done |
| [M37](m37.md) | Dimension visualizations, rollup cadence first | — | done |
| [M38](m38.md) | Folders: API and tree UI | — | in progress (reopened) |
| [M39](m39.md) | Per-domain ownership | M21 | done |
| [M40](m40.md) | Custom domains: verification and serving | M39 M23 | done |
| [M41](m41.md) | QR codes and campaigns | — | done |
| [M42](m42.md) | Webhooks | M30 | done |
| [M43](m43.md) | Automation rules | M22 M35 M42 | done |
| [M44](m44.md) | API keys: rotation and scope choice | M21 | done |
| [M44.9](m44.9.md) | **Pre-release adversarial review** | M21–M44 | done |
| [M45](m45.md) | Deferred findings, documentation pass, 0.2.0 | all | in progress |

New milestone files start from [_template.md](_template.md).
Phase 1's late-added milestones are in [phase-1.md](phase-1.md).

## What every milestone inherits

Not repeated in the files below. These hold for all of Phase 2.

| Rule | Consequence |
| --- | --- |
| Redirect tree stays minimal | No session lookup, CSRF check or template rendering. Tripwire tests must pass unmodified, or the amendment is deliberate, recorded and signed off. |
| Redirects are never permanent | Never 301, never 308. Forever. A link redirect is **302**; the one exception is the verified-password POST, which answers **303** so the browser is required to drop the method and body — [M35](m35.md)'s reopening, D94. *(Label amended 2026-08-04: it read "Redirects are 302", which stopped describing the tree the moment 303 landed. The consequence — never permanent — is unchanged and is what the rule asserts.)* |
| Cache is optional | Redis absent or down degrades behaviour; nothing correctness-critical depends on it. Tested with Redis off. |
| Privacy stance | No IP column anywhere. `ip_prefix` only (/24 v4, /48 v6). Region and city resolve transiently and are never stored. **The stance is about storage, and Redis is the exception it does not reach** (F57): the shared rate limiter's key is `lc:rl:<bucket>:<client>`, which for IPv4 is the **full address**, held for the limiter's window — 120 seconds by default. It is not a column and the stance is not violated; it is also not nothing, because `LINKCTRL_REDIS_URL` may point at a managed service that snapshots by default. Anonymising the v4 side to /24 is **not** available: the limiter would then let one host throttle 255 neighbours, and the /64 used for IPv6 is evasion resistance rather than privacy. Keyed hashing is the only coherent fix and is not built; `docs/deployment.md` tells operators to disable persistence instead. |
| Every UI feature has API support | Both call the same service layer. New operations land in `api/openapi.yaml` and are replayed by the contract test. |
| Dormant structure is jsonb | Until the feature that uses it arrives. |
| Partitioning | `PARTITION OF` never appears in sqlc-visible SQL; partitions are created by application code. |
| DDL is additive | Within a minor version. |
| Permissions | A new permission needs a seed migration that inserts *and* grants it (the 00800 pattern). Delegability follows decision D18 — non-delegable when reading it exposes an actor's identity tied to network data, or when holding it lets a key widen its own reach; delegable otherwise. The milestone records which limb it matched, or that it matched neither. `NonDelegableScopes` is the only mechanism for **whether a key may hold a permission at all**. Three narrower ones govern what a key may do with the permissions it legitimately holds, and each exists because the permission map cannot express *this credential is not the person*: what a key may **produce** — D43 caps the role a key-issued invitation may carry, and the same cap sits on role assignment against an existing membership (`team.ChangeRole`, `team.Grant`), because a key that manufactures an interactive principal has stepped around the map rather than through it; what a key may **see** — a key's reads are bounded to the organization it was issued for where a session's are not, because the workspace switcher exists to cross organizations and a key was issued for one (F103); and whether the **session or the key is the authority** — `requireSessionActor` refuses operations whose subject is the person, meaning their password, their other sessions and where their browser lands, and rotation refuses the inverse, because a key replacing itself is authorized by its own token (D87). Anything branching on credential type outside those four is still a defect. *(Amended 2026-08-02, owner-answered, on [M27](m27.md)'s reopening — it read "`NonDelegableScopes` is the only mechanism; nothing branches on whether the caller holds a session". See decisions.md.)* *(Amended again 2026-08-05, owner-answered, on [M45](m45.md)'s F104 — it named two mechanisms and called everything else a defect, while eleven sites branched on credential type and every one was correct. The owner chose the enumerated form over a rule stated as a test, knowing the enumeration is what drifted twice. See decisions.md.)* |
| `ui` stays stdlib-only | No Node, no CDN, CSP unchanged, no `unsafe-` waivers. |
| Both themes, from [M24.5](m24.5.md) on | New UI colors use the theme tokens; M24.5's template scan fails raw palette utilities. |
| Touching the redirect path | Re-run the [docs/slo.md](../../slo.md) k6 measurement on the built image; cached p99 stays under 20ms. |
| A test that passes first try | Sabotage it, confirm it fails, restore by counter-edit. |
| A new feature somebody can *see* | Extend the demo seeder (`cmd/lctl/demo.go`, and `demo_phase2.go` beside it) so the demo instance shows it. A feature only reachable by building the state yourself is one nobody evaluating this product will find. Does not apply to work with nothing to look at — a timeout bound, an invalidation path, a permission nobody exercises directly. **Enforced since [M33.5](m33.5.md)**: `demoCoverage()` in `cmd/lctl/demo_coverage_test.go` enumerates what the demo must show and fails when a listed feature has no seeded rows, so a milestone that seeds nothing adds a row there or breaks the build. Rows that assert *zero* for a milestone not yet built are turned into real rows when that milestone lands, rather than deleted. **The number of them is deliberately not written here.** It read *four* from [M33.5](m33.5.md) until 2026-08-05, was true when written, and was wrong within one milestone — M34 converted its own row exactly as this rule instructs and the count beside the rule did not move with it. By M45 every one had been converted and the sentence described nothing that existed. Saying *four* was a fact nothing kept true; saying *its trailing rows* is a fact that stays true however many there are (F69). If the obligation ever proves too heavy, narrow the list deliberately and in writing; never delete the test. |

## Decisions already taken

Recorded in full in [Plan.md](../../../Plan.md#phase-2-decisions). Referenced by
number from the milestone files.
