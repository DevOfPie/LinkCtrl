#!/usr/bin/env bash
#
# A built binary reports the version it was built with.
#
# The ldflags path is the kind of thing that breaks silently: the build succeeds,
# the tests pass, and the only symptom is `version` printing "commit unknown" to
# whoever is trying to work out which build they are looking at. That is usually
# the moment when finding out costs the most.
#
# Lived inline in .github/workflows/ci.yml until the workflow became a shim; see
# ci/proposed/README.md for why CI's logic lives in targets rather than in YAML.
#
# Usage: scripts/check-version-stamp.sh bin/linkctrl bin/lctl
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: $0 BINARY [BINARY...]" >&2
  exit 2
fi

status=0
for bin in "$@"; do
  if [ ! -x "$bin" ]; then
    echo "$bin is not an executable — build first" >&2
    status=1
    continue
  fi

  # Printed as well as checked: a version line in the log is what makes a failed
  # run diagnosable without re-running it locally.
  out=$("$bin" version)
  printf '%s\n' "$out"

  if printf '%s' "$out" | grep -q 'commit unknown'; then
    echo "$bin was built without its version stamp" >&2
    status=1
  fi
done

exit "$status"
