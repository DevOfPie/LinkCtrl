package qr

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"image/png"
)

// A logo in the middle of a code (M50.6).
//
// M50.5 accepted a file and stored it; this is what puts it in the picture. The
// whole of the difficulty is that a logo *destroys modules*, and error
// correction is the only reason that is survivable — so every number below is
// derived from what level H can recover rather than chosen because it looks
// about right.
//
// # What level H actually buys
//
// A QR symbol of version v holds C(v) codewords of eight modules each, split
// into Reed-Solomon blocks. At level H the standard's own figure is that
// **about 30% of codewords are recoverable** — ISO/IEC 18004 Table 12. That
// figure is the error-correction capacity and not the EC-codeword share: version
// 1-H carries 17 EC codewords of 26 with one reserved for misdecode protection,
// so t = (17−1)/2 = 8 errors, and 8/26 = 30.8%. Version 4-H corrects 32 of 100
// and version 10-H 112 of 346. [logoBudget] takes ⌊0.30·C⌋, which is under all
// three.
//
// # Why the safe fraction is well under 30%
//
// **Occlusion is spatially correlated, and error correction is priced per
// codeword.** A codeword is eight modules laid out as a 2×4 block inside a
// two-module-wide column strip; damage one module of it and the whole codeword
// is an error. A contiguous square therefore destroys more codewords than its
// area suggests, because it clips partial codewords all round its boundary —
// [logoWorstCase] is that count, as an upper bound rather than an estimate.
//
// **And the budget is not all ours to spend.** Three other claims are on it:
//
//  1. The 30% is a *total* and Reed-Solomon corrects *per block*. Interleaving
//     spreads a contiguous region across blocks fairly evenly, but "fairly" is
//     not "exactly", and one block exhausting its own t fails the symbol while
//     the others sit idle.
//  2. Everything else that damages a printed code — ink spread, paper, folding,
//     lighting, camera angle, motion blur. A code with its whole budget spent on
//     a logo is a code that scans on a screen and fails on a poster.
//  3. The logo's own edge. A renderer antialiasing the boundary of the box can
//     corrupt the ring of modules just outside it, which is damage the box's own
//     module count does not include.
//
// So a share of the budget is reserved rather than spent, and the rest is the
// box's: W(k)·D ≤ B(v)·N, for [logoDamageNumerator] over
// [logoDamageDenominator].
//
// # The share, superseded — 2026-08-12
//
// **The rule was half the budget and is now three quarters**, and this section
// is the supersession rather than an edit over the top of it, because the
// reasoning that chose a half is still the reasoning that reserves a quarter.
//
// The owner read the shipped logo and asked for it "as big as possible without
// making the barcode unreadable" (F215), which reopened M50.6. A half was a
// *derivation* with nothing measured behind it: the three claims above are all
// real and none of them says how much. What the reopening added is the
// measurement — `make verify-scan`, two independent decoders over every size and
// logo shape this product draws, at five simulated viewing distances — so the
// reserve no longer has to carry the whole argument on its own. A quarter still
// pays for the three claims; the difference is that the box below is now the
// largest one a decoder was shown to read rather than the largest one an
// argument allowed.
//
// # The cap, as a number
//
// **A centred square three tenths of the symbol's width — 9% of its area** —
// rounded down to an odd number of modules so it centres on the module grid
// rather than straddling half a module. [LogoBoxModules] applies the budget
// check on top of that and takes whichever is smaller, so the claim is true by
// construction for every version rather than only for the ones this product
// happens to produce.
//
// For every version the product's content lengths reach, three tenths is what
// binds and the budget check still has headroom — version 5 is the tightest,
// destroying 24 codewords of a 40-codeword budget where three quarters is 30,
// and TestTheOcclusionCapFitsHsCorrectionBudget asserts it for all of them.
// *(The figure this paragraph carried before the reopening — "version 4 is the
// tightest, spending 24 of a 30-codeword budget's half" — was wrong in the way
// only arithmetic can be: 24 is twice version 5's destroyed count and 30 is
// version 4's budget, so it named neither version's numbers. Version 4 destroys
// 6 of a 30-codeword budget. Corrected here, in Plan.md's D140 and in m50.6.md.)*
//
// # Where three tenths came from
//
// **Measured, and the measurement is kept.** tools/qr-scan decodes the corpus
// internal/qr's TestWriteScanCorpus renders — every symbol version this
// product's content lengths reach, four logo shapes, five *stored* sizes for
// each, and the whole version range again with no logo at all at every level as
// a control, 1360 pictures in two equal halves — through
// zxing-cpp-as-WebAssembly and jsQR, each picture shrunk to
// 8, 6, 4, 3 and 2 pixels per module first.
//
// **Three of those five sizes are what chose the fraction; the other two arrived
// later.** The corpus drew the smallest, the default and the largest — 816
// pictures — when this derivation was made, and M49's second reopening (D182)
// added two more carrying a three-module quiet zone at either end of the scale
// range, because a margin below the ISO floor is measured rather than assumed.
// They test the *quiet zone* and not the cap, so every figure below is the
// 816-picture corpus it was taken on and is not restated here; D182's entry
// carries the re-run over all 1360. The sweep that chose the fraction
// ran 1/5, 2/9, 1/4, 3/10, 5/16, 8/25, 33/100 and 1/3. Everything up to 33/100
// was read by both decoders; **1/3 was not** — at version 13 jsQR loses the code
// for every logo shape and at every distance, which is a detector failure rather
// than a correction-budget one. Three tenths is two module-steps below that
// cliff at the version it appears at: version 13 gets a 19-module box where 1/3
// asks for 23, and the 21 in between reads clean as well.
//
// **What disagrees, recorded rather than hidden.** `zbarimg` 0.23.93 — the third
// engine, a system package this file cannot pin and `--zbar` reports without
// gating — is markedly stricter, and gets stricter as the box grows. Over the
// 816 pictures above, at the five distances, decoding each logo'd picture once:
//
//	one fifth      1484 of 1496      control 1496 of 1496
//	three tenths   1386 of 1496      control 1496 of 1496
//
// against 5984 of 5984 for the two gating decoders at both fractions.
// `make verify-scan SCAN_ARGS=--zbar` is what reproduces it.
//
// **The control is what makes those numbers mean anything**, and it covers the
// whole version range at every level for that reason: 1496 of 1496 says the
// misses above are the logo's doing and not ZBar's own limit, and a control at
// three versions could not have said it — the versions it missed are mostly
// outside them. The misses concentrate at the aggressive end of the distance
// simulation rather than at any one stored size: 84 of the 110 are read at two
// pixels per module, as are all twelve of the old fifth's.
//
// The trade is therefore real and is the owner's, taken knowingly: two modern
// engines read the larger box at two pixels per module, one older one loses
// some of the densest codes when the picture is shrunk that far. The twelve at
// the old fifth are worth their own sentence — the shipped product was already
// not clean under this engine, at a distance the original hand check never
// simulated.
//
// # What is deliberately not offered
//
// **Arbitrary placement.** The derivation is about a *centred* region: centring
// is what keeps the three finder patterns, the format information rings and the
// timing patterns clear, none of which error correction protects at all. A logo
// somewhere else would need its own derivation, and the honest way to not have
// one is to not offer the placement.
//
// **The one thing centring does not keep clear** is a central alignment pattern.
// Versions whose alignment grid has an odd row count put one dead centre —
// version 7 at (22,22) of 45 — and a centred box covers it. A decoder that
// cannot find it falls back to a perspective transform from the three finders,
// which is fine for a flat image and worse for a photographed curve. That is
// stated rather than engineered away, and it is part of why a quarter of the
// budget is reserved rather than none of it.

