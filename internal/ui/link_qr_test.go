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

// qrSelectFor is where a row of the codes list goes on each surface, mirroring
// httpx.qrSelectPath (M50.8's third reopening).
//
// The two are no longer one string. The link page names the code on itself so
// the tab can be swapped in place; the panel page, which has no tab strip, still
// navigates to its own route. A fixture handing both surfaces the same row would
// render markup internal/httpx never builds, which is the failure F191 and F200
// both recorded one attribute at a time.
func qrSelectFor(page, slug string) string {
	const linkID = "0198c9c5-0000-7000-8000-000000000001"
	if page == "link_qr" {
		return "/links/" + linkID + "/qr?code=" + slug
	}
	return "/links/" + linkID + "?tab=qr&code=" + slug
}

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

// renderQRPanelAsReader renders the same two surfaces for somebody holding
// `links.read` and not `links.update`.
//
// **The fixture's owner holds every permission, and that is why this exists.**
// A page test wants every branch drawn, so `owner()` grants the lot — which
// means an assertion about what is *on* the page cannot tell a control every
// reader gets from one only an editor gets. M50.7's default indicator was
// rendered inside the `links.update` gate and every count above it passed;
// what it bought is legibility rather than an action, so gating it delivered
// nothing to the class of user who can only look (D190).
//
// `owner()` builds its permission map fresh on each call, so removing one grant
// from the value it returns cannot reach another test.
func renderQRPanelAsReader(t *testing.T, page string) string {
	t.Helper()
	id := owner()
	delete(id.perms, "links.update")
	if id.Can("links.update") || !id.Can("links.read") {
		t.Fatal("the read-only identity is not read-only, so anything asserted " +
			"against it is asserted against an editor")
	}
	return mainOf(t, renderPage(t, page, map[string]any{"Tab": "qr", "Identity": id}))
}

// The two default glyphs, told apart by the one thing that differs: both draw
// the same ring, and only `icon_default_on` fills its centre.
//
// Spelled as the markup icons.html emits rather than as the define's name,
// because the name does not survive rendering — a test keyed on "icon_default_on"
// is a test of a string that appears in no rendered page at all.
const (
	qrDefaultRing   = `<circle cx="12" cy="12" r="8.6"`
	qrDefaultFilled = `<circle cx="12" cy="12" r="4.2" fill="currentColor"/>`
)

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

