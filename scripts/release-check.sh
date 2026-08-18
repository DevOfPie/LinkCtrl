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

# Which stack the compose questions below are about, derived here rather than
# taken from the environment.
#
# `docs/releasing.md` offers `scripts/release-check.sh v0.3.0` as the equal
# alternative to `make release-check VERSION=v0.3.0`, and it was not equal: the
# integration step asks `docker compose` whether Postgres is up, that question
# only resolves when COMPOSE_PROJECT_NAME and COMPOSE_ENV_FILES are set, and only
# the Makefile set them. So the last gate before a tag printed `skip  Postgres is
# not running` on a machine where it was running, and a skip reads as information
# rather than as a third of the gate not running (F253).
#
# The owner's answer was that this script derives them, taking the drift pair
# knowingly: a Makefile change to either variable has to reach here. The step
# named "the Makefile and this script agree" below is what makes that drift a
# failure instead of a discovery. An already-exported value wins, so `make
# release-check` and CI keep passing theirs.
INSTANCE="${INSTANCE:-test}"
PROJECT="linkctrl-$INSTANCE"
ENV_FILE=".env.$INSTANCE"
export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-$PROJECT}"
export COMPOSE_ENV_FILES="${COMPOSE_ENV_FILES:-$ENV_FILE}"

# The DSNs are the same story one layer down, and the reason the Makefile's own
# comment at `release-check` exists: knowing Postgres is up buys nothing if the
# tests are then run with no connection string. Unset, `test/integration` falls
# back to a literal guess at the password on port 55432 — which is the *demo*
# instance's port, so the direct form did not merely skip, it aimed elsewhere.
#
# Templates, compared literally against the Makefile's below rather than expanded
# there: a password may hold characters that make a `sed` replacement mean
# something else.
# shellcheck disable=SC2016  # make's references, kept literal on purpose
DSN_TEMPLATE='postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)?sslmode=disable'
# shellcheck disable=SC2016  # same
REDIS_TEMPLATE='redis://localhost:$(REDIS_PORT)/0'

# One value out of the instance's env file, stripping inline comments the way
# compose does — whitespace, then `#` — so a `#` inside a password is kept.
# Deliberately the same rule as the Makefile's `envval`.
envval() {
  sed -n "s/^$1=//p" "$ENV_FILE" 2>/dev/null |
    head -1 | tr -d '\r' |
    sed -e 's/[[:space:]][[:space:]]*#.*$//' -e 's/[[:space:]]*$//'
}

POSTGRES_USER="${POSTGRES_USER:-linkctrl}"
POSTGRES_DB="${POSTGRES_DB:-linkctrl}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-$(envval POSTGRES_PASSWORD)}"
POSTGRES_PORT="${POSTGRES_PORT:-$(envval POSTGRES_PORT)}"
REDIS_PORT="${REDIS_PORT:-$(envval REDIS_PORT)}"

if [ -z "${TEST_DATABASE_URL:-}" ] && [ -n "$POSTGRES_PASSWORD" ] && [ -n "$POSTGRES_PORT" ]; then
  TEST_DATABASE_URL="postgres://$POSTGRES_USER:$POSTGRES_PASSWORD@localhost:$POSTGRES_PORT/$POSTGRES_DB?sslmode=disable"
  export TEST_DATABASE_URL
fi
if [ -z "${LINKCTRL_REDIS_URL:-}" ] && [ -n "$REDIS_PORT" ]; then
  LINKCTRL_REDIS_URL="redis://localhost:$REDIS_PORT/0"
  export LINKCTRL_REDIS_URL
