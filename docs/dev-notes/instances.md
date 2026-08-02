# The two development instances

Development runs two LinkCtrl stacks side by side. They are separate compose
projects, so they have separate containers, separate volumes and separate ports,
and nothing done to one can reach the other.

| | **demo** | **test** |
| --- | --- | --- |
| For | Using the product between milestones | Building and breaking things |
| Dashboard | <http://localhost:8080> | <http://localhost:8081> |
| Postgres / Redis | `55432` / `56379` | `55433` / `56380` |
| Metrics (loopback) | `9090` | `9091` |
| Restart policy | `unless-stopped` | `no` — see [when it stops](#when-the-test-instance-stops) |
| Compose project | `linkctrl-demo` | `linkctrl-test` |
| Env file | `.env.demo` | `.env.test` |
| Image | `linkctrl:demo`, rebuilt by `make demo-update` | `linkctrl:test`, rebuilt whenever |
| Lifetime | Survives everything except `make demo-update` | Disposable; `make rebuild` is routine |
| Data | The `lctl demo` installation — two workspaces, three accounts, a review queue — re-anchored to today at each milestone | Whatever the current test needs |

`test` is the default. Every target acts on it unless `INSTANCE=demo` is passed,
because the alternative — a default that follows whichever stack is running — is
how a `make db-reset` meant for a test drops the instance you were using.

## Why two

One stack cannot be both. The demo is only worth opening if it holds a month of
plausible history and the last thing you did to it is still there; testing means
resetting the database, seeding five million click events, rolling back a
migration and pointing a load generator at it. Sharing one stack between those
two jobs means the demo is destroyed roughly weekly, and reaching for a *clean*
database means first deciding whether anything in the current one mattered.

Separating them also buys a check nothing else in this repository performs: the
demo's volume is never recreated, so every `make demo-update` applies that
milestone's migrations to a database that has been through all the previous ones.
CI and the test instance both start from empty, where a migration that cannot
survive existing data passes.

## Everyday commands

```sh
make instances                  # both stacks, and whether they are up
make up                         # start test
make logs                       # follow test's application log
make rebuild                    # test, from nothing: volumes gone, image rebuilt, migrated
make test-integration           # against test
make up INSTANCE=demo           # start demo, without touching its data
make demo-update                # the milestone refresh; see below
```

Anything that writes to a database takes `INSTANCE`:

```sh
make db-reset INSTANCE=test
make seed INSTANCE=test
make migrate-status INSTANCE=demo
```

### Running one integration test

`make test-integration` takes no `-run`. Rather than reconstructing the DSNs by
hand, print the command the target would run and edit that:

```sh
make -n test-integration          # prints it with the real DSNs filled in
```

Then add `-run 'TestName'` and, if the tree changed since the last run,
`-count=1`. The `-count=1` is not optional in that situation: Go caches a test
result against the package's inputs, and the instance's *database state* is not
one of them, so a suite that passed before a schema or seed change will report
`(cached)` and pass again without executing. See the standing rule in
[workflow.md](../build-notes/workflow.md#standing-rules).

## Ports

The demo keeps the numbers the single stack used, because it is the one opened in
a browser. Test sits one above on each.

| | HTTP | Postgres | Redis | Metrics |
| --- | --- | --- | --- | --- |
| demo | 8080 | 55432 | 56379 | 9090 |
| test | 8081 | 55433 | 56380 | 9091 |

Everything except HTTP is published to `127.0.0.1` only, by the compose override.
That includes the metrics listener, which the base file deliberately does not
publish at all: it is unauthenticated and its series describe traffic shape and
saturation. The development override publishes it on loopback because
`idle-stop.sh` needs to ask whether anyone is using the instance, and because a
metrics endpoint you can curl is useful while building one. The production
procedure in [deployment.md](../deployment.md) runs `-f docker-compose.yml`,
which does not apply the override.

## Creating an instance

`make env INSTANCE=<name>` writes `.env.<name>` with fresh secrets, that
instance's ports, and nothing else — every other setting takes the binary's
default, and [.env.example](../../.env.example) documents the full set. `make up`
does it for you if the file is missing.

### Signing in to a fresh instance

A migrated instance has no account. The first-run setup form claims it, and it
is the only path that works regardless of `LINKCTRL_SIGNUP_MODE`:

```
http://localhost:8081/setup      # test; 8080 is the demo
```

It is served only while `users` is empty, and answers `303 → /login` once an
account exists — so a redirect there means the instance is already claimed, not
that the route is missing.

**The test instance's account, as rebuilt 2026-08-01:**

| | |
| --- | --- |
| Address | `dev@killerofpie.com` |
| Password | `linkctrl-test-owner-2026` |

Written down because it is the test instance and it is disposable — this is a
local development credential for a stack that publishes nothing but HTTP on
localhost, and `make rebuild` throws it away and needs a new one. Do not reuse
the pattern anywhere that matters.

If it stops working, do not go hunting for it: `make rebuild` and claim the
instance again through `/setup`. That is faster than recovering a password and
it is what the instance is for.

**Check the port before concluding a credential is wrong.** The two instances
answer on 8080 and 8081, and a sign-in attempt against the wrong one fails with
*the email or password is incorrect* — which reads exactly like a bad password.
This has already cost one debugging session.

These files hold secrets and are **not** committed: `.gitignore` excludes
`.env.*` and always has. They are written mode 600.

`POSTGRES_PASSWORD` is baked into the Postgres volume the first time it starts.
Regenerating an env file therefore means recreating the volume too, or every
later connection fails authentication with no other symptom.
`LINKCTRL_API_KEY_PEPPER` is bound to retained data the same way — it keys the
HMAC over stored API keys, so a new one silently invalidates every key already
issued. Recreating the volume discards those rows anyway, which is why the two
stay consistent as long as you do both together:

```sh
make down INSTANCE=test          # removes the volume
rm .env.test
make up INSTANCE=test            # new secrets, new volume
```

### `.env` is still the operator's file

The repository-root `.env` is what a plain `docker compose up` uses, which is the
quickstart in [README.md](../../README.md) and the deployment in
[deployment.md](../deployment.md). The instances do not read it — except in one
place worth knowing. `lctl` runs on the *host* for `make seed`, `make demo` and
the migrate targets, and `config.Load` reads `.env` from the working directory
whatever instance you meant. The Makefile exports the instance's file over it
first, so the instance wins for everything the instance defines; a variable set
only in `.env` still reaches those commands. Keep `.env` boring, or delete it if
you never run the single-stack quickstart.

## The demo, and when it changes

The demo instance changes at exactly one moment: a milestone has been validated
on test and committed. Then:

```sh
make demo-update
```

which rebuilds `linkctrl:demo` from the current commit, recreates the container
(migrations run at boot), and regenerates the demo workspace with `lctl demo
--reset`.

**A refresh replaces the data.** `lctl demo --reset` deletes everything it
seeded — the links, the second workspace, the seeded accounts, the invitations,
the notifications, the audit records and the disputes — and truncates
`click_events`, so anything you created while using the instance is gone at the
next milestone. That completeness is the point: two runs produce the same demo,
which is what makes running this at every milestone safe. That is the deliberate trade: the demo's value is a
month of *recent* plausible history — the dataset is generated relative to the
day it runs, so charts always end today rather than trailing off weeks ago — and
keeping accumulated hand-made data would mean the history slowly aging into
something nobody would demo. Between milestones, everything you do persists.

Two things stop an accidental refresh from being the wrong build:

- **The tree must be clean.** `make demo-update` refuses when `git status` is not
  empty, because a demo built from uncommitted work is not the milestone anybody
  validated. `FORCE=1` overrides it, for deliberately showing work in progress.
- **The image is only rebuilt here.** The demo runs `linkctrl:demo`, and nothing
  else retags it. `make up INSTANCE=demo` after a crash or a reboot starts the
  same build it was running before.

A fresh demo has no user, and `lctl demo` needs one to own the data. Claim the
instance at <http://localhost:8080> first — the setup form appears once and then
disappears permanently — and run `make demo-update` again.

## When the test instance stops

It is disposable and spends most of its life doing nothing, which on a laptop is
three containers' worth of memory and a Postgres holding its shared buffers. Two
mechanisms keep it from being up when it has no reason to be.

**It does not come back by itself.** `LINKCTRL_RESTART=no` in `.env.test`, so a
reboot, a WSL shutdown or a Docker Desktop restart leaves it down. The demo keeps
`unless-stopped` and comes back on its own, because it exists to be there when
the browser is pointed at it.

**A timer stops it after thirty idle minutes.** `linkctrl-idle-stop.timer` runs
[scripts/idle-stop.sh](../../scripts/idle-stop.sh) every five minutes. Idle means
all three of:

- No HTTP request on any surface except `ops` since the last check — that surface
  is the container's own health probe, which runs every ten seconds forever, so
  counting it would mean never idle. The counter is read from the metrics
  listener, which the development override publishes on loopback for this.
- No process on this host using the instance — `go test`, `lctl`, a server from
  `make run`. These talk to Postgres directly and never appear in the app's
  metrics at all.
- No keep-file. `touch /tmp/linkctrl-test-keep` holds it up for something long
  and unattended; delete it to release.

It never stops the demo — the script refuses that instance outright.

Stopping is `docker compose stop`, so containers keep their state and their
volumes and nothing has to be rebuilt or reseeded. Starting again takes about two
seconds, and you rarely do it by hand: every target that needs a database brings
up Postgres and Redis first, and `make up` brings up all three.

```sh
make idle-stop                      # the same decision, now, with its reasoning
make idle-stop IDLE_MINUTES=0       # stop it if nothing is using it right now
systemctl status linkctrl-idle-stop.timer
journalctl -u linkctrl-idle-stop.service --since -1d
sudo systemctl disable --now linkctrl-idle-stop.timer   # stop managing it
```

The unit and timer live in `/etc/systemd/system/` rather than in this repository,
because they carry absolute paths and a username; their contents are in
[wsl2-environment.md](wsl2-environment.md#10-the-idle-stop-timer) so a rebuild
reproduces them.

## What is guarded

Destructive targets refuse `INSTANCE=demo` unless `CONFIRM=demo` is also passed:

```
down · rebuild · db-reset · migrate-down · seed · seed-slo · demo
test-integration · load · load-uncached
```

```sh
$ make db-reset INSTANCE=demo
Refusing: db-reset would destroy data on the demo instance.
...
  make db-reset INSTANCE=demo CONFIRM=demo
```

`load` and `load-uncached` are in the list even though a load test only reads
links: it writes several hundred thousand click events on the way, and the demo's
analytics are meant to look like a workspace rather than a benchmark.

An unknown instance name is refused rather than defaulted, so a typo cannot
quietly build a third stack that looks like the demo having lost its data.

The guard is in `make`, and it is a seatbelt rather than a lock — a direct
`docker compose -p linkctrl-demo down -v` still does exactly what it says.

## Recovering the demo

There is no backup. The demo is generated data plus whatever you did to it since
the last milestone, and rebuilding it is one command:

```sh
make up INSTANCE=demo
make demo-update FORCE=1        # if the tree is dirty and you want it back now
```

If the volume itself is gone, the instance comes back empty and unclaimed: claim
it at <http://localhost:8080>, then `make demo-update`.

## Related

- [wsl2-environment.md](wsl2-environment.md) — the WSL2 layer these run on
- [development.md](../build-notes/development.md) — project setup, host-agnostic
- [workflow.md](../build-notes/workflow.md) — where the refresh sits in a milestone
- [cli.md](../cli.md) — `lctl demo` and `lctl seed` in full
