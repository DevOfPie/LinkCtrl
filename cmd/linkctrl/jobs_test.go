//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// The scheduler's own tests. This package had none until M45, which is part of
// why F73 shipped: the reload it adds is a *wiring* claim — that a job runs, on
// a clock, without leadership — and nothing about it is visible from
// test/integration, which cannot reach an unexported type in package main.

// jobsPool creates a throwaway database, migrates it, and returns a pool on it.
//
// **It does not connect to TEST_DATABASE_URL itself**, and that is the whole
// point of the helper. The first version did, and it passed here and failed on
// a runner with `relation "domains" does not exist`: a developer's instance has
// a migrated base database, and CI's Postgres service container is empty
// because every other suite migrates a database of its own. So the test was
// asserting against whatever happened to be lying around, which on one machine
// was the right schema and on another was nothing.
//
// Same shape as cmd/lctl's newDemoDB, deliberately — a second way of getting a
// migrated database is a second thing that can be wrong about which one it got.
func jobsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	admin, err := pgxpool.New(ctx, jobsDSNFor("postgres"))
	if err != nil {
		t.Fatalf("connect to the maintenance database: %v\n\n"+
			"These tests need Postgres. Start it with `make up`, and run them with "+
			"`make test-integration` so TEST_DATABASE_URL is set.", err)
	}
	defer admin.Close()

	name := "t_jobs_" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")[:16]
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", name)); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := pgxpool.New(context.Background(), jobsDSNFor("postgres"))
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(),
			fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
	})

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, jobsDSNFor(name), quiet); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	pool, err := pgxpool.New(ctx, jobsDSNFor(name))
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// jobsDSNFor rewrites the database name in TEST_DATABASE_URL.
func jobsDSNFor(name string) string {
	dsn := os.Getenv("TEST_DATABASE_URL")
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

// TestTheHostReloadRunsWithoutASubscriberAndWithoutLeadership is F73's fix as a
// claim that can be shown false.
//
// The verified-hostname set is invalidated over Redis pub/sub, which is
// at-most-once. A published message that is simply lost while the subscription
// stays healthy is never noticed — establish reloads on connect and reconnect,
// and Run bounds silence and probes, but all of that catches silence the replica
// *notices*, and a dropped message on a healthy connection looks like nothing
// happening. On an instance with no Redis at all there is no subscriber, so the
// only reload sites were boot and the subscriber itself, and a second replica
// never reloaded.
//
// No Redis is wired here at all, which is the harsher of the two limbs and the
// one that needs no timing to reproduce.
func TestTheHostReloadRunsWithoutASubscriberAndWithoutLeadership(t *testing.T) {
	pool := jobsPool(t)
	ctx := context.Background()

	id := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO domains (id, hostname, verified_at, verification_token, ssl_status)
		VALUES ($1, 'reload.custom.test', now(), 'tok', 'pending')`, id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM domains WHERE id = $1`, id)
	})

	hosts := redirect.NewHostCache(pool, slog.New(slog.DiscardHandler))
	if err := hosts.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := hosts.Lookup("reload.custom.test"); !ok {
		t.Fatal("the replica did not load the verified hostname; the rest of this test would prove nothing")
	}

	// Unverified with nothing published: no subscriber exists to hear it, which
	// is the state an instance with no Redis is permanently in.
	if _, err := pool.Exec(ctx, `UPDATE domains SET verified_at = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := hosts.Lookup("reload.custom.test"); !ok {
		t.Fatal("the set changed with nothing having reloaded it; this test is not measuring what it thinks")
	}

	j := &jobRunner{
		log:                  slog.New(slog.DiscardHandler),
		hosts:                hosts,
		domainVerifyInterval: time.Hour,
	}
	j.runHostReload(ctx)

	if _, ok := hosts.Lookup("reload.custom.test"); ok {
		t.Error("the replica still serves a hostname that is no longer verified; " +
			"the timed reload is the only backstop pub/sub cannot be")
	}
}

// The job is skipped rather than crashing when there is nothing to reload and
// when the operator switched the pass off, which is what the nil and zero cases
// mean everywhere else on this clock.
func TestTheHostReloadIsSkippedWhenItIsNotConfigured(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)

	(&jobRunner{log: log, domainVerifyInterval: time.Hour}).runHostReload(ctx)

	pool := jobsPool(t)
	hosts := redirect.NewHostCache(pool, log)
	// Zero interval is the documented way to switch re-verification off. The
	// reload follows it, because a set nothing re-checks is one an operator has
	// asked to leave alone.
	(&jobRunner{log: log, hosts: hosts, domainVerifyInterval: 0}).runHostReload(ctx)
	if hosts.Ready() {
		t.Error("a zero interval still reloaded the set")
	}
}
