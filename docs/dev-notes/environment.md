# The development environment

The dev loop runs on a dedicated Linux virtual machine. The repo lives at
`/home/whippy/repos/DevOfPie/LinkCtrl`; VSCodium attaches over a remote
extension, so the editor and the Claude Code sidebar are unchanged.

**This file was rewritten on 2026-08-07 and it replaces a WSL2 runbook.** That
matters for how to read it, so it is said at the top rather than buried: the
previous version described an Ubuntu distro under WSL2 on a Windows host, was
stamped *Built 2026-07-31*, and was never updated when the environment moved.
This machine's `/etc/machine-id` was created **2026-08-02**, so the notes
described the previous machine for five days and three references to a
`/home/natha` path that has never existed here.

**What is verified and what is not.** Every version and path in the tables below
was read off this machine on 2026-08-07. What this file deliberately does *not*
contain is a provisioning script: the old one was a runbook whose steps were run
by the person who wrote it, and reconstructing an equivalent from the current
state would be a guess presented as a procedure. The gaps are enumerated at the
end rather than papered over.

For the host-agnostic project setup — what the app needs, what the Makefile
targets mean — see [development.md](../build-notes/development.md).

## What this machine is

| | |
| --- | --- |
| OS | Ubuntu 26.04 LTS (Resolute Raccoon), server install media `20260420.1` |
| Kernel | 7.0.0-28-generic |
| Virtualization | KVM guest (`systemd-detect-virt` reports `kvm`) |
| Hostname | `whippyrjumi` |
| User | `whippy`, in `sudo` and `docker`, with passwordless sudo |
| Repo | `/home/whippy/repos/DevOfPie/LinkCtrl` |
| Docker | Engine 29.7.1, **native**, socket `unix:///var/run/docker.sock` |
| Git identity | `Whippy Rjumi <312040073+WhippyRjumi@users.noreply.github.com>` |

There is no Windows host in the loop: no `/mnt/c`, no `wslpath`, no
`/etc/wsl.conf`, and `/proc/version` names no Microsoft kernel. Anything in an
older note about `.wslconfig`, Docker Desktop's WSL integration, `wsl.exe`
quoting or CRLF from Windows tooling no longer applies to this machine.

