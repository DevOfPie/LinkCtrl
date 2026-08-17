#!/usr/bin/env bash
#
# Replaces every replica of a running deployment while 2,000 requests a second
# go through a load balancer, and reports what that cost (M57).
#
# docs/slo.md records what this produces. Read that first if you want the
# numbers; read this if you want to know whether to believe them.
#
# ## What is being measured, and against what
#
# docs/operations.md states a contract in three parts: `/readyz` says whether to
# route here, a 503 means remove and a 200 (including `degraded`) means keep,
# and the drain delay must exceed the health-check interval times the failure
# threshold. Every one of those is a promise somebody will build a load balancer
# against. This script builds one — test/ha/haproxy.cfg, with the inequality
# satisfied — and then takes each replica away underneath it.
#
# Three numbers come out, and m57.md names all three: requests **failed**,
# requests **retried**, and the cached **p99** while the replacement is
# happening. The first two are HAProxy's own counters rather than the
# generator's inference; the third is both halves, the way every other figure in
# slo.md is reported.
#
# ## Two modes, because the drain delay has to be worth something
#
#   deploy  SIGTERM, the shipped stop_grace_period, and the drain the product
#           performs — what `docker compose up -d --force-recreate` does, and
#           what an operator's rolling deploy is.
#   kill    SIGKILL. No drain, no readiness change, the listener simply stops
#           existing — a crash, an OOM kill, a yanked cable.
#
# Running only the first would measure a number without saying what it bought.
# The pair prices the drain delay: the difference between the columns *is* the
# contract.
#
# ## The two-leader poll
#
# jobs.go says each job family can have two leaders for the length of a rolling
# deploy. While the deploy runs, this samples pg_locks and counts distinct
# holders per advisory key, so that sentence stops being a possibility and
# becomes a number. The sampling interval bounds what it can see, and the report
# says so rather than claiming a window of zero it could not have observed.
#
# Usage: scripts/rolling-deploy.sh [deploy|kill] [rate] [duration]
set -euo pipefail

MODE="${1:-deploy}"
RATE="${2:-2000}"
DURATION="${3:-2m}"

case "$MODE" in
  deploy|kill) ;;
  *) echo "mode must be deploy or kill, got $MODE" >&2; exit 2 ;;
esac

REPLICAS=(app1 app2 app3)
PROJECT="${COMPOSE_PROJECT_NAME:-linkctrl-test}"
ENV_FILE="${COMPOSE_ENV_FILES:-.env.test}"
NETWORK="${LINKCTRL_NETWORK:-${PROJECT}_default}"
PREFIX="${LINKCTRL_SEED_PREFIX:-ld}"
# Sampling period for the advisory-lock poll, in seconds. It is the resolution
# of the two-leader measurement and is reported with the result.
POLL="${LINKCTRL_LOCK_POLL:-0.1}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${TMPDIR:-/tmp}/lc-rolling-$$"
mkdir -p "$OUT"

export MSYS_NO_PATHCONV=1
if command -v cygpath >/dev/null 2>&1; then
  REPO_ROOT="$(cygpath -m "$REPO_ROOT")"
fi

die() { echo "rolling-deploy: $*" >&2; exit 1; }

command -v docker >/dev/null || die "docker is required"

# The developer override is named explicitly rather than left to be applied
# automatically, because naming any `-f` at all turns the automatic application
# off. It is wanted: it is what publishes the database ports the rest of this
# repository's targets connect through, and a run that recreated Postgres
# without them would leave `make test-integration` unable to reach the instance
# it just measured. It patches `app`, `postgres` and `redis` only, so the three
# replicas below inherit nothing from it.
compose() {
  docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
    -f "${REPO_ROOT}/docker-compose.yml" \
    -f "${REPO_ROOT}/docker-compose.override.yml" \
    -f "${REPO_ROOT}/test/ha/compose.yml" "$@"
}

psql_() {
  compose exec -T postgres psql -U "${POSTGRES_USER:-linkctrl}" \
    -d "${POSTGRES_DB:-linkctrl}" -X -q "$@"
}

curl_() { docker run --rm --network "$NETWORK" curlimages/curl:latest "$@"; }

# A run interrupted halfway leaves a detached generator pushing 2,000 rps at an
# instance nobody is watching, and a psql session looping over pg_locks. Both are
# taken down here rather than left to be noticed.
# shellcheck disable=SC2329,SC2317  # invoked by the trap below, not by name
#
# **Two codes for one fact, because the runner's shellcheck is not this
# machine's.** 0.11 calls an uninvoked function SC2329; older releases call its
# body unreachable, SC2317, and the disable that named only the first was silent
# here and red in CI from 2026-08-09 until M58's close. The Makefile's reason for
# leaving shellcheck unpinned — *output is stable across minor versions* — is
# what this falsifies, and the comment there now says so.
cleanup() {
  [ -n "${K6_CONTAINER:-}" ] && docker rm -f "$K6_CONTAINER" >/dev/null 2>&1
  [ -n "${POLL_PID:-}" ] && kill "$POLL_PID" >/dev/null 2>&1
  return 0
}
trap cleanup EXIT

