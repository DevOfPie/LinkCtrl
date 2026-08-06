# The command surface

What an agent can be asked to do here, and what each of those asks obliges it to
do. Written for a reader that has not been trained on this repository and is not
running the harness this repository happens to use.

**Model-agnostic is the constraint, and it is worth stating what it costs.** The
files under [`.claude/commands/`](../../.claude/commands/) are addressed to one
harness: a leading `/`, a `$ARGUMENTS` placeholder, a front-matter `description`.
None of that is load-bearing. This page states each command as a **contract** —
what it is given, what it may decide, what it must not do, and where its
authority lives — so that an agent reached through some other mechanism, or a
person doing it by hand, is held to the same thing. The cost is a second
description of the same commands, and the risk is that the two drift. The rule
that holds them together: **the command file is the authority on its own
behaviour**, and this page is the authority on nothing. A disagreement between
them is a defect in this page.

---

## What a command is here

A **command** is a named entry point with a written contract. It is not a macro,
not a shortcut, and not a suggestion about how to start: invoking one puts the
invoker under that contract until it ends.

Three properties every command in this repository has, without exception:

| Property | Meaning |
| --- | --- |
| **Bounded authority** | A command names the files it may write. Writing anywhere else is a defect, not initiative. |
| **Prompts, not guesses** | Where the contract says *prompt*, the command stops and asks, and waits. Choosing well is still a failure. |
| **Durable output** | Whatever it produces survives the session. A command whose only product was a conversation did not run. |

Two roles appear throughout, and they are roles rather than identities — the
same agent may hold one and later the other, but never both for the same unit of
work.

- **Orchestrator** — validates, accepts, commits, and talks to the owner. Never
  builds.
- **Worker** — builds exactly one unit of work and stops before the commit.
  Never commits, never answers a prompt, never starts a second unit.

**Owner** is the repository owner: the only party that approves deferred work
and merges.

---

## The surface

