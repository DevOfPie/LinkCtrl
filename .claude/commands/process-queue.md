---
description: Drain the queue — classify, verify, route — into the tracked files
---

Empty `.queue.md` into the files that outlive it. You are the **orchestrator**;
a worker never runs this. Run it at a milestone boundary, never inside step 2 —
routing writes to Plan.md, deferred-findings.md and phase-details/, and a
milestone in flight must not find those changing under it.

No `.queue.md`, or both sections empty → say so and stop.

## 1. Classify

Every row under **Unclassified**, one at a time, by what it changes:

| Type | Is | Test |
| --- | --- | --- |
| `issue` | a change to existing function or design | Something already claimed exists, and the note wants it different |
| `feature` | an addition of new function or design | Nothing today does this |
| `task` | a change to workflow or process, not to the product | The product's behaviour is untouched; the way it gets built changes |

Move each row to **Classified** with its type. A row you cannot classify from
what it says is not a hard row — it is an underspecified one. **Prompt**, do not
guess, and leave it under Unclassified until answered.

## 2. Verify

Then re-read every row under **Classified**, including the ones the owner typed
a type for and the ones you just moved. This step exists because step 1 is done
against the note's wording and this one is done against the tree, and the tree
disagrees more often than the wording admits.

| Dispute | Looks like |
| --- | --- |
| Typed `issue`, is a `feature` | Nothing claims to do this today, so nothing is broken — the note asks for new function. This is the common one, and it matters: an issue gets a findings row, a feature gets five artifacts and an owner decision on scope. |
| Typed `feature`, is an `issue` | The plan or the docs already claim it; the claim is false. That is a defect, and it may reopen a shipped milestone. |
| Typed `issue`, is neither | The behaviour is as designed and the note is a preference. Route it as a feature, or park it — but say which. |
| Typed `task`, touches the product | Workflow changes that need a code, SQL, config or test change are not tasks. Split the row, or reclassify it. |
| Already built | The note asks for something the tree does. Close the row pointing at where. |
| Already scheduled | A Plan.md row or phase-details file covers it. Close the row pointing at the number. |

**Every dispute is a prompt, and prompts wait.** Do not reclassify a row the
owner typed a type for on your own judgement — you have the tree, they have the
intent, and the disagreement is usually about which one the note was about.
Carry the row verbatim, what you found in the tree, and a recommendation, per
the prompt-format rule in [workflow.md](../../docs/build-notes/workflow.md#standing-rules).

Unanswered dispute → that row stays in the queue. The others still route.

## 3. Route

| Type | Destination | How |
| --- | --- | --- |
| `issue` | [deferred-findings.md](../../docs/build-notes/deferred-findings.md) | One row: what, where as `file:line`, evidence it is real, suspected severity, **Found in** = the milestone the queue row carries. Evidence means verified against the tree — an unverified note becoming a findings row is exactly the laundering this queue exists to prevent. Unverifiable → prompt, do not weaken the column. |
| `feature` | [planning.md](../../docs/build-notes/planning.md) | Its whole path — establish absence, decide the phase, number, five artifacts, verify. The owner decides *whether* and prompts happen before anything is written. Several feature rows → prompt with all of them before writing any, so scope is judged together. |
| `task` | Its own commit, or [workflow-changes.md](../../docs/build-notes/workflow-changes.md) | Not a milestone, so per the scope gate it commits alone, when complete. **Made now** → commit, plus a *Made* row so it is findable without grepping git log. **Not made now** → a *Proposed* row, because a process change waiting must be as visible as a defect waiting. Anything non-obvious gets a decisions.md entry with **no** milestone number, naming what prompted it. |

A `blocking?` flag is *yours* to judge, here and at no earlier point. Blocking
means the milestone in flight would build something the note makes wrong. If it
is: stop, report, and prompt — the milestone is the thing that pauses, not this
command. If it is not: drop the flag and route normally, and say that you did.

## 4. Close out

- Remove every routed row from `.queue.md`. Rows left behind are only the ones
  carrying an unanswered prompt, and the report names each and why.
- **A row leaves the queue only into a tracked file.** Every removal names its
  destination — a findings row, a Plan.md row and milestone file, a
  workflow-changes row, or a decisions.md entry recording that it was dropped and
  why. "Handled in conversation" is not a destination, and neither is a commit
  nobody can find without knowing the search term. This is workflow.md's
  no-silent-removal rule at the one place rows are removed in bulk.
- Anything decided while routing goes to decisions.md, or the question goes to
  upcoming-decisions.md. Routing forces judgement calls — which type a row is,
  which phase it belongs to, whether it duplicates something — and those are
  decisions whether or not they felt like it at the time.
- `make check-links` if anything with links was written.
- Report: what routed where, what was closed as already built or scheduled, what
  is waiting on a prompt.

The queue is untracked, so a row that is neither routed nor reported is simply
lost. Draining fully is the point of the command; draining *into somewhere* is
what makes the draining worth anything.
