package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// This file holds one structural fact about internal/store, in the same idiom
// cascade_test.go uses for its counts: read the source and assert what it says,
// because the property is about a signature rather than about a value any run
// produces.

// TestTheTemporaryNarrowingCannotFailALoad fences review finding 10.
//
// `restrictDatabaseTemp` documented itself — twice, at the function and at the
// call site — as *logged and not fatal*, while returning its REVOKE, GRANT and
// has_database_privilege errors into `EnsureAddonSchema`'s transaction. That
// aborts the load, and for a `required` add-on stops the instance. Only the
// non-owner case survived, and only because Postgres answers that one with a
// WARNING rather than an error.
//
// **Asserted on the signature rather than end to end, and that is a limit worth
// naming.** Provoking a genuine REVOKE failure needs a connecting role that is
// neither the database's owner nor a superuser; the integration fixture's role is
// a superuser, which bypasses the check entirely, so the failure is unreachable
// from there. What *is* reachable is the shape: a function returning nothing has
// no error for a caller to propagate, and the savepoint inside it is what makes
// that shape honest rather than a discarded error.
func TestTheTemporaryNarrowingCannotFailALoad(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "addons.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing addons.go: %v", err)
	}
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "restrictDatabaseTemp" {
			return true
		}
		found = true
		if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
			t.Errorf("restrictDatabaseTemp returns %d value(s). It documents itself as "+
				"logged and not fatal, and a returned error is what made that false: "+
				"EnsureAddonSchema propagates it and the add-on's load is aborted, "+
				"which for a required add-on stops the instance",
				len(fn.Type.Results.List))
		}
		return false
	})
	if !found {
		t.Fatal("restrictDatabaseTemp is not in addons.go; this test names the wrong " +
			"function and is asserting nothing")
	}
}
