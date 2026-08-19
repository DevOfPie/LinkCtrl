#!/usr/bin/env bash
#
# One container, one Postgres, nothing else — and the whole product still works.
#
# This is the owner's constraint of 2026-08-06 turned into a gate: **high
# availability must not come at the cost of single-instance installs.** M56 and
# M57 wrote a failover contract, a load-balancer contract and a rolling-deploy
# measurement, and every one of them describes a deployment almost nobody runs.
# What almost everybody runs is this: a container and a database. If a later
# milestone quietly makes Redis, a load balancer or a second replica
# load-bearing, this fails, and that is the entire point of writing it before
# anything does.
#
# ## What it actually asserts
#
# The image is started on a network that has **only** Postgres on it. There is
# no Redis container, and the name `redis` does not resolve. Then the surfaces
# m57.md names are exercised in turn — redirect, dashboard, API, jobs,
# invalidation, rate limiting — against the running container over HTTP, not
# against a test harness that wires the same packages together differently.
#
# ## When this blocks your milestone
#
# It will, eventually, and at a bad moment: that is what it is for. This is the
# kind of check that passes for years and then stands in front of somebody's
# unrelated work with a message about a dependency they did not think they had
# added. **When that happens, the answer is to narrow what this covers, in
# writing, with the reasoning recorded — never to delete it.** Exactly the rule
# `demoCoverage()` in cmd/lctl carries, for exactly the same reason: an
# obligation nobody enforces is a sentence in a document, and the constraint
# this one holds is the owner's, not the loop's, so relaxing it is a
# conversation rather than an edit.
#
# It is deliberately behavioural rather than structural. A list of "required
# dependencies" kept in a file is a list somebody updates; a product that boots
# with nothing but Postgres reachable is a fact. The failure a structural check
# would miss is precisely the one worth catching: a dependency introduced not by
# configuration but by some code path that now assumes a client is non-nil.
#
# ## The second limb
#
# Redis *absent* is one shape. Redis *configured and unreachable* is the other,
# and it is the one an operator hits when their cache falls over rather than
# when they chose not to run one. Both are exercised, because the documented
# degradation covers both and they take different branches: `CACHE_ENABLED=false`
# never builds a client, while an unreachable URL builds one and fails every
# call through it.
#
# ## The third limb: an add-on is loaded (M60)
#
# The WASM host is the reason the add-on answer went to modules rather than to
# sidecars — a sidecar model would have made *two* containers the tested shape
# and quietly retired the constraint this file exists to hold. So the claim owes
# a case: one container, one Postgres, and a real `.wasm` instantiated inside the
# process, still serving everything above.
#
# It is owed work #5 from docs/build-notes/phase-4-candidates.md, and the reason
# it is written here rather than left to the unit tests is the reason the whole
# file is behavioural: the host is exercised over HTTP against the shipped image,
# where a dependency introduced by a code path rather than by configuration is
# the failure that a structural check would miss.
#
# The module is a fixture, built by `make addon-fixtures` and passed in, because
# nothing in this repository ships an add-on and a checked-in binary is refused
# (m60.md). The Makefile target is what supplies it.
#
# **This limb is skipped in two cases, and the other two still run in both.**
# The whole point of this script is that an operator can point it at a published
# image — `ghcr.io/devofpie/linkctrl:0.3.0` is what closed F257 — and that
# invocation has one argument, no repository checkout, and nothing built. Both
# skips exist to keep it, and neither hides a defect:
#
#   1. **No module to load**, or no `sha256sum` to write its digest with. Decided
#      before either container boots, so a run that will skip says so at once.
#   2. **An image older than the add-on host.** Decided from the image's own
#      `linkctrl_build_info` version at the limb itself, because that is the
#      first moment the image can be asked. Below `ADDON_HOST_SINCE` there is
#      nothing to assert; at or above it, every assertion runs. Only a bare
#      `major.minor.patch` below the floor skips; `ci`, `dev`, a prerelease, a
#      `git describe` tail and no series at all every one assert (D223).
#
# `make single-instance` builds the fixture and drives all three against a
# freshly built image, so the gate itself is unchanged.
#
# Usage: scripts/single-instance-check.sh [image] [path/to/module.wasm]
set -euo pipefail

IMAGE="${1:-linkctrl:ci}"
ADDON_WASM="${2:-internal/addon/testdata/build/minimal.wasm}"

