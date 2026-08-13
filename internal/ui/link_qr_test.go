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

// --- the size control, at the second M49 reopening ---------------------------

// TestTheSizeControlIsASliderAndANumber is the reopening's fourth bullet (D182,
// F221).
//
// **Four claims, and each one is a way the control could ship broken.** That the
// slider exists at all and spans this code's whole range, so both ends are
// reachable rather than only the marks. That the marks are the owner's eight,
// declared through a `<datalist>` — HTML's own way of saying them, which is what
// keeps `script-src 'self'` untouched. That the number beside it is still an
// editable input and not a label, because typing a value is half of what was
// asked for. And that the witness the server resolves the two against is on the
// form, because without it the rule in `httpx.requestedQRSize` has nothing to
// compare and the number would silently win every drag.
//
// The bounds are read off the fixture rather than typed here: they are passed
// into the template precisely so the form and internal/qr cannot drift, and a
// test that hardcoded them would be a third copy to drift from.
func TestTheSizeControlIsASliderAndANumber(t *testing.T) {
	for _, page := range qrPanelPages {
		data, ok := pageData(t)[page].(map[string]any)
		if !ok {
			t.Fatalf("the %s fixture is not a map", page)
		}
		body := renderQRPanel(t, page)

		lo, ok := data["QRMinSize"].(int)
		if !ok {
			t.Fatalf("the %s fixture carries no QRMinSize", page)
		}
		hi, ok := data["QRMaxSize"].(int)
		if !ok {
			t.Fatalf("the %s fixture carries no QRMaxSize", page)
		}
		slider := elementWithID(t, body, "qr_size_slider")
		for _, want := range []string{
			`type="range"`, `name="size_slider"`, `list="qr_size_stops"`,
			`min="` + strconv.Itoa(lo) + `"`, `max="` + strconv.Itoa(hi) + `"`,
		} {
			if !strings.Contains(slider, want) {
				t.Errorf("%s: the size slider is missing %s. Without the range and both "+
					"bounds it is not a slider over the sizes this code can be drawn "+
					"at:\n%s", page, want, slider)
			}
		}

		// The marks, every one the fixture was handed. A datalist that dropped
		// one is a stop the owner asked for and the control does not offer.
		stops, ok := data["QRSizeStops"].([]int)
		if !ok || len(stops) == 0 {
			t.Fatalf("the %s fixture carries no QRSizeStops", page)
		}
		if !strings.Contains(body, `<datalist id="qr_size_stops">`) {
			t.Errorf("%s draws no datalist for the size stops. The marks are declared "+
				"in HTML because a script to place them is a script this page's CSP "+
				"refuses", page)
		}
		for _, s := range stops {
			if !strings.Contains(body, `<option value="`+strconv.Itoa(s)+`">`) {
				t.Errorf("%s: the slider offers no stop at %d", page, s)
			}
		}

		// And the number, still typed into rather than shown.
		number := elementWithID(t, body, "qr_size")
		for _, want := range []string{`type="number"`, `name="size"`,
			`min="` + strconv.Itoa(lo) + `"`, `max="` + strconv.Itoa(hi) + `"`} {
			if !strings.Contains(number, want) {
				t.Errorf("%s: the size number box is missing %s; a value you cannot type "+
					"is half the control the owner asked for:\n%s", page, want, number)
			}
		}

		// The witness. Nothing in the browser syncs the two inputs, so this is
		// what lets the server say which of them moved.
		if !strings.Contains(body, `name="size_shown"`) {
			t.Errorf("%s carries no size_shown. Without it the slider and the number "+
				"are two sources of truth for one setting, which is the risk the "+
				"milestone named", page)
		}
	}
}

// TestTheSizePanelNoLongerPromisesASnap is the other half of the first bullet:
// the sentence that explained the snap has to go with the snap.
//
// A page that still said the size moves to the nearest whole-module one would be
// describing the behaviour the owner rejected, on the tab where they rejected
// it — and a reader would believe the page over the product.
func TestTheSizePanelNoLongerPromisesASnap(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)
		for _, gone := range []string{"snaps to the nearest", "keeps them whole"} {
			if strings.Contains(body, gone) {
				t.Errorf("%s still says %q. The size set is the size stored since D182, "+
					"so the sentence explaining why it was not is describing a product "+
					"that no longer exists", page, gone)
			}
		}
	}
}

// elementWithID cuts one element out of rendered markup by its id.
func elementWithID(t *testing.T, body, id string) string {
	t.Helper()
	at := strings.Index(body, `id="`+id+`"`)
	if at < 0 {
		t.Fatalf("no element with id %q in the rendered panel", id)
	}
	start := strings.LastIndex(body[:at], "<")
	end := strings.Index(body[at:], ">")
	if start < 0 || end < 0 {
		t.Fatalf("the element with id %q is not a complete tag", id)
	}
	return body[start : at+end+1]
}

