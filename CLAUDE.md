# LinkCtrl

Read [docs/build-notes/workflow.md](docs/build-notes/workflow.md) before doing
any work. It is short, and it is the operating contract.

| Say | Means | Read |
| --- | --- | --- |
| `/work phase` (or **"Work on Phase"**) | Build the phase milestone by milestone until it ends | [phase-loop.md](docs/build-notes/phase-loop.md) |
| `/work workflow` | Make the approved process changes, one per commit, until none is left | [work-loop.md](docs/build-notes/work-loop.md) |
| `/work` with anything else | Resolve the route first; an unknown target or kind is a prompt | [work-loop.md](docs/build-notes/work-loop.md) |
| `/stop` (or `/stop --checkpoint`) | End the loop now, or after the unit in flight lands | [phase-loop.md](docs/build-notes/phase-loop.md) |
| A feature request | Place it in the plan before building it | [planning.md](docs/build-notes/planning.md) |
| `/note` | Capture it to `.queue.md` and change nothing else | [workflow.md](docs/build-notes/workflow.md) |
| `/process-queue` | Drain the queue: classify, verify, route | [workflow.md](docs/build-notes/workflow.md) |
| `/preview-decisions` | Ask the questions the loop has not reached yet | [workflow.md](docs/build-notes/workflow.md#a-decision-is-coming-and-the-loop-has-not-reached-it-yet) |
| Anything else | — | [workflow.md](docs/build-notes/workflow.md) |

Every command's contract, stated for a reader outside this harness, is
[commands.md](docs/build-notes/commands.md).

Scope is [Plan.md](Plan.md); the rules every milestone inherits, and the
template a definition of done starts from, are
[milestone-rules.md](docs/build-notes/milestone-rules.md).

**Records live in Mustur, not in this tree.** Milestones and their status, each
milestone's definition of done, decisions, findings and questions are LNK
records. Before any work, call `mustur_route` (server `mustur`) with repository
`DevOfPie/LinkCtrl`; call it with `id` for one record in full, and read only the
milestone being built. Write with `~/.local/bin/mustur`: `add decision|finding
--project LNK`, `ask --project LNK`, `amend` for a milestone's `Status`. No
`.mcp.json` is committed — the server and its token are per machine.
