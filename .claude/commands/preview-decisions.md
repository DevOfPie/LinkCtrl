---
description: Find the decisions the loop will hit next, so they can be answered early
---

Read ahead of the loop and collect the questions it has not reached yet, write
them down, **then ask them**. You are the **orchestrator**. This command builds
nothing, edits no code, and answers nothing itself.

Its durable product is open LNK questions in Mustur, raised with
`mustur ask --project LNK` **before** a single question is put to the owner. The
order is the point: a run interrupted halfway through asking has still recorded
every question it found — *write it down first, then answer it*.

An unanswered prompt is the phase loop's only self-inflicted stop condition, and
since W33 it is a *conditional* one — the loop parks the question and falls back
to an independent milestone where one exists, and stops only where none does.
That makes reading ahead worth more rather than less: an answer given early costs
the loop no stall at all, where an answer given late costs whatever the loop
built past the question in the meantime.

## Scope

Default: the next **three** un-`done` rows of the
[status table](../../docs/build-notes/phase-loop.md#1-validate).
`$ARGUMENTS` may name a count or specific milestones instead. Never cross the
phase boundary — the loop does not, and neither does reading ahead of it.

## 1. Find

For each milestone in scope, run [step 1](../../docs/build-notes/phase-loop.md#1-validate)'s
checks in *read-only* form. Read MN's work unit, the inherited rules in
`milestone-rules.md`, and MN's LNK milestone record. The check that matters most is
**decisions cover it** — a choice no LNK decision has made — but the
others surface questions too, and a question found early is worth the same
whichever check found it.

Two things this must not do:

- **Do not fix anything.** A dependency ordering bug or a bullet naming a file
  that no longer exists is a *finding*, and it becomes a question
  like any other. Silently correcting it here means the loop validates against a
  plan someone quietly edited.
- **Do not decide anything.** Not even the obvious ones. A decision taken by the
  actor that will implement it is the failure the *ask, never assume* rule
  exists to prevent, and reading ahead does not relax it.

A milestone whose decisions are all covered gets no entry and one line in the
report. That is the good outcome, not an empty result.

## 2. Write

Raise each with `mustur ask --project LNK`, one question per record, its answers
as `--option`. Every one carries:

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

Do not duplicate an open LNK question. One whose milestone has since been built
and which went unasked is withdrawn (`mustur answer --withdraw`), and the report
says so — it means the milestone did not need it after all.

## 3. Ask

Put every entry this run wrote to the owner. Not a summary of them — the entry
already carries options, costs, a recommendation and its assumptions, because
step 2 required all four, so **the prompt is that content and nothing newly
authored**. A question phrased differently in the conversation than in Mustur
is two questions.

Ask only what this run wrote. Questions already open were asked when they
were raised; re-asking them every run is how a read-ahead becomes noise.

| The owner | Then |
| --- | --- |
| Answers | The answer is recorded on the question (`mustur answer`) and filed as an LNK decision **now** (`mustur add decision --project LNK`, citing the question), carrying the date it was given. A question holds no decision, so an answer left only on it is an answer that will be re-taken |
| Says *you decide* | That is an answer: the entry's stated **default** is what happens. Record it as the owner's, with the note that it was taken as the default rather than chosen |
| Does not answer | The question stays open, unanswered and untouched. Nothing is inferred from silence, and the report names it |

**Never answer one yourself.** Reading ahead does not relax *ask, never assume*;
it moves when the asking happens, and this step is when.

## 4. Report

Per milestone: covered, or the questions raised. Then what each answer did —
filed as an LNK decision, or still waiting — and the count of open LNK
questions, including ones older than this run.
