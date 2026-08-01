package mail

import (
	"errors"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func mustAddr(t *testing.T, s string) *mail.Address {
	t.Helper()
	a, err := mail.ParseAddress(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return a
}

// Header injection is the one way a plain-text mail can still be an attack: a
// value carrying CRLF ends the header it was in and starts a new one, and "a
// new header" includes Bcc.
//
// The renderer strips control characters from everything it interpolates, so
// this is the second of two independent guards. It is worth having twice
// because the two fail differently — the renderer would have to be bypassed,
// and this would have to be removed.
func TestBuildMessageRefusesHeaderInjection(t *testing.T) {
	from := mustAddr(t, "LinkCtrl <links@example.com>")

	tests := []struct {
		name    string
		to      string
		subject string
	}{
		{
			name:    "a bcc smuggled through the subject",
			to:      "owner@example.com",
			subject: "Hello\r\nBcc: everyone@example.com",
		},
		{
			name:    "a bare newline is enough, since relays are lenient",
			to:      "owner@example.com",
			subject: "Hello\nBcc: everyone@example.com",
		},
		{
			name:    "a subject that ends the header block and writes its own body",
			to:      "owner@example.com",
			subject: "Hello\r\n\r\nA message nobody wrote",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildMessage(from, tt.to, tt.subject, "body", time.Now())
			if !errors.Is(err, ErrHeaderInjection) {
				t.Fatalf("BuildMessage accepted %q: err = %v; a line break in a "+
					"header value is an injected header, not a formatting quirk",
					tt.subject, err)
			}
		})
	}

	// A recipient carrying a line break is refused before it can become a
	// header, and ParseAddress is what refuses it.
	if _, err := BuildMessage(from, "owner@example.com>\r\nBcc: everyone@example.com", "hi", "body", time.Now()); err == nil {
		t.Error("BuildMessage accepted a recipient containing a line break")
	}
}

// The shape of what actually goes on the wire. Asserted on the bytes rather
// than inferred from a relay's behaviour, because the properties that matter —
// plain text, CRLF, a sender, an Auto-Submitted marker — are properties of the
// message and not of any particular server's tolerance.
func TestBuildMessageShape(t *testing.T) {
	from := mustAddr(t, "LinkCtrl <links@example.com>")
	at := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	msg, err := BuildMessage(from, "owner@example.com", "A subject", "line one\nline two\n", at)
	if err != nil {
		t.Fatal(err)
	}
	got := string(msg)

	for _, want := range []string{
		"From: \"LinkCtrl\" <links@example.com>\r\n",
		"To: <owner@example.com>\r\n",
		"Subject: A subject\r\n",
		`Content-Type: text/plain; charset="utf-8"` + "\r\n",
		"MIME-Version: 1.0\r\n",
		// RFC 3834: this was not typed by a person, so a vacation responder
		// must not reply to it.
		"Auto-Submitted: auto-generated\r\n",
		"Date: Fri, 31 Jul 2026 12:00:00 +0000\r\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message is missing %q\n---\n%s", want, got)
		}
	}

	// No HTML part, ever. A multipart message with one is where mail acquires
	// remote images that report when it was opened and anchor text that
	// disagrees with its href; a text/plain body has no active content to make
	// inert in the first place.
	if strings.Contains(got, "multipart") || strings.Contains(got, "text/html") {
		t.Errorf("message is not plain text only:\n%s", got)
	}

	// SMTP is a CRLF protocol. A bare LF in the body is what makes a message
	// arrive with its lines run together on some relays and rejected by others.
	body := got[strings.Index(got, "\r\n\r\n")+4:]
	if strings.Contains(strings.ReplaceAll(body, "\r\n", ""), "\n") {
		t.Errorf("body contains a bare newline:\n%q", body)
	}

	// Two messages must not share a Message-ID, or a receiver is entitled to
	// treat the second as a duplicate of the first and drop it.
	second, err := BuildMessage(from, "owner@example.com", "A subject", "body", at)
	if err != nil {
		t.Fatal(err)
	}
	if idOf(t, got) == idOf(t, string(second)) {
		t.Error("two messages share a Message-ID; a receiver may drop the second as a duplicate")
	}
}

func idOf(t *testing.T, msg string) string {
	t.Helper()
	for line := range strings.SplitSeq(msg, "\r\n") {
		if strings.HasPrefix(line, "Message-ID:") {
			return line
		}
	}
	t.Fatal("no Message-ID header")
	return ""
}

// A subject outside ASCII has to be encoded, or it arrives as mojibake in
// clients that read the header as Latin-1 — which is what the standard says to
// do with an unencoded header.
func TestBuildMessageEncodesNonASCIISubjects(t *testing.T) {
	from := mustAddr(t, "links@example.com")

	msg, err := BuildMessage(from, "owner@example.com", "Überfällig — größer", "body", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(msg), "Subject: =?utf-8?q?") {
		t.Errorf("a non-ASCII subject was not encoded:\n%s", msg)
	}
	// The ASCII case must stay readable in a raw message rather than being
	// encoded for no reason.
	msg, err = BuildMessage(from, "owner@example.com", "Plain ascii", "body", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(msg), "Subject: Plain ascii\r\n") {
		t.Errorf("an ASCII subject was encoded when it did not need to be:\n%s", msg)
	}
}

// Bounded, and bounded in a shape somebody can reason about: doubling, capped,
// and never zero. A backoff of zero would turn the retry into a spin.
func TestBackoffDoublesAndCaps(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		// Defensive: a caller that has not counted an attempt yet still gets a
		// real delay rather than none.
		{0, time.Minute},
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, 16 * time.Minute},
		{6, BackoffMax},
		{100, BackoffMax},
	}
	for _, tt := range tests {
		if got := Backoff(tt.attempts); got != tt.want {
			t.Errorf("Backoff(%d) = %s, want %s", tt.attempts, got, tt.want)
		}
	}
}

func TestNewSMTPSenderRefusesAnUnparseableSender(t *testing.T) {
	if _, err := NewSMTPSender(SMTPOptions{Host: "smtp.example.com", From: "not an address"}); err == nil {
		t.Error("NewSMTPSender accepted a From that is not an address")
	}
}
