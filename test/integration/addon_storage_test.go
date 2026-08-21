//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// This file is m63.md's first risk, executed: schema confinement verified against
// **hostile** SQL rather than polite SQL.
//
// It lives here rather than in internal/addon because every claim in it needs a
// real Postgres — a role, a schema, privileges and an error code — and none of it
// can be asserted against a fake. What it costs is a copy of two small fixture
// helpers, which is cheaper than adding a fourth package to the integration
// target's four mirrored invocations.
//
// The hostile statements themselves are in the guest, not here:
// internal/addon/testdata/modules/storage is a real consumer of the published SDK
// that reaches for the product's tables eleven ways and panics if any of them
// works. This file installs it, loads it, and reads back what it reported — plus
// the assertions a module cannot make about itself, which are the ones about the
// catalogue.

// --- fixtures ----------------------------------------------------------------

// fixtureRoot is internal/addon's own build output. Shared deliberately: the module
// this test loads is the same artifact `make addon-fixtures` builds and the same one
// internal/addon's unit tests load, so there is one set of bytes rather than two
// that can disagree.
const (
	fixtureRoot = "../../internal/addon/testdata/build"
	fixtureSrc  = "../../internal/addon/testdata/modules"
)

var fixtureBuilds sync.Mutex

// addonFixture reads a built test module, building it first if it is not there.
//
// It builds rather than skips, for the reason internal/addon's own fixture() does:
// a skip would be a green run of a test whose whole subject is loading wasm.
func addonFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join(fixtureRoot, name+".wasm")
	if code, err := os.ReadFile(path); err == nil {
		return code
	}
	fixtureBuilds.Lock()
	defer fixtureBuilds.Unlock()
	if code, err := os.ReadFile(path); err == nil {
		return code
	}
	if err := os.MkdirAll(fixtureRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// cmd.Dir is the repository root, so both paths are relative to it — the
	// `../../` these constants carry is for this package's own working
	// directory and must not be joined on a second time. It was, and the
	// artifact landed two levels above the repository: every local gate stayed
	// green because `make test-integration` takes `addon-fixtures`, so the file
	// already existed and this builder never ran. CI's target did not take it,
	// so CI was the only place that ever exercised this path — F255's shape, and
	// what `make check-ci` caught.
	cmd := exec.Command("go", "build", "-buildmode=c-shared",
		"-o", filepath.Join(fixtureRoot[6:], name+".wasm"),
		"./"+filepath.Join(fixtureSrc[6:], name))
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the %s test module will not build: %v\n%s\n"+
			"build it by hand to see why: make addon-fixtures", name, err, out)
	}
	code, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the %s test module is still not readable after building it: %v", name, err)
	}
	return code
}

// installAddon writes one add-on's directory: the manifest, the module, and the
// migrations the manifest describes.
//
// The digests are computed here from the bytes being written, so a test cannot
// accidentally assert against a manifest that lies — the lying manifests get built
// deliberately, one field at a time, by the tests that want one.
func installAddon(t *testing.T, root, name string, code []byte, perms []string,
	migrations map[string]string,
) addon.Manifest {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(code)
	m := addon.Manifest{
		SchemaVersion: addon.SchemaVersion,
		Name:          name,
		Version:       "1.0.0",
		ABIVersion:    1,
		Module:        name + ".wasm",
		SHA256:        hex.EncodeToString(sum[:]),
		FailureClass:  addon.ClassRequired,
		Permissions:   perms,
	}
	if err := os.WriteFile(filepath.Join(dir, m.Module), code, 0o644); err != nil {
		t.Fatal(err)
	}
	if len(migrations) > 0 {
		path := filepath.Join(dir, addon.MigrationsDir)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		for file, body := range migrations {
			if err := os.WriteFile(filepath.Join(path, file), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			d := sha256.Sum256([]byte(body))
			m.Migrations = append(m.Migrations, addon.MigrationFile{
				File: file, SHA256: hex.EncodeToString(d[:]),
			})
		}
	}
	writeManifest(t, dir, m)
	return m
}

func writeManifest(t *testing.T, dir string, m addon.Manifest) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, addon.ManifestFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// scrape reads a metrics registry the way Prometheus would. The integration
// package has no shared one; internal/addon's unit tests carry the same helper,
// for the same reason.
func scrape(t *testing.T, m *observability.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d", rec.Code)
	}
	return rec.Body.String()
}

// seriesLike is every scraped line with the given prefix, so a failure prints the
// series that *do* exist rather than only the one that does not.
func seriesLike(body, prefix string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// logSink collects a host's log so a test can read what a module reported through
// the ABI's own log function.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// --- the database an add-on gets ---------------------------------------------

// addonName is a fresh add-on name per test.
//
// Fresh because a Postgres **role** is cluster-wide while the database is not: two
// tests sharing a name would share a role, and the second would inherit whatever
// the first left. The prefix keeps them recognisable in a cluster somebody is
// debugging, and the cleanup below drops each one.
func addonName(t *testing.T) string {
	t.Helper()
	raw := strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")
	return "t_" + raw[:16]
}

// newAddonDB clones the template database and returns a pool, its DSN and the
// directory add-ons will be installed into.
//
// The DSN is what an add-on's own pool re-points at its own role, so a test that
// only had a pool could not open one — which is why this exists beside newDB
// rather than calling it.
//
// **The add-on names are passed in so that one cleanup can drop the roles in the
// right order**, which is the whole reason they are a parameter rather than a
// second helper. A Postgres role that owns objects in a live database cannot be
// dropped, so the role drop has to happen after the database drop — and two
// separate t.Cleanup registrations put them in whichever order the test happened
// to call them in. That footgun leaked fifty-one roles into the development
// cluster before it was noticed, which is exactly the failure a single ordered
// closure cannot have.
func newAddonDB(t *testing.T, addons ...string) (*pgxpool.Pool, string, string) {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsnFor("postgres"))
	if err != nil {
		t.Fatalf("connect to maintenance database: %v", err)
	}
	defer admin.Close()

	name := "t_" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")[:20]
	if _, err := admin.Exec(ctx,
		fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateDB)); err != nil {
		t.Fatalf("clone template: %v", err)
	}
	dsn := dsnFor(name)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pgxpool.New(context.Background(), dsnFor("postgres"))
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(),
			fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
		// After the database, never before: the role owns tables in it.
		for _, a := range addons {
			_, _ = cleanup.Exec(context.Background(),
				fmt.Sprintf("DROP ROLE IF EXISTS %s", store.AddonSchema(a)))
		}
	})
	return pool, dsn, t.TempDir()
}

