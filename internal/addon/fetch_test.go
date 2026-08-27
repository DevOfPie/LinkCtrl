package addon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// M68.5, and this file is where the capability is falsifiable.
//
// The milestone's own risk says it in as many words: *SSRF protections are
// famously defeated by the gap between check and use, by redirects, by DNS
// rebinding and by IPv6 representations of IPv4 addresses … the risk is that the
// test suite is written against the bypasses somebody thought of.* Nothing here
// closes that. What it does is split the claim in two so that neither half rests
// on the other:
//
//   - the **policy** is a pure function over an address, driven by a table with
//     every representation this file's author could name in it;
//   - the **wiring** is asserted by dialling 127.0.0.1 for real, with the policy
//     untouched, through a name that only resolves to it — which is the shape a
//     parse-time check passes and a dial-time one does not.
//
// A suite that only relaxed the policy in order to reach a test server would be a
// suite in which the policy was never connected to anything.

// --- the policy ------------------------------------------------------------

// TestTheAddressPolicyRefusesEverythingOffThePublicInternet drives
// [refuseAddress] directly.
//
// The table is the enumeration, and the IPv4-mapped rows are the ones that earn
// it: `::ffff:127.0.0.1` is loopback written as an IPv6 address, and a check that
// asked `ip.Is4()` before deciding would let it through. So would one that read
// the address out of the URL and compared strings.
func TestTheAddressPolicyRefusesEverythingOffThePublicInternet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		addr    string
		refused bool
		why     string
	}{
		// Loopback, in four spellings.
		{"127.0.0.1", true, "loopback"},
		{"127.1.2.3", true, "the rest of 127/8, which is all loopback"},
		{"::1", true, "IPv6 loopback"},
		{"::ffff:127.0.0.1", true, "loopback as an IPv4-mapped IPv6 address"},
		// The unspecified address, which on many stacks connects to localhost.
		{"0.0.0.0", true, "the unspecified address"},
		{"::", true, "the IPv6 unspecified address"},
		{"::ffff:0.0.0.0", true, "the unspecified address, mapped"},
		// Link-local, and the one that matters most.
		{"169.254.169.254", true, "the cloud metadata service"},
		{"169.254.0.1", true, "link-local"},
		{"fe80::1", true, "IPv6 link-local"},
		{"::ffff:169.254.169.254", true, "the metadata service, mapped"},
		// RFC 1918, each range, and each mapped.
		{"10.0.0.1", true, "RFC 1918"},
		{"10.255.255.255", true, "the top of 10/8"},
		{"172.16.0.1", true, "RFC 1918"},
		{"172.31.255.254", true, "the top of 172.16/12"},
		{"192.168.1.1", true, "RFC 1918"},
		{"::ffff:10.0.0.1", true, "RFC 1918, mapped"},
		{"::ffff:192.168.0.1", true, "RFC 1918, mapped"},
		// RFC 4193.
		{"fc00::1", true, "unique-local"},
		{"fd12:3456:789a::1", true, "unique-local, the half that is actually used"},
		// The ranges that are neither private nor public.
		{"100.64.0.1", true, "carrier-grade NAT"},
		{"192.0.0.1", true, "IETF protocol assignments"},
		{"198.18.0.1", true, "benchmarking"},
		{"192.0.2.1", true, "documentation"},
		{"198.51.100.1", true, "documentation"},
		{"203.0.113.1", true, "documentation"},
		{"192.88.99.1", true, "the 6to4 relay anycast"},
		{"2001:db8::1", true, "IPv6 documentation"},
		{"240.0.0.1", true, "reserved for future use"},
		{"255.255.255.255", true, "the limited broadcast address"},
		// Multicast, both families.
		{"224.0.0.1", true, "multicast"},
		{"ff02::1", true, "IPv6 link-local multicast"},
		// The transition mechanisms, each of which embeds an IPv4 address.
		{"2002:7f00:0001::", true, "6to4 wrapping 127.0.0.1"},
		{"2001:0:1234::", true, "Teredo"},
		{"64:ff9b::7f00:1", true, "NAT64 wrapping 127.0.0.1"},
		{"64:ff9b:1::1", true, "the local-use NAT64 prefix"},
		{"::7f00:1", true, "IPv4-compatible IPv6, which is 127.0.0.1"},
		// The rest of 0/8, which the unspecified check does not reach: the first
		// review of this file found 0.0.0.1 dialled, and it is on every published
		// SSRF list.
		{"0.0.0.1", true, "\"this network\", one past the unspecified address"},
		{"0.1.2.3", true, "the rest of 0/8"},
		{"::ffff:0.0.0.1", true, "\"this network\", mapped"},
		// The IANA special-purpose entries nothing above covered.
		{"192.52.193.1", true, "AMT"},
		{"192.31.196.1", true, "AS112-v4"},
		{"192.175.48.1", true, "the direct delegation AS112 service"},
		// F337, closed at M68.6. The one address in this table that no registry
		// produces: Azure's WireServer is ordinary public IPv4, so it is global
		// unicast, inside routable space, and reached by no predicate — the exact
		// shape a denylist misses and an allowlist does not save you from, because
		// it is inside the allowed space.
		{"168.63.129.16", true, "Azure's WireServer, which is public IPv4 and is not a destination"},
		{"168.63.129.15", false, "its neighbour, which is somebody's ordinary host"},
		{"100::1", true, "the IPv6 discard-only prefix"},
		{"2001:2::1", true, "IPv6 benchmarking"},
		{"2001:3::1", true, "AMT over IPv6"},
		{"2001:4:112::1", true, "AS112-v6"},
		{"2001:10::1", true, "ORCHID, deprecated"},
		{"2001:20::1", true, "ORCHIDv2"},
		{"2620:4f:8000::1", true, "the AS112 delegation"},
		// The six the second review of this file found dialled, every one of them an
		// IPv6 registry entry younger than the list that was meant to enumerate the
		// registry. The first four are why 2001::/23 is refused as a block; the last
		// two are top-level allocations, and 5f00::/16 no longer needs an entry at
		// all — it is outside 2000::/3 and the allowlist refuses it for free.
		{"2001:1::1", true, "the PCP anycast"},
		{"2001:1::2", true, "the TURN anycast"},
		{"2001:1::3", true, "DNS-SD service registration"},
		{"2001:30::1", true, "DRIP, RFC 9374"},
		{"5f00::1", true, "SRv6 SIDs, RFC 9602"},
		{"3fff::1", true, "IPv6 documentation, RFC 9637"},
		// And the rest of the block, which is refused because the block is: an
		// address IANA has not assigned yet is refused before it is assigned, which
		// is the whole point of refusing it whole.
		{"2001:4:113::1", true, "unassigned inside the IETF protocol assignments block"},
		{"2001:1ff::1", true, "the top of the IETF protocol assignments block"},
		// **Deprecated IPv6 site-local, RFC 3879**, which the fourth review of this
		// file found dialled. It is not in the special-purpose registry the old
		// denylist's claim was scoped to, fc00::/7 is unique-local and does not
		// contain it, and IsLinkLocalUnicast is fe80::/10 and does not reach it —
		// so nothing in the old mechanism could have refused it. The allowlist does,
		// because it is outside 2000::/3 and everything outside is refused.
		{"fec0::1", true, "deprecated IPv6 site-local, RFC 3879"},
		{"fed0::abcd", true, "the middle of fec0::/10"},
		{"feff::1", true, "the top of fec0::/10"},
		// And the public internet, which has to still work or this is a refusal to
		// implement the milestone rather than a bound on it.
		{"8.8.8.8", false, ""},
		{"1.1.1.1", false, ""},
		{"93.184.216.34", false, ""},
		{"2606:2800:220:1:248:1893:25c8:1946", false, ""},
		{"172.32.0.1", false, "just above 172.16/12, and public"},
		{"172.15.255.255", false, "just below 172.16/12, and public"},
		{"100.128.0.1", false, "just above 100.64/10, and public"},
		{"11.0.0.1", false, "just above 10/8, and public"},
		{"9.255.255.255", false, "just below 10/8, and public"},
		{"1.0.0.1", false, "the bottom of the IPv4 space this host dials"},
		{"223.255.255.255", false, "the top of it"},
		{"192.52.194.1", false, "just above the AMT block, and public"},
		{"2001:200::1", false, "just above the IETF protocol assignments block, and public"},
		{"3ffe:ffff::1", false, "just below the documentation block, and public"},
		{"2000::1", false, "the bottom of 2000::/3"},
		{"3fff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", false, "the top of 2000::/3"},
		// **The rows the inversion is for.** Every one of these is in neither list
		// in fetch.go: not in routableSpace, not in carvedOut, and not reached by any
		// stdlib predicate — IsGlobalUnicast is true for all four. Under the denylist
		// this file shipped four attempts running, each was dialled. Under the
		// allowlist each is refused because refusal is what happens to an address
		// nobody wrote anything about, and that is the whole of what inverting bought.
		// TestARangeInNeitherListIsRefused says the same thing about the rule.
		{"4000::1", true, "IPv6 space IANA has not allocated"},
		{"6000::1", true, "more of it"},
		{"a000::1", true, "and more"},
		{"0200::1", true, "below every allocation, and not a predicate's business"},
		{"0.255.255.255", true, "the top of 0/8, below the space this host dials"},
		// The two edges of the IPv4 cover from the other side. The second is
		// multicast as well, and is here for the boundary rather than for the rule.
		{"224.0.0.0", true, "the bottom of multicast, one past the top of the cover"},
	} {
		ip, err := netip.ParseAddr(tc.addr)
		if err != nil {
			t.Fatalf("%s is not an address: %v", tc.addr, err)
		}
		err = refuseAddress(ip)
		switch {
		case tc.refused && err == nil:
			t.Errorf("%s (%s) is dialled, and it is off the public internet", tc.addr, tc.why)
		case !tc.refused && err != nil:
			t.Errorf("%s is a public address and was refused: %v. A bound that refuses "+
				"the internet is a capability nobody can use", tc.addr, err)
		}
		// And the same answer from the two lists alone, which is what makes the
		// named predicates naming rather than mechanism. A predicate deleted by
		// accident costs a legible refusal here and nothing more; if this line ever
		// disagrees with the one above, one of the two ranges the predicates reach
		// inside the IPv4 cover — 127.0.0.0/8 and 169.254.0.0/16 — has left
		// carvedOut, and the policy is resting on a stdlib predicate again.
		if listed := refuseByList(ip.Unmap()) != nil; listed != tc.refused {
			t.Errorf("the lists alone %s %s and the policy %s it: the predicates in "+
				"refuseAddress are meant to name a refusal, not to be one",
				refusedWord(listed), tc.addr, refusedWord(tc.refused))
		}
	}
}