## ---- the image has to be the code under test -------------------------------

# m57.md is explicit that the container is recreated first, "or the run measures
# the previous image and passes for the wrong reason". Building is the half that
# rule leaves implicit: force-recreating from a stale image recreates the stale
# image.
echo "== building the image from the working tree =="
compose build app1

echo
echo "== bringing up three replicas behind a load balancer =="
# The single-replica `app` is stopped first. Four replicas against one database
# is not what is being measured, and the fourth would hold job leadership for
# the whole run.
compose stop app >/dev/null 2>&1 || true
compose up -d --wait postgres redis
compose up -d --force-recreate --wait "${REPLICAS[@]}"
compose up -d --force-recreate --wait haproxy

## ---- dataset ---------------------------------------------------------------

echo
echo "== dataset =="
links=$(psql_ -tAc \
  "SELECT count(*) FROM links WHERE alias LIKE '${PREFIX}%' AND deleted_at IS NULL")
clicks=$(psql_ -tAc "SELECT count(*) FROM click_events")
echo "seeded links:  ${links}"
echo "click events:  ${clicks}"
[ "$links" -gt 0 ] || die "no seeded links; run 'make seed-slo' first (see docs/slo.md)"

## ---- helpers ---------------------------------------------------------------

# scrape HOST FILE — one replica's metrics. The port is deliberately unpublished,
# so this runs on the compose network like everything else here.
scrape() { curl_ -fsS "http://$1:9090/metrics" > "$2"; }

# bucket FILE CACHE LE — cumulative count in one bucket of the redirect
# histogram, successful redirects from one cache tier. Same reader as
# scripts/load-test.sh, because two readers of one histogram is two things that
# can disagree about what the SLO is.
bucket() {
  awk -v c="$2" -v le="$3" '
    /^linkctrl_redirect_duration_seconds_bucket/ &&
    index($0, "cache=\"" c "\"") &&
    index($0, "outcome=\"redirect\"") &&
    index($0, "le=\"" le "\"") { s += $2 }
    END { printf "%d", s + 0 }' "$1"
}

# cached_bucket FILE LE — memory and redis are both cache hits; database is not.
cached_bucket() {
  echo $(( $(bucket "$1" memory "$2") + $(bucket "$1" redis "$2") ))
}

# summed LE — the measured window, across every process that served part of it.
#
# A rolling deploy replaces the processes holding the histogram, so the
# before/after delta scripts/load-test.sh takes is not available: two thirds of
# the counters are destroyed mid-run. Instead each replica is scraped when the
# window opens and again immediately before it is replaced, and its replacement
# is scraped at the end from a counter that started at zero. The sum is the whole
# window with nothing double-counted, and it is why the script snapshots a
# replica before stopping it rather than only at the ends.
summed() {
  local le="$1" total=0 r
  for r in "${REPLICAS[@]}"; do
    total=$(( total
      + $(cached_bucket "$OUT/$r-pre.txt" "$le") - $(cached_bucket "$OUT/$r-start.txt" "$le")
      + $(cached_bucket "$OUT/$r-end.txt" "$le") ))
  done
  echo "$total"
}

