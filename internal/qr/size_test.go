package qr

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
)

// The size arithmetic (M49).
//
// **This is the milestone's real risk and these tests were written before the
// snap was.** *Nearest size that keeps modules whole* is a small change that
// silently affects every rendered code, and its edge cases are at both ends of
// the range and at the content lengths that push a code into a larger version.
// So the range is swept rather than sampled at three points, and the module
// counts are **derived from the encoder** rather than typed in: the set of
// matrix sizes a link in this product can produce is a fact about the encoder
// and the URL shape, and a list of it written here would be a list that goes
// stale the first time either moves.
//
// What is deliberately not asserted: which particular scale a size resolves
// to. That is FitSize's answer and re-stating it here would be a copy of the
// implementation. What is asserted is the properties the answer has to have —
// whole modules, inside the bounds, monotone, within half a span of what was
// asked, and a quiet zone that is exactly the floor. That last one is the M49
// reopening's bound (F213): the margin used to be a second search knob and the
// owner saw the white it bought.

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
	// does, and the snap's edge cases are exactly at those boundaries.
	if len(moduleCounts) < 12 {
		t.Fatalf("only %d distinct module counts across the product's content lengths: "+
			"%v. The sweep is barely crossing a version boundary and the snap's edge "+
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
// counts, and the round numbers a person actually types.
//
// Ascending and deduplicated, because the monotonicity check below reads it in
// order and an unsorted list would fail that for the list's reasons rather than
// for the snap's.
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
	add(MinSize, MaxSize, 100, 128, 200, 250, 256, 300, 500, 512, 600, 1000, 1024, 1500)
	for size := MinSize; size <= MaxSize; size += 7 {
		add(size)
	}
	sort.Ints(want)
	return want
}

// TestTheSizeSnapsToWholeModules is the milestone's snap bullet, over every
// module count the product produces and the whole size range.
//
// The four properties are separate on purpose. A snap that produced fractional
// modules would be the drawing bug; one that left the bounds would be a style
// the renderer refuses; one further from the request than half a span would be
// a snap that is not snapping to the nearest whole-module size; and one that is
// not monotone would be a rounding that leans the wrong way at some sizes and
// not others, which is the failure a three-point test misses.
func TestTheSizeSnapsToWholeModules(t *testing.T) {
	for _, modules := range productModuleCounts(t) {
		previous := 0
		for _, want := range sweep() {
			fit, err := FitSize(modules, want)
			if err != nil {
				t.Fatalf("%d modules at %dpx: %v", modules, want, err)
			}

			// Whole modules, which is the claim. The size is a whole number of
			// modules across because it is a product of two integers, and this
			// is the assertion that it is *those* two integers.
			if got := (modules + 2*fit.Margin) * fit.Scale; got != fit.Size {
				t.Fatalf("%d modules at %dpx: fit says %dpx but %d modules at %d "+
					"pixels each is %dpx. The two encoders both multiply this out, "+
					"so a size that is not the product is a size neither of them draws",
					modules, want, fit.Size, modules+2*fit.Margin, fit.Scale, got)
			}
			if fit.Requested != want {
				t.Fatalf("%d modules at %dpx: the fit reports %dpx was asked for",
					modules, want, fit.Requested)
			}
			if fit.Snapped() != (fit.Size != want) {
				t.Fatalf("%d modules at %dpx: Snapped()=%v for a %dpx result",
					modules, want, fit.Snapped(), fit.Size)
			}

			// Inside what the renderer accepts, so a fit is always a style that
			// Normalize will not refuse, and inside what the rasteriser allows.
			if fit.Margin < DefaultMargin || fit.Margin > MaxMargin {
				t.Fatalf("%d modules at %dpx: quiet zone %d modules, want %d to %d",
					modules, want, fit.Margin, DefaultMargin, MaxMargin)
			}
			if fit.Scale < MinScale || fit.Scale > MaxScale {
				t.Fatalf("%d modules at %dpx: scale %d, want %d to %d",
					modules, want, fit.Scale, MinScale, MaxScale)
			}
			if fit.Size > MaxSize {
				t.Fatalf("%d modules at %dpx: fit is %dpx, past the %dpx rasteriser "+
					"bound", modules, want, fit.Size, MaxSize)
			}

			// Within half a span of the request, unless the range clamps it: the
			// achievable sizes step by one span per scale, so a nearest snap can
			// miss by at most half of one — the whole remainder a module grid
			// forces, now that the quiet zone no longer absorbs any of it
			// (F213). At the range's ends the nearest multiple can sit outside
			// what the renderer accepts, and the clamped size is the honest
			// answer there.
			span := modules + 2*fit.Margin
			if clamped := fit.Size == span*MinScale || fit.Size == span*(MaxSize/span); !clamped {
				if 2*abs(fit.Size-want) > span {
					t.Fatalf("%d modules at %dpx: fit chose %dpx, which is %dpx off "+
						"for a %d-module span. The nearest whole-module size is never "+
						"further than half a span away, so this snap is not snapping "+
						"to it", modules, want, fit.Size, abs(fit.Size-want), span)
				}
			}

			if fit.Size < previous {
				t.Fatalf("%d modules: asking for more pixels gave fewer — %dpx produced "+
					"%dpx after a smaller request produced %dpx. A tie-break that leans "+
					"one way at some sizes and the other way at others is how that happens",
					modules, want, fit.Size, previous)
			}
			previous = fit.Size
		}
	}
}

// TestTheQuietZoneNeverGoesBelowTheFloor is the specification's four modules,
// asserted across the size range rather than at the default.
//
// ISO/IEC 18004 requires four modules of quiet zone and scanners start failing
// against busy backgrounds below it. The size control derives the quiet zone
// now, so "four" stopped being a constant somebody sets and became an invariant
// of a search — which is exactly the kind of thing that holds for the sizes
// anybody tried and fails at 64 pixels.
func TestTheQuietZoneNeverGoesBelowTheFloor(t *testing.T) {
	if DefaultMargin != 4 {
		t.Fatalf("the quiet zone floor is %d modules and the specification's is 4; "+
			"this test is asserting the wrong number", DefaultMargin)
	}
	for _, modules := range productModuleCounts(t) {
		for _, want := range sweep() {
			fit, err := FitSize(modules, want)
			if err != nil {
				t.Fatalf("%d modules at %dpx: %v", modules, want, err)
			}
			if fit.Margin < 4 {
				t.Fatalf("%d modules at %dpx resolved to a %d-module quiet zone. Below "+
					"four the code stops scanning on a busy background, and a size "+
					"control that trades the quiet zone for pixels is trading the one "+
					"thing that must not be traded", modules, want, fit.Margin)
			}
			// And in pixels, which is what the drawing actually leaves empty.
			if origin := fit.Margin * fit.Scale; origin < 4*fit.Scale {
				t.Fatalf("%d modules at %dpx: %dpx of quiet zone for a %dpx module",
					modules, want, origin, fit.Scale)
			}
		}
	}
}

// TestTheQuietZoneIsTheMinimumThatScans is the M49 reopening's bound (F213).
//
// The owner, against the running product: *"Too much quiet zone at almost any
// size and it gets really bad at 2000px — the majority of the image should be
// QR code with just enough quiet zone to be usable."* The first FitSize
// searched the quiet zone as a second knob, so the margin grew whenever growing
// it landed nearer the requested pixel count — a few pixels of exactness bought
// with dozens of pixels of white — and at 2000px, where MaxScale capped what
// the scale could reach, the margin filled the rest of the request: a 29-module
// code drew a 16-module quiet zone and the code was under a quarter of the
// picture.
//
// The bound, in the reopened bullet's terms: at every acceptable size the
// majority of the image is code, and the margin does not grow past the floor at
// all — the whole remainder a module grid forces lives in the drawn size, where
// the form already reports it. "Majority" is asserted by area, which is the
// owner's sentence read strictly, and it is the tighter reading: the smallest
// code this package can encode is 21 modules, and at the four-module floor that
// is (21/29)² = 52% — the floor is the most quiet zone the majority claim
// leaves room for.
func TestTheQuietZoneIsTheMinimumThatScans(t *testing.T) {
	assertMinimal := func(t *testing.T, modules, want int) {
		t.Helper()
		fit, err := FitSize(modules, want)
		if err != nil {
			t.Fatalf("%d modules at %dpx: %v", modules, want, err)
		}
		if fit.Margin != DefaultMargin {
			t.Fatalf("%d modules at %dpx resolved to a %d-module quiet zone; the "+
				"minimum that scans is %d, and every module past it is white the "+
				"owner asked to have back", modules, want, fit.Margin, DefaultMargin)
		}
		span := modules + 2*fit.Margin
		if 2*modules*modules <= span*span {
			t.Fatalf("%d modules at %dpx: the code is %d of %d modules a side — "+
				"%.0f%% of the picture by area — and the majority of the image "+
				"must be code", modules, want, modules, span,
				100*float64(modules*modules)/float64(span*span))
		}
	}
	for _, modules := range productModuleCounts(t) {
		for _, want := range sweep() {
			assertMinimal(t, modules, want)
		}
	}
	// 2000px by name, where the owner saw it worst. In the sweep above too, but
	// this failure is the reopening's own case and deserves its own line.
	t.Run("at 2000px", func(t *testing.T) {
		for _, modules := range productModuleCounts(t) {
			assertMinimal(t, modules, MaxSize)
		}
	})
}

// TestASizeOutOfRangeIsRefusedRatherThanClamped. Clamping reports success for a
// setting nobody asked for — the same rule margin and scale have had since M41,
// applied to the control that replaced them.
func TestASizeOutOfRangeIsRefusedRatherThanClamped(t *testing.T) {
	for _, want := range []int{0, -1, MinSize - 1, MaxSize + 1, 1 << 20} {
		if _, err := FitSize(29, want); !errors.Is(err, ErrSizeOutOfRange) {
			t.Errorf("FitSize(29, %d) = %v, want ErrSizeOutOfRange", want, err)
		}
	}
	if _, err := FitSize(0, 300); err == nil {
		t.Error("a code of no modules was given a size")
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
	// since the M49 reopening resolves to margin 4 scale 9 on that link's
	// 37-module code. What this asserts is the read path, which consults no fit
	// at all, so the numbers only have to be a style Normalize accepts.
	stored := Style{Foreground: "#123a6b", Background: "#f5f7fa", Level: LevelQ, Margin: 4, Scale: 10}
	norm, errs := stored.Normalize()
	if len(errs) > 0 {
		t.Fatalf("the demo's stored style is refused: %v", errs)
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
	// without touching anything.
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

// TestARowFromTheOldSearchKeepsRenderingAndReportsAReSave is the stored-row
// half of the M49 reopening's second bullet (F213).
//
// **Two claims, and only the first is unconditional.** A style already in
// `qr_codes.style` is drawn from the margin and scale it carries — nothing on
// the read path consults [FitSize] — so a row written by the old margin search
// renders in the pixels it always did, whatever the arithmetic now prefers.
// That is the one that had to hold, because those rows are printed.
//
// The second is where the honesty is owed. Re-saving such a row goes through
// [FitSize] again, which now pins the quiet zone at [DefaultMargin] and cannot
// reproduce a 14-module one — so the drawn size moves, by at most half a span,
// and [SizeFit.Snapped] is true, which is what the form's flash message reads.
// Silently keeping the size by widening the margin back is the behaviour the
// owner reported; silently moving it without saying so would be the same defect
// wearing the other hat. So the assertion is that it moves *and* reports.
func TestARowFromTheOldSearchKeepsRenderingAndReportsAReSave(t *testing.T) {
	// Margin 14 at scale 7 is a real answer the old search gave: a 29-module
	// code asked for 400px resolved to exactly this, 399px with more than three
	// times the ISO quiet zone. It is the shape of row this milestone stopped
	// producing and did not stop honouring.
	old := Style{Foreground: "#123a6b", Background: "#f5f7fa", Level: LevelQ, Margin: 14, Scale: 7}
	norm, errs := old.Normalize()
	if len(errs) > 0 {
		t.Fatalf("a row the old search wrote is no longer a style Normalize accepts: %v", errs)
	}
	if norm.Margin != old.Margin || norm.Scale != old.Scale {
		t.Fatalf("Normalize rewrote a stored row: margin %d scale %d became margin %d "+
			"scale %d. A row already in the database is drawn as written, or every "+
			"printed code drawn from one changed appearance", old.Margin, old.Scale,
			norm.Margin, norm.Scale)
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

	// The re-save. It moves, because the margin it was written with is not one
	// the fit will choose any more.
	fit, err := FitSize(code.Size, drawn)
	if err != nil {
		t.Fatal(err)
	}
	if fit.Margin != DefaultMargin {
		t.Errorf("re-saving the row kept a %d-module quiet zone; a save is a new fit "+
			"and a new fit pins the zone at %d", fit.Margin, DefaultMargin)
	}
	span := code.Size + 2*fit.Margin
	if 2*abs(fit.Size-drawn) > span {
		t.Errorf("re-saving %dpx produced %dpx, %dpx away for a %d-module span. A "+
			"re-fit is a snap, not a resize: half a span is the whole of what it "+
			"may move", drawn, fit.Size, abs(fit.Size-drawn), span)
	}
	if !fit.Snapped() {
		t.Errorf("re-saving the row landed on %dpx and reported no snap. The size "+
			"moved from %dpx and the form's message is the only place a reader "+
			"learns it did", fit.Size, drawn)
	}
}
