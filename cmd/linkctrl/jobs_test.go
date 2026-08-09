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

// TestEachJobFamilyHasItsOwnAdvisoryLockKey pins the property the per-family
// scheduler goroutines depend on: every family locks a key of its own.
//
// Two families sharing a key would not serialize against each other — they
// would *skip* each other. pg_try_advisory_lock never blocks, so with each
// family on its own goroutine, the same-key loser on the same leader drops its
// whole tick and counts it as a follower's skip: work is lost, and the
// follower-liveness reading ObserveJobSkipped exists for is poisoned on a
// replica whose scheduler is perfectly healthy. And no family may take the
// retired generation-0 key, because an old binary in a rolling deploy still
// holds that one for everything.
//
// No database: this is a test of the table, not of Postgres.
func TestEachJobFamilyHasItsOwnAdvisoryLockKey(t *testing.T) {
	fams := (&jobRunner{}).families()

	const wantFamilies = 8
	if len(fams) != wantFamilies {
		t.Fatalf("got %d job families, want %d; a family added or merged has to update this count and the key block in jobs.go together",
			len(fams), wantFamilies)
	}

	const (
		keyPrefix     = int64(0x6c63_6a6f_6273) // "lcjobs", the shared high six bytes
		keyGeneration = int64(0x01)             // generation 0x00 was the single-key scheduler
	)
	seenKey := make(map[int64]string, len(fams))
	seenName := make(map[string]bool, len(fams))
	for _, f := range fams {
		if f.name == "" {
			t.Fatal("a job family has no name")
		}
		if seenName[f.name] {
			t.Errorf("family name %q is used twice", f.name)
		}
		seenName[f.name] = true
		if f.every <= 0 {
			t.Errorf("family %q has no interval; its ticker would panic", f.name)
		}
		if f.onTick == nil {
			t.Errorf("family %q has nothing to run", f.name)
		}
		if other, dup := seenKey[f.key]; dup {
			t.Errorf("families %q and %q share advisory key %#x; on one leader they would skip each other's ticks, not queue behind them",
				other, f.name, f.key)
		}
		seenKey[f.key] = f.name
		if f.key>>16 != keyPrefix || (f.key>>8)&0xff != keyGeneration {
			t.Errorf("family %q key %#x is outside the documented derivation (%#x + generation %#x + family byte); an operator following the psql recipe in jobs.go would inspect the wrong lock",
				f.name, f.key, keyPrefix, keyGeneration)
		}
		if f.key == advisoryLockKeyRetiredV1 {
			t.Errorf("family %q reuses the retired single-scheduler key; an old binary in a rolling deploy still holds it, so this family would contend with every job of the old world at once",
				f.name)
		}
	}
	for _, name := range []string{"mail", "webhooks"} {
		if !seenName[name] {
			t.Errorf("no %q family: the mail and webhook drains are separate on purpose, so one stalled remote cannot delay the other's queue",
				name)
		}
	}
}

// TestOneFamilysLockDoesNotBlockAnothers is the defect's mechanism, run
// directly: a session holding one family's key must not stop a different
// family running, and must stop that same family exactly.
//
// This is what makes per-family goroutines safe at all. Under a single shared
// key, concurrent families on the same replica would have turned into skips;
// with per-family keys the only thing a held key suppresses is its own family.
func TestOneFamilysLockDoesNotBlockAnothers(t *testing.T) {
	pool := jobsPool(t)
	ctx := context.Background()

	// A second session holds the mail family's key, as another replica's
	// leader would.
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	var held bool
	if err := holder.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", advisoryLockKeyMail).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("could not take the mail key on a fresh database; the rest of this test would prove nothing")
	}

	j := &jobRunner{pool: pool, log: slog.New(slog.DiscardHandler)}

	ran := false
	j.withLeadership(ctx, advisoryLockKeyWebhooks, "webhooks", func(context.Context) error {
		ran = true
		return nil
	})
	if !ran {
		t.Error("holding the mail family's key stopped the webhook family; the families are still sharing a lock")
	}

	ran = false
	j.withLeadership(ctx, advisoryLockKeyMail, "mail", func(context.Context) error {
		ran = true
		return nil
	})
	if ran {
		t.Error("the mail family ran while another session held its key; leadership is not being checked per family")
	}
}