func refusedWord(refused bool) string {
	if refused {
		return "refuse"
	}
	return "permit"
}

// **The property the inversion buys, and the one a denylist could never assert.**
//
// Every address here is in neither list in fetch.go — not in routableSpace, not in
// carvedOut — and every one of them is global unicast by the stdlib's reckoning, so
// no named predicate touches it either. Nobody wrote anything about any of them.
// Under the denylist this file shipped for four attempts each was dialled; under
// the allowlist each is refused, and refused by the rule that says so, which is
// what an operator will find in the log when this policy is one day wrong about a
// legitimate origin (D375).
func TestARangeInNeitherListIsRefused(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{
		// IPv6 outside 2000::/3: what IANA has not allocated global unicast from,
		// which is where the next top-level allocation will come from and where
		// 5f00::/16 and fec0::/10 already are.
		"4000::1", "6000::1", "8000::1", "a000::1", "c000::1", "0200::1", "1fff::1",
		// And the IPv4 side of the same thing: below the delegated space.
		"0.255.255.255",
	} {
		ip := netip.MustParseAddr(addr)
		if !ip.IsGlobalUnicast() {
			t.Fatalf("%s is not global unicast, so this row proves nothing about the "+
				"lists — a predicate refuses it before they are reached", addr)
		}
		for _, p := range append(append([]netip.Prefix{}, routableSpace...), carvedOut...) {
			if p.Contains(ip) {
				t.Fatalf("%s is in %s; this test is about addresses no list mentions "+
					"and it has stopped being about one", addr, p)
			}
		}
		var refusal *addressRefusal
		if err := refuseAddress(ip); !errors.As(err, &refusal) {
			t.Errorf("%s is dialled and nothing in this file has ever mentioned it. "+
				"That is the denylist failure mode returning: a range nobody thought "+
				"of is reachable, and the symptom is an SSRF nobody observes", addr)
		} else if refusal.rule != "outside-routable-space" {
			t.Errorf("%s was refused by the rule %q; it is refused because it is "+
				"outside the space this host dials, and the rule an operator greps "+
				"has to say which one refused", addr, refusal.rule)
		}
	}
}

// carvedOut is exceptions rather than mechanism, and an exception outside the
// space it excepts from is a line that looks like a bound and is not.
//
// Both halves: inside routableSpace, and actually refused at both ends. Cheap, and
// it is the shape that catches an entry deleted while somebody was fixing
// something else — or one written for a range the allowlist already refuses, which
// under the old denylist was indistinguishable from a real one.
func TestEveryCarvedOutRangeIsInsideRoutableSpaceAndRefused(t *testing.T) {
	t.Parallel()
	for _, c := range carvedOut {
		inside := false
		for _, r := range routableSpace {
			if r.Contains(c.Addr()) {
				inside = true
				break
			}
		}
		if !inside {
			t.Errorf("%s is carved out of a space this host does not dial anyway. "+
				"It is not wrong, it is empty — and an empty exception is how a list "+
				"comes to look longer than the policy it states", c)
		}
		for _, edge := range []netip.Addr{c.Addr(), lastAddrIn(c)} {
			if err := refuseAddress(edge); err == nil {
				t.Errorf("%s is in %s and is dialled", edge, c)
			}
		}
	}
}

// lastAddrIn is the top of a prefix: every bit outside the mask set.
func lastAddrIn(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As16()
	// Bits are counted from the front of the address family, and As16 puts an IPv4
	// address in the last four bytes, so the offset moves with the family.
	off := 128 - p.Addr().BitLen()
	for i := off + p.Bits(); i < 128; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	out := netip.AddrFrom16(b)
	if p.Addr().Is4() {
		out = out.Unmap()
	}
	return out
}

// routableSpace is nine IPv4 prefixes and one IPv6 one, and a reader cannot check
// by eye that the nine cover 1.0.0.0 through 223.255.255.255 and nothing either
// side. This walks all 256 /8s and both edges of the IPv6 block instead.
//
// It matters more than an arithmetic check usually would: under an allowlist a
// prefix written one bit too wide is a range that gets dialled, and a prefix
// written one bit too narrow is a piece of the public internet that silently stops
// working.
func TestTheRoutableSpaceIsExactlyWhatItSaysItIs(t *testing.T) {
	t.Parallel()
	covered := func(ip netip.Addr) bool {
		for _, p := range routableSpace {
			if p.Contains(ip) {
				return true
			}
		}
		return false
	}
	for octet := range 256 {
		ip := netip.AddrFrom4([4]byte{byte(octet), 0, 0, 0})
		want := octet >= 1 && octet <= 223
		if got := covered(ip); got != want {
			t.Errorf("%s/8 covered=%v, want %v; the IPv4 space this host dials is "+
				"1.0.0.0 through 223.255.255.255, which is what the nine prefixes are "+
				"there to spell", ip, got, want)
		}
		// And the top of the /8, so a prefix that is right at its own boundary and
		// wrong in the middle is not passed by a first-address-only walk.
		top := netip.AddrFrom4([4]byte{byte(octet), 255, 255, 255})
		if got := covered(top); got != want {
			t.Errorf("%s covered=%v, want %v", top, got, want)
		}
	}
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"1fff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", false},
		{"2000::", true},
		{"3fff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", true},
		{"4000::", false},
	} {
		if got := covered(netip.MustParseAddr(tc.addr)); got != tc.want {
			t.Errorf("%s covered=%v, want %v; the IPv6 space this host dials is "+
				"2000::/3 and its edges are where an off-by-one lives", tc.addr, got, tc.want)
		}
	}
}

// --- the wiring ------------------------------------------------------------

// TestAFetchToLoopbackIsRefusedWhenTheNameHidesIt is the one that matters.
//
// The policy above is exercised without a socket, so on its own it proves that a
// function returns an error. This proves the function is *connected*: the fetcher
// is built exactly as an instance builds one, the address policy is untouched, and
// the URL names `localhost` — a name, resolved by the operating system, which no
// amount of reading the URL can tell from any other name. Only a check that runs
// after resolution and before connect sees 127.0.0.1 here.
//
// That is the DNS-rebinding shape without a DNS server: parse-time inspection
// passes it and dial-time inspection does not.
func TestAFetchToLoopbackIsRefusedWhenTheNameHidesIt(t *testing.T) {
	t.Parallel()
	reached := false
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(ts.Close)

	f := newTestFetcher(t, ts, keepTheAddressPolicy)
	port := mustURL(t, ts.URL).Port()

	for _, host := range []string{"localhost", "127.0.0.1"} {
		u := "https://" + host + ":" + port + "/.well-known/openid-configuration"
		got := f.fetch(context.Background(), "probe", fetchRequest{URL: u}, everywhere{})
		if got.Outcome != "address_refused" {
			t.Errorf("fetching %s answered %q, want address_refused", u, got.Outcome)
		}
	}
	if reached {
		t.Error("the server was reached: the address check is not in the dial path, " +
			"and everything else in this file is testing a function nothing calls")
	}
}

