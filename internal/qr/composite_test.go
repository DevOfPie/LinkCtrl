package qr

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A logo in the middle of a code (M50.6).
//
// **The shipping claim here is geometric and nothing else.** There is no QR
// decoder in this tree and m50.6.md decides that adding one is a separate
// decision rather than a side effect of this milestone, so what these tests hold
// is the arithmetic: the occluded module count against what level H can recover,
// for every symbol version the product's content lengths produce. Whether a
// scanner reads the result is measured by hand and written into the milestone —
// it is not, and does not pretend to be, a gate.

// minProductContent spells out [MinProductContent], so the number and the URL
// it was derived from sit beside each other. internal/link measures the same
// floor against the alias and hostname bounds it actually comes from.
const minProductContent = "https://a.b/abc?src=qr"

// solidLogo is an opaque square in a colour that is neither foreground nor
// background, PNG-encoded the way a stored logo is.
//
// Opaque and single-coloured on purpose: every pixel of the box becomes exactly
// this colour, so "which modules did the logo occlude" is answerable by looking
// at the picture instead of by trusting the code that drew it.
func solidLogo(t *testing.T, side int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

var logoColour = color.NRGBA{R: 0xd0, G: 0x21, B: 0x33, A: 0xff}

// decodeNRGBA reads a composited PNG back.
//
// A logo'd code is **not** the paletted image the two-colour path produces, and
// that is the allocation change pngWithLogo's comment states: four bytes a pixel
// rather than one. Asserting the form here is what keeps that comment honest.
func decodeNRGBA(t *testing.T, raw []byte) *image.NRGBA {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the PNG does not decode: %v", err)
	}
	if format != "png" {
		t.Fatalf("the bytes decoded as %q", format)
	}
	out := image.NewNRGBA(img.Bounds())
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			c, _ := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			out.SetNRGBA(x, y, c)
		}
	}
	return out
}

// TestTheCodewordCountIsTheStandardsOwn checks the arithmetic composite.go
// derives the correction budget from.
//
// **The budget is a fraction of this number, so this number being wrong makes
// the cap meaningless while every other test still passes.** The formula
// subtracts the function patterns from the symbol's modules; the values below
// are ISO/IEC 18004's own table, and they are what says the subtraction is
// right — the alignment grid in particular, which is the term that changes
// shape at versions 2, 7 and every seventh after.
func TestTheCodewordCountIsTheStandardsOwn(t *testing.T) {
	// version: total data-and-error-correction codewords, ISO/IEC 18004 Table 1.
	want := map[int]int{
		1: 26, 2: 44, 3: 70, 4: 100, 5: 134, 6: 172,
		7: 196, 10: 346, 20: 1085, 40: 3706,
	}
	for version, codewords := range want {
		if got := totalCodewords(version); got != codewords {
			t.Errorf("version %d holds %d codewords, and the standard says %d. "+
				"Every occlusion cap in this package is a fraction of this number",
				version, got, codewords)
		}
	}

	// And the symbol size the version is read back out of, which is the other
	// half of the same arithmetic.
	for _, version := range []int{1, 7, 40} {
		if got := symbolVersion(4*version + 17); got != version {
			t.Errorf("a %d-module symbol reads back as version %d, want %d",
				4*version+17, got, version)
		}
	}
}

