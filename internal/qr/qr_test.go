package qr

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// What these tests are for, and what they deliberately are not.
//
// The encoder is a vetted third-party library (D72) and re-testing ISO/IEC 18004
// conformance here would be testing somebody else's test suite. **What is this
// package's own is the drawing**, and that is where a silent failure lives: an
// SVG that is subtly wrong scans on the machine it was written on and fails on a
// printed poster six weeks later.
//
// So the drawing is read back. rendered() parses the emitted rects into a matrix
// and every test below asks a question about that matrix rather than about the
// bytes — and one of them (the finder patterns) is answered against the picture
// alone, without consulting the encoder, so a renderer that flipped or
// transposed the matrix would still be caught.

const sample = "https://links.example/summer?src=qr"

// rendered parses an SVG back into the module grid it draws, in the **code's**
// own module coordinates — (0,0) is the code's top-left module, not the
// picture's — together with the geometry the picture was drawn at.
//
// **It reads pixels and divides, and it did neither before D182.** The drawing
// was in module units and the parser could index the rects straight into a grid;
// the quiet zone is measured in pixels now, so the viewBox is too, and the
// parser has to undo the same arithmetic both encoders did. That is deliberately
// not a weakening: every division below is asserted exact, so a rect landing off
// the module grid — the one thing the pixel viewBox could have cost — is a
// failure here rather than a picture nobody checked.
//
// It stays strict in the way it was: anything it does not recognise is a failure
// rather than a skipped rect.
func rendered(t *testing.T, svg []byte, modules int, st Style) ([][]bool, geometry) {
	t.Helper()
	s := string(svg)

	norm, errs := st.Normalize()
	if len(errs) > 0 {
		t.Fatalf("style rejected: %v", errs)
	}
	g := fitGeometry(modules, norm)
	if px := attrInt(t, s, `viewBox="0 0 (\d+) (\d+)"`); px != g.px {
		t.Fatalf("the viewBox is %dpx across and the geometry says %dpx", px, g.px)
	}

	grid := make([][]bool, modules)
	for i := range grid {
		grid[i] = make([]bool, modules)
	}

	// The module group and nothing after it. Since M50.6 a drawing may carry a
	// logo's box and its image *after* `</g>`, and those are not modules — a
	// parser that read them would count the box as a rect it could not recognise
	// and fail every logo'd drawing.
	body := s[strings.Index(s, "<g ") : strings.Index(s, "</g>")+len("</g>")]
	rect := regexp.MustCompile(`<rect x="(\d+)" y="(\d+)" width="(\d+)" height="(\d+)"/>`)
	matches := rect.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatal("no module rects in the drawing")
	}
	// Everything inside <g> must be a rect this pattern recognises: if the
	// renderer ever emits a shape this parser skips, the comparison below would
	// pass while the picture had gained modules nobody checked.
	if n := strings.Count(body, "<rect"); n != len(matches) {
		t.Fatalf("drawing holds %d rects, %d of them parseable", n, len(matches))
	}
	for _, m := range matches {
		px, py := atoi(t, m[1]), atoi(t, m[2])
		pw, ph := atoi(t, m[3]), atoi(t, m[4])
		if ph != g.scale {
			t.Fatalf("a module run at (%d,%d) is %dpx tall and a module is %dpx",
				px, py, ph, g.scale)
		}
		// Exact, or the run is not on the module grid at all — which is the
		// failure a pixel-space drawing has to be held to and a module-space one
		// could not express.
		if (px-g.origin)%g.scale != 0 || (py-g.origin)%g.scale != 0 || pw%g.scale != 0 {
			t.Fatalf("a module run at (%d,%d) %dpx wide does not divide by a %dpx "+
				"module from a %dpx origin", px, py, pw, g.scale, g.origin)
		}
		x, y, w := (px-g.origin)/g.scale, (py-g.origin)/g.scale, pw/g.scale
		for i := range w {
			if y < 0 || y >= modules || x+i < 0 || x+i >= modules {
				t.Fatalf("run at module (%d,%d) width %d runs outside the %d-module code",
					x, y, w, modules)
			}
			if grid[y][x+i] {
				t.Fatalf("module (%d,%d) is drawn twice", x+i, y)
			}
			grid[y][x+i] = true
		}
	}
	return grid, g
}

