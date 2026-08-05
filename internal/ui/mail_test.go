package ui

import (
	"strings"
	"testing"
)

// mailData is a plausible value for every key any mail template names.
//
// One map for every template, deliberately: a template that adds a key without
// adding it here fails with "map has no entry", which is exactly the failure
// wanted — missingkey=error means a missing value can never reach a stranger's
// inbox as "<no value>".
func mailData() map[string]string {
	return map[string]string{
		"Instance":  "links.example.com",
		"Name":      "owner@example.com",
		"Size":      "6.0 GiB",
		"Threshold": "5.0 GiB",
		"AppURL":    "https://links.example.com",
		// The invitation (M27).
		"Inviter":      "Ada Lovelace",
		"Organization": "Analytical Engines",
		"Role":         "editor",
		"Email":        "invitee@example.com",
		"URL":          "https://links.example.com/invite/2ZQ3jd0eGkE",
		"Expires":      "7 August 2026, 09:00 UTC",
		// Whether redemption may create an account (F54, D7). Non-empty is
		// "open"; the closed branch has a test of its own below, because a
		// fixture that only ever renders one branch would leave the other
		// unexercised — which is how the unconditional sentence survived.
		"NewAccounts": "yes",
		// The dispute outcome (M32, D1's addendum). Host and reason code are
		// what internal/dispute actually hands over: the host already defanged,
		// and the reason code from this program's own vocabulary. The
		// inert-input test overwrites both with something hostile, which is the
		// case that matters — this is the one message whose subject matter is a
		// string somebody else chose.
		"Status":     "upheld",
		"Host":       "evil[.]example",
		"ReasonCode": "low_confidence.feed_reputation",
		"Outcome":    "The refusal stands, so that destination still cannot be used here.",
	}
}

// The invitation mail says something different when the instance is closed.
//
// It said "if you do not have an account yet, that form creates one"
// unconditionally, which is false wherever redemption may not create one — a
// closed instance with a mail relay, which is an intended and documented state.
// The service has always known the answer and the template was handed six keys,
// none of them this, so it could not have branched (F54).
//
// Both branches are rendered because a template with an `if` nobody exercises on
// both sides is a template with an untested half, and the untested half is the
// one that only appears on somebody else's instance.
func TestTheInvitationMailBranchesOnWhetherSignupsAreOpen(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}

	openData := mailData()
	_, openBody, err := r.RenderMail("invitation", openData)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(openBody, "that form creates one") {
		t.Error("an open instance's invitation no longer says the form creates an " +
			"account, which is the thing an invitee needs to know")
	}

	closedData := mailData()
	closedData["NewAccounts"] = ""
	_, closedBody, err := r.RenderMail("invitation", closedData)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(closedBody, "that form creates one") {
		t.Error("a closed instance's invitation still promises the form creates an " +
			"account. Redemption refuses that, and the landing page says so — so " +
			"the mail is the one surface in the flow telling a stranger the " +
			"opposite of what happens (F54)")
	}
	if !strings.Contains(closedBody, "not accepting new sign-ups") {
		t.Error("a closed instance's invitation does not say why an account is " +
			"needed first; the reader is left to discover it at the form")
	}
}

// Every mail template renders, with a subject and a body. The mail equivalent
// of TestEveryPageRenders, and it exists for the same reason: a template nobody
// exercises is discovered by the person who was supposed to receive it.
func TestEveryMailRenders(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}

	names := r.MailTemplates()
	if len(names) == 0 {
		t.Fatal("no mail templates were parsed")
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			subject, body, err := r.RenderMail(name, mailData())
			if err != nil {
				t.Fatalf("RenderMail: %v", err)
			}
			if subject == "" {
				t.Error("rendered an empty subject")
			}
			if strings.ContainsAny(subject, "\r\n") {
				t.Errorf("subject spans more than one line: %q", subject)
			}
			if strings.TrimSpace(body) == "" {
				t.Error("rendered an empty body")
			}
			// The header name belongs to the file, not to the subject. Leaving
			// it on would send a mail titled "Subject: Subject: …".
			if strings.HasPrefix(subject, subjectPrefix) {
				t.Errorf("the subject carries its own header name: %q", subject)
			}
			// The subject header belongs to the file's shape, not to the
			// message this package hands on. Leaving it in the body would send
			// the subject twice.
			if strings.HasPrefix(body, subjectPrefix) {
				t.Errorf("the subject line leaked into the body: %q", body[:40])
			}
			// Plain text means plain text. A stray tag here would be the first
			// step towards a multipart message and everything that comes with
			// one.
			if strings.Contains(body, "<html") || strings.Contains(body, "<a ") {
				t.Errorf("body contains markup:\n%s", body)
			}
		})
	}
}

// A value that no template names must not silently render as nothing.
func TestRenderMailRefusesAMissingValue(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range r.MailTemplates() {
		if _, _, err := r.RenderMail(name, map[string]string{}); err == nil {
			t.Errorf("%s rendered with no data at all; a mail naming a value "+
				"nobody supplied must fail rather than send \"<no value>\"", name)
		}
	}
}

func TestRenderMailRejectsAnUnknownTemplate(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.RenderMail("no-such-mail", mailData()); err == nil {
		t.Error("RenderMail accepted a template that does not exist")
	}
}