// openAddonHost opens a host over dir with a real database behind it.
func openAddonHost(t *testing.T, dir string, pool *pgxpool.Pool, dsn string,
) (*addon.Host, *observability.Metrics, *logSink, error) {
	t.Helper()
	sink := &logSink{}
	metrics := observability.NewMetrics()
	h, err := addon.Open(context.Background(), addon.Options{
		Dir:     dir,
		DB:      pool,
		DSN:     dsn,
		Metrics: metrics,
		Logger:  slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if h != nil {
		t.Cleanup(func() { _ = h.Close(context.Background()) })
	}
	return h, metrics, sink, err
}

// ownSchema is the benign half of the storage fixture's DDL: the table it writes
// to, and the SECURITY DEFINER function whose whole purpose is to prove that DDL
// the host runs cannot install a way around the boundary.
const ownSchemaMigration = `-- +goose Up
CREATE TABLE notes (id bigserial PRIMARY KEY, body text NOT NULL);

-- +goose StatementBegin
CREATE FUNCTION peek_links() RETURNS bigint LANGUAGE sql SECURITY DEFINER AS $$
    SELECT count(*) FROM public.links
$$;
-- +goose StatementEnd
`

// --- the milestone's own claims ---------------------------------------------

// The whole of m63.md's confinement bullet, asserted from inside a guest.
//
// Loading the module at all is most of the assertion: every check in the storage
// fixture panics on a mismatch, and a panic during package initialization fails
// instantiation, which fails the load. The log is read back afterwards so a failure
// says *which* statement was answered wrongly rather than only that one was.
func TestAnAddonReachesItsOwnSchemaAndNothingElse(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)

	code := addonFixture(t, "storage")
	installAddon(t, dir, name, code, []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": ownSchemaMigration})

	h, _, sink, err := openAddonHost(t, dir, pool, dsn)
	if err != nil {
		t.Fatalf("the storage fixture did not load, so one of its checks failed: %v\n%s",
			err, sink.String())
	}
	if h.Len() != 1 {
		t.Fatalf("loaded %d add-ons, want 1", h.Len())
	}

	logs := sink.String()
	if strings.Contains(logs, "MISMATCH") {
		t.Errorf("the fixture reported a mismatch\n%s", logs)
	}
	// Named individually so a regression says which boundary moved. The list is the
	// fixture's, and a check added there without a line here is caught by the count
	// below.
	for _, check := range []string{
		"own_insert=ok", "own_select=ok", "args_and_shape=ok",
		"qualified_product_read=ok", "qualified_product_users=ok",
		"qualified_product_sessions=ok", "cte_product_read=ok",
		"repointed_search_path=ok", "do_block_reset_role=ok",
		"session_authorization=ok", "set_role=ok",
		"security_definer=ok", "two_statements=ok", "query_cannot_write=ok",
		"ddl_into_public=ok", "copy_to_program=ok", "empty_statement=ok",
		"object_argument=ok",
	} {
		if !strings.Contains(logs, "storage: "+check) {
			t.Errorf("the fixture did not report %q\n%s", check, logs)
		}
	}
	// A check added to the fixture and not to the list above would otherwise go
	// unread, which is the failure the list is meant to prevent rather than cause.
	if got, want := strings.Count(logs, "storage: "), 18; got != want {
		t.Errorf("the fixture reported %d checks and this test names %d", got, want)
	}

	// The confinement refusals reach the operator's log as warnings, because a module
	// reaching outside its schema is either a bug its author has not noticed or an
	// attempt, and neither is a debug-level fact.
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "an add-on's statement failed") {
		t.Errorf("a refused statement was not warned about\n%s", logs)
	}
	// And the message never crosses back. A Postgres error names tables and columns;
	// what the guest got is a number.
	if strings.Contains(logs, "storage: qualified_product_read=MISMATCH") {
		t.Errorf("the guest saw more than a status\n%s", logs)
	}

	// The row the add-on wrote is in its own schema and nowhere else.
	var body string
	if err := pool.QueryRow(context.Background(),
		fmt.Sprintf(`SELECT body FROM %s.notes ORDER BY id LIMIT 1`,
			store.AddonSchema(name))).Scan(&body); err != nil {
		t.Fatalf("the add-on's own table does not hold its row: %v", err)
	}
	if body != "first" {
		t.Errorf("the add-on's row reads %q, want %q", body, "first")
	}
	// Nothing of the add-on's is anywhere else, and nothing in its schema is anybody
	// else's — the post-condition the host itself checks after every migration.
	violations, err := store.AddonConfinementViolations(context.Background(), pool, name)
	if err != nil {
		t.Fatalf("checking the confinement: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("the add-on's confinement does not hold: %v", violations)
	}
}

// The declared limb becomes real: the two functions are live in the ABI, so the
// documentation, the SDK and the host module all say so from one place.
func TestTheStorageFunctionsAreLive(t *testing.T) {
	for _, name := range []string{"storage_query", "storage_exec"} {
		var found bool
		for _, f := range abi.Functions {
			if f.Name != name {
				continue
			}
			found = true
			if !f.Live {
				t.Errorf("%s is not marked live, and this milestone implemented it", name)
			}
			if f.Requires != abi.PermissionStorage {
				t.Errorf("%s costs %q, want %q", name, f.Requires, abi.PermissionStorage)
			}
		}
		if !found {
			t.Errorf("the ABI does not declare %s at all", name)
		}
	}
}

// F-1, at the site itself: a migration filename reaches an operator's log on the
// **success** path, and the filename is the module's.
//
// `store.MigrateAddon` logs `slog.String("name", r.Source.Path)` for every migration
// it applies, `migrationFileRe` admits every code point but a newline, and
// `internal/store` cannot import the neutralizer because `internal/addon` imports
// `internal/store`. What closes it is the logger the host hands over — D286 wraps it
// in `Open`, so every line any package writes with it is neutralized without that
// package knowing. This drives the real function against a real database, because
// the unit test that drives the same shape through `h.log` cannot reach goose.
//
// The success path is the one that matters: three log sites were enumerated in five
// documents last round and this was not one of them, precisely because it is the
// line that fires when nothing is wrong.
func TestAMigrationFilenameReachesAnOperatorNeutralized(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)

	// A right-to-left override, a zero-width space and an ANSI erase, inside a name
	// the manifest's own pattern accepts. What a reader would otherwise see is a line
	// that says a migration called `everything is fine` was applied.
	const hostile = "00001_\u202eeverything is fine\u200b\x1b[2KSECRET=hunter2.sql"
	code := addonFixture(t, "storage")
	installAddon(t, dir, name, code, []string{abi.PermissionStorage},
		map[string]string{hostile: ownSchemaMigration})

	h, _, sink, err := openAddonHost(t, dir, pool, dsn)
	if err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}
	if h.Len() != 1 {
		t.Fatalf("loaded %d add-ons, want 1", h.Len())
	}
	logs := sink.String()
	if !strings.Contains(logs, "add-on migration applied") {
		t.Fatalf("no migration was applied, so the line under test never happened\n%s", logs)
	}
	for what, r := range map[string]rune{
		"a right-to-left override": '\u202e',
		"a zero-width space":       '\u200b',
		"an ANSI escape":           '\x1b',
	} {
		if strings.ContainsRune(logs, r) {
			t.Errorf("%s in a migration filename reached an operator's log as itself", what)
		}
	}
	// **The ANSI escape is the discriminating one and the others are not**, which is
	// worth saying rather than leaving to be rediscovered: slog's text handler quotes
	// with strconv, which spells a `Cf` code point `\u202e` on its own — so the two
	// invisible characters look escaped whether or not this boundary ran. strconv
	// spells `U+001B` as `\x1b`; only the host's escaping spells it `\u001b`. Both
	// are asserted, the second as an absence, so what this test measures is this
	// boundary and not the handler underneath it.
	if !strings.Contains(logs, `\u001b`) {
		t.Errorf("the filename did not go through the host's escaping\n%s", logs)
	}
	if strings.Contains(logs, `\x1b`) {
		t.Errorf("the ANSI escape reached the log in strconv's spelling, which means the "+
			"handler quoted it and the host did not escape it\n%s", logs)
	}
	for _, want := range []string{"u202e", "u200b"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the filename did not arrive escaped; want %s in the log\n%s", want, logs)
		}
	}
}

// m63.md's *versioned like the host's own*: the migration state lives in a goose
// table inside the add-on's schema, so loading the same module twice applies
// nothing the second time.
func TestLoadingTheSameModuleTwiceIsIdempotent(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)

	code := addonFixture(t, "storage")
	installAddon(t, dir, name, code, []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": ownSchemaMigration})

	first, _, sink, err := openAddonHost(t, dir, pool, dsn)
	if err != nil {
		t.Fatalf("the first load failed: %v\n%s", err, sink.String())
	}
	if !strings.Contains(sink.String(), "add-on migration applied") {
		t.Errorf("the first load applied no migration\n%s", sink.String())
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("closing the first host: %v", err)
	}

	// The version table is the add-on's own, inside the add-on's own schema. Read
	// before the second load, because that is what makes the second load's silence
	// mean *already applied* rather than *never ran*.
	var version int64
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT max(version_id) FROM %s.goose_db_version`, store.AddonSchema(name))).Scan(&version); err != nil {
		t.Fatalf("the add-on's goose table is not in its own schema: %v", err)
	}
	if version != 1 {
		t.Errorf("the add-on's applied version is %d, want 1", version)
	}
	// And it is owned by the add-on's role, not by the product's user, which is what
	// makes an orphaned schema self-describing rather than half in a table the
	// product owns.
	var owner string
	if err := pool.QueryRow(context.Background(), `
		SELECT r.rolname FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  JOIN pg_roles r ON r.oid = c.relowner
		 WHERE n.nspname = $1 AND c.relname = 'goose_db_version'`,
		store.AddonSchema(name)).Scan(&owner); err != nil {
		t.Fatalf("reading the goose table's owner: %v", err)
	}
	if owner != store.AddonSchema(name) {
		t.Errorf("the add-on's goose table is owned by %q, want %q", owner, store.AddonSchema(name))
	}

	second, _, sink2, err := openAddonHost(t, dir, pool, dsn)
	if err != nil {
		t.Fatalf("the second load of the same module failed: %v\n%s", err, sink2.String())
	}
	if second.Len() != 1 {
		t.Fatalf("the second load produced %d add-ons, want 1", second.Len())
	}
	if strings.Contains(sink2.String(), "add-on migration applied") {
		t.Errorf("the second load re-applied a migration\n%s", sink2.String())
	}
	// The fixture inserted a second row, so its own writes are additive across loads
	// while its DDL is not — which is the whole distinction between a migration and
	// an exec.
	var rows int
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT count(*) FROM %s.notes`, store.AddonSchema(name))).Scan(&rows); err != nil {
		t.Fatalf("counting the add-on's rows: %v", err)
	}
	if rows != 2 {
		t.Errorf("the add-on's table holds %d rows after two loads, want 2", rows)
	}
}

// m63.md's *DDL that names any other schema is refused, asserted by a test module
// that tries*. Four modules, four migrations, each reaching somewhere it may not.
//
// A `required` add-on whose migration fails stops the instance, which is M60's
// class rule and m63.md's third risk — so the assertion is that Open returns an
// error naming the add-on, and that the outcome is counted under its own label.
func TestAMigrationThatNamesAnotherSchemaIsRefused(t *testing.T) {
	hostile := map[string]string{
		"a table in the product's schema": `-- +goose Up
CREATE TABLE public.evil (x int);
`,
		"a read of the product's tables": `-- +goose Up
CREATE TABLE mine AS SELECT * FROM public.links;
`,
		"a write to the product's tables": `-- +goose Up
INSERT INTO public.permissions (name, description) VALUES ('addon.everything', 'mine now');
`,
		"a schema of its own choosing": `-- +goose Up
CREATE SCHEMA somewhere_else;
`,
		"becoming a superuser": `-- +goose Up
-- +goose StatementBegin
DO $$ BEGIN EXECUTE 'ALTER ROLE ' || current_user || ' SUPERUSER'; END $$;
-- +goose StatementEnd
`,
	}

	for label, sql := range hostile {
		t.Run(label, func(t *testing.T) {
			name := addonName(t)
			pool, dsn, dir := newAddonDB(t, name)

			code := addonFixture(t, "minimal")
			installAddon(t, dir, name, code, []string{abi.PermissionStorage},
				map[string]string{"00001_hostile.sql": sql})

			_, metrics, sink, err := openAddonHost(t, dir, pool, dsn)
			if err == nil {
				t.Fatalf("a migration doing %s was applied\n%s", label, sink.String())
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the failure does not name the add-on:\n%v", err)
			}
			series := fmt.Sprintf(
				`linkctrl_addon_loads_total{addon="%s",outcome="%s"} 1`, name, addon.OutcomeStorageFailed)
			if scraped := scrape(t, metrics); !strings.Contains(scraped, series) {
				t.Errorf("the scrape does not carry %s\n%s", series,
					seriesLike(scraped, "linkctrl_addon_loads_total"))
			}
			// Nothing of the add-on's is anywhere but its own schema, whatever the
			// migration tried. Checked as well as the refusal, because a migration that
			// failed *after* creating something would leave the refusal true and the
			// boundary broken.
			violations, err := store.AddonConfinementViolations(context.Background(), pool, name)
			if err != nil {
				t.Fatalf("checking the confinement: %v", err)
			}
			if len(violations) > 0 {
				t.Errorf("the refused migration still left %v", violations)
			}
			var superuser bool
			if err := pool.QueryRow(context.Background(),
				`SELECT rolsuper FROM pg_roles WHERE rolname = $1`,
				store.AddonSchema(name)).Scan(&superuser); err == nil && superuser {
				t.Error("the add-on's role is a superuser")
			}
		})
	}
}