// **The cost D375 accepts, and the only thing that makes it payable.**
//
// An allowlist refuses something legitimate eventually — IPv6 space IANA allocates
// after this ships is the case to expect — and what the operator on the other end
// of that sees is an add-on reporting that a name will not resolve. So a refusal
// says *which rule* refused it, on its own log line, under a key worth grepping:
// `address_rule=`. A silent `address_refused` would leave them with nothing.
//
// Both rules that can reach the wire are here: the named one, which is what an
// operator meeting a misconfiguration sees, and the allowlist's own, which is what
// they see the day this policy is wrong.
func TestAnAddressRefusalNamesTheRuleThatRefusedIt(t *testing.T) {
	t.Parallel()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(ts.Close)

	var log strings.Builder
	f := newFetcher(2*time.Second, DefaultFetchMaxBytes,
		slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelWarn})))
	patchTLS(t, f, ts)
	port := mustURL(t, ts.URL).Port()

	for _, tc := range []struct {
		url  string
		rule string
		addr string
	}{
		// The literal rather than `localhost`, because this row asserts *which
		// address* the line names and a name does not decide that — a dual-stack
		// resolver answers `::1` first and CI's does, so the row demanded
		// 127.0.0.1 of a refusal that correctly named ::1 and was correctly
		// `loopback`. A *name* resolving to loopback is what
		// TestAFetchToLoopbackIsRefusedWhenTheNameHidesIt covers; this one is
		// about the log line, so it dials something unambiguous.
		{"https://127.0.0.1:" + port + "/x", "loopback", "127.0.0.1"},
		// Not in routableSpace and not in carvedOut: nothing in fetch.go mentions
		// it, and the log still says why.
		{"https://[4000::1]/x", "outside-routable-space", "4000::1"},
	} {
		log.Reset()
		if got := f.fetch(context.Background(), "oidc", fetchRequest{URL: tc.url}, everywhere{}); got.Outcome != "address_refused" {
			t.Fatalf("fetching %s answered %q, want address_refused", tc.url, got.Outcome)
		}
		line := log.String()
		for _, want := range []string{
			// The dedicated line, so this cannot be satisfied by the rule happening
			// to appear inside a wrapped error somebody later stops wrapping.
			`msg="this host refused to dial an address`,
			"address_rule=" + tc.rule,
			"address=" + tc.addr,
			`addon=oidc`,
		} {
			if !strings.Contains(line, want) {
				t.Errorf("the refusal of %s does not carry %s. An operator meeting a "+
					"name that will not resolve has this line and nothing else:\n%s",
					tc.url, want, line)
			}
		}
	}
}