func attrInt(t *testing.T, s, pattern string) int {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no match for %s in the drawing", pattern)
	}
	return atoi(t, m[1])
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mustRender(t *testing.T, content string, st Style) ([]byte, Style) {
	t.Helper()
	normalized, errs := st.Normalize()
	if len(errs) > 0 {
		t.Fatalf("style rejected: %v", errs)
	}
	svg, err := Render(content, st)
	if err != nil {
		t.Fatal(err)
	}
	return svg, normalized
}

// TestTheDrawingIsTheEncodersMatrix is the renderer's correctness claim: every
// dark module the encoder produced is drawn, at the right place, and nothing
// else is.
func TestTheDrawingIsTheEncodersMatrix(t *testing.T) {
	for _, level := range Levels {
		t.Run(string(level), func(t *testing.T) {
			// Both forms of the geometry, because D182 added one and the older
			// one is what every row written before it carries: a margin in
			// modules, and a size in pixels the margin is derived from.
			for _, st := range []Style{
				{Level: level, Margin: 3, Scale: 6},
				{Level: level, Margin: DefaultMargin, Scale: 6, Size: 400},
			} {
				code, err := Encode(sample, level)
				if err != nil {
					t.Fatal(err)
				}
				svg, _ := mustRender(t, sample, st)
				grid, _ := rendered(t, svg, code.Size, st)

				for y := range code.Size {
					for x := range code.Size {
						if want := code.Dark(x, y); grid[y][x] != want {
							t.Fatalf("%+v: module (%d,%d) drawn=%v, encoder says %v",
								st, x, y, grid[y][x], want)
						}
					}
				}
			}
		})
	}
}

// TestTheFinderPatternsAreWhereAScannerLooks reads the picture and nothing else.
//
// This is the test that does not consult the encoder, and that is the point: a
// renderer that transposed the matrix, mirrored it, or drew it a module out
// would still agree with `code.Dark` if the comparison were made against the
// same source. A QR code's three finder patterns are at the top-left, top-right
// and bottom-left of the *picture* — 7x7, a dark ring, a light ring, a 3x3 dark
// centre — and a scanner will not read one that is anywhere else.
func TestTheFinderPatternsAreWhereAScannerLooks(t *testing.T) {
	st := Style{Margin: 4}
	code, err := Encode(sample, DefaultLevel)
	if err != nil {
		t.Fatal(err)
	}
	svg, _ := mustRender(t, sample, st)
	grid, _ := rendered(t, svg, code.Size, st)
	inner := code.Size

	corners := map[string][2]int{
		"top-left":    {0, 0},
		"top-right":   {inner - 7, 0},
		"bottom-left": {0, inner - 7},
	}
	for name, at := range corners {
		ox, oy := at[0], at[1]
		for dy := range 7 {
			for dx := range 7 {
				// The finder pattern, spelled out: the outer ring and the 3x3
				// centre are dark, the ring between them is light.
				edge := dx == 0 || dx == 6 || dy == 0 || dy == 6
				centre := dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4
				want := edge || centre
				if got := grid[oy+dy][ox+dx]; got != want {
					t.Fatalf("%s finder pattern: module (%d,%d) is %v, want %v",
						name, dx, dy, got, want)
				}
			}
		}
	}

	// The fourth corner carries no finder pattern, which is how a scanner works
	// out the code's rotation. Asserting it is what stops a renderer that drew
	// four from passing the loop above.
	corner := true
	for dy := range 7 {
		for dx := range 7 {
			edge := dx == 0 || dx == 6 || dy == 0 || dy == 6
			centre := dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4
			if grid[inner-7+dy][inner-7+dx] != (edge || centre) {
				corner = false
			}
		}
	}
	if corner {
		t.Error("the bottom-right corner holds a finder pattern; a scanner cannot " +
			"tell which way up this code is")
	}
}