// D273, and it is the one claim in M60's reopening that needs a real database.
//
// The load budget F287 added bounds the two steps that run the add-on's **own
// code** — compiling the module, and instantiating it — and deliberately not the
// host's work with the database in between. The first fix laid the budget over the
// whole of the load, which capped a wait m63.md deliberately set to five minutes:
// `MigrateAddon` asks for the migration lock with `lock.WithLockTimeout(5, 60)`
// because *a replica arriving mid-migration should wait rather than fail into a
// crash loop*, and thirty seconds over the whole load makes that unreachable — the
// second replica reports `load_timeout`, and a `required` add-on then stops the
// instance. That is F287's repair breaking M63's claim from the other side.
//
// Asserted here at a scale a test can afford: a migration that takes longer than
// the whole budget, under a budget the module's own compile fits inside. The
// add-on loads, its table exists, and nothing was counted as a timeout.
//
// **The budget is measured rather than chosen.** Compiling this fixture is a few
// hundred milliseconds on this machine and several seconds under `-race` alongside
// the rest of this package, so a fixed budget picked against either number expires
// during compilation under the other — and the test would then pass for the wrong
// reason, having proved only that a short budget refuses a slow compile.
//
// Sabotage is the shape of the defect: put the budget back around the whole of
// `loadOne`, and this fails as `load_timeout` while every other test the reopening
// added still passes. Which is exactly how it shipped.
func TestTheLoadBudgetDoesNotCapAMigration(t *testing.T) {
	code := addonFixture(t, "minimal")

	// One healthy load with no database behind it, to price a compile under
	// whatever conditions this run is happening in.
	measured := func() time.Duration {
		dir := t.TempDir()
		installAddon(t, dir, "measure", code, nil, nil)
		start := time.Now()
		h, err := addon.Open(context.Background(), addon.Options{Dir: dir})
		took := time.Since(start)
		if err != nil {
			t.Fatalf("measuring an ordinary load: %v", err)
		}
		_ = h.Close(context.Background())
		return took
	}()
	budget := measured + 2*time.Second
	// Past the budget by enough that a clock's worth of slack cannot decide the
	// outcome, and stated in whole seconds because pg_sleep takes one.
	sleep := int(budget.Seconds()) + 2

	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)
	installAddon(t, dir, name, code, []string{abi.PermissionStorage},
		map[string]string{"00001_slow.sql": fmt.Sprintf(`-- +goose Up
SELECT pg_sleep(%d);
CREATE TABLE notes (id bigserial PRIMARY KEY, body text NOT NULL);
`, sleep)})

	sink := &logSink{}
	metrics := observability.NewMetrics()
	start := time.Now()
	h, err := addon.Open(context.Background(), addon.Options{
		Dir: dir, DB: pool, DSN: dsn, Metrics: metrics,
		Logger:      slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
		LoadTimeout: budget,
	})
	if h != nil {
		t.Cleanup(func() { _ = h.Close(context.Background()) })
	}
	took := time.Since(start)
	if err != nil {
		t.Fatalf("a %ds migration under a %v budget refused the add-on: %v\n%s",
			sleep, budget, err, sink.String())
	}
	if h.Len() != 1 {
		t.Fatalf("%d add-ons loaded, want 1\n%s", h.Len(), sink.String())
	}
	if took < time.Duration(sleep)*time.Second {
		t.Errorf("the load returned in %v, inside the %ds the migration sleeps for; "+
			"the migration cannot have run, so this test is not watching what it claims to",
			took, sleep)
	}
	// The migration ran to completion rather than merely being started, which is
	// the difference between a wait that was allowed and one that was interrupted
	// somewhere the host did not notice.
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT to_regclass($1) IS NOT NULL`, store.AddonSchema(name)+".notes").Scan(&exists); err != nil {
		t.Fatalf("looking for the add-on's table: %v", err)
	}
	if !exists {
		t.Error("the add-on loaded but its migration left no table")
	}
	if body := scrape(t, metrics); !strings.Contains(body,
		fmt.Sprintf(`linkctrl_addon_loads_total{addon="%s",outcome="%s"} 1`, name, addon.OutcomeLoaded)) {
		t.Errorf("the load was not counted as a success:\n%s",
			seriesLike(body, "linkctrl_addon_loads_total"))
	}
}

// The manifest is what makes the DDL the add-on author's, so a manifest that does
// not describe what is on disk refuses the add-on before a schema exists.
func TestMigrationsThatTheManifestDoesNotDescribeRefuseTheAddon(t *testing.T) {
	t.Run("bytes the manifest does not describe", func(t *testing.T) {
		name := addonName(t)
		pool, dsn, dir := newAddonDB(t, name)

		code := addonFixture(t, "minimal")
		installAddon(t, dir, name, code, []string{abi.PermissionStorage},
			map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE mine (x int);\n"})
		// One byte of DDL changed after the manifest was written, which is what an
		// edited install looks like.
		if err := os.WriteFile(filepath.Join(dir, name, addon.MigrationsDir, "00001_own.sql"),
			[]byte("-- +goose Up\nCREATE TABLE public.evil (x int);\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		_, _, sink, err := openAddonHost(t, dir, pool, dsn)
		if err == nil {
			t.Fatalf("a migration whose bytes the manifest does not describe was applied\n%s",
				sink.String())
		}
		if !strings.Contains(err.Error(), "hashes to") {
			t.Errorf("the refusal is not about the digest:\n%v", err)
		}
		// Refused *before* the schema exists, so a rejected install costs nothing in
		// the database and leaves no orphan behind.
		schemas, err := store.AddonSchemas(context.Background(), pool)
		if err != nil {
			t.Fatal(err)
		}
		if len(schemas) != 0 {
			t.Errorf("a refused add-on left schemas behind: %v", schemas)
		}
	})

	t.Run("DDL added beside a manifest that does not list it", func(t *testing.T) {
		name := addonName(t)
		pool, dsn, dir := newAddonDB(t, name)

		code := addonFixture(t, "minimal")
		installAddon(t, dir, name, code, []string{abi.PermissionStorage},
			map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE mine (x int);\n"})
		if err := os.WriteFile(filepath.Join(dir, name, addon.MigrationsDir, "00002_added.sql"),
			[]byte("-- +goose Up\nCREATE TABLE public.evil (x int);\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		_, _, sink, err := openAddonHost(t, dir, pool, dsn)
		if err == nil {
			t.Fatalf("DDL the manifest does not list was applied\n%s", sink.String())
		}
		if !strings.Contains(err.Error(), "00002_added.sql") {
			t.Errorf("the refusal does not name the file nobody listed:\n%v", err)
		}
	})
}

// m63.md's *an orphan is detectable*: remove the module's directory, reboot, and
// the schema is still there with nothing claiming it.
//
// Nothing is deleted here, which is the point — a purge is an operator's explicit
// act and M68's flow. What this asserts is that the host can *name* what is left.
func TestARemovedModuleLeavesADetectableOrphan(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)
	schema := store.AddonSchema(name)

	code := addonFixture(t, "minimal")
	installAddon(t, dir, name, code, []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE kept (x int);\n"})

	first, _, sink, err := openAddonHost(t, dir, pool, dsn)
	if err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}
	// While it is loaded it is not an orphan, which is the half that makes the other
	// half mean something.
	orphans, err := first.OrphanSchemas(context.Background())
	if err != nil {
		t.Fatalf("enumerating orphans: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("a loaded add-on's schema reads as an orphan: %v", orphans)
	}
	if got := first.Schemas(); len(got) != 1 || got[0] != schema {
		t.Errorf("the loaded add-on's schemas are %v, want [%s]", got, schema)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("closing the first host: %v", err)
	}

	// The module's file removed and the instance rebooted, which is the whole of the
	// scenario m63.md describes.
	if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	second, _, sink2, err := openAddonHost(t, dir, pool, dsn)
	if err != nil {
		t.Fatalf("a host with no add-ons and one leftover schema did not open: %v", err)
	}
	if second.Len() != 0 {
		t.Fatalf("the second boot loaded %d add-ons, want 0", second.Len())
	}
	orphans, err = second.OrphanSchemas(context.Background())
	if err != nil {
		t.Fatalf("enumerating orphans: %v", err)
	}
	if len(orphans) != 1 || orphans[0] != schema {
		t.Fatalf("the orphan enumeration is %v, want [%s]", orphans, schema)
	}
	// Said at boot, because an orphan is data an operator is paying for and does not
	// know about.
	if !strings.Contains(sink2.String(), "add-on schemas with no loaded module") {
		t.Errorf("the boot did not mention the orphaned schema\n%s", sink2.String())
	}
	// And the data is still there. Nothing in this milestone deletes it.
	var exists bool
	if err := pool.QueryRow(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = $1 AND tablename = 'kept')`,
		schema).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("the orphaned schema's table is gone; nothing here should have deleted it")
	}
}

// The size metric, which is the whole of this milestone's answer to schema quotas:
// there is no cap, and the growth is visible instead.
func TestSchemaSizeIsPublishedPerAddon(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)

	code := addonFixture(t, "storage")
	installAddon(t, dir, name, code, []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": ownSchemaMigration})

	h, metrics, sink, err := openAddonHost(t, dir, pool, dsn)
	if err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}

	// Nothing before the measurement runs: the gauge is set from the maintenance
	// job's schedule, not from Open, so a series existing at boot would be a
	// different metric than the one documented.
	if scraped := scrape(t, metrics); strings.Contains(scraped, "linkctrl_addon_schema_bytes") {
		t.Errorf("the gauge exists before anything measured\n%s",
			seriesLike(scraped, "linkctrl_addon_schema_bytes"))
	}

	h.ObserveSchemaSizes(context.Background())
	scraped := scrape(t, metrics)
	prefix := fmt.Sprintf(`linkctrl_addon_schema_bytes{addon="%s"} `, name)
	line := seriesLike(scraped, prefix)
	if line == "" {
		t.Fatalf("the scrape does not carry %s\n%s", prefix,
			seriesLike(scraped, "linkctrl_addon_schema_bytes"))
	}
	// A real number rather than zero: the fixture's migration made a table and its
	// init wrote a row, so the schema has storage.
	if strings.HasSuffix(strings.TrimSpace(line), " 0") {
		t.Errorf("the add-on's schema measures zero bytes and it holds a table with a row\n%s", line)
	}
	measured, err := store.AddonSchemaBytes(context.Background(), pool, name)
	if err != nil {
		t.Fatalf("measuring the schema: %v", err)
	}
	if measured <= 0 {
		t.Errorf("the schema measures %d bytes", measured)
	}
}

