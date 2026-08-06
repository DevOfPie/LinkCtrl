// Package link owns link and tag business logic.
package link

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"golang.org/x/net/idna"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// DestinationPolicy governs which URLs a link may point at.
//
// Note what is not here any more. This struct used to carry BlockPrivateIPs and
// BlockedHostSuffixes, and M30 took both away for opposite reasons.
//
// BlockPrivateIPs was an override switch on the unappealable tier: setting it
// false accepted 169.254.169.254, which is the SSRF this validator exists to
// prevent, decided by an operator on behalf of visitors who never agreed to it.
// The refusals below are now unconditional and there is no field through which
// they could be turned off — asserted by TestUnappealableTierHasNoOverrideSwitch,
// which walks this struct by reflection and fails when it grows a field.
//
// BlockedHostSuffixes left for the opposite reason: it was the low-confidence
// tier before there was one, and it now lives in Postgres where the instance
// owner can change it without a restart. LINKCTRL_DESTINATION_BLOCKLIST still
// works and still means the same thing; it seeds those rows at boot.
type DestinationPolicy struct {
	// Schemes is the allowlist. Anything outside it is refused. Config
	// validation confines it to a subset of {http, https}, so it can narrow the
	// unappealable tier and never widen it.
	Schemes   []string
	MaxLength int
}

func DefaultDestinationPolicy() DestinationPolicy {
	return DestinationPolicy{
		Schemes:   []string{"http", "https"},
		MaxLength: 2048,
	}
}

