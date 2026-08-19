package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/pressly/goose/v3/lock"
)

// This file is the database half of an add-on's storage: the schema it owns, the
// role that confines it to that schema, the host-run migrations that build it,
// and the two statements the ABI lets it run.
//
// # The boundary is a login role, not a search path
//
// m63.md asks for confinement "with a role/search-path confined to its schema",
// and the two halves are not equal. A search path decides where an *unqualified*
// name resolves and stops there: `SELECT * FROM public.links` never consults it,
// so search_path alone confines nothing. Privileges are the boundary, and the
// search path is the convenience that makes an add-on's own tables reachable
// without writing its schema name twice.
//
// Privileges then only bind if the session *is* the confined role, which is why
// each add-on gets a LOGIN role of its own and its own pool authenticated as
// that role. `SET ROLE` from the application's session was measured and is not
// a boundary — two single statements escape it, and both were run against
// Postgres 17.10 rather than reasoned about:
//
//   - `DO $$ BEGIN EXECUTE 'RESET ROLE'; ... END $$` is one statement, so no
//     multiple-statement rule catches it, and it returns the session to the
//     application's own role;
//   - `SET SESSION AUTHORIZATION` is checked against the *session* user rather
//     than the current role, so it succeeds whenever the application connects as
//     a superuser — which `docker compose up` does by default.
//
// Authenticated as the role, both fail: RESET ROLE returns to the add-on's own
// role, and SET SESSION AUTHORIZATION is refused for want of superuser. That is
// the difference between a boundary and a convention, and it is the reason this
// file opens a second pool per add-on instead of issuing SET ROLE on the first.
//
// # What the role may reach
//
// Its own schema, which it owns, and the catalogues. Nothing of this product's:
// no migration in this package grants anything to PUBLIC, and Postgres grants no
// table privileges to PUBLIC by default, so a role created here starts with no
// access to any table outside the schema it owns. `pg_catalog` and
// `information_schema` stay readable — that is not optional in Postgres — so an
// add-on can see that this product's tables *exist* and read their column names.
// It cannot read a row of one. docs/SECURITY.md states that as a gap rather than
// leaving it implied.

// AddonSchemaPrefix is what every add-on's schema and role name begins with.
//
// One prefix for both, and the schema and the role are the *same identifier*:
// they live in different namespaces in Postgres, and using one string means the
// enumeration in [AddonSchemas] finds exactly the objects the confinement is
// about. It is also what makes an orphan detectable — a schema whose name starts
// with this and matches no loaded add-on.
const AddonSchemaPrefix = "addon_"

// addonNameRe re-states the bound addon.Manifest already validates, because this
// package interpolates the name into DDL as an identifier and DDL takes no
// parameters. A caller that validated is not a defence this file may rely on:
// the one place a schema name becomes SQL is the one place to check it.
var addonNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,30}$`)

// addonPasswordRe bounds the generated credential for the same reason. It is
// this file's own hex, so the check can be exact rather than escaping-based —
// and an escaping bug in a password literal is a role somebody else can log in
// as.
var addonPasswordRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrAddonDenied is a statement the database refused for want of privilege,
// which is what confinement looks like from the inside. Distinguished from every
// other SQL failure because it is the one the add-on's author can do nothing
// about: it means the statement reached outside the schema they own.
var ErrAddonDenied = errors.New("the statement reached outside the add-on's own schema")

// AddonStatementTimeout bounds one statement an add-on runs.
//
// Enforced by Postgres rather than by a context alone, because a context
// cancellation asks the server to stop and this makes the server stop itself. It
// is not configurable: an add-on's query is not on any latency budget yet — M66
// is what prices the redirect path — and a knob whose only effect is to let a
// misbehaving module hold a connection longer is a knob with one setting worth
// having.
const AddonStatementTimeout = 5 * time.Second

// AddonMaxConns is how much of the database one add-on may hold at once.
//
// Small, and deliberately not derived from the application pool's size: the
// point of a separate pool is that an add-on cannot starve the product of
// connections, and a limit that scales with the product's own would give that
// back. Four is enough for a module answering requests concurrently.
//
// **It is not outside the connection budget, and this comment said it was.**
// internal/config refuses `DB_MAX_CONNS + DB_REDIRECT_MAX_CONNS > 90` against the
// compose file's `max_connections = 100`, so ten storage add-ons at four apiece
// are forty connections that sum cannot see — exactly the re-planning of
// max_connections this once claimed to avoid. The defaults, 20 and 6, leave room
// for several; the guard's own ceiling leaves room for none. Teaching the guard
// and the two operator documents about add-on pools is F278.
const AddonMaxConns = 4

// AddonMaxResultBytes bounds the JSON one query may hand back.
//
// The host builds the whole result in memory before the guest sees any of it —
// the ABI's out-parameter convention has no streaming form — so this bounds what
// crosses to the guest: a megabyte is far more than a configuration read or a
// token lookup needs and small enough that a module looping over `SELECT * FROM
// big_table` fails rather than swells.
//
// **It is not a bound on the host's heap, and this comment claimed it was.**
// [encodeRows] checks it after pgconn has read a whole `DataRow` and after
// `rows.Values` and `json.Marshal` have materialized that row, so one row wider
// than this is allocated in full before the refusal — times [AddonMaxConns], and
// an add-on's own schema has no quota by design. Bounding the heap means refusing
// a row by its size before it is decoded, which is F276.
const AddonMaxResultBytes = 1 << 20

// AddonSchema is the schema an add-on owns, which is also the name of the role
// that reaches it.
func AddonSchema(name string) string { return AddonSchemaPrefix + name }

// checkAddonName refuses a name that must not become an identifier.
func checkAddonName(name string) error {
	if !addonNameRe.MatchString(name) {
		return fmt.Errorf("add-on name %q cannot be a schema name", name)
	}
	return nil
}

