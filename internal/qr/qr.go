// Package qr encodes a string as a QR code and draws it as SVG or PNG (M41,
// M49).
//
// **It was SVG only, and that was decision D11**: the output is vector text, so
// no image encoder joined the dependency set and nothing rasterised on a
// request. The package comment said *"a PNG download, if it is ever wanted, is
// an additive change here and nowhere else"*, and M49 is that change. D11's
// premise was that the rasteriser is never called; a person who asked for a file
// calls it, so the reversal honours the reasoning rather than overriding it. The
// encoder is `image/png` from the standard library, so the dependency set is
// still what it was, and the rasteriser is bounded by [MaxSize].
//
// **One arithmetic, two encoders.** [Code.SVGClass] and [Code.PNG] both draw
// from [Code.runs] at the geometry [Code.geometry] computes, so the two outputs
// cannot round differently — which is the claim
// TestTheSVGAndThePNGAreTheSamePicture holds them to.
//
// **The encoder is github.com/boombuler/barcode, MIT, with no module
// dependencies of its own** — see decisions.md, D72, for what it was weighed
// against. This package uses it for one thing: turning a string into a matrix of
// dark and light modules. Everything a reader can see — the quiet zone, the
// colours, the size — is drawn here, because `qr_codes.style` has to drive it
// and a library's own renderer would have its own opinions instead.
//
// **Nothing an attacker controls reaches the output.** The SVG is built from
// integers and from colours that have already been parsed as `#rrggbb`, and it
// carries no title, no aria-label naming the destination, and no metadata. That
// is what makes it safe to inline into a dashboard page as `template.HTML`: the
// bytes cannot contain a `<` that did not come from this file. A QR code that
// announced its own URL to a screen reader would read better and would put a
// workspace-controlled string inside markup the template engine no longer
// escapes, so the surrounding page carries the label instead.
package qr

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"strings"

	"github.com/boombuler/barcode/qr"
)

// Level is the error-correction level, as ISO/IEC 18004 names them. A higher
// level survives more damage and costs modules, which makes the code denser at
// the same printed size.
type Level string

const (
	LevelL Level = "L" // ~7% recoverable
	LevelM Level = "M" // ~15%
	LevelQ Level = "Q" // ~25%
	LevelH Level = "H" // ~30%
)

// Levels is every level a style may name, in the order a form should offer them.
var Levels = []Level{LevelL, LevelM, LevelQ, LevelH}

// Defaults. M is the level nearly every printed QR code in the world uses, and
// four modules of quiet zone is the minimum ISO/IEC 18004 specifies — below it
// scanners start failing against busy backgrounds. Eight pixels per module puts
// a short URL at roughly 300px, which is a size a phone camera reads from a
// screen without zooming.
const (
	DefaultLevel  = LevelM
	DefaultMargin = 4
	DefaultScale  = 8

	// MinScale and MaxScale bound what a stored style may ask for. The ceiling
	// is not about pixels — SVG has none — but about the `width` attribute a
	// downloaded file carries into whatever opens it.
	MinScale = 2
	MaxScale = 32
	// MaxMargin. Beyond this the quiet zone is most of the picture.
	MaxMargin = 16

	// DefaultForeground and DefaultBackground are dark-on-light, and the
	// background is always painted rather than left transparent. A QR code
	// inverted onto a dark page is refused by a large share of scanners, and a
	// transparent one becomes inverted the moment somebody views the dashboard
	// in dark mode. So the code carries its own background and does not follow
	// the theme; the frame around it does. See decisions.md, D74.
	DefaultForeground = "#000000"
	DefaultBackground = "#ffffff"
)

// MaxContent is the longest string this package will encode. Version 40 at
// level L holds 2953 bytes, and a short URL is two orders of magnitude below
// that; the bound exists so an oversized input is a sentence rather than a
// library error nobody can act on.
const MaxContent = 1024