fi

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
  printf '%s\n' "$non_exec" | sed 's/^/        /'
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
  # Whether the section exists and what date it carries are read once, together,
  # so the two cannot disagree about which heading they are looking at. Both are
  # fixed-string and anchored to column 1: interpolating the version into a regex
  # turns its dots into any-char, and a section headed "## [0130] - 2026-08-17"
  # would then satisfy a date check for 0.3.0 that a fixed-string existence check
  # had just refused. The sentinel "=" is what distinguishes "no such heading"
  # from "heading with nothing after it", which are different failures.
  heading="## [${VERSION#v}]"
  found=$(awk -v want="$heading" '
    index($0, want) == 1 { print "=" substr($0, length(want) + 1); exit }
  ' CHANGELOG.md 2>/dev/null)
  if [ -n "$found" ]; then
    ok "CHANGELOG.md has a section for ${VERSION#v}"
  else
    bad "CHANGELOG.md has no '$heading' section"
  fi

  # A section existing is not the same as a section describing the release. The
  # release workflow extracts from "## [<version>]" to the next "## [", so
  # anything still sitting in [Unreleased] — which is *above* it, this file being
  # newest-first — is never published. Both guards asked only whether the section
  # existed, and 0.3.0 accordingly came one commit from shipping notes that
  # omitted the whole dashboard redesign, 217 lines of it (F251).
  #
  # Empty means no content between the [Unreleased] heading and the next section
  # or the link-reference block. The heading itself stays: the next phase writes
  # into it immediately, and a check that demanded its removal would be asking
  # for the file to be wrong in the other direction. So its absence is a failure
  # of its own rather than an empty count — a file with no [Unreleased] heading
  # at all would otherwise pass this as "empty", while the "[Unreleased]:"
  # definition at the foot points at nothing and check-links.sh, which resolves
  # references but does not look for unreferenced definitions, says nothing.
  if grep -q '^## \[Unreleased\]' CHANGELOG.md 2>/dev/null; then
    unreleased=$(awk '
      /^## \[Unreleased\]/     { inside = 1; next }
      inside && /^## \[/       { exit }
      inside && /^\[[^]]+\]: / { exit }
      inside                   { print }
    ' CHANGELOG.md | grep -c '[^[:space:]]')
    if [ "$unreleased" -eq 0 ]; then
      ok "[Unreleased] is empty — the ${VERSION#v} section is the whole release"
    else
      bad "[Unreleased] holds $unreleased line(s) that ${VERSION#v}'s notes will not contain — fold them into '$heading'"
    fi
  else
    bad "CHANGELOG.md has no '## [Unreleased]' heading — it stays in place and empty, or its link reference at the foot of the file points at nothing and the next release has nowhere to be written"
  fi

  # And the date, because the fold above is only true on the day it is done. A
  # section dated when it was written and tagged a week later re-states the same
  # defect in the other direction: notes that describe the release, under a date
  # that describes nothing. Keep a Changelog's date is a claim about when a
  # version was released, and this script runs when it is about to be.
  #
  # Trailing whitespace is stripped before comparing, or a heading dated today
  # with a space after it fails with a message saying it is dated today.
  today=$(date +%F)
  if [ -z "$found" ]; then
    printf "  skip  no '%s' section to carry a release date\n" "$heading"
  else
    suffix=${found#=}
    case "$suffix" in
      " - "*) dated=$(printf '%s' "${suffix# - }" | sed 's/[[:space:]]*$//') ;;
      *)      dated="" ;;
    esac
    if [ -z "$dated" ]; then
      bad "'$heading' carries no '- YYYY-MM-DD' release date"
    elif [ "$dated" = "$today" ]; then
      ok "the ${VERSION#v} section is dated today ($today)"
    else
      bad "the ${VERSION#v} section is dated $dated and the tag is being cut on $today — re-date it"
    fi
  fi
fi

step "documentation links"
# workflow.md makes this a commit gate. It went unenforced for all of Phase 1,
# which is what happens to a gate that needs someone to remember it.
require "every link and anchor resolves, and every table row matches its header" \
        scripts/check-links.sh

step "continuous integration"
# The gate that was missing. Every other check in this file, and every check
# workflow.md names, runs on this machine — so a build that is red only on the
# runner is invisible to all of them, and one was: twelve consecutive red CI runs
# across a phase, two adversarial reviews and a release candidate, while local
# `make check` reported 0 issues at every push. The owner found it on the release
# PR (F255).
#
# Run rather than required, because the three outcomes are not two: exit 2 is
# "could not ask", which must not read as a failing build. It is reported and
# does not count against the tag — an offline machine cannot answer this question
# and pretending otherwise would make the gate a coin toss rather than a check.
ci_out=$(scripts/check-ci.sh 2>&1); ci_rc=$?
printf '%s\n' "$ci_out"
case "$ci_rc" in
  0) : ;;
  1) fails=$((fails + 1)) ;;
  *) printf '  skip  CI'\''s verdict is unknown, not green — this check did not run\n' ;;