// TestTheQuietZoneIsEmptyAndPainted covers the two halves of the same claim: no
// module is drawn in the margin, and the background is painted across all of it.
//
// The second half is decision D74. A transparent quiet zone takes the colour of
// whatever the code is placed on, so a code that scans on a white page becomes a
// code on a dark field the moment somebody switches theme.
// **Both forms of the geometry**, because the quiet zone is where the two
// differ: the older one measures it in modules and the newer one is whatever
// pixels the requested size leaves over (D182). The emptiness half is enforced
// by `rendered` itself, which refuses a run that lands outside the code's own
// grid, so what is left to assert here is the *width* of the zone and the paint
// across it.
func TestTheQuietZoneIsEmptyAndPainted(t *testing.T) {
	code, err := Encode(sample, DefaultLevel)
	if err != nil {
		t.Fatal(err)
	}
	fit, err := FitSize(code.Size, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		style  Style
		origin int
		px     int
	}{
		{"margin in modules", Style{Margin: 5, Scale: DefaultScale},
			5 * DefaultScale, (code.Size + 10) * DefaultScale},
		{"size in pixels", Style{Margin: DefaultMargin, Scale: fit.Scale, Size: 500},
			(500 - code.Size*fit.Scale) / 2, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svg, _ := mustRender(t, sample, tc.style)
			_, g := rendered(t, svg, code.Size, tc.style)

			if g.origin != tc.origin || g.px != tc.px {
				t.Fatalf("the picture is %dpx with a %dpx quiet zone; want %dpx and %dpx",
					g.px, g.origin, tc.px, tc.origin)
			}
			want := `<rect width="` + strconv.Itoa(g.px) + `" height="` + strconv.Itoa(g.px) +
				`" fill="` + DefaultBackground + `"/>`
			if !strings.Contains(string(svg), want) {
				t.Errorf("the background does not cover the whole picture; want %s", want)
			}
		})
	}
}

// TestScaleDecidesThePixelSizeAndNothingElse pins the one thing a scale change
// may do. A style is a preference about drawing, so changing it must never
// change what the code says.
func TestScaleDecidesThePixelSizeAndNothingElse(t *testing.T) {
	code, err := Encode(sample, DefaultLevel)
	if err != nil {
		t.Fatal(err)
	}
	smallStyle := Style{Scale: 4, Margin: 4}
	largeStyle := Style{Scale: 20, Margin: 4}
	small, _ := mustRender(t, sample, smallStyle)
	large, _ := mustRender(t, sample, largeStyle)

	a, _ := rendered(t, small, code.Size, smallStyle)
	b, _ := rendered(t, large, code.Size, largeStyle)
	for y := range code.Size {
		for x := range code.Size {
			if a[y][x] != b[y][x] {
				t.Fatalf("scale changed module (%d,%d)", x, y)
			}
		}
	}
	span := code.Size + 8
	if got := attrInt(t, string(small), `width="(\d+)"`); got != span*4 {
		t.Errorf("small width = %d, want %d", got, span*4)
	}
	if got := attrInt(t, string(large), `width="(\d+)"`); got != span*20 {
		t.Errorf("large width = %d, want %d", got, span*20)
	}
}

// TestNothingButAColourReachesTheDrawing is the security claim that makes it
// safe to inline this output into a dashboard page as template.HTML.
//
// The stored style is the only workspace-controlled input, and every one of
// these is a string somebody could put in a jsonb column by hand or post to the
// API. If any of them reached the output, the SVG would stop being markup this
// file wrote.
func TestNothingButAColourReachesTheDrawing(t *testing.T) {
	hostile := []string{
		`"/><script>alert(1)</script>`,
		`red`,
		`rgb(1,2,3)`,
		`url(#x)`,
		`#fff" onload="x`,
		`#GGGGGG`,
		`#ffff`,
		`currentColor`,
		"#ff\x00ff",
	}
	for _, colour := range hostile {
		t.Run(colour, func(t *testing.T) {
			_, errs := Style{Foreground: colour}.Normalize()
			if len(errs) == 0 {
				t.Fatalf("%q was accepted as a foreground colour", colour)
			}
			if _, err := Render(sample, Style{Background: colour}); err == nil {
				t.Fatalf("%q was accepted as a background colour", colour)
			}
		})
	}
}

