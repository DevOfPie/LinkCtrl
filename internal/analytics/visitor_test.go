package analytics

import (
	"bytes"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	wsA = uuid.MustParse("019fb100-0000-7000-8000-00000000000a")
	wsB = uuid.MustParse("019fb100-0000-7000-8000-00000000000b")
)

func testSalt(t *testing.T) []byte {
	t.Helper()
	s, err := NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVisitorHashIsStableForTheSameVisitor(t *testing.T) {
	salt := testSalt(t)
	ip := netip.MustParseAddr("203.0.113.42")
	const ua = "Mozilla/5.0 (X11; Linux x86_64) Chrome/120"

	a := VisitorHash(salt, ip, ua, wsA)
	b := VisitorHash(salt, ip, ua, wsA)

	if !bytes.Equal(a, b) {
		t.Fatal("the same visitor hashed differently; unique counting would be meaningless")
	}
	if len(a) != VisitorHashLength {
		t.Errorf("hash length %d, want %d", len(a), VisitorHashLength)
	}
}

func TestVisitorHashDiffersAcrossInputs(t *testing.T) {
	salt := testSalt(t)
	ip := netip.MustParseAddr("203.0.113.42")
	const ua = "Mozilla/5.0 Chrome/120"
	base := VisitorHash(salt, ip, ua, wsA)

	cases := map[string][]byte{
		"different ip":        VisitorHash(salt, netip.MustParseAddr("203.0.113.43"), ua, wsA),
		"different ua":        VisitorHash(salt, ip, "Mozilla/5.0 Firefox/121", wsA),
		"different workspace": VisitorHash(salt, ip, ua, wsB),
		"different salt":      VisitorHash(testSalt(t), ip, ua, wsA),
	}
	for name, got := range cases {
		if bytes.Equal(base, got) {
			t.Errorf("%s produced an identical hash", name)
		}
	}
}

// TestVisitorHashIsNotCorrelatableAcrossWorkspaces is the multi-tenancy
// privacy property. Two workspaces on one instance share a daily salt, so if
// the workspace were not part of the message their analytics could be joined
// to track one person across both.
func TestVisitorHashIsNotCorrelatableAcrossWorkspaces(t *testing.T) {
	salt := testSalt(t)
	ip := netip.MustParseAddr("198.51.100.7")
	const ua = "Mozilla/5.0 Safari/17"

	if bytes.Equal(VisitorHash(salt, ip, ua, wsA), VisitorHash(salt, ip, ua, wsB)) {
		t.Fatal("the same visitor hashes identically in two workspaces; their analytics " +
			"could be joined to follow one person across both")
	}
}

// TestVisitorHashRotatesWithTheSalt is the whole privacy mechanism. Once a
// day's salt is deleted, that day's hashes cannot be reproduced from an
// address, which is what puts click_events outside the scope of an erasure
// request.
func TestVisitorHashRotatesWithTheSalt(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.42")
	const ua = "Mozilla/5.0 Chrome/120"

	day1 := VisitorHash(testSalt(t), ip, ua, wsA)
	day2 := VisitorHash(testSalt(t), ip, ua, wsA)

	if bytes.Equal(day1, day2) {
		t.Fatal("hashes survived a salt rotation; a visitor would be trackable indefinitely")
	}
}

// TestVisitorHashFoldsIPv4MappedAddresses: the same client reaching the server
// over IPv4 and over an IPv4-mapped IPv6 socket must be one visitor, not two.
func TestVisitorHashFoldsIPv4MappedAddresses(t *testing.T) {
	salt := testSalt(t)
	const ua = "Mozilla/5.0 Chrome/120"

	plain := VisitorHash(salt, netip.MustParseAddr("203.0.113.42"), ua, wsA)
	mapped := VisitorHash(salt, netip.MustParseAddr("::ffff:203.0.113.42"), ua, wsA)

	if !bytes.Equal(plain, mapped) {
		t.Error("an IPv4-mapped IPv6 address hashed differently from the same IPv4 address; " +
			"one visitor would be counted twice")
	}
}

// TestVisitorHashSeparatesFields guards against a crafted user agent shifting
// the field boundary to collide with a different address.
func TestVisitorHashSeparatesFields(t *testing.T) {
	salt := testSalt(t)
	ip := netip.MustParseAddr("203.0.113.42")

	a := VisitorHash(salt, ip, "abc", wsA)
	b := VisitorHash(salt, ip, "ab\x00c", wsA)
	if bytes.Equal(a, b) {
		t.Error("user agents differing only at a NUL boundary collided")
	}
}

func TestVisitorHashHandlesInvalidAddress(t *testing.T) {
	salt := testSalt(t)
	// An unparseable client address must not panic; the visitor is simply
	// identified by user agent alone that day.
	h := VisitorHash(salt, netip.Addr{}, "Mozilla/5.0", wsA)
	if len(h) != VisitorHashLength {
		t.Errorf("hash length %d for an invalid address", len(h))
	}
}

func TestAnonymizeIP(t *testing.T) {
	tests := map[string]string{
		"203.0.113.42":          "203.0.113.0/24",
		"10.1.2.3":              "10.1.2.0/24",
		"2001:db8:1234:5678::1": "2001:db8:1234::/48",
		// Must fold first: masking this to /48 as IPv6 would keep the whole
		// embedded IPv4 address, which is the data being discarded.
		"::ffff:203.0.113.42": "203.0.113.0/24",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := AnonymizeIP(netip.MustParseAddr(in)); got != want {
				t.Errorf("AnonymizeIP(%s) = %q, want %q", in, got, want)
			}
		})
	}
	if got := AnonymizeIP(netip.Addr{}); got != "" {
		t.Errorf("AnonymizeIP(invalid) = %q, want empty", got)
	}
}

