#!/usr/bin/env bash
#
# What the always-read documentation costs, predicted against realized.
#
# This repository's operating contract is documentation, and documentation is
# read into a context window on every task. That cost recurs forever and nothing
# was measuring it. decisions.md is append-only and already larger than every
# other build-note combined, so the growth is real and it is one-directional.
#
# Two numbers, because either one alone misleads:
#
#   Predicted  what a trigger's documented read set costs, charged from what the
#              contract actually instructs. A file the contract says to read is
#              charged in full. A file it says to read *one row of* is charged at
#              that file's longest such row, which is the ceiling for a single-row
#              read. Exact in bytes either way.
#
#   Realized   what Read actually returned, from this machine's session
#              transcripts. Exact, and the only way to know whether partial
#              reads are working.
#
# The gap between them is the signal. Realized close to predicted means the file
# is read whole every time and its size is a recurring tax worth paying down.
# Realized far below predicted means partial reads are doing their job. Realized
# climbing across snapshots means something started reading whole what it used
# to grep.
#
# Deliberately not measured, because it cannot be measured honestly here:
# content reaching the context through Bash (cat, sed, grep), through search
# results, or through the harness's own automatic CLAUDE.md load. Realized is
# therefore a floor, not a total, and the report says so.
#
# Emits markdown on stdout. No generation timestamp: the commit date is the
# timestamp, it is not invented, and its absence means re-running on an
# unchanged tree produces no diff.
#
# Usage: scripts/doc-cost.sh
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

# Read sets, from what the documents themselves instruct. Keep these in step
# with workflow.md's triggers and phase-loop.md's step 1: a set that drifts from
# the contract reports a cost nobody pays.
EVERY_TASK=(CLAUDE.md docs/build-notes/workflow.md)
# An entry may be `path` — charged whole — or `path::regex`, charged at the
# longest line matching that regex. The second form is for a file the contract
# reads one row of: phase-loop.md's step 1 says "Plan.md's ordering row for N",
# and charging the whole 170KB file for one 185-byte row reported a cost nobody
# has ever paid. See the note in the generated header about comparability.
PHASE_LOOP=(
	docs/build-notes/phase-loop.md
	docs/build-notes/phase-details/README.md
	'Plan.md::^\| \[M'
)
FEATURE=(
	docs/build-notes/planning.md
	docs/build-notes/phase-details/README.md
	docs/build-notes/phase-details/_template.md
)
REFERENCE=(
	docs/build-notes/decisions.md
	docs/build-notes/deferred-findings.md
	docs/build-notes/development.md
	docs/build-notes/upcoming-decisions.md
)

bytes_of() { wc -c <"$1" | tr -d ' '; }

# The longest line matching a regex, plus its newline. Prints nothing when the
# regex matches no line, and the caller falls back to the whole file and says so
# — a pattern that stops matching means the file was restructured, and reporting
# zero for it would understate the cost in exactly the direction that flatters.
# The regex reaches awk through the environment rather than through -v, because
# -v processes escape sequences in the value first: `\[` arrives as a bare `[`,
# which opens a bracket expression that never closes, and awk dies on an invalid
# regex. ENVIRON hands the bytes over untouched.
longest_row_of() {
	DOC_COST_RE="$2" awk '
		$0 ~ ENVIRON["DOC_COST_RE"] { n = length($0) + 1; if (n > m) m = n }
		END { if (m) print m }
	' "$1"
}

# 4 bytes per token. An approximation, labelled as one everywhere it appears —
# the byte counts are measurements and the token counts are not, and this
# repository's standing rule is that the difference stays visible.
est_tokens() { echo $(($1 / 4)); }

