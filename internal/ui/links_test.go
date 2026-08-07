package ui

import (
	"regexp"
	"strings"
	"testing"
)

// mainOf returns what the page itself drew, with the shell cut away.
//
// Every assertion about "the first thing on this page" needs it. The header is
// interactive by definition — it is a navigation bar — so a claim about the page
// that counted the header would be a claim no page could satisfy.
func mainOf(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, "<main ")
	if !ok {
		t.Fatal("the page has no <main>; the shell did not render")
	}
	inner, _, ok := strings.Cut(rest, "</main>")
	if !ok {
		t.Fatal("the page's <main> is never closed")
	}
	return inner
}

// formControl matches the elements a hand and a Tab key land on: the things you
// type into, choose from, or press. Anchors are deliberately absent — a link is
// interactive and the sub-navigation above the list is made of them, and a
// navigation bar preceding page content is not the trap this milestone is
// removing. What trapped the click was a *text box*.
var formControl = regexp.MustCompile(`<(input|select|textarea|button)\b[^>]*>`)

// namedControls lists the `name` of every form control in a stretch of markup,
// in order. A submit button carries none and is therefore not counted as one of
// the controls on the page, which is right: the <noscript> button is how the
// others are applied, not a seventh thing to look at.
var controlName = regexp.MustCompile(`\bname="([^"]+)"`)

