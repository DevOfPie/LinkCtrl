package domain_test

import (
	"slices"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// TestNoAutomationActionWritesATriggerSource is the structural half of "a rule
// cannot trigger itself or loop".
//
// m43.md names the failure as *an automation that fires a webhook that fires an
// automation*. The runtime guard against a rule re-firing on its own subject is
// the watermark, and an integration test holds that. This one holds the other
// half, which no amount of watermarking would catch: rule A's action producing
// the thing rule B triggers on, and rule B's action producing the thing rule A
// triggers on. Two rules, each firing once per subject, feeding each other
// forever.
//
// The assertion is that the cycle **cannot be drawn**, because the set of things
// actions write and the set of things triggers read do not intersect. That is a
// property of the vocabulary rather than of any rule, so it holds for every
// wiring of every rule anybody can write.
//
// **This test is the reason the webhook action cannot choose its event.** Letting
// a rule emit `destination.blocked` would put SourceBlockedAudit in
// ActionWrites, this would go red, and the cycle would be one form submission
// away. Anybody who widens the action vocabulary meets this test before they
// meet the outage.
func TestNoAutomationActionWritesATriggerSource(t *testing.T) {
	read := map[domain.AutomationSource][]string{}
	for trigger, sources := range domain.TriggerReads {
		for _, s := range sources {
			read[s] = append(read[s], trigger)
		}
	}

	for action, sources := range domain.ActionWrites {
		for _, s := range sources {
			if triggers, clash := read[s]; clash {
				t.Errorf("action %q writes %q, which triggers %v read. An automation "+
					"can now feed an automation: a rule taking this action produces "+
					"exactly what another rule watches for, and the pair loops. "+
					"Either the action must not produce it, or the trigger must not "+
					"read it — the watermark does not help here, because each firing "+
					"is a genuinely new subject.",
					action, s, triggers)
			}
		}
	}
}

// TestEveryTriggerAndActionIsDeclared stops the map above from being an
// incomplete picture of the vocabulary.
//
// A trigger with no TriggerReads row, or an action with no ActionWrites row,
// would pass the disjointness test by not being in it — which is the one way
// that test could quietly stop meaning anything.
func TestEveryTriggerAndActionIsDeclared(t *testing.T) {
	for _, trigger := range domain.AutomationTriggers {
		if len(domain.TriggerReads[trigger]) == 0 {
			t.Errorf("trigger %q declares nothing in TriggerReads, so the "+
				"disjointness assertion does not cover it", trigger)
		}
	}
	for _, action := range domain.AutomationActions {
		if len(domain.ActionWrites[action]) == 0 {
			t.Errorf("action %q declares nothing in ActionWrites, so the "+
				"disjointness assertion does not cover it", action)
		}
	}
	if len(domain.TriggerReads) != len(domain.AutomationTriggers) {
		t.Errorf("TriggerReads has %d rows for %d triggers; a row for a name that is "+
			"not in the vocabulary is a declaration nothing enforces",
			len(domain.TriggerReads), len(domain.AutomationTriggers))
	}
	if len(domain.ActionWrites) != len(domain.AutomationActions) {
		t.Errorf("ActionWrites has %d rows for %d actions; a row for a name that is "+
			"not in the vocabulary is a declaration nothing enforces",
			len(domain.ActionWrites), len(domain.AutomationActions))
	}
}

// TestAutomationFiredIsNotATriggerSource is the same claim from the other end,
// and it is worth stating separately because it is the one somebody will be
// tempted to break.
//
// "Trigger when an automation fires" is a natural feature request, and it is
// exactly the cycle m43.md names. The event exists and nothing reads it.
func TestAutomationFiredIsNotATriggerSource(t *testing.T) {
	if domain.IsAutomationTrigger(domain.EventAutomationFired) {
		t.Fatalf("%q is now an automation trigger, and the webhook action emits it: "+
			"a rule with that trigger and that action fires itself forever",
			domain.EventAutomationFired)
	}
	if !domain.IsWebhookEvent(domain.EventAutomationFired) {
		t.Fatalf("%q is not in the webhook vocabulary, so the webhook action emits "+
			"an event internal/webhook refuses and the action silently does nothing",
			domain.EventAutomationFired)
	}
}

// TestTheBlockedVocabularyIsOneWord pins three strings in three packages that
// name one event.
//
// The blocked-attempt trigger matches audit rows by action, and it does it by
// passing the *trigger* name as the action. That works because the trigger, the
// audit action and the webhook event are the same string — and if any of the
// three were renamed on its own the rule would match nothing, silently, forever.
func TestTheBlockedVocabularyIsOneWord(t *testing.T) {
	if domain.TriggerDestinationBlocked != audit.ActionDestinationBlocked {
		t.Errorf("trigger %q and audit action %q have drifted; the blocked-attempt "+
			"trigger queries the audit log by action and would match nothing",
			domain.TriggerDestinationBlocked, audit.ActionDestinationBlocked)
	}
	if domain.TriggerDestinationBlocked != domain.EventDestinationBlocked {
		t.Errorf("trigger %q and webhook event %q have drifted",
			domain.TriggerDestinationBlocked, domain.EventDestinationBlocked)
	}
}

// TestAThresholdIsAlwaysReachable holds the two bounds together.
//
// `min_count` is how many subjects must accumulate before a rule fires, and a
// run sees at most AutomationMatchesPerRule of them. A threshold above that cap
// would be a rule that validates, saves, displays, and never fires — the failure
// a closed vocabulary exists to prevent rather than to cause.
func TestAThresholdIsAlwaysReachable(t *testing.T) {
	if domain.MaxAutomationMinCount > domain.AutomationMatchesPerRule {
		t.Fatalf("min_count may be as high as %d but one run matches at most %d "+
			"subjects, so a rule at the maximum threshold can never fire",
			domain.MaxAutomationMinCount, domain.AutomationMatchesPerRule)
	}
}

func TestValidateAutomationActionsRefusesArchiveWithoutALink(t *testing.T) {
	_, errs := domain.ValidateAutomationActions(
		domain.TriggerDestinationBlocked,
		[]string{domain.ActionNotify, domain.ActionArchiveLink})
	if errs == nil {
		t.Fatal("archive_link was accepted on destination.blocked, which has no link " +
			"to archive; the rule would save and then do nothing")
	}
	if errs[0].Code != "no_link_subject" {
		t.Errorf("refusal code is %q, want no_link_subject", errs[0].Code)
	}

	// And the other direction: it is legal on both link-subject triggers.
	for _, trigger := range []string{domain.TriggerLinkExpired, domain.TriggerLinkMaxClicks} {
		if _, errs := domain.ValidateAutomationActions(trigger,
			[]string{domain.ActionArchiveLink}); errs != nil {
			t.Errorf("archive_link refused on %s: %v", trigger, errs)
		}
	}
}

func TestValidateAutomationActionsCanonicalizes(t *testing.T) {
	got, errs := domain.ValidateAutomationActions(domain.TriggerLinkExpired,
		[]string{domain.ActionWebhook, domain.ActionNotify, domain.ActionNotify})
	if errs != nil {
		t.Fatalf("unexpected refusal: %v", errs)
	}
	want := []string{domain.ActionNotify, domain.ActionWebhook}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v — the stored list must be deduplicated and in "+
			"vocabulary order, or two rules that mean the same thing do not compare equal",
			got, want)
	}
}

