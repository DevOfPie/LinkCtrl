package ui

import (
	"regexp"
	"strings"
	"testing"
)

// M48's two claims about the panel, each asserted where a reader can see it
// fail.
//
// The milestone's stated risk is that the panel "is the most reusable decision
// in the phase and the one most likely to be got wrong once and then copied", so
// what these tests defend is not any surface but the fact that there is one
// mechanism.
//
// **Shipped M48 had two popup callers and required them** — "One mechanism,
// used by **at least two** surfaces" — and this file held a fatal on fewer.
// The F212 reopening (2026-08-11) retired the QR popup: the tabs un-buried its
// section, so its contents render in flow on the QR tab and the reviewer
// roster is the mechanism's one remaining caller. The claim is amended in
// m48.md where it stands; what survives here is "defined once", asserted over
// however many callers exist, plus the page-form half, which the QR contents
// still satisfy at their route.

// panelOpen matches a panel container: the element panel_open emits.
//
// Both attributes, in this order, because the class list is the second capture
// and comparing class lists across pages is how "one mechanism" is checked. A
// surface that wrote its own popover would either not match this at all or match
// it with a different class string, and both are failures.
var panelOpen = regexp.MustCompile(`(?s)<div id="([a-z0-9-]+)" popover="auto"\s+class="([^"]*)">`)

// anyPopover is the general form, and it is what the absence assertions actually
// need (F237).
//
// [panelOpen] matches exactly one spelling — a `<div>`, with `id` before
// `popover="auto"`, before `class` — which was exact while `panel_open` was the
// only thing emitting a popover inside <main>. M50.7 widened what those
// assertions *mean* (no panel, then no popover except the row menus), and a
// matcher tuned to one emitter cannot support a general claim: a popover on a
// `<button>` or a `<dialog>`, with the attributes in another order, with an
// uppercase letter in its id, or with a bare `popover` rather than `popover="auto"`
// was invisible to every one of them. Nothing in the tree escapes panelOpen today,
// so this closes a gap between the guard and the sentence beside it rather than a
// live hole.
//
// Two captures, because a caller needs the element's id to say which popover it
// found; the panel is still told apart by [panelSheet], read out of the element's
// class list wherever it sits in the tag.
//
// The lookahead is load-bearing: without it `\bpopover` matches the prefix of
// `popovertarget`, which is the *invoker* attribute and sits on a button that is
// not a popover at all. Caught by the existing assertions the moment this matcher
// was written, which is the same failure this row is about arriving from the
// other side — a pattern that matches more than it means.
var anyPopover = regexp.MustCompile(
	`(?is)<(?:div|button|dialog|span|section|ul|p)\s[^>]*\bpopover(?:="(?:auto|manual|hint)")?(?:\s|>)[^>]*>?`)

// popoverID pulls the id out of whatever anyPopover matched, or "" if it carries
// none — an unnamed popover is still a popover, and reporting it as `""` is more
// use than dropping it.
var popoverID = regexp.MustCompile(`(?i)\bid="([^"]*)"`)

// popoversIn is every popover in a rendered page, by id, in the general form.
func popoversIn(body string) []string {
	var out []string
	for _, tag := range anyPopover.FindAllString(body, -1) {
		id := ""
		if m := popoverID.FindStringSubmatch(tag); m != nil {
			id = m[1]
		}
		out = append(out, id)
	}
	return out
}

// popoverClasses is the class list of whatever anyPopover matched.
var popoverClasses = regexp.MustCompile(`(?i)\bclass="([^"]*)"`)

