// Package geo holds the world map as SVG path data, and lays a choropleth out
// over it.
//
// Stdlib-only, like the rest of `ui`, and deliberately dumb: the shapes arrived
// here as generated Go source (see mapgen), so nothing at request time parses
// TopoJSON, projects a coordinate or reaches a CDN. Rendering a map is a range
// over a slice of strings.
//
// The paths are in countries_gen.go. Regenerate with `make mapgen`; on an
// unchanged tree that must produce no diff.
package geo

import "sort"

// Country is one shape on the map.
type Country struct {
	// Code is the ISO 3166-1 alpha-2 code, which is what a GeoIP database
	// answers with and therefore what click_events.country holds.
	Code string
	// Name is world-atlas's own label, kept for the shape's accessible name.
	// It is Natural Earth's spelling — "Dem. Rep. Congo", "eSwatini" — rather
	// than a normalized one, because inventing a second set of names would
	// make the map and its source disagree about what a country is called.
	Name string
	// Path is the `d` attribute: one path per country, several rings inside it.
	Path string
}

// FillRule is the fill rule every generated path must be rendered with.
//
// evenodd, not the SVG default. A country whose geometry contains a hole —
// South Africa contains Lesotho — is one path with two rings, and under the
// default `nonzero` rule the hole fills in solid unless the two rings wind in
// opposite directions. mapgen emits rings in whatever order arc traversal
// produces them, so rather than depend on winding surviving that, the fill rule
// is chosen so winding cannot matter.
const FillRule = "evenodd"

// Countries returns every shape on the map, in code order.
func Countries() []Country { return countries }

// Shape is one country ready to render: its path, and which shading step it
// falls in.
type Shape struct {
	Country
	// Step is 0 for a country with no clicks and 1..Steps otherwise. Zero is a
	// distinct state rather than the bottom of the scale, because "nobody came
	// from here" and "one person came from here" are different answers and a
	// scale that renders them alike is a scale that lies about its own bottom.
	Step int
	// Value is the raw figure behind the shading, for the shape's title.
	Value int64
	// Share is Value as a percentage of the total, rounded to one place.
	Share float64
}

// Steps is how many shaded bands the scale has, above the no-data state.
//
// Five. Fewer and a distribution with one dominant country and a long tail —
// which is every link's country breakdown — collapses into two colours; more
// and adjacent bands stop being distinguishable at the size a country is drawn
// on a 1000-unit-wide map.
const Steps = 5

// Choropleth lays a per-country metric out over the map.
//
// Banding is by **share of the largest value**, not by rank and not by share of
// the total. Rank would give the fifth-busiest country the same colour whether
// it sent half the traffic or four clicks. Share of the total would put every
// country in band one the moment traffic is spread across forty of them, which
// is what a working link looks like.
//
// The bands are linear in that share: a country at 80% of the leader is in the
// top band, one at 5% is in the bottom. A nonzero value never lands in step 0,
// for the same reason a nonzero bar is never zero pixels tall.
func Choropleth(values map[string]int64, total int64) []Shape {
	var maxV int64
	for _, v := range values {
		if v > maxV {
			maxV = v
		}
	}

	out := make([]Shape, 0, len(countries))
	for _, c := range countries {
		s := Shape{Country: c, Value: values[c.Code]}
		if s.Value > 0 && maxV > 0 {
			s.Step = int((s.Value*int64(Steps) + maxV - 1) / maxV)
			if s.Step < 1 {
				s.Step = 1
			}
			if s.Step > Steps {
				s.Step = Steps
			}
		}
		if total > 0 && s.Value > 0 {
			s.Share = float64(int64(float64(s.Value)*1000/float64(total)+0.5)) / 10
		}
		out = append(out, s)
	}
	return out
}

// Unmapped reports the codes in values that no shape on the map carries.
//
// A GeoIP database resolves addresses to territories Natural Earth's 110m
// countries do not draw — Hong Kong, Monaco, every small island — and a click
// from one of them would otherwise be counted in the total, shade nothing, and
// leave the map quietly disagreeing with the ranked list beside it. The caller
// says so instead. Sorted, because it is rendered.
func Unmapped(values map[string]int64) []string {
	known := make(map[string]bool, len(countries))
	for _, c := range countries {
		known[c.Code] = true
	}
	var out []string
	for code, v := range values {
		if v > 0 && !known[code] {
			out = append(out, code)
		}
	}
	sort.Strings(out)
	return out
}