// --- the upload control, at the F214 reopening -------------------------------

// logoForm cuts the logo upload form out of a rendered panel.
//
// Anchored on the action rather than on a class, because the class list is
// exactly the thing these tests must be free to change: what is asserted below
// is the behaviour the attributes declare, not how the control is painted.
func logoForm(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `/qr/logo" enctype="multipart/form-data"`)
	if i < 0 {
		t.Fatal("the panel renders no logo upload form")
	}
	start := strings.LastIndex(body[:i], "<form")
	end := strings.Index(body[i:], "</form>")
	if start < 0 || end < 0 {
		t.Fatal("the logo upload form is not a complete element")
	}
	return body[start : i+end]
}

// TestChoosingALogoAppliesIt is F214(c): the two-step went away.
//
// **The button is asserted absent, not just the trigger present.** A form that
// grew an htmx trigger and kept its submit control would pass a test for the
// trigger alone and would still be the interference the owner reported —
// choosing a file would apply it *and* leave a button implying it had not.
//
// The swap target is asserted on the page as well as on the form. htmx does not
// swap a 4xx, so the refusal path renders the page at 200 and this selects the
// panel out of it; a target the page does not contain would silently swap
// nothing, which is the failure mode this control was reopened for in the first
// place.
func TestChoosingALogoAppliesIt(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)
		form := logoForm(t, body)

		for _, want := range []string{
			`hx-trigger="change"`, `hx-post="/links/`, `hx-encoding="multipart/form-data"`,
			`hx-target="#qr"`, `hx-select="#qr"`,
		} {
			if !strings.Contains(form, want) {
				t.Errorf("%s: the logo form is missing %s, so choosing a file does not "+
					"apply it (F214c)", page, want)
			}
		}
		if strings.Contains(form, "<button") || strings.Contains(form, `type="submit"`) {
			t.Errorf("%s: the logo form still carries a submit control. The file applies "+
				"on selection, so the button is the second step F214 asked to remove:\n%s",
				page, form)
		}
		if !strings.Contains(body, `id="qr"`) {
			t.Errorf("%s renders no id=\"qr\", which is what the logo form's refusal "+
				"swaps; without it htmx replaces nothing and the refusal is invisible",
				page)
		}
	}
}

// TestTheBrowseControlAcknowledgesTheClick is F214(b).
//
// The OS file dialog takes about a second to open and the control said nothing
// in the meantime. What can be asserted from here is that the class the state
// hangs off is on the input and that the stylesheet actually carries rules for
// it — a class with no rules is the same dead control with a longer attribute.
//
// `:focus` is checked by name because it is the one that spans the wait: the
// press is over in a moment and the input holds focus for as long as the dialog
// is up. **And the `:not(:focus-visible)` is checked with it**, because focus
// outlives the dialog: keyed on focus alone the rule paints a permanent pressed
// background on anybody who merely tabs onto the control. That the exclusion
// does what it is written for — pointer focus in, keyboard focus out — is a
// browser fact and is driven in `tools/agent-browser/specs/qr-logo.spec.mjs`;
// what belongs here is that the selector shipped.
func TestTheBrowseControlAcknowledgesTheClick(t *testing.T) {
	css := builtStylesheet(t)
	for _, want := range []string{
		".file-pick:active::file-selector-button",
		".file-pick:focus:not(:focus-visible)::file-selector-button",
		"form.htmx-request .file-pick",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the built stylesheet has no rule for %s. The state has to ship in "+
				"app.css: `style-src 'self'` refuses an injected stylesheet, which is why "+
				"htmx's own indicator styles are off (F206)", want)
		}
	}
	// The inset is a theme token, on the same terms the templates are held to by
	// TestTemplatesUseThemeTokensOnly — which scans templates and therefore
	// cannot see a palette value written in the stylesheet itself. This rule is
	// the only place that gap has bitten, so it is closed here rather than by a
	// second scanner: 0.25 black reads as a notch over the light theme's
	// line-strong and as nothing at all over the dark theme's.
	if !strings.Contains(css, "var(--t-press-shadow)") {
		t.Error("the pressed state's shadow is not drawn from --t-press-shadow, so it " +
			"is a palette value in a block whose own comment promises theme tokens")
	}
	for _, page := range qrPanelPages {
		if form := logoForm(t, renderQRPanel(t, page)); !strings.Contains(form, "file-pick") {
			t.Errorf("%s: the file input does not carry file-pick, so none of those "+
				"rules reaches it", page)
		}
	}
}

