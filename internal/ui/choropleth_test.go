package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/ui/geo"
)

// renderLinkDetail renders the page with the map replaced by whatever this test
// wants to look at, and returns the markup.
func renderLinkDetail(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	data, ok := pageData(t)["link_detail"].(map[string]any)
	if !ok {
		t.Fatal("the link_detail page data is not a map")
	}
	if mutate != nil {
		mutate(data)
	}
	rec := httptest.NewRecorder()
	if err := r.Render(rec, http.StatusOK, "link_detail", data); err != nil {
		t.Fatalf("render link_detail: %v", err)
	}
	return rec.Body.String()
}

// TestWithNoGeoIPTheMapSaysSoAndDrawsNothing is the bullet M37 spells out, and
// the failure it names is specific: "no world uniformly coloured unknown".
//
// A choropleth degrades badly. With every country at zero it still renders 174
// shapes, a legend and a heading — a confident picture of an instance that
// cannot resolve a country at all. So the assertion is in two halves: the
// sentence is present, and the map is *absent*.
func TestWithNoGeoIPTheMapSaysSoAndDrawsNothing(t *testing.T) {
	body := renderLinkDetail(t, func(d map[string]any) {
		d["Map"] = Choropleth(nil, "clicks", "estimates", false)
		// The handler gives the ranked list nothing when the instance cannot
		// resolve a country, which is what makes its empty state reachable at
		// all — see linkDetailPageData.Countries.
		d["Countries"] = []map[string]any{}
	})

	if !strings.Contains(body, GeoUnavailable) {
		t.Errorf("with no GeoIP database the page does not say %q", GeoUnavailable)
	}
	// Twice: once where the map would be, once where the ranked list would be.
	// Both views have to say it, because somebody who scrolled past one has to
	// meet it at the other rather than meet an empty card.
	if n := strings.Count(body, GeoUnavailable); n != 2 {
		t.Errorf("the unavailable sentence appears %d times, want 2 — the map and "+
			"the ranked list must both say it", n)
	}
	// The map itself must be absent, not merely unshaded. Its frame and its
	// fill rule are the two markers nothing else on the page emits: the rings
	// beside it are squares drawn with the same colour ramp, so testing for the
	// ramp alone would pass for the wrong reason.
	if strings.Contains(body, `viewBox="`+geo.ViewBox+`"`) {
		t.Error("the world map is rendered with no GeoIP database configured; a " +
			"world uniformly coloured \"unknown\" is a picture of nothing that " +
			"looks like a picture of something")
	}
	if strings.Contains(body, `fill-rule="`+geo.FillRule+`"`) {
		t.Error("the map's country group is rendered with no GeoIP database configured")
	}
}

// TestTheMapAndTheRankedListUseOneSentence. Two views of the same fact must not
// be able to disagree about whether this instance can resolve a country, and
// the way that goes wrong is somebody editing one string.
func TestTheMapAndTheRankedListUseOneSentence(t *testing.T) {
	m := Choropleth(nil, "clicks", "", false)
	if m.Unavailable != GeoUnavailable {
		t.Errorf("the map says %q; the constant is %q", m.Unavailable, GeoUnavailable)
	}
	if !strings.Contains(GeoUnavailable, "no GeoIP database is configured") {
		t.Errorf("the shared sentence no longer names the cause: %q", GeoUnavailable)
	}
}

// TestTheVisitorLayerRepeatsTheCaveatVerbatim.
//
// Shading a map by unique visitors without the sentence that says they are
// privacy-preserving estimates at daily resolution would launder an estimate
// into a fact, and a map is far more persuasive than a table. Verbatim, not
// paraphrased: a second wording is a second claim, and only one of them is the
// one the API returns.
func TestTheVisitorLayerRepeatsTheCaveatVerbatim(t *testing.T) {
	const caveat = "Unique visitors are privacy-preserving estimates at daily resolution."

	m := Choropleth(map[string]int64{"US": 5}, "visitors", caveat, true)
	if m.Caveat != caveat {
		t.Errorf("the visitors layer carries %q, want the caveat verbatim: %q", m.Caveat, caveat)
	}

	body := renderLinkDetail(t, func(d map[string]any) { d["Map"] = m })
	if !strings.Contains(body, caveat) {
		t.Error("the rendered visitors map does not carry the caveat")
	}

	// And the clicks layer does not, because clicks are counted rather than
	// estimated and attaching the sentence to them would make it noise.
	clicks := Choropleth(map[string]int64{"US": 5}, "clicks", caveat, true)
	if clicks.Caveat != "" {
		t.Errorf("the clicks layer carries a visitor caveat: %q", clicks.Caveat)
	}
}

