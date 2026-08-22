package store

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// This file makes one number countable, because it has now been wrong twice in
// the same way.
//
// **A soft delete fires no foreign key.** M52's account deletion is an `UPDATE`,
// so every `ON DELETE CASCADE` against `users` is inert and
// `DeleteAccountDependents` is what stands in for it. That statement is therefore
// a hand-maintained mirror of a set the schema defines elsewhere, and a table
// added to one and not the other leaves a standing credential behind a deleted
// account — which is the `password_resets` defect M52 was written out for, found
// again in M53's two tables and again in M65's one.
//
// Beside that set sit three prose counts, in `internal/account/account.go`, in the
// statement's own header and in `docs/SECURITY.md`. All were written by hand, two
// said *eight* when the answer was nine, and none had anything checking it. That
// is the same shape `TestAllActionsIsExhaustive` exists for in internal/audit: a
// number stated where nothing counts.
//
// So this counts. It reads the migrations for the set, reads the statement for
// what it deletes, compares them both ways, and holds the three sentences to the
// answer.
//
// # Why this is a parse, and what keeps a parse honest
//
// There is no schema to ask at `go test` time — this package's unit tests run
// with no database — so the set has to be read out of the migration files, and
// reading SQL with regular expressions is exactly the mechanism that lets a check
// quietly stop checking. The first version of this file did: it matched
// `REFERENCES users(id) ON DELETE CASCADE` on **one line**, attributed it to the
// last `CREATE TABLE` it had seen, and dropped anything it could not follow
// **silently**. A reviewer mutation-tested it and five ordinary spellings passed
// straight through, including `ALTER TABLE … ADD COLUMN … REFERENCES users(id) ON
// DELETE CASCADE`, which is this repository's own style in three shipped
// migrations, and `REFERENCES public.users(id)`, which is the same statement
// written out.
//
// The repair is not a better regular expression. It is that **nothing is dropped
// quietly**: the parse works on whole statements rather than lines, so where a
// clause wrapped is not part of the claim; every cascade it finds must be
// attributed to a named table or the test **fails**, naming the file and the
// statement; and a statement that mentions `users` and `ON DELETE CASCADE` and
// yields no cascade **fails** too, because that is precisely the shape of a
// spelling this parse has not learned. Total parse failure was always loud.
// Partial parse failure is what this file was blind to, and it is the defect it
// exists to catch, wearing its badge (D306).
//
// `cascadeFloor` is the last of it: a floor, so a parse that stops understanding
// the whole schema at once cannot report an empty set as agreement.

// cascadeFloor is how many cascading *keys* this walk has found since M65.
//
// Keys rather than tables, because a table may reference `users` more than once
// and each such column is a separate thing the deletion statement owes a
// predicate for. It is nine either way today — no table here cascades twice —
// and the distinction is what stops the floor being satisfied by a walk that
// found the same table twice and the one beside it not at all.
//
// It is not a count anybody has to maintain upward — a key added to the schema
// and to the statement passes without touching it. It fails when the walk finds
// *fewer*, which is either a key that deliberately stopped cascading, in which
// case move this number and say why in the commit, or a parse that has stopped
// reading migrations it used to read. Those two are indistinguishable from the
// test's side, which is the reason for asking.
const cascadeFloor = 9

// refUsers is a foreign key naming the accounts table.
//
// The column list is optional because Postgres makes it optional — `REFERENCES
// users` alone targets the primary key — the schema qualifier is allowed because
// `public.users` is the same table written out, and the closing `\b` is what stops
// this matching `users_something`.
var refUsers = regexp.MustCompile(`(?i)REFERENCES\s+(?:public\s*\.\s*)?"?users"?\b(?:\s*\(\s*"?id"?\s*\))?`)

// onDeleteCascade is the referential action that makes such a key one this
// statement has to stand in for.
var onDeleteCascade = regexp.MustCompile(`(?i)\bON\s+DELETE\s+CASCADE\b`)

// createTable and alterTable are the two statements that can attach a foreign key
// to a table, and the capture is the table it attaches to.
//
// Digits belong in the character class: `mfa_pending_logins` has none but
// `oauth2_tokens` would, and a table this parse cannot name is a table it drops.
var (
	createTable = regexp.MustCompile(`(?i)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:public\s*\.\s*)?"?([a-z_][a-z0-9_]*)"?`)
	alterTable  = regexp.MustCompile(`(?i)^\s*ALTER\s+TABLE\s+(?:ONLY\s+)?(?:IF\s+EXISTS\s+)?(?:public\s*\.\s*)?"?([a-z_][a-z0-9_]*)"?`)
)

// deleteFrom is one CTE of DeleteAccountDependents, read for the table alone.
var deleteFrom = regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+(?:ONLY\s+)?(?:public\s*\.\s*)?"?([a-z_][a-z0-9_]*)"?\b`)

// deleteCTE is the same statement read for the table **and the column its
// predicate binds to the account**, which is the half the table name alone says
// nothing about.
//
// The alias group is optional and cannot be `WHERE`, because a CTE may be written
// with or without one and a pattern that required an alias would silently read
// zero CTEs out of a statement that dropped them.
var deleteCTE = regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+(?:ONLY\s+)?(?:public\s*\.\s*)?"?([a-z_][a-z0-9_]*)"?\s+(?:AS\s+)?(?:"?([a-z_][a-z0-9_]*)"?\s+)?WHERE\s+(?:"?[a-z_][a-z0-9_]*"?\s*\.\s*)?"?([a-z_][a-z0-9_]*)"?\s*=\s*@account_id\b`)

