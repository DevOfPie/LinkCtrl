package ui

import (
	"net/http"
	"net/http/httptest"
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
		`value="0198c9c5-0000-7000-8000-000000000010" selected`, // the current one
		`value="0198c9c5-0000-7000-8000-000000000011"`,
		"Acme · Marketing",
		`name="next" value="/dashboard"`, // switching returns to the page you were on
	} {
		if !strings.Contains(two, want) {
			t.Errorf("the switcher is missing %q", want)
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
