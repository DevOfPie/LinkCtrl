package ui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// renderPage renders one page with the shared fixture, after applying overrides.
func renderPage(t *testing.T, page string, override map[string]any) string {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	d, ok := pageData(t)[page].(map[string]any)
	if !ok {
		t.Fatalf("the %s page data is not a map", page)
	}
	for k, v := range override {
		d[k] = v
	}
	rec := httptest.NewRecorder()
	if err := r.Render(rec, http.StatusOK, page, d); err != nil {
		t.Fatalf("render %s: %v", page, err)
	}
	return rec.Body.String()
}

// TestWorkspaceSwitcherAppearsOnlyWhenThereIsSomewhereToGo is the milestone's
// no-op claim, asserted where a reader can see it.
//
// Every instance today has one membership per user. A dropdown listing that one
// workspace is a control that can do nothing, and it would change every page of
// every existing dashboard to no purpose — so the partial draws nothing until a
// second membership exists. The two directions are asserted together because
// only the pair is meaningful: "renders with two" alone would pass on a partial
// that always renders.
func TestWorkspaceSwitcherAppearsOnlyWhenThereIsSomewhereToGo(t *testing.T) {
	const form = `action="/workspace/switch"`

	two := renderPage(t, "dashboard", nil)
	if !strings.Contains(two, form) {
		t.Error("with two workspaces the header draws no switcher")
	}
	for _, want := range []string{
		`value="0198c9c5-0000-7000-8000-000000000011"`,
		"Acme · Marketing",
		`name="next" value="/dashboard"`, // switching returns to the page you were on
	} {
		if !strings.Contains(two, want) {
			t.Errorf("the switcher is missing %q", want)
		}
	}
	// M46: the current workspace is not one of the places you can go. The owner
	// asked for it to leave on blind task 9, and workspace_label is what made
	// that possible — see TestTheHeaderNamesWhereYouAreAtEveryMembershipCount.
	if strings.Contains(two, `value="0198c9c5-0000-7000-8000-000000000010"`) {
		t.Error("the switcher still offers the workspace you are already in; the " +
			"header label is where the current workspace is named now, and a " +
			"switcher lists the places you can move to")
	}
	// Reopened M46.6 (F209): the opened state is the shell's own popover panel,
	// not the engine's select popup. A select had to display something in its
	// closed face, so it carried a blank disabled placeholder; a button need
	// not, and the placeholder assertion that stood here retired with its
	// premise — owner-approved in approving the reopening. What replaces it:
	// no select survives in the header, and the panel holds no blank entry —
	// every option is a submit button carrying a workspace id and naming its
	// workspace.
	header, _, ok := strings.Cut(two, "</header>")
	if !ok {
		t.Fatal("the page draws no header")
	}
	if strings.Contains(header, "<select") {
		t.Error("the switcher still renders a native select; its opened state is " +
			"the engine's popup, which cannot be positioned, styled, or purged of " +
			"its placeholder row (F209)")
	}
	options := regexp.MustCompile(
		`<button type="submit" name="workspace_id" value="([^"]*)"[^>]*>([^<]*)</button>`).
		FindAllStringSubmatch(header, -1)
	if len(options) == 0 {
		t.Fatal("the switcher's panel offers no workspace buttons")
	}
	for _, o := range options {
		if o[1] == "" {
			t.Error("a workspace button posts an empty workspace_id; the blank row " +
				"is what the popover panel exists to not draw (F209)")
		}
		if strings.TrimSpace(o[2]) == "" {
			t.Error("a workspace button names nothing; every entry in the panel " +
				"names its workspace")
		}
	}

	one := renderPage(t, "dashboard", map[string]any{
		"Workspaces": twoWorkspaces()[:1],
	})
	if strings.Contains(one, form) {
		t.Error("a single-membership account is shown a switcher that can do nothing; " +
			"this milestone must be invisible to every instance that exists today")
	}

	none := renderPage(t, "dashboard", map[string]any{"Workspaces": []map[string]any{}})
	if strings.Contains(none, form) {
		t.Error("the switcher rendered with no workspaces at all")
	}

	// Signed out there is no header, so there is nothing to switch and nothing
	// to leak about which workspaces exist.
	if out := renderPage(t, "login", nil); strings.Contains(out, form) {
		t.Error("the sign-in page renders a workspace switcher")
	}
}