NET="lc-single-$$"
PG="lc-single-pg-$$"
APP="lc-single-app-$$"
PGPASS="single-instance-check"
DSN="postgres://linkctrl:${PGPASS}@${PG}:5432/linkctrl?sslmode=disable"
PEPPER="c2luZ2xlLWluc3RhbmNlLWNoZWNrLXBlcHBlci1ub3QtYS1zZWNyZXQtMDAwMDAw"
EMAIL="single@example.com"
PASSWORD="a-long-passphrase-for-the-check"

fail() { echo "single-instance: $*" >&2; exit 1; }
step() { echo; echo "-- $*"; }

# shellcheck disable=SC2329,SC2317  # invoked by the trap below, not by name (both codes: see rolling-deploy.sh)
cleanup() {
  docker logs "$APP" > "${TMPDIR:-/tmp}/${APP}.log" 2>&1 || true
  docker rm -f "$APP" "$PG" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  [ -n "${ADDON_DIR:-}" ] && rm -rf "$ADDON_DIR"
  return 0
}
trap cleanup EXIT

command -v docker >/dev/null || fail "docker is required"
command -v curl >/dev/null || fail "curl is required"

# Decided here rather than at the third limb, so a run that will skip it says so
# in the first two seconds instead of after two container boots. Both
# prerequisites are the add-on limb's alone, which is why neither is a `fail`:
# sha256sum is needed only to write the manifest's digest, and the module only to
# have something to write it about.
ADDON_SKIP=""
if [ ! -f "$ADDON_WASM" ]; then
  ADDON_SKIP="no module at $ADDON_WASM (build one: make addon-fixtures)"
elif ! command -v sha256sum >/dev/null; then
  ADDON_SKIP="sha256sum is not installed, and the add-on manifest carries a digest"
fi
[ -z "$ADDON_SKIP" ] || echo "note: the add-on limb will be skipped — $ADDON_SKIP"

docker image inspect "$IMAGE" >/dev/null 2>&1 \
  || fail "no such image: $IMAGE (build it first, e.g. make docker-build)"

echo "== one container, no Redis, no load balancer =="
echo "image: $IMAGE"

docker network create "$NET" >/dev/null

# Postgres, and nothing else on this network. Deliberately not published: only
# the container under test may reach it, so nothing about this run depends on
# the host's own stack.
docker run -d --name "$PG" --network "$NET" \
  -e POSTGRES_USER=linkctrl -e POSTGRES_PASSWORD="$PGPASS" -e POSTGRES_DB=linkctrl \
  -e TZ=UTC postgres:17-alpine -c timezone=UTC >/dev/null

# **Wait on the socket the application will actually use** (F256).
#
# This waited for `pg_isready` over the container's **unix socket**, broke on the
# first yes, and then re-ran it once as a confirmation. Those are two different
# servers. `postgres:17-alpine` runs `initdb` against a *temporary* server on
# first boot, and that server answers on the socket, is shut down, and the real
# one starts behind it — so the loop could break on the temporary server and the
# confirming shot could land in the window where nothing was listening. The gate
# then failed and passed on the same commit twice, in opposite directions,
# decided only by where the one-second cadence fell.
#
# **The entrypoint starts that temporary server with `listen_addresses=""`**, so
# it has no TCP listener at all. Asking over TCP therefore cannot see it, and the
# boundary stops being a race: measured by polling both from container start, the
# socket went `........RR...RRRR` while TCP went `............RRRRR` — **zero**
# ready-to-not-ready transitions, first yes four polls later.
#
# That is also the honest question to ask. The container under test reaches
# Postgres across a docker network, so TCP readiness is the thing this check
# depends on and the socket never was.
#
# The streak is belt, not the mechanism, and it is cheap: a future image that
# published TCP during `initdb` would put the race back, and three successes at
# this cadence span more than two seconds against a window measured in fractions
# of one. There is deliberately **no check after the loop** — a single shot
# outside it is exactly the defect being removed.
ready=0
for _ in $(seq 1 60); do
  if docker exec "$PG" pg_isready -h 127.0.0.1 -U linkctrl -d linkctrl >/dev/null 2>&1; then
    ready=$((ready + 1))
    if [ "$ready" -ge 3 ]; then break; fi
  else
    ready=0
  fi
  sleep 1
