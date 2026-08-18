package qr

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The PNG, and the claim that it and the SVG are the same picture (M49).
//
// **The claim is the milestone's, stated as a mechanism rather than as an
// intention.** Both outputs come from one matrix, one snapped pixels-per-module
// and one origin offset — geometry and runs in qr.go — and this file is what
// holds them to it. It reads the SVG's rects back into a grid, decodes the PNG,
// and asks the PNG what colour it put at the centre of every module the SVG
// says is dark, and of every module the SVG says is light.
//
// **The limit, stated as m46.md would want it stated.** This asserts the module
// *geometry* matches. It does not assert that a browser's rasterisation of the
// SVG is byte-identical to the PNG — that is untrue of any two rasterisers, it
// is not what was asked for, and a test claiming it would be a test that fails
// for reasons nobody can act on.

// decodePNG reads the bytes back as an image, refusing anything that is not the
// paletted form MaxSize's allocation figure is calculated from.
func decodePNG(t *testing.T, raw []byte) *image.Paletted {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the PNG does not decode: %v", err)
	}
	if format != "png" {
		t.Fatalf("the bytes decoded as %q", format)
	}
	p, ok := img.(*image.Paletted)
	if !ok {
		t.Fatalf("the image decoded as %T, not a paletted one. MaxSize's allocation "+
			"figure — one byte per pixel, 4,194,304 at 2048px — is calculated from the "+
			"paletted form, and an RGBA buffer is four times it", img)
	}
	return p
}

// TestTheSVGAndThePNGAreTheSamePicture is the milestone's matching claim.
func TestTheSVGAndThePNGAreTheSamePicture(t *testing.T) {
	// Five styles, chosen for the ways the two encoders could disagree: a quiet
	// zone at the floor and one well above it, so the origin offset is exercised
	// at more than one value; an odd scale, so a centre pixel is not on a
	// boundary by luck; colours that are neither black nor white, so a palette
	// written in the wrong order is visible — and, since D182, **two sizes that
	// the module grid cannot divide**, which is where the quiet zone is a pixel
	// remainder rather than a module count and where an odd remainder puts the
	// symbol one pixel off centre. That last case is the one the whole reopening
	// turns on: it is a *requested* size, and the claim is that both encoders
	// draw it.
	code, err := Encode(sample, DefaultLevel)
	if err != nil {
		t.Fatal(err)
	}
	odd, err := FitSize(code.Size, 501)
	if err != nil {
		t.Fatal(err)
	}
	even, err := FitSize(code.Size, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []Style{
		{Margin: 4, Scale: 8},
		{Margin: 11, Scale: 3, Foreground: "#123a6b", Background: "#f5f7fa"},
		{Margin: 6, Scale: 7, Foreground: "#036", Background: "#fff"},
		{Margin: DefaultMargin, Scale: even.Scale, Size: 500},
		{Margin: DefaultMargin, Scale: odd.Scale, Size: 501,
			Foreground: "#036", Background: "#fff"},
	} {
		name := strconv.Itoa(st.Margin) + "x" + strconv.Itoa(st.Scale)
		if st.Size != 0 {
			name = "at" + strconv.Itoa(st.Size)
		}
		t.Run(name, func(t *testing.T) {
			norm, errs := st.Normalize()
			if len(errs) > 0 {
				t.Fatalf("style refused: %v", errs)
			}

			svg := code.SVG(norm)
			grid, g := rendered(t, svg, code.Size, norm)
			px := g.px

			// A requested size is the size drawn, exactly — the reopening's first
			// bullet, asserted where both encoders can be held to it at once.
			if st.Size != 0 && px != st.Size {
				t.Fatalf("a style asking for %dpx drew %dpx", st.Size, px)
			}

			// The SVG states its pixel size rather than leaving it to a viewBox,
			// which is the bullet that makes "rendered at the set pixel size"
			// something a browser does rather than something a stylesheet
			// arranges. Both attributes, and both the same, because the picture
			// is square.
			for _, attr := range []string{"width", "height"} {
				if got := attrInt(t, string(svg), attr+`="(\d+)"`); got != px {
					t.Fatalf("the SVG's %s is %d, want %d", attr, got, px)
				}
			}
			if !strings.Contains(string(svg), `viewBox="0 0 `+strconv.Itoa(px)) {
				t.Error("the SVG has no viewBox; the pixel size is explicit and the " +
					"viewBox is what makes it scale cleanly anyway")
			}

			raw, err := code.PNG(norm)
			if err != nil {
				t.Fatal(err)
			}
			img := decodePNG(t, raw)

			if b := img.Bounds(); b.Dx() != px || b.Dy() != px {
				t.Fatalf("the PNG is %dx%d and the SVG is %dx%d. The two are supposed to "+
					"be one arithmetic; a size that differs at all means they are two",
					b.Dx(), b.Dy(), px, px)
			}

			fg, bg := parseHex(norm.Foreground), parseHex(norm.Background)
			dark, light := 0, 0
			for y := range code.Size {
				for x := range code.Size {
					// The centre of the module, in pixels. Not a corner: a corner
					// is shared by four modules and would agree with the wrong
					// one under an off-by-one that this is meant to catch.
					at := image.Pt(g.origin+x*g.scale+g.scale/2, g.origin+y*g.scale+g.scale/2)
					want := bg
					if grid[y][x] {
						want = fg
						dark++
					} else {
						light++
					}
					got, _ := color.NRGBAModel.Convert(img.At(at.X, at.Y)).(color.NRGBA)
					if got != want {
						t.Fatalf("module (%d,%d) of %d: the SVG draws it %s and the PNG's "+
							"pixel at (%d,%d) is %v, want %v.\n\nThe two encoders are "+
							"drawing different pictures, which is the one thing the shared "+
							"geometry exists to prevent.",
							x, y, code.Size, drawnAs(grid[y][x]), at.X, at.Y, got, want)
					}
				}
			}

			// **And the quiet zone, every pixel of it.** It was covered by the
			// loop above while the picture was a module grid the parser could
			// walk whole; the parser walks the symbol now, so the band around it
			// is asserted here or nowhere — and it is exactly where a pixel
			// remainder can go wrong.
			symbol := code.Size * g.scale
			for y := range px {
				for x := range px {
					if x >= g.origin && x < g.origin+symbol &&
						y >= g.origin && y < g.origin+symbol {
						continue
					}
					if got, _ := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA); got != bg {
						t.Fatalf("the pixel at (%d,%d) is %v and it is in the quiet zone "+
							"of a %dpx picture whose symbol starts at %d and is %dpx "+
							"across; the zone is background or it is not a quiet zone",
							x, y, got, px, g.origin, symbol)
					}
				}
			}

			// A blank image would satisfy every comparison above if the grid it
			// was compared against were also blank. It is not — rendered()
			// already fails on a drawing with no rects — but the counts say so
			// out loud, because "both were empty" is the shape this class of
			// test fails in.
			if dark == 0 || light == 0 {
				t.Fatalf("the picture is %d dark modules and %d light ones", dark, light)
			}
		})
	}
}

