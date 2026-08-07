# Workflow

Operating rules for whoever is building this, human or model.

**Style is deliberate.** Terse, trigger-first, no rationale. The rest of this
project explains itself at length; this file is read on every task, so it is
optimized for scanning and low token cost instead. Do not rewrite it into prose.
Rationale belongs in [decisions.md](decisions.md).

**Precedence.** [Plan.md](../../Plan.md) is the scope contract and wins on *what*.
This file wins on *gates* — what must pass. [phase-loop.md](phase-loop.md) wins
on *sequence* — what order, who does it, and when to stop. If any two conflict,
the conflict is a bug — report it, do not pick.

---

## Vocabulary

| Term | Meaning |
| --- | --- |
| **In spec** | Required by the milestone currently being worked. |
| **Out of spec** | Anything else, including defects found by accident. |
| **Work** | A change beyond spelling, phrasing, formatting, or the wording of docs. Anything touching code, SQL, config, tests, generated output, or documented behaviour is work. |
| **Owner** | The repository owner. The only one who approves deferred findings and merges. |

---

## Triggers

### An issue is found — any time, any source

```
in spec      → fix now, inside the current milestone
out of spec  → DO NOT FIX
               append one row to deferred-findings.md
               continue the current milestone
```

Row: what, where (`file:line`), evidence it is real, suspected severity.

Out-of-spec items are worked only after the owner reviews and approves that item.
They collect into the phase's final milestone. Approval is per item, not per
batch — an unreviewed row is not scheduled work.

A defect that makes the *current* milestone's claim false is in spec, whatever it
looks like. Judge by the claim, not by the subsystem.

A defect that makes a **shipped** milestone's claim false **reopens that
milestone** — status row back to `in progress`, the correction written into its
own file — rather than arriving as a successor. A successor leaves a `done` row
asserting something untrue, which is the one outcome worth spending a reopening
to avoid, and it scatters one piece of work across two numbers. The defect still
gets a deferred-findings row first: reopening is scheduling, and scheduling is
the owner's.

### A feature is requested

Not a defect — defects follow the trigger above. Features follow
[planning.md](planning.md): establish absence, decide the phase, place by
number (`X.9` is reserved for scheduled reviews), write the five artifacts,
verify, then **review**. The owner decides scope; planning.md decides
everything else.