done
[ "$ready" -ge 3 ] || fail "Postgres never became ready"

# start_app EXTRA_ENV... — the container under test, on an ephemeral host port.
#
# The port is published so curl can drive the product the way a person would.
# That is a different bargain from docs/slo.md's — a published port is wrong for
# *latency* and irrelevant for *does this work at all*, which is what is being
# asked here.
start_app() {
  docker rm -f "$APP" >/dev/null 2>&1 || true
  docker run -d --name "$APP" --network "$NET" \
    -p 127.0.0.1:0:8080 -p 127.0.0.1:0:9090 \
    -e TZ=UTC \
    -e LINKCTRL_APP_ENV=development \
    -e LINKCTRL_SECURE_COOKIES=false \
    -e LINKCTRL_HTTP_ADDR=":8080" \
    -e LINKCTRL_METRICS_ADDR=":9090" \
    -e LINKCTRL_DATABASE_URL="$DSN" \
    -e LINKCTRL_API_KEY_PEPPER="$PEPPER" \
    -e LINKCTRL_MIGRATE_ON_START=true \
    -e LINKCTRL_UPDATE_CHECK=false \
    -e LINKCTRL_LOG_LEVEL=info \
    "$@" \
    "$IMAGE" >/dev/null
  BASE="http://$(docker port "$APP" 8080/tcp | head -1)"
  METRICS="http://$(docker port "$APP" 9090/tcp | head -1)"
  export BASE METRICS
}

wait_serving() {
  for _ in $(seq 1 90); do
    if curl -fsS -o /dev/null "${BASE}/healthz" 2>/dev/null; then return 0; fi
    sleep 1
  done
  docker logs "$APP" 2>&1 | tail -30 >&2
  fail "the container never answered /healthz"
}

# code METHOD URL [curl args...] — the status line and nothing else.
code() {
  local method="$1" url="$2"; shift 2
  curl -sS -o /dev/null -w '%{http_code}' -X "$method" "$url" "$@"
}

## ---- limb one: no Redis at all ---------------------------------------------

step "boot with LINKCTRL_CACHE_ENABLED=false, on a network with no Redis on it"
start_app -e LINKCTRL_BASE_URL="http://localhost:8080" -e LINKCTRL_CACHE_ENABLED=false
wait_serving
echo "serving at $BASE"

# The name is not merely unset — it does not exist. Anything that had quietly
# come to require a cache would fail here rather than degrade.
if docker exec "$PG" getent hosts redis >/dev/null 2>&1; then
  fail "something called 'redis' resolves on this network; the run would prove nothing"
fi

step "health: liveness never depends on a dependency, readiness says which are there"
[ "$(code GET "${BASE}/healthz")" = 200 ] || fail "/healthz did not answer 200"
ready=$(curl -sS -o /tmp/lc-ready.$$ -w '%{http_code}' "${BASE}/readyz")
body=$(cat /tmp/lc-ready.$$); rm -f /tmp/lc-ready.$$
[ "$ready" = 200 ] || fail "/readyz answered $ready with Postgres up and no Redis; want 200 — a 503 here means a cache became load-bearing: $body"
echo "$body" | grep -q '"redis": *"disabled"' \
  || fail "readyz does not report redis as disabled: $body"
echo "$body" | grep -q '"status": *"ok"' \
  || fail "readyz is not 'ok' on a single instance with no Redis; that is a new required dependency: $body"
echo "readyz: ok, redis disabled"

step "API: claim the instance, then issue a key and use it"
setup=$(curl -sS -X POST "${BASE}/api/v1/auth/setup" -H 'Content-Type: application/json' \
  -d "{\"email\":\"${EMAIL}\",\"name\":\"Single\",\"password\":\"${PASSWORD}\",\"update_check\":false}")
echo "$setup" | grep -q '"user_id"' || fail "could not claim the instance: $setup"

# lctl out of the same image, against the same database. It is the documented
# bootstrap path and it is the second binary the image ships.
KEY=$(docker run --rm --network "$NET" --entrypoint /lctl \
  -e LINKCTRL_DATABASE_URL="$DSN" -e LINKCTRL_API_KEY_PEPPER="$PEPPER" \
  -e LINKCTRL_BASE_URL="http://localhost:8080" -e LINKCTRL_APP_ENV=development \
  -e LINKCTRL_CACHE_ENABLED=false \
  "$IMAGE" apikey create --user "$EMAIL" --name conformance \
  --scopes links.read,links.create,links.update | tail -1)
