package ui

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"
)

// DayCount is one day of a click series as the chart helpers consume it.
//
// A local type rather than analytics.DayPoint, because this package depends on
// nothing outside the standard library. Handlers convert; the conversion is
// three assignments and it keeps a template change from ever pulling a service
// package into the UI.
type DayCount struct {
	Day      string // 2006-01-02
	Clicks   int64
	Visitors int64
	Bots     int64
}

// Chart is a bar chart laid out in Go rather than in the browser.
//
// The dashboard's charts are server-rendered SVG. A charting library would be
// the only piece of custom JavaScript in the product, for four rectangles and
// an axis — and the CSP disallows inline styles, which most of them generate.
// Computing integer geometry here keeps the template a dumb loop.
type Chart struct {
	W, H  int
	PlotH int
	// MaxY is the top of the axis: a rounded ceiling nobody observed, which is
	// what makes the gridlines readable. It is **not** a reading, and labelling
	// it as one is the defect F164 records — a 30-day maximum of 2,351 rendered
	// as "peak 5,000/day", 113% high, with nothing beside it to contradict the
	// number.
	MaxY int64
	// Peak is the largest reading in the series, which is the figure a reader
	// takes "peak" to mean. Zero for an empty series, like every other field
	// here.
	Peak  int64
	Bars  []Bar
	Ticks []Tick
	First string // label of the first day, for the x axis
	Last  string
}

type Bar struct {
	X, Y, W, H int
	Day        string
	Clicks     int64
	Visitors   int64
	Bots       int64
}

type Tick struct {
	// Y is the gridline, in the plot's own coordinates.
	Y int
	// LabelY is the baseline the label sits on, in the outer viewport's — see
	// series_chart.html for why the two are the same number of units and only
	// the labels escape the horizontal stretch. Text hangs above its baseline,
	// so the top gridline's label drops below the line and every other one sits
	// just above it; a label drawn at Y=0 renders outside the box entirely.
	LabelY int
	Label  string
	// BoxY, BoxW and BoxH are the chip the label is drawn on.
	//
	// Not decoration. A gridline label sits at the left of the plot and the
	// leftmost bar is often the tallest, so the figure lands on `fill-accent-
	// hover` — where `subtle` ink measures **1.66:1** in the light theme and
	// 3.08:1 in the dark, against a package that has never shipped text under
	// 4.5:1. The chip is `sunken`, which puts `muted` at 6.92:1 and 12.61:1.
	// All four are WCAG 2.x ratios computed from input.css's own hex values at
	// M58 — they read 1.82, 3.15, 6.84 and 12.0 until then, and every
	// conclusion drawn from them is unchanged.
	BoxY, BoxW, BoxH int
}

// The geometry of a tick label, in user units — CSS pixels, because the labels
// are drawn in the chart's unstretched outer viewport.
//
// text-xs is 12px. So 11 clears the top edge with a pixel to spare, 4 keeps a
// label clear of the line it belongs to, and the chip is a pixel taller than the
// text with its top ten above the baseline.
//
// tickCharW is a digit's advance at 12px in the default sans stack, rounded up:
// a comma is narrower, so a chip is at worst a few pixels wide of its figure,
// which reads as padding rather than as a mistake. The alternative is measuring
// a font in Go.
const (
	tickLabelDrop = 11
	tickLabelLift = 4
	tickChipUp    = 10
	tickChipH     = 13
	tickChipPad   = 6
	tickCharW     = 7
)

// tick assembles one gridline and the label that names it.
func tick(y int, baseline int, label string) Tick {
	return Tick{
		Y: y, LabelY: baseline, Label: label,
		BoxY: baseline - tickChipUp,
		BoxW: tickChipPad + tickCharW*len(label),
		BoxH: tickChipH,
	}
}

