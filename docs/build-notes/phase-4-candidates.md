# Phase 4 candidates

What Phase 4 might take, and the decisions already taken about it. Same job as
[phase-3-candidates.md](phase-3-candidates.md), which stays where it is: its
*What Phase 3 shipped, and what is still on this list* section is the inventory
this file draws from, and nothing here restates it.

**Written on 2026-08-18, before any milestone exists.** Everything below was
answered by the owner in one planning conversation on that date. It is recorded
here rather than in [decisions.md](decisions.md) because no milestone uses it
yet — the answers get `D` numbers when the milestones that rest on them land,
carrying the date they were *given* as well as the date they were used, which is
the convention [upcoming-decisions.md](upcoming-decisions.md) states and this
file follows.

---

## The spine: add-ons

**Phase 4's top target is add-on support, and it is the phase's foundation
rather than one of its features.** Owner-set 2026-08-18.

The motivation is stated by the owner and is not this file's to re-derive: the
platforms this product is meant as an alternative to have add-on support that is
limited and difficult to manage, or none at all, and that is what made them
unviable for use cases a module could have solved. **Area G's commercial add-on
is one consumer of this infrastructure, not the thing itself** — the owner has
planned for the commercial module and wants operators to be able to add modules
of their own.

**Absence established 2026-08-18**, per [planning.md](planning.md)'s first step:
nothing in the tree is an add-on, a plugin, or an extension point. What exists
and is adjacent is `internal/webhook` (outbound, one-way), `internal/automation`
(rules over this product's own vocabulary), `internal/feed` (an inbound
third-party reputation feed, config-driven) and `internal/update`. None of them
lets anything be *added*.

### The answers that set its shape

All owner-set 2026-08-18, in one conversation, and each was put with options,
costs and a recommendation. Two answers went against the recommendation and are
marked, because a recommendation overruled is worth more on the record than one
followed.

| Question | Answer |
| --- | --- |
| **How does an add-on get into a running instance?** | **WASM modules loaded at runtime**, against sidecars, compile-time modules and deferring the mechanism |
| **How much reach?** | **Everything OIDC needs** — routes, migrations, templates, config, a hook on session mint — **proven by building it** |
| **What is an add-on trusted with?** | **Declared permissions, enforced.** An add-on names the tables, events and routes it needs and the host refuses the rest |
| **May an add-on run on the redirect path?** | **Yes** — see below; the answer reframed the question |
| **Who owns an add-on's tables?** | **A Postgres schema per add-on**, host-run migrations |
| **Load failure: degrade or refuse?** | **The add-on declares which it is.** Anything on the authentication path defaults to required |
| **ABI compatibility promise?** | **SemVer with deprecation windows** — *against the recommendation*, which was path-versioning like `/api/v1` |
| **Who owns the ABI definition?** | **The host.** Add-ons consume a generated, versioned SDK, so the contract has one author |
| **Does `LinkCtrl-OIDC` run this repository's process?** | **The same gates, no phase loop.** CI green, tests sabotage-verified, a changelog and a checksummed release — but no milestone table and no phase reviews |
| **How is an add-on's build verified?** | **Both** published provenance and load-time verification — they cover different attacks |
| **What licence does `LinkCtrl-OIDC` take?** | **MIT**, matching the product |
| **What does 1.0 mean now?** | **That the add-on contract is stable**, rather than that identity is built in |
| **Areas C and D?** | **Deferred to Phase 5** — *this supersedes the earlier answer in the same conversation*, which took A, C and D |
| **The commercial module?** | **Phase 5**, once the foundation has been through a real consumer |

**Two answers carry owed work that nothing else in this file names.** SemVer
needs a deprecation policy, which does not exist and which the phase that writes
the ABI has to write with it — *is this minor or major* is a judgement call, and
the shellcheck argument that cost nine days of red CI was exactly that shape.
And *the add-on contract is stable* as a 1.0 gate means
[releasing.md](../releasing.md)'s pre-1.0 sentence is rewritten rather than
satisfied.

### The redirect path: the answer reframed the question

Four options were put — observe-only, inline under a budget, never, or defer.
The owner took none of them and set the boundary somewhere better:

> *"We are only responsible for maintaining the core redirect promise, if an
> add-on ruins that is on the add-on and does not ruin our promise."*

**So an add-on may run inline, and the published promise is scoped to core.**
That is a change to a claim this product already makes in public —
[slo.md](../slo.md), `docs/SECURITY.md` and `README.md` all state the measured
figure — and restating them as *core, with no add-on on the path* is work this
phase owes rather than a consequence it can leave implicit.

