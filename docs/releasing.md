# Releasing

How a version gets published, and what a version number means.

## What is versioned

Two contracts, deliberately separate:

| | Versioned by | Breaking change means |
| --- | --- | --- |
| The REST API | The path: `/api/v1` | A new path, `/api/v2`. Never a change to `v1`. |
| The product | The release version | A new major version. |

The product is pre-1.0 while account lifecycle and identity are incomplete —
there is no SSO, OAuth, OIDC or SCIM, and each of those moves the sign-in surface
and adds tables — so releases stay in the `0.x` range until that has settled.
`0.x` says "the product surface may still move", not "unfinished". Everything
documented as built is tested and exercised end to end, and the SLO is measured.

*(This read "pre-1.0 while Phase 2 is outstanding — shared workspaces, folders and
custom domains will move the dashboard and add tables" until 0.3.0. All three
shipped in 0.2.0, so the sentence named its own contents as future work for a
whole release.)*

The database schema only changes additively within a minor version. Migrations run
at boot, and `LINKCTRL_MIGRATE_ON_START=false` makes them a deliberate step for
change-controlled deployments — **the product's own, not an installed add-on's**,
whose DDL the host applies at every boot regardless (F282).

## Cutting a release

```sh
# 1. Write the changelog section first. It is the thing an operator reads to
#    decide whether to take the upgrade, so it is written for them, not from
#    `git log`. Everything in [Unreleased] moves into it, dated today: the
#    release notes are that section and nothing else.
$EDITOR CHANGELOG.md          # add "## [0.3.0] - YYYY-MM-DD", empty
                              # [Unreleased] into it, update the links

git add CHANGELOG.md && git commit -m "Changelog for 0.3.0"

# 2. Everything that must hold. Runs the same checks CI does, plus the ones that
#    only matter when publishing.
make release-check VERSION=v0.3.0
#   or: scripts/release-check.sh v0.3.0

# 3. Tag and push. The tag is what the workflow builds, and the version the
#    binaries report comes from it.
git tag -a v0.3.0 -m "v0.3.0"
git push origin v0.3.0
```

`release-check` verifies: the working tree is clean, the tag does not exist, the
changelog has a section for it, that section is dated today, `[Unreleased]` is
still there and empty, **the branch's latest CI run is green**, `sqlc` output
matches its SQL, the vendored assets match their checksums, the stylesheet is
built, the build and tests pass under the race detector, the OpenAPI document
matches the registered routes, and every release platform cross-compiles.

The two forms above are equal, and one of them was not. The integration tests run
only when Postgres is up, that question is asked through `docker compose`, and
`docker compose` needs `COMPOSE_PROJECT_NAME` and `COMPOSE_ENV_FILES` to know
which stack is meant — which the Makefile exported and a plain shell did not. So
`scripts/release-check.sh v0.3.0` reported `skip  Postgres is not running` on a
machine where it was, for a whole phase, and a skip reads as information rather
than as a third of the gate not running (F253). The script derives both variables
and both DSNs itself now, and a step of its own fails if either derivation stops
agreeing with the Makefile's.

CI's verdict is asked rather than assumed, because every other check here runs on
the machine doing the release and a build that is red only on the runner is
invisible to all of them — one was, for nine days (F255). It has three outcomes,
not two: green, red, and **could not ask**. The last is reported and does not
block, since an offline machine cannot answer the question and a gate that
guessed would be worse than none.

The three changelog checks beyond *a section exists* are there because the section
existing is what both guards used to ask, and that passes on notes describing the
wrong thing. The workflow publishes the version's section only, so anything left
in `[Unreleased]` is written and never read; a date is a claim about when a
version was released, so one written on the day the notes were drafted is wrong
by however long the tag took to follow; and the `[Unreleased]` heading itself has
to still be there, because deleting it rather than emptying it leaves its link
reference at the foot of the file pointing at nothing and the next release with
nowhere to be written.

The clean-tree check is not fussiness: a release has to be reproducible from the
tag, and an uncommitted file means the artifacts contain something the tag does
not.

### When the tag is not cut on the day the notes were folded

**Expected, not a fault.** Step 1 dates the section *today* and step 3 is the
owner's act on a day nobody schedules, so any gap between them makes step 2 fail
— and that is the check doing its job rather than getting in the way. Two things
can have gone stale, and they are checked separately because either can happen
without the other:

```text
FAIL  the 0.3.0 section is dated 2026-08-17 and the tag is being cut on
      2026-08-21 — re-date it
FAIL  [Unreleased] holds 14 line(s) that 0.3.0's notes will not contain —
      fold them into '## [0.3.0]'
```

The second one is the normal consequence of ordinary work continuing: every
commit is required to describe itself in `[Unreleased]`, so a day of commits
after the fold refills it. The remedy is the same edit as step 1, made again:

```sh
$EDITOR CHANGELOG.md          # move whatever is back in [Unreleased] into the
                              # 0.3.0 section, and change its date to today

git commit -am "Changelog for 0.3.0"   # or --amend, if the changelog commit is
                                       # still the tip. Either is fine: the tag
                                       # has not been cut and nothing is public

make release-check VERSION=v0.3.0
```

