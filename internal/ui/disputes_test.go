package ui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The review queue is the one page in this product whose content an attacker
// chooses. Somebody who wants an instance owner to visit a URL only has to be
// refused once and then ask for a review, and the page renders what they typed.
//
// Two properties keep that from being a delivery mechanism, and both are
// asserted here against the rendered HTML rather than against the template
// source, because what matters is what a browser receives.

// urlBearing matches an attribute a browser resolves. Everything that fetches or
// navigates, not just href: an <img src> loaded from the owner's machine is the
// same disclosure as a click, and it needs no click.
var urlBearing = regexp.MustCompile(
	`(?i)\b(href|src|srcset|action|formaction|data|poster|cite|ping|background|` +
		`hx-get|hx-post|hx-put|hx-patch|hx-delete)\s*=\s*"([^"]*)"`)

// anchors matches a whole <a> element, attributes and text.
var anchors = regexp.MustCompile(`(?is)<a\b[^>]*>.*?</a>`)

// renderDisputes renders the queue page from the shared fixture.
func renderDisputes(t *testing.T) string {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	if err := r.Render(rec, http.StatusOK, "disputes", pageData(t)["disputes"]); err != nil {
		t.Fatalf("render disputes: %v", err)
	}
	return rec.Body.String()
}

// disputedStrings are the defanged values the fixture puts on the page, and the
// live forms those defanged values stand for.
//
// Both halves matter. The defanged form has to be present, or every assertion
// below passes because the page is empty. The live form has to be absent, or the
// page re-fanged what the service made inert.
var disputedStrings = []struct{ defanged, live string }{
	{"login[.]evil[.]example", "login.evil.example"},
	{"evil[.]example", "evil.example"},
	{"https[:]//login[.]evil[.]example/promo%3Cscript%3E", "https://login.evil.example/promo"},
	{"xn--80ak6aa92e[.]com", "xn--80ak6aa92e.com"},
	{"bit[.]ly", "bit.ly"},
	{"phish[.]example", "phish.example"},
}

// TestTheQueueRendersEveryDisputedDestinationDefanged is the first half.
func TestTheQueueRendersEveryDisputedDestinationDefanged(t *testing.T) {
	body := renderDisputes(t)

	for _, d := range disputedStrings {
		if !strings.Contains(body, d.defanged) {
			t.Errorf("the page does not render %q. Every assertion in this file "+
				"depends on the destination actually reaching the HTML; if the "+
				"fixture stopped supplying it, the rest of these tests are vacuous.",
				d.defanged)
		}
		if strings.Contains(body, d.live) {
			t.Errorf("the page contains the live form %q. The service defangs on the "+
				"way in and on the way out; a template that re-assembles the "+
				"original has undone the one transformation that makes this page "+
				"safe to read.", d.live)
		}
	}

	// The property behind the two markers: nothing on this page looks like a
	// URL a client would auto-link, whatever the fixture happens to hold.
	if strings.Contains(body, "://") {
		t.Error(`the rendered queue contains "://". Defanging exists so that no ` +
			`consumer — a browser, a terminal, a mail client pasted into — can ` +
			`turn a disputed destination back into something followable.`)
	}
}

// TestTheQueueNeverRendersADisputedDestinationAsALink is the second half, and
// the milestone's own words: never as an anchor element.
//
// Checked twice over, because the two failures are different. A destination
// inside a URL-bearing attribute is followable whether or not it is an anchor —
// an <img src> fetches with no interaction at all. A destination inside an
// anchor is followable even when the href is something else, because the visible
// text is what a reader trusts.
func TestTheQueueNeverRendersADisputedDestinationAsALink(t *testing.T) {
	body := renderDisputes(t)

	for _, m := range urlBearing.FindAllStringSubmatch(body, -1) {
		attr, value := m[1], m[2]
		for _, d := range disputedStrings {
			if strings.Contains(value, d.defanged) || strings.Contains(value, d.live) {
				t.Errorf("%s=%q carries a disputed destination. Nothing a browser "+
					"resolves may hold one: this page hands an instance owner a URL "+
					"a stranger chose.", attr, value)
			}
		}
		// Every URL on this page is a local literal the template wrote. Anything
		// else is a value that came from a row, which is how the check above
		// gets bypassed by a shape nobody predicted.
		if !isLocal(value) {
			t.Errorf("%s=%q is not a local path. Every URL the queue emits is a "+
				"literal in the template — a decision endpoint, the paging link, "+
				"or an asset.", attr, value)
		}
	}

	for _, a := range anchors.FindAllString(body, -1) {
		for _, d := range disputedStrings {
			if strings.Contains(a, d.defanged) || strings.Contains(a, d.live) {
				t.Errorf("a disputed destination appears inside an anchor:\n  %s\n"+
					"Rendering it as a link — even with a harmless href — invites the "+
					"one click this whole feature exists to make deliberate.", a)
			}
		}
	}
}