// The gauge above, asserted as a **shape** rather than as a case.
//
// The test beside it asserts that a schema holding one table with one row measures
// more than zero, which every version of this query has satisfied — including the
// one that summed `relkind IN ('r', 'm')` and therefore reported **0** for a schema
// holding 24,000 sequences and 188 MB of storage. A list of the relation kinds that
// have storage is a denylist of every kind not on it, and that was the defect: found
// by M63's fourth review, argued in D254.
//
// So this asserts an identity instead. The gauge sums `pg_total_relation_size` over
// the relations that are not already counted inside another; the same bytes are
// available a second way, as `pg_table_size` of **every** relation in the schema,
// which counts each one exactly once and each index as itself rather than through
// its parent. The two agree only if nothing with storage is missing and nothing is
// counted twice, so the assertion bites in both directions. The identity itself
// names no relation kind, which is the point: it cannot go stale the way a list does.
// The relkinds that do appear below are a guard that the schema really holds each
// shape, and a sum over sequences alone — evidence about the fixture, not the claim.
//
// The shapes are chosen to make each direction reachable: a table with an index and
// a TOASTed column, a partitioned table with a partition and an index over it (a
// `'I'` parent and an `'i'` child), a materialized view, a plain view, and sequences
// created through the ABI's write path. Sequences are also asserted on their own,
// because they are what the enumeration missed.
//
// **And it was never only an adversary's case, which this asserts rather than
// remarks.** `goose_db_version` — the migration state the *host* creates inside the
// add-on's schema — declares `id integer PRIMARY KEY GENERATED BY DEFAULT AS
// IDENTITY`, and an identity column owns a sequence. So every storage add-on that
// has ever loaded, however well behaved, has had 8192 bytes in its schema that this
// gauge reported as nothing, before the add-on wrote a single row of its own.
func TestSchemaSizeCountsEveryRelationWithStorage(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)
	ctx := context.Background()
	schema := store.AddonSchema(name)

	// Every shape at once, as the add-on's own DDL, so the role owns all of it.
	installAddon(t, dir, name, addonFixture(t, "minimal"), []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": `-- +goose Up
CREATE TABLE notes (id bigserial PRIMARY KEY, label text, body text);
CREATE INDEX notes_label_idx ON notes (label);
INSERT INTO notes (label, body)
     SELECT 'l' || g, repeat(md5(g::text), 400) FROM generate_series(1, 2000) g;
CREATE MATERIALIZED VIEW note_labels AS SELECT id, label FROM notes;
CREATE VIEW note_count AS SELECT count(*) AS n FROM notes;
CREATE TABLE spans (id int, k int) PARTITION BY RANGE (id);
CREATE TABLE spans_low PARTITION OF spans FOR VALUES FROM (0) TO (100000);
CREATE INDEX ON spans (k);
INSERT INTO spans SELECT g, g FROM generate_series(1, 50000) g;
`})
	h, metrics, sink, err := openAddonHost(t, dir, pool, dsn)
	if err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}

	// The second reading of the same bytes. Not the gauge's query with a different
	// filter — a different decomposition, which is what makes agreement evidence.
	groundTruth := func() int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, `
			SELECT coalesce(sum(pg_table_size(c.oid)), 0)::bigint
			  FROM pg_class c
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1`, schema).Scan(&n); err != nil {
			t.Fatalf("summing the schema a second way: %v", err)
		}
		return n
	}
	sequenceBytes := func() int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, `
			SELECT coalesce(sum(pg_total_relation_size(c.oid)), 0)::bigint
			  FROM pg_class c
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1 AND c.relkind = 'S'`, schema).Scan(&n); err != nil {
			t.Fatalf("summing the schema's sequences: %v", err)
		}
		return n
	}
	kinds := func() map[string]int {
		t.Helper()
		rows, err := pool.Query(ctx, `
			SELECT c.relkind::text, count(*)::int
			  FROM pg_class c
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1 GROUP BY 1`, schema)
		if err != nil {
			t.Fatalf("inventorying the schema: %v", err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var k string
			var c int
			if err := rows.Scan(&k, &c); err != nil {
				t.Fatal(err)
			}
			out[k] = c
		}
		return out
	}

	// The shapes are actually there. Without this the identity below could hold over
	// a schema that never had an index or a partition in it.
	inventory := kinds()
	for _, want := range []string{"r", "m", "v", "p", "i", "I", "S"} {
		if inventory[want] == 0 {
			t.Fatalf("the schema holds no relkind %q, so this test is not measuring what it says: %v",
				want, inventory)
		}
	}

	// The well-behaved case, before anything adversarial happens: the host's own
	// migration table owns a sequence, so this schema already holds bytes the
	// enumeration could not see.
	var gooseOwnsASequence bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_depend d
			  JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
			  JOIN pg_class t ON t.oid = d.refobjid
			  JOIN pg_namespace n ON n.oid = t.relnamespace
			 WHERE n.nspname = $1 AND t.relname = 'goose_db_version'
			   AND d.deptype IN ('a', 'i'))`, schema).Scan(&gooseOwnsASequence); err != nil {
		t.Fatal(err)
	}
	if !gooseOwnsASequence {
		t.Errorf("goose_db_version no longer owns a sequence, so the argument that this " +
			"was never only an adversary's case needs re-measuring before it is repeated")
	}

	before, err := store.AddonSchemaBytes(ctx, pool, name)
	if err != nil {
		t.Fatalf("measuring the schema: %v", err)
	}
	if seq := sequenceBytes(); seq <= 0 {
		t.Errorf("a schema with two identity columns holds %d bytes of sequences", seq)
	}
	if ground := groundTruth(); before != ground {
		t.Errorf("the gauge reads %d bytes and the same schema summed relation by relation "+
			"is %d — %d bytes are missing or double-counted, over %v",
			before, ground, ground-before, inventory)
	}

	// Now the case the enumeration missed, through the path an add-on really has.
	confined, err := store.OpenAddonDB(ctx, pool, dsn, name, mustResetPassword(t, pool, name), nil)
	if err != nil {
		t.Fatalf("opening the add-on's own connection: %v", err)
	}
	t.Cleanup(confined.Close)
	sequencesBefore := sequenceBytes()
	if err := confined.Exec(ctx, `
		DO $$ BEGIN
			FOR i IN 1..400 LOOP
				EXECUTE format('CREATE SEQUENCE s%s', i);
			END LOOP;
		END $$`, nil); err != nil {
		t.Fatalf("the confined role could not create sequences, which is what this "+
			"test is about: %v", err)
	}

	// 8192 bytes each, in the add-on's own schema, durable — and outside
	// pg_total_relation_size of any table, which is why a list of kinds missed them.
	grew := sequenceBytes() - sequencesBefore
	if grew <= 0 {
		t.Fatalf("400 new sequences added %d bytes of sequence storage", grew)
	}

	after, err := store.AddonSchemaBytes(ctx, pool, name)
	if err != nil {
		t.Fatalf("measuring the schema: %v", err)
	}
	if after-before != grew {
		t.Errorf("the gauge moved by %d bytes for %d bytes of new sequences",
			after-before, grew)
	}
	if ground := groundTruth(); after != ground {
		t.Errorf("with sequences added the gauge reads %d and the second sum reads %d", after, ground)
	}

	// Accounting rather than a violation: the sequences are owned by the role and are
	// inside its schema, so the post-condition is right to stay silent and the gauge
	// is the only thing that was ever going to report them.
	violations, err := store.AddonConfinementViolations(ctx, pool, name)
	if err != nil {
		t.Fatalf("checking the confinement: %v", err)
	}
	if len(violations) != 0 {
		t.Errorf("the post-condition reports %v for objects the add-on legitimately owns", violations)
	}

	// And the published series carries the number, not just the function.
	h.ObserveSchemaSizes(ctx)
	prefix := fmt.Sprintf(`linkctrl_addon_schema_bytes{addon="%s"} `, name)
	line := seriesLike(scrape(t, metrics), prefix)
	if line == "" {
		t.Fatalf("the scrape does not carry %s", prefix)
	}
	scraped, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), prefix)), 64)
	if err != nil {
		t.Fatalf("the series is not a number: %q", line)
	}
	if int64(scraped) != after {
		t.Errorf("the scrape publishes %d bytes and the function measures %d", int64(scraped), after)
	}
}

