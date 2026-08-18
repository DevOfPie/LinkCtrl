#!/usr/bin/env bash
#
# Is the branch's latest CI run green?
#
# Every gate this project's contract names runs on the machine doing the work, so
# a build that is red only on the runner is invisible to all of them. CI was red
# for nine days across a whole phase, two adversarial reviews and a release
# candidate while `make check` reported `0 issues` at every push, and the owner
# found it on the release PR (F255). Nothing was watching, because nothing asked.
#
# What it answers is deliberately the *branch's* verdict rather than this
# commit's. The run for a push that has just happened is still in flight, so a
# check that insisted on it would either block for minutes or pass on `queued`
# and gate nothing; the newest run with a conclusion is what a boundary can
# actually read. Red therefore surfaces at the next boundary after the push that
# caused it, which is the whole distance between nine days and one milestone.
#
# Three outcomes, three exit codes, because two of them are non-zero for
# different reasons and a caller that cannot tell them apart reads an offline
# laptop as a failing build:
#
#   0  green    — the newest concluded run on the branch succeeded
#   1  red      — it failed
#   2  unknown  — the question could not be asked: no gh, no auth, no network,
#                 or no run has ever concluded on this branch
#
# Cancelled, skipped, neutral and stale are *not* verdicts. CI cancels a run in
# progress when a newer push supersedes it (`concurrency.cancel-in-progress`), so
# treating one as red would fail this gate on every second commit; the scan looks
# past them for the newest run that actually decided something.
#
# Usage: scripts/check-ci.sh [branch]
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

WORKFLOW=ci.yml
branch="${1:-$(git rev-parse --abbrev-ref HEAD 2>/dev/null)}"

if [ -z "$branch" ] || [ "$branch" = HEAD ]; then
  printf '  UNKNOWN  could not ask: no branch name (detached HEAD — pass one)\n'
  exit 2
fi

if ! command -v gh >/dev/null 2>&1; then
  printf '  UNKNOWN  could not ask whether CI is green on %s: gh is not installed\n' "$branch"
  printf '           https://cli.github.com — this is not a red build\n'
  exit 2
fi

# stderr is kept rather than discarded: "gh auth login" and a DNS failure are
# both this exit, and the difference is the only useful thing in the message.
err=$(mktemp)
runs=$(gh run list --workflow "$WORKFLOW" --branch "$branch" --limit 20 \
         --json status,conclusion,headSha,displayTitle,url \
         --jq '.[] | [.status, .conclusion, .headSha[0:7], .url, .displayTitle] | @tsv' \
         2>"$err")
rc=$?
message=$(tr -d '\r' < "$err" | grep -v '^[[:space:]]*$' | head -3)
rm -f "$err"

if [ "$rc" -ne 0 ]; then
  printf '  UNKNOWN  could not ask whether CI is green on %s: gh exited %d\n' "$branch" "$rc"
  [ -n "$message" ] && printf '%s\n' "$message" | sed 's/^/           /'
  exit 2
fi

if [ -z "$runs" ]; then
  printf '  UNKNOWN  could not ask whether CI is green on %s: no %s run recorded\n' "$branch" "$WORKFLOW"
  printf '           the branch has never been pushed, or CI has not started — this is not a red build\n'
  exit 2
fi

inflight=0
while IFS=$'\t' read -r status conclusion sha url title; do
  [ -n "$status" ] || continue
  if [ "$status" != completed ]; then
    inflight=$((inflight + 1))
    continue
  fi
  case "$conclusion" in
    cancelled|skipped|neutral|stale) continue ;;
    success)
      printf '  ok    CI is green on %s (%s %s)\n' "$branch" "$sha" "$title"
      [ "$inflight" -gt 0 ] && printf '  note  %d newer run(s) still in flight — this is the newest concluded one\n' "$inflight"
      exit 0
      ;;
    *)
      printf '  FAIL  CI is %s on %s (%s %s)\n' "${conclusion:-unknown}" "$branch" "$sha" "$title"
      printf '        %s\n' "$url"
      [ "$inflight" -gt 0 ] && printf '        %d newer run(s) are still in flight and may yet be green\n' "$inflight"
      exit 1
      ;;
  esac
done <<<"$runs"

printf '  UNKNOWN  could not ask whether CI is green on %s: no run in the last %d has concluded\n' "$branch" 20
printf '           %d still in flight, the rest cancelled or skipped — this is not a red build\n' "$inflight"
exit 2
