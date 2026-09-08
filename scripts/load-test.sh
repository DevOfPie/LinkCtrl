#!/usr/bin/env bash
#
# Runs the redirect load test and reports both halves of the SLO measurement:
# what the generator saw, and what the server's own histogram recorded.
#
# The server histogram is cumulative since boot, so it is sampled before and
# after and reported as a delta. Without that, a measurement includes the warm-up,
# every earlier run, and whatever traffic the container has served since it
# started — which is how a load test comes to report a number nobody can
# reproduce.
#
# Usage: scripts/load-test.sh [cached|uncached] [rate] [duration]
set -euo pipefail

MODE="${1:-cached}"
RATE="${2:-2000}"
DURATION="${3:-2m}"

# The compose network is named after the project, and the development instances
# are separate projects (docs/dev-notes/instances.md). Every `docker compose`
# call below reads COMPOSE_PROJECT_NAME and COMPOSE_ENV_FILES from the
# environment on its own; the network has to be derived from the same place, or
# the load generator joins one instance's network and measures the other's app.
NETWORK="${LINKCTRL_NETWORK:-${COMPOSE_PROJECT_NAME:-linkctrl}_default}"
APP="${LINKCTRL_APP_HOST:-app}"
PREFIX="${LINKCTRL_SEED_PREFIX:-ld}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${TMPDIR:-/tmp}"

# Git Bash rewrites arguments that look like Unix paths before docker sees them,
# which turns a bind mount into nonsense and /scripts into C:/Program Files/...
# cygpath gives the form the daemon understands; both are no-ops elsewhere.
export MSYS_NO_PATHCONV=1
if command -v cygpath >/dev/null 2>&1; then
  REPO_ROOT="$(cygpath -m "$REPO_ROOT")"
fi

case "$MODE" in
  cached|uncached) ;;
  *) echo "mode must be cached or uncached, got $MODE" >&2; exit 2 ;;
esac

die() { echo "load-test: $*" >&2; exit 1; }

command -v docker >/dev/null || die "docker is required"

# The stack has to be up, and it has to be the code under test. A load result from
# a stale image is worse than no result.
running=$(docker compose ps --status running --services 2>/dev/null || true)
grep -qx app <<<"$running" \
  || die "the app service is not running; start it with 'docker compose up -d --wait'"

echo "== dataset =="
links=$(docker compose exec -T postgres psql -U "${POSTGRES_USER:-linkctrl}" \
  -d "${POSTGRES_DB:-linkctrl}" -tAc \
  "SELECT count(*) FROM links WHERE alias LIKE '${PREFIX}%' AND deleted_at IS NULL")
clicks=$(docker compose exec -T postgres psql -U "${POSTGRES_USER:-linkctrl}" \
  -d "${POSTGRES_DB:-linkctrl}" -tAc "SELECT count(*) FROM click_events")
echo "seeded links:  ${links}"
echo "click events:  ${clicks}"
[ "$links" -gt 0 ] || die "no seeded links found; run 'lctl seed' first (see docs/slo.md)"

# scrape writes the metrics text to $1. Runs inside the compose network because
# the metrics port is deliberately not published.
scrape() {
  docker run --rm --network "$NETWORK" curlimages/curl:latest \
    -fsS "http://${APP}:9090/metrics" > "$1"
}

# bucket FILE CACHE LE — the cumulative count in one bucket of the redirect
# histogram, for successful redirects served from one cache tier.
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

# k6 PHASE [k6 flags...] — one invocation of the generator. Extra arguments are
# k6 flags and go after `run`, never before the image name: an earlier version
# put them among the docker arguments, where docker consumed --quiet itself.
k6() {
  local phase="$1"; shift
  docker run --rm -i --network "$NETWORK" \
    -v "${REPO_ROOT}/test/load:/scripts:ro" \
    -e "BASE=http://${APP}:8080" \
    -e "PHASE=${phase}" \
    -e "MODE=${MODE}" -e "RATE=${RATE}" -e "DURATION=${DURATION}" \
    -e "PREFIX=${PREFIX}" -e "TOTAL=${links}" \
    -e "SUFFIX=${SUFFIX:-}" \
    grafana/k6 run "$@" /scripts/redirect.js
}

# Warm-up is its own invocation, and the snapshot is taken after it. The server's
# histogram is cumulative, so a snapshot taken before the warm-up would fold every
# cold read into the measurement — which is how "cached p99" comes to include a
# few thousand database queries.
if [ "$MODE" = cached ]; then
  echo
  echo "== warm-up (populating the cache tiers) =="
  k6 warm --quiet >/dev/null || die "warm-up failed"
  echo "cache populated"
fi

echo
echo "== before =="
scrape "$OUT/lc-before.txt"
echo "histogram snapshot taken"

echo
echo "== k6 (${MODE}, ${RATE} rps, ${DURATION}) =="
# `k6 slo; k6_status=$?` would never reach the assignment: k6 exits 99 when a
# threshold fails, and set -e aborts first — which is precisely the run whose
# server-side numbers are worth reading.
k6_status=0
k6 slo || k6_status=$?

echo
echo "== after =="
scrape "$OUT/lc-after.txt"

echo
echo "== server-side histogram, this run only =="
total_before=$(cached_bucket "$OUT/lc-before.txt" "+Inf")
total_after=$(cached_bucket "$OUT/lc-after.txt" "+Inf")
total=$(( total_after - total_before ))

if [ "$total" -le 0 ]; then
  echo "no cached redirects recorded; nothing to report"
  exit "$k6_status"
fi

printf 'cached redirects served: %d\n' "$total"
for le in 0.0005 0.001 0.0025 0.005 0.01 0.02; do
  before=$(cached_bucket "$OUT/lc-before.txt" "$le")
  after=$(cached_bucket "$OUT/lc-after.txt" "$le")
  n=$(( after - before ))
  printf '  under %7ss: %8d  %6.3f%%\n' "$le" "$n" \
    "$(awk -v n="$n" -v t="$total" 'BEGIN { printf "%.3f", (n / t) * 100 }')"
done

echo
echo "The 0.02 line is the SLO: the target is stated as a fraction of cached"
echo "redirects under 20ms, which is why the histogram has a boundary there."

# Cache mix, because the ratio above is only meaningful if these really were hits.
echo
echo "== cache mix for this run =="
for tier in memory redis database negative; do
  b=$(bucket "$OUT/lc-before.txt" "$tier" "+Inf")
  a=$(bucket "$OUT/lc-after.txt" "$tier" "+Inf")
  printf '  %-9s %8d\n' "$tier" "$(( a - b ))"
done

# What the SLO number depends on but does not show. A starved redirect pool is the
# difference between "the query is slow" and "the request never got a connection",
# and only one of those is fixed by tuning the query.
echo
echo "== redirect pool during this run =="
poolstat() {
  awk -v m="$2" '$0 ~ "^" m "\\{pool=\"redirect\"\\}" { print $2 }' "$1"
}
for m in linkctrl_db_pool_acquire_waits_total linkctrl_db_pool_acquire_wait_seconds_total; do
  b=$(poolstat "$OUT/lc-before.txt" "$m"); a=$(poolstat "$OUT/lc-after.txt" "$m")
  printf '  %-42s %s\n' "${m#linkctrl_db_pool_}" \
    "$(awk -v a="${a:-0}" -v b="${b:-0}" 'BEGIN { printf "%.3f", a - b }')"
done

exit "$k6_status"
