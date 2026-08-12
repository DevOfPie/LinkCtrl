package ui

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The QR panel's two view-layer claims from the M49 reopening (F213), asserted
// on both surfaces that render the partial.
//
// **Both, because the partial is rendered twice.** `linkQRView` is one struct
// embedded in the link page and in the panel's own page, and m48.md's
// requirement is that they stay the same markup — a bound that held on one of
// them and not the other would be exactly the drift that requirement exists to
// catch. The list is written out rather than discovered so that a third surface
// has to be added here deliberately.
var qrPanelPages = []string{"link_detail", "link_qr"}

// renderQRPanel renders one of those surfaces with the QR section on screen.
//
// The link page draws one tab at a time since M47's reopening, so the section is
// absent from every rendering but `?tab=qr` — a test that rendered the landing
// tab would pass on a page that had lost the panel entirely. The panel's own
// page reads no tab and takes the override harmlessly.
func renderQRPanel(t *testing.T, page string) string {
	t.Helper()
	return mainOf(t, renderPage(t, page, map[string]any{"Tab": "qr"}))
}

// qrPreviewFrame matches the frame the preview is drawn into: the element whose
// class list states the footprint.
//
// Anchored on the border utility and the flex box together, because that pair
// is what the frame is and neither alone is distinctive on a page of cards.
var qrPreviewFrame = regexp.MustCompile(`<div class="(flex [^"]*rounded-md border border-line-strong[^"]*)">\s*<svg`)

// qrPreviewBox is the footprint, in Tailwind's spacing units, that the frame is
// required to declare.
//
// **A number rather than "some bound", which is the whole point of the
// reopening.** The frame used to be `inline-block`, so it took whatever size
// `internal/qr` had drawn the code at: the owner set 2000px and the page grew
// under the picture. 18rem is a preview — big enough to see the code is a code
// and to read whether the colours are the ones chosen, small enough that the
// column does not move when the number changes.
//
// Square, because a QR code is. Stating only the height would leave the `width`
// attribute `internal/qr` writes to fight it, which is the same reasoning
// `ui.QRThumbClass` carries for the thumbnail.
const qrPreviewBox = "72"

// TestTheQRPreviewKeepsAFixedFootprint is the reopening's first bullet.
//
// **What is asserted, and what cannot be.** That the frame declares a fixed box
// and that the drawing inside it is bounded by its own class — both readable
// from markup. What a Go test cannot do is lay the page out, so "never grows the
// page" is enforced here as the two declarations that make it true rather than
// as a measurement: a `w-72 h-72` frame is 18rem whatever it contains, and
// `qr.FluidClass` is `max-w-full h-auto` on the <svg>, which shrinks a wider
// picture into the frame and leaves a narrower one alone.
//
// The <svg>'s half is named rather than spelled — the fixture carries
// `qr.FluidClass` exactly as the emitter writes it, so this cannot drift from
// what internal/qr produces without the fixture drifting first, which
// TestAnInlinedCodeFitsTheBoxItIsDrawnInto catches at the other end.
func TestTheQRPreviewKeepsAFixedFootprint(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)

		m := qrPreviewFrame.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s draws no QR preview frame around its <svg>. The frame is "+
				"what states the footprint; without it the preview is whatever "+
				"size the stored code happens to be, which is F213", page)
		}
		classes := strings.Fields(m[1])
		has := func(want string) bool {
			for _, c := range classes {
				if c == want {
					return true
				}
			}
			return false
		}
		for _, want := range []string{"h-" + qrPreviewBox, "w-" + qrPreviewBox, "max-w-full"} {
			if !has(want) {
				t.Errorf("%s: the QR preview frame is %q and is missing %q. The "+
					"footprint has to be a number the markup states — both "+
					"dimensions, so the <svg>'s own width and height attributes "+
					"cannot fight it, and max-w-full so a narrow column wins over "+
					"the box rather than overflowing", page, m[1], want)
			}
		}
		if has("inline-block") {
			t.Errorf("%s: the QR preview frame is %q. `inline-block` shrink-wraps "+
				"the drawing, which is precisely how the frame grew with the "+
				"stored size (F213)", page, m[1])
		}

		// And the drawing shrinks into it. The class is on the <svg> the
		// emitter writes, not on the frame.
		svg := body[strings.Index(body, m[0]):]
		svg = svg[:strings.Index(svg, ">")+1]
		if !strings.Contains(svg, "max-w-full") {
			t.Errorf("%s: the preview <svg> is %q and does not bound itself. A "+
				"fixed frame around an unbounded picture is a picture that "+
				"overflows the frame", page, svg)
		}
	}
}

