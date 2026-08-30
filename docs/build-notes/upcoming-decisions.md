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

*The F204 shape question entered here on 2026-08-11 and left the same day, in
the direction this file only travels: wireframes were drawn, the owner chose
B1 and amended it to a chevron-only switch face, and the answer is in
[decisions.md](decisions.md#2026-08-11--m466-the-workspace-pair-reads-as-one-control),
with its `D` number — **D177**, assigned when [M46.6](phase-details/m46.6.md),
the milestone planned from the answer, landed.*

*Both questions this section held were answered by the owner on 2026-08-08, at
[M52](phase-details/m52.md)'s step 1, and have left it in the direction this
file only travels: the answers are in
[decisions.md](decisions.md#2026-08-08--m52-three-answers-at-step-1-and-the-conflict-resolved-by-the-command-that-meets-it)
as **D148** (the erased actor's tombstone) and **D149** (the update checker's
default), and in [Plan.md](../../Plan.md#phase-3-decisions)'s decision table.
D149 was given ahead of the milestone that uses it, which is what this file is
for; it is recorded with the date it was given and is used when
[M55](phase-details/m55.md) lands.*

*The one question that arrived here on 2026-08-08 came from
[step 3.4](phase-loop.md#3-land) rather than from
[`/preview-decisions`](../../.claude/commands/preview-decisions.md) — the gap in
an answer already given, found by building against it — and was answered the same
day as **D164**, which corrects D159 in
[decisions.md](decisions.md#2026-08-08--m55-d159-corrected--an-upgraded-instance-is-asked-not-assumed).*

*Three entries arrived from Phase 4 and all three have left. Two arrived and left
on 2026-08-18 — the F253 and F254 repair shapes, asked and owner-answered at the
plan's review, and recorded as **D212** and **D213** in
[decisions.md](decisions.md#2026-08-18--m59-the-two-repair-shapes-the-owner-chose-at-the-plans-review)
when [M59](phase-details/m59.md) used them. The third arrived the same day and
left on 2026-08-22: **what the inline add-on deadline's default is**, filed for
[M66](phase-details/m66.md) with the shape of its answer fixed in advance and the
number deliberately left to be measured. It is
[D318](decisions.md#2026-08-22--m66-what-the-extension-point-costs-and-where-it-sits),
25ms, taken from that milestone's own runs and stated in
[docs/slo.md](../slo.md) beside them. **The entry's own escape clause did not
fire**: it said that if the honest number did not fit the 20ms budget that was an
owner prompt rather than a bigger default, and the honest number is single-digit
milliseconds exactly as planning expected — what exceeds the budget is the safety
margin on top of it, which is not the measurement the clause was about. All three
headings have left this file, the direction it only travels; nothing pointed at
any of them.*

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

**M55 has landed, and this heading stays anyway — checked at
[M58](phase-details/m58.md)'s documentation pass rather than left to be
noticed.** The rule above says a heading leaves for good once its milestone has
landed *and the references have moved with it*. The milestone landed; one of the
two references cannot move. It sits inside an entry in
[decisions.md](decisions.md), which is append-only — *never edit an entry; a
later entry corrects an earlier one* — so repointing it is not available, and
deleting this heading would break a link that
[`make check-links`](../../scripts/check-links.sh) is there to catch.

That is not a conflict between the two rules; it is this section doing the one
job it was created for, and the first time it has had to. The heading is a
pointer and holds no answer, which is the whole of what it is permitted to be.
It leaves when `decisions.md` no longer points at it, which will be never, so in
practice it is permanent — said plainly here so that a later reader does not
find an *awaiting the milestone* heading for a shipped milestone and take it for
an oversight.

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
| **Phase 4, beside *Moving links between workspaces*** *(recommended)* | The two cross-workspace capabilities land together, and they share the hard part — every scoped query in `internal/link` and `internal/analytics` assumes one workspace id. Neither Phase 2 nor Phase 3 has a milestone this belongs inside | Somebody with several workspaces keeps switching to compare until it lands. *(This option read **Phase 3** until [M58](phase-details/m58.md). Phase 3 is closing without it, and its companion — Plan.md's `Moving links between workspaces` — was deferred to Phase 4 by the owner on 2026-08-07, so the option now names the phase its companion is actually in. The question itself is still open and still the owner's; the two remaining options are Phase 2's and are kept as the record of what was weighed.)* |
| A Phase 2 milestone of its own | The demo M33.5 is about to make multi-workspace instances the normal thing to look at, which is exactly when the gap gets noticed | It is a new scope row late in a phase whose remaining milestones are already substrate for each other, and it widens a query path M34–M37 are about to build on |
| Fold into [M37](phase-details/m37.md) | M37 is the dashboard milestone, so the surface is already being touched | M37 is about how a *dimension* is visualized, not which workspaces are in scope. Different question wearing the same page |

**Default if unanswered:** it stays unbuilt and unscheduled, which is the status
quo and costs nothing until somebody asks for it a second time.

**Assumes:** that the dashboard and links pages remain workspace-scoped — true
and verified on 2026-08-01 by reproduction, not by reading — and that no
milestone between here and M45 introduces a cross-workspace view for its own
reasons.

### M70 — Does the SDK bump reach the *published* worked example, or only its main branch?

**Needed by:** [M70](phase-details/m70.md), which is next.

M70's definition of done is that `DevOfPie/LinkCtrl-OIDC`'s go.mod names
`github.com/DevOfPie/LinkCtrl v0.4.0`, its CI is green on the bump, and its
changelog says the SDK moved from a pseudo-version to a release. Bumping that
repository's `main` satisfies every word of that and **not** the purpose the
milestone states for it — *immutable and publicly resolvable is what the exercise
needs; tagged is what the worked example needs*. Every operator-facing pointer
this repository publishes names `v0.1.0`, whose immutable go.mod requires
`v0.3.1-0.20260827043355-9509158d0e75`. After M70 as written, the add-on a reader
downloads is still built against a pseudo-version of an unreleased commit.

| Option | Buys | Costs |
| --- | --- | --- |
| **Cut a second `LinkCtrl-OIDC` release in M70** *(recommended)* | The published example is actually built against the released SDK, which is the sentence M70 gives as its reason for existing. Moves the four pins in `scripts/oidc-fixture.sh:52-61` and the pointers in `docs/configuration.md`, `docs/SECURITY.md` and `CHANGELOG.md` to the new tag | A second cross-repo release inside the phase close, in a repository this tree cannot commit to — so it is the owner's hands, not the loop's, and M70 stalls until it happens. It also re-cuts the fixture digest that F348 just stabilised, so `make oidc-fixture` and `test/integration/addon_oidc_test.go`'s literals both move together or the suite goes red |
| Bump `main` only, and say so | Cheapest, entirely inside M70's written words, and no second release to coordinate | Leaves m70.md asserting a purpose it did not discharge, which is the shape this project reopens milestones over. Wants the bullet amended to claim what it will actually do |
| Defer the second release to 0.4.1 | Keeps the phase close small and lets 0.4.0 ship on schedule; the pins move once, later, when nothing else is in flight | Two releases where a reader has to know which pairs with which, and the phase's acceptance test keeps pointing at an add-on built against an unreleased commit for however long that is |

**Default if unanswered:** M70 does what it says — bumps `main` — and the
published example stays on the pseudo-version, unremarked. That is the outcome
this entry exists to stop happening silently.

**Assumes:** that `LinkCtrl-OIDC` `v0.1.0` remains the release every pointer in
this repository names — true and verified on 2026-08-30 by reading
`scripts/oidc-fixture.sh`, `test/integration/addon_oidc_test.go`,
`docs/configuration.md`, `docs/SECURITY.md` and `CHANGELOG.md` — and that the SDK
is still untagged at M70, which `git ls-tree -r --name-only v0.3.0 | grep ^sdk/`
returning nothing is what makes true today.

### <milestone> — <the question in one sentence>

**Needed by:** M31, after M25 and M29 land.

| Option | Buys | Costs |
| --- | --- | --- |
| A — *recommended* | … | … (the recommended option states its own cost) |
| B | … | … |

**Default if unanswered:** what the loop does if it arrives here with no answer.

**Assumes:** the specific, falsifiable things this rests on.
```
