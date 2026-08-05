---
description: End the loop in flight — now, or at the next checkpoint
---

Stop the running loop. `docs/build-notes/phase-loop.md`'s *Stop work* section is
the contract and this command does not add to it; it makes both stops invokable
rather than only sayable.

## Arguments

`$ARGUMENTS` selects which stop. Nothing else is accepted.

| Given | Means | Phrase it replaces |
| --- | --- | --- |
| *(empty)* | **Now.** Mid-build, mid-gate, wherever it is | "stop work", "stop" |
| `--checkpoint` | At the next checkpoint. The unit in flight lands first | "stop at the checkpoint" |

`-checkpoint`, `-check` and `-c` are accepted for `--checkpoint`, matching
[`/work`](work.md)'s concession on flag spelling.

Anything else → say what was given, name the two stops, and **stop nothing**. The
stops differ precisely in what they cost, so a misread argument that stops
immediately when the owner asked for deferred throws away the work the deferred
stop exists to save.

## Stopping now

Follow [Stopping now](../../docs/build-notes/phase-loop.md#stopping-now):

1. Spawn nothing further. A worker in flight is stopped.
2. Rewrite `.current-task.md` against `git status --short` and `git log -1` — the
   tree, not any worker's report. Which unit, which step, which actor held it,
   what is uncommitted, which gates had passed.
3. Report and end the turn. Nothing is committed, pushed, or reverted.
   Uncommitted work stays in the tree; removing it is the owner's call.

## Stopping at the checkpoint

Follow [Stopping at the checkpoint](../../docs/build-notes/phase-loop.md#stopping-at-the-checkpoint):

1. Record the pending stop in `.current-task.md` **immediately**, on its own
   line, before anything else. A deferred stop living only in the conversation is
   lost to a crash or a context limit, and the run then continues past the point
   the owner asked it to end.
2. The unit in flight finishes normally, including a rejection and a fresh worker
   if the work does not pass. Nothing is rushed and no gate is skipped — a stop
   is not a reason to accept work that would otherwise be rejected.
3. At the checkpoint, stop instead of returning to step 1, naming the stop as the
   reason.

The **checkpoint** is the end of the landing step for the unit in flight —
committed, demo-updated where that applies, pushed, note reset. Nothing else is
one: not the end of a gate, not a worker's report, not a tidy-looking moment
mid-build. That is
[step 3.9](../../docs/build-notes/phase-loop.md#stopping-at-the-checkpoint) for a
phase and [step 3](../../docs/build-notes/work-loop.md#3-land) for the workflow
loop.

Three things override a pending deferred stop, and each simply arrives first: a
stop condition the loop already had, nothing being in flight, and the owner
saying `/stop` afterwards. Immediate always wins.

## No loop is running

Say so, and stop nothing.

- Asked for the immediate stop → rewrite nothing. A `.current-task.md`
  reconciled by a command that stopped nothing is a note asserting an
  interruption that did not happen.
- Asked for `--checkpoint` → the orchestrator holding a validate step with no
  worker spawned is already at a checkpoint, so the deferred stop is an
  immediate one. Report which it became.
