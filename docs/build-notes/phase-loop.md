# The phase loop

Trigger: `/work phase`, or **"Work on Phase"**. Builds the current phase one
milestone at a time, unattended, until the phase ends. Routing here is
[work-loop.md](work-loop.md)'s; everything from step 0 on is this file's.

*Until the phase ends* is literal. One invocation is expected to land many
milestones in sequence, and the run stops only on a condition in
[§4](#4-repeat-or-stop). Stopping early has been this loop's most common
failure — see the *Not stop conditions* table there before deciding you have a
reason.

Style matches [workflow.md](workflow.md) — terse, trigger-first, no rationale —
because this file is re-read at every resume. The *why* is in
[decisions.md](decisions.md).

**Precedence.** [Plan.md](../../Plan.md) wins on *what*. workflow.md wins on
*gates* — what must pass. This file wins on *sequence* — what order, who does
it, and when to stop. A conflict between them is a bug: report it, do not pick.

---

## The loop

```
0 resume    ORCH    read .current-task.md → rejoin the milestone in flight, or start fresh
1 validate  ORCH    the next milestone, against the tree as it is now
2 build     WORKER  that milestone, and nothing else
3 land      WORKER  gates, then stop before the commit
            ORCH    accept or reject → commit → demo-update → push
4 repeat    ORCH    from 1, unless a stop condition fired
```

Five rules outrank every step:

| Rule | Meaning |
| --- | --- |
| **Ask, never assume** | Any choice the owner would want → decision prompt, then *wait*. Picking one and proceeding is a process failure even when the pick is right. Every prompt carries options, costs and a recommendation, per [workflow.md](workflow.md#standing-rules). |
| **Prompts belong to the orchestrator** | Only the orchestrator talks to the owner. A worker that meets a prompt writes it verbatim into the note, returns it unanswered, and stops. |
| **Never cross a phase boundary** | The loop ends at the phase's last milestone. Starting the next phase is a new instruction, not the next iteration. |
| **Only the table stops you** | [§4](#4-repeat-or-stop) lists every stop condition and is exhaustive. A reason that is not in it is not a reason — continue. Stopping early is the most common way this loop has failed. |
| **Keep `.current-task.md` true** | Rewrite it at every step boundary, so that a stop you did not choose costs effort only, never knowledge. It is a safety net for being interrupted, never a licence to interrupt yourself. |

---

## Two actors

| | Holds | Never |
| --- | --- | --- |
| **Orchestrator** | Steps 0, 1, 3.4–3.9, 4. Every prompt to the owner. Runs `X.9` reviews and M45 itself. | Builds. Edits no code, no tests, no SQL. |
| **Worker** | Steps 2 and 3.1–3.3, for exactly one milestone. | Commits, pushes, runs `make demo-update`, answers a prompt, or starts a second milestone. |

One worker per attempt, spawned fresh. A rejected milestone gets a **new**
worker, never the same one continued — the split exists so the second attempt
does not inherit the first one's reasoning.

### Spawning a worker

Pass the milestone number, the branch, and this file. Nothing else. A worker
handed a summary of `phase-details/mN.md` builds the summary; it reads the
milestone file itself. On a re-spawn, add the rejection verbatim and nothing
more.

Run it synchronously — the orchestrator needs its report before step 3.4.

### What a worker returns

Short, and the only thing the orchestrator reads from it:

- what it built, and what it deliberately did not
- each gate run, and its result
- proposed commit message — prose about *why*
- rows appended to [deferred-findings.md](deferred-findings.md)
- any prompt, verbatim and unanswered

A report is not evidence. The tree is.

### Marking what gets appended

Appends outlive the context that made them. A worker returns and everything it
knew about *why* a decision entry or a finding exists goes with it, so the
milestone number is written at the time or it is not recoverable at all.

**Anything appended while a milestone is under way carries that milestone's
number, whoever appends it.**

| File | Marker |
| --- | --- |
| decisions.md | `## <date> — MN, <title>` — the number leads the title, before any prose |
| [deferred-findings.md](deferred-findings.md) | the **Found in** column |
| Plan.md, phase-details/ | none — every row already sits under its own number |
| CHANGELOG.md | none — it is written for operators, and `MN` means nothing outside this repo |

Entries no milestone produced — a process change, a phase close — carry no
number, and name what prompted them in their first line instead. An unmarked
entry is therefore a claim that nothing was being built, not an oversight.

A reopened milestone keeps its number: work added to it marks the number it was
added under rather than acquiring a new one. That is the whole reason reopening
is preferred to a successor milestone — the trail stays in one place.

---

## 0. Resume

`.current-task.md`, repo root, gitignored. Absent → start at step 1.

Present → it names a milestone, a step, and which actor held it. Trust it for
*intent*; verify *state* independently — `git status`, `git log -1`, and the
status row in [phase-details/README.md](phase-details/README.md) are what
actually landed. The note is only what was being attempted.

Held by a worker at step 2 or 3 → spawn a new worker from step 2. Never continue
the old one.

Note records an unanswered prompt → re-ask it. Never answer it yourself, and
spawn nothing while it stands.

## 1. Validate

**Next milestone** = first row in
[phase-details/README.md](phase-details/README.md) that is not `done`.

Read exactly three things. Not the other milestone files — the split exists so
you do not.

1. `phase-details/mN.md` — the definition of done
2. that README's *What every milestone inherits* table
3. Plan.md's ordering row for N — dependencies, discharges

Then check the plan against the tree as it actually is. The plan was written
before the code existed; validation is where that gap surfaces.

| Check | Fails when | Then |
| --- | --- | --- |
| Dependencies landed | a `Depends on` milestone is not `done` | ordering bug → **prompt** |
| Not already built | code already satisfies a bullet | record in the note, do not rebuild |
| Discharges real | the Plan.md row or limitation it claims to close is gone or reworded | plan drift → **prompt** |
| Bullets falsifiable now | a bullet names a file, test or behaviour that no longer exists | [amend](#amending-a-bullet) — correct a fact, **prompt** on an assertion |
| Decisions cover it | it needs a choice no `D`-numbered decision made | **prompt** |
| Deferred overlap | an open row in [deferred-findings.md](deferred-findings.md) would make *this* milestone's claim false | in spec → fix here, close the row pointing at MN |

That last row is the only path by which an unapproved finding becomes work. It
follows from workflow.md — *a defect that makes the current milestone's claim
false is in spec, whatever it looks like* — and not from owner approval, so name
the exception in the commit message and keep it visible.

Output is one line: `MN validates`, plus notes; or `MN does not validate:
<reason>`, plus the prompt. **Validation never edits code**, and mN.md stays
read for step 3.4.

### Amending a bullet

A milestone file written before the code existed will sometimes name a tree that
is no longer there. Two kinds of wrongness hide under that, and they do not cost
the same:

| The bullet is wrong about | Example | Then |
| --- | --- | --- |
| **A fact** — a count, a filename, a test name | "across the eight pages" when there are nine | correct it, log the amendment, carry on |
| **What is being asserted** | which pages must render a control at all | **prompt**, carrying the proposed amendment |

Only the orchestrator amends, at step 1 or at [step 3.4](#3-land) — both, because
a stale fact surfaces as often while reading the tree against the bullets as
while validating. A worker never amends: it meets the bullet as written, or it
reports and stops.

The test is whether anyone could have decided differently. Nine pages is not a
choice, so prompting about it spends the owner's attention on arithmetic. Which
pages *should* carry a control is a choice, and correcting it silently would be
the loop editing its own definition of done.

**Every amendment gets a decisions.md entry**, marked with the milestone under
way, carrying three things:

- the bullet **as it stood**, quoted
- the bullet **as amended**, quoted
- the **tree fact** that forced it — what was looked at, and what it said

All three, because the first is what makes it an amendment rather than a
conclusion — argued in
[the M24.5 entry](decisions.md#2026-07-31--plan-drift-is-allowed-silent-plan-drift-is-not),
which is also the amendment that forced this rule.

Plan drift is allowed here. Silent plan drift is not.

## 2. Build

Worker.

- Status row → `in progress` in phase-details/README.md.
- **In spec only.** Anything else → one row in deferred-findings.md, marked per
  [Marking what gets appended](#marking-what-gets-appended), then carry on.
  Never fix out of spec, never bundle a second milestone.
- A test that passes first try → sabotage it, confirm red, restore by
  counter-edit. Never `git checkout`.
- Work on the **test** instance. The demo is touched at step 3 and nowhere else.
- Rewrite `.current-task.md` the moment a line in it stops being true.

## 3. Land

Order is load-bearing. Do not reorder. The actor changes in the middle.

**Worker — 3.1 to 3.3:**

1. `make check`, then `make test-integration` with the stack up (`make up`)
2. Every gate in workflow.md's *Before completing a commit* table
3. Docs true: Plan.md, CHANGELOG.md, `docs/*.md`; decisions.md **appended**
   (never edited) with its index row, and every append
   [marked](#marking-what-gets-appended)

Then stop and report. **The worker does not commit.** One that commits, pushes,
or runs `make demo-update` has broken the split; the milestone is unaccepted and
gets a new worker.

**Orchestrator — accept, or reject:**

Re-read `phase-details/mN.md`, then read the tree: `git status --short`,
`git diff`, and the tests the milestone named. The question is never *did the
worker say it was done*. It is *does the tree satisfy every bullet*.

| Reject when | Carrying |
| --- | --- |
| A bullet in mN.md is not satisfied by the tree | the bullet |
| A gate reported passing does not pass when re-run | the failure |
| The diff holds work no bullet asked for | what to remove — or a deferred row, if it is a real finding |
| A deferred row was closed without the exception named | the row |

Rejection spawns a new worker from step 2. Re-running `make check` yourself is a
gate, not a courtesy.

A bullet the tree contradicts on a *fact* is not a rejection — it is an
[amendment](#amending-a-bullet), and it happens here as readily as at step 1,
because reading the tree against the bullets is what surfaces one. Amend, log
it, and judge the work against the bullet as amended. A bullet the tree
contradicts on what it *asserts* is still a prompt, and the milestone waits.

**Orchestrator — 3.4 to 3.9, on acceptance:**

4. Status row → `done` in phase-details/README.md
5. `make check-links`
6. **Commit.** One milestone maximum. Message is the worker's proposed prose,
   edited as needed — *why*, not what. Name any deferred row closed under step
   1's exception.
7. `make demo-update` — refuses on a dirty tree, and that refusal means the
   milestone is not finished. Failure → a new worker from step 2; its fix is
   amended into the commit, which has not been pushed. **Do not push.**
8. `git push` to the phase branch
9. Read the note's `Stop:` line before overwriting it — a
   [deferred stop](#stopping-at-the-checkpoint) ends the run here. Then reset
   `.current-task.md` to the next milestone at step 1, check it against
   [the resume bar](#the-resume-bar), and scan `.queue.md` for rows
   marked `blocking?` — **those only**

3.9 is the **checkpoint**, and the report says so in as many words. It is the
only state in which nothing is in flight, which makes it the only point where
compacting or restarting the session costs effort and not knowledge. Neither is
the loop's to do — `--autocompact` is fixed when the session is launched and
`/compact` is the owner's — so naming the moment is the whole of the loop's part
in it, and it is why the note is checked against its bar one line earlier rather
than at the next resume, when whoever needed it is already gone.

The queue scan is the one point in the loop that reads it, and it reads it for
one thing. A `blocking?` row means the owner believes the next milestone would
build something the note makes wrong, and judging that is the orchestrator's, at
this boundary and nowhere earlier: a worker never sees the queue, and `/note`
itself decides nothing. Blocking in fact → **prompt** and wait. Not blocking →
say so in the report and continue.

Unmarked rows are not read, not counted, and not acted on. They wait for
`/process-queue`, which the owner runs deliberately — draining routes work into
Plan.md and deferred-findings.md, and an unattended run that quietly grew its own
scope is the failure the whole deferral system exists to prevent.

demo-update sits between commit and push because it rebuilds from the commit
just made: it is the last gate that can still fail before the work is published.

A gate that fails twice on the same cause → stop and report. Retrying is not
progress.

## 4. Repeat, or stop

Repeat from step 1. Otherwise stop, report where things stand, and do not
continue:

| Stop when | Because |
| --- | --- |
| A prompt is unanswered | Ask, never assume |
| The milestone just landed was the phase's last row — Phase 2: [M45](phase-details/m45.md) | Never cross a phase boundary |
| No un-`done` rows remain | Same |
| The same cause failed a gate twice | Retrying is not progress |
| The same gap survived two workers | Same |
| The owner said stop work | [Stop work](#stop-work) |
| The owner asked to stop at the checkpoint, and 3.9 just finished | [Stopping at the checkpoint](#stopping-at-the-checkpoint) — the `Stop:` line in the note is what carries it |
| The milestone just landed is the run's **milestone target** | [work-loop.md](work-loop.md#a-milestone-target) — `/work M45 phase` bounds the run at that row. Same `Stop:` line, same reason |
| The next milestone is an `X.9` review **and this run has already landed one** | [A review gets its own session](#a-review-gets-its-own-session) |

**That table is exhaustive.** Landing a milestone is not an event; it is one
iteration. The default after step 3 is step 1 again, and it takes a row above to
override that.

### Not stop conditions

Every one of these has ended a run in practice, which is why each is named
rather than left to judgement. None of them is a reason.

| Not a reason | Do this instead |
| --- | --- |
| Context is long, running out, or due to be compacted | Keep the note true and carry on. Context is summarized automatically and the run continues; wrapping up early throws away a working run to avoid a problem that handles itself. Compaction is an iteration boundary at worst, never an ending — and the loop cannot trigger one anyway. |
| A worker returned, and its milestone was accepted and pushed | That is one iteration ending, not the run. Spawn the next worker in the same turn. |
| The next milestone is large, or touches many files | Start it. Step 2 is interruptible at any point, which is what the note is for. An `X.9` review is the one exception, and it is a [§4](#4-repeat-or-stop) row rather than a judgement about size. |
| The next milestone needs a long job — k6, a reseed, a rebuild | Start it. A job being slow is not a job being risky. |
| Two, or three, or five milestones have landed | The phase ends at its last row, not at a round number. |
| It looks like a clean place to hand off | Handing off is the note's job, and the note is already written. A "clean boundary" is indistinguishable from the next iteration. |
| The work so far deserves review | Land it and keep going. It is committed and pushed; the owner can read it whenever they like without the loop pausing. This means somebody *reading* the work, not the `X.9` review milestone — that one is a scheduled row with its own stop condition. |

Reporting mid-run is fine and costs nothing — say what landed, then start the
next milestone in the same turn. What is not fine is ending the turn on a
summary when [§4](#4-repeat-or-stop)'s table has not fired.

### Two milestones that do not end like the others

Neither is delegated. The orchestrator runs both itself, because the product of
each is a conversation with the owner.

**Reviews** (`X.9` — [M32.9](phase-details/m32.9.md),
[M44.9](phase-details/m44.9.md)). Their product is findings, and findings are
the owner's to schedule. Fix only what makes a shipped milestone's own claim
false; everything else becomes deferred rows. Then **prompt** with the triage
before acting on it.

**Phase close** ([M45](phase-details/m45.md)). Every deferred row needs
individual owner review, so expect a conversation rather than an iteration. Its
release actions reach outside the repo — tagging, pushing a tag, opening the
phase PR are each confirmed before they happen, and merging is the owner's
alone.

### A review gets its own session

**The loop does not enter an `X.9` review it did not start the session with.**
Reaching one after landing milestones is a [§4](#4-repeat-or-stop) stop: report
where things stand, say the review is next, and ask the owner to start a fresh
session for it.

A fresh session that opens on a review runs it immediately. The condition is
*this run has already landed a milestone*, not *a review is next* — otherwise
the session started to do the review would stop on its own first move, and the
loop would deadlock politely.

This is **not** the context rule in the table above, which stands: a long context
is never a reason to stop. This one is about the *kind* of work, not its
quantity. Why that difference holds, and the run that forced it, are in
[W17's entry](decisions.md#2026-08-01--two-rules-the-last-run-earned).

## Stop work

Two stops, and the owner picks by what they intend to do with the tree.

| Say | Means | Costs |
| --- | --- | --- |
| **stop work**, or **stop** | Now. Mid-build, mid-gate, wherever it is. | Whatever the worker had not finished. Uncommitted work stays in the tree. |
| **stop at the checkpoint**, or **finish this milestone and stop** | At the next checkpoint. The milestone in flight lands first. | One milestone's worth of waiting. |

Immediate is the right one when something is wrong — the loop is building
against a stale assumption, or the owner has read something they want changed
before more lands on top of it. Deferred is the right one when nothing is wrong
and the owner simply wants the run to end without abandoning work that is nearly
done: a half-built milestone is the one state this repository cannot commit, so
stopping immediately means throwing it away or leaving it in the tree.

### Stopping now

1. Orchestrator spawns nothing further. A worker in flight is stopped.
2. Orchestrator rewrites `.current-task.md` against `git status --short` and
   `git log -1` — the tree, not the worker's report. Which milestone, which
   step, which actor held it, what is uncommitted, which gates had passed.
3. Report and end the turn. Nothing is committed, pushed, or reverted.
   Uncommitted work stays in the tree; removing it is the owner's call.

A worker stopped mid-tool-call cannot write the note. That is why step 2
rewrites it the moment a line stops being true, and why the reconciliation above
reads the tree instead of trusting what was reported.

### Stopping at the checkpoint

The **checkpoint** is the end of [step 3.9](#3-land) for the milestone in
flight — committed, demo-updated, pushed, note reset. Nothing else is one. Not
the end of a gate, not the worker's report, not a tidy-looking moment inside
step 2: a checkpoint is a state the repository can be left in, and that is
exactly the state 3.9 produces.

1. Orchestrator records the pending stop in `.current-task.md` **immediately**,
   on its own line, before doing anything else. A deferred stop that lives only
   in the conversation is lost to a crash or a context limit, and the run then
   continues past the point the owner asked it to end.
2. The milestone in flight finishes normally — steps 2 through 3.9, including a
   rejection and a fresh worker if the work does not pass. Nothing is rushed and
   no gate is skipped. A stop is not a reason to accept work that would
   otherwise be rejected.
3. At 3.9, stop instead of returning to step 1. Report as [§4](#4-repeat-or-stop)
   would, naming the stop as the reason.

Three things override it, and each simply arrives first:

- A [§4](#4-repeat-or-stop) stop condition — the phase ends, a prompt is raised,
  a gate fails twice. The run was ending anyway; say which reason won.
- Nothing in flight. The orchestrator holding step 1 with no worker spawned is
  already at a checkpoint, so a deferred stop there is an immediate one.
- The owner saying **stop** afterwards. Immediate always wins, and asking for it
  after asking for deferred is a change of mind, not a conflict to resolve.

---

## The current-task note

`.current-task.md`, repo root, gitignored — `git status --porcelain` skips
ignored files, so it never dirties the tree that `make demo-update` and
`make release-check` require clean.

Working state only. Everything else already has a home:

| Not here | There |
| --- | --- |
| Milestone status | phase-details/README.md — status lives there and only there |
| Why anything was decided | decisions.md |
| A decision not yet taken | upcoming-decisions.md |
| Out-of-spec findings | deferred-findings.md |
| Anything the owner said in passing | `.queue.md`, via `/note` |
| Scope, definitions of done | Plan.md, phase-details/ |

Two untracked files sit in the repo root and they are not interchangeable.
`.current-task.md` is *this milestone's* working state and is reset at step 3.9.
`.queue.md` is a list of things to deal with **later**, survives milestones, and
empties only when `/process-queue` drains it.

A line that would still matter after the milestone lands is in the wrong file.
Move it, do not copy it.

Contents — nothing beyond these:

```markdown
# Current task

M21 — Audit log · step 2 (build) · worker · branch phase-2

Done:    writer; actor_label + ip_prefix asserted by test; root-redirect event
Next:    GET /api/v1/audit — keyset pagination, audit.read gate
Blocked: none        # or the prompt, verbatim, and that it is unanswered
Stop:    none        # or: at the checkpoint — owner asked <date>
                     # or: after M45 — milestone target, /work M45 phase

Cost too much to re-derive:
- audit.read seed follows the 00800 insert-and-grant pattern
- retention default 0 = keep forever (D5)
```

The header names the actor holding the milestone. Whichever actor that is
rewrites the note at every step boundary; the orchestrator reconciles it against
the tree at each handoff, and step 3.9 resets it.

`Stop:` is the one line step 3.9 reads before resetting the rest, and it is why
a [deferred stop](#stopping-at-the-checkpoint) — or a
[milestone target](work-loop.md#a-milestone-target)'s bound — survives a crash.
It is the orchestrator's alone: a worker never writes it, because a worker never
learns of one.

*Cost too much to re-derive* carries weight it did not before: a worker starts
with no memory of the last milestone, so what a worker would otherwise
rediscover belongs here.

It exists so that an interruption — a crash, a compaction, a context limit, the
owner saying stop — costs effort and not knowledge. It is not a reason to
interrupt yourself: a note that clears the bar below is the normal state of this
file, not a signal that stopping is now free.

### The resume bar

*Good enough to resume from* is an intention, and an intention cannot be wrong.
The bar is a claim instead:

> **A fresh session that reads this note and [step 0](#0-resume)'s inputs reaches
> step 1 for the next milestone without asking the owner anything.**

Checked at [3.9](#3-land), against the note just written, by the actor that wrote
it. The test is to name what the note omits that would force a question — a
decision taken this session and not yet in decisions.md, a rejection whose reason
lives only in the conversation, a gate that passed for a reason nobody recorded.
Nothing named, it clears. Something named, it goes in the note or in the file
that owns it *before* 3.9 ends.

The bar is deliberately about questions rather than about completeness. A note
listing everything is unreadable and is not the goal; a note that leaves the next
session unable to proceed without the owner is the failure, and it is the only
one worth checking for.