// One add-on cannot reach another's schema, and the host offers it no way to ask.
// That is half of m63.md's additive-ness answer; the other half is the add-on's own
// grant, which
// [TestAnAddonCanGiveItsOwnSchemaAwayAndTheHostSeesItAtTheNextLoad] asserts.
func TestOneAddonCannotReachAnothersSchema(t *testing.T) {
	victim, intruder := addonName(t), addonName(t)
	pool, dsn, dir := newAddonDB(t, victim, intruder)

	minimal := addonFixture(t, "minimal")
	installAddon(t, dir, victim, minimal, []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE secrets (v text);\n"})
	installAddon(t, dir, intruder, minimal, []string{abi.PermissionStorage}, nil)

	h, _, sink, err := openAddonHost(t, dir, pool, dsn)
	if err != nil {
		t.Fatalf("the two add-ons did not load: %v\n%s", err, sink.String())
	}
	if h.Len() != 2 {
		t.Fatalf("loaded %d add-ons, want 2", h.Len())
	}
	// Reached through the store layer rather than through a guest, because the
	// statement has to name a schema whose name is only known at runtime — a fixture
	// compiled ahead of time cannot spell it.
	confined, err := store.OpenAddonDB(context.Background(), pool, dsn, intruder,
		mustResetPassword(t, pool, intruder), nil)
	if err != nil {
		t.Fatalf("opening the intruder's own connection: %v", err)
	}
	t.Cleanup(confined.Close)

	read := fmt.Sprintf(`SELECT * FROM %s.secrets`, store.AddonSchema(victim))
	if _, err := confined.Query(context.Background(), read, nil); !errorIsAddonDenied(err) {
		t.Errorf("%q answered %v, want a privilege refusal", read, err)
	}
	// Through Exec rather than Query, so the refusal is about privilege and not
	// about the read-only transaction a query runs in — which would be a true
	// refusal for the wrong reason, and therefore no evidence about the schema
	// boundary at all.
	for _, statement := range []string{
		fmt.Sprintf(`INSERT INTO %s.secrets (v) VALUES ('mine')`, store.AddonSchema(victim)),
		fmt.Sprintf(`DROP TABLE %s.secrets`, store.AddonSchema(victim)),
		fmt.Sprintf(`ALTER TABLE %s.secrets ADD COLUMN mine text`, store.AddonSchema(victim)),
	} {
		if err := confined.Exec(context.Background(), statement, nil); !errorIsAddonDenied(err) {
			t.Errorf("%q answered %v, want a privilege refusal", statement, err)
		}
	}
	// Its own schema still works, which is what makes the three refusals a boundary
	// rather than a broken connection.
	if _, err := confined.Query(context.Background(), `SELECT 1 AS n`, nil); err != nil {
		t.Errorf("the intruder cannot run a statement of its own: %v", err)
	}
}

// An add-on can hand its own schema to anybody, because it owns it — and the host
// sees that it did, at the add-on's next load.
//
// This is the half of *no other reader exists* that four documents once stated as an
// absolute. Two statements the add-on runs through the ordinary write path, each
// within the privileges it already holds over its own schema, and the other add-on
// reads the row. Nothing about it is an escalation: what A gives away is A's data.
//
// What must hold is that the post-condition **notices**, which the ownership half
// cannot: a grant is not an object, and neither `pg_shdepend` nor `pg_depend`
// records one. So the ACLs of the schema and of the relations in it are the third
// direction, and this test is the only thing that makes it non-vacuous — nothing an
// add-on does in the normal course of its life produces a single row of it.
func TestAnAddonCanGiveItsOwnSchemaAwayAndTheHostSeesItAtTheNextLoad(t *testing.T) {
	giver, taker := addonName(t), addonName(t)
	pool, dsn, dir := newAddonDB(t, giver, taker)
	ctx := context.Background()
	schema := store.AddonSchema(giver)

	minimal := addonFixture(t, "minimal")
	installAddon(t, dir, giver, minimal, []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE secrets (v text);\n"})
	installAddon(t, dir, taker, minimal, []string{abi.PermissionStorage}, nil)
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err != nil {
		t.Fatalf("the two add-ons did not load: %v\n%s", err, sink.String())
	}

	// Clean to begin with, which is what makes the dirty answer below mean
	// something — and what says a well-behaved add-on is not reported. The schema
	// holds the goose table the host created in it as well as the add-on's own.
	if violations, err := store.AddonConfinementViolations(ctx, pool, giver); err != nil {
		t.Fatalf("checking the confinement: %v", err)
	} else if len(violations) > 0 {
		t.Fatalf("a freshly migrated add-on already fails its confinement: %v", violations)
	}

	from, err := store.OpenAddonDB(ctx, pool, dsn, giver, mustResetPassword(t, pool, giver), nil)
	if err != nil {
		t.Fatalf("opening the giver's own connection: %v", err)
	}
	t.Cleanup(from.Close)
	into, err := store.OpenAddonDB(ctx, pool, dsn, taker, mustResetPassword(t, pool, taker), nil)
	if err != nil {
		t.Fatalf("opening the taker's own connection: %v", err)
	}
	t.Cleanup(into.Close)

	if err := from.Exec(ctx, `INSERT INTO secrets (v) VALUES ('given away')`, nil); err != nil {
		t.Fatalf("the giver could not write its own table: %v", err)
	}
	read := fmt.Sprintf(`SELECT v FROM %s.secrets`, schema)
	if _, err := into.Query(ctx, read, nil); !errorIsAddonDenied(err) {
		t.Fatalf("%q answered %v before any grant, want a privilege refusal", read, err)
	}

	// One statement each, through the same path a guest's storage_exec takes. USAGE
	// on the schema is the gate; the table privilege is what is then reachable
	// through it.
	for _, statement := range []string{
		fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO PUBLIC`, schema),
		fmt.Sprintf(`GRANT SELECT ON ALL TABLES IN SCHEMA %s TO PUBLIC`, schema),
	} {
		if err := from.Exec(ctx, statement, nil); err != nil {
			t.Fatalf("%q was refused, so this test proves nothing: %v", statement, err)
		}
	}

	// The capability is real, and saying so is the point of the test: the four
	// documents that called it impossible were wrong, and the repair is what they
	// say now.
	out, err := into.Query(ctx, read, nil)
	if err != nil {
		t.Fatalf("the other add-on still cannot read after the grant: %v", err)
	}
	if !strings.Contains(string(out), "given away") {
		t.Errorf("the other add-on read %s, want the giver's row", out)
	}

	violations, err := store.AddonConfinementViolations(ctx, pool, giver)
	if err != nil {
		t.Fatalf("checking the confinement: %v", err)
	}
	// Three, because `ALL TABLES` includes the migration table the host created in
	// that schema. Membership rather than order, because the count is a property of
	// what the host put there and not of the grant.
	for _, want := range []string{
		fmt.Sprintf("it has granted USAGE on schema %s to PUBLIC", schema),
		fmt.Sprintf("it has granted SELECT on %s.secrets to PUBLIC", schema),
		fmt.Sprintf("it has granted SELECT on %s.goose_db_version to PUBLIC", schema),
	} {
		if !slices.Contains(violations, want) {
			t.Errorf("the post-condition answered %v, which does not include %q", violations, want)
		}
	}

	// And the load itself refuses the add-on, which is what makes it a narrowing
	// rather than a metric: the host does not merely publish the grant, it declines
	// to run the module until an operator revokes it.
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err == nil {
		t.Errorf("a second load accepted an add-on that had given its schema away\n%s", sink.String())
	} else if !strings.Contains(err.Error(), "does not hold") {
		t.Errorf("the second load failed for some other reason: %v", err)
	}

	// Revoked, and the answer is empty again — so the check is about the grant and
	// not about anything else this schema acquired along the way.
	for _, statement := range []string{
		fmt.Sprintf(`REVOKE SELECT ON ALL TABLES IN SCHEMA %s FROM PUBLIC`, schema),
		fmt.Sprintf(`REVOKE USAGE ON SCHEMA %s FROM PUBLIC`, schema),
	} {
		if err := from.Exec(ctx, statement, nil); err != nil {
			t.Fatalf("%q: %v", statement, err)
		}
	}
	if violations, err := store.AddonConfinementViolations(ctx, pool, giver); err != nil {
		t.Fatalf("checking the confinement: %v", err)
	} else if len(violations) > 0 {
		t.Errorf("the revoked schema still reports %v", violations)
	}
}

// mustResetPassword issues a fresh credential for an add-on's role, which is what
// EnsureAddonSchema does at every load: the password lives no longer than the
// process that uses it, so a test that wants its own connection asks for its own.
func mustResetPassword(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	password, err := store.EnsureAddonSchema(context.Background(), pool, name, nil)
	if err != nil {
		t.Fatalf("issuing a credential for %s: %v", name, err)
	}
	return password
}

func errorIsAddonDenied(err error) bool {
	return err != nil && strings.Contains(err.Error(), store.ErrAddonDenied.Error())
}

// The post-condition the host runs after every migration, tested directly.
//
// It exists as a belt: privileges are what confine an add-on's DDL, and this asks
// the catalogue whether they did. Nothing an add-on can do reaches it while the
// role is what it is, so the only way to show it works is to create the situation
// it watches for — a relation outside the add-on's schema owned by the add-on's
// role — using the privileges the *product* has and the add-on does not.
//
// Without this, the check would be code no test had ever seen fail.
func TestTheDDLPostConditionSeesAnObjectOutsideTheSchema(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)
	ctx := context.Background()

	code := addonFixture(t, "minimal")
	installAddon(t, dir, name, code, []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE mine (x int);\n"})
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}

	// Clean to begin with, which is what makes the dirty answer below mean
	// something.
	violations, err := store.AddonConfinementViolations(ctx, pool, name)
	if err != nil {
		t.Fatalf("checking the confinement: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("a freshly migrated add-on already fails its confinement: %v", violations)
	}

	// TOAST is the case that made the first version of this check fail on every
	// add-on with a text column, so a table that has one is created deliberately.
	if _, err := pool.Exec(ctx, `CREATE TABLE public.smuggled (body text)`); err != nil {
		t.Fatalf("creating the table: %v", err)
	}
	if _, err := pool.Exec(ctx,
		fmt.Sprintf(`ALTER TABLE public.smuggled OWNER TO %s`, store.AddonSchema(name))); err != nil {
		t.Fatalf("handing it to the add-on's role: %v", err)
	}

	violations, err = store.AddonConfinementViolations(ctx, pool, name)
	if err != nil {
		t.Fatalf("checking the confinement: %v", err)
	}
	want := fmt.Sprintf("it owns table public.smuggled, which is not in %s", store.AddonSchema(name))
	if len(violations) != 1 || violations[0] != want {
		t.Errorf("the post-condition answered %v, want [%s]", violations, want)
	}
}

// --- what the role can reach that is in no schema at all ---------------------

// A large object is data an add-on's role owns, outside its schema, and it is the
// case the first version of this milestone's confinement claim did not cover.
//
// Nothing here asserts that the capability is *closed*, because it is not:
// `EXECUTE` on `lo_from_bytea` belongs to `PUBLIC`, Postgres has no per-role deny,
// and revoking it needs ownership of a `pg_catalog` function — which the
// application's own role does not have, and where it is not superuser the REVOKE
// is a **silent no-op** rather than an error. D248 has the measurements. So what is
// asserted is the accounting: the count is published, the load post-condition sees
// it, and the add-on is refused on its next load until an operator purges it.
func TestALargeObjectIsSeenAndRefusesTheAddonOnItsNextLoad(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)
	ctx := context.Background()

	installAddon(t, dir, name, addonFixture(t, "minimal"), []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE mine (x int);\n"})
	h, metrics, sink, err := openAddonHost(t, dir, pool, dsn)
	if err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}

	confined, err := store.OpenAddonDB(ctx, pool, dsn, name, mustResetPassword(t, pool, name), nil)
	if err != nil {
		t.Fatalf("opening the add-on's own connection: %v", err)
	}
	t.Cleanup(confined.Close)

	// One statement, inside the five-second timeout, from the write half of the ABI.
	// The read half cannot do it — a READ ONLY transaction answers *cannot execute
	// lo_from_bytea() in a read-only transaction* — and that asymmetry is checked
	// too, because it is the reason storage_query needs no accounting of its own.
	if err := confined.Exec(ctx, `SELECT lo_from_bytea(0, repeat('x', 100000)::bytea)`, nil); err != nil {
		t.Fatalf("the confining role could not create a large object, "+
			"which is what the rest of this test is about: %v", err)
	}
	if _, err := confined.Query(ctx, `SELECT lo_from_bytea(0, 'x'::bytea)`, nil); err == nil {
		t.Error("the read-only path created a large object")
	}

	// Invisible to the schema's size, by construction: it is in pg_largeobject and
	// not in pg_class.
	bytes, err := store.AddonSchemaBytes(ctx, pool, name)
	if err != nil {
		t.Fatalf("measuring the schema: %v", err)
	}
	if bytes > 50000 {
		t.Errorf("the schema measures %d bytes, so this test is no longer about "+
			"data the size gauge cannot see", bytes)
	}
	los, err := store.AddonLargeObjects(ctx, pool, name)
	if err != nil {
		t.Fatalf("counting large objects: %v", err)
	}
	if los != 1 {
		t.Errorf("the add-on's role owns %d large objects, want 1", los)
	}

	h.ObserveSchemaSizes(ctx)
	prefix := fmt.Sprintf(`linkctrl_addon_large_objects{addon="%s"} `, name)
	if line := seriesLike(scrape(t, metrics), prefix); strings.TrimSpace(line) != strings.TrimSpace(prefix+"1") {
		t.Errorf("the scrape carries %q, want %q", strings.TrimSpace(line), strings.TrimSpace(prefix+"1"))
	}

	// The post-condition names it, and it names it as a large object rather than as
	// a relation, so an operator reading the refusal knows which purge to run.
	violations, err := store.AddonConfinementViolations(ctx, pool, name)
	if err != nil {
		t.Fatalf("checking the confinement: %v", err)
	}
	if len(violations) != 1 || !strings.HasPrefix(violations[0], "it owns large object ") {
		t.Fatalf("the post-condition answered %v, want one large object", violations)
	}

	// And the next load refuses the add-on. This add-on shipped a migration, but the
	// check is deliberately outside that branch — a module with no .sql file at all
	// can own one of these, because a query is what creates it.
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err == nil {
		t.Errorf("the add-on loaded again while owning data outside its schema\n%s", sink.String())
	} else if !strings.Contains(err.Error(), "large object") {
		t.Errorf("the refusal does not name the large object: %v", err)
	}
}

// The purge docs/operations.md documents, run as written.
//
// It grew a line for exactly this reason: `DROP SCHEMA … CASCADE` does not touch a
// large object and `DROP ROLE` then fails with *owner of large object*, so the
// procedure as it shipped left the disk allocated and the role behind. `DROP OWNED
// BY` is what drops one.
func TestThePurgeDropsALargeObjectTheSchemaDropDoesNot(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)
	ctx := context.Background()
	schema := store.AddonSchema(name)

	installAddon(t, dir, name, addonFixture(t, "minimal"), []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE mine (x int);\n"})
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}
	confined, err := store.OpenAddonDB(ctx, pool, dsn, name, mustResetPassword(t, pool, name), nil)
	if err != nil {
		t.Fatalf("opening the add-on's own connection: %v", err)
	}
	if err := confined.Exec(ctx, `SELECT lo_from_bytea(0, repeat('x', 100000)::bytea)`, nil); err != nil {
		t.Fatalf("creating the large object: %v", err)
	}
	confined.Close()

	if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA %s CASCADE`, schema)); err != nil {
		t.Fatalf("dropping the schema: %v", err)
	}
	// The message says only *some objects depend on it*; which object is in the
	// DETAIL, which is where an operator reads it and where this test looks.
	_, err = pool.Exec(ctx, fmt.Sprintf(`DROP ROLE %s`, schema))
	var pgErr *pgconn.PgError
	if err == nil {
		t.Fatal("the role dropped with a large object still owned, so this test proves nothing")
	} else if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Detail, "large object") {
		t.Errorf("the role refused to drop for some other reason: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP OWNED BY %s`, schema)); err != nil {
		t.Fatalf("DROP OWNED BY: %v", err)
	}
	if los, err := store.AddonLargeObjects(ctx, pool, name); err != nil || los != 0 {
		t.Errorf("%d large objects survive DROP OWNED BY (err %v)", los, err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP ROLE %s`, schema)); err != nil {
		t.Errorf("the role still cannot be dropped: %v", err)
	}
}

// A temp relation was the fourth place an add-on could own something that an
// enumeration of places had not looked for, and this asserts both halves of the
// answer to it.
//
// The narrowing first: EnsureAddonSchema revokes PUBLIC's TEMPORARY on the
// database, so every spelling is refused. Five of them, because one refusal proves
// a keyword and not a capability.
//
// Then the post-condition, with the privilege granted back — which is not a
// contrivance. The revoke is a no-op when the application does not own the
// database, and `pg_dump -Fc` carries no database ACL, so *TEMPORARY is back* is
// the state after any restore. What must hold there is that the shape sees a
// `pg_temp_N` relation without anybody having added `pg_temp` to a list.
func TestATempRelationIsRefusedAndSeenAnyway(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)
	ctx := context.Background()

	installAddon(t, dir, name, addonFixture(t, "minimal"), []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE mine (x int);\n"})
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}
	confined, err := store.OpenAddonDB(ctx, pool, dsn, name, mustResetPassword(t, pool, name), nil)
	if err != nil {
		t.Fatalf("opening the add-on's own connection: %v", err)
	}
	t.Cleanup(confined.Close)

	for _, stmt := range []string{
		// A name each, so a spelling that is permitted cannot make the next one fail
		// with *relation already exists* and look like a refusal.
		`CREATE TEMP TABLE smuggled1 (body text)`,
		`CREATE TABLE pg_temp.smuggled2 (body text)`,
		`CREATE TEMPORARY TABLE smuggled3 AS SELECT 1`,
		`CREATE UNLOGGED TABLE pg_temp.smuggled4 (body text)`,
		`SELECT 1 AS x INTO TEMP smuggled5`,
	} {
		err := confined.Exec(ctx, stmt, nil)
		if err == nil {
			t.Errorf("%q was accepted", stmt)
			continue
		}
		if !errorIsAddonDenied(err) || !strings.Contains(err.Error(), "temporary tables") {
			t.Errorf("%q failed with %v, want the refusal to name temporary tables", stmt, err)
		}
	}

	// The state a restore leaves, and the state an instance whose database user is
	// not the owner is in from the start.
	if _, err := pool.Exec(ctx, `GRANT TEMPORARY ON DATABASE `+
		pgx.Identifier{dbNameOf(t, pool)}.Sanitize()+` TO PUBLIC`); err != nil {
		t.Fatalf("granting the privilege back: %v", err)
	}
	if err := confined.Exec(ctx, `CREATE TEMP TABLE smuggled6 (body text)`, nil); err != nil {
		t.Fatalf("with TEMPORARY granted back the add-on still could not create one, "+
			"so the rest of this test proves nothing: %v", err)
	}
	// Held by the same pooled connection, which is why it is still there to be seen:
	// a pgxpool is not a session, and that cuts this way too.
	violations, err := store.AddonConfinementViolations(ctx, pool, name)
	if err != nil {
		t.Fatalf("checking the confinement: %v", err)
	}
	if len(violations) != 1 || !strings.Contains(violations[0], ".smuggled6, which is not in ") ||
		!strings.Contains(violations[0], "pg_temp") {
		t.Fatalf("the post-condition answered %v, want one pg_temp relation", violations)
	}
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err == nil {
		t.Errorf("the add-on loaded again while owning a temp relation\n%s", sink.String())
	} else if !strings.Contains(err.Error(), "pg_temp") {
		t.Errorf("the refusal does not name the temp relation: %v", err)
	}
}

// --- what the role can set on itself -----------------------------------------

// A setting the add-on's role carries is not an object, so nothing in the two
// catalogues sees it — and until this milestone's fourth pass it survived every
// boot, because the load re-applied the host's own search path without clearing
// anything else.
//
// `work_mem` is `PGC_USERSET`, so the confined role may `ALTER ROLE CURRENT_USER
// SET` it on itself and there is no per-role deny for that any more than for
// `EXECUTE` on `lo_from_bytea`. It lands in `rolconfig` and every connection the
// add-on's pool opens afterwards inherits it, times store.AddonMaxConns: measured,
// one `READ ONLY` query inside the statement timeout peaked a backend at 1.37 GB
// resident against 31 MB for the same query at the 4 MB default, which spills to
// disk instead.
//
// The repair is a reset before the search-path pin, and it is the only narrowing in
// this family conditional on neither superuser nor database ownership. D253.
//
// **`ALTER ROLE … RESET ALL` was not the whole of it, and this test could not have
// said so.** It set one variant and asked `pg_roles`, which holds the
// `pg_db_role_setting` rows whose `setdatabase` is 0 and no others — so the
// `IN DATABASE` row the add-on can write beside it was outside the catalogue this
// test reads, and it passed for the whole life of the defect. F288. It now sets
// three variants and reads `pg_db_role_setting` itself, which is the only
// catalogue that can be asked *is there anything left*.
//
// The third variant is the one that decides the repair's shape: `IN DATABASE
// postgres` is accepted by a role that has no business in that database, so a
// second reset scoped to `current_database()` is evaded by naming any other name.
// D279.
//
// **Both halves are asserted here and the order matters**: that the role can still
// set the parameter, because a refusal would make the rest of the test prove
// nothing, and that the next load has cleared it while leaving the search path
// pinned, because a `RESET ALL` after the pin would clear that too.
func TestARoleSettingTheAddonMadeDoesNotSurviveTheNextLoad(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)
	ctx := context.Background()

	installAddon(t, dir, name, addonFixture(t, "minimal"), []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE mine (x int);\n"})
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}
	confined, err := store.OpenAddonDB(ctx, pool, dsn, name, mustResetPassword(t, pool, name), nil)
	if err != nil {
		t.Fatalf("opening the add-on's own connection: %v", err)
	}
	t.Cleanup(confined.Close)

	own := dbNameOf(t, pool)
	// Three scopes, and the third is not this add-on's database. `postgres` is a
	// database the confined role has no reason to reach and need not be able to
	// reach: `IN DATABASE` takes a name, not a connection.
	for _, stmt := range []string{
		`ALTER ROLE CURRENT_USER SET work_mem = '4GB'`,
		`ALTER ROLE CURRENT_USER IN DATABASE ` + own + ` SET work_mem = '4GB'`,
		`ALTER ROLE CURRENT_USER IN DATABASE postgres SET work_mem = '2GB'`,
	} {
		if err := confined.Exec(ctx, stmt, nil); err != nil {
			t.Fatalf("the confined role could not run %q, so this test's subject "+
				"does not exist: %v", stmt, err)
		}
	}
	before := roleSettings(t, pool, name)
	for _, where := range []string{"", own, "postgres"} {
		if !slices.ContainsFunc(before[where], func(e string) bool {
			return strings.HasPrefix(e, "work_mem=")
		}) {
			t.Fatalf("pg_db_role_setting holds %v, want a work_mem parked in %q — "+
				"without which the reset below proves nothing", before, where)
		}
	}

	// The next load, which is the only thing that clears it: nothing on the request
	// path resets a role.
	if _, err := store.EnsureAddonSchema(ctx, pool, name, nil); err != nil {
		t.Fatalf("re-running the load: %v", err)
	}
	want := map[string][]string{"": {"search_path=" + store.AddonSchema(name)}}
	if got := roleSettings(t, pool, name); !maps.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("pg_db_role_setting holds %v after the load, want exactly %v", got, want)
	}
	// The catalogue is the claim; this is the consequence, asked in the database the
	// add-on does not run in and where no connection of this test's will ever look
	// again.
	if got := roleSettings(t, pool, name)["postgres"]; len(got) != 0 {
		t.Errorf("the load left %v parked in another database", got)
	}

	// A pool of its own, so these are new backends reading the role's defaults
	// rather than connections that predate the reset — which is where the setting
	// was inherited from in the first place.
	fresh, err := store.OpenAddonDB(ctx, pool, dsn, name, mustResetPassword(t, pool, name), nil)
	if err != nil {
		t.Fatalf("opening a second connection as the add-on: %v", err)
	}
	t.Cleanup(fresh.Close)
	rows, err := fresh.Query(ctx,
		`SELECT current_setting('work_mem') AS wm, current_setting('search_path') AS sp`, nil)
	if err != nil {
		t.Fatalf("reading the settings back: %v", err)
	}
	var read []struct {
		WM string `json:"wm"`
		SP string `json:"sp"`
	}
	if err := json.Unmarshal(rows, &read); err != nil || len(read) != 1 {
		t.Fatalf("the read answered %s (%v)", rows, err)
	}
	if read[0].WM == "4GB" {
		t.Errorf("a fresh connection still reads work_mem=%s", read[0].WM)
	}
	// The re-pin, asserted rather than assumed: the reset runs first, so a wrong
	// order would leave the add-on's unqualified names resolving in public.
	if read[0].SP != store.AddonSchema(name) {
		t.Errorf("the search path is %q, want %q — the reset cleared the pin",
			read[0].SP, store.AddonSchema(name))
	}
}

// roleSettings is every session default an add-on's role carries, keyed by the
// database it applies in — "" for the cluster-wide row.
//
// **It reads `pg_db_role_setting` and not `pg_roles`**, which is the difference
// between this helper and the one it replaces. `pg_roles.rolconfig` is that
// catalogue filtered to `setdatabase = 0`, so a verifier built on it cannot see an
// `IN DATABASE` row however many assertions are stacked on top of it — F288's
// second half, and the reason the test above passed throughout the defect's life.
func roleSettings(t *testing.T, pool *pgxpool.Pool, name string) map[string][]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT coalesce(d.datname, ''), s.setconfig
		  FROM pg_db_role_setting s
		  LEFT JOIN pg_database d ON d.oid = s.setdatabase
		 WHERE s.setrole = (SELECT oid FROM pg_roles WHERE rolname = $1)`,
		store.AddonSchema(name))
	if err != nil {
		t.Fatalf("reading %s's role settings: %v", store.AddonSchema(name), err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var db string
		var config []string
		if err := rows.Scan(&db, &config); err != nil {
			t.Fatalf("reading %s's role settings: %v", store.AddonSchema(name), err)
		}
		out[db] = config
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading %s's role settings: %v", store.AddonSchema(name), err)
	}
	return out
}

// The post-condition on a setting, which is the half of F288's answer that does not
// depend on the reset being right.
//
// A setting is not an object, so the five ownership and grant branches of
// [store.AddonConfinementViolations] cannot see one however carefully they
// enumerate — the same shape of blindness that let the defect ship, one catalogue
// over. The sixth branch reads `pg_db_role_setting`, and what it must do is report
// every scope while permitting exactly the row the load itself writes.
//
// Both directions are asserted, because a check that reported the pin would refuse
// every add-on on every boot and a check that permitted a parked row would report
// nothing ever. Then the load, which repairs what the report named: in ordinary
// operation this branch is silent, and it is a regression detector rather than a
// gate the add-on trips today. D279.
func TestSettingsParkedInAnyDatabaseAreReportedByTheConfinementCheck(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)
	ctx := context.Background()

	installAddon(t, dir, name, addonFixture(t, "minimal"), []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE mine (x int);\n"})
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}
	confined, err := store.OpenAddonDB(ctx, pool, dsn, name, mustResetPassword(t, pool, name), nil)
	if err != nil {
		t.Fatalf("opening the add-on's own connection: %v", err)
	}
	t.Cleanup(confined.Close)

	// The pin is in place and nothing else is, so the empty answer below is a claim
	// about the check rather than about an empty catalogue.
	violations, err := store.AddonConfinementViolations(ctx, pool, name)
	if err != nil {
		t.Fatalf("checking the confinement: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("a freshly loaded add-on is already in violation: %v", violations)
	}

	own := dbNameOf(t, pool)
	for _, stmt := range []string{
		`ALTER ROLE CURRENT_USER SET statement_timeout = '1h'`,
		`ALTER ROLE CURRENT_USER IN DATABASE ` + own + ` SET work_mem = '4GB'`,
		`ALTER ROLE CURRENT_USER IN DATABASE postgres SET work_mem = '2GB'`,
	} {
		if err := confined.Exec(ctx, stmt, nil); err != nil {
			t.Fatalf("the confined role could not run %q: %v", stmt, err)
		}
	}

	violations, err = store.AddonConfinementViolations(ctx, pool, name)
	if err != nil {
		t.Fatalf("checking the confinement: %v", err)
	}
	want := []string{
		"it has parked statement_timeout in every database",
		"it has parked work_mem in database " + own,
		"it has parked work_mem in database postgres",
	}
	slices.Sort(want)
	if got := slices.Clone(violations); !slices.Equal(got, want) {
		t.Fatalf("the post-condition answered %v, want %v", got, want)
	}

	// And the load repairs every one of them, which is why an operator never sees
	// this finding unless the reset has stopped working.
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err != nil {
		t.Fatalf("the add-on did not load over its own parked settings: %v\n%s",
			err, sink.String())
	}
	violations, err = store.AddonConfinementViolations(ctx, pool, name)
	if err != nil {
		t.Fatalf("checking the confinement: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("the load left %v behind", violations)
	}
}

// The invariant the load's tolerance of a vanished database rests on.
//
// `resetRoleSettings` enumerates the databases a role has settings in and then
// resets each, and a `DROP DATABASE` between the two answers `3D000` — which
// inside the load's transaction is an abort rather than a row to skip, so it used
// to stop a `required` add-on's instance over a drop anywhere in the cluster. The
// repair tolerates that one code, and what makes tolerating it lossless is this:
// the drop takes the role's rows for that database with it, so there was nothing
// left for the statement to clear.
//
// The window itself has no test. It is between two statements this code issues and
// there is no seam a test can hold it open at; what is asserted here is the fact
// the argument rests on, and the load surviving the state that follows. D280.
func TestDroppingADatabaseTakesTheRoleSettingsParkedInItWithIt(t *testing.T) {
	name := addonName(t)
	pool, dsn, _ := newAddonDB(t, name)
	ctx := context.Background()

	other := "t_" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")[:20]
	admin, err := pgxpool.New(ctx, dsnFor("postgres"))
	if err != nil {
		t.Fatalf("connect to maintenance database: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+other); err != nil {
		t.Fatalf("creating a second database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+other+` WITH (FORCE)`)
	})

	confined, err := store.OpenAddonDB(ctx, pool, dsn, name, mustResetPassword(t, pool, name), nil)
	if err != nil {
		t.Fatalf("opening the add-on's own connection: %v", err)
	}
	if err := confined.Exec(ctx,
		`ALTER ROLE CURRENT_USER IN DATABASE `+other+` SET work_mem = '4GB'`, nil); err != nil {
		t.Fatalf("parking a setting in the second database: %v", err)
	}
	confined.Close()
	if got := roleSettings(t, pool, name)[other]; len(got) != 1 {
		t.Fatalf("the setting did not land in %s: %v", other, roleSettings(t, pool, name))
	}

	if _, err := admin.Exec(ctx, `DROP DATABASE `+other+` WITH (FORCE)`); err != nil {
		t.Fatalf("dropping the second database: %v", err)
	}
	if got := roleSettings(t, pool, name)[other]; len(got) != 0 {
		t.Errorf("dropping %s left %v in pg_db_role_setting — the load's tolerance of "+
			"3D000 would then be losing a row rather than skipping a no-op", other, got)
	}
	// And the load still runs over the state the drop left.
	if _, err := store.EnsureAddonSchema(ctx, pool, name, nil); err != nil {
		t.Fatalf("the load did not survive a dropped database: %v", err)
	}
}

// --- the direction a restore breaks ------------------------------------------

// `pg_dump` carries no roles, so a restore into a cluster whose roles were not
// restored leaves an add-on's tables owned by the application — and asking only
// what the *role* owns passes that state cleanly.
//
// Reproduced by re-owning the table rather than by running pg_restore, because the
// end state is what the post-condition has to answer for and a dump adds nothing
// to it: the measured restore's own output was three failed `ALTER … OWNER TO`
// lines and a schema the next boot repaired while its tables stayed behind.
func TestATableTheApplicationOwnsInsideTheSchemaRefusesTheAddon(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)
	ctx := context.Background()
	schema := store.AddonSchema(name)

	installAddon(t, dir, name, addonFixture(t, "minimal"), []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE notes (body text);\n"})
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}
	confined, err := store.OpenAddonDB(ctx, pool, dsn, name, mustResetPassword(t, pool, name), nil)
	if err != nil {
		t.Fatalf("opening the add-on's own connection: %v", err)
	}
	t.Cleanup(confined.Close)
	if err := confined.Exec(ctx, `INSERT INTO notes (body) VALUES ('before')`, nil); err != nil {
		t.Fatalf("the add-on could not write its own table: %v", err)
	}

	if _, err := pool.Exec(ctx,
		fmt.Sprintf(`ALTER TABLE %s.notes OWNER TO CURRENT_USER`, schema)); err != nil {
		t.Fatalf("re-owning the table: %v", err)
	}
	// The consequence, asserted before the detection: this is what a `degrade`
	// add-on serves and what a `required` one stops the instance over.
	if err := confined.Exec(ctx, `INSERT INTO notes (body) VALUES ('after')`, nil); !errorIsAddonDenied(err) {
		t.Errorf("the add-on could still write a table it no longer owns: %v", err)
	}

	violations, err := store.AddonConfinementViolations(ctx, pool, name)
	if err != nil {
		t.Fatalf("checking the confinement: %v", err)
	}
	want := fmt.Sprintf("it does not own table %s.notes, which is in %s", schema, schema)
	if len(violations) != 1 || violations[0] != want {
		t.Fatalf("the post-condition answered %v, want [%s]", violations, want)
	}
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err == nil {
		t.Errorf("the add-on loaded over a table it does not own\n%s", sink.String())
	} else if !strings.Contains(err.Error(), "does not own") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// The schema itself is the one part of a restore the next boot repairs, and this
// is what proves the repair rather than assuming it: EnsureAddonSchema's
// `ALTER SCHEMA … OWNER TO` runs on every load, so a schema owned by the
// application is the add-on's again before the post-condition looks.
func TestASchemaTheApplicationOwnsIsRepairedOnLoad(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)
	ctx := context.Background()
	schema := store.AddonSchema(name)

	installAddon(t, dir, name, addonFixture(t, "minimal"), []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE mine (x int);\n"})
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}
	if _, err := pool.Exec(ctx,
		fmt.Sprintf(`ALTER SCHEMA %s OWNER TO CURRENT_USER`, schema)); err != nil {
		t.Fatalf("re-owning the schema: %v", err)
	}
	violations, err := store.AddonConfinementViolations(ctx, pool, name)
	if err != nil {
		t.Fatalf("checking the confinement: %v", err)
	}
	want := fmt.Sprintf("it does not own schema %s, which is its own", schema)
	if len(violations) != 1 || violations[0] != want {
		t.Fatalf("the post-condition answered %v, want [%s]", violations, want)
	}
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err != nil {
		t.Fatalf("the load did not repair the schema's owner: %v\n%s", err, sink.String())
	}
	var owner string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = $1`, schema).Scan(&owner); err != nil {
		t.Fatalf("reading the schema's owner: %v", err)
	}
	if owner != schema {
		t.Errorf("the schema is owned by %s, want %s", owner, schema)
	}
}

