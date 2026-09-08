# Phase 4 — the milestones

Released as **0.4.0** on 2026-09-08. History, kept for the reason Phase 1,
2 and 3's records are: the build plan and decisions below are what the shipped
milestones were judged against, and moving them out of the live path is what
keeps `/work phase` from resuming into a phase that already closed.

Eighteen milestones, M59 through M70, against a plan of fourteen. What the four
additions were and why the build needed them is in the section below, which came
with the table.

Every milestone file named here is still in this directory. Only the status
table moved: [README.md](README.md) carries the **live** phase and nothing else.

Planned 2026-08-18 — fourteen milestones, M59–M70; the ordering table and its
dependency edges are [Plan.md](../../../Plan.md#phase-4-build-plan)'s. **Eighteen
as built**, and the four additions are named rather than counted: [M66.5](m66.5.md)
on 2026-08-23, after measuring what M66's inline class costs a visitor when
nothing is pooled; [M68.5](m68.5.md) and [M68.6](m68.6.md) on 2026-08-25, when
M69's validation found the foundation cannot make an outbound request at all
([F334](../deferred-findings.md#closed)) and the owner corrected a constraint M67
had shipped on nobody's decision. [M69.5](m69.5.md) on 2026-08-27, when
building the OIDC add-on found that the flow it proves is reachable only by
being handed a URL ([F345](../deferred-findings.md#closed)). All four are
milestones the build turned out to need rather than optimistic planning, which
is the case
[planning.md](../planning.md#the-size-target-a-phase-stays-under-sixteen-milestones)'s
2026-08-11 clarification allows. **Eighteen is past the planning target of
fifteen and **at** the cap of eighteen, so there is no slack left**, which is the honest way to say it. Phase 3
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
| [M68](m68.md) | The Add-on manager | M63 · M66 · M67 · M64 *(ordering)* | done |
| [M68.5](m68.5.md) | An add-on reaches outward, and only where the operator pointed it | M61 · M62 · M64 · M68 *(ordering)* | done |
| [M68.6](m68.6.md) | A module arrives from a URL, because that was always the intention | M67 · M68.5 · M68 *(ordering)* | done |
| [M69](m69.md) | The OIDC add-on: the foundation's acceptance test | M61 · M63 · M64 · M65 · **M68.5** · M68 *(ordering)* | done |
| [M69.5](m69.5.md) | Somebody can start the sign-in an add-on made possible | M64 *(ordering)* · M65 *(ordering)* · M68 *(ordering)* · M69 | done |
| [M69.9](m69.9.md) | Pre-release adversarial review | everything below it | done |
| [M70](m70.md) | Deferred findings, documentation pass, 0.4.0 | all | done |