// TestThePreviewDoesNotCallASizeStored is what replaced
// TestTheQRPreviewSaysWhichSizeIsServed at M49's third reopening (D185).
//
// **The paragraph that test guarded is gone, and this is the assertion that it
// stays gone.** It printed the served size under the picture and named it *the
// stored size* on a styled code, because the first reopening required the
// stored-vs-drawn distinction be stated where the preview renders. D182 then
// made the two one number, so there was no distinction to state — and the owner,
// reading the sentence they had commissioned, read *stored* as bytes: *"The
// message specifies that the stored size is X pixels, which is not an amount of
// data or a true representation of size of the image."*
//
// Deleting a test and asserting nothing would leave the removal indistinguishable
// from nobody having looked, which is why this stands in its place. The number
// itself is not asserted absent — the size control below the preview carries it
// in a `value` attribute and must — only the words that made it a claim about
// storage.
func TestThePreviewDoesNotCallASizeStored(t *testing.T) {
	for _, page := range qrPanelPages {
		for _, stored := range []bool{true, false} {
			body := mainOf(t, renderPage(t, page,
				map[string]any{"Tab": "qr", "QRStored": stored}))
			if strings.Contains(body, "stored size") {
				t.Errorf("%s (QRStored=%v) calls a size stored. The stored size and "+
					"the drawn size have been one number since D182, and the word "+
					"reads as an amount of data — which is how the owner read it "+
					"when they asked for the paragraph to go", page, stored)
			}
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
// **The button is asserted absent, not just the trigger present.** A control
// that grew an htmx trigger and kept its submit alongside would pass a test for
// the trigger alone and would still be the interference the owner reported —
// choosing a file would apply it *and* leave a button implying it had not.
//
// The swap target is asserted on the page as well as on the control. htmx does
// not swap a 4xx, so the refusal path renders the page at 200 and this selects
// the panel out of it; a target the page does not contain would silently swap
// nothing, which is the failure mode this control was reopened for in the first
// place.
//
// **The attributes are read off the input rather than off the form** since
// M50.8's second reopening (F246b). The input moved into the style form's grid
// and reaches its own form by `form="qr-logo-upload"`, and a `change` event
// bubbles up the DOM rather than to the form a control is associated with — so
// a trigger left on the form would never fire. The association is asserted with
// them: an input carrying every htmx attribute and naming no form posts a body
// with no `next` and no `code` in it, which the handler answers by sending the
// reader somewhere they did not ask to go.
func TestChoosingALogoAppliesIt(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)
		form := logoForm(t, body)
		input := elementWithID(t, body, "qr_logo")

		for _, want := range []string{
			`hx-trigger="change"`, `hx-post="/links/`, `hx-encoding="multipart/form-data"`,
			`hx-target="#qr"`, `hx-select="#qr"`, `form="qr-logo-upload"`,
		} {
			if !strings.Contains(input, want) {
				t.Errorf("%s: the logo input is missing %s, so choosing a file does not "+
					"apply it (F214c, F246b):\n  %s", page, want, input)
			}
		}
		if !strings.Contains(form, `id="qr-logo-upload"`) {
			t.Errorf("%s: the upload form does not carry the id the input names, so the "+
				"input belongs to no form at all:\n%s", page, form)
		}
		// **No filter on what it posts, and that is measured rather than assumed.**
		// htmx serializes the requesting element plus `elt.form || closest(elt,
		// 'form')` — the association wins, so the body is the file and this form's
		// two hidden fields, not the style form's. An `hx-params` filter stood here
		// on the opposite belief; the browser spec reads the FormData htmx builds
		// and is what settled it. If that ever stops being true the spec goes red,
		// which is where a claim about htmx belongs.
		if strings.Contains(input, "hx-params") {
			t.Errorf("%s: the logo input narrows what it posts. Nothing needs narrowing "+
				"— htmx takes the owner form, not the ancestor — so this is machinery "+
				"guarding a body it cannot see:\n  %s", page, input)
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
		// Both, since F246b. htmx marks the element that issued the request, and
		// that is the input now the attributes live on it — the form selector is
		// the one that used to do the work and would do it again if they moved
		// back.
		"form.htmx-request .file-pick",
		".file-pick.htmx-request",
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
		input := elementWithID(t, renderQRPanel(t, page), "qr_logo")
		if !strings.Contains(input, "file-pick") {
			t.Errorf("%s: the file input does not carry file-pick, so none of those "+
				"rules reaches it:\n  %s", page, input)
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
//
// **Both claims survive M50.7 and only their markup moved**: the worded
// buttons became icons, so what is counted here is the submit's `name` rather
// than its text. The names are what the handler branches on, so counting them
// is closer to the claim than counting labels ever was — and the labels
// themselves are TestTheDefaultIsAnIconOnEveryRow's and
// TestRemoveIsAMinusThatSaysWhatItRemoves' to assert.
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
		// One submit, not two: the code that already holds the flag has nowhere
		// to move it to, so its own icon is inert rather than a second way to
		// write a value that is already written (D188).
		if n := strings.Count(body, `name="make_default"`); n != 1 {
			t.Errorf("%s draws %d \"make default\" submits over a two-code list "+
				"whose first row is the default, want 1. Removing the flag-holder "+
				"promotes a code the reader did not pick, so choosing one has to "+
				"be reachable without deleting anything", page, n)
		}
	}
}

// --- the row, relaid out at M50.7 --------------------------------------------

// qrRows cuts the codes list's <li> elements out of a rendered panel.
//
// Anchored on the list's own border classes rather than on a marker inside a
// row, so what the assertions below read is the whole row including the parts
// that carry no text at all.
func qrRows(t *testing.T, body string) []string {
	t.Helper()
	start := strings.Index(body, `<ul class="divide-y divide-line rounded-md border border-line">`)
	if start < 0 {
		t.Fatal("the panel renders no codes list")
	}
	end := strings.Index(body[start:], "</ul>")
	if end < 0 {
		t.Fatal("the codes list is not a complete element")
	}
	rows := strings.Split(body[start:start+end], "<li ")
	if len(rows) < 2 {
		t.Fatalf("the codes list holds %d rows, want at least 1", len(rows)-1)
	}
	return rows[1:]
}

// TestSelectingACodeDoesNotLeaveTheLinkPage is M50.8's third reopening, from
// F246(d) and F244(b) — which are one navigation and are answered together.
// Owner: *"the selected code is changed with the default, which means we should
// be able to prevent scrolling when selecting a different code as well?"*
//
// Every row went to `/links/{id}/qr?code=` on both surfaces, so picking a code
// on the link page was a document load onto a page with no link heading row,
// starting at the top. The answer is that there is no load: the row is the tab
// strip's own mechanism (partials/link_tabs.html) carrying `&code=` as well as
// `?tab=qr`, so `#link-tabs` is swapped in place and everything outside it —
// the heading row and its thumbnail — is never redrawn.
//
// **Three things are asserted and they fail separately.** That the href is the
// link page rather than the panel route, which is what a reader with the script
// blocked follows (D178). That the swap attributes are all four of the strip's,
// because three of four is a link that navigates. And that `hx-get` is the same
// string as the href, because two different URLs would mean the scripted reader
// and the script-blocked one see different codes.
//
// The panel's own page is asserted in the negative: it has no `#link-tabs` to
// swap, so it keeps the plain link it always had and htmx is handed nothing it
// cannot place. Whether the swap actually redraws the tab, and whether the
// position and the thumbnail survive it, is the kept spec's
// (tools/agent-browser/specs/qr-tab-controls.spec.mjs).
func TestSelectingACodeDoesNotLeaveTheLinkPage(t *testing.T) {
	const linkID = "0198c9c5-0000-7000-8000-000000000001"
	swap := []string{
		`hx-target="#link-tabs"`, `hx-select="#link-tabs"`,
		`hx-swap="outerHTML"`, `hx-push-url="true"`,
	}

	for _, page := range qrPanelPages {
		rows := qrRows(t, renderQRPanel(t, page))
		if len(rows) < 2 {
			t.Fatalf("%s renders %d code rows; the claim is about picking one of "+
				"several, so a one-row fixture asserts nothing", page, len(rows))
		}
		for i, row := range rows {
			at := strings.Index(row, `<a href="/links/`)
			if at < 0 {
				t.Fatalf("%s row %d carries no link at all, so nothing selects it", page, i)
			}
			anchor := row[at : at+strings.Index(row[at:], ">")]

			if page == "link_qr" {
				// The bookmarkable surface, unchanged. It is also the assertion
				// that the swap is conditional rather than always rendered:
				// htmx raises a target error where `#link-tabs` does not exist.
				if !strings.Contains(anchor, `href="/links/`+linkID+`/qr?code=`) {
					t.Errorf("the panel page's row %d no longer links to its own route; "+
						"it has no tab strip to swap, so this href is the whole "+
						"mechanism:\n  %s", i, anchor)
				}
				for _, hx := range append([]string{"hx-get="}, swap...) {
					if strings.Contains(anchor, hx) {
						t.Errorf("the panel page's row %d carries %s, and there is no "+
							"#link-tabs on this page for htmx to place the response "+
							"into:\n  %s", i, hx, anchor)
					}
				}
				continue
			}

			// `&amp;` because html/template escapes an interpolated `&` in an
			// attribute, which is what correct HTML looks like; the browser and
			// htmx both read the parsed value back as `&`.
			want := `/links/` + linkID + `?tab=qr&amp;code=`
			if !strings.Contains(anchor, `href="`+want) {
				t.Errorf("the link page's row %d still sends the reader off the page. "+
					"Selecting a code is a swap of this page's own tab strip since "+
					"M50.8's third reopening, and the href is what a script-blocked "+
					"reader follows to the same place (D178):\n  %s", i, anchor)
				continue
			}
			if !strings.Contains(anchor, `hx-get="`+want) {
				t.Errorf("the link page's row %d fetches a different URL from the one "+
					"its href names, so a reader running the script and one who is "+
					"not would be shown different codes:\n  %s", i, anchor)
			}
			for _, hx := range swap {
				if !strings.Contains(anchor, hx) {
					t.Errorf("the link page's row %d is missing %s. Three of the "+
						"strip's four attributes is a link that navigates, which is "+
						"the load this reopening exists to remove:\n  %s", i, hx, anchor)
				}
			}
		}

		// And nothing on the link page's tab points at the panel route any
		// more, which is the half of F244(b) that closes: the surface still
		// answers to a bookmark, and the list stops sending anybody there.
		if page == "link_detail" {
			if body := renderQRPanel(t, page); strings.Contains(body, `href="/links/`+linkID+`/qr?code=`) {
				t.Error("the link page's QR tab still links to the panel route, which " +
					"is the page with no link heading row that F244(b) reported")
			}
		}
	}
}

// TestARowSelectsOnItsWholeArea is F224(f)'s first half, owner-reported:
// *"Clicking in any unoccupied space on the QR list items should select the
// code, not just the name."*
//
// The row painted a selected background across its full width while only the
// name was clickable, so the affordance was wider than the target — which reads
// as a dead row rather than as a small one.
//
// **What a template test can assert, and what it cannot.** That the row is a
// containing block, that the selecting anchor declares an overlay spanning it,
// and that the acting controls sit above that overlay: three class strings, all
// readable from markup. What no Go test can do is lay the page out and click,
// which is why D24's precedent — *"a top-layer element ignores its ancestor's
// containing block, so positioning is verified in a browser rather than
// asserted from markup"* — applies here too, and why the click itself is a case
// in tools/agent-browser/specs/qr-codes-list.spec.mjs.
func TestARowSelectsOnItsWholeArea(t *testing.T) {
	for _, page := range qrPanelPages {
		for i, row := range qrRows(t, renderQRPanel(t, page)) {
			open := row[:strings.Index(row, ">")]
			if !strings.Contains(open, "relative") {
				t.Errorf("%s row %d is not a containing block (%q), so the overlay "+
					"below spans the panel rather than the row", page, i, open)
			}
			// The selecting element: the anchor to this row's own panel URL,
			// stretched over the row by a pseudo-element rather than by nesting
			// the row's text inside a block-level link.
			at := strings.Index(row, `<a href="/links/`)
			if at < 0 {
				t.Fatalf("%s row %d carries no link to its own panel", page, i)
			}
			anchor := row[at : at+strings.Index(row[at:], ">")]
			for _, want := range []string{"after:absolute", "after:inset-0", "after:content-['']"} {
				if !strings.Contains(anchor, want) {
					t.Errorf("%s row %d: the selecting anchor is missing %s, so the "+
						"click target is the name and the affordance is the row "+
						"(F224f):\n  %s", page, i, want, anchor)
				}
			}
		}
	}
}

// TestTheActionClusterIsSeparatedAndExcluded is F224(f)'s second half:
// *"There should be some blank space around the remove and download buttons to
// prevent accidentally switching when a different action was intended."*
//
// Two claims in one element. The cluster is **excluded** from the selecting
// overlay — positioned, and later in tree order, which is what keeps its own
// clicks — and it is **separated** from the area that selects, by a margin
// rather than by the row's gap: the gap applies between every pair of children
// and the separation is owed at one seam.
//
// The rendered gap is the browser's to measure, and the spec does.
func TestTheActionClusterIsSeparatedAndExcluded(t *testing.T) {
	for _, page := range qrPanelPages {
		for i, row := range qrRows(t, renderQRPanel(t, page)) {
			at := strings.Index(row, `<div class="relative z-10`)
			if at < 0 {
				t.Errorf("%s row %d draws no action cluster above the selecting "+
					"overlay; without `relative` and a stacking level its controls "+
					"are under the overlay and unclickable:\n  %s", page, i, row)
				continue
			}
			cluster := row[at : at+strings.Index(row[at:], ">")]
			if !strings.Contains(cluster, "ml-4") {
				t.Errorf("%s row %d: the action cluster declares no separating "+
					"margin (%q). A destructive control 8px from a download button "+
					"on a 22px row is the misclick F224f reports", page, i, cluster)
			}
			// And the destructive one is separated again from the control
			// beside it, which is the half of the report about Remove itself.
			if !strings.Contains(row, `name="remove" value="1"`) {
				continue
			}
			rm := row[strings.Index(row, `name="remove" value="1"`):]
			rm = rm[:strings.Index(rm, ">")]
			if !strings.Contains(rm, "ml-1") {
				t.Errorf("%s row %d: Remove carries no margin of its own (%q), so it "+
					"sits at the cluster's own gap from a download control", page, i, rm)
			}
		}
	}
}

// TestRemoveIsAMinusThatSaysWhatItRemoves is F224(e), owner-set: *"The remove
// button should be replaced with a button using a '-' icon."*
//
// **The accessible name is the assertion that matters.** The word went, so the
// only thing left telling a screen reader — or anybody hovering — what this
// button does is the name, and a `−` with no name is a button whose meaning is
// its shape. It names the code as well as the action, because there are up to
// twenty of these on one page and *Remove* twenty times is a list nobody can
// navigate.
func TestRemoveIsAMinusThatSaysWhatItRemoves(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)
		if strings.Contains(body, ">Remove</button>") {
			t.Errorf("%s still draws a worded Remove button; the owner asked for a "+
				"'-' icon (F224e)", page)
		}
		// The minus glyph, one per removable row, carrying a name that says
		// which code it takes away. The fixture's second row is the named one.
		for _, want := range []string{
			`aria-label="Remove Autumn poster"`,
			`<title>Remove Autumn poster</title>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the remove control is missing %s. `icon_minus` carries "+
					"its name twice — on the svg and in a <title> — and neither is "+
					"optional when the button has no text at all", page, want)
			}
		}
		// The glyph itself, so a caller that stopped using the shared define
		// and drew its own path fails here rather than drifting quietly.
		if !strings.Contains(body, `<path d="M5 12h14"/>`) {
			t.Errorf("%s does not render icon_minus's glyph; the row draws some "+
				"other minus, which is the icon vocabulary growing a second copy",
				page)
		}
	}
}

// TestOneDownloadControlPerCode is F224(d) and F224(i) together, and they are
// one claim: *"The download buttons in the QR list 'PNG' and 'SVG' should be
// replaced with a single button using the download icon that provides a
// dropdown to choose, allowing more file types in the future without adding
// additional buttons"* — and the pair below the preview, which hit the same two
// URLs as the row's, is gone.
//
// **Counted, because the number is the claim.** A two-code panel used to draw
// six download controls for two pictures: PNG and SVG on each row, and the
// worded pair under the preview. It draws two now — one per code — and both
// formats are reachable from each.
func TestOneDownloadControlPerCode(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)

		if n := strings.Count(body, `popovertarget="qr-download-`); n != 2 {
			t.Errorf("%s draws %d download controls over a two-code list, want one "+
				"per code (F224d)", page, n)
		}
		if strings.Contains(body, "Download the PNG") || strings.Contains(body, "Download the SVG") {
			t.Errorf("%s still draws the worded download pair below the preview. It "+
				"hit the same two URLs as the row's controls, and a per-code action "+
				"belongs on the per-code row (F224i)", page)
		}
		// Both formats, from inside the menu, for each code — the point of the
		// menu being that a third costs an entry rather than a third button.
		for _, slug := range []string{"d3f4u1t0", "k7m2qh4b"} {
			menu := elementsAfter(t, body, `<div id="qr-download-`+slug+`"`)
			for _, want := range []string{"image.png", "image.svg", ">PNG</a>", ">SVG</a>"} {
				if !strings.Contains(menu, want) {
					t.Errorf("%s: the menu for %s does not reach %s", page, slug, want)
				}
			}
			if !strings.Contains(menu, `popover="auto"`) {
				t.Errorf("%s: the menu for %s is not an auto popover, so it closes on "+
					"neither Escape nor a click outside (D24)", page, slug)
			}
		}
		// And the invoker is a button rather than a submit, which is what keeps
		// a menu from posting the form it sits beside.
		if !strings.Contains(body, `<button type="button" popovertarget="qr-download-`) {
			t.Errorf("%s: the download invoker is not type=\"button\"", page)
		}
	}
}

// elementsAfter returns the markup from a marker to the next closing </div>,
// which is the whole of a download menu: it holds two anchors and no nested
// block, so the first close is its own.
func elementsAfter(t *testing.T, body, marker string) string {
	t.Helper()
	at := strings.Index(body, marker)
	if at < 0 {
		t.Fatalf("no %s in the rendered panel", marker)
	}
	end := strings.Index(body[at:], "</div>")
	if end < 0 {
		t.Fatalf("the element at %s is not closed", marker)
	}
	return body[at : at+end]
}

// TestTheMenusAnchoringShipsInTheStylesheet is the half of the download menu
// that lives in CSS, and it is asserted here for the reason
// TestTheBrowseControlAcknowledgesTheClick asserts `.file-pick`'s rules:
// `style-src 'self'` refuses an injected stylesheet, so a rule that did not
// reach app.css is a control with no positioning at all and nothing in the
// markup would say so.
//
// **Two rules and they fail differently, which is why both are named.**
// `.qr-menu-anchor` is the name every invoker declares; without it no menu
// anchors to anything and they all open centred. The `anchor-scope` utility is
// what bounds that name to one row; without it every menu in a twenty-row list
// resolves to the **last** invoker and opens on the wrong row — a failure that
// looks like a layout bug and is a missing class.
//
// And the scope is the one of the two Tailwind has to *generate*, which is what
// makes it worth a test rather than a reading: it is an arbitrary utility, and
// arbitrary utilities exist only if Tailwind scanned the literal string (F216
// is that scanner's known gap). A rename in the template that missed the
// stylesheet would ship a list of menus that all open on the last row.
func TestTheMenusAnchoringShipsInTheStylesheet(t *testing.T) {
	css := builtStylesheet(t)
	for _, want := range []string{
		".qr-menu-anchor{anchor-name:--linkctrl-qr-menu}",
		"anchor-scope:--linkctrl-qr-menu",
		"position-anchor:--linkctrl-qr-menu",
		"top:anchor(bottom)",
		"right:anchor(right)",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the built stylesheet carries no %s. Without it the row menus "+
				"either anchor to nothing or all anchor to the last row's button, "+
				"and no template scan can see either", want)
		}
	}
	// The scope reaches the row, which is the half the template owns.
	for _, page := range qrPanelPages {
		if !strings.Contains(renderQRPanel(t, page), "[anchor-scope:--linkctrl-qr-menu]") {
			t.Errorf("%s: the codes list's rows do not scope the anchor name, so the "+
				"rule above reaches every menu and bounds none of them", page)
		}
	}
}