// panelSheet is what tells the panel mechanism from any other popover, and it
// exists because those stopped being the same set at M50.7.
//
// **The absence assertions below mean "no *panel* here", and until M50.7 they
// could say "no popover here" and mean it** — inside <main> the panel was the
// only thing using the API. The QR tab's per-row download menu is a popover too
// (D24's mechanism, chosen for Escape and light dismiss, the same reason the
// header's menus use it) and it is not a panel: it holds two links, hangs off
// the row's own button, and carries none of the chrome panel_open owes — no
// title, no Close, no *Open as a page*. Reading it as a returning popup would
// be the proxy speaking over the claim.
//
// What the claim actually is, from m48.md's F212 amendment: the QR *settings*
// are in the tab's flow rather than behind a popup, and the panel mechanism is
// defined once. Both still hold, and TestTheQRSettingsRenderInTheTabsFlow below
// is the assertion that the first one is checked rather than assumed.
//
// Anchored on the modal sheet's own geometry, which is panel_open's and which
// nothing else declares. TestThePanelMechanismIsDefinedOnce asserts that the
// real panel still carries it, so changing panel.html's geometry fails there
// and points here rather than quietly widening what may pass below.
const panelSheet = "max-h-[85vh]"

// panelsIn returns the panel containers inside a rendered page — popovers
// carrying panel_open's sheet geometry, and not every popover.
func panelsIn(body string) [][]string {
	var out [][]string
	for _, m := range panelOpen.FindAllStringSubmatch(body, -1) {
		if strings.Contains(m[2], panelSheet) {
			out = append(out, m)
		}
	}
	return out
}

// qrRowMenuID is the id prefix M50.7's per-row download menus write, and
// qrAddID is the id M50.8's add prompt writes. Together they are the *whole* of
// what is allowed to use the popover API inside <main>.
//
// **The exception widened deliberately and in the milestone's own diff**
// (M50.8, F238h). The add prompt is the third popover on this surface, and
// D189's exception as written named only the first two — so it would have
// arrived as three red assertions somebody widened to make green, which is the
// silent-extension shape D189 exists to prevent. It is named here instead, and
// F237's warning still stands: this matcher reads one spelling of a popover
// declaration, so a fourth one written differently is a popover nobody counts.
const (
	qrRowMenuID = "qr-download-"
	qrAddID     = "qr-add"
)

