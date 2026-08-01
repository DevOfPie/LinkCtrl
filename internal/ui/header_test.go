package ui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The two markers the header's controls are counted by. Both are load-bearing
// attributes rather than test hooks: the identity menu exists to hold sign-out,
// and the bell exists to reach the full page.
//
// `href="/notifications"` is written with its closing quote on purpose. The
// notifications page's own "older" link is `href="/notifications?cursor=…"`, and
// a prefix match would count it.
const (
	identityMenuMark = `action="/logout"`
	bellMark         = `href="/notifications"`
)

// chromelessPages are served with no session at all, so the layout draws no
// header and neither control may appear. Naming them here rather than deriving
// them from the fixture means a page that quietly stops carrying an identity
// fails this test instead of silently lowering the expected count.
var chromelessPages = []string{"login", "setup", "error"}

// TestExactlyOneIdentityMenuAndBellPerPage is M24.5's assertion applied to the
// two controls this milestone moves, and for the same reason.
//
// Both were in the layout, which every page renders. "Move" is one edit away
// from "copy": adding a site inside a page without removing the layout's gives
// two controls that can disagree — one showing a stale count, two forms posting
// to /logout — and the cheapest possible way to get that wrong is to leave the
// original in place. So the count is asserted exactly, on every page, in both
// directions.
func TestExactlyOneIdentityMenuAndBellPerPage(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	data := pageData(t)

	chromeless := make(map[string]bool, len(chromelessPages))
	for _, p := range chromelessPages {
		chromeless[p] = true
	}

	for _, page := range r.Pages() {
		t.Run(page, func(t *testing.T) {
			d, ok := data[page]
			if !ok {
				t.Fatalf("no test data for page %q", page)
			}
			rec := httptest.NewRecorder()
			if err := r.Render(rec, http.StatusOK, page, d); err != nil {
				t.Fatalf("render %s: %v", page, err)
			}
			body := rec.Body.String()

			want := 1
			if chromeless[page] {
				want = 0
			}
			if got := strings.Count(body, identityMenuMark); got != want {
				t.Errorf("%s renders %d identity menus, want %d", page, got, want)
			}
			if got := strings.Count(body, bellMark); got != want {
				t.Errorf("%s renders %d notification bells, want %d", page, got, want)
			}
		})
	}
}

// headerHrefs is every link in the bar to the left of the right-hand group —
// the top-level nav, plus the logo, which links to the dashboard too.
var headerHrefs = regexp.MustCompile(`href="(/[^"]*)"`)

// TestTopLevelNavHoldsThreeDestinations is F6 and F7's visible outcome.
//
// Account is a preference surface and Notifications is a count; neither is a
// place you go to do the work, and both spent a slot competing with the three
// that are. The count is asserted rather than the absences, because "Account is
// gone" would still pass if it had been replaced by something else that does not
// belong at this level — and four milestones are queued behind this one, each
// wanting a slot.
func TestTopLevelNavHoldsThreeDestinations(t *testing.T) {
	body := renderPage(t, "dashboard", nil)

	// The right-hand group is where the switcher, the bell and the identity menu
	// live. What sits between the opening <nav> and it is the top-level nav.
	_, nav, opened := strings.Cut(body, "<nav ")
	nav, _, found := strings.Cut(nav, `<div class="ml-auto`)
	if !opened || !found {
		t.Fatal("the header has no nav or no right-hand group; the nav's shape " +
			"cannot be read")
	}

	var got []string
	for _, m := range headerHrefs.FindAllStringSubmatch(nav, -1) {
		got = append(got, m[1])
	}
	want := []string{"/dashboard", "/dashboard", "/links", "/keys"}

	if len(got) != len(want) {
		t.Fatalf("the top-level nav links to %v, want %v (the first is the logo)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the top-level nav links to %v, want %v", got, want)
		}
	}
}

// TestSignOutStaysAFormPost is the regression that would not be visible by
// looking.
//
// Signing out changes state. Moving it inside a menu is a layout edit, and the
// tidy-looking way to write a menu item is an anchor — which would make sign-out
// reachable by anything that follows links, and indistinguishable from the item
// above it in a rendered page. The menu looks identical either way, so this is
// asserted rather than reviewed.
func TestSignOutStaysAFormPost(t *testing.T) {
	body := renderPage(t, "dashboard", nil)

	if strings.Contains(body, `href="/logout"`) {
		t.Error("sign out is reachable as a link; it changes state, and a link " +
			"can be followed by a prefetch, a crawler or a mistyped middle click")
	}
	at := strings.Index(body, `<form method="post" action="/logout">`)
	if at < 0 {
		t.Fatal("sign out is not a form post")
	}
	// Inside the menu, not beside it: the panel opens with Account and ends with
	// sign-out.
	if !strings.Contains(body[:at], `href="/account"`) {
		t.Error("Account does not sit above Sign out in the identity menu")
	}
	if !strings.Contains(body[at:], "Sign out") {
		t.Error("the sign-out form has no label")
	}
}

// inlineHandler matches the attribute an obvious dropdown implementation would
// reach for and the CSP would then refuse to run.
var inlineHandler = regexp.MustCompile(`(?i)\son(click|change|toggle|keydown|keyup|focus|blur|submit|load)\s*=`)

