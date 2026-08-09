package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/account"
	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/automation"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/mail"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/recovery"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/signup"
	"github.com/DevOfPie/LinkCtrl/internal/store"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/update"
	"github.com/DevOfPie/LinkCtrl/internal/webhook"
)

// jobRunner runs the periodic maintenance work, one goroutine per job family.
//
// Leader election is a Postgres advisory lock rather than a scheduler service:
// it needs no extra infrastructure, and the lock is released automatically if
// the holder dies, so a crashed replica does not block the others. Every job
// re-checks the lock, so leadership can move between runs without coordination.
// The lock is per *family*, not global — see the key block below for why the
// distinction is load-bearing — so leadership over the mail drain and
// leadership over the dimension rollup are separate facts that can land on
// separate replicas.
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
	// the drain does not run at all.
	//
	// Nil does not mean the table is empty, which this comment used to assume:
	// an instance that *had* a relay and had it cleared keeps everything queued
	// before the change. The purge pass handles that case explicitly rather
	// than inheriting the assumption (F52).
	mailer *mail.Service
	// signup sweeps registrations nobody completed. Never nil in the process —
	// the service is always built — but held as a pointer so a test runner
	// without one skips the sweep rather than panicking.
	signup *signup.Service
	// recovery sweeps password-reset tokens (M51), on the same terms as signup
	// above and for the same reason: a waiting-room table with no sweep is the
	// one shape that grows forever with nothing watching it.
	recovery *recovery.Service
	// accounts erases the residue of deleted accounts (M52), on the same terms
	// as recovery above: it is a sweep rather than a job family, because a new
	// advisory key and a new goroutine for a pass that finds nothing on almost
	// every run is cost without a reason.
	accounts *account.Service
	// mfa sweeps the pending logins that sit between a right password and a
	// session (M53) — lapsed ones and spent ones. Third instance of the same
	// waiting-room shape, held on the same terms.
	mfa *auth.MFAService
	// links re-verifies custom domains (M40). Nil skips the pass entirely,
	// which is what a runner built without the link service gets.
	links *link.Service
	// domainVerifyInterval is the re-verification cadence, and
	// domainVerifyBatch caps one pass. Zero interval disables the job, leaving
	// verification on-demand only.
	domainVerifyInterval time.Duration
	domainVerifyBatch    int32
	// hosts is this replica's verified-hostname set, reloaded on a timer.
	//
	// **Not under leadership, unlike the verification pass that shares its
	// family.** The set is in-process and every replica holds its own, so a
	// reload the leader performs does nothing for the other three. Nil skips
	// the job, which is what a runner built without a host cache gets (F73).
	hosts *redirect.HostCache
	// webhooks drains the outbound delivery queue (M42). Nil skips the job
	// entirely, which is what a runner built without one gets.
	webhooks *webhook.Service
	// automation evaluates the workspaces' standing instructions (M43). Nil
	// skips the job entirely, which is what a runner built without one gets —
	// and which is what makes "evaluation runs here and nowhere else" a property
	// of the wiring rather than a promise.
	automation *automation.Service
	// updates asks whether a newer LinkCtrl has been published (M55).
	//
	// **Nil is LINKCTRL_UPDATE_CHECK=false**, and it is where that variable takes
	// effect: main does not build the service at all, so the pass has nothing to
	// call and this process opens no socket outwards on any schedule. The
	// operator's *other* half of the switch — the answer they gave at first run —
	// is inside the service, in the statement that claims the day, because that
	// is a row somebody may change after boot.
	updates *update.Service
	cancel  context.CancelFunc
	// wg accounts for every family goroutine start launches; stop waits on it,
	// so shutdown never leaves a pass mid-flight against a pool that is about
	// to close. The single scheduler goroutine had a done channel doing this
	// job for one; the WaitGroup is the same discipline at N.
	wg sync.WaitGroup
}

