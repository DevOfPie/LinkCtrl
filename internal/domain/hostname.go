package domain

import (
	"fmt"
	"net"
	"strings"
)

// Hostname validation, for a domain somebody registers (M39).
//
// This is a *syntax* check and nothing more. Whether the person registering
// `go.example.com` controls it is a question DNS answers, and answering it is
// M40's — until then a registered hostname is stored unverified and nothing on
// the instance will serve it. Adding a half-measure here, refusing names that
// look like they belong to somebody else, would be a check that reads as
// protection while proving nothing.
//
// What it does refuse is names that cannot work: names DNS itself would reject,
// and the two shapes people actually type instead of a hostname — a whole URL,
// and an IP address.

// MaxHostnameLength is the wire limit on a domain name, in the presentation
// form this product stores. RFC 1035 bounds the encoded name at 255 octets,
// which is 253 characters once the length prefix and the root label are taken
// off.
const MaxHostnameLength = 253

// MaxLabelLength bounds one dot-separated label, from the same RFC.
const MaxLabelLength = 63

// MaxDomainsPerWorkspace bounds how many hostnames one workspace may register.
//
// **Unlike the campaign and folder caps, this one bounds work rather than a
// page.** Every registered hostname is a recurring outbound DNS lookup from
// whichever replica holds the leader lock, aimed at a nameserver the registrant
// chose, and each of those lookups can block for `DOMAIN_VERIFY_DNS_TIMEOUT`
// against a pass with a fixed budget. Without a bound, one workspace can decide
// how much of that budget exists for everybody else — and it does not even need
// the hostnames to resolve, which is what makes an unbounded registration surface
// an amplifier somebody can aim rather than a quota somebody can exceed.
//
// **Twenty-five, and the number is a judgement.** It is bounded below by what a
// real tenant needs — a brand with a hostname per market, plus the ones it is
// migrating between — and above by the share of one pass a single workspace
// should be able to consume: at the default five-second timeout, twenty-five
// wholly unresponsive hostnames cost about two minutes of a ten-minute pass. It
// is deliberately not operator configuration, for the reason the campaign and
// folder caps are not: a number nobody has needed to raise is a constant, and
// making it a knob invites raising it to make a symptom go away.
//
// Registration is bounded and never reaped. A hostname that fails every check is
// somebody's cut-over in progress, not an abandoned row, and nothing anywhere
// treats a domain's age or its unchecked state as licence to remove it.
const MaxDomainsPerWorkspace = 25

// ValidateHostname normalizes and checks a hostname, returning the form to
// store and every reason it cannot be stored.
//
// Normalization is lowercasing, trimming surrounding space, and dropping a
// single trailing dot. The trailing dot is the fully-qualified form and is
// legitimate to type; storing it would make `example.com.` and `example.com`
// two rows the unique index treats as different names for one host. Everything
// else that differs is a different name.
func ValidateHostname(raw string) (string, ValidationErrors) {
	host := strings.ToLower(strings.TrimSpace(raw))
	host = strings.TrimSuffix(host, ".")

	if host == "" {
		return "", ValidationErrors{{
			Field: "hostname", Code: "required",
			Message: "a hostname is required",
		}}
	}
	// The two things typed instead of a hostname, each named rather than left to
	// the label rules below — "://" and "/" both fail those rules, and being told
	// a label is malformed is not being told to paste the host on its own.
	if strings.Contains(host, "://") || strings.ContainsAny(host, "/?#@ ") {
		return "", ValidationErrors{{
			Field: "hostname", Code: "not_a_hostname",
			Message: "that looks like a URL; register the hostname on its own, " +
				"with no scheme and no path",
		}}
	}
	if len(host) > MaxHostnameLength {
		return "", ValidationErrors{{
			Field: "hostname", Code: "too_long",
			Message: fmt.Sprintf("a hostname is at most %d characters", MaxHostnameLength),
		}}
	}
	if net.ParseIP(host) != nil {
		return "", ValidationErrors{{
			Field: "hostname", Code: "not_a_hostname",
			Message: "an IP address cannot be registered; a short link needs a name " +
				"a certificate can be issued for",
		}}
	}

	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", ValidationErrors{{
			Field: "hostname", Code: "not_a_hostname",
			Message: "a hostname needs at least two labels, like example.com — a " +
				"single label is a name only this network can resolve",
		}}
	}
	for _, label := range labels {
		if !validLabel(label) {
			return "", ValidationErrors{{
				Field: "hostname", Code: "malformed",
				Message: fmt.Sprintf("%q is not a usable part of a hostname: each part is "+
					"1 to %d letters, digits or hyphens, and may not begin or end with a hyphen",
					label, MaxLabelLength),
			}}
		}
	}
	// The last label is what a certificate authority and a resolver both read as
	// the top-level domain. Digits there means somebody typed a partial IP or a
	// port; every real TLD is alphabetic.
	if !allLetters(labels[len(labels)-1]) {
		return "", ValidationErrors{{
			Field: "hostname", Code: "malformed",
			Message: "the last part of a hostname is a top-level domain, like com or dev",
		}}
	}
	return host, nil
}

// validLabel applies the LDH rule: letters, digits and hyphens, not leading or
// trailing a hyphen, and never empty.
//
// Non-ASCII is refused rather than punycoded. An internationalized name reaches
// DNS as its A-label — xn--… — and that is the form a certificate is issued for
// and the form a Host header carries, so it is the form to register. Converting
// here would mean storing something the operator did not type and cannot match
// against their own DNS zone.
func validLabel(label string) bool {
	if label == "" || len(label) > MaxLabelLength {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

func allLetters(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}