// TestAValidStyleReachesTheDrawingExactly is the other half: the colours that
// are accepted are the colours that appear.
func TestAValidStyleReachesTheDrawingExactly(t *testing.T) {
	svg, _ := mustRender(t, sample, Style{
		Foreground: "#1A2B3C", Background: "#FFF", Level: "h", Margin: 2, Scale: 10,
	})
	s := string(svg)
	// Folded to lowercase on the way in, so the stored value and the drawn value
	// are the same string.
	if !strings.Contains(s, `fill="#1a2b3c"`) {
		t.Error("the foreground colour is not in the drawing")
	}
	if !strings.Contains(s, `fill="#fff"`) {
		t.Error("the background colour is not in the drawing")
	}
	if strings.Contains(s, "<script") || strings.Contains(s, "javascript:") {
		t.Error("the drawing holds something that is not a picture")
	}
}

// TestTheSameColourTwiceIsRefused. A code drawn in its own background colour is
// a blank square, and it would be accepted by every other check here.
func TestTheSameColourTwiceIsRefused(t *testing.T) {
	if _, errs := (Style{Foreground: "#123456", Background: "#123456"}).Normalize(); len(errs) == 0 {
		t.Fatal("a code and its background were allowed to be the same colour")
	}
}

// TestDefaultsAreDarkOnLight. The zero Style is the default style, and the
// default is the one a scanner expects.
func TestDefaultsAreDarkOnLight(t *testing.T) {
	got, errs := Style{}.Normalize()
	if len(errs) > 0 {
		t.Fatalf("the zero style was refused: %v", errs)
	}
	want := Style{
		Foreground: DefaultForeground, Background: DefaultBackground,
		Level: DefaultLevel, Margin: DefaultMargin, Scale: DefaultScale,
	}
	if got != want {
		t.Errorf("defaults = %+v, want %+v", got, want)
	}
}

// TestOutOfRangeSizesAreRefusedRatherThanClamped. Silently clamping would report
// success for a setting nobody asked for.
func TestOutOfRangeSizesAreRefusedRatherThanClamped(t *testing.T) {
	for _, st := range []Style{
		{Margin: -1}, {Margin: MaxMargin + 1},
		{Scale: MinScale - 1}, {Scale: MaxScale + 1},
		{Level: "X"},
	} {
		if _, errs := st.Normalize(); len(errs) == 0 {
			t.Errorf("%+v was accepted", st)
		}
	}
}

// TestContentTooLongIsASentence. The bound exists so an oversized input is
// something a caller can act on.
func TestContentTooLongIsASentence(t *testing.T) {
	if _, err := Encode(strings.Repeat("x", MaxContent+1), LevelM); err == nil {
		t.Fatal("an oversized input was encoded")
	}
	if _, err := Encode("", LevelM); err == nil {
		t.Fatal("nothing was encoded")
	}
}

// TestAHigherLevelCostsModules. Not a conformance test — a sanity check that the
// level reaches the encoder at all, which is the one way the style could
// silently stop mattering.
func TestAHigherLevelCostsModules(t *testing.T) {
	low, err := Encode(sample, LevelL)
	if err != nil {
		t.Fatal(err)
	}
	high, err := Encode(sample, LevelH)
	if err != nil {
		t.Fatal(err)
	}
	if high.Size <= low.Size {
		t.Errorf("level H produced a %d-module code, level L %d; the level is not "+
			"reaching the encoder", high.Size, low.Size)
	}
}