// foreignKeyColumn reads a table-level constraint's column: `FOREIGN KEY
// (user_id) REFERENCES users …`. No migration here writes one today, and it is
// read anyway, because the alternative is a loud failure on a spelling Postgres
// documents first.
var foreignKeyColumn = regexp.MustCompile(`(?i)FOREIGN\s+KEY\s*\(\s*"?([a-z_][a-z0-9_]*)"?\s*\)\s*$`)

// identifier is a bare column name, quoted or not.
var identifier = regexp.MustCompile(`^"?([a-z_][a-z0-9_]*)"?$`)

// notAColumn is the words a definition can open with that are never the name of
// the column being defined. A clause landing on one of these is a spelling this
// walk has not learned, and it says so rather than reporting the keyword as a
// column name.
var notAColumn = map[string]bool{
	"alter": true, "create": true, "table": true, "add": true, "column": true,
	"constraint": true, "foreign": true, "primary": true, "unique": true,
	"check": true, "exists": true, "only": true, "if": true, "not": true,
	"public": true, "with": true,
}

// cascadingTable is one cascading key: a table that cascades from `users`, and
// the column the cascade is declared on.
//
// The column is here because the statement is a *mirror* of the cascade, and a
// mirror is only one if it deletes by the same key. Without it this walk read
// `DELETE FROM addon_identity_links il WHERE il.id = @account_id` as agreement —
// a CTE deleting the row whose primary key happens to equal an account id, which
// is no rows for every account — and both tests stayed green, because a table
// name is all either of them compared.
//
// **One entry per key, not per table**, and that is the second half of the same
// repair. Both walks used to keep the first cascade they saw for a table and skip
// the rest, so a table with two columns cascading from `users` was collapsed to
// whichever one came first in file order — and the deletion statement then had to
// delete by that column to be green, while the rows matching only the other one
// survived the account. Neither choice was right, and nothing said a choice was
// being made. This schema is one migration away from the shape: `invitations`
// already carries `invited_by` and `redeemed_by`, both to `users`, and only their
// `ON DELETE SET NULL` keeps them out of this set today. A table with two
// cascading keys now needs *both* deleted by, because a cascade deletes on either.
type cascadingTable struct {
	table  string
	column string
}

// keySet collects cascading keys, dropping an exact repeat and keeping a second
// key on the same table.
//
// Shared by both walks and by [TestATableCascadingOnTwoKeysOwesTwoPredicates], so
// that the one line deciding what counts as a repeat is the one line the test
// drives. It used to be written out twice as `seen[table]`, and being written
// twice is how both halves of the comparison came to collapse a table in the same
// direction with nothing comparing them.
type keySet struct {
	seen map[string]bool
	out  []cascadingTable
}

func (k *keySet) add(table, column string) {
	if k.seen == nil {
		k.seen = map[string]bool{}
	}
	if k.seen[table+"."+column] {
		return
	}
	k.seen[table+"."+column] = true
	k.out = append(k.out, cascadingTable{table: table, column: column})
}

// anyReference is any foreign key target at all, so a reference to `users` written
// in a way refUsers does not read can be found and reported rather than skipped.
var anyReference = regexp.MustCompile(`(?i)REFERENCES\s+(?:"?([a-z_][a-z0-9_]*)"?\s*\.\s*)?"?([a-z_][a-z0-9_]*)"?`)

// anyDelete is every `DELETE` keyword, so that one the pattern above could not
// read is counted rather than missed.
var anyDelete = regexp.MustCompile(`(?i)\bDELETE\b`)

// TestEveryCascadeToUsersIsInTheDeletionStatement.
//
// The two directions are not the same failure and both are worth naming. A table
// in the schema and not in the statement is the defect: rows outliving the
// account, and for four of these tables that means a credential outliving it. A
// table in the statement and not in the schema is a statement deleting from
// something that no longer cascades, which is either a table that stopped
// referencing `users` or a name that was renamed — harmless today and a sign the
// mirror has stopped being one.
func TestEveryCascadeToUsersIsInTheDeletionStatement(t *testing.T) {
	schema := tablesCascadingFromUsers(t)
	statement := tablesTheDeletionReaches(t)

	// Keyed by table, because the two directions below are about names; the keys
	// each table cascades on are the check after them and are compared as sets.
	cascadesOn, deletedBy := byTable(schema), byTable(statement)

	for _, table := range slices.Sorted(maps.Keys(cascadesOn)) {
		if _, ok := deletedBy[table]; !ok {
			t.Errorf("%s cascades from users and DeleteAccountDependents does not delete "+
				"from it. A soft delete fires no foreign key, so those rows outlive the "+
				"account — which for a table holding a credential is the deleted account "+
				"still signing in", table)
		}
	}
	for _, table := range slices.Sorted(maps.Keys(deletedBy)) {
		if _, ok := cascadesOn[table]; !ok {
			t.Errorf("DeleteAccountDependents deletes from %s and no migration gives it an "+
				"ON DELETE CASCADE against users. The statement is a mirror of that set; "+
				"a row in one and not the other means it has stopped being one", table)
		}
	}

	// **And it deletes by every key that cascades.** The two directions above
	// agree on names, which is what they always compared; a CTE naming the right
	// table and the wrong column deletes nothing and reports the same agreement.
	// Measured on this statement: `WHERE il.id = @account_id` in place of `WHERE
	// il.user_id = @account_id` left every unit test here green, and only an
	// integration test with the stack up could tell.
	//
	// **Every key, not the one that came first.** `ON DELETE CASCADE` fires on
	// whichever key matches, so a table referencing `users` twice loses a row when
	// *either* column names the deleted account — and a statement deleting by one
	// of them leaves the rows that matched only the other. There is no column to
	// choose here: the mirror owes a **CTE per key**, and that is the only shape
	// this walk reads. `WHERE a = @account_id OR b = @account_id` deletes the right
	// rows and is invisible to [deleteCTE], whose pattern ends at the first
	// predicate — so an author repairing a red this way lands on a red that says
	// the same thing again, which is worse than the gap.
	for _, missing := range keysNotDeletedBy(cascadesOn, deletedBy) {
		t.Error(missing)
	}
}