// The advisory lock keys, one per job family.
//
// Derivation, kept legible on purpose: the high six bytes are the ASCII string
// "lcjobs" (0x6c63_6a6f_6273) — the prefix the original single key used — the
// seventh byte is a generation, and the eighth is the family. Hand-picked
// constants, NOT hashes of anything, so an operator can inspect or hold any
// family's lock from psql with the literal beside it.
//
// Keys must never collide, and a retired key must never be reused for a new
// family. A collision does not serialize two families — it makes them *skip*
// each other: pg_try_advisory_lock never blocks, so with each family on its
// own goroutine, the same-key loser on the same leader drops its whole tick,
// and the drop is counted as a follower's skip, which both loses work and
// poisons the follower-liveness reading ObserveJobSkipped exists for, on a
// replica whose scheduler is perfectly healthy. Reuse has the same effect
// across binaries instead of within one: an old binary still holds its keys
// for as long as a rolling deploy keeps it alive.
//
// Deploy overlap, stated as a cost rather than discovered: the generation-0
// binary holds advisoryLockKeyRetiredV1 for everything, and none of the keys
// below contend with it, so for the length of a rolling deploy each family can
// have two leaders — one old, one new. The window costs duplicate effort, not
// duplicate effects: the drains claim rows with FOR UPDATE SKIP LOCKED, the
// rollups recompute whole days idempotently, partition creation is
// IF-NOT-EXISTS-shaped, and the automation watermark is compare-and-set — all
// of which already had to hold, because an advisory lock is released the
// moment its holder dies and two leaders was always a window, not an
// impossibility.
const (
	// advisoryLockKeyRetiredV1 is generation 0: one key serializing every job
	// on one goroutine. Nothing takes it any more. It stays declared so no
	// future family can be given this value without tripping the key test —
	// during a rolling deploy the old binary still holds it.
	advisoryLockKeyRetiredV1 int64 = 0x6c63_6a6f_6273_0001 // 7810203205416189953

	advisoryLockKeyRollup      int64 = 0x6c63_6a6f_6273_0101 // 7810203205416190209
	advisoryLockKeyDimension   int64 = 0x6c63_6a6f_6273_0102 // 7810203205416190210
	advisoryLockKeyMail        int64 = 0x6c63_6a6f_6273_0103 // 7810203205416190211
	advisoryLockKeyWebhooks    int64 = 0x6c63_6a6f_6273_0104 // 7810203205416190212
	advisoryLockKeyMaintenance int64 = 0x6c63_6a6f_6273_0105 // 7810203205416190213
	advisoryLockKeyDomains     int64 = 0x6c63_6a6f_6273_0106 // 7810203205416190214
	advisoryLockKeyAutomation  int64 = 0x6c63_6a6f_6273_0107 // 7810203205416190215

	// The update check (M55). Its own key because it is its own family, and it
	// is its own family because it is the only scheduled work in this product
	// that opens a socket to a host outside the deployment — putting the one
	// outbound job inside a family called *maintenance* is how egress stops
	// being auditable in one place.
	advisoryLockKeyUpdates int64 = 0x6c63_6a6f_6273_0108 // 7810203205416190216
)

func newJobRunner(pool *pgxpool.Pool, salts *analytics.SaltCache, roller *analytics.Roller,
	log *slog.Logger, metrics *observability.Metrics, notifier *notify.Service,
	mailer *mail.Service, signups *signup.Service, resets *recovery.Service,
	accounts *account.Service, mfa *auth.MFAService,
	links *link.Service,
	webhooks *webhook.Service, automations *automation.Service, hosts *redirect.HostCache,
	updates *update.Service,
	domains config.DomainsConfig,
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
		recovery:               resets,
		accounts:               accounts,
		mfa:                    mfa,
		links:                  links,
		//nolint:gosec // G115: range-checked above.
		domainVerifyInterval: domains.VerifyInterval,
		domainVerifyBatch:    int32(batch),
		webhooks:             webhooks,
		automation:           automations,
		hosts:                hosts,
		updates:              updates,
	}
}

