# Milestone rules

The rules every milestone inherits, and the template its definition of done
starts from. Rules, not records: milestones, their status and each definition of
done are LNK records in [Mustur](https://github.com/DevOfPie/Mustur). These two
stayed in the tree when `phase-details/` left in W48, because no Mustur kind holds
them (LNK-D-0952, answering LNK-Q-0003).

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
| Redirects are never permanent | Never 301, never 308. Forever. A link redirect is **302**; the one exception is the verified-password POST, which answers **303** so the browser is required to drop the method and body — LNK-M-0040's reopening, D94. *(Label amended 2026-08-04: it read "Redirects are 302", which stopped describing the tree the moment 303 landed. The consequence — never permanent — is unchanged and is what the rule asserts.)* |
| Cache is optional | Redis absent or down degrades behaviour; nothing correctness-critical depends on it. Tested with Redis off. |
| Privacy stance | No IP column anywhere. `ip_prefix` only (/24 v4, /48 v6). Region and city resolve transiently and are never stored. **The stance is about storage, and Redis is the exception it does not reach** (F57): the shared rate limiter's key is `lc:rl:<bucket>:<client>`, which for IPv4 is the **full address**, held for the limiter's window — 120 seconds by default. It is not a column and the stance is not violated; it is also not nothing, because `LINKCTRL_REDIS_URL` may point at a managed service that snapshots by default. Anonymising the v4 side to /24 is **not** available: the limiter would then let one host throttle 255 neighbours, and the /64 used for IPv6 is evasion resistance rather than privacy. Keyed hashing is the only coherent fix and is not built; `docs/deployment.md` tells operators to disable persistence instead. |
| Every UI feature has API support | Both call the same service layer. New operations land in `api/openapi.yaml` and are replayed by the contract test. |
| Dormant structure is jsonb | Until the feature that uses it arrives. |
| Partitioning | `PARTITION OF` never appears in sqlc-visible SQL; partitions are created by application code. |
| DDL is additive | Within a minor version. |
| Permissions | A new permission needs a seed migration that inserts *and* grants it (the 00800 pattern). Delegability follows decision D18 — non-delegable when reading it exposes an actor's identity tied to network data, or when holding it lets a key widen its own reach; delegable otherwise. The milestone records which limb it matched, or that it matched neither. `NonDelegableScopes` is the only mechanism for **whether a key may hold a permission at all**. Three narrower ones govern what a key may do with the permissions it legitimately holds, and each exists because the permission map cannot express *this credential is not the person*: what a key may **produce** — D43 caps the role a key-issued invitation may carry, and the same cap sits on role assignment against an existing membership (`team.ChangeRole`, `team.Grant`), because a key that manufactures an interactive principal has stepped around the map rather than through it; what a key may **see** — a **pinned** key's reads are bounded to the organization it was issued for where a session's are not, because the workspace switcher exists to cross organizations and such a key was issued for one (F103); an *account-wide* key is bounded only by the organizations it has been **cut out of** — M54 removed the pinned premise rather than the rule, so such a key reads about the tenants it works in, and a reach revocation is exactly what stops one being a tenant it works in (F183); and whether the **session or the key is the authority** — `requireSessionActor` refuses operations whose subject is the person, meaning their password, their other sessions and where their browser lands, and rotation refuses the inverse, because a key replacing itself is authorized by its own token (D87). Anything branching on credential type outside those four is still a defect. *(Amended 2026-08-02, owner-answered, on LNK-M-0026's reopening — it read "`NonDelegableScopes` is the only mechanism; nothing branches on whether the caller holds a session". See the LNK decisions.)* *(Amended again 2026-08-05, owner-answered, on LNK-M-0052's F104 — it named two mechanisms and called everything else a defect, while eleven sites branched on credential type and every one was correct. The owner chose the enumerated form over a rule stated as a test, knowing the enumeration is what drifted twice. See the LNK decisions.)* *(**The *see* limb has been amended twice for one drift**, facts both times: 2026-08-08 at LNK-M-0069 it described a bound the tree no longer applied to every key, a key having stopped being issued for one organization; 2026-08-10 at LNK-M-0075 it then read "an *account-wide* key is not bounded", which LNK-F-0183 falsified inside that same milestone. The limb is unchanged and still enforced at `Service.Workspaces`, which now asks `Identity.keyReaches` — one predicate over both reaches. `Identity.APIKeyOrgID` still tells the two apart, inside it rather than at the call site, so the count of sites branching on credential type is what it was. See LNK-D-0155 to LNK-D-0158.)* |
| `ui` stays stdlib-only | No Node, no CDN, CSP unchanged, no `unsafe-` waivers. |
| Both themes, from LNK-M-0020 on | New UI colors use the theme tokens; M24.5's template scan fails raw palette utilities. |
| Touching the redirect path | Re-run the [docs/slo.md](../slo.md) k6 measurement on the built image; cached p99 stays under 20ms. |
| A test that passes first try | Sabotage it, confirm it fails, restore by counter-edit. |
| A new feature somebody can *see* | Extend the demo seeder (`cmd/lctl/demo.go`, and `demo_phase2.go` beside it) so the demo instance shows it. A feature only reachable by building the state yourself is one nobody evaluating this product will find. Does not apply to work with nothing to look at — a timeout bound, an invalidation path, a permission nobody exercises directly. **Enforced since LNK-M-0038**: `demoCoverage()` in `cmd/lctl/demo_coverage_test.go` enumerates what the demo must show and fails when a listed feature has no seeded rows, so a milestone that seeds nothing adds a row there or breaks the build. Rows that assert *zero* for a milestone not yet built are turned into real rows when that milestone lands, rather than deleted. **The number of them is deliberately not written here.** It read *four* from LNK-M-0038 until 2026-08-05, was true when written, and was wrong within one milestone — M34 converted its own row exactly as this rule instructs and the count beside the rule did not move with it. By M45 every one had been converted and the sentence described nothing that existed. Saying *four* was a fact nothing kept true; saying *its trailing rows* is a fact that stays true however many there are (F69). If the obligation ever proves too heavy, narrow the list deliberately and in writing; never delete the test. |

**Which of them Phase 3 inherits was confirmed on 2026-08-07**, one at a time,
and the confirmation — a table naming, for each rule, the milestone that tests
it and how — is in
[phase-3-candidates.md](phase-3-candidates.md#phase-3-inherits-all-fourteen-confirmed-2026-08-07).
**It moved there on 2026-08-08, at LNK-M-0066's doc-cost judgement**, and
the number is why: it is 3,556 bytes charged against every `/work phase` resume,
while `phase-details/README.md`'s realized read ratio had fallen to 0.71 — evidence that the part
of it doing planning work was being paid for and skipped. Nothing was dropped.

## The template

A new LNK work unit (`mustur add work-unit --project LNK`) starts from this. The
rules above are **not** repeated per unit.

```markdown
# M<N> — <title>

**Depends on:** LNK identifiers, `LNK-M-0030`-style, or "nothing". Say when an edge is an
*ordering preference* rather than a hard dependency — a soft edge presented as a
hard one hides a scheduling choice.
**Discharges:** the Plan.md scope row(s), known limitation(s), or repo promise
this closes. If it closes none, say so rather than inventing one.

One or two sentences on what this is and why it sits here. Not a restatement of
the title.

## Done means

Verifiable claims, in the Phase 1 idiom: things a skeptical reviewer can check,
not intentions. Each bullet should be falsifiable.

- Prefer "asserted by test" over "is correct".
- Name the test or file where one already exists.
- A `file:line` citation claims the tree at this milestone's own commit and
  nothing later (W39). One written after the milestone landed carries the
  commit — `gates.go:167` at `abc1234` — and a symbol name beats a line number
  where one exists, because line numbers rot on every insertion above them.
- If a number is claimed, say where it was measured.
- If something is deliberately *not* done, say that here rather than leaving its
  absence to be discovered.

Anything touching the redirect path must state how the <20ms cached SLO is
re-verified. Anything adding a destination-writing surface must state that it
passes LNK-M-0030 (M30)'s tier check. Anything emitting audit events depends on
LNK-M-0016 (M21); anything notifying depends on LNK-M-0017 (M22).

## Risks

What could go wrong, or what is genuinely unknown until the work starts. "Low" is
an acceptable answer; an empty section is not, because it reads as "not
considered" rather than "considered and small".
```
