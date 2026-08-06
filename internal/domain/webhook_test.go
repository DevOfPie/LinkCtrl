package domain

import "testing"

// TestEveryWebhookEventIsClassifiedForDisclosure.
//
// The `/feeds` disclosure tells a workspace whether anything it registered
// receives the destinations it submits, and it decides that by asking the
// database which of *these* events the workspace subscribed to (M45, F135). The
// split is therefore load-bearing in a way a vocabulary list is not: an event
// left out of both halves reads as "carries no destination", so the page would
// go on saying nothing leaves for a webhook that receives every URL somebody
// types.
//
// This is the test that makes adding an eighth event a decision. It fails on an
// event in neither list, on one in both, and on a name in a list that is not in
// the vocabulary at all.
func TestEveryWebhookEventIsClassifiedForDisclosure(t *testing.T) {
	carries := map[string]bool{}
	for _, e := range WebhookDestinationEvents {
		if !IsWebhookEvent(e) {
			t.Errorf("%q is listed as carrying a destination but is not a webhook event; "+
				"the disclosure would ask the database about a name nothing can subscribe to", e)
		}
		carries[e] = true
	}
	for _, e := range webhookEventsWithoutDestination {
		if !IsWebhookEvent(e) {
			t.Errorf("%q is listed as carrying no destination but is not a webhook event", e)
		}
		if carries[e] {
			t.Errorf("%q is in both halves of the classification; the disclosure cannot "+
				"be built from a split that contradicts itself", e)
		}
		carries[e] = false
	}
	for _, e := range WebhookEvents {
		if _, classified := carries[e]; !classified {
			t.Errorf("webhook event %q is in neither WebhookDestinationEvents nor "+
				"webhookEventsWithoutDestination. Classify it: an unclassified event "+
				"defaults to carrying nothing, so /feeds would tell a workspace "+
				"subscribed to it that no destination leaves — see M45's F135 entry "+
				"in decisions.md", e)
		}
	}
	if got, want := len(WebhookDestinationEvents)+len(webhookEventsWithoutDestination),
		len(WebhookEvents); got != want {
		t.Errorf("the two halves hold %d names for a vocabulary of %d; a duplicate in "+
			"either list is a classification nothing checks", got, want)
	}
}

// TestCarriesDestinationMatchesTheClassification asserts the predicate the query
// and the page both go through, rather than trusting that it reads the list it
// is built from.
func TestCarriesDestinationMatchesTheClassification(t *testing.T) {
	for _, e := range WebhookDestinationEvents {
		if !CarriesDestination(e) {
			t.Errorf("CarriesDestination(%q) = false for an event that carries one", e)
		}
	}
	for _, e := range webhookEventsWithoutDestination {
		if CarriesDestination(e) {
			t.Errorf("CarriesDestination(%q) = true for an event that carries none; the "+
				"disclosure would warn a workspace about egress it does not have", e)
		}
	}
	if CarriesDestination("link.exploded") {
		t.Error("CarriesDestination answered true for a name outside the vocabulary")
	}
}