// LogoBoxNumerator and LogoBoxDenominator are the cap: the logo's box spans at
// most this fraction of the symbol's width, so nine hundredths — 9% — of its
// area. The package comment above is where the fraction came from and what it
// was measured against.
const (
	LogoBoxNumerator   = 3
	LogoBoxDenominator = 10
)

// logoDamageNumerator and logoDamageDenominator are the share of level H's
// correction budget a box may spend: three quarters, with the remaining quarter
// reserved for the three claims the package comment names. It superseded a half
// at the 2026-08-12 reopening, and that section says why.
const (
	logoDamageNumerator   = 3
	logoDamageDenominator = 4
)

// MinProductContent is the shortest payload, in bytes, that this product asks
// for a QR code of.
//
// **It is the floor of the range the cap was checked over**, and it is here
// rather than derived because this package knows nothing about aliases or
// hostnames: `https://` (8) + the shortest registrable hostname `a.b` (3) + `/`
// + an alias at `alias.MinLength` (3) + `?src=qr` (7). A named code adds
// `&qrc=` and eight characters and is therefore longer.
//
// Versions 1 and 2 hold 7 and 14 bytes at level H, so nothing this product
// encodes reaches them — which matters, because neither can carry a logo inside
// the share of H's correction budget a box may spend, and [LogoBoxModules]
// shrinks the box to almost nothing for both.
// TestTheShortestContentIsWhatInternalQRAssumes, in
// internal/link, is what holds this number to the two bounds it came from.
const MinProductContent = 22