// keysNotDeletedBy is the comparison itself, taken out of the test above so that
// [TestATableCascadingOnTwoKeysOwesTwoPredicates] can drive it on a shape this
// schema does not have yet.
//
// A table in one set and not the other is the caller's to report; this answers
// only the tables both sets name, because a table the statement never deletes
// from has already been reported and saying it again once per column says nothing
// new.
func keysNotDeletedBy(cascadesOn, deletedBy map[string][]string) []string {
	var out []string
	for _, table := range slices.Sorted(maps.Keys(cascadesOn)) {
		by, ok := deletedBy[table]
		if !ok {
			continue
		}
		for _, column := range cascadesOn[table] {
			if slices.Contains(by, column) {
				continue
			}
			out = append(out, fmt.Sprintf("%s cascades from users on %s.%s and "+
				"DeleteAccountDependents deletes it by %v. Deleting by any other column "+
				"deletes the wrong rows or none, and a mirror of a cascade is only one "+
				"if it deletes by every key the cascade fires on. Add a CTE reading "+
				"DELETE FROM %s x WHERE x.%s = @account_id; an OR-ed second predicate "+
				"deletes the rows and this walk cannot see it, so it would leave you "+
				"here",
				table, table, column, qualified(table, by), table, column))
		}
	}
	return out
}

// byTable groups cascading keys by the table they are declared on, so that a
// table referencing `users` more than once is a set of columns rather than
// whichever one the walk saw first.
func byTable(in []cascadingTable) map[string][]string {
	out := map[string][]string{}
	for _, c := range in {
		out[c.table] = append(out[c.table], c.column)
	}
	for table := range out {
		sort.Strings(out[table])
	}
	return out
}

// qualified writes a table's columns the way the failure above reads them.
func qualified(table string, columns []string) []string {
	out := make([]string, 0, len(columns))
	for _, c := range columns {
		out = append(out, table+"."+c)
	}
	return out
}

// TestTheCountsAroundTheDeletionStatementAreTheRealOnes.
//
// Six sentences, in five files, each stating how many tables there are. None is
// decorative: the first is what a reader of internal/account is told about why a
// soft delete needs a statement at all, the second is how the statement's own
// header explains which tables are there for M52's reasons and which are there
// because leaving them would falsify a claim the schema already makes, the third
// is the disclosure page an operator reads to decide what ending an account
// removes, and the last two are the enumerations a person and an API client
// actually read — `docs/usage.md` and the OpenAPI description of the delete
// operation, both of which listed six of nine until M65.
//
// Held here rather than left to whoever last thought to check, because *whoever
// last thought to check* is exactly what produced the wrong number twice.
//
// **It shares the walk above and that is not independent evidence.** One hole in
// the parse disables both tests, which is why the loudness lives in the walk
// rather than in either test: this one cannot notice a table the walk never saw.
func TestTheCountsAroundTheDeletionStatementAreTheRealOnes(t *testing.T) {
	// **Tables, not keys.** The walk answers cascading *keys*, because a table may
	// reference `users` more than once and each column is a separate thing the
	// deletion statement owes a predicate for. Every sentence below says *tables*
	// and means tables — nine rows that go when an account does — so the count they
	// are held to is the distinct table names. The two numbers are equal today and
	// the day they stop being equal is the day taking the wrong one would put a
	// number in `docs/SECURITY.md` that no reader could reproduce by counting.
	total := len(byTable(tablesCascadingFromUsers(t)))
	// The four M52 enumerates by name, which is the fixed half of the split; the
	// rest are the ones later milestones added.
	const m52Tables = 4
	beyond := total - m52Tables

	for _, tc := range []struct {
		file, sentence string
		count          int
		// lead marks a word that opens its sentence, so the check is on the prose
		// as written rather than on a lowercase word a reader would not find.
		lead bool
	}{
		{
			file:     "../account/account.go",
			sentence: "%s tables declare `ON DELETE CASCADE` against",
			count:    total,
			lead:     true,
		},
		{
			file:     "../account/account.go",
			sentence: "says why %s are there beyond the four M52 names",
			count:    beyond,
		},
		{
			file:     "query/accounts.sql",
			sentence: "%s more are here because leaving them would",
			count:    beyond,
			lead:     true,
		},
		{
			// The claim this one repairs is its own: the sentence said the number
			// was counted rather than kept there, and nothing counted it.
			file:     "../../docs/SECURITY.md",
			sentence: "%s tables, and the number is counted rather than kept here",
			count:    total,
		},
		// **The two enumerations, which were six of nine.** Both list what a
		// deletion removes and both stopped at the six a person remembers: M53's two
		// credential tables were already absent and M65's `addon_identity_links`
		// widened the gap to three — a standing credential that signs somebody in
		// with no password, missing from the page that says what ending an account
		// removes. Completing a list buys nothing if nothing holds it complete, so
		// each now states the count and is held to it here.
		{
			file:     "../../docs/usage.md",
			sentence: "%s tables, and the list is the whole of it",
			count:    total,
		},
		{
			file:     "../../api/openapi.yaml",
			sentence: "%s tables, which is every one",
			count:    total,
			lead:     true,
		},
	} {
		src, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		// Flattened, so where a comment happened to wrap is not part of the claim.
		flat := strings.Join(strings.Fields(strings.ReplaceAll(
			strings.ReplaceAll(string(src), "--", " "), "//", " ")), " ")
		word := spelled(t, tc.count)
		if tc.lead {
			word = strings.ToUpper(word[:1]) + word[1:]
		}
		want := fmt.Sprintf(tc.sentence, word)
		if !strings.Contains(flat, want) {
			t.Errorf("%s does not say %q. The number moved and the sentence did not, "+
				"which is how it has been wrong twice", tc.file, want)
		}
	}
}

