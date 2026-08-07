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
// What is deliberately not asserted: which particular margin and scale a size
// resolves to. That is FitSize's answer and re-stating it here would be a copy
// of the implementation. What is asserted is the properties the answer has to
// have — whole modules, inside the bounds, never below the quiet-zone floor,
// never worse than the obvious alternative, and monotone.

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
// the renderer refuses; one worse than the naive alternative would be a search
// that is not searching; and one that is not monotone would be a tie-break that
// leans the wrong way at some sizes and not others, which is the failure a
// three-point test misses.
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

			// Never worse than the obvious alternative — hold the quiet zone at
			// the floor and round the scale. That is the version of this the
			// milestone rejected as too coarse, and it is the independent
			// yardstick the search has to beat or match.
			if naive, ok := floorSnap(modules, want); ok {
				if abs(fit.Size-want) > abs(naive-want) {
					t.Fatalf("%d modules at %dpx: fit chose %dpx, and holding the quiet "+
						"zone at %d modules gives %dpx which is nearer. The search over "+
						"the quiet zone is meant to improve on that, not lose to it",
						modules, want, fit.Size, DefaultMargin, naive)
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

// floorSnap is the one-knob version: quiet zone at the floor, scale rounded to
// the nearest whole pixel. Written out rather than called into FitSize, because
// a yardstick sharing the implementation measures nothing.
func floorSnap(modules, want int) (int, bool) {
	span := modules + 2*DefaultMargin
	scale := (want + span/2) / span
	if scale < MinScale {
		scale = MinScale
	}
	if scale > MaxScale {
		scale = MaxScale
	}
	if span*scale > MaxSize {
		return 0, false
	}
	return span * scale, true
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
	// The style cmd/lctl seeds on the demo, which is the actual pre-M49 row this
	// product has.
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