// BarChart lays out a day series in a w×h viewBox.
//
// Exported for its geometry tests; templates reach it through the func map.
func BarChart(points []DayCount, w, h int) Chart {
	c := Chart{W: w, H: h, PlotH: h - 16}
	if len(points) == 0 {
		return c
	}

	var maxV int64
	for _, p := range points {
		if p.Clicks > maxV {
			maxV = p.Clicks
		}
	}
	// An all-zero week still gets an axis, so the chart reads as "no clicks"
	// rather than looking broken.
	top := niceCeil(max(maxV, 1))

	gap := 2
	n := len(points)
	barW := (c.W - gap*(n-1)) / n
	if barW < 1 {
		barW, gap = 1, 0
	}

	for i, p := range points {
		bh := int(int64(c.PlotH) * p.Clicks / top)
		if p.Clicks > 0 && bh == 0 {
			// A day with traffic must be visible, even at 1 click against a
			// 10,000 axis. Invisible data reads as missing data.
			bh = 1
		}
		c.Bars = append(c.Bars, Bar{
			X: i * (barW + gap), Y: c.PlotH - bh, W: barW, H: bh,
			Day: p.Day, Clicks: p.Clicks, Visitors: p.Visitors, Bots: p.Bots,
		})
	}

	c.MaxY = top
	// The peak is the largest reading, not the axis it is drawn against. The
	// rounding above is correct and stays — an axis of 5,000 is readable where
	// one of 2,351 is not — but the rounded ceiling is not an observation, and
	// the footer that called it one was the only number the chart carried.
	c.Peak = maxV
	// The top and the baseline, and the last of them is what the 16 units above
	// the bottom edge were held back for. `PlotH = h - 16` has reserved that band
	// since the chart was written and nothing was ever drawn in it, because no
	// label was drawn at all — so the baseline label goes there, clear of every
	// bar, and the reservation stops being a tenth of the box spent on nothing.
	//
	// The middle one is drawn **only when halving the axis is exact**. `fmtInt`
	// takes an int64 and `top/2` is integer division, so an odd ceiling labels
	// the middle gridline with a figure it is not drawn at — which is F164's
	// defect, an unobserved number presented as a reading, at a third of the size
	// and on the line a reader measures every bar against.
	//
	// niceCeil's candidates are 1, 2, 5 and the powers of ten between, so odd is
	// exactly two cases and both are at the small end: top=1, the all-zero week
	// designed for above, where the middle line would read "0" beside a baseline
	// already reading "0"; and top=5, where a 2.5 gridline would read "2". Each
	// loses one gridline of three on an axis with at most five to read off, which
	// is cheaper than a line whose label is wrong — and cheaper than teaching this
	// chart to render a decimal for two values of `top`.
	c.Ticks = []Tick{tick(0, tickLabelDrop, fmtInt(top))}
	if top%2 == 0 {
		c.Ticks = append(c.Ticks,
			tick(c.PlotH/2, c.PlotH/2-tickLabelLift, fmtInt(top/2)))
	}
	c.Ticks = append(c.Ticks, tick(c.PlotH, c.PlotH+tickLabelDrop, "0"))
	c.First = dayShort(points[0].Day)
	c.Last = dayShort(points[len(points)-1].Day)
	return c
}

// Donut is a ring chart laid out in Go (M37).
//
// The other half of "richer charts for the other dimensions". A ranked list
// answers "how many from Chrome"; it does not answer "is this link's traffic
// one browser or five", which is the question a share chart exists for and the
// one a column of numbers is worst at.
//
// A ring rather than a pie because the hole is where the total goes, and a
// total in the middle is what stops somebody reading a 60% slice as a big
// number when it is 60% of nine clicks.
type Donut struct {
	// Size is the square viewBox side. Geometry is absolute inside it.
	Size     int
	Segments []DonutSegment
	Total    int64
	// Empty is true when there is nothing to draw, so the template can say so
	// rather than render a ring of nothing.
	Empty bool
}

// DonutSegment is one slice, already turned into a path.
type DonutSegment struct {
	Path  string
	Class string
	Label string
	Value int64
	Share int
}

// donutSlices is how many values get their own segment before the rest are
// gathered into one.
//
// Five, because the shading ramp has five bands and because a ring cut into
// twelve pieces is a ring nobody can read. The remainder is not dropped — it
// becomes an "other" segment, so the ring always closes and always sums to the
// total it is showing.
const donutSlices = 5