func drawnAs(dark bool) string {
	if dark {
		return "dark"
	}
	return "light"
}

// TestThePNGCarriesExactlyTwoColours backs the allocation figure and the
// no-antialiasing claim in one assertion.
//
// A QR code has two colours. A rasteriser that smoothed an edge would produce a
// third, and a scanner reading a printed poster is exactly who pays for that —
// which is why the SVG carries `shape-rendering="crispEdges"` and why the PNG is
// built from filled blocks rather than from a scaled bitmap.
func TestThePNGCarriesExactlyTwoColours(t *testing.T) {
	raw, err := RenderPNG(sample, Style{Foreground: "#123a6b", Background: "#f5f7fa"})
	if err != nil {
		t.Fatal(err)
	}
	img := decodePNG(t, raw)
	if n := len(img.Palette); n != 2 {
		t.Fatalf("the PNG's palette holds %d colours, want 2: %v", n, img.Palette)
	}

	want := map[color.NRGBA]bool{parseHex("#123a6b"): true, parseHex("#f5f7fa"): true}
	for _, c := range img.Palette {
		got, _ := color.NRGBAModel.Convert(c).(color.NRGBA)
		if !want[got] {
			t.Errorf("the palette holds %v, which is neither of the style's colours", got)
		}
	}
}

