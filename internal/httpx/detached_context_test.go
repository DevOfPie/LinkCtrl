package httpx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A detached context is never handed straight to a call that does I/O.
//
// context.WithoutCancel strips the deadline as well as the cancellation — that
// is what it is for, and the deadline is the half that is easy to forget. A call
// that detaches from the request and then waits forever is worse than one that
// inherits the request's cancellation: the goroutine outlives the client with
// nothing to stop it.
//
// Two rows are why this is a test and not a review habit. F101: one of the four
// detached Redis calls in internal/redirect had no WithTimeout beside it, while
// D26 leaves go-redis's MaxRetries at its default on the stated ground that
// "every Redis call site in this tree carries one". F129: /tls-check's write ran
// on a mux outside appHandler, where RequestTimeout lives, against pools with no
// statement_timeout — so it was bounded by nothing at all, on an unauthenticated
// route. Both were single sites in files where every neighbouring call did it
// right, which is the shape a scan catches and a reader does not.
//
// # What this cannot see
//
// It matches one shape: context.WithoutCancel(ctx) passed *directly* as an
// argument to a call that is not WithTimeout or WithDeadline. That is the shape
// both defects had. It does **not** follow a detached context through a
// variable, and two sites in internal/redirect use that form:
//
//   - hosts.go's Refresh assigns `base` and passes it to Reload, which
//     immediately re-detaches and applies hostLoadTimeout itself.
//   - invalidation.go assigns `base` and bounds it inside the goroutine that
//     uses it, with RedisTimeout.
//
// Both are correct, and both were read rather than assumed when this scan first
// flagged them. Following a variable properly needs dataflow, and a guard that
// half-follows one would report the same two sites forever until somebody
// silenced it — which is how a guard stops being read. The limit is written down
// here instead, so the next person adding a variable-form detach knows this test
// will not check it for them.
func TestNoDetachedContextIsHandedStraightToACall(t *testing.T) {
	// The packages that serve requests: this one because F129 was here, and
	// internal/redirect because F101 was there and the resolver is the busiest
	// detacher in the product.
	for _, dir := range []string{".", filepath.Join("..", "redirect")} {
		scanDetachedContexts(t, dir)
	}
}

func scanDetachedContexts(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	seen := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			outer, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			for _, arg := range outer.Args {
				inner, ok := arg.(*ast.CallExpr)
				if !ok || !isContextCall(inner, "WithoutCancel") {
					continue
				}
				seen++
				if isContextCall(outer, "WithTimeout", "WithDeadline") {
					continue
				}
				pos := fset.Position(inner.Pos())
				t.Errorf("%s:%d: a detached context is passed straight to %s. "+
					"context.WithoutCancel strips the deadline as well as the "+
					"cancellation, so this call is bounded by nothing — which is what "+
					"F101 and F129 both were. Wrap it: "+
					"context.WithTimeout(context.WithoutCancel(ctx), d)",
					pos.Filename, pos.Line, callName(outer))
			}
			return true
		})
	}

	if seen == 0 {
		t.Errorf("%s: this scan matched no context.WithoutCancel argument at all, so it "+
			"is asserting nothing. Either the tree stopped detaching contexts inline — "+
			"in which case delete this test rather than leaving it green — or the shape "+
			"moved and the scan no longer finds it, which is F130's failure one level up",
			dir)
	}
}

// isContextCall reports whether call is `context.<one of names>(…)`.
func isContextCall(call *ast.CallExpr, names ...string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "context" {
		return false
	}
	for _, n := range names {
		if sel.Sel.Name == n {
			return true
		}
	}
	return false
}

// callName renders the callee for a failure message, best effort.
func callName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok {
			return x.Name + "." + fn.Sel.Name
		}
		return fn.Sel.Name
	default:
		return "a call"
	}
}
