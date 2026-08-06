# The work loop

Trigger: `/work [target …] <kind> [flags]`.

`/work` does one thing: it resolves a **route** and enters that route's **loop**.
It builds nothing itself, decides nothing the route's loop decides, and it never
ends in a one-shot — every route terminates in a loop that runs until one of
that loop's own stop conditions fires.

Style matches [workflow.md](workflow.md) — terse, trigger-first, no rationale.
The *why* is in [decisions.md](decisions.md).

**Precedence.** This file wins on *routing* — which target, which kind, which
loop, and what happens when one of them is unknown. It wins on nothing else.
Once a loop is entered, that loop's file wins on sequence, [workflow.md](workflow.md)
wins on gates, and [Plan.md](../../Plan.md) wins on scope. A conflict between
them is a bug: report it, do not pick.

---

## The grammar

```
/work                            backlog for every kind, and a recommendation. Enters nothing.
/work <kind>                     this repository, that kind
/work <target> <kind>            that target, that kind
/work <target> <target> <kind>   nested — each target narrows the one before it
/work <milestone> phase          the phase loop, stopping when that milestone lands
/work … --revalidate             re-derive the route and prompt before entering
```

**The kind is the last token. Everything before it is a target.** That is the
whole parse, and it is stated as a rule rather than inferred so that a target
that happens to share a name with a kind still parses.

Empty target list → this repository. `/work phase` and `/work linkctrl phase`
are the same instruction.

---

## Kinds

