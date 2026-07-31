# The WSL2 development environment

The dev loop runs inside a WSL2 Ubuntu distro, not on the Windows host. The repo
lives at `~/dev/LinkCtrl` in the Linux filesystem; VSCodium attaches to the distro
over a remote extension, so the editor and the Claude Code sidebar are unchanged.

This document is a rebuild runbook. Every version below was verified on the live
machine, not copied from upstream docs. Built 2026-07-31.

For the host-agnostic project setup — what the app needs, what the Makefile
targets mean — see [development.md](../build-notes/development.md). This file
covers only the WSL2 layer beneath it.

## Why the environment moved off native Windows

Three problems, all of which disappear rather than get worked around:

**Recurring firewall alerts.** [config.go](../../internal/config/config.go) defaults
`HTTP_ADDR` to `:8080`, a wildcard bind across every interface, and
[main.go](../../cmd/linkctrl/main.go) opens a second listener for metrics. Windows
Defender Firewall prompts on a wildcard bind, and its rules are keyed to the
**image path** — but `go run` and `go test` compile to a freshly named `.exe` in a
temp directory on every invocation, so an approved rule never matches the next
run. The alerts were unbounded and no amount of approving them helped. WSL2
listeners sit behind its own NAT and never reach the Windows firewall at all.

**Latency was unmeasurable.** Go's monotonic clock on Windows cannot resolve the
intervals this project cares about; see
[development.md](../build-notes/development.md#latency-measured-on-a-windows-host-is-not-a-latency-measurement).
On Linux those numbers are real.

**Missing tooling.** Neither `make` nor `task` existed on the host, so every
Makefile target had to be hand-expanded into raw commands — slower, and it drifted
from what CI actually verifies. `shellcheck` was absent too, with six scripts in
[scripts/](../../scripts/) unchecked.

The environment now matches the deploy target: the product ships as Linux
containers, and it is developed on Linux.

## What is installed

| Component | Version | Source and why |
| --- | --- | --- |
| Ubuntu | 26.04 LTS | `wsl --install -d Ubuntu` |
| Kernel | 6.18.33.2-microsoft-standard-WSL2 | ships with WSL |
| Go | 1.26.5 | **official tarball, not apt** — [go.mod](../../go.mod) requires 1.26.5 and apt is years behind |
| gcc | 15.2.0 | `build-essential`; cgo, so `go test -race` is genuinely armed |
| make | 4.4.1 | apt |
| shellcheck | 0.11.0 | apt |
| sqlc | v1.31.1 | pinned to match [ci.yml](../../.github/workflows/ci.yml) |
| golangci-lint | 2.12.2 | pinned to match [ci.yml](../../.github/workflows/ci.yml) |
| task | 3.52.0 | `go install github.com/go-task/task/v3/cmd/task@latest`; the Taskfile mirrors the Makefile and cannot be kept in sync unverified |
| gh | 2.97.0 | **official GitHub apt repo, not Ubuntu's** — apt ships 2.46.0 (early 2024) |
| Docker | server 29.6.2 | Docker Desktop on the host, shared into the distro |

The two pinned Go tools matter: a linter that updates itself turns an unrelated
release into a red build, and sqlc's version is stamped into the generated code in
`internal/store/dbgen`. If either drifts from CI, local and CI disagree about a
clean tree. Re-check both pins against `ci.yml` when rebuilding — the table above
is a snapshot, `ci.yml` is the source of truth.

## Host prerequisites

Windows side, before any of the below:

- **WSL** 2.7.11 or newer (`wsl --version`). WSL2 must already be enabled; if
  Docker Desktop runs, it is.
- **Docker Desktop**, running.
- **VSCodium**, whose extension gallery points at Open VSX
  (`resources/app/product.json` → `extensionsGallery.serviceUrl`).
- **`%USERPROFILE%\.wslconfig`** with the two settings below. It governs every
  WSL2 VM including Docker Desktop's, and applies on the next `wsl --shutdown`,
  not live:

  ```ini
  [wsl2]
  autoMemoryReclaim=gradual
  sparseVhd=true
  ```

  The first returns idle page cache to Windows — Go builds and Postgres push the
  distro's cache to several GB, and without it WSL keeps that memory even when
  Windows wants it. The second makes *newly created* VHDs allocate sparsely;
  existing ones are converted, stopped, with
  `wsl --manage <distro> --set-sparse true`. Relevant here because `make
  seed-slo` writes five million rows into the docker-desktop VHD, which never
  shrinks on its own.

## Rebuild from scratch

Run every script through `tr -d '\r'` first if it was authored on Windows — see
[Things that will bite you](#things-that-will-bite-you).

### 1. Create the distro

```powershell
wsl --install -d Ubuntu --no-launch
```

`--no-launch` is required for unattended setup. Without it the distro opens an
interactive username/password prompt that hangs any non-interactive session.

### 2. Provision as root

```sh
#!/usr/bin/env bash
set -euo pipefail
USERNAME=natha
GO_VER=1.26.5

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq build-essential make shellcheck git curl ca-certificates

id -u "$USERNAME" >/dev/null 2>&1 || adduser --disabled-password --gecos "" "$USERNAME"
usermod -aG sudo "$USERNAME"
printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$USERNAME" > "/etc/sudoers.d/90-$USERNAME"
chmod 0440 "/etc/sudoers.d/90-$USERNAME"

cat > /etc/wsl.conf <<EOF
[user]
default=$USERNAME

[boot]
systemd=true
EOF

curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VER}.linux-amd64.tar.gz"
rm -rf /usr/local/go
tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz

# /etc/profile.d loads for login shells only; symlinks are on the default PATH
# unconditionally, which is what non-interactive `bash -c` needs.
ln -sf /usr/local/go/bin/go    /usr/local/bin/go
ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
```

Pipe it in with `Get-Content provision.sh -Raw | wsl -d Ubuntu -u root -- bash -s`,
or run it by path under `/mnt/c/...`.

**On passwordless sudo:** it is deliberate. An automated session runs with stdin
closed, so any `sudo` password prompt does not fail — it hangs. This is a
single-purpose local dev distro; tighten it if that tradeoff stops being worth it.

### 3. Restart the distro

```powershell
wsl --terminate Ubuntu
```

`/etc/wsl.conf` is read at boot, so the default user and systemd do not apply until
the distro restarts. After this, `wsl -d Ubuntu` lands as `natha` with no `-u` flag.

### 4. User environment and clone

Run as `natha`:

```sh
set -euo pipefail
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
grep -q '/usr/local/go/bin' "$HOME/.bashrc" || \
  printf '\nexport PATH=$PATH:/usr/local/go/bin:$HOME/go/bin\n' >> "$HOME/.bashrc"

git config --global user.name  "KillerOfPie"
git config --global user.email "nathan@thors.org"
git config --global init.defaultBranch main

mkdir -p "$HOME/dev"
git clone https://github.com/DevOfPie/LinkCtrl.git "$HOME/dev/LinkCtrl"
```

Clone into the **Linux** filesystem. A working copy under `/mnt/c` is slow across
the 9p boundary and breaks file watching.

The clone needs no credentials — the repository is public. Push authentication
arrives with the GitHub CLI in step 6; nothing here should configure a
credential helper. An earlier version of this environment pointed
`credential.helper` at the Windows Git Credential Manager under `/mnt/c/...`,
which worked but made every push exec a Windows binary across the interop
boundary and made git auth depend on the Git-for-Windows install staying where
it was. `gh` holds a token anyway, so git now uses that instead.

### 5. Go tools, pinned to CI

```sh
go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
go install github.com/go-task/task/v3/cmd/task@latest
sudo ln -sf "$HOME/go/bin/sqlc"          /usr/local/bin/sqlc
sudo ln -sf "$HOME/go/bin/golangci-lint" /usr/local/bin/golangci-lint
sudo ln -sf "$HOME/go/bin/task"          /usr/local/bin/task
```

`task` is unpinned on purpose: it runs no gate, and the Taskfile exists to be
kept in sync with the Makefile rather than to be authoritative. The other two are
pinned because CI runs them.

golangci-lint v2 lives under the `/v2/` module path; the v1 path silently installs
the wrong major and then rejects this repo's v2 config schema.

### 6. GitHub CLI

Ubuntu's `gh` is far behind, so use the official repo. As root:

```sh
install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
  -o /etc/apt/keyrings/githubcli-archive-keyring.gpg
chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg
printf 'deb [arch=%s signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main\n' \
  "$(dpkg --print-architecture)" > /etc/apt/sources.list.d/github-cli.list
apt-get update -qq && apt-get install -y -qq gh
```

Then authenticate interactively — see [Deliberately manual](#deliberately-manual) —
and make git use the same token:

```sh
gh auth setup-git
```

That writes per-host `credential.https://github.com` helpers pointing at
`gh auth git-credential`, so `git push` authenticates with the token `gh`
already holds: one credential, no Windows dependency, nothing new to mint.
Verify with `git ls-remote origin HEAD` and `git push --dry-run origin <branch>`
from the clone.

### 7. VSCodium remote

```powershell
& "$env:ProgramFiles\VSCodium\bin\codium.cmd" --install-extension jeanp413.open-remote-wsl
```

Microsoft's Remote-WSL is proprietary and is **not** on Open VSX, so VSCodium
cannot use it. `jeanp413.open-remote-wsl` is the working substitute. The Claude
Code extension publishes a `linux-x64` target on Open VSX, so VSCodium installs it
into the remote automatically on first connect.

### 8. Docker Desktop integration — manual

Docker Desktop → Settings → Resources → WSL Integration → enable `Ubuntu`. There is
no reliable unattended equivalent; editing `settings-store.json` under a live
daemon is fragile.

The daemon is shared with the Windows host: same containers, same volumes. A tree
on either side resolves to the same compose project for the same project name, so
volumes carry across — which is exactly why an env file must keep the password its
volume was initialised with.

Development runs two instances on this daemon, `linkctrl-demo` and
`linkctrl-test`, rather than the single `linkctrl` project of the Windows era.
[instances.md](instances.md) covers them. The old `linkctrl_pgdata` volume from
that era is not used by either and can be removed once nothing in it is wanted:
`docker volume rm linkctrl_pgdata`.

### 9. Instance configuration

Env files are gitignored and never arrive with a clone. Both instances generate
their own, with fresh secrets and their own ports:

```sh
cd ~/dev/LinkCtrl
make env INSTANCE=test
make env INSTANCE=demo
```

`make up` does it too when the file is missing, so this step is only worth
running deliberately. See [instances.md](instances.md).

Carrying a volume over from another tree instead of generating one means
`POSTGRES_PASSWORD` must match what that volume was initialised with, or
authentication fails against the retained data with no other symptom.

### 10. The idle-stop timer

The test instance is stopped when nothing has used it for thirty minutes; see
[instances.md](instances.md#when-the-test-instance-stops) for what counts as use.
The unit files are here rather than in the repository because they carry an
absolute path and a username. As root:

```sh
cat > /etc/systemd/system/linkctrl-idle-stop.service <<'UNIT'
[Unit]
Description=Stop the LinkCtrl test instance when nothing is using it
Documentation=file:///home/natha/dev/LinkCtrl/docs/dev-notes/instances.md

[Service]
Type=oneshot
User=natha
WorkingDirectory=/home/natha/dev/LinkCtrl
ExecStart=/home/natha/dev/LinkCtrl/scripts/idle-stop.sh test 30
UNIT

cat > /etc/systemd/system/linkctrl-idle-stop.timer <<'UNIT'
[Unit]
Description=Check every five minutes whether the LinkCtrl test instance is idle

[Timer]
OnBootSec=5min
OnUnitActiveSec=5min
AccuracySec=30s

[Install]
WantedBy=timers.target
UNIT

systemctl daemon-reload
systemctl enable --now linkctrl-idle-stop.timer
```

This is why `systemd=true` is in `/etc/wsl.conf`. `User=natha` matters twice: the
script reads `.env.test` from the repository, and it reaches Docker through the
`docker` group rather than as root. It needs no `DOCKER_HOST` — Docker Desktop's
integration puts the CLI on the default PATH at `/usr/bin/docker` and the socket
at `/var/run/docker.sock`, both reachable from a unit with no shell profile. When
Docker Desktop is not running, the script says so and exits 0 rather than
failing every five minutes.

### 11. Verify

```sh
cd ~/dev/LinkCtrl
go version && make --version | head -1 && shellcheck --version | grep version:
go env CGO_ENABLED          # must be 1, or -race is not actually running
make test                   # full unit suite under the race detector
make up                     # the test instance
make instances              # both, and whether they are up
```

`make test` passing under `-race` is the real proof — it exercises the toolchain,
cgo, and `make` in one step.

## Things that will bite you

**CRLF in shell scripts.** A script authored with Windows tooling and piped into
`bash` fails with `$'\r': command not found`, often only on the last line, so it
looks like it succeeded. Always `tr -d '\r' < in.sh > out.sh` before running. Verify
config files it wrote with `grep -cU $'\r' <file>`.

**CRLF in `.env`.** A trailing `\r` makes `POSTGRES_PASSWORD=secret\r`, Postgres
initialises with an invisible carriage return, and every later connection fails
authentication for no visible reason. Writing natively in Linux retires this.

**Non-interactive shells miss the Go PATH.** `/etc/profile.d/go.sh` loads for login
shells and `~/.bashrc` for interactive ones — a plain `bash -c "go build"` gets
neither. The `/usr/local/bin` symlinks in steps 2 and 5 exist for exactly this; if
tooling reports `go: command not found`, they are missing.

**Quoting through `wsl.exe`.** Nested quotes in
`wsl -d Ubuntu -- bash -c "..."` mangle badly from PowerShell. Write the script to
a file and invoke it by `/mnt/c/...` path instead. From Git Bash, absolute Unix
paths get rewritten — prefix with `MSYS_NO_PATHCONV=1`.

**Do not put the working copy on `/mnt/c`.** Nor in OneDrive; see
[development.md](../build-notes/development.md#do-not-keep-the-working-copy-in-onedrive).

**`gh` stores its token differently here.** On Windows it sits in the OS keyring.
WSL has no keyring by default, so `gh` writes it in plaintext to
`~/.config/gh/hosts.yml` at mode 600. Normal for Linux `gh`, but a real change in
where a credential lives. The alternative is exporting `GH_TOKEN`, which `gh` reads
without persisting anything.

## Deliberately manual

Three steps resist automation and must be done by hand on a rebuild:

1. **Docker Desktop WSL integration** — GUI toggle, step 8.
2. **`gh auth login`** — a browser device flow. When it asks *"Authenticate Git with
   your GitHub credentials?"*, answer **Yes** — that is `gh auth setup-git`, which is
   wanted here (step 6). Answering No and running it afterwards is the same thing.
3. **Claude Code sign-in in the remote** — credentials live in `~/.claude/` per
   filesystem, so the WSL side starts unauthenticated. Any global preferences such
   as `effortLevel` in `~/.claude/settings.json` also need recreating.

## Permission allowlist

`.claude/settings.json` in the repo root allowlists the read-only and build commands
this project actually uses — `go`, `make`, `git` queries, `docker compose` reads,
`golangci-lint`, `sqlc`, `shellcheck`, and `gh` reads. Destructive operations are
deliberately excluded and still prompt: `git commit`/`push`/`reset`, `make db-reset`,
`make migrate-down`, and `docker compose down`.

If personal overrides are ever added in `.claude/settings.local.json`, add that file
to `.gitignore` — the shared `settings.json` is meant to be tracked, the local one
is not.