// DonutChart lays a breakdown out as a ring.
//
// Segments are ordered largest first and shaded darkest first, which makes the
// colour encode rank rather than identity. That is deliberate: a categorical
// palette would need a token per category and would put "Chrome" and "Safari"
// in colours that mean nothing, whereas the ramp says "this one is bigger" in
// the same visual language the map beside it uses.
func DonutChart(items []DimensionSlice, total int64, size int) Donut {
	d := Donut{Size: size, Total: total}
	if total <= 0 || len(items) == 0 {
		d.Empty = true
		return d
	}

	type slice struct {
		label string
		value int64
	}
	var slices []slice
	var named int64
	for i, it := range items {
		if i >= donutSlices {
			break
		}
		if it.Count <= 0 {
			continue
		}
		label := it.Name
		if label == "" {
			label = "unknown"
		}
		slices = append(slices, slice{label: label, value: it.Count})
		named += it.Count
	}
	if rest := total - named; rest > 0 {
		slices = append(slices, slice{label: "other", value: rest})
	}
	if len(slices) == 0 {
		d.Empty = true
		return d
	}

	cx, cy := float64(size)/2, float64(size)/2
	outer := float64(size)/2 - 1
	inner := outer * 0.58

	var at float64
	for i, s := range slices {
		sweep := 2 * math.Pi * float64(s.value) / float64(total)
		class := choroFill(donutSlices - i)
		if s.label == "other" && i >= donutSlices {
			class = choroFill(0)
		}
		d.Segments = append(d.Segments, DonutSegment{
			Path:  ringPath(cx, cy, outer, inner, at, at+sweep),
			Class: class,
			Label: s.label,
			Value: s.value,
			Share: pct(s.value, total),
		})
		at += sweep
	}
	return d
}

// DimensionSlice is the shape DonutChart consumes.
//
// A local type for the same reason DayCount is one: this package depends on
// nothing outside the standard library, and a handler converting two fields is
// cheaper than the UI importing the analytics package.
type DimensionSlice struct {
	Name  string
	Count int64
}

// ringPath draws one annulus segment, clockwise from twelve o'clock.
//
// A segment spanning the whole circle is split in two, because an SVG arc whose
// start and end points coincide draws nothing at all — a link whose traffic is
// 100% one browser would otherwise render an empty ring, which is the one case
// where the chart has the most to say.
func ringPath(cx, cy, outer, inner, from, to float64) string {
	if to-from >= 2*math.Pi-1e-9 {
		mid := from + math.Pi
		return ringPath(cx, cy, outer, inner, from, mid) +
			ringPath(cx, cy, outer, inner, mid, to)
	}
	x0o, y0o := onCircle(cx, cy, outer, from)
	x1o, y1o := onCircle(cx, cy, outer, to)
	x0i, y0i := onCircle(cx, cy, inner, from)
	x1i, y1i := onCircle(cx, cy, inner, to)
	large := "0"
	if to-from > math.Pi {
		large = "1"
	}
	return fmt.Sprintf("M%s,%s A%s,%s 0 %s,1 %s,%s L%s,%s A%s,%s 0 %s,0 %s,%s Z",
		coord(x0o), coord(y0o), coord(outer), coord(outer), large, coord(x1o), coord(y1o),
		coord(x1i), coord(y1i), coord(inner), coord(inner), large, coord(x0i), coord(y0i))
}

func onCircle(cx, cy, r, angle float64) (float64, float64) {
	return cx + r*math.Sin(angle), cy - r*math.Cos(angle)
}

// coord formats to two places, which is finer than a pixel at the sizes these
// charts are drawn and keeps the markup out of float noise.
func coord(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "-0" || s == "" {
		return "0"
	}
	return s
}

// niceCeil rounds up to 1, 2 or 5 times a power of ten, which is what makes an
// axis label readable: a maximum of 8,347 becomes an axis of 10,000.
func niceCeil(v int64) int64 {
	if v <= 1 {
		return 1
	}
	mag := int64(1)
	for mag*10 <= v {
		mag *= 10
	}
	for _, m := range []int64{1, 2, 5, 10} {
		if m*mag >= v {
			return m * mag
		}
	}
	return 10 * mag // unreachable; the loop's last candidate always satisfies
}

// fmtInt renders 1234567 as "1,234,567".
func fmtInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// pct returns part as a whole percentage of total, clamped to [0,100].
func pct(part, total int64) int {
	if total <= 0 || part <= 0 {
		return 0
	}
	p := int(part * 100 / total)
	if p > 100 {
		p = 100
	}
	// A nonzero share always shows at least a sliver, for the same reason a
	// nonzero bar is at least a pixel.
	if p == 0 {
		p = 1
	}
	return p
}

// asTime accepts the time.Time and *time.Time that domain types carry.
func asTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, !t.IsZero()
	case *time.Time:
		if t == nil {
			return time.Time{}, false
		}
		return *t, !t.IsZero()
	default:
		return time.Time{}, false
	}
}

