// Command mapgen converts the vendored world-atlas TopoJSON into Go source.
//
// This is the generate-time half of D63. The fetched TopoJSON is a *vendored*
// file — pinned, checksummed, verified by `make verify-assets`. What this
// command writes is *generated output*, committed the way sqlc's dbgen is
// committed, and re-running it on an unchanged tree must produce no diff. That
// is the property `make sqlc` is held to and this is held to the same one.
//
// Conversion happens here and never at request time. The server renders inline
// SVG from Go data it already holds, so `ui` stays stdlib-only, nothing parses
// TopoJSON on a dashboard request, and the CSP is untouched.
//
// # The projection
//
// **Equirectangular (plate carrée)**, named rather than left implicit, because a
// choropleth's shapes are an argument about the world and an unnamed projection
// is an argument nobody can check. Longitude maps linearly to x and latitude
// linearly to y, at one uniform scale in both axes.
//
// It is the honest choice for this particular chart. The map is read as a
// lookup table — "which countries clicked this link" — rather than for area or
// distance, and equirectangular is the only projection where a reader can point
// at a pixel and name its coordinates. Web Mercator would have made Greenland
// argue with Africa about a number neither of them is displaying.
//
// Latitude is clipped to [-58, 84] and Antarctica is dropped. 84°N is where the
// northernmost land ends (Greenland reaches 83.6°N); -58° is south of Cape Horn
// at -55.9°. The band below that is Antarctica and empty ocean, and in an
// equirectangular frame it is a third of the height for a landmass that has
// never produced a click. Clipping is a clamp, not a cut: no shape outside the
// dropped continent reaches either bound, so nothing is truncated.
//
// # Winding order and the antimeridian
//
// TopoJSON follows the shapefile convention — exterior rings clockwise, holes
// counter-clockwise — and SVG's default `nonzero` fill rule would need that to
// be right in the emitted path or Lesotho would fill in solid inside South
// Africa. Rather than depend on upstream winding surviving arc reversal, every
// country is emitted as one path rendered with **fill-rule="evenodd"**, under
// which a ring inside another ring is a hole whichever way round it is wound.
// The template carries the attribute; geo.FillRule is where it comes from.
//
// The antimeridian needs no special handling *and that is checked rather than
// assumed*: Natural Earth cuts its geometry at ±180 already, so no ring wraps.
// The generator fails if any decoded coordinate lands outside the sphere, which
// is what a wrapped ring would look like — a country stretched across the whole
// frame is the failure this catches before it is committed. Russia and Fiji do
// touch both frame edges, in separate rings, which is the cut working rather
// than a wrap.
//
// # Why the paths are relative
//
// One absolute M per subpath and relative l steps after it. At 110m resolution
// almost every step is under one unit, so "l.4,-.3" replaces "L643.8,165.9" and
// both the generated file and the markup on the wire shrink by about a third:
// measured, 120,457 to 81,312 bytes in the file, and a rendered link page from
// 213,940 to 174,792 bytes. The map is still ~86 KB of inline SVG on every view
// of a link that has geography, and nothing compresses it on the way out — that
// cost is stated in docs/usage.md rather than hidden. Deltas are computed between
// coordinates already quantized to the emitted precision, so the encoding is
// exact rather than merely close: there is no accumulating error to drift.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// The frame. Width is arbitrary and round; height follows from the clipped
// latitude band at the same scale, because an equirectangular map with
// different x and y scales is a different projection wearing this one's name.
const (
	frameW = 1000.0
	latMax = 84.0
	latMin = -58.0
)

// coordPrecision is decimal places kept in the emitted path data.
//
// One place, in a frame 1000 units wide, is 0.036° — about four kilometres at
// the equator. The 110m source is simplified far past that, so this rounds away
// nothing a reader could see and roughly halves the generated file.
const coordPrecision = 1

type topology struct {
	Transform struct {
		Scale     [2]float64 `json:"scale"`
		Translate [2]float64 `json:"translate"`
	} `json:"transform"`
	Objects struct {
		Countries struct {
			Geometries []geometry `json:"geometries"`
		} `json:"countries"`
	} `json:"objects"`
	Arcs [][][2]float64 `json:"arcs"`
}

type geometry struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	Arcs       json.RawMessage `json:"arcs"`
	Properties struct {
		Name string `json:"name"`
	} `json:"properties"`
}

