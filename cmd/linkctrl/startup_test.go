package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Nothing on the startup path dials a relay this product does not depend on.
//
// This is finding F173 as a guard rather than as a memory. `sender.Verify`
// opens a TCP connection to the configured SMTP relay and, called inline in
// `run`, it ran *before* the HTTP listener bound — so a configured but
// unreachable relay kept the whole server dark for the whole of
// `LINKCTRL_SMTP_TIMEOUT`. Measured on the test instance on 2026-08-08 at
// **10.05 seconds**, which is the shipped default; at `SMTP_TIMEOUT=300s` the
// container never became healthy at all and `docker compose up --wait` gave up.
//
// A rolling deploy is where it bites, and it is why M56 owns the fix: every new
// replica is unready for the timeout at every start, and a health check whose
// start period is shorter fails it outright.
//
// The probe is a diagnostic. Nothing between it and `ListenAndServe` reads its
// result, and the mail outbox retries regardless of what it finds, so the only
// thing waiting for it bought was the order of two log lines.
//
// # What this cannot see
//
// One shape: a call whose selector is `Verify`, inside `func run`, not lexically
// inside a `go` statement. Moving the probe into a helper that `run` then calls
// synchronously would pass this test and reintroduce the defect — which is the
// cost of a syntactic guard, written down rather than discovered. What it does
// catch is the change that actually happened: somebody deleting a `go` because
// the sequential form reads more simply.
func TestTheSMTPProbeDoesNotGateTheListener(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	run := findFunc(file, "run")
	if run == nil {
		t.Fatal("no func run in main.go; this guard has stopped looking at the boot path")
	}

	// Every `go` statement's extent, so containment is a position test.
	type span struct{ from, to token.Pos }
	var detached []span
	ast.Inspect(run, func(n ast.Node) bool {
		if g, ok := n.(*ast.GoStmt); ok {
			detached = append(detached, span{g.Pos(), g.End()})
		}
		return true
	})

	found := 0
	ast.Inspect(run, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Verify" {
			return true
		}
		found++
		for _, d := range detached {
			if call.Pos() > d.from && call.End() < d.to {
				return true
			}
		}
		t.Errorf("%s: the relay probe is called on the startup path, not in a goroutine; "+
			"an unreachable relay now keeps this replica from serving redirects for the "+
			"whole of LINKCTRL_SMTP_TIMEOUT at every boot (F173)",
			fset.Position(call.Pos()))
		return true
	})

	if found == 0 {
		t.Error("no Verify call in run at all; either the boot-time relay check was " +
			"removed — in which case delete this guard deliberately — or it moved " +
			"somewhere this test cannot see it")
	}
}

func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}
