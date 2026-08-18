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

### Three answers that set its shape

| Question | Answer | What it costs, as put at the time |
| --- | --- | --- |
| **How does an add-on get into a running instance?** | **WASM modules loaded at runtime.** Offered against sidecar services, compile-time modules, and deferring the mechanism | A real runtime dependency in the core image; a host-function ABI that must be versioned **forever**; and database access, route registration and UI rendering all become explicit host calls rather than things an add-on inherits |
| **How much reach does the foundation grant?** | **Everything OIDC needs — routes, migrations, templates, config, and a hook on session mint — proven by building a first-party OIDC add-on**, because a seam nobody has built through is a seam that does not work | The widest surface committable in one phase, and an add-on that can mint a session can impersonate anybody — so the trust model is owed in the same phase rather than after it |
| **What is an add-on trusted with?** | **Declared permissions, enforced** — an add-on names the tables, events and routes it needs and the host refuses the rest, on the shape `NonDelegableScopes` already gives API keys | Enforcement is only real on a mechanism that can enforce, which is what makes the sandbox load-bearing rather than optional |

**The three are mutually consistent, and that is worth stating because two of the
alternatives were not.** Enforced permissions require a mechanism that can
enforce them, which rules out compile-time modules; a reach that includes minting
sessions rules out treating add-ons as untrusted-but-unsandboxed. WASM is the
only offered mechanism that carries both answers.

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
| **Which areas?** | **A** (identity), **C** (analytics and reporting), **D** (redirect path and routing) — plus add-ons as the spine | B, E and F are not taken. C and D have now waited three phases |
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

**And it moves what 1.0 means.** `docs/releasing.md` says the product stays
pre-1.0 while there is no SSO, OAuth, OIDC or SCIM. If those ship as add-ons, that
sentence needs rewriting rather than satisfying — *is a capability available as an
add-on the same as the product having it* is a question about the version number,
and it is **open**.

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

## Open questions

Nothing below has been answered, and each is named because the loop will stand
still on it.

1. **May an add-on run on the redirect path?** The invariant is that the tree
   stays minimal — no session lookup, no CSRF check, no template rendering — and
   the SLO is measured at p99 under 20ms cached. A hook there is the most useful
   place an add-on could sit and the one place the product has a measured promise.
2. **Who owns an add-on's tables, and what happens when it is removed?** DDL is
   additive within a minor version. An add-on's migrations are not this
   product's, its removal leaves rows behind, and *additive* has no meaning
   across a boundary the operator controls.
3. **Does an add-on's failure degrade or refuse?** The cache-is-optional rule
   says Redis absent degrades and nothing correctness-critical depends on it. An
   OIDC add-on that fails to load is a sign-in path that does not exist, which is
   not a degradation.
4. **What is the ABI's compatibility promise?** The REST API is versioned by path
   and `v1` never changes. A host-function ABI is a second forever-contract, and
   the phase that writes it decides how it is versioned.
5. **Does *available as an add-on* satisfy the 1.0 gate?** See above.
6. **Which of C and D's rows does the phase actually take?** Both areas are taken
   in principle; neither has been cut to milestones. D's rows each owe the k6
   re-measurement, which is a cost per milestone rather than per area.
7. **Where does the commercial module sit relative to the foundation?** It is the
   named first consumer, but whether it lands in Phase 4 or waits for a
   foundation that has been used once is not decided.
8. **Which repository owns the ABI's definition, and how does a change to it
   reach the other?** The host is here; the first consumer is not. A contract
   with two owners and no stated direction is one that drifts.
9. **Does `LinkCtrl-OIDC` run this repository's process, a lighter one, or its
   own?** Phase loops, adversarial reviews and an append-only decision log are
   this product's discipline, not a law of nature, and imposing them on a single
   add-on may cost more than it buys.
10. **How is an add-on's build reproduced and verified?** This repository
    cross-compiles five platforms, checksums them, and publishes a multi-arch
    image with provenance and an SBOM. A `.wasm` artifact somebody installs
    deserves an answer of the same shape, and it does not have one.

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
