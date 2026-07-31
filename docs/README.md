# Documentation map

The rule for this directory: **a file sits at `docs/` root only if someone
running or using LinkCtrl reads it.** Anyone changing the product reads the
subfolders. When adding a document, that question — hosting it, or building
it? — decides the location.

## For operators and users — this folder

| File | What it answers |
| --- | --- |
| [usage.md](usage.md) | Using the dashboard and the API |
| [configuration.md](configuration.md) | Every environment variable, and what takes effect |
| [deployment.md](deployment.md) | TLS, reverse proxy, backups, upgrades — the real-deployment answers |
| [operations.md](operations.md) | Metrics, monitoring, what to watch |
| [cli.md](cli.md) | `lctl`, the operator CLI |
| [releasing.md](releasing.md) | What a version number means, upgrading, rolling back, building artifacts yourself |
| [slo.md](slo.md) | The redirect latency promise, and the measurement behind it |
| [SECURITY.md](SECURITY.md) | The security model, its deliberate gaps, and how to report a vulnerability |

Two of these are judgment calls, recorded so they are not re-litigated:
`releasing.md` stays although cutting a release is the maintainer's job, because
what versions mean, how to upgrade and how to roll back are the operator's
contract. `slo.md` stays although it contains methodology, because it is the
performance promise README makes — evidence a host can check is part of the
promise.

## For people building it — subfolders

| Folder | What lives there |
| --- | --- |
| [build-notes/](build-notes/) | Process and record: [workflow.md](build-notes/workflow.md) (operating rules), [phase-loop.md](build-notes/phase-loop.md) (the validate–build–land cycle a phase repeats), [planning.md](build-notes/planning.md) (how a requested feature enters the plan), [decisions.md](build-notes/decisions.md) (append-only rationale), [development.md](build-notes/development.md) (contributor setup), [deferred-findings.md](build-notes/deferred-findings.md), and [phase-details/](build-notes/phase-details/) (per-milestone definitions of done) |
| [adr/](adr/) | Investigations that outgrew a decision-log entry |
| [dev-notes/](dev-notes/) | This machine's environment: the [two instances](dev-notes/instances.md), the [WSL2 setup](dev-notes/wsl2-environment.md) |

The scope contract itself is [../Plan.md](../Plan.md), at the repository root
beside the README that cites it.