// The other half of the wiring claim, and it is what keeps the test above from
// being satisfied by a fetcher that refuses everything: with the policy relaxed —
// and only then — the same server answers.
func TestAFetchReachesAServerWhenTheAddressIsPermitted(t *testing.T) {
	t.Parallel()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"issuer":"https://idp.test"}`)
	}))
	t.Cleanup(ts.Close)

	f := newTestFetcher(t, ts, permitLoopback)
	got := f.fetch(context.Background(), "probe", fetchRequest{URL: ts.URL + "/x"}, everywhere{})
	if got.Outcome != abi.FetchOK {
		t.Fatalf("outcome %q, want ok", got.Outcome)
	}
	if got.Status != http.StatusOK {
		t.Errorf("status %d, want 200", got.Status)
	}
	if got.Body != `{"issuer":"https://idp.test"}` {
		t.Errorf("body %q", got.Body)
	}
	if got.BodyBase64 {
		t.Error("a UTF-8 body came back base64")
	}
	if !strings.HasPrefix(got.ContentType, "application/json") {
		t.Errorf("content_type %q", got.ContentType)
	}
}

// --- what the host sends, and what it does not ------------------------------

// The header set is the host's whole answer to "what may an add-on put on the
// wire", because the ABI carries no header map at all. Asserted from the server's
// side rather than from the request the host built, so it is the bytes that are
// checked and not the intent.
func TestAnAddonNamesNoHeaderAndSendsNoCookie(t *testing.T) {
	t.Parallel()
	var seen http.Header
	var method, body, ctype string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		method = r.Method
		ctype = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(ts.Close)
	f := newTestFetcher(t, ts, permitLoopback)

	got := f.fetch(context.Background(), "oidc", fetchRequest{
		URL:    ts.URL + "/token",
		Method: http.MethodPost,
		Body:   "grant_type=authorization_code&code=abc",
	}, everywhere{})
	if got.Outcome != abi.FetchOK {
		t.Fatalf("outcome %q", got.Outcome)
	}
	if method != http.MethodPost {
		t.Errorf("method %q", method)
	}
	if body != "grant_type=authorization_code&code=abc" {
		t.Errorf("body %q did not arrive as written", body)
	}
	if ctype != "application/x-www-form-urlencoded" {
		t.Errorf("content-type %q: the host sets it and a token endpoint needs this one", ctype)
	}
	// The absences, which is what the claim is about. Each of these is a header an
	// add-on would set if the record carried a map, and each is a way around
	// something: Cookie sends this instance's own credentials, Host defeats the
	// origin check, Authorization sends a credential nobody granted.
	for _, h := range []string{"Cookie", "Authorization", "X-Forwarded-For", "Forwarded", "Referer"} {
		if v := seen.Get(h); v != "" {
			t.Errorf("the request carried %s: %q. This ABI has no header map and this "+
				"host adds none", h, v)
		}
	}
	if ua := seen.Get("User-Agent"); !strings.Contains(ua, "LinkCtrl/") || !strings.Contains(ua, "oidc") {
		t.Errorf("User-Agent %q: it names this product and the add-on the request is for", ua)
	}
}

// --- where a fetch may point ------------------------------------------------

// The origin allowlist, all three states, against a real server.
//
// The middle one is the milestone's central claim: *the host follows what a
// configured origin returns only on that same origin, and refuses a discovery
// document that points elsewhere*. A discovery document is JSON the guest reads
// and acts on, so the enforcement cannot be in the document — it is in the fetch
// the guest makes next, which is this.
func TestOnlyTheOriginsTheOperatorNamedAreReachable(t *testing.T) {
	t.Parallel()
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(idp.Close)
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(elsewhere.Close)

	f := newTestFetcher(t, idp, permitLoopback)
	// One transport reaches both, because both test servers are signed by the same
	// httptest certificate authority — which is what makes the refusal below a
	// refusal rather than a TLS failure wearing its clothes.
	patchTLS(t, f, elsewhere)

	configured := originSetOf(t, "provider_origins", originURL(t, idp))

	if got := f.fetch(context.Background(), "oidc",
		fetchRequest{URL: idp.URL + "/.well-known/openid-configuration"}, configured); got.Outcome != abi.FetchOK {
		t.Errorf("the configured origin answered %q, want ok", got.Outcome)
	}
	// The token endpoint a hostile or compromised discovery document points at.
	if got := f.fetch(context.Background(), "oidc",
		fetchRequest{URL: elsewhere.URL + "/token"}, configured); got.Outcome != "origin_refused" {
		t.Errorf("an origin nobody named answered %q, want origin_refused", got.Outcome)
	}
	// And nothing configured at all, which is what a freshly installed add-on is.
	if got := f.fetch(context.Background(), "oidc",
		fetchRequest{URL: idp.URL + "/x"}, originSet{settings: []string{"provider_origins"}}); got.Outcome != "unconfigured" {
		t.Errorf("an add-on with nothing configured answered %q, want unconfigured", got.Outcome)
	}
}

// A port is part of an origin, and an operator who named one has not authorized
// the others. Worth its own case because a comparison written on the hostname
// alone passes every other test in this file.
func TestAnOriginIsSchemeHostAndPort(t *testing.T) {
	t.Parallel()
	set := originSetOf(t, "origins", "https://idp.test:8443")
	for _, tc := range []struct {
		url     string
		permits bool
	}{
		{"https://idp.test:8443/token", true},
		{"https://idp.test:8443/", true},
		{"https://idp.test/token", false},
		{"https://idp.test:443/token", false},
		{"https://idp.test:9443/token", false},
		{"https://other.test:8443/token", false},
		{"https://idp.test.evil.test:8443/token", false},
	} {
		u := mustURL(t, tc.url)
		got, _ := set.permits(u)
		if got != tc.permits {
			t.Errorf("permits(%s) = %v, want %v", tc.url, got, tc.permits)
		}
	}
	// And the default port on both sides is the same origin, because an operator
	// who typed the URL out of a browser's address bar wrote one form of it.
	plain := originSetOf(t, "origins", "https://idp.test")
	for _, u := range []string{"https://idp.test/x", "https://idp.test:443/x"} {
		if ok, _ := plain.permits(mustURL(t, u)); !ok {
			t.Errorf("%s is not the origin https://idp.test, and it is", u)
		}
	}
}

// What an operator may write in an origin setting, and what authorizes nothing.
//
// The refusals matter more than the acceptances: each of them is somebody
// believing they narrowed the grant. A path does not narrow an origin — the host
// dials the origin, not the path — so accepting one and ignoring it would be the
// page lying to the person filling it in.
func TestWhatAnOperatorMayWriteInAnOriginSetting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{"https://idp.test", true},
		{"https://idp.test/", true},
		{"https://idp.test:8443", true},
		{"HTTPS://IDP.TEST", true},
		{"  https://idp.test  ", true},
		{"http://idp.test", false},
		{"idp.test", false},
		{"https://", false},
		{"https://idp.test/realms/main", false},
		{"https://idp.test/?x=1", false},
		{"https://idp.test#f", false},
		{"https://user:pass@idp.test", false},
		{"", false},
		{"not a url at all", false},
	} {
		_, err := parseOrigin(tc.raw)
		if tc.ok && err != nil {
			t.Errorf("parseOrigin(%q) refused it: %v", tc.raw, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("parseOrigin(%q) accepted it, and it is not an origin", tc.raw)
		}
	}
	// Case folding is not cosmetic: an operator who typed the host in capitals has
	// named the same origin, and a comparison that said otherwise would refuse them
	// their own configuration.
	o, err := parseOrigin("HTTPS://IDP.TEST")
	if err != nil || o.host != "idp.test" || o.port != "443" {
		t.Errorf("parseOrigin folded to %+v (%v)", o, err)
	}
}

// One field, several origins. Google's discovery document names three hosts, so an
// add-on whose author anticipated one field per origin would be an add-on nobody
// could point at it — and the origins are still every one of them the operator's.
func TestOneSettingCarriesSeveralOrigins(t *testing.T) {
	t.Parallel()
	set := originSetOf(t, "provider_origins",
		"https://accounts.example.test https://oauth2.example.test\nhttps://keys.example.test")
	for _, u := range []string{
		"https://accounts.example.test/.well-known/openid-configuration",
		"https://oauth2.example.test/token",
		"https://keys.example.test/certs",
	} {
		if ok, why := set.permits(mustURL(t, u)); !ok {
			t.Errorf("%s was refused as %q and it is one of the three named", u, why)
		}
	}
	if ok, _ := set.permits(mustURL(t, "https://evil.example.test/token")); ok {
		t.Error("an origin nobody named was permitted")
	}
}

// A value that is not an origin authorizes nothing and does not take the good
// entries with it. The direction is what matters: a parser that failed open here
// would turn an operator's typo into an unbounded grant.
func TestAMalformedOriginAuthorizesNothingAndBreaksNothing(t *testing.T) {
	t.Parallel()
	set := originSetOf(t, "provider_origins", "https://good.test not-a-url http://bare.test")
	if ok, _ := set.permits(mustURL(t, "https://good.test/x")); !ok {
		t.Error("the well-formed entry stopped working because a neighbour was malformed")
	}
	for _, u := range []string{"https://bare.test/x", "https://not-a-url/x"} {
		if ok, _ := set.permits(mustURL(t, u)); ok {
			t.Errorf("%s was permitted by a malformed setting value", u)
		}
	}
}

// --- redirects --------------------------------------------------------------

// A redirect is a second destination chosen by the first one, which is the whole
// reason it needs a rule: the operator authorized an origin, not whatever that
// origin decides to forward to.
func TestARedirectMayNotLeaveTheOriginItStartedOn(t *testing.T) {
	t.Parallel()
	away := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "reached somewhere else")
	}))
	t.Cleanup(away.Close)

	var target string
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, target, http.StatusFound)
		case "/here":
			http.Redirect(w, r, "/landed", http.StatusFound)
		case "/landed":
			_, _ = io.WriteString(w, "landed")
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		}
	}))
	t.Cleanup(idp.Close)
	target = away.URL + "/token"

	f := newTestFetcher(t, idp, permitLoopback)
	patchTLS(t, f, away)

	if got := f.fetch(context.Background(), "oidc",
		fetchRequest{URL: idp.URL + "/away"}, everywhere{}); got.Outcome != "redirect_refused" {
		t.Errorf("a redirect to another origin answered %q, want redirect_refused", got.Outcome)
	}
	// Same-origin redirects are followed, or an origin's own trailing-slash
	// normalisation would break the capability for no gain.
	got := f.fetch(context.Background(), "oidc", fetchRequest{URL: idp.URL + "/here"}, everywhere{})
	if got.Outcome != abi.FetchOK || got.Body != "landed" {
		t.Errorf("a same-origin redirect answered %q / %q, want ok / landed", got.Outcome, got.Body)
	}
	// And a chain that never ends is a loop somebody else controls the length of.
	if got := f.fetch(context.Background(), "oidc",
		fetchRequest{URL: idp.URL + "/loop"}, everywhere{}); got.Outcome != "redirect_refused" {
		t.Errorf("an endless redirect answered %q, want redirect_refused", got.Outcome)
	}
}

// The address check runs again on **every hop**, which is the property
// [fetcher.control] has and a parse-time check cannot — m68.5.md's *again on any
// address a redirect leads to — the rebinding case*.
//
// The shape is forced by what a redirect may be. A hop that changes the origin is
// refused by [fetcher.checkRedirect] before anything is dialled, so the only
// redirect that can reach the address policy at all is a **same-origin** one — and
// a same-origin hop is the same name, which means the second address can differ
// from the first only because the resolver answered differently. That is DNS
// rebinding exactly, and it is what the policy below imitates: the first dial is
// genuinely permitted, the server genuinely redirects, and the second dial of the
// same name is refused.
//
// So the first hop here is not refused, which the previous version of this test
// believed it was not and was wrong about — it pointed a loopback-refusing policy
// at a loopback server, refused hop one, and passed identically with the whole
// redirect limb deleted. Two things keep this one honest: the dial count is
// asserted, and the same fetch is run first under a policy that permits both
// dials, where it must reach the second hop's body.
func TestARedirectIsCheckedForItsAddressToo(t *testing.T) {
	t.Parallel()
	var paths []string
	var mu sync.Mutex
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/first" {
			// Same origin, so checkRedirect permits it and only the address policy
			// stands between this fetch and the second connection.
			http.Redirect(w, r, "/second", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "the second hop")
	}))
	t.Cleanup(idp.Close)

	// The control. Both dials permitted, so the redirect is followed and the body
	// is the second hop's — which is what makes the refusal below a statement about
	// the address check rather than about a redirect that never happened.
	f := newTestFetcher(t, idp, permitLoopback)
	dials := countingDials(f)
	got := f.fetch(context.Background(), "oidc", fetchRequest{URL: idp.URL + "/first"}, everywhere{})
	if got.Outcome != abi.FetchOK || got.Body != "the second hop" {
		t.Fatalf("with both dials permitted the fetch answered %q / %q, want ok / the second hop",
			got.Outcome, got.Body)
	}
	if n := dials(); n != 2 {
		t.Fatalf("the fetch dialled %d times; a followed redirect on a keep-alive-free "+
			"transport is two dials, and this test has nothing to say about a hop that "+
			"reused a connection", n)
	}

	// And the rebinding case. The name is the same on both hops; the address the
	// second one resolves to is one this host will not dial.
	f = newTestFetcher(t, idp, permitLoopback)
	var seen int
	f.allowAddr = func(ip netip.Addr) error {
		seen++
		if seen > 1 {
			return fmt.Errorf("%w: %s is refused on the second lookup", errAddressRefused, ip)
		}
		return nil
	}
	mu.Lock()
	paths = nil
	mu.Unlock()
	got = f.fetch(context.Background(), "oidc", fetchRequest{URL: idp.URL + "/first"}, everywhere{})
	if got.Outcome != "address_refused" {
		t.Errorf("outcome %q, want address_refused on the redirect's own dial", got.Outcome)
	}
	if seen != 2 {
		t.Errorf("the address policy ran %d times; the hop the redirect led to was not "+
			"checked", seen)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/first" {
		t.Errorf("the server saw %v; the first hop had to be served for there to be a "+
			"redirect, and the second had to be refused before it was", paths)
	}
}

// countingDials replaces a fetcher's address policy with one that counts and
// permits, and hands back the count. Whether a hop dialled at all is the fact this
// file's redirect test rests on, and it is not visible from the outcome.
func countingDials(f *fetcher) func() int {
	var n int
	inner := f.allowAddr
	f.allowAddr = func(ip netip.Addr) error {
		n++
		return inner(ip)
	}
	return func() int { return n }
}

// --- the size cap and the clock ---------------------------------------------

// A response over the cap comes back with no body at all. The alternative —
// truncating — hands an add-on a JSON document that will not parse and lets its
// author spend an afternoon on their own code.
func TestAResponseOverTheCapIsRefusedWhole(t *testing.T) {
	t.Parallel()
	var size int
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, size))
	}))
	t.Cleanup(ts.Close)

	f := newTestFetcher(t, ts, permitLoopback)
	f.maxBytes = 1024

	// Exactly the cap succeeds, which is what makes the bound a bound rather than
	// an approximation.
	size = 1024
	if got := f.fetch(context.Background(), "x", fetchRequest{URL: ts.URL}, everywhere{}); got.Outcome != abi.FetchOK {
		t.Errorf("a body of exactly the cap answered %q, want ok", got.Outcome)
	}
	size = 1025
	got := f.fetch(context.Background(), "x", fetchRequest{URL: ts.URL}, everywhere{})
	if got.Outcome != "too_large" {
		t.Fatalf("a body one byte over the cap answered %q, want too_large", got.Outcome)
	}
	if got.Body != "" {
		t.Errorf("too_large came back with %d bytes of body; a truncated document is a "+
			"parse error blamed on the wrong party", len(got.Body))
	}
}

// The timeout, and the composition the milestone asked about: a fetch spends the
// invocation's budget, so whichever ends first ends the fetch.
func TestAFetchEndsAtTheHostsTimeoutOrTheInvocationsBudget(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = io.WriteString(w, "late")
	}))
	t.Cleanup(func() { close(release); ts.Close() })

	f := newTestFetcher(t, ts, permitLoopback)
	f.timeout = 100 * time.Millisecond
	started := time.Now()
	if got := f.fetch(context.Background(), "x", fetchRequest{URL: ts.URL}, everywhere{}); got.Outcome != "timeout" {
		t.Errorf("outcome %q, want timeout", got.Outcome)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("the fetch took %s against a 100ms timeout", elapsed)
	}

	// And the other direction: a generous host timeout does not extend an
	// invocation that is nearly out of budget.
	f.timeout = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started = time.Now()
	if got := f.fetch(ctx, "x", fetchRequest{URL: ts.URL}, everywhere{}); got.Outcome != "timeout" {
		t.Errorf("outcome %q with a spent invocation budget, want timeout", got.Outcome)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("the fetch outlived its invocation by %s: a guest can buy time by "+
			"reaching outward", elapsed)
	}
}

// --- what a guest may ask for ------------------------------------------------

// The request record's own bounds. Each of these is refused rather than corrected,
// because a request that quietly did something other than what it said is how an
// add-on's author ends up debugging the host.
func TestWhatAFetchRequestMayCarry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		req  fetchRequest
		ok   bool
	}{
		{"a plain GET", fetchRequest{URL: "https://idp.test/x"}, true},
		{"GET named", fetchRequest{URL: "https://idp.test/x", Method: "GET"}, true},
		{"a form POST", fetchRequest{URL: "https://idp.test/t", Method: "POST", Body: "a=b"}, true},
		{"cleartext", fetchRequest{URL: "http://idp.test/x"}, false},
		{"no scheme", fetchRequest{URL: "idp.test/x"}, false},
		{"a file URL", fetchRequest{URL: "file:///etc/passwd"}, false},
		{"a gopher URL", fetchRequest{URL: "gopher://idp.test/"}, false},
		{"no host", fetchRequest{URL: "https:///x"}, false},
		{"credentials in the URL", fetchRequest{URL: "https://u:p@idp.test/x"}, false},
		{"a method nobody needs", fetchRequest{URL: "https://idp.test/x", Method: "PUT"}, false},
		{"DELETE", fetchRequest{URL: "https://idp.test/x", Method: "DELETE"}, false},
		{"lowercase get", fetchRequest{URL: "https://idp.test/x", Method: "get"}, false},
		{"a body on a GET", fetchRequest{URL: "https://idp.test/x", Body: "a=b"}, false},
		{"a URL past the bound", fetchRequest{URL: "https://idp.test/" + strings.Repeat("x", maxFetchURL)}, false},
		{"a body past the bound", fetchRequest{
			URL: "https://idp.test/t", Method: "POST", Body: strings.Repeat("x", maxFetchBody+1),
		}, false},
	} {
		_, why := checkFetchRequest(tc.req)
		if tc.ok && why != "" {
			t.Errorf("%s was refused: %s", tc.name, why)
		}
		if !tc.ok && why == "" {
			t.Errorf("%s was accepted, and it should not be", tc.name)
		}
	}
	// The methods the ABI publishes and the methods the host accepts are one set,
	// held together here rather than in two places that agree today.
	for _, m := range abi.FetchMethods {
		if _, why := checkFetchRequest(fetchRequest{URL: "https://idp.test/x", Method: m}); why != "" {
			t.Errorf("abi.FetchMethods carries %q and the host refuses it: %s", m, why)
		}
	}
}

// --- which invocations may fetch --------------------------------------------

// m68.5.md's answer for the redirect path, asserted rather than described: *the
// default answer is that egress is refused on the redirect path*, and the observe
// class is refused beside it because it has no caller whose budget a network round
// trip could be spent against.
//
// Driven through [hostState.doFetch] rather than through the fetcher, because the
// class is a property of the state and the whole point is that no manifest can
// change it.
//
// **It is below the dispatch gate, and that is a real limit on what it can say.**
// An inline invocation never reaches this function at all — `network_fetch` is
// outside abi.InlineSafe, so dispatch answers ErrDenied first — so the inline row
// below asserts the second line of defence rather than what a guest is told.
// TestNeitherRedirectClassMayFetchAndTheGuestIsToldSo is the one that drives a
// module above the gate and reports what each class actually receives. The third
// review of this milestone found this test cited as covering both, which it never
// could.
func TestOnlyARouteInvocationMayFetch(t *testing.T) {
	t.Parallel()
	m := Manifest{
		Name:        "oidc",
		Permissions: []string{abi.PermissionNetworkFetch},
		Settings:    []Setting{{Name: "provider_origins", Type: SettingText, Origin: true}},
	}
	grants, _ := resolveGrants(m)
	sink := &logSink{}
	base := newHostState(m, grants, nil,
		newSettingValues(map[string]config.Secret{"provider_origins": "https://idp.test"}),
		slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})), nil, false)
	base.fetcher = newFetcher(time.Second, 1024, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := fetchRequest{URL: "https://idp.test/.well-known/openid-configuration"}
	for _, tc := range []struct {
		name string
		st   *hostState
	}{
		{"the instance made at load", base},
		{"an inline redirect invocation", base.forRedirect(&RedirectDecision{Alias: "a"}, nil)},
		{"an observing invocation", base.forRedirect(nil, &RedirectEvent{LinkID: "l"})},
		{"a pooled inline instance", base.forPool(ClassInline)},
		{"a pooled observing instance", base.forPool(ClassObserve)},
	} {
		got := tc.st.doFetch(context.Background(), req)
		if got.Outcome != "class_refused" {
			t.Errorf("%s answered %q, want class_refused", tc.name, got.Outcome)
		}
	}

	// And the one that may. It reaches nothing here — idp.test does not resolve —
	// but the answer is a wire outcome rather than the class refusal, which is the
	// distinction this test is about.
	route := base.forRequest(&Request{Method: "GET", Path: "/"}, SessionContext{}, RequestIn{})
	if got := route.doFetch(context.Background(), req); got.Outcome == "class_refused" {
		t.Error("a route invocation was refused for its class, and it is the one that may fetch")
	}
}

// --- the unconfigured state --------------------------------------------------

// *An unconfigured add-on that talks outward is inert rather than trusting a
// default*, and the failure is a log line naming the setting — which is the
// difference between an operator who can fix it and one reading a refusal with no
// field attached to it.
func TestAnUnconfiguredAddonIsInertAndTheLogNamesTheSetting(t *testing.T) {
	t.Parallel()
	m := Manifest{
		Name:        "oidc",
		Permissions: []string{abi.PermissionNetworkFetch},
		Settings:    []Setting{{Name: "provider_origins", Type: SettingText, Origin: true}},
	}
	grants, _ := resolveGrants(m)
	sink := &logSink{}
	st := newHostState(m, grants, nil, nil,
		slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})), nil, false)
	st.fetcher = newFetcher(time.Second, 1024, slog.New(slog.NewTextHandler(io.Discard, nil)))
	st = st.forRequest(&Request{Method: "GET", Path: "/"}, SessionContext{}, RequestIn{})

	got := st.doFetch(context.Background(), fetchRequest{URL: "https://idp.test/x"})
	if got.Outcome != "unconfigured" {
		t.Fatalf("outcome %q, want unconfigured", got.Outcome)
	}
	logs := sink.String()
	if !strings.Contains(logs, "provider_origins") {
		t.Errorf("the refusal does not name the setting to fill in:\n%s", logs)
	}

	// And once it is configured, the same add-on is no longer inert — the origin
	// reaches the next call rather than the one after a restart, which is what
	// reading it off the holder at the call buys.
	st.values = newSettingValues(map[string]config.Secret{"provider_origins": "https://idp.test"})
	if got := st.doFetch(context.Background(), fetchRequest{URL: "https://idp.test/x"}); got.Outcome == "unconfigured" {
		t.Error("the add-on is still inert after its origin was configured")
	}
}

// --- the vocabulary ----------------------------------------------------------

// The vocabulary is closed, it is what a guest branches on, and it is what an
// operator reads off a counter. All three break the same way if the host answers
// with a word the ABI does not publish, so the mapping is asserted rather than
// reviewed.
func TestEveryOutcomeTheHostProducesIsInTheVocabulary(t *testing.T) {
	t.Parallel()
	known := map[string]bool{}
	for _, o := range abi.FetchOutcomes {
		known[o] = true
	}
	if !known[abi.FetchOK] {
		t.Fatalf("%q is not in the vocabulary it is supposed to be a member of", abi.FetchOK)
	}
	// Every word the failure classifier can return.
	for _, err := range []error{
		errAddressRefused, errRedirectRefused, context.DeadlineExceeded, context.Canceled,
		fmt.Errorf("something else entirely"),
	} {
		if got := fetchFailure(err); !known[got] {
			t.Errorf("fetchFailure(%v) = %q, which is not in abi.FetchOutcomes", err, got)
		}
	}
	// And the words written by hand elsewhere in this package, which is where a
	// typo would otherwise become a metric label nobody could group by.
	for _, o := range []string{
		"unconfigured", "origin_refused", "class_refused", "invalid_request",
		"dns_failed", "address_refused", "redirect_refused", "too_large", "timeout",
		"connect_failed",
	} {
		if !known[o] {
			t.Errorf("this package answers %q and the ABI does not publish it", o)
		}
	}
	if len(abi.FetchOutcomes) != 11 {
		t.Errorf("the vocabulary has %d members; it was written with eleven and a "+
			"change to it is a change to what a guest may branch on and to how many "+
			"series this counter can create", len(abi.FetchOutcomes))
	}
}

// **The two places the vocabulary is hand-copied and nothing checked it.**
//
// `abi.FetchOutcomes` is the vocabulary. The Add-on manager's meaning map is tied
// to it in both directions by httpx's TestEveryFetchOutcomeHasAnOperatorsReading,
// and the host's own mapping by the test above. These two were not: the `outcome`
// enum in api/openapi.yaml, which is what somebody writing against this API reads,
// and the `Help` on `linkctrl_addon_fetch_total`, which is what an operator reads
// off `/metrics` itself. A word added, renamed or removed in the ABI would leave
// both wrong and nothing would say so — and m68.5.md's own bullet is the one about
// four enumerations going stale in this phase.
//
// Set equality, not containment, and in both directions: a word the ABI publishes
// and a document omits is a reader who does not know it can happen, and a word a
// document publishes and the ABI does not is a promise nothing can keep. The
// pattern is auth's documented_scopes_test.go and store's cascade_test.go, which
// anchor a claim in openapi.yaml the same way.
func TestTheOutcomeVocabularyIsTheSameWhereverItIsEnumerated(t *testing.T) {
	t.Parallel()
	for _, doc := range []struct {
		path   string
		read   func(string) []string
		remedy string
	}{
		{
			path:   "../../api/openapi.yaml",
			read:   outcomeEnumInOpenAPI,
			remedy: "the `outcome` enum under AddonFetchSummary",
		},
		{
			path:   "../observability/metrics.go",
			read:   outcomeListInCounterHelp,
			remedy: "the parenthesised list in linkctrl_addon_fetch_total's Help",
		},
	} {
		t.Run(doc.path, func(t *testing.T) {
			b, err := os.ReadFile(doc.path)
			if err != nil {
				t.Fatalf("read %s: %v", doc.path, err)
			}
			got := doc.read(string(b))
			if len(got) == 0 {
				t.Fatalf("%s no longer carries an enumeration this test can find. If it "+
					"moved, move the anchor; if it was deleted, %s is where a reader "+
					"learned what an outcome can be", doc.path, doc.remedy)
			}
			if !slices.Equal(got, abi.FetchOutcomes) {
				t.Errorf("%s enumerates\n  %v\nand abi.FetchOutcomes is\n  %v\n"+
					"%s is hand-written and this is the only thing holding it to the "+
					"vocabulary a guest actually branches on", doc.path, got, abi.FetchOutcomes, doc.remedy)
			}
		})
	}
}

// outcomeEnumInOpenAPI reads the `- word` items of the enum whose description this
// anchor sits in. Anchored on the description rather than on indentation, because
// `enum:` appears dozens of times in that file and whitespace is not a claim.
func outcomeEnumInOpenAPI(text string) []string {
	const anchor = "A closed vocabulary, and the same words the add-on"
	i := strings.Index(text, anchor)
	if i < 0 {
		return nil
	}
	j := strings.LastIndex(text[:i], "enum:")
	if j < 0 {
		return nil
	}
	var out []string
	for _, line := range strings.Split(text[j:i], "\n") {
		if item, ok := strings.CutPrefix(strings.TrimSpace(line), "- "); ok {
			out = append(out, item)
		}
	}
	return out
}

// outcomeListInCounterHelp reads the words between the parentheses of that
// counter's Help, which is a Go string built from several literals — so the quotes
// and the concatenation come out before the commas are read.
func outcomeListInCounterHelp(text string) []string {
	i := strings.Index(text, `Name: "linkctrl_addon_fetch_total"`)
	if i < 0 {
		return nil
	}
	decl := text[i:]
	if end := strings.Index(decl, "}, []string{"); end > 0 {
		decl = decl[:end]
	}
	flat := strings.Join(strings.Fields(strings.NewReplacer(`"`, "", "+", "").Replace(decl)), " ")
	from := strings.Index(flat, "(")
	to := strings.Index(flat, ").")
	if from < 0 || to < from {
		return nil
	}
	var out []string
	for _, w := range strings.Split(flat[from+1:to], ",") {
		if w = strings.TrimSpace(w); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// The counters, which are how an operator sees an add-on talking outward at all.
// Both series and both label sets, from the path that writes them.
//
// **And the line between them**, which the third attempt at this milestone had to
// move: `address_refused` was being timed, so this milestone's headline refusal —
// decided in the Control hook, before connect(2) — was inflating the p99 of the
// add-on it protected. The rule the switch in doFetch now states is *who decided*,
// and the two rows below are the two sides of it: an origin nobody named and an
// address the policy will not dial are both this host's decisions and neither is
// timed, while the one request that was attempted is.
func TestAFetchIsCountedAndOnlyAnAttemptedOneIsTimed(t *testing.T) {
	t.Parallel()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(ts.Close)

	metrics := observability.NewMetrics()
	m := Manifest{
		Name:        "oidc",
		Permissions: []string{abi.PermissionNetworkFetch},
		Settings:    []Setting{{Name: "provider_origins", Type: SettingText, Origin: true}},
	}
	grants, _ := resolveGrants(m)
	st := newHostState(m, grants, nil,
		newSettingValues(map[string]config.Secret{
			// Two origins, and the second is the point: it is *configured*, so a fetch
			// to it clears the origin check and reaches the address policy, which is
			// the only way to produce an address_refused on the counted path.
			"provider_origins": config.Secret(originURL(t, ts) + " https://10.0.0.1"),
		}),
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil, false)
	st.metrics = metrics
	st.fetcher = newTestFetcher(t, ts, permitLoopback)
	route := st.forRequest(&Request{Method: "GET", Path: "/"}, SessionContext{}, RequestIn{})

	if got := route.doFetch(context.Background(), fetchRequest{URL: ts.URL + "/x"}); got.Outcome != abi.FetchOK {
		t.Fatalf("outcome %q", got.Outcome)
	}
	// A refusal this host decided before anything was dialled: counted, and
	// deliberately not timed.
	if got := route.doFetch(context.Background(),
		fetchRequest{URL: "https://elsewhere.test/x"}); got.Outcome != "origin_refused" {
		t.Fatalf("outcome %q", got.Outcome)
	}
	// And the one this milestone is named for. It is refused later than the two
	// above — the origin matched, the dial started, the Control hook stopped it —
	// and it is still this host's own decision, so it is still not a latency.
	if got := route.doFetch(context.Background(),
		fetchRequest{URL: "https://10.0.0.1/x"}); got.Outcome != "address_refused" {
		t.Fatalf("outcome %q, want address_refused", got.Outcome)
	}

	body := scrape(t, metrics)
	for _, want := range []string{
		`linkctrl_addon_fetch_total{addon="oidc",outcome="ok"} 1`,
		`linkctrl_addon_fetch_total{addon="oidc",outcome="origin_refused"} 1`,
		`linkctrl_addon_fetch_total{addon="oidc",outcome="address_refused"} 1`,
		// One, not three: the histogram's population is the attempt, and two of the
		// three calls above never became one.
		`linkctrl_addon_fetch_duration_seconds_count{addon="oidc"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape does not carry %s\n%s", want, addonSeries(body))
		}
	}
}

// --- the manifest ------------------------------------------------------------

// *A manifest naming a host anywhere is refused, so the capability cannot grow a
// second, quieter allowlist later.* The whole design rests on the destination
// being the operator's, and the only way a publisher could take it back is by
// writing one into the file the operator does not edit.
func TestAManifestCannotNameAHost(t *testing.T) {
	t.Parallel()
	base := func() Manifest {
		m := valid()
		m.Permissions = []string{abi.PermissionNetworkFetch}
		m.Settings = []Setting{{Name: "provider_origins", Type: SettingText, Origin: true}}
		return m
	}
	// The coherent shape validates, or every refusal below would be meaningless.
	if err := base().Validate(); err != nil {
		t.Fatalf("an add-on declaring the grant and an origin setting was refused: %v", err)
	}

	for _, tc := range []struct {
		name  string
		tweak func(*Manifest)
	}{
		{"a default origin", func(m *Manifest) { m.Settings[0].Default = "https://idp.example" }},
		{"a list of origins as options", func(m *Manifest) {
			m.Settings[0].Options = []string{"https://a.example", "https://b.example"}
		}},
		{"an origin setting that is a select", func(m *Manifest) {
			m.Settings[0].Type = SettingSelect
			m.Settings[0].Options = []string{"https://a.example", "https://b.example"}
		}},
		{"an origin setting that is a secret", func(m *Manifest) { m.Settings[0].Type = SettingSecret }},
		{"an origin setting that is a toggle", func(m *Manifest) { m.Settings[0].Type = SettingToggle }},
		{"a permission token carrying a URL", func(m *Manifest) {
			m.Permissions = []string{"network.fetch:https://idp.example"}
		}},
		{"an origin field with no grant behind it", func(m *Manifest) { m.Permissions = nil }},
	} {
		m := base()
		tc.tweak(&m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}

	// And the structural half: no field of a manifest may *be* a host, whatever a
	// later milestone decides it needs. The flag that marks an origin setting is a
	// boolean on purpose — it says *the operator fills this in* and cannot say
	// *fill it in with this*.
	if got := jsonFields(reflect.TypeOf(Setting{}))["origin"]; got == nil {
		t.Fatal("Setting has no `origin` field, so nothing above asserts what it claims")
	} else if got.Kind().String() != "bool" {
		t.Errorf("Setting.origin is %s; a manifest may say that an operator names an "+
			"origin and may never name one", got)
	}
	for name, typ := range jsonFields(reflect.TypeOf(Manifest{})) {
		switch name {
		case "host", "hosts", "origin", "origins", "url", "urls", "endpoint",
			"endpoints", "domain", "domains", "allowlist", "allowed_hosts":
			t.Errorf("Manifest carries a %q field of type %s: a destination in a manifest "+
				"is the allowlist this design refuses to have", name, typ)
		}
	}
}

// An add-on holding the grant with no origin field is legal and inert, and the
// boot log says so. Legal, because refusing it would make validation ask whether a
// declared capability is *useful*; said out loud, because an operator who granted
// something and sees nothing happen is owed the reason.
func TestTheGrantWithNoOriginSettingIsLegalAndSaidOutLoud(t *testing.T) {
	t.Parallel()
	m := valid()
	m.Permissions = []string{abi.PermissionNetworkFetch}
	if err := m.Validate(); err != nil {
		t.Errorf("an add-on declaring the grant and no origin setting was refused: %v", err)
	}
	if m.hasOriginSetting() {
		t.Error("a manifest with no settings reports an origin setting")
	}
	m.Settings = []Setting{{Name: "provider_origins", Type: SettingText, Origin: true}}
	if !m.hasOriginSetting() {
		t.Error("a manifest with an origin setting reports none")
	}
}

// --- helpers -----------------------------------------------------------------

// everywhere is the origin policy that permits anything, used by the tests whose
// subject is a bound *other* than the allowlist — the address policy, the redirect
// rule, the size cap, the clock.
//
// It exists because those bounds have to hold for a caller with no allowlist at
// all, which is m68.6.md's own requirement on this milestone: an install is
// authorized by the operator typing a URL rather than by a configured origin, so a
// bound written as *check the configured origin* would not transfer. A test that
// could only reach the address policy through an allowlist would be a test that
// never asked.
type everywhere struct{}

func (everywhere) permits(*url.URL) (bool, string) { return true, abi.FetchOK }

// The two address policies a test may run under, named rather than passed as a
// boolean so that a reader of a call site can see which claim is being made.
const (
	keepTheAddressPolicy = false
	permitLoopback       = true
)

// newTestFetcher builds a fetcher the way an instance does and then does exactly
// two things to it: it trusts the test server's certificate, and — only where the
// test says so — it permits loopback.
//
// The second is the compromise this file is honest about. A test server binds
// 127.0.0.1, which is the first address the policy refuses, so a suite that never
// relaxed it could not drive a single response through this code. What keeps that
// from hollowing out the claim is that the relaxation is per test and the two
// tests that are *about* the address check do not use it.
func newTestFetcher(t *testing.T, ts *httptest.Server, relax bool) *fetcher {
	t.Helper()
	f := newFetcher(5*time.Second, DefaultFetchMaxBytes,
		slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	patchTLS(t, f, ts)
	if relax {
		f.allowAddr = func(ip netip.Addr) error {
			if ip.Unmap().IsLoopback() {
				return nil
			}
			return refuseAddress(ip)
		}
	}
	return f
}

// patchTLS teaches a fetcher to trust a test server's certificate authority. Every
// httptest TLS server in one process shares one, so calling it twice is how a test
// reaches two servers through one transport.
func patchTLS(t *testing.T, f *fetcher, ts *httptest.Server) {
	t.Helper()
	tr, ok := f.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("the fetcher's transport is not an *http.Transport")
	}
	client, ok := ts.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("the test server's client transport is not an *http.Transport")
	}
	tr.TLSClientConfig = client.TLSClientConfig.Clone()
}

func originSetOf(t *testing.T, setting, value string) originSet {
	t.Helper()
	m := Manifest{Name: "oidc", Settings: []Setting{{Name: setting, Type: SettingText, Origin: true}}}
	st := newHostState(m, Grants{}, nil,
		newSettingValues(map[string]config.Secret{setting: config.Secret(value)}),
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil, false)
	return st.originsFor()
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return u
}

// originURL is a test server's URL reduced to its origin, which is what an
// operator would type into the setting.
func originURL(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	o, err := parseOrigin(ts.URL)
	if err != nil {
		t.Fatalf("%s is not an origin: %v", ts.URL, err)
	}
	return o.String()
}

// --- the route deadline ------------------------------------------------------

// m68.5.md's *first job*: say what bounds a fetching route handler, and build it.
//
// The answer is that every route invocation is bounded, not only a fetching one.
// A deadline conditional on a permission would leave a module that declared
// nothing able to hold an instance slot for as long as a visitor will wait, and
// would be a second rule to reason about for no gain — the hole is the same hole
// either way, and a capability that makes network calls is what turned it from
// latent into worth closing.
//
// Driven with a module that never returns, because that is the case with no other
// bound on it: the runtime is built WithCloseOnContextDone, so the deadline is
// what closes the instance underneath the loop.
func TestARouteInvocationIsBounded(t *testing.T) {
	h, _ := pagesHost(t, nil)
	h.routeDeadline = 300 * time.Millisecond

	started := time.Now()
	_, err := h.Route(context.Background(), "pages",
		RequestIn{Method: http.MethodGet, Path: "/spin"})
	elapsed := time.Since(started)

	if err == nil {
		t.Error("a handler that never returns answered a page")
	}
	if elapsed > 5*time.Second {
		t.Errorf("the invocation ran for %s against a 300ms deadline; nothing is "+
			"bounding a route handler", elapsed)
	}
	if deadline := h.deadlineForRoute(); deadline != 300*time.Millisecond {
		t.Errorf("the host reports a route deadline of %s", deadline)
	}
	if got := routeDeadlineFrom(0); got != DefaultRouteDeadline {
		t.Errorf("an unset route deadline is %s, want the default %s", got, DefaultRouteDeadline)
	}
}

// And the half above cannot see, which is what the first attempt at this milestone
// got wrong.
//
// [Host.Route] wraps the caller's context, and context.WithTimeout keeps whichever
// deadline is **earlier** — so a host bound is only ever the one that fires while
// it is shorter than what the caller brought. On the application path the caller
// brings LINKCTRL_HTTP_REQUEST_TIMEOUT's own context deadline, fifteen seconds, and
// a route deadline of fifteen therefore never fired once. Driving with
// context.Background() proves the host bound *exists* and is structurally unable to
// see that it never binds, so both directions are asserted here and
// internal/config refuses the ordering that makes the first one dead.
func TestTheRouteDeadlineNestsInsideTheCallersOwn(t *testing.T) {
	h, _ := pagesHost(t, nil)
	h.routeDeadline = 300 * time.Millisecond

	// Host shorter: the host's is what ends it, well before the caller's would.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	if _, err := h.Route(ctx, "pages", RequestIn{Method: http.MethodGet, Path: "/spin"}); err == nil {
		t.Error("a handler that never returns answered a page")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("the invocation ran for %s against a 300ms host deadline under a 10s "+
			"caller; the host's bound is not the one binding", elapsed)
	}
	if ctx.Err() != nil {
		t.Error("the caller's context was cancelled, so it was the caller's deadline " +
			"that fired and this test proves nothing about the host's")
	}

	// Caller shorter: the caller's is, and the host does not extend it. This is the
	// direction that makes the bound a *sub-request* one rather than a second
	// budget an add-on could spend.
	h.routeDeadline = 30 * time.Second
	short, cancelShort := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelShort()
	started = time.Now()
	if _, err := h.Route(short, "pages", RequestIn{Method: http.MethodGet, Path: "/spin"}); err == nil {
		t.Error("a handler that never returns answered a page")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("the invocation ran for %s against a 200ms caller; a host deadline of "+
			"30s extended somebody else's request", elapsed)
	}
}

// F-shaped, and the shape is logsafe.go's: a call site that escapes what it is
// about to log doubles the backslashes a second time, and the reader meets four of
// them where the error carries one.
//
// [hostState.originsFor] had it — moduleText(err.Error()) into the neutralizing
// logger — and so did [originString], which four log sites call. The value here is
// an operator's rather than a module's and is escaped all the same, so what this
// asserts is *once*, not *whether*.
//
// Read out of a JSON record rather than off the text handler's output, because
// counting backslashes through two quoting layers is how a test like this comes to
// assert the wrong number.
func TestAMalformedOriginReachesTheLogEscapedExactlyOnce(t *testing.T) {
	t.Parallel()
	const zeroWidth = "\u200b"
	sink := &logSink{}
	m := Manifest{Name: "escapes",
		Settings: []Setting{{Name: "provider_origins", Type: SettingText, Origin: true}}}
	st := newHostState(m, Grants{}, nil,
		newSettingValues(map[string]config.Secret{
			"provider_origins": config.Secret("https://idp.example.test/pa" + zeroWidth + "th"),
		}),
		neutralizingLogger(slog.New(slog.NewJSONHandler(sink, nil))), nil, false)

	if got := st.originsFor(); len(got.origins) != 0 {
		t.Fatalf("a malformed origin authorized %v", got.origins)
	}
	var rec struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(sink.String()), &rec); err != nil {
		t.Fatalf("the warning is not one JSON record: %v\n%s", err, sink.String())
	}
	// parseOrigin formats the path with %q, so the error already carries the six
	// characters `\u200b`. The handler escapes that one backslash and the reason
	// carries two; a call site that escaped first would make it four.
	if want := `\\u200b`; !strings.Contains(rec.Reason, want) {
		t.Fatalf("reason is %q and does not carry %s escaped once", rec.Reason, want)
	}
	if bad := `\\\\u200b`; strings.Contains(rec.Reason, bad) {
		t.Errorf("reason is %q — the call site pre-escaped what the handler escapes",
			rec.Reason)
	}
}

// internal/observability spells the success outcome itself, because it is imported
// by everything and cannot import the add-on ABI without inverting that. This is
// the seam that holds the two strings together, and it lives here because this is
// the package that can see both.
func TestTheMetricsPackageAndTheABIAgreeOnTheSuccessOutcome(t *testing.T) {
	t.Parallel()
	metrics := observability.NewMetrics()
	metrics.ObserveAddonFetch("oidc", abi.FetchOK, 5*time.Millisecond)
	metrics.ObserveAddonFetch("oidc", "timeout", 0)
	perf := metrics.AddonPerformance()["oidc"]
	if perf.Fetch.Refused != 1 {
		t.Errorf("refused = %d, want 1: %q is counted as a refusal, which means the "+
			"two packages disagree about which word means success", perf.Fetch.Refused, abi.FetchOK)
	}
	if perf.Fetch.Count != 1 {
		t.Errorf("count = %d, want 1", perf.Fetch.Count)
	}
}

// The `performance` object is published exactly when there is a record of either
// kind, and M68.5 is the milestone that could have broken that.
//
// `json:",omitzero"` on [Managed.Performance] compares the whole struct, and until
// this milestone that comparison and [observability.AddonPerformance.Observed]
// coincided because the only fields were the redirect ones. Fetch is a third
// field on a second path, so the coincidence ended: the shape below —
// `routes.own_prefix` and `network.fetch`, no redirect class, one outbound
// request — has an all-zero redirect record and a non-zero struct. Before
// [observability.AddonPerformance.IsZero] the encoder published a `performance`
// object for it while api/openapi.yaml said such an object cannot exist, and a
// client using absence to tell *no observations* from *fast* was reading a lie.
// Nothing else caught it: the object is schema-valid either way, so the false
// thing was the prose.
//
// The second assertion is the other half and is why IsZero is not simply
// Observed: a module that has only ever fetched must still read *unobserved* on
// the redirect path, because that is what the manager's list draws its dash from
// and what m68.md promised.
func TestThePerformanceObjectIsPublishedExactlyWhenThereIsARecord(t *testing.T) {
	t.Parallel()
	fetched := observability.AddonPerformance{
		Fetch: observability.AddonFetchStats{Count: 1, P99Seconds: 0.04, SumSeconds: 0.04},
	}
	for _, tc := range []struct {
		name string
		perf observability.AddonPerformance
		want bool
	}{
		{"nothing at all", observability.AddonPerformance{}, false},
		{"one redirect observation", observability.AddonPerformance{
			Classes: []observability.AddonRedirectStats{{Class: "inline", Count: 1}},
		}, true},
		{"a kill and nothing else", observability.AddonPerformance{
			Kills: observability.AddonKills{Call: 1},
		}, true},
		{"M69's add-on after one outbound request", fetched, true},
		{"a success counted with no duration beside it", observability.AddonPerformance{
			Fetch: observability.AddonFetchStats{Outcomes: []observability.AddonFetchOutcome{
				{Outcome: abi.FetchOK, Count: 1},
			}},
		}, true},
		{"a refusal that never dialled", observability.AddonPerformance{
			Fetch: observability.AddonFetchStats{Refused: 1, Outcomes: []observability.AddonFetchOutcome{
				{Outcome: "unconfigured", Count: 1},
			}},
		}, true},
	} {
		body, err := json.Marshal(Managed{Performance: tc.perf})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_, present := got["performance"]
		if present != tc.want {
			t.Errorf("%s: performance present = %t, want %t — the object the API "+
				"document describes and the object the encoder emits disagree\n%s",
				tc.name, present, tc.want, body)
		}
		if zero := tc.perf.IsZero(); zero == present {
			t.Errorf("%s: IsZero() = %t with the key present = %t; omitzero is not "+
				"asking this predicate", tc.name, zero, present)
		}
	}
	if fetched.Observed() {
		t.Error("a module that has only ever fetched reads as observed on the " +
			"redirect path; the manager's list would print zeros where m68.md " +
			"promised a dash")
	}
}

// The three guest-drivable refusals this milestone added do not log at Warn, and
// the rule that decides it is in [hostState.refusedFetch]'s doc comment.
//
// Each of the three is reachable from a guest at CPU speed with no socket opened:
// `class_refused` off an observing invocation, which visitor traffic drives, and
// the two configuration-shaped ones off a route handler, which holds a ten-second
// deadline on a publicly reachable page. All three are counted as
// `linkctrl_addon_fetch_total{addon,outcome}` and rendered per add-on by the
// manager, so the level would buy an operator nothing and cost an instance
// whatever a module cared to make it cost — the argument hostabi.go already made
// for the inline path, applied where it applies equally.
//
// The malformed-origin line is the control and is the same rule read the other
// way: nothing counts it and no page draws it, so the log is the only channel and
// it stays at Warn. It is also what proves this sink would have caught one.
// [hostState.storageFailed]'s denial is the other Warn that stays, for the same
// reason, and hostabi.go argues it in place.
func TestTheConfigurationShapedRefusalsDoNotWarn(t *testing.T) {
	t.Parallel()
	m := Manifest{
		Name:        "oidc",
		Permissions: []string{abi.PermissionNetworkFetch},
		Settings:    []Setting{{Name: "provider_origins", Type: SettingText, Origin: true}},
	}
	grants, _ := resolveGrants(m)

	warnState := func(t *testing.T, origins string) (*hostState, *logSink) {
		t.Helper()
		sink := &logSink{}
		var values *settingValues
		if origins != "" {
			values = newSettingValues(map[string]config.Secret{
				"provider_origins": config.Secret(origins),
			})
		}
		st := newHostState(m, grants, nil, values,
			slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelWarn})),
			nil, false)
		st.fetcher = newFetcher(time.Second, 1024,
			slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
		return st, sink
	}

	for _, tc := range []struct {
		name    string
		origins string
		of      func(*hostState) *hostState
		url     string
		outcome string
	}{
		{
			name: "class_refused, which visitor traffic drives", origins: "https://idp.test",
			of:      func(st *hostState) *hostState { return st.forRedirect(nil, &RedirectEvent{LinkID: "l"}) },
			url:     "https://idp.test/x",
			outcome: "class_refused",
		},
		{
			name: "unconfigured, which opens no socket at all",
			of: func(st *hostState) *hostState {
				return st.forRequest(&Request{Method: "GET", Path: "/"}, SessionContext{}, RequestIn{})
			},
			url:     "https://idp.test/x",
			outcome: "unconfigured",
		},
		{
			name: "origin_refused, which is refused before the dial", origins: "https://idp.test",
			of: func(st *hostState) *hostState {
				return st.forRequest(&Request{Method: "GET", Path: "/"}, SessionContext{}, RequestIn{})
			},
			url:     "https://elsewhere.test/x",
			outcome: "origin_refused",
		},
	} {
		base, sink := warnState(t, tc.origins)
		got := tc.of(base).doFetch(context.Background(), fetchRequest{URL: tc.url})
		if got.Outcome != tc.outcome {
			t.Fatalf("%s: outcome %q, want %q — the case is not the one being asserted",
				tc.name, got.Outcome, tc.outcome)
		}
		if line := sink.String(); line != "" {
			t.Errorf("%s: logged at warn or above, and a module can drive it as fast as "+
				"it likes:\n%s", tc.name, line)
		}
	}

	// The control: an operator's typo has no counter and no page, so it warns.
	st, sink := warnState(t, "not-an-origin")
	if got := st.originsFor(); len(got.origins) != 0 {
		t.Fatalf("a malformed origin authorized %v", got.origins)
	}
	if !strings.Contains(sink.String(), "provider_origins") {
		t.Errorf("the malformed-origin line is not at warn, and nothing else carries "+
			"it — an operator would never learn of their typo:\n%s", sink.String())
	}
}

// The response *headers* are bounded too, and before this they were not: Go's
// default is 10 MiB against a body cap of 256 KiB.
//
// configuration.md's argument for refusing an unbounded body is the same argument
// — the response is held in memory to cross the add-on boundary — and a server
// this product does not run does not have to send a body to spend this host's
// memory. A compromised IdP is inside docs/SECURITY.md's threat model for this
// door, so the bound is asserted rather than described.
//
// Both sides of it, because a bound only one side of which is tested is a number
// nobody has shown to bind.
func TestAResponseHeaderOverTheBoundIsRefused(t *testing.T) {
	t.Parallel()
	var size int
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Padding", strings.Repeat("p", size))
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(ts.Close)

	f := newTestFetcher(t, ts, permitLoopback)

	// Comfortably inside the bound: a real discovery document's headers are a
	// couple of hundred bytes and this is sixteen kilobytes of them.
	size = 16 << 10
	if got := f.fetch(context.Background(), "x", fetchRequest{URL: ts.URL}, everywhere{}); got.Outcome != abi.FetchOK {
		t.Errorf("a %d-byte header answered %q, want ok — the bound is refusing "+
			"headers a legitimate provider sends", size, got.Outcome)
	}

	// And past it. Well past, rather than one byte past: the transport counts the
	// whole block including the status line and Go's own framing, so an exact
	// boundary here would be asserting the arithmetic of net/http.
	size = int(maxResponseHeaderBytes) * 2
	got := f.fetch(context.Background(), "x", fetchRequest{URL: ts.URL}, everywhere{})
	if got.Outcome != "connect_failed" {
		t.Fatalf("a %d-byte header answered %q, want connect_failed", size, got.Outcome)
	}
	if got.Body != "" {
		t.Errorf("the refusal came back with %d bytes of body", len(got.Body))
	}
}