| Command | One line | Contract |
| --- | --- | --- |
| [`work`](../../.claude/commands/work.md) | Resolve a route from the arguments, enter that route's loop | [work-loop.md](work-loop.md) |
| [`stop`](../../.claude/commands/stop.md) | End the loop in flight — now, or at the next checkpoint | [phase-loop.md](phase-loop.md#stop-work) |
| [`note`](../../.claude/commands/note.md) | Capture something said, and change nothing else | [workflow.md](workflow.md#something-is-noticed-and-now-is-not-the-time) |
| [`process-queue`](../../.claude/commands/process-queue.md) | Drain the capture file: classify, verify, route | [workflow.md](workflow.md#something-is-noticed-and-now-is-not-the-time) |
| [`preview-decisions`](../../.claude/commands/preview-decisions.md) | Collect the questions the loop has not reached yet | [workflow.md](workflow.md#a-decision-is-coming-and-the-loop-has-not-reached-it-yet) |

### `work`

**Given.** A list of targets, then a kind, then optional flags. The kind is the
last token; everything before it is a target. No arguments at all is a valid
invocation and means *report, enter nothing*. A target may name a **milestone**,
which bounds where the loop stops rather than choosing what it builds.

**Decides.** Which route the arguments name. Nothing else — every decision past
that point belongs to the loop it hands off to.

**Must not.** Build. Route an unknown target or kind to the nearest match.
Enter a loop after `--revalidate` without prompting first. Cross a phase
boundary.

**Authority.** [work-loop.md](work-loop.md), which carries the route table.
Once a loop is entered, that loop's file.

**Typed elsewhere.** The same words reach a **dispatcher** when the working
directory is another repository. It resolves the outermost target to a checkout,
verifies the checkout still declares the kind asked for, and hands everything
else to that repository's own contract — holding no repository logic itself,
because anything it decided would be a second route table drifting against the
one it dispatched to. This repository versions that contract, in
[work-loop.md](work-loop.md#dispatch-from-outside-this-repository), and does not
version the dispatcher: a command visible from every directory cannot live in one
checkout, so it survives no clone.

**Produces.** A loop that runs until one of that loop's stop conditions fires —
one more of them when a milestone target bounded the run — and, where a target
was unknown and the owner named a real one, a row appended to the route table.

### `stop`

**Given.** Nothing, or a checkpoint flag.

**Decides.** Which of the two stops was asked for. An unrecognised argument is
not resolved by judgement: it is reported, and nothing stops.

**Must not.** Commit, push, or revert. Discard uncommitted work. Accept work at
a checkpoint that would otherwise be rejected.

**Authority.** [phase-loop.md](phase-loop.md#stop-work).

**Produces.** A `.current-task.md` reconciled against the tree — not against any
report — so that an unchosen stop costs effort and never knowledge.

### `note`

**Given.** Free text, optionally prefixed with a type, and optionally marked for
discussion.

**Decides.** Nothing. This is the property the command exists for: it does not
classify, does not read the tree, does not search for prior art, and does not
touch the work in flight. Capture that costs anything is capture the owner
learns not to use. Marking a note for discussion does not weaken this: it writes
down a request, and the conversation happens at the drain.

**Must not.** Infer a type that was not given, or a discussion marker that was
not given. A guess written into the file is indistinguishable from the owner's
own answer once the context that made it is gone.

**Authority.** [workflow.md](workflow.md#something-is-noticed-and-now-is-not-the-time).

**Produces.** One row in an untracked capture file, carrying the date and the
unit of work in flight.

### `process-queue`

**Given.** Nothing. It reads the capture file.

**Decides.** A type per row — a change to existing function, an addition of new
function, or a change to process — and a destination for each. Both against the
tree rather than against the row's wording. A row marked for discussion is the
exception: it gets the tree read for it and then a prompt, and the owner's answer
is what types it.

**Must not.** Run inside a build step; routing writes to files a unit in flight
must not find changing under it. Reclassify a row the owner typed a type for on
its own judgement. Type a row the owner marked for discussion, however obvious it
looks. Let a row leave the capture file into anything other than a tracked
destination.

**Authority.** [workflow.md](workflow.md#something-is-noticed-and-now-is-not-the-time),
and [planning.md](planning.md) for anything routed as new function.

**Produces.** Rows in tracked trackers, and a report naming every removal's
destination.

### `preview-decisions`

**Given.** Optionally a count or a list of units to read ahead over. Default is
the next three.

**Decides.** Which choices the plan does not yet cover, and when they are put to
the owner. Not the choices themselves.

**Must not.** Fix anything it finds — a plan defect is a question, not a
correction, because silently correcting it means the loop later validates
against a plan somebody quietly edited. Decide anything, including the obvious
ones. Ask before the file is written, or ask a question in words the entry does
not use. Cross a phase boundary.

**Authority.** [workflow.md](workflow.md#a-decision-is-coming-and-the-loop-has-not-reached-it-yet).

**Produces.** Open questions in a tracked file, each with options, costs, a
recommendation, and what it assumes about a tree that is not built yet — and
then the asking of them, so a read-ahead ends with answers rather than with a
file nobody was told about. An answer moves to decisions.md immediately; an
unanswered question stays where it was written.

---

## What is deliberately not a command

Named because absence reads as oversight.

- **Approving work.** Deferred findings and proposed process changes are
  approved by the owner, per item, in conversation. A command that approved
  would be the actor scheduling its own work.
- **Merging, tagging, releasing.** Each reaches outside the repository and is
  confirmed before it happens; merging is the owner's alone.
- **Fixing a defect found out of scope.** That is a row in a tracker and then
  the owner's decision. There is no command for it because there is no
  invocation that should produce one.

---

## Reading this without the harness

An agent reached some other way holds the same contracts, and needs three things
this repository's harness supplies implicitly:

1. **The invocation must be identifiable.** The contracts above are selected by
   name; a request that does not name one is not a command invocation and the
   general rules in [workflow.md](workflow.md) apply instead.
2. **Arguments are positional and untyped.** No command here takes structured
   input. Every one of them parses free text, and the parse rules are in the
   command's own file.
3. **Prompting must be possible, and blocking.** Every contract above has at
   least one path that stops and waits for the owner. An agent that cannot stop
   and wait cannot hold these contracts, and should run none of them rather than
   run them with the prompts elided.