// MaxLogoRasterSide bounds the composited raster, in pixels a side.
//
// **512 is not a new number.** It is ⌊√[MaxLogoPixels]⌋, so the largest raster
// this file can produce is exactly the largest image M50.5 *stores*. Since D180
// that figure bounds neither the decode nor what is accepted — a header is
// refused only past [MaxLogoDimension], and everything inside it is decoded and
// resampled down to fit — so what this raster matches is the stored artefact it
// is drawn from. The worst case its PNG encodes to is the 1,050,132-byte figure
// logo.go already derives, bounded by [MaxLogoStoredBytes] and enforced there.
// Reusing that arithmetic is the point: a second bound would be a second place
// to keep the same number.
//
// **It binds on both paths since the 2026-08-12 reopening**, and it did not
// before. A rasterised code stops at [MaxSize] pixels, so its box stops at
// MaxSize·[LogoBoxNumerator]/[LogoBoxDenominator]; at the old one fifth that is
// 409 and never reached this, and at three tenths it is 614 and does. What
// happens then is already written: [logoDrawing.drawPNG] resamples the clamped
// raster up to the rectangle the box needs, on the same arithmetic the SVG path
// has always used, so the two outputs still draw the same rectangle and the
// only cost is that a logo filling a 2048px code is drawn from 512 pixels of
// detail rather than 614.
const MaxLogoRasterSide = 512

// ForLogo is the style a code carrying a logo is drawn at: this one, at level H.
//
// **Forced, not defaulted, and forced in two places on purpose.** The service
// writes H into the row when a logo is set so that a `GET` reports what will be
// drawn (D141), and the renderer forces it again so the geometric claim above
// holds for *any* row — including one written before this milestone, or by hand.
// The two cannot disagree, because the second is what draws the picture.
//
// The style must already be normalized; this changes one field of a style that
// has been through [Style.Normalize] and adds nothing that needs checking.
func (s Style) ForLogo() Style {
	s.Level = LevelH
	return s
}

// LogoBoxModules is the side, in modules, of the centred square a logo occupies
// in a symbol of `modules` modules.
//
// Zero for a symbol too small to carry one at all, which no version this product
// encodes to reaches.
func LogoBoxModules(modules int) int {
	if modules < 21 {
		return 0
	}
	// Odd, so that (modules − side) is even and the box centres on the grid. A
	// QR symbol is always an odd number of modules across.
	side := modules * LogoBoxNumerator / LogoBoxDenominator
	if side%2 == 0 {
		side--
	}
	budget := logoBudget(symbolVersion(modules))
	for side >= 1 && logoWorstCase(side)*logoDamageDenominator > budget*logoDamageNumerator {
		side -= 2
	}
	if side < 1 {
		return 0
	}
	return side
}

