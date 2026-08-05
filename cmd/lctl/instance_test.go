package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestMovingThePrincipalRefusesWithoutATarget.
//
// The check runs before the configuration is loaded and before a pool is opened,
// which is the whole of what makes it worth asserting: an operator who mistypes
// the flag on a box in trouble gets a sentence naming what is missing rather than
// a connection error, and nothing has been touched by the time they read it.
func TestMovingThePrincipalRefusesWithoutATarget(t *testing.T) {
	for _, args := range [][]string{nil, {"--to", ""}, {"--to", "   "}} {
		err := principalMove(args)
		if err == nil {
			t.Fatalf("principalMove(%q) succeeded; it must refuse before it opens "+
				"anything", args)
		}
		if !strings.Contains(err.Error(), "--to") {
			t.Errorf("principalMove(%q) refused with %q, which does not name the "+
				"flag that is missing", args, err)
		}
	}

	// The dispatch, so a subcommand nobody wrote is a usage error and not a
	// panic.
	for _, args := range [][]string{{}, {"nonsense"}, {"principal"}, {"principal", "nonsense"}} {
		if err := instanceCmd(args); err == nil {
			t.Errorf("instanceCmd(%q) succeeded", args)
		}
	}
}

// TestThePrincipalMoveIsGuardedTheWaySeedAndDemoAre.
//
// `lctl seed` and `lctl demo` both refuse under `APP_ENV=production` unless
// `--force`, because neither is something anybody should be able to do to a live
// instance by pressing up-arrow in the wrong terminal. Moving the instance
// principal is the third command in that class and the only one whose effect is
// on authority rather than on data.
//
// It is asserted as *all three carry it* rather than as *this one carries it*,
// because the guard's value is the habit: a fourth destructive subcommand arrives
// with the next milestone, and the failure this catches is the one where somebody
// reads `principalMove` for a pattern and finds the guard has been dropped from
// the two it was copied from.
//
// The scan is for a call to `IsProduction` inside each function body, which is
// the decision itself rather than the sentence around it — a reworded refusal
// message must not fail this, and a deleted check must.
func TestThePrincipalMoveIsGuardedTheWaySeedAndDemoAre(t *testing.T) {
	want := map[string]bool{"seedCmd": false, "demoCmd": false, "principalMove": false}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Failing rather than skipping, for the reason the sibling scan in
		// demo_prohibitions_test.go fails: a file the scan gave up on is the one
		// place a missing guard would hide.
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if _, watched := want[fn.Name.Name]; !watched {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "IsProduction" {
					want[fn.Name.Name] = true
				}
				return true
			})
		}
	}

	for fn, found := range want {
		if !found {
			t.Errorf("%s does not consult APP_ENV. Every destructive lctl subcommand "+
				"refuses on a production instance unless --force says otherwise, and "+
				"moving the instance principal is the one whose effect is on who "+
				"administers the box rather than on data.", fn)
		}
	}
}