// TestTheRankedListStaysOneClickFromTheMap. The map answers "where from"; only
// the list answers "exactly how many", which is the whole reason a five-band
// scale is acceptable at all.
func TestTheRankedListStaysOneClickFromTheMap(t *testing.T) {
	body := renderLinkDetail(t, nil)
	if !strings.Contains(body, `id="countries"`) {
		t.Error("the ranked country list has no anchor, so nothing can link to it")
	}
	if !strings.Contains(body, `#countries`) {
		t.Error("the map does not link to the ranked list")
	}
}

// TestTheMapRendersEveryCountryOnce, and carries the exact figure the shading
// cannot express.
func TestTheMapRendersEveryCountryOnce(t *testing.T) {
	m := Choropleth(map[string]int64{"US": 24, "GB": 10, "ZA": 4, "HK": 2}, "clicks", "", true)

	if m.Countries != 3 {
		t.Errorf("Countries = %d, want 3: HK has traffic but no shape on the 110m map", m.Countries)
	}
	if len(m.Unmapped) != 1 || m.Unmapped[0] != "HK" {
		t.Errorf("Unmapped = %v, want [HK]", m.Unmapped)
	}
	if m.Max != 24 {
		t.Errorf("Max = %d, want 24", m.Max)
	}
	if len(m.Legend) != 5 {
		t.Errorf("the legend has %d bands, want 5", len(m.Legend))
	}

	var withData int
	for _, s := range m.Shapes {
		if s.Class != "fill-sunken" {
			withData++
		}
		if s.Title == "" {
			t.Fatal("a shape has no title; the exact figure is what keeps a five-band scale honest")
		}
	}
	if withData != 3 {
		t.Errorf("%d shapes are shaded, want 3", withData)
	}

	body := renderLinkDetail(t, func(d map[string]any) { d["Map"] = m })
	if !strings.Contains(body, "United States of America — 24 clicks (60%)") {
		t.Error("the rendered map does not carry the exact per-country figure")
	}
	if !strings.Contains(body, "HK") {
		t.Error("the rendered map does not name the territory it counted but could not draw")
	}
}

// TestTheDonutClosesOnASingleValue. An SVG arc whose start and end points
// coincide draws nothing, so a link whose traffic is entirely one browser would
// render an empty ring — the one case where the chart has the most to say.
func TestTheDonutClosesOnASingleValue(t *testing.T) {
	d := DonutChart([]DimensionSlice{{Name: "Chrome", Count: 40}}, 40, 100)
	if d.Empty {
		t.Fatal("a single value with a matching total renders nothing")
	}
	if len(d.Segments) != 1 {
		t.Fatalf("got %d segments, want 1", len(d.Segments))
	}
	if n := strings.Count(d.Segments[0].Path, "M"); n != 2 {
		t.Errorf("the full-circle segment has %d subpaths, want 2: a 360° arc has "+
			"coincident endpoints and draws nothing", n)
	}
	if d.Segments[0].Share != 100 {
		t.Errorf("share = %d, want 100", d.Segments[0].Share)
	}
}

// TestTheDonutAccountsForEverythingItDoesNotName. Five slices plus a remainder,
// so the ring always closes and always sums to the total it is showing.
func TestTheDonutAccountsForEverythingItDoesNotName(t *testing.T) {
	items := []DimensionSlice{
		{Name: "a", Count: 30}, {Name: "b", Count: 20}, {Name: "c", Count: 15},
		{Name: "d", Count: 10}, {Name: "e", Count: 5}, {Name: "f", Count: 4},
		{Name: "g", Count: 3},
	}
	d := DonutChart(items, 100, 100)

	if len(d.Segments) != 6 {
		t.Fatalf("got %d segments, want 5 named plus one remainder", len(d.Segments))
	}
	last := d.Segments[len(d.Segments)-1]
	if last.Label != "other" {
		t.Errorf("the last segment is %q, want \"other\"", last.Label)
	}
	if last.Value != 20 {
		t.Errorf("the remainder is %d, want 20 (100 total less the 80 named)", last.Value)
	}

	var sum int64
	for _, s := range d.Segments {
		sum += s.Value
	}
	if sum != 100 {
		t.Errorf("the segments sum to %d, want the total 100: a ring that does not "+
			"close is a share chart with a missing share", sum)
	}
}

func TestTheDonutSaysNothingRatherThanDrawingNothing(t *testing.T) {
	if !DonutChart(nil, 40, 100).Empty {
		t.Error("a breakdown with no values does not report itself empty")
	}
	if !DonutChart([]DimensionSlice{{Name: "x", Count: 5}}, 0, 100).Empty {
		t.Error("a zero total does not report itself empty")
	}
}