case "$KEY" in
  lk_*) ;;
  *) fail "lctl did not mint a key from inside the image: '$KEY'" ;;
esac

created=$(curl -sS -X POST "${BASE}/api/v1/links" -H "Authorization: Bearer ${KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"alias":"conform","url":"https://example.com/one"}')
echo "$created" | grep -q '"alias":"conform"' || fail "the API did not create a link: $created"
LINK_ID=$(echo "$created" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$LINK_ID" ] || fail "no link id in $created"
echo "API: key issued, link created"

step "redirect: the hot path, with no cache tier behind it but the in-process one"
loc=$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' "${BASE}/conform")
[ "${loc% *}" = 302 ] || fail "GET /conform answered ${loc}, want 302"
[ "${loc#* }" = "https://example.com/one" ] || fail "wrong destination: ${loc}"
# Twice, so the second one is served from the in-process tier — the tier that is
# *not* optional and never was.
curl -sS -o /dev/null "${BASE}/conform"
echo "redirect: 302 to the destination, twice"

step "invalidation: an edit reaches the cache with no pub/sub to carry it"
# With no Redis there is no channel and no subscriber. On one replica there is
# also nothing to tell: the process that wrote the edit is the process holding
# the cache. That is the documented degradation, and this is the assertion that
# it degrades to *correct* rather than to *stale*.
edited=$(curl -sS -X PATCH "${BASE}/api/v1/links/${LINK_ID}" \
  -H "Authorization: Bearer ${KEY}" -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/two"}')
echo "$edited" | grep -q 'example.com/two' || fail "the API did not edit the link: $edited"
after=$(curl -sS -o /dev/null -w '%{redirect_url}' "${BASE}/conform")
[ "$after" = "https://example.com/two" ] \
  || fail "the redirect still serves '$after' after an edit; invalidation needs Redis, which is a new required dependency"
echo "invalidation: the edit is served immediately"

step "dashboard: the HTML surface, sessions and all"
[ "$(code GET "${BASE}/login")" = 200 ] || fail "the login page did not render"
jar="${TMPDIR:-/tmp}/lc-jar-$$"; rm -f "$jar"
# No CSRF token to fetch: protection here is origin-based
# (`http.NewCrossOriginProtection`, internal/httpx/router.go), so a client that
# sends neither `Origin` nor `Sec-Fetch-Site` — which curl is — is not a browser
# being made to submit a form and is allowed through.
signin=$(curl -sS -b "$jar" -c "$jar" -o /dev/null -w '%{http_code}' -X POST "${BASE}/login" \
  --data-urlencode "email=${EMAIL}" --data-urlencode "password=${PASSWORD}")
case "$signin" in 200|303|302) ;; *) fail "signing in answered $signin" ;; esac
dash=$(curl -sS -b "$jar" -o /dev/null -w '%{http_code}' "${BASE}/dashboard")
[ "$dash" = 200 ] || fail "the dashboard answered $dash to a signed-in session"
rm -f "$jar"
echo "dashboard: login form, session, dashboard"

step "jobs: the scheduler is in this process and it ran"
# Every family runs once at startup. job_state is written by the leader, and on
# one container the leader is the only candidate — which is the single-instance
# half of the advisory-lock design, asserted rather than assumed.
ok=""
for _ in $(seq 1 60); do
  if curl -fsS "${METRICS}/metrics" 2>/dev/null \
      | grep -q '^linkctrl_job_last_success_timestamp_seconds{'; then ok=1; break; fi
  sleep 1
done
[ -n "$ok" ] || fail "no job reported a success; the scheduler is not running on a single instance"
jobs=$(curl -fsS "${METRICS}/metrics" | grep -c '^linkctrl_job_last_success_timestamp_seconds{')
echo "jobs: ${jobs} job(s) have reported a success"

step "rate limiting: in memory, because the shared limiter's backing store is gone"
# LOGIN_RATE_PER_MIN is 10 by default and the credential limiter falls back to
# per-process buckets without Redis. A single instance therefore still refuses a
# password-guessing run, which is the security property the fallback exists to
# keep rather than a nicety.
limited=""
for _ in $(seq 1 20); do
  rc=$(code POST "${BASE}/api/v1/auth/login" -H 'Content-Type: application/json' \
    -d "{\"email\":\"${EMAIL}\",\"password\":\"wrong-on-purpose\"}")
  if [ "$rc" = 429 ]; then limited=1; break; fi
done
[ -n "$limited" ] || fail "twenty wrong passwords were never rate limited; the limiter needs Redis, which is a new required dependency"
echo "rate limiting: 429 without Redis"

## ---- limb two: Redis configured, and not there ------------------------------

step "boot again with a Redis URL that resolves to nothing"
# The other shape: an operator who has a cache and lost it, rather than one who
# never had one. A client is built, every call through it fails, and the product
# is required to serve anyway — 200 and the word `degraded`, which is the
# distinction the whole load-balancer contract is built on.
start_app -e LINKCTRL_BASE_URL="http://localhost:8080" \
  -e LINKCTRL_REDIS_URL="redis://no-such-redis:6379/0" \
  -e LINKCTRL_REDIS_DIAL_TIMEOUT=200ms
wait_serving

ready=$(curl -sS -o /tmp/lc-ready2.$$ -w '%{http_code}' "${BASE}/readyz")
body=$(cat /tmp/lc-ready2.$$); rm -f /tmp/lc-ready2.$$
[ "$ready" = 200 ] \
  || fail "/readyz answered $ready with only Redis gone; 503 removes every replica from rotation over a cache problem: $body"
echo "$body" | grep -q '"status": *"degraded"' \
  || fail "readyz is not 'degraded' with Redis unreachable: $body"
loc=$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' "${BASE}/conform")
[ "${loc% *}" = 302 ] || fail "a redirect answered ${loc} with Redis unreachable"
echo "degraded: 200, and redirects still resolve"

## ---- limb three: one container, with an add-on loaded -----------------------

if [ -n "$ADDON_SKIP" ]; then
  step "skipped: one container with an add-on loaded"
  echo "$ADDON_SKIP"
  echo "the two limbs above ran; this one needs a module to load and had none."
  echo
  echo "== a single container remains a supported, tested configuration =="
  echo "Postgres is the only thing it had to reach. The add-on limb did not run."
  exit 0
fi

step "stage an add-on: a directory, a manifest, and the module it describes"
# The layout an operator builds by hand and the Add-on manager will build for
# them at M67: one directory per add-on, named for the add-on, holding
# addon.json and the .wasm the manifest's digest describes.
ADDON_DIR=$(mktemp -d -t lc-addons-XXXXXX)
mkdir -p "$ADDON_DIR/minimal"
cp "$ADDON_WASM" "$ADDON_DIR/minimal/minimal.wasm"
DIGEST=$(sha256sum "$ADDON_DIR/minimal/minimal.wasm" | cut -d' ' -f1)
cat > "$ADDON_DIR/minimal/addon.json" <<JSON
{
  "schema_version": 1,
  "name": "minimal",
  "version": "1.0.0",
  "abi_version": 1,
  "module": "minimal.wasm",
  "sha256": "$DIGEST",
  "failure_class": "required",
  "settings": [
    {"name": "greeting", "type": "text", "default": "hello"}
  ]
}
JSON
# The image runs as nonroot, so the mount has to be readable by somebody who is
# not the user that created the temporary directory.
chmod -R a+rX "$ADDON_DIR"
echo "staged minimal v1.0.0 (sha256 ${DIGEST:0:12}…), failure_class=required"

step "boot with the add-on mounted read-only, still on Postgres alone"
# Read-only, which is what docs/configuration.md tells an operator to do: a
# module in this directory is code the instance executes, so the process that
# executes it has no business being able to rewrite it.
#
# failure_class=required is deliberate here. If the host cannot verify and
# instantiate this module the container does not come up at all, so
# `wait_serving` below is itself the assertion — a silent skip is not available.
start_app -e LINKCTRL_BASE_URL="http://localhost:8080" \
  -e LINKCTRL_CACHE_ENABLED=false \
  -e LINKCTRL_ADDONS_DIR=/addons \
  -v "$ADDON_DIR:/addons:ro"
wait_serving

step "the host says what it loaded, per module, on the metrics listener"
scrape=$(curl -fsS "${METRICS}/metrics")

# **Does this image have an add-on host at all?** Asked of the image itself,
# before anything is asserted about it.
#
# A published release older than the host has no `linkctrl_addon_` series to
# find, ignores LINKCTRL_ADDONS_DIR as the unknown variable it is, and is
# otherwise perfectly conformant — so asserting here would fail a good artifact,
# which is the one direction this whole file exists to avoid. The answer is in
# the scrape just taken: `linkctrl_build_info` carries the version, on this same
# listener, published by every version that has ever had a metrics listener.
#
# **The predicate fails closed, and only a bare release skips** (D223). The
# version must be exactly `major.minor.patch` once the tag's leading `v` is off,
# and below the floor. Everything else asserts: `ci`, `dev`, `dev-dirty`, an
# empty label, a missing series, a prerelease such as `0.4.0-rc1`, and a
# `git describe` tail such as `0.3.0-14-g<sha>-dirty` — which is what
# `make docker-build` stamps, and is a build *after* that tag rather than that
# tag. The last is why no tail is stripped: reducing it to the triple skipped
# against an image that had the host, silently. The accepted cost is one narrow
# false red, the gate run against an old *prerelease* image, taken because a
# false red is visible and a false skip is not. A current image whose host is
# broken is exactly what limb three is for and still fails.
ADDON_HOST_SINCE=0.4.0

# $1 < $2, both `major.minor.patch` and digits only. 10# because a version
# component may be written 08, which arithmetic would otherwise read as octal.
version_below() {
  local -a have want
  IFS=. read -r -a have <<<"$1"
  IFS=. read -r -a want <<<"$2"
  local i
  for i in 0 1 2; do
    if [ "$((10#${have[i]}))" -lt "$((10#${want[i]}))" ]; then return 0; fi
    if [ "$((10#${have[i]}))" -gt "$((10#${want[i]}))" ]; then return 1; fi
  done
  return 1
}

IMAGE_VERSION=$(printf '%s\n' "$scrape" \
  | sed -n 's/^linkctrl_build_info{.*version="\([^"]*\)".*$/\1/p' | head -n 1)
# Only the `v` the release workflow's tag carries comes off. Nothing else: a
# tail means the image is not that release (D223).
core=${IMAGE_VERSION#v}
if printf '%s' "$core" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' \
    && version_below "$core" "$ADDON_HOST_SINCE"; then
  step "skipped: one container with an add-on loaded"
  echo "$IMAGE reports version ${IMAGE_VERSION}, and the add-on host arrives in ${ADDON_HOST_SINCE}."
  echo "the two limbs above ran against it; there is no host here for a module to load into."
  echo
  echo "== a single container remains a supported, tested configuration =="
  echo "Postgres is the only thing it had to reach. The add-on limb did not run."
  exit 0
fi

echo "$scrape" | grep -q '^linkctrl_addon_loads_total{addon="minimal",outcome="loaded"} 1$' \
  || fail "the add-on did not load, and version ${IMAGE_VERSION} does not predate the add-on host (${ADDON_HOST_SINCE}): $(echo "$scrape" | grep '^linkctrl_addon_' || echo 'no linkctrl_addon_ series at all')"
echo "$scrape" | grep -q '^linkctrl_addon_info{.*addon="minimal".*} 1$' \
  || fail "no identity series for the loaded add-on: $(echo "$scrape" | grep '^linkctrl_addon_info' || echo none)"
echo "metrics: minimal loaded, identity published"

step "and the product is unchanged: redirect, dashboard, API, all with a module in the process"
loc=$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' "${BASE}/conform")
[ "${loc% *}" = 302 ] || fail "a redirect answered ${loc} with an add-on loaded"
[ "$(code GET "${BASE}/login")" = 200 ] || fail "the login page did not render with an add-on loaded"
[ "$(code GET "${BASE}/api/v1/links" -H "Authorization: Bearer ${KEY}")" = 200 ] \
  || fail "the API did not answer with an add-on loaded"
ready=$(curl -sS -o /tmp/lc-ready3.$$ -w '%{http_code}' "${BASE}/readyz")
body=$(cat /tmp/lc-ready3.$$); rm -f /tmp/lc-ready3.$$
[ "$ready" = 200 ] || fail "/readyz answered $ready with an add-on loaded: $body"
echo "one container, one Postgres, one add-on: everything still answers"

echo
echo "== a single container remains a supported, tested configuration =="
echo "Postgres is the only thing it had to reach, with a WASM module in the process."