// The hostile-input claim, asserted on every template rather than on one.
//
// text/template does no escaping — unlike the html/template the pages use — so
// the only thing standing between a display name and the header block is inert,
// and it runs inside RenderMail so a template author cannot forget it.
func TestEveryMailRendersUntrustedInputInert(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}

	hostile := map[string]string{
		// The header injection. If any of this survives, the message gains a
		// Bcc nobody asked for.
		"Name": "Bob\r\nBcc: everyone@example.com\r\n\r\nA message nobody wrote",
		// A subject that tries to end the header block from inside an
		// interpolated value.
		"Instance": "example.com\nSubject: Your password has expired",
		// Markup and a scheme that is only dangerous if something renders it.
		// In a text/plain body they are letters; the assertion is that they
		// stay letters and never gain a line of their own.
		"Size":      `<script>alert(1)</script>`,
		"Threshold": "javascript:alert(1)",
		// A lone dot on its own line ends the SMTP DATA phase. The transport
		// dot-stuffs, and the value cannot make a line of its own anyway.
		"AppURL": "https://links.example.com\n.\nMAIL FROM:<attacker@example.com>",
		// The invitation's own values (M27). An inviter's display name is the
		// one field on this template a stranger chooses, and the invited address
		// is chosen by whoever holds members.write — so both are hostile here.
		"Inviter":      "Ada\r\nBcc: everyone@example.com",
		"Organization": "Engines\nSubject: Your invitation was cancelled",
		"Role":         "editor\r\n.\r\nQUIT",
		"Email":        "invitee\u202e@example.com",
		"URL":          "https://links.example.com/invite/x\nBcc: someone@example.com",
		"Expires":      "never\r\nMAIL FROM:<attacker@example.com>",
		// The dispute outcome (M32). Host is the interesting one: it is the
		// only value in any of these templates that a *stranger* chose and that
		// this product then mails to somebody who did not ask for it. It arrives
		// defanged from internal/dispute, and is hostile here anyway, because a
		// second layer that only works when the first one did is not a layer.
		"Status":     "upheld\r\nBcc: everyone@example.com",
		"Host":       "evil[.]example\r\n.\r\nMAIL FROM:<attacker@example.com>",
		"ReasonCode": "low_confidence.feed_reputation\nSubject: You have been paid",
		"Outcome":    "The refusal stands.\r\n\u202eBcc: someone@example.com",
		// A flag rather than a value, and hostile all the same: a non-empty
		// string takes the open branch, so this asserts the branch a stranger
		// could steer if the value ever came from outside. It does not — the
		// service reads it from configuration — and the key is here because
		// RenderMail refuses a template referencing a key nobody passed, which
		// is the property that caught this template's new branch at all.
		"NewAccounts": "yes\r\nBcc: everyone@example.com",
	}

	for _, name := range r.MailTemplates() {
		t.Run(name, func(t *testing.T) {
			subject, body, err := r.RenderMail(name, hostile)
			if err != nil {
				t.Fatalf("RenderMail: %v", err)
			}
			for what, s := range map[string]string{"subject": subject, "body": body} {
				if strings.ContainsAny(s, "\r") {
					t.Errorf("%s contains a carriage return: %q", what, s)
				}
			}
			if strings.ContainsAny(subject, "\n") {
				t.Errorf("subject spans more than one line: %q", subject)
			}
			// The injected header names survive as text — nothing is deleted
			// but the control characters — and that is the point: they are
			// words in a sentence rather than headers, because the line break
			// that would have made them headers is gone.
			for _, line := range strings.Split(subject+"\n"+body, "\n") {
				line = strings.TrimSpace(line)
				for _, header := range []string{"Bcc:", "Subject:", "MAIL FROM:"} {
					if strings.HasPrefix(line, header) {
						t.Errorf("a line begins with %q, so an interpolated value "+
							"became a header: %q", header, line)
					}
				}
				if line == "." {
					t.Error("a body line is a lone dot, which ends the SMTP DATA phase")
				}
			}
		})
	}
}

// inert is where the claim above is actually made, so its edges are asserted
// directly rather than only through a template.
func TestInert(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ordinary text is untouched", "Ada Lovelace", "Ada Lovelace"},
		{"a URL is untouched", "https://links.example.com/x?a=1", "https://links.example.com/x?a=1"},
		{"unicode is untouched", "Ada Lovelace — Überfällig", "Ada Lovelace — Überfällig"},
		{"CRLF goes", "Bob\r\nBcc: x@y.z", "BobBcc: x@y.z"},
		{"a bare newline goes", "Bob\nBcc: x@y.z", "BobBcc: x@y.z"},
		{"NUL goes", "Bob\x00Smith", "BobSmith"},
		{"a tab goes", "Bob\tSmith", "BobSmith"},
		{"DEL goes", "Bob\x7fSmith", "BobSmith"},
		// A right-to-left override makes an address render as one it is not,
		// which no amount of escaping addresses because nothing is escaped.
		// Written as escapes rather than as the characters themselves: they are
		// invisible, and a reviewer cannot check what they cannot see.
		{"a bidi override goes", "bob\u202e@example.com", "bob@example.com"},
		{"an isolate goes", "\u2066bob\u2069", "bob"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inert(tt.in); got != tt.want {
				t.Errorf("inert(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	long := strings.Repeat("a", MailValueLimit+50)
	got := inert(long)
	if !strings.HasSuffix(got, "… (truncated)") {
		t.Errorf("a value past the limit was not marked truncated: %q", got[len(got)-20:])
	}
	if n := len([]rune(strings.TrimSuffix(got, "… (truncated)"))); n != MailValueLimit {
		t.Errorf("truncated to %d runes, want %d", n, MailValueLimit)
	}
	// The boundary itself must not be marked: a value exactly at the limit was
	// not truncated.
	if got := inert(strings.Repeat("a", MailValueLimit)); strings.Contains(got, "truncated") {
		t.Error("a value exactly at the limit was reported as truncated")
	}
}
