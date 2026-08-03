package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/automation"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/mail"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/signup"
	"github.com/DevOfPie/LinkCtrl/internal/store"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/webhook"
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
	// links re-verifies custom domains (M40). Nil skips the pass entirely,
	// which is what a runner built without the link service gets.
	links *link.Service
	// domainVerifyInterval is the re-verification cadence, and
	// domainVerifyBatch caps one pass. Zero interval disables the job, leaving
	// verification on-demand only.
	domainVerifyInterval time.Duration
	domainVerifyBatch    int32
	// webhooks drains the outbound delivery queue (M42). Nil skips the job
	// entirely, which is what a runner built without one gets.
	webhooks *webhook.Service
	// automation evaluates the workspaces' standing instructions (M43). Nil
	// skips the job entirely, which is what a runner built without one gets —
	// and which is what makes "evaluation runs here and nowhere else" a property
	// of the wiring rather than a promise.
	automation *automation.Service
	cancel     context.CancelFunc
	done       chan struct{}
}

// advisoryLockKey is a hand-picked constant — the ASCII bytes "lcjobs" plus a
// version suffix — NOT a hash of anything. To inspect or hold the leader lock
// from psql, use the literal value:
//
//	SELECT pg_try_advisory_lock(7810203205416189953);
const advisoryLockKey int64 = 0x6c63_6a6f_6273_0001

