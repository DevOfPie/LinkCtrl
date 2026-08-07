#!/usr/bin/env bash
#
# Stop a development instance that nothing has used for a while.
#
# The test instance is disposable and spends most of its life doing nothing,
# which on a laptop is three containers' worth of memory and a Postgres holding
# its shared buffers. This is run by a systemd timer every few minutes; see
# docs/dev-notes/instances.md and the unit in docs/dev-notes/environment.md.
#
# Usage: scripts/idle-stop.sh [instance] [idle-minutes]
#
# Idle means all of:
#
#   - No HTTP request on any surface except `ops` since the last check. The ops
#     surface is health probes, which the container runs every ten seconds
#     forever, so counting them would mean never idle.
#   - No process on this host using the instance: `go test`, `lctl`, a server
#     started by `make run`. These talk to Postgres directly and never appear in
#     the app's metrics at all.
#   - No keep-file. `touch /tmp/linkctrl-<instance>-keep` holds the stack up for
#     something long and unattended; delete it to release.
#
# Stopping is `docker compose stop`: containers keep their state and their
# volumes, `make up` brings them back in about two seconds, and no data moves.
set -euo pipefail

cd "$(dirname "$0")/.."

INSTANCE="${1:-test}"
IDLE_MINUTES="${2:-30}"

case "$INSTANCE" in
	test) ;;
	demo)
		# The demo is meant to be there whenever the browser is pointed at it.
		echo "idle-stop: refusing to manage the demo instance" >&2
		exit 1
		;;
	*) echo "idle-stop: unknown instance '$INSTANCE'" >&2; exit 1 ;;
esac

ENV_FILE=".env.$INSTANCE"
PROJECT="linkctrl-$INSTANCE"
KEEP_FILE="/tmp/linkctrl-$INSTANCE-keep"
STATE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/linkctrl"
STATE_FILE="$STATE_DIR/idle-stop.$INSTANCE"

log() { printf 'idle-stop: %s\n' "$*"; }

[ -f "$ENV_FILE" ] || { log "$ENV_FILE does not exist, nothing to do"; exit 0; }

# Docker Desktop provides the CLI over /mnt/wsl and the socket with it, so both
# disappear when it is not running. That is not a failure worth a red unit every
# five minutes — there is nothing running to stop either.
command -v docker >/dev/null 2>&1 || { log "docker is not available"; exit 0; }

compose() { docker compose -p "$PROJECT" --env-file "$ENV_FILE" "$@"; }

# Nothing running means nothing to stop, and the state is stale either way.
#
# Any service counts, not just the app. `make migrate-status` on a stopped
# instance starts Postgres and Redis and leaves the app down, and an earlier
# version of this check called that "not running" — so the half-stack it creates
# was the one state the timer could never clean up.
running="$(compose ps --status running --services 2>/dev/null | tr '\n' ' ' || true)"
if [ -z "${running// /}" ]; then
	log "not running, nothing to do"
	rm -f "$STATE_FILE"
	exit 0
fi

app_running=false
case " $running " in *" app "*) app_running=true ;; esac

envval() {
	sed -n "s/^$1=//p" "$ENV_FILE" 2>/dev/null | head -1 | tr -d '\r' |
		sed -e 's/[[:space:]][[:space:]]*#.*$//' -e 's/[[:space:]]*$//'
}

# Requests on every surface except ops. A single number that only moves when
# somebody uses the instance: a redirect, a page, an API call.
requests_served() {
	local port total
	port="$(envval LINKCTRL_METRICS_PORT)"
	[ -n "$port" ] || { echo unknown; return; }
	total="$(curl -fsS --max-time 5 "http://127.0.0.1:${port}/metrics" 2>/dev/null |
		awk '/^linkctrl_http_request_duration_seconds_count\{/ {
			if ($0 !~ /surface="ops"/) sum += $NF
		} END { printf "%d", sum }')" || true
	[ -n "$total" ] || total=unknown
	printf '%s' "$total"
}

# Anything on this host talking to the instance. `go test` covers the
# integration suite, and the two binaries cover `make run`, `make seed` and
# `lctl` by hand. pgrep -f matches the whole command line, so the paths are
# specific enough not to match an unrelated editor or shell.
host_users() {
	pgrep -f 'go test .*(integration|linkctrl)|(^|/)lctl( |$)|go run .*cmd/(lctl|linkctrl)|(^|/)linkctrl( |$)' \
		2>/dev/null | grep -cv "^$$\$" || true
}

now="$(date +%s)"

if $app_running; then
	current="$(requests_served)"
	# A scrape that failed says nothing about whether the instance is in use,
	# and guessing "idle" from a failed measurement is how a stack gets stopped
	# while somebody is using it. Treat it as activity and try again next tick.
	if [ "$current" = unknown ]; then
		log "could not read the request counter; treating as active"
		mkdir -p "$STATE_DIR"
		printf '%s %s\n' "unknown" "$now" > "$STATE_FILE"
		exit 0
	fi
else
	# Databases up, app down: `make run` or a test suite left them that way, and
	# nothing can be serving HTTP. A sentinel rather than a count, so the only
	# thing that can mark this state active is a process on this host — and so
	# that starting the app later reads as activity rather than as a counter
	# that happened not to move.
	current="app-down"
fi

previous="" since="$now"
if [ -f "$STATE_FILE" ]; then
	read -r previous since < "$STATE_FILE" || true
	[ -n "${since:-}" ] || since="$now"
fi

active_reason=""
if [ -e "$KEEP_FILE" ]; then
	active_reason="keep-file $KEEP_FILE"
elif [ "$(host_users)" -gt 0 ]; then
	active_reason="a process on this host is using it"
elif [ -z "$previous" ]; then
	active_reason="first check since it started"
elif [ "$previous" != "$current" ]; then
	if $app_running && [ "$previous" != app-down ]; then
		active_reason="requests served since the last check ($previous -> $current)"
	else
		active_reason="the stack changed state since the last check"
	fi
fi

mkdir -p "$STATE_DIR"

if [ -n "$active_reason" ]; then
	printf '%s %s\n' "$current" "$now" > "$STATE_FILE"
	log "active: $active_reason"
	exit 0
fi

idle_for=$(( (now - since) / 60 ))
if [ "$idle_for" -lt "$IDLE_MINUTES" ]; then
	printf '%s %s\n' "$current" "$since" > "$STATE_FILE"
	log "idle for ${idle_for}m of ${IDLE_MINUTES}m"
	exit 0
fi

log "idle for ${idle_for}m, stopping $PROJECT"
compose stop
rm -f "$STATE_FILE"
