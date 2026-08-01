---
description: Find the decisions the loop will hit next, so they can be answered early
---

Read ahead of the loop and collect the questions it has not reached yet. You are
the **orchestrator**. This command builds nothing, edits no code, and answers
nothing — its whole product is
[upcoming-decisions.md](../../docs/build-notes/upcoming-decisions.md), a file of
open questions.

An unanswered prompt is the phase loop's only self-inflicted stop condition. The
point of reading ahead is that the owner can answer at leisure, in any session,
before the loop is standing still waiting.

## Scope

Default: the next **three** un-`done` rows in
[phase-details/README.md](../../docs/build-notes/phase-details/README.md).
`$ARGUMENTS` may name a count or specific milestones instead. Never cross the
phase boundary — the loop does not, and neither does reading ahead of it.

## 1. Find

For each milestone in scope, run [step 1](../../docs/build-notes/phase-loop.md#1-validate)'s
checks in *read-only* form. Read `phase-details/mN.md`, that README's inherited
rules, and Plan.md's ordering row for N. The check that matters most is
**decisions cover it** — a choice no `D`-numbered decision has made — but the
others surface questions too, and a question found early is worth the same
whichever check found it.

Two things this must not do:

- **Do not fix anything.** A dependency ordering bug or a bullet naming a file
  that no longer exists is a *finding*, and it becomes a question in the file
  like any other. Silently correcting it here means the loop validates against a
  plan someone quietly edited.
- **Do not decide anything.** Not even the obvious ones. A decision taken by the
  actor that will implement it is the failure the *ask, never assume* rule
  exists to prevent, and reading ahead does not relax it.

A milestone whose decisions are all covered gets no entry and one line in the
report. That is the good outcome, not an empty result.

## 2. Write

Append to `upcoming-decisions.md`, newest last, one entry per question, in the
format that file's header defines. Every entry carries:

- **The milestone** that will need it, and roughly when the loop gets there.
- **The question**, in one sentence, answerable without reading the milestone
  file.
- **Options with costs, and a recommendation** — the same standard as any live
  prompt, per [workflow.md](../../docs/build-notes/workflow.md#standing-rules).
  Reading ahead is not an excuse for a thinner prompt; it is the case where the
  owner has the most time to weigh one.
- **What it assumes** about a tree that is not built yet. This field is why the
  answer can be trusted later. Be specific and falsifiable — "M31 lands the
  workspace switcher before this" — because validation re-checks exactly these
  and a false one re-opens the question.

Do not duplicate a question already in the file. An entry whose milestone has
since been built and whose question went unasked is deleted, and the report says
so — it means the milestone did not need it after all.

## 3. Report

Per milestone: covered, or the questions raised. Then the count now waiting in
the file. Answering happens in conversation, or by the owner editing the file
directly; either way an entry leaves it only via
[decisions.md](../../docs/build-notes/decisions.md), and the answer is appended
there with its `D` number when the milestone that uses it lands — carrying the
date it was *given*, not only the date it was used.