// TestAPictureTooLargeToRasteriseIsRefused is the bound D11 was protecting,
// turned into a refusal.
//
// It is reachable only through a stored style: the size control cannot ask for
// more than MaxSize, but a row written before M49 carries a margin and a scale
// read forward exactly as they were, and MaxMargin at MaxScale on a long URL
// describes a picture several times the bound. The SVG for the same style still
// draws, because vector text allocates nothing of the sort.
func TestAPictureTooLargeToRasteriseIsRefused(t *testing.T) {
	huge := Style{Margin: MaxMargin, Scale: MaxScale}
	norm, errs := huge.Normalize()
	if len(errs) > 0 {
		t.Fatalf("the widest pre-M49 style is refused by Normalize: %v", errs)
	}
	// A 64-character alias, which is alias.MaxLength — the longest short URL
	// this product issues, and therefore the largest matrix a stored style can
	// be asked to draw.
	longest := "https://links.example/" + strings.Repeat("a", 64) + "?src=qr"
	code, err := Encode(longest, norm.Level)
	if err != nil {
		t.Fatal(err)
	}
	if px := OutputSize(code.Size, norm); px <= MaxSize {
		t.Fatalf("the widest style this product can have stored draws %dpx, inside the "+
			"%dpx bound; this test is no longer exercising the refusal", px, MaxSize)
	}

	if _, err := code.PNG(norm); !errors.Is(err, ErrTooLarge) {
		t.Errorf("rasterising a picture past the bound returned %v, want ErrTooLarge. "+
			"The bound is the whole of what makes reversing D11 safe: without it a "+
			"single request allocates whatever a stored style asks for", err)
	}
	if len(code.SVG(norm)) == 0 {
		t.Error("the SVG for the same style draws nothing; the bound is on the " +
			"rasteriser and vector output has no allocation to bound")
	}
}

// TestNoModuleDependencyJoinedTheSetForThePNG is m49.md's dependency bullet,
// checked where the claim actually lives.
//
// **D72 rejected a QR library for pulling `image/png` and `compress/zlib` in
// "for a PNG path this product will never call".** That path is now called, and
// the encoder is the standard library's — so the require block is what proves
// the reversal cost nothing. The list is written out rather than counted,
// because a count would pass for a swap.
func TestNoModuleDependencyJoinedTheSetForThePNG(t *testing.T) {
	// As of M49. A milestone that adds a direct dependency changes this list
	// deliberately and says why in decisions.md; a milestone that adds one by
	// accident finds out here.
	want := map[string]bool{
		"github.com/boombuler/barcode":            true,
		"github.com/caarlos0/env/v11":             true,
		"github.com/getkin/kin-openapi":           true,
		"github.com/google/uuid":                  true,
		"github.com/jackc/pgx/v5":                 true,
		"github.com/joho/godotenv":                true,
		"github.com/oschwald/maxminddb-golang/v2": true,
		"github.com/pressly/goose/v3":             true,
		"github.com/prometheus/client_golang":     true,
		"github.com/redis/go-redis/v9":            true,
		"golang.org/x/crypto":                     true,
		"golang.org/x/net":                        true,
		"golang.org/x/sync":                       true,
		"gopkg.in/yaml.v3":                        true,
	}

	for _, path := range directRequires(t) {
		if !want[path] {
			t.Errorf("go.mod requires %s directly, and M49's claim is that a PNG "+
				"download joins no module to the dependency set. If this is a "+
				"deliberate addition from a later milestone, add it to the list here "+
				"with its reason in decisions.md", path)
		}
		delete(want, path)
	}
	for path := range want {
		t.Errorf("%s is in this test's list and no longer in go.mod. A dependency "+
			"leaving is fine; a list that no longer describes the file is not", path)
	}

	// And the encoder this package actually uses is the standard library's, so
	// the assertion above is about the right thing.
	src, err := os.ReadFile("qr.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"image/png"`) {
		t.Error("internal/qr does not import image/png; the require block above is " +
			"then proving nothing about where the encoder came from")
	}
}

// directRequires is every non-indirect module path in go.mod.
//
// Parsed by hand rather than through golang.org/x/mod, which would be a module
// dependency added by the test that asserts no module dependency was added.
func directRequires(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "require (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case !inBlock, line == "", strings.HasPrefix(line, "//"):
			continue
		case strings.Contains(line, "// indirect"):
			continue
		}
		if path, _, ok := strings.Cut(line, " "); ok {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		t.Fatal("no direct requirements were parsed out of go.mod; the parser is " +
			"reading nothing and would pass for any dependency at all")
	}
	return paths
}

// Registering the PNG decoder for image.Decode above. Named rather than blank so
// the reason is attached to it: this package encodes and never decodes, and the
// import exists for the test's benefit alone.
var _ = png.Encode
