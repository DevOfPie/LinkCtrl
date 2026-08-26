package addon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/build"
)

// This file is the one door in this product through which a server-side request
// reaches an address somebody else chose (M68.5).
//
// Everything else that takes a URL from outside refuses exactly this: a link's
// destination is checked by internal/domain's validator at a single call site
// held by test (M30), and M67's install reads bytes from a request body rather
// than fetching them. So the machinery here is not a convenience wrapper around
// net/http — it is the bounds, and net/http is what it drives.
//
// # Two layers, and they are deliberately separable
//
// **Who may be dialled at all** is [refuseAddress], and it is a property of the
// address rather than of anybody's configuration: an address is dialled only if it
// falls in globally-routable unicast space and is refused otherwise, checked at
// the moment the connection is made. **Where a particular caller may
// point** is the origin allowlist, which for an add-on is what its operator wrote
// in a setting and for [M68.6](../../docs/build-notes/phase-details/m68.6.md)'s
// URL install will be the URL the operator typed.
//
// The separation is that milestone's own requirement, written into its file as a
// risk before this one was built: *if M68.5's bounds are written as "check the
// configured origin" rather than as "check this address", they will not
// transfer*. So [fetcher] takes the origin policy as an argument and enforces the
// address policy itself, and a caller that has no allowlist at all still cannot
// reach 169.254.169.254.
//
// # Why the check is at dial time
//
// Resolving a name and then connecting to it is two operations, and a resolver
// that answers differently the second time is the whole of DNS rebinding. Go's
// [net.Dialer] has a Control hook that runs **after** the address is resolved and
// **before** connect(2), with the address actually about to be dialled — so the
// check has nothing to race against, and it runs again on every hop of a redirect
// chain because every hop dials. Parsing the URL and checking what it looks like
// would be the version of this that a hostile name defeats by answering twice.

// maxFetchRedirects is how many hops one fetch may take.
//
// Three, and every one of them has to stay on the origin the fetch started on —
// see [fetcher.checkRedirect]. It is not zero because an origin's own
// `http://…/x` → `…/x/` normalisation is ordinary and refusing it would make this
// capability fail on well-behaved servers for no security gain, and it is small
// because a chain is a loop somebody else controls the length of.
const maxFetchRedirects = 3

// DefaultFetchTimeout bounds one outbound request, connect through last byte of
// body.
//
// Three seconds, and the measurement behind it is what an OIDC relying party
// actually fetches: on 2026-08-26 the discovery documents of four public
// providers were 839, 1,217, 1,399 and 1,728 bytes and their JWKS documents 2,880,
// 5,547 and 12,852 — documents small enough that the time is a round trip and a
// TLS handshake rather than a transfer. Three seconds is an order of magnitude
// over what that costs, and it is the largest value at which three of them —
// discovery, token exchange, key set, which is what an authorization-code flow
// makes — still fit inside [DefaultRouteDeadline], which itself has to fit inside
// the request deadline. What internal/config **enforces** is the pair of nestings —
// this may not exceed the route deadline, and that may not reach the request
// timeout; what a test asserts is the three-fetch arithmetic above, because it is a
// claim about the *defaults* rather than a rule an operator has to obey. The first
// attempt at this milestone sized this number against a budget that did not exist,
// and neither of those two is a comment.
//
// It is a ceiling and not a reservation: the fetch is bounded by this *or* by
// whatever is left of the invocation's own deadline, whichever comes first, so an
// add-on cannot buy time by fetching.
const DefaultFetchTimeout = 3 * time.Second

// DefaultFetchMaxBytes is how large a response body this host will accept.
//
// 256 KiB, which is twenty times the largest of the eight documents measured for
// [DefaultFetchTimeout] and the same number [maxResponseBody] uses for what an
// add-on may answer with — one figure for what crosses this boundary in either
// direction is one figure for an operator to reason about.
//
// **A response over it comes back with no body at all**, as the `too_large`
// outcome. Truncating would hand an add-on a JSON document that fails to parse
// and let its author blame their own code.
const DefaultFetchMaxBytes int64 = 256 << 10