// symbolVersion is the ISO version of a symbol `modules` modules across. A
// version-v symbol is 4v+17 modules.
func symbolVersion(modules int) int { return (modules - 17) / 4 }

// logoBudget is how many codewords level H can recover in a version-v symbol.
//
// ⌊0.30·C⌋, and the package comment above is where the 0.30 comes from and what
// it was checked against.
func logoBudget(version int) int { return totalCodewords(version) * 30 / 100 }

// totalCodewords is C(v): the data and error-correction codewords a version-v
// symbol holds, together.
//
// **Arithmetic rather than a forty-row table**, which is what keeps it
// reviewable. The symbol is 4v+17 modules square; subtract the three finder
// patterns with their separators, the two timing patterns, the alignment grid
// and the format and version information, and what is left is the raw data
// modules. Divided by eight, discarding the remainder bits the standard leaves
// unused, that is the codeword count.
//
// Checked against the standard's own table in
// TestTheCodewordCountIsTheStandardsOwn: 26 at version 1, 44 at 2, 196 at 7.
func totalCodewords(version int) int {
	raw := (16*version+128)*version + 64
	if version >= 2 {
		// The alignment grid: (a²−3) patterns of 5×5, less the 5 modules each of
		// the 2(a−2) patterns beside a timing pattern already shares with it.
		a := version/7 + 2
		raw -= (25*a-10)*a - 55
		if version >= 7 {
			// Two 3×6 version-information blocks.
			raw -= 36
		}
	}
	return raw / 8
}