// spelled is how these files write a number, and a count with no word here fails
// loudly rather than asserting nothing.
//
// A table rather than a formatter because the check is on prose: every one of
// these sentences writes the number as a word, and a test that accepted digits
// would pass on a sentence no reader would write.
func spelled(t *testing.T, n int) string {
	t.Helper()
	words := map[int]string{
		3: "three", 4: "four", 5: "five", 6: "six", 7: "seven",
		8: "eight", 9: "nine", 10: "ten", 11: "eleven", 12: "twelve",
	}
	w, ok := words[n]
	if !ok {
		t.Fatalf("no spelling for %d. Add one here and to the sentences that state it; "+
			"the alternative is a test that quietly stops checking", n)
	}
	return w
}

// tablesCascadingFromUsers reads the schema rather than a list.
//
// Only the `Up` half of each migration: the `Down` halves drop these same tables,
// and a teardown is not a statement about the shape the product runs in.
//
// Every step that could drop something instead reports it. See the file comment.
func tablesCascadingFromUsers(t *testing.T) []cascadingTable {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no migrations were read; this test is not looking at what it thinks")
	}
	var found keySet
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range statementsIn(upHalf(string(src))) {
			// Every way this statement names `users` as a reference target was read
			// as one, or a spelling has appeared that this walk counts as nothing.
			// Loud, because a silent nothing here is the defect the file was found
			// to have: the answer is to teach refUsers, not to leave it counting a
			// subset it happens to understand.
			for _, unread := range unreadUsersReferences(stmt) {
				t.Errorf("%s names users as a foreign key target in a spelling this walk "+
					"does not read, so whatever it cascades counts for nothing: %q in %s",
					filepath.Base(file), unread, excerpt(stmt))
			}
			refs := cascadingRefs(stmt)
			if len(refs) == 0 {
				continue
			}
			table, ok := targetTable(stmt)
			if !ok {
				t.Errorf("%s has a cascade to users in a statement this walk cannot attribute "+
					"to a table, so it counts for nothing. Only CREATE TABLE and ALTER TABLE "+
					"are understood: %s", filepath.Base(file), excerpt(stmt))
				continue
			}
			for _, at := range refs {
				column, named := cascadeColumn(stmt, at[0])
				if !named {
					// Loud for the reason an unattributable cascade is: a column this
					// walk cannot name is a predicate it cannot check, and a check that
					// quietly stopped checking is what this file exists over.
					t.Errorf("%s cascades to users in %s and this walk cannot name the column "+
						"it is declared on, so nothing checks what the deletion deletes by: %s",
						filepath.Base(file), table, excerpt(stmt))
					continue
				}
				found.add(table, column)
			}
		}
	}
	if len(found.out) < cascadeFloor {
		t.Fatalf("the walk found %d keys cascading from users and the floor is %d. "+
			"Either a key deliberately stopped cascading — move cascadeFloor and say "+
			"why — or this parse has stopped reading migrations it used to read, which "+
			"is the failure it exists to make loud. Found: %v", len(found.out), cascadeFloor, found.out)
	}
	sort.Slice(found.out, func(i, j int) bool { return found.out[i].table < found.out[j].table })
	return found.out
}

