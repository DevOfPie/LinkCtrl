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
cp .env.example .env      # fill in the three required secrets
docker compose up -d --wait
make assets               # build app.css + verify vendored htmx (before first go build)
make test                 # unit tests, race detector on
make lint
make migrate-up
make migrate-status
make db-reset             # drop, recreate, re-migrate
make test-integration     # needs the stack up
```

`make assets` matters before the first build: the stylesheet is generated from
the templates and embedded into the binary, so a build without it runs fine but
serves unstyled pages (the server warns at boot). The vendored htmx is
committed; the target just verifies it against its pinned checksum.

`Taskfile.yml` mirrors the Makefile for contributors without `make`.

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