Three requirements come with it, two of them the owner's own:

1. **Two declaration classes, not one.** An add-on declares whether it is in the
   async observe-only part of the redirect path or in the actual path. They are
   different permissions and a module cannot acquire the second by accident.
2. **Add-on performance is auditable, per module.** Owner's reason, stated
   plainly: so an operator can efficiently track a problem down and report it to
   the right team, *"to minimize the flack we receive if poor performance exists
   due to add-on issues"*. That makes per-module timing attribution a
   first-class requirement of the foundation rather than observability polish.
3. **A runtime-enforced deadline, because performance and availability are
   different questions.** The answer above settles latency — an add-on's is its
   own. It does not settle a module that never returns, which is not a slow
   redirect but a hung one. The WASM runtime interrupts an overrunning module,
   the redirect completes without it, and the Add-on manager reports what was
   killed and how often. The instance's availability stays this product's
   promise; the add-on's latency does not.

### The Add-on manager is a real surface, not a settings page

Owner-set 2026-08-18, and it is where several answers above become visible to
somebody:

- installing and removing add-ons, and which declaration class each holds;
- **per-module performance against the redirect path**, which is requirement 2
  above and the reason the manager exists at all;
- **orphaned data named explicitly** — a schema left behind by a removed add-on
  is listed rather than merely present;
- **an option to remove an add-on's data at the moment the add-on is removed**,
  so purging is a choice offered at the point of decision rather than a chore
  discovered later.

### What this collides with, named now rather than discovered

**None is a blocker; every one is a milestone's argument to make in writing.**
And the frame was owner-confirmed at the plan's review: **Phase 4 inherits all
fourteen rules as written**, the collisions staying arguments each milestone must
win in its own file, never waivers.

**This list is a record of what was weighed when the phase was planned, and it
was wrong in both directions** (F301). It said each entry is an inherited
invariant from [phase-details/README.md](phase-details/README.md)'s *What every
milestone inherits*, and **`Single container is a tested configuration` is not
among those fourteen at all** — it is a real property of this product, gated by
`scripts/single-instance-check.sh`, and it is not one of the inherited rules the
sentence claims to be quoting. It also said *five*, and stopped being updated:
[M64](phase-details/m64.md) engaged a sixth, deferring *every UI feature has API
support* to M69 and arguing it in its own file, which is exactly where the
pointer requires such an argument to live.

So this list is not an enumeration of what the phase touched. What it is, and all
it ever was, is the four collisions somebody could see on 2026-08-18 plus the one
the review added. The frame above is what was confirmed; the count below was
never re-counted, and the honest form of that is to say so rather than to keep a
number nobody maintains.

- **`ui` stays stdlib-only** — no Node, no CDN, CSP unchanged, no `unsafe-`
  waivers. An add-on that renders UI has to reach the page without moving any of
  that.
- **Single container is a tested configuration**, gated by
  `scripts/single-instance-check.sh`. A WASM host keeps that true where a sidecar
  model would not, which is part of why the answer went this way — but the gate
  has to grow a case that says so with an add-on loaded.
- **DDL is additive within a minor version.** An add-on that owns tables owns
  migrations, and *whose* additive-ness that is has no answer yet.
- **The redirect tree stays minimal**, and touching it re-triggers the
  [slo.md](../slo.md) k6 measurement. Whether an add-on may run on the redirect
  path at all is **open**, below.
- **The privacy stance** — no IP column anywhere, `ip_prefix` only — meets a
  storage-holding add-on that watches redirects, which nothing in the first
  four collisions covers. *Added at the plan's review, 2026-08-18.* The plan's
  answer is [M61](phase-details/m61.md)'s: the stance binds **at the ABI, not
  by auditing add-on DDL** — no host function hands a module a raw client
  address, so an add-on cannot store what it is never handed, asserted by a
  test over the ABI surface rather than promised by review vigilance.

---

## The phase's shape