// maxResponseHeaderBytes is how much response *header* this host will read before
// it gives up on the exchange.
//
// It exists because [DefaultFetchMaxBytes] bounds the body and nothing bounded the
// headers: Go's default is 10 MiB, forty times the body cap, and configuration.md's
// argument for refusing an unbounded body applies to a header field verbatim — *the
// response is held in memory to cross the add-on boundary and an unbounded body
// from a server this product does not run is an unbounded heap*. A compromised or
// hostile IdP is inside the threat model docs/SECURITY.md names for this door, and
// it does not have to send a body to spend this host's memory.
//
// 64 KiB, which is eight times the largest header block ordinary servers will
// *emit* — nginx and Apache both refuse a request field over about 8 KiB and no
// discovery document or token response carries more than a few — and small enough
// that sixteen slots fetching at once is megabytes rather than gigabytes. Not
// configurable, deliberately: the two bounds an operator turns are the ones a
// legitimate provider can plausibly need turned, and no provider needs this one.
//
// **The outcome is `connect_failed`**, because the exchange failed below the
// response: the transport raises an ordinary error and [fetchFailure] classifies
// it as it classifies any other wire failure. `too_large` stays what it says on
// the ABI's own table — the body was over the cap — rather than becoming two
// different faults wearing one word, and reading the transport's error text to
// tell them apart would rest a documented outcome on a string Go may change.
const maxResponseHeaderBytes int64 = 64 << 10

// DefaultRouteDeadline is how long one request to an add-on's own route may take.
//
// **It is a sub-request bound, and what makes it one is a number the first attempt
// at this milestone did not look at.** A route runs under the application tree's
// request context, and internal/httpx bounds that with
// LINKCTRL_HTTP_REQUEST_TIMEOUT — a *context* deadline, fifteen seconds by
// default, started strictly earlier than this one. So a route deadline of fifteen
// seconds never fired: the request deadline always closed the spinning instance
// first, measured at 300.7ms under a 300ms parent. The comment that defended the
// old number argued against LINKCTRL_HTTP_WRITE_TIMEOUT, which is thirty seconds
// and does not cancel a context, and so was arguing against the wrong bound
// entirely.
//
// Ten seconds, and two things fix it once the request deadline is in view:
//
//   - **It has to fire first, and the margin is what it buys.** When the request
//     deadline is what elapses, host and guest end together and there is nothing
//     left of the budget to turn the failure into a page, a log line or a counter.
//     Five seconds under it means the host closes the guest, sees ErrGuestFailed,
//     and still has a request to answer with. It is also the only bound at all
//     when an operator sets LINKCTRL_HTTP_REQUEST_TIMEOUT to zero, which disables
//     that middleware outright.
//   - **It has to hold three fetches at [DefaultFetchTimeout]**, which is what an
//     authorization-code flow costs — discovery, token exchange, key set. Nine of
//     the ten seconds.
//
// Neither relationship is left to arithmetic in a comment. internal/config refuses
// a fetch timeout over this and a value of this at or over the request timeout, for
// the reason it already refuses FEED_TIMEOUT over the request timeout — a knob whose
// upper half cannot take effect is not a knob. The three-fetch fit is a claim about
// the shipped defaults rather than a rule, so a test asserts it instead:
// TestTheAddonEgressBoundsNestInsideTheRequestDeadline.
//
// **It is not a latency target.** A page an add-on draws is on the dashboard's
// budget like any other, and this is the point at which the host stops waiting for
// somebody else's code — the same thing [DefaultInlineDeadline] is for the redirect
// path, three orders of magnitude apart because the two paths promise different
// things. What it buys is that a module which loops, or which fetches in a loop,
// gives an instance slot back.
const DefaultRouteDeadline = 10 * time.Second

// maxFetchURL and maxFetchBody bound what a guest may put in one FetchRequest.
//
// Neither is a security property — [maxStringIn] already stops a guest handing
// the host a gigabyte — and both are here so that a refusal names the field.
const (
	maxFetchURL  = 2048
	maxFetchBody = 8 << 10
)

// The refusals that have to be told apart from an ordinary connection failure,
// because each is a different word in [abi.FetchOutcomes] and an add-on branches
// on them differently.
var (
	errAddressRefused  = errors.New("this host does not dial that address")
	errRedirectRefused = errors.New("a redirect left the origin it started on")
)

// # The address policy is an allowlist (D375)