// tablesTheDeletionReaches reads the statement, from the `-- name:` header that
// opens it to the one that opens whatever comes next.
//
// The header's own comment is stripped before anything is matched. It used not to
// be, and that is not a tidiness point: the header explains at length which tables
// are deleted and why, so a CTE removed from the statement while the paragraph
// about it stayed would have been read straight out of the prose.
func tablesTheDeletionReaches(t *testing.T) []cascadingTable {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("query", "accounts.sql"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(src), "-- name: DeleteAccountDependents")
	if !ok {
		t.Fatal("DeleteAccountDependents is not in query/accounts.sql under that name")
	}
	if before, _, split := strings.Cut(rest, "\n-- name: "); split {
		rest = before
	}
	stmt := flattenSQL(rest)

	var found keySet
	for _, m := range deleteCTE.FindAllStringSubmatch(stmt, -1) {
		found.add(strings.ToLower(m[1]), strings.ToLower(m[3]))
	}
	// Every DELETE the statement contains was read as one, or a CTE is deleting
	// from something this parse could not name and would be counted as absent.
	//
	// Three counts rather than two, because there are now three ways to lose a
	// CTE: a `DELETE` that is not a `DELETE FROM <table>`, and a `DELETE FROM
	// <table>` whose predicate this walk cannot read — the second being new with
	// the predicate check, and silent in exactly the direction that check exists
	// to close, since a CTE nothing parses is a table reported as never deleted.
	deletes := len(anyDelete.FindAllString(stmt, -1))
	tables := len(deleteFrom.FindAllString(stmt, -1))
	predicates := len(deleteCTE.FindAllString(stmt, -1))
	if tables != deletes {
		t.Errorf("the statement contains %d DELETE keywords and %d of them were read as "+
			"`DELETE FROM <table>`. The rest count for nothing, which is a table this "+
			"test would report as never deleted: %s", deletes, tables, excerpt(stmt))
	}
	if predicates != tables {
		t.Errorf("%d of the statement's %d `DELETE FROM <table>` clauses have a predicate "+
			"this walk reads as `<column> = @account_id`. A CTE whose predicate it cannot "+
			"read is a table it reports as never deleted, and the shape it understands is "+
			"the one every CTE here is written in: %s", predicates, tables, excerpt(stmt))
	}
	if len(found.out) < cascadeFloor {
		t.Fatalf("the statement was read for %d table-and-column pairs and the floor is "+
			"%d; either it deliberately stopped deleting by one — move cascadeFloor and "+
			"say why — "+
			"or this parse stopped reading it. Found: %v", len(found.out), cascadeFloor, found.out)
	}
	sort.Slice(found.out, func(i, j int) bool { return found.out[i].table < found.out[j].table })
	return found.out
}

// upHalf is everything before goose's Down marker, with the marker's own line
// dropped. Read off the raw text, because the marker is a comment and comments are
// what the next step removes.
func upHalf(src string) string {
	var up []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "-- +goose Down") {
			break
		}
		up = append(up, line)
	}
	return strings.Join(up, "\n")
}

// flattenSQL is statementsIn's answer for a fragment that is one statement: every
// comment gone, every run of whitespace one space.
//
// This is the whole answer to the split-clause hole: the old parse read a line at
// a time, so a foreign key written across two lines — which is how a formatter
// writes a long one — was invisible to it. It is also what keeps the deletion
// statement's own header comment out of the match, and that is not tidiness: the
// header explains at length which tables are deleted and why, so a CTE removed
// while the paragraph about it stayed used to be read straight out of the prose.
func flattenSQL(sql string) string {
	return strings.Join(statementsIn(sql), " ")
}

