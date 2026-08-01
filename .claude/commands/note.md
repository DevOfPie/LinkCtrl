---
description: Capture a note into the queue without interrupting the work in flight
---

Append what the owner said to `.queue.md` at the repo root, then stop. Nothing
else.

**This command decides nothing.** It does not classify, does not read the tree,
does not search for prior art, does not touch the milestone in flight, and does
not ask a question. Those all belong to `/process-queue`, which runs later with
the tree in front of it. Capture that costs anything is capture the owner learns
not to use.

## Arguments

`$ARGUMENTS` is the note. It may begin with a type when the owner already knows
it — `--issue`, `--feature`, `--task` — and usually will not.

| Type | Is |
| --- | --- |
| `issue` | a change to existing function or design |
| `feature` | an addition of new function or design |
| `task` | a change to workflow or process, not to the product |

No type given → the row goes under **Unclassified**, and that is the normal
case. Do not infer one. A guess written into the file is indistinguishable from
the owner's own answer once the context that made it is gone.

## Writing the row

Create `.queue.md` from this skeleton if it does not exist:

```markdown
# Queue

Captured notes waiting to be classified and routed. Untracked and transient —
`/process-queue` drains it. See the queue trigger in
docs/build-notes/workflow.md.

## Unclassified

## Classified
```

Then append one row to the right section:

```
- [ ] <date> · <MN or "no milestone"> · <the note, in the owner's words>
```

- **Date** — today's, from context. Never invent one.
- **Milestone** — whatever `.current-task.md` names as in flight, so the row
  carries where it came from. Absent note → `no milestone`. This is the same
  rule as [Marking what gets appended](../../docs/build-notes/phase-loop.md#marking-what-gets-appended):
  written now because it is unrecoverable later.
- **The note** — the owner's words. Tidy the typing, not the meaning. Do not
  expand it into a specification; the queue is a reminder that something was
  said, not a definition of done.
- Classified rows carry their type first: `- [ ] issue · <date> · …`

Append `· blocking?` if the owner said it blocks the current work, or if the
note plainly contradicts what the milestone in flight is building. The question
mark is load-bearing: it is a flag for the orchestrator to judge at the next
step boundary, not a claim, and never a reason for this command to stop
anything.

## Then

Confirm in one line — the section written to and the note as recorded — and end
the turn. Do not begin processing it, and do not offer to.
