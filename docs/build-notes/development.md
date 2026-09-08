# Development setup

The deploy target is Linux containers; development happens on Windows, macOS or
Linux. Postgres and Redis always run in Docker — only the Go toolchain is
installed on the host.

## Prerequisites

| Tool | Why |
| --- | --- |
| Go 1.26+ | the application |
| Docker Desktop / Docker Engine | Postgres, Redis, and the production image |
| A C compiler | **only** for `go test -race`; nothing in the product uses cgo |

Node is **not** required. Tailwind is built by the pinned standalone CLI, and
the production image contains no Node runtime.

### Windows

```powershell
winget install GoLang.Go
winget install Docker.DockerDesktop          # needs elevation and a reboot
winget install --id BrechtSanders.WinLibs.POSIX.UCRT --scope user
```

WinLibs installs without elevation but **does not add itself to PATH**. Add it:

```
%LOCALAPPDATA%\Microsoft\WinGet\Packages\BrechtSanders.WinLibs.POSIX.UCRT_Microsoft.Winget.Source_8wekyb3d8bbwe\mingw64\bin
```

Then confirm cgo is usable, since the race detector silently degrades without
it:

```
gcc --version
go env CGO_ENABLED     # must be 1
```

Pick the **POSIX/UCRT** WinLibs variant. The MSVCRT builds work for plain C but
UCRT is what current Go targets on Windows.

### Do not keep the working copy in OneDrive

Sync interferes with the Go build cache and with `.git`, and cloud-placeholder
files bind-mount into containers as zero bytes. Symptoms are intermittent
`Access is denied` from `go build`, phantom empty files, and fsnotify storms
breaking live reload. Use a plain local path such as `C:\dev\LinkCtrl`.

Also worth adding to Windows Defender exclusions, which commonly cuts build
times by more than half: the repository, `%LOCALAPPDATA%\go-build`, and
`%USERPROFILE%\go\pkg\mod`.

## Everyday commands

```sh
make assets               # build app.css + verify vendored htmx (before first go build)
make up                   # start the stack; writes .env.test with fresh secrets if needed
make test                 # unit tests, race detector on
make lint
make migrate-up
make migrate-status
make db-reset             # drop, recreate, re-migrate
make test-integration     # needs the stack up
make rebuild              # the stack from nothing: volumes gone, image rebuilt, migrated
```

These act on the **test** instance. Development runs two stacks — a long-lived
`demo` and a disposable `test` — and everything above takes `INSTANCE=demo` to
act on the other one, with the destructive targets refusing it unless
`CONFIRM=demo` is also passed. See
[dev-notes/instances.md](../dev-notes/instances.md); it is the reference for
ports, guards and the milestone refresh.

`cp .env.example .env` is still how the *single* stack in
[README.md](../../README.md)'s quickstart is configured. The instances write
their own `.env.demo` and `.env.test` and do not read it.

`make assets` matters before the first build: the stylesheet is generated from
the templates and embedded into the binary, so a build without it runs fine but
serves unstyled pages (the server warns at boot). The vendored htmx is
committed; the target just verifies it against its pinned checksum.

**Three targets need Node**, and none of them is reachable from anything else —
not `check`, not any `ci-` target, not `release-check`. D25 is why Node is
allowed to appear at all; each one is opt-in because what it needs is a download
no other target wants.

| | Re-checks | Needs |
| --- | --- | --- |
| `make verify-render` | [M26.5](phase-details/m26.5.md)'s popover geometry, in Blink, Gecko and WebKit — [tools/render-verify/](../../tools/render-verify/README.md) | Three browser engines, several hundred megabytes |
| `make verify-ui` | The kept browser spec, against a running test instance — [tools/agent-browser/](../../tools/agent-browser/README.md) | One engine, and `make up` |
| `make verify-scan` | [M50.6](phase-details/m50.6.md)'s logo cap: every code the product can draw, decoded at simulated distance — [tools/qr-scan/](../../tools/qr-scan/README.md) | Two decoders, and a few minutes |

`Taskfile.yml` carries the Makefile's tasks for contributors without `make`, and
it is **not** a complete mirror — it said it was until 0.4.0 while two of the
three browser and scan gates had never been added to it (F220). What it does not
carry, counted against the tree rather than recalled:

| Missing | Why it is not a gap worth closing by copying |
| --- | --- |
| `verify-ui`, `verify-scan` | Both drive a browser or a decoder against a running instance. `verify-render` is in the Taskfile and its own precondition explains what it needs; adding two more would put three heavyweight, environment-dependent tasks in a file whose purpose is the ordinary loop |
| `oidc-fixture`, `idp-up`, `idp-down` | The integration suite's fixtures. `task test-integration` is not offered either, for the same reason |
| `browse`, `load-breaking-point` | Tools, not gates |
| `help`, `require-db-password` | Makefile mechanics with no Taskfile equivalent |

Everything a commit is gated on **is** carried, and `task check` runs the same
seven steps `make check` does. That is the sentence to keep true: nothing in this
file mirrors the Makefile's *whole* surface, and nothing needs to.

Everything that connects to the database reads `POSTGRES_PASSWORD` out of
`.env`, because that is where compose reads it and therefore what the database
was initialised with. An exported `POSTGRES_PASSWORD` takes precedence, for CI.
An empty one fails with a message saying so rather than with an authentication
error several steps from the cause.

Those targets also set `LINKCTRL_APP_ENV=development` explicitly. Configuration
loading checks that variable *before* it reads `.env`, so without it in the
environment the file is ignored and the command fails on missing secrets instead
of doing anything about the database.

## The race detector

`go test -race` needs cgo, and therefore a C compiler. Without one the flag
fails outright rather than quietly skipping — but a suite that has never been
run under it can still look healthy, so treat "no C compiler" as "the
concurrency tests are not being checked".

To confirm the detector is actually armed rather than merely silent, a
deliberate race must fail under `-race` and pass without it. The concurrency
this project cares about is the alias generator under parallel creators, the
readiness cache, and the click-event batcher.

CI runs `go test -race` on Linux regardless, so a host without a compiler
blocks local verification, not the merge gate.

## Latency measured on a Windows host is not a latency measurement

Go's monotonic clock on Windows cannot resolve the intervals this project cares
about. A loop of 100,000 back-to-back `time.Since` calls returns exactly zero
every single time, so anything faster than a clock tick measures as 0.

Two visible consequences, both harmless once known:

- `linkctrl_redirect_duration_seconds_sum` stays at 0 for cache-served
  redirects, so averages read as zero. Bucket counts are still right, which is
  what the SLO is stated against.
- `click_events.latency_us` is 0 for the same requests.

Both resolve to real figures on Linux, which is where the SLO is measured and
where the service runs. Do not chase either as a bug, and do not quote a local
number as a latency result.

## Git Bash notes

Git Bash rewrites arguments that look like absolute Unix paths, so a container
path such as `/lctl` becomes `C:/Program Files/Git/lctl`. Disable it per
command:

```sh
MSYS_NO_PATHCONV=1 docker run --rm --entrypoint /lctl ghcr.io/devofpie/linkctrl:latest version
```

Line endings are normalised to LF by `.gitattributes`. This is not cosmetic: a
CRLF in `.env` gives `POSTGRES_PASSWORD=secret\r`, so Postgres initialises with
a password containing an invisible carriage return and every later connection
fails authentication for no visible reason.
