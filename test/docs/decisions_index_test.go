// Package docs holds tests over the repository's own documents, where the thing
// being asserted is a structural property a reader relies on and no compiler can
// see.
//
// It is a test rather than a step in scripts/check-links.sh because check-links
// is not run by CI — `ci-build` does not call it — and the defect this file
// exists for has now recurred once already while every anchor resolved.
package docs

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// decisionsPath is relative to this package's directory, which is where `go
// test` runs.
const decisionsPath = "../../docs/build-notes/decisions.md"

var (
	headingRE = regexp.MustCompile(`^#{1,6} +(.*)$`)
	anchorRE  = regexp.MustCompile(`\]\(#([^)]*)\)`)
	dropRE    = regexp.MustCompile(`[^a-z0-9 _-]`)
)

// slug renders a heading the way GitHub does: lowercase, drop everything that is
// not alphanumeric, space, hyphen or underscore, then each space becomes a
// hyphen.
//
// Runs of spaces are not collapsed. "2026-07-29 — Phase 1" loses its em-dash to
// the punctuation strip and keeps both surrounding spaces, so the anchor is
// "2026-07-29--phase-1" with two hyphens. This matches slugs() in
// scripts/check-links.sh, and collapsing here would reject correct anchors.
func slug(heading string) string {
	return strings.ReplaceAll(dropRE.ReplaceAllString(strings.ToLower(heading), ""), " ", "-")
}

// TestDecisionsIndexIsInFileOrder holds decisions.md's index to the rule its own
// header states in four words: *Newest last, matching the file.*
//
// This is F143 recurring as F179. F143 was closed by fixing the order and
// nothing was added that would notice it drifting again, so it drifted again —
// five rows by the time anybody looked, four of them from a single day. The
// motion that causes it is natural and invisible: appending an entry and then
// inserting its index row *near related rows* rather than at the end reads like
// tidying, and nothing in the tree objects.
//
// `make check-links` cannot see it, and that is not a gap in check-links. Every
// one of those five anchors resolved, and resolving is the whole of what a link
// check claims. Order is a different property and needs a different assertion.
//
// The failure message names both rows by their anchor, because an index row is
// three hundred characters wide and a line number alone sends the reader
// hunting.
func TestDecisionsIndexIsInFileOrder(t *testing.T) {
	body, err := os.ReadFile(decisionsPath)
	if err != nil {
		t.Fatalf("reading %s: %v", decisionsPath, err)
	}
	lines := strings.Split(string(body), "\n")

	// Where each heading sits. First occurrence wins, which is the same rule an
	// anchor follows: two identical headings resolve to the first.
	headingLine := make(map[string]int, len(lines)/8)
	for i, line := range lines {
		if m := headingRE.FindStringSubmatch(line); m != nil {
			if s := slug(m[1]); s != "" {
				if _, seen := headingLine[s]; !seen {
					headingLine[s] = i + 1
				}
			}
		}
	}

	// The index is the first table in the file: the contiguous run of table rows
	// after the first `| --- |` separator. Bounded deliberately — the body of the
	// file is full of tables whose cells carry anchors, and sweeping the whole
	// file would compare an entry's internal cross-reference against the index
	// and report an ordering defect that is not one.
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "| ---") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s has no table separator, so the index cannot be located", decisionsPath)
	}

	type row struct {
		line    int
		anchor  string
		heading int
	}
	var rows []row
	for i := start; i < len(lines) && strings.HasPrefix(lines[i], "|"); i++ {
		m := anchorRE.FindStringSubmatch(lines[i])
		if m == nil {
			t.Errorf("decisions.md:%d is an index row with no anchor link", i+1)
			continue
		}
		pos, ok := headingLine[m[1]]
		if !ok {
			// check-links.sh owns this failure and states it better; asserting it
			// here too costs nothing and keeps this test honest about what it
			// skipped.
			t.Errorf("decisions.md:%d anchors #%s, which matches no heading", i+1, m[1])
			continue
		}
		rows = append(rows, row{line: i + 1, anchor: m[1], heading: pos})
	}

	if len(rows) < 2 {
		t.Fatalf("found %d usable index rows; the index cannot have been located correctly", len(rows))
	}

	inversions := 0
	for i := 1; i < len(rows); i++ {
		if rows[i].heading < rows[i-1].heading {
			inversions++
			t.Errorf(
				"index out of file order at decisions.md:%d\n"+
					"  this row  #%s  anchors the entry at line %d\n"+
					"  the row above it (line %d) anchors the entry at line %d\n"+
					"  the index header says: Newest last, matching the file",
				rows[i].line, rows[i].anchor, rows[i].heading,
				rows[i-1].line, rows[i-1].heading)
		}
	}

	// Only on a clean run. Sabotaging this test to prove it fails showed the
	// summary printing "all in file order" underneath the errors saying it was
	// not, which is exactly the green-summary-over-a-red-run that workflow.md
	// calls the worst possible output.
	if inversions == 0 && !t.Failed() {
		t.Logf("%d index rows, all anchors resolved, all in file order", len(rows))
	}
}