// dbNameOf is the database a pool is connected to, which the test databases make
// per-test and therefore cannot be a constant.
func dbNameOf(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatalf("reading the database name: %v", err)
	}
	return name
}

// --- the product's own locks -------------------------------------------------

// An add-on's session may take one of this product's job leader-election locks —
// the keys are constants in a public repository and `pg_advisory_lock` is EXECUTE
// to PUBLIC — and what this asserts is that it cannot **hold** one.
//
// The rollback both storage paths perform does not release a session-level lock,
// which is why the release is its own mechanism: AddonDB.releaseLocks runs
// `pg_advisory_unlock_all` before the connection goes back to the pool, and
// AddonDB.pin runs it again as belt. Without it the lock lives on the pooled
// connection until pgxpool reaps it — on every replica, since every replica uses
// the same key — and rollup, mail, webhooks, automation and housekeeping stop.
//
// **The ordering is half the claim, and this test is what proved it.** The first
// implementation used pgxpool's AfterRelease hook, which pgxpool runs in a
// goroutine; the lock was released, eventually, and the product asking for it the
// moment the add-on's call returned still found it held.
func TestAnAddonCannotHoldOneOfTheProductsJobLocks(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)
	ctx := context.Background()

	installAddon(t, dir, name, addonFixture(t, "minimal"), []string{abi.PermissionStorage}, nil)
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}
	confined, err := store.OpenAddonDB(ctx, pool, dsn, name, mustResetPassword(t, pool, name), nil)
	if err != nil {
		t.Fatalf("opening the add-on's own connection: %v", err)
	}
	t.Cleanup(confined.Close)

	// advisoryLockKeyMaintenance's value, which cmd/linkctrl declares and this
	// package cannot import. The assertion does not depend on the number — a lock
	// under any key must not survive the call — and naming the product's own is what
	// makes the test about the finding rather than about the mechanism.
	const maintenance = 7810203205416190213

	// One connection, held: a session-level advisory lock belongs to a session, so
	// asking a *pool* whether the lock is free can take it on one connection and try
	// to release it on another. That is how this test first failed, and it was the
	// test that was wrong.
	leader, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring the product's own connection: %v", err)
	}
	defer leader.Release()

	for _, statement := range []string{
		fmt.Sprintf(`SELECT pg_advisory_lock(%d)`, maintenance),
		// Twice over, because the read path runs in a READ ONLY transaction and that
		// does not refuse a lock either.
		fmt.Sprintf(`SELECT pg_advisory_lock(%d) IS NULL`, maintenance),
	} {
		if strings.Contains(statement, "IS NULL") {
			if _, err := confined.Query(ctx, statement, nil); err != nil {
				t.Fatalf("%q: %v", statement, err)
			}
		} else if err := confined.Exec(ctx, statement, nil); err != nil {
			t.Fatalf("%q: %v", statement, err)
		}
		// From the product's own pool: the lock is free the moment the add-on's call
		// has returned.
		var got bool
		if err := leader.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(maintenance)).Scan(&got); err != nil {
			t.Fatalf("asking for the lock: %v", err)
		}
		if !got {
			t.Errorf("after %q the product cannot take its own maintenance lock", statement)
		}
		if _, err := leader.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(maintenance)); err != nil {
			t.Fatalf("releasing the lock: %v", err)
		}
	}
}

