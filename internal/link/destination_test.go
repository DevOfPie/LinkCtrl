package link

import (
	"errors"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

func TestValidateDestinationAccepts(t *testing.T) {
	p := DefaultDestinationPolicy()
	tests := []struct{ in, want string }{
		{"https://example.com", "https://example.com"},
		{"http://example.com/path?a=1#frag", "http://example.com/path?a=1#frag"},
		{"https://Example.COM/Path", "https://example.com/Path"}, // host folded, path preserved
		{"https://example.com:8443/x", "https://example.com:8443/x"},
		{"  https://example.com  ", "https://example.com"},
		{"https://sub.domain.example.co.uk/a/b", "https://sub.domain.example.co.uk/a/b"},
		{"https://example.com/unicode/%E2%9C%93", "https://example.com/unicode/%E2%9C%93"},
		// A public IP literal is fine.
		{"https://93.184.216.34/", "https://93.184.216.34/"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ValidateDestination(tc.in, p)
			if err != nil {
				t.Fatalf("rejected a valid destination: %v", err)
			}
			if got != tc.want {
				t.Errorf("normalized to %q, want %q", got, tc.want)
			}
		})
	}
}

// TestValidateDestinationRejectsDangerousSchemes is the reason the policy is an
// allowlist. Each of these is a real redirect-based attack if permitted.
func TestValidateDestinationRejectsDangerousSchemes(t *testing.T) {
	p := DefaultDestinationPolicy()
	dangerous := []string{
		"javascript:alert(document.cookie)",
		"JavaScript:alert(1)",
		"data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==",
		"vbscript:msgbox(1)",
		"file:///etc/passwd",
		"file://C:/Windows/System32/config/SAM",
		"ftp://example.com/x",
		"gopher://example.com",
		"intent://scan#Intent;scheme=zxing;end",
		"chrome://settings",
		"about:blank",
		"blob:https://example.com/uuid",
	}

	for _, raw := range dangerous {
		t.Run(raw, func(t *testing.T) {
			if _, err := ValidateDestination(raw, p); err == nil {
				t.Errorf("accepted %q; only http and https may be permitted", raw)
			}
		})
	}
}

func TestValidateDestinationBlocksPrivateAddresses(t *testing.T) {
	p := DefaultDestinationPolicy()
	// A short link pointing at a private address turns the shortener into a
	// tool for making someone else's browser probe their own network.
	private := []string{
		"http://10.0.0.1/admin",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"http://127.0.0.1:8080/",
		"http://localhost:3000/",
		"http://app.localhost/",
		"http://[::1]/",
		"http://0.0.0.0/",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata endpoint
		"http://100.64.0.1/",                       // carrier-grade NAT
		// IPv4-mapped IPv6 must be folded before the check, or this slips past
		// every IPv4 rule.
		"http://[::ffff:10.0.0.1]/",
		"http://[fe80::1]/",
		"http://[fc00::1]/",
	}

	for _, raw := range private {
		t.Run(raw, func(t *testing.T) {
			_, err := ValidateDestination(raw, p)
			if err == nil {
				t.Errorf("accepted %q, which points at a private or local address", raw)
			}
		})
	}
}

func TestPrivateAddressBlockingCanBeDisabled(t *testing.T) {
	// A self-hoster pointing links at an intranet is a legitimate
	// configuration, so this must be a policy rather than a hard rule.
	p := DefaultDestinationPolicy()
	p.BlockPrivateIPs = false

	if _, err := ValidateDestination("http://10.0.0.1/admin", p); err != nil {
		t.Errorf("private address rejected with blocking disabled: %v", err)
	}
	// Scheme restrictions still apply.
	if _, err := ValidateDestination("javascript:alert(1)", p); err == nil {
		t.Error("scheme allowlist must still apply when private blocking is off")
	}
}

func TestValidateDestinationRejectsControlCharacters(t *testing.T) {
	p := DefaultDestinationPolicy()
	// A newline reaching a Location header is response splitting.
	bad := []string{
		"https://example.com/\r\nSet-Cookie: admin=1",
		"https://example.com/\npath",
		"https://example.com/\x00",
		"https://example.com/\ttab",
	}
	for _, raw := range bad {
		t.Run(strings.ReplaceAll(raw, "\n", "\\n"), func(t *testing.T) {
			if _, err := ValidateDestination(raw, p); err == nil {
				t.Errorf("accepted %q, which contains a control character", raw)
			}
		})
	}
}

func TestValidateDestinationMiscRejections(t *testing.T) {
	p := DefaultDestinationPolicy()
	tests := map[string]string{
		"empty":         "",
		"whitespace":    "   ",
		"no scheme":     "example.com/path",
		"scheme only":   "https://",
		"no host":       "https:///path",
		"too long":      "https://example.com/" + strings.Repeat("a", 3000),
		"relative path": "/just/a/path",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateDestination(raw, p); err == nil {
				t.Errorf("accepted %q", raw)
			}
		})
	}
}

func TestBlockedHostSuffixesMatchOnLabelBoundary(t *testing.T) {
	p := DefaultDestinationPolicy()
	p.BlockedHostSuffixes = []string{"evil.com", "Bad.example"}

	blocked := []string{
		"https://evil.com/x",
		"https://sub.evil.com/x",
		"https://deep.sub.evil.com/x",
		"https://bad.example/x",
		"https://a.BAD.example/x",
	}
	for _, raw := range blocked {
		if _, err := ValidateDestination(raw, p); err == nil {
			t.Errorf("accepted blocked host %q", raw)
		}
	}

	// Must not over-match: blocking evil.com should not block notevil.com.
	allowed := []string{
		"https://notevil.com/x",
		"https://evil.com.example.org/x",
		"https://myevil.com/x",
	}
	for _, raw := range allowed {
		if _, err := ValidateDestination(raw, p); err != nil {
			t.Errorf("rejected %q; suffix matching must respect label boundaries: %v", raw, err)
		}
	}
}

func TestValidationErrorsCarryFieldAndCode(t *testing.T) {
	p := DefaultDestinationPolicy()
	_, err := ValidateDestination("javascript:alert(1)", p)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("error does not identify as a validation failure: %v", err)
	}

	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("error is %T, want domain.ValidationErrors so a form can highlight the field", err)
	}
	if len(ve) == 0 || ve[0].Field != "url" {
		t.Errorf("field errors = %+v, want one against the url field", ve)
	}
	if ve[0].Code == "" {
		t.Error("field error has no machine-readable code")
	}
}

func TestHostOf(t *testing.T) {
	tests := map[string]string{
		"https://Example.COM/path":      "example.com",
		"http://sub.example.org:8080/x": "sub.example.org",
		"not a url":                     "",
		"https://[2001:db8::1]/":        "2001:db8::1",
	}
	for in, want := range tests {
		if got := HostOf(in); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func FuzzValidateDestination(f *testing.F) {
	seeds := []string{
		"https://example.com", "javascript:alert(1)", "", "http://10.0.0.1",
		"https://example.com/\r\n", "://", "https://[::1]", strings.Repeat("a", 5000),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	p := DefaultDestinationPolicy()
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := ValidateDestination(raw, p)
		if err != nil {
			return
		}
		// Anything accepted must be safe to put in a Location header.
		lower := strings.ToLower(got)
		if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
			t.Errorf("accepted %q which normalized to %q, not an http(s) URL", raw, got)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Errorf("accepted %q which normalized to %q containing a control character", raw, got)
			}
		}
	})
}