// strayPopoversIn returns the ids of popovers inside a rendered page that are
// neither a panel nor one of the QR tab's own two.
//
// **Why this exists rather than panelsIn alone.** Until M50.7 the absence
// assertions read "no popover inside <main>" and that was exact. Widening them
// to "no panel" was right on the QR tab, where a menu that is not a panel
// legitimately uses the API — and wrong everywhere else, because the six other
// link tabs and the panel's own page have no reason to hold a popover of any
// kind, and after the widening a second popup on any of them would have passed
// in silence. Naming the exception keeps the rule; dropping it did not.
//
// Not `panelsIn`'s complement: a panel is caught by the assertion beside every
// call of this, and a panel and a stray popover fail for different reasons.
func strayPopoversIn(body string) []string {
	var out []string
	// [anyPopover] rather than [panelOpen] (F237): this function's sentence is
	// about *any* popover, and matching one spelling could not support it.
	for _, tag := range anyPopover.FindAllString(body, -1) {
		id := ""
		if m := popoverID.FindStringSubmatch(tag); m != nil {
			id = m[1]
		}
		classes := ""
		if m := popoverClasses.FindStringSubmatch(tag); m != nil {
			classes = m[1]
		}
		if strings.Contains(classes, panelSheet) || qrListPopover(id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// qrListPopover says whether an id is one the codes list is allowed to write.
func qrListPopover(id string) bool {
	return strings.HasPrefix(id, qrRowMenuID) || id == qrAddID
}

// qrCodesListPopoversIn returns the ids of the popovers the QR codes list draws
// — one download menu per row, and the add prompt beside the counter.
//
// **Every caller of strayPopoversIn owes this one too.** That function answers
// *is this a popup arriving under another name*, and neither of these ever is,
// so it waves them through wherever they appear. *Where they are allowed* is the
// second question, and without it the exception is unbounded: a `qr-download-`
// popover would pass on a page that renders no codes list at all, which is the
// hole the exception was written to avoid rather than open. Both belong to the
// codes list, so a page without the list must hold none — and the page with it
// must hold some, or the exception is guarding nothing.
func qrCodesListPopoversIn(body string) []string {
	var out []string
	for _, m := range panelOpen.FindAllStringSubmatch(body, -1) {
		if qrListPopover(m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// panelSurfaces is one page that renders a popup panel, and the route the
// panel's contents are served at.
//
// A list rather than a scan, and this is the one place a new surface has to be
// added. That is deliberate: a test that discovered its own subjects could not
// hold a new popup to the shared chrome.
var panelSurfaces = []struct {
	page string
	// body is the page the panel's Href leads to, rendered on its own.
	body string
	href string
	// marker is markup that appears in the panel's contents and nowhere else on
	// either page, so "the same block" is checked against something rather than
	// asserted.
	marker string
}{
	{
		page: "disputes", body: "dispute_reviewers",
		href:   "/disputes/reviewers",
		marker: `id="reviewer-email"`,
	},
}

// panelPages are the routes that serve on-demand contents as an ordinary page.
//
// A superset of panelSurfaces' body pages: the QR contents keep their route
// after their popup retired — it is what a bookmark reaches, and `?code=` on it
// opens the block on one code — so the page-form obligation outlives the popup
// that first carried it. The codes list stopped selecting through it at M50.8's
// third reopening, which took the route's last incoming link off the tab and
// left the obligation exactly where it was.
var panelPages = []struct {
	body   string
	href   string
	marker string
	// codes says the page renders the QR codes list, which is the only thing
	// that renders the per-row download menus or the add prompt beside the
	// counter. It bounds M50.7's popover
	// exception to the page it was written for, exactly as the link page's loop
	// bounds it to the QR tab.
	codes bool
}{
	{body: "link_qr", href: "/links/0198c9c5-0000-7000-8000-000000000001/qr",
		marker: `id="qr_foreground"`, codes: true},
	{body: "dispute_reviewers", href: "/disputes/reviewers",
		marker: `id="reviewer-email"`},
}

// TestThePanelMechanismIsDefinedOnce is the "defined once" half.
//
// It reads the rendered HTML rather than the templates, because what must be
// identical is what a browser receives: two partials that happen to import the
// same words but emit different geometry would pass a template-level check and
// fail a reader.
func TestThePanelMechanismIsDefinedOnce(t *testing.T) {
	var class string
	var from string
	for _, s := range panelSurfaces {
		page := mainOf(t, renderPage(t, s.page, nil))

		// Inside <main>, so the two header menus — which are popovers too, and
		// deliberately a different shape (D24, dropdowns anchored to the bar) —
		// are not counted. They live in the shell.
		found := panelsIn(page)
		if len(found) != 1 {
			t.Fatalf("%s renders %d panels inside <main>, want exactly 1. A second "+
				"popup on one surface is the pattern being invented twice, which is "+
				"the failure m48.md names.", s.page, len(found))
		}
		id, got := found[0][1], found[0][2]

		// And nothing else on this page uses the API. The line above was
		// `panelOpen` matches inside <main>, count exactly 1, until M50.7
		// widened it to panels only; on a page that is neither the QR tab nor
		// the codes list's own route, that widening would let any second popup
		// through in silence. The rule is kept and the exception named (D189).
		if stray := strayPopoversIn(page); len(stray) > 0 {
			t.Errorf("%s renders the popovers %v inside <main> beside its panel, "+
				"which are neither the panel mechanism nor the QR codes list's own "+
				"two. A second popup on one surface is the pattern being invented "+
				"twice, whatever it calls itself", s.page, stray)
		}
		if menus := qrCodesListPopoversIn(page); len(menus) > 0 {
			t.Errorf("%s renders the QR codes list's popovers %v; they belong to that "+
				"list, which this page does not render, so the exception is being "+
				"borrowed by a page it was not written for", s.page, menus)
		}

		// The signature the absence assertions key on, checked against the real
		// panel rather than trusted. Without this, changing panel_open's
		// geometry would silently stop those assertions catching anything.
		if !strings.Contains(got, panelSheet) {
			t.Errorf("the panel on %s no longer declares %s, which is what tells it "+
				"from an ordinary popover. panelSheet has to move with it, or "+
				"every \"no panel here\" assertion below stops meaning anything:\n  %s",
				s.page, panelSheet, got)
		}

		if class == "" {
			class, from = got, s.page
		} else if got != class {
			t.Errorf("the panel on %s carries different classes from the one on %s, "+
				"so they are two mechanisms wearing one name:\n  %s: %s\n  %s: %s\n\n"+
				"Both must come from panel_open in partials/panel.html. Geometry that "+
				"differs between panels is geometry nobody derived twice.",
				s.page, from, from, class, s.page, got)
		}

		// The chrome the mechanism owes every caller: something to open it with,
		// something to close it with, and the route its contents live at.
		for _, want := range []string{
			`popovertarget="` + id + `"`,
			`popovertarget="` + id + `" popovertargetaction="hide"`,
			`href="` + s.href + `"`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the panel on %s is missing %s", s.page, want)
			}
		}

		if !strings.Contains(page, s.marker) {
			t.Errorf("%s renders a panel that does not contain %s; the panel's "+
				"contents are supposed to be on the page that carries it, so the "+
				"popup needs no request to fill itself", s.page, s.marker)
		}
	}
}

// TestEveryPanelIsAlsoACompletePage is the no-JavaScript half, and it is the
// bullet m48.md states as a requirement rather than a nicety.
//
// *"The panel is a route that renders as an ordinary page when opened directly,
// and the popup is what the browser does with it when it can."* The shell was
// already inconsistent about reachability without script — links.html has a
// <noscript> submit, the workspace switcher deliberately does not (D103/F21) —
// and this is the third answer that must not become a fourth: the contents live
// at a URL, and no browser capability is needed to read them.
//
// What it cannot check is that the route exists, because this package has no
// router. TestEveryPanelRouteIsMounted in internal/httpx is that half, and the
// `href` above is the value both ends compare against.
func TestEveryPanelIsAlsoACompletePage(t *testing.T) {
	for _, s := range panelPages {
		t.Run(s.body, func(t *testing.T) {
			body := renderPage(t, s.body, nil)

			// A complete page, in the same terms TestEveryPageRenders uses: it
			// went through the layout and it was not truncated.
			for _, want := range []string{"<!doctype html>", "</html>", "<main "} {
				if !strings.Contains(body, want) {
					t.Errorf("the route's page does not render %s, so opening it "+
						"directly gives a fragment rather than a page", want)
				}
			}
			// And the chrome, so somebody who arrived here has somewhere to go.
			// A page reachable only from the surface it backs is not a fallback.
			if !strings.Contains(body, bellMark) {
				t.Error("the route's page renders without the header; it is a route " +
					"somebody can bookmark, and a bookmark that lands outside the " +
					"dashboard is not the page form of anything")
			}

			// The same contents the in-app surface holds, checked against the
			// same marker.
			if !strings.Contains(body, s.marker) {
				t.Errorf("the page at %s does not render %s. The two surfaces are "+
					"supposed to be one block: a divergence between them is the "+
					"first thing this pattern would get wrong.", s.href, s.marker)
			}

			// It must not draw its own popup. The page *is* the contents here,
			// and a panel on it would be the mechanism nested in itself. An
			// ordinary popover is not that — see panelSheet.
			inMain := mainOf(t, body)
			if n := len(panelsIn(inMain)); n > 0 {
				t.Errorf("%s renders %d panels of its own; this page is the plain "+
					"form of an on-demand surface, so a panel here nests the "+
					"mechanism in itself", s.body, n)
			}
			// And no other popover than the exception D189 names. The QR
			// contents' page carries the codes list, so its row menus and its
			// add prompt are expected here; anything else is the popup arriving
			// by a side door.
			if stray := strayPopoversIn(inMain); len(stray) > 0 {
				t.Errorf("%s renders the popovers %v, which are neither the panel "+
					"mechanism nor the QR codes list's own two (D189, widened for "+
					"M50.8's add prompt)", s.body, stray)
			}
			// The exception bounded, both ways, as the link page's tabs bound it:
			// these belong to the codes list and go where it goes.
			menus := qrCodesListPopoversIn(inMain)
			if !s.codes && len(menus) > 0 {
				t.Errorf("%s renders the QR codes list's popovers %v; they belong to "+
					"that list, which this page does not render", s.body, menus)
			}
			if s.codes && len(menus) == 0 {
				t.Error("the codes list's own page renders none of the popovers that " +
					"list draws, so the exception above is guarding nothing and would " +
					"pass on a page that had lost the list")
			}
		})
	}
}

// TestThePopoverMatcherSeesEverySpellingOfOne is F237's own evidence, kept.
//
// The absence assertions on the link page's tabs and on the panel route say "no
// popover here". They routed through [panelOpen], which matches exactly one
// spelling — a `<div>`, `id` before `popover="auto"` before `class` — so the popup
// they exist to refuse could return in a shape none of them could read. Nothing in
// the tree escaped it, which is why this was a row and not a rejection: the guard
// was narrower than the sentence beside it.
//
// Each case below is a real way to write a popover, and each was invisible to the
// old matcher. The last case is the trap the general form introduces and is the
// reason this test exists rather than a comment: `popovertarget` is the *invoker*
// attribute, it begins with the same eight letters, and a matcher that counts it
// reports every button that opens a menu as a stray popup.
func TestThePopoverMatcherSeesEverySpellingOfOne(t *testing.T) {
	for _, tc := range []struct{ name, html string }{
		{"the spelling panelOpen was written for", `<div id="x" popover="auto" class="sheet">y</div>`},
		{"on a button", `<button id="b" popover="auto" class="c">y</button>`},
		{"on a dialog", `<dialog id="d" popover="auto" class="c">y</dialog>`},
		{"attributes in another order", `<div popover="auto" id="e" class="c">y</div>`},
		{"a bare popover attribute", `<div id="f" popover class="c">y</div>`},
		{"an id carrying an uppercase letter", `<div id="G" popover="auto" class="c">y</div>`},
		{"popover=manual", `<div id="h" popover="manual" class="c">y</div>`},
	} {
		if got := popoversIn(tc.html); len(got) != 1 {
			t.Errorf("%s: the popover matcher found %d, want 1. A popup written this way "+
				"would pass every absence assertion on every tab", tc.name, len(got))
		}
	}
	if got := popoversIn(`<button type="button" popovertarget="qr-add">Add</button>`); len(got) != 0 {
		t.Errorf("an invoker was counted as a popover: %v. `popovertarget` opens one and "+
			"is not one, and counting it makes every menu button a stray popup", got)
	}

	// And the half that actually guards the pages: [strayPopoversIn] is what every
	// absence assertion calls, so it is the function that has to see the general
	// form. Testing the matcher alone would leave the assertions free to keep using
	// the narrow one, which is the state this row found them in.
	stray := `<main><dialog id="surprise" popover class="whatever">a second popup</dialog></main>`
	if got := strayPopoversIn(stray); len(got) != 1 || got[0] != "surprise" {
		t.Errorf("strayPopoversIn found %v in a page holding a popover written on a "+
			"<dialog> with a bare attribute; the absence assertions on seven tabs and "+
			"the panel route all read this function, and a popup they cannot see is a "+
			"popup they permit", got)
	}
	// The panel itself is not a stray, whichever spelling it arrives in.
	panel := `<dialog id="panel-x" popover class="` + panelSheet + ` more">the panel</dialog>`
	if got := strayPopoversIn(panel); len(got) != 0 {
		t.Errorf("the panel mechanism was reported as a stray popover: %v", got)
	}
}
