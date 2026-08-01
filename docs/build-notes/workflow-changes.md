# Workflow changes

Changes to **how this project is built** — the operating contract, the loop, the
commands — as opposed to changes to the product. The process backlog, and the
record of what the process used to be.

It exists because process changes were the one kind of work with no tracker.
A product defect gets a row in [deferred-findings.md](deferred-findings.md); a
product feature gets a Plan.md row and a milestone file; a process change got a
commit and nothing else. Once `.queue.md` was drained the only surviving record
that a change had been *asked for* was a commit message, and `.queue.md` is
untracked, so it survives no clone.

| Not here | There |
| --- | --- |
| Why a change was made | [decisions.md](decisions.md) — append-only, and every row below points at its entry |
| A question with no answer yet | [upcoming-decisions.md](upcoming-decisions.md) |
| A defect in the product | [deferred-findings.md](deferred-findings.md) |
| Product scope | [Plan.md](../../Plan.md) |
| The rules themselves | [workflow.md](workflow.md), [phase-loop.md](phase-loop.md), [planning.md](planning.md) |

A row here is a change to make, or one already made. It is not the change: the
rule always lives in the file it governs, and a row that tries to state the rule
becomes a second copy that drifts.

**Nothing leaves this file silently.** A row is moved to *Made* with its commit,
or removed with the removal logged in decisions.md, per workflow.md's standing
rule. Abandoning a proposed change is a decision.

---

## Proposed

Asked for, or identified, and not yet made. Approval is the owner's, the same as
for a deferred finding — an unapproved row is a suggestion, not scheduled work.

