# Upcoming decisions

Questions the phase loop will reach but has not reached yet, so they can be
answered at leisure instead of while the loop stands still waiting. Written by
`/preview-decisions`, which reads ahead of the build; see the trigger in
[workflow.md](workflow.md).

**This file holds questions, never answers.** An entry leaves it when it is
answered, and the answer is appended to [decisions.md](decisions.md) with its
`D` number when the milestone that uses it lands — carrying the date it was
*given* as well as the date it was used, so the trail shows the decision
predated the work. One direction, always: a decision recorded here and nowhere
else is a decision that will be re-taken by whoever builds the milestone.

An answer given here binds exactly as much as one given inside the loop. The
early timing is a scheduling convenience and not a lower standard, which is why
entries carry options and a recommendation like any other prompt.

**Assumptions are what make an early answer safe.** Each entry names what it
takes for granted about a tree that is not built yet. Validation re-checks those
when it reaches the milestone; a false assumption re-opens the question rather
than letting the milestone inherit a stale answer.

**Two sections, by what forces an answer.** *Open — a milestone needs this*
holds questions the loop will stand still on, and is read before building the
milestone named. *Open — nothing forces this* holds questions with no deadline:
a convention taken by judgement that nobody ratified, a thing built that could be
struck. Nothing stalls on that section, and it is meant to be read at leisure —
but a question is in it rather than in somebody's memory, which is the whole
point.

A question answered in conversation and nowhere else is a question that will be
re-asked, and answered differently. Write it here first, then answer it.

An entry whose milestone shipped without the question ever being asked leaves
this file with a one-line note in [decisions.md](decisions.md) saying it was
dropped and why — usually that the build answered it implicitly. It is not
silently deleted: "the question was not real" is a conclusion, and unrecorded
conclusions are what this file exists to stop.

---

## Open — a milestone needs this

### Every milestone from M26.5 on — does README describe the released product, or this branch?

**Needed by:** every commit, since `b068b73` made README.md and
docs/SECURITY.md part of the per-commit Docs gate. Forced at
[M45](phase-details/m45.md), where the README status line moves to 0.2.0.

**Currently behaving as B**, chosen by the orchestrator on 2026-07-31 without
being put to the owner. That is the reason this entry exists: the convention was
settled in prose, and the gate now enforces whatever it is.

| Option | Buys | Costs |
| --- | --- | --- |
| **B — describes this branch** *(recommended, and what the tree does)* | The gate is meaningful from the day it was added; a reader of the phase branch sees what the phase branch does. Matches what M23, M24 and M24.5 already did to README | Anyone reading README on `phase-2` sees features no released version has. The status line says "released as 0.1.0" while the feature table describes more than 0.1.0, which is a contradiction a careful reader will find |
| A — describes the released product | README is always true for somebody who installed a tag, which is who reads it | M23, M24 and M24.5's README edits become wrong and need reverting; the Docs gate becomes almost always a no-op, so it stops catching the drift it was added for; every phase's features land in README in one M45 lump |

**Default if unanswered:** B continues. It is what the tree does and what the
gate assumes.

**Assumes:** that CHANGELOG's `[Unreleased]` section remains how released and
unreleased work are told apart — true as of 2026-07-31, and the thing that makes
B survivable.

## Open — nothing forces this

No deadline, no milestone waiting. Read when convenient; an answer here is worth
exactly what an answer anywhere else is worth.

### An 'All Workspaces' dashboard scope — which phase, and whose milestone?

**Needed by:** nothing yet. It is the feature half of a queue row split on
2026-08-01; the other half — *the dashboard should show only the selected
workspace* — turned out to be already built, so only this remains.

The dashboard and links pages scope to the acting workspace and always have.
What does not exist anywhere is a way to see **across** workspaces at once: no
handler, no query and no UI takes an all-workspaces scope, and
`actor.WorkspaceID` is a single value threaded through the service layer rather
than a filter that could be widened.

| Option | Buys | Costs |
| --- | --- | --- |
| **Phase 3, beside *Moving links between workspaces*** *(recommended)* | The two cross-workspace capabilities land together, and they share the hard part — every scoped query in `internal/link` and `internal/analytics` assumes one workspace id. Phase 2 has no milestone this belongs inside | Somebody with several workspaces keeps switching to compare, for the whole of Phase 2 |
| A Phase 2 milestone of its own | The demo M33.5 is about to make multi-workspace instances the normal thing to look at, which is exactly when the gap gets noticed | It is a new scope row late in a phase whose remaining milestones are already substrate for each other, and it widens a query path M34–M37 are about to build on |
| Fold into [M37](phase-details/m37.md) | M37 is the dashboard milestone, so the surface is already being touched | M37 is about how a *dimension* is visualized, not which workspaces are in scope. Different question wearing the same page |

