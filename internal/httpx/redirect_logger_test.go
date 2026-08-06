package httpx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// Nothing on the redirect path dereferences Logger directly.
//
// A nil *slog.Logger panics inside slog.(*Logger).Enabled, and the redirect
// handler's failure branches are exactly where a fixture that never set one
// meets it: the 503 taken when Postgres is unwell, and the sampled success line.
// M35 added log() for this reason and used it in the gate code, while the two
// pre-existing calls beside it kept dereferencing the field — which is F65, and
// which is the shape of defect an accessor is supposed to make impossible.
//
// So the accessor is not enough on its own; what makes it hold is that nothing
// may go round it. Production always sets Logger, so this cannot be caught by
// running the product — it is reachable from a fixture or an embedder of httpx,
// and the branch it lands on is the one that runs when the database is already
// in trouble.
func TestNothingOnTheRedirectPathDereferencesLoggerDirectly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "redirect") ||
			!strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			// Match x.Logger.Method(…) — a call whose receiver is a Logger
			// field selection rather than the log() accessor.
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			method, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			field, ok := method.X.(*ast.SelectorExpr)
			if !ok || field.Sel.Name != "Logger" {
				return true
			}
			pos := fset.Position(call.Pos())
			t.Errorf("%s:%d: .Logger.%s(…) dereferences the field instead of calling "+
				"log(). A nil Logger panics inside slog, and this path must degrade "+
				"rather than fail — that is F65. Use h.log().%s(…)",
				pos.Filename, pos.Line, method.Sel.Name, method.Sel.Name)
			return true
		})
	}

	if checked == 0 {
		t.Error("no redirect*.go files were scanned, so this test asserts nothing")
	}
}

// The accessor answers on a handler that was never given a logger, which is the
// ordinary state of a fixture that only cares about status codes.
func TestTheRedirectLoggerAccessorToleratesANilLogger(t *testing.T) {
	h := &RedirectHandler{}
	if got := h.log(); got == nil {
		t.Fatal("log() returned nil, which panics at the first call rather than at the second")
	}
	// Exercised rather than only compared: Enabled is the method that panics on
	// a nil receiver, and it is what every logging call reaches first.
	if h.log().Enabled(t.Context(), slog.LevelError) != slog.Default().Enabled(t.Context(), slog.LevelError) {
		t.Error("log() on a handler with no Logger does not behave like the default logger")
	}
}
