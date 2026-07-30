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

// Validate canonicalizes a user-supplied alias and reports whether it is
// acceptable, returning the canonical form on success.
//
// It is idempotent: Validate(Validate(x)) == Validate(x) for any x that
// validates. The property test relies on this, and so does the service layer,
// which validates on both create and update.
func Validate(input string) (string, error) {
	s := Canonical(input)

	if s == "" {
		return "", newError(ReasonEmpty, "alias must not be empty")
	}
	// Count runes, not bytes: a multi-byte input should report "invalid
	// characters" rather than a confusing length error.
	if n := len([]rune(s)); n < MinLength {
		if !isAllowedASCII(s) {
			return "", newError(ReasonInvalidChars,
				"alias may only contain lowercase letters, digits, hyphen and underscore")
		}
		return "", newError(ReasonTooShort, "alias must be at least %d characters", MinLength)
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

	if IsReserved(s) {
		return "", newError(ReasonReserved, "alias %q is reserved", s)
	}

	if IsProfane(s) {
		return "", newError(ReasonProfane, "alias contains disallowed language")
	}

	return s, nil
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
