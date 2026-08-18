package qr

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
)

// The size arithmetic (M49, rebuilt at the second 2026-08-12 reopening, F221).
//
// **This is the milestone's real risk and these tests were written before the
// arithmetic was.** A size control silently affects every rendered code, and its
// edge cases are at both ends of the range and at the content lengths that push
// a code into a larger version. So the range is swept rather than sampled at
// three points, and the module counts are **derived from the encoder** rather
// than typed in: the set of matrix sizes a link in this product can produce is a
// fact about the encoder and the URL shape, and a list of it written here would
// be a list that goes stale the first time either moves.
//
// **What the sweep is asserting changed with D182, and the shape of the file
// did not.** It used to assert a snap — whole modules, monotone, within half a
// span of the request — because the drawn size was allowed to move off the
// requested one. It is not any more: the requested size is the drawn size at
// every value, and what is swept now is that equality, the quiet zone the
// remainder leaves, and the band that zone is asked to land in.
//
// What is deliberately not asserted: which particular scale a size resolves to.
// That is FitSize's answer and re-stating it here would be a copy of the
// implementation. What is asserted is the properties the answer has to have.

// productModuleCounts is every matrix size a link in this product encodes to.
//
// Content is the short URL with `?src=qr` on it, so its length runs from a
// three-character alias on a short host to a 64-character alias
// (alias.MaxLength) on a long one. 20 to 300 bytes covers that with room either
// side; every error-correction level is included, because the level changes the
// version and therefore the count.
// Encoding a thousand codes is the slow part of this file and the answer does
// not change between tests, so it is derived once.
var (
	moduleCountsOnce sync.Once
	moduleCounts     []int
	moduleCountsErr  error
)

func productModuleCounts(t *testing.T) []int {
	t.Helper()
	moduleCountsOnce.Do(func() {
		seen := map[int]bool{}
		// Every seventh byte rather than every byte. The distinct counts are
		// identical either way — versions 2 to 18, every four modules — because a
		// version spans tens of bytes of capacity and no step this small can jump
		// one; and encoding 1124 codes under `-race` cost 28 seconds of a unit
		// test suite for the same seventeen numbers. The floor below is what
		// notices if that ever stops being true.
		for length := 20; length <= 300; length += 7 {
			content := "https://links.example/" + repeat('a', length-22) + "?src=qr"
			for _, level := range Levels {
				code, err := Encode(content, level)
				if err != nil {
					moduleCountsErr = fmt.Errorf("content of %d bytes at level %s: %w",
						length, level, err)
					return
				}
				if !seen[code.Size] {
					seen[code.Size] = true
					moduleCounts = append(moduleCounts, code.Size)
				}
			}
		}
		sort.Ints(moduleCounts)
	})
	if moduleCountsErr != nil {
		t.Fatal(moduleCountsErr)
	}
	// Seventeen today: 25 to 89 modules, every four. Twelve is the floor rather
	// than seventeen, so a QR library that packs slightly differently does not
	// fail this — but a sweep that quietly stopped crossing version boundaries
	// does, and the arithmetic's edge cases are exactly at those boundaries.
	if len(moduleCounts) < 12 {
		t.Fatalf("only %d distinct module counts across the product's content lengths: "+
			"%v. The sweep is barely crossing a version boundary and the edge "+
			"cases are at those boundaries", len(moduleCounts), moduleCounts)
	}
	return moduleCounts
}