// TestTheDefaultIsAnIconOnEveryRow is D188's second answer, owner-set over both
// shapes that were offered: *"an icon button on every row, it becomes filled in
// when the row is the default and empty when it isn't. It should update all the
// icons when any of the icons is changed."*
//
// **What that buys is a list where the default is readable without reading**,
// and it is why the answer beat both options: a control only on the rows that
// are *not* the default puts nothing on the one that is, so the state was
// legible only from a sentence in the meta line.
//
// Four assertions, and the counts are what make them about the set rather than
// about one row. Exactly one filled icon, `len(codes)-1` empty ones, the filled
// one on the row that holds the flag, and a name on each that says which code it
// is about — because an exclusive set of icons is a radio group, and a radio
// group whose members are all called the same thing is one control repeated.
//
// The fill *moving* when one is clicked is the browser's to show, and the spec
// drives it. Nothing here is swapped and no script is added: the control posts,
// the handler redirects, and the list re-renders whole.
func TestTheDefaultIsAnIconOnEveryRow(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)
		rows := qrRows(t, body)

		var filled, empty int
		for i, row := range rows {
			on := strings.Contains(row, qrDefaultFilled)
			if !strings.Contains(row, qrDefaultRing) {
				t.Errorf("%s row %d draws no default icon at all. It renders on every "+
					"row including the default's own, where it is inert (D188)", page, i)
				continue
			}
			if on {
				filled++
				if !strings.Contains(row, `aria-pressed="true"`) {
					t.Errorf("%s row %d is filled and does not expose a pressed state. "+
						"The set is a radio group and the fill is a picture; the state "+
						"has to be readable without seeing it", page, i)
				}
				if !strings.Contains(row, "disabled") {
					t.Errorf("%s row %d is the default and its icon is not inert. The "+
						"flag is already its own, so a submit there is a second way to "+
						"write a value that is already written (D188)", page, i)
				}
			} else {
				empty++
				if !strings.Contains(row, `aria-pressed="false"`) {
					t.Errorf("%s row %d is not the default and does not say so", page, i)
				}
			}
		}
		if filled != 1 {
			t.Errorf("%s draws %d filled default icons over %d codes, want exactly 1. "+
				"A link always has a default (D183), so one filled icon is not a "+
				"convention — it is the data", page, filled, len(rows))
		}
		if want := len(rows) - 1; empty != want {
			t.Errorf("%s draws %d empty default icons, want %d", page, empty, want)
		}
		// The fixture's first row holds the flag, and it is the one that must
		// be filled: a list that filled the right *number* on the wrong row
		// would pass every count above.
		if !strings.Contains(rows[0], qrDefaultFilled) {
			t.Errorf("%s fills a default icon on a row that does not hold the flag", page)
		}
		// And the two names, which are the owner's own words (M50.8, F238i)
		// and no longer name the code.
		//
		// **They are on the button now, not on the glyph**, which is what the
		// tooltip pattern requires and is asserted here rather than left to
		// TestTheQRTooltipsAreThisPagesOwn: a name that stayed on a `h-3 w-3`
		// <svg> would also be a native tooltip drawn beside the page's own,
		// saying the same words twice (D192).
		for _, want := range []string{
			`aria-label="Default QR Code"`,
			`aria-label="Make Default QR Code"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: no default control carries %s (F238i, owner-set over "+
					"\"%%s is the default code\")", page, want)
			}
		}
		// The strings they replace, gone from the whole page. A test for the new
		// wording alone passes on a page carrying both.
		for _, gone := range []string{
			"is the default code", "Make Autumn poster the default code",
		} {
			if strings.Contains(body, gone) {
				t.Errorf("%s still carries %q. The owner replaced the sentence form "+
					"with two fixed names, knowing the code's own name goes with it — "+
					"the row's link carries that name immediately before the control",
					page, gone)
			}
		}
	}
}

// TestTheDefaultIconRendersForAReaderWhoCannotChangeIt is D190, owner-answered
// after the first attempt of this milestone shipped the set inside
// `{{if $.Identity.Can "links.update"}}`.
//
// **The bullet says *every row*, and the first attempt read it as every row of
// the rows an editor sees.** That is where the **Make default** button it
// replaces belonged and it is wrong here: what D188 bought is that which code
// is the default becomes readable without reading, so a `links.read` viewer
// with no icon on any row was returned to the sentence in the meta line — the
// thing the icon exists to replace. Nothing above this catches it, because the
// fixture's owner holds every permission.
//
// **The same counts, and no form.** A reader's row is the shape the default's
// own row already has for an editor: an inert button, `aria-pressed` carrying
// the state, an accessible name saying which code. The empty one says the code
// is *not* the default rather than offering to make it so, because for this
// identity there is nothing to offer.
func TestTheDefaultIconRendersForAReaderWhoCannotChangeIt(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanelAsReader(t, page)
		rows := qrRows(t, body)

		var filled, empty int
		for i, row := range rows {
			if !strings.Contains(row, qrDefaultRing) {
				t.Errorf("%s row %d draws no default icon for a reader who holds "+
					"only links.read. The indicator is what says which code an "+
					"untagged scan resolves through, and it is legibility rather "+
					"than a control (D190)", page, i)
				continue
			}
			// No form, and nothing that could post one: this identity may not
			// write the flag, so a control here would be an offer the server
			// refuses.
			for _, forbidden := range []string{"<form", `name="make_default"`, `type="submit"`} {
				if strings.Contains(row, forbidden) {
					t.Errorf("%s row %d carries %s for an identity without "+
						"links.update. The indicator renders as the static pair "+
						"with no form around it (D190)", page, i, forbidden)
				}
			}
			if strings.Contains(row, qrDefaultFilled) {
				filled++
				if !strings.Contains(row, `aria-pressed="true"`) {
					t.Errorf("%s row %d is filled and does not expose a pressed "+
						"state; the fill is a picture", page, i)
				}
			} else {
				empty++
				if !strings.Contains(row, `aria-pressed="false"`) {
					t.Errorf("%s row %d is not the default and does not say so", page, i)
				}
			}
			if !strings.Contains(row, "disabled") {
				t.Errorf("%s row %d draws an enabled control for somebody who cannot "+
					"use it", page, i)
			}
		}
		if filled != 1 {
			t.Errorf("%s draws %d filled default icons for a reader over %d codes, "+
				"want exactly 1 — the same set the editor sees, since a link always "+
				"has a default (D183)", page, filled, len(rows))
		}
		if want := len(rows) - 1; empty != want {
			t.Errorf("%s draws %d empty default icons for a reader, want %d", page, empty, want)
		}
		if !strings.Contains(rows[0], qrDefaultFilled) {
			t.Errorf("%s fills a reader's default icon on a row that does not hold "+
				"the flag", page)
		}
		// The names move with the editor's (M50.8, F238i) and the empty one
		// still names the state rather than an action nobody here can take.
		for _, want := range []string{
			`aria-label="Default QR Code"`,
			`aria-label="Not the Default QR Code"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: no default control carries %s", page, want)
			}
		}
		if strings.Contains(body, `aria-label="Make Default QR Code"`) {
			t.Errorf("%s offers a reader a name that promises a write they cannot "+
				"make", page)
		}
	}
}