// logoWorstCase is the most codewords a centred k×k occlusion can destroy.
//
// **An upper bound, derived from the layout rather than measured.** Codeword
// modules are placed in column strips two modules wide, filled four rows at a
// time, so every codeword lies inside one strip — or, at a turn, across two
// adjacent ones, which this counts twice and is therefore still an upper bound.
// A k-wide region touches at most ⌈(k+1)/2⌉ strips, and within a strip a k-tall
// region touches at most ⌈(k+3)/4⌉ of the four-row groups.
//
// Both directions are conservative in the same direction: an irregular codeword
// bent around an alignment pattern spans *more* rows of its strip and therefore
// fewer codewords per strip, and modules the region covers that belong to a
// function pattern are not codewords at all.
func logoWorstCase(side int) int {
	return ceilDiv(side+1, 2) * ceilDiv(side+3, 4)
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// logoBox is where the logo goes, in the **code's** own module coordinates —
// (0,0) is the code's top-left module, not the picture's, which is the same
// origin [Code.runs] walks in.
//
// It was the picture's until D182. The quiet zone is measured in pixels now and
// a picture's corner is no longer a whole number of modules from the symbol's,
// so the offset both encoders add is [geometry.origin] rather than a module
// count — one number, computed once, exactly as the modules themselves are.
type logoBox struct {
	x, y, side int
}

// logoBoxFor places the box, or reports that there is none.
func (c *Code) logoBoxFor() (logoBox, bool) {
	side := LogoBoxModules(c.Size)
	if side <= 0 {
		return logoBox{}, false
	}
	off := (c.Size - side) / 2
	return logoBox{x: off, y: off, side: side}, true
}

// logoInsetModules is the ring of the code's own background the logo is held
// back from, inside the occluded box.
//
// **This is the one number in this file that a measurement chose rather than a
// derivation, and it is the one the hand decode check earned.** m50.6.md
// requires a decode check against real scanners; run over every symbol version
// this product produces, with a fully opaque box-filling logo, `zbarimg` 0.23.93
// failed three of thirty-four with no inset — versions 7, 8 and 32 — and
// **thirty-four of thirty-four with one module of background around the box's
// edge**. The failures were not monotonic in the box's size (version 10 failed
// at three modules and read at eleven), which is what says they are the
// detector's grid search rather than the correction budget: a large flat block
// running straight into the modules gives it nothing to lock onto.
//
// **It costs the cap nothing.** The occluded region is still the whole box —
// the ring is painted the background colour, so the modules under it are
// destroyed exactly as the ones under the image are, and every number above is
// measured against the box rather than against the image inside it. What the
// ring changes is only what the box's edge looks like.
//
// The results, in full, are in m50.6.md.
const logoInsetModules = 1

// logoDrawing is the resampled logo both encoders draw, and the rectangle it
// occupies inside the box.
//
// **One resample, two encoders**, on the reason [Code.geometry] exists: a second
// scaler in the rasteriser is exactly the drift "the SVG and the PNG describe the
// same image" forbids.
type logoDrawing struct {
	box logoBox
	// img is the logo at raster resolution, which may be below the size it is
	// drawn at — see MaxLogoRasterSide.
	img *image.NRGBA
	// offX, offY, drawW and drawH are where the image goes inside the box, in
	// the box's own pixels. The rest of the box is background, and the ring
	// logoInsetModules holds back is part of that rest.
	offX, offY, drawW, drawH int
	// png is img encoded, for the SVG's data URI.
	png []byte
}

// prepareLogo decodes the stored image and fits it to the box.
//
// The input is bytes this product wrote — [NormalizeLogo] re-encoded them — so
// the decoder here is not the untrusted one M50.5 was written around. It is
// still bounded, and by both numbers rather than by the one an upload is
// refused for: what NormalizeLogo writes is inside [MaxLogoDimension] *and*
// [MaxLogoPixels], and this refuses anything that is not.
func (c *Code) prepareLogo(st Style, logo []byte) (*logoDrawing, error) {
	box, ok := c.logoBoxFor()
	if !ok {
		return nil, fmt.Errorf("qr logo: a %d-module code has no room for one", c.Size)
	}

	cfg, err := DecodeLogoConfig(logo)
	if err != nil {
		return nil, err
	}
	// The area bound, checked here rather than in DecodeLogoConfig since the F214
	// reopening. That function guards the *upload* path, where MaxLogoPixels
	// stopped being a refusal and became the size an image is fitted to; this is
	// the *stored* path, where it is still a bound, because everything
	// NormalizeLogo writes is inside it. A row past it is one somebody wrote by
	// hand, and decoding it would put a buffer past D142's stated figure behind
	// D127's rasteriser without D127 saying so. The area is what is bounded and
	// not the bytes: what this product encodes is eight-bit, so a stored row
	// decodes at four bytes a pixel, and only a hand-written bit-depth-16 row
	// reaches MaxDecodedLogoBytesPerPixel here.
	if cfg.Width*cfg.Height > MaxLogoPixels {
		return nil, &LogoBoundError{
			Width: cfg.Width, Height: cfg.Height, Bound: "pixels", Limit: MaxLogoPixels,
		}
	}
	src, err := png.Decode(bytes.NewReader(logo))
	if err != nil {
		if cfg.Format != "png" {
			return nil, fmt.Errorf("%w: a stored logo is a PNG this product encoded, "+
				"and this one sniffed as %s", ErrLogoUndecodable, cfg.Format)
		}
		return nil, fmt.Errorf("%w: stored png: %w", ErrLogoUndecodable, err)
	}

	// The box in pixels, less the ring of background logoInsetModules holds back.
	// A box too small to spare the ring keeps its whole area, which no version
	// this product encodes to reaches.
	boxPx := box.side * st.Scale
	inset := logoInsetModules * st.Scale
	if box.side <= 2*logoInsetModules+1 {
		inset = 0
	}
	innerPx := boxPx - 2*inset

	// The raster, clamped to what MaxLogoPixels already bounds — never larger
	// than the drawing needs, and never larger than the stored image it is drawn
	// from, which NormalizeLogo fitted to that figure on the way in.
	limit := innerPx
	if limit > MaxLogoRasterSide {
		limit = MaxLogoRasterSide
	}

	// Fitted inside the square, aspect ratio kept: a wordmark stays a wordmark,
	// and what is not covered by it is the background the box is painted with.
	w, h := fitInside(cfg.Width, cfg.Height, limit)
	img := resampleNRGBA(src, w, h)

	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode composited logo: %w", err)
	}

	// The drawn size is in the *box's* pixels rather than the raster's, because
	// the raster may have been clamped below what it is drawn at: the SVG places
	// it by the box's geometry and lets the viewer scale it there.
	drawW, drawH := scaleToBox(w, h, limit, innerPx)
	return &logoDrawing{
		box:   box,
		img:   img,
		offX:  inset + (innerPx-drawW)/2,
		offY:  inset + (innerPx-drawH)/2,
		drawW: drawW,
		drawH: drawH,
		png:   buf.Bytes(),
	}, nil
}