// jobFamily is one scheduler goroutine: a ticker, the pass it runs, and the
// advisory lock key that pass's leader-elected work takes.
//
// The key rides here as well as inside the pass's own withLeadership calls so
// that families() is a complete, testable statement of the scheduler's shape —
// which families exist, on what clock, under which lock — and so the
// key-uniqueness test has one table to hold to account.
type jobFamily struct {
	name  string
	key   int64
	every time.Duration
	// onStart runs once, before the first tick, so a freshly started instance
	// has current numbers rather than waiting out a full interval. It is
	// allowed to differ from onTick: the domains family verifies at boot but
	// does not reload the host set, because main has already loaded it before
	// this runner starts.
	onStart func(context.Context)
	// onTick runs on every tick. Within one family everything is strictly
	// sequential — a slow pass delays its own family's next tick and nothing
	// else's, and the orderings the passes rely on (partitions before
	// retention, verification before reload) hold because they share this one
	// goroutine.
	onTick func(context.Context)
}

// families is the scheduler's whole shape, in one place.
//
// One goroutine per family, rather than the single select every job used to
// share, because inline was a latency contract nobody could keep: the
// dimension rollup is deliberately allowed up to its fifteen-minute timeout,
// and on a shared goroutine those minutes were charged to every other job —
// the thirty-second outbox included — while a Go ticker holds only one tick,
// so the runs queued behind a long pass were dropped, not delayed. Grouping is
// by data dependency, not by period: jobs that read what another job just
// wrote share a family and keep their order; jobs that merely share a clock do
// not, and now cannot stall each other.
func (j *jobRunner) families() []jobFamily {
	// Custom-domain re-verification (M40, decision D70) runs on operator
	// configuration, because the pair of numbers an operator tunes — how often
	// it checks, and how long a failing hostname keeps serving — is
	// meaningless if one of them is a constant here. Zero disables both the
	// pass and the reload (their own guards check it); the ticker still needs
	// a positive period to exist.
	domainInterval := j.domainVerifyInterval
	if domainInterval <= 0 {
		domainInterval = time.Hour
	}

	return []jobFamily{
		{
			// The totals, with the staleness report riding after them inside
			// runRollup — coupled by design: the report reads job_state right
			// after the rollup writes it, and it must keep publishing on this
			// fast clock while some *other* family is the thing that is stuck.
			// Cadence chosen so a missed run is never load-bearing: rollups
			// recompute whole days from raw events.
			name: "rollup", key: advisoryLockKeyRollup, every: 60 * time.Second,
			onStart: j.runRollup, onTick: j.runRollup,
		},
		{
			// The dimension breakdowns, on their own clock (M37) and now their
			// own goroutine. Sixty seconds is the right cadence for numbers
			// whose upsert count is bounded by the links that were clicked; it
			// is the wrong one for numbers bounded by the distinct (link, day,
			// dimension, value) tuples those clicks imply, which at the SLO
			// dataset is 553k rows and 16-21 seconds of work. Same work, a
			// clock it fits inside — and a goroutine of its own, because those
			// seconds (and anything up to the pass's fifteen-minute bound) are
			// a price only the next *breakdown* may pay, not the outbox.
			name: "dimension-rollup", key: advisoryLockKeyDimension, every: analytics.DimensionInterval,
			// The startup run is deliberate here too: waiting fifteen minutes
			// for a breakdown on a box that has just come up would look
			// exactly like the breakdown being broken.
			onStart: j.runDimensionRollup, onTick: j.runDimensionRollup,
		},
		{
			// Mail is on a fast clock because an invitation is something a
			// person is waiting for with a browser open; an hour of latency
			// would make the outbox feel like a fault rather than a queue.
			// Thirty seconds costs one indexed query that usually returns
			// nothing. The startup run is so mail queued before a restart goes
			// out at once — surviving the restart is the reason the outbox
			// exists, and waiting after it would be a strange way to honour
			// that.
			name: "mail", key: advisoryLockKeyMail, every: 30 * time.Second,
			onStart: j.runMail, onTick: j.runMail,
		},
		{
			// The webhook drain used to ride the mail ticker, coupled for
			// scheduler-latency economics alone: when both ran inline on one
			// select, a second timer at the same period was a second thing to
			// reason about for no difference anybody could observe. On its own
			// goroutine the difference is observable and is the point — a
			// relay that accepts connections and says nothing costs mail its
			// SMTP_TIMEOUT without costing anybody's events a millisecond, and
			// a blackholed receiver cannot hold an invitation (F133's harm
			// shape, ended rather than bounded). Same clock, same startup
			// reasoning as the outbox above.
			name: "webhooks", key: advisoryLockKeyWebhooks, every: 30 * time.Second,
			onStart: j.runWebhooks, onTick: j.runWebhooks,
		},
		{
			// The hourly pass stays one family because its inside is ordered:
			// retention runs after partition creation so a run can never drop
			// the partition the same run just made, and the audit-size
			// measurement feeds the growth warning that follows it — a data
			// dependency, not a habit. Cadence chosen so a missed run is never
			// load-bearing: partitions are maintained two months ahead.
			name: "maintenance", key: advisoryLockKeyMaintenance, every: time.Hour,
			onStart: j.runMaintenance, onTick: j.runMaintenance,
		},
		{
			// Verification and the host reload share a family *in that order*
			// on purpose: verification (leader-only) writes the rows the
			// reload (every replica, no leadership — F73) reads, so on the
			// leader a revocation is served out on the same tick that wrote it
			// rather than racing a sibling timer for it. The startup half is
			// verification alone — so a hostname whose record was published
			// while this instance was down starts serving on boot, and one
			// whose record went away is re-checked promptly — while the host
			// set was already loaded by main before this runner started.
			name: "domains", key: advisoryLockKeyDomains, every: domainInterval,
			onStart: j.runDomainVerification,
			onTick: func(ctx context.Context) {
				j.runDomainVerification(ctx)
				j.runHostReload(ctx)
			},
		},
		{
			// Automation evaluation, on its own clock (M43): a change to how
			// often the breakdowns recompute must not silently change how
			// often somebody's standing instruction runs. Run once at startup
			// so a rule whose subject appeared while this instance was down
			// fires on boot rather than a minute later — safe eagerly, because
			// the watermark means a restart cannot make a rule fire twice for
			// one subject.
			name: "automation", key: advisoryLockKeyAutomation, every: domain.AutomationInterval,
			onStart: j.runAutomation, onTick: j.runAutomation,
		},
		{
			// The update check (M55), and the one family whose work leaves the
			// deployment. It is separate from `maintenance` on the same grounds
			// its advisory key is separate: an operator asking *what does this
			// process connect to, and when* has one family to read, and a later
			// step added to the hourly pass cannot quietly become a second
			// outbound call under a name that does not suggest one.
			//
			// **The ticker is hourly and the period is daily**, which is not a
			// contradiction. The day is bounded by the row ClaimUpdateCheck
			// updates, so a fast ticker cannot produce a second request; what it
			// buys is that an instance redeployed most afternoons still checks,
			// where a 24-hour ticker on a process that rarely lives 24 hours
			// would check on the startup run and never again. One indexed UPDATE
			// an hour that usually matches nothing is the whole cost.
			//
			// The startup run is deliberate for the same reason and is safe for
			// the same one: a box that has been off for a week asks on the way
			// up, and a box restarted twice in ten minutes asks once.
			name: "update-check", key: advisoryLockKeyUpdates, every: time.Hour,
			onStart: j.runUpdateCheck, onTick: j.runUpdateCheck,
		},
	}
}

