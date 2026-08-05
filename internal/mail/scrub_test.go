package mail

import (
	"errors"
	"strings"
	"testing"
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