// headerMenus is the two controls and the panels they open, by id.
//
// Named here rather than discovered from the markup so that a menu which loses
// its panel fails this file instead of quietly reducing what it checks.
var headerMenus = []struct{ name, panel string }{
	{"identity menu", "linkctrl-identity-menu"},
	{"notification bell", "linkctrl-notification-menu"},
}

func invokerFor(panelID string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)<button\b[^>]*\bpopovertarget="` + regexp.QuoteMeta(panelID) + `"[^>]*>`)
}

func panelTag(panelID string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)<div\b[^>]*\bid="` + regexp.QuoteMeta(panelID) + `"[^>]*>`)
}

// TestHeaderMenusAreScriptFreePopovers holds the no-JavaScript rule at the only
// place it can be held: the markup that ships.
//
// `ui` is stdlib-only and the CSP forbids inline handlers, so both menus are the
// Popover API — a `<button popovertarget>` invoker and a `popover` panel, which
// the browser wires together with nothing loaded and nothing evaluated. A menu
// that quietly grew a handler would still render, still look right in a
// screenshot, and simply not open for anyone whose browser enforces the policy.
//
// `<details>` is asserted absent rather than merely not asserted present. It is
// what this header shipped first, it is what somebody reaching for a
// script-free dropdown reaches for, and it looks correct in every respect
// except the one below: it cannot close on Escape.
func TestHeaderMenusAreScriptFreePopovers(t *testing.T) {
	signedIn := renderPage(t, "dashboard", nil)

	for _, m := range headerMenus {
		if got := len(invokerFor(m.panel).FindAllString(signedIn, -1)); got != 1 {
			t.Errorf("the %s renders %d <button popovertarget=%q>, want 1", m.name, got, m.panel)
		}
		if got := len(panelTag(m.panel).FindAllString(signedIn, -1)); got != 1 {
			t.Errorf("the %s renders %d panels with id=%q, want 1; a popovertarget "+
				"naming an id that is not there is a button that does nothing, and it "+
				"renders identically to one that works", m.name, got, m.panel)
		}
	}

	if strings.Contains(signedIn, "<details") || strings.Contains(signedIn, "<summary") {
		t.Error("a header menu is a <details> disclosure again; no engine closes one " +
			"on Escape, which is the whole reason these are popovers (D24)")
	}
	if m := inlineHandler.FindString(signedIn); m != "" {
		t.Errorf("the page carries the inline handler attribute %q; the CSP refuses "+
			"to run it, so the control it belongs to would be dead in a browser and "+
			"alive in a test", strings.TrimSpace(m))
	}
	// One script tag, the deferred htmx bundle, and it is external.
	if got := strings.Count(signedIn, "<script"); got != 1 {
		t.Errorf("the page renders %d script tags, want 1 (htmx, external)", got)
	}
	if strings.Contains(signedIn, "<script>") {
		t.Error("the page carries an inline script")
	}

	// Signed out there is no header at all, so neither menu leaks onto a page
	// with nothing to hang them on.
	if out := renderPage(t, "login", nil); strings.Contains(out, "popovertarget") {
		t.Error("the sign-in page renders a header menu")
	}
}

// TestBothMenusCloseOnEscapeAndAreReachableByKeyboard is the milestone's
// keyboard bullet, asserted against the rendered markup.
//
// Neither half of it is visible in a screenshot, and both are one attribute
// away from being false:
//
//   - Escape is `popover="auto"` and nothing else. An auto popover light
//     dismisses — Escape, and a click outside. `popover="manual"` renders the
//     same panel, opens from the same button, looks identical, and dismisses on
//     neither. So the value is asserted rather than the attribute's presence.
//   - Keyboard reach is the invoker being a real `<button>`: focusable, in the
//     tab order, activating on Enter and Space with nothing written to make it
//     so. A `<div popovertarget>` would open on a click and be unreachable
//     without one, and the two are indistinguishable with a mouse.
//
// The panel's contents are checked for the same reason: an `<a href>` and a
// `<button>` are focusable in sequence after their invoker without a roving
// tabindex, and a hand-written `tabindex` in here would be somebody rebuilding
// that by hand — the point at which this stops working without script.
func TestBothMenusCloseOnEscapeAndAreReachableByKeyboard(t *testing.T) {
	body := renderPage(t, "dashboard", nil)

	for _, m := range headerMenus {
		t.Run(m.name, func(t *testing.T) {
			invoker := invokerFor(m.panel).FindString(body)
			if invoker == "" {
				t.Fatalf("no <button popovertarget=%q> in the header", m.panel)
			}
			if !strings.Contains(invoker, `type="button"`) {
				t.Errorf("the invoker is %s; without type=button a <button> inside a "+
					"form submits it", invoker)
			}
			if strings.Contains(invoker, "tabindex") || strings.Contains(invoker, "disabled") {
				t.Errorf("the invoker takes itself out of the tab order: %s", invoker)
			}

			open := panelTag(m.panel).FindString(body)
			if open == "" {
				t.Fatalf("no panel with id=%q in the header", m.panel)
			}
			if !strings.Contains(open, `popover="auto"`) {
				t.Errorf("the panel is %s; only popover=\"auto\" light dismisses, and "+
					"Escape is light dismissal — a manual popover opens the same way "+
					"and then cannot be closed from a keyboard at all", open)
			}

			panel := elementAt(t, body, strings.Index(body, open))
			if strings.Contains(panel, "tabindex") {
				t.Error("the panel sets tabindex by hand; its items are links and " +
					"buttons, which are already focusable in order after the invoker")
			}
			if !strings.Contains(panel, "<a ") && !strings.Contains(panel, "<button") {
				t.Error("the panel holds nothing focusable, so opening it strands the " +
					"keyboard inside a menu with no way on")
			}

			// The positioning half of the same bullet, in the part markup can carry.
			// An open popover is in the top layer, whose containing block is the
			// viewport, so `absolute` inside the header anchors the panel to nothing
			// — it is the mistake that looks right until it is looked at.
			if !hasClass(open, "fixed") || !hasClass(open, "inset-auto") {
				t.Errorf("the panel is not positioned explicitly against the viewport: %s", open)
			}
			if hasClass(open, "absolute") {
				t.Errorf("the panel is positioned with `absolute`, which resolves "+
					"against the viewport anyway once it is in the top layer, and so "+
					"claims an anchor it does not have: %s", open)
			}
		})
	}
}

