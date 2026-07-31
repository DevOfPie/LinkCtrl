# Workflow

Operating rules for whoever is building this, human or model.

**Style is deliberate.** Terse, trigger-first, no rationale. The rest of this
project explains itself at length; this file is read on every task, so it is
optimized for scanning and low token cost instead. Do not rewrite it into prose.
Rationale belongs in [decisions.md](decisions.md).

**Precedence.** [Plan.md](../../Plan.md) is the scope contract and wins on *what*.
This file wins on *process*. If they conflict, the conflict is a bug — report it,
do not pick.

---

## Vocabulary

| Term | Meaning |
| --- | --- |
| **In spec** | Required by the milestone currently being worked. |
| **Out of spec** | Anything else, including defects found by accident. |
| **Work** | A change beyond spelling, phrasing, formatting, or the wording of docs. Anything touching code, SQL, config, tests, generated output, or documented behaviour is work. |
| **Owner** | The repository owner. The only one who approves deferred findings and merges. |

---

## Triggers

### An issue is found — any time, any source

```
in spec      → fix now, inside the current milestone
out of spec  → DO NOT FIX
               append one row to Plan.md → "Deferred findings"
               continue the current milestone
```

Row: what, where (`file:line`), evidence it is real, suspected severity.

Out-of-spec items are worked only after the owner reviews and approves that item.
They collect into the phase's final milestone. Approval is per item, not per
batch — an unreviewed row is not scheduled work.

A defect that makes the *current* milestone's claim false is in spec, whatever it
looks like. Judge by the claim, not by the subsystem.

### Before completing a commit

All must pass. Failure means the commit does not happen.

```sh
make check              # tidy + lint + unit tests, race enabled
make test-integration   # needs the stack up: make up
```

Then:

| Gate | Condition |
| --- | --- |
| `golangci-lint` | 0 issues |
| Tests | Unit and integration green under `-race` |
| Generated code | If any `.sql` changed: `make sqlc` produces no diff |
| OpenAPI | If any API surface changed: `make openapi` passes |
| Docs | Plan.md reflects new truth; decisions.md has the *why* for anything non-obvious |
| Links | Every relative link and anchor in tracked `.md` resolves |
| Scope | **One milestone per commit, maximum.** Never bundle two. Splitting one across several is fine. |

Commit messages are long prose explaining *why*, not what. The diff shows what.

### A milestone is finished — validated, committed

```
make demo-update
```

Rebuilds the demo instance from the commit just made and regenerates its data.
Refuses on a dirty tree; that refusal means the milestone is not finished.

Work happens on the **test** instance. The demo changes here and nowhere else.
Both are in [dev-notes/instances.md](../dev-notes/instances.md).

### A test passes on the first try

Sabotage it. Break the thing it claims to protect, confirm the test fails, then
restore. A test that has never failed has not been shown to test anything.

Restore by **counter-edit**, never `git checkout` — checkout has twice destroyed
uncommitted work in this repo.

### Before a phase PR is created

1. Full validation:

   ```sh
   make release-check      # tree clean, generated code current, docs consistent
   make check
   make test-integration
   ```

2. Verify live against the composed stack, not only tests. Recreate the container
   first (`docker compose up -d --force-recreate --wait app`) or the check runs
   against the previous image and passes for the wrong reason.

3. **If validation triggers work, repeat validation from step 1.** A fix that is
   only spelling, phrasing, formatting, or docs wording does not re-trigger.
   Anything else does. No exceptions for "obviously safe".

4. Then, and only then, the documentation pass below.

5. Then create the PR.

### Documentation pass — after validation, before the PR exists

Update, clean, and minimize every documentation file. Not only the ones this
phase touched.

| File | Check |
| --- | --- |
| `Plan.md` | Build status, milestone rows, scope tables, known limitations all true as of now |
| `README.md` | Status line, feature claims, "Not built yet" |
| `CHANGELOG.md` | Entry for what shipped, with its limitations |
| `docs/*.md` | Configuration, usage, operations, deployment, CLI, releasing — every documented behaviour still behaves that way |
| `docs/build-notes/decisions.md` | Append-only. Never edit an entry; a later entry corrects an earlier one |
| `docs/build-notes/SECURITY.md` | New defences, new gaps, new operator responsibilities |
| `docs/build-notes/workflow.md` | This file. Rules learned this phase |
| `docs/adr/` | Investigations that outgrew a decision-log entry |

Minimize means: delete what is no longer true, merge what is duplicated, and cut
what restates something the reader already read. It does not mean shortening
explanations that carry a reason.

Every documented claim must be verifiable by a reader who does not trust you. No
numbers without a measurement, no "should be fast", no feature described in the
present tense that is not built.

---

## Standing rules

**Verify, do not assume.** Never report a measurement taken on a dirty tree, or
before the code under test was actually built and deployed. If a check passed for
a reason you cannot name, it did not pass.

**Report failures plainly.** Tests that fail, steps skipped, things not verified —
say so, with the output. A green summary over a red run is the worst possible
output.

**No byte-mangling tools on source.** PowerShell `Get-Content`/`Set-Content` has
corrupted UTF-8 in this repo (em-dashes into mojibake). Use editors that preserve
bytes.

**Plan.md states what is true. decisions.md states why.** Rationale in the plan
and status in the decision log are both wrong.

**Stop and ask** for: destructive operations, scope changes, anything the owner
would reasonably want to decide. Proceed without asking for reversible work that
follows from the current milestone.

---

## Quick reference

```sh
make up                 # start Postgres, Redis, app — the test instance
make check              # tidy + lint + unit tests (race)
make test-integration   # integration tests (needs the stack)
make generate           # sqlc + openapi
make release-check      # full pre-release validation
make rebuild            # test instance from nothing, migrated
make down               # stop and remove volumes
make instances          # both instances, and whether they are up
make demo-update        # refresh the demo — after a milestone, not during one
```

Targets act on the **test** instance unless given `INSTANCE=demo`. Destructive
ones refuse `demo` without `CONFIRM=demo`.

The test instance is stopped after thirty idle minutes and does not restart
itself. Anything needing a database starts one, so this is rarely visible; when
it matters — a long unattended job — `touch /tmp/linkctrl-test-keep`.