// TestTheOcclusionCapFitsHsCorrectionBudget is m50.6.md's geometric assertion.
//
// *"Asserted geometrically: the occluded module count against the symbol's
// codeword budget, for every symbol version the product's content lengths
// produce."* The versions are derived from the encoder rather than listed, the
// budget from level H's ~30%, and the destroyed-codeword count from the layout —
// see composite.go for all three derivations.
func TestTheOcclusionCapFitsHsCorrectionBudget(t *testing.T) {
	if len(minProductContent) != MinProductContent {
		t.Fatalf("%q is %d bytes and MinProductContent is %d",
			minProductContent, len(minProductContent), MinProductContent)
	}
	lowest, highest := productVersions(t)
	if lowest < 3 {
		t.Fatalf("the shortest content this product can encode reaches version %d. "+
			"Versions 1 and 2 hold 7 and 14 bytes at level H and cannot carry a "+
			"logo inside half of H's budget; if one is now reachable, the cap needs "+
			"deciding again rather than asserting", lowest)
	}

	for version := lowest; version <= highest; version++ {
		modules := 4*version + 17
		side := LogoBoxModules(modules)
		budget := logoBudget(version)
		destroyed := logoWorstCase(side)

		if side <= 0 {
			t.Errorf("version %d gets no logo box at all", version)
			continue
		}
		// The cap, as m50.6.md states it: a centred square at most one fifth of
		// the symbol's width.
		if side*LogoBoxDivisor > modules {
			t.Errorf("version %d: the box is %d modules of %d, past the one-fifth cap",
				version, side, modules)
		}
		// Whole modules, centred: an even side in an odd symbol would put the box
		// half a module off and make "the same modules in both outputs" undefined.
		if side%2 == 0 || (modules-side)%2 != 0 {
			t.Errorf("version %d: a %d-module box does not centre on a %d-module grid",
				version, side, modules)
		}
		// And the derivation itself: half of H's budget, no more.
		if 2*destroyed > budget {
			t.Errorf("version %d: a %d-module box destroys at most %d codewords and "+
				"level H recovers %d. The rule is half the budget — the other half "+
				"pays for uneven distribution across Reed-Solomon blocks, for print "+
				"and optics, and for the logo's own edge",
				version, side, destroyed, budget)
		}
	}

	// The fifth is what binds, everywhere the product actually goes. If the
	// budget check ever starts reducing the box, the cap has stopped being the
	// number the milestone and the dashboard both state.
	for version := lowest; version <= highest; version++ {
		modules := 4*version + 17
		fifth := modules / LogoBoxDivisor
		if fifth%2 == 0 {
			fifth--
		}
		if got := LogoBoxModules(modules); got != fifth {
			t.Errorf("version %d: the box is %d modules where one fifth is %d — the "+
				"budget check is what is binding, and the stated cap is no longer "+
				"the operative one", version, got, fifth)
		}
	}
}

// productVersions is the range of symbol versions this product's content
// lengths reach at level H.
//
// **Two endpoints and a monotonicity check, rather than a thousand encodes.** A
// longer payload never encodes to a smaller symbol, so the versions the product
// produces are exactly the range between the shortest content it can build and
// [MaxContent]. The step below is what says the encoder agrees.
func productVersions(t *testing.T) (int, int) {
	t.Helper()
	versionOf := func(content string) int {
		code, err := Encode(content, LevelH)
		if err != nil {
			t.Fatalf("%d bytes did not encode at level H: %v", len(content), err)
		}
		return symbolVersion(code.Size)
	}

	lowest := versionOf(minProductContent)
	previous := lowest
	for n := len(minProductContent); n <= MaxContent; n += 32 {
		got := versionOf(padTo(n))
		if got < previous {
			t.Fatalf("%d bytes encodes to version %d and %d bytes to version %d; the "+
				"range below assumes a longer payload never gets a smaller symbol",
				n-32, previous, n, got)
		}
		previous = got
	}
	return lowest, versionOf(padTo(MaxContent))
}

// padTo builds a short URL of exactly n bytes. Byte mode, like every URL this
// product issues: the scheme is lowercase, so nothing is alphanumeric-mode.
func padTo(n int) string {
	const head, tail = "https://a.b/", "?src=qr"
	return head + strings.Repeat("a", n-len(head)-len(tail)) + tail
}