// scaleToBox maps a raster fitted inside `limit` back onto the `innerPx` square
// it is drawn at. They differ only when the raster was clamped at
// [MaxLogoRasterSide].
func scaleToBox(w, h, limit, innerPx int) (int, int) {
	if limit == innerPx || limit == 0 {
		return w, h
	}
	return w * innerPx / limit, h * innerPx / limit
}

// fitInside is the largest w×h with the source's aspect ratio that fits a square
// of `side`. At least one pixel each way, because a zero-sized draw is a picture
// that silently is not there.
func fitInside(srcW, srcH, side int) (int, int) {
	if srcW <= 0 || srcH <= 0 {
		return 1, 1
	}
	w, h := side, side
	if srcW > srcH {
		h = side * srcH / srcW
	} else if srcH > srcW {
		w = side * srcW / srcH
	}
	return max(w, 1), max(h, 1)
}

// resampleNRGBA scales an image to w×h by averaging over the source area each
// destination pixel covers.
//
// **Written here rather than imported.** `golang.org/x/image/draw` has a scaler
// and adding it would put a module in the require block one milestone after M49
// asserted the QR path adds none — see m50.6.md, which decides that question
// explicitly rather than importing it in a sub-clause. The standard library's
// `image/draw` copies and composites; it does not scale.
//
// **Area averaging, in both directions.** Downscaling is the case that matters —
// a 512-pixel logo into a 60-pixel box — and dropping pixels there produces the
// aliasing a scanner sees as noise. Upscaling degenerates to nearest-neighbour,
// which is blocky and honest; a logo drawn larger than it was uploaded has no
// detail to invent.
//
// Alpha is averaged with the colour rather than premultiplied, because
// [image.NRGBA] is not premultiplied and averaging premultiplied values here
// would darken a transparent edge against the background it is composited over.
func resampleNRGBA(src image.Image, w, h int) *image.NRGBA {
	b := src.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	if b.Empty() || w <= 0 || h <= 0 {
		return out
	}
	// The fast path: no scaling at all. draw.Src rather than the loop below, so
	// an unscaled logo is byte-for-byte its own pixels.
	if b.Dx() == w && b.Dy() == h {
		draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)
		return out
	}

	// The source is converted once rather than through src.At in the inner loop:
	// At goes through the image.Image interface and, for a paletted or YCbCr
	// source, does the colour conversion per sample rather than per pixel.
	norm := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(norm, norm.Bounds(), src, b.Min, draw.Src)

	sw, sh := b.Dx(), b.Dy()
	for y := range h {
		// The half-open source rows this destination row covers, at least one.
		y0, y1 := y*sh/h, (y+1)*sh/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := range w {
			x0, x1 := x*sw/w, (x+1)*sw/w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n int
			for sy := y0; sy < y1; sy++ {
				row := norm.Pix[sy*norm.Stride+x0*4 : sy*norm.Stride+x1*4]
				for i := 0; i < len(row); i += 4 {
					r += int(row[i])
					g += int(row[i+1])
					bl += int(row[i+2])
					a += int(row[i+3])
					n++
				}
			}
			o := out.PixOffset(x, y)
			out.Pix[o] = uint8(r / n)    //nolint:gosec // G115: an average of uint8s
			out.Pix[o+1] = uint8(g / n)  //nolint:gosec // G115: an average of uint8s
			out.Pix[o+2] = uint8(bl / n) //nolint:gosec // G115: an average of uint8s
			out.Pix[o+3] = uint8(a / n)  //nolint:gosec // G115: an average of uint8s
		}
	}
	return out
}