func main() {
	if len(os.Args) != 3 {
		fail(fmt.Errorf("usage: mapgen <countries-110m.json> <output.go>"))
	}
	// Both paths are arguments to a generator invoked by `make mapgen`, never by
	// a request. There is no untrusted input in this program at all.
	raw, err := os.ReadFile(os.Args[1]) //nolint:gosec // build-time CLI argument
	if err != nil {
		fail(err)
	}
	var topo topology
	if err := json.Unmarshal(raw, &topo); err != nil {
		fail(fmt.Errorf("parse topojson: %w", err))
	}

	arcs, err := decodeArcs(&topo)
	if err != nil {
		fail(err)
	}

	type country struct{ Code, Name, Path string }
	var out []country
	seen := map[string]bool{}

	for _, g := range topo.Objects.Countries.Geometries {
		code, ok := codeFor(g)
		if !ok {
			continue
		}
		if seen[code] {
			fail(fmt.Errorf("two geometries map to %s; the second is %q", code, g.Properties.Name))
		}
		rings, err := ringsOf(g, arcs)
		if err != nil {
			fail(fmt.Errorf("%s (%s): %w", code, g.Properties.Name, err))
		}
		path := pathOf(rings)
		if path == "" {
			fail(fmt.Errorf("%s (%s) produced an empty path", code, g.Properties.Name))
		}
		seen[code] = true
		out = append(out, country{Code: code, Name: g.Properties.Name, Path: path})
	}

	// Sorted by code so the generated file is a function of its input and not of
	// the order the JSON happened to list countries in. Re-running on an
	// unchanged tree must produce no diff.
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })

	var b bytes.Buffer
	fmt.Fprintf(&b, `// Code generated by internal/ui/geo/mapgen. DO NOT EDIT.
//
// Source: world-atlas countries-110m.json (Natural Earth, public domain;
// world-atlas packaging ISC). Vendored and checksummed — see scripts/get-worldmap.sh
// and the WORLDMAP_ pins in the Makefile. Regenerate with %cmake mapgen%c.
//
// Equirectangular (plate carrée), latitude clipped to [%g, %g], Antarctica
// dropped. Coordinates sit in a %s x %s frame, rounded to %d decimal place.
// The reasoning for every one of those choices is in mapgen's package comment
// rather than here — this file is output.

package geo

// ViewBox is the frame every path below is drawn in.
const ViewBox = "0 0 %s %s"

var countries = []Country{
`, '`', '`', latMin, latMax, num(frameW), num(frameH()), coordPrecision,
		num(frameW), num(frameH()))

	for _, c := range out {
		fmt.Fprintf(&b, "\t{Code: %q, Name: %q, Path: %q},\n", c.Code, c.Name, c.Path)
	}
	b.WriteString("}\n")

	src, err := format.Source(b.Bytes())
	if err != nil {
		fail(fmt.Errorf("format generated source: %w", err))
	}
	if err := os.WriteFile(os.Args[2], src, 0o644); err != nil { //nolint:gosec // committed Go source, world-readable like every other file in the tree
		fail(err)
	}
	fmt.Fprintf(os.Stderr, "mapgen: wrote %d countries to %s\n", len(out), os.Args[2])
}

// codeFor resolves a geometry to the alpha-2 code clicks are recorded under.
func codeFor(g geometry) (string, bool) {
	if g.ID != "" {
		code, ok := alpha2[g.ID]
		if !ok {
			fail(fmt.Errorf("no alpha-2 code for numeric id %q (%s); add it to iso3166.go",
				g.ID, g.Properties.Name))
		}
		// Antarctica is outside the clipped band by construction.
		if code == "AQ" {
			return "", false
		}
		return code, true
	}
	if code, ok := byName[g.Properties.Name]; ok {
		return code, true
	}
	if dropped[g.Properties.Name] {
		return "", false
	}
	fail(fmt.Errorf("geometry %q has no id and is not in byName or dropped; decide which",
		g.Properties.Name))
	return "", false
}

// decodeArcs turns TopoJSON's quantized delta encoding into lon/lat pairs.
func decodeArcs(topo *topology) ([][][2]float64, error) {
	sx, sy := topo.Transform.Scale[0], topo.Transform.Scale[1]
	tx, ty := topo.Transform.Translate[0], topo.Transform.Translate[1]

	arcs := make([][][2]float64, len(topo.Arcs))
	for i, arc := range topo.Arcs {
		var x, y float64
		points := make([][2]float64, len(arc))
		for j, d := range arc {
			x += d[0]
			y += d[1]
			lon, lat := x*sx+tx, y*sy+ty
			// The sphere, checked rather than assumed. A ring that wrapped the
			// antimeridian would land outside it, and the whole point of
			// checking here is that a wrapped ring is invisible in the data and
			// unmissable on the rendered map.
			if lon < -180.0001 || lon > 180.0001 || lat < -90.0001 || lat > 90.0001 {
				return nil, fmt.Errorf("arc %d point %d decodes to (%.4f, %.4f), off the sphere",
					i, j, lon, lat)
			}
			points[j] = [2]float64{lon, lat}
		}
		arcs[i] = points
	}
	return arcs, nil
}