// statementsIn splits SQL into statements, and it is a scanner rather than a
// `strings.Split` on `;` because this schema contains dollar-quoted bodies whose
// semicolons belong to the body.
//
// It also drops line and block comments as it goes, which is what keeps prose out
// of the claim: a migration's header explains what it is doing and several of them
// use the words this walk matches on.
//
// **A single-quoted literal keeps its quotes and loses its body.** It used to keep
// both, and the file comment claimed the body was stepped over when only the
// comment scanner stepped over it — so `CREATE TABLE widgets (note text DEFAULT
// 'user_id uuid REFERENCES users(id) ON DELETE CASCADE')` was read as a cascade and
// attributed to `widgets`. Loud rather than silent, so it was a false red rather
// than a missed table, and a red nobody can act on is still a test that has stopped
// meaning what it says. Nothing this walk asks about is inside a literal — a table
// name, a referential action and a predicate are all syntax — so the body goes and
// the empty pair stays, keeping the statement parseable where a literal was an
// operand.
//
// **A dollar-quoted body is kept**, and that is the opposite decision for the
// opposite reason: `DO $$ … $$` is executable SQL, so a migration that adds a
// foreign key inside one is a migration this walk has to read. The cost is that a
// dollar-quoted *string* would be scanned as though it were code; no migration
// here writes one, and the failure mode is the loud one either way.
func statementsIn(sql string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.Join(strings.Fields(cur.String()), " "); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(sql); {
		switch {
		case strings.HasPrefix(sql[i:], "--"):
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				i = len(sql)
			} else {
				i += j
			}
			cur.WriteByte(' ')
		case strings.HasPrefix(sql[i:], "/*"):
			j := strings.Index(sql[i+2:], "*/")
			if j < 0 {
				i = len(sql)
			} else {
				i += 2 + j + 2
			}
			cur.WriteByte(' ')
		case sql[i] == '\'':
			j := i + 1
			for j < len(sql) {
				if sql[j] == '\'' {
					if j+1 < len(sql) && sql[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			// The quotes and not what was between them. See the comment above: a
			// literal's contents are somebody's text, and reading them as SQL is how
			// a `DEFAULT` string became a cascade against the table declaring it.
			cur.WriteString("''")
			i = j
		case sql[i] == '$':
			tag, ok := dollarTag(sql[i:])
			if !ok {
				cur.WriteByte(sql[i])
				i++
				continue
			}
			end := strings.Index(sql[i+len(tag):], tag)
			if end < 0 {
				cur.WriteString(sql[i:])
				i = len(sql)
				continue
			}
			stop := i + len(tag) + end + len(tag)
			cur.WriteString(sql[i:stop])
			i = stop
		case sql[i] == ';':
			flush()
			i++
		default:
			cur.WriteByte(sql[i])
			i++
		}
	}
	flush()
	return out
}

// dollarTag reads `$$` or `$name$` at the start of s.
func dollarTag(s string) (string, bool) {
	if len(s) == 0 || s[0] != '$' {
		return "", false
	}
	for j := 1; j < len(s); j++ {
		c := s[j]
		if c == '$' {
			return s[:j+1], true
		}
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return "", false
		}
	}
	return "", false
}

// cascadesToUsers counts the foreign keys in one statement that name `users` and
// cascade on delete.
//
// A reference is read together with the referential action that follows it, up to
// the comma that ends the clause, so `REFERENCES users(id) ON DELETE SET NULL` is
// correctly *not* a cascade and `REFERENCES users(id) ON UPDATE CASCADE ON DELETE
// CASCADE` correctly is.
func cascadingRefs(stmt string) [][]int {
	var out [][]int
	for _, loc := range refUsers.FindAllStringIndex(stmt, -1) {
		if onDeleteCascade.MatchString(clauseAt(stmt, loc[1])) {
			out = append(out, loc)
		}
	}
	return out
}

// clauseBefore is the front half of one column or constraint definition: from the
// separator that opens it to an offset inside it.
//
// [clauseAt]'s mirror, and paren-aware for the same reason in the other
// direction: a table-level `FOREIGN KEY (user_id) REFERENCES users` has a
// parenthesized column list between the separator and the reference, and stopping
// at its closing paren would cut the clause in half. Nothing precedes the
// definition in `ALTER TABLE … ADD COLUMN`, so the head of the statement comes
// back and [cascadeColumn] is what knows to look past it.
func clauseBefore(stmt string, to int) string {
	depth := 0
	for i := to - 1; i >= 0; i-- {
		switch stmt[i] {
		case ')':
			depth++
		case '(':
			if depth == 0 {
				return stmt[i+1 : to]
			}
			depth--
		case ',':
			if depth == 0 {
				return stmt[i+1 : to]
			}
		}
	}
	return stmt[:to]
}

// cascadeColumn names the column a cascading reference is declared on, or reports
// that this walk cannot read the spelling.
//
// Two spellings, and the second is read although this schema does not use it,
// because refusing to read a form Postgres documents would be a loud failure on
// correct SQL. A column-level key names its column first in its own definition;
// a table-level one names it in parentheses after `FOREIGN KEY`.
//
// It never guesses. A clause whose first word is a keyword rather than a name is
// reported as unread, which is the same treatment a cascade that cannot be
// attributed to a table gets and for the same reason: what this file cannot read,
// it must not silently count.
func cascadeColumn(stmt string, refStart int) (string, bool) {
	clause := strings.TrimSpace(clauseBefore(stmt, refStart))
	if m := foreignKeyColumn.FindStringSubmatch(clause); m != nil {
		return strings.ToLower(m[1]), true
	}
	words := strings.Fields(clause)
	// `ALTER TABLE t ADD COLUMN c uuid REFERENCES …` puts the statement's own head
	// in front of the definition, because there is no separator to stop at.
	for i := len(words) - 1; i >= 0; i-- {
		if strings.EqualFold(words[i], "add") || strings.EqualFold(words[i], "column") {
			words = words[i+1:]
			break
		}
	}
	if len(words) == 0 {
		return "", false
	}
	m := identifier.FindStringSubmatch(words[0])
	if m == nil || notAColumn[strings.ToLower(m[1])] {
		return "", false
	}
	return strings.ToLower(m[1]), true
}

// clauseAt is the rest of one column or constraint definition, from an offset to
// the comma that ends it.
//
// Paren-aware, because a composite key writes `REFERENCES users (a, b) ON DELETE
// CASCADE` and cutting at the first comma would stop inside the column list and
// read the referential action as absent — an under-count, and a silent one.
func clauseAt(stmt string, from int) string {
	depth := 0
	for i := from; i < len(stmt); i++ {
		switch stmt[i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return stmt[from:i]
			}
			depth--
		case ',':
			if depth == 0 {
				return stmt[from:i]
			}
		}
	}
	return stmt[from:]
}