// MinSize and MaxSize bound the output size in pixels a caller may ask for, and
// MaxSize is also the bound on what this package will rasterise (M49).
//
// **The ceiling is what D11 was protecting.** D11 refused an image encoder
// partly so that nothing would allocate a bitmap on a request; M49 makes that
// happen, so the allocation gets a number rather than a hope. The PNG is
// [image.Paletted] over a two-colour palette — one byte per pixel — so 2000×2000
// is **4,000,000 bytes**, about 3.8 MiB, and that is the largest buffer a
// request can cause here. The SVG path allocates nothing of the sort and is
// bounded by the module count instead.
//
// 2000px is a QR code 6.7 inches across at 300 DPI, which covers the poster the
// milestone is written for. The floor is the smallest picture the existing
// bounds can produce: the shortest code is 21 modules, the quiet zone is at
// least [DefaultMargin] on each side, and [MinScale] pixels per module makes
// 58 — so 64 is a request that always has something to snap to.
//
// A request outside the range is **refused rather than clamped**, on the rule
// TestOutOfRangeSizesAreRefusedRatherThanClamped already states for margin and
// scale: clamping reports success for a setting nobody asked for.
const (
	MinSize = 64
	MaxSize = 2000
)

// ErrSizeOutOfRange is a requested output size outside [MinSize, MaxSize].
var ErrSizeOutOfRange = errors.New("output size out of range")

// ErrTooLarge is a drawing whose pixel size exceeds MaxSize. It is reachable
// only from the PNG path and only for a style stored before M49, whose margin
// and scale are read forward exactly as written and can therefore describe a
// picture larger than anything the size control will now produce.
var ErrTooLarge = errors.New("too large to rasterise")

// Style is how a code is drawn. It is what `qr_codes.style` holds, field for
// field, and the zero value is the default style rather than a blank one — a
// style row that has never been written renders exactly as a link with no row
// at all.
type Style struct {
	Foreground string `json:"foreground,omitempty"`
	Background string `json:"background,omitempty"`
	Level      Level  `json:"level,omitempty"`
	// Margin is the quiet zone, in modules.
	Margin int `json:"margin,omitempty"`
	// Scale is pixels per module, and decides only the `width` and `height`
	// attributes. The drawing itself is in module units inside a viewBox, so a
	// consumer that sizes the element with CSS gets the same code at any size.
	Scale int `json:"scale,omitempty"`
}

// Normalize fills in the defaults and returns the field errors for anything it
// cannot. It is the only way a Style reaches the renderer, so every colour in an
// SVG this package emits has been through the parser below.
func (s Style) Normalize() (Style, []FieldError) {
	var errs []FieldError
	out := Style{
		Foreground: strings.ToLower(strings.TrimSpace(s.Foreground)),
		Background: strings.ToLower(strings.TrimSpace(s.Background)),
		Level:      Level(strings.ToUpper(strings.TrimSpace(string(s.Level)))),
		Margin:     s.Margin,
		Scale:      s.Scale,
	}

	if out.Foreground == "" {
		out.Foreground = DefaultForeground
	} else if !validHex(out.Foreground) {
		errs = append(errs, FieldError{"foreground", "invalid",
			"a colour is #rgb or #rrggbb; anything else would be markup inside the drawing"})
	}
	if out.Background == "" {
		out.Background = DefaultBackground
	} else if !validHex(out.Background) {
		errs = append(errs, FieldError{"background", "invalid",
			"a colour is #rgb or #rrggbb; anything else would be markup inside the drawing"})
	}
	if out.Foreground == out.Background && validHex(out.Foreground) {
		errs = append(errs, FieldError{"foreground", "invalid",
			"the code and its background are the same colour, which no scanner can read"})
	}

	switch out.Level {
	case "":
		out.Level = DefaultLevel
	case LevelL, LevelM, LevelQ, LevelH:
	default:
		errs = append(errs, FieldError{"level", "invalid",
			"error correction is one of L, M, Q or H"})
	}

	if out.Margin == 0 {
		out.Margin = DefaultMargin
	}
	if out.Margin < 0 || out.Margin > MaxMargin {
		errs = append(errs, FieldError{"margin", "out_of_range",
			fmt.Sprintf("the quiet zone is 0 to %d modules; %d is not a picture of a code",
				MaxMargin, out.Margin)})
	}
	if out.Scale == 0 {
		out.Scale = DefaultScale
	}
	if out.Scale < MinScale || out.Scale > MaxScale {
		errs = append(errs, FieldError{"scale", "out_of_range",
			fmt.Sprintf("a module is %d to %d pixels wide", MinScale, MaxScale)})
	}
	return out, errs
}

