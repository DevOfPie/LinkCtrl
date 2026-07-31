# The phase loop

Trigger: **"Work on Phase"**, or `/work-on-phase`. Builds the current phase one
milestone at a time, unattended, until the phase ends.

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
| **Ask, never assume** | Any choice the owner would want → decision prompt, then *wait*. Picking one and proceeding is a process failure even when the pick is right. |
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
| Bullets falsifiable now | a bullet names a file, test or behaviour that no longer exists | **prompt**, carrying the proposed amendment |
| Decisions cover it | it needs a choice no `D`-numbered decision made | **prompt** |
| Deferred overlap | an open row in [deferred-findings.md](deferred-findings.md) would make *this* milestone's claim false | in spec → fix here, close the row pointing at MN |

That last row is the only path by which an unapproved finding becomes work. It
follows from workflow.md — *a defect that makes the current milestone's claim
false is in spec, whatever it looks like* — and not from owner approval, so name
the exception in the commit message and keep it visible.

Output is one line: `MN validates`, plus notes; or `MN does not validate:
<reason>`, plus the prompt. **Validation never edits code**, and mN.md stays
read for step 3.4.

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
9. Reset `.current-task.md` to the next milestone at step 1

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

**That table is exhaustive.** Landing a milestone is not an event; it is one
iteration. The default after step 3 is step 1 again, and it takes a row above to
override that.

### Not stop conditions

Every one of these has ended a run in practice, which is why each is named
rather than left to judgement. None of them is a reason.

| Not a reason | Do this instead |
| --- | --- |
| Context is long, or running out | Keep the note true and carry on. Context is summarized automatically and the run continues; wrapping up early throws away a working run to avoid a problem that handles itself. |
| A worker returned, and its milestone was accepted and pushed | That is one iteration ending, not the run. Spawn the next worker in the same turn. |
| The next milestone is large, or touches many files | Start it. Step 2 is interruptible at any point, which is what the note is for. |
| The next milestone needs a long job — k6, a reseed, a rebuild | Start it. A job being slow is not a job being risky. |
| Two, or three, or five milestones have landed | The phase ends at its last row, not at a round number. |
| It looks like a clean place to hand off | Handing off is the note's job, and the note is already written. A "clean boundary" is indistinguishable from the next iteration. |
| The work so far deserves review | Land it and keep going. It is committed and pushed; the owner can read it whenever they like without the loop pausing. |

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

---

## Stop work

Trigger: the owner says **stop work**.

1. Orchestrator spawns nothing further. A worker in flight is stopped.
2. Orchestrator rewrites `.current-task.md` against `git status --short` and
   `git log -1` — the tree, not the worker's report. Which milestone, which
   step, which actor held it, what is uncommitted, which gates had passed.
3. Report and end the turn. Nothing is committed, pushed, or reverted.
   Uncommitted work stays in the tree; removing it is the owner's call.

A worker stopped mid-tool-call cannot write the note. That is why step 2
rewrites it the moment a line stops being true, and why the reconciliation above
reads the tree instead of trusting what was reported.

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
| Out-of-spec findings | deferred-findings.md |
| Scope, definitions of done | Plan.md, phase-details/ |

A line that would still matter after the milestone lands is in the wrong file.
Move it, do not copy it.

Contents — nothing beyond these:

```markdown
# Current task

M21 — Audit log · step 2 (build) · worker · branch phase-2

Done:    writer; actor_label + ip_prefix asserted by test; root-redirect event
Next:    GET /api/v1/audit — keyset pagination, audit.read gate
Blocked: none        # or the prompt, verbatim, and that it is unanswered

Cost too much to re-derive:
- audit.read seed follows the 00800 insert-and-grant pattern
- retention default 0 = keep forever (D5)
```

The header names the actor holding the milestone. Whichever actor that is
rewrites the note at every step boundary; the orchestrator reconciles it against
the tree at each handoff, and step 3.9 resets it.

*Cost too much to re-derive* carries weight it did not before: a worker starts
with no memory of the last milestone, so what a worker would otherwise
rediscover belongs here.

It exists so that an interruption — a crash, a context limit, the owner saying
stop — costs effort and not knowledge. It is not a reason to interrupt yourself:
a note good enough to resume from is the normal state of this file, not a signal
that stopping is now free.
