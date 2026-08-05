---
description: End the loop in flight, now
---

Stop the running loop immediately. `docs/build-notes/phase-loop.md`'s *Stop work*
section is the contract and this command does not add to it; it makes the stop
invokable rather than only sayable. Equivalent to the owner saying **"stop
work"**.

`$ARGUMENTS` is not read. Anything given → say what was given, say that this
command takes no arguments, and **stop nothing**. A misread argument that stops
immediately when the owner meant something else throws away work.

## What to do

Follow [Stopping now](../../docs/build-notes/phase-loop.md#stopping-now):

1. Spawn nothing further. A worker in flight is stopped.
2. Rewrite `.current-task.md` against `git status --short` and `git log -1` — the
   tree, not any worker's report. Which unit, which step, which actor held it,
   what is uncommitted, which gates had passed.
3. Report and end the turn. Nothing is committed, pushed, or reverted.
   Uncommitted work stays in the tree; removing it is the owner's call.

## No loop is running

Say so, and stop nothing. Rewrite nothing — a `.current-task.md` reconciled by a
command that stopped nothing is a note claiming an interruption that did not
happen.

## The other stop

The owner may instead want the unit in flight to land first, and then stop. That
is **"stop at the checkpoint"**, it is
[phase-loop.md](../../docs/build-notes/phase-loop.md#stopping-at-the-checkpoint),
and this command does not perform it. Asked for it, say so and do not stop.
