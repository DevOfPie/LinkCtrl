package qr

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The corpus the scan check decodes (M50.6, reopened).
//
// **This writes pictures; it asserts nothing.** The assertion is
// tools/qr-scan/scan.mjs's, because the decoder is there — Go has none, and
// m50.6.md's reopening puts it in tooling under D25 rather than in the require
// block. What this half owes the other half is that every picture comes off the
// **shipping** path: [RenderPNGWithLogo], the same call the download endpoint
// makes, so a fraction that passes here is the fraction the product draws.
//
// It is skipped unless QR_SCAN_CORPUS_DIR names somewhere to write, which is
// what keeps `make check` free of it. `make verify-scan` sets it.

// TestWriteScanCorpus renders every size, level and logo combination the scan
// check reads, and the manifest that says what each one should decode to.
func TestWriteScanCorpus(t *testing.T) {
	dir := os.Getenv("QR_SCAN_CORPUS_DIR")
	if dir == "" {
		t.Skip("QR_SCAN_CORPUS_DIR is unset — make verify-scan is what runs this")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}

	logos := scanLogos(t)
	lowest, highest := productVersions(t)
	payloads := scanPayloads(t, lowest, highest)

	var manifest bytes.Buffer
	manifest.WriteString("file\tpayload\tversion\tmodules\tscale\tmargin\tlogo\tlevel\n")
	written := 0

	for version := lowest; version <= highest; version++ {
		payload := payloads[version]
		code, err := Encode(payload, LevelH)
		if err != nil {
			t.Fatal(err)
		}
		if got := symbolVersion(code.Size); got != version {
			t.Fatalf("the payload for version %d encoded to version %d", version, got)
		}

		for _, st := range scanStyles(code.Size) {
			zone := st.zone(t, code.Size)
			for _, lg := range logos {
				name := fmt.Sprintf("v%02d-s%02d-m%02d-%s-%s.png",
					version, st.Scale, zone, lg.name, st.tag)
				raw, err := RenderPNGWithLogo(payload, st.Style, lg.png)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&manifest, "%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\n",
					name, payload, version, code.Size, st.Scale, zone, lg.name, LevelH)
				written++
			}
		}
	}

	// The control: the **whole** version range with no logo, at every level the
	// product offers. A scan check that only ever sees logo'd codes cannot tell
	// a cap that is too big from a distance simulation that is too aggressive,
	// and these are what separate the two — they carry no occlusion at all, so a
	// failure among them is the harness rather than the fraction.
	//
	// **The whole range rather than a sample of it**, because the claim the
	// control carries is that a decoder's misses are the logo's doing and not its
	// own limit — and a control at three versions cannot say that about the
	// thirty-one it does not cover. It was three versions until the 2026-08-12
	// reopening's review, where the versions zbarimg missed turned out to be
	// mostly outside them.
	//
	// The payload is the one chosen for this version *at level H*, so at the
	// other three levels it encodes to a smaller symbol — the filename carries
	// the version it was chosen for and the manifest carries the version it
	// actually became.
	//
	// **The level names a floor and the picture is drawn at whatever the rule
	// answers for it** (D184, D187), which is why the manifest records
	// `LevelFor` rather than the loop variable. Two consequences, both stated
	// because neither is visible from the filenames:
	//
	// The `L` slot no longer draws an `L` symbol — the free level is never below
	// [DefaultLevel], so `L` is unreachable everywhere, here included. It draws
	// what the `M` slot draws, and the corpus keeps its 1360 pictures with one
	// duplicate a version rather than shrinking. That is deliberate: the count is
	// quoted in three shipped documents, and a control half that stopped covering
	// a version to save a decode would be paying for tidiness with evidence.
	//
	// An **unset** level has no slot of its own and needs none. Every code this
	// product stores resolves to one of these four levels, and a picture drawn at
	// a resolved `Q` is byte for byte the picture drawn at this loop's `Q` with
	// the same style.
	for version := lowest; version <= highest; version++ {
		payload := payloads[version]
		for _, level := range Levels {
			code, err := Encode(payload, level)
			if err != nil {
				t.Fatal(err)
			}
			for _, st := range scanStyles(code.Size) {
				st.Level = level
				zone := st.zone(t, code.Size)
				name := fmt.Sprintf("v%02d-s%02d-m%02d-none-%s-%s.png",
					version, st.Scale, zone, strings.ToLower(string(level)), st.tag)
				raw, err := RenderPNG(payload, st.Style)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&manifest, "%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\n",
					name, payload, symbolVersion(code.Size), code.Size,
					st.Scale, zone, "none", LevelFor(payload, level))
				written++
			}
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "manifest.tsv"), manifest.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d pictures to %s", written, dir)
}

