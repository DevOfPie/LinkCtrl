# Phase details

One file per milestone, named for its number — `m46.md`, and the fractional
insertions beside it, where `X.9` is reserved for scheduled reviews and
`X.1`–`X.8` for added scope; the rules are in [planning.md](../planning.md).
Read exactly the one you are building — that is what this split is for. Nothing
here restates another file.

**This file carries the live phase's status table and nothing else.** A finished
phase's rows move to their own file, so the resume path reads the milestones that
can still be next rather than every milestone there has ever been: Phase 1 is in
[phase-1.md](phase-1.md), Phase 2 in [phase-2.md](phase-2.md), Phase 3 in
[phase-3.md](phase-3.md). The live table below is Phase 4's, added when it was
planned on 2026-08-18.

[Plan.md](../../../Plan.md) holds the scope contract and the ordering table.
This directory holds the definitions of done those rows point at.

**Status lives in this directory**, and nowhere else — here while a phase is
live, in its `phase-N.md` once it is released. Plan.md carries neither: its Build
status is three lines naming the releases, and the per-area table it used to
carry was Phase 1's own record, which is now in
[phase-1.md](phase-1.md#build-status-detail). Update the row when a milestone
starts and when it lands.

A milestone that shipped and came back reads `in progress (reopened)`. The
parenthesis earns its place: without it, a milestone below the one being built
looks like an ordering mistake. Reopening is what happens when a shipped
milestone's claim turns out false — the rule, and why it beats a successor
milestone, is in [workflow.md](../workflow.md).

## Phase 4

Planned 2026-08-18 — fourteen milestones, M59–M70; the ordering table and its
dependency edges are [Plan.md](../../../Plan.md#phase-4-build-plan)'s. **Fifteen
as built**: [M66.5](m66.5.md) was added on 2026-08-23, after measuring what M66's
inline class costs a visitor when nothing is pooled — a milestone the build turned
out to need, which [planning.md](../planning.md#the-size-target-a-phase-stays-under-sixteen-milestones)
allows without a phase-boundary conversation, and which leaves the phase at the
planning target rather than past it. Phase 3
released as **0.3.0** the same day; its record is in [phase-3.md](phase-3.md).

| # | Milestone | Depends on | Status |
| --- | --- | --- | --- |
| [M59](m59.md) | Process debt: the gates that were not watching | — | done |
| [M60](m60.md) | The host: a module loads, or is refused | M59 *(ordering)* | done |
| [M61](m61.md) | The ABI: what an add-on may import, written down and versioned | M60 | done |
| [M62](m62.md) | Declared permissions: an add-on gets what it named and nothing else | M61 | done |
| [M63](m63.md) | An add-on's tables: a schema of its own, migrated by the host | M62 | done |
| [M64](m64.md) | An add-on reaches the page: routes, templates, config | M62 · M63 *(ordering)* | done |
| [M64.9](m64.9.md) | Mid-phase adversarial review | M59–M64 | done |
| [M65](m65.md) | The authentication hook: a session minted on an add-on's word | M61 · M62 · M64 | done |
| [M66](m66.md) | Add-ons on the redirect path: two classes, a deadline, and a promise rescoped | M60 · M62 | done |
| [M66.5](m66.5.md) | Instances are reused, so a visitor stops paying for a cold start | M66 · M60 *(ordering)* | done |
| [M67](m67.md) | Runtime lifecycle: an add-on arrives and leaves without a reboot | M60 · M62 · M63 · M66.5 | done |
| [M68](m68.md) | The Add-on manager | M63 · M66 · M67 · M64 *(ordering)* | not started |
| [M69](m69.md) | The OIDC add-on: the foundation's acceptance test | M61 · M63 · M64 · M65 · M68 *(ordering)* | not started |
| [M69.9](m69.9.md) | Pre-release adversarial review | everything below it | not started |
| [M70](m70.md) | Deferred findings, documentation pass, 0.4.0 | all | not started |

**Phase 4 inherits all fourteen rules below, as written — owner-confirmed
2026-08-18** at the plan's review, recorded in
[phase-4-candidates.md](../phase-4-candidates.md#what-this-collides-with-named-now-rather-than-discovered).
The five rules the phase collides with on purpose stay arguments each colliding
milestone must win in its own file, never waivers.

## What every milestone inherits

Not repeated in the milestone files. **These are Phase 2's**, and they stayed
here rather than moving with its status table because most are product
invariants that outlast the phase that wrote them — never permanent redirects,
the privacy stance, `ui` stays stdlib-only, sabotage a test that passes first
try. **Which of them Phase 3 inherits was confirmed on 2026-08-07**, one at a
time, rather than assumed by the table having been left in place. It did not
happen at planning time as this sentence once promised — a review found the
omission, and it is recorded rather than backdated. Where the confirmation lives
is below the table.

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
| Permissions | A new permission needs a seed migration that inserts *and* grants it (the 00800 pattern). Delegability follows decision D18 — non-delegable when reading it exposes an actor's identity tied to network data, or when holding it lets a key widen its own reach; delegable otherwise. The milestone records which limb it matched, or that it matched neither. `NonDelegableScopes` is the only mechanism for **whether a key may hold a permission at all**. Three narrower ones govern what a key may do with the permissions it legitimately holds, and each exists because the permission map cannot express *this credential is not the person*: what a key may **produce** — D43 caps the role a key-issued invitation may carry, and the same cap sits on role assignment against an existing membership (`team.ChangeRole`, `team.Grant`), because a key that manufactures an interactive principal has stepped around the map rather than through it; what a key may **see** — a **pinned** key's reads are bounded to the organization it was issued for where a session's are not, because the workspace switcher exists to cross organizations and such a key was issued for one (F103); an *account-wide* key is bounded only by the organizations it has been **cut out of** — M54 removed the pinned premise rather than the rule, so such a key reads about the tenants it works in, and a reach revocation is exactly what stops one being a tenant it works in (F183); and whether the **session or the key is the authority** — `requireSessionActor` refuses operations whose subject is the person, meaning their password, their other sessions and where their browser lands, and rotation refuses the inverse, because a key replacing itself is authorized by its own token (D87). Anything branching on credential type outside those four is still a defect. *(Amended 2026-08-02, owner-answered, on [M27](m27.md)'s reopening — it read "`NonDelegableScopes` is the only mechanism; nothing branches on whether the caller holds a session". See decisions.md.)* *(Amended again 2026-08-05, owner-answered, on [M45](m45.md)'s F104 — it named two mechanisms and called everything else a defect, while eleven sites branched on credential type and every one was correct. The owner chose the enumerated form over a rule stated as a test, knowing the enumeration is what drifted twice. See decisions.md.)* *(**The *see* limb has been amended twice for one drift**, facts both times: 2026-08-08 at [M54](m54.md) it described a bound the tree no longer applied to every key, a key having stopped being issued for one organization; 2026-08-10 at [M58](m58.md) it then read "an *account-wide* key is not bounded", which [F183](../deferred-findings.md#closed) falsified inside that same milestone. The limb is unchanged and still enforced at `Service.Workspaces`, which now asks `Identity.keyReaches` — one predicate over both reaches. `Identity.APIKeyOrgID` still tells the two apart, inside it rather than at the call site, so the count of sites branching on credential type is what it was. See decisions.md, D155–D158.)* |
| `ui` stays stdlib-only | No Node, no CDN, CSP unchanged, no `unsafe-` waivers. |
| Both themes, from [M24.5](m24.5.md) on | New UI colors use the theme tokens; M24.5's template scan fails raw palette utilities. |
| Touching the redirect path | Re-run the [docs/slo.md](../../slo.md) k6 measurement on the built image; cached p99 stays under 20ms. |
| A test that passes first try | Sabotage it, confirm it fails, restore by counter-edit. |
| A new feature somebody can *see* | Extend the demo seeder (`cmd/lctl/demo.go`, and `demo_phase2.go` beside it) so the demo instance shows it. A feature only reachable by building the state yourself is one nobody evaluating this product will find. Does not apply to work with nothing to look at — a timeout bound, an invalidation path, a permission nobody exercises directly. **Enforced since [M33.5](m33.5.md)**: `demoCoverage()` in `cmd/lctl/demo_coverage_test.go` enumerates what the demo must show and fails when a listed feature has no seeded rows, so a milestone that seeds nothing adds a row there or breaks the build. Rows that assert *zero* for a milestone not yet built are turned into real rows when that milestone lands, rather than deleted. **The number of them is deliberately not written here.** It read *four* from [M33.5](m33.5.md) until 2026-08-05, was true when written, and was wrong within one milestone — M34 converted its own row exactly as this rule instructs and the count beside the rule did not move with it. By M45 every one had been converted and the sentence described nothing that existed. Saying *four* was a fact nothing kept true; saying *its trailing rows* is a fact that stays true however many there are (F69). If the obligation ever proves too heavy, narrow the list deliberately and in writing; never delete the test. |

**Which of them Phase 3 inherits was confirmed on 2026-08-07**, one at a time,
and the confirmation — a table naming, for each rule, the milestone that tests
it and how — is in
[phase-3-candidates.md](../phase-3-candidates.md#phase-3-inherits-all-fourteen-confirmed-2026-08-07).
**It moved there on 2026-08-08, at [M51.9](m51.9.md)'s doc-cost judgement**, and
the number is why: it is 3,556 bytes charged against every `/work phase` resume,
while this file's realized read ratio had fallen to 0.71 — evidence that the part
of it doing planning work was being paid for and skipped. Nothing was dropped.

## Decisions already taken

Recorded in full in [phase-2.md](phase-2.md#phase-2-decisions) and
[phase-3.md](phase-3.md#phase-3-decisions). Referenced by number from the
milestone files. Plan.md keeps the headings those numbers were cited under, so an
older link still lands somewhere true.
