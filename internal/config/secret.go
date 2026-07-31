package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// Secret is a string that refuses to print itself.
//
// Every obvious way of accidentally disclosing a value is overridden: fmt's %v
// and %s go through String, structured logging goes through LogValue,
// json.Marshal goes through MarshalJSON. A config dump, a panic that formats a
// struct, or a well-meaning slog.Any("config", cfg) therefore cannot leak the
// database password or the API-key pepper.
//
// Reveal is the only way to read the value, and its name is deliberately
// awkward so that calls to it stand out in review.
type Secret string

const redacted = "[REDACTED]"

// Reveal returns the underlying value. Call it at the point of use, never to
// pass a secret into logging or error text.
func (s Secret) Reveal() string { return string(s) }

// IsZero reports whether the secret is unset, without disclosing it.
func (s Secret) IsZero() bool { return s == "" }

// Len returns the length of the secret. Useful for validation messages such as
// "must be at least 32 bytes" that need to say something specific without
// echoing the value.
func (s Secret) Len() int { return len(s) }

func (s Secret) String() string { return redacted }

// GoString covers %#v, which would otherwise print the underlying string.
func (s Secret) GoString() string { return redacted }

// Format covers the remaining verbs. Without it, %q on a Secret prints the
// value, because fmt falls back to the underlying string kind for verbs that
// Stringer does not handle.
func (s Secret) Format(f fmt.State, verb rune) {
	switch verb {
	case 'q':
		fmt.Fprintf(f, "%q", redacted)
	default:
		fmt.Fprint(f, redacted)
	}
}

func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// UnmarshalJSON accepts a value so that a Secret can be read from a config
// file, even though the round trip is deliberately lossy.
func (s *Secret) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*s = Secret(v)
	return nil
}

// MarshalText covers encoders that prefer TextMarshaler, including YAML.
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// UnmarshalText lets caarlos0/env populate the field from an environment
// variable.
func (s *Secret) UnmarshalText(b []byte) error {
	*s = Secret(b)
	return nil
}

// LogValue makes slog print the redacted form.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

var (
	_ fmt.Stringer   = Secret("")
	_ fmt.Formatter  = Secret("")
	_ slog.LogValuer = Secret("")
	_ json.Marshaler = Secret("")
)
