package analytics

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	// Destinations is the per-destination breakdown a split test is read from
	// (M36). Empty for a link that has never sent a click anywhere other than
	// its own destination, which is every link on a default instance.
	Destinations []DestinationSplit `json:"destinations"`
	// Caveat travels with the data rather than living only in documentation,
	// so a client rendering these numbers can surface it.
	Caveat string `json:"caveat"`
}

// DestinationSplit is one destination's share of a link's clicks (M36).
//
// The row for the link's own destination is synthesized from what the other rows
// do not account for, because a click that went there carries a NULL
// destination_id — see migration 02200. So `Clicks` here sums to the link's
// non-bot total by construction rather than by the rollup happening to agree.
type DestinationSplit struct {
	DestinationID uuid.UUID `json:"destination_id"`
	URL           string    `json:"url"`
	// Weight is the arm's configured weight, or 0 where it is not a weighted
	// arm. It is what makes the breakdown readable as a test: "40% configured,
	// 41% observed" is a working split and "40% configured, 3% observed" is a
	// broken destination.
	Weight int32 `json:"weight"`
	// IsPrimary marks the link's own destination.
	IsPrimary bool `json:"is_primary"`
	// Removed marks clicks attributed to a destination that has since been
	// deleted. They are reported rather than dropped: a running test's totals
	// must not change because somebody tidied up an arm.
	Removed bool `json:"removed,omitempty"`
	// Approximate marks a row whose click count includes traffic the
	// per-destination rollup has not attributed yet.
	//
	// It is only ever the link's own destination, and only because that row is
	// computed as a remainder rather than read. A click on the link's own
	// destination carries the zero uuid and the dimension rollup filters
	// `destination_id IS NOT NULL`, so the primary has no rollup row to read —
	// its clicks are whatever the 60-second totals hold that the 15-minute
	// destination rollup has not accounted for. Between those two cadences that
	// remainder is the primary's real clicks *plus* every split-arm click of the
	// last quarter-hour, and it was rendered as positive attribution to a named
	// destination (F107). Worst case is a split test viewed inside its first
	// fifteen minutes: 100% to the link's own destination and 0% to the arms.
	Approximate bool  `json:"approximate,omitempty"`
	Clicks      int64 `json:"clicks"`
	// UniqueVisitors is the count, and VisitorsKnown says whether it is one.
	//
	// Two fields rather than a pointer, because this crosses to a template and
	// `0` and *not measured* had been the same value on the primary row since
	// M36 — permanently, not just during the lag. The remainder carries no
	// visitor figure at all: unique visitors are counted per destination by the
	// rollup, and the row the rollup never writes has none to carry.
	UniqueVisitors int64   `json:"unique_visitors"`
	VisitorsKnown  bool    `json:"visitors_known"`
	Share          float64 `json:"share"`
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

	destinations, err := r.destinationSplit(ctx, actor.WorkspaceID, linkID, from, to, out.Totals.Clicks)
	if err != nil {
		return nil, err
	}
	out.Destinations = destinations

	return out, nil
}

// destinationDimension is the name RollupDestinationDaily writes its rows under.
const destinationDimension = "destination"

// destinationSplit assembles the per-destination breakdown (M36).
//
// Two reads and no scan of click_events: the rolled-up per-destination rows,
// under the same dimension machinery every other breakdown uses, and the link's
// destination list to give each id a URL. Storing the URL in the rollup instead
// would freeze it at the moment of the rollup and make an edited arm read as two.
//
// The link's own destination is not in the rollup at all — a click that went
// there has a NULL destination_id — so its row is the remainder: total non-bot
// clicks minus everything attributed elsewhere. That is also why this is skipped
// entirely when nothing was attributed: a link with no split would otherwise
// grow a one-row "breakdown" saying all of its traffic went where it was always
// going to go.
func (r *Reader) destinationSplit(
	ctx context.Context, workspaceID, linkID uuid.UUID, from, to time.Time, totalClicks int64,
) ([]DestinationSplit, error) {
	rows, err := r.q.GetLinkDimensions(ctx, dbgen.GetLinkDimensionsParams{
		LinkID: linkID, Dimension: destinationDimension,
		FromDay: from, ToDay: to, RowLimit: int32(domain.MaxRulesPerLink) + 1,
	})
	if err != nil {
		return nil, fmt.Errorf("read destination breakdown: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	known, err := r.q.ListLinkDestinations(ctx, dbgen.ListLinkDestinationsParams{
		LinkID: linkID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("read link destinations: %w", err)
	}
	byID := make(map[uuid.UUID]dbgen.ListLinkDestinationsRow, len(known))
	var primary DestinationSplit
	for _, d := range known {
		byID[d.ID] = d
		if d.IsPrimary {
			primary = DestinationSplit{
				DestinationID: d.ID, URL: d.Url, Weight: d.Weight, IsPrimary: true,
			}
		}
	}

	out := make([]DestinationSplit, 0, len(rows)+1)
	var attributed int64
	for _, row := range rows {
		id, perr := uuid.Parse(row.Value)
		if perr != nil {
			// A value the rollup did not write. Skipped rather than surfaced: the
			// only way to produce one is to write into link_dimension_daily by
			// hand, and a breakdown is not the place to report that.
			continue
		}
		entry := DestinationSplit{
			DestinationID: id, Clicks: row.Clicks,
			UniqueVisitors: row.UniqueVisitors, VisitorsKnown: true,
		}
		if d, ok := byID[id]; ok {
			entry.URL, entry.Weight, entry.IsPrimary = d.Url, d.Weight, d.IsPrimary
		} else {
			entry.Removed = true
			entry.URL = "(destination removed)"
		}
		attributed += row.Clicks
		out = append(out, entry)
	}

	// The remainder is the link's own destination, plus whatever the destination
	// rollup has not caught up on. Clamped at zero because the two figures come
	// from two passes over the same events and a recompute landing between them
	// can leave the totals apart — a negative bar would render that race rather
	// than anything that happened.
	//
	// **Marked approximate rather than presented as attribution.** The clamp's
	// comment used to say the two figures "can leave the totals a few clicks
	// apart", and that was true when it was written: M36 ran both rollups on one
	// clock. M37 moved the destination breakdown to fifteen minutes and left the
	// sentence behind, so "a few clicks" became a quarter-hour of traffic, all of
	// it landing on one named row (F107). The number is still the best available
	// and is still shown; what changes is that the row no longer claims the
	// clicks were attributed to it.
	if remainder := totalClicks - attributed; remainder > 0 && primary.DestinationID != uuid.Nil {
		primary.Clicks = remainder
		// Never known, and not merely unknown during the lag: unique visitors
		// are counted per destination by the rollup, and this is the row the
		// rollup does not write. Reporting 0 was reporting a measurement that
		// had not been taken.
		primary.VisitorsKnown = false
		primary.Approximate = attributed > 0
		out = append(out, primary)
	}

	if totalClicks > 0 {
		for i := range out {
			out[i].Share = float64(out[i].Clicks) * 100 / float64(totalClicks)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Clicks > out[j].Clicks })
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