// disputeCards splits the rendered queue into one string per dispute.
//
// Crude on purpose: the assertion below is that a *particular* card carries a
// particular pair of values, and a whole-page Contains would pass on a page that
// printed the entry against the wrong dispute.
//
// Split on the card's own id rather than on its class list, which is what it
// used to be. M48 gave each card `id="dispute-{{.ID}}"` — the anchor a
// `dispute.filed` notification lands on — and moved the classes along behind it,
// which silently turned this splitter into one that found nothing. The id is the
// more honest seam anyway: a card is identified by the dispute it is about, and
// a class list is styling that may change again.
func disputeCards(body string) []string {
	parts := strings.Split(body, `<li id="dispute-`)
	if len(parts) < 2 {
		return nil
	}
	return parts[1:]
}

// cardFor returns the card carrying a dispute's decision endpoints.
func cardFor(t *testing.T, body, id string) string {
	t.Helper()
	for _, c := range disputeCards(body) {
		if strings.Contains(c, "/disputes/"+id+"/") {
			return c
		}
	}
	t.Fatalf("no card on the page belongs to dispute %s", id)
	return ""
}

// TestTheQueueNamesTheEntryAllowWouldRemove is the rendering half of F33.
//
// The runtime list matches on label boundaries, so somebody refused at
// login.evil.example was refused by the row that says evil.example — and that
// row, which every workspace on the instance is refused by, is what Allow
// deletes. Until M45 the page rendered the typed host and nothing else, so the
// string the owner read was not the string the button acted on.
//
// Asserted per card rather than per page: the entry appearing *somewhere* in the
// queue is not the claim. The claim is that the dispute offering Allow says what
// Allow removes.
func TestTheQueueNamesTheEntryAllowWouldRemove(t *testing.T) {
	body := renderDisputes(t)

	// The fixture's first item is the one whose entry is a parent of its host.
	card := cardFor(t, body, "0198c9c5-0000-7000-8000-000000000030")
	if !strings.Contains(card, "/allow") {
		t.Fatal("the fixture's liftable dispute lost its Allow control; the rest of " +
			"this test would pass on a card with no decision to describe")
	}
	if !strings.Contains(card, "login[.]evil[.]example") {
		t.Error("the card does not render the host that was typed")
	}
	// `>evil[.]example</code>` and not a bare Contains: the typed host ends with
	// the entry, so any looser match is satisfied by the value this test exists
	// to say is not sufficient.
	if !strings.Contains(card, "Blocklist entry") ||
		!strings.Contains(card, ">evil[.]example</code>") {
		t.Error("the card offers Allow without naming the blocklist entry Allow " +
			"deletes. The entry is a parent of the host above it and is removed " +
			"for every workspace on the instance; a queue that shows only the " +
			"typed host asks the owner to approve a decision it has not described.")
	}

	// And a refusal with no row behind it must not claim one. The heuristic card
	// draws no Allow at all, so an "entry" there would be a value describing a
	// deletion that cannot happen.
	computed := cardFor(t, body, "0198c9c5-0000-7000-8000-000000000031")
	if strings.Contains(computed, "Blocklist entry") {
		t.Error("a dispute whose rule is computed from the URL names a blocklist " +
			"entry; there is no row behind it and none anybody may add")
	}
}

// isLocal reports whether an attribute value is a path this application serves.
func isLocal(v string) bool {
	switch {
	case v == "", strings.HasPrefix(v, "#"):
		return true
	case strings.HasPrefix(v, "//"):
		// Protocol-relative: a different origin wearing a local-looking prefix.
		return false
	default:
		return strings.HasPrefix(v, "/")
	}
}

// TestTheQueueOffersAllowOnlyWhereItCouldWork.
//
// An allow deletes a row from the low-confidence blocklist. A punycode homograph
// is computed from the destination every time it is judged, so there is no row —
// and 01500 has no allow column, deliberately, so none can be added. Drawing the
// button anyway would offer the owner a decision that can only fail.
func TestTheQueueOffersAllowOnlyWhereItCouldWork(t *testing.T) {
	body := renderDisputes(t)

	// The fixture's second item is the un-liftable one; its id is what tells the
	// two open disputes apart in the markup.
	const liftable = "0198c9c5-0000-7000-8000-000000000030"
	const computed = "0198c9c5-0000-7000-8000-000000000031"

	if !strings.Contains(body, "/disputes/"+liftable+"/allow") {
		t.Error("a liftable dispute has no Allow control")
	}
	if strings.Contains(body, "/disputes/"+computed+"/allow") {
		t.Error("a dispute whose rule is computed from the URL offers Allow, which " +
			"has no row to delete and would refuse")
	}
	// Uphold is always available: "the owner looked and said no" has to be
	// recordable for every open dispute, or the queue cannot be emptied.
	for _, id := range []string{liftable, computed} {
		if !strings.Contains(body, "/disputes/"+id+"/uphold") {
			t.Errorf("dispute %s cannot be upheld", id)
		}
	}
}
