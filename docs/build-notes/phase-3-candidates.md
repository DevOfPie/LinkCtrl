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

**The loop can now do half of 2.** [W33](workflow-changes.md#made)'s fallback half
was made on 2026-08-06: when the next milestone is blocked — including by an
unanswered prompt — [step 1](phase-loop.md#1-validate) takes the first
*independent* un-`done` row instead of stopping, where independent means its
dependencies are `done` **and it shares no work area with the blocked one**. That
is what makes the grouping below load-bearing rather than merely tidy.

Running two at once is still not possible and is **not** approved: the worker is
forbidden from starting a second milestone, and `.current-task.md` names one
milestone, one step, one actor. So an area boundary buys a fallback destination,
not concurrency.

---

## The areas

Cut so that two milestones in different areas do not edit the same files. Where
an area's boundary is not clean, the row says so — a hidden overlap is what
would turn parallel work into a merge conflict.

### A — Identity and account lifecycle

`internal/auth`, `internal/signup`, `internal/invite`, `internal/team`,
`internal/instance`, and `internal/httpx/web_keys.go` for the account surface.

*(Corrected 2026-08-06, at planning. This read "`internal/auth`,
`internal/identity`, the `002xx` migrations, `web_account.go`" and two of those
four do not exist: there is no `internal/identity` package and no
`web_account.go` file. `002xx` is one file, `00200_identity.sql` — an area
marker, **not** a free numbering band; the next free migration number is
`03700`. The area's boundary is unchanged, because it was cut by which files a
milestone would touch and they are the same files under their real names.)*

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

**Which areas does Phase 3 take?** **A, B, E and F.** Owner-set 2026-08-06.

| Area | In Phase 3 | |
| --- | --- | --- |
| **A** — Identity and account lifecycle | **Yes** | Carries F44 and F141, the only two candidates here that make a claim the tree makes today false |
| **B** — Dashboard UI and UX | **Yes** | Asked for first, and the area everything else rebases onto. Cannot be specified without the owner's walkthrough |
| **E** — Infrastructure and resilience | **Yes** | |
| **F** — QR codes and campaigns | **Yes** | Overlaps B on the settings vocabulary — one milestone or an ordered pair, never two independent ones |
| **C** — Analytics and reporting | No | Stays a candidate. Not dropped, not re-homed |
| **D** — Redirect path and routing | No | Stays a candidate. Every row here owes the `slo.md` k6 measurement, which is a cost the phase does not take on |
| **G** — Commercial and entitlements | No | Stays a candidate. Plan.md already carries entitlements at 3+ and a plugin system at 4, so the seam question keeps until then |

**And it is planned in full before anything is built.** Owner-set the same day:
every chosen area gets its Plan.md rows, milestone files and numbering before
milestone one starts, so the fifteen-milestone target is enforced by arithmetic
rather than discovered at milestone twenty. What that costs is a planning stretch
with nothing shipping, and it is bounded by B's walkthrough rather than by the
planning itself — see *Open questions*.

**Which candidates each area takes.** Owner-set 2026-08-06, recorded as D109–D111
in [Plan.md](../../Plan.md#phase-3-decisions) with the reasoning in
[decisions.md](decisions.md#2026-08-06--phase-3-planned-what-each-area-takes-and-the-twelve-slots).
Twelve slots for work, after two reviews and the close come out of fifteen. *(The phase later ran to seventeen, and to eighteen when M57.5 was added after the close — see Plan.md's Phase 3 build plan for how the target moved and why.)*

| Area | Takes | Leaves on this list |
| --- | --- | --- |
| **A** | F141 recovery ([M51](phase-details/m51.md)), F44 erasure ([M52](phase-details/m52.md)), MFA/TOTP ([M53](phase-details/m53.md)), the multi-organization key F75 ([M54](phase-details/m54.md)) | OAuth, OIDC, SSO, SCIM; the runtime signup toggle |
| **B** | Three milestones, M46–M48, specified by the walkthrough | The rows folded into the redesign are decided by it, including the workspace-selector label |
| **E** | The update checker ([M55](phase-details/m55.md)), high availability ([M56](phase-details/m56.md), [M57](phase-details/m57.md)) | Redis Streams as a work queue; the circuit breaker |
| **F** | All of it — PNG and pixel sizing ([M49](phase-details/m49.md)), several codes per link ([M50](phase-details/m50.md)) | Nothing |

Four rows above are now **scheduled** and their reasons live where they always
did; this file points at the milestone instead of at the deferral. The rows left
behind keep their reason and stay candidates.

---

## What Phase 3 shipped, and what is still on this list

Written at [M58](phase-details/m58.md), the phase close, against the tree rather
than against the plan. **Seven areas were listed and the phase took four**; C, D
and G stay candidates — not dropped, not re-homed, and *we stopped caring about
this* is the decision this project keeps losing.

The third column is the one this section exists for. A candidate can survive a
phase and still not be the same candidate: the work that landed beside it moves
what it would cost, what it would touch, or what is already true of it. A row
that changed shape and says nothing is a row somebody re-derives from scratch.

### A — Identity and account lifecycle

| Candidate | Now | What moved |
| --- | --- | --- |
| MFA, OAuth, OIDC, SSO, SCIM | **MFA shipped** ([M53](phase-details/m53.md)); the other four stay candidates | D109 discharged only the MFA limb. The `password_hash` nullability comment and `auth.Service.verifyPassword` promised SSO *"(Phase 3)"* and no longer carry a phase number — M58's sweep. The row is now a candidate with no date rather than a promise |
| Account recovery (F141) | **Shipped** — [M51](phase-details/m51.md) | — |
| Account deletion and erasure (F44) | **Shipped** — [M52](phase-details/m52.md) | The residue is smaller than M52 left it: [F177](deferred-findings.md) and [F181](deferred-findings.md) closed at M58, so the erasure pass reaches audit `metadata` and invitation addresses too |
| An API key reaching more than one organization (F75) | **Shipped** — [M54](phase-details/m54.md) | New ground the candidate did not anticipate: an administrator can cut their own organization out of somebody's account-wide key (D158), and M58 closed both halves of it — the read bound ([F183](deferred-findings.md)) and the owner's view of it ([F178](deferred-findings.md)) |
| A runtime signup toggle from the dashboard | **Stays a candidate** | Still parked at D38. But the phase built the surface the parking was partly about: D161's `instance_settings` singleton and its checkbox exist now, so the toggle has somewhere to live that it did not have |

### B — Dashboard UI and UX

| Candidate | Now | What moved |
| --- | --- | --- |
| The UI/UX redesign | **Shipped** — [M46](phase-details/m46.md)–[M48](phase-details/m48.md), plus the seven defects it produced, fixed at M58 | All three complaints answered. The blind tasks that specified it are recorded nowhere (D146), so the exercise cannot be re-run |
| The workspace selector should always render | **Half shipped** | M46 added the label, which the row itself identified as the real gap — the current workspace and organization now appear in the shell unconditionally. The control still renders only above one membership, and D117 settled *why*: a switcher offers the places you can go. What is left of this row is a preference, not a gap |
| Mobile navigation rework | **Stays a candidate**, narrowed | M46 made *no horizontal scroll at 360px* a scanned property of the shell, and [F184](deferred-findings.md) closed the one page that broke it. The header hiding the signed-in address below `sm` is untouched and is what the row is now about |
| Notification severity, grouping, filtering | **Stays a candidate** | Unchanged — still needs the column M22's model has no room for |
| Trash/restore UI · Bulk operations · An **All Workspaces** scope | **Stay candidates** | Unchanged. The all-workspaces question is still a question, in [upcoming-decisions.md](upcoming-decisions.md) |

### C — Analytics and reporting *(area not taken)*

Every row stays. Two changed shape:

- **Campaign analytics** — [M50](phase-details/m50.md) gave per-code counts by
  making the code *its own stored referrer value* (D132) rather than by building
  the rollup this row was deferred for. The row's reason was job load, and that
  reason is intact; what moved is that the cheapest version of it is now done and
  the row is about the expensive part only.
- **Distinguishing a blocked bot click from an observed one** — unchanged as
  work, but M58 removed the phase number from the *bypass* promise beside it, so
  this row and D's first row no longer imply a shared schedule.

### D — Redirect path and routing *(area not taken)*

Every row stays. One changed shape:

- **A human check or dispute path for a blocked bot** — **seven** sites promised
  it *"in Phase 3"*, counted across the tracked tree at M58 rather than recalled,
  and all seven now say it is unscheduled: `01800_bot_blocking.sql:29`,
  `internal/link/domain_settings.go:276`, Plan.md's *Known limitations* row,
  `docs/SECURITY.md`'s *A human blocked as a bot cannot get through*,
  `test/integration/bots_test.go:291`, and both of
  [m32.5.md](phase-details/m32.5.md)'s — its *Deliberately not in this
  milestone* bullet and its *Risks* paragraph. This entry said *the three sites*
  and then listed four; the count is stated because a section whose method is
  counting cannot afford to recall. `decisions.md`'s two mentions are the
  exception and stay: the log is append-only, and *why the bypass was Phase 3* is
  a true record of a decision taken when it was. The candidate is unchanged; what
  changed is that the product no longer tells a reader it is coming.

### E — Infrastructure and resilience

| Candidate | Now | What moved |
| --- | --- | --- |
| High availability as a claim somebody could rely on | **Shipped** — [M56](phase-details/m56.md), [M57](phase-details/m57.md) | The load-balancer contract is D167's; the two-leader window is D168's, closed for deploys and bounded for crashes |
| The update checker | **Shipped** — [M55](phase-details/m55.md) | D149 defaults it on and asks at first run, which required the instance-settings surface the row did not anticipate |
| Redis Streams as a work queue | **Stays a candidate** | Verified unexercised at M58, not assumed: `cmd/linkctrl/recorder.go` and `internal/webhook/webhook.go` both state it as an upgrade path nothing is written against, and both are still true |
| Redis resilience beyond a bounded failure | **Stays a candidate** | Unchanged |

### F — QR codes and campaigns

| Candidate | Now | What moved |
| --- | --- | --- |
| A PNG QR code | **Shipped** — [M49](phase-details/m49.md), D11 reversed | — |
| More than one QR code per link, and per-code scan counts | **Shipped** — [M50](phase-details/m50.md) | — |

This area's *Leaves on this list* cell read **Nothing**, and after the phase it
is nearly true — but not quite, and the exception was found by M58's comment
sweep rather than by the plan. Logos were not on this list at all and shipped
anyway ([M50.5](phase-details/m50.5.md), [M50.6](phase-details/m50.6.md)), from
the walkthrough. **Module shape is the one thing `qr_codes.style`'s comment
claimed and nothing built** — square modules only — and it is named here so that
removing the claim from the schema does not also remove the idea.

### G — Commercial and entitlements *(area not taken)*

The row stays, and **both** its anchors in the tree lost their dates.
`orgs.create` names itself the call site an entitlement check would hang on in
two places — `CreateOrganization`'s doc in `internal/team/organization.go`, and
[M28](phase-details/m28.md)'s own bullet — and each said *"(Phase 3+)"* until
[M58](phase-details/m58.md); each now says unscheduled. **This close-out counted
one of them**, corrected 2026-08-10: the one it missed is in a shipped
milestone's file, which is exactly where the bot-bypass count had been caught
hiding two more of itself hours earlier. A third mention, `Plan.md`'s D16 row,
carries no phase number and needs nothing — it is named here so the next sweep
does not re-find it and wonder. The seam argument is unchanged and is still the
urgent half.

---

## Open questions

**Which areas does Phase 3 take?** **Answered 2026-08-06 — A, B, E and F.** See
*Answered* above. What remains open is everything below.

**What does the redesign actually specify?** **Answered 2026-08-07.** Eighteen
blind tasks over two rounds produced [M46](phase-details/m46.md),
[M47](phase-details/m47.md) and [M48](phase-details/m48.md), plus seven defects
(F160–F166) that are fixed at [M58](phase-details/m58.md) rather than costing a
redesign slot. The reasoning, including why *irritating* turned out to mean
*defective* in four of six areas, is
[D114](decisions.md#2026-08-07--what-eighteen-blind-tasks-specified-and-the-six-defects-hiding-inside-a-word).

Three asks the walkthrough produced are **deferred to Phase 4** rather than
dropped — folders path-entry, organization switching, and API-key scope grouping.
Their reasons are in Plan.md's *Not in Phase 3*, which is where a deferral's
reason lives; this file does not restate them.

**Does the update checker default on or off?** **Re-homed 2026-08-06** to
[upcoming-decisions.md](upcoming-decisions.md#m55--does-the-update-checker-default-on-or-off),
which is where it said it belonged once the phase was planned. It carries three
options, their costs and a recommendation, and [M55](phase-details/m55.md) reads
the answer from there rather than pre-empting it.

**How do B and F share the QR work?** **Answered 2026-08-06 — an ordered pair.**
The settings vocabulary is B's templates over F's generator, so
[M49](phase-details/m49.md) sits behind M48 as an ordering preference
rather than inside it: the generator work — PNG, the pixel-size arithmetic, the
snap — has no edge on B at all and can land first if B stalls, which is exactly
the fallback [W33](workflow-changes.md#made) exists for. What must not happen is
a settings rewrite landing into a page still being rebuilt, because that writes
it twice.

## Phase 3 inherits all fourteen, confirmed 2026-08-07

**Moved here from [phase-details/README.md](phase-details/README.md#what-every-milestone-inherits)
on 2026-08-08**, at [M51.9](phase-details/m51.9.md)'s doc-cost judgement. It is
planning evidence — produced once, when the phase was scoped — and it was being
charged against the `/work phase` resume at every milestone. The rules
themselves have not moved and are still in that file's *What every milestone
inherits* table, which is what step 1 reads.

One at a time, as the lede requires. **All fourteen carry**, and none was
weakened. What follows is only the ones a Phase 3 milestone actually engages, so
a validator knows where the rule does work rather than sits:

| Rule | Which milestone tests it, and how |
| --- | --- |
| Redirect tree stays minimal | [M50](phase-details/m50.md) parses a second query parameter on the hot path. Its tripwires must pass unmodified; if the code identity needs a lookup the resolver does not already hold, M50 says it does not ship in that form. |
| Redirects are never permanent | Untouched. No Phase 3 milestone writes a redirect status. |
| Cache is optional | [M56](phase-details/m56.md) and [M57](phase-details/m57.md) are where this could quietly break: M57's conformance test asserts one container with **no Redis** exercises the full surface, which is this rule turned into a gate rather than a habit. [M50.5](phase-details/m50.5.md)'s storage decision is bounded by that same test — an object store would be a new required dependency. |
| Privacy stance | [M52](phase-details/m52.md) writes the first erasure routine in the product and [M51](phase-details/m51.md) audits a reset with an IP prefix only. Neither adds a column the stance forbids. [M50.5](phase-details/m50.5.md) adds the first *user-uploaded* content — which the stance is not about, and which account erasure deliberately does **not** reach. |
| Every UI feature has API support | [M51](phase-details/m51.md) (recovery routes), [M50](phase-details/m50.md) (QR code CRUD), [M50.5](phase-details/m50.5.md) (upload and clear — **and teaching the contract test multipart, which it has never done**) and [M54](phase-details/m54.md) (key reach) each land operations in `api/openapi.yaml`. |
| Dormant structure is jsonb | [M50](phase-details/m50.md) touches `qr_codes.style`; [M49](phase-details/m49.md) reads pre-milestone styles forward out of the same blob; [M50.6](phase-details/m50.6.md) draws a logo, but **not** out of the blob — [D134](../../Plan.md#phase-3-decisions) put it in a `bytea` column, so the *logo reference* the blob's comment has promised since Phase 1 is still unbuilt and the rule is untested by it. *(Amended 2026-08-07: written at planning time, made false by M50.5's storage answer.)* |
| Partitioning | Untouched. No Phase 3 milestone adds a partitioned table. |
| DDL is additive | [M54](phase-details/m54.md) makes `api_keys.organization_id` nullable and [M50](phase-details/m50.md) drops a unique index. Both are additive within 0.3.0; M54's risk section states that the *resolution logic* is what is not reversible, which the rule does not cover. |
| Permissions | No Phase 3 milestone adds a permission. [M54](phase-details/m54.md) re-derives D18's delegability reasoning against a credential that crosses tenancies, and [M52](phase-details/m52.md) declines an administrative delete-somebody-else rather than inventing one. |
| `ui` stays stdlib-only | [M46](phase-details/m46.md)–[M48](phase-details/m48.md) are a redesign, which is exactly where the argument for a framework gets made. All three restate the rule for that reason. |
| Both themes | Same three. New markup uses the theme tokens and M24.5's template scan applies unchanged. |
| Touching the redirect path | [M50](phase-details/m50.md), [M57](phase-details/m57.md) and [M57.9](phase-details/m57.9.md) — three k6 runs this phase. |
| A test that passes first try | Everywhere. [M54](phase-details/m54.md) names it as doing real work rather than ceremony: there is no existing test that would fail if scope intersection were taken against the wrong role. |
| A new feature somebody can *see* | [M50](phase-details/m50.md) and [M53](phase-details/m53.md) add `demoCoverage()` rows. [M49](phase-details/m49.md) deliberately adds none and says why; [M57](phase-details/m57.md) is exempt because there is nothing to look at. |
