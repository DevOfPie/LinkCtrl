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