// hasClass reports whether an opening tag carries a class exactly, rather than
// as a substring: "absolute" is inside no other utility here, but "fixed" is
// inside "max-w-fixed"-shaped names somebody may add later, and the check that
// silently stops applying is the one worth spelling out.
func hasClass(tag, name string) bool {
	at := strings.Index(tag, `class="`)
	if at < 0 {
		return false
	}
	rest := tag[at+len(`class="`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return false
	}
	for _, c := range strings.Fields(rest[:end]) {
		if c == name {
			return true
		}
	}
	return false
}

// elementAt returns the whole <div> element starting at i, by counting opening
// and closing div tags. The panels nest divs, so a search for the first
// "</div>" would cut one of them in half.
func elementAt(t *testing.T, body string, i int) string {
	t.Helper()
	if i < 0 {
		t.Fatal("no element at that offset")
	}
	depth := 0
	for j := i; j < len(body); {
		switch {
		case strings.HasPrefix(body[j:], "<div"):
			depth++
			j += 4
		case strings.HasPrefix(body[j:], "</div>"):
			depth--
			j += 6
			if depth == 0 {
				return body[i:j]
			}
		default:
			j++
		}
	}
	t.Fatalf("the element starting at %d is never closed", i)
	return ""
}

// TestBellPreviewsUnreadAndAlwaysOffersTheFullPage covers both states, because
// only the pair is meaningful: an empty state that never renders and a preview
// that never renders look the same from one direction.
func TestBellPreviewsUnreadAndAlwaysOffersTheFullPage(t *testing.T) {
	withItems := renderPage(t, "dashboard", nil)
	for _, want := range []string{
		"The audit log has passed its size threshold", // the title
		"audit_logs now uses 5.2 GiB on disk.",        // the body
		"A notice with no body",                       // the branch with no body
		bellMark,                                      // View all
	} {
		if !strings.Contains(withItems, want) {
			t.Errorf("the bell's preview is missing %q", want)
		}
	}

	empty := renderPage(t, "dashboard", map[string]any{
		"Unread": int64(0), "UnreadPreview": []map[string]any{},
	})
	if !strings.Contains(empty, "Nothing new.") {
		t.Error("a bell with nothing unread opens onto a blank box; an empty inbox " +
			"is the ordinary state of a new account, not an error state")
	}
	if !strings.Contains(empty, bellMark) {
		t.Error("an empty bell drops the link to /notifications; the preview must " +
			"never be the only way to reach one")
	}
	// No badge with nothing unread — the count is exact in both directions.
	if strings.Contains(empty, "unread") {
		t.Error("the bell renders an unread badge with nothing unread")
	}
}

// TestWorkspaceSwitcherStaysInTheChrome is the boundary this milestone had to
// hold while moving everything around it.
//
// Which workspace you are acting in is a property of the request, changed often
// and from wherever you happen to be. Account and Sign out are things you do to
// yourself. Sweeping the whole right-hand side into one menu is the tempting
// edit and it would bury a control M25 deliberately put in the chrome, so the
// switcher's position relative to the identity menu is asserted rather than
// merely its presence.
func TestWorkspaceSwitcherStaysInTheChrome(t *testing.T) {
	body := renderPage(t, "dashboard", nil)

	switcher := strings.Index(body, `action="/workspace/switch"`)
	if switcher < 0 {
		t.Fatal("the header draws no workspace switcher")
	}
	menu := strings.Index(body, "popovertarget=")
	if menu < 0 {
		t.Fatal("the header draws no menus")
	}
	if switcher > menu {
		t.Error("the workspace switcher has moved inside a header menu; it is " +
			"organization-scoped rather than identity-scoped, and it belongs in " +
			"the bar where a person can reach it from any page")
	}
}