// FieldError is one thing wrong with a style. Deliberately its own type rather
// than domain.FieldError: this package draws pictures and knows nothing about
// HTTP, and the service that calls it converts.
type FieldError struct {
	Field   string
	Code    string
	Message string
}

// Code is an encoded matrix, before anything has been drawn.
type Code struct {
	// Size is the width of the matrix in modules, quiet zone excluded.
	Size    int
	modules []bool
}

// Dark reports whether the module at (x, y) is dark. Out of range is light,
// which is what the quiet zone is.
func (c *Code) Dark(x, y int) bool {
	if x < 0 || y < 0 || x >= c.Size || y >= c.Size {
		return false
	}
	return c.modules[y*c.Size+x]
}

// ErrTooLong is returned for content past MaxContent.
var ErrTooLong = errors.New("too long to encode as a QR code")

// Encode turns content into a matrix at the style's error-correction level.
//
// The style's colours and sizes do not reach here: they change the drawing, not
// the encoding, which is why a workspace re-styling its code cannot change what
// the code says.
func Encode(content string, level Level) (*Code, error) {
	if content == "" {
		return nil, errors.New("nothing to encode")
	}
	if len(content) > MaxContent {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrTooLong, len(content), MaxContent)
	}
	var ec qr.ErrorCorrectionLevel
	switch level {
	case LevelL:
		ec = qr.L
	case LevelQ:
		ec = qr.Q
	case LevelH:
		ec = qr.H
	default:
		ec = qr.M
	}
	// Auto picks the most compact of numeric, alphanumeric and byte mode for the
	// content. A short URL is byte mode; an all-uppercase one is alphanumeric and
	// comes out a version smaller, which is a smaller picture for free.
	bc, err := qr.Encode(content, ec, qr.Auto)
	if err != nil {
		return nil, fmt.Errorf("encode qr: %w", err)
	}
	b := bc.Bounds()
	size := b.Dx()
	if size <= 0 || b.Dy() != size {
		return nil, fmt.Errorf("encode qr: matrix is %dx%d", b.Dx(), b.Dy())
	}
	code := &Code{Size: size, modules: make([]bool, size*size)}
	for y := range size {
		for x := range size {
			// The encoder hands back an image; dark is black. Comparing against
			// color.Black rather than converting to grey keeps this exact, and a
			// library that ever returned a third colour would show up as a code
			// that does not scan rather than as one silently half-drawn.
			if bc.At(b.Min.X+x, b.Min.Y+y) == color.Color(color.Black) {
				code.modules[y*size+x] = true
			}
		}
	}
	return code, nil
}

// Render encodes content and draws it, in one call, for the common case.
func Render(content string, style Style) ([]byte, error) {
	return RenderClass(content, style, "")
}

// RenderClass is Render with a CSS class on the root `<svg>` (M48).
//
// **Why a class is worth an entry point of its own.** `Scale` sizes the drawing
// in pixels, and the pixel count is a function of the *encoded version* — a
// longer URL is a bigger matrix, so the same style produces a taller picture for
// a longer link. That is fine for a code somebody scans and wrong for one drawn
// into a fixed row of a page, where the height has to be a property of the page
// rather than of the data. A class is how a caller states it.
//
// The class is **validated, not trusted**, which is what keeps the package
// comment's promise true — the bytes of an SVG this package emits cannot contain
// a `<` that did not come from this file. `validClass` accepts the characters a
// class list is made of and nothing that could close an attribute or a tag, so a
// caller cannot smuggle markup through it whatever it passes.
//
// An empty class writes no attribute at all, which is what Render relies on.
func RenderClass(content string, style Style, class string) ([]byte, error) {
	if !validClass(class) {
		return nil, fmt.Errorf("qr class: %q is not a class list", class)
	}
	st, errs := style.Normalize()
	if len(errs) > 0 {
		return nil, fmt.Errorf("qr style: %s", errs[0].Message)
	}
	code, err := Encode(content, st.Level)
	if err != nil {
		return nil, err
	}
	return code.SVGClass(st, class), nil
}

// SVG draws the matrix, with no class on the root element.
func (c *Code) SVG(st Style) []byte { return c.SVGClass(st, "") }