// --- what saves, saves once (M50.7) ------------------------------------------

// TestNothingPostsRename is F224(g), owner-set: *"the 'Rename' button should be
// removed with the 'Save the style' button taking up all saving functions."*
//
// The form carried two submits for two halves of one row, so changing a colour
// and a name together took two presses — or one press and a silently dropped
// field. The server's branch stays and is commented as deliberately unreachable
// (D188); what must not survive is a dashboard path that posts it.
func TestNothingPostsRename(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)
		if strings.Contains(body, `name="rename"`) {
			t.Errorf("%s still posts `rename`. One control saves this form now, and "+
				"a second submit for the name is the two-press save the owner asked "+
				"to remove (F224g)", page)
		}
		if strings.Contains(body, ">Rename</button>") {
			t.Errorf("%s still draws a Rename button", page)
		}
		// The field it stood beside stays: what went is the second submit, not
		// the ability to name a code.
		if !strings.Contains(body, `id="qr_label_edit"`) {
			t.Errorf("%s no longer offers a name field. The button went; the field "+
				"is what Save now writes", page)
		}
	}
}

// TestTheSaveControlReadsSave is F224(h), owner-set from three offered — a save
// icon, `Save`, `Apply`.
//
// One label, and it is worth a test because nothing else reads it: the old one
// named a style while the control now writes the name too, so the word that
// described half of what it does had to go whatever was picked. Both directions,
// because a test for the new string alone passes on a page that grew a second
// button.
func TestTheSaveControlReadsSave(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)
		if !strings.Contains(body, ">Save</button>") {
			t.Errorf("%s has no \"Save\" button (F224h)", page)
		}
		if strings.Contains(body, "Save the style") {
			t.Errorf("%s still says \"Save the style\"; it saves the name as well "+
				"now, so the label named half of what it does", page)
		}
	}
}

// bareDisabled matches the HTML attribute and not Tailwind's `disabled:`
// variants, which sit in the same class list and would otherwise read as the
// attribute on every render.
var bareDisabled = regexp.MustCompile(`(^|\s)disabled(\s|$)`)

// TestRestoreDefaultsIsAlwaysDrawn is F224(j): the control rendered only
// `{{if .QRStored}}`, so a code nobody has styled showed no control and no
// reason — and *the thing you are looking for is absent because it would do
// nothing* is not deducible from an empty space.
//
// **Both states, and the stored-less one is asserted on both halves.** The
// earlier wording of this bullet asserted presence and left the promised
// explanation untested, which the plan review caught: a disabled control with
// no reason beside it is the same dead affordance in a lighter colour.
func TestRestoreDefaultsIsAlwaysDrawn(t *testing.T) {
	for _, page := range qrPanelPages {
		for _, stored := range []bool{true, false} {
			body := mainOf(t, renderPage(t, page,
				map[string]any{"Tab": "qr", "QRStored": stored}))

			at := strings.Index(body, `name="reset" value="1"`)
			if at < 0 {
				t.Errorf("%s (QRStored=%v) draws no Restore defaults control. It is "+
					"drawn in both states now (F224j)", page, stored)
				continue
			}
			button := body[at : at+strings.Index(body[at:], ">")]
			// The bare attribute, not the `disabled:` variants the class list
			// carries for the look of it — a substring test reads those as the
			// attribute and reports every state as disabled.
			if got := bareDisabled.MatchString(button); got != !stored {
				t.Errorf("%s (QRStored=%v): the control's disabled attribute is %v, "+
					"want %v — it is enabled exactly when there is a stored style to "+
					"clear:\n  %s", page, stored, got, !stored, button)
			}
			reason := "This code has no stored style yet."
			if got := strings.Contains(body, reason); got != !stored {
				t.Errorf("%s (QRStored=%v): the reason %q is present=%v, want %v. A "+
					"disabled control with no explanation reads as a bug, which is "+
					"the whole of why it is drawn rather than hidden",
					page, stored, reason, got, !stored)
			}
		}
	}
}

// --- the tab says less (M50.7) -----------------------------------------------

// TestTheCodeCounterReplacesTheCapSentence is F224(a), owner-reported: *"'A link
// carries at most 20' — this text is useless where it is and should be removed
// and a 'N/20' counter added near the top of the tab."*
//
// Two directions, because either alone is half the report: the counter renders
// with both numbers near the top of the list, and neither sentence that carried
// the cap in prose survives — the one under the add form, and the one that
// replaced the add form at capacity.
//
// **It does not duplicate the tab strip's QR badge and both stand**, which the
// owner answered on 2026-08-13: they are different quantities — how many codes
// exist, and how much of the cap is spent — rendered from the one `len(QRCodes)`
// the badge reads. That is what `attachTabBadges`' governing comment asks for,
// and it is why this is not a second copy of a number.
func TestTheCodeCounterReplacesTheCapSentence(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)

		// Two codes and a cap of twenty, both from the fixture.
		if !strings.Contains(body, ">2/20</p>") {
			t.Errorf("%s draws no N/20 counter over its codes list. Both numbers, "+
				"because a bare count is what the tab badge already says (F224a)", page)
		}
		for _, gone := range []string{
			"A link carries at most",
			"carrying the most codes one link can have",
		} {
			if strings.Contains(body, gone) {
				t.Errorf("%s still says %q. The counter is where the cap is stated "+
					"now, and a sentence saying it again is the text the owner "+
					"called useless where it is", page, gone)
			}
		}
	}
}

// TestTheEncodedURLIsNotPrintedUnderThePicture is F224(c), owner-set: *"The link
// that the QR resolves to doesn't need to be displayed under the QR code."*
//
// The string is the link's own short URL with `?src=qr` on it — which the page
// states above the picture and the heading row states as the link itself, so it
// was the third rendering of one value on one page.
//
// Asserted against the fixture's own content value rather than a literal, so a
// fixture that changed what the code encodes cannot leave this passing against a
// string nothing renders.
func TestTheEncodedURLIsNotPrintedUnderThePicture(t *testing.T) {
	for _, page := range qrPanelPages {
		data, ok := pageData(t)[page].(map[string]any)
		if !ok {
			t.Fatalf("the %s fixture is not a map", page)
		}
		content, ok := data["QRContent"].(string)
		if !ok || content == "" {
			t.Fatalf("the %s fixture carries no QRContent", page)
		}
		if strings.Contains(renderQRPanel(t, page), content) {
			t.Errorf("%s still prints %q under the picture (F224c)", page, content)
		}
	}
}

