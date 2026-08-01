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
	{"evil[.]example", "evil.example"},
	{"https[:]//evil[.]example/promo%3Cscript%3E", "https://evil.example/promo"},
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