// --- more than one replica ---------------------------------------------------

// EnsureAddonSchema mints a fresh credential on every load, so the second replica
// to boot invalidates the first one's. Recovering from that is what keeps add-on
// storage working on every replica rather than only on the last one to start.
//
// The stale credential is what a running replica holds after another has booted;
// opening with one is therefore the same code path a replica's next connection
// takes, and the assertion is that it succeeds and says so at warn.
func TestACredentialAnotherReplicaRotatedIsMintedAgain(t *testing.T) {
	name := addonName(t)
	pool, dsn, dir := newAddonDB(t, name)
	ctx := context.Background()

	installAddon(t, dir, name, addonFixture(t, "minimal"), []string{abi.PermissionStorage},
		map[string]string{"00001_own.sql": "-- +goose Up\nCREATE TABLE mine (x int);\n"})
	if _, _, sink, err := openAddonHost(t, dir, pool, dsn); err != nil {
		t.Fatalf("the add-on did not load: %v\n%s", err, sink.String())
	}

	stale := mustResetPassword(t, pool, name)
	// The other replica boots.
	if fresh := mustResetPassword(t, pool, name); fresh == stale {
		t.Fatal("two calls produced the same credential, so nothing was rotated")
	}

	// Without the application's pool there is nothing to re-mint with, which is what
	// this looked like before: FATAL 28P01, at the add-on's next connection.
	if _, err := store.OpenAddonDB(ctx, nil, dsn, name, stale, nil); err == nil {
		t.Error("a stale credential connected with no way to re-mint one")
	} else if !strings.Contains(err.Error(), "28P01") {
		t.Errorf("the failure is not a password refusal: %v", err)
	}

	sink := &logSink{}
	log := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	confined, err := store.OpenAddonDB(ctx, pool, dsn, name, stale, log)
	if err != nil {
		t.Fatalf("a stale credential did not recover: %v\n%s", err, sink.String())
	}
	t.Cleanup(confined.Close)
	if _, err := confined.Query(ctx, `SELECT count(*) AS n FROM mine`, nil); err != nil {
		t.Errorf("the add-on's own table is unreachable after the re-mint: %v", err)
	}
	if !strings.Contains(sink.String(), "rotated by another replica") {
		t.Errorf("nothing was logged about the rotation\n%s", sink.String())
	}
}