// EnsureAddonSchema creates the schema and the role an add-on is confined to,
// and returns the password its own pool must authenticate with.
//
// Idempotent, which is what makes a second boot and a re-load cheap: the role
// and the schema are created when absent and left alone when present. The
// password is **not** idempotent and that is the design — a fresh one is
// generated every time this is called, so the credential *this host* uses lives no
// longer than the process that uses it and nothing has to store it. There is
// nowhere to store it that would not be a new secret for an operator to manage.
//
// **That is a claim about the host's credential and not about the role's**, and it
// once read as both. Postgres lets any role change its own password and offers no
// way to forbid it: measured through [AddonDB.Exec]'s path as the confined role,
// `ALTER ROLE CURRENT_USER PASSWORD 'x'` is accepted and a session then
// authenticates with `x`, while `NOLOGIN`, `CONNECTION LIMIT 0` and a read of
// `pg_authid` are each refused. So a password an add-on set itself does outlive the
// process — it sits in `pg_authid` until the next load's `ALTER ROLE … PASSWORD`
// replaces it. `PASSWORD NULL` is accepted too, after which every connection gets
// 28P01 and [AddonDB.acquire] re-mints, so the availability half self-heals.
// F280 carries what the exposure is worth, which is more than this schema.
//
// On more than one replica that also means **the newest boot invalidates every
// other replica's credential**, which is why [AddonDB.acquire] re-mints on 28P01
// rather than treating a refused connection as the add-on's problem. D250 has the
// measurement and the two shapes that were rejected.
//
// Runs as the application's own database user, which therefore needs CREATEROLE
// (or superuser). docs/deployment.md names that requirement; an instance that
// cannot meet it cannot load an add-on that asks for storage, and the failure
// says so rather than degrading into an unconfined one.
//
// It also **clears every role-level setting** before pinning the search path, so
// a parameter the add-on set on its own role does not outlive the load that found
// it — the reason is at the statement.
//
// And it narrows the database once, which is the one statement here that is not
// about this add-on alone — see [restrictDatabaseTemp].
func EnsureAddonSchema(ctx context.Context, admin *pgxpool.Pool, name string, log *slog.Logger) (string, error) {
	if err := checkAddonName(name); err != nil {
		return "", err
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	schema := AddonSchema(name)

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate the %s role's password: %w", schema, err)
	}
	password := hex.EncodeToString(raw[:])
	if !addonPasswordRe.MatchString(password) {
		// Unreachable from hex.EncodeToString, and checked because the next line
		// interpolates it into DDL. A password that could carry a quote is a role
		// somebody else can log in as.
		return "", errors.New("generated an add-on password that cannot be written safely")
	}

	// One transaction, so a boot interrupted between the role and the schema
	// leaves neither. Postgres makes CREATE ROLE transactional, which is the only
	// reason this can be stated as an invariant rather than as an ordering.
	tx, err := admin.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, schema).Scan(&exists); err != nil {
		return "", fmt.Errorf("look for the %s role: %w", schema, err)
	}
	if !exists {
		// Every attribute stated, including the ones that are already the default.
		// The defaults are a property of the cluster's template role and of the
		// Postgres version, and this role's whole purpose is to hold nothing it was
		// not given.
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`CREATE ROLE %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT LOGIN`,
			schema)); err != nil {
			return "", fmt.Errorf("create the %s role: %w", schema, err)
		}
	}
	// Membership, so the application can own and later inspect the role's objects.
	// Postgres 16 and newer grant the creator membership automatically, so this is
	// for the role that outlived the boot that made it — and it is what fails
	// loudly if some other principal created a role with this name first.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`GRANT %s TO CURRENT_USER`, schema)); err != nil {
		return "", fmt.Errorf("grant the %s role to this database user: %w", schema, err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`ALTER ROLE %s LOGIN PASSWORD '%s'`, schema, password)); err != nil {
		return "", fmt.Errorf("set the %s role's password: %w", schema, err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`CREATE SCHEMA IF NOT EXISTS %s AUTHORIZATION %s`, schema, schema)); err != nil {
		return "", fmt.Errorf("create schema %s: %w", schema, err)
	}
	// Ownership is what lets the role run its own DDL, and IF NOT EXISTS above
	// changes nothing about a schema that already exists — including one created
	// by an earlier version of this code with a different owner.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER SCHEMA %s OWNER TO %s`, schema, schema)); err != nil {
		return "", fmt.Errorf("give %s ownership of its schema: %w", schema, err)
	}
	// Belt. PUBLIC's grant on schema public is what actually lets the role resolve
	// a name there, and this does not touch it — revoking that would change the
	// cluster for every other role. What it removes is any grant a previous
	// operator made to this role by hand, and it costs one statement to be sure
	// the boundary is not something somebody widened last year.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`REVOKE ALL ON SCHEMA public FROM %s`, schema)); err != nil {
		return "", fmt.Errorf("revoke %s's access to the public schema: %w", schema, err)
	}
	// Everything the role has been told about itself, cleared — then the one setting
	// this product chooses put back on top. Order is the whole of it: setting the
	// search path first and resetting afterwards would clear the pin as well.
	//
	// The add-on's role may `ALTER ROLE CURRENT_USER SET` any `PGC_USERSET`
	// parameter on itself, and there is no per-role deny for that any more than for
	// `EXECUTE` on `lo_from_bytea`. Measured through the write path as the confined
	// role: `work_mem = '4GB'` is accepted, lands in `rolconfig`, and every
	// connection the add-on's pool opens afterwards inherits it — a fresh session
	// read `4GB` — times [AddonMaxConns]. One `READ ONLY` query inside
	// [AddonStatementTimeout] then peaked a backend at **1.37 GB** resident against
	// 31 MB for the same query at the 4 MB default, which spills to disk instead.
	//
	// Nothing about it is an escalation, and that was checked rather than assumed:
	// `NOLOGIN`, `CONNECTION LIMIT 0` and `temp_file_limit` are each
	// refused to the role, and the two settings it can make that would otherwise
	// matter — `search_path`, `statement_timeout` — are beaten by [AddonDB.pin]'s
	// `SET LOCAL`. What it was is a setting the host never chose, surviving every
	// boot, because the load re-applied its own search path without clearing
	// anything else.
	//
	// **This is the only narrowing in this family that is conditional on neither
	// superuser nor database ownership** — the condition that makes
	// [restrictDatabaseTemp] a narrowing and made D248's revoke unavailable
	// altogether. `CREATEROLE` over the role is enough, which is what
	// docs/deployment.md already asks for: measured as a NOSUPERUSER CREATEROLE role
	// that does not own the database, against a role it had created and that had set
	// `work_mem` on itself. D253.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER ROLE %s RESET ALL`, schema)); err != nil {
		return "", fmt.Errorf("clear %s's role settings: %w", schema, err)
	}
	// The role's own default, so a connection that somehow arrives without the
	// runtime parameter below still resolves unqualified names inside the add-on's
	// schema rather than in public.
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`ALTER ROLE %s SET search_path = %s`, schema, schema)); err != nil {
		return "", fmt.Errorf("pin %s's search path: %w", schema, err)
	}
	// In the same transaction, so a boot that built the role built the narrowing
	// too. Its outcome is logged rather than fatal, for the reason the function
	// gives.
	if err := restrictDatabaseTemp(ctx, tx, schema, log); err != nil {
		return "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return password, nil
}

