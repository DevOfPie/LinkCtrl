package geo

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestTheProjectionPutsCountriesWhereTheyBelong is the test that would have
// caught a generator that silently produced a plausible-looking wrong map.
//
// A choropleth fails quietly: mirrored, offset by a hemisphere or scaled wrong,
// it still renders 174 shapes in indigo and still looks like a world. So the
// assertion is arithmetic rather than aesthetic — every country's bounding box
// is checked against its real longitude and latitude, pushed through the
// equirectangular mapping the generator documents.
func TestTheProjectionPutsCountriesWhereTheyBelong(t *testing.T) {
	// Coarse real-world extents, deliberately loose: the point is to catch a
	// map that is wrong by tens of degrees, not to re-specify Natural Earth.
	tests := []struct {
		code           string
		lonMin, lonMax float64
		latMin, latMax float64
	}{
		{"US", -180, -66, 18, 72}, // Alaska carries it past the antimeridian's edge
		{"GB", -9, 2, 49, 61},
		{"AU", 112, 154, -44, -9},
		{"BR", -74, -34, -34, 6},
		{"JP", 122, 154, 24, 46},
		{"ZA", 16, 33, -35, -22},
	}

	byCode := map[string]Country{}
	for _, c := range Countries() {
		byCode[c.Code] = c
	}

	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			c, ok := byCode[tt.code]
			if !ok {
				t.Fatalf("%s is not on the map at all", tt.code)
			}
			minX, minY, maxX, maxY := bounds(t, c.Path)

			wantMinX, wantMaxY := project(tt.lonMin, tt.latMin)
			wantMaxX, wantMinY := project(tt.lonMax, tt.latMax)

			// One unit is 0.36 degrees. Two units of slack absorbs the rounding
			// in the generated path and the looseness of the extents above.
			const slack = 2
			if minX < wantMinX-slack || maxX > wantMaxX+slack {
				t.Errorf("%s spans x [%.1f, %.1f]; longitudes [%.0f, %.0f] project to [%.1f, %.1f]",
					tt.code, minX, maxX, tt.lonMin, tt.lonMax, wantMinX, wantMaxX)
			}
			if minY < wantMinY-slack || maxY > wantMaxY+slack {
				t.Errorf("%s spans y [%.1f, %.1f]; latitudes [%.0f, %.0f] project to [%.1f, %.1f]",
					tt.code, minY, maxY, tt.latMin, tt.latMax, wantMinY, wantMaxY)
			}
		})
	}
}

// project repeats the generator's equirectangular mapping. Repeated rather than
// imported, because a test that reuses the code under test only proves the code
// agrees with itself.
func project(lon, lat float64) (float64, float64) {
	const (
		frameW = 1000.0
		latMax = 84.0
		latMin = -58.0
	)
	lat = math.Max(latMin, math.Min(latMax, lat))
	scale := frameW / 360.0
	return (lon + 180.0) * scale, (latMax - lat) * scale
}

// TestEveryPathStaysInsideTheViewBox catches the failure the projection test
// cannot: a shape that wraps the antimeridian is inside its own bounding box
// and outside the frame.
func TestEveryPathStaysInsideTheViewBox(t *testing.T) {
	w, h := viewBoxSize(t)
	for _, c := range Countries() {
		minX, minY, maxX, maxY := bounds(t, c.Path)
		if minX < 0 || minY < 0 || maxX > w || maxY > h {
			t.Errorf("%s (%s) spans x [%.1f, %.1f] y [%.1f, %.1f], outside the %s frame",
				c.Code, c.Name, minX, maxX, minY, maxY, ViewBox)
		}
	}
}

// TestTheMapCoversTheCodesTrafficActuallyArrivesUnder is a floor, not an
// inventory. A generator that dropped half the world would still pass every
// geometry check above.
func TestTheMapCoversTheCodesTrafficActuallyArrivesUnder(t *testing.T) {
	if got := len(Countries()); got < 170 {
		t.Errorf("the map has %d countries; the 110m source carries 174 after "+
			"Antarctica and the two unnumbered territories are dropped", got)
	}
	byCode := map[string]bool{}
	for _, c := range Countries() {
		if c.Code == "" || len(c.Code) != 2 {
			t.Errorf("%q is not an alpha-2 code", c.Code)
		}
		if c.Name == "" {
			t.Errorf("%s has no name, so its shape has no accessible label", c.Code)
		}
		if byCode[c.Code] {
			t.Errorf("%s appears twice", c.Code)
		}
		byCode[c.Code] = true
	}
	// Antarctica is outside the clipped band and must not have been clamped
	// into a stripe along the bottom edge.
	if byCode["AQ"] {
		t.Error("AQ is on the map; the projection clips latitude at -58 and " +
			"Antarctica would render as a solid bar across the frame")
	}
}

// TestShadingSeparatesNoDataFromTheBottomOfTheScale is the claim the colour
// scale rests on. A country nobody visited and a country one person visited are
// different answers, and a map that renders them alike is a map that lies about
// its own bottom.
func TestShadingSeparatesNoDataFromTheBottomOfTheScale(t *testing.T) {
	shapes := Choropleth(map[string]int64{"US": 1000, "GB": 1}, 1001)

	steps := map[string]int{}
	for _, s := range shapes {
		steps[s.Code] = s.Step
	}
	if steps["US"] != Steps {
		t.Errorf("the largest value is in step %d, want %d", steps["US"], Steps)
	}
	if steps["GB"] != 1 {
		t.Errorf("a value of 1 against a maximum of 1000 is in step %d, want 1: "+
			"a nonzero figure must never share a band with no data", steps["GB"])
	}
	if steps["FR"] != 0 {
		t.Errorf("a country with no clicks is in step %d, want 0", steps["FR"])
	}
}

