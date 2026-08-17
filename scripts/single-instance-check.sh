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
# Usage: scripts/single-instance-check.sh [image]
set -euo pipefail

IMAGE="${1:-linkctrl:ci}"

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
  return 0
}
trap cleanup EXIT

command -v docker >/dev/null || fail "docker is required"
command -v curl >/dev/null || fail "curl is required"

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

for _ in $(seq 1 60); do
  if docker exec "$PG" pg_isready -U linkctrl -d linkctrl >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "$PG" pg_isready -U linkctrl -d linkctrl >/dev/null 2>&1 \
  || fail "Postgres never became ready"

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

echo
echo "== a single container remains a supported, tested configuration =="
echo "Postgres is the only thing it had to reach."