// start launches one goroutine per job family.
//
// What still serializes does so inside a family, on purpose; across families
// nothing serializes at all, which is what the per-family advisory keys exist
// to make safe — under the old shared key, two families running concurrently
// on the same leader would have skipped each other rather than queued.
func (j *jobRunner) start(parent context.Context) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	j.cancel = cancel

	for _, f := range j.families() {
		j.wg.Add(1)
		go func() {
			defer j.wg.Done()
			tick := time.NewTicker(f.every)
			defer tick.Stop()

			if f.onStart != nil {
				f.onStart(ctx)
			}
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					f.onTick(ctx)
				}
			}
		}()
	}
}

// stop cancels every family goroutine and waits for all of them to return, so
// shutdown never abandons a pass mid-flight against a pool that is about to
// close. The WaitGroup is the accounting: each goroutine start launches is
// added before it exists and released on its way out, the same discipline the
// single scheduler goroutine's done channel provided for one.
func (j *jobRunner) stop() {
	if j.cancel != nil {
		j.cancel()
		j.wg.Wait()
	}
}

// withLeadership runs fn only if this process holds the family's advisory lock.
//
// pg_try_advisory_lock never blocks, so a follower skips the work instead of
// queueing behind the leader. The key must be the calling family's own — the
// constants above — and never shared across families: with each family on its
// own goroutine, two same-key callers on one replica would race each other,
// and the loser is skipped entirely rather than delayed, dropping work the
// replica held leadership for.
//
// fn runs synchronously on this call, and the lock is held until it returns —
// which is a contract, not an implementation detail. Advisory locks are
// session-scoped, so anything fn hands to a goroutine that outlives this call
// runs without the lock; both Drains wait on their own WaitGroups before
// returning for exactly this reason (see internal/mail.Service.Drain).
func (j *jobRunner) withLeadership(ctx context.Context, key int64, name string, fn func(context.Context) error) {
	conn, err := j.pool.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		j.log.Debug("could not attempt job lock", slog.String("job", name), slog.Any("error", err))
		return
	}
	if !acquired {
		// Counted, not ignored: on a healthy multi-replica deployment most runs
		// are skips. The reading is per family now that each family holds its
		// own key — a follower reporting no skips across a family's job names
		// is a follower whose goroutine for that family has stopped, and the
		// old whole-scheduler reading is the same check summed over all seven.
		j.metrics.ObserveJobSkipped(name)
		return
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", key)
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
	j.withLeadership(runCtx, advisoryLockKeyRollup, "rollup", func(ctx context.Context) error {
		return j.roller.RunRecentTotals(ctx, time.Now())
	})

	// Staleness is reported on the fast tick and outside leadership, because it
	// is an observation of shared state rather than work: a follower that never
	// set it would report nothing, and whether the alert fired would depend on
	// which replica Prometheus reached. It is also the one job metric that has
	// to keep being published while the *dimension* job is the thing that is
	// broken — which is why it rides this family and not that one: a stuck
	// dimension pass now cannot delay the report that says it is stuck.
	j.reportJobStaleness(runCtx)
}