emit_table() {
	local total=0 entry f re b label
	printf '| File | Bytes | ≈tokens |\n| --- | ---: | ---: |\n'
	for entry in "$@"; do
		f=${entry%%::*}
		re=""
		[ "$entry" != "$f" ] && re=${entry#*::}
		[ -f "$f" ] || continue
		# shellcheck disable=SC2016  # the backticks are markdown, not a subshell
		label="\`$f\`"
		if [ -n "$re" ]; then
			b=$(longest_row_of "$f" "$re")
			if [ -n "$b" ]; then
				label="$label, longest row"
			else
				b=$(bytes_of "$f")
				label="$label — **row pattern matched nothing, charged whole**"
			fi
		else
			b=$(bytes_of "$f")
		fi
		total=$((total + b))
		printf '| %s | %s | %s |\n' "$label" "$b" "$(est_tokens "$b")"
	done
	printf '| **Total** | **%s** | **%s** |\n\n' "$total" "$(est_tokens "$total")"
	echo "$total" >/tmp/doc-cost-total.$$
}

subtotal() { cat "/tmp/doc-cost-total.$$" 2>/dev/null || echo 0; }

cat <<'HEADER'
# Documentation cost

What this repository's operating contract costs to read, measured rather than
guessed. Generated by `make doc-cost`; do not edit by hand.

Byte counts are exact. **Token counts are an approximation** — bytes divided by
four — and are marked `≈` wherever they appear. No tokeniser is run, so no
number here should be quoted as a measured token count.

There is no generation date. The commit date is the date, and leaving it out
means regenerating on an unchanged tree produces no diff, so every diff in this
file is real growth.

**How charging works, and when it changed.** A file the contract says to read is
charged in full. A file the contract says to read *one row of* is charged at that
file's longest such row. Before 2026-08-06 every file was charged in full,
including `Plan.md`, which step 1 reads a single ordering row of — so the
`/work phase` resume floor was reported at 225579 bytes when three quarters of
that was one file nobody reads whole. Numbers from before that commit are **not
comparable** with numbers after it. The change was made at a phase boundary for
exactly that reason.

---

## Predicted — what the documented read sets cost

Charged from what the contract instructs, per the note above. It remains a
ceiling: a whole-file entry assumes the whole file is read, and a by-row entry
assumes the longest row rather than a typical one.

HEADER

echo "### Every task"
echo
echo "Read before anything else happens, per CLAUDE.md."
echo
emit_table "${EVERY_TASK[@]}"
every=$(subtotal)

echo "### \`/work phase\` — per resume, on top of the above"
echo
echo "Step 0 and step 1 of the loop, before a single milestone file is opened."
echo
emit_table "${PHASE_LOOP[@]}"
loop=$(subtotal)

echo "### A feature request — on top of *every task*"
echo
echo "planning.md's path, per workflow.md's feature trigger."
echo
emit_table "${FEATURE[@]}"

echo "### Reference — named by the contract, not read whole by it"
echo
echo "These are pointed at, grepped, or appended to. Charging them in full is"
echo "what makes the predicted column a ceiling rather than an estimate."
echo
emit_table "${REFERENCE[@]}"

echo "### Floors"
echo
printf '| Trigger | Bytes | ≈tokens |\n| --- | ---: | ---: |\n'
printf '| Any task | %s | %s |\n' "$every" "$(est_tokens "$every")"
# shellcheck disable=SC2016  # the backticks are markdown, not a subshell
printf '| `/work phase` resume | %s | %s |\n' \
	"$((every + loop))" "$(est_tokens "$((every + loop))")"
echo
echo "Plus one \`phase-details/mN.md\` per milestone, which the split exists to"
echo "keep small — the loop reads the one being built and no others."
echo

rm -f "/tmp/doc-cost-total.$$"

# ---- realized -------------------------------------------------------------

echo "---"
echo
echo "## Realized — what Read actually returned"
echo

slug=$(pwd | sed 's|/|-|g')
transcripts="$HOME/.claude/projects/$slug"

if ! command -v python3 >/dev/null 2>&1; then
	echo "_Not measured: python3 is not installed, and parsing the session"
	echo "transcripts needs it. The predicted section above is unaffected._"
	exit 0
fi

if [ ! -d "$transcripts" ]; then
	echo "_Not measured: no session transcripts at \`$transcripts\`._"
	exit 0
fi

python3 - "$transcripts" <<'PY'
import glob
import json
import os
import sys

transcripts = sys.argv[1]

pending = {}          # tool_use_id -> file_path
per_file = {}         # path -> [reads, bytes]
sessions = set()
reads_total = 0

def result_len(content):
    if isinstance(content, str):
        return len(content)
    if isinstance(content, list):
        n = 0
        for block in content:
            if isinstance(block, dict) and isinstance(block.get("text"), str):
                n += len(block["text"])
            elif isinstance(block, str):
                n += len(block)
        return n
    return 0

for path in sorted(glob.glob(os.path.join(transcripts, "*.jsonl"))):
    sessions.add(os.path.basename(path))
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            try:
                rec = json.loads(line)
            except ValueError:
                continue
            msg = rec.get("message")
            if not isinstance(msg, dict):
                continue
            content = msg.get("content")
            if not isinstance(content, list):
                continue
            for block in content:
                if not isinstance(block, dict):
                    continue
                if block.get("type") == "tool_use" and block.get("name") == "Read":
                    fp = (block.get("input") or {}).get("file_path")
                    if fp:
                        pending[block.get("id")] = fp
                elif block.get("type") == "tool_result":
                    fp = pending.pop(block.get("tool_use_id"), None)
                    if not fp:
                        continue
                    rel = os.path.relpath(fp, os.getcwd()) if fp.startswith("/") else fp
                    # Scratchpad and other out-of-tree reads are session noise,
                    # not a cost this repository can pay down.
                    if rel.startswith(".."):
                        continue
                    row = per_file.setdefault(rel, [0, 0])
                    row[0] += 1
                    row[1] += result_len(block.get("content"))
                    globals()["reads_total"] = globals()["reads_total"] + 1

if not per_file:
    print("_No Read calls found in %d session transcript(s)._" % len(sessions))
    sys.exit(0)

print("From %d session transcript(s) on this machine, %d Read call(s)."
      % (len(sessions), reads_total))
print()
print("Realized bytes are what the tool returned, including the line-number")
print("prefix Read adds, so a whole-file read measures slightly above the")
print("file's own size. **Mean ÷ size** is the number that matters: near 1.0")
print("means the file is read whole and its size is a recurring cost; well")
print("below 1.0 means partial reads are working.")
print()
print("Only files read **more than once** are listed. A file read a single time")
print("is not a recurring cost and nothing here can be optimised about it; the")
print("singletons are rolled up in the last row instead.")
print()
print("| File | Reads | Total bytes | ≈tokens | Mean/read | Size now | Mean ÷ size |")
print("| --- | ---: | ---: | ---: | ---: | ---: | ---: |")

rows = sorted(per_file.items(), key=lambda kv: -kv[1][1])
grand = 0
once_files = once_bytes = 0
for rel, (count, total) in rows:
    grand += total
    if count < 2:
        once_files += 1
        once_bytes += total
        continue
    mean = total // count
    try:
        size = os.path.getsize(rel)
    except OSError:
        size = 0
    ratio = ("%.2f" % (mean / size)) if size else "—"
    shown = "%d" % size if size else "gone"
    print("| `%s` | %d | %d | %d | %d | %s | %s |"
          % (rel, count, total, total // 4, mean, shown, ratio))
if once_files:
    print("| _%d file(s) read once_ | %d | %d | %d | | | |"
          % (once_files, once_files, once_bytes, once_bytes // 4))
print("| **Total** | **%d** | **%d** | **%d** | | | |"
      % (reads_total, grand, grand // 4))
print()
print("A floor, not a total: content also reaches the context through Bash")
print("(`cat`, `sed`, `grep`), through search results, and through the")
print("harness's automatic CLAUDE.md load. None of those are counted here.")
print()
print("These figures come from **this machine's** transcripts. A fresh clone,")
print("or another person's sessions, will report different realized numbers")
print("against the same predicted ones.")
PY