Re-run until it is quiet, then tag. Do not tag against a failing `release-check`
on the grounds that only the date is wrong: the date is the one claim in the file
that cannot be corrected after the fact, because the notes are published from the
tag.

## What the tag produces

[`.github/workflows/release.yml`](../.github/workflows/release.yml) runs on
`v*.*.*` and does four things in order, each gated on the previous:

1. **Verify.** Assets, build, vet, unit tests with the race detector, the OpenAPI
   parity test, and the full integration suite against a real Postgres and Redis.
   Plus **one** of `release-check`'s four changelog checks again — that a
   `## [<version>]` section exists — because a machine should not trust that the
   local gate was run. The other three stay local on purpose. A date check here
   would compare against the *workflow's* run date and would refuse a release
   legitimately tagged the evening before, so the same rule in the two places is
   not the same rule; and the `[Unreleased]` checks are only meaningful beside
   it, since folding and re-dating are one edit. `release-check` is where the tag
   is cut, and that is where they belong. Nothing in CI runs
   `scripts/release-check.sh`.
2. **Image.** `linux/amd64` and `linux/arm64`, pushed to
   `ghcr.io/devofpie/linkctrl` tagged with the exact version, the `major.minor`
   series, and `latest`. Provenance and an SBOM are attached. **Lowercase, and
   the tags carry no `v`**: a registry reference must be lowercase — `docker
   pull ghcr.io/DevOfPie/LinkCtrl:0.3.0` is refused as an invalid reference
   before it reaches the network — and `docker/metadata-action`'s `{{version}}`
   strips the prefix, so the tag beside `v0.3.0` is `0.3.0`. This line said
   `ghcr.io/DevOfPie/LinkCtrl` until 0.3.0's post-release check;
   [release.yml](../.github/workflows/release.yml)'s own comment had already
   named the hazard, which is why the notes it generates were always right and
   only this sentence was wrong.
3. **Binaries.** `linkctrl` and `lctl` cross-compiled for linux (amd64, arm64),
   macOS (arm64, amd64) and Windows (amd64), each archive carrying `LICENSE`,
   `README.md`, `CHANGELOG.md` and `.env.example`, with a `SHA256SUMS` file.
   (Three were listed until 0.2.0 and the Makefile copied four — [F45](build-notes/deferred-findings.md).) The workflow unpacks
   the linux/amd64 archive and asserts the binaries report the version being
   released — a stamp that silently fails is invisible until someone needs it.
4. **Publish.** A GitHub release whose notes are this version's changelog section,
   plus the image tag, the image digest, and how to verify a download.

`workflow_dispatch` runs everything except publishing, so the artifact path can be
exercised without releasing anything.

## Building it yourself

Nothing above needs GitHub. The same artifacts come out of:

```sh
make docker-build                 # image, stamped from git describe
make dist                         # cross-compiled archives + SHA256SUMS in dist/
make dist VERSION=v0.3.0          # or with an explicit version
```

`make dist` builds the stylesheet first, because it is embedded — a binary built
without it serves an unstyled dashboard, and the server warns about that at boot
rather than failing.

## Upgrading an instance

The image tag is the whole upgrade mechanism:

```sh
# Pin the version rather than tracking latest, so a restart is never a surprise.
echo LINKCTRL_TAG=0.3.0 >> .env
docker compose pull app
docker compose up -d --wait
```

Migrations run at boot before the listener opens, so there is no separate step.
Readiness fails first and the drain delay elapses before the old container's
listener closes, which is what makes a rolling restart lossless.

For a deployment that has to be reproducible, pin the digest instead — it is in
the release notes:

```yaml
services:
  app:
    image: ghcr.io/devofpie/linkctrl@sha256:…
```

A tag can be repointed; a digest cannot. `latest` exists for the quick start and
is the wrong thing to run in production.

### Rolling back

Point `LINKCTRL_TAG` at the previous version and bring it up again. The constraint
is the schema: migrations are additive within a minor version, so rolling back
inside a series is safe, and rolling back across one may leave columns the older
binary does not know about — harmless — or require `lctl migrate down`, which drops
columns and therefore data. Test that on a copy first.

Keep a backup from before the upgrade. `docs/deployment.md` has the command.

## Release checklist

Things a script cannot check:

- [ ] The changelog section is written for an operator deciding whether to upgrade,
      not assembled from commit subjects.
- [ ] Anything that changed behaviour an operator configures is in
      [configuration.md](configuration.md), and anything that changed what they
      watch is in [operations.md](operations.md).
- [ ] New limitations are in [Plan.md](../Plan.md#known-limitations) and the
      changelog, not only in a commit message.
- [ ] Removed environment variables are in `config.Removed`, so a stale `.env`
      produces a warning rather than silence.
- [ ] Any performance claim in the documentation was measured on this version, or
      is labelled with the version it was measured on.
