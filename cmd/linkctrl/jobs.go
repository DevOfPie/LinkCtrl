package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// jobRunner runs the periodic maintenance work.
//
// Leader election is a Postgres advisory lock rather than a scheduler service:
// it needs no extra infrastructure, and the lock is released automatically if
// the holder dies, so a crashed replica does not block the others. Every job
// re-checks the lock, so leadership can move between runs without coordination.
type jobRunner struct {
	pool   *pgxpool.Pool
	salts  *analytics.SaltCache
	roller *analytics.Roller
	log    *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}
}

// advisoryLockKey is hashtext('linkctrl_jobs'), computed once so the value is
// stable across processes and versions.
const advisoryLockKey int64 = 0x6c63_6a6f_6273_0001

func newJobRunner(pool *pgxpool.Pool, salts *analytics.SaltCache, roller *analytics.Roller, log *slog.Logger) *jobRunner {
	return &jobRunner{pool: pool, salts: salts, roller: roller, log: log, done: make(chan struct{})}
}

func (j *jobRunner) start(parent context.Context) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	j.cancel = cancel

	go func() {
		defer close(j.done)

		// Cadences chosen so a missed run is never load-bearing: rollups
		// recompute from raw events, and partitions are maintained two months
		// ahead.
		rollup := time.NewTicker(60 * time.Second)
		hourly := time.NewTicker(time.Hour)
		defer rollup.Stop()
		defer hourly.Stop()

		// Run once at startup rather than waiting a full interval, so a
		// freshly started instance has current numbers.
		j.runRollup(ctx)
		j.runMaintenance(ctx)

		for {
			select {
			case <-ctx.Done():
				return
			case <-rollup.C:
				j.runRollup(ctx)
			case <-hourly.C:
				j.runMaintenance(ctx)
			}
		}
	}()
}

func (j *jobRunner) stop() {
	if j.cancel != nil {
		j.cancel()
		<-j.done
	}
}

// withLeadership runs fn only if this process holds the advisory lock.
//
// pg_try_advisory_lock never blocks, so a follower skips the work instead of
// queueing behind the leader.
func (j *jobRunner) withLeadership(ctx context.Context, name string, fn func(context.Context) error) {
	conn, err := j.pool.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", advisoryLockKey).Scan(&acquired); err != nil {
		j.log.Debug("could not attempt job lock", slog.String("job", name), slog.Any("error", err))
		return
	}
	if !acquired {
		return
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", advisoryLockKey)
	}()

	if err := fn(ctx); err != nil {
		j.log.Error("job failed", slog.String("job", name), slog.Any("error", err))
	}
}

func (j *jobRunner) runRollup(ctx context.Context) {
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	j.withLeadership(runCtx, "rollup", func(ctx context.Context) error {
		return j.roller.RunRecent(ctx, time.Now())
	})
}

func (j *jobRunner) runMaintenance(ctx context.Context) {
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	j.withLeadership(runCtx, "partitions", func(ctx context.Context) error {
		n, err := store.EnsurePartitions(ctx, j.pool, store.PartitionLookahead)
		if err == nil && n > 0 {
			j.log.Info("partitions created", slog.Int("count", n))
		}
		return err
	})

	// Purging expired salts is the de-identification step, not housekeeping:
	// once a salt is gone, that day's visitor hashes cannot be linked back to
	// an address by anyone, including us.
	j.withLeadership(runCtx, "salt-purge", func(ctx context.Context) error {
		n, err := j.salts.Purge(ctx)
		if err == nil && n > 0 {
			j.log.Info("expired analytics salts purged", slog.Int64("count", n))
		}
		return err
	})

	j.withLeadership(runCtx, "partition-check", func(ctx context.Context) error {
		counts, err := store.DefaultPartitionCounts(ctx, j.pool)
		if err != nil {
			return err
		}
		for table, n := range counts {
			if n > 0 {
				j.log.Warn("rows in default partition; new partitions may fail to attach",
					slog.String("table", table), slog.Int64("rows", n))
			}
		}
		return nil
	})
}
