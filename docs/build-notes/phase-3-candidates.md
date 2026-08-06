# Phase 3 candidates

What Phase 3 might take, grouped by **work area**. Nothing here is scheduled:
this file is the parking destination [planning.md](planning.md#3-this-phase-or-a-future-one)
already sanctions for a future-phase feature — *"a row in Not in Phase N (or the
next phase's candidate list)"* — and the list it names.

**It restates nothing.** Almost every candidate already has a home in
[Plan.md](../../Plan.md)'s *Not in Phase 2* list, its *Other surfaces* table, or
[deferred-findings.md](deferred-findings.md), each carrying the reason it was
deferred. This file adds one thing those cannot: a **grouping**. A row here is a
pointer plus which area it belongs to, and the reason stays where it was written.

Two owner directives shape it, both 2026-08-06:

1. **A phase stays under sixteen milestones**, insertions counted — fifteen at
   most, owner-set and revisitable, now a rule in
   [planning.md](planning.md#the-size-target-a-phase-stays-under-sixteen-milestones).
   Phase 2 ran **33 milestone files**: 25 integers, M21 through M45, plus 8
   fractional insertions. Counted from the status table and the directory, not
   recalled — this said *31 and six* on first writing and both were wrong.
2. **Milestones should sit in separate work areas**, so more than one can be
   worked at a time, or so a blocked milestone has an independent one to fall
   back to. That is what the grouping is *for*, and it is why the areas are cut
   by which files a milestone would touch rather than by which feature sounds
   like which.

**The loop cannot do 2 yet.** [phase-loop.md](phase-loop.md) is built on one
milestone at a time — the worker is explicitly forbidden from starting a second,
and `.current-task.md` names one milestone, one step, one actor. Both halves of
the directive are proposed as [W33](workflow-changes.md#proposed) and neither is
built. Grouping the candidates is useful either way: the fallback half needs only
that the loop may *choose* a different independent milestone instead of stopping,
which is much cheaper than running two at once.

---

## The areas

Cut so that two milestones in different areas do not edit the same files. Where
an area's boundary is not clean, the row says so — a hidden overlap is what
would turn parallel work into a merge conflict.

### A — Identity and account lifecycle

`internal/auth`, `internal/identity`, the `002xx` migrations, `web_account.go`.

| Candidate | Where it is recorded |
| --- | --- |
| MFA, OAuth, OIDC, SSO, SCIM | Plan.md *Not in Phase 2*; Phase 3 by the scope table |
| **Account recovery — a forgotten password locks the account out permanently** | [F141](deferred-findings.md), and Plan.md *Not in Phase 2* |
| **Account deletion and GDPR erasure — the schema and four sites describe both as existing** | [F44](deferred-findings.md), and Plan.md *Not in Phase 2* |
| An API key that reaches more than one organization | [F75](deferred-findings.md), owner-directed 2026-08-05; Plan.md *Not in Phase 2* |
| A runtime signup toggle changeable from the dashboard | Plan.md *Not in Phase 2*, parked at D38, still parked after D98 gave the instance a principal |

**Two of these are defects, not features.** F44 and F141 falsify claims the tree
makes *today*, so their milestones close findings rather than add scope — which
also means they are the two candidates in this whole file that are not optional
in the way the rest are.

### B — Dashboard UI and UX

`internal/ui/templates`, `internal/httpx/web_*.go`.

| Candidate | Where it is recorded |
| --- | --- |
| **A UI/UX redesign, owner-requested for early Phase 3** | Queue row, 2026-08-06. Three complaints named: the link configuration page *"leaves a massive mess … difficult to find what you are looking for in"*; high-traffic items like retrieving a QR code are *"buried deep in the page"* alongside config that belongs in an on-demand popup; and the QR settings expose *Quiet zone* and *Module size* where an end user wants **output size in pixels** and the rest handled in the background. **It cannot be specified without the owner** — the row asks for a walkthrough, area by area, plus blind-task exercises where the owner is given a task with no instructions so the UX is judged by whether it can be done. That walkthrough is a Phase 3 planning input, and this row is where it is remembered |
| **The workspace selector should always render, not only above one membership** | Queue row, 2026-08-02. `nav.html:38` is `{{if gt (len .Workspaces) 1}}`, which is [decisions.md](decisions.md)'s M25 entry working as designed — *"a control that cannot do anything"*. What the decision did not weigh is its **label**: with one membership the current workspace and organization name appear nowhere in the shell. Not a defect, because nothing claims the control always shows. Folding it into the redesign is recommended and is the owner's call |
| Mobile navigation rework | Plan.md *Not in Phase 2* — the header hides the signed-in address below `sm` |
| Notification severity, grouping and filtering | Plan.md *Not in Phase 2* — needs a schema change M22's model has no column for |
| Trash/restore UI | Plan.md *Not in Phase 2* — Phase 1 decided against it |
| Bulk operations | Plan.md *Not in Phase 2*, a "2+" row |
| An **All Workspaces** dashboard scope | [upcoming-decisions.md](upcoming-decisions.md) — an open question, not a parked row, and it stays there because that file holds questions. Nothing takes an all-workspaces scope today: no handler, no query, no UI, and `actor.WorkspaceID` is a single value threaded through the service layer rather than a filter that could be widened. It is the surviving half of a queue row split on 2026-08-01 |

**Boundary note:** a redesign large enough to answer the three complaints will
touch templates that every other area's UI also touches. If B runs in parallel
with anything, B is the one that has to land first or the others rebase onto it.
This is the clearest case in the file for *not* running two milestones at once.

### C — Analytics and reporting

`internal/analytics`, `internal/store/query/analytics.sql`.

| Candidate | Where it is recorded |
| --- | --- |
| Advanced analytics | Plan.md *Other surfaces* — Phase 3 |
| Campaign analytics | Plan.md *Not in Phase 2*, kept there after M41 shipped campaigns; the reason is job load, not scope |
| The new-vs-returning analytics split | Plan.md *Not in Phase 2*, D12 |
| Storing region or city | Plan.md *Not in Phase 2* — reverses the privacy stance, so it is a decision before it is work |
| ASN/VPN detection | Plan.md *Not in Phase 2*, a "2+" row |
| Activity feed and comments | Plan.md *Not in Phase 2*, "2+" rows |
| **Distinguishing a blocked bot click from an observed one** | [decisions.md](decisions.md), 2026-08-06 — the residual left by the queue row that closed as already built. A column, an ingest field, one more `FILTER` |

### D — Redirect path and routing

`internal/redirect`, `internal/httpx/redirect.go`.

| Candidate | Where it is recorded |
| --- | --- |
| A human check or dispute path for a blocked bot | Plan.md *Not in Phase 2*, and the *Known limitations* row that says a misjudged person gets a 403 with no appeal |
| Per-bot allowlists, and improving bot classification | Plan.md *Not in Phase 2* — editing `Classify`'s marker list moves every existing analytics figure at the same time, which is why it is not a small change |
| The cookies routing condition | Plan.md *Not in Phase 2*, D2 |
| `links.status = 'disabled'` gaining a writer | Plan.md *Not in Phase 2*, D10 |
| Sharing the 404-probe limiter across replicas | Plan.md *Not in Phase 2* — a network round trip inside the 20ms budget |
| Re-checking already-accepted links against new blocklist tiers | Plan.md *Not in Phase 2* — a separate job |

**Boundary note:** every row here lands on the hot path, so each one owes the
[slo.md](../slo.md) k6 measurement its inherited rule requires. That is a shared
gate, not a shared file, so they do not block each other — but two of them
landing in one phase means the measurement runs twice.

### E — Infrastructure and resilience

`cmd/linkctrl`, `internal/redirect/invalidation.go`, the deployment docs.

| Candidate | Where it is recorded |
| --- | --- |
| High availability as a claim somebody could rely on | Plan.md *Other surfaces* — Phase 3. Multi-replica operation itself already works and is documented; what is missing is failover, a health-gated load-balancer contract, and measured rolling-deploy behaviour ([decisions.md](decisions.md), 2026-08-06) |
| Redis Streams as a work queue, for webhooks or the analytics recorder | Plan.md *Not in Phase 2* |
| Redis resilience beyond a bounded failure — a circuit breaker | Plan.md *Not in Phase 2* |
| **An update checker against GitHub releases, notifying instance owners** | Queue row, 2026-08-06. Absent everywhere — no version comparison against a remote, no release notification. **It introduces a new outbound-connection class**: the only one today is M42's webhooks, which `docs/SECURITY.md` treats as an operator-visible property, so this owes a decision on default-on-versus-off and an opt-out before it owes any code |

### F — QR codes and campaigns

`internal/qr`, the campaign code.

| Candidate | Where it is recorded |
| --- | --- |
| A PNG QR code | Plan.md *Not in Phase 2*, D11 — SVG only, no image encoder in the dependency set |
| More than one QR code per link, and per-code scan counts | Plan.md *Not in Phase 2* |

**Boundary note:** the redesign's third complaint is about QR *settings
vocabulary* — output size instead of quiet zone and module size. That is B's
template work over F's generator, so if both are taken they are one milestone or
an ordered pair, never two independent ones.

### G — Commercial and entitlements

Mostly out of this tree.

| Candidate | Where it is recorded |
| --- | --- |
| **A commercial plugin — billing and production-commercial features, in its own repository under a more restrictive license** | Queue row, 2026-08-06. Partly scheduled already: Plan.md *Other surfaces* has *entitlements or billing* at **3+** and *plugin system* at **4**, D17 records billing groundwork as deliberately absent, and D16 names `orgs.create` as *"the call site a future entitlement check would hang on"*. What is **not** covered anywhere is the second repository and the second license. This repo is MIT, so a restrictive satellite depending on it needs no relicensing and no contributor consent — the licensing half is therefore not urgent. **The seam is.** `orgs.create` is the only thing in the tree anticipating this, and a plugin architecture designed in Phase 4 without knowing what the commercial plugin needs may not fit it |

### Not an area — compliance

Plan.md's *Other surfaces* puts *compliance features* in Phase 3, and it does not
cut cleanly: erasure is A, retention and analytics minimisation are C, and the
audit log is neither. It is named here so that "compliance" is not mistaken for a
work area with a single owner. Whatever it turns into is assembled from rows
above rather than added beside them.

Also not areas, because neither is a milestone: the root-level `SECURITY.md`
pointer (Plan.md *Not in Phase 2*, and still nobody has asked for it), and
version history, scheduled changes and approval workflows, which are "3+" rather
than 3.

---

## Answered

**How many milestones is "shorter than Phase 2"?** **Under sixteen — fifteen at
most, insertions counted.** Owner-set 2026-08-06 and explicitly revisitable; the
rule and the trap it invites are in [planning.md](planning.md#the-size-target-a-phase-stays-under-sixteen-milestones),
which is where a planner will meet it, and [W32](workflow-changes.md#made) records
that it was made. Phase 2 ran 33.

**The consequence for this file is a real one.** Seven areas are listed above and
fifteen milestones will not cover them at Phase 2's depth — that phase spent 33 on
comparable ground. So Phase 3 takes some of these areas, not all, and the target
is what forces that to be decided rather than discovered at milestone twenty.

---

## Open questions

**Which areas does Phase 3 take?** No recommendation is offered on the whole
set — that is scope, and it is the owner's alone. Two observations that are the
actor's to make rather than the owner's to derive: **A carries the two defects**
(F44, F141), which are the only candidates in this file that make a current claim
false; and **B is what the owner asked for first** and is also the area that
blocks parallel work hardest, per its boundary note.
