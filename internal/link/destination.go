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
type DestinationPolicy struct {
	// Schemes is the allowlist. Anything outside it is refused.
	Schemes   []string
	MaxLength int
	// BlockPrivateIPs refuses literal addresses in private, loopback,
	// link-local and unique-local ranges.
	BlockPrivateIPs bool
	// BlockedHostSuffixes refuses matching hosts, matched on label boundaries.
	BlockedHostSuffixes []string
}

func DefaultDestinationPolicy() DestinationPolicy {
	return DestinationPolicy{
		Schemes:         []string{"http", "https"},
		MaxLength:       2048,
		BlockPrivateIPs: true,
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
			Field: "url", Code: "scheme_not_allowed",
			Message: fmt.Sprintf("scheme %q is not allowed; use %s",
				scheme, strings.Join(p.Schemes, " or ")),
		})
	}
	u.Scheme = scheme

	host := u.Hostname()
	if host == "" {
		return "", append(errs, domain.FieldError{
			Field: "url", Code: "no_host", Message: "destination must include a host",
		})
	}

	// Lowercase the host so the blocklist and reporting are consistent; leave
	// the path alone, since paths are case-sensitive.
	lowerHost := strings.ToLower(host)
	if u.Port() != "" {
		u.Host = net.JoinHostPort(lowerHost, u.Port())
	} else {
		u.Host = lowerHost
	}

	if p.BlockPrivateIPs {
		if addr, err := netip.ParseAddr(strings.Trim(lowerHost, "[]")); err == nil {
			if isRestricted(addr) {
				return "", append(errs, domain.FieldError{
					Field: "url", Code: "private_address",
					Message: "destination must not be a private, loopback or link-local address",
				})
			}
		}
		// "localhost" is not an IP literal but resolves to loopback everywhere.
		if lowerHost == "localhost" || strings.HasSuffix(lowerHost, ".localhost") {
			return "", append(errs, domain.FieldError{
				Field: "url", Code: "private_address",
				Message: "destination must not point at localhost",
			})
		}
	}

	for _, suffix := range p.BlockedHostSuffixes {
		suffix = strings.ToLower(strings.TrimSpace(suffix))
		if suffix == "" {
			continue
		}
		// Match on a label boundary, so blocking "evil.com" does not also
		// block "notevil.com".
		if lowerHost == suffix || strings.HasSuffix(lowerHost, "."+suffix) {
			return "", append(errs, domain.FieldError{
				Field: "url", Code: "host_blocked",
				Message: fmt.Sprintf("destination host %q is not allowed", lowerHost),
			})
		}
	}

	return u.String(), nil
}

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
