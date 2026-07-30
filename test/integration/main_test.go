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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/store"
)

const (
	// Used only when TEST_DATABASE_URL is unset. `make test-integration` passes
	// the DSN built from .env, so this literal is the last resort for someone
	// running `go test` by hand — and it is a guess at their password.
	fallbackDSN = "postgres://linkctrl:devpassword@localhost:55432/linkctrl?sslmode=disable"
	// A template database of our own, rather than the application database.
	//
	// Postgres refuses CREATE DATABASE ... TEMPLATE while anything is connected
	// to the source, and in development the app container holds a pool open
	// against linkctrl permanently. Cloning a database nothing connects to
	// avoids having to stop the stack to run tests.
	templateDB = "linkctrl_test_template"
)

func baseDSN() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return fallbackDSN
}

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
func newDB(t *testing.T) *pgxpool.Pool {
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

	pool, err := pgxpool.New(ctx, dsnFor(name))
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

	return pool
}

// newCookieJar returns a cookie jar so a test client keeps its session across
// requests, the way a browser does.
func newCookieJar() (http.CookieJar, error) { return cookiejar.New(nil) }
