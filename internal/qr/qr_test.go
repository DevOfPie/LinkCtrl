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

// rendered parses an SVG back into the module grid it draws, quiet zone
// included. It is deliberately strict: anything it does not recognise is a
// failure rather than a skipped rect.
func rendered(t *testing.T, svg []byte) [][]bool {
	t.Helper()
	s := string(svg)

	span := attrInt(t, s, `viewBox="0 0 (\d+) (\d+)"`)
	grid := make([][]bool, span)
	for i := range grid {
		grid[i] = make([]bool, span)
	}

	body := s[strings.Index(s, "<g "):]
	rect := regexp.MustCompile(`<rect x="(\d+)" y="(\d+)" width="(\d+)" height="1"/>`)
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
		x, y, w := atoi(t, m[1]), atoi(t, m[2]), atoi(t, m[3])
		for i := range w {
			if y >= span || x+i >= span {
				t.Fatalf("rect at (%d,%d) width %d runs outside the %d-module picture", x, y, w, span)
			}
			if grid[y][x+i] {
				t.Fatalf("module (%d,%d) is drawn twice", x+i, y)
			}
			grid[y][x+i] = true
		}
	}
	return grid
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
			st := Style{Level: level, Margin: 3, Scale: 6}
			svg, norm := mustRender(t, sample, st)
			grid := rendered(t, svg)

			code, err := Encode(sample, level)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := len(grid), code.Size+2*norm.Margin; got != want {
				t.Fatalf("picture is %d modules across, want %d", got, want)
			}
			for y := range len(grid) {
				for x := range len(grid) {
					want := code.Dark(x-norm.Margin, y-norm.Margin)
					if grid[y][x] != want {
						t.Fatalf("module (%d,%d) drawn=%v, encoder says %v", x, y, grid[y][x], want)
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
	margin := 4
	svg, _ := mustRender(t, sample, Style{Margin: margin})
	grid := rendered(t, svg)
	span := len(grid)
	inner := span - 2*margin

	corners := map[string][2]int{
		"top-left":    {margin, margin},
		"top-right":   {margin + inner - 7, margin},
		"bottom-left": {margin, margin + inner - 7},
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
	fourth := grid[margin+inner-4][margin+inner-4]
	corner := true
	for dy := range 7 {
		for dx := range 7 {
			edge := dx == 0 || dx == 6 || dy == 0 || dy == 6
			centre := dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4
			if grid[margin+inner-7+dy][margin+inner-7+dx] != (edge || centre) {
				corner = false
			}
		}
	}
	if corner {
		t.Error("the bottom-right corner holds a finder pattern; a scanner cannot " +
			"tell which way up this code is")
	}
	_ = fourth
}

// TestTheQuietZoneIsEmptyAndPainted covers the two halves of the same claim: no
// module is drawn in the margin, and the background is painted across all of it.
//
// The second half is decision D74. A transparent quiet zone takes the colour of
// whatever the code is placed on, so a code that scans on a white page becomes a
// code on a dark field the moment somebody switches theme.
func TestTheQuietZoneIsEmptyAndPainted(t *testing.T) {
	const margin = 5
	svg, _ := mustRender(t, sample, Style{Margin: margin})
	grid := rendered(t, svg)
	span := len(grid)

	for y := range span {
		for x := range span {
			inMargin := x < margin || y < margin || x >= span-margin || y >= span-margin
			if inMargin && grid[y][x] {
				t.Fatalf("module drawn at (%d,%d), inside the quiet zone", x, y)
			}
		}
	}

	want := `<rect width="` + strconv.Itoa(span) + `" height="` + strconv.Itoa(span) +
		`" fill="` + DefaultBackground + `"/>`
	if !strings.Contains(string(svg), want) {
		t.Errorf("the background does not cover the whole picture; want %s", want)
	}
}

// TestScaleDecidesThePixelSizeAndNothingElse pins the one thing a scale change
// may do. A style is a preference about drawing, so changing it must never
// change what the code says.
func TestScaleDecidesThePixelSizeAndNothingElse(t *testing.T) {
	small, _ := mustRender(t, sample, Style{Scale: 4, Margin: 4})
	large, _ := mustRender(t, sample, Style{Scale: 20, Margin: 4})

	a, b := rendered(t, small), rendered(t, large)
	if len(a) != len(b) {
		t.Fatalf("scale changed the module count: %d vs %d", len(a), len(b))
	}
	for y := range len(a) {
		for x := range len(a) {
			if a[y][x] != b[y][x] {
				t.Fatalf("scale changed module (%d,%d)", x, y)
			}
		}
	}
	if got := attrInt(t, string(small), `width="(\d+)"`); got != len(a)*4 {
		t.Errorf("small width = %d, want %d", got, len(a)*4)
	}
	if got := attrInt(t, string(large), `width="(\d+)"`); got != len(b)*20 {
		t.Errorf("large width = %d, want %d", got, len(b)*20)
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

	// And Render is still the same bytes it was, so every caller that wants no
	// class gets no attribute rather than an empty one.
	plain, err := Render(sample, Style{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "class=") {
		t.Error("Render writes a class attribute; an empty class must write nothing, " +
			"because this is also the document served at /qr.svg and downloaded")
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