// TestALogoIsDrawnAtLevelH is the forcing, at the renderer.
//
// The service writes H into the row as well (D141), and this is the half that
// makes the geometric claim unconditional: a row that says `L` — written before
// this milestone, or by hand — still draws at H, so there is no picture in this
// product whose occlusion is measured against the wrong budget.
func TestALogoIsDrawnAtLevelH(t *testing.T) {
	logo := solidLogo(t, 64, logoColour)

	atH, err := Encode(sample, LevelH)
	if err != nil {
		t.Fatal(err)
	}
	atL, err := Encode(sample, LevelL)
	if err != nil {
		t.Fatal(err)
	}
	if atH.Size == atL.Size {
		t.Fatal("level L and level H produce the same symbol for this content; " +
			"the assertion below cannot tell them apart")
	}

	svg, err := RenderWithLogo(sample, Style{Level: LevelL}, logo)
	if err != nil {
		t.Fatal(err)
	}
	norm, _ := Style{Level: LevelL}.Normalize()
	if got, want := viewBox(t, svg), atH.Size+2*norm.Margin; got != want {
		t.Errorf("a code asked for at level L with a logo drew %d modules across; "+
			"level H is %d and level L is %d. A logo that did not force H would be "+
			"occluding modules against a budget a quarter of the size",
			got, want, atL.Size+2*norm.Margin)
	}

	// And without a logo the level asked for is the level drawn, unchanged.
	plain, err := Render(sample, Style{Level: LevelL})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := viewBox(t, plain), atL.Size+2*norm.Margin; got != want {
		t.Errorf("a code with no logo drew %d modules across at level L, want %d; "+
			"the forcing has escaped the case it is for", got, want)
	}
}

func viewBox(t *testing.T, svg []byte) int {
	t.Helper()
	return attrInt(t, string(svg), `viewBox="0 0 (\d+) (\d+)"`)
}

// svgLogoBox reads back the module rectangle the drawing says is occluded.
//
// The background rect after `</g>`, not the `<image>`: the box is what is
// painted over, whatever shape the logo inside it is, and the box is what the
// cap is a cap on.
func svgLogoBox(t *testing.T, svg []byte) (int, int, int) {
	t.Helper()
	s := string(svg)
	after := s[strings.Index(s, "</g>"):]
	m := regexp.MustCompile(
		`<rect x="(\d+)" y="(\d+)" width="(\d+)" height="(\d+)" fill="[^"]*"/>`,
	).FindStringSubmatch(after)
	if m == nil {
		t.Fatalf("no logo box in the drawing:\n%.200s", after)
	}
	x, y, w, h := atoi(t, m[1]), atoi(t, m[2]), atoi(t, m[3]), atoi(t, m[4])
	if w != h {
		t.Fatalf("the logo box is %dx%d and the cap is derived for a square", w, h)
	}
	return x, y, w
}

