#!/usr/bin/env bash
#
# Which workflow proposals are still waiting for the owner to apply them.
#
# The agent building this repository holds a token without the `Workflows`
# permission, so it cannot write `.github/workflows/` at all — a push carrying one
# of those paths is rejected before any review happens. Changes to CI are
# therefore written to ci/proposed/ and applied by the owner; ci/proposed/README.md
# is the contract.
#
# The failure that convention invites is a proposal nobody notices, so this
# reports the state rather than leaving it to be remembered. It is not a gate:
# a pending proposal is a normal state, and failing CI on one would mean CI going
# red for a change that has not been asked for yet.
#
# Usage: scripts/workflow-proposals.sh
set -uo pipefail

proposed_dir="ci/proposed"
live_dir=".github/workflows"

if [ ! -d "$proposed_dir" ]; then
  echo "no $proposed_dir directory — nothing proposed"
  exit 0
fi

pending=0
found=0

for proposal in "$proposed_dir"/*.yml "$proposed_dir"/*.yaml; do
  [ -e "$proposal" ] || continue
  found=$((found + 1))
  name=$(basename "$proposal")
  live="$live_dir/$name"

  if [ ! -e "$live" ]; then
    printf 'PENDING  %s — no %s yet; this proposal creates it\n' "$name" "$live"
    pending=$((pending + 1))
  elif cmp -s "$proposal" "$live"; then
    printf 'applied  %s — matches %s\n' "$name" "$live"
  else
    printf 'PENDING  %s — differs from %s\n' "$name" "$live"
    pending=$((pending + 1))
    diff -u "$live" "$proposal" | sed 's/^/         /'
  fi
done

if [ "$found" -eq 0 ]; then
  echo "no proposals in $proposed_dir"
  exit 0
fi

echo
if [ "$pending" -eq 0 ]; then
  echo "$found proposal(s), none pending — every one matches the live workflow."
  echo "A proposal that matches has been applied; move its row to Made in"
  echo "docs/build-notes/workflow-changes.md and delete the file."
else
  echo "$pending of $found proposal(s) pending. Apply them per ci/proposed/README.md."
fi

exit 0
