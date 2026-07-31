package alias

import (
	"fmt"
	"strings"
)

// Reason identifies why an alias was rejected. It is stable and safe to expose
// in an API error body, so a client can render a specific message or map the
// failure to a form field.
type Reason string

const (
	ReasonEmpty         Reason = "empty"
	ReasonTooShort      Reason = "too_short"
	ReasonTooLong       Reason = "too_long"
	ReasonInvalidChars  Reason = "invalid_characters"
	ReasonEdgeSeparator Reason = "edge_separator"
	ReasonReserved      Reason = "reserved"
	ReasonProfane       Reason = "profane"
)

// Error is a validation failure carrying a machine-readable reason.
type Error struct {
	Reason  Reason
	Message string
}

func (e *Error) Error() string { return e.Message }

func newError(r Reason, format string, args ...any) *Error {
	return &Error{Reason: r, Message: fmt.Sprintf(format, args...)}
}

// Canonical folds an alias to its stored form.
//
// Aliases are case-insensitive and stored lowercase. This means /GitHub and
// /github are the same link and the former renders as the latter. That is a
// deliberate trade: a single canonical form keeps the unique index correct and
// the cache key unambiguous, at the cost of not preserving display casing.
func Canonical(input string) string {
	return strings.ToLower(strings.TrimSpace(input))
}

// Policy carries the operator-supplied part of alias validation.
//
// Both fields are named for their non-default state so that the zero Policy is
// the safe one: no extra reservations, profanity filtering on. A struct whose
// zero value quietly disabled the filter would be wrong in the direction that
// matters, and Policy{} appears in tests and in the package-level Validate.
type Policy struct {
	// ReservedExtra are additional words an operator wants refused, merged with
	// the built-in list rather than replacing it. Compared canonically, so entries
	// need no particular casing.
	ReservedExtra []string

	// ProfanityDisabled switches the built-in profanity filter off.
	//
	// Worth having as a switch: the list cannot know the context it is applied in,
	// and an instance used internally for engineering links has different needs
	// from a public shortener. It does not affect the reserved list.
	ProfanityDisabled bool

	// MinUserLength raises the floor on a user-supplied alias. Zero means the
	// package default, so the zero Policy keeps the documented behaviour.
	//
	// Raising it and lowering it are both legitimate: the two-character space is
	// held back for routing, and an operator who wants /go to be claimable can
	// say so, while a public instance may want short aliases kept scarce.
	// Clamped to the package bounds — a policy cannot permit an alias the column
	// or the redirect's WellFormed pre-filter would refuse.
	MinUserLength int

	// GeneratedLength is the starting length for generated codes. Zero means
	// DefaultLength. Larger values buy collision headroom on a big instance at
	// the cost of a longer URL; Generate still escalates from here.
	GeneratedLength int
}

// minUserLength and generatedLength resolve the configured values against the
// package bounds. Clamping rather than validating, because these arrive from
// configuration that has already been range-checked, and a policy that silently
// refuses to apply would be the same class of defect as one nothing reads.
func (p Policy) minUserLength() int {
	switch {
	case p.MinUserLength <= 0:
		return MinLength
	case p.MinUserLength > MaxLength:
		return MaxLength
	default:
		return p.MinUserLength
	}
}

func (p Policy) generatedLength() int {
	switch {
	case p.GeneratedLength <= 0:
		return DefaultLength
	case p.GeneratedLength < MinLength:
		return MinLength
	case p.GeneratedLength > MaxGeneratedLength:
		return MaxGeneratedLength
	default:
		return p.GeneratedLength
	}
}

// Validate canonicalizes a user-supplied alias under the default policy.
//
// It is idempotent: Validate(Validate(x)) == Validate(x) for any x that
// validates. The property test relies on this, and so does the service layer,
// which validates on both create and update.
func Validate(input string) (string, error) {
	return Policy{}.Validate(input)
}

// Validate canonicalizes a user-supplied alias and reports whether it is
// acceptable, returning the canonical form on success.
func (p Policy) Validate(input string) (string, error) {
	s := Canonical(input)

	if s == "" {
		return "", newError(ReasonEmpty, "alias must not be empty")
	}
	// Count runes, not bytes: a multi-byte input should report "invalid
	// characters" rather than a confusing length error.
	minLen := p.minUserLength()
	if n := len([]rune(s)); n < minLen {
		if !isAllowedASCII(s) {
			return "", newError(ReasonInvalidChars,
				"alias may only contain lowercase letters, digits, hyphen and underscore")
		}
		return "", newError(ReasonTooShort, "alias must be at least %d characters", minLen)
	} else if n > MaxLength {
		return "", newError(ReasonTooLong, "alias must be at most %d characters", MaxLength)
	}

	if !isAllowedASCII(s) {
		return "", newError(ReasonInvalidChars,
			"alias may only contain lowercase letters, digits, hyphen and underscore")
	}

	// A leading or trailing separator reads as a typo and makes copied links
	// look broken when a trailing character is dropped by a text wrap.
	if isSeparator(s[0]) || isSeparator(s[len(s)-1]) {
		return "", newError(ReasonEdgeSeparator,
			"alias must not begin or end with a hyphen or underscore")
	}

	if p.IsReserved(s) {
		return "", newError(ReasonReserved, "alias %q is reserved", s)
	}

	if !p.ProfanityDisabled && IsProfane(s) {
		return "", newError(ReasonProfane, "alias contains disallowed language")
	}

	return s, nil
}

// IsReserved reports whether an alias is reserved under this policy: on the
// built-in list, or in the operator's additions.
//
// The built-in list is always consulted. Operator additions extend it and cannot
// shrink it, because every route the router registers is on that list and an
// alias shadowing one of them would take a working page out of service.
func (p Policy) IsReserved(s string) bool {
	if IsReserved(s) {
		return true
	}
	canonical := Canonical(s)
	for _, extra := range p.ReservedExtra {
		// Canonicalized on both sides at comparison time rather than at
		// construction: a Policy is a plain struct that anything may build, so
		// normalizing here means no caller can get it wrong.
		if Canonical(extra) == canonical {
			return true
		}
	}
	return false
}

// WellFormed reports whether a string has the shape of a stored alias: allowed
// length, allowed characters, no separator at either end.
//
// Shape only. It says nothing about reserved words or profanity, which are
// policies about what may be *created* rather than what may exist, and it
// performs no list lookups and no allocation.
//
// The redirect path uses it to answer "could this possibly be in the database"
// before touching the database. /favicon.ico, /robots.txt, /apple-touch-icon.png
// and every /wp-login.php-style scan fail here, so ordinary browser noise and
// bulk scanning cost one byte scan each instead of a query and a negative cache
// entry — and cannot be mistaken for probing, since refusing them costs nothing
// to begin with.
func WellFormed(s string) bool {
	// Byte length rather than rune count, which would allocate. A multi-byte
	// string that slips through this bound fails the character check below, and a
	// stored alias is ASCII by construction.
	if len(s) < MinLength || len(s) > MaxLength {
		return false
	}
	if !isAllowedASCII(s) {
		return false
	}
	return !isSeparator(s[0]) && !isSeparator(s[len(s)-1])
}

// isAllowedASCII reports whether every byte is in [a-z0-9_-].
//
// Note the deliberate absence of '.': allowing dots would let an alias look
// like a static file ("logo.png", "index.html"), which invites confusion with
// real asset routes and with extension-based handling in proxies sitting in
// front of the app. Excluding the character removes the whole class of problem
// rather than pattern-matching for it afterwards.
func isAllowedASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

func isSeparator(c byte) bool { return c == '-' || c == '_' }