func TestShareIsOfTheTotalAndBandingIsOfTheMaximum(t *testing.T) {
	shapes := Choropleth(map[string]int64{"US": 50, "GB": 30, "FR": 20}, 100)
	for _, s := range shapes {
		switch s.Code {
		case "US":
			if s.Share != 50 {
				t.Errorf("US share = %v, want 50", s.Share)
			}
			if s.Step != Steps {
				t.Errorf("US step = %d, want %d", s.Step, Steps)
			}
		case "GB":
			if s.Share != 30 {
				t.Errorf("GB share = %v, want 30", s.Share)
			}
			// 30 of a 50 maximum is 60%, which is band 3 of 5.
			if s.Step != 3 {
				t.Errorf("GB step = %d, want 3", s.Step)
			}
		}
	}
}

// TestUnmappedNamesWhatTheMapCannotDraw. GeoIP resolves territories Natural
// Earth's 110m countries do not draw, and a click from one of them counts
// towards the total. Naming them is what stops the map and the ranked list
// beside it quietly disagreeing.
func TestUnmappedNamesWhatTheMapCannotDraw(t *testing.T) {
	got := Unmapped(map[string]int64{"US": 10, "HK": 3, "MC": 1, "XX": 0})
	want := []string{"HK", "MC"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Unmapped = %v, want %v (sorted, zero counts excluded, mapped "+
			"codes excluded)", got, want)
	}
}

// TestTheFillRuleIsEvenOdd. South Africa's polygon contains a hole where
// Lesotho is. Under SVG's default nonzero rule that hole fills in solid unless
// the two rings wind in opposite directions, and mapgen emits rings in whatever
// order arc traversal produces.
func TestTheFillRuleIsEvenOdd(t *testing.T) {
	if FillRule != "evenodd" {
		t.Fatalf("FillRule = %q; a country whose geometry contains an enclave "+
			"needs evenodd or the enclave disappears", FillRule)
	}
	for _, c := range Countries() {
		if c.Code == "ZA" {
			if n := strings.Count(c.Path, "M"); n < 2 {
				t.Errorf("South Africa's path has %d subpaths; it should carry at "+
					"least the mainland and the Lesotho hole, which is what makes "+
					"the fill rule matter", n)
			}
			return
		}
	}
	t.Error("South Africa is not on the map")
}

// bounds walks a path's coordinates.
//
// The generated paths use exactly three commands — an absolute M that opens a
// subpath, relative l steps, and Z — which is what makes this a loop rather than
// an SVG parser. Walking the relative steps rather than trusting them is the
// point: a delta encoding that drifts would put a country a few units off its
// own outline, and every check in this file is about catching a map that is
// wrong while still looking like a map.
//
// The walk accumulates in tenths as integers, because the deltas are exact
// multiples of a tenth and float addition is not. An earlier version added them
// as float64 and reported Russia starting at -0.0, which is a fact about
// binary floating point rather than about the map.
func bounds(t *testing.T, path string) (minX, minY, maxX, maxY float64) {
	t.Helper()
	var x, y, lo, hi, loY, hiY int64
	first := true

	i := 0
	for i < len(path) {
		switch c := path[i]; c {
		case 'Z':
			i++
		case 'M', 'l':
			i++
			j := i
			for j < len(path) && path[j] != 'M' && path[j] != 'l' && path[j] != 'Z' {
				j++
			}
			dx, dy := parsePair(t, path[i:j])
			if c == 'M' {
				x, y = dx, dy
			} else {
				x, y = x+dx, y+dy
			}
			if first {
				lo, hi, loY, hiY = x, x, y, y
				first = false
			}
			lo, hi = min(lo, x), max(hi, x)
			loY, hiY = min(loY, y), max(hiY, y)
			i = j
		default:
			t.Fatalf("unexpected command %q in path; the generator emits only M, l and Z", string(c))
		}
	}
	if first {
		t.Fatalf("path %q has no coordinates", path)
	}
	const tenths = 10.0
	return float64(lo) / tenths, float64(loY) / tenths, float64(hi) / tenths, float64(hiY) / tenths
}

// parsePair reads "x,y", "x-y" or "-x-y" as tenths. SVG needs no separator
// before a minus sign and the generator takes advantage of that.
func parsePair(t *testing.T, s string) (int64, int64) {
	t.Helper()
	split := -1
	for i := 1; i < len(s); i++ {
		if s[i] == ',' || s[i] == '-' {
			split = i
			break
		}
	}
	if split < 0 {
		t.Fatalf("path segment %q is not an x,y pair", s)
	}
	xs, ys := s[:split], s[split:]
	if ys[0] == ',' {
		ys = ys[1:]
	}
	return tenthsOf(t, xs), tenthsOf(t, ys)
}

func tenthsOf(t *testing.T, s string) int64 {
	t.Helper()
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return int64(math.Round(v * 10))
}

func viewBoxSize(t *testing.T) (float64, float64) {
	t.Helper()
	parts := strings.Fields(ViewBox)
	if len(parts) != 4 {
		t.Fatalf("ViewBox = %q, want four numbers", ViewBox)
	}
	w, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		t.Fatal(err)
	}
	h, err := strconv.ParseFloat(parts[3], 64)
	if err != nil {
		t.Fatal(err)
	}
	return w, h
}
