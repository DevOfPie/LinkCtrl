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

Each of these is an inherited invariant from
[phase-details/README.md](phase-details/README.md)'s *What every milestone
inherits*, and a WASM host touches four of them. **None is a blocker; every one
is a milestone's argument to make in writing.**

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

---

## The phase's shape

| Decision | Answer | Notes |
| --- | --- | --- |
| **Which areas?** | **Add-ons as the spine, and A (identity) expressed through them.** C and D were taken and then **deferred to Phase 5** later in the same conversation | B, E and F are not taken either. C and D have now waited three phases and will wait a fourth — recorded as a change of mind rather than reconciled away, because the first answer is what the arithmetic below was computed against |
| **Version** | **Another 0.x** | [releasing.md](../releasing.md) ties 1.0 to identity being complete. See the OIDC row below, which changes what that sentence will mean |
| **Size target** | **Raise the cap to 18; plan to 15** | Phase 2 ran 33, Phase 3 ran 23 against a plan of 15 with eight insertions. The cap moves once, deliberately, and the planning number stays where the last two phases put the pressure |
| **Process debt** | **One milestone, early in the phase** | [F248](deferred-findings.md#open), [F253](deferred-findings.md#open), [F254](deferred-findings.md#open), [F255](deferred-findings.md#open). Early, because F255 is *nothing asks whether CI is green* and the phase should not run without that gate |

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
   about to stop being true of an instance with an inline add-on.
2. **A deprecation policy is written with the ABI**, because SemVer without one
   is a version number and a hope.
3. **[releasing.md](../releasing.md)'s pre-1.0 sentence is rewritten**, since
   1.0 now turns on the add-on contract rather than on identity being built in.
4. **`LinkCtrl-OIDC` gets an MIT `LICENSE`.** It is public and unlicensed today,
   which means nobody may use it. Not this repository's file, so it is the
   owner's to add.
5. **The single-instance gate grows a case with an add-on loaded**, or *one
   container is a tested configuration* quietly stops covering the shipped shape.

**Genuinely open, and the phase's planning has to answer them.**

- **What the deadline is.** A number bounding an inline add-on exists in
  principle and has no value yet, and there is no data to pick one from until
  something runs.
- **What the host functions actually are.** *Everything OIDC needs* is a
  requirement, not an interface. The set of imports is the ABI, and it is the
  hardest single artifact of the phase.
- **How declared permissions are expressed and checked.** The shape is
  `NonDelegableScopes`; whether that generalises to tables, events and routes is
  unexamined.
- **How many milestones this is.** With C, D and the commercial module out, the
  phase is the foundation, the OIDC add-on, one process-debt milestone, two
  reviews and a close. That is well inside the plan of 15 for the first time in
  three phases, and the slack is deliberate rather than a shortfall: an ABI is
  the kind of artifact insertions come from.

---

## What is not in Phase 4

Carried from [Plan.md](../../Plan.md#not-in-phase-3), which is where their
reasons live and stay:

- Filing a link into a folder by typing its path — the one thing round two called
  irritating with no defect behind it.
- Switching organizations anywhere except the workspace dropdown.
- Moving links between workspaces, and the *All Workspaces* scope in
  [upcoming-decisions.md](upcoming-decisions.md) that shares its hard part.
- Grouping API-key scopes by the object they act on.

**Areas B, E and F are not taken**, and their surviving rows are in
[phase-3-candidates.md](phase-3-candidates.md)'s close-out section rather than
copied here. *We stopped caring about this* remains the decision this project
keeps losing, so nothing above is dropped — only unscheduled.