// TestAClassGoesOnTheRootElementAndNowhereElse is M48's addition, and the whole
// point of it is that the class is on the `<svg>` rather than on something
// wrapping it: the link page's guard reads the height off the drawing itself,
// because the drawing is what has a height nobody stated.
func TestAClassGoesOnTheRootElementAndNowhereElse(t *testing.T) {
	svg, err := RenderClass(sample, Style{}, "h-24 w-24")
	if err != nil {
		t.Fatal(err)
	}
	got := string(svg)

	if !strings.HasPrefix(got, `<svg xmlns="http://www.w3.org/2000/svg" class="h-24 w-24" width="`) {
		t.Errorf("the class is not on the root element:\n  %.120s…", got)
	}
	if n := strings.Count(got, "class="); n != 1 {
		t.Errorf("the drawing carries %d class attributes, want 1; the modules are "+
			"not styled individually and never should be", n)
	}

	// And the empty class still writes no attribute rather than an empty one,
	// which is what the document served at /qr.svg is rendered with: a class
	// list resolves against the dashboard's stylesheet and a downloaded file is
	// nowhere near it. *(Until F184 this was asserted of Render, because Render
	// was the empty class. It is not any more — see below — so the claim moved
	// to the call that actually makes it.)*
	plain, err := RenderClass(sample, Style{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "class=") {
		t.Error("an empty class writes a class attribute; it must write nothing, " +
			"because this is the document served at /qr.svg and downloaded")
	}
}

// TestAnInlinedCodeFitsTheBoxItIsDrawnInto is F184: the drawing states its size
// in pixels, and a page is not measured in pixels.
//
// **Why it is asserted here rather than only in the page that broke.**
// /account/mfa reached 174px past a 360px viewport because internal/qr wrote
// `width="488"` onto an element inside a 160px frame, and the caller — the MFA
// enrolment handler — states no class of its own. The constraint therefore has
// to come from the default, and a default is a claim about every call site that
// has not overridden it. internal/ui's overflow scan is the other half; it reads
// the class off rendered markup and cannot see this function at all.
func TestAnInlinedCodeFitsTheBoxItIsDrawnInto(t *testing.T) {
	for _, tc := range []struct {
		name string
		draw func() ([]byte, error)
	}{
		{"Render", func() ([]byte, error) { return Render(sample, Style{}) }},
		{"RenderWithLogo/none", func() ([]byte, error) {
			return RenderWithLogo(sample, Style{}, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svg, err := tc.draw()
			if err != nil {
				t.Fatal(err)
			}
			want := `class="` + FluidClass + `"`
			if !strings.Contains(string(svg), want) {
				t.Errorf("%s draws a code carrying no %s.\n  %.140s…\n\n"+
					"The width and height attributes are an intrinsic size, and an "+
					"inlined code with nothing bounding it drags the page sideways "+
					"on a phone — which is what /account/mfa did, by 174px at 360px.",
					tc.name, want, svg)
			}
		})
	}

	// The picture itself is untouched: the class bounds the element, and the
	// geometry inside the viewBox is what the PNG has to match.
	fluid, err := Render(sample, Style{})
	if err != nil {
		t.Fatal(err)
	}
	bare, err := RenderClass(sample, Style{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Replace(string(fluid), ` class="`+FluidClass+`"`, "", 1),
		string(bare); got != want {
		t.Error("the fluid class changes more than the class attribute; it must be " +
			"the only difference, or the two outputs stop being the same picture")
	}
}

// TestNothingButAClassNameReachesTheClassAttribute is
// TestNothingButAColourReachesTheDrawing for the second caller-controlled input
// this package acquired.
//
// The class is a Go constant today and an attacker reaches none of it. That is
// exactly the argument that stops being true the first time somebody derives one
// from data, and the package's promise — that the bytes cannot hold a `<` this
// file did not write — is worth more than the argument.
func TestNothingButAClassNameReachesTheClassAttribute(t *testing.T) {
	hostile := []string{
		`h-24" onload="x`,
		`"><script>alert(1)</script>`,
		`h-[6rem]`,
		"h-24\nw-24",
		"h-24\x00",
		`h-24;`,
		`h-24'`,
	}
	for _, class := range hostile {
		t.Run(class, func(t *testing.T) {
			if _, err := RenderClass(sample, Style{}, class); err == nil {
				t.Fatalf("%q was accepted as a class list", class)
			}
		})
	}
}
