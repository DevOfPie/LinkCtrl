package mail

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// A recipient's address does not reach the log, on the first bounce or any
// other.
//
// The relay's response line routinely echoes the address it refused — `550 5.1.1
// <alice@example.com>: User unknown` — and that error was logged at ERROR as an
// `error` attribute. The log redactor works by attribute *name* and `error` is
// not in its list, so the address went straight through, into a stream typically
// shipped somewhere with no LinkCtrl-side retention bound at all (F109).
//
// `mail_outbox.last_error` is deliberately not scrubbed: that row already
// carries `recipient` as a column, and it is access-controlled and
// retention-bounded.
func TestARelaysAnswerDoesNotCarryTheAddressIntoTheLog(t *testing.T) {
	for name, tc := range map[string]struct {
		recipient string
		relay     string
	}{
		"the ordinary bounce": {
			recipient: "alice@example.com",
			relay:     "550 5.1.1 <alice@example.com>: User unknown",
		},
		"a relay that changes the case": {
			recipient: "alice@example.com",
			relay:     "550 5.1.1 <Alice@Example.COM> recipient rejected",
		},
		"the local part on its own": {
			recipient: "alice@example.com",
			relay:     "550 mailbox alice does not exist",
		},
		"twice in one line": {
			recipient: "alice@example.com",
			relay:     "450 alice@example.com deferred; retry alice@example.com later",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := scrubAddress(errors.New(tc.relay), tc.recipient).Error()

			if strings.Contains(strings.ToLower(got), "alice") {
				t.Errorf("the scrubbed error still carries the recipient: %q", got)
			}
			if !strings.Contains(got, "[recipient]") {
				t.Errorf("nothing was replaced in %q; the marker is what says a "+
					"redaction happened rather than a relay being terse", got)
			}
			// The operational content survives, which is the reason this is a
			// replacement and not a drop: an error reduced to "delivery failed"
			// trades a disclosure for an outage nobody can diagnose.
			if code := strings.Fields(tc.relay)[0]; !strings.Contains(got, code) {
				t.Errorf("the relay's status code %q was lost: %q", code, got)
			}
		})
	}

	// A nil error and an empty recipient are both left alone rather than
	// becoming an empty error nobody can read.
	if got := scrubAddress(nil, "alice@example.com"); got != nil {
		t.Errorf("scrubAddress(nil) = %v, want nil", got)
	}
	if got := scrubAddress(errors.New("boom"), "").Error(); got != "boom" {
		t.Errorf("scrubAddress with no recipient = %q, want it unchanged", got)
	}
}

// Lowercasing does not preserve byte length, and the scrub must survive that.
//
// strings.ToLower is rune-wise: U+212A (the Kelvin sign) is three bytes and
// lowers to a one-byte `k`; U+023A (Ⱥ) is two bytes and lowers to the
// three-byte U+2C65. So an index found in a lowered copy of a relay line does
// not address the original line, and a scrub that mixes the two coordinate
// systems either leaks the address it exists to remove (shrinking runes pull
// the match point backwards until the redaction misses entirely) or slices
// past the end of the message and takes the drain goroutine down with it —
// there is no recover() anywhere on that path (growing runes push the match
// point past the real end).
//
// A relay is entitled to put any of these runes in its response line; a
// display-name echo or a localized diagnostic is enough. And the recipient
// side is not hypothetical either: SMTPUTF8 local parts are non-ASCII by
// design, which is the third case.
func TestScrubSurvivesRunesWhoseLowercaseIsADifferentLength(t *testing.T) {
	for name, tc := range map[string]struct {
		recipient string
		relay     string
		want      string
	}{
		// Nine Kelvin signs shrink the lowered copy by 18 bytes — more than
		// the address is long — so the broken arithmetic exhausts the lowered
		// haystack before the redaction fires and emits the whole address.
		"runes that shrink when lowered": {
			recipient: "alice@example.com",
			relay:     "550 " + strings.Repeat("K", 9) + "alice@example.com",
			want:      "550 " + strings.Repeat("K", 9) + "[recipient]",
		},
		// One growing rune before the address pushes the end of the match one
		// byte past the end of the message: a slice-bounds panic, not a leak.
		"a rune that grows when lowered": {
			recipient: "alice@example.com",
			relay:     "550 Ⱥalice@example.com",
			want:      "550 Ⱥ[recipient]",
		},
		// The local-part pass has the same arithmetic with the needle taken
		// from the recipient, so a non-ASCII local part diverges the needle's
		// own length between the two coordinate systems.
		"a non-ASCII local part": {
			recipient: "Ⱥbc@example.com",
			relay:     "550 Ⱥ mailbox Ⱥbc",
			want:      "550 Ⱥ mailbox [recipient]",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := scrubAddress(errors.New(tc.relay), tc.recipient).Error()

			if got != tc.want {
				t.Errorf("scrubAddress(%q, %q) = %q, want %q",
					tc.relay, tc.recipient, got, tc.want)
			}
			// Byte-offset drift shows up as torn runes before it shows up as
			// anything else, so validity is asserted on its own even though
			// the exact-match above implies it — the failure message is the
			// difference between "wrong output" and "cut a rune in half".
			if !utf8.ValidString(got) {
				t.Errorf("the scrubbed error is not valid UTF-8: %q", got)
			}
		})
	}
}