| # | Change | Why | Raised | Approved |
| --- | --- | --- | --- | --- |
| W9 | Add a `/stop` command, so stopping the loop is a command and not only a phrase | [phase-loop.md](phase-loop.md) documents *stop work* and *stop at the checkpoint* as things the owner says, and `.claude/commands/` has no `/stop`, so the two stops are discoverable only by reading the loop file. Verified against the tree at 2026-08-01 | Owner, `/note`, 2026-07-31 (M26.5) | No |
| W10 | Fold stop-at-the-checkpoint into that command as a flag — `/stop --checkpoint` | Owner's note, verbatim: *move stop-at-next-checkpoint command to a flag on the existing stop command so `/stop --checkpoint` would perform the function*. Depends on W9, which is what creates the command the flag would hang on. The note also asks whether a checkpoint needs redefining; it does not — [phase-loop.md](phase-loop.md#stopping-at-the-checkpoint) already defines it as the end of step 3.9 and nothing else, which is exactly what a flag would invoke | Owner, `/note`, 2026-07-31 (M26.5) | No |
| W11 | Add a `/work` command taking the kind of work as a parameter — `/work --phase` for today's `/work-on-phase`, `/work --workflow` to run a loop over pending workflow changes; with no parameter, show the backlog for each and recommend one | Owner's note. The second half is the interesting part: this file's *Proposed* rows have no loop that consumes them, so a process change waits until somebody remembers it, while product work has `/work-on-phase` driving it continuously. A recommendation would need a way to judge how necessary a pending change is, which nothing here records today | Owner, `/note`, 2026-07-31 (M27) | No |
| W12 | Document the command surface for agents — what each command does, its rules, and its contract — in a form that is as model-agnostic as possible | Owner's note. Today a command's behaviour lives in its own `.claude/commands/*.md` and in the build-notes files it points at, so an agent learns the surface by reading all of them. The model-agnostic constraint is the substance: the description has to work for a reader that has not been trained on this repo's idiom | Owner, `/note`, 2026-07-31 (M27) | No |
| W13 | An `X.9` adversarial review also audits doc-cost and token optimization | Owner's note, raised during M32.5. `make doc-cost` exists and W1 already proposes judging the always-read contract's growth at the documentation pass; this would move that judgement to the review milestones, where the cost of the contract is being paid most visibly. **Note the tension to resolve first:** the note says *trigger a worker*, and [phase-loop.md](phase-loop.md#two-milestones-that-do-not-end-like-the-others) says `X.9` reviews are never delegated, because their product is a conversation with the owner. Either the audit is the orchestrator's too, or the no-delegation rule gains a stated exception | Owner, `/note`, 2026-08-01 (M32.5) | No |
| W14 | Commit the browser harness that verifies rendered-page claims, or state explicitly that those claims are verified by hand at named engines | [M26.5](phase-details/m26.5.md)'s popover positioning was verified in Blink, Gecko and WebKit, and the harness that did it lives only in a session scratchpad — so the check cannot be repeated by anyone else, and the next milestone making a rendered-geometry claim starts from nothing. D25 already settled that verification tooling may use Node; what it did not settle is whether the tooling is kept | Orchestrator, at M26.5, 2026-07-31 | No |
| W15 | Record how to authenticate to the test instance — the credential, or a supported way to reset it — in [dev-notes/instances.md](../dev-notes/instances.md) | The account password is written down nowhere, and the value carried in the loop's own note was stale; discovered when a live check needed a login. **This currently blocks work:** four queue rows reporting product defects in the dashboard could not be verified against the running app on 2026-08-01 for exactly this reason, and writing an argon2id hash into the users table is refused by the sandbox. A reseed path or a documented credential is the difference between a reported defect being confirmable and not | Orchestrator, at M26.5, 2026-07-31 | No |
| W8 | `/preview-decisions` puts each question to the owner as a prompt, instead of only writing it to [upcoming-decisions.md](upcoming-decisions.md) | The command's stated product is the file, and answering is left to "conversation, or the owner editing the file directly" — so a read-ahead session ends with every question still open, which is the state it was run to avoid. On 2026-07-31 it wrote six entries and asked nothing; all six were answered minutes later, in the same session, only because the owner asked why no prompts appeared. The file stays the durable artifact either way — this changes when the asking happens, not where the answer lives | 2026-07-31, by the owner, immediately after that run | Not yet — the owner asked for this row, not for the change |
| W1 | Judge the always-read contract's growth at the phase's documentation pass, and either defend it or trim to pay for it | `make doc-cost` records the number and obliges nobody to act on it. `phase-loop.md` grew from 16725 to 21941 bytes on 2026-07-31 alone, with nothing removed, and its realized read cost fell to 0.39 of the file | 2026-07-31, while regenerating doc-cost | Yes — scheduled into [M45](phase-details/m45.md) as a bullet rather than left here |

## Made

Newest first. The commit is the change; this row is how somebody finds it
without knowing what to grep for.

| # | Change | Commit | Entry |
| --- | --- | --- | --- |
| W7 | Trackers gained a no-silent-removal rule, upcoming-decisions gained a section for questions no milestone forces, and this file was created | *this commit* | [Nothing leaves a tracker silently](decisions.md) |
| W6 | The per-commit Docs gate now names `README.md` and `docs/SECURITY.md`, not only Plan.md and decisions.md | `b068b73` | [The gate that never asked about README](decisions.md) |
| W5 | A workflow summary for an outside reader, at [README.md](README.md) in this directory | `035a9a0` | — (the file explains itself) |
| W4 | `stop at the checkpoint` — a second stop that lands the milestone in flight instead of abandoning it | `70ef4ac` | — (the rule carries its own reasoning) |
| W3 | Amending a bullet: the orchestrator corrects a **fact** and logs it; changing what a bullet **asserts** still prompts | `a21bdc3` | [Plan drift is allowed; silent plan drift is not](decisions.md) |
| W2 | The M24.5 amendment backfilled into the decision log, in the format W3 established | `87339bd` | [M24.5, amendment: the eight pages were nine](decisions.md) |

Phase 1's process history is not backfilled here. It is in decisions.md, in the
order it happened, and rewriting it into this table would be a second copy of a
record that is already append-only.
