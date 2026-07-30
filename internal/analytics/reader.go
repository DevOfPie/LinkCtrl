package analytics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// PermRead is the permission analytics reads require.
const PermRead = "analytics.read"

// Reader serves dashboard queries.
//
// Every query here reads a rollup table, never click_events, except the
// deliberately bounded recent-activity feed. That is what holds analytics
// under the 2s target once the raw table reaches tens of millions of rows.
type Reader struct {
	q *dbgen.Queries
}

func NewReader(pool *pgxpool.Pool) *Reader { return &Reader{q: dbgen.New(pool)} }

// DayPoint is one day in a time series.
type DayPoint struct {
	Day            string `json:"day"`
	Clicks         int64  `json:"clicks"`
	UniqueVisitors int64  `json:"unique_visitors"`
	BotClicks      int64  `json:"bot_clicks"`
}

// DimensionValue is one bucket of a breakdown.
type DimensionValue struct {
	Value          string `json:"value"`
	Clicks         int64  `json:"clicks"`
	UniqueVisitors int64  `json:"unique_visitors"`
}

// LinkStats is a link's analytics over a window.
type LinkStats struct {
	LinkID     uuid.UUID                   `json:"link_id"`
	From       string                      `json:"from"`
	To         string                      `json:"to"`
	Totals     Totals                      `json:"totals"`
	Series     []DayPoint                  `json:"series"`
	Dimensions map[string][]DimensionValue `json:"dimensions"`
	// Caveat travels with the data rather than living only in documentation,
	// so a client rendering these numbers can surface it.
	Caveat string `json:"caveat"`
}

type Totals struct {
	Clicks         int64 `json:"clicks"`
	UniqueVisitors int64 `json:"unique_visitors"`
	BotClicks      int64 `json:"bot_clicks"`
}

// visitorCaveat states the honest limitation of cookie-free counting.
//
// Carrier-grade NAT collapses many people behind one address into a single
// visitor; someone moving from WiFi to cellular becomes two. And because
// uniqueness is per-day, a multi-day total is a sum of daily figures rather
// than a distinct-person count — the exact number cannot be recovered once the
// salts are purged, which is the point of purging them.
const visitorCaveat = "Unique visitors are privacy-preserving estimates at daily resolution. " +
	"Multi-day totals sum daily figures, so a person visiting on several days counts once per day."

// Dimensions reported by default.
var reportedDimensions = []string{"device", "browser", "os", "country", "referrer", "language"}

