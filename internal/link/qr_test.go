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
// The picture here is the row's own: 37 modules before the upload, margin 13 at
// scale 31, which draws 1953px and is inside the raster bound — 2000px when this
// was measured, 2048 since D182 — with room to spare until the level moves.
//
// **The payload is not the row's own, and since D187 it cannot be.** F171 was
// measured on an 89-byte payload at level `L`, which was 37 modules; the level
// is a floor now, so `L` is a string this product accepts and never draws, and
// that payload's 89 bytes come out at 41 modules and 2077px — already past the
// bound, which would make the test's premise false rather than its assertion.
// So the fixture is re-measured onto the payload that draws the same 37-module
// symbol at the level a code actually carries: **69 bytes, at no named level at
// all**, 37 modules free and 49 at H. Every number the comment above quotes is
// the number this test measures; the payload behind them moved because the level
// under it became unreachable.
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
		"https://links.example/"+strings.Repeat("a", 40), "")
	if len(content) != 69 {
		t.Fatalf("the payload is %d bytes and the measurement was re-taken at 69; "+
			"the module counts below are that payload's", len(content))
	}

	before, err := qr.Encode(content, "")
	if err != nil {
		t.Fatal(err)
	}
	if before.Size != 37 {
		t.Fatalf("this payload draws %d modules and the measurement is 37's; the "+
			"pixel numbers below are that symbol's", before.Size)
	}
	style, _ := qr.Style{Margin: 13, Scale: 31}.Normalize()
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

// TestASizeSurvivesThePayloadGrowingUnderIt is M49's third reopening, at the
// arithmetic (F225, F226, D185).
//
// **This is the measurement in both findings, run forward.** Alias `summer`
// encodes to 29 modules untagged; the eight-character slug a code gains when it
// stops being alone pushes the same payload to 33, and a size fitted against the
// smaller symbol — 70px, which is that symbol's own floor — no longer holds the
// larger one with a quiet zone anything can read. `fitGeometry` then falls back
// to margin-and-scale and the picture measures 82px against a row that says 70,
// which is the second reopening's *the requested size is the size stored and
// drawn, exactly* made false by an operation nobody performed on that code.
//
// The fixture asserts its own module counts before it measures anything, because
// every number below is that payload's rather than a property of the arithmetic.
func TestASizeSurvivesThePayloadGrowingUnderIt(t *testing.T) {
	const shortURL = "https://links.example/summer"
	untagged := QRContent(shortURL, "")
	tagged := QRContent(shortURL, strings.Repeat("a", domain.QRCodeSlugLength))

	before, err := qr.Encode(untagged, qr.DefaultLevel)
	if err != nil {
		t.Fatal(err)
	}
	after, err := qr.Encode(tagged, qr.DefaultLevel)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size != 29 || after.Size != 33 {
		t.Fatalf("the payload is %d modules untagged and %d tagged; F225 and F226 "+
			"were measured at 29 and 33, and every number in this test is that "+
			"payload's", before.Size, after.Size)
	}

	// A code stored at its own floor, which is the bottom of the size control's
	// range for this code and the case both findings name.
	floor := qr.MinSizeFor(before.Size)
	fit, err := qr.FitSize(before.Size, floor)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := qr.Style{Margin: qr.DefaultMargin, Scale: fit.Scale, Size: fit.Size}.Normalize()
	if stored.Size != 70 {
		t.Fatalf("the floor for a 29-module code is %d and the findings measured 70",
			stored.Size)
	}
	if drawn := qr.OutputSize(after.Size, stored); drawn != 82 {
		t.Fatalf("the un-re-fitted row draws %dpx on the tagged payload; the findings "+
			"measured 82, and without that gap this test asserts nothing", drawn)
	}

	// The re-fit. The size rises, because at this one it has to — 78 is the
	// tagged symbol's own floor — and the rise is reported, which is the whole of
	// what the reader is told.
	out, rise := refitForPayload(tagged, stored)
	if got := qr.OutputSize(after.Size, out); got != out.Size {
		t.Errorf("the row says %dpx and the drawing measures %dpx. A stored size that "+
			"is not the drawn size is the defect this reopening exists to end, and "+
			"moving it onto a different number does not fix it", out.Size, got)
	}
	if out.Size != qr.MinSizeFor(after.Size) {
		t.Errorf("the re-fit raised the size to %dpx and this symbol's floor is %dpx. "+
			"It rises to the smallest size that works and no further — the owner "+
			"accepted a moving floor, not a size of the product's choosing",
			out.Size, qr.MinSizeFor(after.Size))
	}
	if rise != (QRSizeRise{From: 70, To: 78}) {
		t.Errorf("the re-fit reported %+v, want 70 → 78. The reader is told exactly "+
			"when the size they set could not be kept, and both numbers are what "+
			"makes the sentence say what happened", rise)
	}
	if !rise.Rose() {
		t.Errorf("%+v does not report as a rise, so nothing tells the reader", rise)
	}

	// **And the common case is silent**, which is what makes the notice mean
	// something. 512px on the same code has room for the larger symbol, so only
	// the scale moves — a number nobody set — and the size the reader typed is
	// still the size stored and drawn.
	roomy, err := qr.FitSize(before.Size, 512)
	if err != nil {
		t.Fatal(err)
	}
	held, _ := qr.Style{Margin: qr.DefaultMargin, Scale: roomy.Scale, Size: roomy.Size}.Normalize()
	kept, quiet := refitForPayload(tagged, held)
	if kept.Size != 512 || quiet.Rose() {
		t.Errorf("512px became %dpx and reported %+v. The size only moves where the "+
			"larger symbol leaves no scale that draws it, and 512 on a 33-module "+
			"code is not that case", kept.Size, quiet)
	}
	if kept.Scale == held.Scale {
		t.Errorf("the scale stayed at %d across a symbol that went from %d modules to "+
			"%d. If nothing moved, the row is still fitted against the payload it no "+
			"longer draws", kept.Scale, before.Size, after.Size)
	}
	if got := qr.OutputSize(after.Size, kept); got != 512 {
		t.Errorf("the silent re-fit draws %dpx against a stored 512. Silence is only "+
			"honest while the number is kept", got)
	}

	// A style with no size at all is the pre-M49 form, and it is left exactly as
	// written: it has no number to keep, it grows with the payload by
	// construction, and rewriting it would change a row nobody asked about.
	old, _ := qr.Style{Margin: 6, Scale: 9}.Normalize()
	if got, r := refitForPayload(tagged, old); got != old || r.Rose() {
		t.Errorf("a pre-M49 style %+v became %+v (%+v). Read-forward is M49's own "+
			"claim: such a row renders as it always did", old, got, r)
	}
}

