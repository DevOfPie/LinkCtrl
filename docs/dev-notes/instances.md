# The two development instances

Development runs two LinkCtrl stacks side by side. They are separate compose
projects, so they have separate containers, separate volumes and separate ports,
and nothing done to one can reach the other.

| | **demo** | **test** |
| --- | --- | --- |
| For | Using the product between milestones | Building and breaking things |
| Dashboard | <http://localhost:8080> | <http://localhost:8081> |
| From anywhere else | <https://linkctrl-demo.devofpie.com> — see [the tunnel](#the-demo-is-also-reachable-from-off-the-host) | Not published |
| Postgres / Redis | `55432` / `56379` | `55433` / `56380` |
| Metrics (loopback) | `9090` | `9091` |
| Restart policy | `unless-stopped` | `no` — see [when it stops](#when-the-test-instance-stops) |
| Compose project | `linkctrl-demo` | `linkctrl-test` |
| Env file | `.env.demo` | `.env.test` |
| Image | `linkctrl:demo`, rebuilt by `make demo-update` | `linkctrl:test`, rebuilt whenever |
| Lifetime | Survives everything except `make demo-update` | Disposable; `make rebuild` is routine |
| Data | The `lctl demo` installation — two workspaces, three accounts, a review queue — re-anchored to today at each milestone | Whatever the current test needs |
| Custom-domain re-verification | **Off in a generated `.env.demo`** — `LINKCTRL_DOMAIN_VERIFY_INTERVAL=0`, the one setting here a real deployment must not copy. A demo whose env file predates that change is still **on**, and the paragraph directly below this table says why and what to do about it | On, at the shipped default |

**Why the demo does not re-verify its hostname.** `lctl demo` verifies
`go.linkctrl.example` through the real `VerifyDomain` against a stub resolver
that lives inside the seeder process and dies with it. The name is an RFC 2606
`.example` one that resolves for nobody, so the long-running server failed that
check every hour and unverified the hostname 24 hours after every reseed —
silently, because the coverage row asserting one verified domain seeds a
throwaway database and asserts in the same instant, so it measures the seed and
never the instance. Zero disables the pass, which is what the setting is
documented to mean. `scripts/instance.sh` writes it into a freshly generated
`.env.demo` with the full reasoning; **an existing `.env.demo` does not get it,
because `instance.sh init` refuses to touch a file that is already there.**

**Zero switches off a second job with it**, and the demo is arranged around that
rather than unaffected by it. `jobRunner.runHostReload` returns immediately on a
non-positive interval, so each replica's periodic re-read of the
verified-hostname set stops too — the backstop F73 bought for a replica that
missed a pub/sub invalidation. One replica is why the demo can spend it. The
part that is not free is the reseed: `lctl demo --reset` deletes and rewrites the
`domains` rows, `lctl` runs on the host with no Redis so it publishes no
invalidation, and the set is held whole in memory precisely so an unknown `Host`
costs no query — so nothing reloads it lazily either. **`make demo-update`
therefore recreates the app container after the reseed, not only before it**, and
that ordering is what keeps this setting safe here. Reversed, a fresh demo serves
nothing on `go.linkctrl.example` and a repeat serves a cached entry naming a
domain id the reseed deleted.

**So the running demo needs the line added by hand, once**, before the next
`make demo-update`: `.env.demo` is gitignored and generated, `Makefile`'s
`instance.sh init` runs only when the file is absent, and no commit can reach it.
Until it is there the pass runs, whatever this page says the setting is — which
is why the row above states what the generator writes rather than what the
instance is doing. Check with `docker exec linkctrl-demo-app-1 env | grep
DOMAIN_VERIFY`.

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
make test-integration           # against test, plus the suite's own identity provider
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

## A third container, and it belongs to neither instance

`make test-integration` starts one more thing: **dex**, on `127.0.0.1:5554`,
from `docker-compose.integration.yml`. It is the identity provider M69's
acceptance test signs a person in through, and it is not an instance — it is the
suite's, under its own compose project (`linkctrl-idp`), so `make down` does not
take it and `INSTANCE=demo` does not make a second one.

```sh
make idp-up                     # start it and wait for a discovery document
make idp-down                   # stop it and remove its volumes
make oidc-fixture               # rebuild the OIDC add-on the test installs
```

Both are prerequisites of `make test-integration` and `make ci-integration`, so
the ordinary path needs neither by hand. Reach for them when a run failed on the
provider rather than on the product, or after changing the pin in
`scripts/oidc-fixture.sh`.

Two generated things sit under `test/integration/testdata/` and are gitignored:
the certificate dex serves, which `scripts/idp.sh` makes with `openssl` on first
use, and the add-on itself, which `scripts/oidc-fixture.sh` fetches from the
module proxy at a pinned version and rebuilds. The second prints the digest it
produced and refuses to hand over anything that does not match what the published
release names.

## Ports

The demo keeps the numbers the single stack used, because it is the one opened in
a browser. Test sits one above on each.

| | HTTP | Postgres | Redis | Metrics |
| --- | --- | --- | --- | --- |
| demo | 8080 | 55432 | 56379 | 9090 |
| test | 8081 | 55433 | 56380 | 9091 |

Everything except HTTP is published to `127.0.0.1` only, by the compose override.
That includes the metrics listener, which the base file deliberately does not
publish at all: it is unauthenticated and its series describe traffic shape,
saturation, and — with `LINKCTRL_ADDONS_DIR` set — which add-ons are installed
and at which versions. The development override publishes it on loopback because
`idle-stop.sh` needs to ask whether anyone is using the instance, and because a
metrics endpoint you can curl is useful while building one. The production
procedure in [deployment.md](../deployment.md) runs `-f docker-compose.yml`,
which does not apply the override.

## Both instances run the sample add-on

`scripts/instance.sh` sets `LINKCTRL_ADDONS_DIR=/addons` for the demo and for the
test instance, and every image carries a first-party sample at
`/addons/pageviews` — see [examples/addons](../../examples/addons/README.md) for
what it is and why the image ships it. So both have an add-on host, an installed
module, and an Add-on manager page with something on it.

**The test instance gained it at M68 and the reason is the browser gate.** The
milestone names the browser harness as what asserts that the manager's table does
not shift when Remove turns each row's chevron into a checkbox, and with no host
the manager's routes are not mounted at all — so both add-on specs skipped on a
404, and a skip is the same string as a pass. They run now. The 404 branch stays
in the spec for the instance that genuinely has no host, which is the state of
every instance whose operator installed none.

Three consequences while developing.

**The add-ons directory comes from a read-only container filesystem**, on both
instances, so installing and removing are refused — the documented behaviour of a
`:ro` mount, and the right posture for an instance anybody can sign into. The
*page* says so and stays a page: it redirects back to itself with the reason, the
way every refusal on it does. `503` is the **API's** answer to the same case. The
specs need neither, because what they drive is the list, select-mode and the two
confirmations.

**The core SLO column needs the variable gone.** [slo.md](../slo.md) measures
*core, no add-on* on an ordinary instance, and an instance running an
observe-class module is not one. Take the line out of `.env.test`, recreate the
app container, run, and put it back — the same shape the add-on columns already
use, in the other direction.

**`lctl` runs on the host, where `/addons` does not exist**, and `config.Load`
refuses a directory it cannot stat. The Makefile's `DEV_ENV` empties the variable
for that reason, so `make seed`, `make migrate-up` and the rest are unaffected; a
hand-rolled `go run ./cmd/lctl` that sources the instance file is not, and wants
`LINKCTRL_ADDONS_DIR=` in front of it.

`.env.demo` and `.env.test` are generated, so an instance created before this
existed does not have the variable. Add the line by hand or delete the file and
run `make env INSTANCE=<name>`, which mints new secrets and therefore needs the
volume recreated — the header of the generated file says so.

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

**The test instance's account, as rebuilt 2026-08-11 for [M46.6](../build-notes/phase-details/m46.6.md):**

| | |
| --- | --- |
| Address | `review@killerofpie.com` |
| Password | `m46-6-workspace-pair-2026` |

Rebuilt because the credential recorded here from M51.9's rebuild no longer
signed in — exactly the case the paragraph below prescribes a rebuild for. The
instance carries `lctl demo`'s data, which is what gives this account the two
memberships the workspace switcher needs to render at all.

**This table is load-bearing now**: the kept browser spec
`tools/agent-browser/specs/workspace-control.spec.mjs` asserts a signed-in
surface and reads its credentials from `LINKCTRL_UI_EMAIL` /
`LINKCTRL_UI_PASSWORD`, falling back to parsing the Address and Password rows
above — so a rebuild that does not update this table turns `make verify-ui`
red, with a message pointing here.

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

**Using the workspace switcher on the demo does not move the demo.** The refresh
writes into the owning account's oldest workspace — the one it was given when it
claimed the instance — whatever the switcher was last left on. A workspace you
created yourself is never the oldest, so it is neither seeded into nor reset.

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

## The demo is also reachable from off the host

<https://linkctrl-demo.devofpie.com> serves the **demo** instance through a
Cloudflare tunnel. It is the URL to use when the browser is not on the machine
the containers run on, which is the normal case — the host is a development VM
and the person looking at the demo usually is not sitting on it.

**`localhost:8080` and that hostname are the same instance**, not two
deployments. The chain, verified 2026-08-07 rather than assumed:

| Link | How it is established |
| --- | --- |
| Hostname → port | `cloudflared` logs its ingress at startup: `linkctrl-demo.devofpie.com` → `http://localhost:8080`, everything else `http_status:404` |
| Port → container | `linkctrl-demo-app-1` publishes `0.0.0.0:8080->8080` |
| Container → what `make demo-update` rebuilds | that target runs `docker compose -p linkctrl-demo … --force-recreate`, and `linkctrl-demo` is the project those containers belong to |
| Same database, not merely the same image | a demo link alias returns the identical `302` destination on both, and the test instance answers `404` for it |
| Not the test instance | `/readyz` reports `"version": "demo"` through the tunnel and `"version": "test"` on `8081` |

The tunnel is `linkctrl-tunnel.service`, a systemd unit on the host. Its
contents are in
[environment.md](environment.md#the-demo-tunnel) so a rebuild
reproduces it, as with every other unit that carries a path or a credential.

**Ingress is not in a file on this host.** The tunnel is token-managed, so which
hostname maps to which local port is configured in the Cloudflare Zero Trust
dashboard. `journalctl -u linkctrl-tunnel` is where the running configuration
can actually be read, and it is the only local source of truth for it:

```sh
journalctl -u linkctrl-tunnel | grep 'Updated to new configuration'
```

Two consequences worth knowing before debugging:

- **The test instance is not published and must not be.** The ingress has a
  single hostname and a catch-all `404`; adding `8081` would put a stack that
  gets `make rebuild` and load generators pointed at it on the public internet.
- **Absolute URLs the demo generates carry the tunnel hostname.** `.env.demo`
  sets `LINKCTRL_BASE_URL=https://linkctrl-demo.devofpie.com` — owner-asked
  2026-08-11, replacing `http://localhost:8080`, because the demo is reached
  through the tunnel and displayed short links, invitation links
  (`internal/invite/invite.go:904-906` at `a37f2d0`) and freshly minted signed
  links were all pointing at a host the reader cannot reach. Both origins
  follow `BaseURL` while `LINKCTRL_APP_BASE_URL` stays unset. The container
  still binds `:8080` and answers `Host: localhost:8080` too — the base URL
  names what URLs *say*, not what the listener accepts — and
  `LINKCTRL_SECURE_COOKIES` stays `false`, since the VM-local paths that drive
  the demo (`make demo-update`, the seeder) speak plain HTTP.

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
[environment.md](environment.md#the-idle-stop-timer) so a rebuild
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

- [environment.md](environment.md) — the machine these run on
- [development.md](../build-notes/development.md) — project setup, host-agnostic
- [workflow.md](../build-notes/workflow.md) — where the refresh sits in a milestone
- [cli.md](../cli.md) — `lctl demo` and `lctl seed` in full