func namedControls(html string) []string {
	var out []string
	for _, tag := range formControl.FindAllString(html, -1) {
		if m := controlName.FindStringSubmatch(tag); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// TestTheSearchBoxIsTheFirstControlOnTheLinksPage is M46's structural claim
// about the links list.
//
// Round one: the owner "accidentally clicked on the create a link destination
// URL box instead of the search box a few times". That is not a slip. The create
// form sat above the filter bar, so the first text input on a page whose subject
// is a list of things was the one that made a new thing — and a hand looking for
// the search box on a list page goes to the first box it sees.
//
// Asserted as *first*, rather than as "the create form is below the filters",
// because there are many ways to put something in front of it and only one way
// to be first.
func TestTheSearchBoxIsTheFirstControlOnTheLinksPage(t *testing.T) {
	page := mainOf(t, renderPage(t, "links", nil))

	first := formControl.FindString(page)
	if first == "" {
		t.Fatal("the links page renders no form control at all")
	}
	if !strings.Contains(first, `type="search"`) || !strings.Contains(first, `name="search"`) {
		t.Errorf("the first control on the links page is %s, want the search box; "+
			"a hand reaching for search on a list page lands on the first box it "+
			"sees, and this is the box it lands on", first)
	}

	// The same claim from the other end, and it is the one that regressed before:
	// the destination URL box must not be reachable ahead of the search box.
	search := strings.Index(page, `name="search"`)
	create := strings.Index(page, `id="url"`)
	if create >= 0 && create < search {
		t.Error("the create form's destination URL box comes before the search box " +
			"again, which is the exact trap this milestone removed")
	}
}

// TestTheFilterBarKeepsOneHotControlAndHidesTheRest is the owner's own
// prescription, asserted.
//
// "Only leave 1-2 hot controls on the page and add a filter control that
// displays all the filter controls including the hot ones." Six existed. Search
// is the one that stays out, because it is the only one any blind task reached
// for by name; the second hot slot is left empty rather than filled by guessing,
// and the prescription permits one.
//
// Search is not repeated inside the panel. Two elements named `search` in one
// form submit `?search=a&search=b` for a parameter this milestone promised not
// to change, and with the hot control sitting against the panel's own summary
// both are on screen at once regardless.
func TestTheFilterBarKeepsOneHotControlAndHidesTheRest(t *testing.T) {
	page := mainOf(t, renderPage(t, "links", nil))

	form := regexp.MustCompile(`(?s)<form method="get" action="/links".*?</form>`).FindString(page)
	if form == "" {
		t.Fatal("the links page has no filter form")
	}
	details := strings.Index(form, "<details")
	if details < 0 {
		t.Fatal("the links page has no filter disclosure, so all six controls are " +
			"still on it")
	}
	panel, _, ok := strings.Cut(form[details:], "</details>")
	if !ok {
		t.Fatal("the filter disclosure is never closed")
	}

	// **Hot means outside the panel, and the count is exact in that direction.**
	// Asserting only that the five are inside would pass a page that also kept a
	// copy of one of them out here, which is the shape of the complaint: six
	// controls on the page, now with a panel as well.
	hot := namedControls(form[:details])
	if len(hot) != 1 || hot[0] != "search" {
		t.Errorf("the controls outside the filter panel are %v, want [search]; the "+
			"owner asked for one or two hot controls, and search is the only one any "+
			"blind task reached for by name", hot)
	}

	// And the other five are inside. The fixture draws folders, campaigns and
	// hostnames, so all of them render on this page.
	for _, name := range []string{"status", "folder", "campaign", "domain", "sort"} {
		if !strings.Contains(panel, `name="`+name+`"`) {
			t.Errorf("the %q filter is not inside the disclosure; the owner asked "+
				"for one control that holds the rest, not for five that stayed out",
				name)
		}
	}
	// Nothing is in both places. Two elements sharing a name in one form submit
	// two values for one query parameter, and the parameters are unchanged.
	for _, name := range []string{"search", "status", "folder", "campaign", "domain", "sort"} {
		if n := strings.Count(form, `name="`+name+`"`); n != 1 {
			t.Errorf("the filter form carries %d controls named %q, want 1", n, name)
		}
	}

	// The <noscript> submit survives the reshuffle. Not because the dashboard
	// degrades without JavaScript — D103 answered that the other way — but
	// because it was on this page before a milestone about decluttering started
	// moving controls, and removing a capability is a decision to put on its own.
	at := strings.Index(form, "<noscript>")
	if at < 0 || !strings.Contains(form[at:], `type="submit"`) {
		t.Fatal("the no-JavaScript submit is gone from the links page")
	}
	if at > details {
		t.Error("the no-JavaScript submit is inside the filter disclosure; it " +
			"applies every filter and belongs where it can be reached without " +
			"opening one")
	}
}

// TestAHiddenFilterOpensTheFilterPanel is what makes demoting five controls
// honest.
//
// The risk of a filter panel is a list that is filtered for a reason nobody can
// see. `open` is an attribute the server writes, so the answer is in the first
// response rather than in a script, and the closed state is asserted as well —
// a panel that is always open has not hidden anything.
func TestAHiddenFilterOpensTheFilterPanel(t *testing.T) {
	openTag := regexp.MustCompile(`<details\b[^>]*>`)

	shut := openTag.FindString(mainOf(t, renderPage(t, "links", nil)))
	if shut == "" {
		t.Fatal("no filter disclosure on the links page")
	}
	if strings.Contains(shut, " open") {
		t.Errorf("the filter panel is open with nothing hidden set: %s", shut)
	}

	for _, tc := range []struct{ field, value string }{
		{"Status", "archived"},
		{"Folder", "none"},
		{"Campaign", "none"},
		{"Domain", "0198c9c5-0000-7000-8000-000000000051"},
		{"Sort", "clicks"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			got := openTag.FindString(mainOf(t, renderPage(t, "links",
				map[string]any{tc.field: tc.value})))
			if !strings.Contains(got, " open") {
				t.Errorf("%s=%q is set and the filter panel is shut: %s; a list that "+
					"is filtered for an invisible reason is worse than six controls",
					tc.field, tc.value, got)
			}
		})
	}
}

// TestCreatingALinkStaysOneActionAway is the bullet that stops this milestone
// fixing one complaint by causing another.
//
// The create form was demoted below the filter bar and folded into a disclosure.
// One action opens it, it is above the list rather than under it, and the
// handler's failure path re-renders this page with the typed values, the field
// errors and — after a low-confidence refusal — the appeal. Every one of those
// lives inside the panel, so a panel that stayed shut after a failure would show
// a page that had silently done nothing.
func TestCreatingALinkStaysOneActionAway(t *testing.T) {
	page := mainOf(t, renderPage(t, "links", map[string]any{
		"FieldErrors": map[string]string{}, "DisputeURL": "",
	}))

	if !strings.Contains(page, `action="/links"`) || !strings.Contains(page, `id="url"`) {
		t.Fatal("the links page no longer offers a way to create a link")
	}
	if !strings.Contains(page, "Create a link") {
		t.Error("the create control has no label saying what it opens")
	}
	// Above the list, not below it. Below a page of links is the definition of
	// buried, and "reachable" would be true of it anyway.
	if strings.Index(page, `id="url"`) > strings.Index(page, `id="links-table"`) {
		t.Error("creating a link sits below the list; demoting it must not mean " +
			"burying it")
	}

	// The failure path. Each of these is set on its own, because they arrive on
	// their own: a rejected alias carries no DisputeURL, and a refused
	// destination carries no alias error.
	create := regexp.MustCompile(`(?s)<details class="mb-6[^"]*"[^>]*>`)
	if got := create.FindString(page); strings.Contains(got, " open") {
		t.Errorf("the create panel is open on a page with no error on it: %s", got)
	}
	for _, tc := range []struct {
		name string
		with map[string]any
	}{
		{"a field error", map[string]any{"FieldErrors": map[string]string{"url": "bad"}}},
		{"an alias error", map[string]any{"FieldErrors": map[string]string{"alias": "taken"}}},
		{"a refused destination", map[string]any{"DisputeURL": "https://bit.ly/xyz"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			with := map[string]any{"FieldErrors": map[string]string{}, "DisputeURL": ""}
			for k, v := range tc.with {
				with[k] = v
			}
			got := create.FindString(mainOf(t, renderPage(t, "links", with)))
			if !strings.Contains(got, " open") {
				t.Errorf("the create panel is shut after %s: %s; the typed values, "+
					"the errors and the appeal are all inside it, so the reader is "+
					"shown a page that appears to have done nothing", tc.name, got)
			}
		})
	}
}

// TestTheLinksAreaHasItsOwnNavigation is where Campaigns and Folders went.
//
// They were two text links at the top right of the links page, which the owner
// had "not even noticed … existed until now". They are a bar in the chrome now,
// drawn on all four pages whose Nav is "links" rather than pasted into three
// templates, and absent everywhere else — a sub-navigation that renders on the
// dashboard is not a sub-navigation.
func TestTheLinksAreaHasItsOwnNavigation(t *testing.T) {
	for _, tc := range []struct{ page, current string }{
		{"links", "/links"},
		{"link_detail", "/links"},
		{"campaigns", "/campaigns"},
		{"folders", "/folders"},
	} {
		t.Run(tc.page, func(t *testing.T) {
			body := renderPage(t, tc.page, nil)
			bar := regexp.MustCompile(`(?s)<nav aria-label="Links".*?</nav>`).FindString(body)
			if bar == "" {
				t.Fatalf("%s draws no links-area navigation", tc.page)
			}
			for _, href := range []string{"/links", "/campaigns", "/folders"} {
				if !strings.Contains(bar, `href="`+href+`"`) {
					t.Errorf("the links bar does not offer %s", href)
				}
			}
			want := `<a href="` + tc.current + `" aria-current="page"`
			if !strings.Contains(bar, want) {
				t.Errorf("the links bar does not mark %s as the current page; "+
					"navigation that never says where you are is the complaint this "+
					"milestone is answering, one level down", tc.current)
			}
			// In the chrome, so the page's own first control is still the search
			// box. A bar rendered as page content would sit in front of it.
			if strings.Contains(mainOf(t, body), `<nav aria-label="Links"`) {
				t.Error("the links bar is inside <main>; it belongs to the shell, " +
					"and page content is where it would get in front of the search box")
			}
		})
	}

	// And nowhere else. The dashboard is not in the links area, and neither is an
	// account that belongs to nothing.
	for _, page := range []string{"dashboard", "keys", "members", "organization_new"} {
		if strings.Contains(renderPage(t, page, nil), `<nav aria-label="Links"`) {
			t.Errorf("%s draws the links-area navigation", page)
		}
	}
}
