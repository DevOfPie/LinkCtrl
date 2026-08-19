//go:build wasip1

// Command storage is the confinement fixture: it runs the hostile SQL m63.md's
// first risk names and states, from inside the guest, what the host answered.
//
// It exists because "confined to its own schema" is a claim about what a module
// *cannot* do, and a claim like that is only worth what the attempts against it
// are worth. The suite around it is adversarial by design: every check here is a
// statement that reached for something outside the add-on's schema and was
// refused, and the reaching is done through the published SDK, compiled the way a
// real add-on is compiled, so what it reports is what somebody else's module would
// see.
//
// Two ways it reports, the same two `probe` uses:
//
//   - every check logs one `storage: <check>=<outcome>` line through the ABI's own
//     log function, which a test reads back;
//   - a mismatch **panics**, which fails instantiation and therefore fails any
//     test that loads this module at all.
//
// **Every hostile statement is a single statement.** That is not incidental: the
// host parses through Postgres's extended protocol, so a payload carrying two
// commands is refused before either runs, and an escape that needed two would be
// testing the protocol rather than the boundary. The escapes here each fit in one
// — a DO block that resets the role, a schema-qualified name, a re-pointed search
// path, a SECURITY DEFINER function this add-on's own migration installed — and
// every one of them was measured against Postgres before it was written down.
//
// The manifest and the migrations this expects are installed by the test: the
// migration in migrations/ creates this add-on's own table and the SECURITY
// DEFINER function check `security_definer` calls.
package main

import (
	"errors"
	"strings"

	"github.com/DevOfPie/LinkCtrl/sdk"
)

func check(name string, ok bool, detail string) {
	outcome := "ok"
	if !ok {
		outcome = "MISMATCH: " + detail
	}
	_ = sdk.Log(sdk.LevelInfo, "storage: "+name+"="+outcome)
	if !ok {
		panic("storage: " + name + ": " + detail)
	}
}

// denied asserts that a statement was refused for want of privilege, which is
// what confinement looks like from in here.
//
// ErrDenied specifically, never "some error": a test that accepted any failure
// would pass against a typo, and the whole point of the host telling ErrDenied and
// ErrInvalid apart is that this fixture can insist on the first.
func denied(name, statement string) {
	_, err := sdk.StorageQuery(statement, nil)
	check(name, errors.Is(err, sdk.ErrDenied), "err "+errText(err))
}

