package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// perReplicaJobs is the failover contract's exception list, in the tree.
//
// Two scheduled passes deliberately run on **every** replica rather than on a
// leader, and both are observations of shared state rather than work:
//
//   - reportJobStaleness publishes linkctrl_rollup_staleness_seconds. A follower
//     that never set it would report nothing, and whether an alert fired would
//     depend on which replica Prometheus happened to reach.
//   - runHostReload re-reads the verified-hostname set. Invalidation travels on
//     Redis pub/sub, which is at-most-once and which an instance with no Redis
//     does not have at all, so a follower that only reloaded under leadership
//     would serve a hostname that had stopped being verified (F73).
//
// Everything else is leader-elected, and an operator reading a metric that
// appears once per replica should be able to find out which is which from
// docs/operations.md without reverse-engineering the scheduler.
var perReplicaJobs = []string{"reportJobStaleness", "runHostReload"}

// TestOnlyTheDocumentedJobsRunOnEveryReplica keeps that list and the tree the
// same list.
//
// This is a count that has already drifted once. m56.md said **three** and named
// the audit-size gauge as the third; the gauge runs inside runMaintenance under
// advisoryLockKeyMaintenance, so it is leader-only and was on the wrong side of
// the exact distinction the sentence exists to draw. Nobody could have decided
// that a call wrapped in withLeadership runs everywhere — it was miscounted, by
// reading rather than by counting, which is what this test replaces.
//
// # What this cannot see
//
// A direct `j.withLeadership(` call in the method's own body. A pass that
// delegated its leadership to a helper would read as per-replica here and would
// need adding to the list with a reason, exactly as this test's failure message
// asks. That shape does not exist today: every one of the fifteen call sites in
// jobs.go is written inline in the pass that owns it.
func TestOnlyTheDocumentedJobsRunOnEveryReplica(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "jobs.go", nil, 0)
	if err != nil {
		t.Fatalf("parse jobs.go: %v", err)
	}

	var unguarded []string
	total := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		if !strings.HasPrefix(name, "run") && !strings.HasPrefix(name, "report") {
			continue
		}
		total++
		if !callsWithLeadership(fn) {
			unguarded = append(unguarded, name)
		}
	}

	// A floor, so a refactor that renamed every pass out of the run/report
	// convention cannot turn this guard into a test of an empty set.
	const passFloor = 8
	if total < passFloor {
		t.Fatalf("only %d scheduled passes matched run*/report* in jobs.go, want at least %d; "+
			"this guard has stopped seeing the scheduler", total, passFloor)
	}

	sort.Strings(unguarded)
	want := append([]string(nil), perReplicaJobs...)
	sort.Strings(want)

	if strings.Join(unguarded, ",") != strings.Join(want, ",") {
		t.Errorf("passes running on every replica are %v, documented as %v.\n"+
			"A pass that takes no advisory lock runs N times on N replicas. If that is "+
			"deliberate, add it here and to the per-replica paragraph in docs/operations.md "+
			"with the reason; if it is not, wrap it in j.withLeadership.",
			unguarded, want)
	}
}

func callsWithLeadership(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "withLeadership" {
			found = true
		}
		return !found
	})
	return found
}
