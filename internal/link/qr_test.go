package link

import (
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/alias"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/qr"
)

// TestTheCodeCarriesTheSourceParameter is the whole attribution mechanism in one
// assertion (M41).
//
// A QR code is scanned by a camera, which sends no `Referer`. If the parameter
// is not inside the picture there is nowhere else it can come from, and every
// scan lands as `direct` — indistinguishable from somebody typing the URL. So
// what this asserts is not string formatting: it is that a scan is countable at
// all.
func TestTheCodeCarriesTheSourceParameter(t *testing.T) {
	cases := []struct {
		name     string
		shortURL string
		want     string
	}{
		{
			name:     "an ordinary short URL",
			shortURL: "https://links.example/summer",
			want:     "https://links.example/summer?src=qr",
		},
		{
			// A short URL never carries a query today, but the separator has to
			// be right if one ever does: `?` twice is a URL whose second
			// parameter is part of the first one's value, and the scan would be
			// attributed to nothing.
			name:     "one that already has a query",
			shortURL: "https://links.example/summer?utm_source=poster",
			want:     "https://links.example/summer?utm_source=poster&src=qr",
		},
		{
			// Nothing to encode. Returning "?src=qr" would produce a QR code
			// pointing at a relative path, which scans and goes nowhere.
			name:     "no URL at all",
			shortURL: "",
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QRContent(tc.shortURL, ""); got != tc.want {
				t.Errorf("QRContent(%q) = %q, want %q", tc.shortURL, got, tc.want)
			}
		})
	}
}

// TestTheSourceValueIsOneTheRedirectPathAccepts pins the two ends of the
// mechanism together. The code writes a value and the redirect path resolves it
// against a closed vocabulary; if the two ever disagree, scans are silently
// unattributed and nothing else fails.
func TestTheSourceValueIsOneTheRedirectPathAccepts(t *testing.T) {
	if _, ok := domain.ClickSource(domain.ClickSourceQR); !ok {
		t.Fatalf("the value QR codes encode (%q) is not one the redirect path "+
			"recognises; every scan would be attributed as direct",
			domain.ClickSourceQR)
	}
}

// TestTheShortestContentIsWhatInternalQRAssumes is the other end of
// internal/qr's `minProductContent` (M50.6).
//
// **The occlusion cap holds for the symbol versions this product's content
// lengths produce, and the floor of that range is an assumption internal/qr
// cannot check.** It knows nothing about aliases or hostnames — the layering is
// deliberate and predates this milestone — so the shortest URL it assumes is
// written there as a constant. This is where that constant is measured against
// the two bounds it was derived from, so a change to either one fails here
// rather than silently widening a range a cap was checked over.
//
// Versions 1 and 2 at level H hold 7 and 14 bytes. Anything at or above this
// floor is version 3 or larger, which is where the cap's arithmetic starts
// working.
func TestTheShortestContentIsWhatInternalQRAssumes(t *testing.T) {
	// The shortest registrable hostname: two labels, the last alphabetic —
	// internal/domain's ValidateHostname refuses one label and a numeric TLD.
	const shortestHost = "a.b"
	shortestAlias := strings.Repeat("a", alias.MinLength)

	got := QRContent("https://"+shortestHost+"/"+shortestAlias, "")
	if n := len(got); n != qr.MinProductContent {
		t.Errorf("the shortest content this product can build is %d bytes (%q) and "+
			"internal/qr's cap is checked from %d. The two have to agree: the range "+
			"of symbol versions the occlusion cap was asserted over starts at "+
			"whichever version that floor encodes to",
			n, got, qr.MinProductContent)
	}

	// A named code is longer, never shorter, so the floor above is the floor for
	// every code this product draws rather than only for the default one.
	named := QRContent("https://"+shortestHost+"/"+shortestAlias, "abcdefgh")
	if len(named) <= len(got) {
		t.Errorf("a named code's payload is %d bytes and the default's is %d; the "+
			"floor is then the wrong one", len(named), len(got))
	}
}