// scanStyle is a style the corpus draws at, and the word the filename carries
// for it.
type scanStyle struct {
	Style
	tag string
}

// zone is the quiet zone this style actually draws, in modules — which since
// D182 is not [Style.Margin]: a style carrying a size derives the zone from the
// remainder, and Margin is only what the drawing falls back to. The filename and
// the manifest carry what was drawn, because that is what the decoder saw.
func (s scanStyle) zone(t *testing.T, modules int) int {
	t.Helper()
	norm, errs := s.Normalize()
	if len(errs) > 0 {
		t.Fatalf("a corpus style is refused: %v", errs)
	}
	g := fitGeometry(modules, norm)
	if g.origin%g.scale != 0 {
		t.Fatalf("a corpus style draws a %dpx quiet zone at %dpx a module, which is "+
			"not a whole number of them; the corpus names the zone in modules",
			g.origin, g.scale)
	}
	return g.origin / g.scale
}

// scanStyles is every size a code of `modules` modules can be *stored* at,
// reduced to the five that decide the answer.
//
// **The smallest and the largest, because those are the ends m50.6.md's
// reopening names**, and the default in between because it is what the product
// draws unless somebody moves the slider. [MinScale] is two pixels a module,
// which is the smallest picture this product will produce at all; the top is
// whatever [MaxSize] leaves room for at this version.
//
// **Two of the five carry the band's low end**, and they are the second M49
// reopening's own requirement (D182). [MinMarginModules] is three modules of
// quiet zone, one below what ISO/IEC 18004 specifies, and the milestone is
// explicit that a margin under the floor is *measured* rather than assumed —
// this is the instrument. They are drawn at both ends of the scale range,
// because a narrow quiet zone at two pixels a module and one at sixty are
// different pictures to a detector. The style is built the way the size control
// stores one, so what is decoded is what the product serves.
//
// **Neither bound on `top` binds over the range this corpus walks**, and the
// numbers are worth writing down because a `top` that collided with
// [DefaultScale] used to be branched on here. The symbols the corpus reaches
// run from 25 modules — version 2, which only the control gets to, by encoding
// version 3's payload at level L — to 161 at version 36, so `top` runs from
// 2048/(25+8) = 62 down to 2048/(161+8) = 12. [MaxScale]'s 75 is above the
// first and [DefaultScale]'s 8 is below the second.
//
// **The branch is gone rather than made reachable, because neither thing it
// could have met needs it.** It fired at `top <= DefaultScale` and dropped the
// default style, saying the two would otherwise be one picture under two names.
// At `top == DefaultScale` that is false — they differ in palette, which is the
// whole reason the largest carries one. Below it the default style asks for a
// picture past [MaxSize] and [RenderPNG] refuses with [ErrTooLarge], which
// fails TestWriteScanCorpus by the line that renders it. So the case it existed
// for is loud already and the case it fired on was not a duplicate.
//
// The largest also carries a non-default palette, because a scanner reads
// contrast rather than black: a dark-blue-on-cream code is a picture the
// product can draw and a picture a decoder can decline.
func scanStyles(modules int) []scanStyle {
	span := modules + 2*DefaultMargin
	top := min(MaxScale, MaxSize/span)
	// The band's low end, expressed as the size control expresses it: a size in
	// pixels that leaves exactly MinMarginModules of quiet zone at that scale.
	//
	// The scale ends are the *narrow* picture's, not the four-module one's, and
	// they are not the same numbers: a 25-module symbol with a three-module zone
	// at [MinScale] is 62 pixels, which is under [MinSize] and therefore a
	// picture the control cannot ask for. Measuring one would be measuring
	// something this product does not serve, so the floor is raised until the
	// size is one it does.
	narrowSpan := modules + 2*MinMarginModules
	narrow := func(scale int) Style {
		return Style{Margin: DefaultMargin, Scale: scale, Size: narrowSpan * scale}
	}
	return []scanStyle{
		{Style{Margin: DefaultMargin, Scale: MinScale}, "smallest"},
		{Style{Margin: DefaultMargin, Scale: DefaultScale}, "default"},
		{Style{
			Margin: DefaultMargin, Scale: top,
			Foreground: "#102a54", Background: "#fdf6e3",
		}, "largest"},
		{narrow(max(MinScale, ceilDiv(MinSize, narrowSpan))), "narrowsmall"},
		{narrow(min(MaxScale, MaxSize/narrowSpan)), "narrowlarge"},
	}
}

