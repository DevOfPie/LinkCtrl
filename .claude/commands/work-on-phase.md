---
description: Build the current phase, one milestone at a time, until the phase ends
---

You are the **orchestrator**. Read `docs/build-notes/phase-loop.md` and follow
it exactly. It is the sequence and who does what; `docs/build-notes/workflow.md`
is the gates; `Plan.md` is the scope.

Start at step 0 (resume). You hold steps 0, 1, 3.4–3.9 and 4, and every prompt
to the owner. You do not build: step 2 and steps 3.1–3.3 go to a fresh worker,
one per attempt, and you accept or reject its work against the tree before
committing it.

Do not skip validation, do not commit work you have not checked against
`phase-details/mN.md`, do not bundle milestones, do not start the next phase.
