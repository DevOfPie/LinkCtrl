package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/store"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// jobRunner runs the periodic maintenance work.
//
// Leader election is a Postgres advisory lock rather than a scheduler service:
// it needs no extra infrastructure, and the lock is released automatically if
// the holder dies, so a crashed replica does not block the others. Every job
// re-checks the lock, so leadership can move between runs without coordination.
type jobRunner struct {
	pool    *pgxpool.Pool
	salts   *analytics.SaltCache
	roller  *analytics.Roller
	log     *slog.Logger
	metrics *observability.Metrics
	// retention is the per-table window. Analytics and audit have separate
	// policies and different defaults; zero for a table keeps it forever.
	retention store.RetentionPolicy
	// auditRetentionDays is kept alongside the policy for the log line that
	// reports a drop, which is the only record that history was deleted.
	auditRetentionDays     int
	analyticsRetentionDays int
	cancel                 context.CancelFunc
	done                   chan struct{}
}

// advisoryLockKey is a hand-picked constant — the ASCII bytes "lcjobs" plus a
// version suffix — NOT a hash of anything. To inspect or hold the leader lock
// from psql, use the literal value:
//
//	SELECT pg_try_advisory_lock(7810203205416189953);
const advisoryLockKey int64 = 0x6c63_6a6f_6273_0001

func newJobRunner(pool *pgxpool.Pool, salts *analytics.SaltCache, roller *analytics.Roller,
	log *slog.Logger, metrics *observability.Metrics, analyticsRetentionDays, auditRetentionDays int,
) *jobRunner {
	return &jobRunner{
		pool: pool, salts: salts, roller: roller, log: log, metrics: metrics,
		retention:              store.NewRetentionPolicy(analyticsRetentionDays, auditRetentionDays),
		analyticsRetentionDays: analyticsRetentionDays,
		auditRetentionDays:     auditRetentionDays,
		done:                   make(chan struct{}),
	}
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
		// Counted, not ignored: on a healthy multi-replica deployment most runs
		// are skips, and a follower reporting no skips at all is a follower
		// whose scheduler has stopped.
		j.metrics.ObserveJobSkipped(name)
		return
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", advisoryLockKey)
	}()

	err = fn(ctx)
	j.metrics.ObserveJob(name, err)
	if err != nil {
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

	// Retention. Dropping a whole month is instant and reclaims the space, which
	// is the reason these tables are partitioned by month in the first place —
	// and it runs after partition creation so a run can never drop the partition
	// the same run just made.
	//
	// Analytics and audit are dropped by the same pass under different windows.
	// The audit window defaults to zero, so on a default instance this drops no
	// audit partition at all and linkctrl_audit_log_bytes is what tells the
	// operator what that is costing.
	j.withLeadership(runCtx, "retention", func(ctx context.Context) error {
		dropped, err := store.DropExpiredPartitions(ctx, j.pool, j.retention, time.Now())
		for _, name := range dropped {
			// Info, not Debug: this is irreversible deletion, and the log is the
			// only record that it happened. Named per table, because "an audit
			// partition was dropped" is a different sentence to answer for than
			// "a click partition was dropped".
			days := j.analyticsRetentionDays
			kind := "analytics"
			if strings.HasPrefix(name, store.AuditTable+"_") {
				days, kind = j.auditRetentionDays, "audit"
			}
			j.log.Info("dropped expired partition",
				slog.String("partition", name),
				slog.String("kind", kind),
				slog.Int("retention_days", days))
		}
		return err
	})

	// The audit log's size, measured on every replica rather than only the
	// leader: a gauge the followers never set reads as zero, and an operator's
	// alert would then depend on which replica answered the scrape. It is a
	// catalogue read, not a scan, so paying for it N times is cheap.
	//
	// Outside withLeadership for the same reason. This is a measurement, not
	// work that must happen once.
	if bytes, err := store.PartitionedTableBytes(runCtx, j.pool, store.AuditTable); err != nil {
		j.log.Debug("could not measure the audit log", slog.Any("error", err))
	} else {
		j.metrics.SetAuditLogBytes(bytes)
	}

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

	// The reapers: the end of the link trash window, expired sessions, and
	// long-revoked API keys. One job because they share a shape — rows whose
	// deadline passed — and none is time-critical: a missed run means rows
	// linger an hour longer, nothing more.
	//
	// The link purge is the one with teeth. It is what makes the 30-day
	// recovery window a window rather than forever, and it is where a
	// trafficked alias enters reserved_aliases — the same statement that
	// deletes the row, so a crash cannot separate the two.
	j.withLeadership(runCtx, "housekeeping", func(ctx context.Context) error {
		return j.housekeeping(ctx)
	})
}

// purgeBatch bounds one purge statement. Purging is hourly housekeeping, not a
// backlog race: a burst of ten thousand deletions drains over a few runs
// rather than holding row locks for one long one.
const purgeBatch = 1000

func (j *jobRunner) housekeeping(ctx context.Context) error {
	q := dbgen.New(j.pool)
	var errs []error

	purged, err := q.PurgeExpiredLinks(ctx, purgeBatch)
	if err != nil {
		errs = append(errs, fmt.Errorf("purge links: %w", err))
	}
	reserved := 0
	for _, p := range purged {
		if p.Reserved {
			reserved++
		}
		// One line per link, by alias. The comment here used to say "the log is
		// the only record of which aliases went" while logging nothing but a
		// count, so the record it described did not exist — an operator
		// answering "which link disappeared?" had a number and no names. The
		// batch is bounded at purgeBatch and the job runs hourly, so the volume
		// is bounded too.
		j.log.Info("purged link past its recovery window",
			slog.String("alias", p.Alias),
			slog.Bool("alias_reserved", p.Reserved))
	}
	if len(purged) > 0 {
		// Info, not Debug: this is irreversible deletion of user data, and
		// these lines plus the ones above are its only record.
		j.log.Info("purged links past their recovery window",
			slog.Int("links", len(purged)),
			slog.Int("aliases_reserved", reserved))
	}

	if n, err := q.DeleteExpiredSessions(ctx); err != nil {
		errs = append(errs, fmt.Errorf("delete expired sessions: %w", err))
	} else if n > 0 {
		j.log.Debug("expired sessions deleted", slog.Int64("count", n))
	}

	if n, err := q.DeleteRevokedAPIKeys(ctx); err != nil {
		errs = append(errs, fmt.Errorf("delete revoked api keys: %w", err))
	} else if n > 0 {
		j.log.Debug("long-revoked api keys deleted", slog.Int64("count", n))
	}

	return errors.Join(errs...)
}
