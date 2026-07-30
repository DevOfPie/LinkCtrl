package analytics

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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

// RunRecent recomputes yesterday and today.
//
// Two days rather than one because a click just before midnight UTC can be
// written just after it, and a run covering only today would miss it until the
// next finalize pass.
func (r *Roller) RunRecent(ctx context.Context, now time.Time) error {
	today := SaltDay(now)
	return r.Run(ctx, today.AddDate(0, 0, -1), today.AddDate(0, 0, 1))
}