// ValidateDestination checks a destination URL and returns its normalized form.
//
// This is an allowlist, not a blocklist, and that is the whole design. A
// blocklist of dangerous schemes is a game you lose: javascript:, data:,
// vbscript:, file:, intent:, and whatever the next browser ships. Permitting
// only http and https means a new scheme is refused by default.
//
// Known limitation, deliberately not papered over: blocking private literals
// does not defend against DNS rebinding, where a hostname resolves to a public
// address at creation and a private one when a visitor follows the link.
// Defending against that requires resolving at redirect time on the hot path,
// which cannot be afforded, or an egress policy outside this process. Recorded
// in docs/build-notes/SECURITY.md rather than pretended away.
//
// This is the unappealable tier and only the unappealable tier. It is called
// from exactly one place — Service.checkDestination — and that is enforced by
// TestEveryDestinationSurfaceGoesThroughTheCheck rather than by discipline,
// because a caller that reached this function directly would inherit the SSRF
// refusals while silently skipping every tier above them.
func ValidateDestination(raw string, p DestinationPolicy) (string, error) {
	var errs domain.ValidationErrors

	s := strings.TrimSpace(raw)
	if s == "" {
		return "", append(errs, domain.FieldError{
			Field: "url", Code: "required", Message: "a destination URL is required",
		})
	}
	if p.MaxLength > 0 && len(s) > p.MaxLength {
		return "", append(errs, domain.FieldError{
			Field: "url", Code: "too_long",
			Message: fmt.Sprintf("destination must be at most %d characters", p.MaxLength),
		})
	}

	// Reject control characters before parsing. A newline in a URL that later
	// reaches a Location header is a response-splitting vector, and Go's
	// parser is lenient about some of them.
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", append(errs, domain.FieldError{
				Field: "url", Code: "invalid",
				Message: "destination must not contain control characters",
			})
		}
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", append(errs, domain.FieldError{
			Field: "url", Code: "invalid", Message: "destination is not a valid URL",
		})
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		return "", append(errs, domain.FieldError{
			Field: "url", Code: "no_scheme",
			Message: "destination must start with http:// or https://",
		})
	}
	if !contains(p.Schemes, scheme) {
		return "", append(errs, domain.FieldError{
			Field: "url", Code: TierUnappealable.Code(RuleSchemeForbidden),
			Message: fmt.Sprintf("scheme %q is not allowed; use %s",
				scheme, strings.Join(p.Schemes, " or ")),
		})
	}
	u.Scheme = scheme

	// Fold the host into the one spelling every tier below is asked about. This
	// is the only place in the program that canonicalizes a destination's host,
	// and canonicalHost is the whole of what that means.
	//
	// Folded here, before any tier looks, and nowhere else. Every tier reads its
	// host off the URL this function returns, so one fold covers all of them;
	// the shape that produced both of M30's reopenings was a normalization each
	// tier did for itself, which is a rule three places have to keep and only
	// two did.
	//
	// Hostname() strips the brackets from an IPv6 authority and Port() returns
	// "" when there is no ":port" suffix, so the no-port branch has to put the
	// brackets back itself. Without that, https://[2606:4700:4700::1111]/ is
	// stored and served as https://2606:4700:4700::1111/, which no client can
	// follow and which re-parses as a different host entirely.
	lowerHost, hostErr := canonicalHost(u.Hostname())
	if hostErr != nil {
		// Refused rather than passed through raw, which is the only direction
		// that fails closed: the raw spelling is exactly the value the tiers
		// cannot read, so accepting it here would re-open F77 for every host
		// UTS-46 declines to map. See canonicalHost for why this is not a tier
		// refusal and carries no reason code naming one.
		return "", append(errs, domain.FieldError{
			Field: "url", Code: "invalid",
			Message: "destination host is not a usable name",
		})
	}
	if lowerHost == "" {
		return "", append(errs, domain.FieldError{
			Field: "url", Code: "no_host", Message: "destination must include a host",
		})
	}
	switch {
	case u.Port() != "":
		u.Host = net.JoinHostPort(lowerHost, u.Port())
	case strings.Contains(lowerHost, ":"):
		u.Host = "[" + lowerHost + "]"
	default:
		u.Host = lowerHost
	}

	// Unconditional. There is no policy field, environment variable, list entry
	// or review path that reaches this block, and adding one would be the whole
	// of what M30 refuses: the party these refusals protect — a visitor whose
	// browser would do the fetching — is not the party who would be appealing.
	addr, addrErr := netip.ParseAddr(lowerHost)
	switch {
	case addrErr == nil && isRestricted(addr):
		return "", append(errs, domain.FieldError{
			Field: "url", Code: TierUnappealable.Code(RulePrivateAddress),
			Message: "destination must not be a private, loopback or link-local address",
		})
	case addrErr != nil && looksNumeric(lowerHost):
		// A host that is not a parseable address but is not a hostname
		// either. netip.ParseAddr accepts only dotted-quad IPv4 and rejects
		// leading zeros, so 2130706433, 0177.0.0.1, 0x7f000001, 127.1 and
		// 0xa9fea9fe all fail to parse — and skipping the check on a parse
		// failure would wave every one of them through. Browsers use the
		// WHATWG parser and resolve all of them, 0xa9fea9fe to the cloud
		// metadata endpoint this check exists to keep visitors away from.
		//
		// Refused rather than canonicalized: no legitimate destination is
		// written this way, so rejecting is both safer and clearer than
		// reimplementing an alternate address grammar to block it.
		return "", append(errs, domain.FieldError{
			Field: "url", Code: TierUnappealable.Code(RulePrivateAddress),
			Message: "destination host must be a hostname or a standard IP address",
		})
	}
	// "localhost" is not an IP literal but resolves to loopback everywhere.
	if lowerHost == "localhost" || strings.HasSuffix(lowerHost, ".localhost") {
		return "", append(errs, domain.FieldError{
			Field: "url", Code: TierUnappealable.Code(RulePrivateAddress),
			Message: "destination must not point at localhost",
		})
	}

	return u.String(), nil
}