// routableSpace is the space this host will dial, and an address outside it is
// refused without anybody having had to think of it.
//
// **This is an allowlist because a denylist of it was found short in three of four
// reviews of this file** — four IPv4 special-purpose entries at the first, six IPv6
// entries at the second, and `fec0::/10` at the fourth, which is deprecated IPv6
// site-local and sits in IANA's IPv6 Address Space registry rather than in the
// special-purpose registry the old list's claim was scoped to. That claim was
// honest and the hole was real at the same time, which is what a completeness
// claim over somebody else's document is worth.
//
// Inverting it changes the failure mode, and that is the whole argument: a range
// nobody thought of is now **refused** rather than dialled, so the symptom of
// being wrong is an operator reporting that a legitimate origin will not resolve
// instead of an SSRF nobody observes. It will be wrong that way eventually — IPv6
// space IANA allocates after this ships is the case to expect — which is why every
// refusal below names the rule that made it and is greppable. See [addressRefusal].
//
//   - **IPv4** is the unicast space delegated to the regional registries,
//     1.0.0.0 through 223.255.255.255, as the nine prefixes that cover exactly that
//     range and nothing either side of it. `0.0.0.0/8` is below it and multicast,
//     the reserved `240.0.0.0/4` and the limited broadcast address are above it, so
//     none of the four needs an entry anywhere.
//   - **IPv6** is `2000::/3`, which is the only block IANA allocates global unicast
//     from. Unique-local `fc00::/7`, link-local `fe80::/10`, site-local `fec0::/10`,
//     multicast `ff00::/8`, the NAT64 prefixes, the discard prefix, IPv4-compatible
//     `::/96` and the SRv6 SID block `5f00::/16` are all outside it and are refused
//     by this line rather than by an entry somebody had to add.
//
// The exactness of both is asserted rather than eyeballed:
// TestTheRoutableSpaceIsExactlyWhatItSaysItIs walks every IPv4 /8 and both edges
// of the IPv6 block.
var routableSpace = []netip.Prefix{
	netip.MustParsePrefix("1.0.0.0/8"),
	netip.MustParsePrefix("2.0.0.0/7"),
	netip.MustParsePrefix("4.0.0.0/6"),
	netip.MustParsePrefix("8.0.0.0/5"),
	netip.MustParsePrefix("16.0.0.0/4"),
	netip.MustParsePrefix("32.0.0.0/3"),
	netip.MustParsePrefix("64.0.0.0/2"),
	netip.MustParsePrefix("128.0.0.0/2"),
	netip.MustParsePrefix("192.0.0.0/3"),
	netip.MustParsePrefix("2000::/3"),
}

// carvedOut is what sits inside [routableSpace] and is still not a destination.
//
// **It is a set of exceptions and not the mechanism.** Forgetting an entry here
// leaves one range reachable that should not be; forgetting a range that is not in
// [routableSpace] leaves nothing reachable at all, because the default is refusal.
// Every entry is inside routable space and is refused there —
// TestEveryCarvedOutRangeIsInsideRoutableSpaceAndRefused holds both halves, so an
// entry that stops meaning anything fails rather than sitting here looking like a
// bound.
var carvedOut = []netip.Prefix{
	// RFC 1918, and the reason this file exists: an add-on pointed at a private
	// range is an add-on reading somebody's internal network from inside their
	// perimeter.
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	// Carrier-grade NAT (RFC 6598). Not private by RFC 1918's definition and routed
	// as if it were: on a host behind a CGNAT it reaches neighbours.
	netip.MustParsePrefix("100.64.0.0/10"),
	// Loopback and link-local, which the named predicates in [refuseAddress] reach
	// first and which are here anyway: they are the only two ranges off the public
	// internet that sit *inside* the IPv4 cover above, and with them here the two
	// lists are the whole policy and the predicates are only naming. The table in
	// fetch_test.go asserts that, address by address.
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	// IETF protocol assignments (RFC 6890), which holds 192.0.0.8 and the NAT64
	// discovery addresses; benchmarking (RFC 2544); documentation (RFC 5737, RFC
	// 3849); the 6to4 relay anycast (RFC 7526); AMT (RFC 7450), AS112-v4 (RFC 7535)
	// and the direct delegation AS112 service (RFC 7534). None is a destination and
	// every one is global unicast by the stdlib's reckoning.
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.175.48.0/24"),
	// IANA's IPv6 IETF Protocol Assignments block (RFC 2928), refused whole rather
	// than entry by entry: everything in it is delegated to the IETF and nothing in
	// it is a host anybody reaches — Teredo, benchmarking, AMT, AS112-v6, both
	// ORCHID generations, the PCP, TURN and DNS-SD anycasts and DRIP — so a
	// carve-out IANA makes inside it is refused before it is written.
	netip.MustParsePrefix("2001::/23"),
	// Documentation (RFC 3849 and RFC 9637), 6to4 (which embeds an IPv4 address
	// this policy would otherwise have to decode through a second parser), and the
	// AS112-v6 delegation. `3fff::/20` is the sharper of the two documentation
	// blocks: it is what examples and tutorials now use, so it is what an operator
	// copies out of one.
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
}

