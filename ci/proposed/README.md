# Proposed workflows

Changes to `.github/workflows/` are written here and applied by the owner. This
file says why, what belongs here rather than in the Makefile, and the two
commands that apply a proposal.

## Why a proposal and not a commit

The token the agent building this repository holds is a fine-grained PAT without
the `Workflows` permission. That is not an oversight to be corrected: it is the
one permission that would let a token rewrite the file that decides what runs on
a runner, and everything below follows from keeping it absent.

The mechanism is blunt. GitHub refuses the **push**, not the merge:

```
! [remote rejected] task/x -> task/x
  (refusing to allow a Personal Access Token to create or update workflow
   .github/workflows/ci.yml without `workflows` permission)
```

So there is no version of "open a PR and let the owner review it" that works. A
branch carrying a workflow change cannot leave the machine at all, which is why
the change arrives as a file at a path that is not `.github/workflows/`.

### Why the permission stays absent

Granting it would not merely allow workflow edits. A workflow file is code that
runs with `GITHUB_TOKEN`, and a workflow's own `permissions:` block overrides the
repository's read-only default — that setting is a default, not a ceiling. Write
access to `.github/workflows/` therefore converts into `packages: write` (publish
to `ghcr.io`, including moving `latest`), `actions: write` (delete run logs, which
is the audit trail), and `pages: write`, none of which appear on the token. It
also routes around the tag ruleset that is the only control on the release path,
because a job with `packages: write` on `on: push` needs no tag. And a `schedule:`
or `workflow_dispatch:` workflow keeps running after the token is revoked.

The permission is a lever on all of that, not a lever on YAML. One manual step per
workflow change is the price, and workflow changes are rare by design — see the
split below.

## What lives where

| Change | Where | Needs the owner |
| --- | --- | --- |
| A new check, or a changed one | A make target, reached by `ci-build` / `ci-lint` / `ci-integration` | No |
| A tool version — `SQLC_VERSION`, asset checksums | `Makefile` | No |
| What a check actually does | `scripts/*.sh` | No |
| Triggers, `permissions:`, `concurrency:` | `.github/workflows/` | **Yes** |
| Service container images, `runs-on` | `.github/workflows/` | **Yes** |
| Action versions and their SHA pins | `.github/workflows/` | **Yes** |
| `GO_VERSION` | `.github/workflows/` | **Yes** |

The left column is the common case and the right column is not, which is what
makes the manual step affordable. Adding a check to CI is a Makefile edit that
reaches the next push; changing what CI *is* takes a proposal.

`make workflow-proposals` prints which proposals are still pending, with a diff
against the live file. It is deliberately **not** a gate: a pending proposal is a
normal state, and failing CI on one would turn every proposal into a red build
for a change nobody has agreed to yet.

## Applying one

From a checkout with the owner's credentials:

```sh
cp ci/proposed/NAME.yml .github/workflows/NAME.yml
make workflow-proposals          # must now report: applied
git add .github/workflows/NAME.yml
git commit
```

`make workflow-proposals` compares the two files, so `applied` means the live
workflow is the file that was reviewed and not an edited-in-transit version of
it. Use `cp` rather than copying the text through an editor: only the **final
newline** is normalised in that comparison, and everything else counts,
whitespace included. The first proposal applied by hand lost its trailing newline
and reported pending until the comparison was taught to ignore that one byte.

Then close the loop the way the repository closes every process change: move the
row in [`docs/build-notes/workflow-changes.md`](../../docs/build-notes/workflow-changes.md)
to *Made* with the commit, and delete the file from this directory. A proposal
that stays here after being applied becomes a second copy of the workflow, free
to drift from the one that runs — which is the failure this directory would
otherwise invite.

## Writing one

- Change one thing. A proposal is reviewed by a human reading YAML, and a diff
  that mixes a trigger change with a refactor gets approved for the refactor.
- Say what is *not* changing. The reviewer's question is always "does this alter
  what runs on push", and the answer belongs in the file's header comment.
- Raise a row in `workflow-changes.md` under *Proposed* at the same time, so the
  waiting change is as visible as a waiting defect.
