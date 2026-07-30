#!/usr/bin/env bash
#
# Everything that must hold before a tag is pushed.
#
# The point is that a release is a claim, and this is the list of things the claim
# depends on. It runs the same checks CI does, plus the ones that only matter when
# publishing: that the tree is clean, that generated code matches its source, that
# the version the binary will report is the version being tagged, and that the
# documentation does not still describe the previous release.
#
# Usage: scripts/release-check.sh [version]
#        VERSION=v1.2.3 scripts/release-check.sh
set -uo pipefail

VERSION="${1:-${VERSION:-}}"
fails=0

step() { printf '\n\033[1m== %s ==\033[0m\n' "$1"; }
ok()   { printf '  ok    %s\n' "$1"; }
bad()  { printf '  FAIL  %s\n' "$1"; fails=$((fails + 1)); }

require() { # require DESCRIPTION command...
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then ok "$desc"; else bad "$desc"; fi
}

step "working tree"
if [ -z "$(git status --porcelain)" ]; then
  ok "clean"
else
  bad "uncommitted changes — a release must be reproducible from the tag"
  git status --short | sed 's/^/        /'
fi

step "version"
if [ -z "$VERSION" ]; then
  printf '  skip  no version given; pass one to check the tag and changelog\n'
else
  case "$VERSION" in
    v[0-9]*.[0-9]*.[0-9]*) ok "shape ($VERSION)" ;;
    *) bad "version must look like v1.2.3, got $VERSION" ;;
  esac
  if git rev-parse "$VERSION" >/dev/null 2>&1; then
    bad "tag $VERSION already exists"
  else
    ok "tag is free"
  fi
  if grep -qF "## [${VERSION#v}]" CHANGELOG.md 2>/dev/null; then
    ok "CHANGELOG.md has a section for ${VERSION#v}"
  else
    bad "CHANGELOG.md has no '## [${VERSION#v}]' section"
  fi
fi

step "generated code matches its source"
# sqlc and the OpenAPI document are both hand-triggered. A release built from a
# tree where they were not regenerated ships a binary whose behaviour does not
# match the SQL and the spec in the same commit.
if command -v sqlc >/dev/null 2>&1; then
  before=$(git status --porcelain internal/store/dbgen)
  sqlc generate >/dev/null 2>&1
  if [ "$(git status --porcelain internal/store/dbgen)" = "$before" ]; then
    ok "sqlc output is current"
  else
    bad "sqlc generate produced a diff — commit it"
    git --no-pager diff --stat internal/store/dbgen | sed 's/^/        /'
  fi
else
  printf '  skip  sqlc not installed\n'
fi

step "assets the binary embeds"
# Through make, so the pinned versions and checksums have one home rather than
# being restated here where they would drift.
require "vendored htmx and Swagger UI match their checksums" make htmx swagger-ui
if [ -s internal/ui/static/css/app.css ] && \
   [ "$(wc -c < internal/ui/static/css/app.css)" -gt 8192 ]; then
  ok "stylesheet is built and plausible"
else
  bad "internal/ui/static/css/app.css is missing or too small — run 'make css'"
fi

step "tests and lint"
require "go build ./..."            go build ./...
require "go vet ./..."              go vet ./...
require "unit tests (race)"         go test -race -count=1 ./...
require "OpenAPI matches the routes" go test -count=1 -run TestOpenAPI ./internal/httpx/
if command -v golangci-lint >/dev/null 2>&1; then
  require "golangci-lint" golangci-lint run
else
  printf '  skip  golangci-lint not installed\n'
fi

step "integration tests"
if docker compose ps --status running --services 2>/dev/null | grep -qx postgres; then
  require "integration tests (race)" go test -tags=integration -race -count=1 ./test/integration/
else
  printf '  skip  Postgres is not running (docker compose up -d)\n'
fi

step "release artifacts build"
require "cross-compilation for every release platform" make dist

printf '\n'
if [ "$fails" -eq 0 ]; then
  printf '\033[1mready to tag\033[0m\n'
  exit 0
fi
printf '\033[1m%d check(s) failed\033[0m\n' "$fails"
exit 1
