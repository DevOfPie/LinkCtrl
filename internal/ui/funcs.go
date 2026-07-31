package ui

import (
	"fmt"
	"html/template"
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
	MaxY  int64
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
	Y     int
	Label string
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
	c.Ticks = []Tick{
		{Y: 0, Label: fmtInt(top)},
		{Y: c.PlotH / 2, Label: fmtInt(top / 2)},
	}
	c.First = dayShort(points[0].Day)
	c.Last = dayShort(points[len(points)-1].Day)
	return c
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
	}
}