// TestTheQRSettingsRenderInTheTabsFlow is the assertion panelSheet's comment
// promises, and it exists because M50.7 put a popover on this tab.
//
// M48's F212 amendment claims the QR settings live in the tab's flow rather than
// behind a popup. Until this milestone that claim was checked by "no popover
// inside <main>", which is a proxy that stopped fitting the moment a control on
// the tab used the platform's popover API for something that is not a panel. So
// the claim is checked directly instead: the only popovers here are the per-row
// download menus, and the settings themselves are ordinary markup.
// **Widened for the add prompt in M50.8's own diff** (F238h, D189). The prompt
// is a third popover on this surface and it is named here rather than
// discovered as a red assertion: extending an exception because a test went red
// is exactly the silent widening D189 was written to prevent, and the two look
// identical in the diff a month later. The settings themselves are still
// asserted to be in flow, which is the claim; what moved is the list of things
// that legitimately are not.
func TestTheQRSettingsRenderInTheTabsFlow(t *testing.T) {
	popovers := regexp.MustCompile(`<div id="([a-z0-9-]+)" popover="auto"`)
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)
		for _, m := range popovers.FindAllStringSubmatch(body, -1) {
			if !strings.HasPrefix(m[1], qrRowMenuID) && m[1] != qrAddID {
				t.Errorf("%s renders a popover %q that is neither a row's download menu "+
					"nor the add prompt. The QR settings live in the tab's flow since "+
					"the F212 reopening, and a popup here is that mechanism returning "+
					"under another name", page, m[1])
			}
		}
		// And the settings are on the page, outside anything that has to be
		// opened: the style form's fields, the size control and the list.
		for _, want := range []string{`id="qr_foreground"`, `id="qr_size"`, `id="qr_label_edit"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not render %s in flow", page, want)
			}
		}
	}
}

// TestTheDefaultCodeIsNamedByItsIconAndNotByProse is F244(d), owner-set:
// *"remove the 'the default - …' text and its description on the Default
// item"*.
//
// **This test used to assert the opposite**, and it was the reader's half of
// D183: the flag decides where a picture carrying no code of its own is
// counted, which is not deducible from the word *default*, so the row said so.
// The owner has now asked for both halves of that clause — the label and its
// explanation — off the row.
//
// So what it asserts is the removal, plus the thing that makes the removal
// survivable: **the default is still identifiable without reading**. M50.8 put
// a filled icon on every row and named it `Default QR Code`, which is what the
// sort order used to do and what this sentence used to do, and a milestone that
// removed the words *and* let the icon go would leave the default unmarked. The
// third assertion is therefore not padding — it is what keeps this one from
// being a licence to delete the whole affordance.
//
// The sentence's claim is not withdrawn, only moved off this surface:
// `api/openapi.yaml` and `docs/usage.md` both still state it, which is
// m50.8.md's own condition for the removal.
func TestTheDefaultCodeIsNamedByItsIconAndNotByProse(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)

		if strings.Contains(body, "a scan carrying no code of its own is counted against this one") {
			t.Errorf("%s still explains the default on the row. The owner asked for "+
				"the \"the default — …\" text and its description to go (F244d); what "+
				"the flag does is still in api/openapi.yaml and docs/usage.md", page)
		}
		if strings.Contains(body, "&middot; the default") {
			t.Errorf("%s still labels the row \"· the default\" in its meta line. Both "+
				"halves of that clause were removed, not only the explanation", page)
		}
		if strings.Contains(body, "the code every already-printed picture of this link resolves to") {
			t.Errorf("%s still describes the default as the code every printed picture "+
				"resolves to. That was true while the default was the row without a "+
				"slug; the flag moves now, and so does what it describes", page)
		}
		// And what is left saying which row it is. Without this the assertions
		// above pass on a list that marks its default nowhere at all.
		if !strings.Contains(body, "Default QR Code") {
			t.Errorf("%s names no default code at all. The prose came off the row on "+
				"the argument that the filled icon and its tooltip say which one it "+
				"is (M50.8, D192) — with those gone too, nothing does", page)
		}
	}
}

// oneCodeList renders either surface with a link that carries a single code,
// which is the state nearly every link on an instance is in and therefore the
// one most readers meet first.
func oneCodeList(t *testing.T, page string) string {
	t.Helper()
	return mainOf(t, renderPage(t, page, map[string]any{
		"Tab": "qr",
		"QRCodes": []map[string]any{{
			"Slug": "d3f4u1t0", "Label": "", "Name": "The original code",
			"Size": 740, "Default": true, "Selected": true,
			"Select":      qrSelectFor(page, "d3f4u1t0"),
			"Download":    "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr/codes/d3f4u1t0/image.svg",
			"DownloadPNG": "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr/codes/d3f4u1t0/image.png",
		}},
	}))
}

// TestTheLastCodesRemoveButtonIsDisabledRatherThanAbsent is F238(g), owner-set:
// *"leave the remove button on the row but have it grayed out if it is the last
// item. A hover tooltip on the disabled button can say 'Every link must have at
// least 1 QR code.'"*
//
// **This replaces TestALinkWithOneCodeSaysWhyItCannotBeRemoved and reverses
// it.** That test asserted the control was *absent* and that a sentence under
// the list carried the reason, which is what `link_qr.html` argued in writing:
// *"Absent rather than disabled: the sentence below the list is where the reason
// goes, because a disabled control with no explanation reads as a bug."* The
// objection in that sentence is to a disabled control **with no explanation**,
// and the owner's tooltip is precisely the explanation it says is missing — so
// the reversal is narrow and the old reasoning survives it. The sentence it
// pointed at is deleted in the same milestone, which is why both halves are
// asserted here: a page that grew the button and kept the paragraph would be
// saying it twice.
//
// The sentence is the owner's wording, character for character, and is asserted
// as such rather than paraphrased.
func TestTheLastCodesRemoveButtonIsDisabledRatherThanAbsent(t *testing.T) {
	const reason = "Every link must have at least 1 QR code."

	for _, page := range qrPanelPages {
		// One code: the button is drawn, and it refuses.
		body := oneCodeList(t, page)
		at := strings.Index(body, `name="remove" value="1"`)
		if at < 0 {
			t.Errorf("%s draws no remove control on a link's only code. It is drawn on "+
				"every row now and disabled on this one (F238g); absent is the shape "+
				"the owner asked to replace", page)
			continue
		}
		button := body[at : at+strings.Index(body[at:], ">")]
		if !bareDisabled.MatchString(button) {
			t.Errorf("%s: the only code's remove control is not disabled:\n  %s. A "+
				"control whose only outcome is a refusal has to say so before it is "+
				"pressed", page, button)
		}
		if !strings.Contains(body, reason) {
			t.Errorf("%s disables the only code's remove control and gives no reason. "+
				"A disabled control with no explanation reads as a bug, which is what "+
				"the comment this bullet reverses was defending against", page)
		}
		// The reason is the tooltip's, tied to the button, and not a paragraph
		// under the list: that paragraph is what this milestone deleted.
		if strings.Contains(body, "A link always has a QR code, so this one cannot be removed") {
			t.Errorf("%s still carries the sentence under the list. The reason moved "+
				"onto the control; carrying both is saying it twice", page)
		}
		if !strings.Contains(button, `aria-describedby="qr-tip-remove-`) {
			t.Errorf("%s: the disabled remove control is not tied to its reason by "+
				"aria-describedby, so the explanation is visible and unannounced:\n  %s",
				page, button)
		}

		// Two codes: both are drawn and neither refuses, so the disabled state
		// is a property of being last rather than of being drawn.
		two := renderQRPanel(t, page)
		if n := strings.Count(two, `name="remove" value="1"`); n != 2 {
			t.Errorf("%s draws %d remove controls over a two-code list, want 2", page, n)
		}
		if strings.Contains(two, reason) {
			t.Errorf("%s tells a two-code link that a link must have at least one "+
				"code. The reason belongs to the refusal, and there is none here", page)
		}
		for _, row := range qrRows(t, two) {
			rm := strings.Index(row, `name="remove" value="1"`)
			if rm < 0 {
				continue
			}
			if b := row[rm : rm+strings.Index(row[rm:], ">")]; bareDisabled.MatchString(b) {
				t.Errorf("%s disables a remove control on a link with two codes:\n  %s. "+
					"Any code may be removed while another is left (D183)", page, b)
			}
		}
	}
}

// --- the QR tab's third report (M50.8) ---------------------------------------

// qrTip matches one of this page's own tooltips: the span a `.qr-tip-host`
// wraps around its control, addressed by the two attributes that make it one
// rather than by its class alone.
var qrTip = regexp.MustCompile(`<span id="([a-z0-9-]+)" role="tooltip" class="qr-tip">([^<]*)</span>`)

