package domain

import "testing"

// TestBlocksBotsCoversEveryCombination is the whole precedence rule, and the
// reason there is one table rather than assertions scattered across the redirect
// handler and the link service.
//
// Nine cells, named individually. A loop over two slices would produce the same
// coverage and would not say what any cell means — and the interesting cells are
// exactly the ones whose meaning is arguable: an enforcing domain overruling a
// link that says off, and an inheriting link under a domain that blocks.
func TestBlocksBotsCoversEveryCombination(t *testing.T) {
	tests := []struct {
		name string
		link BotPolicy
		dom  DomainBotPolicy
		want bool
		why  string
	}{
		{
			"inherit under a domain that blocks nothing", BotInherit, DomainBotsOff, false,
			"the default state of every link on a fresh instance; anything else here " +
				"would mean installing LinkCtrl turned blocking on for everybody",
		},
		{
			"inherit under a blocking domain", BotInherit, DomainBotsOn, true,
			"what a domain-level switch is for: it reaches links whose owners have " +
				"not decided",
		},
		{
			"inherit under an enforcing domain", BotInherit, DomainBotsEnforced, true,
			"enforcement is stricter than on, never looser",
		},
		{
			"link blocks, domain does not", BotBlock, DomainBotsOff, true,
			"a link may block on its own; the domain setting is a default, not a ceiling",
		},
		{"link blocks, domain blocks", BotBlock, DomainBotsOn, true, "agreement"},
		{"link blocks, domain enforces", BotBlock, DomainBotsEnforced, true, "agreement"},
		{
			"link allows, domain blocks nothing", BotAllow, DomainBotsOff, false,
			"agreement, and the only cell where an explicit off is redundant",
		},
		{
			"link allows, domain blocks but does not enforce", BotAllow, DomainBotsOn, false,
			"the point of a domain default: a link owner may opt out of it",
		},
		{
			"link allows, domain enforces", BotAllow, DomainBotsEnforced, true,
			"the whole reason enforcement exists. It must hold for rows stored " +
				"before enforcement was switched on, which is why the override is here " +
				"and not only in the validation that refuses new ones",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := BlocksBots(tc.link, tc.dom); got != tc.want {
				t.Errorf("BlocksBots(%q, %q) = %v, want %v — %s",
					tc.link, tc.dom, got, tc.want, tc.why)
			}
		})
	}
}

// TestBlocksBotsReadsUnknownValuesAsTheSafeDefault covers the two inputs no
// caller writes on purpose.
//
// A cached snapshot from a build that predates this feature decodes with both
// fields empty, and a rolling upgrade means one replica is serving from another
// replica's payloads. Reading an absent value as "block" would refuse traffic on
// the strength of a field this build cannot interpret, which is a worse answer
// than the behaviour that build already had.
func TestBlocksBotsReadsUnknownValuesAsTheSafeDefault(t *testing.T) {
	if BlocksBots("", "") {
		t.Error("an empty pair blocked; a snapshot written before this feature " +
			"existed must behave exactly as it did then")
	}
	if BlocksBots("something-newer", DomainBotsOff) {
		t.Error("an unreadable link policy blocked under a domain that blocks nothing")
	}
	if !BlocksBots("", DomainBotsEnforced) {
		t.Error("an enforcing domain did not reach a link whose policy is absent; " +
			"enforcement that a missing field can escape is not enforcement")
	}
}

// TestDomainBotsRoundTrips pins the fold and its inverse together. They are used
// on opposite sides of the same form — the handler reads two checkboxes, the
// precedence rule takes one value — and a disagreement between them would show
// up as a setting that will not stay switched on.
func TestDomainBotsRoundTrips(t *testing.T) {
	for _, want := range []DomainBotPolicy{DomainBotsOff, DomainBotsOn, DomainBotsEnforced} {
		block, enforced := want.Booleans()
		if got := DomainBots(block, enforced); got != want {
			t.Errorf("%q folded to (%v, %v) and back to %q", want, block, enforced, got)
		}
	}

	// The combination migration 01800's CHECK refuses. It cannot be stored, so
	// the only way it reaches this function is a bug — and reading it as "off"
	// is the answer that does not invent an enforcement nobody configured.
	if got := DomainBots(false, true); got != DomainBotsOff {
		t.Errorf("DomainBots(false, true) = %q, want %q: enforcement without "+
			"blocking is not a state, and guessing that it means 'enforce' would "+
			"turn a corrupt row into a site-wide refusal", got, DomainBotsOff)
	}
}

func TestParseBotPolicy(t *testing.T) {
	tests := []struct {
		in    string
		want  BotPolicy
		valid bool
	}{
		{"inherit", BotInherit, true},
		{"on", BotBlock, true},
		{"off", BotAllow, true},
		// Absent and explicit-inherit mean the same thing, so a client that omits
		// the field and one that sends "" are not told different stories.
		{"", BotInherit, true},
		{"true", BotInherit, false},
		{"block", BotInherit, false},
		{"ON", BotInherit, false},
	}
	for _, tc := range tests {
		got, ok := ParseBotPolicy(tc.in)
		if ok != tc.valid || got != tc.want {
			t.Errorf("ParseBotPolicy(%q) = (%q, %v), want (%q, %v)",
				tc.in, got, ok, tc.want, tc.valid)
		}
	}
}

func TestBotPolicyLocked(t *testing.T) {
	if BotPolicyLocked(DomainBotsOn) {
		t.Error("a domain that blocks without enforcing locked the link setting; " +
			"the whole difference between on and enforced is that on can be opted out of")
	}
	if !BotPolicyLocked(DomainBotsEnforced) {
		t.Error("an enforcing domain did not lock the link setting, so the API " +
			"would accept an off that BlocksBots then ignores")
	}
}