// restrictDatabaseTemp removes PUBLIC's TEMPORARY privilege on this database, so
// an add-on's role cannot own a relation in `pg_temp`.
//
// `PUBLIC` holds `TEMPORARY` on a database by default, and a temp table was the
// fourth way out of the confinement that an enumeration of places did not look
// for — the reason [AddonConfinementViolations] no longer enumerates. Measured
// through a faithful reproduction of [AddonDB.Exec]: `CREATE TEMP TABLE` is
// accepted, 51 MB went into one in a single statement inside
// [AddonStatementTimeout], and it survived across calls because a pgxpool is not
// a session. After this, five spellings are each refused with *permission denied
// to create temporary tables* — `CREATE TEMP TABLE`, `CREATE TABLE pg_temp.x`,
// `CREATE TEMPORARY TABLE … AS`, `CREATE UNLOGGED TABLE pg_temp.x`, and
// `SELECT … INTO TEMP`.
//
// **A narrowing, not the boundary**, and three measurements say why:
//
//   - It only takes effect when the application owns the database. As a
//     non-superuser CREATEROLE role that owns it, `has_database_privilege` goes
//     `t` to `f` silently; as a role that does not own it, `WARNING: no privileges
//     could be revoked` and the capability is intact — the silent no-op that made
//     D248's revoke unavailable. docs/deployment.md asks for CREATEROLE, not for
//     ownership, so this cannot be relied on.
//   - It does not survive the restore docs/deployment.md ships. `pg_dump -Fc`
//     without `--create` emits no `CREATE DATABASE` and no `GRANT`/`REVOKE … ON
//     DATABASE`, measured over the dump's own contents.
//   - It changes the database for every role, which is the objection
//     [EnsureAddonSchema] already states about `REVOKE ALL ON SCHEMA public`. A
//     per-role revoke is not available — `REVOKE TEMPORARY … FROM addon_x` leaves
//     `has_database_privilege` at `t`, because the privilege arrives through
//     `PUBLIC` and Postgres has no per-role deny. So an application sharing this
//     database and using temp tables loses them when a storage add-on is
//     installed; docs/deployment.md states that rather than leaving it to be
//     discovered.
//
// **The conditionality is shared with D248's revoke and is not shared by the
// `RESET ALL` in [EnsureAddonSchema]**, which is the one narrowing in this family
// that needs neither superuser nor ownership of the database — `CREATEROLE` over
// the add-on's own role is enough, and that is what docs/deployment.md already
// asks for. So it is the only one of the three that holds on every deployment
// shape this product documents, and the only one no restore silently undoes.
//
// The application grants itself back what PUBLIC lost, so the narrowing costs
// this product nothing now — nothing here creates a temp table — and costs it
// nothing later either.
//
// Failure is therefore logged and not fatal. What makes the confinement true is
// [AddonConfinementViolations], which reports a `pg_temp` relation whether this
// succeeded or not. D251.
func restrictDatabaseTemp(ctx context.Context, tx pgx.Tx, schema string, log *slog.Logger) error {
	var db string
	if err := tx.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		return fmt.Errorf("read the database name: %w", err)
	}
	// Sanitized rather than trusted: REVOKE takes an identifier and no parameter,
	// and the name comes from an operator's DSN.
	quoted := pgx.Identifier{db}.Sanitize()
	if _, err := tx.Exec(ctx, `REVOKE TEMPORARY ON DATABASE `+quoted+` FROM PUBLIC`); err != nil {
		return fmt.Errorf("revoke PUBLIC's temporary privilege on %s: %w", db, err)
	}
	// Back to the application only. Without this the revoke above would take the
	// capability from this product as well, and a future need for a temp table
	// would fail somewhere far from here.
	if _, err := tx.Exec(ctx, `GRANT TEMPORARY ON DATABASE `+quoted+` TO CURRENT_USER`); err != nil {
		return fmt.Errorf("grant the application temporary privilege on %s: %w", db, err)
	}
	// Measured rather than assumed. pgx surfaces a WARNING nowhere a caller can
	// read it, so the no-op case is detected by asking the catalogue what the
	// add-on's role can now do.
	var temp bool
	if err := tx.QueryRow(ctx,
		`SELECT has_database_privilege($1, current_database(), 'TEMP')`, schema).Scan(&temp); err != nil {
		return fmt.Errorf("check whether %s may still create temporary tables: %w", schema, err)
	}
	if temp {
		log.Warn("this database user does not own the database, so an add-on's role "+
			"can still create temporary tables; they are outside its schema and a "+
			"load will refuse the add-on for owning one",
			slog.String("database", db),
			slog.String("role", schema))
	}
	return nil
}

// addonConnConfig is the application's DSN, re-pointed at an add-on's own role.
//
// Derived from the configured DSN rather than assembled, so the host, the port,
// the database and every TLS setting an operator chose are inherited and there is
// no second connection string to configure. Only the credential and the search
// path differ, which is exactly the difference the confinement is.
func addonConnConfig(dsn, name, password string) (*pgx.ConnConfig, error) {
	if err := checkAddonName(name); err != nil {
		return nil, err
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// Not wrapped, for the reason openStdlib gives: the error text can echo the
		// DSN, password included.
		return nil, errors.New("parse database URL: invalid connection string")
	}
	schema := AddonSchema(name)
	cfg.User = schema
	cfg.Password = password
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["timezone"] = "UTC"
	cfg.RuntimeParams["search_path"] = schema
	cfg.RuntimeParams["application_name"] = "linkctrl-" + schema
	return cfg, nil
}

// addonLockID is the advisory lock one add-on's migrations serialize on.
//
// Per schema rather than goose's single default, so two replicas booting with
// three add-ons each do not queue behind one lock. It is a 63-bit hash of the
// schema name and it is **checked** against goose's default rather than argued to
// differ from it — a collision there would make an add-on's migrations serialize
// against the product's own, which is slow rather than wrong, and checking costs
// one comparison. The sign bit is cleared so the id is a positive bigint, which is
// what the advisory lock functions read.
func addonLockID(name string) int64 {
	sum := sha256.Sum256([]byte("linkctrl-addon-migrations:" + AddonSchema(name)))
	id := int64(binary.BigEndian.Uint64(sum[:8]) & 0x7fff_ffff_ffff_ffff) //nolint:gosec // G115: the sign bit is masked off on the line itself
	if id == lock.DefaultLockID {
		// Astronomically unlikely and not worth reasoning about: one increment, and
		// the claim above is a fact rather than a probability.
		id++
	}
	return id
}