// logoDataURI is the embedded form the SVG carries.
//
// **This is the one place a workspace's own bytes reach the inside of an SVG
// this package writes, and base64 is what keeps the package comment's promise
// true.** The alphabet is `A-Za-z0-9+/=` and nothing else, so the encoded bytes
// cannot hold a quote, an angle bracket or anything that closes an attribute —
// whatever the image is. The bytes are also ones this product encoded twice over:
// once by [NormalizeLogo] on upload and again by [Code.prepareLogo] here.
func logoDataURI(raw []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

// writeSVG puts the box and the picture into an SVG being built.
//
// The box is painted the background colour first and the image drawn over it, so
// a logo with an alpha channel sits on the code's own background rather than on
// whatever module happened to be under it. That is also what makes the occluded
// area exactly the box, whatever the logo's shape — which is the number the cap
// above is a cap on.
// **Every coordinate here is a whole pixel, and that is D182's doing.** The
// drawing used to be in module units, so a logo whose aspect ratio is not 1:1
// landed on a fractional module and the SVG carried it as a decimal rounded to
// three places — a rounding the PNG did not share. The viewBox is in pixels now,
// which is the unit the rasteriser was always working in, so the two encoders
// write the same integers and the last place they could round differently is
// gone.
func (d *logoDrawing) writeSVG(b *bytes.Buffer, st Style, g geometry) {
	x := g.origin + d.box.x*g.scale
	y := g.origin + d.box.y*g.scale
	side := d.box.side * g.scale
	fmt.Fprintf(b, `<rect x="%d" y="%d" width="%d" height="%d" fill="%s"/>`,
		x, y, side, side, st.Background)
	fmt.Fprintf(b,
		`<image x="%d" y="%d" width="%d" height="%d" href="%s" preserveAspectRatio="none"/>`,
		x+d.offX, y+d.offY, d.drawW, d.drawH, logoDataURI(d.png))
}

// drawPNG paints the box and composites the logo into a rasterised code.
//
// `draw.Over` rather than `draw.Src`, so the alpha channel means what it means:
// the background painted underneath shows through a transparent logo, exactly as
// it does in the SVG.
func (d *logoDrawing) drawPNG(dst *image.NRGBA, st Style, g geometry) {
	x := g.origin + d.box.x*g.scale
	y := g.origin + d.box.y*g.scale
	boxPx := d.box.side * g.scale
	rect := image.Rect(x, y, x+boxPx, y+boxPx)
	draw.Draw(dst, rect, &image.Uniform{C: parseHex(st.Background)}, image.Point{}, draw.Src)

	at := image.Rect(
		rect.Min.X+d.offX, rect.Min.Y+d.offY,
		rect.Min.X+d.offX+d.drawW, rect.Min.Y+d.offY+d.drawH)
	if at.Dx() == d.img.Bounds().Dx() && at.Dy() == d.img.Bounds().Dy() {
		draw.Draw(dst, at, d.img, image.Point{}, draw.Over)
		return
	}
	// The raster was clamped below the box; resample it up to what this picture
	// needs so the two outputs still draw the same rectangle.
	//
	// Two ways to get here, and since the 2026-08-12 reopening both are reached.
	// The SVG's geometry has always been able to ask for a box larger than the
	// stored logo. The raster path now can too: at three tenths a code drawn at
	// [MaxSize] has a 614-pixel box against [MaxLogoRasterSide]'s 512, where at
	// the old one fifth the box stopped at 409 and this branch was SVG-only.
	draw.Draw(dst, at, resampleNRGBA(d.img, at.Dx(), at.Dy()), image.Point{}, draw.Over)
}
