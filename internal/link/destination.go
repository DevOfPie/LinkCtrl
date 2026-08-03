// Package link owns link and tag business logic.
package link

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

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

	// Lowercase the host so the blocklist and reporting are consistent; leave
	// the path alone, since paths are case-sensitive.
	//
	// The trailing dot goes at the same time, and this is the only place in the
	// program that takes one off a destination. "169.254.169.254." is the same
	// address, "localhost." is the same name and "metadata.google.internal." is
	// the same listed host — a browser and a resolver read them that way — but
	// netip.ParseAddr refuses the dotted spelling, looksNumeric read an empty
	// last label as evidence of a name, the localhost test is an equality and
	// the embedded list is an exact-match map. One character therefore walked
	// past four unrelated checks and two whole tiers at once (F26). Only the
	// Postgres tier was safe, because HostCandidates trims for itself.
	//
	// Folded here, before any tier looks, and nowhere else. Every tier reads its
	// host off the URL this function returns, so one fold covers all of them;
	// the shape that produced the defect was a normalization each tier did for
	// itself, which is a rule three places have to keep and only two did.
	//
	// Canonicalized, never refused. A trailing dot is a fully qualified name and
	// an ordinary thing to type, which is why the accepted-destinations test
	// requires https://example.com./ to keep working — it is now stored without
	// the dot, so what the tiers judged is what a visitor is sent to.
	//
	// TrimRight rather than one TrimSuffix: "127.0.0.1.." also has an empty last
	// label, and trimming a single dot would leave one behind for looksNumeric
	// to misread exactly as before.
	//
	// Hostname() strips the brackets from an IPv6 authority and Port() returns
	// "" when there is no ":port" suffix, so the no-port branch has to put the
	// brackets back itself. Without that, https://[2606:4700:4700::1111]/ is
	// stored and served as https://2606:4700:4700::1111/, which no client can
	// follow and which re-parses as a different host entirely.
	lowerHost := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
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