// runDimensionRollup recomputes the per-dimension and per-destination
// breakdowns (M37).
//
// A longer timeout than the totals, and not because it is expected to need one.
// The pass it runs is measured at 16-21 seconds on the SLO dataset and it is
// allowed to grow well past that before the cadence has to change again; a
// two-minute bound would have turned the first slow day into a failed job and a
// stale breakdown rather than into a slow one. The whole bound is chargeable
// only to this family: a pass that spends all fifteen minutes delays the next
// breakdown and nothing else, where on the old shared goroutine it held the
// thirty-second outbox, the automation clock and the host reload for the
// duration.
func (j *jobRunner) runDimensionRollup(ctx context.Context) {
	runCtx, cancel := context.WithTimeout(ctx, analytics.DimensionInterval)
	defer cancel()
	j.withLeadership(runCtx, advisoryLockKeyDimension, "dimension-rollup", func(ctx context.Context) error {
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
// to somebody else's server, and bounded for the same reason: a relay that
// accepts connections and then says nothing must not hold this queue forever.
// Each message carries its own shorter deadline inside this one — SMTP_TIMEOUT,
// ten seconds by default — and the batch is handed over together rather than in
// turn, so what this call costs is *one* of those and not twenty (see
// internal/mail.SendConcurrency, and finding F133). A slow drain now delays
// only the outbox's own next tick — the family goroutine is mail's alone — but
// one attempt is still the right cost, because the queue it delays is the one
// an invitation is waiting in.
func (j *jobRunner) runMail(ctx context.Context) {
	if j.mailer == nil {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	j.withLeadership(runCtx, advisoryLockKeyMail, "mail", func(ctx context.Context) error {
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
// accepts a connection and then says nothing must not hold this queue forever.
// Each attempt carries its own shorter deadline inside this one —
// WEBHOOK_TIMEOUT, ten seconds by default — and the batch is dialled together
// rather than in turn, so what this call costs is *one* of those and not twenty
// (see internal/webhook.DeliveryConcurrency). A slow drain now delays only the
// deliveries' own next tick — the family goroutine is this queue's alone — but
// one attempt is still the right cost, because an event nobody receives for
// minutes is an event that reads as lost.
func (j *jobRunner) runWebhooks(ctx context.Context) {
	if j.webhooks == nil {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	j.withLeadership(runCtx, advisoryLockKeyWebhooks, "webhooks", func(ctx context.Context) error {
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
// accepts a query and never answers must not hold this family — the host
// reload runs behind this call on the same goroutine, and an unbounded pass
// would stall the one job that takes revoked hostnames out of service. Each
// lookup carries its own shorter deadline inside this one.
func (j *jobRunner) runDomainVerification(ctx context.Context) {
	if j.links == nil || j.domainVerifyInterval <= 0 {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	j.withLeadership(runCtx, advisoryLockKeyDomains, "domain-verification", func(ctx context.Context) error {
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

// runHostReload re-reads this replica's verified-hostname set.
//
// It is the backstop pub/sub cannot be. Redis pub/sub is at-most-once, so a
// published invalidation that is simply lost while the subscription stays
// healthy is never noticed: `Subscriber.establish` reloads on connect and
// reconnect, and `Run` bounds silence and probes rather than trusting it, but
// all of that catches silence the replica *notices*. A dropped message on a
// healthy connection looks like nothing happening. And on an instance with no
// Redis at all there is no subscriber, so before this the only reload sites
// were boot and the subscriber itself — a second replica never reloaded (F73).
//
// **No leadership**, which is the whole point and the opposite of the
// verification pass that runs just before it on this family's goroutine. The
// set lives in each process; a leader reloading its own copy leaves every
// other replica serving whatever it last knew, which is the staleness this
// closes.
//
// Reload rather than Refresh: this is a timer with nothing behind it, not a
// burst of invalidations to collapse, and doing it inline on the family means
// a slow query delays the family's next tick rather than overlapping with it.
// A failure is logged and dropped — the replica keeps the set it has, which is
// the same direction every other failure in this cache takes, and the next
// tick tries again.
func (j *jobRunner) runHostReload(ctx context.Context) {
	if j.hosts == nil || j.domainVerifyInterval <= 0 {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	if err := j.hosts.Reload(runCtx); err != nil {
		j.log.Warn("could not reload the verified-hostname set", slog.Any("error", err))
	}
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
// The timeout is domain.AutomationTimeout, twice the interval, so a slow run
// overlaps at most one tick and a stuck one is cut off rather than holding its
// own family's clock indefinitely. What bounds the run itself is not the
// timeout: it is the four constants in internal/domain that the package's doc
// comment multiplies out.
func (j *jobRunner) runAutomation(ctx context.Context) {
	if j.automation == nil {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, domain.AutomationTimeout)
	defer cancel()

	j.withLeadership(runCtx, advisoryLockKeyAutomation, "automation", func(ctx context.Context) error {
		return j.automation.Evaluate(ctx)
	})
}

// runUpdateCheck asks whether a newer LinkCtrl has been published (M55).
//
// **Under leadership, so an eight-replica deployment makes one request a day and
// not eight.** That is the milestone's own bullet, and it is belt-and-braces
// with the row: ClaimUpdateCheck would already stop the other seven, since only
// one UPDATE can match a row whose timestamp the first one moved. Leadership is
// what stops the race being run at all, and the pair is the same choice D77 made
// for webhook delivery and for the same reason — an advisory lock is released
// the moment its holder dies, so two leaders is a window rather than an
// impossibility, and here that window would be a second socket opened to a host
// outside the deployment.
//
// Nil service is LINKCTRL_UPDATE_CHECK=false and nothing runs, not even the
// lock acquisition.
//
// The timeout is short because the work is one small GET with its own ten-second
// bound inside this one. Nothing here is retried: see update.Service.Run.
func (j *jobRunner) runUpdateCheck(ctx context.Context) {
	if j.updates == nil {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	j.withLeadership(runCtx, advisoryLockKeyUpdates, "update-check", func(ctx context.Context) error {
		return j.updates.Run(ctx)
	})
}

func (j *jobRunner) runMaintenance(ctx context.Context) {
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	j.withLeadership(runCtx, advisoryLockKeyMaintenance, "partitions", func(ctx context.Context) error {
		n, err := store.EnsurePartitions(ctx, j.pool, store.PartitionLookahead)
		if err == nil && n > 0 {
			j.log.Info("partitions created", slog.Int("count", n))
		}
		return err
	})

	// Purging expired salts is the de-identification step, not housekeeping:
	// once a salt is gone, that day's visitor hashes cannot be linked back to
	// an address by anyone, including us.
	j.withLeadership(runCtx, advisoryLockKeyMaintenance, "salt-purge", func(ctx context.Context) error {
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
	j.withLeadership(runCtx, advisoryLockKeyMaintenance, "retention", func(ctx context.Context) error {
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
		j.withLeadership(runCtx, advisoryLockKeyMaintenance, "audit-growth-warning", func(ctx context.Context) error {
			return j.notifier.WarnAuditGrowth(ctx, auditBytes, j.auditSizeWarnBytes)
		})
	}

	// Registrations nobody completed, and spent rows past their short window.
	// Under leadership because it is a delete, and hourly because nothing here
	// is urgent — a lapsed row does nothing until it is swept, it simply must
	// not accumulate forever.
	if j.signup != nil {
		j.withLeadership(runCtx, advisoryLockKeyMaintenance, "signup-purge", func(ctx context.Context) error {
			n, err := j.signup.PurgeLapsed(ctx)
			if err == nil && n > 0 {
				j.log.Info("lapsed sign-up registrations purged", slog.Int64("count", n))
			}
			return err
		})
	}

	j.withLeadership(runCtx, advisoryLockKeyMaintenance, "partition-check", func(ctx context.Context) error {
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
	j.withLeadership(runCtx, advisoryLockKeyMaintenance, "housekeeping", func(ctx context.Context) error {
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

	// Reset tokens nobody used, and spent ones past the same short window a
	// spent registration gets (M51).
	//
	// Here rather than in a job family of its own, deliberately: a new advisory
	// key and a new goroutine to delete a handful of rows an hour is cost
	// without a reason, and this pass already holds the lock and already runs
	// the two sweeps this one is shaped like. Batched at purgeBatch for the
	// reason the link purge is — a burst drains over a few runs instead of
	// holding row locks for one long one.
	//
	// Debug rather than Info. Unlike the link purge, nothing irreversible about
	// a person's data goes here: the token is already dead by expiry or by use,
	// and the audit record of the reset is what survives.
	if j.recovery != nil {
		if n, err := j.recovery.PurgeFinished(ctx, purgeBatch); err != nil {
			errs = append(errs, err)
		} else if n > 0 {
			j.log.Debug("finished password resets purged", slog.Int64("count", n))
		}
	}

	// Pending second-factor logins that lapsed, and the ones already spent
	// (M53). Same shape and same reasoning as the reset sweep above; no
	// retention window, because a spent pending login is evidence of nothing —
	// the session it minted is the record.
	if j.mfa != nil {
		if n, err := j.mfa.PurgePendingLogins(ctx, purgeBatch); err != nil {
			errs = append(errs, err)
		} else if n > 0 {
			j.log.Debug("finished second-factor logins purged", slog.Int64("count", n))
		}
	}

	// The residue of accounts somebody deleted (M52).
	//
	// **The one sweep here that is not reclaiming space.** Everything else in
	// this pass removes rows whose deadline passed; this one *keeps* the rows and
	// takes the person out of them, because the two tables it touches —
	// `audit_logs` and `destination_disputes` — deliberately have no foreign key
	// to `users` so that a record survives its subject. Deleting them would be
	// destroying the trail; leaving them as they were would be keeping an address
	// somebody asked to have removed.
	//
	// Hourly is the whole bound on how long the residue lasts, and it is
	// documented as a number in docs/SECURITY.md rather than left to be
	// discovered here: access ends inside the deleting transaction, and only the
	// residue waits. Batched at purgeBatch for the reason the link purge is.
	//
	// Info rather than Debug: this is the irreversible destruction of somebody's
	// identifying data, done on their instruction, and — like the link purge —
	// this line is its only record. The count and nothing else; naming the
	// account here would write the identifier into a log the sweep cannot reach.
	if j.accounts != nil {
		if n, err := j.accounts.ErasePending(ctx, purgeBatch); err != nil {
			errs = append(errs, err)
		} else if n > 0 {
			j.log.Info("deleted accounts erased", slog.Int64("count", n))
		}
	}

	// Uploaded QR logos belonging to links that are already deleted (M50.5).
	//
	// **The only orphan a column can leave, and it exists because one of the
	// four deletions is soft.** Removing a code, a workspace or an organization
	// takes its logos by cascade, and replacing one is a single UPDATE, so none
	// of those can separate the row from the bytes — that is what D134 bought.
	// Deleting a *link* does not: the row is kept with a purge deadline so the
	// alias stays reserved and the link can be brought back by hand, and its
	// `qr_codes` rows go on holding up to a megabyte each, unreachable through
	// every read (they filter on `l.deleted_at IS NULL`) and therefore
	// unclearable through the endpoint that would clear them.
	//
	// Directly above the link purge's own statement in this function and after
	// it in the pass, which is the ordering that matters: a link the purge has
	// already removed took its logos with it, so what is left here is only the
	// window between deletion and purge — bounded by the trash window for a
	// small backlog and by purgeBatch for a large one.
	//
	// Idempotent, and it does nothing on almost every run: the predicate is
	// `logo IS NOT NULL`, and the partial index 03800 adds is what makes asking
	// cheap. Info rather than Debug, because it is irreversible deletion of
	// something a workspace uploaded, and this line is its only record.
	if n, err := q.ClearOrphanedQRCodeLogos(ctx, purgeBatch); err != nil {
		errs = append(errs, fmt.Errorf("clear orphaned qr code logos: %w", err))
	} else if n > 0 {
		j.log.Info("qr code logos removed from deleted links", slog.Int64("count", n))
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
	} else {
		// No relay, which is not the same as no outbox. An instance that never
		// had SMTP_HOST has an empty table and this does nothing; an instance
		// that *had* one and had it cleared holds every message enqueued before
		// the change, and holds it forever — the drain is gated on the mailer,
		// and the purge below it takes only rows that are not pending, which is
		// correct precisely because a mailer would never have left one (F52).
		//
		// Failed rather than deleted, so the row leaves by the same retention
		// path as every other finished message instead of by a second delete.
		// Past the same window rather than at once, so clearing SMTP_HOST by
		// mistake and restoring it the same afternoon still delivers the queue.
		if n, err := q.AbandonUnsendableMail(ctx, int32(mail.FinishedRetentionDays)); err != nil {
			errs = append(errs, fmt.Errorf("abandon unsendable mail: %w", err))
		} else if n > 0 {
			j.log.Warn("pending mail abandoned: no SMTP relay is configured",
				slog.Int64("count", n), slog.Int("older_than_days", mail.FinishedRetentionDays))
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
