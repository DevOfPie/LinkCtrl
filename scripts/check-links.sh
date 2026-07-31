#!/usr/bin/env bash
#
# Every relative link and anchor in tracked markdown resolves.
#
# workflow.md has listed this as a commit gate since Phase 1 and nothing enforced
# it, which is the reliable way to end up with a gate nobody runs. Documentation
# that points at a file which moved is worse than documentation that says nothing:
# it costs a reader their trust in the rest of the file.
#
# Checks two things per link:
#   - the path exists, relative to the file containing the link
#   - the #anchor matches a heading in the target, slugified the way GitHub does
#     (lowercase, punctuation dropped, spaces to hyphens)
#
# External links are not checked. They fail for reasons this repository cannot
# fix, and a gate that depends on someone else's uptime is a gate that blocks a
# release for no defect of its own.
#
# Usage: scripts/check-links.sh
set -uo pipefail

cd "$(dirname "$0")/.."

fails=0
checked=0

# Heading slugs for a file, one per line, in GitHub's scheme: lowercase, drop
# everything that is not alphanumeric/space/hyphen/underscore, then replace each
# space with a hyphen.
#
# Each space individually — runs are NOT collapsed. "2026-07-29 — Phase 1" loses
# its em-dash to the punctuation strip and keeps both surrounding spaces, so the
# real anchor is "2026-07-29--phase-1" with two hyphens. Collapsing here made this
# script reject twenty correct anchors the first time it ran against them.
slugs() {
  grep -oE '^#{1,6} +.*' "$1" 2>/dev/null |
    sed -E 's/^#+ +//' |
    tr '[:upper:]' '[:lower:]' |
    sed -E 's/[^a-z0-9 _-]//g' |
    tr ' ' '-'
}

while IFS= read -r file; do
  dir=$(dirname "$file")

  # [text](target) — grab the target only. Skip images the same way; a missing
  # image is the same class of defect.
  while IFS= read -r link; do
    [ -n "$link" ] || continue
    case "$link" in
      http://*|https://*|mailto:*|'') continue ;;
    esac

    checked=$((checked + 1))

    path="$link"
    anchor=""
    case "$link" in
      \#*)    path=""            ; anchor="${link#\#}" ;;
      *\#*)   path="${link%%#*}" ; anchor="${link#*#}" ;;
    esac

    target="$file"
    if [ -n "$path" ]; then
      target="$dir/$path"
      if [ ! -e "$target" ]; then
        printf '  FAIL  %s -> %s (no such file)\n' "$file" "$link"
        fails=$((fails + 1))
        continue
      fi
    fi

    # A link to a directory resolves if the directory exists; there is no
    # heading to check inside one.
    [ -d "$target" ] && continue
    [ -n "$anchor" ] || continue

    if ! slugs "$target" | grep -qxF "$anchor"; then
      printf '  FAIL  %s -> %s (no such heading)\n' "$file" "$link"
      fails=$((fails + 1))
    fi
  done < <(grep -oE '\]\([^)]*\)' "$file" | sed -E 's/^\]\(//; s/\)$//')
done < <(git ls-files '*.md')

if [ "$fails" -eq 0 ]; then
  printf '  ok    %d links resolve\n' "$checked"
  exit 0
fi
printf '  %d broken link(s) of %d\n' "$fails" "$checked"
exit 1