**Why the environment is on Linux at all** is still worth keeping, because it is
the reason not to drift back. Three problems disappeared rather than being
worked around when the dev loop left native Windows: Windows Defender prompted
on every wildcard bind and its rules key on an image path, which `go run` and
`go test` change on every invocation; Go's monotonic clock on Windows cannot
resolve the intervals this project measures, so latency numbers were meaningless
(see
[development.md](../build-notes/development.md#latency-measured-on-a-windows-host-is-not-a-latency-measurement));
and neither `make` nor `shellcheck` existed there, so Makefile targets had to be
hand-expanded and six scripts went unchecked. The product ships as Linux
containers and is developed on Linux.

## Toolchain

Read off this machine on 2026-08-07.

| Component | Version | Where it comes from, and why |
| --- | --- | --- |
| Go | 1.26.5 | official tarball at `/usr/local/go`, symlinked to `/usr/local/bin/go`. [go.mod](../../go.mod) requires 1.26.5 and apt is years behind |
| gcc | present | `build-essential`; cgo, so `go test -race` is genuinely armed |
| make | 4.4.1 | apt |
| shellcheck | 0.11.0 | apt, `/usr/bin/shellcheck` |
| sqlc | v1.31.1 | `go install`, symlinked into `/usr/local/bin`. Pinned to match [ci.yml](../../.github/workflows/ci.yml) |
| golangci-lint | 2.12.2 | `go install` of the **`/v2/`** module path, symlinked into `/usr/local/bin`. Pinned to match `ci.yml` |
| task | 3.52.0 | `go install`, on `PATH` via `$HOME/go/bin` rather than a symlink |
| gh | 2.97.0 | `~/.local/bin/gh-shim/gh`, prepended to `PATH` by `~/.bashrc` |
| Docker | 29.7.1 | native engine on this machine |

**The two pinned Go tools matter.** A linter that updates itself turns an
unrelated release into a red build, and sqlc's version is stamped into the
generated code in `internal/store/dbgen`. If either drifts from CI, local and CI
disagree about a clean tree. `ci.yml` is the source of truth; the table is a
snapshot.

`task` is unpinned on purpose: it runs no gate, and the Taskfile exists to be
kept in sync with the Makefile rather than to be authoritative.
golangci-lint v2 lives under the `/v2/` module path — the v1 path silently
installs the wrong major and then rejects this repo's v2 config schema.

**The `/usr/local/bin` symlinks are load-bearing.** `~/.bashrc` extends `PATH`
for interactive shells only, so a plain `bash -c "go build"` gets neither it nor
a login profile. If tooling reports `go: command not found`, a symlink is
missing rather than the install being broken.

## Docker, and the two instances

The daemon is this machine's own. Development runs two compose projects on it,
`linkctrl-demo` and `linkctrl-test`; [instances.md](instances.md) covers what
each is for, their ports, and which targets act on which.

**Carrying a volume over from another tree means two values must come with it,
not one.** `POSTGRES_PASSWORD` must match what that volume was initialised with,
or authentication fails against the retained data with no other symptom.
`LINKCTRL_API_KEY_PEPPER` must match too: it keys the HMAC over every stored API
key, so a freshly generated one leaves the rows in place and every key that was
ever issued failing to authenticate. `internal/config/config.go` says so at the
point of validation — *"Changing this invalidates every existing API key"* — and
nothing at startup compares the pepper against the data, so the only symptom is
a key that used to work and does not.

Carry both, or carry neither and recreate the volume. Half of each is the case
that looks like it worked.

## Host units

Two systemd units live in `/etc/systemd/system/` rather than in this repository,
because they carry absolute paths or a credential. Their contents are here so a
rebuild reproduces them.

### The idle-stop timer

The test instance is stopped when nothing has used it for thirty minutes; see
[instances.md](instances.md#when-the-test-instance-stops) for what counts as
use. As root:

```sh
cat > /etc/systemd/system/linkctrl-idle-stop.service <<'UNIT'
[Unit]
Description=Stop the LinkCtrl test instance when nothing is using it
Documentation=file:///home/whippy/repos/DevOfPie/LinkCtrl/docs/dev-notes/instances.md

[Service]
Type=oneshot
User=whippy
WorkingDirectory=/home/whippy/repos/DevOfPie/LinkCtrl
ExecStart=/home/whippy/repos/DevOfPie/LinkCtrl/scripts/idle-stop.sh test 30
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

`User=whippy` matters twice: the script reads `.env.test` from the repository,
and it reaches Docker through the `docker` group rather than as root. It needs
no `DOCKER_HOST` — the socket is at `/var/run/docker.sock`, reachable from a
unit with no shell profile.

**Installed on 2026-08-07, and it had been missing.** The environment move on
2026-08-02 did not carry these two units across, while
[instances.md](instances.md) and
[workflow.md](../build-notes/workflow.md#quick-reference) both went on stating
that the test instance stops after thirty idle minutes. It did not, for five
days. Verified on installation: the first run exited 0 and correctly declined to
stop an instance a host process was using.

### The demo tunnel

The demo instance is published at <https://linkctrl-demo.devofpie.com> so it can
be opened from a browser that is not on this machine — which is the normal case.
[instances.md](instances.md#the-demo-is-also-reachable-from-off-the-host) says
what it serves and how that is established.

```sh
install -m 0700 -d /etc/cloudflared
# The connector token from the Cloudflare Zero Trust dashboard, as
# TUNNEL_TOKEN=<token>. Root-only so it never reaches `ps` or shell history.
install -m 0600 /dev/null /etc/cloudflared/env
$EDITOR /etc/cloudflared/env

cat > /etc/systemd/system/linkctrl-tunnel.service <<'UNIT'
[Unit]
Description=Cloudflare Tunnel for the LinkCtrl demo
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=notify
EnvironmentFile=/etc/cloudflared/env
ExecStart=/usr/bin/cloudflared --no-autoupdate --loglevel info tunnel run
TimeoutStartSec=0
Restart=on-failure
RestartSec=5s
DynamicUser=yes
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now linkctrl-tunnel.service
```

**`DynamicUser=yes` is why the token lives in a file.** The unit runs as an
allocated user with `ProtectHome=yes`, so it has no home to read a
`~/.cloudflared/` credential from, and `EnvironmentFile` is the only path in.

**The ingress is not on this machine.** A token-managed tunnel takes its
configuration — which hostname maps to which local service — from the Cloudflare
Zero Trust dashboard, so there is no `config.yml` here to read or restore. A
rebuild reproduces the *connector*; the hostname mapping already exists in
Cloudflare and reattaches when the connector comes back. What the running
connector was told is recoverable only from its log:

```sh
journalctl -u linkctrl-tunnel | grep 'Updated to new configuration'
```

Worth knowing before debugging a 404 that turns out to be the dashboard's
catch-all rather than anything here.

## Verify

```sh
cd /home/whippy/repos/DevOfPie/LinkCtrl
go version && make --version | head -1 && shellcheck --version | grep version:
go env CGO_ENABLED          # must be 1, or -race is not actually running
make test                   # full unit suite under the race detector
make up                     # the test instance
make instances              # both, and whether they are up
systemctl list-timers linkctrl-idle-stop.timer
systemctl is-active linkctrl-tunnel.service
```

`make test` passing under `-race` is the real proof — it exercises the
toolchain, cgo, and `make` in one step.

## Things that will bite you

**Non-interactive shells miss the Go PATH.** `~/.bashrc` loads for interactive
shells only, so a plain `bash -c "go build"` gets nothing from it. The
`/usr/local/bin` symlinks exist for exactly this.

**`~/.bashrc` is re-sourced by nested shells**, and unguarded appends duplicate —
which is why `$PATH` has carried `$HOME/go/bin` several times over. Guard any
addition with a `case ":$PATH:"` test.

**Do not keep the working copy in a synced folder.** See
[development.md](../build-notes/development.md#do-not-keep-the-working-copy-in-onedrive).

**A unit is not carried by a machine move.** Both units above live outside the
repository, so nothing in a clone recreates them, and their absence is silent —
the idle-stop timer was missing for five days while two documents said it ran.
After any rebuild, run the two `systemctl` checks in *Verify* rather than
assuming.

## Deliberately manual

Two steps resist automation and must be done by hand on a rebuild:

1. **`gh auth login`** — a browser device flow. When it asks *"Authenticate Git
   with your GitHub credentials?"*, answer **Yes**; that is `gh auth setup-git`,
   which is what makes `git push` use the token `gh` already holds.
2. **Claude Code sign-in** — credentials live in `~/.claude/` per machine, so a
   rebuilt environment starts unauthenticated. Global preferences in
   `~/.claude/settings.json` need recreating too.

## Permission allowlist

`.claude/settings.json` in the repo root allowlists the read-only and build
commands this project actually uses — `go`, `make`, `git` queries, `docker
compose` reads, `golangci-lint`, `sqlc`, `shellcheck`, and `gh` reads.
Destructive operations are deliberately excluded and still prompt: `git
commit`/`push`/`reset`, `make db-reset`, `make migrate-down`, and `docker
compose down`.

Personal overrides go in `.claude/settings.local.json`, which is gitignored; the
shared `settings.json` is tracked.

## What this file does not cover

Named rather than left to be discovered missing:

- **A provisioning script.** The old file carried one; it was written by whoever
  ran it. This machine's exact build sequence was not observed, and inventing an
  equivalent would be a guess formatted as a procedure. The tables above are
  enough to check a rebuild against.
- **How `gh` is installed and authenticated here.** It resolves to
  `~/.local/bin/gh-shim/gh`, prepended to `PATH` by `~/.bashrc`, and reports
  authentication via `GH_TOKEN` rather than a stored credential file — and where
  that variable is set was not established. It is not apt-installed.
- **How `claude` is installed.** `~/.local/bin/claude` is a symlink into
  `~/.local/share/claude/versions/`, which is the standalone installer's layout
  rather than the editor-extension wrapper an older note described.