// TestTheHeaderNamesWhereYouAreAtEveryMembershipCount is M46's first bullet, and
// the case it is written for is the one that renders nothing today.
//
// Blind task 9 could not be completed: the owner could not confirm which
// workspace they were acting in. With two memberships the switcher's selected
// option was the only place the answer appeared anywhere in the shell, and with
// one — which is every instance that exists — `{{if gt (len .Workspaces) 1}}`
// drew no switcher, so the answer appeared nowhere at all.
//
// Both counts are asserted in one test because only the pair is meaningful. A
// label that renders whenever the switcher does would pass a two-membership
// check and still leave the single-membership case exactly as it was, which is
// the case the candidate row was filed about.
func TestTheHeaderNamesWhereYouAreAtEveryMembershipCount(t *testing.T) {
	const switcher = `action="/workspace/switch"`

	for _, tc := range []struct {
		name       string
		workspaces []map[string]any
		wantSwitch bool
	}{
		{"one membership", twoWorkspaces()[:1], false},
		{"two memberships", twoWorkspaces(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := renderPage(t, "dashboard", map[string]any{"Workspaces": tc.workspaces})

			header, _, ok := strings.Cut(body, "</header>")
			if !ok {
				t.Fatal("the page draws no header, so it names nothing")
			}
			// The organization and the workspace, both, and inside the header
			// rather than anywhere on the page: the point is that the answer is
			// on every page without going to look for it.
			for _, want := range []string{"Owner", "Default"} {
				if !strings.Contains(header, ">"+want+"</span>") {
					t.Errorf("the header does not name %q; with %s the shell has to "+
						"say which organization and which workspace this is",
						want, tc.name)
				}
			}
			if !strings.Contains(header, `<span class="sr-only">Current workspace:</span>`) {
				t.Error("the two names are in the header with nothing saying what " +
					"they are, so a screen reader gets an organization and a workspace " +
					"and no reason to think they describe the current one")
			}

			// M25 is honoured, not reversed: what fills the one-membership gap is
			// a label, never a dropdown with a single entry.
			if got := strings.Contains(body, switcher); got != tc.wantSwitch {
				t.Errorf("with %s the header draws switcher=%v, want %v; a control "+
					"that cannot do anything stays absent (M25)", tc.name, got, tc.wantSwitch)
			}
		})
	}

	// An account that belongs to nothing has no workspace, and the label must not
	// invent one — D36 made that a state somebody can legitimately be in.
	none := renderPage(t, "organization_new", nil)
	if strings.Contains(none, `<span class="sr-only">Current workspace:</span>`) {
		t.Error("the header names a current workspace for an account that belongs " +
			"to no organization")
	}
}