func TestValidateAutomationActionsRefusesTheUnknown(t *testing.T) {
	if _, errs := domain.ValidateAutomationActions(domain.TriggerLinkExpired,
		[]string{"delete_link"}); errs == nil {
		t.Fatal("an action outside the vocabulary was accepted; the rule would save " +
			"and the scheduler would ignore it")
	}
	if _, errs := domain.ValidateAutomationActions(domain.TriggerLinkExpired, nil); errs == nil {
		t.Fatal("a rule with no actions was accepted")
	}
}

func TestValidateAutomationTriggerConfigBoundsTheThreshold(t *testing.T) {
	if errs := domain.ValidateAutomationTriggerConfig(
		domain.AutomationTriggerConfig{MinCount: -1}); errs == nil {
		t.Error("a negative min_count was accepted")
	}
	if errs := domain.ValidateAutomationTriggerConfig(
		domain.AutomationTriggerConfig{MinCount: domain.MaxAutomationMinCount + 1}); errs == nil {
		t.Error("a min_count above the per-run match cap was accepted, which is a rule " +
			"that can never fire")
	}
	if got := (domain.AutomationTriggerConfig{}).Threshold(); got != 1 {
		t.Errorf("an omitted min_count is %d, want 1 — the zero value has to be a "+
			"legal config", got)
	}
}
