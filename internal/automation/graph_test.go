package automation_test

import (
	"go/build"
	"path/filepath"
	"strings"
	"testing"
)

// TestNothingOnTheRequestPathImportsTheEvaluator is how "evaluation runs on the
// leader-elected scheduler, never the request path" stops being a promise.
//
// m43.md's first hard claim is a *negative*, and a negative about where code
// runs cannot be asserted by running it — a test that calls Evaluate and watches
// it work says nothing about whether a handler could also call it. What can be
// asserted is that no package on a request path can reach the evaluator at all.
//
// The four listed below are every package a request touches: the router and its
// handlers, the redirect tier, the link service and the gate. None of them may
// import internal/automation, so there is no handler, no middleware and no link
// write that can start an evaluation even by accident. `cmd/linkctrl` is the one
// place that imports it, and its only call site is the job runner.
//
// go/build rather than golang.org/x/tools: reading a directory's import list is
// all this needs, it is in the standard library, and adding a dependency to
// assert a dependency is the wrong shape.
func TestNothingOnTheRequestPathImportsTheEvaluator(t *testing.T) {
	const evaluator = "github.com/DevOfPie/LinkCtrl/internal/automation"

	// Relative to internal/automation, which is where this test file lives.
	onTheRequestPath := map[string]string{
		"internal/httpx":    "../httpx",
		"internal/redirect": "../redirect",
		"internal/link":     "../link",
		"internal/gate":     "../gate",
	}

	for name, dir := range onTheRequestPath {
		abs, err := filepath.Abs(dir)
		if err != nil {
			t.Fatalf("resolve %s: %v", dir, err)
		}
		// ImportComment is off and tests are included: a *_test.go that imported
		// the evaluator would be harmless, but the package's own files are what
		// this is about, so both are read and the package files are what the
		// message names.
		pkg, err := build.ImportDir(abs, 0)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, imp := range pkg.Imports {
			if imp == evaluator {
				t.Errorf("%s imports %s. Evaluation is supposed to run on the "+
					"leader-elected scheduler and nowhere else; a package on the "+
					"request path that can reach the evaluator is one handler away "+
					"from running trigger matching, notification writes and link "+
					"archiving inside somebody's HTTP request. If a handler needs a "+
					"number from this package, move the number to internal/domain — "+
					"which is what AutomationInterval and AutomationTimeout are "+
					"doing there.", name, evaluator)
			}
		}
	}
}

// TestTheEvaluatorDependsOnNothingItCouldBeCalledBack.
//
// The other direction of the same claim. This package holds interfaces it
// declares itself for the archive, the notification and the webhook emit, so
// internal/link never imports it — which is what stops a link write reaching
// evaluation through a cycle the compiler would otherwise permit.
func TestTheEvaluatorDependsOnNothingItCouldBeCalledBack(t *testing.T) {
	abs, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve the package directory: %v", err)
	}
	pkg, err := build.ImportDir(abs, 0)
	if err != nil {
		t.Fatalf("read internal/automation: %v", err)
	}
	for _, imp := range pkg.Imports {
		if !strings.HasPrefix(imp, "github.com/DevOfPie/LinkCtrl/") {
			continue
		}
		switch imp {
		case "github.com/DevOfPie/LinkCtrl/internal/audit",
			"github.com/DevOfPie/LinkCtrl/internal/auth",
			"github.com/DevOfPie/LinkCtrl/internal/domain",
			"github.com/DevOfPie/LinkCtrl/internal/store/dbgen":
			// The vocabulary, the identity type the audit writer takes, the audit
			// writer itself, and the generated queries. None of them can reach
			// back here.
		default:
			t.Errorf("internal/automation imports %s. Every dependency of this "+
				"package is either the shared vocabulary or something that cannot "+
				"import it back; a service imported concretely is one that could "+
				"grow a call into the evaluator. Declare an interface here instead, "+
				"the way Archiver, Notifier and Emitter already do.", imp)
		}
	}
}