// TestTheLogoOccupiesTheSameModulesInBothOutputs is m50.6.md's parity bullet,
// and the grid check outside the box is the other half of it.
//
// **The box is read out of each output rather than compared to what the code
// intended.** The SVG states it as a rect in module units; the PNG is asked
// which of its pixels are the logo's colour, and the bounding box of those is
// converted back to modules. Two independent readings of one geometry, which is
// the shape TestTheSVGAndThePNGAreTheSamePicture already uses for the modules.
func TestTheLogoOccupiesTheSameModulesInBothOutputs(t *testing.T) {
	logo := solidLogo(t, 96, logoColour)

	for _, st := range []Style{
		{Margin: 4, Scale: 8},
		{Margin: 11, Scale: 5, Foreground: "#123a6b", Background: "#f5f7fa"},
		{Margin: 6, Scale: 7, Foreground: "#036", Background: "#fff"},
	} {
		t.Run(strconv.Itoa(st.Margin)+"x"+strconv.Itoa(st.Scale), func(t *testing.T) {
			norm, errs := st.Normalize()
			if len(errs) > 0 {
				t.Fatalf("style refused: %v", errs)
			}
			norm = norm.ForLogo()

			svg, err := RenderWithLogo(sample, st, logo)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := RenderPNGWithLogo(sample, st, logo)
			if err != nil {
				t.Fatal(err)
			}

			boxX, boxY, boxSide := svgLogoBox(t, svg)
			img := decodeNRGBA(t, raw)

			// Where the logo's colour actually landed in the raster. The box is
			// the occlusion; the image sits inside it, held back by
			// logoInsetModules of the code's own background on every side.
			minX, minY, maxX, maxY := img.Bounds().Dx(), img.Bounds().Dy(), -1, -1
			for y := range img.Bounds().Dy() {
				for x := range img.Bounds().Dx() {
					if img.NRGBAAt(x, y) != logoColour {
						continue
					}
					minX, minY = min(minX, x), min(minY, y)
					maxX, maxY = max(maxX, x), max(maxY, y)
				}
			}
			if maxX < 0 {
				t.Fatal("the rasterised code holds no pixel of the logo's colour; " +
					"nothing was composited into it")
			}

			drawX := boxX + logoInsetModules
			drawSide := boxSide - 2*logoInsetModules
			for _, c := range []struct {
				name      string
				got, want int
			}{
				{"left", minX, drawX * norm.Scale},
				{"top", minY, drawX * norm.Scale},
				{"right", maxX + 1, (drawX + drawSide) * norm.Scale},
				{"bottom", maxY + 1, (drawX + drawSide) * norm.Scale},
			} {
				if c.got != c.want {
					t.Errorf("the logo's %s edge is at %dpx in the PNG and the SVG's box "+
						"puts it at %dpx. The two outputs are drawing different "+
						"rectangles, which is the one thing the shared geometry exists to "+
						"prevent", c.name, c.got, c.want)
				}
			}

			// The SVG's own two elements agree with each other, so a reader of the
			// document sees the same inset the raster has.
			im := regexp.MustCompile(
				`<image x="(\d+)" y="(\d+)" width="(\d+)" height="(\d+)"`,
			).FindStringSubmatch(string(svg))
			if im == nil {
				t.Fatalf("no whole-module image element in the drawing:\n%.300s", svg)
			}
			if atoi(t, im[1]) != drawX || atoi(t, im[2]) != drawX ||
				atoi(t, im[3]) != drawSide || atoi(t, im[4]) != drawSide {
				t.Errorf("the SVG draws the image at (%s,%s) %sx%s and its box implies "+
					"(%d,%d) %dx%d", im[1], im[2], im[3], im[4], drawX, drawX, drawSide, drawSide)
			}

			// **And the whole box is occluded, ring included.** The cap is measured
			// against the box rather than against the image inside it, so no module
			// in it may still be showing the code.
			fg := parseHex(norm.Foreground)
			for y := boxY; y < boxY+boxSide; y++ {
				for x := boxX; x < boxX+boxSide; x++ {
					at := image.Pt(x*norm.Scale+norm.Scale/2, y*norm.Scale+norm.Scale/2)
					if got := img.NRGBAAt(at.X, at.Y); got == fg {
						t.Fatalf("module (%d,%d) inside the box still draws the code. The "+
							"occluded area the cap is derived for is the whole box, and a "+
							"module surviving in it is the cap measuring the wrong region", x, y)
					}
				}
			}

			// The box is centred and inside the cap, read off the drawing rather
			// than off the function that produced it.
			span := viewBox(t, svg)
			if boxX != boxY || boxX+boxSide != span-boxX {
				t.Errorf("the box is at (%d,%d) with side %d in a %d-module picture, "+
					"which is not centred; the cap's derivation is for a centred region "+
					"and holds for no other placement", boxX, boxY, boxSide, span)
			}

			// And the grid outside it is untouched — the M49 claim, unchanged.
			grid := rendered(t, svg)
			checked := 0
			for y := range span {
				for x := range span {
					inBox := x >= boxX && x < boxX+boxSide && y >= boxY && y < boxY+boxSide
					if inBox {
						continue
					}
					at := image.Pt(x*norm.Scale+norm.Scale/2, y*norm.Scale+norm.Scale/2)
					want := parseHex(norm.Background)
					if grid[y][x] {
						want = parseHex(norm.Foreground)
					}
					if got := img.NRGBAAt(at.X, at.Y); got != want {
						t.Fatalf("module (%d,%d), outside the logo box: the SVG draws it %s "+
							"and the PNG's pixel is %v, want %v", x, y, drawnAs(grid[y][x]), got, want)
					}
					checked++
				}
			}
			if checked == 0 {
				t.Fatal("no module was compared outside the box")
			}
		})
	}
}

