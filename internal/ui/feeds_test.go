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
		"Title": "Reputation feeds and webhooks", "Nav": "feeds", "Identity": owner(),
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

// The four states this page has, which are two channels and not one.
//
// It had two before M45, because the disclosure knew only about the feed and the
// green panel promised on the instance's behalf what only the workspace's own
// registrations could answer for (F135). Every fixture below therefore carries
// both keys: a state assembled from one of them is the bug this page had.
func disclosure(feedOn, webhooks bool, count int) map[string]any {
	d := map[string]any{
		"Enabled": feedOn,
		"Webhooks": map[string]any{
			"Receiving": webhooks,
			"Count":     count,
		},
	}
	if feedOn {
		d["Name"] = "Example Reputation"
		d["Endpoint"] = "https://feed.example/v1/check"
		d["Method"] = "POST"
		d["TimeoutSeconds"] = 2.0
	}
	return d
}

// feedOn and feedOff keep their names and their meaning: the *feed's* two
// states, with the workspace channel silent. What changed is that neither of
// them is any longer the whole disclosure.
func feedOn() map[string]any  { return disclosure(true, false, 0) }
func feedOff() map[string]any { return disclosure(false, false, 0) }

// webhooksOnly is the state that made this milestone: no feed, and a workspace
// that has registered somewhere for its destinations to go.
func webhooksOnly() map[string]any { return disclosure(false, true, 1) }

// bothChannels is the fourth corner, which nothing rendered before.
func bothChannels() map[string]any { return disclosure(true, true, 2) }

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
		{"webhooks receiving", webhooksOnly()},
		{"both channels", bothChannels()},
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

// TestTheDisclosureAnswersTheSameQuestionInEveryState.
//
// m32.md's Risks section says the wording is the deliverable as much as the code
// is, so the wording is asserted. The four things D40 requires when a feed is on
// — which third party, what is sent, when, and who can change it — and the one
// thing that matters when it is off, which is that the page still answers rather
// than rendering nothing.
//
// **Four states rather than two, since M45.** The subtests below changed with the
// page (F135), and each still asserts what its predecessor asserted:
//
//   - "feed on, no webhooks" is the old "configured" case verbatim. Nothing about
//     the feed's six statements moved.
//   - "neither channel" is the old "not configured" case, and the string it pins
//     changed from "No destination leaves this instance" to "Nothing you point a
//     link at leaves this instance". The claim being pinned is the same claim —
//     the page still refuses to answer by rendering nothing — and the old wording
//     could not be kept, because it was the false one: it promised on the whole
//     instance's behalf from a reading of the feed setting alone.
//   - "webhooks only" and "both channels" are new, and they are the states the
//     old fixture could not express.
func TestTheDisclosureAnswersTheSameQuestionInEveryState(t *testing.T) {
	t.Run("feed on, no webhooks", func(t *testing.T) {
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
		// The feed being on says nothing about the workspace's own channel, and
		// the page must still answer it rather than leaving the reader to assume.
		if !strings.Contains(content, "registered none that would receive a destination") {
			t.Error("with a feed on and no webhooks the page does not say the workspace " +
				"registered none; a page that answers one channel and goes quiet on the " +
				"other is how this one shipped wrong")
		}
	})

	t.Run("neither channel", func(t *testing.T) {
		content := feedsContent(t, feedOff())
		if !strings.Contains(content, "Nothing you point a link at leaves this instance") {
			t.Error("with no feed and no webhook the page must say so plainly; an empty " +
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

	t.Run("webhooks only", func(t *testing.T) {
		content := feedsContent(t, webhooksOnly())
		// The strong claim must be gone. This is the whole of F135: a workspace
		// with a webhook was being told, in a green panel, that nothing leaves.
		for _, gone := range []string{
			"Nothing you point a link at leaves this instance",
			"No destination leaves this instance",
			"Nothing you point a link at is sent anywhere",
		} {
			if strings.Contains(content, gone) {
				t.Errorf("a workspace with a webhook registered is still told %q", gone)
			}
		}
		if !strings.Contains(content, "Destinations you type here are sent somewhere else") {
			t.Error("the banner does not warn at all with a webhook receiving destinations")
		}
		// Scoped to the workspace, and said so. The instance-wide reading is the
		// error this page made in the other direction, and it must not be made
		// again about somebody else's workspace.
		if !strings.Contains(content, "this workspace") {
			t.Error("the webhook warning does not scope itself to the workspace; a claim " +
				"about the instance from a per-workspace read is the same defect inverted")
		}
		// No feed is configured, and the page must not let a warning about one
		// channel imply the other.
		if !strings.Contains(content, "None is configured") {
			t.Error("the feed section does not state that no feed is configured")
		}
		for _, leaked := range []string{"Example Reputation", "feed.example"} {
			if strings.Contains(content, leaked) {
				t.Errorf("a warning about webhooks mentions %q; no feed is configured "+
					"in this state", leaked)
			}
		}
	})

	t.Run("both channels", func(t *testing.T) {
		content := feedsContent(t, bothChannels())
		for what, want := range map[string]string{
			"the third party":       "Example Reputation",
			"the webhook count":     "2",
			"the operator's leg":    "Only the operator",
			"the workspace's leg":   "An owner or an administrator of this workspace",
			"that something leaves": "Destinations you type here are sent somewhere else",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("with both channels live the page does not state %s (looked for %q)",
					what, want)
			}
		}
	})
}

// TestTheDisclosureNamesNoWebhookAddress.
//
// The count is owed to every member; the registry is not. `webhooks.read` is
// what gates who a workspace posts to, and this page is gated on nothing, so a
// URL rendered here would widen that permission by way of a disclosure page —
// the exact shape of accident D40's no-controls test exists to prevent one turn
// earlier. link.WebhookDisclosure carries no URL, and this asserts that the
// template did not go and find one another way.
func TestTheDisclosureNamesNoWebhookAddress(t *testing.T) {
	d := disclosure(false, true, 1)
	// A field the real type does not have. If the template ever reads one, this
	// is what it would find.
	hooks, ok := d["Webhooks"].(map[string]any)
	if !ok {
		t.Fatal("the webhook half of the fixture is no longer a map; this test cannot " +
			"plant an address in it")
	}
	hooks["URL"] = "https://receiver.example/hook"
	content := feedsContent(t, d)
	if strings.Contains(content, "receiver.example") {
		t.Error("the disclosure page rendered a webhook's address. Who this workspace " +
			"posts to is behind webhooks.read; this page is behind nothing")
	}
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