// TestLeadershipMovesToAFollowerWhenTheLeaderDies is the failover mechanism,
// run as one (M56).
//
// Every claim in the failover contract rests on a single Postgres property: a
// session-level advisory lock is released the instant the session holding it
// ends, whether it ended politely or was killed. Nothing in this product
// watches for a dead leader, holds a heartbeat, or elects anything — the next
// follower to tick simply finds the lock free. That is the whole of it, and it
// is why running several replicas needs no coordinator.
//
// The kill is real rather than simulated. `pg_terminate_backend` on the
// leader's own backend is exactly what Postgres sees when a replica's container
// is killed: the connection goes away without an unlock ever being sent. What
// this cannot reproduce is the leader's *goroutine* dying with it, so the pass
// is abandoned by hand at the same moment — which is the honest half, because
// a leader that keeps working after its lock is gone is the two-leaders window
// D77 already documents rather than the failover this test is about.
//
// Bounded, not instant, and the bound is a family's tick interval away in
// production: the fastest family ticks every 60 seconds, so a killed leader
// costs at most one interval of that family's work. Here the follower is ticked
// in a loop, so what is measured is the lock becoming free — the part this
// product is responsible for.
func TestLeadershipMovesToAFollowerWhenTheLeaderDies(t *testing.T) {
	pool := jobsPool(t)
	ctx := context.Background()

	// A scratch table so "the work" is a row somebody can count, rather than a
	// boolean only this test can see. The database is this test's own.
	if _, err := pool.Exec(ctx, `CREATE TABLE failover_probe (replica text NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.DiscardHandler)

	// The leader takes a connection of its own so the kill has one target.
	leaderConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	leaderPID := leaderConn.Conn().PgConn().PID()

	var held bool
	if err := leaderConn.QueryRow(ctx,
		"SELECT pg_try_advisory_lock($1)", advisoryLockKeyRollup).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("could not take the rollup key on a fresh database; the rest of this test would prove nothing")
	}

	follower := &jobRunner{pool: pool, log: log}
	work := func(replica string) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := pool.Exec(ctx, `INSERT INTO failover_probe (replica) VALUES ($1)`, replica)
			return err
		}
	}

	// While the leader is up, the follower skips. Work is not done twice.
	follower.withLeadership(ctx, advisoryLockKeyRollup, "rollup", work("follower"))
	if n := probeRows(t, pool); n != 0 {
		t.Fatalf("the follower did %d passes while the leader held the family's lock, want 0; "+
			"two replicas are running the same job", n)
	}

	// The leader dies. No unlock is sent — that is the point of terminating the
	// backend rather than releasing the lock.
	var terminated bool
	if err := pool.QueryRow(ctx, "SELECT pg_terminate_backend($1)", leaderPID).Scan(&terminated); err != nil {
		t.Fatal(err)
	}
	if !terminated {
		t.Fatal("could not terminate the leader's backend; nothing below would be a failover")
	}
	leaderConn.Release()

	const bound = 5 * time.Second
	start := time.Now()
	var took time.Duration
	for time.Since(start) < bound {
		follower.withLeadership(ctx, advisoryLockKeyRollup, "rollup", work("follower"))
		if probeRows(t, pool) > 0 {
			took = time.Since(start)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	switch n := probeRows(t, pool); {
	case n == 0:
		t.Errorf("no follower picked the family up within %s of the leader dying; "+
			"the advisory lock is not the failover mechanism the contract says it is", bound)
	case n > 1:
		t.Errorf("the family ran %d times after one failover, want 1; work is being done twice", n)
	default:
		t.Logf("the follower took leadership %s after the leader's backend was terminated", took)
	}
}

func probeRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM failover_probe`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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
// mean everywhere else in the runner.
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