func TestSaltDayIsUTC(t *testing.T) {
	// A local-time boundary would rotate at a different instant per
	// deployment and split a visitor across a daylight-saving change.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("timezone database unavailable")
	}
	// 2026-07-30 01:00 in New York is 05:00 UTC the same day.
	local := time.Date(2026, 7, 30, 1, 0, 0, 0, ny)
	day := SaltDay(local)

	if day.Location() != time.UTC {
		t.Errorf("SaltDay returned %v, want UTC", day.Location())
	}
	if day.Format(time.DateOnly) != "2026-07-30" {
		t.Errorf("SaltDay = %s, want 2026-07-30", day.Format(time.DateOnly))
	}

	// 21:00 in New York is 01:00 UTC the NEXT day, and must bucket there.
	evening := time.Date(2026, 7, 30, 21, 0, 0, 0, ny)
	if got := SaltDay(evening).Format(time.DateOnly); got != "2026-07-31" {
		t.Errorf("SaltDay(21:00 New York) = %s, want 2026-07-31 (UTC day)", got)
	}
}

// --- user agent classification ---------------------------------------------

func TestClassifyBrowsersInOrderOfSpecificity(t *testing.T) {
	// Order is load-bearing: Edge and Opera both claim Chrome, and Chrome
	// claims Safari, so a naive check misreports all of them.
	tests := []struct {
		ua      string
		browser string
		os      string
		device  Device
	}{
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0",
			"Edge", "Windows", DeviceDesktop},
		{"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36 OPR/106.0.0.0",
			"Opera", "Windows", DeviceDesktop},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36",
			"Chrome", "Windows", DeviceDesktop},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/17.0 Safari/605.1.15",
			"Safari", "macOS", DeviceDesktop},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0",
			"Firefox", "Linux", DeviceDesktop},
		// iOS agents contain "mac os x"; the iOS check must come first.
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Version/17.0 Mobile/15E148 Safari/604.1",
			"Safari", "iOS", DeviceMobile},
		{"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148 Safari/604.1",
			"Safari", "iOS", DeviceTablet},
		// Android agents contain "linux"; the Android check must come first.
		{"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/120.0.0.0 Mobile Safari/537.36",
			"Chrome", "Android", DeviceMobile},
		// Android without "mobile" is conventionally a tablet.
		{"Mozilla/5.0 (Linux; Android 14; SM-X200) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36",
			"Chrome", "Android", DeviceTablet},
	}

	for _, tc := range tests {
		t.Run(tc.browser+"/"+tc.os, func(t *testing.T) {
			got := Classify(tc.ua)
			if got.Browser != tc.browser {
				t.Errorf("browser = %q, want %q", got.Browser, tc.browser)
			}
			if got.OS != tc.os {
				t.Errorf("os = %q, want %q", got.OS, tc.os)
			}
			if got.Device != tc.device {
				t.Errorf("device = %q, want %q", got.Device, tc.device)
			}
			if got.IsBot {
				t.Error("a real browser was classified as a bot")
			}
		})
	}
}

