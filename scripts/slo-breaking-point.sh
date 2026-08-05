#!/usr/bin/env bash
#
# Grows the dataset until a documented check fails, and says which one.
#
# scripts/load-test.sh answers "does this build meet the SLO at the size the SLO
# is defined against". It cannot answer "at what size does it stop", because the
# size is fixed by whoever seeded the database — so every column in docs/slo.md
# is a pass, and none of them is a bound. This script is the other question: seed,
# measure, check, multiply, repeat, and stop at the first thing that breaks.
#
# What it is not: a benchmark. The numbers depend on the machine, the disk and
# what else is running, so a run here is evidence about *this* box and about the
# shape of the degradation — which check gives first, and how far past the
# documented size it happens. That shape is the transferable part.
#
# Usage: scripts/slo-breaking-point.sh [start-links] [multiplier] [max-steps]
#
#   scripts/slo-breaking-point.sh                # 100k links, ×2, 6 steps
#   scripts/slo-breaking-point.sh 25000 4 4      # coarser, faster
#
# Reads the same instance variables the other scripts do; run it against the
# **test** instance and expect it to reseed that database repeatedly.
set -euo pipefail

START_LINKS="${1:-100000}"
MULTIPLIER="${2:-2}"
MAX_STEPS="${3:-6}"

# Clicks scale with links, because the rollup's cost is driven by click volume
# and the redirect path's by neither. Two per link matches what the SLO dataset
# uses at its own size, so step one reproduces a documented column rather than
# inventing a new baseline.
CLICKS_PER_LINK="${CLICKS_PER_LINK:-2}"

RATE="${RATE:-2000}"
DURATION="${DURATION:-1m}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTANCE="${INSTANCE:-test}"
ENV_FILE="${REPO_ROOT}/.env.${INSTANCE}"
PROJECT="linkctrl-${INSTANCE}"
NETWORK="${PROJECT}_default"
APP="${LINKCTRL_APP_HOST:-app}"
OUT="${TMPDIR:-/tmp}"

export COMPOSE_PROJECT_NAME="$PROJECT"
export COMPOSE_ENV_FILES="$ENV_FILE"
export LINKCTRL_NETWORK="$NETWORK"

die() { echo "slo-breaking-point: $*" >&2; exit 1; }

[ -f "$ENV_FILE" ] || die "no $ENV_FILE; create the instance first"
command -v docker >/dev/null || die "docker is required"

# The database URL as the host sees it, for `lctl seed`.
# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a
DB_PORT="${POSTGRES_PORT:?}"
DSN="postgres://${POSTGRES_USER:-linkctrl}:${POSTGRES_PASSWORD:?}@localhost:${DB_PORT}/${POSTGRES_DB:-linkctrl}?sslmode=disable"

psql_q() {
  docker compose exec -T postgres psql -U "${POSTGRES_USER:-linkctrl}" \
    -d "${POSTGRES_DB:-linkctrl}" -tAc "$1"
}

scrape() {
  docker run --rm --network "$NETWORK" curlimages/curl:latest \
    -fsS "http://${APP}:9090/metrics"
}

# metric FILE NAME [LABEL-SUBSTRING] — sum of one series' samples.
metric() {
  awk -v n="$2" -v l="${3:-}" '
    index($0, n) == 1 && (l == "" || index($0, l)) { s += $NF }
    END { printf "%.6f", s + 0 }' "$1"
}

# cached_le FILE LE — cumulative cached-redirect count in one histogram bucket.
cached_le() {
  awk -v le="$2" '
    /^linkctrl_redirect_duration_seconds_bucket/ &&
    index($0, "outcome=\"redirect\"") &&
    (index($0, "cache=\"memory\"") || index($0, "cache=\"redis\"")) &&
    index($0, "le=\"" le "\"") { s += $NF }
    END { printf "%d", s + 0 }' "$1"
}

echo "== plan =="
echo "start        : ${START_LINKS} links, $(( START_LINKS * CLICKS_PER_LINK )) clicks"
echo "multiplier   : x${MULTIPLIER} per step, up to ${MAX_STEPS} steps"
echo "load         : ${RATE} rps for ${DURATION}, cached"
echo
echo "Checks, in the order they are evaluated. Each names the document it comes"
echo "from, so a failure is a claim being broken rather than a number somebody"
echo "did not like:"
echo "  1. cached redirect p99 under 20ms        docs/slo.md, the SLO itself"
echo "  2. every cached redirect under 20ms      the same target, as a fraction"
echo "  3. no analytics events dropped           bounded queue, docs/operations.md"
echo "  4. totals rollup inside its 60s tick     docs/operations.md's alert"
echo "  5. dimension rollup inside its 15m tick  the same, at its own cadence"
echo

links="$START_LINKS"
step=0
last_pass=""

