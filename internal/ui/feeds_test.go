package ui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// renderFeeds draws the disclosure in one of its two states and returns the
// HTML, minus the layout's own chrome.
//
// The chrome has to go. The layout carries a sign-out form, the appearance
// control and the workspace switcher on every page, so a naive "no <form> here"
// assertion against a whole render would fail on markup that has nothing to do
// with this page — and the one that matters is whether the *content* block grew
// a control.
func renderFeeds(t *testing.T, disclosure map[string]any) string {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	err = r.Render(rec, http.StatusOK, "feeds", map[string]any{
		"Title": "Reputation feeds", "Nav": "feeds", "Identity": owner(),
		"HasOrganization": true,
		// Two of them, so the switcher renders and its <form> is in the chrome
		// this test has to exclude — a fixture with one workspace would leave
		// the exclusion untested and the assertion weaker than it looks.
		"Workspaces": twoWorkspaces(),
		"Disclosure": disclosure,
	})
	if err != nil {
		t.Fatalf("render feeds: %v", err)
	}
	return rec.Body.String()
}

// mainRegion is the layout's content area, which is where this page's markup
// ends up. Matched rather than assumed: if the layout stops using <main>, this
// fails loudly instead of quietly asserting nothing.
var mainRegion = regexp.MustCompile(`(?s)<main[^>]*>(.*?)</main>`)

func feedsContent(t *testing.T, disclosure map[string]any) string {
	t.Helper()
	page := renderFeeds(t, disclosure)
	m := mainRegion.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("the layout no longer wraps page content in <main>; this test cannot " +
			"tell page markup from chrome any more")
	}
	return m[1]
}

func feedOn() map[string]any {
	return map[string]any{
		"Enabled": true, "Name": "Example Reputation",
		"Endpoint": "https://feed.example/v1/check",
		"Method":   "POST", "TimeoutSeconds": 2.0,
	}
}

func feedOff() map[string]any { return map[string]any{"Enabled": false} }

// TestTheDisclosurePageHasNoControls is half of D40's mechanism. The other half
// — that no unsafe method is accepted — is asserted against the live router in
// test/integration/feed_test.go, because a template test cannot see a route.
//
// Why a test rather than a convention: D38 removed the ability to change
// instance-wide settings from the dashboard, on the finding that this product
// has no instance-level principal who could be trusted with one. This page is
// read-only and therefore does not reverse that. The risk is the row somebody
// adds here next year with a toggle beside it, at which point D38 has been
// reversed by nobody in particular. This is what makes that an explicit act.
func TestTheDisclosurePageHasNoControls(t *testing.T) {
	// Every element that submits, and the two attributes that make something
	// else submit. hx-post is in the list because this codebase uses htmx: a
	// control here would not have to be a <form> to be a control.
	forbidden := []string{
		"<form", "<button", "<input", "<select", "<textarea",
		"formaction", "hx-post", "hx-put", "hx-patch", "hx-delete",
	}
	for _, state := range []struct {
		name string
		d    map[string]any
	}{
		{"feed configured", feedOn()},
		{"no feed configured", feedOff()},
	} {
		t.Run(state.name, func(t *testing.T) {
			content := strings.ToLower(feedsContent(t, state.d))
			for _, bad := range forbidden {
				if strings.Contains(content, bad) {
					t.Errorf("the disclosure page contains %q. It is read-only by "+
						"decision D40: reading what an instance does with your "+
						"destinations needs no principal, changing it does, and this "+
						"product has none. A control here reverses D38 by accident.", bad)
				}
			}
		})
	}
}

// TestTheDisclosureAnswersTheSameQuestionInBothStates.
//
// m32.md's Risks section says the wording is the deliverable as much as the code
// is, so the wording is asserted. The four things D40 requires when a feed is on
// — which third party, what is sent, when, and who can change it — and the one
// thing that matters when it is off, which is that the page still answers rather
// than rendering nothing.
func TestTheDisclosureAnswersTheSameQuestionInBothStates(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		content := feedsContent(t, feedOn())
		for what, want := range map[string]string{
			"which third party":            "Example Reputation",
			"where it goes":                "https://feed.example/v1/check",
			"that something is sent to it": "sent",
			"what is sent":                 "The destination URL, and nothing else",
			"when it is sent":              "creating a link",
			"who can change it":            "Only the operator",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("the page does not state %s (looked for %q)", what, want)
			}
		}
	})

	t.Run("not configured", func(t *testing.T) {
		content := feedsContent(t, feedOff())
		if !strings.Contains(content, "No destination leaves this instance") {
			t.Error("with no feed configured the page must say so plainly; an empty " +
				"page answers the question by not answering it")
		}
		// The default state must not name a third party, or a reader who scans
		// for a name finds one and concludes their destinations are being sent.
		for _, leaked := range []string{"Example Reputation", "feed.example"} {
			if strings.Contains(content, leaked) {
				t.Errorf("the off state mentions %q", leaked)
			}
		}
	})
}

// TestTheDisclosureNeverLinksToTheFeed.
//
// The endpoint is a third party's address, printed on a page whose subject is
// not sending people places without telling them. Making it clickable would be
// that failure one layer up, and it is the obvious "improvement" somebody makes
// while tidying.
func TestTheDisclosureNeverLinksToTheFeed(t *testing.T) {
	content := feedsContent(t, feedOn())
	for _, line := range strings.Split(content, "\n") {
		if !strings.Contains(line, "feed.example") {
			continue
		}
		for _, attr := range []string{"href", "src", "action", "background"} {
			if strings.Contains(strings.ToLower(line), attr+"=") {
				t.Errorf("the feed endpoint appears in a %s attribute: %s", attr, strings.TrimSpace(line))
			}
		}
	}
}