// hostProfile is the UTS-46 mapping a host is judged through.
//
// The settings are WHATWG's own "domain to ASCII" with beStrict false, which is
// what a browser applies to a host before it resolves one — deliberately, since
// the thing being defended against is a spelling that a browser resolves to a
// blocked name and this program's tiers do not see. idna.Lookup was the obvious
// choice and is the wrong one: it sets UseSTD3ASCIIRules and CheckHyphens, so it
// refuses "my_host.example", "under_score.example.com" and the real
// "r3---sn-apo3qvuoxuxbt-j5pe.googlevideo.com", all three of which this
// validator accepts today. A canonicalizer that refuses ordinary destinations is
// a canonicalizer operators route around.
//
// Non-transitional, so "ß" stays "ß" rather than folding to "ss" — the same
// choice net/http made in Go 1.18 (golang/go#47510) and the one every current
// browser makes.
var hostProfile = idna.New(
	idna.MapForLookup(),
	idna.StrictDomainName(false),
	idna.CheckHyphens(false),
	idna.BidiRule(),
	idna.Transitional(false),
)

// canonicalHost folds a URL's host into the single spelling every tier reads.
//
// Two mechanisms, in this order, and the order is load-bearing.
//
// **UTS-46 ToASCII**, because a host written outside ASCII reaches none of the
// checks below it (F77). "169。254。169。254" separated by U+3002 has no ASCII dot
// in it at all, so looksNumeric's strings.LastIndexByte finds nothing and reads
// the entire host as one label; "ｌｏｃａｌｈｏｓｔ" in fullwidth Latin is not equal to
// "localhost" and the localhost test is an equality; "metadata。google。internal"
// is not the map key "metadata.google.internal"; and isHomograph returns early
// unless a label already starts "xn--", so the one tier built for lookalikes
// never examined a raw Unicode host. Five spellings, one missing conversion.
//
// A hand-written map of the separators would have been the tempting fix and is
// the one that reads as complete without being it: U+3002, U+FF0E and U+FF61
// cover three of those five, and the fullwidth-digit and fullwidth-Latin
// spellings carry no separator to map. Only the real mapping table catches
// "１６９.２５４.１６９.２５４", and it also catches "①⑥⑨.la", "𝟏𝟔𝟗" and a soft hyphen
// hiding inside an otherwise ordinary name — shapes nobody would have thought to
// enumerate. That is what D91 bought.
//
// **The trailing dot, after the mapping and not before** (F26). "169.254.169.254."
// is the same address, "localhost." the same name and "metadata.google.internal."
// the same listed host — a browser and a resolver read them that way — but
// netip.ParseAddr refuses the dotted spelling, looksNumeric read an empty last
// label as evidence of a name, the localhost test is an equality and the
// embedded list is an exact-match map. Trimming has to come second because
// "169。254。169。254。" ends in a separator that is not a dot until ToASCII has
// made it one. TrimRight rather than one TrimSuffix: "127.0.0.1.." also has an
// empty last label, and trimming a single dot leaves one behind.
//
// **Canonicalized, never refused**, for both mechanisms. A trailing dot is a
// fully qualified name and "müller.de" and "テスト.example" are ordinary names; a
// fix that refused either would close the hole by breaking the product, and the
// accepted-destinations tests exist to say so. What is stored is the ToASCII
// form, so the value a visitor's browser is handed is the value the tiers judged
// rather than a spelling they never saw.
//
// An all-ASCII host skips the mapping entirely, which is what net/http's own
// idnaASCII does. It is not an optimization and it is not a hole: with
// UseSTD3ASCIIRules off, the mapping's only effect on ASCII is the case folding
// already done above, and everything else the profile would do to such a host is
// a *rejection* rather than a different spelling. Skipping it is what keeps an
// invalid "xn--" label — which is unresolvable rather than dangerous, and which
// punycode_test.go names — from becoming a refusal this milestone never asked
// for.
//
// The error is deliberately not a tier refusal. A host UTS-46 declines to map is
// not a name at all — a disallowed rune, a bidi violation, a broken A-label —
// which is the same kind of thing as a URL that will not parse, and reporting it
// as unappealable.* would claim a judgement about the destination that nothing
// here made. blocking.go makes the same argument about minting reason codes from
// values no documentation explains.
func canonicalHost(host string) (string, error) {
	host = strings.ToLower(host)
	if isASCII(host) {
		return strings.TrimRight(host, "."), nil
	}
	ascii, err := hostProfile.ToASCII(host)
	if err != nil {
		return "", fmt.Errorf("canonicalize host %q: %w", host, err)
	}
	return strings.TrimRight(strings.ToLower(ascii), "."), nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// looksNumeric reports whether a host is written as a number rather than as a
// name — the shapes the WHATWG URL spec resolves as IPv4 and netip.ParseAddr
// refuses: decimal (2130706433), octal (0177.0.0.1), hex (0x7f000001) and short
// dotted forms (127.1). Also true for a bare IPv6 literal that failed to parse,
// which is malformed rather than a hostname.
//
// A real hostname always has a non-numeric TLD, so this cannot reject one: the
// last label is what decides, and "com", "co.uk" and "example" are all names.
// It expects a host whose trailing dot has already been folded away, which is
// what makes that true — see the empty-label branch.
func looksNumeric(host string) bool {
	if host == "" {
		return false
	}
	if strings.ContainsAny(host, ":[]") {
		return true // an IPv6 shape that ParseAddr already rejected
	}
	last := host
	if i := strings.LastIndexByte(host, '.'); i >= 0 {
		last = host[i+1:]
	}
	if last == "" {
		// A trailing dot. This used to answer false — "a fully-qualified name,
		// not a number" — and that sentence is where 2130706433. got in: the
		// empty label made an obfuscated address look like a hostname, and the
		// numeric check waved it through. Callers now hand this function a host
		// with the dot already folded away (ValidateDestination) or refuse a
		// dotted entry outright (checkListEntry), so nothing reaches here; the
		// answer is true so that a third caller, if one ever appears, fails
		// closed on a shape no legitimate destination has by the time it is
		// judged.
		return true
	}
	if strings.HasPrefix(last, "0x") {
		return true
	}
	for _, r := range last {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// IsRestrictedAddr is isRestricted, exported for the one caller outside this
// package that needs the same answer about a *resolved* address.
//
// internal/webhook calls it from the dialer's Control hook, after DNS has
// answered and before connect(2), which is the only place a rebinding check can
// stand. It is deliberately the same function and not a second list: two
// definitions of "private address" in one program is a drift bug waiting for the
// day somebody adds a range to one of them.
//
// Nothing else should reach for this. A *destination* — anything a visitor's
// browser will be sent to — goes through Service.checkDestination, and a caller
// that took this predicate instead would inherit the SSRF refusals while
// skipping every tier above them, which is exactly the bypass
// TestEveryDestinationSurfaceGoesThroughTheCheck exists to catch.
func IsRestrictedAddr(addr netip.Addr) bool { return isRestricted(addr) }

// isRestricted reports whether an address is in a range a public short link
// should never point at.
func isRestricted(addr netip.Addr) bool {
	// Fold IPv4-mapped IPv6 first, or ::ffff:10.0.0.1 slips past the IPv4
	// checks entirely.
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	switch {
	case addr.IsLoopback(),
		addr.IsPrivate(),
		addr.IsLinkLocalUnicast(),
		addr.IsLinkLocalMulticast(),
		addr.IsInterfaceLocalMulticast(),
		addr.IsMulticast(),
		addr.IsUnspecified():
		return true
	}
	// Ranges the standard predicates do not cover, or cover without saying so.
	// Contains is family-aware, so an IPv6 address simply does not match.
	for _, p := range extraRestrictedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// extraRestrictedPrefixes are written as CIDRs rather than byte comparisons, so
// what they mean is legible and adding one is a single line.
var extraRestrictedPrefixes = []netip.Prefix{
	// Carrier-grade NAT. Not covered by IsPrivate.
	netip.MustParsePrefix("100.64.0.0/10"),
	// Link-local, so already caught above — but the cloud metadata endpoint
	// 169.254.169.254 is the case that matters here and is worth naming: a
	// short link pointing at it turns the shortener into a way to make someone
	// else's browser probe their own infrastructure.
	netip.MustParsePrefix("169.254.0.0/16"),
}

// HostOf extracts the lowercase host, stored alongside the URL so the hot path
// and reporting never have to re-parse.
func HostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(strings.TrimSpace(h), needle) {
			return true
		}
	}
	return false
}
