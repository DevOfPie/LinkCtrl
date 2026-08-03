package analytics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Roller recomputes the pre-aggregated tables the dashboard reads.
type Roller struct {
	q   *dbgen.Queries
	log *slog.Logger
}

func NewRoller(pool *pgxpool.Pool, log *slog.Logger) *Roller {
	if log == nil {
		log = slog.Default()
	}
	return &Roller{q: dbgen.New(pool), log: log}
}

// Run recomputes every rollup for [from, to).
//
// Recomputation rather than incremental accumulation is deliberate. Each run
// derives whole days from the raw events and upserts, so a retry after a crash
// mid-run converges to the same numbers. An "add what arrived since the last
// watermark" design double-counts on every retry and drifts permanently once
// it does — and the drift is invisible until someone reconciles by hand.
//
// The scheduler no longer calls this: since M37 the two halves run on different
// clocks. It stays as the whole-of-analytics pass for an explicit window, which
// is what a startup run and an operator recomputing a repaired day both want.
func (r *Roller) Run(ctx context.Context, from, to time.Time) error {
	if err := r.RunTotals(ctx, from, to); err != nil {
		return err
	}
	return r.RunDimensions(ctx, from, to)
}

// RunTotals recomputes the per-link and per-workspace daily totals.
//
// The cheap half, and the half the dashboard's headline numbers come from. It
// writes one row per (link, day) and one per (workspace, day), so its upsert
// count is bounded by the number of links that were clicked rather than by the
// number of distinct dimension values they were clicked from.
func (r *Roller) RunTotals(ctx context.Context, from, to time.Time) error {
	from, to = from.UTC(), to.UTC()
	start := time.Now()

	if err := r.q.RollupLinkDaily(ctx, dbgen.RollupLinkDailyParams{
		WindowStart: from, WindowEnd: to,
	}); err != nil {
		return fmt.Errorf("rollup link daily: %w", err)
	}
	if err := r.q.RollupWorkspaceDaily(ctx, dbgen.RollupWorkspaceDailyParams{
		WindowStart: from, WindowEnd: to,
	}); err != nil {
		return fmt.Errorf("rollup workspace daily: %w", err)
	}

	r.log.Debug("totals rollup complete",
		slog.Time("from", from), slog.Time("to", to),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}

// RunDimensions recomputes the per-dimension and per-destination breakdowns.
//
// The expensive half, and the reason M37 exists. Measured on the SLO dataset
// (5.7M events, ~830k inside the recomputed window) this takes 16-21 seconds,
// and the cost is the ~553,053 conflicting tuples a whole-day recompute of
// (link, day, dimension, value) implies — not the scan, which was already
// rewritten to read click_events once. See docs/slo.md.
//
// Recomputing whole days is not negotiable: it is what makes a retry converge
// instead of double-counting. So the fix is to run it less often than the
// totals, which is what DimensionInterval is, rather than to make it cheaper.
func (r *Roller) RunDimensions(ctx context.Context, from, to time.Time) error {
	from, to = from.UTC(), to.UTC()
	start := time.Now()

	if err := r.q.RollupDimensionDaily(ctx, dbgen.RollupDimensionDailyParams{
		WindowStart: from, WindowEnd: to,
	}); err != nil {
		return fmt.Errorf("rollup dimension daily: %w", err)
	}
	// The per-destination breakdown (M36). Last, and separate from the pass
	// above, because it reads only the clicks that carry a destination_id — a
	// partial index over rows that do not exist on an instance running no split
	// test. Folding it into the dimension pass would have made every click on
	// every link pay for it.
	if err := r.q.RollupDestinationDaily(ctx, dbgen.RollupDestinationDailyParams{
		WindowStart: from, WindowEnd: to,
	}); err != nil {
		return fmt.Errorf("rollup destination daily: %w", err)
	}

	r.log.Debug("dimension rollup complete",
		slog.Time("from", from), slog.Time("to", to),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}

// The two jobs' rows in job_state.
//
// TotalsJob keeps the name the single job had, so an instance upgrading into
// M37 carries its watermark forward instead of reopening ninety days on the
// first tick. DimensionJob is new and therefore has no row, which is exactly
// right: its first run finds no watermark, covers the default two-day window,
// and is correct from then on.
const (
	TotalsJob    = "analytics_rollup"
	DimensionJob = "analytics_dimension_rollup"
)

// DimensionInterval is how often the dimension breakdowns are recomputed.
//
// Fifteen minutes against a job measured at 16-21 seconds is a duty cycle of
// about 2.3%, where sixty seconds was about 33%. That is the whole of the fix:
// the same work, on a clock it fits inside, with roughly forty times the
// headroom before it stops fitting again. The recorded fallback if cadence
// alone stops holding is to narrow the recomputed window — see Plan.md's
// dimension-rollup row and docs/slo.md.
//
// The cost is paid by the reader, and it is named rather than hidden: a
// breakdown can be up to fifteen minutes behind the totals on the same page.
// The staleness gauge is what makes that observable instead of merely true.
const DimensionInterval = 15 * time.Minute

// maxCatchupDays bounds the window a single RunRecent will reopen.
//
// An instance down for a year must not answer its first tick with a scan of a
// year of click_events — that turns a recovery into an outage. The gap beyond
// the bound is left alone and logged at warn level with the days it did not
// cover, so it is a decision an operator can act on (`lctl` can be pointed at
// an explicit window) rather than a silent hole.
const maxCatchupDays = 90

// RunRecent recomputes everything not known to be final, and at minimum
// yesterday and today.
//
// Both halves, under their own watermarks. The scheduler does not call this —
// it ticks the two halves separately — but a startup run and `lctl` do, because
// "make the numbers current" is one request even when the maintenance of them
// is two jobs.
func (r *Roller) RunRecent(ctx context.Context, now time.Time) error {
	if err := r.RunRecentTotals(ctx, now); err != nil {
		return err
	}
	return r.RunRecentDimensions(ctx, now)
}

// RunRecentTotals recomputes the per-link and per-workspace totals for
// everything not known to be final.
func (r *Roller) RunRecentTotals(ctx context.Context, now time.Time) error {
	return r.runRecent(ctx, now, TotalsJob, r.RunTotals)
}

// RunRecentDimensions recomputes the breakdowns for everything not known to be
// final, on its own watermark.
//
// Its own watermark and not a share of the totals' one, because the two jobs
// now advance at different rates: a dimension pass that read the totals'
// watermark would find it already past the day it had not covered yet and would
// leave that day permanently unaggregated — the exact failure the watermark was
// introduced to fix, reintroduced by splitting the cadence.
func (r *Roller) RunRecentDimensions(ctx context.Context, now time.Time) error {
	return r.runRecent(ctx, now, DimensionJob, r.RunDimensions)
}

// runRecent is the shared window logic: how far back to reopen, and what to
// record afterwards.
//
// Two days is the floor rather than the window because a click just before
// midnight UTC can be written just after it, and a run covering only today
// would miss it until the next pass. The upper end of the reopened window comes
// from job_state: a fixed two-day window meant that downtime spanning a UTC day
// left that day permanently unaggregated, because by the time the process came
// back the day was no longer "yesterday" and no run would ever look at it
// again. Recomputation makes reopening old days safe — each run derives whole
// days from raw events and upserts — so the watermark only ever decides how far
// back to start, never what the numbers are.
func (r *Roller) runRecent(
	ctx context.Context, now time.Time, job string,
	run func(context.Context, time.Time, time.Time) error,
) error {
	today := SaltDay(now)
	from := today.AddDate(0, 0, -1)

	if w, err := r.q.GetJobWatermark(ctx, job); err != nil {
		// No row yet is the normal first-boot case; anything else is worth a
		// line but not worth skipping the rollup for.
		if !errors.Is(err, pgx.ErrNoRows) {
			r.log.Warn("could not read the rollup watermark; covering the default window",
				slog.String("job", job), slog.String("error", err.Error()))
		}
	} else if w != nil {
		if earliest := SaltDay(*w); earliest.Before(from) {
			if limit := today.AddDate(0, 0, -maxCatchupDays); earliest.Before(limit) {
				r.log.Warn("rollup gap exceeds the catch-up bound; the oldest days are not being recomputed",
					slog.String("job", job),
					slog.Time("watermark", *w),
					slog.Time("recomputing_from", limit),
					slog.Int("days_skipped", int(limit.Sub(earliest).Hours()/24)))
				earliest = limit
			}
			from = earliest
		}
	}

	to := today.AddDate(0, 0, 1)
	if err := run(ctx, from, to); err != nil {
		// Best-effort: the run already failed, and failing to record that is
		// not a reason to lose the error itself.
		if rerr := r.q.RecordJobFailure(ctx, dbgen.RecordJobFailureParams{
			Job: job, LastError: ptr(err.Error()),
		}); rerr != nil {
			r.log.Warn("could not record the rollup failure",
				slog.String("job", job), slog.String("error", rerr.Error()))
		}
		return err
	}

	// Today is still accumulating, so only the days before it are final. The
	// next run reopens from here, which under normal operation is the same
	// two-day window as before.
	if err := r.q.SetJobWatermark(ctx, dbgen.SetJobWatermarkParams{
		Job: job, Watermark: &today,
	}); err != nil {
		// The rollup itself succeeded. A missing watermark only means the next
		// run reopens its default window, which is what it did before this
		// existed — worth a warning, not a failure.
		r.log.Warn("could not advance the rollup watermark",
			slog.String("job", job), slog.String("error", err.Error()))
	}
	return nil
}

// Staleness is how long ago a job last succeeded, read from job_state.
type Staleness struct {
	Job     string
	Seconds float64
}

// Staleness reports every job that has ever succeeded, and how long ago.
//
// Jobs that have never succeeded are omitted by the query rather than reported
// as infinitely stale: a series invented for a job that has not run yet is
// indistinguishable from one for a job that stopped, and the first is what every
// fresh instance looks like for its first few seconds.
func (r *Roller) Staleness(ctx context.Context) ([]Staleness, error) {
	rows, err := r.q.GetJobStaleness(ctx)
	if err != nil {
		return nil, fmt.Errorf("read job staleness: %w", err)
	}
	out := make([]Staleness, 0, len(rows))
	for _, row := range rows {
		out = append(out, Staleness{Job: row.Job, Seconds: row.StaleSeconds})
	}
	return out, nil
}

func ptr[T any](v T) *T { return &v }
