package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/config"
)

// resolvedIP runs one request through RealIP and reports the address the
// handler saw. Every field of the request matters to the middleware, so the
// caller supplies them all.
func resolvedIP(t *testing.T, trusted []netip.Prefix, remoteAddr string, xff ...string) netip.Addr {
	t.Helper()
	var got netip.Addr
	h := RealIP(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = ClientIPFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = remoteAddr
	for _, v := range xff {
		// Add, not Set: multiple calls become multiple header LINES, which is
		// exactly the case that distinguishes Values from Get.
		req.Header.Add("X-Forwarded-For", v)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

func prefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}

// The safe default: with no trust list, the header is a lie anyone can tell.
func TestRealIPIgnoresXFFWithoutTrustedProxies(t *testing.T) {
	got := resolvedIP(t, nil, "203.0.113.9:4444", "198.51.100.7")
	if got != netip.MustParseAddr("203.0.113.9") {
		t.Errorf("resolved %v, want the socket address; XFF must be ignored without a trust list", got)
	}
}

func TestRealIPIgnoresXFFFromAnUntrustedPeer(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8")
	got := resolvedIP(t, trusted, "203.0.113.9:4444", "198.51.100.7")
	if got != netip.MustParseAddr("203.0.113.9") {
		t.Errorf("resolved %v; a peer outside the trust list must not speak for anyone", got)
	}
}

func TestRealIPTakesRightmostUntrustedHop(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8")
	// Client forged "1.2.3.4", proxy at 10.0.0.5 appended the real client.
	got := resolvedIP(t, trusted, "10.0.0.5:4444", "1.2.3.4, 198.51.100.7")
	if got != netip.MustParseAddr("198.51.100.7") {
		t.Errorf("resolved %v, want 198.51.100.7 (rightmost untrusted); the forged "+
			"left entry must never win", got)
	}
}

// The finding that motivated this file: HAProxy-style proxies append the client
// as a separate header line. Get() sees only the first line — the client's
// forgery — so the parser must join every occurrence.
func TestRealIPJoinsMultipleXFFHeaderLines(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8")
	got := resolvedIP(t, trusted, "10.0.0.5:4444",
		"1.2.3.4",      // client-supplied line, a forgery
		"198.51.100.7", // proxy-appended line, the real client
	)
	if got != netip.MustParseAddr("198.51.100.7") {
		t.Errorf("resolved %v, want 198.51.100.7 from the proxy-appended second "+
			"header line; reading only the first line hands the client the spoof", got)
	}
}

// A proxy that writes its upstream hop in IPv4-mapped form must still match an
// IPv4 trust prefix, or the walk stops at the mapped hop and reports the proxy
// as the client.
func TestRealIPUnmapsMappedHopsBeforeTheTrustCheck(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8")
	got := resolvedIP(t, trusted, "10.0.0.5:4444",
		"198.51.100.7, ::ffff:10.0.0.6")
	if got != netip.MustParseAddr("198.51.100.7") {
		t.Errorf("resolved %v, want 198.51.100.7; the mapped internal hop "+
			"::ffff:10.0.0.6 must be recognised as trusted and skipped", got)
	}
}

// An unparseable entry stops the walk instead of being skipped past.
//
// This test asserted the opposite until it was found to be describing a spoof.
// Everything to the left of an entry is client-controlled; only the rightmost
// entries were written by the trusted proxy. Real proxies do emit hops this
// parser rejects — IIS and ARR append the client as ip:port, Squid writes the
// literal "unknown" — and skipping past one promotes the next entry leftward,
// which the client wrote, to "the address the proxy vouched for". Falling back
// to the socket address is less specific and always true.
func TestRealIPStopsAtAnUnparseableEntry(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8")

	// The IIS/ARR shape: the real client, with a port, appended by the proxy.
	// 6.6.6.6 is what an attacker sent in the header they control.
	got := resolvedIP(t, trusted, "10.0.0.5:4444", "6.6.6.6, 203.0.113.9:51544")
	if got == netip.MustParseAddr("6.6.6.6") {
		t.Error("resolved the client-supplied entry after failing to parse the " +
			"proxy-appended one; every rate limit would key on an attacker's choice")
	}
	if got != netip.MustParseAddr("10.0.0.5") {
		t.Errorf("resolved %v, want the socket address 10.0.0.5 as the fallback", got)
	}

	// Squid's "unknown", with the same consequence.
	got = resolvedIP(t, trusted, "10.0.0.5:4444", "6.6.6.6, unknown")
	if got != netip.MustParseAddr("10.0.0.5") {
		t.Errorf("resolved %v after an `unknown` hop, want the socket address", got)
	}
}

// Every hop trusted means the request originated inside the proxy tier; the
// socket address is the honest answer, not a forged left entry... which is
// already covered by rightmost-untrusted: an all-trusted list leaves addr as
// the direct peer.
func TestRealIPAllTrustedChainKeepsTheSocketAddress(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8")
	got := resolvedIP(t, trusted, "10.0.0.5:4444", "10.0.0.7, 10.0.0.8")
	if got != netip.MustParseAddr("10.0.0.5") {
		t.Errorf("resolved %v, want the socket address when every hop is trusted", got)
	}
}

func TestRealIPMappedDirectPeerMatchesIPv4TrustPrefix(t *testing.T) {
	// The direct peer arrives mapped (dual-stack listener); directAddr Unmaps
	// it, so the trust check must succeed and the XFF must be honoured.
	trusted := prefixes(t, "10.0.0.0/8")
	got := resolvedIP(t, trusted, "[::ffff:10.0.0.5]:4444", "198.51.100.7")
	if got != netip.MustParseAddr("198.51.100.7") {
		t.Errorf("resolved %v, want 198.51.100.7 via the mapped trusted peer", got)
	}
}

// TestTheImageSourceIsSelfAndDataAndNothingElse pins `img-src` (M50.6).
//
// **Nothing pinned it before this milestone.** The existing CSP assertions check
// `script-src` and the absence of an `unsafe-` waiver, so a quiet widening of
// `img-src` to `blob:` or to a host would have passed every test this product
// had. M50.6 is what makes that matter: a QR code with a logo carries the image
// as a `data:` URI inside SVG the dashboard inlines, so the page now depends on
// this directive being exactly what it is — and a feature depending on a
// directive is the moment to stop taking the directive on trust.
//
// The list is written out rather than searched for a substring, because
// `strings.Contains(csp, "img-src 'self' data:")` is true of
// `img-src 'self' data: https://cdn.example` as well.
func TestTheImageSourceIsSelfAndDataAndNothingElse(t *testing.T) {
	const want = "img-src 'self' data:"

	var found string
	for _, directive := range strings.Split(csp, ";") {
		directive = strings.TrimSpace(directive)
		if strings.HasPrefix(directive, "img-src") {
			found = directive
		}
	}
	if found == "" {
		t.Fatalf("the policy names no img-src at all, so images fall back to "+
			"default-src and a change there moves them silently:\n  %s", csp)
	}
	if found != want {
		t.Errorf("img-src is %q, want %q.\n\nA QR code carrying a logo embeds the "+
			"image as a data: URI in SVG the dashboard inlines, so `data:` has to "+
			"stay and nothing may join it. `blob:` or a host here is a place a page "+
			"can be made to load somebody else's bytes from.", found, want)
	}

	// And on a real response, because a constant nothing serves is not a policy.
	rec := httptest.NewRecorder()
	SecurityHeaders(config.Config{})(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {},
	)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, want+";") {
		t.Errorf("the served policy does not carry %q:\n  %s", want, got)
	}
}
