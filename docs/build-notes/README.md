# How this project is built

An outsider's map of the working method, for someone reviewing *how* LinkCtrl is
built rather than *what* it does. The product is [README.md](../../README.md);
the mechanics are the files in this directory, and this page says what each is
for and why it exists at all.

It summarizes. Every file named here is the authority on its own subject, and
where this page and one of them disagree, they are right.

## The problem this method solves

Most of the code here is written by a language model working unattended, for
hours at a stretch, with no memory between sessions. That constrains the method
in ways an all-human project is not constrained:

- **Nothing can rely on remembering.** A decision not written down did not
  happen. An intention held only in someone's head is lost at the next context
  boundary.
- **Scope creep is the default failure**, not an occasional one. An agent that
  notices a defect while building something else will fix it, bundle it, and
  produce a diff nobody can review.
- **A report is not evidence.** An actor that did work is the worst judge of
  whether the work is done, and will say it is.

Every rule below is a response to one of those, and most were added after the
corresponding failure actually happened. [decisions.md](decisions.md) is
append-only and records which.

## The shape of it

| File | Holds | Read when |
| --- | --- | --- |
| [../../Plan.md](../../Plan.md) | Scope. What is in, what is out, what order | Deciding *what* |
| [workflow.md](workflow.md) | The gates. What must pass before anything lands | Every task |
| [work-loop.md](work-loop.md) | Routing. Which target, which kind, which loop — and the loop over process changes | `/work` is invoked |
| [phase-loop.md](phase-loop.md) | The sequence. Who does what, in what order, and when to stop | Building a phase |
| [planning.md](planning.md) | How a requested feature becomes planned work | A feature is asked for |
| [phase-details/](phase-details/) | One definition of done per milestone, plus the status table | Building one milestone |
| [decisions.md](decisions.md) | Why. Append-only; a later entry corrects an earlier one, nothing is edited | Wondering why something is the way it is |
| [deferred-findings.md](deferred-findings.md) | Defects found at the wrong moment, parked rather than fixed | A defect turns up out of scope |
| [upcoming-decisions.md](upcoming-decisions.md) | Questions with no answer yet — one section the loop will stall on, one it will not | Answering ahead of the loop, or at leisure |
| [workflow-changes.md](workflow-changes.md) | Changes to the process itself: proposed, and made | Asking what the contract used to be, or what is queued to change |
| [development.md](development.md) | Toolchain and local setup | Running it yourself |
| [doc-cost.md](doc-cost.md) | What the always-read documents cost to read, measured | Adding to them |

Precedence is fixed and conflicts are bugs: Plan.md wins on *what*, workflow.md
on *what must pass*, phase-loop.md on *what order*, work-loop.md on *which loop
is entered at all*. An actor that finds two of them disagreeing reports it rather
than picking one.

## Four ideas that carry most of the weight

**One milestone per commit, and a milestone is a written definition of done.**
Scope is fixed before the work starts, in a file the builder reads and the
reviewer re-reads. Not a description of what was built — a claim to be checked
against the tree afterwards.

**The builder does not accept its own work.** The loop runs as two actors: a
*worker* builds one milestone and stops before the commit, and an *orchestrator*
validates beforehand, checks the tree against the definition of done, and
commits. A fresh worker per attempt, so a second try does not inherit the first
one's reasoning. The orchestrator re-runs the gates itself; the worker's report
of a passing gate is not evidence that it passes.

**Out of spec means it does not get fixed.** A defect found while building
something else is written to [deferred-findings.md](deferred-findings.md) — what,
where, the evidence it is real, how bad — and then left alone. The owner reviews
each row individually and approves it or does not. The single exception is a
defect that makes the *current* milestone's own claim false, which is in scope by
definition. Everything else waits, and that is what keeps an unattended run from
quietly growing its own scope.

There is one consequence worth stating plainly: a milestone that shipped and
later turns out to have claimed something untrue is **reopened** rather than
succeeded by a follow-up. A `done` row asserting something false is the outcome
worth spending a reopening to avoid. It has happened; see M24.5.

**A test that has never failed has not been shown to test anything.** A test that
passes on the first attempt is deliberately sabotaged — break the thing it
protects, watch it fail, restore. Written down because it is the step everyone
skips.

## What an external reviewer can check

The method is designed to be auditable by someone who does not trust any of its
participants, so the artifacts are the point:

- **Every claim is falsifiable.** Documentation is held to "no numbers without a
  measurement, no *should be fast*, no feature described in the present tense
  that is not built." Where a measurement is quoted, the way it was taken is
  recorded next to it.
- **The record shows changes of mind.** decisions.md is append-only, so a
  reversed decision leaves both entries standing. When a milestone's definition
  of done is amended mid-build, the entry carries the bullet as it stood, the
  bullet as amended, and the tree fact that forced it.
- **Status lives in exactly one place** — the table in
  [phase-details/](phase-details/README.md). Rationale never appears in the plan
  and status never appears in the decision log.
- **Nothing leaves a tracker silently.** A row is re-homed into another tracker,
  which names where it went, or its removal is logged with a reason. Deciding
  something no longer matters is itself a decision, and an unrecorded one comes
  back later as a fresh idea with its reasoning lost.
- **The gates are runnable.** `make check`, `make test-integration`,
  `make check-links`, `make release-check`. Nothing lands without them, and they
  do not depend on anybody's judgement.

## What it costs

This is not a lightweight process and it is not proposed as one. The
always-read documents are measured in [doc-cost.md](doc-cost.md) precisely so
their growth is visible in a diff rather than absorbed. The two-actor split
roughly doubles the reading done per milestone, since the orchestrator re-reads
the definition of done and re-runs the gates the worker already ran. The
deferral discipline means known defects sit unfixed, sometimes for a whole phase.

Each of those is a deliberate trade against a failure that had already happened
more than once. Whether the trade is worth it for a project with different
constraints is a fair question, and nothing here assumes it is.
