package qr

import (
	"fmt"
	"testing"
)

// The level rule (M50.6's second reopening, D184 and D187).
//
// **What is being asserted is a bound, not a table.** D184 is one sentence — the
// level is the strongest one that does not push the symbol into a larger version
// — and the levels it produces are a consequence of ISO/IEC 18004's capacity
// tables rather than of anything this package decides. So these tests state the
// property over the whole version range and pin the measured answers only where
// a document names them.
//
// **D187 made the rule a floor rather than a default**, so the level a style
// names is the *minimum* the picture is drawn at and never the maximum. That
// splits what has to be asserted in two: at or below the free level the module
// count is [DefaultLevel]'s, which is what every stored size in the product is
// fitted against; above it the symbol grows, which is what a logo has always
// cost and what the fit sites already encode through [Encode] to find out.

// TestTheRuleNeverChangesTheModuleCount is the one that protects everything
// else.
//
// A stored [Style.Size] is fitted against a module count, and M49's claim is
// that the requested size is the size stored and drawn, exactly. If raising the
// level could move the module count, every stored size in the product would be
// fitted against one symbol and drawn as another — the defect F225 and F226
// were, reached through a door nobody would be watching. The rule is safe for
// exactly one reason and this is it: the level it returns encodes to the same
// matrix width as [DefaultLevel] does.
//
// It sweeps the whole version range at the floor **the product's own rows
// carry**, which is none at all. That `L` and `M` are the same floor as none —
// D187's rule binding upward, so they draw the same symbol — and that `Q` and
// `H` are floors which may cost a version are the test below's, at the range's
// ends and on the measured shapes: an encode at version 36 is milliseconds, and
// asking every level at every intermediate version put twenty seconds into
// `make check` to re-assert what the ends and the arithmetic already say.
func TestTheRuleNeverChangesTheModuleCount(t *testing.T) {
	lowest, highest := productVersions(t)
	for version := lowest; version <= highest; version++ {
		// **Parallel, because this sweep is where `make check` would feel the
		// rule.** Four encodes a version over thirty-four versions is a minute
		// under the race detector run in sequence, and the encodes are pure
		// functions of their payload with a mutex around the only shared state
		// (the memo above). Nothing here shares anything else.
		t.Run(fmt.Sprintf("v%02d", version), func(t *testing.T) {
			t.Parallel()
			content := payloadForVersion(t, version)
			base, err := encodeAt(content, DefaultLevel)
			if err != nil {
				t.Fatalf("at %s: %v", DefaultLevel, err)
			}
			// Through the unexported pair, so the level the rule chose is in hand
			// without a second resolution: LevelFor and Encode are one-line callers
			// of it, and the sweep is four encodes a version either way.
			drawn, level, err := encode(content, "")
			if err != nil {
				t.Fatalf("with no level: %v", err)
			}
			if level == "" {
				t.Fatal("this version resolved to no level at all")
			}
			if drawn.Size != base.Size {
				t.Fatalf("level %s encodes to %d modules and the rule drew %d. Every "+
					"stored size in this product is fitted against the first number "+
					"and drawn at the second, so they cannot differ",
					DefaultLevel, base.Size, drawn.Size)
			}
		})
	}
}

// TestTheRuleTakesTheStrongestFreeLevel is D184's sentence, stated as three
// claims because that is how many it is: the level is at least [DefaultLevel] —
// correction is never *given up* to shrink a picture — it costs no modules, and
// nothing stronger would have been free either. The third is what makes it *the
// highest* rather than merely one of them, and it is the half a rule like this
// loses silently.
//
// At the ends of the product's version range and on the four shapes D184
// measured. The sweep between the ends is the test above.
func TestTheRuleTakesTheStrongestFreeLevel(t *testing.T) {
	lowest, highest := productVersions(t)
	for _, content := range levelSamples(t, lowest, highest) {
		t.Run(fmt.Sprintf("%dbytes", len(content)), func(t *testing.T) {
			t.Parallel()
			width := make(map[Level]int, len(Levels))
			for _, level := range Levels {
				code, err := encodeAt(content, level)
				if err != nil {
					t.Fatalf("%d bytes at %s: %v", len(content), level, err)
				}
				width[level] = code.Size
			}
			got := LevelFor(content, "")
			if levelRank(got) < levelRank(DefaultLevel) {
				t.Fatalf("%d bytes: the rule answered %s, below the %s it starts from. "+
					"Correction is taken where it is free and never handed back",
					len(content), got, DefaultLevel)
			}
			if width[got] != width[DefaultLevel] {
				t.Fatalf("%d bytes: %s is %d modules and the rule chose %s at %d. The "+
					"whole rule is that it costs no version",
					len(content), DefaultLevel, width[DefaultLevel], got, width[got])
			}
			for _, stronger := range Levels {
				if levelRank(stronger) > levelRank(got) &&
					width[stronger] == width[DefaultLevel] {
					t.Fatalf("%d bytes: the rule chose %s, and %s is the same %d modules. "+
						"The rule is the *strongest* free level",
						len(content), got, stronger, width[DefaultLevel])
				}
			}
		})
	}
}