**Default if unanswered:** it stays unbuilt and unscheduled, which is the status
quo and costs nothing until somebody asks for it a second time.

**Assumes:** that the dashboard and links pages remain workspace-scoped — true
and verified on 2026-08-01 by reproduction, not by reading — and that no
milestone between here and M45 introduces a cross-workspace view for its own
reasons.

### F21 — once the workspace switcher's button is gone, what does a browser without JavaScript get?

**Needed by:** nothing yet. [F21](deferred-findings.md) is unapproved, so no
milestone is waiting. It becomes forced the moment F21 is scheduled.

The owner directed the fix on 2026-08-02, via `/note`: *"The workspace drop down
should perform the switch action when changed, a separate button to switch can't
stay."* That settles the affordance and is not re-asked here. It leaves one
thing open, and it is the kind that gets picked silently by whoever builds it.

Switching on change needs a change handler, and the only one available is htmx —
already served, already loaded on every page, already doing this exact thing at
`internal/ui/templates/pages/links.html:56`. The current **Switch** button is
also the only reason the control works with scripting off. Delete it and the
switcher stops working entirely for that reader; keep it and the directive is
not carried out.

| Option | Buys | Costs |
| --- | --- | --- |
| **A `<noscript>` fallback — htmx on change, the button only when scripting is off** *(recommended)* | Carries the directive literally for every reader who has JavaScript, which is all of them in practice, and keeps the control usable for the one who does not. `<noscript>` is markup, so it costs no script, no CSP change and no new convention | It is more template than either alternative, and the fallback path is the one nobody will look at again. The recommendation is also the one that avoids making a call — if the project does not actually support scripting-off, this spends markup defending a reader it does not have |
| Delete the button outright | Exactly what the directive says, and the simplest template in the tree. Matches `links.html`, whose filters already only work with htmx | A no-JS browser can see it has several workspaces and cannot reach any but one. Nothing else in the dashboard fails that way today |
| Keep the button, add the change handler | Both paths work with no conditional markup | Directly refused by the directive, and the redundant control is the affordance problem F21 is about |

**Default if unanswered:** nothing is built — F21 is unapproved, so the two-step
switcher stays as it is. This question does not stall anything until F21 is
scheduled.

**Assumes:** that htmx stays loaded on every page from `layout.html:9` and stays
under `script-src 'self'` — both true and read from the tree on 2026-08-02 — and
that no explicit no-JavaScript support promise exists anywhere. That last one is
the weak assumption: `nav.html`'s own comment (*"A plain form: a select and a
button, no JavaScript"*) reads like a stance, but it is stated as a consequence
of the CSP belief this finding disproves, and no tracked document promises the
dashboard works with scripting off. If the owner holds that stance, say so and
option A stops being a judgement call.

### M26 — keep or strike the outbox's thirty-day purge?

Finished `mail_outbox` rows are deleted after thirty days by the existing
housekeeping reaper. **No bullet in [m26.md](phase-details/m26.md) asked for
it.** The worker flagged it, the orchestrator accepted it and named it in
`13df367`'s commit message as strikeable, and the owner has not said either way.

| Option | Buys | Costs |
| --- | --- | --- |
| **Keep** *(recommended)* | The outbox does not become the one table in the schema growing forever with nothing watching it. Matches the thirty-day link purge the same reaper already runs | Thirty days is a constant, not a setting, and it deletes delivery history nobody configured — which is the shape D5 rejected for the audit log. The recommendation is also the cheap one, since keeping it means doing nothing |
| Strike | The milestone ships exactly its bullets, and retention becomes its own decision with its own reasoning | The table grows unbounded until somebody schedules that decision |
| Keep, but make it a setting | Both, honestly | A configuration variable, its documentation and its test, for a table nobody has yet complained about |

**Default if unanswered:** it stays as built. Which is itself the thing worth
noticing — unasked-for work defaults to shipped unless somebody objects.

**Assumes:** `mail_outbox` stays the only table M26 added, and that no consumer
begins depending on old rows being readable. Both true as of 2026-07-31.

---

Entry format:

```markdown
### <milestone> — <the question in one sentence>

**Needed by:** M31, after M25 and M29 land.

| Option | Buys | Costs |
| --- | --- | --- |
| A — *recommended* | … | … (the recommended option states its own cost) |
| B | … | … |

**Default if unanswered:** what the loop does if it arrives here with no answer.

**Assumes:** the specific, falsifiable things this rests on.
```