func newJobRunner(pool *pgxpool.Pool, salts *analytics.SaltCache, roller *analytics.Roller,
	log *slog.Logger, metrics *observability.Metrics, notifier *notify.Service,
	mailer *mail.Service, signups *signup.Service, links *link.Service,
	webhooks *webhook.Service, automations *automation.Service, domains config.DomainsConfig,
	analyticsRetentionDays, auditRetentionDays int, auditSizeWarnBytes int64,
) *jobRunner {
	batch := domains.VerifyBatch
	if batch <= 0 || batch > math.MaxInt32 {
		batch = 500
	}
	return &jobRunner{
		pool: pool, salts: salts, roller: roller, log: log, metrics: metrics,
		retention:              store.NewRetentionPolicy(analyticsRetentionDays, auditRetentionDays),
		analyticsRetentionDays: analyticsRetentionDays,
		auditRetentionDays:     auditRetentionDays,
		notifier:               notifier,
		auditSizeWarnBytes:     auditSizeWarnBytes,
		mailer:                 mailer,
		signup:                 signups,
		links:                  links,
		//nolint:gosec // G115: range-checked above.
		domainVerifyInterval: domains.VerifyInterval,
		domainVerifyBatch:    int32(batch),
		webhooks:             webhooks,
		automation:           automations,
		done:                 make(chan struct{}),
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
		// Automation evaluation, on its own clock (M43). One minute, and its own
		// ticker rather than the rollup's because the two are unrelated jobs
		// that happen to share a period: a change to how often the breakdowns
		// recompute must not silently change how often somebody's standing
		// instruction runs.
		automations := time.NewTicker(domain.AutomationInterval)
		// The dimension breakdowns, on their own clock (M37). Sixty seconds is
		// the right cadence for numbers whose upsert count is bounded by the
		// links that were clicked; it is the wrong one for numbers whose upsert
		// count is bounded by the distinct (link, day, dimension, value) tuples
		// those clicks imply, which at the SLO dataset is 553k rows and 16-21
		// seconds of every minute. Same work, a clock it fits inside.
		dimension := time.NewTicker(analytics.DimensionInterval)
		hourly := time.NewTicker(time.Hour)
		// Mail is on its own, faster clock. Nothing here is time-critical
		// except this: an invitation is something a person is waiting for with
		// a browser open, and an hour of latency would make the outbox feel
		// like a fault rather than a queue. Thirty seconds costs one indexed
		// query that usually returns nothing.
		outbox := time.NewTicker(30 * time.Second)
		// Custom-domain re-verification (M40, decision D70). Its own clock
		// rather than the hourly maintenance tick, because the cadence is
		// operator configuration: the pair of numbers an operator tunes —
		// how often it checks, and how long a failing hostname keeps serving —
		// is meaningless if one of them is a constant here.
		//
		// A ticker that is never read when the interval is zero, which is the
		// documented way to switch the pass off and leave verification
		// on-demand only.
		domainInterval := j.domainVerifyInterval
		if domainInterval <= 0 {
			domainInterval = time.Hour
		}
		domains := time.NewTicker(domainInterval)
		defer rollup.Stop()
		defer dimension.Stop()
		defer hourly.Stop()
		defer outbox.Stop()
		defer domains.Stop()
		defer automations.Stop()

		// Run once at startup rather than waiting a full interval, so a
		// freshly started instance has current numbers. Both halves, because
		// waiting fifteen minutes for a breakdown on a box that has just come
		// up would look exactly like the breakdown being broken.
		j.runRollup(ctx)
		j.runDimensionRollup(ctx)
		j.runMaintenance(ctx)
		// And so mail queued before a restart goes out at once rather than
		// half a minute later. Surviving the restart is the reason the outbox
		// exists; waiting after it would be a strange way to honour that.
		j.runMail(ctx)
		// And so anything queued before a restart goes out at once rather than
		// half a minute later, for the reason the outbox does: surviving the
		// restart is the point of the queue, and waiting after it would be a
		// strange way to honour that.
		j.runWebhooks(ctx)
		// And once at startup, so a hostname whose record was published while
		// this instance was down starts serving on boot rather than an hour
		// later — and so one whose record went away is re-checked promptly
		// rather than inheriting a stale verification from before the restart.
		j.runDomainVerification(ctx)
		// And once at startup, so a rule whose subject appeared while this
		// instance was down fires on boot rather than a minute later. Safe to
		// run eagerly for the reason every other job here is: the watermark
		// means a restart cannot make a rule fire twice for one subject.
		j.runAutomation(ctx)

		for {
			select {
			case <-ctx.Done():
				return
			case <-rollup.C:
				j.runRollup(ctx)
			case <-dimension.C:
				j.runDimensionRollup(ctx)
			case <-outbox.C:
				j.runMail(ctx)
				// On the mail clock rather than one of its own. The two are the
				// same job in every respect that matters — a bounded batch of
				// network round trips to somebody else's server, drained under
				// leadership, retried with backoff — and an event nobody
				// receives for an hour is an event that reads as lost. A second
				// ticker at the same period would be two timers to reason about
				// for no difference anybody can observe.
				j.runWebhooks(ctx)
			case <-hourly.C:
				j.runMaintenance(ctx)
			case <-domains.C:
				j.runDomainVerification(ctx)
			case <-automations.C:
				j.runAutomation(ctx)
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
		return j.roller.RunRecentTotals(ctx, time.Now())
	})

	// Staleness is reported on the fast tick and outside leadership, because it
	// is an observation of shared state rather than work: a follower that never
	// set it would report nothing, and whether the alert fired would depend on
	// which replica Prometheus reached. It is also the one job metric that has
	// to keep being published while the *dimension* job is the thing that is
	// broken, which is exactly when the leader is busy.
	j.reportJobStaleness(runCtx)
}

// runDimensionRollup recomputes the per-dimension and per-destination
// breakdowns (M37).
//
// A longer timeout than the totals, and not because it is expected to need one.
// The pass it runs is measured at 16-21 seconds on the SLO dataset and it is
// allowed to grow well past that before the cadence has to change again; a
// two-minute bound would have turned the first slow day into a failed job and a
// stale breakdown rather than into a slow one.
func (j *jobRunner) runDimensionRollup(ctx context.Context) {
	runCtx, cancel := context.WithTimeout(ctx, analytics.DimensionInterval)
	defer cancel()
	j.withLeadership(runCtx, "dimension-rollup", func(ctx context.Context) error {
		return j.roller.RunRecentDimensions(ctx, time.Now())
	})
}

// reportJobStaleness publishes how long ago each job last succeeded.
//
// Read from job_state rather than remembered in the process. The existing
// linkctrl_job_last_success_timestamp_seconds is set by whichever replica did
// the work and is cleared by a restart, so it cannot answer "are the breakdowns
// stale?" on a deployment that has more than one replica or that was deployed
// this week — and M37 makes that question one an operator has to be able to ask,
// because the breakdowns are now allowed to be quarter of an hour behind.
func (j *jobRunner) reportJobStaleness(ctx context.Context) {
	stale, err := j.roller.Staleness(ctx)
	if err != nil {
		j.log.Debug("could not read job staleness", slog.Any("error", err))
		return
	}
	for _, s := range stale {
		j.metrics.SetJobStaleness(s.Job, s.Seconds)
	}
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

// runWebhooks drains the outbound delivery queue (M42).
//
// Under leadership *and* claimed with FOR UPDATE SKIP LOCKED, which is not
// belt-and-braces — it is the decision this milestone had to make and record.
// Leadership is an advisory lock released the moment its holder dies, so two
// replicas can briefly believe they are the leader; skip-locked makes that
// moment cost nothing instead of delivering somebody's event twice. Leadership
// on its own would let a crash produce a duplicate, and skip-locked on its own
// would have every replica dialling every receiver.
//
// The timeout is generous because the batch is a batch of network round trips to
// somebody else's server, and bounded for the same reason: a receiver that
// accepts a connection and then says nothing must not hold the scheduler. Each
// attempt carries its own shorter deadline inside this one — WEBHOOK_TIMEOUT,
// ten seconds by default, against a batch of twenty.
func (j *jobRunner) runWebhooks(ctx context.Context) {
	if j.webhooks == nil {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	j.withLeadership(runCtx, "webhooks", func(ctx context.Context) error {
		return j.webhooks.Drain(ctx)
	})
}

// runDomainVerification re-checks every registered hostname's DNS challenge.
//
// Under leadership, because it writes: three replicas each unverifying the same
// domain would write three audit records and send three notifications for one
// event. The *reading* half — which hostnames are served — is not leader-elected
// at all; every replica holds it in memory and learns of a change through M23's
// pub/sub, which is what stops a follower serving a domain the leader has just
// unverified.
//
// The timeout is generous because the pass is a batch of DNS queries to other
// people's nameservers, and bounded for the same reason: a nameserver that
// accepts a query and never answers must not hold the scheduler. Each lookup
// carries its own shorter deadline inside this one.
func (j *jobRunner) runDomainVerification(ctx context.Context) {
	if j.links == nil || j.domainVerifyInterval <= 0 {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	j.withLeadership(runCtx, "domain-verification", func(ctx context.Context) error {
		sum, err := j.links.ReverifyDomains(ctx, time.Now(), j.domainVerifyBatch)
		// Logged whenever anything changed, at Info, because both halves are
		// events an operator has to be able to find afterwards: a hostname
		// starting to serve links, and one that stopped because its DNS had been
		// failing for the whole grace window.
		if sum.Verified > 0 || sum.Unverified > 0 || sum.Failing > 0 {
			j.log.Info("custom domain verification",
				slog.Int("checked", sum.Checked),
				slog.Int("newly_verified", sum.Verified),
				slog.Int("failing", sum.Failing),
				slog.Int("stopped_serving", sum.Unverified))
		}
		return err
	})
}

// runAutomation evaluates the workspaces' standing instructions (M43).
//
// **This is the only caller of automation.Evaluate anywhere in the product**,
// and that is the first of the two claims m43.md turns on: evaluation runs on
// the leader-elected scheduler, never on the request path. There is no endpoint
// that runs it, no link write that triggers it, and internal/httpx does not
// import the package's Service at all.
//
// Under leadership, because it writes: three replicas each firing the same rule
// would archive the same links three times, put three copies of one notification
// in an inbox, and queue three identical webhook deliveries. The watermark makes
// that cost nothing even so — the compare-and-set means the second replica loses
// rather than duplicates — but leadership is what stops the race being run at
// all, and the pair is the same belt-and-braces D77 chose for webhook delivery
// and for the same reason: an advisory lock is released the moment its holder
// dies.
//
// The timeout is domain.AutomationTimeout, twice the interval, so a slow run overlaps
// at most one tick and a stuck one is cut off rather than holding the scheduler.
// What bounds the run itself is not the timeout: it is the four constants in
// internal/domain that the package's doc comment multiplies out.
func (j *jobRunner) runAutomation(ctx context.Context) {
	if j.automation == nil {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, domain.AutomationTimeout)
	defer cancel()

	j.withLeadership(runCtx, "automation", func(ctx context.Context) error {
		return j.automation.Evaluate(ctx)
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

	// Rotated predecessors whose grace window has closed (M44).
	//
	// This is bookkeeping, not enforcement, and the distinction is worth holding
	// on to: authentication already refuses a key past its grace window by
	// reading the column, so a lapsed predecessor stops working whether or not
	// this job has ever run. What the sweep buys is a key list that agrees with
	// that behaviour — without it the owner sees a key reading "active" that
	// authenticates nothing, which is the sort of disagreement somebody
	// debugging an outage will believe over the truth.
	//
	// Info rather than Debug: a key becoming permanently unusable is a thing an
	// operator may need to find afterwards, and the count here is its only log.
	if n, err := q.RevokeLapsedAPIKeyGraces(ctx); err != nil {
		errs = append(errs, fmt.Errorf("revoke lapsed api key graces: %w", err))
	} else if n > 0 {
		j.log.Info("rotated api keys revoked at the end of their grace window",
			slog.Int64("count", n))
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

	// Delivered and abandoned webhook deliveries past WEBHOOK_RETENTION_DAYS.
	// The same shape and the same reason as the outbox above, and a sharper
	// need: this table grows by one row per link write per enabled webhook,
	// which is the fastest-growing thing in the schema that is not analytics.
	if j.webhooks != nil {
		if n, err := j.webhooks.PurgeFinished(ctx); err != nil {
			errs = append(errs, err)
		} else if n > 0 {
			j.log.Debug("finished webhook deliveries purged", slog.Int64("count", n))
		}
	}

	return errors.Join(errs...)
}