// SVGClass draws the matrix. The style must already be normalized and the class
// must already have been checked — Render and RenderClass are the entry points
// that guarantee both, and passing an unchecked one is a programming error
// rather than a runtime one.
//
// **Dark modules are drawn as horizontal runs, one rect per run.** A rect per
// module is the obvious shape and produces roughly ten times the bytes for a
// version-10 code; a single path with move-and-draw commands is smaller still
// and cannot be read back by anything simpler than a path parser. Runs are the
// middle: about a quarter of the size of per-module rects, and a shape whose
// test can reconstruct the matrix and compare it to the encoder's.
func (c *Code) SVGClass(st Style, class string) []byte {
	g := c.geometry(st)
	span, px := g.span, g.px

	var b bytes.Buffer
	b.Grow(1024 + c.Size*c.Size/2)
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" `)
	if class != "" {
		b.WriteString(`class="`)
		b.WriteString(class)
		b.WriteString(`" `)
	}
	b.WriteString(`width="`)
	b.WriteString(strconv.Itoa(px))
	b.WriteString(`" height="`)
	b.WriteString(strconv.Itoa(px))
	b.WriteString(`" viewBox="0 0 `)
	b.WriteString(strconv.Itoa(span))
	b.WriteString(` `)
	b.WriteString(strconv.Itoa(span))
	// crispEdges, because the modules are axis-aligned squares and antialiasing
	// their edges is what makes a code scan badly when it is drawn small.
	// role="img" with no label: the page names the picture, not the picture.
	b.WriteString(`" shape-rendering="crispEdges" role="img">`)

	// The background covers the quiet zone as well as the code, which is the
	// whole reason it is drawn: an unpainted quiet zone is the page's colour,
	// and on a dark page that is a code with no quiet zone at all.

	b.WriteString(`<rect width="`)
	b.WriteString(strconv.Itoa(span))
	b.WriteString(`" height="`)
	b.WriteString(strconv.Itoa(span))
	b.WriteString(`" fill="`)
	b.WriteString(st.Background)
	b.WriteString(`"/>`)

	b.WriteString(`<g fill="`)
	b.WriteString(st.Foreground)
	b.WriteString(`">`)
	c.runs(func(x, y, width int) {
		b.WriteString(`<rect x="`)
		b.WriteString(strconv.Itoa(x + st.Margin))
		b.WriteString(`" y="`)
		b.WriteString(strconv.Itoa(y + st.Margin))
		b.WriteString(`" width="`)
		b.WriteString(strconv.Itoa(width))
		b.WriteString(`" height="1"/>`)
	})
	b.WriteString(`</g></svg>`)
	return b.Bytes()
}

// geometry is the arithmetic both encoders draw from (M49).
//
// It exists so that "the SVG and the PNG describe the same image" is true by
// construction rather than by two implementations agreeing. Everything either
// encoder needs to place a module is here, computed once from the style: a
// second copy of `margin*scale` in the rasteriser is exactly the drift the claim
// forbids.
//
// The style must already be normalized.
type geometry struct {
	// span is the picture's width in modules, quiet zone included.
	span int
	// scale is pixels per module.
	scale int
	// px is the output size — the picture is square, so one number.
	px int
	// origin is the offset in pixels from the picture's edge to the first
	// module of the code, which is the quiet zone drawn in pixels.
	origin int
}

func (c *Code) geometry(st Style) geometry {
	span := c.Size + 2*st.Margin
	return geometry{
		span:   span,
		scale:  st.Scale,
		px:     span * st.Scale,
		origin: st.Margin * st.Scale,
	}
}

// runs walks the dark modules as horizontal runs, in module coordinates with the
// quiet zone excluded — (0,0) is the code's top-left module, not the picture's.
//
// **Both encoders walk this and nothing else.** The SVG writes one `<rect>` per
// run and the PNG fills one block per run, so a module either encoder drew and
// the other did not would have to come from here, where there is one of it.
// Runs rather than modules for the reason SVGClass gives: about a quarter of the
// bytes of a rect per module, and a shape a test can reconstruct.
func (c *Code) runs(fn func(x, y, width int)) {
	for y := range c.Size {
		x := 0
		for x < c.Size {
			if !c.Dark(x, y) {
				x++
				continue
			}
			run := 1
			for x+run < c.Size && c.Dark(x+run, y) {
				run++
			}
			fn(x, y, run)
			x += run
		}
	}
}

