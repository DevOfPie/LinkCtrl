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
*gates* — what must pass. This file wins on *sequence* — what order, and when to
stop. A conflict between them is a bug: report it, do not pick.

---

## The loop

```
0 resume    read .current-task.md → rejoin the milestone in flight, or start fresh
1 validate  the next milestone, against the tree as it is now
2 build     that milestone, and nothing else
3 land      gates → commit → demo-update → push
4 repeat    from 1, unless a stop condition fired
```

Four rules outrank every step:

| Rule | Meaning |
| --- | --- |
| **Ask, never assume** | Any choice the owner would want → decision prompt, then *wait*. Picking one and proceeding is a process failure even when the pick is right. |
| **Never cross a phase boundary** | The loop ends at the phase's last milestone. Starting the next phase is a new instruction, not the next iteration. |
| **Only the table stops you** | [§4](#4-repeat-or-stop) lists every stop condition and is exhaustive. A reason that is not in it is not a reason — continue. Stopping early is the most common way this loop has failed. |
| **Keep `.current-task.md` true** | Rewrite it at every step boundary, so that a stop you did not choose costs effort only, never knowledge. It is a safety net for being interrupted, never a licence to interrupt yourself. |

---

## 0. Resume

`.current-task.md`, repo root, gitignored. Absent → start at step 1.

Present → it names a milestone and a step. Trust it for *intent*; verify
*state* independently — `git status`, `git log -1`, and the status row in
[phase-details/README.md](phase-details/README.md) are what actually landed. The
note is only what was being attempted.

Note records an unanswered prompt → re-ask it. Never answer it yourself.

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
<reason>`, plus the prompt. **Validation never edits code.**

## 2. Build

- Status row → `in progress` in phase-details/README.md.
- **In spec only.** Anything else → one row in deferred-findings.md, then carry
  on. Never fix out of spec, never bundle a second milestone.
- A test that passes first try → sabotage it, confirm red, restore by
  counter-edit. Never `git checkout`.
- Work on the **test** instance. The demo is touched at step 3 and nowhere else.
- Rewrite `.current-task.md` the moment a line in it stops being true.

## 3. Land

Order is load-bearing. Do not reorder.

1. `make check`, then `make test-integration` with the stack up (`make up`)
2. Every gate in workflow.md's *Before completing a commit* table
3. Docs true: Plan.md, CHANGELOG.md, `docs/*.md`; decisions.md **appended**
   (never edited) with its index row
4. Status row → `done`
5. `make check-links`
6. **Commit.** One milestone maximum. Message is prose about *why*.
7. `make demo-update` — refuses on a dirty tree, and that refusal means the
   milestone is not finished. Failure → back to step 2. **Do not push.**
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
| The owner said stop | — |

**That table is exhaustive.** Landing a milestone is not an event; it is one
iteration. The default after step 3 is step 1 again, and it takes a row above to
override that.

### Not stop conditions

Every one of these has ended a run in practice, which is why each is named
rather than left to judgement. None of them is a reason.

| Not a reason | Do this instead |
| --- | --- |
| Context is long, or running out | Keep the note true and carry on. Context is summarized automatically and the run continues; wrapping up early throws away a working run to avoid a problem that handles itself. |
| The next milestone is large, or touches many files | Start it. Step 2 is interruptible at any point, which is what the note is for. |
| The next milestone needs a long job — k6, a reseed, a rebuild | Start it. A job being slow is not a job being risky. |
| Two, or three, or five milestones have landed | The phase ends at its last row, not at a round number. |
| It looks like a clean place to hand off | Handing off is the note's job, and the note is already written. A "clean boundary" is indistinguishable from the next iteration. |
| The work so far deserves review | Land it and keep going. It is committed and pushed; the owner can read it whenever they like without the loop pausing. |

Reporting mid-run is fine and costs nothing — say what landed, then start the
next milestone in the same turn. What is not fine is ending the turn on a
summary when [§4](#4-repeat-or-stop)'s table has not fired.

### Two milestones that do not end like the others

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

M21 — Audit log · step 2 (build) · branch phase-2

Done:    writer; actor_label + ip_prefix asserted by test; root-redirect event
Next:    GET /api/v1/audit — keyset pagination, audit.read gate
Blocked: none        # or the prompt, verbatim, and that it is unanswered

Cost too much to re-derive:
- audit.read seed follows the 00800 insert-and-grant pattern
- retention default 0 = keep forever (D5)
```

Rewritten at every step boundary; reset by step 3's last line.

It exists so that an interruption — a crash, a context limit, the owner saying
stop — costs effort and not knowledge. It is not a reason to interrupt yourself:
a note good enough to resume from is the normal state of this file, not a signal
that stopping is now free.
