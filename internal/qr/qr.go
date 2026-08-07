// Package qr encodes a string as a QR code and draws it as SVG (M41).
//
// **SVG only, and that is decision D11**: the output is vector text, so no image
// encoder joins the dependency set and no rasteriser runs on a request. A PNG
// download, if it is ever wanted, is an additive change here and nowhere else.
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
	"image/color"
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
	span := c.Size + 2*st.Margin
	px := span * st.Scale

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
			b.WriteString(`<rect x="`)
			b.WriteString(strconv.Itoa(x + st.Margin))
			b.WriteString(`" y="`)
			b.WriteString(strconv.Itoa(y + st.Margin))
			b.WriteString(`" width="`)
			b.WriteString(strconv.Itoa(run))
			b.WriteString(`" height="1"/>`)
			x += run
		}
	}
	b.WriteString(`</g></svg>`)
	return b.Bytes()
}

// ContentType is what a QR response is served as.
const ContentType = "image/svg+xml"

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