// scanPayloads is one short URL per symbol version in the range, each the
// shortest content this product can build that reaches that version at level H.
//
// **One sweep rather than one search per version.** A longer payload never
// encodes to a smaller symbol — [productVersions] is what holds the encoder to
// that — so a single pass from [MinProductContent] up records the first length
// that lands on each version, and the whole corpus costs one encode per byte
// instead of one per byte per version.
func scanPayloads(t *testing.T, lowest, highest int) map[int]string {
	t.Helper()
	out := make(map[int]string, highest-lowest+1)
	for n := MinProductContent; n <= MaxContent; n++ {
		content := padTo(n)
		code, err := Encode(content, LevelH)
		if err != nil {
			continue
		}
		if v := symbolVersion(code.Size); out[v] == "" {
			out[v] = content
		}
	}
	for version := lowest; version <= highest; version++ {
		if out[version] == "" {
			t.Fatalf("no payload this product can build encodes to version %d at level H",
				version)
		}
	}
	return out
}

// scanLogo is one logo the corpus composites, and the name its files carry.
type scanLogo struct {
	name string
	png  []byte
}

// scanLogos is the four shapes the corpus draws.
//
// **Chosen for what they do to a decoder, not for variety.** An opaque square
// filling its box is the worst case — every module under it is gone and the
// edge is a hard straight line the detector's grid search can lock onto
// wrongly, which is the failure logoInsetModules was measured to fix. The
// transparent disc is what the demo actually carries. The wordmark is the
// aspect ratio that leaves background inside the box, which is the *easiest*
// case and is here so a failure can be told apart from a hard one.
func scanLogos(t *testing.T) []scanLogo {
	t.Helper()
	return []scanLogo{
		{"black", scanSquare(t, 256, color.NRGBA{A: 0xff})},
		{"red", scanSquare(t, 256, logoColour)},
		{"disc", scanDisc(t, 256)},
		{"wordmark", scanRect(t, 256, 64, logoColour)},
	}
}

func scanSquare(t *testing.T, side int, c color.NRGBA) []byte {
	t.Helper()
	return scanRect(t, side, side, c)
}

func scanRect(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return scanEncode(t, img)
}

// scanDisc is an opaque circle on a transparent square — the shape the demo's
// logo is, and the one that leaves the box's corners showing the background.
func scanDisc(t *testing.T, side int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	r := side / 2
	for y := range side {
		for x := range side {
			dx, dy := x-r, y-r
			if dx*dx+dy*dy > r*r {
				continue
			}
			img.SetNRGBA(x, y, logoColour)
		}
	}
	return scanEncode(t, img)
}

func scanEncode(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