func init() {
	// The add-on's own schema works, which is what makes every refusal below a
	// boundary rather than a broken host.
	err := sdk.StorageExec(`INSERT INTO notes (body) VALUES ($1)`, []byte(`["first"]`))
	check("own_insert", err == nil, "err "+errText(err))

	rows, err := sdk.StorageQuery(`SELECT body FROM notes ORDER BY id`, nil)
	check("own_select", err == nil && strings.Contains(string(rows), "first"),
		"got "+string(rows)+" err "+errText(err))

	// Arguments cross as a JSON array and come back as JSON objects keyed by column
	// name. Asserted because the shape is the ABI's contract and a host that changed
	// it would break every consumer silently.
	rows, err = sdk.StorageQuery(`SELECT $1::int AS n, $2::text AS s`, []byte(`[7,"x"]`))
	check("args_and_shape", err == nil && string(rows) == `[{"n":7,"s":"x"}]`,
		"got "+string(rows)+" err "+errText(err))

	// --- the hostile half ---------------------------------------------------

	// A schema-qualified read of the product's own tables. This is the one the
	// search path cannot stop — it is never consulted for a qualified name — so it
	// is the statement that proves privileges are the boundary.
	denied("qualified_product_read", `SELECT * FROM public.links`)
	denied("qualified_product_users", `SELECT * FROM public.users`)
	denied("qualified_product_sessions", `SELECT * FROM public.sessions`)

	// The same read hidden inside a CTE, because a boundary that only looked at the
	// top-level FROM would pass the three above and fail this.
	denied("cte_product_read", `WITH x AS (SELECT * FROM public.links) SELECT * FROM x`)

	// Re-point the search path and then read unqualified, inside one DO block so the
	// protocol has nothing to refuse. The SET succeeds — search_path is a setting
	// every role may change — and the read is still refused, which is the whole
	// difference between where a name resolves and what may be read.
	err = sdk.StorageExec(
		`DO $$ BEGIN EXECUTE 'SET search_path = public'; PERFORM * FROM links; END $$`, nil)
	check("repointed_search_path", errors.Is(err, sdk.ErrDenied), "err "+errText(err))

	// The escape that defeats SET ROLE and does not defeat a login role: one DO
	// block, resetting the role and then reading. Measured against Postgres 17 both
	// ways — as a SET ROLE session it reads the product's tables, as this role it
	// does not.
	err = sdk.StorageExec(`DO $$ BEGIN EXECUTE 'RESET ROLE'; PERFORM * FROM public.links; END $$`, nil)
	check("do_block_reset_role", errors.Is(err, sdk.ErrDenied), "err "+errText(err))

	// Becoming another role outright, both spellings. SET SESSION AUTHORIZATION is
	// refused for want of superuser — which the add-on's role is not, and which is
	// precisely what the *session* would have had if the host had issued SET ROLE on
	// its own connection instead of authenticating as this role. SET ROLE is refused
	// for want of membership.
	//
	// The target is `pg_read_server_files` rather than the application's own database
	// user, because a predefined role certainly exists: naming a user that might not
	// would let this check pass on "role does not exist", which is a refusal about
	// spelling and not about privilege.
	err = sdk.StorageExec(`SET SESSION AUTHORIZATION pg_read_server_files`, nil)
	check("session_authorization", errors.Is(err, sdk.ErrDenied), "err "+errText(err))
	err = sdk.StorageExec(`SET ROLE pg_read_server_files`, nil)
	check("set_role", errors.Is(err, sdk.ErrDenied), "err "+errText(err))

	// A SECURITY DEFINER function this add-on's own migration created. It runs as its
	// owner, and its owner is this add-on's role, so it escalates to nothing. The
	// function exists because the risk names it: DDL the host runs is DDL that could
	// otherwise have installed a way around the boundary.
	denied("security_definer", `SELECT peek_links()`)

	// Two statements in one payload. Refused by the extended protocol before either
	// runs, which is the second of the two answers `RESET ROLE` gets.
	_, err = sdk.StorageQuery(`SELECT 1; RESET ROLE`, nil)
	check("two_statements", err != nil, "err "+errText(err))

	// Writing through the read function. Refused by the transaction being READ ONLY
	// at the server, so which of the ABI's two storage functions a module called is a
	// fact rather than a description.
	_, err = sdk.StorageQuery(`INSERT INTO notes (body) VALUES ('via query')`, nil)
	check("query_cannot_write", err != nil, "err "+errText(err))

	// DDL against the product's schema, which is the migration-time attack arriving
	// at runtime instead.
	err = sdk.StorageExec(`CREATE TABLE public.evil (x int)`, nil)
	check("ddl_into_public", errors.Is(err, sdk.ErrDenied), "err "+errText(err))

	// Reading the server's files, and running a program on it. Both need roles this
	// one is not a member of.
	err = sdk.StorageExec(`COPY (SELECT 1) TO PROGRAM 'id'`, nil)
	check("copy_to_program", err != nil, "err "+errText(err))

	// An empty statement. Refused before it reaches Postgres, which would answer an
	// empty result rather than an error — so a module that built its SQL wrongly and
	// produced nothing learns that instead of reading a successful empty answer.
	_, err = sdk.StorageQuery("", nil)
	check("empty_statement", errors.Is(err, sdk.ErrInvalid), "err "+errText(err))

	// An argument the ABI does not carry. Refused as the guest's own fault rather
	// than guessed at, because whether an object meant jsonb or a record is not
	// knowable from the value.
	_, err = sdk.StorageQuery(`SELECT $1::text`, []byte(`[{"a":1}]`))
	check("object_argument", errors.Is(err, sdk.ErrInvalid), "err "+errText(err))
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func main() {}
