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
	"github.com/DevOfPie/LinkCtrl/internal/mail"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/signup"
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
	// notifier raises the audit-growth warning. Nil disables it entirely.
	notifier *notify.Service
	// auditSizeWarnBytes is the threshold. Zero means never warn.
	auditSizeWarnBytes int64
	// mailer drains the outbox. Nil when no SMTP relay is configured, and then
	// the job does not run at all — there is nothing to drain, because with no
	// mailer nothing is ever enqueued.
	mailer *mail.Service
	// signup sweeps registrations nobody completed. Never nil in the process —
	// the service is always built — but held as a pointer so a test runner
	// without one skips the sweep rather than panicking.
	signup *signup.Service
	cancel context.CancelFunc
	done   chan struct{}
}

// advisoryLockKey is a hand-picked constant — the ASCII bytes "lcjobs" plus a
// version suffix — NOT a hash of anything. To inspect or hold the leader lock
// from psql, use the literal value:
//
//	SELECT pg_try_advisory_lock(7810203205416189953);
const advisoryLockKey int64 = 0x6c63_6a6f_6273_0001

func newJobRunner(pool *pgxpool.Pool, salts *analytics.SaltCache, roller *analytics.Roller,
	log *slog.Logger, metrics *observability.Metrics, notifier *notify.Service,
	mailer *mail.Service, signups *signup.Service,
	analyticsRetentionDays, auditRetentionDays int, auditSizeWarnBytes int64,
) *jobRunner {
	return &jobRunner{
		pool: pool, salts: salts, roller: roller, log: log, metrics: metrics,
		retention:              store.NewRetentionPolicy(analyticsRetentionDays, auditRetentionDays),
		analyticsRetentionDays: analyticsRetentionDays,
		auditRetentionDays:     auditRetentionDays,
		notifier:               notifier,
		auditSizeWarnBytes:     auditSizeWarnBytes,
		mailer:                 mailer,
		signup:                 signups,
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
		// Mail is on its own, faster clock. Nothing here is time-critical
		// except this: an invitation is something a person is waiting for with
		// a browser open, and an hour of latency would make the outbox feel
		// like a fault rather than a queue. Thirty seconds costs one indexed
		// query that usually returns nothing.
		outbox := time.NewTicker(30 * time.Second)
		defer rollup.Stop()
		defer hourly.Stop()
		defer outbox.Stop()

		// Run once at startup rather than waiting a full interval, so a
		// freshly started instance has current numbers.
		j.runRollup(ctx)
		j.runMaintenance(ctx)
		// And so mail queued before a restart goes out at once rather than
		// half a minute later. Surviving the restart is the reason the outbox
		// exists; waiting after it would be a strange way to honour that.
		j.runMail(ctx)

		for {
			select {
			case <-ctx.Done():
				return
			case <-rollup.C:
				j.runRollup(ctx)
			case <-outbox.C:
				j.runMail(ctx)
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

// runMail drains the outbox.
//
// Under leadership, unlike the size measurement above, because sending is work
// rather than observation: three replicas draining the same table would each
// try to deliver the same message, and skip-locked would only narrow the window
// rather than close it.
//
// The timeout is generous because the batch is a batch of network round trips
// to somebody else's server. It is still bounded, so a relay that accepts
// connections and then says nothing cannot hold the scheduler forever — and the
// sender sets its own per-message deadline inside this one.
func (j *jobRunner) runMail(ctx context.Context) {
	if j.mailer == nil {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	j.withLeadership(runCtx, "mail", func(ctx context.Context) error {
		return j.mailer.Drain(ctx)
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
	auditBytes, err := store.PartitionedTableBytes(runCtx, j.pool, store.AuditTable)
	if err != nil {
		j.log.Debug("could not measure the audit log", slog.Any("error", err))
	} else {
		j.metrics.SetAuditLogBytes(auditBytes)
	}

	// Warning the owner about that size is work, not measurement, so it does
	// run under leadership: three replicas each writing the same notification
	// would put three copies in one inbox.
	if err == nil && j.notifier != nil {
		j.withLeadership(runCtx, "audit-growth-warning", func(ctx context.Context) error {
			return j.notifier.WarnAuditGrowth(ctx, auditBytes, j.auditSizeWarnBytes)
		})
	}

	// Registrations nobody completed, and spent rows past their short window.
	// Under leadership because it is a delete, and hourly because nothing here
	// is urgent — a lapsed row does nothing until it is swept, it simply must
	// not accumulate forever.
	if j.signup != nil {
		j.withLeadership(runCtx, "signup-purge", func(ctx context.Context) error {
			n, err := j.signup.PurgeLapsed(ctx)
			if err == nil && n > 0 {
				j.log.Info("lapsed sign-up registrations purged", slog.Int64("count", n))
			}
			return err
		})
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

	// Delivered and abandoned mail past its window. Same shape as the three
	// above — rows whose deadline passed — and here for the same reason: the
	// outbox is a record of what was attempted, not an archive, and a table
	// nothing ever deletes from is the growth problem D5 spent a metric and a
	// notification learning about.
	if j.mailer != nil {
		if n, err := j.mailer.PurgeFinished(ctx); err != nil {
			errs = append(errs, err)
		} else if n > 0 {
			j.log.Debug("finished mail purged", slog.Int64("count", n))
		}
	}

	return errors.Join(errs...)
}
