package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestAllActionsIsExhaustive is what makes the audit vocabulary countable.
//
// The list is enumerated outside the code — docs/SECURITY.md states a coverage
// count, and a reader checks that claim by counting. The number has been wrong
// twice: twelve until M32.5 while omitting destination.blocked, and eighteen
// until 0.2.0 while the list had grown past it. Both times a hand-maintained
// number sat beside a list nothing checked, and F18 named the mechanical cause —
// two of the actions were declared in another package entirely, so anything
// enumerating from here was short by two whatever care was taken.
//
// So this parses the source rather than trusting the slice. Adding a constant
// and forgetting AllActions is a failing build here, not a documentation defect
// found two milestones later.
func TestAllActionsIsExhaustive(t *testing.T) {
	src, err := os.ReadFile("audit.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "audit.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}

	declared := map[string]string{}
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Action") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", name.Name, err)
				}
				declared[name.Name] = v
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("parsed no Action constants; this test is not reading what it thinks")
	}

	listed := AllActions()
	for name, value := range declared {
		if !slices.Contains(listed, value) {
			t.Errorf("%s (%q) is declared and missing from AllActions. Anything that "+
				"counts this vocabulary — docs/SECURITY.md included — is now wrong by "+
				"at least one, which is how that number has been wrong twice already",
				name, value)
		}
	}
	for _, value := range listed {
		if !slices.Contains(slicesValues(declared), value) {
			t.Errorf("AllActions lists %q, which no constant in this file declares", value)
		}
	}
	if len(listed) != len(declared) {
		t.Errorf("AllActions has %d entries against %d declared constants; a duplicate "+
			"in the list would pass every check above and still make the count wrong",
			len(listed), len(declared))
	}
}

func slicesValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
