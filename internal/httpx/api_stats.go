package httpx

import (
	"net/http"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// StatsAPI serves analytics reads.
type StatsAPI struct {
	Reader *analytics.Reader
}

// maxRange caps a query window. Unbounded ranges are how a dashboard query
// turns into a full scan of the rollup tables.
const maxRangeDays = 400

// parseRange reads from/to query parameters, defaulting to the last 30 days.
func parseRange(r *http.Request) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	to := analytics.SaltDay(now)
	from := to.AddDate(0, 0, -29)

	q := r.URL.Query()
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.DateOnly, v)
		if err != nil {
			return time.Time{}, time.Time{}, domain.ValidationErrors{{
				Field: "from", Code: "invalid", Message: "from must be a date in YYYY-MM-DD form",
			}}
		}
		from = t.UTC()
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.DateOnly, v)
		if err != nil {
			return time.Time{}, time.Time{}, domain.ValidationErrors{{
				Field: "to", Code: "invalid", Message: "to must be a date in YYYY-MM-DD form",
			}}
		}
		to = t.UTC()
	}

	if to.Before(from) {
		return time.Time{}, time.Time{}, domain.ValidationErrors{{
			Field: "to", Code: "invalid_range", Message: "to must not be before from",
		}}
	}
	if to.Sub(from) > maxRangeDays*24*time.Hour {
		return time.Time{}, time.Time{}, domain.ValidationErrors{{
			Field: "from", Code: "range_too_large",
			Message: "the range must not exceed 400 days",
		}}
	}
	return from, to, nil
}

func (a *StatsAPI) LinkStats(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	from, to, err := parseRange(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	stats, err := a.Reader.LinkStats(r.Context(), IdentityFrom(r.Context()), id, from, to)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, stats)
}

func (a *StatsAPI) LinkClicks(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	clicks, err := a.Reader.RecentClicks(r.Context(), IdentityFrom(r.Context()), id, 100)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": clicks})
}

func (a *StatsAPI) Overview(w http.ResponseWriter, r *http.Request) {
	from, to, err := parseRange(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	overview, err := a.Reader.Overview(r.Context(), IdentityFrom(r.Context()), from, to)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, overview)
}