// TestTheLevelComesBackWhenTheLogoLeaves is F223 at the arithmetic, and it is
// the inverse of TestALogoDoesNotBreakThePNGDownload above.
//
// The upload's re-fit holds the picture at the size it already was while the
// symbol grows; this one holds it at the size the reader chose while the symbol
// shrinks back. What it owes is that the number does not move — a level change
// moves the module count, and a module count is what a stored size is fitted
// against, which is the whole of what M49's third reopening was about.
func TestTheLevelComesBackWhenTheLogoLeaves(t *testing.T) {
	const shortURL = "https://links.example/summer"
	content := QRContent(shortURL, "")

	// A code the size control produced: no level of its own, so it draws at the
	// rule's, and a size fitted against that symbol.
	fitted, err := qr.FitSize(mustEncodeSize(t, content, ""), 512)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := qr.Style{
		Foreground: "#123456", Background: "#fedcba",
		Margin: qr.DefaultMargin, Scale: fitted.Scale, Size: fitted.Size,
	}.Normalize()

	// The upload, exactly as SetQRCodeLogo performs it.
	withLogo := refitForLogo(content, stored)
	if withLogo.Level != qr.LevelH {
		t.Fatalf("the upload left the level at %q, want H", withLogo.Level)
	}
	if got := qr.OutputSize(mustEncodeSize(t, content, withLogo.Level), withLogo); got != 512 {
		t.Fatalf("the logo'd code draws %dpx against a stored 512, so this fixture is "+
			"in the re-fit's escape and what follows measures nothing", got)
	}

	// And the removal.
	cleared := refitFromLogo(content, withLogo)
	if cleared.Level != "" {
		t.Errorf("the removal left the level at %q. An unset level is what asks for "+
			"the rule, and a level nobody chose is the finding this closes",
			cleared.Level)
	}
	if cleared.Size != 512 {
		t.Errorf("the code was 512px under the logo and is %dpx without it. The size "+
			"is the reader's number and the level is not, so only one of them moves",
			cleared.Size)
	}
	if got := qr.OutputSize(mustEncodeSize(t, content, cleared.Level), cleared); got != 512 {
		t.Errorf("the row says 512px and the drawing measures %dpx", got)
	}
	if cleared.Foreground != stored.Foreground || cleared.Background != stored.Background {
		t.Errorf("removing the logo repainted the code: %+v became %+v", stored, cleared)
	}
	// The scale is what absorbs the smaller symbol, and it has to have moved:
	// holding H's scale over the rule's symbol would leave the same 512 pixels
	// with a quiet zone nobody fitted.
	if cleared.Scale == withLogo.Scale {
		t.Errorf("the scale stayed at %d across a symbol that went from %d modules to "+
			"%d", cleared.Scale, mustEncodeSize(t, content, withLogo.Level),
			mustEncodeSize(t, content, cleared.Level))
	}

	// A style with no size is the pre-M49 form and keeps its shape here for the
	// reason refitForPayload keeps it: there is no number to hold, and the level
	// still comes back.
	old, _ := qr.Style{Level: qr.LevelH, Margin: 6, Scale: 9}.Normalize()
	got := refitFromLogo(content, old)
	old.Level = ""
	if got != old {
		t.Errorf("a pre-M49 style became %+v, want %+v — the level and nothing else",
			got, old)
	}
}

// mustEncodeSize is the module count of a payload at a level, which is the
// number every size in the tests above is fitted against.
func mustEncodeSize(t *testing.T, content string, level qr.Level) int {
	t.Helper()
	code, err := qr.Encode(content, level)
	if err != nil {
		t.Fatal(err)
	}
	return code.Size
}