func TestClassifyDetectsBots(t *testing.T) {
	bots := []string{
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		"Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
		"curl/8.4.0",
		"Wget/1.21.4",
		"python-requests/2.31.0",
		"Go-http-client/2.0",
		"PostmanRuntime/7.36.0",
		// Unfurlers matter most for a shortener: a link pasted into Slack
		// generates a fetch that is not a visit.
		"Mozilla/5.0 (compatible; Slackbot-LinkExpanding 1.0; +https://api.slack.com/robots)",
		"facebookexternalhit/1.1",
		"Twitterbot/1.0",
		"Discordbot/2.0",
		"WhatsApp/2.23",
		"", // absent user agent
	}
	for _, ua := range bots {
		name := ua
		if name == "" {
			name = "(empty)"
		}
		t.Run(name, func(t *testing.T) {
			if !Classify(ua).IsBot {
				t.Errorf("%q was not detected as automated traffic", ua)
			}
		})
	}
}

func TestPrimaryLanguage(t *testing.T) {
	tests := map[string]string{
		"en-GB,en;q=0.9,fr;q=0.8": "en",
		"fr-CA":                   "fr",
		"de":                      "de",
		"":                        "",
		"  es-ES  ":               "es",
		// Region is dropped: it adds granularity nobody reports on and
		// narrows the anonymity set.
		"pt-BR,pt;q=0.9": "pt",
	}
	for in, want := range tests {
		if got := PrimaryLanguage(in); got != want {
			t.Errorf("PrimaryLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReferrerHostStripsEverythingButTheHost(t *testing.T) {
	// Full referrers routinely carry session tokens and search terms in the
	// query string, so only the host is kept.
	tests := map[string]string{
		"https://example.com/path?token=secret&q=private": "example.com",
		"http://Sub.Example.COM:8080/a/b":                 "sub.example.com",
		"https://user:pass@example.com/x":                 "example.com",
		"https://example.com":                             "example.com",
		"":                                                "",
		"android-app://com.google.android.gm":             "com.google.android.gm",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			got := ReferrerHost(in)
			if got != want {
				t.Errorf("ReferrerHost(%q) = %q, want %q", in, got, want)
			}
			// Whatever is returned must never contain a query string.
			if len(got) > 0 && (containsAny(got, "?&=")) {
				t.Errorf("ReferrerHost(%q) = %q, which retains query data", in, got)
			}
		})
	}
}

func containsAny(s, chars string) bool {
	for _, c := range chars {
		for _, r := range s {
			if r == c {
				return true
			}
		}
	}
	return false
}

func FuzzClassify(f *testing.F) {
	f.Add("Mozilla/5.0")
	f.Add("")
	f.Add("curl/8.0")
	f.Fuzz(func(t *testing.T, ua string) {
		c := Classify(ua)
		if c.Device == "" {
			t.Errorf("Classify(%q) returned an empty device", ua)
		}
	})
}

func FuzzReferrerHost(f *testing.F) {
	f.Add("https://example.com/x?y=1")
	f.Add("")
	f.Add("://")
	f.Fuzz(func(t *testing.T, ref string) {
		got := ReferrerHost(ref)
		if len(got) > 253 {
			t.Errorf("ReferrerHost(%q) returned %d bytes", ref, len(got))
		}
	})
}

func BenchmarkVisitorHash(b *testing.B) {
	salt := make([]byte, SaltLength)
	ip := netip.MustParseAddr("203.0.113.42")
	const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0.0.0 Safari/537.36"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = VisitorHash(salt, ip, ua, wsA)
	}
}

func BenchmarkClassify(b *testing.B) {
	const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Classify(ua)
	}
}
