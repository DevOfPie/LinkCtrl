package ui

import (
	"fmt"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/ui/geo"
)

// WorldMap is a country choropleth laid out in Go, ready for a dumb template
// loop.
//
// Same idiom as BarChart, and for the same three reasons: a charting library
// would be the only custom JavaScript in the product, the CSP disallows the
// inline styles most of them generate, and geometry computed here means the
// template holds no arithmetic. The shapes themselves are generated Go source —
// see internal/ui/geo — so a dashboard request never parses a map file.
type WorldMap struct {
	ViewBox  string
	FillRule string
	Shapes   []MapShape
	Legend   []MapBand

	// Metric is "clicks" or "visitors": which figure the shading is of.
	Metric string
	// MetricLabel names it in the heading.
	MetricLabel string
	// Caveat is the unique-visitor caveat, repeated **verbatim** whenever the
	// shading is of unique visitors and empty otherwise.
	//
	// Verbatim is the requirement, not a style note. Unique visitors are a
	// privacy-preserving estimate at daily resolution; shading a map by one
	// without carrying the sentence that says so would launder an estimate into
	// a fact, and a map is a great deal more persuasive than a table.
	Caveat string

	// Max is the largest per-country figure, which is what the bands are a
	// fraction of. Zero when nothing was resolved.
	Max int64
	// Countries is how many countries have a nonzero figure.
	Countries int
	// Unmapped lists codes with traffic that this map has no shape for.
	Unmapped []string

	// Unavailable is set when no GeoIP database is configured, and carries the
	// same sentence the ranked list has always used. The map is not rendered at
	// all in that state: a world drawn entirely in the no-data colour is a
	// picture of nothing that looks like a picture of something.
	Unavailable string
}

// MapShape is one country as the template needs it.
type MapShape struct {
	Path string
	// Class is the fill utility, already resolved. A template cannot build
	// `fill-choro-{{.Step}}` and have Tailwind find it, so the whole string
	// comes from Go — which is why this file is a @source in input.css, exactly
	// as funcs.go is for the status badges.
	Class string
	// Title is the shape's accessible name and its hover text: the country, and
	// its exact figure. It is what keeps the map honest about a five-band scale.
	Title string
}

// MapBand is one step of the legend.
type MapBand struct {
	Class string
	// Upper is the largest figure that falls in this band.
	Upper int64
}

// GeoUnavailable is the sentence a page shows when no GeoIP database is
// configured.
//
// One constant, used by the map and by the ranked list, because the two views
// must not be able to disagree about whether this instance can resolve a
// country. TestCountryViewsAgreeWhenGeoIPIsAbsent asserts they use it.
const GeoUnavailable = "Geographic data is unavailable: no GeoIP database is configured."

// Choropleth lays a country breakdown out over the world map.
//
// values is per alpha-2 code. available says whether this instance has a GeoIP
// database at all — passed in rather than inferred from an empty map, because a
// link with no clicks yet and an instance that cannot resolve a country are
// different facts and only one of them is worth telling somebody about.
func Choropleth(values map[string]int64, metric, caveat string, available bool) WorldMap {
	m := WorldMap{
		ViewBox:     geo.ViewBox,
		FillRule:    geo.FillRule,
		Metric:      metric,
		MetricLabel: "clicks",
	}
	if metric == "visitors" {
		m.MetricLabel = "unique visitors"
		m.Caveat = caveat
	}
	if !available {
		m.Unavailable = GeoUnavailable
		return m
	}

	var total int64
	for _, v := range values {
		total += v
		if v > m.Max {
			m.Max = v
		}
	}

	for _, s := range geo.Choropleth(values, total) {
		if s.Value > 0 {
			m.Countries++
		}
		m.Shapes = append(m.Shapes, MapShape{
			Path:  s.Path,
			Class: choroFill(s.Step),
			Title: mapTitle(s.Name, s.Value, s.Share, m.MetricLabel),
		})
	}
	m.Unmapped = geo.Unmapped(values)

	if m.Max > 0 {
		for step := 1; step <= geo.Steps; step++ {
			m.Legend = append(m.Legend, MapBand{
				Class: choroFill(step),
				Upper: (m.Max*int64(step) + int64(geo.Steps) - 1) / int64(geo.Steps),
			})
		}
	}
	return m
}

// choroFill maps a shading step to its fill utility.
//
// A switch over literals rather than "fill-choro-" + itoa(step), because
// Tailwind scans for whole class names and a string it cannot see is a utility
// that is not in app.css. Same reason statusBadge writes its classes out.
func choroFill(step int) string {
	switch step {
	case 1:
		return "fill-choro-1"
	case 2:
		return "fill-choro-2"
	case 3:
		return "fill-choro-3"
	case 4:
		return "fill-choro-4"
	case 5:
		return "fill-choro-5"
	default:
		// No data. Not the bottom of the ramp: a country nobody visited and a
		// country one person visited are different answers, and a scale that
		// renders them alike is a scale that lies about its own bottom.
		return "fill-sunken"
	}
}

// mapTitle is the exact figure behind a shape, because the shading is not one.
//
// Five bands cannot express a distribution with one dominant country and a long
// tail, which is what every link's country breakdown looks like. The band says
// roughly where a country sits; this says exactly.
func mapTitle(name string, value int64, share float64, metric string) string {
	if value == 0 {
		return name + " — no " + metric
	}
	return fmt.Sprintf("%s — %s %s (%s%%)", name, fmtInt(value), metric, trimZero(share))
}

// trimZero renders 12.0 as "12" and 12.3 as "12.3".
func trimZero(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	return strings.TrimSuffix(s, ".0")
}
