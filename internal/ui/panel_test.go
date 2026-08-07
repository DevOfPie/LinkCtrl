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
// what these tests defend is not either surface but the fact that there is one
// of it.

// panelOpen matches a panel container: the element panel_open emits.
//
// Both attributes, in this order, because the class list is the second capture
// and comparing class lists across pages is how "one mechanism" is checked. A
// surface that wrote its own popover would either not match this at all or match
// it with a different class string, and both are failures.
var panelOpen = regexp.MustCompile(`(?s)<div id="([a-z0-9-]+)" popover="auto"\s+class="([^"]*)">`)

// panelSurface is one page that renders a panel, and the route the panel's
// contents are served at.
//
// A list rather than a scan, and this is the one place a third surface has to be
// added. That is deliberate: m48.md asks for "at least two" callers, and a test
// that discovered its own subjects would pass on a milestone that shipped one.
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
		page: "link_detail", body: "link_qr",
		href:   "/links/0198c9c5-0000-7000-8000-000000000001/qr",
		marker: `id="qr_foreground"`,
	},
	{
		page: "disputes", body: "dispute_reviewers",
		href:   "/disputes/reviewers",
		marker: `id="reviewer-email"`,
	},
}

// TestBothPanelsUseTheOnePanelMechanism is the "defined once" half.
//
// It reads the rendered HTML rather than the templates, because what must be
// identical is what a browser receives: two partials that happen to import the
// same words but emit different geometry would pass a template-level check and
// fail a reader.
func TestBothPanelsUseTheOnePanelMechanism(t *testing.T) {
	if len(panelSurfaces) < 2 {
		t.Fatal("m48.md requires at least two callers of the panel; a pattern with " +
			"one caller is a component with extra steps")
	}

	var class string
	var from string
	for _, s := range panelSurfaces {
		page := mainOf(t, renderPage(t, s.page, nil))

		// Inside <main>, so the two header menus — which are popovers too, and
		// deliberately a different shape (D24, dropdowns anchored to the bar) —
		// are not counted. They live in the shell.
		found := panelOpen.FindAllStringSubmatch(page, -1)
		if len(found) != 1 {
			t.Fatalf("%s renders %d panels inside <main>, want exactly 1. A second "+
				"popup on one surface is the pattern being invented twice, which is "+
				"the failure m48.md names.", s.page, len(found))
		}
		id, got := found[0][1], found[0][2]

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
	for _, s := range panelSurfaces {
		t.Run(s.body, func(t *testing.T) {
			body := renderPage(t, s.body, nil)

			// A complete page, in the same terms TestEveryPageRenders uses: it
			// went through the layout and it was not truncated.
			for _, want := range []string{"<!doctype html>", "</html>", "<main "} {
				if !strings.Contains(body, want) {
					t.Errorf("the panel's route does not render %s, so opening it "+
						"directly gives a fragment rather than a page", want)
				}
			}
			// And the chrome, so somebody who arrived here has somewhere to go.
			// A page reachable only from the popup it replaces is not a fallback.
			if !strings.Contains(body, bellMark) {
				t.Error("the panel's page renders without the header; it is a route " +
					"somebody can bookmark, and a bookmark that lands outside the " +
					"dashboard is not the page form of anything")
			}

			// The same contents the popup holds, checked against the same marker.
			if !strings.Contains(body, s.marker) {
				t.Errorf("the page at %s does not render %s, which the popup on %s "+
					"does. The two are supposed to be one block: a divergence between "+
					"a panel and the page it is a panel *of* is the first thing this "+
					"pattern would get wrong.", s.href, s.marker, s.page)
			}

			// It must not draw its own popup. The page *is* the panel here, and a
			// popover on it would be the panel inside itself.
			if panelOpen.MatchString(mainOf(t, body)) {
				t.Errorf("%s renders a panel of its own; this page is what the panel "+
					"opens onto, so a popover here nests the mechanism in itself", s.body)
			}
		})
	}
}