// TestTheRuleBindsALevelSomebodyNamed is D187, and it replaces the test that
// asserted the opposite.
//
// The build shipped the rule for an unset level only and honoured a named one
// exactly, on D185's ground — a `PUT` that sets a field and reads it back
// changed is the surprise this product refused for `scale`. The owner overruled
// it: **the rule binds everything below it.** So a named level is a floor. It is
// honoured upward, which is what keeps `H` under a logo, and ignored downward,
// which is what stops anybody asking for less correction than costs nothing.
//
// Four claims, and the last two are the ones a floor can lose silently:
//
//   - The drawn level is never below the named one. `H` is `H`, at whatever
//     version it costs — [Style.ForLogo] is that case and D141 is why.
//   - The drawn level is never below the free one. A named `L` is drawn at the
//     free level, so `L` is a value this product accepts and can never draw.
//   - Where the floor is at or below the free level, the symbol is
//     [DefaultLevel]'s. That is the half every stored size rests on.
//   - Where the floor is above it, the symbol is the floor's own — the drawing
//     obeys the field rather than quietly returning something smaller.
//
// The expected answers are derived from the four levels' own symbol widths,
// which is four encodes a sample, and each named level is resolved once through
// the unexported pair rather than twice through [LevelFor] and [Encode] — they
// are one-line callers of it and asking both would double the sweep's cost for
// no claim. Parallel over the samples for the same reason the module-count
// sweep is: the encodes are pure functions of their payload.
func TestTheRuleBindsALevelSomebodyNamed(t *testing.T) {
	lowest, highest := productVersions(t)
	for _, content := range levelSamples(t, lowest, highest) {
		t.Run(fmt.Sprintf("%dbytes", len(content)), func(t *testing.T) {
			t.Parallel()
			width := make(map[Level]int, len(Levels))
			for _, level := range Levels {
				code, err := encodeAt(content, level)
				if err != nil {
					t.Fatalf("%d bytes at %s: %v", len(content), level, err)
				}
				width[level] = code.Size
			}
			// The free level, derived rather than asked for: the strongest whose
			// symbol matches DefaultLevel's. TestTheRuleTakesTheStrongestFreeLevel
			// is what holds the product to it; this test needs it to know what a
			// floor below it should climb to.
			free := DefaultLevel
			for _, level := range Levels {
				if levelRank(level) > levelRank(free) && width[level] == width[DefaultLevel] {
					free = level
				}
			}
			for _, named := range Levels {
				want := named
				if levelRank(free) > levelRank(named) {
					want = free
				}
				drawn, got, err := encode(content, named)
				if err != nil {
					t.Fatalf("%d bytes at %s: %v", len(content), named, err)
				}
				if got != want {
					t.Errorf("%d bytes: a style naming %s is drawn at %s, want %s — the "+
						"rule is a floor, so the answer is the stronger of %s and the free %s",
						len(content), named, got, want, named, free)
				}
				if drawn.Size != width[want] {
					t.Errorf("%d bytes: a style naming %s draws a %d-module symbol where %s "+
						"alone is %d", len(content), named, drawn.Size, want, width[want])
				}
				if levelRank(named) <= levelRank(free) && drawn.Size != width[DefaultLevel] {
					t.Errorf("%d bytes: %s is at or below the free %s and drew %d modules "+
						"against %s's %d. A size stored against the second is drawn against "+
						"the first, and M49's claim is that they are one number",
						len(content), named, free, drawn.Size, DefaultLevel, width[DefaultLevel])
				}
				if named == LevelL && got == LevelL && free != LevelL {
					t.Errorf("%d bytes: a style naming L drew L while %s was free",
						len(content), free)
				}
			}
		})
	}
}

