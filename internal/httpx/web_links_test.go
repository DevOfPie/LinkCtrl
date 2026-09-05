package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// The country breakdown's three states (F160).
//
// Both surfaces — the choropleth and the ranked list — used to be suppressed on
// whether a GeoIP database is configured, which is a question about what this
// instance can resolve *next* rather than about what it already resolved. The
// demo is the case that proves the difference: `.env.demo` sets no GeoIP path,
// the database holds 8,123 `link_dimension_daily` rows at `dimension =
// 'country'` and 34,683 click events carrying one, two `demoCoverage()` rows
// exist to guarantee the map is worth looking at — and the page said
// ui.GeoUnavailable over every one of them. It is not
// demo-specific either: an instance that configured GeoIP, accumulated history
// and later removed the file saw the same sentence over real data.

// linkAnalytics runs the page's analytics half and renders it, which is the
// only place the two surfaces meet. Asserting on the WorldMap alone would pass
// on a build where the template ignores it, and asserting on the handler alone
// would not see the ranked list at all.
func linkAnalytics(t *testing.T, geoConfigured bool, countries []analytics.DimensionValue) string {
	t.Helper()

	r, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New: %v", err)
	}
	id := uuid.MustParse("0198c9c5-0000-7000-8000-000000000001")
	to := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -6)

	data := linkDetailPageData{
		Link:    &domain.Link{ID: id, Alias: "demo"},
		Days:    7,
		Windows: []int{7, 30, 90},
		Stats: &analytics.LinkStats{
			Totals: analytics.Totals{Clicks: 40, UniqueVisitors: 12},
			Dimensions: map[string][]analytics.DimensionValue{
				"country": countries,
			},
			Caveat: "Unique visitors are privacy-preserving estimates at daily resolution.",
		},
		GeoAvailable: geoConfigured,
	}
	req := httptest.NewRequest(http.MethodGet, "/links/"+id.String()+"?days=7", nil)
	fillLinkAnalytics(req, &data, from, to)

	rec := httptest.NewRecorder()
	if err := r.RenderPartial(rec, http.StatusOK, "link_detail", "link_analytics", data); err != nil {
		t.Fatalf("render link_analytics: %v", err)
	}
	return rec.Body.String()
}

// mapDrawn reports whether the choropleth itself rendered, by the marker
// choropleth_test.go uses for the same question: the map's frame. The rings
// beside it are drawn with the same colour ramp, so testing for the ramp would
// pass for the wrong reason.
func mapDrawn(body string) bool {
	return strings.Contains(body, `aria-label="World map shaded by`)
}

func TestTheCountryBreakdownFollowsTheDataAndNotTheConfiguration(t *testing.T) {
	resolved := []analytics.DimensionValue{
		{Value: "US", Clicks: 24, UniqueVisitors: 9},
		{Value: "GB", Clicks: 10, UniqueVisitors: 2},
		{Value: "ZA", Clicks: 4, UniqueVisitors: 1},
	}

	// The demo, exactly: history, and no database configured today.
	t.Run("history without a database", func(t *testing.T) {
		body := linkAnalytics(t, false, resolved)

		if strings.Contains(body, ui.GeoUnavailable) {
			t.Error("the page says geographic data is unavailable over 38 clicks of " +
				"country history. The rows are present, the rollup is correct, and " +
				"the only thing missing is a database nothing here needs (F160)")
		}
		if !mapDrawn(body) {
			t.Error("the choropleth is not drawn for a window holding country " +
				"rows; the demo exists to be looked at and this is the surface " +
				"two demoCoverage() rows guarantee the data for")
		}
		if !strings.Contains(body, "US") || !strings.Contains(body, "GB") {
			t.Error("the ranked country list is empty over country rows that exist")
		}
	})

	// The state the sentence is actually about. "unknown" is what the rollup
	// writes for an address that did not resolve, so a window made of it is a
	// window in which nothing resolved — and with no database, nothing will.
	t.Run("no database and nothing resolved", func(t *testing.T) {
		body := linkAnalytics(t, false, []analytics.DimensionValue{
			{Value: "unknown", Clicks: 40, UniqueVisitors: 12},
		})

		// Twice: once where the map would be, once where the list would be.
		// Somebody who scrolled past one has to meet it at the other rather than
		// meet an empty card — TestWithNoGeoIPTheMapSaysSoAndDrawsNothing asserts
		// the same count from the ui side.
		if n := strings.Count(body, ui.GeoUnavailable); n != 2 {
			t.Errorf("the unavailable sentence appears %d times, want 2 — the map "+
				"and the ranked list must both say it", n)
		}
		if mapDrawn(body) {
			t.Error("a world uniformly coloured \"unknown\" is drawn for a window " +
				"in which nothing resolved")
		}
		if strings.Contains(body, "unknown") {
			t.Error("the ranked list offers \"unknown\" as a country. It is what the " +
				"rollup writes for an address that did not resolve, and it is not a place")
		}
	})

	// And the case the configuration check was added for, which still holds: a
	// database, and a link nobody has clicked. Nothing is wrong with this
	// instance, so it is told nothing about itself.
	t.Run("a database and no clicks yet", func(t *testing.T) {
		body := linkAnalytics(t, true, nil)

		if strings.Contains(body, ui.GeoUnavailable) {
			t.Error("a link with a GeoIP database and no clicks yet tells its owner " +
				"the instance has no database")
		}
		if !strings.Contains(body, "No data yet") {
			t.Error("the ranked list has no empty state at all for a link nobody has clicked")
		}
		if !strings.Contains(body, "No clicks resolved to a country in this window") {
			t.Error("the map has no empty state for a link nobody has clicked")
		}
	})
}

// TestOnlyTheRuleFormAsksWhetherGeoIPIsConfigured is the other half of F160, and
// the half that must NOT move.
//
// A country, region or city routing rule matches going forward, which genuinely
// depends on the database configured now — history cannot make one match. So the
// rule form's warning is correct on exactly the instance the breakdown was wrong
// on, and the fix has to leave the two branching on different facts.
func TestOnlyTheRuleFormAsksWhetherGeoIPIsConfigured(t *testing.T) {
	const warning = "No GeoIP database is configured on this instance, so a country"

	r, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New: %v", err)
	}
	// The demo's state: history, no database. The breakdown shows the history;
	// the rule form still says a geo rule will never match.
	data := linkDetailPageData{
		Link:         &domain.Link{ID: uuid.MustParse("0198c9c5-0000-7000-8000-000000000001")},
		GeoAvailable: false,
		RuleWeekdays: domain.RuleWeekdays,
		RuleHelp:     ruleConditionHelp,
	}
	rec := httptest.NewRecorder()
	if err := r.RenderPartial(rec, http.StatusOK, "link_detail", "link_rule_form", data); err != nil {
		t.Fatalf("render link_rule_form: %v", err)
	}
	if !strings.Contains(rec.Body.String(), warning) {
		t.Error("the rule form no longer warns that a geo condition cannot match " +
			"without a database. F160 is about reporting what was resolved; a rule " +
			"is about resolving what has not happened yet, and only one of those " +
			"can be answered from history")
	}

	data.GeoAvailable = true
	rec = httptest.NewRecorder()
	if err := r.RenderPartial(rec, http.StatusOK, "link_detail", "link_rule_form", data); err != nil {
		t.Fatalf("render link_rule_form: %v", err)
	}
	if strings.Contains(rec.Body.String(), warning) {
		t.Error("the rule form warns about a missing database on an instance that has one")
	}
}