// MigrateAddon applies an add-on's own migrations inside the schema it owns.
//
// The same discipline as [Migrate], deliberately: in-process, before the listener
// opens, and serialized across replicas by a Postgres session lock. What differs
// is who runs them. The connection authenticates as the add-on's role, so the
// DDL is bounded by the same privileges the add-on's queries are — a migration
// naming another schema is refused by Postgres rather than by a check this
// package would have to write, and a `SECURITY DEFINER` function it creates is
// owned by the add-on's role and therefore escalates to nothing.
//
// goose's bookkeeping goes in the add-on's schema too, which is what makes a
// re-load idempotent and an orphaned schema self-describing: the versions applied
// to it are inside it, so nothing about an add-on's state lives in a table the
// product owns.
func MigrateAddon(ctx context.Context, dsn, name, password string, fsys fs.FS, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg, err := addonConnConfig(dsn, name, password)
	if err != nil {
		return err
	}
	schema := AddonSchema(name)

	db := stdlib.OpenDB(*cfg)
	defer func() { _ = db.Close() }()

	locker, err := lock.NewPostgresSessionLocker(
		lock.WithLockID(addonLockID(name)),
		// The same five minutes the host's own migrations wait. A replica arriving
		// mid-migration should wait rather than fail into a crash loop.
		lock.WithLockTimeout(5, 60),
	)
	if err != nil {
		return fmt.Errorf("create the %s migration locker: %w", schema, err)
	}

	provider, err := goose.NewProvider(
		database.DialectPostgres,
		db,
		fsys,
		goose.WithSessionLocker(locker),
		// Schema-qualified, which goose supports: the version table is the add-on's,
		// in the add-on's schema, created by the add-on's role.
		//
		// **It is belt, not the mechanism**, and saying so is the point — sabotaging
		// this qualification alone left the test green, because the search path pinned
		// on the connection already puts an unqualified table in the add-on's schema
		// and goose's own existence check reads current_schema(). What makes the
		// placement true is the search path; what this adds is that it stays true if
		// somebody later changes how the search path is set.
		goose.WithTableName(schema+".goose_db_version"),
		goose.WithVerbose(false),
	)
	if err != nil {
		return fmt.Errorf("create the %s migration provider: %w", schema, err)
	}

	start := time.Now()
	results, err := provider.Up(ctx)
	for _, r := range results {
		log.Info("add-on migration applied",
			slog.String("addon", name),
			slog.String("schema", schema),
			slog.Int64("version", r.Source.Version),
			slog.String("name", r.Source.Path),
			slog.Int64("duration_ms", r.Duration.Milliseconds()),
		)
	}
	if err != nil {
		return fmt.Errorf("apply the %s migrations: %w", schema, err)
	}
	if len(results) > 0 {
		log.Info("add-on migrations complete",
			slog.String("addon", name),
			slog.String("schema", schema),
			slog.Int("applied", len(results)),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		)
	}
	return nil
}

