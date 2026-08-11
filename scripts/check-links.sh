#!/usr/bin/env bash
#
# Tracked markdown says on the page what it says in the file: every relative link
# and anchor resolves, and every table row has the cells its header declares.
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
# And one thing per table row: its cell count against its own header's. GFM
# **drops** cells past the header count rather than widening the table, so a row
# with one cell too many loses its last column on every rendered page while
# reading correctly in the file — invisible to `git show`, to an editor, and to
# the links half of this script. F193 found two such rows by hand; the ad-hoc
# scan that found them missed a third, F103, and the miss is the reason this
# lives in a gate now. See the note on `cells` for what it got wrong.
#
# And one thing per table: the delimiter row's cell count against the header's
# (F198). A header wider than its delimiter is the malformation GFM answers by
# dropping the *whole table* — the dropped-cell class one size up, and invisible
# to a scan that reads only body rows against the header. Fenced blocks are
# skipped throughout, so a document can demonstrate a malformed row without
# failing the gate that documents it. The fence toggle matches what this tree
# writes — ``` at up to three spaces of indent; a tilde or four-backtick fence
# would be content to it, and none exists in tracked markdown today.
#
# Usage: scripts/check-links.sh
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

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

    # Not `slugs "$target" | grep -qxF`: `grep -q` exits at the first match, the
    # writers upstream of it take SIGPIPE, and `pipefail` then reports 141 for a
    # pipeline that succeeded. That failed roughly one anchor in five, a
    # different one each run.
    if ! grep -qxF "$anchor" <<<"$(slugs "$target")"; then
      printf '  FAIL  %s -> %s (no such heading)\n' "$file" "$link"
      fails=$((fails + 1))
    fi
  done < <(grep -oE '\]\([^)]*\)' "$file" | sed -E 's/^\]\(//; s/\)$//')
done < <(git ls-files '*.md')

if [ "$fails" -eq 0 ]; then
  printf '  ok    %d links resolve\n' "$checked"
else
  printf '  %d broken link(s) of %d\n' "$fails" "$checked"
fi

# Table rows against their own header.
#
# Written as awk over the file rather than as a regex over `^| F` lines, because
# the shape of the bug is that a row does not look wrong. The counting rule is
# GFM's: a leading pipe is optional, a trailing pipe is optional, `\|` is content
# and not a delimiter.
#
# **The optional trailing pipe is the whole of what the ad-hoc scan got wrong.**
# It counted `split(line, "|") - 2`, charging one empty field to each end, which
# is right for `| a | b |` and one short for `| a | b`. F28 and F5 ended with a
# trailing pipe and a surplus *empty* cell, so they were found; F103 ended with
# no trailing pipe and a surplus cell holding an entire amendment, so it was
# counted as conforming and its M54 amendment rendered nowhere. A scan that can
# only see the malformation it has already seen is not a scan.
# shellcheck disable=SC2016  # $0 and $1 are awk's fields, not the shell's
tables=$(git ls-files '*.md' | xargs awk '
function cells(s,   t, n) {
  gsub(/\\\|/, "\001", s)             # \| is content, not a delimiter
  sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s)
  sub(/^\|/, "", s)                   # both pipes optional, independently
  sub(/\|$/, "", s)
  return split(s, t, "|")
}
function isrow(s) { gsub(/\\\|/, "\001", s); return (s ~ /\|/) }
function isdelim(s,   t, n, i, c) {
  if (!isrow(s)) return 0
  gsub(/\\\|/, "\001", s)
  sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s)
  sub(/^\|/, "", s); sub(/\|$/, "", s)
  n = split(s, t, "|")
  for (i = 1; i <= n; i++) { c = t[i]; gsub(/[ \t]/, "", c)
    if (c !~ /^:?-{2,}:?$/) return 0 }
  return (n > 0)
}
FNR == 1 { inbody = 0; prev = ""; infence = 0 }
/^ {0,3}```/ { infence = !infence; inbody = 0; prev = ""; next }
infence { next }
isdelim($0) && prev != "" {
  want = cells(prev); hdr = FNR - 1
  if (cells($0) != want)
    printf "  FAIL  %s:%d is a %d-cell delimiter under the %d-column header at :%d\n",
           FILENAME, FNR, cells($0), want, hdr
  inbody = 1; prev = $0; next
}
{
  if (inbody && isrow($0)) {
    rows++
    if (cells($0) != want)
      printf "  FAIL  %s:%d has %d cells against the %d-column header at :%d\n",
             FILENAME, FNR, cells($0), want, hdr
  } else if (!isrow($0)) inbody = 0
  prev = isrow($0) ? $0 : ""
}
END { printf "  rows  %d\n", rows }
')
rowcount=$(printf '%s\n' "$tables" | sed -n 's/^  rows  //p')
badrows=$(printf '%s\n' "$tables" | grep -c '^  FAIL' || true)
printf '%s\n' "$tables" | grep '^  FAIL' || true

if [ "$badrows" -eq 0 ]; then
  printf '  ok    %d table rows match their headers\n' "$rowcount"
else
  printf '  %d malformed table row(s) of %d\n' "$badrows" "$rowcount"
fi

[ "$fails" -eq 0 ] && [ "$badrows" -eq 0 ] && exit 0
exit 1