// TestTheQRTooltipsAreThisPagesOwn is D192, owner-answered on the plan review's
// finding that the cheap fix could not be checked and did not reach everybody.
//
// **Why the browser's tooltip could not be moved onto the button and left at
// that.** A native tooltip is drawn by the operating system and has no DOM
// presence, so no assertion anywhere — here or in a browser — can watch it
// appear; and a `disabled` button is unfocusable, so a keyboard reader never
// triggers one at all, on the two controls whose entire purpose is to explain a
// refusal. The owner took the build over the attribute.
//
// Four claims, and each is a way the pattern could ship broken:
//
//   - every tooltip is tied to a control by `aria-describedby`, so what a
//     sighted reader hovers and what a screen reader announces are one string;
//   - every `aria-describedby` on this tab resolves to a tooltip that is
//     actually rendered, because a description pointing at nothing is silence;
//   - a host wrapping a **disabled** control is focusable itself, which is the
//     whole of what the native tooltip could not do;
//   - the glyph inside a tooltip host is decorative, because a `<title>` there
//     is a second tooltip over the same control saying the same words.
func TestTheQRTooltipsAreThisPagesOwn(t *testing.T) {
	hosts := regexp.MustCompile(`(?s)<span class="qr-tip-host[^"]*"([^>]*)>(.*?)</span>\s*</span>`)
	described := regexp.MustCompile(`aria-describedby="([a-z0-9-]+)"`)

	for _, page := range qrPanelPages {
		for _, body := range []string{renderQRPanel(t, page), oneCodeList(t, page)} {
			tips := map[string]string{}
			for _, m := range qrTip.FindAllStringSubmatch(body, -1) {
				tips[m[1]] = m[2]
			}
			if len(tips) == 0 {
				t.Errorf("%s renders no tooltip of its own. D192 is a styled element "+
					"this page owns, not the browser's <title>", page)
				continue
			}
			// Every description resolves. A dangling one is a control that says
			// nothing to a screen reader and everything to a hovering pointer.
			for _, m := range described.FindAllStringSubmatch(body, -1) {
				if _, ok := tips[m[1]]; !ok {
					t.Errorf("%s: aria-describedby=%q names no tooltip that renders",
						page, m[1])
				}
			}
			found := hosts.FindAllStringSubmatch(body, -1)
			if len(found) == 0 {
				t.Errorf("%s renders a tooltip with no .qr-tip-host around it, so "+
					"nothing hovers or focuses it into view", page)
			}
			for _, m := range found {
				open, inner := m[1], m[2]
				if !strings.Contains(inner, "aria-describedby=") {
					t.Errorf("%s: a tooltip host wraps a control that is not tied to "+
						"its tooltip:\n  %s", page, strings.TrimSpace(inner))
				}
				if !strings.Contains(inner, "aria-label=") {
					t.Errorf("%s: a tooltip host wraps a control with no accessible "+
						"name of its own. The glyph inside is decorative, so the "+
						"button is the only thing left to carry one:\n  %s",
						page, strings.TrimSpace(inner))
				}
				// The glyph is decorative. `<title>` inside it would be a native
				// tooltip beside this page's, on the same control.
				if strings.Contains(inner, "<title>") || strings.Contains(inner, "role=\"img\"") {
					t.Errorf("%s: the glyph inside a tooltip host is not decorative, so "+
						"the operating system draws a second tooltip over the same "+
						"button:\n  %s", page, strings.TrimSpace(inner))
				}
				// A disabled control cannot take focus, so the host has to.
				if bareDisabled.MatchString(inner) && !strings.Contains(open, `tabindex="0"`) {
					t.Errorf("%s: a tooltip host wrapping a disabled control is not "+
						"focusable, so a keyboard reader never reaches the tooltip "+
						"explaining the refusal — which is the one thing the native "+
						"tooltip could not do either (D192):\n  %s<%s>",
						page, open, strings.TrimSpace(inner))
				}
			}
		}
	}
}

// TestTheTooltipShipsInTheStylesheet is the half of the pattern that lives in
// CSS, asserted here for the reason TestTheBrowseControlAcknowledgesTheClick
// asserts `.file-pick`'s rules: `style-src 'self'` refuses an injected
// stylesheet, so a rule that never reached app.css is a tooltip that is
// permanently invisible and nothing in the markup would say so.
//
// Both triggers are named because they fail differently. Without `:hover` the
// tooltip never appears for a pointer, which is what the owner asked for.
// Without `:focus-within` it never appears for a keyboard, which is the reason
// this pattern exists at all rather than the native one.
func TestTheTooltipShipsInTheStylesheet(t *testing.T) {
	css := builtStylesheet(t)
	for _, want := range []string{
		".qr-tip-host:hover>.qr-tip",
		".qr-tip-host:focus-within>.qr-tip",
	} {
		if !strings.Contains(strings.ReplaceAll(css, " ", ""), want) {
			t.Errorf("the built stylesheet has no rule for %s. A tooltip with no "+
				"trigger is markup nobody can see, and no template scan catches it", want)
		}
	}
	// Hidden by default, or it is a label. Both properties, because opacity
	// alone leaves an invisible box over the control beside it.
	flat := strings.ReplaceAll(css, " ", "")
	for _, want := range []string{"visibility:hidden", "pointer-events:none"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the tooltip's resting state is missing %s", want)
		}
	}
}

// TestAddingACodeIsAPromptRatherThanAFormInThePage is F238(h), owner-set: *"Add
// a button with a '+' icon near the code count that provides a prompt with a
// text field and add button when pressed. This replaces the current 'Add
// another code' label/text field/Add code button."*
//
// **Three claims, and the second is what makes it a replacement rather than an
// addition.** One add control, invoking a popover — the mechanism this file
// already uses for the row menus and nav.html for the header's, so nothing new
// reaches `script-src 'self'`. The label and the submit it replaces are gone
// from the page. And the field inside it posts what `LinkQRStyle` actually
// reads — `label` under an `add` submit — because a prompt whose field the
// handler ignores is a prompt that swallows what somebody typed.
func TestAddingACodeIsAPromptRatherThanAFormInThePage(t *testing.T) {
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)

		if n := strings.Count(body, `popovertarget="qr-add"`); n != 1 {
			t.Errorf("%s draws %d add controls, want exactly 1 beside the counter "+
				"(F238h)", page, n)
		}
		if !strings.Contains(body, `<div id="qr-add" popover="auto"`) {
			t.Errorf("%s: the add control opens no popover. The prompt is the "+
				"mechanism the row menus already use, so Escape and a click outside "+
				"close it and no script is added (D24)", page)
		}
		// The `+` glyph itself, so a caller that drew its own path fails here
		// rather than the icon vocabulary quietly growing a second copy.
		if !strings.Contains(body, `<path d="M5 12h14M12 5v14"/>`) {
			t.Errorf("%s does not render icon_plus's glyph", page)
		}
		// What it replaced.
		for _, gone := range []string{"Add another code", "What a second code buys"} {
			if strings.Contains(body, gone) {
				t.Errorf("%s still carries %q. The prompt replaces the label, the "+
					"field and the submit that stood under the list, and the sentence "+
					"beside them went with them (F238e, F238h)", page, gone)
			}
		}
		// And the prompt posts what the handler reads.
		menu := body[strings.Index(body, `<div id="qr-add" popover="auto"`):]
		menu = menu[:strings.Index(menu, "</form>")]
		for _, want := range []string{
			`action="/links/`, `name="next"`, `name="label"`, `name="add" value="1"`,
		} {
			if !strings.Contains(menu, want) {
				t.Errorf("%s: the add prompt is missing %s, so pressing it posts a body "+
					"LinkQRStyle does not branch on:\n%s", page, want, menu)
			}
		}
	}
}

// TestTheAddControlIsDisabledAtCapacityRatherThanAbsent is the other half of
// D192, and it exists so that one list does not carry two conventions.
//
// The add form used to vanish at 20/20 with the counter left to explain it,
// which is the same absent-versus-disabled question F238(g) answers the other
// way one control along. The owner settled it: grayed out with the reason.
func TestTheAddControlIsDisabledAtCapacityRatherThanAbsent(t *testing.T) {
	data, ok := pageData(t)["link_qr"].(map[string]any)
	if !ok {
		t.Fatal("the link_qr fixture is not a map")
	}
	max, ok := data["QRMaxCodes"].(int)
	if !ok || max < 2 {
		t.Fatalf("the fixture carries no usable QRMaxCodes (%v)", data["QRMaxCodes"])
	}
	// Twenty rows, built per surface: a row's Select is the surface's since
	// M50.8's third reopening, so one slice shared between the two would carry
	// the panel route onto the link page and render markup no handler builds.
	full := func(page string) []map[string]any {
		rows := make([]map[string]any, 0, max)
		for i := range max {
			slug := "code" + strconv.Itoa(i)
			rows = append(rows, map[string]any{
				"Slug": slug, "Label": slug, "Name": slug, "Size": 740,
				"Default": i == 0, "Selected": i == 0,
				"Select":      qrSelectFor(page, slug),
				"Download":    "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr/codes/" + slug + "/image.svg",
				"DownloadPNG": "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr/codes/" + slug + "/image.png",
			})
		}
		return rows
	}

	for _, page := range qrPanelPages {
		body := mainOf(t, renderPage(t, page,
			map[string]any{"Tab": "qr", "QRCodes": full(page)}))

		if !strings.Contains(body, ">"+strconv.Itoa(max)+"/"+strconv.Itoa(max)+"</p>") {
			t.Errorf("%s does not read %d/%d at capacity, so the fixture is not "+
				"exercising the state this test is about", page, max, max)
		}
		at := strings.Index(body, `aria-label="Add a QR code"`)
		if at < 0 {
			t.Errorf("%s draws no add control at capacity. It grays out rather than "+
				"disappearing (D192), so that this list answers absent-versus-disabled "+
				"the same way for both of its controls", page)
			continue
		}
		start := strings.LastIndex(body[:at], "<button")
		button := body[start : at+strings.Index(body[at:], ">")]
		if !bareDisabled.MatchString(button) {
			t.Errorf("%s: the add control is enabled at capacity:\n  %s", page, button)
		}
		if !strings.Contains(body, "A link carries at most "+strconv.Itoa(max)+" QR codes.") {
			t.Errorf("%s grays out the add control at capacity and gives no reason", page)
		}
		if strings.Contains(body, `popovertarget="qr-add"`) {
			t.Errorf("%s still renders a prompt the disabled control cannot open. A "+
				"form nothing can reach is a form that will be wrong before anybody "+
				"notices", page)
		}
	}
}