// unreadUsersReferences returns every place a statement names `users` as a
// foreign key target that refUsers did not match.
//
// The two patterns differ on purpose: refUsers accepts the spellings this schema
// uses, and this one accepts any qualifier and any column list. What comes back is
// therefore the difference between *a reference to users exists here* and *this
// walk read it*, which is the only thing standing between a new spelling and a
// count that silently drops it.
func unreadUsersReferences(stmt string) []string {
	read := make(map[int]bool)
	for _, loc := range refUsers.FindAllStringIndex(stmt, -1) {
		read[loc[0]] = true
	}
	var out []string
	for _, loc := range anyReference.FindAllStringSubmatchIndex(stmt, -1) {
		table := ""
		if loc[4] >= 0 {
			table = strings.ToLower(stmt[loc[4]:loc[5]])
		}
		if table != "users" || read[loc[0]] {
			continue
		}
		out = append(out, stmt[loc[0]:loc[1]])
	}
	return out
}

// targetTable names the table a statement attaches its keys to.
func targetTable(stmt string) (string, bool) {
	if m := createTable.FindStringSubmatch(stmt); m != nil {
		return m[1], true
	}
	if m := alterTable.FindStringSubmatch(stmt); m != nil {
		return m[1], true
	}
	return "", false
}

// excerpt is enough of a statement to find it by, in a failure message that is
// asking somebody to go and look at it.
func excerpt(stmt string) string {
	const max = 160
	if len(stmt) <= max {
		return stmt
	}
	return stmt[:max] + "…"
}

// TestTheScannerReadsSyntaxAndNotSomebodysText.
//
// The walk above is only as good as what it calls a statement, and that has been
// wrong once in each direction. This pins the three cases, because none of them is
// visible in the migrations this repository ships today and all three are one edit
// away.
func TestTheScannerReadsSyntaxAndNotSomebodysText(t *testing.T) {
	t.Run("a literal is not SQL", func(t *testing.T) {
		// Measured against the parse before this was fixed: red, attributing a
		// cascade to `widgets`. A seed migration is exactly where a `DEFAULT`
		// string carrying SQL-shaped prose would appear, and this repository ships
		// one.
		const src = `CREATE TABLE widgets (` +
			`note text DEFAULT 'user_id uuid REFERENCES users(id) ON DELETE CASCADE')`
		stmts := statementsIn(src)
		if len(stmts) != 1 {
			t.Fatalf("read %d statements, want 1: %q", len(stmts), stmts)
		}
		if n := len(cascadingRefs(stmts[0])); n != 0 {
			t.Errorf("a literal's contents were read as %d cascade(s), so a string in a "+
				"DEFAULT clause can put a table into the set this file compares", n)
		}
		if got := unreadUsersReferences(stmts[0]); len(got) != 0 {
			t.Errorf("a literal's contents were read as a reference to users: %q", got)
		}
	})

	t.Run("a comment marker inside a literal is not a comment", func(t *testing.T) {
		// The claim the file comment made before the body was dropped, kept: the
		// scanner must still find the statement boundary after a literal holding a
		// `--`, or everything after one silently becomes one statement.
		stmts := statementsIn(`INSERT INTO t VALUES ('-- not a comment'); CREATE TABLE u (id uuid)`)
		if len(stmts) != 2 {
			t.Fatalf("read %d statements, want 2: %q", len(stmts), stmts)
		}
		if !strings.Contains(stmts[1], "CREATE TABLE u") {
			t.Errorf("the second statement is %q; the literal swallowed it", stmts[1])
		}
	})

	t.Run("a dollar-quoted body is still read", func(t *testing.T) {
		// The opposite decision to the first case, and deliberately: `DO $$ … $$` is
		// executable SQL, so a foreign key added inside one is one the schema has.
		const src = `DO $$ BEGIN ` +
			`ALTER TABLE widgets ADD COLUMN owner_id uuid REFERENCES users(id) ON DELETE CASCADE; ` +
			`END $$`
		stmts := statementsIn(src)
		found := 0
		for _, stmt := range stmts {
			found += len(cascadingRefs(stmt))
		}
		if found == 0 {
			t.Error("a cascade inside a DO block was read as nothing. A dollar-quoted body " +
				"is SQL the database runs, and dropping it is how a table joins the schema " +
				"without joining this walk")
		}
	})
}

// TestTheDeletionPredicateIsReadWithOrWithoutAnAlias.
//
// [deleteCTE]'s alias group is optional, and an optional group that can also match
// `WHERE` is the kind of pattern that reads zero CTEs the day somebody drops the
// aliases — which this file would report as the statement having stopped deleting
// from anything, or, past the floor, as agreement.
func TestTheDeletionPredicateIsReadWithOrWithoutAnAlias(t *testing.T) {
	for _, tc := range []struct{ name, sql, column string }{
		{"aliased", `DELETE FROM memberships m WHERE m.user_id = @account_id RETURNING 1`, "user_id"},
		{"AS aliased", `DELETE FROM memberships AS m WHERE m.user_id = @account_id RETURNING 1`, "user_id"},
		{"bare", `DELETE FROM memberships WHERE user_id = @account_id RETURNING 1`, "user_id"},
		{"qualified", `DELETE FROM public.memberships m WHERE m.user_id = @account_id RETURNING 1`, "user_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := deleteCTE.FindStringSubmatch(tc.sql)
			if m == nil {
				t.Fatalf("no CTE was read out of %q, so this shape counts as a table that "+
					"is never deleted from", tc.sql)
			}
			if m[1] != "memberships" || m[3] != tc.column {
				t.Errorf("read table %q column %q, want memberships/%s", m[1], m[3], tc.column)
			}
		})
	}
}