// AddonSchemas is every schema in this database that belongs to an add-on.
//
// The enumeration m63.md's orphan bullet needs: subtract the loaded add-ons from
// this and what remains is data whose module is gone. Nothing here deletes
// anything — a purge is an operator's explicit act, and M68's flow.
//
// The underscore in the prefix is escaped, because in LIKE it is a wildcard and
// an unescaped one would also match a schema called `addonx`.
func AddonSchemas(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT nspname
		  FROM pg_namespace
		 WHERE nspname LIKE $1 ESCAPE '\'
		 ORDER BY nspname`, `addon\_%`)
	if err != nil {
		return nil, fmt.Errorf("list add-on schemas: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("list add-on schemas: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// AddonSchemaBytes is the on-disk size of one add-on's schema — every relation in
// it that has storage, with its indexes and its TOAST.
//
// Catalogue arithmetic, not a scan, for the reason [PartitionedTableBytes] is:
// this is a measurement taken on a schedule and it must stay cheap on the schema
// it matters most for.
//
// # The relkind filter excludes, it does not enumerate
//
// For the reason [AddonConfinementViolations] asks a shape: a list of the kinds
// that have storage is a denylist of every kind not on it, and this function kept
// one — `relkind IN ('r', 'm')` — one function away from where that argument was
// made. A **sequence** is `relkind 'S'`, lives in the add-on's own schema, holds an
// 8192-byte page from the moment it is created, and `pg_total_relation_size` of a
// table does **not** include a sequence that table owns. So a schema of nothing but
// sequences measured zero: 24,000 of them from three faithful reproductions of
// [AddonDB.Exec], well inside [AddonStatementTimeout], moved `pg_database_size` by
// 188 MB with this gauge reading 0 throughout. Found by M63's fourth review.
//
// **It was never only an adversary's case.** A `serial` or identity column carries a
// sequence, so for a **well-behaved** add-on the number an operator read was 8192
// bytes short per such column from the day the gauge shipped.
//
// Three kinds are excluded, each because counting it would double something already
// counted. An index — `'i'`, and `'I'` for the parent of a partitioned one — is
// inside `pg_total_relation_size` of its table. A TOAST relation, `'t'`, is inside
// its table's as well, and it lives in `pg_toast` rather than here, so excluding it
// is belt and braces. Everything else either has storage of its own or answers
// zero: a view, a composite type and a partitioned table each measure 0, and a
// partitioned table's answer does not include its partitions, which are counted as
// the ordinary tables they are.
//
// Measured before shipping, against a schema holding all of that at once: this sum
// equals `sum(pg_table_size(oid))` over every relation in the schema, which counts
// each relation exactly once and each index as itself rather than through its
// parent. TestSchemaSizeCountsEveryRelationWithStorage asserts that identity, and it
// bites in both directions — short if a kind with storage is dropped, long if
// anything is counted twice.
func AddonSchemaBytes(ctx context.Context, pool *pgxpool.Pool, name string) (int64, error) {
	if err := checkAddonName(name); err != nil {
		return 0, err
	}
	var bytes int64
	err := pool.QueryRow(ctx, `
		SELECT coalesce(sum(pg_total_relation_size(c.oid)), 0)::bigint
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1
		   AND c.relkind NOT IN ('i', 'I', 't')`, AddonSchema(name)).Scan(&bytes)
	if err != nil {
		return 0, fmt.Errorf("size of schema %s: %w", AddonSchema(name), err)
	}
	return bytes, nil
}

// AddonLargeObjects is how many large objects an add-on's role owns.
//
// The other half of *stored growth is visible by metric* — the two gauges cover
// data an add-on has stored and nothing else, which is the qualifier F279 exists
// under. *And nothing else* was the half that had to be earned: the claim also
// asserts the two are complete over stored data, and it was false while
// [AddonSchemaBytes] enumerated relation kinds instead of excluding the ones that
// double, because a sequence is stored data in the schema that the enumeration
// missed (D254).
//
// It is a count rather than a
// size because a size is not available: `pg_largeobject` holds the bytes and is
// readable by superusers only — measured as a non-superuser CREATEROLE role, the
// shape docs/deployment.md requires, `SELECT sum(length(data)) FROM
// pg_largeobject` answers *permission denied for table pg_largeobject*.
// `pg_largeobject_metadata` is readable, one row per object, so what an operator
// gets from this product is *how many* and the ceiling is what
// [AddonStatementTimeout] and [AddonMaxConns] make of it. docs/operations.md gives
// the superuser query for the bytes.
//
// Nonzero is a defect by construction: nothing in LinkCtrl creates a large object
// — measured, `pg_largeobject_metadata` is empty on both instances — and the ABI
// offers an add-on no way to want one. [AddonConfinementViolations] refuses such an
// add-on at its next load, so this gauge is what shows the growth *between* loads.
func AddonLargeObjects(ctx context.Context, pool *pgxpool.Pool, name string) (int64, error) {
	if err := checkAddonName(name); err != nil {
		return 0, err
	}
	var n int64
	err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_largeobject_metadata l
		  JOIN pg_roles r ON r.oid = l.lomowner
		 WHERE r.rolname = $1`, AddonSchema(name)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count %s's large objects: %w", AddonSchema(name), err)
	}
	return n, nil
}

// AddonConfinementViolations is everything about an add-on's schema that the
// confinement forbids, in three directions: what the role owns outside its own
// schema, what sits inside its schema that the role does not own, and what it has
// granted on its schema to anybody but itself.
//
// It should always be empty, and that is why it exists. Privileges are what
// confine an add-on's DDL, and this asks the catalogue whether they did rather
// than trusting that they did — a post-condition on a migration written by
// somebody else, run once per load. A non-empty answer refuses the add-on.
//
// # It asks a shape, not a list of places
//
// Three earlier versions of this function enumerated the places an add-on could
// own something — relations; then relations minus the schemas Postgres reserves;
// then large objects as well — and each round of review found a fourth. The last
// was a temp table: `PUBLIC` holds `TEMPORARY` on a database by default, a pooled
// connection is not a fresh session so the relation survives across ABI calls, and
// the `NOT LIKE 'pg\_%'` exclusion that TOAST had needed hid `pg_temp_N` too. A
// list of places is a denylist, and this is the same inversion to default-deny
// that D242 and D243 made to the log sanitizer, for the same reason.
//
// Postgres already knows the answer, in the two catalogues its own `DROP`
// statements consult, each authoritative for one direction:
//
//   - `pg_shdepend` is what `DROP OWNED BY` reads, so it is everything a role
//     owns, of whatever kind and wherever it lives. Measured on Postgres 17.10
//     against a role built statement for statement the way [EnsureAddonSchema]
//     builds one: a temp relation appears as `pg_class` / `pg_temp_N.name`, a
//     large object as `pg_largeobject` / oid, a function as `pg_proc`, the schema
//     itself as `pg_namespace` — and a TOAST relation appears **not at all**, zero
//     rows in the whole database, which is what lets the exclusion be deleted
//     rather than widened.
//   - `pg_depend`'s dependency on a namespace is what `DROP SCHEMA` reads, so it
//     is everything that lives in a schema.
//
// `pg_identify_object` turns a `(classid, objid, objsubid)` into a type, a schema
// and an identity, so *inside its own schema* is Postgres's judgement rather than
// a string comparison of this function's. Both catalogues and that function are
// readable by an ordinary role — measured as the add-on's own role, which holds
// nothing.
//
// # What the shape closes is every catalogued way out, and that is the claim
//
// Not *every* way out, and the difference is worth the paragraph, because the
// argument for asking a shape rather than keeping a list is what would otherwise
// be overclaimed. Both catalogues are catalogues of **objects**. Something that is
// in neither is not found here, and there is one measured case: a `WITH HOLD`
// cursor materialized at commit holds a temporary **file** for the life of the
// session — 553 MB of `base/pgsql_tmp` for one 600,000-row cursor inside
// [AddonStatementTimeout], measured through a faithful reproduction of
// [AddonDB.Exec], constant across samples and freed only when the backend ended.
// It needs no privilege, temp *files* are not temp tables so [restrictDatabaseTemp]
// does not reach it, and this function is empty while it sits on disk. It is
// transient rather than stored, which is why no gauge covers it and why this is a
// residual rather than a hole in the claim; the bound that would close it,
// `ALTER ROLE … SET temp_file_limit`, needs superuser — measured refused to a
// NOSUPERUSER CREATEROLE role — which is D251's shape rather than a boundary.
// F279 carries it.
//
// # The third direction is grants, and the schema's ACL is the choke point
//
// Ownership is not the whole of *no other add-on reads this*, because a grant is
// not an object and neither catalogue above records one. The two ownership
// directions catch only the sub-case where another add-on *creates* a relation in
// a schema it was granted `CREATE` on. Measured on Postgres 17.10 as two roles
// built statement for statement the way [EnsureAddonSchema] builds one: after
// `GRANT USAGE ON SCHEMA addon_a TO PUBLIC` and `GRANT SELECT, INSERT ON ALL
// TABLES IN SCHEMA addon_a TO PUBLIC` — one statement each through the write path,
// no privilege the role does not already hold over its own schema — the second
// role read and wrote the first's table, and this function's ownership half
// answered **zero rows**. `pg_roles` is readable, so the other add-on's role name
// is discoverable too.
//
// So the ACLs are read: `pg_namespace.nspacl` for the schema and `pg_class.relacl`
// for every relation in it, `aclexplode`d, and any grantee that is not the
// add-on's own role is a finding. `PUBLIC` is grantee 0 and has no `pg_roles` row,
// which is why the message coalesces the name.
//
// **It cannot come from `pg_shdepend` even though that catalogue looks right.** A
// `deptype = 'a'` row records a role *mentioned in an ACL*, but a grant to `PUBLIC`
// mentions no role and gets no row: measured in the same state, a column-level
// grant to the other add-on's role appeared as one `'a'` row and the two `PUBLIC`
// grants that actually leaked the data appeared not at all.
//
// **Two ACL columns and not five, because `USAGE` on the schema is necessary for
// every path.** With the table and column grants left in place and only
// `USAGE ON SCHEMA` revoked from `PUBLIC`, the other role's qualified read answers
// *permission denied for schema* — so nspacl is the gate every reach through has
// to pass, and relacl is where the privilege on the data itself lives. The columns
// not read are `pg_proc.proacl`, `pg_type.typacl` and `pg_attribute.attacl`, and
// reading them would buy less than it looks: `NULL` there is not *no grant*, it is
// *the default*, and the default for a function is `EXECUTE` to `PUBLIC` — the
// other role called `addon_a.f()` the moment it had schema `USAGE`, with `proacl`
// still `NULL`. An enumeration of ACL columns therefore cannot express *no other
// reader* at all while the gate is open, and closing the gate is what puts every
// one of them out of reach. A column-level grant is the same shape from the other
// side: it sets `attacl` and leaves `relacl` `NULL` — measured — so relacl is not
// complete over grants either, and the schema branch is what makes that not matter.
// A large object needs no schema, but a role that owns one is already reported by
// the ownership half, so its ACL is moot.
//
// **A load-time narrowing, not a boundary**, for the same reason
// [restrictDatabaseTemp] is. Postgres has no way to stop an owner granting on what
// it owns, and an add-on's data is the add-on's to give: what this adds is that the
// host *notices*, at the add-on's next load, and refuses it until an operator
// revokes. The cost is that a grant an operator made deliberately — a reporting
// role on an add-on's schema — refuses the add-on too, and for a `required` one
// stops the instance; nothing this product documents asks for such a grant, the
// finding names the privilege and the grantee, and the remedy is one `REVOKE`.
// D255.
//
// # The two ownership directions are not symmetric, and the asymmetry is measured
//
// `pg_shdepend` records **no row** for an object owned by the bootstrap superuser,
// and in the compose file's cluster the application *is* that role: 248 relations
// in `public`, zero `pg_shdepend` rows for `linkctrl`. So the inside direction
// cannot ask *who owns this* through `pg_shdepend` — in this very cluster the
// answer would be empty for the case it exists to catch. It asks what is in the
// schema and subtracts what the role owns instead, and the schema's own owner is
// read from `pg_namespace` directly. The outside direction is unaffected: an
// add-on's role is created by `CREATE ROLE` and is never pinned, so everything it
// owns is recorded — five rows for five objects, measured.
//
// Indexes are in neither set and need not be: `ALTER INDEX … OWNER TO` answers
// *cannot change owner of index*, and `ALTER TABLE … OWNER TO` moves the table's
// indexes and its owned sequences with it — measured both ways.
//
// # Why the inside direction is here at all
//
// `pg_dump` carries no roles; that is `pg_dumpall --roles-only`, and the restore
// procedure docs/deployment.md ships uses neither. Measured: dump a database, drop
// an add-on's role and schema, restore — the three `ALTER … OWNER TO` lines fail
// with *role does not exist*, the next boot's `ALTER SCHEMA … OWNER TO` repairs
// the schema, and **nothing re-owns the tables**. The add-on's role is then
// refused on its own rows, [MigrateAddon] fails on `goose_db_version`, and a
// `required` add-on stops the instance. Asking only the outside direction passes
// that state, which is why this asks both: the load then says what is wrong
// instead of failing inside goose. docs/deployment.md tells an operator to restore
// roles as well.
func AddonConfinementViolations(ctx context.Context, pool *pgxpool.Pool, name string) ([]string, error) {
	if err := checkAddonName(name); err != nil {
		return nil, err
	}
	schema := AddonSchema(name)
	// One statement for all three directions, so a load costs one round trip. `dbid = 0`
	// is a shared object — a database, a tablespace — which this role cannot create
	// and which is included anyway, because the point of the shape is that no
	// *catalogued* kind of object is left to be looked for later. What is not
	// catalogued is not covered, and the doc comment says which case that is.
	//
	// **The `identity IS NOT NULL` filters are the one place this fails open, and it
	// is deliberate.** `pg_identify_object` reads the syscache rather than the
	// statement's snapshot, so an object dropped between the catalogue read and the
	// lookup comes back with a NULL type, name and identity rather than an error —
	// measured. Without the filter the concatenation is NULL and the scan below
	// fails, which refuses the add-on for an object that no longer exists; a
	// `required` add-on would then stop the instance over a race with its own pool
	// reaping a connection. An object that is gone is not data outside the schema,
	// so dropping the row is the correct answer as well as the safe one.
	rows, err := pool.Query(ctx, `
		WITH role_oid AS (
			SELECT oid FROM pg_roles WHERE rolname = $1
		), owned AS (
			SELECT s.classid, s.objid, s.objsubid, o.type, o.schema, o.name, o.identity
			  FROM pg_shdepend s
			  CROSS JOIN LATERAL pg_identify_object(s.classid, s.objid, s.objsubid) o
			 WHERE s.deptype = 'o'
			   AND s.refobjid = (SELECT oid FROM role_oid)
			   AND s.dbid IN (0, (SELECT oid FROM pg_database WHERE datname = current_database()))
			   AND (o.identity IS NOT NULL OR o.name IS NOT NULL)
		), inside AS (
			SELECT d.classid, d.objid, d.objsubid, o.type, o.identity
			  FROM pg_depend d
			  CROSS JOIN LATERAL pg_identify_object(d.classid, d.objid, d.objsubid) o
			 WHERE d.refclassid = 'pg_namespace'::regclass
			   AND d.refobjid = (SELECT oid FROM pg_namespace WHERE nspname = $1)
			   AND (o.identity IS NOT NULL OR o.name IS NOT NULL)
		)
		SELECT 'it owns ' || w.type || ' ' || coalesce(w.identity, w.name) ||
		       ', which is not in ' || $1
		  FROM owned w
		 WHERE w.schema IS DISTINCT FROM $1
		   AND NOT (w.type = 'schema' AND w.name = $1)
		 UNION ALL
		SELECT 'it does not own ' || i.type || ' ' || i.identity ||
		       ', which is in ' || $1
		  FROM inside i
		 WHERE NOT EXISTS (
			SELECT 1 FROM owned w
			 WHERE w.classid = i.classid AND w.objid = i.objid AND w.objsubid = i.objsubid)
		 UNION ALL
		SELECT 'it does not own schema ' || n.nspname || ', which is its own'
		  FROM pg_namespace n
		 WHERE n.nspname = $1
		   AND n.nspowner IS DISTINCT FROM (SELECT oid FROM role_oid)
		 UNION ALL
		SELECT 'it has granted ' || a.privilege_type || ' on schema ' || $1 ||
		       ' to ' || coalesce(g.rolname, 'PUBLIC')
		  FROM pg_namespace n
		  CROSS JOIN LATERAL aclexplode(n.nspacl) a
		  LEFT JOIN pg_roles g ON g.oid = a.grantee
		 WHERE n.nspname = $1
		   AND a.grantee IS DISTINCT FROM (SELECT oid FROM role_oid)
		 UNION ALL
		SELECT 'it has granted ' || a.privilege_type || ' on ' || $1 || '.' || c.relname ||
		       ' to ' || coalesce(g.rolname, 'PUBLIC')
		  FROM pg_class c
		  CROSS JOIN LATERAL aclexplode(c.relacl) a
		  LEFT JOIN pg_roles g ON g.oid = a.grantee
		 WHERE c.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = $1)
		   AND a.grantee IS DISTINCT FROM (SELECT oid FROM role_oid)
		 ORDER BY 1`, schema)
	if err != nil {
		return nil, fmt.Errorf("check %s's confinement: %w", schema, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var finding string
		if err := rows.Scan(&finding); err != nil {
			return nil, fmt.Errorf("check %s's confinement: %w", schema, err)
		}
		out = append(out, finding)
	}
	return out, rows.Err()
}

// AddonDB is one add-on's confined connection to its own schema.
//
// Held for the life of the host, closed with it. The pool authenticates as the
// add-on's role — see this file's header for why nothing weaker is a boundary —
// and every statement runs inside a transaction that pins the search path and
// the statement timeout locally, so nothing a previous statement left on a
// pooled connection changes what the next one means.
type AddonDB struct {
	name   string
	schema string
	pool   *pgxpool.Pool
	log    *slog.Logger

	// admin is the application's own pool, held for one purpose: re-minting this
	// add-on's credential when another replica has rotated it. Nil in a test that
	// opened a connection without one, which then fails as it did before.
	admin *pgxpool.Pool

	// mu guards password, which every new connection reads through BeforeConnect.
	mu       sync.Mutex
	password string
	// refresh serializes the re-mint itself, so two calls failing at once produce
	// one ALTER ROLE rather than two that invalidate each other.
	refresh sync.Mutex
}

// Schema is the schema this connection is confined to.
func (a *AddonDB) Schema() string { return a.schema }

// OpenAddonDB opens an add-on's own pool and proves it works.
//
// The Ping is not a courtesy. Password authentication has to be available for
// the add-on's role — a deployment authenticating by peer or by IAM cannot offer
// it — and finding that out at boot is what lets a `required` add-on's failure
// class do its job. Discovering it at the add-on's first query instead would
// mean an instance that booted clean and then refused every call.
//
// `admin` is the application's own pool and it is what makes this survive a
// second replica. [EnsureAddonSchema] mints a fresh password on every load, so
// replica B booting invalidates the credential replica A is holding — measured:
// after `ALTER ROLE … PASSWORD`, a connection with the old one is refused with
// `FATAL: password authentication failed` (SQLSTATE 28P01). With [AddonDB.acquire]
// re-minting on exactly that code, A recovers on its next connection instead of
// failing quietly for the rest of its life. D250 records the alternatives and why
// this one needs no new secret and no new table.
func OpenAddonDB(ctx context.Context, admin *pgxpool.Pool, dsn, name, password string, log *slog.Logger) (*AddonDB, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("parse database URL: invalid connection string")
	}
	conn, err := addonConnConfig(dsn, name, password)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig = conn
	cfg.MaxConns = AddonMaxConns
	// Nothing held open. An add-on that is never called should cost the database
	// no connection at all, which is what makes installing one cheap.
	cfg.MinConns = 0
	// **Required, not a tuning choice.** The default caches a prepared statement
	// per distinct query text, and the query text here is written by a module: a
	// loop generating unique statements would grow the cache without bound on
	// every connection. The unnamed-statement mode also parses through the
	// extended protocol, which is what makes a multiple-statement payload a
	// server-side error instead of a batch — see AddonDB.Query.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec

	a := &AddonDB{
		name:     name,
		schema:   AddonSchema(name),
		log:      log,
		admin:    admin,
		password: password,
	}
	// Every connection reads the credential from the add-on's own state rather than
	// from the frozen config, so a re-mint reaches connections opened after it
	// without rebuilding the pool.
	cfg.BeforeConnect = func(_ context.Context, cc *pgx.ConnConfig) error {
		cc.Password = a.currentPassword()
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open the %s pool: %w", AddonSchema(name), err)
	}
	a.pool = pool
	// Through acquire rather than Ping, so a credential another replica rotated
	// between EnsureAddonSchema and here is re-minted instead of failing the load.
	probe, err := a.acquire(ctx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect as %s: %w", AddonSchema(name), err)
	}
	probe.Release()
	return a, nil
}

// releaseLocks drops every session-level advisory lock the guest's statement may
// have taken, before the connection can be used for anything else.
//
// **A boundary rather than tidiness.** `pg_advisory_lock` is `EXECUTE` to
// `PUBLIC`, this product's job leader-election keys are compile-time constants in a
// public repository, and a session-level lock is **not** released by the rollback
// [AddonDB.Query] and [AddonDB.Exec] perform — measured on both paths, the
// `READ ONLY` one included, which does not refuse the lock either. Without this an
// add-on takes the maintenance, rollup, dimension, mail, webhook, domain,
// automation or update-check lock and holds it on the pooled connection until
// pgxpool reaps it, on every replica at once because every replica uses the same
// key, and retakes it after. Revoking the family from `PUBLIC` was measured and is
// not available — D249.
//
// **Synchronous, and pgxpool's `AfterRelease` hook is not**, which is the whole
// reason this is a function here rather than one line of pool configuration:
// `pgxpool.Conn.Release` runs `AfterRelease` in a **goroutine**, so the release is
// unordered against the caller's next statement. The hook was the first
// implementation and the test caught it — the product asked for its own lock
// immediately after the add-on's call returned and found it held.
//
// A failure hijacks the connection out of the pool and closes it: a connection that
// may still hold one of this product's locks must not be handed to the next caller,
// and Postgres releases everything a backend held when the backend ends. Hijack
// leaves the deferred Release a no-op, which is what makes this safe to call before
// it.
func (a *AddonDB) releaseLocks(ctx context.Context, conn *pgxpool.Conn) {
	// Not the caller's context: a cancelled or timed-out call is exactly when a lock
	// is most likely to have been left held, and a cancelled context cannot run the
	// statement that releases it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), AddonStatementTimeout)
	defer cancel()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock_all()`); err != nil {
		a.log.Warn("could not release an add-on's advisory locks; discarding the connection",
			slog.String("addon", a.name),
			slog.String("schema", a.schema),
			slog.Any("error", err))
		_ = conn.Hijack().Close(ctx)
	}
}

// currentPassword is the credential new connections authenticate with.
func (a *AddonDB) currentPassword() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.password
}

// acquire takes a connection, re-minting the credential once if another replica
// has rotated it out from under this one.
//
// Narrow on purpose: only 28P01, only one retry. Any other failure is returned as
// it arrives — a role that has been dropped answers 28000, and re-creating it here
// would fight an operator's purge rather than recover from a boot.
func (a *AddonDB) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	stale := a.currentPassword()
	conn, err := a.pool.Acquire(ctx)
	if err == nil || !isPasswordFailure(err) || a.admin == nil {
		return conn, err
	}
	if err := a.reauthenticate(ctx, stale); err != nil {
		return nil, err
	}
	return a.pool.Acquire(ctx)
}

// isPasswordFailure is the one connection failure that is recoverable by minting
// a new credential: another replica ran ALTER ROLE … PASSWORD.
func isPasswordFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "28P01"
}

// reauthenticate mints this add-on's credential again and stores it.
//
// `stale` is what the caller failed with. If it is no longer current, another
// call has already refreshed and this one only has to retry — which is what keeps
// a burst of concurrent failures to a single ALTER ROLE.
func (a *AddonDB) reauthenticate(ctx context.Context, stale string) error {
	a.refresh.Lock()
	defer a.refresh.Unlock()
	if a.currentPassword() != stale {
		return nil
	}
	password, err := EnsureAddonSchema(ctx, a.admin, a.name, a.log)
	if err != nil {
		return fmt.Errorf("re-mint the %s credential: %w", a.schema, err)
	}
	a.mu.Lock()
	a.password = password
	a.mu.Unlock()
	// Warned, not debugged. It means another replica loaded this add-on, which is
	// ordinary in a multi-replica deployment and is a misconfiguration in a
	// single-replica one — two processes on one database.
	a.log.Warn("an add-on's database credential had been rotated by another replica; minted a new one",
		slog.String("addon", a.name),
		slog.String("schema", a.schema))
	return nil
}

// Close releases the add-on's connections.
func (a *AddonDB) Close() {
	if a == nil || a.pool == nil {
		return
	}
	a.pool.Close()
	a.pool = nil
}

// Query runs one read and returns its rows as a JSON array of objects.
//
// Read-only at the server, which is what separates this from [AddonDB.Exec]
// rather than a promise about what the statement looks like: a transaction begun
// READ ONLY refuses a write whatever the SQL says, so an add-on cannot use the
// read function to write and the ABI's two functions mean two different things.
//
// A payload carrying more than one statement is refused by Postgres, because
// [OpenAddonDB] parses through the extended protocol. That matters more than it
// sounds: `RESET ROLE` as a second statement is the obvious escape, and it is
// refused twice over — once here and once by the role being the session's own.
func (a *AddonDB) Query(ctx context.Context, statement string, args []any) ([]byte, error) {
	if a == nil || a.pool == nil {
		return nil, errors.New("the add-on has no database")
	}
	conn, err := a.acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	// Before the release and after the rollback below, which is what the defer order
	// buys: the statement runs on an idle connection nothing else can have yet.
	defer a.releaseLocks(ctx, conn)

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	// Rolled back always, including on success: a read has nothing to commit, and
	// rolling back also undoes any session setting the statement itself made.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := a.pin(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	out, err := encodeRows(rows)
	if err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// Exec runs one write.
func (a *AddonDB) Exec(ctx context.Context, statement string, args []any) error {
	if a == nil || a.pool == nil {
		return errors.New("the add-on has no database")
	}
	conn, err := a.acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	defer a.releaseLocks(ctx, conn)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := a.pin(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, statement, args...); err != nil {
		return classify(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return classify(err)
	}
	return nil
}

// pin sets the search path and the statement timeout for this transaction only.
//
// LOCAL, and both of them, because the pool hands out connections a previous
// statement has already run on: a module's `SET search_path = public` survives a
// committed transaction, and LOCAL is what makes the next transaction mean what
// it says regardless. Re-pointing the search path buys the module nothing on its
// own — privileges are the boundary — and leaving it re-pointed would make the
// add-on's own unqualified names stop resolving, which is a bug report against
// the host.
//
// The role is not re-pinned, and does not need to be: it is the session's own
// authenticated identity, and Postgres will not let this role become another one.
func (a *AddonDB) pin(ctx context.Context, tx pgx.Tx) error {
	// set_config rather than SET, and the reason is worth the sentence: SET takes one
	// parameter per statement and takes no arguments, so two settings would be two
	// statements and the schema name would be interpolated into DDL-shaped SQL.
	// set_config is a function — one round trip for both, both values as bind
	// parameters, and its third argument is `is_local`, which is exactly SET LOCAL.
	//
	// pg_advisory_unlock_all rides along as belt: [AddonDB.releaseLocks] is the
	// mechanism, and this clears anything a previous statement on this connection
	// left held if that ever failed to run. It costs no round trip, being in the same
	// statement, and it releases nothing of this session's own — the guest's
	// statement has not run yet. Sabotaged alone it leaves the test green, and saying
	// so is the point.
	if _, err := tx.Exec(ctx,
		`SELECT set_config('search_path', $1, true), set_config('statement_timeout', $2, true),
		        pg_advisory_unlock_all()`,
		a.schema, strconv.FormatInt(AddonStatementTimeout.Milliseconds(), 10)); err != nil {
		return fmt.Errorf("pin the %s session: %w", a.schema, err)
	}
	return nil
}

// encodeRows turns a result set into the JSON array of objects the ABI carries.
//
// Assembled row by row against a bound rather than marshalled whole, so what
// crosses to the guest is bounded by [AddonMaxResultBytes] and not by what the
// query matched — the *host's* heap is not, which [AddonMaxResultBytes] says and
// F276 is. A duplicate column name is refused rather than silently collapsed:
// two columns called `id` would become one key, and an add-on reading the answer
// would have no way to know which it got.
func encodeRows(rows pgx.Rows) ([]byte, error) {
	fields := rows.FieldDescriptions()
	names := make([]string, len(fields))
	seen := make(map[string]bool, len(fields))
	for i, f := range fields {
		if seen[f.Name] {
			return nil, fmt.Errorf("the result has two columns called %q; name them apart", f.Name)
		}
		seen[f.Name] = true
		names[i] = f.Name
	}

	var buf bytes.Buffer
	buf.WriteByte('[')
	first := true
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, classify(err)
		}
		row := make(map[string]any, len(names))
		for i, name := range names {
			if i < len(values) {
				row[name] = values[i]
			}
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			// A column type with no JSON form. The add-on's own schema chose it, so
			// this is the guest's fault and the fix is a cast in the statement.
			return nil, fmt.Errorf("a column in the result has no JSON form: %w", err)
		}
		if buf.Len()+len(encoded)+2 > AddonMaxResultBytes {
			return nil, fmt.Errorf("the result is larger than %d bytes; narrow the query",
				AddonMaxResultBytes)
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.Write(encoded)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// classify separates a refusal from every other failure.
//
// 42501 is insufficient_privilege, which is what reaching outside the schema
// looks like: `permission denied for table links`, `permission denied for schema
// public`. It is the one SQL failure an add-on's author cannot fix by editing
// their statement, so it is the one this package names — and it is what the
// confinement tests assert, because a test that accepted any error would pass
// against a typo.
func classify(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		return fmt.Errorf("%w: %s", ErrAddonDenied, pgErr.Message)
	}
	return err
}

// DecodeAddonArgs turns the JSON array an add-on passes into query arguments.
//
// Numbers arrive as json.Number and are converted to int64 where they are whole,
// because encoding/json's default is float64 and a float64 bound to a bigint
// column is a value nobody wrote. Anything else crosses as itself: a string, a
// bool, or null.
//
// It lives in this package rather than in the host because the shape it produces
// is a pgx argument list, and the rule about what pgx does with each Go type is
// this layer's to know.
func DecodeAddonArgs(raw []byte) ([]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var list []any
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("arguments: expected a JSON array: %w", err)
	}
	// A second document is an argument list whose author believed something the
	// host will not read — the same answer parseManifest gives.
	if err := dec.Decode(new(json.RawMessage)); err == nil {
		return nil, errors.New("arguments: trailing content after the array")
	}
	out := make([]any, len(list))
	for i, v := range list {
		converted, err := convertAddonArg(v)
		if err != nil {
			return nil, fmt.Errorf("argument %d: %w", i+1, err)
		}
		out[i] = converted
	}
	return out, nil
}

func convertAddonArg(v any) (any, error) {
	switch t := v.(type) {
	case nil, bool, string:
		return t, nil
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, nil
		}
		f, err := t.Float64()
		if err != nil {
			return nil, fmt.Errorf("%q is not a number Postgres can be given", t.String())
		}
		return f, nil
	default:
		// An object or an array. Refused rather than passed as jsonb, because
		// which of the two an add-on meant is not knowable from the value and
		// guessing would make the ABI's argument list mean different things in
		// different columns. An add-on that wants jsonb passes the JSON as a
		// string and casts it.
		return nil, errors.New("must be a string, a number, a boolean or null; " +
			"pass JSON as a string and cast it in the statement")
	}
}

// AddonSchemaSuffix is the add-on name inside a schema name, or "" for a schema
// that is not an add-on's.
//
// The inverse of [AddonSchema], and it exists so the orphan report can name the
// add-on an operator would look for rather than the schema they have never
// typed.
func AddonSchemaSuffix(schema string) string {
	name, ok := strings.CutPrefix(schema, AddonSchemaPrefix)
	if !ok {
		return ""
	}
	return name
}