// TestTheRowControlsAnswerThePointerOnTheSelectedRow is F238(j),
// owner-reported: *"The highlighting for the download and remove buttons is
// only visible when not on the selected row."*
//
// **This one is a defect rather than a preference** — the affordance is written
// and does not appear. The selected `<li>` paints `bg-sunken` and the controls
// inside it hovered to `hover:bg-sunken`, so on the row a reader is most likely
// to be pointing at, the two controls they use most resolved to the background
// they sit on.
//
// The row's own class is read out of the markup rather than typed here, so this
// cannot pass by both sides being changed to the same new token — which is the
// only way the defect comes back.
func TestTheRowControlsAnswerThePointerOnTheSelectedRow(t *testing.T) {
	selectedBG := regexp.MustCompile(`\bbg-([a-z-]+)\b`)
	hoverBG := regexp.MustCompile(`\bhover:bg-([a-z-]+)\b`)

	for _, page := range qrPanelPages {
		rows := qrRows(t, renderQRPanel(t, page))
		var painted string
		for _, row := range rows {
			open := row[:strings.Index(row, ">")]
			if m := selectedBG.FindStringSubmatch(open); m != nil {
				painted = m[1]
				break
			}
		}
		if painted == "" {
			t.Fatalf("%s paints no selected row, so there is nothing for a control's "+
				"hover to disappear into and this test is asserting nothing", page)
		}
		for i, row := range rows {
			// The buttons in the action cluster, and only those. The selecting
			// anchor's hover is a text colour, and the entries *inside* a
			// download menu are drawn on `bg-raised` in the top layer rather
			// than on the row — they are the one place `hover:bg-sunken` on this
			// tab is still correct, which is why the scan is by element rather
			// than over the row's whole markup.
			at := strings.Index(row, `<div class="relative z-10`)
			if at < 0 {
				continue
			}
			for _, tag := range qrButtonTags(row[at:]) {
				for _, m := range hoverBG.FindAllStringSubmatch(tag, -1) {
					if m[1] == painted {
						t.Errorf("%s row %d: a control hovers to bg-%s, which is what the "+
							"selected row is painted. On the one row a reader is most "+
							"likely to be on, the control gives no feedback at all "+
							"(F238j):\n  %s", page, i, painted, tag)
					}
				}
			}
		}
	}
}

// qrButtonTags returns the opening tag of every <button> in some markup.
func qrButtonTags(body string) []string {
	var out []string
	for at := strings.Index(body, "<button"); at >= 0; at = strings.Index(body, "<button") {
		end := strings.Index(body[at:], ">")
		if end < 0 {
			break
		}
		out = append(out, body[at:at+end+1])
		body = body[at+end+1:]
	}
	return out
}

// TestTheLogoControlsRenderInsideTheStyleSection is F238(f), owner-set:
// *"Upload a logo should be moved into the style section and the
// header/text/section it is currently in should be fully removed."*
//
// **Section, and not form, and the distinction is load-bearing** — which is why
// the third assertion is here rather than only the first two. A file needs
// `enctype="multipart/form-data"`, which the style form cannot carry without
// every one of its other buttons arriving in a body the handler does not read,
// and HTML forbids nesting one form inside another. So what moved is where the
// upload sits, and what must not have moved is which route it posts to or what
// it declares. The plan's first draft asserted the input was *inside the style
// form*, which the tree argues is impossible; the review caught it.
func TestTheLogoControlsRenderInsideTheStyleSection(t *testing.T) {
	for _, page := range qrPanelPages {
		for _, hasLogo := range []bool{true, false} {
			body := mainOf(t, renderPage(t, page,
				map[string]any{"Tab": "qr", "QRHasLogo": hasLogo}))

			if strings.Contains(body, ">Logo</h3>") {
				t.Errorf("%s (QRHasLogo=%v) still draws a Logo heading. The owner asked "+
					"for the header and its section to be fully removed", page, hasLogo)
			}
			// The section, and the input **inside** it. `elementAt` counts div
			// nesting to `#qr-style`'s own close, which is the whole assertion:
			// that div is the last container in <main>, so a scan from its
			// opening tag to the end of the string passes for an input placed
			// anywhere after it — including outside the section entirely, which
			// is exactly the placement the bullet forbids. Verified by moving
			// the upload one line past `</div>` and watching this go red.
			at := strings.Index(body, `<div id="qr-style"`)
			if at < 0 {
				t.Errorf("%s renders no style section to move the upload into", page)
				continue
			}
			section := elementAt(t, body, at)
			if !strings.Contains(section, `id="qr_logo"`) {
				t.Errorf("%s (QRHasLogo=%v): the file input does not render inside the "+
					"style section", page, hasLogo)
			}
			// Its own form, still, and still multipart. This is the claim the
			// bullet was corrected to make — and the one M50.8's second reopening
			// kept while moving the control itself into the style form's grid:
			// the file still travels in a body of its own, reached by `form=`.
			form := logoForm(t, body)
			for _, want := range []string{
				`enctype="multipart/form-data"`, `/qr/logo"`, `id="qr-logo-upload"`,
			} {
				if !strings.Contains(form, want) {
					t.Errorf("%s (QRHasLogo=%v): the upload lost %s in the move. It moved "+
						"section, not form — a file cannot travel in the style form's "+
						"body at all", page, hasLogo, want)
				}
			}
			if !strings.Contains(elementWithID(t, body, "qr_logo"), `hx-trigger="change"`) {
				t.Errorf("%s (QRHasLogo=%v): the trigger is not on the input. `change` "+
					"bubbles up the DOM, not to the form a control names, so from the "+
					"grid a trigger on the form never fires (F246b)", page, hasLogo)
			}
			// And the logo's other two controls still post where they did.
			label := "Upload a logo"
			if hasLogo {
				label = "Replace the logo"
				if !strings.Contains(body, `name="remove_logo" value="1"`) {
					t.Errorf("%s: the remove-logo control did not survive the move", page)
				}
			}
			if !strings.Contains(body, ">"+label+"<") {
				t.Errorf("%s (QRHasLogo=%v) does not label the upload %q", page, hasLogo, label)
			}
		}
	}
}

// TestTheLogoControlsRenderBetweenTheColoursAndTheSize is F246(b), owner-set:
// *"The logo upload thould be below the color pickers and above size."*
//
// It replaces TestTheLogoControlsRenderAboveTheStyleFormsSave, which was F244(g)
// — *"the logo picker should be above the save button"* — and it is strictly
// stronger: between the background colour and the size is above `Save`, since
// both of those are above it. What that test's comment asserted is what this
// milestone reversed, so it is written out rather than left standing beside its
// own contradiction: *"the upload cannot sit between the style form's fields
// and the style form's submit"* was drawn from the nesting ban, and the ban is
// on the `<form>` element and not on a control carrying `form="…"`.
//
// **Order in the rendered document, not layout.** A grid can be reordered by
// CSS, so a class would prove nothing about what a reader with the stylesheet
// in front of them sees; and the owner asked for a position in a sequence of
// controls. Both markers are anchored on ids the page has to have anyway.
//
// **The association is asserted with the position**, because the two failures
// look identical from here otherwise: a file input in the right place that has
// lost its `form=` renders exactly as this one does and posts to the style
// form's route, where the handler ignores it and answers with a page saying
// nothing happened.
func TestTheLogoControlsRenderBetweenTheColoursAndTheSize(t *testing.T) {
	for _, page := range qrPanelPages {
		for _, hasLogo := range []bool{true, false} {
			body := mainOf(t, renderPage(t, page,
				map[string]any{"Tab": "qr", "QRHasLogo": hasLogo}))

			background := strings.Index(body, `id="qr_background"`)
			size := strings.Index(body, `id="qr_size_slider"`)
			if background < 0 || size < 0 {
				t.Fatalf("%s (QRHasLogo=%v) renders no background colour input or no size "+
					"slider, so there is no interval to place the logo controls in",
					page, hasLogo)
			}
			if background > size {
				t.Fatalf("%s (QRHasLogo=%v): the size control renders before the colours, "+
					"so this test's own interval is inverted", page, hasLogo)
			}

			controls := map[string]string{
				"the file input":     `id="qr_logo"`,
				"the remove control": `name="remove_logo" value="1"`,
			}
			for what, marker := range controls {
				if !hasLogo && what == "the remove control" {
					continue
				}
				at := strings.Index(body, marker)
				if at < 0 {
					t.Errorf("%s (QRHasLogo=%v) renders %s nowhere at all", page, hasLogo, what)
					continue
				}
				if at < background || at > size {
					t.Errorf("%s (QRHasLogo=%v): %s renders outside the interval between "+
						"the background colour (%d) and the size slider (%d); it is at %d. "+
						"The owner asked for the logo upload below the colour pickers and "+
						"above size (F246b)", page, hasLogo, what, background, size, at)
				}
			}

			// And they still reach their own forms from there. The ids are the
			// association; a control naming a form the page does not render
			// submits to nothing.
			for control, form := range map[string]string{
				"qr_logo": "qr-logo-upload", "remove_logo": "qr-logo-remove",
			} {
				if control == "remove_logo" && !hasLogo {
					continue
				}
				if !strings.Contains(body, `form="`+form+`"`) {
					t.Errorf("%s (QRHasLogo=%v): nothing names the form %q, so %s posts to "+
						"the style form's route instead of the logo's", page, hasLogo, form, control)
				}
				if !strings.Contains(body, `id="`+form+`"`) {
					t.Errorf("%s (QRHasLogo=%v): the form %q named by %s does not render, so "+
						"the control belongs to no form at all", page, hasLogo, form, control)
				}
			}
		}
	}
}

// sizeMark cuts one drawn detent out of the size control.
//
// The position is captured with the value because the two failures are
// different: eight marks in the wrong places is arithmetic, and the right
// arithmetic over the wrong list is a control offering sizes the save refuses.
var sizeMark = regexp.MustCompile(`<line data-stop="(\d+)" x1="([0-9.]+)%" x2="([0-9.]+)%"`)