// ContentType is what a QR response is served as, and PNGContentType is the
// second one since M49.
const (
	ContentType    = "image/svg+xml"
	PNGContentType = "image/png"
)

// RenderPNG encodes content and rasterises it, the way Render draws it (M49).
func RenderPNG(content string, style Style) ([]byte, error) {
	st, errs := style.Normalize()
	if len(errs) > 0 {
		return nil, fmt.Errorf("qr style: %s", errs[0].Message)
	}
	code, err := Encode(content, st.Level)
	if err != nil {
		return nil, err
	}
	return code.PNG(st)
}

// PNG rasterises the matrix at the same geometry SVGClass draws it.
//
// **A paletted image, two colours, and that is not a size optimisation.** It is
// what makes the allocation MaxSize's comment states — one byte per pixel rather
// than four — and it is also what makes the output honest: a QR code has exactly
// two colours, so an RGBA buffer would be three quarters padding and would let
// an antialiasing bug produce a third colour without anything noticing. Go's PNG
// encoder writes a two-entry palette at one bit per pixel, so the file is small
// as a side effect rather than as an aim.
//
// The style must already be normalized — RenderPNG is the entry point that
// guarantees it, on the same terms SVGClass is written for.
func (c *Code) PNG(st Style) ([]byte, error) {
	g := c.geometry(st)
	if g.px > MaxSize {
		return nil, fmt.Errorf("%w: the style draws %dpx and the bound is %dpx",
			ErrTooLarge, g.px, MaxSize)
	}

	// Index 0 is the background, which is why nothing paints the quiet zone
	// here: NewPaletted zeroes the pixels, so the whole picture starts as the
	// background colour and D74's painted quiet zone comes out of that for free.
	img := image.NewPaletted(image.Rect(0, 0, g.px, g.px), color.Palette{
		parseHex(st.Background), parseHex(st.Foreground),
	})
	c.runs(func(x, y, width int) {
		left := g.origin + x*g.scale
		top := g.origin + y*g.scale
		for py := top; py < top+g.scale; py++ {
			row := img.Pix[py*img.Stride+left : py*img.Stride+left+width*g.scale]
			for i := range row {
				row[i] = 1
			}
		}
	})

	var b bytes.Buffer
	b.Grow(1024 + g.px*g.px/64)
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&b, img); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	return b.Bytes(), nil
}

// OutputSize is the width in pixels a normalized style draws a code of `modules`
// modules at. The picture is square, so one number (M49).
//
// This is the read direction of the size control: a style stored before M49
// carries a margin and a scale and no size, and the size it means is whatever
// those two already produce.
func OutputSize(modules int, st Style) int {
	return (modules + 2*st.Margin) * st.Scale
}

// SizeFit is a requested output size resolved onto a whole number of modules.
type SizeFit struct {
	// Requested is the size in pixels that was asked for.
	Requested int
	// Size is the size in pixels the geometry below actually produces.
	Size int
	// Margin is the quiet zone in modules, and Scale the pixels per module,
	// that a Style should carry to draw at Size.
	Margin int
	Scale  int
}

// Snapped reports whether the fit had to move off the requested size.
func (f SizeFit) Snapped() bool { return f.Size != f.Requested }