// TestTheHeaderCannotPushThePageSideways is the shell half of M46's
// no-horizontal-scroll bullet.
//
// Round two: the "page needs to be scrolled to the side to see anything past
// half of the workspace switcher". The cause is not one wide element — it is
// that a flex item refuses by default to shrink below its own content, so one
// long organization name widens the bar, the bar widens the document, and every
// page on the instance scrolls sideways at once.
//
// Asserted as classes rather than as pixels, in the idiom of M24.5's template
// scan, because the alternative is a browser in the unit-test path. The pixel
// claim is checked separately and by hand; what this holds is the property that
// makes it true, so a later edit that drops `min-w-0` from the group fails here
// instead of failing on somebody's phone.
func TestTheHeaderCannotPushThePageSideways(t *testing.T) {
	body := renderPage(t, "dashboard", nil)

	bar := regexp.MustCompile(`(?s)<nav\b[^>]*>`).FindString(body)
	if bar == "" {
		t.Fatal("the page draws no header bar")
	}
	// Two lines below `sm`, one above it. Nothing narrower than 640px fits the
	// logo, the destinations, the label, the switcher, the bell and the identity
	// control on one line, and a row that cannot fit either wraps or overflows.
	if !hasClass(bar, "flex-wrap") {
		t.Errorf("the header bar does not wrap: %s", bar)
	}
	if !hasClass(bar, "sm:flex-nowrap") {
		t.Errorf("the header bar wraps at every width, so the panel geometry "+
			"written against h-14 is wrong above `sm`: %s", bar)
	}

	group := regexp.MustCompile(`(?s)<div class="ml-auto[^"]*"`).FindString(body)
	if group == "" {
		t.Fatal("the header has no right-hand group")
	}
	if !strings.Contains(group, " min-w-0") {
		t.Errorf("the header's right-hand group cannot shrink, so the longest "+
			"name in it sets the width of every page: %s", group)
	}

	// The two unbounded strings in the bar. Everything else in it is a fixed
	// glyph or a word this repository chose. The label is matched whole rather
	// than as its opening tag, because the shrink is on the wrapper and the
	// ellipsis is on the two names inside it.
	label := regexp.MustCompile(`(?s)<p class="flex min-w-0[^"]*" title="[^"]*">.*?</p>`).FindString(body)
	if label == "" {
		t.Fatal("the workspace label is not in the header, or it no longer carries " +
			"min-w-0 on its wrapper")
	}
	sel := regexp.MustCompile(`<button type="button" popovertarget="linkctrl-workspace-menu"[^>]*>`).FindString(body)
	if sel == "" {
		t.Fatal("the workspace switcher's invoker is not in the header")
	}
	if !strings.Contains(label, "min-w-0") {
		t.Errorf("the workspace label will not shrink below its text: %s", label)
	}
	if !strings.Contains(label, "truncate") {
		t.Errorf("the workspace label does not truncate, so shrinking it clips "+
			"rather than ellipsises and the name reads as cut off: %s", label)
	}
	// M46.6: the switcher's closed face is a chevron alone — since the
	// reopening, a button whose face is the chevron glyph, holding no text at
	// all. What protects the page is that its width is fixed — it can neither
	// grow with an option's text nor collapse out of the shared boundary —
	// rather than that it shrinks below content.
	for _, want := range []string{"w-8", "shrink-0"} {
		if !hasClass(sel, want) {
			t.Errorf("the workspace switcher's face is not fixed-width (%s missing), "+
				"so its box no longer holds the shared boundary's shape: %s", want, sel)
		}
	}
}

