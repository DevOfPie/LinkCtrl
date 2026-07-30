// Package store owns the database layer: migrations, partition maintenance
// and the sqlc-generated queries.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/pressly/goose/v3/lock"
)

// Migrations must live inside this package: go:embed patterns cannot traverse
// "..", so a top-level db/migrations directory could not be embedded into a
// package that needs it.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations is the embedded set rooted at the migration files themselves.
// goose scans the root of the FS it is given, so the "migrations/" prefix has
// to be stripped or it finds nothing.
var Migrations = func() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic("store: cannot open embedded migrations: " + err.Error())
	}
	return sub
}()

// PartitionLookahead is how many months of partitions to create beyond the
// current one.
const PartitionLookahead = 2

// Migrate applies all pending migrations, then ensures partitions exist.
//
// Runs in-process at boot, before the listener opens. An init container would
// need either a shell (distroless has none) or a second image, plus
// depends_on wiring that confuses a first-time operator; in-process means
// `docker compose up` on an empty volume produces a working app with no extra
// concepts.
//
// A Postgres session lock serializes replicas racing at startup, so a rolling
// deploy cannot run the same migration twice.
func Migrate(ctx context.Context, dsn string, log *slog.Logger) error {
	db, err := openStdlib(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	locker, err := lock.NewPostgresSessionLocker(
		// Retry for up to five minutes. A replica that starts while another is
		// mid-migration should wait, not fail and get restarted by the
		// orchestrator into a crash loop.
		lock.WithLockTimeout(5, 60),
	)
	if err != nil {
		return fmt.Errorf("create migration locker: %w", err)
	}

	provider, err := goose.NewProvider(
		database.DialectPostgres,
		db,
		Migrations,
		goose.WithSessionLocker(locker),
		goose.WithVerbose(false),
	)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}

	start := time.Now()
	results, err := provider.Up(ctx)
	for _, r := range results {
		log.Info("migration applied",
			slog.Int64("version", r.Source.Version),
			slog.String("name", r.Source.Path),
			slog.Int64("duration_ms", r.Duration.Milliseconds()),
		)
	}
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	if len(results) == 0 {
		log.Debug("no pending migrations")
	} else {
		log.Info("migrations complete",
			slog.Int("applied", len(results)),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		)
	}

	return nil
}

// Status reports applied and pending migrations.
func Status(ctx context.Context, dsn string) ([]string, error) {
	db, err := openStdlib(dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider(database.DialectPostgres, db, Migrations)
	if err != nil {
		return nil, err
	}
	sources, err := provider.Status(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(sources))
	for _, s := range sources {
		state := "pending"
		applied := ""
		if s.State == goose.StateApplied {
			state = "applied"
			applied = s.AppliedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, fmt.Sprintf("%-6d %-8s %-40s %s",
			s.Source.Version, state, s.Source.Path, applied))
	}
	return out, nil
}

// Down rolls back the most recent migration.
func Down(ctx context.Context, dsn string) error {
	db, err := openStdlib(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider(database.DialectPostgres, db, Migrations)
	if err != nil {
		return err
	}
	_, err = provider.Down(ctx)
	return err
}

// openStdlib bridges pgx to database/sql, which is the interface goose needs.
// Using the pgx stdlib adapter avoids pulling in a second Postgres driver.
func openStdlib(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// Not wrapped: the error text can echo the DSN, password included.
		return nil, fmt.Errorf("parse database URL: invalid connection string")
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	// Same reason as the pgx pool: partition bounds resolve against the
	// session timezone, and migrations are exactly where partitions get made.
	cfg.RuntimeParams["timezone"] = "UTC"
	cfg.RuntimeParams["application_name"] = "linkctrl-migrate"

	return stdlib.OpenDB(*cfg), nil
}