// TestTheEmbeddedLogoCannotCarryMarkup is
// TestNothingButAColourReachesTheDrawing for the third caller-controlled input
// this package acquired.
//
// A logo is workspace bytes, and they now travel *inside* an SVG this package
// promises holds no character it did not write. Base64 is what keeps that
// promise: its alphabet is `A-Za-z0-9+/=` and cannot express a quote or an
// angle bracket, whatever the image holds. This puts markup inside a real PNG —
// in a `tEXt` chunk, where it survives being a valid image — and asserts none of
// it reaches the document.
func TestTheEmbeddedLogoCannotCarryMarkup(t *testing.T) {
	hostile := []byte(`"/><script>alert(1)</script><rect fill="`)
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for i := range img.Pix {
		img.Pix[i] = 0xa0
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	// The markup, appended raw after IEND. It is in the bytes NormalizeLogo would
	// have been handed and is exactly the shape a polyglot takes.
	stored := append(buf.Bytes(), hostile...)

	svg, err := RenderWithLogo(sample, Style{}, stored)
	if err != nil {
		t.Fatal(err)
	}
	got := string(svg)
	for _, needle := range []string{"<script", "alert(1)", "javascript:"} {
		if strings.Contains(got, needle) {
			t.Errorf("the drawing holds %q. The logo's bytes reached the document "+
				"unencoded, and the package's promise — that the bytes cannot hold a "+
				"`<` this package did not write — is false", needle)
		}
	}

	// And what did reach it is the base64 alphabet and nothing else.
	m := regexp.MustCompile(`href="data:image/png;base64,([^"]*)"`).FindStringSubmatch(got)
	if m == nil {
		t.Fatal("no data URI in the drawing")
	}
	if bad := strings.TrimLeft(m[1],
		"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="); bad != "" {
		t.Errorf("the data URI holds %q, which is not base64", bad)
	}
}

// TestALogoIsRefusedOnTheSameTermsAnUploadIs. The stored bytes are ones this
// product wrote, so this should never fire in practice — which is exactly why it
// is asserted rather than assumed. A column edited by hand, or a decoder that
// changed under a Go release, arrives here.
func TestALogoIsRefusedOnTheSameTermsAnUploadIs(t *testing.T) {
	for name, logo := range map[string][]byte{
		"not an image": []byte("this is not a png"),
		"an SVG":       []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`),
		"a truncated PNG": append([]byte("\x89PNG\r\n\x1a\n"),
			bytes.Repeat([]byte{0}, 32)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := RenderWithLogo(sample, Style{}, logo); err == nil {
				t.Error("it was composited into a drawing this product serves")
			}
			if _, err := RenderPNGWithLogo(sample, Style{}, logo); err == nil {
				t.Error("it was composited into a raster this product serves")
			}
		})
	}
}

// TestALogoUpscalesRatherThanBeingDrawnTiny, and keeps its aspect ratio.
//
// A wordmark is not square and must not be squashed into a square box; a small
// logo in a large box must not be a dot in the middle of a picture. Both are
// read off the drawing's own numbers.
func TestALogoKeepsItsAspectRatio(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 200, 50))
	for i := range img.Pix {
		img.Pix[i] = 0x40
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	svg, err := RenderWithLogo(sample, Style{Scale: 12}, buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(
		`<image x="[\d.]+" y="[\d.]+" width="([\d.]+)" height="([\d.]+)"`,
	).FindStringSubmatch(string(svg))
	if m == nil {
		t.Fatalf("no image element in the drawing:\n%.300s", svg)
	}
	w, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	h, err := strconv.ParseFloat(m[2], 64)
	if err != nil {
		t.Fatal(err)
	}
	if ratio := w / h; ratio < 3.6 || ratio > 4.4 {
		t.Errorf("a 200x50 logo is drawn at %gx%g, a ratio of %.2f; 4:1 is what it "+
			"was uploaded as, and a box that squashed it would be drawing a "+
			"different picture", w, h, ratio)
	}
	_, _, side := svgLogoBox(t, svg)
	if w > float64(side)+0.001 || h > float64(side)+0.001 {
		t.Errorf("the logo is drawn %gx%g modules and its box is %d; it is outside "+
			"the area the cap was derived for", w, h, side)
	}
}
