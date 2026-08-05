# LinkCtrl

Read [docs/build-notes/workflow.md](docs/build-notes/workflow.md) before doing
any work. It is short, and it is the operating contract.

| Say | Means | Read |
| --- | --- | --- |
| `/work phase` (or **"Work on Phase"**) | Build the phase milestone by milestone until it ends | [phase-loop.md](docs/build-notes/phase-loop.md) |
| `/work workflow` | Make the approved process changes, one per commit, until none is left | [work-loop.md](docs/build-notes/work-loop.md) |
| `/work` with anything else | Resolve the route first; an unknown target or kind is a prompt | [work-loop.md](docs/build-notes/work-loop.md) |
| `/stop` | End the loop now, wherever it is | [phase-loop.md](docs/build-notes/phase-loop.md) |
| A feature request | Place it in the plan before building it | [planning.md](docs/build-notes/planning.md) |
| `/note` | Capture it to `.queue.md` and change nothing else | [workflow.md](docs/build-notes/workflow.md) |
| `/process-queue` | Drain the queue: classify, verify, route | [workflow.md](docs/build-notes/workflow.md) |
| `/preview-decisions` | Ask the questions the loop has not reached yet | [upcoming-decisions.md](docs/build-notes/upcoming-decisions.md) |
| Anything else | — | [workflow.md](docs/build-notes/workflow.md) |

Scope is [Plan.md](Plan.md); definitions of done are
[docs/build-notes/phase-details/](docs/build-notes/phase-details/), one file per
milestone — read only the one being built. Rationale is append-only in
[decisions.md](docs/build-notes/decisions.md); status lives in the phase-details
README and nowhere else.