// levelSamples is the two ends of the product's version range and the four URL
// shapes D184 measured — the payloads worth asking every level about, as against
// the whole range, which the module-count sweep covers at the one floor the
// product's own rows carry.
func levelSamples(t *testing.T, lowest, highest int) []string {
	t.Helper()
	return []string{
		payloadForVersion(t, lowest), payloadForVersion(t, highest),
		"https://lnk.io/ab3x9?src=qr",
		"https://lnk.io/ab3x9?src=qr&qrc=autumn",
		"https://links.example.com/spring-sale?src=qr",
		"https://links.example.com/spring-sale?src=qr&qrc=autumn-poster",
	}
}

// TestTheDrawingCarriesTheChosenLevelsMatrix. Same width is not the same
// picture: the point of the rule is that the code carries more error correction,
// which is a different arrangement of the same number of modules. A rule that
// returned the floor's matrix would pass every size assertion in this file and
// buy nothing at all.
func TestTheDrawingCarriesTheChosenLevelsMatrix(t *testing.T) {
	content := "https://lnk.io/ab3x9?src=qr"
	chosen := LevelFor(content, "")
	if chosen == DefaultLevel {
		t.Fatalf("this payload resolves to %s, so it cannot tell the two matrices "+
			"apart; pick one the rule raises", chosen)
	}
	drawn, err := Encode(content, "")
	if err != nil {
		t.Fatal(err)
	}
	want, err := encodeAt(content, chosen)
	if err != nil {
		t.Fatal(err)
	}
	floor, err := encodeAt(content, DefaultLevel)
	if err != nil {
		t.Fatal(err)
	}
	same := func(a, b *Code) bool {
		if a.Size != b.Size {
			return false
		}
		for y := range a.Size {
			for x := range a.Size {
				if a.Dark(x, y) != b.Dark(x, y) {
					return false
				}
			}
		}
		return true
	}
	if !same(drawn, want) {
		t.Errorf("the picture is not the matrix level %s encodes; the rule reported a "+
			"level it did not draw", chosen)
	}
	if same(drawn, floor) {
		t.Errorf("the picture is byte for byte level %s's, so the extra correction the "+
			"rule claims to have taken is not in it", DefaultLevel)
	}
}

// TestTheProductsOwnShapesLandWhereD184Says. The four payloads D184 measured,
// with the levels it reports — the *ordinary* shapes at Q, where the default M
// was chosen by nothing, and the two the next level would cost a version at M.
//
// Pinned because they are the numbers the decision, the milestone file and the
// finding all quote: a change to this package that moved them would leave three
// documents describing a product that had stopped doing it.
//
// **Asked at three floors, because D187 says all three answer the same.** Its
// own example is *a named `L` on a payload where `Q` is free is drawn at `Q`*,
// and the first row is that payload.
func TestTheProductsOwnShapesLandWhereD184Says(t *testing.T) {
	for _, c := range []struct {
		content string
		want    Level
	}{
		{"https://lnk.io/ab3x9?src=qr", LevelQ},
		{"https://lnk.io/ab3x9?src=qr&qrc=autumn", LevelM},
		{"https://links.example.com/spring-sale?src=qr", LevelQ},
		{"https://links.example.com/spring-sale?src=qr&qrc=autumn-poster", LevelM},
	} {
		for _, floor := range []Level{"", LevelL, LevelM} {
			if got := LevelFor(c.content, floor); got != c.want {
				t.Errorf("%q (%d bytes) at floor %q resolves to %s, want %s",
					c.content, len(c.content), floor, got, c.want)
			}
		}
	}
}

// TestAnUnencodableContentReportsTheFloor. LevelFor is called beside a size that
// is already reporting the failure, so it answers rather than refuses — and what
// it answers is the floor, which is the only honest thing left to say: the free
// level is not knowable without a symbol, and there is no symbol.
func TestAnUnencodableContentReportsTheFloor(t *testing.T) {
	long := padTo(MaxContent) + "x"
	for _, c := range []struct{ named, want Level }{
		{LevelQ, LevelQ},
		{LevelH, LevelH},
		{"", DefaultLevel},
		// Below DefaultLevel, so the floor is DefaultLevel — the same answer the
		// rule gives it on content that does encode (D187).
		{LevelL, DefaultLevel},
	} {
		if got := LevelFor(long, c.named); got != c.want {
			t.Errorf("content past MaxContent naming %q resolved to %s, want %s",
				c.named, got, c.want)
		}
	}
}