// addressRefusal is one refused address and the rule that refused it.
//
// **The rule is a token because D375's cost lands on an operator.** An allowlist
// refuses something legitimate eventually, and the only thing standing between
// that and a name that mysteriously will not resolve is a log line somebody can
// grep. Every refusal that reaches the wire is logged with `address_rule=` and one
// of the tokens below, and docs/operations.md tells an operator to grep for it.
//
// It wraps [errAddressRefused] rather than replacing it, so [fetchFailure] keeps
// mapping the whole family to the `address_refused` outcome.
type addressRefusal struct {
	rule string
	addr netip.Addr
	why  string
}

func (r *addressRefusal) Error() string {
	if r.addr.IsValid() {
		return fmt.Sprintf("%s: %s %s [address_rule=%s]", errAddressRefused, r.addr, r.why, r.rule)
	}
	return fmt.Sprintf("%s: %s [address_rule=%s]", errAddressRefused, r.why, r.rule)
}

func (r *addressRefusal) Unwrap() error { return errAddressRefused }

// refuseAddress is the address policy, and it is the half of this file that has
// nothing to do with who is calling.
//
// Two steps, and only the second is the mechanism. The named predicates come first
// because "loopback" is worth more to whoever reads the log than "outside routable
// space"; each of them is refused by [refuseByList] as well, and the table in
// fetch_test.go asserts that address by address, so removing one would cost a
// legible refusal and nothing else.
func refuseAddress(ip netip.Addr) error {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return &addressRefusal{rule: "not-an-address", why: "it is not an address"}
	}
	switch {
	case ip.IsUnspecified():
		return &addressRefusal{rule: "unspecified", addr: ip, why: "is the unspecified address"}
	case ip.IsLoopback():
		return &addressRefusal{rule: "loopback", addr: ip, why: "is loopback"}
	case ip.IsLinkLocalUnicast():
		// 169.254.0.0/16 and fe80::/10. The cloud metadata services live here and
		// they are the single most valuable thing an SSRF reaches.
		return &addressRefusal{rule: "link-local", addr: ip, why: "is link-local"}
	case ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return &addressRefusal{rule: "multicast", addr: ip, why: "is multicast"}
	case !ip.IsGlobalUnicast():
		return &addressRefusal{rule: "not-global-unicast", addr: ip, why: "is not global unicast"}
	}
	return refuseByList(ip)
}

// refuseByList is the allowlist itself: in [routableSpace], out of [carvedOut], or
// refused.
func refuseByList(ip netip.Addr) error {
	routable := false
	for _, p := range routableSpace {
		if p.Contains(ip) {
			routable = true
			break
		}
	}
	if !routable {
		return &addressRefusal{
			rule: "outside-routable-space",
			addr: ip,
			why: "is outside the globally-routable unicast space this host dials, " +
				"which is 1.0.0.0/8 through 223.255.255.255 and 2000::/3",
		}
	}
	for _, p := range carvedOut {
		if p.Contains(ip) {
			return &addressRefusal{
				rule: "carved-out",
				addr: ip,
				why:  "is in " + p.String() + ", which is inside routable space and is not a destination",
			}
		}
	}
	return nil
}

// origin is a scheme, a host and a port, which is the whole of what an operator
// authorizes when they name one.
//
// A value rather than a *url.URL because two of them are compared for equality on
// every fetch and a URL carries eight fields that must not participate: a path, a
// query and a fragment are not part of an origin, and an operator who typed one
// has not thereby narrowed what they authorized — they have written something this
// host has to normalise away rather than half-honour.
type origin struct {
	scheme string
	host   string
	port   string
}

func (o origin) String() string { return o.scheme + "://" + net.JoinHostPort(o.host, o.port) }

// originOf reduces a URL to its origin. https only: this capability exists so an
// add-on can reach an identity provider, and reaching one over cleartext is not
// what "as securely as possible" admits.
func originOf(u *url.URL) (origin, error) {
	if u == nil || u.Scheme != "https" {
		return origin{}, fmt.Errorf("only https is reachable from an add-on")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return origin{}, fmt.Errorf("it names no host")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return origin{scheme: u.Scheme, host: host, port: port}, nil
}

// parseOrigin reads one origin out of what an operator typed.
//
// Strict about what it accepts and silent about what it discards, which is the
// wrong combination for a security control — so it accepts only what an origin
// *is*. A trailing slash is tolerated because a browser's address bar produces
// one; a path, a query, a fragment or credentials are refused, because each of
// them is an operator believing they narrowed the grant when an origin cannot be
// narrowed that way.
func parseOrigin(raw string) (origin, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return origin{}, fmt.Errorf("it is not a URL")
	}
	if u.User != nil {
		return origin{}, fmt.Errorf("an origin carries no credentials")
	}
	if u.Path != "" && u.Path != "/" {
		return origin{}, fmt.Errorf("an origin is a scheme, a host and a port, and %q is a path", u.Path)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return origin{}, fmt.Errorf("an origin carries no query and no fragment")
	}
	return originOf(u)
}