// TestATableCascadingOnTwoKeysOwesTwoPredicates.
//
// **This schema is one referential action away from the shape.** `invitations`
// references `users` twice — `invited_by` and `redeemed_by` — and only their `ON
// DELETE SET NULL` keeps them out of the set this file compares. Change either to
// `ON DELETE CASCADE` and the walk meets a table with two cascading keys for the
// first time.
//
// Before this test's fix both walks kept the first key per table and skipped the
// rest, so such a table was collapsed to whichever column came first in file
// order. The deletion statement then had to delete by *that* column to be green
// while the rows matching only the other one survived the account, and nothing
// anywhere said a choice was being made — which is the same failure as the
// table-name-only comparison this file already exists over, one level down.
//
// Driven from SQL text rather than from hand-built maps, so the case exercises
// the parse as well as the comparison: a walk that reads only one of the two
// `ADD COLUMN` clauses fails here too.
func TestATableCascadingOnTwoKeysOwesTwoPredicates(t *testing.T) {
	const schemaSQL = `ALTER TABLE widgets ` +
		`ADD COLUMN owner_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE, ` +
		`ADD COLUMN user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE`

	var schema keySet
	for _, stmt := range statementsIn(schemaSQL) {
		table, ok := targetTable(stmt)
		if !ok {
			t.Fatalf("the fixture's table could not be named out of %q", stmt)
		}
		for _, at := range cascadingRefs(stmt) {
			column, named := cascadeColumn(stmt, at[0])
			if !named {
				t.Fatalf("the fixture declares a cascade this walk cannot name a column "+
					"for, so the case below is not testing what it says: %q", stmt)
			}
			schema.add(table, column)
		}
	}
	cascadesOn := byTable(schema.out)
	if got := len(cascadesOn["widgets"]); got != 2 {
		t.Fatalf("the walk read %d cascading keys off a table declaring two (%v). A "+
			"table collapsed to one key is exactly the defect this test exists for, and "+
			"here it would hide it", got, cascadesOn["widgets"])
	}

	read := func(sql string) map[string][]string {
		var statement keySet
		for _, m := range deleteCTE.FindAllStringSubmatch(flattenSQL(sql), -1) {
			statement.add(strings.ToLower(m[1]), strings.ToLower(m[3]))
		}
		return byTable(statement.out)
	}

	t.Run("deleting by one key leaves the other's rows behind", func(t *testing.T) {
		deletedBy := read(`DELETE FROM widgets w WHERE w.user_id = @account_id`)
		missing := keysNotDeletedBy(cascadesOn, deletedBy)
		if len(missing) != 1 {
			t.Fatalf("want one unmatched key, got %d: %v. A statement deleting by one of "+
				"two cascading columns leaves every row that matched only the other, and "+
				"reporting nothing here is the walk choosing a column by file order",
				len(missing), missing)
		}
		if !strings.Contains(missing[0], "widgets.owner_id") {
			t.Errorf("the unmatched key is reported as %q and the column left undeleted is "+
				"widgets.owner_id", missing[0])
		}
	})

	t.Run("deleting by both is the mirror", func(t *testing.T) {
		deletedBy := read(
			`DELETE FROM widgets w WHERE w.user_id = @account_id; ` +
				`DELETE FROM widgets w WHERE w.owner_id = @account_id`)
		if missing := keysNotDeletedBy(cascadesOn, deletedBy); len(missing) != 0 {
			t.Errorf("a statement deleting by both cascading keys is the mirror and this "+
				"reports it as short: %v. If there is no pair of predicates that satisfies "+
				"the check, the check cannot be acted on", missing)
		}
	})

	// **The shape the failure message used to recommend, and no longer does.**
	//
	// `WHERE a = @account_id OR b = @account_id` deletes exactly the rows a mirror
	// owes — and [deleteCTE]'s pattern ends at the first `= @account_id`, so the
	// second disjunct is invisible and the walk still reports the key as missing.
	// The guidance therefore led an author from a red they could act on to a red
	// they could not, which is worse than the gap it was written about.
	//
	// This asserts the blind spot rather than repairing it: widening the pattern to
	// read disjunctions is a change to what *counts* as a mirror, on the statement
	// where being wrong is an account that still has credentials after it was
	// deleted, and it is not this milestone's. What the message now names is the
	// repair that works, and this is what keeps that claim a fact.
	t.Run("an OR-ed second predicate is invisible to the walk", func(t *testing.T) {
		deletedBy := read(
			`DELETE FROM widgets w WHERE w.user_id = @account_id OR w.owner_id = @account_id`)
		missing := keysNotDeletedBy(cascadesOn, deletedBy)
		if len(missing) != 1 || !strings.Contains(missing[0], "widgets.owner_id") {
			t.Fatalf("the walk read the OR-ed predicate as two keys: %v. It no longer has "+
				"the blind spot the failure message warns about, so the warning is now "+
				"wrong in the other direction and has to go", missing)
		}
		if !strings.Contains(missing[0], "Add a CTE reading") {
			t.Errorf("the failure names no repair that works:\n%s", missing[0])
		}
		if strings.Contains(missing[0], "OR-ed predicate this walk reads as two") {
			t.Errorf("the failure still recommends the shape it cannot see:\n%s", missing[0])
		}
	})
}