// TestALogoDoesNotBreakThePNGDownload is F171, at the numbers it was measured
// at.
//
// **The defect is invisible from the surface and that is why it is a test rather
// than a note.** A logo forces the error-correction level to H, H spends its
// budget on modules, and the margin and scale the reader chose are a *pixel*
// arithmetic over a module count that just grew. Nothing in the upload path said
// so before D174: somebody put a picture on a code and `GET …/qr.png` began
// answering 422 for a file that downloaded a moment earlier.
//
// The payload here is the row's own: 89 bytes, 37 modules at L and 53 at H, and
// margin 13 at scale 31, which draws 1953px and is inside the raster bound —
// 2000px when this was measured, 2048 since D182 — with room to spare until the
// level moves.
//
// **That style is hand-built, and since the M49 reopenings it has to be.** When
// the row was measured the size control produced it — the old qr.FitSize
// searched the quiet zone as a second knob and answered margin 13 for 2000px on
// this symbol. It does not store a quiet zone in modules at all now: since D182
// the control writes a pixel size, and the fit answers scale 44 with a
// 186-pixel zone for 2000px on this symbol. So a 13-module margin is
// unreachable from the form and the shape survives only in a style an API
// caller sets or a row written before 2026-08-12. The defect is a property of
// the payload and the level rather than of the fit, so it is still exactly this
// test.
func TestALogoDoesNotBreakThePNGDownload(t *testing.T) {
	content := QRContent(
		"https://links.example/"+strings.Repeat("a", 60), "")
	if len(content) != 89 {
		t.Fatalf("the payload is %d bytes and the measurement was taken at 89; "+
			"the module counts below are that payload's", len(content))
	}

	before, err := qr.Encode(content, qr.LevelL)
	if err != nil {
		t.Fatal(err)
	}
	style, _ := qr.Style{Level: qr.LevelL, Margin: 13, Scale: 31}.Normalize()
	if got := qr.OutputSize(before.Size, style); got != 1953 {
		t.Fatalf("the style draws %dpx, want the measured 1953", got)
	}

	after := refitForLogo(content, style)
	if after.Level != qr.LevelH {
		t.Errorf("the upload left the level at %q, want H; a logo occludes modules "+
			"and H's budget is what the occlusion cap is measured against", after.Level)
	}

	symbol, err := qr.Encode(content, after.Level)
	if err != nil {
		t.Fatal(err)
	}
	if symbol.Size <= before.Size {
		t.Fatalf("level H encodes this payload in %d modules and L in %d; if H is "+
			"not the larger symbol this test is measuring nothing",
			symbol.Size, before.Size)
	}

	drawn := qr.OutputSize(symbol.Size, after)
	if drawn > qr.MaxSize {
		t.Errorf("after the upload the code draws %dpx, past the %dpx raster bound, "+
			"so GET …/qr.png answers 422 for a code that downloaded a moment "+
			"earlier. Carrying margin %d and scale %d forward onto a %d-module "+
			"symbol is what does it", drawn, qr.MaxSize,
			style.Margin, style.Scale, symbol.Size)
	}

	// And it is *re-fitted*, not merely shrunk to fit: the size the reader set is
	// the size they keep — **exactly, since D182**. The allowance this assertion
	// used to carry was the fit's own: qr.FitSize could only land on sizes a
	// module grid admitted, so the nearest sat up to half a span away and 31px
	// was the bound for this symbol. Only the symbol needs whole modules now and
	// the quiet zone carries the remainder in pixels, so `FitSize(53, 1953)`
	// answers 1953 — scale 32 and a 128-pixel zone — and the assertion is
	// equality, matching its sibling below.
	if drawn != 1953 {
		t.Errorf("the code was 1953px and is %dpx after the upload. The re-fit keeps "+
			"the size somebody chose, and since D182 it can keep it to the pixel",
			drawn)
	}
}

// TestTheReFitOnlyMovesTheSizeItHasTo is the other half: D174 bought the re-fit
// with a restyle of a code that may already be printed, so the restyle has to
// stay confined to the two fields that carry the size.
func TestTheReFitOnlyMovesTheSizeItHasTo(t *testing.T) {
	content := QRContent("https://links.example/summer", "")
	style, _ := qr.Style{
		Foreground: "#123456", Background: "#fedcba", Level: qr.LevelM,
	}.Normalize()

	after := refitForLogo(content, style)
	if after.Foreground != style.Foreground || after.Background != style.Background {
		t.Errorf("the upload repainted the code: %+v became %+v. A logo is not a "+
			"colour change", style, after)
	}

	// The symbol grew — 29 modules at M, 37 at H for this content — so the style
	// moves, and only far enough to keep the picture the size the stored style
	// already drew it at. **Exactly that size, since D182**: the allowance was
	// one module at M50.6, then half a span at the first M49 reopening, because
	// a fit could only land on sizes the module grid admitted. It lands on the
	// number now, so the assertion is equality and the allowance is gone.
	base, err := qr.Encode(content, style.Level)
	if err != nil {
		t.Fatal(err)
	}
	symbol, err := qr.Encode(content, after.Level)
	if err != nil {
		t.Fatal(err)
	}
	if symbol.Size <= base.Size {
		t.Fatalf("H encodes this payload in %d modules and M in %d; if H is not "+
			"the larger symbol this fixture is no longer the growing case",
			symbol.Size, base.Size)
	}
	want := qr.OutputSize(base.Size, style)
	if drawn := qr.OutputSize(symbol.Size, after); drawn != want {
		t.Errorf("the code was %dpx and is %dpx after the upload; margin %d scale %d "+
			"became margin %d scale %d size %d. The re-fit exists to keep the picture "+
			"the size it was, and since D182 it can hit that number exactly",
			want, drawn, style.Margin, style.Scale, after.Margin, after.Scale, after.Size)
	}

	// A code whose symbol did not grow comes back byte for byte: a style already
	// at level H re-encodes to the same symbol, so the re-fit has nothing to
	// keep and must move nothing.
	kept, _ := qr.Style{
		Foreground: "#123456", Background: "#fedcba", Level: qr.LevelH,
	}.Normalize()
	if got := refitForLogo(content, kept); got != kept {
		t.Errorf("the symbol did not grow and the style still moved: %+v became %+v",
			kept, got)
	}

	// No case here holds the level at M over a symbol that cannot grow: equal
	// module counts at M and H exist only in version 1, and QRContent always
	// appends ?src=qr, whose lowercase bytes force byte mode past version 1's
	// 7-byte capacity — so that input is a property of internal/qr this product
	// never produces, not a missing assertion (F199).
}