// TestTheWorkspacePairReadsAsOneControl is M46.6's boundary, at every
// membership count.
//
// F204: the label said where you are and the select said how to leave, and
// nothing said the two sentences were one claim. The fix is a shared bordered
// container — label, hairline divider, select — and the three states are the
// contract. Several memberships draw all three and never offer the current
// workspace. One membership draws the label alone: no divider, no dead
// affordance (M25). No current workspace draws no container at all, never an
// empty bordered box (D36).
func TestTheWorkspacePairReadsAsOneControl(t *testing.T) {
	const (
		divider = `h-4 w-px`
		form    = `action="/workspace/switch"`
		srOnly  = `<span class="sr-only">Current workspace:</span>`
	)
	// The container's opening tag through the first </div> after it. With a
	// switcher present that is the popover panel's own close — the panel nests
	// inside the form, after the label, the divider and every workspace button
	// — so everything asserted below still sits inside the match. With one
	// membership nothing nests a div and it is the container's own.
	container := regexp.MustCompile(
		`(?s)<div class="flex min-w-0 items-center gap-2 rounded-md border border-line-strong[^"]*">.*?</div>`)

	two := renderPage(t, "dashboard", nil)
	box := container.FindString(two)
	if box == "" {
		t.Fatal("with two memberships the header draws no shared container, so the " +
			"label and the switcher read as two unrelated fragments (F204)")
	}
	if !hasClass(box, "focus-within:border-accent") {
		t.Errorf("the shared container does not indicate focus, and the select's own "+
			"border is gone — a keyboard user tabbing to it sees nothing: %s", box)
	}
	for _, want := range []string{srOnly, ">Owner</span>", ">Default</span>", divider, form} {
		if !strings.Contains(box, want) {
			t.Errorf("with two memberships the shared container is missing %q; the "+
				"pair reads as one control only if label, divider and select share "+
				"the boundary", want)
		}
	}
	// The closed face is the chevron alone: since the reopening, a button whose
	// face is the chevron glyph — nothing to paint before, during, or after a
	// switch. Its opened state is the shell's own popover panel (D24's pattern,
	// F209's fix), anchored to this container so it hangs off the control
	// rather than wherever the engine puts it. The classes assert the
	// properties that make the rendered claims true, in M24.5's idiom; what it
	// looks like is the kept browser spec's half.
	if !strings.Contains(box, `popovertarget="linkctrl-workspace-menu"`) {
		t.Error("the fused control holds no popover invoker, so the switcher's " +
			"opened state is not the shell's own panel (F209)")
	}
	if !strings.Contains(box, `popover="auto"`) {
		t.Error(`the switcher's panel is not popover="auto", which is the value ` +
			"carrying Escape, light dismiss and one-open-at-a-time (D24)")
	}
	if !hasClass(box, "[anchor-name:--linkctrl-workspace]") {
		t.Error("the container carries no anchor-name, so the panel right-aligns " +
			"to the menus' shared edge instead of to the control it opens from")
	}
	// The list it opens still never offers the workspace you are in.
	if strings.Contains(box, `value="0198c9c5-0000-7000-8000-000000000010"`) {
		t.Error("the fused control offers the workspace you are already in")
	}

	one := renderPage(t, "dashboard", map[string]any{"Workspaces": twoWorkspaces()[:1]})
	box = container.FindString(one)
	if box == "" {
		t.Fatal("with one membership the container is gone; the account must stay " +
			"named in the same box whether or not there is anywhere to go")
	}
	for _, want := range []string{srOnly, ">Owner</span>", ">Default</span>"} {
		if !strings.Contains(box, want) {
			t.Errorf("with one membership the container is missing %q", want)
		}
	}
	for _, absent := range []string{divider, form} {
		if strings.Contains(box, absent) {
			t.Errorf("with one membership the container draws %q — a divider or a "+
				"switcher with nowhere to go is a dead affordance (M25)", absent)
		}
	}

	none := renderPage(t, "dashboard", map[string]any{"Workspaces": []map[string]any{}})
	if container.MatchString(none) {
		t.Error("with no memberships the header draws an empty bordered box; an " +
			"account that belongs to nothing has no workspace to name (D36)")
	}
}

// TestDefaultWorkspaceControlDefaultsToLastUsed pins the shape of the account
// setting: the derived behaviour is what the control shows until somebody pins
// something, and pinning has to be reversible.
func TestDefaultWorkspaceControlDefaultsToLastUsed(t *testing.T) {
	unpinned := renderPage(t, "account", nil)
	if !strings.Contains(unpinned, `action="/workspace/default"`) {
		t.Fatal("the account page has no default-workspace control")
	}
	if !strings.Contains(unpinned, `<option value="" selected>Last-Used</option>`) {
		t.Error("with nothing pinned the control does not show Last-Used as the choice; " +
			"the derived behaviour has to be the default the control reads back")
	}

	pinned := twoWorkspaces()
	pinned[1]["Default"] = true
	page := renderPage(t, "account", map[string]any{
		"Workspaces": pinned, "WorkspacePinned": true,
	})
	if strings.Contains(page, `<option value="" selected>Last-Used</option>`) {
		t.Error("a pinned account still shows Last-Used as the current choice")
	}
	if !strings.Contains(page, `value="0198c9c5-0000-7000-8000-000000000011" selected`) {
		t.Error("the pinned workspace is not the selected option")
	}
	// The way back has to stay reachable, or pinning is a one-way door.
	if !strings.Contains(page, `<option value="">Last-Used</option>`) {
		t.Error("a pinned account cannot choose Last-Used again")
	}
}