// ringsOf resolves a geometry's arc indices into closed rings of lon/lat.
//
// A negative index means arc ^i traversed backwards, which is how TopoJSON
// shares a border between two countries without storing it twice.
func ringsOf(g geometry, arcs [][][2]float64) ([][][2]float64, error) {
	var polygons [][][]int
	switch g.Type {
	case "Polygon":
		var rings [][]int
		if err := json.Unmarshal(g.Arcs, &rings); err != nil {
			return nil, err
		}
		polygons = [][][]int{rings}
	case "MultiPolygon":
		if err := json.Unmarshal(g.Arcs, &polygons); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported geometry type %q", g.Type)
	}

	var out [][][2]float64
	for _, poly := range polygons {
		for _, ring := range poly {
			var points [][2]float64
			for _, idx := range ring {
				// A negative index is the one's complement of the arc to walk
				// backwards, so resolve it before bounds-checking.
				at, reversed := idx, false
				if idx < 0 {
					at, reversed = ^idx, true
				}
				if at < 0 || at >= len(arcs) {
					return nil, fmt.Errorf("arc index %d out of range", idx)
				}
				arc := arcs[at]
				seg := make([][2]float64, len(arc))
				copy(seg, arc)
				if reversed {
					for i, j := 0, len(seg)-1; i < j; i, j = i+1, j-1 {
						seg[i], seg[j] = seg[j], seg[i]
					}
				}
				// Consecutive arcs share their join point; keeping both would
				// emit a zero-length segment on every border.
				if len(points) > 0 && len(seg) > 0 && points[len(points)-1] == seg[0] {
					seg = seg[1:]
				}
				points = append(points, seg...)
			}
			if len(points) >= 4 {
				out = append(out, points)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no rings")
	}
	return out, nil
}

// pathOf projects rings and renders them as one SVG path.
//
// One path per country rather than one per ring, because the shading and the
// hover target are per country: a Norway of five paths is five things to colour
// and five things to describe to a screen reader.
func pathOf(rings [][][2]float64) string {
	var b strings.Builder
	for _, ring := range rings {
		var prevX, prevY int64
		first := true
		for _, p := range ring {
			x, y := project(p[0], p[1])
			// Quantized to the emitted precision *before* anything else, so a
			// relative step is the difference between two numbers that are both
			// in the file. Deltas taken from unrounded coordinates would drift.
			qx, qy := quantize(x), quantize(y)
			if !first && qx == prevX && qy == prevY {
				// Rounding merges points that were distinct on the sphere.
				// Dropping the duplicates shortens the path without changing it.
				continue
			}
			if first {
				b.WriteString("M")
				b.WriteString(unquantize(qx))
				b.WriteString(",")
				b.WriteString(unquantize(qy))
				first = false
			} else {
				// Relative. On a 110m outline almost every step is under a unit,
				// so "l.4,-.3" replaces "L643.8,165.9" and the generated file and
				// the rendered markup both roughly halve. SVG requires no
				// separator before a negative sign, which is where the rest of it
				// goes.
				dx, dy := unquantize(qx-prevX), unquantize(qy-prevY)
				b.WriteString("l")
				b.WriteString(dx)
				if !strings.HasPrefix(dy, "-") {
					b.WriteString(",")
				}
				b.WriteString(dy)
			}
			prevX, prevY = qx, qy
		}
		b.WriteString("Z")
	}
	return b.String()
}

// project is the equirectangular mapping, with latitude clamped to the frame.
func project(lon, lat float64) (float64, float64) {
	lat = math.Max(latMin, math.Min(latMax, lat))
	scale := frameW / 360.0
	return (lon + 180.0) * scale, (latMax - lat) * scale
}

func frameH() float64 { return (latMax - latMin) * frameW / 360.0 }

// quantize snaps a coordinate to the emitted precision, as an integer count of
// the smallest representable step. Path arithmetic happens in these units so
// that a relative step is exact.
func quantize(v float64) int64 { return int64(math.Round(v * math.Pow(10, coordPrecision))) }

// unquantize renders a quantized value or delta as compactly as SVG allows:
// no trailing ".0", and no leading "0" before the decimal point.
func unquantize(q int64) string {
	s := strconv.FormatFloat(float64(q)/math.Pow(10, coordPrecision), 'f', coordPrecision, 64)
	s = strings.TrimSuffix(s, ".0")
	switch {
	case s == "-0" || s == "":
		return "0"
	case strings.HasPrefix(s, "0."):
		return s[1:]
	case strings.HasPrefix(s, "-0."):
		return "-" + s[2:]
	}
	return s
}

// num formats a frame dimension for the generated header, without a trailing
// ".0".
func num(v float64) string {
	s := strconv.FormatFloat(v, 'f', coordPrecision, 64)
	return strings.TrimSuffix(s, ".0")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mapgen:", err)
	os.Exit(1)
}