// restoringRule finds the held-paint selector in the built stylesheet, and the
// character class is the whole point of it: `.qr-restoring-off` contains
// `.qr-restoring`, so a substring search is green on a rule that reaches nothing
// the script ever sets.
var restoringRule = regexp.MustCompile(`\.qr-restoring[^-\w]`)

// TestTheSizeSliderDrawsItsStops is F246(c), owner-set: *"The Size slider
// should have visible detents at the stop points."*
//
// The stops were never missing from the page — the `<datalist>` has named them
// since D182 and Chromium draws ticks from one. `appearance: none` in input.css
// is what erases those along with the native track, and the track is where the
// theme lives, so the marks are drawn rather than the appearance given back.
//
// **What is asserted is the correspondence, not the count.** A strip of eight
// marks is easy to draw and would be wrong on the codes that matter: a dense
// code's floor drops the low stops, so a mark at 128 on an 89-module code is an
// offer its own save turns down. So the marks are compared against the list the
// view hands the template, and the second half of this test renders a raised
// floor to show the marks follow it.
//
// **The positions are checked too**, because a mark is a claim about where a
// value sits: `(stop - min) / (max - min)`, which is what rangePct computes and
// what a reader checks by dragging the thumb onto one.
func TestTheSizeSliderDrawsItsStops(t *testing.T) {
	for _, page := range qrPanelPages {
		data, ok := pageData(t)[page].(map[string]any)
		if !ok {
			t.Fatalf("the %s fixture is not a map", page)
		}
		stops, ok := data["QRSizeStops"].([]int)
		if !ok || len(stops) == 0 {
			t.Fatalf("the %s fixture carries no QRSizeStops, so there is nothing this "+
				"test could be comparing the marks against", page)
		}
		lo, okLo := data["QRMinSize"].(int)
		hi, okHi := data["QRMaxSize"].(int)
		if !okLo || !okHi || hi <= lo {
			t.Fatalf("the %s fixture carries no usable size range (%v..%v)", page, lo, hi)
		}

		assertMarks(t, page, renderQRPanel(t, page), stops, lo, hi)

		// A denser code: the floor rises past the first two stops, and the marks
		// have to lose them. This is the case a fixed strip of eight gets wrong,
		// and it is the reason the marks are rendered from the list rather than
		// from the stylesheet.
		raised := []int{300, 512, 600, 1024, 1200, 2048}
		body := mainOf(t, renderPage(t, page, map[string]any{
			"Tab": "qr", "QRMinSize": 280, "QRSizeStops": raised, "QRSize": 600,
		}))
		assertMarks(t, page+" (floor 280)", body, raised, 280, hi)
	}
}

// assertMarks compares the detents a rendering drew against the stops it was
// given, in order, with their positions.
func assertMarks(t *testing.T, page, body string, stops []int, lo, hi int) {
	t.Helper()
	got := sizeMark.FindAllStringSubmatch(body, -1)
	if len(got) != len(stops) {
		t.Errorf("%s draws %d detents for %d stops (%v). A mark the slider does not "+
			"stop at, or a stop it does not mark, is what F246(c) asked to be able "+
			"to see", page, len(got), len(stops), stops)
		return
	}
	for i, m := range got {
		value, err := strconv.Atoi(m[1])
		if err != nil || value != stops[i] {
			t.Errorf("%s draws a detent for %q where stop %d is %d", page, m[1], i, stops[i])
			continue
		}
		want := rangePct(value, lo, hi)
		if m[2] != want || m[3] != want {
			t.Errorf("%s draws the detent for %d at %s%%/%s%%, want %s%% — the position "+
				"of a mark is (stop-min)/(max-min) and nothing else, or the thumb does "+
				"not land on it", page, value, m[2], m[3], want)
		}
	}
}

// TestTheDrawnDetentsShipInTheStylesheet is the other half of F246(c): SVG
// carries the geometry, and the one thing it cannot carry is the inset.
//
// A range input's thumb travels between its own half-widths, so a strip of
// marks drawn edge to edge is right in the middle and wrong at both ends —
// which is the hardest kind of wrong to see. The measurement is in input.css
// beside the thumb it is derived from, and `style-src 'self'` is why it is
// there rather than on the element.
func TestTheDrawnDetentsShipInTheStylesheet(t *testing.T) {
	css := builtStylesheet(t)
	if !strings.Contains(css, ".qr-slider-marks") {
		t.Fatal("the built stylesheet has no rule for .qr-slider-marks, so the strip " +
			"is drawn edge to edge and every mark is off by half a thumb at the ends")
	}
	if !strings.Contains(css, "var(--t-line-strong)") {
		t.Error("the marks take no theme token, so they are a palette value in a " +
			"stylesheet whose own comment promises tokens")
	}
	for _, page := range qrPanelPages {
		body := renderQRPanel(t, page)
		if !strings.Contains(body, `class="qr-slider-marks`) {
			t.Errorf("%s draws the detents without the class the stylesheet rules hang "+
				"off, so the inset and the colour reach nothing", page)
		}
		// Drawn from `currentColor`, which is what the rule above sets. A mark
		// with its own colour would be black on both themes.
		if !strings.Contains(body, `stroke="currentColor"`) {
			t.Errorf("%s draws a detent that does not take its colour from the "+
				"stylesheet", page)
		}
	}
}

// TestThePaintIsHeldUntilTheSaveIsPutBack is F246(a)'s markup half.
//
// The claim itself — that a save never shows the reader a position they did not
// stand at — is a browser fact and is driven under emulated network conditions
// in tools/agent-browser/specs/qr-tab-controls.spec.mjs. What can be asserted
// from here is that the two halves of the mechanism ship: the rule that
// withholds the paint, and a stylesheet that is loaded before the script that
// depends on it.
//
// `visibility` is asserted by name. `display: none` would take the document's
// height with it, and a document with no height cannot be scrolled at all — the
// restore would silently clamp to the top, which is the failure this rule
// exists to prevent, wearing the same class name.
func TestThePaintIsHeldUntilTheSaveIsPutBack(t *testing.T) {
	css := builtStylesheet(t)
	// The class name, and not a name beginning with it. Caught by sabotage:
	// renaming the rule to `.qr-restoring-off` left this test green, which is a
	// substring search passing for a rule that reaches nothing.
	rule := restoringRule.FindStringIndex(css)
	if rule == nil {
		t.Fatal("the built stylesheet has no .qr-restoring rule. static/js/qr-size.js " +
			"puts that class on <html> before the first paint and takes it off once the " +
			"reader's position is back; with no rule behind it the class does nothing " +
			"and the page paints at the top first (F246a)")
	}
	end := strings.Index(css[rule[0]:], "}")
	if end < 0 {
		t.Fatal("the .qr-restoring rule is not a complete block")
	}
	block := css[rule[0] : rule[0]+end]
	if !strings.Contains(block, "visibility:hidden") && !strings.Contains(block, "visibility: hidden") {
		t.Errorf("the held paint is not `visibility: hidden` but %q. Anything that "+
			"takes the document out of layout takes its height with it, and scrolling "+
			"a document with no height clamps to the top", block)
	}
}

// TestTheSizeControlStillWorksWithTheScriptBlocked is M50.8's degradation
// bullet, and it is the one assertion that keeps the script from becoming a
// dependency rather than an improvement.
//
// static/js/qr-size.js copies whichever of the two inputs moved into the other.
// With it blocked — a reader with JavaScript off, a CSP a proxy tightened, a
// file that failed to load — the two inputs must still be two ordinary form
// fields carrying their own values, and `httpx.requestedQRSize` must still have
// its witness to arbitrate against. That is exactly what M49 shipped, so what is
// asserted is that nothing was taken away to make room for the script.
func TestTheSizeControlStillWorksWithTheScriptBlocked(t *testing.T) {
	for _, page := range qrPanelPages {
		data, ok := pageData(t)[page].(map[string]any)
		if !ok {
			t.Fatalf("the %s fixture is not a map", page)
		}
		size, ok := data["QRSize"].(int)
		if !ok || size == 0 {
			t.Fatalf("the %s fixture carries no QRSize", page)
		}
		body := renderQRPanel(t, page)
		value := `value="` + strconv.Itoa(size) + `"`

		for id, name := range map[string]string{
			"qr_size":        `name="size"`,
			"qr_size_slider": `name="size_slider"`,
		} {
			el := elementWithID(t, body, id)
			if !strings.Contains(el, name) {
				t.Errorf("%s: %s no longer posts %s, so with the script blocked the "+
					"form submits one input instead of two:\n  %s", page, id, name, el)
			}
			if !strings.Contains(el, value) {
				t.Errorf("%s: %s does not carry the rendered size %s. The script is what "+
					"copies one into the other while somebody is looking; the value "+
					"attribute is what the form posts when it never ran:\n  %s",
					page, id, value, el)
			}
		}
		if !strings.Contains(body, `name="size_shown" value="`+strconv.Itoa(size)+`"`) {
			t.Errorf("%s carries no size_shown witness at the rendered size. Without "+
				"it requestedQRSize cannot tell which of the two inputs moved, which "+
				"is the whole script-blocked path", page)
		}
		// Nothing on this tab is script-only: no control whose markup says it
		// needs one, and no inline handler for the CSP to refuse.
		if strings.Contains(body, "onchange=") || strings.Contains(body, "oninput=") {
			t.Errorf("%s carries an inline handler on the size control. `script-src "+
				"'self'` refuses it, so the control would be dead in a browser and "+
				"alive in this test", page)
		}
	}
}