| Decision | Answer | Notes |
| --- | --- | --- |
| **Which areas?** | **Add-ons as the spine, and A (identity) expressed through them.** C and D were taken and then **deferred to Phase 5** later in the same conversation | B, E and F are not taken either. C and D have now waited three phases and will wait a fourth — recorded as a change of mind rather than reconciled away, because the first answer is what the arithmetic below was computed against |
| **Version** | **Another 0.x** | [releasing.md](../releasing.md) ties 1.0 to identity being complete. See the OIDC row below, which changes what that sentence will mean |
| **Size target** | **Raise the cap to 18; plan to 15** | Phase 2 ran 33, Phase 3 ran 23 against a plan of 15 with eight insertions. The cap moves once, deliberately, and the planning number stays where the last two phases put the pressure |
| **Process debt** | **One milestone, early in the phase** | [F248](deferred-findings.md#closed), [F253](deferred-findings.md#closed), [F254](deferred-findings.md#closed), [F255](deferred-findings.md#closed). Early, because F255 is *nothing asks whether CI is green* and the phase should not run without that gate |

### OIDC moves out of core, and that is the phase's biggest structural change

Owner-set 2026-08-18, revisiting their own first answer: *"OIDC may be better off
in an Add-on to allow more room for un-expected options and future changes to the
space as well as lowering the load if operators have no use/infrastructure for
it."*

Two things follow, and they pull in opposite directions.

**It collapses the arithmetic.** The floor put to the owner was 12–15 milestones
before add-ons and before a single insertion, on the reading that Area A meant
OAuth, OIDC, SSO and SCIM in core at four to six milestones. With identity
expressed as add-ons, core's share of Area A drops to whatever the host needs to
support it, and the phase fits its own plan.

**It raises the bar on everything else.** An add-on that can be an
authentication provider is the strongest possible statement of the reach
question, and it is now the acceptance test for the whole foundation rather than
a feature beside it. If the OIDC add-on cannot be built, the foundation is wrong,
and that is the point of building it inside the phase that designs the seams.

**And it moved what 1.0 means, which was put to the owner and answered.**
`docs/releasing.md` says the product stays pre-1.0 while there is no SSO, OAuth,
OIDC or SCIM. **The gate is now that the add-on contract is stable** — a more
honest promise, and the one an operator actually needs before committing to build
against it. The cost was stated when it was offered and is taken: somebody
tracking 1.0 for SSO sees it arrive as an optional module they install and verify
themselves, and that sentence in releasing.md is rewritten rather than
satisfied.

---

## The OIDC add-on has a repository of its own

`DevOfPie/LinkCtrl-OIDC`, created by the owner on 2026-08-18, **public**,
default branch `main`, currently a README and nothing else. Read rather than
assumed: `gh repo view` and its contents listing, the same day.

**This is a bigger fact than it looks and it lands before any milestone.** The
reach answer above makes a first-party OIDC add-on the acceptance test for the
whole foundation — *a seam nobody has built through is a seam that does not
work* — and that test now **spans two repositories**. Three consequences follow
immediately.

**The host ABI is a published cross-repo contract from day one, not eventually.**
Open question 4 below asks what the ABI's compatibility promise is; a
second repository building against it turns that from a question the phase can
answer late into one it has to answer before the add-on repo can hold anything.
There is no version of *we will firm it up later* that works across a boundary
somebody else is already compiling against.

**Neither repository can hold the other's record.** This repository's contract —
milestones, `decisions.md`, deferred findings, the phase loop — is not the add-on
repo's, and the add-on repo's code is not this one's. That is the same split
`~/repos/DevOfPie/CLAUDE.md` already documents for another project, for the same
reason, and it is worth reading before this one invents a different answer.

**And it is public with no licence.** This repository is MIT; `LinkCtrl-OIDC`
has no `licenseInfo` at all, which for a public repository means default
copyright — nobody may use it. If the add-on is meant to be the worked example
operators copy, that is a one-file fix and it is the owner's to make.

---

## What is still open

**The ten questions this file opened on 2026-08-18 were all answered the same
day**, in three rounds, and their answers are in the table above. What follows is
what those answers created rather than what they left.

**Owed work, not open questions.** Each of these is a consequence with no
decision left in it, named so the milestone that meets it does not rediscover it:

1. **The SLO claim is restated as core-only** in [slo.md](../slo.md),
   `docs/SECURITY.md` and `README.md`. It is a published measurement and it is
   about to stop being true of an instance with an inline add-on. **Discharged by
   [M66](phase-details/m66.md) in two of the three, and the third is deliberate**:
   slo.md now opens by scoping every figure in it to core with no inline add-on on
   the path and carries both runs — core unmoved, and a module that never returns
   — while `docs/SECURITY.md` gains a row saying the same thing and what stays this
   product's, which is availability. `README.md` is **not** in that diff, because
   D104 keeps it describing the *released* product and add-ons are not released
   until the tag; `CHANGELOG.md`'s `[Unreleased]` carries the rescoping until
   [M70](phase-details/m70.md)'s documentation pass moves it, which m66.md states
   so the close does not rediscover it.
2. **A deprecation policy is written with the ABI**, because SemVer without one
   is a version number and a hope. **Discharged by
   [M61](phase-details/m61.md)**: [docs/addon-abi.md](../addon-abi.md) states what
   counts as breaking as a table rather than a judgement, fixes the minimum window
   at two minor releases and 90 days whichever ends later, and names the four
   places a deprecation is announced — one of them the SDK's generated Go
   `Deprecated:` markers, so a deprecation reaches a consumer's editor and not only
   a changelog.
3. **[releasing.md](../releasing.md)'s pre-1.0 sentence is rewritten**, since
   1.0 now turns on the add-on contract rather than on identity being built in.
4. **`LinkCtrl-OIDC` gets an MIT `LICENSE`.** It is public and unlicensed today,
   which means nobody may use it. Not this repository's file, so it is the
   owner's to add.
5. **The single-instance gate grows a case with an add-on loaded**, or *one
   container is a tested configuration* quietly stops covering the shipped shape.

**Genuinely open at planning — each now routed, none silently.** The four
questions this section held when it was written on 2026-08-18 were taken up by
the plan the same day ([D211](decisions.md#2026-08-18--phase-4-planned-the-spine-and-the-fourteen-slots)):

- **What the deadline is** — deliberately *not* answered: no data exists until
  something runs, so the value is measured into at
  [M66](phase-details/m66.md), and the question waits in
  [upcoming-decisions.md](upcoming-decisions.md) with the shape of its answer
  fixed in advance.
- **What the host functions actually are** — [M61](phase-details/m61.md)'s
  central artifact, named there as the hardest of the phase. **Answered**: ten
  functions in `internal/addon/abi`, six capability groups, one wasm module named
  `linkctrl`, one calling convention for all of them. Three are live — `log`,
  `config_get`, `abi_version` — and seven are declared and refused with a status a
  module branches on, because the add-on repository compiles against the boundary
  from its first commit. The list itself is the ABI: the SDK, the documented table
  and the host module the runtime registers are all derived from it.
- **How declared permissions are expressed and checked** —
  [M62](phase-details/m62.md), which examines the `NonDelegableScopes` analogy
  and records the answer either way.
- **How many milestones this is** — **fourteen**: eleven integers, two
  reviews, one close, M59–M70, in
  [Plan.md](../../Plan.md#phase-4-build-plan)'s ordering table — thirteen as
  drafted, fourteen after the plan's independent review split the manager
  milestone in two and the owner took the split. Inside the plan of 15 for
  the first time in three phases, and the remaining slack is deliberate
  rather than a shortfall: an ABI is the kind of artifact insertions come
  from.

---

## Two more answers, given at the plan's review

**Owner-answered 2026-08-18**, when the drafted plan was put to them — same
convention as the table above: recorded here, `D` numbers when
[M59](phase-details/m59.md) lands, options and costs stated when asked.

| Question | Answer |
| --- | --- |
| **[F253](deferred-findings.md#closed): the direct `release-check` form skips the integration tests — script or docs?** | **The script derives `COMPOSE_PROJECT_NAME` and `COMPOSE_ENV_FILES` itself**, the recommended shape, taking the stated cost: a new drift pair between Makefile and script, which M59 adds a check for. The alternative — docs drop the direct form — left the trap runnable and merely unrecommended |
| **[F254](deferred-findings.md#closed): which shape ends the fold/tag conflict?** | **The release-time gate is named in workflow.md's Docs row**, the recommended shape, taking the stated cost: the conflict is documented rather than removed, and a post-fold reopening still re-folds by hand. The losing shapes: fold-at-the-close (phase-loop grows a step and post-close reopenings still hit the window), and release-check folding it itself (a gate that edits the tree it checks, date-checking a date it wrote) |
| **Does Phase 4 inherit all fourteen rules as written?** | **Yes, all fourteen** — the five collisions above stay per-milestone written arguments, not waivers. A milestone that cannot win its argument comes back as a prompt |

## The manager's layout: chosen from wireframes, amended, confirmed

**Owner-chosen and confirmed 2026-08-18** — the D177 route — against a visual
plan carrying three drawn options with costs beside each: A (one flat table),
B (card per add-on, the recommendation), C (list + detail). The choice went
against the recommendation: **Option A's table as the list page, Option C's
detail pages behind each row**, then two amendments made and redrawn in the
same review before the owner confirmed the final frames.

**Removal is select-mode** — not per-row, and not the dropdown the first
redraw offered: Remove sits in-line with Install; pressing it turns each
row's trailing `›` into a checkbox **in the same column, so the table never
shifts**; multiple rows select; and the same button — now *Remove selected
(n)* — opens one confirmation carrying a purge-data choice per selected
module, default off, with a required-class module's consequence stated.

**The detail page gains a Settings section, and that is owner-added scope,
named as such**: settings the add-on declares in its manifest, each with a
typed input the host renders — text, secret, select, toggle — saved behind
`addons.manage`, audited, secrets never echoed. The drafted plan had kept
add-on configuration in the operator's environment; this reverses that for
*declared* settings only, and it was amended into
[M60](phase-details/m60.md) (manifest), [M61](phase-details/m61.md) (ABI
reads) and [M68](phase-details/m68.md) (render and save) after the plan's
independent review, at the owner's direction, on 2026-08-18.

The confirmed frames, compressed to their essentials (the drawn versions live
in the planning visual plan; this record is what the repo keeps):

```
List                            [Install add-on] [Remove…]
NAME        VER    CLASS             FAIL      P99   KILLS
oidc        1.2.1  none              required  —     —    ›
clickstats  0.4.0  redirect-observe  degrade   0.8ms 2    ›
Orphaned data: addon_legacy_geo (2.4 MB)         [Purge…]

Selection mode (Remove pressed — › cells become checkboxes)
Select the add-ons to remove  [Remove selected (2)] [Cancel]
oidc        1.2.1  none              required  —     —    ☑
clickstats  0.4.0  redirect-observe  degrade   0.8ms 2    ☑

Confirmation (one or many; purge per module, default off)
Remove 2 add-ons?
  oidc v1.2.1 — required-class: external sign-in stops
    ☐ also delete addon_oidc
  clickstats v0.4.0
    ☐ also delete addon_clickstats
                          [Cancel]  [Remove 2 add-ons]

Detail page: ← back · name + class/failure badges ·
Performance (own p99, kills) · Settings (text / secret /
select / toggle inputs + Save) · Declared permissions ·
Data (schema, size) + [Remove add-on…]
```

`D` numbers when M68 lands, like every answer above.

## What is not in Phase 4

**Deferred to Phase 5 by the owner in the planning conversation itself** —
recorded in the shape table above and repeated here because this is the list a
reader checks: **Areas C and D** (analytics/reporting and redirect-path
work, waiting their fourth phase, recorded as a change of mind rather than
reconciled away) and **the commercial module** (Phase 5, once the foundation
has been through a real consumer).

Carried from [Plan.md](../../Plan.md#not-in-phase-3), which is where their
reasons live and stay:

- Filing a link into a folder by typing its path — the one thing round two called
  irritating with no defect behind it.
- Switching organizations anywhere except the workspace dropdown.
- Moving links between workspaces, and the *All Workspaces* scope in
  [upcoming-decisions.md](upcoming-decisions.md) that shares its hard part.
- Grouping API-key scopes by the object they act on.

### Provisioning from an add-on's assertion — deferred by M65, on purpose

**[M65](phase-details/m65.md) ships linking-only**, and this is where its own
bullet says the other half is recorded so that the phase which wants it does not
have to rediscover the shape.

An add-on that holds `session.mint` asserts *this external subject
authenticated*. Today the host answers by looking the subject up in
`addon_identity_links` and minting nothing when there is no row: an account is
reached, never created. **Whether an unknown external subject may become a new
account is a separate question**, and it is a policy one rather than a
mechanical one:

- It has to answer to `LINKCTRL_SIGNUP_MODE` ([D38](decisions.md)), which is the
  operator's and not an add-on's. `closed` means closed, and an identity provider
  that could create accounts under it would be a way around the setting rather
  than a feature beside it.
- It has to say which organization and which workspace a provisioned account
  lands in. Phase 2's signup section is the precedent and the reason this is not
  a one-line answer: a self-registered account gets an organization and a
  workspace of its own, which is a tenancy decision somebody has to have made.
- It has to say what an operator sees. An add-on that can create accounts can
  create them faster than anybody reads an audit log.

**Not blocked on anything** — the linking table, the assertion path and the
provenance record are all built and are what provisioning would be written on
top of. What it needs is the decision, and the decision is the owner's.

**Areas B, E and F are not taken**, and their surviving rows are in
[phase-3-candidates.md](phase-3-candidates.md)'s close-out section rather than
copied here. *We stopped caring about this* remains the decision this project
keeps losing, so nothing above is dropped — only unscheduled.
