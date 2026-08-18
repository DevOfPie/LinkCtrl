package account

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// `users.status` has admitted three values since the first migration and, until
// M52, had a writer for none of them: every row carried the column default.
//
// This milestone gives two of them one. The third, `suspended`, deliberately
// stays without, and the reason it needs a test rather than a sentence is that a
// stated absence is invisible — the next person to read the enum finds three
// values, two of which the code writes, and has no way to tell whether the third
// is a feature they have not found yet or one nobody built. Suspension is a
// moderation feature nobody has asked for; it is not erasure, and it is not being
// smuggled in beside erasure because the enum happened to have a slot.
//
// The two halves are split by what they can see. Whether the enum still *admits*
// it is a fact about a live database and is asserted in
// test/integration/account_test.go. Whether anything *writes* it is a fact about
// the source, and reading the source is the only way to answer it — a runtime
// test can only ever show that nothing wrote it during that test.

// statusAssignment matches an assignment to the status column in a SQL
// statement: `status = 'deleted'`, `status='active'`, `SET status  =  'x'`.
var statusAssignment = regexp.MustCompile(`status\s*=\s*'([a-z]+)'`)

// statusInsert matches the INSERT that creates an account, whose status comes
// from a parameter rather than a literal. Named so the test below can say which
// path it is talking about rather than silently ignoring one.
const createUserStatusParam = "INSERT INTO users (id, email, name, password_hash, status, email_verified_at)"

func TestNothingWritesTheSuspendedStatus(t *testing.T) {
	dir := filepath.Join("..", "store", "query")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	// A guard on the scan itself. If the query directory moves or the regex
	// stops matching the one assignment that does exist, this test would pass by
	// finding nothing at all, which is the failure mode a source scan has.
	found := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		if strings.Contains(string(body), createUserStatusParam) {
			found["<parameter>"] = append(found["<parameter>"], e.Name())
		}
		for _, m := range statusAssignment.FindAllStringSubmatch(string(body), -1) {
			found[m[1]] = append(found[m[1]], e.Name())
		}
	}

	if files := found["suspended"]; len(files) > 0 {
		t.Errorf("something writes status = 'suspended', in %s. M52 states in "+
			"writing that nothing does, and docs/data-model.md repeats it. If "+
			"suspension is being built, that is a milestone with its own "+
			"definition of done — not a value that appears in a diff.",
			strings.Join(files, ", "))
	}
	if len(found["deleted"]) == 0 {
		t.Error("nothing writes status = 'deleted', so this scan is finding " +
			"nothing rather than finding no suspension. M52's SoftDeleteUser " +
			"writes it; either it is gone or the regex above has rotted.")
	}
	if len(found["<parameter>"]) == 0 {
		t.Errorf("CreateUser's INSERT no longer matches %q, so an account could "+
			"be created with any status and this scan would not see it",
			createUserStatusParam)
	}
}