func repeat(c byte, n int) string {
	if n < 1 {
		n = 1
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

// sweep is the requested sizes every test below walks: both bounds, every
// seventh pixel between them so the step shares no factor with most module
// counts, and the round numbers a person actually types — which since D182
// include the slider's own stops, because a stop the fit refuses would be a mark
// the control draws and the save turns down.
//
// Ascending and deduplicated, because a reader comparing consecutive answers
// wants them in order.
func sweep() []int {
	seen := map[int]bool{}
	var want []int
	add := func(sizes ...int) {
		for _, s := range sizes {
			if !seen[s] {
				seen[s] = true
				want = append(want, s)
			}
		}
	}
	add(MinSize, MaxSize, 100, 128, 200, 250, 256, 300, 500, 512, 600, 1000, 1024, 1200, 1500)
	for size := MinSize; size <= MaxSize; size += 7 {
		add(size)
	}
	sort.Ints(want)
	return want
}

// TestTheRequestedSizeIsTheSizeDrawn is the reopening's first bullet, over every
// module count the product produces and the whole size range.
//
// **The whole claim is an equality**, and it is asserted three times over
// because there are three places the number could be lost: the fit's own answer,
// the style the fit implies, and the picture that style draws. A fit that
// reported 500 while the geometry drew 496 would be the shipped defect wearing a
// new hat.
func TestTheRequestedSizeIsTheSizeDrawn(t *testing.T) {
	for _, modules := range productModuleCounts(t) {
		for _, want := range sweep() {
			fit, err := FitSize(modules, want)
			if err != nil {
				if want < MinSizeFor(modules) {
					continue // the refusal has its own test
				}
				t.Fatalf("%d modules at %dpx: %v", modules, want, err)
			}

			if fit.Size != want {
				t.Fatalf("%d modules: asked for %dpx and the fit is %dpx. The size set "+
					"is where it stays — that sentence is the whole reopening",
					modules, want, fit.Size)
			}
			// The quiet zone is the remainder halved, and the far side carries
			// the odd pixel — so the sum is the requested size or one under it,
			// and never anything else. That one pixel is what buys exactness at
			// an odd remainder; see SizeFit.Margin.
			if got := modules*fit.Scale + 2*fit.Margin; got != want && got != want-1 {
				t.Fatalf("%d modules at %dpx: %d modules at %dpx each plus %dpx of quiet "+
					"zone a side is %dpx. The remainder is halved and the odd pixel goes "+
					"to the far side, so nothing but %d or %d is arithmetic either "+
					"encoder does", modules, want, modules, fit.Scale, fit.Margin, got,
					want-1, want)
			}

			// The style a save would store, and the picture it draws. This is
			// the arm that catches a fit whose answer no Style can express.
			st, errs := Style{
				Margin: DefaultMargin, Scale: fit.Scale, Size: fit.Size,
			}.Normalize()
			if len(errs) > 0 {
				t.Fatalf("%d modules at %dpx: the fit is not a style Normalize accepts: %v",
					modules, want, errs)
			}
			if got := OutputSize(modules, st); got != want {
				t.Fatalf("%d modules at %dpx: the stored style draws %dpx", modules, want, got)
			}
			g := fitGeometry(modules, st)
			if g.px != want || g.origin != fit.Margin {
				t.Fatalf("%d modules at %dpx: the drawing is %dpx with a %dpx quiet zone, "+
					"and the fit said %dpx and %dpx", modules, want, g.px, g.origin,
					want, fit.Margin)
			}

			// Inside what the renderer accepts, so a fit is always a style
			// Normalize will not refuse, and inside what the rasteriser allows.
			if fit.Scale < MinScale || fit.Scale > MaxScale {
				t.Fatalf("%d modules at %dpx: scale %d, want %d to %d",
					modules, want, fit.Scale, MinScale, MaxScale)
			}
			if fit.Size > MaxSize {
				t.Fatalf("%d modules at %dpx: fit is %dpx, past the %dpx rasteriser bound",
					modules, want, fit.Size, MaxSize)
			}
		}
	}
}

// TestMaxScaleIsWhatTheRangeNeeds pins the ceiling to its derivation rather than
// to the number somebody typed.
//
// It has been wrong twice — 32 while nothing derived it, then 68 while the quiet
// zone was four modules and MaxSize was 2000 — and each time the symptom was a
// fit the API then refused. The derivation is the smallest symbol at the
// narrowest quiet zone, because that is the picture with the most pixels to
// spend on each module.
func TestMaxScaleIsWhatTheRangeNeeds(t *testing.T) {
	const smallestSymbol = 21
	if want := MaxSize / (smallestSymbol + 2*MinMarginModules); MaxScale != want {
		t.Errorf("MaxScale is %d and the range needs %d: %d pixels over a %d-module "+
			"symbol with %d modules of quiet zone a side. A ceiling below this "+
			"refuses a fit the size control produced",
			MaxScale, want, MaxSize, smallestSymbol, MinMarginModules)
	}
	fit, err := FitSize(smallestSymbol, MaxSize)
	if err != nil {
		t.Fatalf("the smallest symbol cannot be drawn at the largest size: %v", err)
	}
	if _, errs := (Style{Margin: DefaultMargin, Scale: fit.Scale, Size: fit.Size}).Normalize(); len(errs) > 0 {
		t.Errorf("the largest fit is a style Normalize refuses: %v", errs)
	}
}

// TestTheQuietZoneNeverGoesBelowTheFloor is the safety half, and it is
// unconditional.
//
// [MinMarginModules] is three, which is **below** ISO/IEC 18004's four, and that
// is D182's owner-set band read at its low end. Below three nothing here will
// go: the size is refused instead. What three modules costs is measured by `make
// verify-scan` rather than argued — see the corpus, which draws this quiet zone
// across the whole version range.
func TestTheQuietZoneNeverGoesBelowTheFloor(t *testing.T) {
	if DefaultMargin != 4 || MinMarginModules != 3 {
		t.Fatalf("the band is %d modules ±25%%, so its low end is 3 and the constants "+
			"say %d and %d; this test is asserting the wrong numbers",
			DefaultMargin, DefaultMargin, MinMarginModules)
	}
	for _, modules := range productModuleCounts(t) {
		for _, want := range sweep() {
			fit, err := FitSize(modules, want)
			if err != nil {
				continue
			}
			if fit.Margin < MinMarginModules*fit.Scale {
				t.Fatalf("%d modules at %dpx resolved to a %dpx quiet zone at %dpx a "+
					"module — %.2f modules. Below %d the code is outside what was "+
					"measured, and a size control that trades the quiet zone away past "+
					"the measurement is trading the one thing that must not be traded",
					modules, want, fit.Margin, fit.Scale,
					float64(fit.Margin)/float64(fit.Scale), MinMarginModules)
			}
		}
	}
}

// TestTheQuietZoneLandsInTheBand is the reopening's second bullet, **including
// the half that says where the band cannot be held**.
//
// The band is four modules ±25%, so three to five, and it is not reachable at
// every size: the scale is a whole number of pixels, so the achievable quiet
// zones for one symbol step by (modules+8)/2 pixels at a time, and where that
// step is wider than the band there is no scale inside it. The condition is
// arithmetic rather than an observation —
//
//	the band is reachable when want ≥ (modules+10)(modules+6)/4
//
// which is the request at which the interval of admissible scales is at least
// one wide and therefore contains an integer. **Above that line the band is
// asserted; below it the excursion is asserted to be forced**, which is the
// claim that matters: the fit errs *wide*, and only where the next scale up
// would have put the zone under the floor. Wide costs white space; narrow costs
// scannability, and this never chooses narrow.
//
// An 89-module code at 256px is the shape to picture: two pixels a module is the
// only scale that leaves any quiet zone at all, so the zone is 19.5 modules and
// the code is 48% of the picture by area. **That is a claim M49's first
// reopening made and this one gives up** — it required the majority of the
// picture to be code at every size, which is not simultaneously satisfiable with
// an exact size, and D182 is the owner choosing the exact size.
func TestTheQuietZoneLandsInTheBand(t *testing.T) {
	inBand, forced := 0, 0
	for _, modules := range productModuleCounts(t) {
		// The request above which an admissible scale must exist inside the band.
		reachable := ceilDiv((modules+10)*(modules+6), 4)
		for _, want := range sweep() {
			fit, err := FitSize(modules, want)
			if err != nil {
				continue
			}
			// Thousandths of a module, so the comparison is integer arithmetic.
			zone := fit.Margin * 1000 / fit.Scale
			switch {
			case zone >= MinMarginModules*1000 && zone <= 5000:
				inBand++
			case want >= reachable:
				t.Fatalf("%d modules at %dpx: the quiet zone is %.2f modules and the "+
					"band is %d to 5. At %dpx and above the interval of admissible "+
					"scales is a whole scale wide, so one of them lands in the band "+
					"and this fit did not choose it",
					modules, want, float64(zone)/1000, MinMarginModules, reachable)
			default:
				// Out of band, below the line, and therefore obliged to be
				// forced: the next scale up must break the floor. Anything else
				// is a fit that chose white space it did not have to.
				forced++
				if zone < MinMarginModules*1000 {
					t.Fatalf("%d modules at %dpx: %.2f modules of quiet zone, under the "+
						"floor", modules, want, float64(zone)/1000)
				}
				next := fit.Scale + 1
				if margin := (want - modules*next) / 2; next <= MaxScale &&
					margin >= MinMarginModules*next {
					t.Fatalf("%d modules at %dpx: the fit drew %.2f modules of quiet zone "+
						"at scale %d, and scale %d would have left %.2f — inside the "+
						"floor and nearer four. The excursion above the band is only "+
						"honest while it is forced",
						modules, want, float64(zone)/1000, fit.Scale, next,
						float64(margin)/float64(next))
				}
			}
		}
	}
	if inBand == 0 || forced == 0 {
		t.Fatalf("the sweep found %d fits in the band and %d forced outside it; both "+
			"arms of this test have to be exercised or one of them is asserting "+
			"nothing", inBand, forced)
	}
	t.Logf("%d fits inside the 3-to-5 band, %d forced wide of it", inBand, forced)
}

// TestASizeOutOfRangeIsRefusedRatherThanClamped. Clamping reports success for a
// setting nobody asked for — the same rule margin and scale have had since M41,
// applied to the control that replaced them.
//
// **Two refusals, and the second is per code** (D182). Outside [MinSize,
// MaxSize] is the control's own range. Inside it but below what this symbol can
// hold is [MinSizeFor], which is a number the global bounds cannot express: 64
// draws a 25-module code and cannot draw an 89-module one, because the pixels a
// symbol needs are a property of the symbol.
func TestASizeOutOfRangeIsRefusedRatherThanClamped(t *testing.T) {
	for _, want := range []int{0, -1, MinSize - 1, MaxSize + 1, 1 << 20} {
		if _, err := FitSize(29, want); !errors.Is(err, ErrSizeOutOfRange) {
			t.Errorf("FitSize(29, %d) = %v, want ErrSizeOutOfRange", want, err)
		}
	}
	if _, err := FitSize(0, 300); err == nil {
		t.Error("a code of no modules was given a size")
	}

	for _, modules := range productModuleCounts(t) {
		floor := MinSizeFor(modules)
		if floor != MinScale*(modules+2*MinMarginModules) {
			t.Fatalf("MinSizeFor(%d) is %d and the symbol needs %d", modules, floor,
				MinScale*(modules+2*MinMarginModules))
		}
		if floor > MinSize {
			if _, err := FitSize(modules, floor-1); !errors.Is(err, ErrSizeOutOfRange) {
				t.Errorf("a %d-module code was fitted into %dpx, one below its floor of "+
					"%d. A picture with no room for a quiet zone is refused, not squeezed",
					modules, floor-1, floor)
			}
		}
		// And the floor itself draws, which is what makes it a floor rather than
		// a number one either side of the truth.
		fit, err := FitSize(modules, max(floor, MinSize))
		if err != nil {
			t.Fatalf("a %d-module code cannot be drawn at its own floor of %dpx: %v",
				modules, floor, err)
		}
		if fit.Size != max(floor, MinSize) {
			t.Errorf("a %d-module code at its floor drew %dpx", modules, fit.Size)
		}
	}
}

// TestAStoredStyleReadsForwardToTheSizeItAlreadyDrew is the read direction, and
// it is what makes a pre-M49 row keep its appearance.
//
// A style written before this milestone carries a quiet zone and a scale and no
// size at all. OutputSize is the whole of what turns that into the number the
// form shows, so it has to agree with the drawing rather than with an idea of
// it — which is why the expected value is read off the SVG the same style
// produces rather than recomputed here.
func TestAStoredStyleReadsForwardToTheSizeItAlreadyDrew(t *testing.T) {
	// A margin and a scale and no size, which is the shape every row written
	// before M49 carries. It is not the demo's row and no longer claims to be:
	// cmd/lctl seeds through SetQRSize (`demo_phase2.go:1348`) at 400px, which
	// since D182 is stored as a size rather than as a margin at all. What this
	// asserts is the read path for the older form, which consults no fit, so the
	// numbers only have to be a style Normalize accepts.
	stored := Style{Foreground: "#123a6b", Background: "#f5f7fa", Level: LevelQ, Margin: 4, Scale: 10}
	norm, errs := stored.Normalize()
	if len(errs) > 0 {
		t.Fatalf("a pre-M49 stored style is refused: %v", errs)
	}

	code, err := Encode(sample, norm.Level)
	if err != nil {
		t.Fatal(err)
	}
	svg := code.SVG(norm)

	got := OutputSize(code.Size, norm)
	if drawn := attrInt(t, string(svg), `width="(\d+)"`); got != drawn {
		t.Errorf("OutputSize says %dpx and the drawing is %dpx wide. The form shows the "+
			"first number and the reader is looking at the second", got, drawn)
	}
	if height := attrInt(t, string(svg), `height="(\d+)"`); height != got {
		t.Errorf("the drawing is %dpx wide and %dpx tall; a QR code is square and the "+
			"size control is one number because of it", got, height)
	}

	// And the size a fit produces for that number resolves back to the same
	// picture — the round trip a reader makes by opening the panel and saving
	// without touching anything. It was allowed to move by half a span before
	// D182 and it is not allowed to move at all now.
	fit, err := FitSize(code.Size, got)
	if err != nil {
		t.Fatal(err)
	}
	if fit.Size != got {
		t.Errorf("saving the size a stored style already draws (%dpx) moved it to %dpx; "+
			"opening the panel and pressing save must not resize anybody's code",
			got, fit.Size)
	}
}

// TestARowFromTheOldSearchKeepsRenderingAndReSavesExactly is the stored-row half
// of the reopening (F213, then F221).
//
// **Two claims, and the second is what changed.** A style already in
// `qr_codes.style` is drawn from what it carries — nothing on the read path
// consults [FitSize] — so a row written by the old margin search renders in the
// pixels it always did, whatever the arithmetic now prefers. That is the one
// that had to hold, because those rows are printed.
//
// The second used to be that re-saving such a row *moved* the size by up to half
// a span and reported the move. It does not move any more: the re-save goes
// through a fit that can hit the number exactly, so the assertion is equality
// and the flash message that carried the difference is gone with it.
func TestARowFromTheOldSearchKeepsRenderingAndReSavesExactly(t *testing.T) {
	// Margin 14 at scale 7 is a real answer the first search gave: a 29-module
	// code asked for 400px resolved to exactly this, 399px with more than three
	// times the ISO quiet zone. It is the shape of row this milestone stopped
	// producing and has never stopped honouring.
	old := Style{Foreground: "#123a6b", Background: "#f5f7fa", Level: LevelQ, Margin: 14, Scale: 7}
	norm, errs := old.Normalize()
	if len(errs) > 0 {
		t.Fatalf("a row the old search wrote is no longer a style Normalize accepts: %v", errs)
	}
	if norm.Margin != old.Margin || norm.Scale != old.Scale || norm.Size != 0 {
		t.Fatalf("Normalize rewrote a stored row: margin %d scale %d size %d became "+
			"margin %d scale %d size %d. A row already in the database is drawn as "+
			"written, or every printed code drawn from one changed appearance",
			old.Margin, old.Scale, old.Size, norm.Margin, norm.Scale, norm.Size)
	}

	code, err := Encode(sample, norm.Level)
	if err != nil {
		t.Fatal(err)
	}
	drawn := attrInt(t, string(code.SVG(norm)), `width="(\d+)"`)
	if want := OutputSize(code.Size, norm); drawn != want {
		t.Fatalf("the row draws %dpx and OutputSize says %dpx", drawn, want)
	}
	if drawn != (code.Size+2*14)*7 {
		t.Errorf("a %d-module code at margin 14 scale 7 drew %dpx, which is not the "+
			"geometry the row carries. The read path must not consult FitSize",
			code.Size, drawn)
	}

	// The re-save. It lands on the same number, which is the whole of what the
	// second reopening asked for.
	fit, err := FitSize(code.Size, drawn)
	if err != nil {
		t.Fatal(err)
	}
	if fit.Size != drawn {
		t.Errorf("re-saving a row that drew %dpx produced %dpx. A save is not a resize",
			drawn, fit.Size)
	}
	// And what it stores draws that number, through the newer form of the
	// geometry rather than the one the row arrived in.
	stored, errs := Style{Margin: DefaultMargin, Scale: fit.Scale, Size: fit.Size}.Normalize()
	if len(errs) > 0 {
		t.Fatalf("the re-fit is not a style Normalize accepts: %v", errs)
	}
	if got := OutputSize(code.Size, stored); got != drawn {
		t.Errorf("the re-saved row draws %dpx where the original drew %dpx", got, drawn)
	}
	if fit.Margin >= 14*fit.Scale {
		t.Errorf("re-saving kept a %dpx quiet zone at %dpx a module — %.2f modules. A "+
			"save is a new fit and a new fit aims at %d",
			fit.Margin, fit.Scale, float64(fit.Margin)/float64(fit.Scale), DefaultMargin)
	}
}

// TestAStoredSizeTheSymbolOutgrowsFallsBackRatherThanBreaking is the one edge
// the newer form of the geometry has and the older one did not.
//
// A size is stored against the module count the content had when it was saved.
// Rename the link into a longer alias and the payload encodes to a larger
// matrix, which can need more pixels than the stored size holds — and a picture
// whose symbol does not fit inside it is not a picture. So the drawing falls
// back to the margin and scale the row also carries, which is exactly what a
// pre-M49 row does, and the code comes out larger rather than clipped.
func TestAStoredSizeTheSymbolOutgrowsFallsBackRatherThanBreaking(t *testing.T) {
	small, err := Encode("https://a.b/abc?src=qr", LevelM)
	if err != nil {
		t.Fatal(err)
	}
	fit, err := FitSize(small.Size, 300)
	if err != nil {
		t.Fatal(err)
	}
	st, errs := Style{Margin: DefaultMargin, Scale: fit.Scale, Size: fit.Size}.Normalize()
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if got := OutputSize(small.Size, st); got != 300 {
		t.Fatalf("the style draws %dpx for the symbol it was fitted to, want 300", got)
	}

	// The same style against a symbol far too large for it.
	big := small.Size + 40
	want := (big + 2*DefaultMargin) * st.Scale
	if got := OutputSize(big, st); got != want {
		t.Errorf("a %d-module symbol in a style sized for %d drew %dpx; the fallback is "+
			"the row's own margin and scale, which is %dpx", big, small.Size, got, want)
	}
	if got := fitGeometry(big, st).origin; got != DefaultMargin*st.Scale {
		t.Errorf("the fallback drew a %dpx quiet zone, want %dpx — the row's %d modules "+
			"at %dpx each", got, DefaultMargin*st.Scale, DefaultMargin, st.Scale)
	}
}