while [ "$step" -lt "$MAX_STEPS" ]; do
  step=$(( step + 1 ))
  clicks=$(( links * CLICKS_PER_LINK ))

  echo "──────────────────────────────────────────────────────────────────────"
  echo "== step ${step}: ${links} links, ${clicks} clicks =="

  # Reseeded from empty each step rather than topped up: `seed` adds, so
  # growing in place would leave the previous step's rows behind and make the
  # size a running total nobody stated. A step is a size, not a delta.
  echo "-- reseeding"
  docker compose down -v >/dev/null 2>&1 || true
  docker compose up -d --wait >/dev/null || die "stack did not come up"
  curl -sS -X POST "http://localhost:${LINKCTRL_HTTP_PORT}/api/v1/auth/setup" \
    -H 'Content-Type: application/json' \
    -d '{"email":"slo@example.com","password":"a-sufficiently-long-password","name":"SLO"}' \
    -o /dev/null || die "could not claim the instance"

  LINKCTRL_APP_ENV=development LINKCTRL_DATABASE_URL="$DSN" \
    go run "${REPO_ROOT}/cmd/lctl" seed --links "$links" --clicks "$clicks" \
    >/dev/null || die "seed failed at ${links} links"

  echo "-- measuring"
  before="$OUT/bp-before-${step}.txt"
  after="$OUT/bp-after-${step}.txt"

  # Warm the cache tiers, then snapshot: the server histogram is cumulative, so
  # a snapshot taken before the warm-up folds every cold read into the result.
  RATE="$RATE" DURATION="$DURATION" \
    "${REPO_ROOT}/scripts/load-test.sh" cached "$RATE" "$DURATION" \
    >"$OUT/bp-load-${step}.txt" 2>&1 || true
  scrape >"$after"
  cp "$after" "$before" 2>/dev/null || true

  # The load script already reports its own before/after delta; this reads the
  # absolute state afterwards for the checks that are not deltas.
  total=$(cached_le "$after" "+Inf")
  under20=$(cached_le "$after" "0.02")
  dropped=$(metric "$after" linkctrl_analytics_events_dropped_total)
  stale_rollup=$(metric "$after" linkctrl_rollup_staleness_seconds 'job="analytics_rollup"')
  stale_dim=$(metric "$after" linkctrl_rollup_staleness_seconds 'job="analytics_dimension_rollup"')

  fraction=$(awk -v u="$under20" -v t="$total" \
    'BEGIN { if (t == 0) { print "0" } else { printf "%.4f", (u / t) * 100 } }')

  printf 'cached redirects  : %s\n' "$total"
  printf 'under 20ms        : %s (%s%%)\n' "$under20" "$fraction"
  printf 'events dropped    : %s\n' "$dropped"
  printf 'rollup staleness  : %ss (totals), %ss (dimensions)\n' "$stale_rollup" "$stale_dim"

  failed=""
  [ "$total" -gt 0 ] || failed="no cached redirects were recorded at all"
  if [ -z "$failed" ] && awk -v f="$fraction" 'BEGIN { exit !(f < 99.0) }'; then
    failed="only ${fraction}% of cached redirects were under 20ms (SLO: p99)"
  fi
  if [ -z "$failed" ] && awk -v d="$dropped" 'BEGIN { exit !(d > 0) }'; then
    failed="${dropped} analytics events were dropped"
  fi
  if [ -z "$failed" ] && awk -v s="$stale_rollup" 'BEGIN { exit !(s > 600) }'; then
    failed="the totals rollup is ${stale_rollup}s stale (alert threshold 600s)"
  fi
  if [ -z "$failed" ] && awk -v s="$stale_dim" 'BEGIN { exit !(s > 3600) }'; then
    failed="the dimension rollup is ${stale_dim}s stale (alert threshold 3600s)"
  fi

  if [ -n "$failed" ]; then
    echo
    echo "== FAILED at ${links} links / ${clicks} clicks =="
    echo "   $failed"
    echo
    if [ -n "$last_pass" ]; then
      echo "Last size that passed every check: ${last_pass}."
    else
      echo "No size passed, including the first — the failure is not about size."
    fi
    echo "Full load output: $OUT/bp-load-${step}.txt"
    exit 1
  fi

  # A passing step reports its **margin**, not just "passed". A run that only
  # ever says pass tells you the checks hold and nothing about where the edge
  # is, which is the same shortcoming this script exists to fix in load-test.sh
  # one level up.
  echo "-- margin to each threshold"
  printf '  cached under 20ms   : %s%% of 100%% required 99%%\n' "$fraction"
  printf '  totals rollup       : %ss of 600s\n' "$stale_rollup"
  printf '  dimension rollup    : %ss of 3600s  (its own tick is 900s)\n' "$stale_dim"
  awk -v s="$stale_dim" 'BEGIN {
    if (s > 900) {
      print "  NOTE: the dimension rollup is now behind its own 15m cadence."
      print "        The alert threshold is four missed runs, so this passes and"
      print "        is the number to watch: it is the check that moves with size."
    }
  }'

  echo "all checks passed"
  last_pass="${links} links / ${clicks} clicks"
  links=$(( links * MULTIPLIER ))
done

echo "──────────────────────────────────────────────────────────────────────"
echo "== no check failed in ${MAX_STEPS} steps =="
echo "Largest size measured: ${last_pass}."
echo
echo "That is a floor and not a ceiling: it says the checks hold at least this"
echo "far on this machine, and says nothing about where they stop. Raise"
echo "max-steps, or the multiplier, to push it."
echo
echo "Read the margins above rather than the verdict. The redirect columns stay"
echo "flat with size — a cache hit does not care how many rows it did not read —"
echo "so the check that actually moves is the dimension rollup's staleness, which"
echo "is what docs/slo.md already names as the bottleneck. That is where the edge"
echo "is, and a run that stops before finding it should say which number was"
echo "closest rather than that everything was fine."
