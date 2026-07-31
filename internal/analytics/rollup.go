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

// Run recomputes rollups for [from, to).
//
// Recomputation rather than incremental accumulation is deliberate. Each run
// derives whole days from the raw events and upserts, so a retry after a crash
// mid-run converges to the same numbers. An "add what arrived since the last
// watermark" design double-counts on every retry and drifts permanently once
// it does — and the drift is invisible until someone reconciles by hand.
func (r *Roller) Run(ctx context.Context, from, to time.Time) error {
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
	if err := r.q.RollupDimensionDaily(ctx, dbgen.RollupDimensionDailyParams{
		WindowStart: from, WindowEnd: to,
	}); err != nil {
		return fmt.Errorf("rollup dimension daily: %w", err)
	}

	r.log.Debug("rollup complete",
		slog.Time("from", from), slog.Time("to", to),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}

// rollupJob names this job's row in job_state.
const rollupJob = "analytics_rollup"

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
// Two days is the floor rather than the window because a click just before
// midnight UTC can be written just after it, and a run covering only today
// would miss it until the next pass. The upper end of the reopened window comes
// from job_state: a fixed two-day window meant that downtime spanning a UTC day
// left that day permanently unaggregated, because by the time the process came
// back the day was no longer "yesterday" and no run would ever look at it
// again. Recomputation makes reopening old days safe — each run derives whole
// days from raw events and upserts — so the watermark only ever decides how far
// back to start, never what the numbers are.
func (r *Roller) RunRecent(ctx context.Context, now time.Time) error {
	today := SaltDay(now)
	from := today.AddDate(0, 0, -1)

	if w, err := r.q.GetJobWatermark(ctx, rollupJob); err != nil {
		// No row yet is the normal first-boot case; anything else is worth a
		// line but not worth skipping the rollup for.
		if !errors.Is(err, pgx.ErrNoRows) {
			r.log.Warn("could not read the rollup watermark; covering the default window",
				slog.String("error", err.Error()))
		}
	} else if w != nil {
		if earliest := SaltDay(*w); earliest.Before(from) {
			if limit := today.AddDate(0, 0, -maxCatchupDays); earliest.Before(limit) {
				r.log.Warn("rollup gap exceeds the catch-up bound; the oldest days are not being recomputed",
					slog.Time("watermark", *w),
					slog.Time("recomputing_from", limit),
					slog.Int("days_skipped", int(limit.Sub(earliest).Hours()/24)))
				earliest = limit
			}
			from = earliest
		}
	}

	to := today.AddDate(0, 0, 1)
	if err := r.Run(ctx, from, to); err != nil {
		// Best-effort: the run already failed, and failing to record that is
		// not a reason to lose the error itself.
		if rerr := r.q.RecordJobFailure(ctx, dbgen.RecordJobFailureParams{
			Job: rollupJob, LastError: ptr(err.Error()),
		}); rerr != nil {
			r.log.Warn("could not record the rollup failure",
				slog.String("error", rerr.Error()))
		}
		return err
	}

	// Today is still accumulating, so only the days before it are final. The
	// next run reopens from here, which under normal operation is the same
	// two-day window as before.
	if err := r.q.SetJobWatermark(ctx, dbgen.SetJobWatermarkParams{
		Job: rollupJob, Watermark: &today,
	}); err != nil {
		// The rollup itself succeeded. A missing watermark only means the next
		// run reopens its default window, which is what it did before this
		// existed — worth a warning, not a failure.
		r.log.Warn("could not advance the rollup watermark",
			slog.String("error", err.Error()))
	}
	return nil
}

func ptr[T any](v T) *T { return &v }