// TestTheQRPreviewSaysWhichSizeIsServed is the other half of that bullet: once
// the preview stops tracking the size 1:1, the page owes the reader the number.
//
// Without it the size control reads as having no effect — the reader types 2000,
// presses save, and the picture is the same size it was. The number under the
// picture is what both downloads and the API serve, and that sentence is the
// only place the page says so.
//
// **Both states, because only one link in the product is in the first one.**
// `linkQRView.QRSize` is the served size either way, but it is a *stored* size
// only when `QRStored` is true; otherwise it is `QROutputSize` over the default
// style. `cmd/lctl/demo_phase2.go` seeds exactly one styled link, so the
// unstored branch is what almost every reader meets — a caption asserting "the
// stored size" there states the opposite of the distinction the reopening
// asked to have stated. Asserted in both directions: the word is present where
// a style is stored and absent where none is.
func TestTheQRPreviewSaysWhichSizeIsServed(t *testing.T) {
	for _, page := range qrPanelPages {
		size, ok := pageData(t)[page].(map[string]any)["QRSize"].(int)
		if !ok {
			t.Fatalf("the %s fixture carries no QRSize; this test measures the "+
				"number the page prints against the one it was given", page)
		}
		want := strconv.Itoa(size) + " pixels"

		// A stored style: the number is the one the form wrote, and the page
		// says so.
		body := renderQRPanel(t, page)
		if !strings.Contains(body, want) {
			t.Errorf("%s never says %q. The preview is drawn to fit a fixed frame, "+
				"so the size the form wrote is no longer readable off the picture "+
				"and has to be stated (F213)", page, want)
		}
		if !strings.Contains(body, "stored size") {
			t.Errorf("%s prints a size without saying it is the stored one. Drawn "+
				"and stored are different numbers now, and the distinction is the "+
				"reopening's requirement", page)
		}

		// No stored style — the common case. The number is still owed, because
		// it is still what the downloads and the API serve; calling it stored
		// is not.
		body = mainOf(t, renderPage(t, page, map[string]any{"Tab": "qr", "QRStored": false}))
		if !strings.Contains(body, want) {
			t.Errorf("%s drops the size when no style is stored. The default is "+
				"still the size both downloads and the API serve, and the reader "+
				"has no other way to read it off a preview drawn to fit a frame", page)
		}
		if strings.Contains(body, "stored size") {
			t.Errorf("%s calls the size stored on a link that has stored none. "+
				"Every link but the one the demo seeds renders this branch, so "+
				"this is the sentence most readers get (F213)", page)
		}
		if !strings.Contains(body, "by default") {
			t.Errorf("%s prints a size on an unstyled code without saying it is "+
				"the default. Stating the served-versus-drawn split is the "+
				"reopening's requirement, and it is only stated if it is true "+
				"in both states", page)
		}
	}
}

// TestTheQRResetButtonSaysRestoreDefaults is the reopening's third bullet, which
// is one label — and one label is worth a test because nothing else reads it.
//
// The old wording named the colours, which was true when colours were all this
// form carried; M49 put a size on it and reset clears that too. Both directions
// are asserted: the new label present, the old one gone from the whole page. A
// test for the new string alone would pass on a page that had grown a second
// button.
func TestTheQRResetButtonSaysRestoreDefaults(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)

		if !strings.Contains(body, ">Restore defaults<") {
			t.Errorf("%s has no \"Restore defaults\" button. The fixture stores a "+
				"style, so the control is rendered; the owner asked for this "+
				"wording (F213)", page)
		}
		if strings.Contains(body, "Back to black on white") {
			t.Errorf("%s still offers \"Back to black on white\". The button clears "+
				"the size as well as the two colours, so the label named a third "+
				"of what it does", page)
		}
	}
}