// originPolicy answers whether one URL may be fetched, and it is the argument
// [fetcher] takes rather than the rule it holds.
//
// An add-on's policy is [originSet], built from the settings its operator filled
// in. M68.6's URL install will pass one built from the URL the operator typed,
// because an install is authorized by `addons.manage` and the typing rather than
// by an allowlist — which is precisely why this is a parameter.
type originPolicy interface {
	// permits reports whether this URL's origin is one the caller authorized, and
	// when it is not, the outcome to report: an add-on with nothing configured is
	// `unconfigured` and one pointed somewhere else is `origin_refused`, and an
	// operator debugging the two does different things about them.
	permits(u *url.URL) (bool, string)
}

// originSet is the set of origins one add-on's operator named, with the settings
// they came from.
type originSet struct {
	origins []origin
	// settings is the names of the origin-carrying settings this add-on declared,
	// in manifest order. Held so that the refusal can say *which field to fill
	// in*, which is the difference between a log line an operator can act on and
	// one they can only read.
	settings []string
}

func (s originSet) permits(u *url.URL) (bool, string) {
	if len(s.origins) == 0 {
		return false, "unconfigured"
	}
	got, err := originOf(u)
	if err != nil {
		return false, "origin_refused"
	}
	for _, o := range s.origins {
		if o == got {
			return true, abi.FetchOK
		}
	}
	return false, "origin_refused"
}

// fetchRequest is what a guest asked for, decoded and checked.
type fetchRequest struct {
	URL    string `json:"url"`
	Method string `json:"method,omitempty"`
	Body   string `json:"body,omitempty"`
}

