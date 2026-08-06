#!/usr/bin/env bash
#
# The image answers for itself.
#
# Both binaries have to run from inside the image, and the image has to report the
# version it was built with. A stamped image reporting "dev" means the build args
# did not reach the ldflags, which nothing else in CI would notice — the container
# builds, starts and serves, and only the version line is wrong.
#
# Lived inline in .github/workflows/ci.yml until the workflow became a shim; see
# ci/proposed/README.md.
#
# Usage: scripts/ci-image-smoke.sh [image] [expected-version]
set -euo pipefail

image="${1:-linkctrl:ci}"
expected="${2:-ci}"

set -x
docker run --rm "$image" version
docker run --rm --entrypoint /lctl "$image" version
set +x

if ! docker run --rm "$image" version | grep -q "linkctrl $expected"; then
  echo "$image does not report version '$expected' — build args did not reach the ldflags" >&2
  exit 1
fi