// relTime renders "5m ago" for recent times and a date for old ones, because
// "417 days ago" is arithmetic homework rather than an answer.
func relTime(v any) string {
	t, ok := asTime(v)
	if !ok {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2, 2006")
	}
}

func fmtDate(v any) string {
	t, ok := asTime(v)
	if !ok {
		return "—"
	}
	return t.Format("Jan 2, 2006")
}

func fmtDateTime(v any) string {
	t, ok := asTime(v)
	if !ok {
		return "—"
	}
	return t.UTC().Format("Jan 2, 2006 15:04 UTC")
}

// dtLocal formats for <input type="datetime-local">, which takes exactly
// "2006-01-02T15:04" and nothing else.
func dtLocal(v any) string {
	t, ok := asTime(v)
	if !ok {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04")
}

// dayShort turns "2026-07-30" into "Jul 30".
func dayShort(day string) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	return t.Format("Jan 2")
}

// truncate cuts at a rune boundary, because slicing bytes mid-UTF-8 renders as
// replacement characters.
func truncate(s string, n int) string {
	if n <= 1 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

// statusBadge maps a link status to its badge classes.
//
// The class strings live in Go, so input.css lists this file as a Tailwind
// @source — utilities that never appear in a template would otherwise be
// missing from the generated stylesheet.
func statusBadge(status any) string {
	base := "inline-flex items-center rounded-md px-2 py-1 text-xs font-medium ring-1 ring-inset "
	switch fmt.Sprint(status) {
	case "active":
		return base + "bg-ok-soft text-ok-ink ring-ok-line"
	case "archived":
		return base + "bg-sunken text-muted ring-line-strong"
	case "expired":
		return base + "bg-warn-soft text-warn-ink ring-warn-line"
	case "disabled":
		return base + "bg-danger-soft text-danger-ink ring-danger-line"
	default:
		return base + "bg-sunken text-muted ring-line-strong"
	}
}

// dict builds a map for passing several values to a partial, since a template
// invocation takes exactly one argument.
func dict(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		k, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", pairs[i])
		}
		m[k] = pairs[i+1]
	}
	return m, nil
}

func (r *Renderer) funcs() template.FuncMap {
	return template.FuncMap{
		"asset":       r.AssetURL,
		"barChart":    BarChart,
		"donutChart":  DonutChart,
		"fmtInt":      fmtInt,
		"pct":         pct,
		"reltime":     relTime,
		"date":        fmtDate,
		"datetime":    fmtDateTime,
		"dtlocal":     dtLocal,
		"dayShort":    dayShort,
		"truncate":    truncate,
		"statusBadge": statusBadge,
		"dict":        dict,
		"add":         add,
		"mul":         mul,
		"rangePct":    rangePct,
	}
}

// add is integer addition, for turning a range index into a human ordinal.
//
// A template function rather than a field on every row, because "this is the
// third arm" is a fact about the position in the list being rendered and
// carrying it in the data would be a second copy of the loop's own counter.
func add(a, b int) int { return a + b }

// mul is integer multiplication, for stepping a legend's swatches across a
// fixed-width strip. Same justification as add: it is arithmetic about the
// loop's own index, and carrying an x on every band would be a second copy of
// the counter.
func mul(a, b int) int { return a * b }

// rangePct is where a value sits between two bounds, as a percentage, for a
// mark drawn along a track. It answers the size slider's detents (M50.8, D198).
//
// **A function rather than a field, for the reason add and mul are.** The
// position is arithmetic about the drawing, not a fact about the data: a
// `QRSizeMarks` beside `QRSizeStops` would be a second copy of the same list,
// and the two could then disagree about which sizes a code offers — which is
// exactly what the marks exist to say truthfully.
//
// **A string rather than a float, because the caller is an SVG attribute.** Two
// decimals: the track is a few hundred pixels wide, so whole percents would put
// a mark two of them away from the value it names, and more than two decimals is
// below a device pixel on any track this product draws.
//
// A degenerate range gives "0" and an out-of-range value clamps, so a mark is
// always somewhere on the strip. Neither can happen from qrSizeStops, which
// filters to the code's own floor and to qr.MaxSize; the clamp is what keeps a
// future caller's bad bounds a visible mark rather than an invisible one.
func rangePct(v, lo, hi int) string {
	if hi <= lo || v <= lo {
		return "0"
	}
	if v >= hi {
		return "100"
	}
	return strconv.FormatFloat(float64(v-lo)*100/float64(hi-lo), 'f', 2, 64)
}
