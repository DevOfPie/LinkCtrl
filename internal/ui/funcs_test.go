package ui

import (
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The bar chart's axis, and the two things it says about itself (F164).
//
// TestBarChartGeometry in ui_test.go covers the rectangles. What is here is the
// text: the figure the footer calls the peak, and the labels the gridlines were
// given and never drew.

// TestTheChartsPeakIsAReadingAndNotItsAxis is F164.
//
// The dashboard read *"peak 5,000/day"* over a 30-day series whose true maximum
// was 2,351 — a 113% overstatement, on the product's front page, and the only
// number the chart carried. `niceCeil` rounding to 1, 2 or 5 × 10ᵏ is what makes
// an axis readable and is *not* the defect; presenting the rounded ceiling as an
// observation is. So both halves are asserted: the axis still rounds, and the
// sentence beside it no longer quotes the rounding.
//
// The figures are the demo's own, measured 2026-08-07 against
// `workspace_click_daily` for the default workspace.
func TestTheChartsPeakIsAReadingAndNotItsAxis(t *testing.T) {
	c := BarChart([]DayCount{
		{Day: "2026-07-22", Clicks: 1400},
		{Day: "2026-07-23", Clicks: 2351},
		{Day: "2026-07-24", Clicks: 900},
	}, 720, 160)

	if c.Peak != 2351 {
		t.Errorf("Peak = %d, want the largest reading 2351", c.Peak)
	}
	if c.MaxY != 5000 {
		t.Errorf("MaxY = %d, want the nice ceiling 5000: the axis still rounds, "+
			"because 2,351 is not a readable top gridline", c.MaxY)
	}

	// And on the page, because a truthful field nothing renders is the state
	// this finding was already in — Tick.Label had been populated for as long.
	// The fixture's series peaks at 30 against an axis of 50.
	body := renderPage(t, "dashboard", nil)
	if !strings.Contains(body, "peak 30/day") {
		t.Error("the dashboard does not report the largest reading in its series " +
			"as the peak")
	}
	if strings.Contains(body, "peak 50/day") {
		t.Error("the dashboard reports its rounded axis ceiling as the peak. " +
			"Nobody reading it has any way to discover the busiest day was 30")
	}
}

// svgText captures the content of a <text> element.
var svgText = regexp.MustCompile(`<text\b[^>]*>([^<]*)</text>`)

// TestTheChartsGridlinesCarryTheirLabels is F164's second half.
//
// `Tick.Label` was populated in funcs.go and rendered nowhere: series_chart.html
// emitted `<line>` and nothing else, so the chart drew two unlabelled dashes
// across itself and the 16 units `PlotH = h - 16` holds back went to nothing.
// An unlabelled gridline is a rule the reader has to guess the value of.
func TestTheChartsGridlinesCarryTheirLabels(t *testing.T) {
	d, ok := pageData(t)["dashboard"].(map[string]any)
	if !ok {
		t.Fatal("the dashboard page data is not a map")
	}
	points, ok := d["Series"].([]DayCount)
	if !ok {
		t.Fatal("the dashboard fixture's Series is not a []DayCount")
	}
	c := BarChart(points, chartW, chartH)
	if len(c.Ticks) == 0 {
		t.Fatal("the chart computes no gridlines at all")
	}

	body := renderPage(t, "dashboard", nil)
	var drawn []string
	for _, m := range svgText.FindAllStringSubmatch(body, -1) {
		drawn = append(drawn, strings.TrimSpace(m[1]))
	}

	// Each on its chip. The leftmost bar is often the tallest, so a bare label
	// lands on `fill-accent-hover` at 1.82:1 — and a label nobody can read is the
	// state this test was written to leave.
	if n := strings.Count(body, `class="fill-sunken"`); n < len(c.Ticks) {
		t.Errorf("%d of the %d axis labels are drawn straight onto the plot. "+
			"Subtle ink on the bar fill measures 1.82:1 in the light theme",
			len(c.Ticks)-n, len(c.Ticks))
	}

	for _, tick := range c.Ticks {
		var found bool
		for _, got := range drawn {
			if got == tick.Label {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the gridline at y=%d is labelled %q in Go and drawn with no "+
				"label at all; the page's <text> elements are %v",
				tick.Y, tick.Label, drawn)
		}
	}
}

// TestEveryGridlineIsLabelledWithTheFigureItIsDrawnAt is F164's class, met by
// the labels that closed it.
//
// A gridline label is read the way `MaxY` was: as a figure somebody can measure
// a bar against. `fmtInt` takes an int64 and the middle gridline is at `top/2`,
// so an odd ceiling rounds the label down and the line then names a value it is
// not drawn at. `niceCeil` yields exactly two odd ceilings — 1 and 5 — and both
// are reachable from an ordinary week: an all-zero one, which `BarChart` gives
// an axis to on purpose, and any week peaking at three to five clicks.
//
// Stated as the property rather than as the two cases, because a third arrives
// the moment `niceCeil`'s candidate set changes: label × PlotH must equal
// top × (PlotH − Y), which is exact integer arithmetic and admits no rounding.
func TestEveryGridlineIsLabelledWithTheFigureItIsDrawnAt(t *testing.T) {
	// One per shape niceCeil produces, ceilings 1 through 200, so both odd cases
	// are in the set beside the even ones that must keep their middle line.
	for _, peak := range []int64{0, 1, 2, 3, 5, 7, 10, 30, 90, 150} {
		c := BarChart([]DayCount{
			{Day: "2026-08-01", Clicks: peak},
			{Day: "2026-08-02", Clicks: 0},
		}, chartW, chartH)

		seen := map[string]int{}
		for _, tk := range c.Ticks {
			seen[tk.Label]++
			n, err := strconv.ParseInt(strings.ReplaceAll(tk.Label, ",", ""), 10, 64)
			if err != nil {
				t.Errorf("peak %d: the gridline at y=%d is labelled %q, which is not a "+
					"figure at all", peak, tk.Y, tk.Label)
				continue
			}
			if n*int64(c.PlotH) != c.MaxY*int64(c.PlotH-tk.Y) {
				t.Errorf("peak %d: the gridline at y=%d of %d is labelled %q against an "+
					"axis of %d, and is drawn at %.1f. A label is read as a figure to "+
					"measure a bar against, which is what F164 was",
					peak, tk.Y, c.PlotH, tk.Label, c.MaxY,
					float64(c.MaxY)*float64(c.PlotH-tk.Y)/float64(c.PlotH))
			}
		}
		for label, n := range seen {
			if n > 1 {
				t.Errorf("peak %d: %d gridlines are labelled %q, so at least one of them "+
					"is drawn somewhere its own label does not name", peak, n, label)
			}
		}
	}
}

// The viewBox both pages ask the chart for. Named here because the assertion
// below is precisely that this pair and the template's height class agree.
const (
	chartW = 720
	chartH = 160
)

var (
	// barChartCall matches `barChart <series> <w> <h>` in a template.
	barChartCall = regexp.MustCompile(`barChart\s+\S+\s+(\d+)\s+(\d+)`)
	// firstSVG is the outer element of series_chart.html.
	firstSVG = regexp.MustCompile(`<svg\b([^>]*)>`)
)

// TestTheChartsLabelsLineUpWithItsGridlines guards the coupling the labels are
// bought with.
//
// The plot is stretched to the card's width and the labels must not be, so they
// are drawn in the outer viewport — which has no viewBox, making its user unit a
// CSS pixel. `Tick.LabelY` is computed against the `h` the caller passes, so the
// labels land on their gridlines only while the height class on that outer
// element states the same number of pixels. Two numbers in two files that have
// to be equal is exactly the kind of thing that stops being equal.
func TestTheChartsLabelsLineUpWithItsGridlines(t *testing.T) {
	b, err := fs.ReadFile(files, "templates/partials/series_chart.html")
	if err != nil {
		t.Fatalf("read the chart partial: %v", err)
	}
	m := firstSVG.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("series_chart.html draws no <svg>")
	}
	if strings.Contains(m[1], "viewBox") {
		t.Errorf("the chart's outer <svg> carries a viewBox:\n  <svg%s>\n\nIts user "+
			"unit is then whatever that box scales to, and the label geometry Go "+
			"computes in pixels lands wherever the container happens to be wide. "+
			"The viewBox belongs on the nested plot, which is the part that is "+
			"meant to stretch.", m[1])
	}
	px, ok := statedHeight(m[1])
	if !ok {
		t.Fatalf("the chart's outer <svg> states no height: <svg%s>", m[1])
	}

	// Every call, over the embedded templates, because a second page asking for
	// a different box would break the labels on that page alone.
	var calls int
	err = fs.WalkDir(files, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		src, err := fs.ReadFile(files, path)
		if err != nil {
			return err
		}
		for _, call := range barChartCall.FindAllStringSubmatch(string(src), -1) {
			calls++
			if call[1] != itoa(chartW) || call[2] != itoa(chartH) {
				t.Errorf("%s asks for a %s×%s chart; the package's pair is %d×%d",
					path, call[1], call[2], chartW, chartH)
			}
			if call[2] != itoa(px) {
				t.Errorf("%s asks for a chart %s units tall and series_chart.html "+
					"renders it %dpx tall. The axis labels are positioned in units "+
					"of the first and drawn in pixels of the second, so they no "+
					"longer sit on the gridlines they label.", path, call[2], px)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("found %d barChart calls, want the dashboard's and the link "+
			"page's; a page drawing a chart this scan cannot see is a page whose "+
			"axis nothing checks", calls)
	}
}
