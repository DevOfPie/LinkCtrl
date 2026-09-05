package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
)

// TestTheDeprecationAnnouncementsAreEmitted exercises the two places
// docs/addon-abi.md's deprecation policy says a deprecation must appear in code:
// the SDK's generated `Deprecated:` marker, and the published table's status cell.
//
// **Neither was reached by any test** (F272). This package had no test files at
// all, and `check-generate` cannot stand in for one: it diffs committed output
// against `abi.Functions`, in which nothing is deprecated and nothing ever has
// been. So the machinery the policy promises would have run for the first time in
// the release that announced the first deprecation — which is the release where
// getting it wrong is most expensive and least recoverable, since the announcement
// is what starts the two-minor-versions-and-90-days window.
//
// The fixture is a function this ABI does not contain, deliberately: what is under
// test is the emitter, not the current contents of the list.
func TestTheDeprecationAnnouncementsAreEmitted(t *testing.T) {
	deprecated := abi.Function{
		Name: "scratch_function", Go: "ScratchFunction", Since: "0.1.0", Live: true,
		Doc:              "ScratchFunction does nothing and exists for this test.",
		Deprecated:       "Use ScratchFunctionV2 instead.",
		RemovedNotBefore: "0.3.0",
	}

	t.Run("the SDK's Deprecated marker", func(t *testing.T) {
		var b bytes.Buffer
		docComment(&b, deprecated)
		got := b.String()
		// gofmt and every Go tool read this by the exact prefix, so the assertion is
		// on the prefix rather than on the sentence: `// Deprecated: ` at the start of
		// a line in the doc comment is the whole of what makes an editor grey the call
		// out and `staticcheck` report it.
		if !strings.Contains(got, "// Deprecated: Use ScratchFunctionV2 instead.") {
			t.Errorf("the generated doc comment carries no Deprecated: marker:\n%s", got)
		}
		// Flattened, because `comment` wraps at 76 columns and where a sentence
		// breaks is not what is under test — the window's floor being named is.
		flat := strings.Join(strings.Fields(strings.ReplaceAll(got, "//", " ")), " ")
		if !strings.Contains(flat, "It may be removed in ABI 0.3.0 or later.") {
			t.Errorf("the marker does not name the window's floor:\n%s", got)
		}
	})

	t.Run("the published table's status cell", func(t *testing.T) {
		if got := statusFor(deprecated); got != "**live** · **deprecated**, removable in 0.3.0" {
			t.Errorf("the status cell is %q; a reader of the published table has to be "+
				"able to see that a live function is on its way out, and by when", got)
		}
	})

	// And the other direction, so this test fails if the emitters start announcing a
	// deprecation that was never declared: nothing in this ABI is deprecated today,
	// and every row must say so.
	t.Run("nothing currently declared is announced", func(t *testing.T) {
		for _, f := range abi.Functions {
			if f.Deprecated != "" {
				continue
			}
			if strings.Contains(statusFor(f), "deprecated") {
				t.Errorf("%s is not deprecated and its status cell says it is: %q",
					f.Name, statusFor(f))
			}
			var b bytes.Buffer
			docComment(&b, f)
			if strings.Contains(b.String(), "Deprecated:") {
				t.Errorf("%s is not deprecated and its doc comment carries the marker", f.Name)
			}
		}
	})
}