func (r *Reader) LinkStats(ctx context.Context, actor *auth.Identity, linkID uuid.UUID, from, to time.Time) (*LinkStats, error) {
	if !actor.Can(PermRead) {
		return nil, fmt.Errorf("%w: viewing analytics requires %s", domain.ErrForbidden, PermRead)
	}

	// Confirms the link belongs to the actor's workspace. Without it, any
	// authenticated user could read analytics for any link id they guessed.
	if _, err := r.q.GetLink(ctx, dbgen.GetLinkParams{ID: linkID, WorkspaceID: actor.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("load link: %w", err)
	}

	rows, err := r.q.GetLinkStats(ctx, dbgen.GetLinkStatsParams{
		LinkID: linkID, FromDay: from, ToDay: to,
	})
	if err != nil {
		return nil, fmt.Errorf("read link stats: %w", err)
	}

	out := &LinkStats{
		LinkID:     linkID,
		From:       from.Format(time.DateOnly),
		To:         to.Format(time.DateOnly),
		Series:     make([]DayPoint, 0, len(rows)),
		Dimensions: map[string][]DimensionValue{},
		Caveat:     visitorCaveat,
	}
	for _, row := range rows {
		out.Series = append(out.Series, DayPoint{
			Day:            row.Day.Format(time.DateOnly),
			Clicks:         row.Clicks,
			UniqueVisitors: row.UniqueVisitors,
			BotClicks:      row.BotClicks,
		})
		out.Totals.Clicks += row.Clicks
		out.Totals.UniqueVisitors += row.UniqueVisitors
		out.Totals.BotClicks += row.BotClicks
	}

	for _, dim := range reportedDimensions {
		values, err := r.q.GetLinkDimensions(ctx, dbgen.GetLinkDimensionsParams{
			LinkID: linkID, Dimension: dim, FromDay: from, ToDay: to, RowLimit: 20,
		})
		if err != nil {
			return nil, fmt.Errorf("read %s breakdown: %w", dim, err)
		}
		bucket := make([]DimensionValue, 0, len(values))
		for _, v := range values {
			bucket = append(bucket, DimensionValue{
				Value: v.Value, Clicks: v.Clicks, UniqueVisitors: v.UniqueVisitors,
			})
		}
		out.Dimensions[dim] = bucket
	}

	return out, nil
}

// RecentClick is one entry in the live activity feed.
type RecentClick struct {
	OccurredAt time.Time `json:"occurred_at"`
	Device     string    `json:"device,omitempty"`
	Browser    string    `json:"browser,omitempty"`
	OS         string    `json:"os,omitempty"`
	Country    string    `json:"country,omitempty"`
	Referrer   string    `json:"referrer,omitempty"`
	IsBot      bool      `json:"is_bot"`
}

// RecentClicks reads raw events, bounded and index-backed.
//
// The one query that touches click_events directly. Safe because it is capped
// and served by the (link_id, occurred_at DESC) index, so cost does not grow
// with table size.
func (r *Reader) RecentClicks(ctx context.Context, actor *auth.Identity, linkID uuid.UUID, limit int32) ([]RecentClick, error) {
	if !actor.Can(PermRead) {
		return nil, domain.ErrForbidden
	}
	if _, err := r.q.GetLink(ctx, dbgen.GetLinkParams{ID: linkID, WorkspaceID: actor.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("load link: %w", err)
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	rows, err := r.q.GetRecentClicks(ctx, dbgen.GetRecentClicksParams{LinkID: linkID, RowLimit: limit})
	if err != nil {
		return nil, fmt.Errorf("read recent clicks: %w", err)
	}

	out := make([]RecentClick, 0, len(rows))
	for _, row := range rows {
		out = append(out, RecentClick{
			OccurredAt: row.OccurredAt,
			Device:     deref(row.Device),
			Browser:    deref(row.Browser),
			OS:         deref(row.Os),
			Country:    deref(row.Country),
			Referrer:   deref(row.ReferrerHost),
			IsBot:      row.IsBot,
		})
	}
	return out, nil
}

// WorkspaceOverview is the dashboard summary.
type WorkspaceOverview struct {
	From   string     `json:"from"`
	To     string     `json:"to"`
	Totals Totals     `json:"totals"`
	Series []DayPoint `json:"series"`
	Caveat string     `json:"caveat"`
}

func (r *Reader) Overview(ctx context.Context, actor *auth.Identity, from, to time.Time) (*WorkspaceOverview, error) {
	if !actor.Can(PermRead) {
		return nil, domain.ErrForbidden
	}

	rows, err := r.q.GetWorkspaceStats(ctx, dbgen.GetWorkspaceStatsParams{
		WorkspaceID: actor.WorkspaceID, FromDay: from, ToDay: to,
	})
	if err != nil {
		return nil, fmt.Errorf("read workspace stats: %w", err)
	}

	out := &WorkspaceOverview{
		From:   from.Format(time.DateOnly),
		To:     to.Format(time.DateOnly),
		Series: make([]DayPoint, 0, len(rows)),
		Caveat: visitorCaveat,
	}
	for _, row := range rows {
		out.Series = append(out.Series, DayPoint{
			Day:            row.Day.Format(time.DateOnly),
			Clicks:         row.Clicks,
			UniqueVisitors: row.UniqueVisitors,
			BotClicks:      row.BotClicks,
		})
		out.Totals.Clicks += row.Clicks
		out.Totals.UniqueVisitors += row.UniqueVisitors
		out.Totals.BotClicks += row.BotClicks
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
