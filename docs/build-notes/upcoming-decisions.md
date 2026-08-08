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

*Both questions this section held were answered by the owner on 2026-08-08, at
[M52](phase-details/m52.md)'s step 1, and have left it in the direction this
file only travels: the answers are in
[decisions.md](decisions.md#2026-08-08--m52-three-answers-at-step-1-and-the-conflict-resolved-by-the-command-that-meets-it)
as **D148** (the erased actor's tombstone) and **D149** (the update checker's
default), and in [Plan.md](../../Plan.md#phase-3-decisions)'s decision table.
D149 was given ahead of the milestone that uses it, which is what this file is
for; it is recorded with the date it was given and is used when
[M55](phase-details/m55.md) lands.*

*Nothing is waiting here. The next entry is written by
[`/preview-decisions`](../../.claude/commands/preview-decisions.md) or by a
[step 1](phase-loop.md#1-validate) that meets a choice no `D` number has made.*

## Answered, awaiting the milestone that uses it

One heading, kept for one reason: **other files link to it.** An answered
question leaves this file, and this section is what stops that removal breaking
a reference somebody wrote in good faith. It holds pointers, never answers —
the rule at the top of this file is not weakened by a section that says *the
answer is over there*. A heading leaves here for good once the milestone that
uses it has landed and the references have moved with it.

### M55 — Does the update checker default on or off?

**Answered 2026-08-08** by the owner, as
[D149](../../Plan.md#phase-3-decisions) — recorded in
[decisions.md](decisions.md#2026-08-08--m52-three-answers-at-step-1-and-the-conflict-resolved-by-the-command-that-meets-it)
with the date it was given, and read by [M55](phase-details/m55.md), which
carries what it obliges. The question, its three options and its recommendation
are in the history of this file; they are not restated here, because a question
whose answer exists is no longer a question.

Two entries link to this heading and were written while it was open —
[the Phase 3 area-scoping decision](decisions.md#2026-08-06--phase-3-planned-what-each-area-takes-and-the-twelve-slots)
and [phase-3-candidates.md](phase-3-candidates.md). Both say the default *is
deliberately not decided here*, which was true when written; D149 is the later
entry that corrects them, and neither is edited.

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

### <milestone> — <the question in one sentence>

**Needed by:** M31, after M25 and M29 land.

| Option | Buys | Costs |
| --- | --- | --- |
| A — *recommended* | … | … (the recommended option states its own cost) |
| B | … | … |

**Default if unanswered:** what the loop does if it arrives here with no answer.

**Assumes:** the specific, falsifiable things this rests on.
```