esac

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
# The pin moved out of ci.yml and into the Makefile when W27 folded CI's steps
# into make targets, and this check kept reading the workflow — so it stopped
# finding a version at all and reported that as a failure rather than silently
# passing, which is the one direction a broken check should fail in.
stamped=$(sed -n 's|^//   sqlc \(v[0-9.]*\)$|\1|p' internal/store/dbgen/models.go | head -1)
pinned=$(sed -n 's|^SQLC_VERSION *:*= *\(v[0-9.]*\).*|\1|p' Makefile | head -1)
if [ -z "$stamped" ] || [ -z "$pinned" ]; then
  bad "could not read the sqlc version from the generated code or from the Makefile's SQLC_VERSION"
elif [ "$stamped" = "$pinned" ]; then
  ok "sqlc version agrees with the Makefile pin ($stamped)"
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
# All three assignment forms, because the compose variables F253 is about are
# written `?=` and `=` while the asset pins are `:=`. Unexpanded: a value holding
# `$(OTHER)` comes back with the reference in it, which is what the agreement step
# below wants to compare.
mkvar()    { sed -n "s/^$1 *[:?+]*= *//p" Makefile | head -1 | tr -d '\r' | sed 's/[[:space:]]*$//'; }
mkexport() { sed -n "s/^export  *$1 *[:?+]*= *//p" Makefile | head -1 | tr -d '\r' | sed 's/[[:space:]]*$//'; }

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

step "the Makefile and this script agree about which stack"
# The accepted cost of F253's answer, made into a gate. This script derives the
# compose project and env file itself so that the form `docs/releasing.md` offers
# is not a weaker gate than `make release-check`; the price is two derivations of
# one fact, and a price is only accepted if something notices when it is not paid.
# Compared as written, references unexpanded — `$(INSTANCE)` substituted with the
# instance this run is about and nothing else.
agree() { # agree DESCRIPTION expected actual
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1: Makefile says '$3', this script builds '$2'"; fi
}
mk_instance=$(mkvar INSTANCE)
agree "default instance ($INSTANCE)"        "$INSTANCE" "$mk_instance"
agree "env file ($ENV_FILE)"                "$ENV_FILE" "$(mkvar ENV_FILE | sed "s/\$(INSTANCE)/$mk_instance/g")"
agree "compose project ($PROJECT)"          "$PROJECT"  "$(mkvar PROJECT  | sed "s/\$(INSTANCE)/$mk_instance/g")"
# shellcheck disable=SC2016  # $(PROJECT) and $(ENV_FILE) are make's, compared as text
agree 'COMPOSE_PROJECT_NAME is the project' '$(PROJECT)'  "$(mkexport COMPOSE_PROJECT_NAME)"
# shellcheck disable=SC2016  # same
agree 'COMPOSE_ENV_FILES is the env file'   '$(ENV_FILE)' "$(mkexport COMPOSE_ENV_FILES)"
agree "the database DSN"                    "$DSN_TEMPLATE"   "$(mkvar DEV_DATABASE_URL)"
agree "the Redis URL"                       "$REDIS_TEMPLATE" "$(mkvar DEV_REDIS_URL)"

step "integration tests"
# Three outcomes, not two. A skip that cannot say which of "the stack is down"
# and "I could not build a DSN" it means is the shape of F253: the direct form
# reported one while the other was true.
if [ -z "${TEST_DATABASE_URL:-}" ]; then
  bad "no TEST_DATABASE_URL and none could be built from $ENV_FILE (make env INSTANCE=$INSTANCE)"
elif docker compose ps --status running --services 2>/dev/null | grep -qx postgres; then
  require "integration tests (race)" go test -tags=integration -race -count=1 ./test/integration/
else
  printf '  skip  Postgres is not running in project %s (docker compose up -d)\n' "$COMPOSE_PROJECT_NAME"
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