// --- the codes list, at M50's reopening --------------------------------------

// TestEveryCodeCanBeRemovedAndMadeTheDefault is the reopening's third bullet
// (D183, F222).
//
// **The owner reported it against the running product**: *"As long as there are
// multiple QR codes any of them should be able to be removed, currently the
// first one cannot be removed."* The first row had no remove control at all —
// not one that refused — because the default code *was* the row with the empty
// slug and deleting it would have left every already-printed picture resolving
// to nothing.
//
// Two claims, and the second is what stops the fix from being cosmetic. Every
// row carries a remove control, which is what was asked for. And every row that
// is not the default carries one that makes it the default, which is what makes
// the removal safe to offer: the flag has somewhere to go, and a reader who
// wanted a different code answering for their old posters can say so rather than
// deleting one to find out.
func TestEveryCodeCanBeRemovedAndMadeTheDefault(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)

		// The fixture carries two codes, so both rows are removable — and the
		// count is what makes this a claim about *every* row rather than about
		// the one that always had the control.
		if n := strings.Count(body, `name="remove"`); n != 2 {
			t.Errorf("%s draws %d remove controls over a two-code list, want 2. "+
				"The first row is the default, and before D183 it had none: its "+
				"identity was the absence of a slug, which is what made it "+
				"undeletable (F222)", page, n)
		}
		// One, not two: the code that already holds the flag has nowhere to
		// move it to, and a control that says "make this the default" on the
		// default is a control that does nothing.
		if n := strings.Count(body, `name="make_default"`); n != 1 {
			t.Errorf("%s draws %d \"make default\" controls over a two-code list "+
				"whose first row is the default, want 1. Removing the flag-holder "+
				"promotes a code the reader did not pick, so choosing one has to "+
				"be reachable without deleting anything", page, n)
		}
		if !strings.Contains(body, ">Make default<") {
			t.Errorf("%s has no \"Make default\" button", page)
		}
	}
}

// TestTheDefaultCodeSaysWhatBeingTheDefaultMeans is the reader's half of D183.
//
// The flag decides where a picture carrying no code of its own is counted, and
// that is not deducible from the word *default* — a reader who has printed a
// poster wants to know which row their poster's scans are in. The row says so.
//
// It also asserts the old sentence is gone from the whole page. That one named
// the default as *the code every already-printed picture of this link resolves
// to*, which stopped being a property of one row the moment the flag could move.
func TestTheDefaultCodeSaysWhatBeingTheDefaultMeans(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)

		if !strings.Contains(body, "a scan carrying no code of its own is counted against this one") {
			t.Errorf("%s does not say what the default code is for. The flag decides "+
				"where an untagged picture's scans land, and a row labelled only "+
				"\"the default\" leaves a reader to guess (D183)", page)
		}
		if strings.Contains(body, "the code every already-printed picture of this link resolves to") {
			t.Errorf("%s still describes the default as the code every printed picture "+
				"resolves to. That was true while the default was the row without a "+
				"slug; the flag moves now, and so does what it describes", page)
		}
	}
}

// TestALinkWithOneCodeSaysWhyItCannotBeRemoved is the other end of the same
// bullet: *the last remaining code cannot be removed, and that refusal says
// why.*
//
// The control is absent rather than disabled, so without this sentence the page
// would be back to the shape the owner reported — a row with no way to remove it
// and nothing saying so. The one-code state is the state nearly every link on an
// instance is in, so it is also the state most readers meet first.
func TestALinkWithOneCodeSaysWhyItCannotBeRemoved(t *testing.T) {
	for _, page := range qrPanelPages {
		body := mainOf(t, renderPage(t, page, map[string]any{
			"Tab": "qr",
			"QRCodes": []map[string]any{{
				"Slug": "d3f4u1t0", "Label": "", "Name": "The original code",
				"Size": 740, "Default": true, "Selected": true,
				"Panel":       "/links/0198c9c5-0000-7000-8000-000000000001/qr?code=d3f4u1t0",
				"Download":    "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr/codes/d3f4u1t0/image.svg",
				"DownloadPNG": "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr/codes/d3f4u1t0/image.png",
			}},
		}))

		if strings.Contains(body, `name="remove"`) {
			t.Errorf("%s offers to remove a link's only QR code. A link always has "+
				"one, and the service refuses — a control whose only outcome is a "+
				"refusal is a control that should not be drawn", page)
		}
		if !strings.Contains(body, "A link always has a QR code, so this one cannot be removed") {
			t.Errorf("%s draws a single-code list with no remove control and no reason. "+
				"The absence is exactly what the owner reported as the defect, so "+
				"the reason is what distinguishes this from it (D183)", page)
		}
	}
}