| Kind | Loop | Runs over |
| --- | --- | --- |
| `phase` | [phase-loop.md](phase-loop.md) | The un-`done` rows in [phase-details/README.md](phase-details/README.md), until the phase ends |
| `workflow` | [The workflow loop](#the-workflow-loop), below | The **approved** *Proposed* rows in [workflow-changes.md](workflow-changes.md), until none is left |

**A kind with no loop is not a kind.** Adding one means writing its loop first —
its steps, its actors, and an exhaustive table of what stops it. A route that
ends in "then do the work" is a route that ends nowhere, and the loop is the
thing `/work` exists to reach.

An unknown kind is handled exactly like an [unknown target](#an-unknown-target).

---

## Targets

A target names **where** the work is, not what it is. Targets nest, outermost
first, each narrowing the one before it. Two levels exist — the repository, and
a milestone inside it — and the nesting rule is written for the level above
both, which is [cross-repository dispatch](#what-this-cannot-do-yet).

### Route table

Every known target, and what it accepts. This table **is** the route: a target
absent from it is unknown, whatever it looks like.

| Target | Is | Kinds |
| --- | --- | --- |
| `linkctrl` | This repository | `phase`, `workflow` |
| `M<n>` | A milestone, spelled as [phase-details/README.md](phase-details/README.md)'s status table spells it — `M45`, `M24.5` | `phase` |

### A milestone target

`/work M45 phase`. Nested form `/work linkctrl M45 phase` is the same
instruction, the same way `/work phase` and `/work linkctrl phase` are.

**It bounds the loop; it does not choose the work.** The phase loop resumes and
iterates exactly as it always does — [step 0](phase-loop.md#0-resume) reads the
note, [step 1](phase-loop.md#1-validate) takes the next un-`done` row in the
status table's order. One stop condition is added: the run ends when the named
milestone lands.

That is a ceiling and never a floor. A milestone target can only stop a run
**sooner** than it would otherwise stop. It never skips a row ordered before the
named one, never starts one ordered after it, and weakens no other condition —
so [phase-loop.md](phase-loop.md#4-repeat-or-stop)'s exhaustive table gains a row
rather than an exception, and whichever condition fires first wins.

Skipping is the thing it deliberately cannot do. The status table's order and its
*Depends on* column are what decide which milestone is next; a target that jumped
straight to its milestone would build one whose dependencies are un-`done`, which
is a worse outcome than the wait it was trying to avoid.

Resolved against the status table **before** the loop is entered:

| The named row | Then |
| --- | --- |
| Un-`done`, this phase | Route. The bound is written to the note's `Stop:` line, which is what carries it across a resume |
| Already `done` | Enter nothing, and report that. The run being asked for has already happened; re-running it is [reopening](phase-details/README.md), which is scheduling and therefore the owner's |
| A row in another phase | **Prompt.** *Never cross a phase boundary* is one of [phase-loop.md](phase-loop.md#the-loop)'s five overriding rules, and this target asks for exactly that |
| Absent from the table | An [unknown target](#an-unknown-target), handled unchanged |

Milestone targets take `phase` and nothing else. The workflow loop runs over
process rows, which no milestone owns.

### An unknown target

Never guessed, never silently rejected, and never routed to the nearest match
because it was close.

1. **Stop before entering any loop.** Nothing is spawned, no note is rewritten.
2. **Prompt**, carrying: what was typed, the known targets it is nearest to, and
   what each of those would actually do if chosen. Options, costs and a
   recommendation, per [workflow.md](workflow.md#standing-rules).
3. **Then note it, where noting is appropriate** — and which of these applies is
   the owner's answer, not a judgement made from the spelling:

   | The answer is | Then |
   | --- | --- |
   | A real target this table does not know | Append a row to the route table. That is a workflow change, so it commits on its own per the scope gate, and it is a *Made* row in [workflow-changes.md](workflow-changes.md) |
   | A typo for a known target | Route to the intended one. **Note nothing** — a route table that accumulates misspellings stops being a list of targets |
   | A target that should exist and does not yet | One row in [workflow-changes.md](workflow-changes.md)'s *Proposed* table, unapproved. Do not route |

A near-match is the case this exists for. `/work linkctl phase` is one keystroke
from a valid route and one prompt from a wrong one, and the cost of asking is a
sentence.

---

## Flags

| Flag | Also accepted | Does |
| --- | --- | --- |
| `--revalidate` | `-Revalidate`, `-reval`, `-r` | Discards the route recorded for these arguments, re-derives it against the tree, and **prompts with the result before entering the loop** |

The single-dash forms are accepted because the owner asked for them by name. The
long form is `--revalidate`, matching [`/note`](../../.claude/commands/note.md)'s
`--issue|--feature|--task`, so the surface has one convention and three
concessions rather than two conventions.

**Revalidate is about the route and nothing else.** `/work phase -r` re-checks
which loop to enter and asks; the phase loop then reads `.current-task.md` at
[step 0](phase-loop.md#0-resume) exactly as it always does. It is not a way to
force a rebuild, re-run a gate, or discard a milestone in flight.

What it is for: a route table is a written claim about a tree that changes.
Re-deriving means reading the tree the row asserts — that the kind's loop file is
where the row says, that the backlog it names is the backlog that exists — and
saying so before an unattended run starts on it.

---

## `/work` with no kind

Report the backlog for **every** kind, then recommend one. Enter nothing.

Per kind: how many rows are outstanding, which one is next, and what would
happen if it ran. Then one recommendation, with its reason and its cost, per the
prompt-format rule in [workflow.md](workflow.md#standing-rules).

The recommendation is a judgement and it is **weakly grounded, deliberately so**:
nothing in this repository records how necessary a pending workflow change is,
so a recommendation between kinds compares a phase's ordered plan against an
unordered list. Say that in the report rather than presenting a ranked answer.
Recording an urgency for process changes is a change to
[workflow-changes.md](workflow-changes.md)'s shape and is not made here.

---

## The workflow loop

Kind `workflow`. Runs over [workflow-changes.md](workflow-changes.md)'s
*Proposed* table until no approved row is left.

```
0 resume  ORCH  read .current-task.md → rejoin the row in flight, or start fresh
1 select  ORCH  the first approved Proposed row; verify it against the tree
2 make    ORCH  that row, and nothing else
3 land    ORCH  gates → commit → move the row to Made → push
4 repeat  ORCH  from 1, unless a stop condition fired
```

**This loop is not delegated.** One actor throughout, and that is a departure
from [phase-loop.md](phase-loop.md)'s two-actor split, which exists because a
builder is the worst judge of its own work. Two reasons override it here, and
both are about what the work *is* rather than about it being small:

- A workflow change edits the contract the split is written in. A worker
  rewriting [workflow.md](workflow.md) is editing its own instructions mid-run,
  and the orchestrator's acceptance step would then be run against a document
  that moved underneath it.
- Approval is per row and it is the owner's, so the product of each iteration is
  a conversation. That is the same reason `X.9` reviews and the phase close are
  not delegated — see [phase-loop.md](phase-loop.md#two-milestones-that-do-not-end-like-the-others).

What is lost by it is real and is not offset: nobody independent checks the
change against the tree. The gates below are what is left, and they are weaker
than a second reader.

### 1. Select

**Next row** = the first row in the *Proposed* table whose **Approved** column
says yes. An unapproved row is a suggestion, not scheduled work, and this loop
never approves one.

Then verify it against the tree, because a proposed row was written before the
change existed:

| Check | Fails when | Then |
| --- | --- | --- |
| Still absent | the tree already does what the row asks | close it — a *Made* row naming where, and no commit of its own |
| Still a task | making it needs code, SQL, config or a test change | **prompt.** A workflow change that touches the product is not a task; it is a feature or an issue, per [`/process-queue`](../../.claude/commands/process-queue.md)'s dispute table |
| Dependencies made | it names another row that is not yet *Made* | ordering → **prompt** |
| Decisions cover it | it needs a choice no `D`-numbered decision made | **prompt** |

### 2. Make

- **That row only.** Anything else noticed → the trigger in
  [workflow.md](workflow.md#an-issue-is-found--any-time-any-source) applies
  unchanged: in spec, or a deferred row.
- Rewrite `.current-task.md` the moment a line in it stops being true.

### 3. Land

1. `make check-links`, and `make check` if anything outside `.md` changed
2. Every applicable gate in [workflow.md](workflow.md#before-completing-a-commit)'s
   table. `make demo-update` does **not** run — a workflow change adds nothing
   an operator can see, and the demo is rebuilt from milestones
3. **Commit.** One row maximum, per the scope gate
4. Move the row from *Proposed* to *Made*, carrying its commit
5. Append the reasoning to [decisions.md](decisions.md) — **no milestone
   number**, naming what prompted it, per
   [phase-loop.md](phase-loop.md#marking-what-gets-appended)
6. `git push`
7. Reset `.current-task.md` to the next row at step 1

### 4. Repeat, or stop

Repeat from step 1. Otherwise stop and report:

| Stop when | Because |
| --- | --- |
| A prompt is unanswered | Ask, never assume |
| No approved *Proposed* row remains | The backlog is empty. Unapproved rows are not work |
| The same row failed a gate twice | Retrying is not progress |
| A row turns out to need product change | It is not a task. Step 1 already prompted; the loop waits on that answer |
| The owner said stop | [Stop work](phase-loop.md#stop-work) — both stops apply here, and the checkpoint is the end of step 3 |

**That table is exhaustive**, for the reason [phase-loop.md](phase-loop.md#4-repeat-or-stop)'s
is: landing one row is an iteration, not an event.

---

## What this cannot do yet

Stated because the grammar above promises more than the tree delivers, and a
route table that quietly covers one repository while its syntax describes many
is the drift this file exists to prevent.

**Cross-repository dispatch does not work.** `/work linkctrl phase` typed
outside this repository resolves nothing, because the command is defined in
[`.claude/commands/work.md`](../../.claude/commands/work.md) and is visible only
where this repository is checked out. Making it work needs the command visible
from any working directory — which today means an untracked file in the user's
home configuration, surviving no clone. That is
[workflow-changes.md](workflow-changes.md)'s W23, and it is unapproved.

**The route table is local.** Even given a globally visible command, this table
is the routes *this repository* knows. Nothing here is authoritative for another
repository's kinds, and a target row claiming otherwise would be a claim no
reader could check.

The nesting grammar is written now anyway, and that is deliberate: the parse rule
is the thing a second repository would otherwise invent differently.