# stat FILE FIELD — one field of the HAProxy stats CSV, for the app backend as a
# whole. Read by header name: the column order is HAProxy's and has grown
# between versions.
stat() {
  awk -F, -v want="$2" '
    NR == 1 { sub(/^# /, "", $0); n = split($0, h, ","); for (i = 1; i <= n; i++) if (h[i] == want) col = i; next }
    $1 == "app" && $2 == "BACKEND" && col { print $col + 0 }' "$1"
}

delta() { echo $(( $(stat "$OUT/haproxy-after.csv" "$1") - $(stat "$OUT/haproxy-before.csv" "$1") )); }

# k6 PHASE BASE [flags...] — one invocation of the generator, against whatever
# it is pointed at. The measured run goes through the load balancer; warm-up
# goes at each replica directly, for the reason below.
k6() {
  local phase="$1" base="$2"; shift 2
  docker run --rm -i --network "$NETWORK" \
    -v "${REPO_ROOT}/test/load:/scripts:ro" \
    -e "BASE=${base}" -e "PHASE=${phase}" -e "MODE=cached" \
    -e "RATE=${RATE}" -e "DURATION=${DURATION}" \
    -e "PREFIX=${PREFIX}" -e "TOTAL=${links}" \
    grafana/k6 run "$@" /scripts/redirect.js
}

## ---- warm-up ---------------------------------------------------------------

# Each replica directly, not through the load balancer. The in-process tier is
# per process, so a warm-up spread round-robin over three replicas warms a third
# of the set in each and leaves the rest to be read from Redis inside the
# measured window. Warming each one whole is the only way the *surviving*
# replicas are genuinely cached when the window opens.
#
# The replicas created *during* the deploy start cold, and nothing can be done
# about that: it is what a rolling deploy does. Their misses land on the shared
# Redis tier rather than on Postgres, which is what the shared tier is for, and
# the cache mix at the end reports the split rather than hiding it.
echo
echo "== warm-up (each replica's in-process tier, separately) =="
for r in "${REPLICAS[@]}"; do
  k6 warm "http://${r}:8080" --quiet >/dev/null || die "warm-up against $r failed"
  echo "  $r warmed"
done

## ---- snapshots, then the run ------------------------------------------------

echo
echo "== before =="
for r in "${REPLICAS[@]}"; do scrape "$r" "$OUT/$r-start.txt"; done
curl_ -fsS "http://haproxy:8404/stats;csv" > "$OUT/haproxy-before.csv"
echo "histogram snapshot taken on each replica; load-balancer counters read"

# The advisory-lock poll, for the length of the run plus a margin. One psql
# session with a server-side loop rather than a shell loop calling psql: an exec
# per sample would cost more than the window being looked for.
poll_seconds=$(( $(printf '%s' "$DURATION" | sed 's/m/*60/;s/s//') + 30 ))
poll_seconds=$(( poll_seconds ))
{
  psql_ -v ON_ERROR_STOP=1 <<SQL > "$OUT/locks.txt" 2>&1
CREATE TEMP TABLE lock_samples(at timestamptz, key bigint, holders int);
CREATE TEMP TABLE lock_ticks(at timestamptz);
DO \$\$
DECLARE deadline timestamptz := clock_timestamp() + interval '${poll_seconds} seconds';
BEGIN
  WHILE clock_timestamp() < deadline LOOP
    INSERT INTO lock_ticks VALUES (clock_timestamp());
    INSERT INTO lock_samples(at, key, holders)
    SELECT clock_timestamp(), k.key, k.holders
      FROM (
        SELECT ((l.classid::bigint << 32) | l.objid::bigint) AS key,
               count(DISTINCT l.pid) AS holders
          FROM pg_locks l
         WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
         GROUP BY 1
      ) k;
    PERFORM pg_sleep(${POLL});
  END LOOP;
END \$\$;
\echo '--- samples taken ---'
SELECT count(*) FROM lock_ticks;
\echo '--- advisory keys seen held, and the most holders any sample found ---'
SELECT to_hex(key) AS key_hex, count(*) AS samples_held, max(holders) AS max_holders
  FROM lock_samples GROUP BY key ORDER BY key;
\echo '--- samples with more than one holder of one key (the two-leader window) ---'
SELECT at, to_hex(key) AS key_hex, holders FROM lock_samples WHERE holders > 1 ORDER BY at;
SQL
} &
POLL_PID=$!

echo
echo "== k6 (cached, ${RATE} rps, ${DURATION}, through the load balancer) =="
K6_CONTAINER="lc-k6-$$"
docker run -d --name "$K6_CONTAINER" --network "$NETWORK" \
  -v "${REPO_ROOT}/test/load:/scripts:ro" \
  -e "BASE=http://haproxy:8080" -e "PHASE=slo" -e "MODE=cached" \
  -e "RATE=${RATE}" -e "DURATION=${DURATION}" \
  -e "PREFIX=${PREFIX}" -e "TOTAL=${links}" \
  grafana/k6 run /scripts/redirect.js >/dev/null

# Long enough for the arrival-rate executor to reach its rate, so the first
# replica does not go away during the ramp.
sleep 15

## ---- the rolling replacement ------------------------------------------------

echo
echo "== rolling ${MODE}, one replica at a time =="
roll_start=$(date +%s)
for r in "${REPLICAS[@]}"; do
  step_start=$(date +%s)
  # This replica's share of the window, read before it stops existing.
  scrape "$r" "$OUT/$r-pre.txt"
  if [ "$MODE" = kill ]; then
    # SIGKILL. No SIGTERM, so no drain, no readiness change, and the listener
    # simply stops answering — the load balancer finds out by failing.
    compose kill -s KILL "$r" >/dev/null
    compose up -d --force-recreate --no-deps --wait "$r" >/dev/null
  else
    compose up -d --force-recreate --no-deps --wait "$r" >/dev/null
  fi
  # `--wait` says the container is healthy; the load balancer deciding to route
  # to it again is a separate fact and is the one that matters here.
  for _ in $(seq 1 60); do
    curl_ -fsS "http://haproxy:8404/stats;csv" > "$OUT/haproxy-now.csv"
    if awk -F, -v s="$r" 'NR>1 && $1=="app" && $2==s && $18 ~ /^UP/ {found=1} END {exit !found}' \
        "$OUT/haproxy-now.csv"; then
      break
    fi
    sleep 1
  done
  echo "  $r replaced and back in rotation in $(( $(date +%s) - step_start ))s"
done
roll_seconds=$(( $(date +%s) - roll_start ))
echo "whole rolling ${MODE}: ${roll_seconds}s"

## ---- wait for the run, then read everything ---------------------------------

echo
echo "== generator =="
k6_status=0
docker wait "$K6_CONTAINER" >/dev/null
docker logs "$K6_CONTAINER" 2>&1 | tee "$OUT/k6.txt" | tail -40
k6_status=$(docker inspect -f '{{.State.ExitCode}}' "$K6_CONTAINER")

for r in "${REPLICAS[@]}"; do scrape "$r" "$OUT/$r-end.txt"; done
curl_ -fsS "http://haproxy:8404/stats;csv" > "$OUT/haproxy-after.csv"

echo
echo "== server-side histogram, summed across every process that served the window =="
total=$(summed "+Inf")
if [ "$total" -le 0 ]; then
  echo "no cached redirects recorded; nothing to report"
else
  printf 'cached redirects served: %d\n' "$total"
  for le in 0.0005 0.001 0.0025 0.005 0.01 0.02; do
    n=$(summed "$le")
    printf '  under %7ss: %8d  %6.3f%%\n' "$le" "$n" \
      "$(awk -v n="$n" -v t="$total" 'BEGIN { printf "%.3f", (n / t) * 100 }')"
  done
  echo
  echo "The 0.02 line is the SLO: cached p99 under 20ms, held during the replacement."
fi

echo
echo "== cache mix for this run =="
for tier in memory redis database negative; do
  mix=0
  for r in "${REPLICAS[@]}"; do
    mix=$(( mix
      + $(bucket "$OUT/$r-pre.txt" "$tier" "+Inf") - $(bucket "$OUT/$r-start.txt" "$tier" "+Inf")
      + $(bucket "$OUT/$r-end.txt" "$tier" "+Inf") ))
  done
  printf '  %-9s %8d\n' "$tier" "$mix"
done

echo
echo "== load balancer, this run only =="
printf '  requests through the backend       %8d\n' "$(delta stot)"
printf '  requests FAILED (5xx from the LB)  %8d\n' "$(delta hrsp_5xx)"
printf '  requests RETRIED (wretr)           %8d\n' "$(delta wretr)"
printf '  retries sent elsewhere (wredis)    %8d\n' "$(delta wredis)"
printf '  connection errors (econ)           %8d\n' "$(delta econ)"
printf '  response errors (eresp)            %8d\n' "$(delta eresp)"
printf '  client aborts (cli_abrt)           %8d\n' "$(delta cli_abrt)"

echo
echo "== two leaders, sampled every ${POLL}s for the length of the run =="
wait "$POLL_PID" 2>/dev/null || true
POLL_PID=""
cat "$OUT/locks.txt"
echo
echo "A key with max_holders = 1 was never held by two sessions at once in any"
echo "sample. That is evidence bounded by the sampling interval above, and it is"
echo "not the whole argument: from 0.2.0 on every binary takes the same"
echo "per-family keys, so pg_try_advisory_lock is what excludes the second"
echo "leader — see docs/build-notes/decisions.md for M57."

## ---- give the instance back -------------------------------------------------

# The harness borrowed the instance's Postgres and Redis and stopped its `app`.
# Leaving it that way is a trap for whatever runs next: `make test-integration`
# and `make load` both expect one app container, and three replicas plus a
# balancer against the same database is not the shape anything else here
# assumes. The artefacts below outlive the teardown, which is the part worth
# keeping.
echo
echo "== putting the instance back the way it was =="
compose rm -sf "${REPLICAS[@]}" haproxy >/dev/null 2>&1 || true
docker compose -p "$PROJECT" --env-file "$ENV_FILE" up -d --wait app >/dev/null \
  || echo "note: the single-replica app did not come back up; 'make up' will start it"

echo
echo "artefacts: $OUT"
exit "$k6_status"
