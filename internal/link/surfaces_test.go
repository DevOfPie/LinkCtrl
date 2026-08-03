package link

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The bypass this file exists to close.
//
// A later milestone adds a surface that writes a destination — a routing rule's
// target (M34), a split-test variant (M36), a webhook URL (M42) — and calls
// ValidateDestination, because that is what every existing call site appears to
// do. It inherits Phase 1's SSRF refusals and silently skips every tier above
// them: no embedded list, no operator blocklist, no heuristics, no audit record
// of the attempt. Nothing fails, no test goes red, and the gap is invisible
// until somebody looks.
//
// The plan review found that ordering in two of three candidate sequences, which
// is why it is asserted here instead of written down as a rule. A source scan is
// an unusual test; the alternative is a comment nobody reads at the moment they
// would need to.

// validatorCallers are the only functions permitted to call the
// unappealable-tier validator directly. One entry, deliberately.
var validatorCallers = map[string]string{
	"Judge": "internal/link/blocking.go",
}

// judgeCallers are the functions permitted to ask for a verdict without an audit
// record being written.
//
// Two, and they are not the same kind of thing. checkDestination is the door
// every writing surface goes through, and it records. dispute.File asks because
// M31's whole question is "which tier refused this, and may it be appealed" —
// about a refusal that is already on record, so recording it again would
// double-count the numbers an operator tunes the heuristics against.
//
// A third entry is a claim that some new caller needs the verdict and not the
// record. That is occasionally true and usually a bug, which is why adding one
// costs an edit here.
var judgeCallers = map[string]string{
	"checkDestination": "internal/link/blocking.go",
	"File":             "internal/dispute/dispute.go",
}

// destinationSurfaces are the functions permitted to call checkDestination.
//
// Three as of M30, five as of M34, seven as of M36 — and every addition after
// the third is the case this test was written in advance for. Its own comment
// predicted both by name: "a later milestone adds a surface that writes a
// destination — a routing rule's target (M34), a split-test variant (M36)". Each
// time the test failed on the first run and the entries below were added
// deliberately rather than the check being loosened. M34 and M36 both declare
// M30 as a dependency and both assert the full tier check in their own tests —
// TestRuleDestinationsGoThroughEveryTier and
// TestVariantDestinationsGoThroughEveryTier.
var destinationSurfaces = map[string]string{
	"Create":          "internal/link/service.go",
	"Update":          "internal/link/service.go",
	"SetRootRedirect": "internal/link/domain_settings.go",
	"CreateRule":      "internal/link/routing.go",
	"UpdateRule":      "internal/link/routing.go",
	"CreateVariant":   "internal/link/split.go",
	"UpdateVariant":   "internal/link/split.go",
}

func TestEveryDestinationSurfaceGoesThroughTheCheck(t *testing.T) {
	direct := callersOf(t, "ValidateDestination")
	assertCallers(t, "ValidateDestination", direct, validatorCallers,
		"a caller that reaches the validator directly inherits the SSRF refusals "+
			"and skips the embedded list, the operator blocklist, the heuristics "+
			"and the audit record. Route it through Service.checkDestination.")

	judged := callersOf(t, "Judge")
	assertCallers(t, "Judge", judged, judgeCallers,
		"reaching Judge past checkDestination buys the tiers' verdict without the "+
			"`destination.blocked` record. A surface that writes a destination must "+
			"not do that; add it to destinationSurfaces instead.")

	surfaces := callersOf(t, "checkDestination")
	assertCallers(t, "checkDestination", surfaces, destinationSurfaces,
		"a new destination-writing surface must declare M30 as a dependency and "+
			"assert the full tier check in its own tests (m30.md, Coverage and logging). "+
			"Add it to destinationSurfaces once it does.")
}

// TestEmbeddedTierIsWrittenOnlyAtInit is the structural half of "heuristics
// never write into the embedded tier".
//
// The other half is in the type system — a heuristic has no field that could
// carry a tier — but a type cannot stop code from reaching into the map that
// holds the high-confidence list. This can: the list is built once, in init,
// from the embedded file, and any other assignment to it fails here.
func TestEmbeddedTierIsWrittenOnlyAtInit(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}

		var fn *ast.FuncDecl
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				fn = node
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					if !writesEmbeddedHosts(lhs) {
						continue
					}
					if fn == nil || fn.Name.Name != "init" {
						where := "package scope"
						if fn != nil {
							where = "func " + fn.Name.Name
						}
						t.Errorf("%s: embeddedHosts is assigned in %s. The "+
							"high-confidence tier is built once from "+
							"blocked_hosts.txt and never again — anything that "+
							"can add to it at runtime is a heuristic promoting "+
							"itself into the tier that costs a rebuild to overrule.",
							name, where)
					}
				}
			}
			return true
		})
	}
}

func writesEmbeddedHosts(lhs ast.Expr) bool {
	switch e := lhs.(type) {
	case *ast.Ident:
		return e.Name == "embeddedHosts"
	case *ast.IndexExpr:
		id, ok := e.X.(*ast.Ident)
		return ok && id.Name == "embeddedHosts"
	}
	return false
}

// callersOf returns "func name" → repo-relative file, for every non-test call to
// the named function anywhere in the module's own source.
func callersOf(t *testing.T, callee string) map[string]string {
	t.Helper()
	const root = "../.."

	found := map[string]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "node_modules", "dbgen":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		// Failing rather than skipping: every file walked here belongs to this
		// module, so one that will not parse is a broken tree, and skipping it
		// would let a bypass hide inside the file the scan quietly gave up on.
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		var fn *ast.FuncDecl
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				fn = node
			case *ast.CallExpr:
				if !callsNamed(node.Fun, callee) {
					return true
				}
				name := "(package scope)"
				if fn != nil {
					name = fn.Name.Name
				}
				rel, rerr := filepath.Rel(root, path)
				if rerr != nil {
					rel = path
				}
				found[name] = filepath.ToSlash(rel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return found
}

func callsNamed(fun ast.Expr, name string) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == name
	case *ast.SelectorExpr:
		return f.Sel.Name == name
	}
	return false
}

func assertCallers(t *testing.T, callee string, got, want map[string]string, why string) {
	t.Helper()
	for fn, file := range got {
		wantFile, ok := want[fn]
		if !ok {
			t.Errorf("%s is called from %s (%s), which is not a declared destination "+
				"surface.\n%s", callee, fn, file, why)
			continue
		}
		if wantFile != file {
			t.Errorf("%s is called from %s in %s, expected in %s", callee, fn, file, wantFile)
		}
	}
	var missing []string
	for fn := range want {
		if _, ok := got[fn]; !ok {
			missing = append(missing, fn)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%s is no longer called from %v. A surface that stopped checking "+
			"is the failure this test exists for; if the surface itself is gone, "+
			"remove its entry deliberately.", callee, missing)
	}
}
