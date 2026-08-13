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
	"image/draw"
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

	// MinScale and MaxScale bound what a stored style may carry. The ceiling is
	// derived from [MaxSize]: the smallest code is 21 modules, the narrowest
	// quiet zone adds 2×[MinMarginModules], and floor(2048/27) = 75 is the
	// largest pixels-per-module [FitSize] can ever emit — so every fit is a
	// style Normalize accepts. It was 32, capped by nothing but the `width`
	// attribute a downloaded file carries, and that cap is what made the old
	// FitSize fill large requests with quiet zone instead of scale (F213); it
	// was 68 while the quiet zone was pinned at four modules and [MaxSize] was
	// 2000, and both of those moved at the second M49 reopening (F221).
	MinScale = 2
	MaxScale = 75
	// MaxMargin bounds the quiet zone a *stored* style may carry in modules,
	// and since the M49 reopening that is all it bounds: [FitSize] writes the
	// quiet zone in pixels now, and 16 stays only because rows written by the
	// old search carry up to it and must keep rendering as written.
	MaxMargin = 16
	// MinMarginModules is the narrowest quiet zone [FitSize] will produce, and
	// it is **below** the four modules ISO/IEC 18004 specifies (D182).
	//
	// Four ±25% is the owner's band, and three is its low end. Being under the
	// specification, it is measured rather than argued: `make verify-scan`
	// renders the corpus at this quiet zone across the whole version range and
	// decodes it through two pinned decoders at five simulated distances, the
	// same instrument M50.6's logo fraction rests on. The result is in
	// decisions.md; a change to this number is a change that has to be
	// re-measured, not re-reasoned.
	MinMarginModules = 3

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
// [image.Paletted] over a two-colour palette — one byte per pixel — so 2048×2048
// is **4,194,304 bytes**, 4 MiB exactly, and that is the largest buffer a
// request can cause here. The SVG path allocates nothing of the sort and is
// bounded by the module count instead.
//
// **2048 rather than 2000 is owner-instructed** (D182), so the top of the size
// control is a round power of two; it is a QR code 6.8 inches across at 300 DPI,
// which covers the poster the milestone is written for.
//
// The floor is the smallest picture the bounds can produce at all: the shortest
// code is 21 modules, the narrowest quiet zone is [MinMarginModules] a side, and
// [MinScale] pixels per module makes 54 — so 64 is a request the shortest code
// can always draw. **It is not a floor every code can draw**, because the number
// of pixels a symbol needs is a property of the symbol: see [MinSizeFor], which
// is what a request below a particular code's own floor is refused against.
//
// A request outside the range is **refused rather than clamped**, on the rule
// TestOutOfRangeSizesAreRefusedRatherThanClamped already states for margin and
// scale: clamping reports success for a setting nobody asked for.
const (
	MinSize = 64
	MaxSize = 2048
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
	// Margin is the quiet zone, in modules. It is what the picture is built
	// from only when Size is unset — see Size.
	Margin int `json:"margin,omitempty"`
	// Scale is pixels per module. It is the one number both forms of this
	// struct share, because a module is a whole number of pixels in either.
	Scale int `json:"scale,omitempty"`
	// Size is the output size in pixels, and when it is set it is what the
	// picture measures — exactly, at every value (D182).
	//
	// **Two forms of the same geometry, and this one is the newer.** Before the
	// second M49 reopening a style carried Margin and Scale and the picture was
	// whatever those multiplied out to, which is why a requested size could only
	// be honoured by snapping it to the nearest one the module grid admitted.
	// The way out is that **only the symbol needs whole modules**: the quiet
	// zone is white space and can be any pixel count at all. So the picture is
	// Size pixels across, the symbol is `modules × Scale` of them centred inside
	// it, and the remainder is the quiet zone — carried in pixels, which is what
	// makes every requested size reachable.
	//
	// Zero is unset and means the older form, which is what every row written
	// before this milestone carries and what [Code.geometry] falls back to. It
	// is also the fallback for a stored Size the symbol has since outgrown — a
	// link whose alias grew encodes to a larger matrix, and a picture that can
	// no longer hold its own symbol is not a picture.
	Size int `json:"size,omitempty"`
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
		Size:       s.Size,
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
	// Zero is unset — the older Margin-and-Scale form — and anything else is a
	// picture size, bounded by the same two numbers the size control is. There
	// is no check here that the symbol fits inside it, because that needs a
	// module count this function does not have and must not refuse a row over:
	// [Code.geometry] falls back for a size a symbol has outgrown.
	if out.Size != 0 && (out.Size < MinSize || out.Size > MaxSize) {
		errs = append(errs, FieldError{"size", "out_of_range",
			fmt.Sprintf("an output size is %d to %d pixels; %d is not",
				MinSize, MaxSize, out.Size)})
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

// FluidClass is the class an inlined code carries so that the box it is drawn
// into can shrink it (F184).
//
// **`width` and `height` are pixels and the page is not.** The drawing sits
// inside a viewBox of the same extent, so the two attributes decide an intrinsic
// size and nothing else — a consumer that sizes the element with CSS gets the
// same code at any size. What they *also* decide, for a consumer that sizes nothing,
// is how far the element reaches: a 488px enrolment code inside a 160px frame
// took `/account/mfa` 174px past a 360px viewport and made it the one page in
// the dashboard that scrolled sideways. `max-w-full` is the constraint and
// `h-auto` is what keeps the picture square while it applies.
//
// Tailwind's names, in a package that knows nothing else about the dashboard,
// because the alternative is an inline `style` attribute and the dashboard's
// CSP is `style-src 'self'` with no `unsafe-inline`. Both utilities are already
// in the generated stylesheet, so no build step depends on this constant.
// Anything that wants the bare document — a file somebody downloads — asks for
// it, by naming the empty class through [RenderClass]; see
// link.Service.RenderQRBySlug.
const FluidClass = "max-w-full h-auto"

// Render encodes content and draws it, in one call, for the common case.
//
// **The common case is a code inlined into a page**, so it carries [FluidClass]
// and fits the box it is put in. A caller that has stated the element's size
// itself passes its own class to [RenderClass] — see ui.QRThumbClass, which
// fixes the link page's thumbnail at 6rem — and a caller that wants no class at
// all passes the empty string, which writes no attribute.
func Render(content string, style Style) ([]byte, error) {
	return RenderClass(content, style, FluidClass)
}

// RenderWithLogo draws a code with an image composited into the middle of it
// (M50.6). A nil logo is Render, [FluidClass] and all.
func RenderWithLogo(content string, style Style, logo []byte) ([]byte, error) {
	return RenderClassWithLogo(content, style, FluidClass, logo)
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
	return RenderClassWithLogo(content, style, class, nil)
}

// RenderClassWithLogo is RenderClass with an image composited into the middle
// (M50.6).
//
// **The level is forced to H whenever there is a logo**, here rather than only
// at the service that stores the style: a logo occludes modules, and the cap
// composite.go derives is a cap against H's correction budget. A row that says
// otherwise draws at H anyway, so the picture and the claim cannot come apart.
func RenderClassWithLogo(content string, style Style, class string, logo []byte) ([]byte, error) {
	if !validClass(class) {
		return nil, fmt.Errorf("qr class: %q is not a class list", class)
	}
	st, errs := style.Normalize()
	if len(errs) > 0 {
		return nil, fmt.Errorf("qr style: %s", errs[0].Message)
	}
	if len(logo) > 0 {
		st = st.ForLogo()
	}
	code, err := Encode(content, st.Level)
	if err != nil {
		return nil, err
	}
	if len(logo) == 0 {
		return code.SVGClass(st, class), nil
	}
	drawing, err := code.prepareLogo(st, logo)
	if err != nil {
		return nil, err
	}
	return code.svg(st, class, drawing), nil
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
func (c *Code) SVGClass(st Style, class string) []byte { return c.svg(st, class, nil) }

// svg is SVGClass with the logo it may have to composite. A nil drawing is the
// picture M49 shipped, byte for byte.
func (c *Code) svg(st Style, class string, logo *logoDrawing) []byte {
	g := c.geometry(st)
	px := g.px

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
	// **The viewBox is in pixels, and it was in modules until D182.** A quiet
	// zone measured in modules is expressible in a module viewBox and one
	// measured in pixels is not — a picture whose remainder is 47 pixels of a
	// 14-pixel module has no whole-module coordinate for the symbol's corner. So
	// the drawing moved into the unit both encoders already share, which is also
	// the unit `width` and `height` are in. Nothing a consumer can see changes:
	// the viewBox still maps onto those attributes exactly, so an element sized
	// with CSS still draws the same code at any size.
	b.WriteString(`" viewBox="0 0 `)
	b.WriteString(strconv.Itoa(px))
	b.WriteString(` `)
	b.WriteString(strconv.Itoa(px))
	// crispEdges, because the modules are axis-aligned squares and antialiasing
	// their edges is what makes a code scan badly when it is drawn small.
	// role="img" with no label: the page names the picture, not the picture.
	b.WriteString(`" shape-rendering="crispEdges" role="img">`)

	// The background covers the quiet zone as well as the code, which is the
	// whole reason it is drawn: an unpainted quiet zone is the page's colour,
	// and on a dark page that is a code with no quiet zone at all.

	b.WriteString(`<rect width="`)
	b.WriteString(strconv.Itoa(px))
	b.WriteString(`" height="`)
	b.WriteString(strconv.Itoa(px))
	b.WriteString(`" fill="`)
	b.WriteString(st.Background)
	b.WriteString(`"/>`)

	b.WriteString(`<g fill="`)
	b.WriteString(st.Foreground)
	b.WriteString(`">`)
	c.runs(func(x, y, width int) {
		b.WriteString(`<rect x="`)
		b.WriteString(strconv.Itoa(g.origin + x*g.scale))
		b.WriteString(`" y="`)
		b.WriteString(strconv.Itoa(g.origin + y*g.scale))
		b.WriteString(`" width="`)
		b.WriteString(strconv.Itoa(width * g.scale))
		b.WriteString(`" height="`)
		b.WriteString(strconv.Itoa(g.scale))
		b.WriteString(`"/>`)
	})
	b.WriteString(`</g>`)
	// After the modules rather than instead of them: the box is painted over
	// whatever it covers, so what is occluded is the whole box regardless of the
	// logo's own shape — which is the area composite.go's cap is a cap on.
	if logo != nil {
		logo.writeSVG(&b, st, g)
	}
	b.WriteString(`</svg>`)
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
	// scale is pixels per module.
	scale int
	// px is the output size — the picture is square, so one number.
	px int
	// origin is the offset in pixels from the picture's edge to the first
	// module of the code, which is the quiet zone drawn in pixels.
	origin int
}

func (c *Code) geometry(st Style) geometry { return fitGeometry(c.Size, st) }

// fitGeometry is [Code.geometry] for a module count on its own, which is what
// [OutputSize] needs and what keeps the two answers one piece of arithmetic.
//
// **Two forms, and the newer one is a single subtraction** (D182). A style
// carrying [Style.Size] draws a picture that many pixels across with the symbol
// centred in it, so the quiet zone is the remainder and lands wherever the
// division leaves it — including on a half pixel, which is why `origin` is the
// floor and the far side of the picture carries the odd pixel. A style without
// one is the pre-reopening arithmetic, byte for byte: the picture is the symbol
// plus [Style.Margin] modules of quiet zone on each side.
//
// The fallback also catches a stored size the symbol has outgrown. It is
// deliberately not a refusal: the row is a preference somebody expressed about a
// picture, and a link renamed into a longer alias should draw a larger code
// rather than none.
func fitGeometry(modules int, st Style) geometry {
	symbol := modules * st.Scale
	if st.Size >= symbol+2*MinMarginModules*st.Scale {
		return geometry{scale: st.Scale, px: st.Size, origin: (st.Size - symbol) / 2}
	}
	origin := st.Margin * st.Scale
	return geometry{scale: st.Scale, px: symbol + 2*origin, origin: origin}
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
	return RenderPNGWithLogo(content, style, nil)
}

// RenderPNGWithLogo is RenderPNG with an image composited into the middle
// (M50.6). A nil logo is RenderPNG, down to the paletted output.
func RenderPNGWithLogo(content string, style Style, logo []byte) ([]byte, error) {
	st, errs := style.Normalize()
	if len(errs) > 0 {
		return nil, fmt.Errorf("qr style: %s", errs[0].Message)
	}
	if len(logo) > 0 {
		st = st.ForLogo()
	}
	code, err := Encode(content, st.Level)
	if err != nil {
		return nil, err
	}
	if len(logo) == 0 {
		return code.PNG(st)
	}
	if px := OutputSize(code.Size, st); px > MaxSize {
		return nil, fmt.Errorf("%w: the style draws %dpx and the bound is %dpx",
			ErrTooLarge, px, MaxSize)
	}
	drawing, err := code.prepareLogo(st, logo)
	if err != nil {
		return nil, err
	}
	return code.pngWithLogo(st, drawing)
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

// pngWithLogo rasterises the matrix with an image composited into the middle
// (M50.6).
//
// **A logo costs the paletted form, and the allocation figure with it.** A QR
// code has two colours and a logo has whatever it has, so this buffer is
// [image.NRGBA] at four bytes a pixel: at [MaxSize] that is 2048 × 2048 × 4 =
// **16,777,216 bytes**, four times the 4,194,304 the two-colour path allocates
// and the number that replaces it whenever a code carries a logo. The resampled
// logo is bounded separately, at [MaxLogoRasterSide]² × 4 = 1,048,576 — which is
// [MaxLogoPixels] × 4, the figure M50.5 already derived. **Since M50.6's
// 2026-08-12 reopening a second resampled buffer can join it**: the box is now
// three tenths of the symbol's width, so at [MaxSize] it is 614 pixels and
// exceeds MaxLogoRasterSide, and [logoDrawing.drawPNG] scales the clamped raster
// up to what the box needs. That one is bounded by the box itself —
// MaxSize·[LogoBoxNumerator]/[LogoBoxDenominator] squared × 4 = 1,507,984 — and
// neither is the largest buffer here, which is still the picture.
//
// **This path is reachable only from [RenderPNGWithLogo]**, which checks the
// size bound before anything is allocated, exactly as [Code.PNG] does.
func (c *Code) pngWithLogo(st Style, logo *logoDrawing) ([]byte, error) {
	g := c.geometry(st)
	if g.px > MaxSize {
		return nil, fmt.Errorf("%w: the style draws %dpx and the bound is %dpx",
			ErrTooLarge, g.px, MaxSize)
	}

	img := image.NewNRGBA(image.Rect(0, 0, g.px, g.px))
	// The background is painted rather than left at the zero value, which for
	// NRGBA is transparent black — D74's whole point is that a code carries its
	// own background, and a transparent one inverts itself on a dark page.
	draw.Draw(img, img.Bounds(), &image.Uniform{C: parseHex(st.Background)},
		image.Point{}, draw.Src)

	fg := image.NewUniform(parseHex(st.Foreground))
	c.runs(func(x, y, width int) {
		left := g.origin + x*g.scale
		top := g.origin + y*g.scale
		draw.Draw(img, image.Rect(left, top, left+width*g.scale, top+g.scale),
			fg, image.Point{}, draw.Src)
	})
	logo.drawPNG(img, st, g)

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
// This is the read direction of the size control, and it answers for both forms
// of a style: one written since D182 carries the size and this returns it, and
// one written before carries a margin and a scale and the size it means is
// whatever those two already produce.
func OutputSize(modules int, st Style) int { return fitGeometry(modules, st).px }

// SizeFit is a requested output size resolved onto a style that draws it.
type SizeFit struct {
	// Size is the size in pixels, and since D182 it is the size that was asked
	// for — this field exists because a caller needs the number, not because it
	// can differ from the request.
	Size int
	// Scale is the pixels per module, and Margin the quiet zone **in pixels on
	// the near side** — the left and the top.
	//
	// Near side, because the far side carries the odd pixel when the remainder
	// is odd: 71 pixels over a 25-module symbol at 2 pixels a module leaves 21,
	// which is 10 on the left and 11 on the right. Centring it perfectly would
	// mean giving up a pixel of the requested size, and the requested size is
	// the thing this whole arithmetic exists to keep. One pixel of asymmetry in
	// a quiet zone is invisible and costs a scanner nothing; a size that came
	// back one short of what was typed is the defect being fixed.
	Margin int
	Scale  int
}

// MinSizeFor is the smallest picture a code of `modules` modules can be drawn
// at: the symbol at [MinScale], plus [MinMarginModules] of quiet zone a side.
//
// **A floor per code rather than one constant, because the pixels a symbol needs
// are a property of the symbol.** [MinSize] is 64 and a 29-module code cannot be
// drawn at 64 with a quiet zone that scans — it needs 70. The alternative was
// raising MinSize until it covered every code, which at version 40 is 366 and
// would refuse five sixths of the range the product accepts today for the sake
// of payloads no link in it produces. So the global floor stays the control's
// and this is the code's, and a request between the two is refused with this
// number in the sentence.
func MinSizeFor(modules int) int {
	return MinSizeForStyle(modules, Style{Scale: MinScale})
}

// MinSizeForStyle is [MinSizeFor] for a style that has already fixed the module
// width: the smallest picture a code of `modules` modules can be drawn at with
// this style's [Style.Scale], and so the floor [Style.Size] must clear for the
// drawn size to be the requested one.
//
// **Two floors, both real, and which one binds depends on who is asking.**
// MinSizeFor is the floor over every scale, and it is the size control's,
// because the control chooses the scale itself and will take the smallest one
// before it gives up. This is the floor for a caller who set the scale as well,
// which is what the API accepts: a style is `size` *and* `scale`, and
// [fitGeometry] draws the requested size only while the symbol and its quiet
// zone fit inside it — below that it falls back to margin-and-scale and the
// picture measures something else. MinSizeFor is this function at [MinScale].
func MinSizeForStyle(modules int, st Style) int {
	return st.Scale * (modules + 2*MinMarginModules)
}

// FitSize resolves a requested output size in pixels to a style that draws
// exactly it (M49; arithmetic replaced twice, at the 2026-08-12 reopenings
// F213 and F221).
//
// **The requested size is the size drawn, at every value in the range**, and
// this is the second answer to the question — D179 pinned the quiet zone at four
// modules and put the rounding remainder into the drawn size, which is the
// behaviour the owner used and rejected: *"the number set is where it should
// stay, the quiet zone should be reduced to fit"*. D182 is what makes that
// possible, and it is one observation: **only the symbol needs whole modules.**
// The quiet zone is white space, so it can be any pixel count at all —
//
//	size = modules·scale + 2·margin_px
//
// is satisfiable at every size, with `scale` the only integer to choose.
//
// **The scale chosen is the one whose remainder puts the quiet zone nearest
// [DefaultMargin] modules**, subject to never leaving less than
// [MinMarginModules]. That floor is the binding constraint and the objective is
// not: at a coarse scale — a large symbol in a small picture — the two
// candidates either side of four modules can be three tenths of a module and
// twenty-six of them, and this takes the wide one. The consequence is stated
// rather than hidden: the quiet zone lands inside the owner's 3-to-5 band
// wherever the grid admits one, and where it does not it errs **wide**, which
// costs white space and never scannability. TestTheQuietZoneLandsInTheBand is
// where the condition is written down and measured.
//
// A tie goes to the larger scale, so a picture is never looser than it needs to
// be.
func FitSize(modules, want int) (SizeFit, error) {
	if modules <= 0 {
		return SizeFit{}, fmt.Errorf("qr: a code of %d modules has no size", modules)
	}
	if want < MinSize || want > MaxSize {
		return SizeFit{}, fmt.Errorf("%w: an output size is %d to %d pixels; %d is not",
			ErrSizeOutOfRange, MinSize, MaxSize, want)
	}
	if floor := MinSizeFor(modules); want < floor {
		return SizeFit{}, fmt.Errorf(
			"%w: a %d-module code is %d pixels of symbol at the smallest module size and "+
				"still needs a quiet zone, so it starts at %d pixels; %d is below that",
			ErrSizeOutOfRange, modules, modules*MinScale, floor, want)
	}

	// The ceiling: any larger and the quiet zone falls below the floor. The
	// refusal above is exactly the case where this is under MinScale.
	top := min(MaxScale, want/(modules+2*MinMarginModules))
	best, bestErr := MinScale, -1
	for scale := MinScale; scale <= top; scale++ {
		margin := (want - modules*scale) / 2
		// The distance from four modules, in thousandths of a module, so the
		// comparison is integer arithmetic over a fractional quantity.
		off := abs(margin*1000/scale - DefaultMargin*1000)
		if bestErr < 0 || off <= bestErr {
			bestErr, best = off, scale
		}
	}
	return SizeFit{Size: want, Scale: best, Margin: (want - modules*best) / 2}, nil
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