// fetchResponse is what comes back. It mirrors abi's FetchResponse record field
// for field, and abi_test.go's surface assertions are what hold the two together.
type fetchResponse struct {
	Outcome     string `json:"outcome"`
	Status      int    `json:"status,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Body        string `json:"body,omitempty"`
	BodyBase64  bool   `json:"body_base64,omitempty"`
}

// fetcher is the host's outbound HTTP client, and there is one per host.
//
// One rather than one per invocation because a Transport is expensive to build
// and cheap to share, and because sharing it changes nothing about isolation:
// keep-alives are off, so no connection outlives the fetch that opened it and no
// two add-ons can be handed the same socket. That is m68.5.md's *connection
// pooling or keep-alive across invocations* held to deliberately — a pooled
// connection would outlive the instance the pool hands back, and an add-on
// removed at runtime would leave one open to somebody else's server.
type fetcher struct {
	client   *http.Client
	timeout  time.Duration
	maxBytes int64
	log      *slog.Logger

	// allowAddr is [refuseAddress] in every build. It is a field because a test
	// that binds a server has to bind it to loopback, and a suite that could only
	// exercise this path by relaxing the *policy* would never exercise the wiring
	// — so the policy is tested directly against the table in fetch_test.go, and
	// the wiring is tested by leaving this alone and watching a real dial to
	// 127.0.0.1 be refused.
	allowAddr func(netip.Addr) error
}

func newFetcher(timeout time.Duration, maxBytes int64, log *slog.Logger) *fetcher {
	f := &fetcher{timeout: timeout, maxBytes: maxBytes, log: log, allowAddr: refuseAddress}
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: -1,
		Control:   f.control,
	}
	f.client = &http.Client{
		CheckRedirect: f.checkRedirect,
		Transport: &http.Transport{
			// **No proxy, and it is the environment's that is being refused.**
			// http.ProxyFromEnvironment would hand every one of these requests to
			// whatever HTTPS_PROXY names, and a proxy connects on this host's behalf
			// to an address the Control hook below never sees. The whole address
			// policy would then be advisory.
			Proxy:                  nil,
			DialContext:            dialer.DialContext,
			DisableKeepAlives:      true,
			ForceAttemptHTTP2:      true,
			MaxIdleConns:           0,
			IdleConnTimeout:        time.Second,
			TLSHandshakeTimeout:    timeout,
			ExpectContinueTimeout:  time.Second,
			ResponseHeaderTimeout:  timeout,
			MaxResponseHeaderBytes: maxResponseHeaderBytes,
			// The body's cap is [fetcher.maxBytes] and this is the headers', which
			// nothing bounded before: Go's default is 10 MiB. See the constant.

		},
	}
	return f
}

// control is the dial-time address check, and it is the load-bearing line in this
// file.
//
// net.Dialer calls it once per address it is about to connect to, after
// resolution, with that address — so a name resolving to several is checked
// several times, a name that resolves to something else on the second lookup is
// checked again on the second lookup, and a redirect that dials afresh is checked
// afresh. There is no window between the check and the connect for anything to
// change in.
func (f *fetcher) control(network, address string, _ syscall.RawConn) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return &addressRefusal{rule: "not-a-tcp-network", why: network + " is not a network this host dials"}
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return &addressRefusal{rule: "unparseable-address", why: address + " is not an address and a port"}
	}
	return f.allowAddr(ap.Addr())
}

// checkRedirect refuses any hop that leaves the origin the fetch began on.
//
// Compared against the **first** request rather than against the previous one, so
// a chain cannot walk to a new origin one hop at a time — which same-origin-with-
// the-previous-hop would permit and is the shape this bound exists to refuse. A
// discovery document that redirects its token endpoint to somebody else's server
// is the attack; it is refused here and again by the origin policy, because the
// two are different doors and an add-on following the document itself comes
// through the other one.
func (f *fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxFetchRedirects {
		return fmt.Errorf("%w: more than %d hops", errRedirectRefused, maxFetchRedirects)
	}
	first, err := originOf(via[0].URL)
	if err != nil {
		return fmt.Errorf("%w: %w", errRedirectRefused, err)
	}
	next, err := originOf(req.URL)
	if err != nil {
		return fmt.Errorf("%w: %w", errRedirectRefused, err)
	}
	if first != next {
		return fmt.Errorf("%w: %s pointed at %s", errRedirectRefused, first, next)
	}
	return nil
}

// fetch makes one outbound request, or says why it did not.
//
// It never returns an error: every way this can fail is one of
// [abi.FetchOutcomes], because the whole point of that vocabulary is that a guest
// branches on it and an operator reads the same word off a counter. The host's own
// log carries the detail, which is where a URL an add-on chose belongs.
func (f *fetcher) fetch(ctx context.Context, addon string, req fetchRequest, policy originPolicy) fetchResponse {
	u, why := checkFetchRequest(req)
	if why != "" {
		f.log.Debug("refused an add-on's outbound request",
			slog.String("addon", addon), slog.String("reason", why))
		return fetchResponse{Outcome: "invalid_request"}
	}
	if ok, outcome := policy.permits(u); !ok {
		return fetchResponse{Outcome: outcome}
	}

	// The host's timeout *or* what is left of the invocation's, whichever ends
	// first — which is what makes "a fetch spends the call's budget" true rather
	// than a sentence in a document. A guest cannot buy time by fetching.
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(req.Body)
	}
	hr, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return fetchResponse{Outcome: "invalid_request"}
	}
	// The whole header set, and it is the host's. An add-on names no header: a
	// header is the shape through which a request grows a credential it was not
	// granted, a Host override that defeats the origin check, or a cookie nobody
	// declared a prefix for. What an OIDC exchange needs is exactly these.
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("User-Agent", "LinkCtrl/"+build.Get().Version+" (+add-on "+addon+")")
	if method == http.MethodPost {
		hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := f.client.Do(hr)
	if err != nil {
		outcome := fetchFailure(err)
		// An address refusal gets its own line and its own key, and D375 is why:
		// the policy is an allowlist, so it will one day refuse an origin that was
		// perfectly legitimate, and the operator on the other end of that has only
		// a name that will not resolve to go on. `address_rule=` is what they grep
		// for — docs/operations.md says so in as many words — and it names which
		// rule refused, not merely that one did.
		var refusal *addressRefusal
		if errors.As(err, &refusal) {
			f.log.Warn("this host refused to dial an address an add-on's request resolved to",
				slog.String("addon", addon),
				slog.String("origin", originString(u)),
				slog.String("outcome", outcome),
				slog.String("address", refusal.addr.String()),
				slog.String("address_rule", refusal.rule),
				slog.String("reason", refusal.why))
			return fetchResponse{Outcome: outcome}
		}
		f.log.Warn("an add-on's outbound request did not complete",
			slog.String("addon", addon),
			slog.String("origin", originString(u)),
			slog.String("outcome", outcome),
			slog.Any("error", err))
		return fetchResponse{Outcome: outcome}
	}
	defer func() { _ = resp.Body.Close() }()

	// One byte past the cap, so that "exactly the cap" succeeds and "one more than
	// the cap" is distinguishable from a body that happened to end there.
	read, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		outcome := fetchFailure(err)
		f.log.Warn("an add-on's outbound request failed while reading the body",
			slog.String("addon", addon),
			slog.String("origin", originString(u)),
			slog.String("outcome", outcome),
			slog.Any("error", err))
		return fetchResponse{Outcome: outcome}
	}
	if int64(len(read)) > f.maxBytes {
		f.log.Warn("an add-on's outbound request answered more than this host will carry",
			slog.String("addon", addon),
			slog.String("origin", originString(u)),
			slog.Int64("cap_bytes", f.maxBytes))
		return fetchResponse{Outcome: "too_large"}
	}

	out := fetchResponse{
		Outcome:     abi.FetchOK,
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
	}
	// The same pair HTTPRequest carries, and the same function: a JSON document is
	// UTF-8 and arrives as text, and anything that is not is base64 rather than a
	// string with replacement characters in it.
	out.Body, out.BodyBase64 = EncodeRequestBody(read)
	return out
}

// checkFetchRequest is everything about a FetchRequest the host can judge without
// dialling. The second return is the sentence for the log; an empty one is a
// request that passed.
func checkFetchRequest(req fetchRequest) (*url.URL, string) {
	if len(req.URL) > maxFetchURL {
		return nil, fmt.Sprintf("the URL is %d bytes and at most %d may cross", len(req.URL), maxFetchURL)
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		return nil, "the URL does not parse"
	}
	if _, err := originOf(u); err != nil {
		return nil, err.Error()
	}
	if u.User != nil {
		return nil, "a URL carrying credentials is refused rather than stripped"
	}
	switch req.Method {
	case "", http.MethodGet:
		if req.Body != "" {
			// Refused rather than dropped: a module that put a body on a GET has
			// misunderstood something, and a request that quietly did less than it
			// asked for is how that misunderstanding survives.
			return nil, "a GET carries no body"
		}
	case http.MethodPost:
		if len(req.Body) > maxFetchBody {
			return nil, fmt.Sprintf("the body is %d bytes and at most %d may cross",
				len(req.Body), maxFetchBody)
		}
	default:
		return nil, fmt.Sprintf("%q is not one of the methods this host makes", req.Method)
	}
	return u, ""
}

// fetchFailure names what went wrong on the wire, in the vocabulary.
//
// Ordered by specificity: the two sentinels this file raises come first, because a
// refused address arrives wrapped in the same *url.Error a connection refused
// does, and reading it as `connect_failed` would tell an operator their network is
// flaky when what happened is that this host stopped an add-on reaching their
// metadata service.
func fetchFailure(err error) string {
	switch {
	case errors.Is(err, errAddressRefused):
		return "address_refused"
	case errors.Is(err, errRedirectRefused):
		return "redirect_refused"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_failed"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "connect_failed"
}

// originString is the origin of a URL for a log line, or the scheme and host as
// written when it has none. Never the path or the query: an add-on's URL is its
// own business and a log is the host's.
//
// Returned raw, including the fallback, which is the one branch here carrying text
// a module chose. Every caller is an [slog.String] into a logger derived from
// [neutralizingLogger], and logsafe.go's rule is that the call site logs the raw
// value — escaping here made it `\\u200b` by the time an operator read it.
func originString(u *url.URL) string {
	o, err := originOf(u)
	if err != nil {
		return u.Scheme + "://" + u.Host
	}
	return o.String()
}

// originsFor reads the origins one add-on's operator named, out of the settings
// its manifest marked.
//
// Read at the call rather than resolved once at load, because an operator saving
// a setting in the Add-on manager must reach the next invocation — the same
// property [settingValues] exists for, applied to the one setting where being
// stale means dialling somewhere the operator has just revoked.
//
// A value that is not an origin is dropped with a warning naming the setting and
// the reason, and is not an error: an add-on with one good origin and one typo
// keeps working for the good one, which is the behaviour an operator debugging
// their own typing needs. What it must never do is *widen* — a malformed entry
// authorizes nothing.
func (s *hostState) originsFor() originSet {
	var out originSet
	for _, decl := range s.manifest.Settings {
		if !decl.Origin {
			continue
		}
		out.settings = append(out.settings, decl.Name)
		value, ok := s.values.get(decl.Name)
		if !ok || value.IsZero() {
			continue
		}
		for _, field := range strings.Fields(value.Reveal()) {
			o, err := parseOrigin(field)
			if err != nil {
				// Logged **raw**, and that is the rule rather than an omission: hostLog
				// is the neutralizing logger, and logsafe.go's whole discipline is that
				// no call site escapes what it is about to log. Pre-escaping here
				// doubled the backslashes a second time and printed `\\u200b` where an
				// operator had typed a zero-width space — the exact symptom that file
				// names. The value is an operator's rather than a module's and it is
				// escaped all the same, because this product escapes what it did not
				// write wherever it came from.
				s.hostLog.Warn("an add-on's origin setting holds something that is not an "+
					"origin, and it authorizes nothing",
					slog.String("addon", s.manifest.Name),
					slog.String("setting", decl.Name),
					slog.String("reason", err.Error()))
				continue
			}
			out.origins = append(out.origins, o)
		}
	}
	return out
}

// refusedFetch is what the host answers when the add-on may not fetch at all, and
// it is the one place the two configuration-shaped refusals get their log line.
//
// The line names the setting, which is m68.5.md's requirement in as many words:
// an operator whose add-on is inert needs to be told which field to fill in, and
// an add-on that has been pointed somewhere else needs the origins it *is*
// allowed named beside the one it asked for.
//
// # What level a guest-drivable refusal logs at, and why these two are Debug
//
// **Warn is for a refusal an operator has no other way to see.** Where a counter
// and the Add-on manager already carry the fact, the line is Debug: the level buys
// nothing an operator does not already have, and it costs an instance whatever a
// module cares to make it cost. Both of these are counted as
// `linkctrl_addon_fetch_total{addon,outcome}` and rendered per add-on with a
// sentence saying what to do about each word — which is what configuration.md and
// SECURITY.md already promise in the phrase *visible without reading logs* — while
// neither costs a socket, so a route handler holding `network.fetch` and
// `routes.own_prefix` could otherwise write lines at CPU speed for the whole of
// its ten-second deadline, on a page anybody can reach, across every slot. The
// counter is one series however often it moves. `class_refused` in hostabi.go is
// the third site under this rule and points here.
//
// The Warns beside them stay, and each is the rule applied rather than an
// exception to it. Two of them carry a fact nothing else carries: the address
// refusal in [fetcher.fetch] names **which rule** refused under `address_rule=`,
// which the counter's single `address_refused` word cannot, and the wire-failure
// line beside it carries the error the transport raised. Two of them a guest
// cannot drive on its own: [hostState.originsFor]'s malformed-origin line needs an
// operator's typo to exist at all, and [hostState.storageFailed]'s denial has
// neither counter nor page. And the wire failures cost a socket, so the network
// rather than the module sets their rate — which is the second half of the test
// and the half these two configuration refusals fail outright, deciding nothing
// and dialling nothing.
//
// The address refusal is the one that is genuinely both — guest-drivable at CPU
// speed with a literal address, and kept at Warn anyway. Deliberately: a module
// looping there is announcing an attempt on this host's internal space, which is
// the argument [hostState.storageFailed] already makes for its own denial, and the
// rule token has nowhere else to appear.
//
// The cost is stated rather than hidden: an operator watching only the log sees
// nothing when an add-on is inert. TestTheConfigurationShapedRefusalsDoNotWarn is
// what holds this decision to the tree, and D378 is the entry.
func (s *hostState) refusedFetch(outcome string, u *url.URL, set originSet) {
	switch outcome {
	case "unconfigured":
		fields := "none"
		if len(set.settings) > 0 {
			fields = strings.Join(set.settings, ", ")
		}
		s.hostLog.Debug("an add-on tried to reach outward and no origin is configured for "+
			"it, so it can reach nothing; fill in the setting that names where it may go",
			slog.String("addon", s.manifest.Name),
			slog.String("setting", fields))
	case "origin_refused":
		allowed := make([]string, 0, len(set.origins))
		for _, o := range set.origins {
			allowed = append(allowed, o.String())
		}
		s.hostLog.Debug("an add-on tried to reach an origin the operator did not name",
			slog.String("addon", s.manifest.Name),
			slog.String("origin", originString(u)),
			slog.String("configured", strings.Join(allowed, " ")),
			slog.String("setting", strings.Join(set.settings, ", ")))
	}
}

// The three defaults, applied the way every other bound in this package applies
// one: zero means the default, so a caller that does not care writes nothing and a
// test that does writes a number.

func routeDeadlineFrom(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultRouteDeadline
	}
	return d
}

func fetchTimeoutFrom(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultFetchTimeout
	}
	return d
}

func fetchMaxBytesFrom(n int64) int64 {
	if n <= 0 {
		return DefaultFetchMaxBytes
	}
	return n
}
