//go:build integration

package integration

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
)

// The two halves of the rollup, and the watermark each one keeps (M37).
//
// The dimension breakdowns are the expensive half — 16-21 seconds per run at the
// SLO dataset, against a totals pass measured in milliseconds — so they moved to
// a fifteen-minute clock while the totals stayed on sixty seconds. Everything
// below is about what that split must not break.

// TestTheTotalsPassDoesNotWriteBreakdowns is the split itself, asserted from the
// only place it is observable: what each pass leaves in the database.
//
// If RunRecentTotals still wrote link_dimension_daily then the cadence change
// would be cosmetic — the expensive query would run every sixty seconds under a
// different function name, and the measurement in docs/slo.md would be a
// measurement of nothing.
func TestTheTotalsPassDoesNotWriteBreakdowns(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	linkID, wsID := seedLink(t, pool)

	ing := newIngester(t, pool, analytics.IngestConfig{BatchSize: 100, FlushInterval: 20 * time.Millisecond})
	now := time.Now().UTC()
	for i := range 60 {
		ing.Record(analytics.Event{
			LinkID: linkID, WorkspaceID: wsID, OccurredAt: now,
			IP:        netip.MustParseAddr("198.51.100." + itoa(i%12)),
			UserAgent: "Mozilla/5.0 Chrome/120",
			Language:  "en",
		})
	}
	if err := ing.Close(ctx); err != nil {
		t.Fatal(err)
	}

	roller := analytics.NewRoller(pool, quietLogger())
	if err := roller.RunRecentTotals(ctx, now); err != nil {
		t.Fatal(err)
	}

	if got := countDaily(t, ctx, pool, linkID); got == 0 {
		t.Error("the totals pass wrote no link_click_daily rows")
	}
	if got := countDimensions(t, ctx, pool, linkID); got != 0 {
		t.Errorf("the totals pass wrote %d link_dimension_daily rows; it must write "+
			"none, or moving the breakdowns to a longer cadence saves nothing and "+
			"the re-measurement in docs/slo.md measures a query that still runs "+
			"every sixty seconds", got)
	}

	if err := roller.RunRecentDimensions(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got := countDimensions(t, ctx, pool, linkID); got == 0 {
		t.Error("the dimension pass wrote no link_dimension_daily rows")
	}
}

// TestEachHalfKeepsItsOwnWatermark is the failure the split reintroduces if it
// is done carelessly, and it is not a hypothetical — it is the exact bug the
// watermark was added to fix, wearing a new hat.
//
// The watermark decides how far back a run reopens. Two jobs advancing at
// different rates cannot share one: the totals pass, running fifteen times as
// often, would push the watermark past a day the dimension pass had not covered
// yet, and that day's breakdowns would never be computed by anything.
func TestEachHalfKeepsItsOwnWatermark(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	linkID, wsID := seedLink(t, pool)

	ing := newIngester(t, pool, analytics.IngestConfig{BatchSize: 50, FlushInterval: 20 * time.Millisecond})
	now := time.Now().UTC()
	for i := range 20 {
		ing.Record(analytics.Event{
			LinkID: linkID, WorkspaceID: wsID, OccurredAt: now,
			IP:        netip.MustParseAddr("198.51.100." + itoa(i%5)),
			UserAgent: "Mozilla/5.0 Chrome/120",
		})
	}
	if err := ing.Close(ctx); err != nil {
		t.Fatal(err)
	}

	roller := analytics.NewRoller(pool, quietLogger())
	if err := roller.RunRecentTotals(ctx, now); err != nil {
		t.Fatal(err)
	}

	if !hasJobRow(t, ctx, pool, analytics.TotalsJob) {
		t.Errorf("the totals pass left no %s row in job_state", analytics.TotalsJob)
	}
	if hasJobRow(t, ctx, pool, analytics.DimensionJob) {
		t.Errorf("running the totals pass advanced the %s watermark; the dimension "+
			"pass would then skip a day it never covered, which is the permanent "+
			"gap the watermark exists to prevent", analytics.DimensionJob)
	}

	if err := roller.RunRecentDimensions(ctx, now); err != nil {
		t.Fatal(err)
	}
	if !hasJobRow(t, ctx, pool, analytics.DimensionJob) {
		t.Errorf("the dimension pass left no %s row in job_state", analytics.DimensionJob)
	}
}

// TestStalenessIsMeasuredFromTheLastSuccessNotTheLastAttempt.
//
// The metric M37 adds is what an operator alerts on now that a breakdown is
// allowed to be a quarter of an hour behind. `last_run_at` cannot carry it:
// RecordJobFailure stamps that column too, so a job failing on every tick would
// publish itself as perpetually fresh and the alert would never fire. This is
// the assertion that the column being read is the other one.
func TestStalenessIsMeasuredFromTheLastSuccessNotTheLastAttempt(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()

	roller := analytics.NewRoller(pool, quietLogger())
	now := time.Now().UTC()
	if err := roller.RunRecentTotals(ctx, now); err != nil {
		t.Fatal(err)
	}

	before := stalenessOf(t, ctx, roller, analytics.TotalsJob)
	if before < 0 || before > 60 {
		t.Fatalf("staleness immediately after a successful run is %.1fs, want a "+
			"figure near zero", before)
	}

	// A failed run: last_run_at moves, last_success_at must not.
	if _, err := pool.Exec(ctx,
		`UPDATE job_state SET last_success_at = now() - interval '20 minutes' WHERE job = $1`,
		analytics.TotalsJob); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_state SET last_run_at = now(), last_error = 'boom' WHERE job = $1`,
		analytics.TotalsJob); err != nil {
		t.Fatal(err)
	}

	after := stalenessOf(t, ctx, roller, analytics.TotalsJob)
	if after < 1000 {
		t.Errorf("staleness after a failing run is %.1fs; a job whose last success "+
			"was twenty minutes ago must read as stale however recently it last "+
			"tried, or the alert cannot distinguish a working job from a broken one", after)
	}
}

// TestAJobThatHasNeverSucceededHasNoSeries. An invented "infinitely stale"
// figure is what every fresh instance would look like for its first few
// seconds, and an alert that fires on every boot is an alert nobody reads.
func TestAJobThatHasNeverSucceededHasNoSeries(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO job_state (job, last_run_at, last_error)
		 VALUES ('never_worked', now(), 'boom')
		 ON CONFLICT (job) DO UPDATE SET last_run_at = now(), last_error = 'boom',
		                                 last_success_at = NULL`); err != nil {
		t.Fatal(err)
	}

	stale, err := analytics.NewRoller(pool, quietLogger()).Staleness(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stale {
		if s.Job == "never_worked" {
			t.Errorf("a job that has never succeeded reports staleness %.1fs; it "+
				"must report nothing at all", s.Seconds)
		}
	}
}

// TestTheDimensionCadenceIsLongerThanTheTotals. The number itself is a judgement
// about a measured cost and lives in one place; this only holds the relationship
// that the whole milestone rests on.
func TestTheDimensionCadenceIsLongerThanTheTotals(t *testing.T) {
	const totals = 60 * time.Second
	if analytics.DimensionInterval <= totals {
		t.Errorf("DimensionInterval is %s, which is not longer than the %s totals "+
			"cadence; the breakdowns would still run on the clock they do not fit in",
			analytics.DimensionInterval, totals)
	}
}

func countDaily(t *testing.T, ctx context.Context, pool *pgxpool.Pool, linkID uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM link_click_daily WHERE link_id = $1", linkID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countDimensions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, linkID uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM link_dimension_daily WHERE link_id = $1", linkID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func hasJobRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, job string) bool {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM job_state WHERE job = $1 AND watermark IS NOT NULL", job).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func stalenessOf(t *testing.T, ctx context.Context, r *analytics.Roller, job string) float64 {
	t.Helper()
	stale, err := r.Staleness(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stale {
		if s.Job == job {
			return s.Seconds
		}
	}
	t.Fatalf("no staleness reported for %s", job)
	return 0
}
