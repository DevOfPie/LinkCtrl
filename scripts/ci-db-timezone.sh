#!/usr/bin/env bash
#
# The database session really is UTC.
#
# Partition bounds on a timestamptz resolve against the session timezone at DDL
# time, so a non-UTC session silently creates partitions offset by its own offset
# and leaves gaps that swallow rows. The application refuses to start in that
# state; asserting it here means a CI-only difference cannot produce a green run
# that hides it.
#
# Lived inline in .github/workflows/ci.yml until the workflow became a shim; see
# ci/proposed/README.md.
#
# Usage: scripts/ci-db-timezone.sh "postgres://user:pass@host:port/db?sslmode=disable"
set -euo pipefail

dsn="${1:-${TEST_DATABASE_URL:-}}"

if [ -z "$dsn" ]; then
  echo "usage: $0 DSN   (or set TEST_DATABASE_URL)" >&2
  exit 2
fi

tz=$(psql "$dsn" -tAc "SHOW timezone" | tr -d '[:space:]')

if [ "$tz" != "UTC" ]; then
  echo "session timezone is '$tz', not UTC" >&2
  exit 1
fi

echo "session timezone is UTC"