// FitSize resolves a requested output size in pixels to the quiet zone and the
// pixels-per-module that come nearest it (M49).
//
// **A QR grid is a whole number of modules, so an arbitrary pixel size does not
// divide evenly**: 300px over a 29-module code with the minimum quiet zone is
// 10.34 pixels per module. Drawing it anyway would put module boundaries on
// fractional pixels, where the SVG's rasteriser and the PNG's rounding are free
// to disagree — which is precisely what the two-outputs-match claim forbids. So
// the size snaps, and the caller is told what it snapped to.
//
// **Two knobs, not one.** The quiet zone is derived here rather than fixed,
// because a margin one module wider is another `2*scale` pixels of reach and it
// costs nothing a scanner cares about: the ISO/IEC 18004 floor is four modules
// and this only ever goes up from there. 300px on that 29-module code lands on
// 301 with a 7-module quiet zone where the floor alone would have given 296.
//
// Ties go to the smaller picture and then to the smaller quiet zone — under
// rather than over, so a request at the ceiling cannot snap past it, and the
// largest code that fits rather than the same code with more white around it.
func FitSize(modules, want int) (SizeFit, error) {
	if modules <= 0 {
		return SizeFit{}, fmt.Errorf("qr: a code of %d modules has no size", modules)
	}
	if want < MinSize || want > MaxSize {
		return SizeFit{}, fmt.Errorf("%w: an output size is %d to %d pixels; %d is not",
			ErrSizeOutOfRange, MinSize, MaxSize, want)
	}

	best := SizeFit{Requested: want}
	found := false
	for margin := DefaultMargin; margin <= MaxMargin; margin++ {
		span := modules + 2*margin
		for scale := MinScale; scale <= MaxScale; scale++ {
			size := span * scale
			if size > MaxSize {
				break
			}
			c := SizeFit{Requested: want, Size: size, Margin: margin, Scale: scale}
			if !found || nearer(c, best) {
				best, found = c, true
			}
		}
	}
	if !found {
		// Unreachable for any code this package can encode — 21 modules at the
		// floor and MinScale is 58px, and version 40 is 370px — but a bound that
		// is only true by argument is one a future MaxSize change breaks
		// silently.
		return SizeFit{}, fmt.Errorf("%w: no whole-module size for a %d-module code",
			ErrSizeOutOfRange, modules)
	}
	return best, nil
}

// nearer is FitSize's comparison, spelled out so the tie-breaks are readable
// rather than encoded in the loop order.
func nearer(a, b SizeFit) bool {
	da, db := abs(a.Size-a.Requested), abs(b.Size-b.Requested)
	switch {
	case da != db:
		return da < db
	case a.Size != b.Size:
		return a.Size < b.Size
	default:
		return a.Margin < b.Margin
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// parseHex turns a colour that has already been through validHex into a pixel.
//
// It repairs nothing and reports nothing, because there is nothing to report: an
// unparseable colour cannot reach here. Normalize is the only way a Style
// arrives at either encoder and it refuses anything validHex does not accept, so
// this reads a string whose shape is already known.
func parseHex(s string) color.NRGBA {
	if len(s) == 4 {
		// #rgb is #rrggbb with each digit doubled, which is what CSS means by it
		// — #f00 is #ff0000 and not #f00000.
		s = string([]byte{'#', s[1], s[1], s[2], s[2], s[3], s[3]})
	}
	if len(s) != 7 {
		return color.NRGBA{A: 255}
	}
	return color.NRGBA{R: hexByte(s[1], s[2]), G: hexByte(s[3], s[4]), B: hexByte(s[5], s[6]), A: 255}
}

func hexByte(hi, lo byte) byte { return nibble(hi)<<4 | nibble(lo) }

func nibble(c byte) byte {
	if c >= 'a' {
		return c - 'a' + 10
	}
	return c - '0'
}

// validClass accepts a space-separated list of the characters a utility class
// is made of, and nothing else. It is the second gate — beside validHex — between
// a caller and the bytes of an SVG, and it is what lets the package comment go
// on saying that nothing an attacker controls reaches the output.
//
// Deliberately narrower than HTML allows. A quote, an angle bracket or a
// backslash would each be a way out of the attribute, and none of them appears
// in a class name anybody writes; a name that needs one is a change to make here
// with a reason, not a hole to leave open in advance. The same goes for
// Tailwind's arbitrary values — `h-[6rem]` is refused, and `h-24` is the way to
// say it.
func validClass(s string) bool {
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == ' ':
		default:
			return false
		}
	}
	return true
}

// validHex accepts #rgb and #rrggbb, lowercase, and nothing else. It is the one
// gate between a stored style and the bytes of an SVG, so it rejects rather than
// repairs: `red`, `rgb(1,2,3)` and `url(#x)` are all valid CSS paint values and
// none of them is a colour this package will write.
func validHex(s string) bool {
	if len(s) != 4 && len(s) != 7 {
		return false
	}
	if s[0] != '#' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
