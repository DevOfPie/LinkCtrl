//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/store"
)

const (
	// A template database of our own, rather than the application database.
	//
	// Postgres refuses CREATE DATABASE ... TEMPLATE while anything is connected
	// to the source, and in development the app container holds a pool open
	// against linkctrl permanently. Cloning a database nothing connects to
	// avoids having to stop the stack to run tests.
	templateDB = "linkctrl_test_template"
)

// baseDSN refuses rather than guessing, which is F260 and D428's table.
//
// There used to be a literal here for "someone running `go test` by hand", and
// it named port **55432** — which is `.env.demo`'s `POSTGRES_PORT`. The test
// instance is 55433. So the last resort pointed at the one stack the two-instance
// split exists to keep tests away from, and what it does on arrival is
// `ensureTemplate`: create a template database and clone it per test. Nothing
// caught it because the demo's password does not happen to match `devpassword`,
// which is luck — `scripts/instance.sh` generates that password, and an instance
// created before it did would have matched.
//
// A default that is wrong in the destructive direction is worse than no default:
// `make db-reset` already defaults to the disposable instance by written
// decision, and this defaulted the other way. So the DSN is required, and the
// message says the two ways to supply one.
func baseDSN() string { return os.Getenv("TEST_DATABASE_URL") }

// dsnFor rewrites the database name in the base DSN.
func dsnFor(name string) string {
	dsn := baseDSN()
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		return dsn
	}
	rest := ""
	if j := strings.Index(dsn[i:], "?"); j >= 0 {
		rest = dsn[i+j:]
	}
	return dsn[:i+1] + name + rest
}

// TestMain builds the template database once, then every test clones it.
//
// Cloning costs about 30ms against roughly 2s to re-migrate. Unlike wrapping
// each test in a transaction it also works with code that opens its own
// transactions, which Register does.
func TestMain(m *testing.M) {
	// Refused before anything connects, rather than defaulted. See [baseDSN].
	if baseDSN() == "" {
		fmt.Fprint(os.Stderr, "integration setup refused: TEST_DATABASE_URL is not set, "+
			"and this suite will not guess one.\n\n"+
			"It creates and drops a template database, so a guessed port is how that "+
			"reaches an instance nobody meant to touch — which is exactly what the "+
			"literal removed here did (F260).\n\n"+
			"  make test-integration        # builds the DSN from .env, against the test instance\n"+
			"  TEST_DATABASE_URL=... go test -tags=integration ./test/integration/...\n")
		os.Exit(1)
	}
	if err := ensureTemplate(); err != nil {
		fmt.Fprintf(os.Stderr, "integration setup failed: %v\n\n"+
			"These tests need Postgres. Start it with:\n  docker compose up -d\n\n"+
			"On an authentication failure, the DSN is wrong rather than absent:\n"+
			"  make test-integration        # builds it from POSTGRES_PASSWORD in .env\n"+
			"  TEST_DATABASE_URL=... go test -tags=integration ./test/integration/...\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func ensureTemplate() error {
	ctx := context.Background()

	// Connect to the maintenance database: CREATE DATABASE cannot run from
	// inside the database being created, and must not run from the template.
	admin, err := pgxpool.New(ctx, dsnFor("postgres"))
	if err != nil {
		return fmt.Errorf("connect to maintenance database: %w", err)
	}
	defer admin.Close()

	// Recreate from scratch each run, so a schema change is picked up rather
	// than silently tested against a stale template.
	if _, err := admin.Exec(ctx,
		fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", templateDB)); err != nil {
		return fmt.Errorf("drop template: %w", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", templateDB)); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, dsnFor(templateDB), quiet); err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}

	// Partitions are created here so every cloned database has them without
	// each test paying the cost.
	pool, err := pgxpool.New(ctx, dsnFor(templateDB))
	if err != nil {
		return fmt.Errorf("connect to template: %w", err)
	}
	if _, err := store.EnsurePartitions(ctx, pool, store.PartitionLookahead); err != nil {
		pool.Close()
		return fmt.Errorf("ensure partitions: %w", err)
	}
	// Closing matters: the template must have no connections when it is cloned.
	pool.Close()

	return nil
}

// newDB returns an isolated database cloned from the template.
func newDB(t *testing.T) *pgxpool.Pool { pool, _ := newTracedDB(t, nil); return pool }

// newDBWithDSN is newDB and the connection string beside it.
//
// The string is what an add-on's *own* confined role connects with — `addon.Open`
// takes a `DSN` as well as a pool, and without it a module declaring
// `storage.own_schema` loads with no schema created (internal/addon/host.go's
// openStorage). Only a fixture that installs such a module needs it, which is why
// this is a second constructor rather than a second return value on newDB.
func newDBWithDSN(t *testing.T) (*pgxpool.Pool, string) { return newTracedDB(t, nil) }

// newTracedDB is newDB with a query tracer attached, for the tests that assert
// how many queries a thing costs rather than what it answers.
//
// The tracer sits on the connection config rather than wrapping the service,
// because the claim being tested is about the round trips that actually reach
// Postgres. A counter around a Go method would still pass if the method below it
// quietly issued two.
func newTracedDB(t *testing.T, tracer pgx.QueryTracer) (*pgxpool.Pool, string) {
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
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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
	})

	return pool, dsn
}

// newCookieJar returns a cookie jar so a test client keeps its session across
// requests, the way a browser does.
func newCookieJar() (http.CookieJar, error) { return cookiejar.New(nil) }