A plan that adds a milestone is reviewed by an independent actor before it is
built against — a different model from the writer's where the choice exists,
given the standards and none of the author's conclusions. The contract is
[planning.md §7](planning.md#7-review-it-before-anything-is-built-against-it).

### Something is noticed, and now is not the time

`/note` appends one line to `.queue.md` and returns. Capture decides nothing: it
does not classify, does not read the tree, does not touch the milestone in
flight. A line believed to block the current work is marked `blocking?` — the
*orchestrator* judges that at the next step boundary, never the capturing actor
and never a worker. A line the owner wants to think about *before* it is typed is
marked `discuss` (`/note --discuss`); unlike `blocking?` it is never inferred,
and it makes `/process-queue` prompt where it would otherwise classify.

`/process-queue` drains it, at a milestone boundary and nowhere else: classify
what arrived unclassified, then verify every classification and **prompt on any
dispute**, then route. Types are by what they change, not by how they were
found:

| Type | Is | Routes to |
| --- | --- | --- |
| **issue** | a change to existing function or design | [deferred-findings.md](deferred-findings.md), one row |
| **feature** | an addition of new function or design | [planning.md](planning.md)'s five artifacts |
| **task** | a change to workflow or process, not to the product | its own commit, per the scope gate — or a **Proposed** row in [workflow-changes.md](workflow-changes.md) when it is not being made now, so a process change waiting is as visible as a defect waiting |

`.queue.md` is untracked and transient, for the reason `.current-task.md` is:
appending to it mid-milestone must not dirty the tree that `make demo-update`
and `make release-check` refuse to run on. It therefore survives nothing — no
clone, no fresh checkout. A row that overwinters there is a row in the wrong
file, and draining is what makes it durable.

### A decision is coming, and the loop has not reached it yet

`/preview-decisions` reads ahead: it runs [step 1](phase-loop.md#1-validate)'s
*decisions cover it* check across the milestones not yet built, writes what it
finds to [upcoming-decisions.md](upcoming-decisions.md), **and then asks** — the
file first, always, so an interrupted run has still recorded every question.
Answering one there is worth exactly what answering it in the loop is worth, and
costs the loop no stall.

One direction only. An entry leaves that file when it is answered, and the answer
is appended to [decisions.md](decisions.md) with its `D` number on the date it is
*used*, noting the date it was given. Upcoming-decisions is never where a
decision lives; it is where a question waits.

Each entry records what it assumes about a tree that is not built yet.
Validation re-checks those assumptions when it reaches the milestone, and a false
one re-opens the question rather than inheriting a stale answer.

### Work is being done

`/work [target …] <kind>` resolves a **route** and enters that route's loop.
[work-loop.md](work-loop.md) holds the grammar, the route table, and the rule
that an unknown target or kind is a prompt rather than a near-match. It decides
routing and nothing else; the loop it reaches decides the rest.

```
/work phase      → phase-loop.md, until the phase ends
/work workflow   → work-loop.md's own loop, over approved process changes
/work            → the backlog for each kind, and a recommendation. Enters nothing.
```

The trigger phrase **"Work on Phase"** still means `/work phase`.

[phase-loop.md](phase-loop.md) holds the phase cycle: validate the next
milestone, build it, land it, repeat until the phase ends. It sequences the gates
below and never replaces them, and it stops at the phase's last milestone rather
than starting the next one. It runs as two actors — an orchestrator that
validates, accepts and commits, and a worker per milestone that builds and stops
before the commit. The workflow loop is **not** delegated, and
[work-loop.md](work-loop.md#the-workflow-loop) says why.

### The loop is being stopped

`/stop` now, `/stop --checkpoint` after the unit in flight lands. Both are the
phrases they replace — [phase-loop.md](phase-loop.md#stop-work) is still the
contract and the command adds nothing to it beyond being invokable. An
unrecognised argument stops **nothing** and says so.

### Before completing a commit

All must pass. Failure means the commit does not happen.

```sh
make check              # tidy + lint + unit tests, race enabled
make test-integration   # needs the stack up: make up
```

Then:

| Gate | Condition |
| --- | --- |
| `golangci-lint` | 0 issues |
| Tests | Unit and integration green under `-race` |
| Generated code | If any `.sql` changed: `make sqlc` produces no diff |
| OpenAPI | If any API surface changed: `make openapi` passes |
| Docs | Plan.md reflects new truth; decisions.md has the *why* for anything non-obvious. **If the milestone changed what an operator or reader would observe**, `docs/SECURITY.md` says so too — a claim it makes that the milestone just made false is a failing gate, not cleanup for the phase's documentation pass. **`README.md` is not in this gate (D104).** It describes the *released* product, so a mid-phase commit does not touch it and the phase's features land there at the close, when the tag makes them released. The cost is accepted and stated: this gate no longer catches README drift, because there is no mid-phase README to drift — `CHANGELOG.md`'s `[Unreleased]` section is what carries unreleased work until then, and it is load-bearing for that. |
| Demo | **If the milestone added something somebody can see**, `cmd/lctl/demo.go` seeds it, so the demo instance shows the feature instead of an empty page where it would be. The rule and its exceptions are in [phase-details/README.md](phase-details/README.md#what-every-milestone-inherits); this row is where it is checked |
| Links | Every relative link and anchor in tracked `.md` resolves |
| Scope | **No more than one milestone per commit.** Never bundle two; splitting one across several is fine. Work smaller than a milestone — a process or workflow change — is not a milestone and commits on its own, as soon as it is complete. |

Commit messages are long prose explaining *why*, not what. The diff shows what.

### A milestone is finished — validated, committed

```
make demo-update
```

Rebuilds the demo instance from the commit just made and regenerates its data.
Refuses on a dirty tree; that refusal means the milestone is not finished.

Work happens on the **test** instance. The demo changes here and nowhere else.
Both are in [dev-notes/instances.md](../dev-notes/instances.md).

### A test passes on the first try

Sabotage it. Break the thing it claims to protect, confirm the test fails, then
restore. A test that has never failed has not been shown to test anything.

Restore by **counter-edit**, never `git checkout` — checkout has twice destroyed
uncommitted work in this repo.

### Before a phase PR is created

1. Full validation:

   ```sh
   make release-check      # tree clean, generated code current, docs consistent
   make check
   make test-integration
   ```

2. Verify live against the composed stack, not only tests. Recreate the container
   first (`docker compose up -d --force-recreate --wait app`) or the check runs
   against the previous image and passes for the wrong reason.

3. **If validation triggers work, repeat validation from step 1.** A fix that is
   only spelling, phrasing, formatting, or docs wording does not re-trigger.
   Anything else does. No exceptions for "obviously safe".

4. Then, and only then, the documentation pass below.

5. Then create the PR.

### Documentation pass — after validation, before the PR exists

Update, clean, and minimize every documentation file. Not only the ones this
phase touched.

| File | Check |
| --- | --- |
| `Plan.md` | Build status, milestone rows, scope tables, known limitations all true as of now |
| `README.md` | Status line, feature claims, "Not built yet". **This pass is the only place README changes (D104)**, because it describes the released product and the tag is what releases it. So it is written against what the tag will ship rather than against the branch, and the `[Unreleased]` section of CHANGELOG.md is what it draws from. |
| `CHANGELOG.md` | Entry for what shipped, with its limitations |
| `docs/*.md` | Configuration, usage, operations, deployment, CLI, releasing — every documented behaviour still behaves that way |
| `docs/build-notes/decisions.md` | Append-only. Never edit an entry; a later entry corrects an earlier one |
| `docs/build-notes/upcoming-decisions.md` | Answered entries removed, their answers in decisions.md with `D` numbers; entries for milestones now built are gone |
| `docs/build-notes/doc-cost.md` | Regenerated (`make doc-cost`) **and judged** — defend the growth or trim to pay for it, which [phase-loop.md](phase-loop.md#two-milestones-that-do-not-end-like-the-others) defines and this row does not restate. On record is not answered for: the number was in the diff for a whole phase and obliged nobody |
| `docs/SECURITY.md` | New defences, new gaps, new operator responsibilities |
| `docs/build-notes/workflow.md` | This file. Rules learned this phase |
| `docs/build-notes/phase-loop.md` | The loop that ran this phase, and where it needed a human anyway |
| `docs/adr/` | Investigations that outgrew a decision-log entry |

Minimize means: delete what is no longer true, merge what is duplicated, and cut
what restates something the reader already read. It does not mean shortening
explanations that carry a reason.

Every documented claim must be verifiable by a reader who does not trust you. No
numbers without a measurement, no "should be fast", no feature described in the
present tense that is not built.

---

## Standing rules

**Verify, do not assume.** Never report a measurement taken on a dirty tree, or
before the code under test was actually built and deployed. If a check passed for
a reason you cannot name, it did not pass. **A cached result is not a
measurement**: Go caches a test result against the package's inputs, and the
instance's database state is not one of them, so `make test-integration` is
forced with `GOFLAGS=-count=1` once anything the tests run against has been
touched.

**Report failures plainly.** Tests that fail, steps skipped, things not verified —
say so, with the output. A green summary over a red run is the worst possible
output.

**No byte-mangling tools on source.** PowerShell `Get-Content`/`Set-Content` has
corrupted UTF-8 in this repo (em-dashes into mojibake). Use editors that preserve
bytes.

**Plan.md states what is true. decisions.md states why.** Rationale in the plan
and status in the decision log are both wrong.

**Nothing leaves a tracker silently.** A row removed from any tracked list —
Plan.md's scope and *Not in Phase N* tables, [phase-details/](phase-details/)'s
status table, [deferred-findings.md](deferred-findings.md),
[upcoming-decisions.md](upcoming-decisions.md),
[workflow-changes.md](workflow-changes.md) — leaves only one of two ways:

1. **Re-homed.** It appears in another tracker, and the row it left says which.
   Moving is the normal case: a finding becomes a milestone, a question becomes a
   decision, a queue row becomes a Plan.md row.
2. **Logged.** Its removal is an entry in decisions.md naming what was dropped
   and why.

Deciding an item no longer matters *is a decision*, and it is the one kind this
project keeps losing: nobody writes down what they stopped caring about, so it
returns later as a fresh idea with its reasoning gone. A tracker that can be
quietly emptied tracks nothing.

`.queue.md` is the deliberate exception, and only because draining it is exactly
this rule applied — every row leaves it by being routed somewhere durable, and
[`/process-queue`](../../.claude/commands/process-queue.md) will not let a row
go anywhere else.

**A decision made in conversation is written down before it is acted on.**
Answers given in prose evaporate: the reasoning is gone by the next session and
the conclusion gets re-derived, differently. If a milestone forces it, the
answer goes to decisions.md with its `D` number. If nothing forces it yet, the
*question* goes to [upcoming-decisions.md](upcoming-decisions.md) — including
when it has already been acted on, in which case the entry names the behaviour
the tree currently has. This applies to whoever is deciding, and most of all to
an actor deciding on the owner's behalf because the loop would otherwise stall.

**A recorded abuse path is in a fix milestone's scope by default.** A row whose
evidence describes something a principal can *do* — not merely observe, not merely
a claim that is wrong — belongs in the fix milestone that is running, **including
when it is found during that milestone**. It does not wait for the next phase
because it arrived late. Carrying one is available and is the owner's to choose;
it is not a thing to recommend, and a recommendation to carry needs a reason
stated in the prompt rather than a default of cheapness. Owner-set, 2026-08-04.

**A documentation change is approved in advance, and still has to be verified.**
Correcting a document, a comment or a decision's text is **not** a decision to
put to the owner: make it. This is a standing approval and it removes a prompt,
never a check. Every claim written or corrected is verified against the tree the
same way — the sentence is read against the code it describes, `make check-links`
passes, and an enumeration is **counted rather than trusted**, because four
wording rows in one milestone turned out to have more sites than they listed.

The bound is what *counts* as documentation. Prose, comments and a decision's
wording are in. **Anything that changes what the product does is not**, however
small the diff and however much it reads like a correction — a status code, a
default, a predicate, a permission. If a document is wrong because the behaviour
is wrong, the document is not the decision: say which one is being corrected, and
if it is the behaviour, that is still a prompt. Owner-set, 2026-08-05.

**`.github/workflows/` cannot be committed to — propose instead.** The build
token carries no `Workflows` permission, so a push touching those paths is
rejected before any review happens. What a CI step *does* lives in a `make ci-*`
target and in `scripts/`, and is edited normally. What CI *is* — triggers, the
`permissions:` block, service images, action pins, `GO_VERSION` — goes to
[`ci/proposed/`](../../ci/proposed/README.md) with a *Proposed* row in
[workflow-changes.md](workflow-changes.md), and the owner applies it.
`make workflow-proposals` lists what is waiting. Owner-set, 2026-08-06.

**Stop and ask** for: destructive operations, scope changes, anything the owner
would reasonably want to decide. Proceed without asking for reversible work that
follows from the current milestone.

**Every decision prompt carries options, costs, and a recommendation.** Each
option says what it buys *and* what it costs. The recommended one leads, marked,
and states its own con — a recommendation from the actor that will also do the
work drifts toward whatever is cheapest to build, and naming that cost is what
holds it honest. Name the default too: what happens if the answer is "you
decide", so nobody has to re-derive the choice in order to skip it. If nothing
can be recommended, say why. Omitting the recommendation is a thing to justify,
not a thing to leave out quietly.

---

## Quick reference

```sh
make up                 # start Postgres, Redis, app — the test instance
make check              # tidy + lint + unit tests (race)
make test-integration   # integration tests (needs the stack)
make generate           # sqlc + openapi
make release-check      # full pre-release validation
make rebuild            # test instance from nothing, migrated
make down               # stop and remove volumes
make instances          # both instances, and whether they are up
make demo-update        # refresh the demo — after a milestone, not during one
make doc-cost           # what the always-read docs cost, predicted vs realized
```

Targets act on the **test** instance unless given `INSTANCE=demo`. Destructive
ones refuse `demo` without `CONFIRM=demo`.

The test instance is stopped after thirty idle minutes and does not restart
itself. Anything needing a database starts one, so this is rarely visible; when
it matters — a long unattended job — `touch /tmp/linkctrl-test-keep`.
