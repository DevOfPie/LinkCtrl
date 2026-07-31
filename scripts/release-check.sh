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

# Checked here because it is invisible on the machine where it gets broken.
# Windows has no executable bit, so a script committed from there lands in the
# index as 100644, every local run still works — bash reads the file regardless —
# and the first thing to notice is Linux CI refusing to execute it, one step into
# a job, with "Permission denied". Which is exactly how it happened.
step "script permissions"
non_exec=$(git ls-files -s -- 'scripts/*.sh' | awk '$1 != "100755" { print $4 }')
if [ -z "$non_exec" ]; then
  ok "every script is executable in the index"
else
  bad "not executable in git (fix: git update-index --chmod=+x <path>)"
  printf '        %s\n' $non_exec
fi

step "version"
if [ -z "$VERSION" ]; then
  printf '  skip  no version given; pass one to check the tag and changelog\n'
else
  # Anchored regex, the same one the release workflow enforces — a case glob
  # accepts trailing garbage ('v1.2.3;evil' matches v[0-9]*.[0-9]*.[0-9]*).
  if printf '%s' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
    ok "shape ($VERSION)"
  else
    bad "version must look like v1.2.3, got $VERSION"
  fi
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

step "documentation links"
# workflow.md makes this a commit gate. It went unenforced for all of Phase 1,
# which is what happens to a gate that needs someone to remember it.
require "every relative link and anchor resolves" scripts/check-links.sh

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

# The version sqlc stamps into every file it emits has to match the version CI
# installs, or CI regenerates, sees only that comment change, and fails with an
# otherwise empty diff. Checked here rather than left to CI because the developer
# who upgrades sqlc locally is the one who introduces it, and their own run of
# this script is where they should hear about it.
stamped=$(sed -n 's|^//   sqlc \(v[0-9.]*\)$|\1|p' internal/store/dbgen/models.go | head -1)
pinned=$(sed -n 's|.*sqlc-dev/sqlc/cmd/sqlc@\(v[0-9.]*\).*|\1|p' .github/workflows/ci.yml | head -1)
if [ -z "$stamped" ] || [ -z "$pinned" ]; then
  bad "could not read the sqlc version from the generated code or from ci.yml"
elif [ "$stamped" = "$pinned" ]; then
  ok "sqlc version agrees with CI ($stamped)"
else
  bad "generated code says sqlc $stamped, ci.yml installs $pinned — CI will fail on the version comment"
fi

step "assets the binary embeds"
# The pinned versions and checksums have one home, the Makefile, and are read
# from it rather than restated here where they would drift. Read rather than
# invoked, because this script has to run where make does not: development on
# Windows is a supported path — it is why a Taskfile exists — and a release gate
# that only runs on Linux is a gate that gets skipped by the people most likely
# to need it.
mkvar() { sed -n "s/^$1 *:*= *//p" Makefile | head -1 | tr -d '\r'; }

htmx_version=$(mkvar HTMX_VERSION)
htmx_sha=$(mkvar HTMX_SHA256)
swagger_version=$(mkvar SWAGGER_UI_VERSION)
swagger_css=$(mkvar SWAGGER_CSS_SHA256)
swagger_js=$(mkvar SWAGGER_JS_SHA256)

if [ -z "$htmx_version" ] || [ -z "$htmx_sha" ] || [ -z "$swagger_version" ] ||
   [ -z "$swagger_css" ] || [ -z "$swagger_js" ]; then
  # An empty value would make the checks below pass against nothing.
  bad "could not read the pinned asset versions from the Makefile"
else
  # VERIFY_ONLY, or these scripts would silently re-download a mismatching blob
  # and pass — a release gate must fail on a vendored asset that changed.
  require "vendored htmx matches its pinned checksum" \
    env VERIFY_ONLY=1 scripts/get-htmx.sh "$htmx_version" "$htmx_sha" \
      internal/ui/static/js/htmx.min.js
  require "vendored Swagger UI matches its pinned checksums" \
    env VERIFY_ONLY=1 scripts/get-swagger.sh "$swagger_version" "$swagger_css" "$swagger_js" \
      internal/ui/static/vendor
fi
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
# Compiles every release target rather than assembling the archives. What breaks
# here is a build tag or a syscall that does not exist on some GOOS, and that
# shows up at compile time; tarring and checksumming is `make dist`, which the
# release workflow runs. Same platform list, read from the same place.
platforms=$(mkvar RELEASE_PLATFORMS)
if [ -z "$platforms" ]; then
  bad "could not read RELEASE_PLATFORMS from the Makefile"
else
  xbuild_out=$(mktemp -d)
  xbuild_failed=""
  for platform in $platforms; do
    goos=${platform%/*}; goarch=${platform#*/}
    for cmd in linkctrl lctl; do
      if ! CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
           go build -trimpath -o "$xbuild_out/$cmd" "./cmd/$cmd" >/dev/null 2>&1; then
        xbuild_failed="$xbuild_failed $platform:$cmd"
      fi
    done
  done
  rm -rf "$xbuild_out"
  if [ -z "$xbuild_failed" ]; then
    ok "cross-compilation for every release platform"
  else
    bad "cross-compilation failed for:$xbuild_failed"
  fi
fi

printf '\n'
if [ "$fails" -eq 0 ]; then
  printf '\033[1mready to tag\033[0m\n'
  exit 0
fi
printf '\033[1m%d check(s) failed\033[0m\n' "$fails"
exit 1
