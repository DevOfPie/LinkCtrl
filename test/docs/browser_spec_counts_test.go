package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// caseCountWords is how the browser suite's README spells a case count.
//
// A number with no spelling here fails loudly rather than asserting nothing, on
// the same rule every anchored count in this repository follows: whoever adds the
// twelfth case writes the word, which is the same act as going to edit the row.
var caseCountWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
	"seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
	"thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16,
}

// statedCaseCount finds `**eleven cases**` in a README row.
var statedCaseCount = regexp.MustCompile(`\*\*([a-z]+) cases\*\*`)

// declaredTest matches a top-level or once-indented `test(` declaration, which is
// how every spec in this suite writes one.
var declaredTest = regexp.MustCompile(`(?m)^\s*test\(`)

// TestTheBrowserSuiteReadmeCountsItsCases is F252.
//
// `tools/agent-browser/README.md` states a spec's case count so that a list which
// has drifted "fails arithmetic rather than a reading" — a claim written to be
// checkable and checked by nobody. It was wrong by three when M50.8's fourth
// reopening read it, and two of the three predate that diff: the second and third
// reopenings each added a case and neither came back to the README. `make check`
// and `make verify-ui` both passed on a README naming the wrong number, because
// neither reads it.
//
// **What it costs is the one thing that file is for.** A reader who wants to know
// what the suite asserts is handed an inventory two cases short, and the missing
// ones are the assertions a later author is least likely to guess at.
//
// Here rather than in `scripts/check-links.sh`, for this package's own reason:
// check-links is not run by CI, and a gate that only runs on one machine is the
// F255 shape this repository has now paid for twice.
func TestTheBrowserSuiteReadmeCountsItsCases(t *testing.T) {
	// A relative path, like [decisionsPath] beside it: this package's tests run
	// from their own directory and the repository root is two levels up.
	const suite = "../../tools/agent-browser"
	readme := filepath.Join(suite, "README.md")
	body, err := os.ReadFile(readme)
	if err != nil {
		t.Fatalf("reading the browser suite's README: %v", err)
	}

	checked := 0
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		m := statedCaseCount.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		spec := specNamedIn(line)
		if spec == "" {
			t.Errorf("a README row states %q and names no spec file:\n  %s", m[0], line)
			continue
		}
		want, ok := caseCountWords[m[1]]
		if !ok {
			t.Errorf("%s's row says %q and this test has no spelling for it; add one "+
				"here, then edit the row", spec, m[0])
			continue
		}
		src, err := os.ReadFile(filepath.Join(suite, "specs", spec))
		if err != nil {
			t.Errorf("%s is named in the README and is not in specs/: %v", spec, err)
			continue
		}
		if got := len(declaredTest.FindAllString(string(src), -1)); got != want {
			t.Errorf("the README says %s holds %s cases and it declares %d. The count "+
				"is stated so a drifted list fails arithmetic rather than a reading, "+
				"which is what this test is (F252)", spec, m[1], got)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no README row states a case count, so this test read nothing. Either " +
			"the convention was dropped — in which case delete this test deliberately — " +
			"or the row format moved and the pattern above no longer matches it")
	}
}

// specNamedIn pulls `specs/foo.spec.mjs` out of a README row.
func specNamedIn(line string) string {
	i := strings.Index(line, "specs/")
	if i < 0 {
		return ""
	}
	rest := line[i+len("specs/"):]
	j := strings.Index(rest, ".mjs")
	if j < 0 {
		return ""
	}
	return rest[:j+len(".mjs")]
}
