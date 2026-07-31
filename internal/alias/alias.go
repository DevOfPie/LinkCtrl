// Package alias generates and validates the short codes that appear after the
// host in a LinkCtrl URL.
//
// Two properties drive the design.
//
// First, an alias sits in the redirect hot path and in the unique index, so
// validation must be cheap and total: every alias that reaches the database is
// already canonical, and canonicalization is idempotent.
//
// Second, an alias is read aloud, typed from a printed page, and scanned from a
// QR code, so the alphabet excludes characters people confuse. Because 'i', 'l'
// and 'o' are absent, the digits '0' and '1' are unambiguous and are kept —
// which is what brings the alphabet to exactly 32 characters and lets each
// character consume five bits of randomness with no modulo bias.
package alias

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
)

// Alphabet is the set of characters used for generated codes.
//
// Excluded on purpose: 'i', 'l', 'o' (confusable with 1, 1 and 0). Length is
// exactly 32, a power of two, so a uniformly random byte reduced modulo 32 is
// still uniform. Changing the length breaks that guarantee — see
// TestAlphabetIsPowerOfTwo, which fails if anyone does.
const Alphabet = "023456789abcdefghjkmnpqrstuvwxyz"

const (
	// DefaultLength is the length of a generated code. 32^7 is about 3.4e10,
	// which keeps the collision probability negligible at the scale this
	// project targets while staying short enough to print and read aloud.
	DefaultLength = 7

	// MaxGeneratedLength bounds the escalation in Generate. Reaching it means
	// something is wrong other than bad luck.
	MaxGeneratedLength = 12

	// MinLength and MaxLength bound user-supplied aliases. The lower bound
	// keeps the two-character namespace free for future routing use; the upper
	// bound is well under any practical URL limit and matches the column.
	MinLength = 3
	MaxLength = 64

	// attemptsPerLength is how many codes are tried at a given length before
	// escalating. Each failure is a database round trip, so this stays small.
	attemptsPerLength = 4
)

// Random returns a cryptographically random code of the requested length.
//
// The result is guaranteed to satisfy Validate: generated codes are re-checked
// against the reserved and profanity lists, because leetspeak normalization
// maps '0' to 'o' and '1' to 'i', so a random code genuinely can normalize to a
// word we would refuse from a user.
func Random(length int) (string, error) {
	if length < MinLength || length > MaxLength {
		return "", fmt.Errorf("alias: length %d out of range [%d,%d]", length, MinLength, MaxLength)
	}

	// Bounded retry. Each attempt has a very high probability of succeeding;
	// the loop exists only so that a rejected word cannot return an invalid
	// alias, and it is bounded so a pathological list cannot spin forever.
	for attempt := 0; attempt < 100; attempt++ {
		buf := make([]byte, length)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("alias: read random: %w", err)
		}
		for i, b := range buf {
			// len(Alphabet) is 32 and divides 256, so this is unbiased.
			buf[i] = Alphabet[int(b)%len(Alphabet)]
		}
		candidate := string(buf)
		if _, err := Validate(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("alias: exhausted attempts generating a valid code")
}

// TakenFunc reports whether an alias is already in use. It returns the
// underlying error unchanged so Generate can distinguish "taken" from "the
// database is down".
type TakenFunc func(ctx context.Context, candidate string) (bool, error)

// Generate returns an unused random alias.
//
// It tries a few codes at DefaultLength, then lengthens. Escalating matters
// because collision probability rises with the number of existing links, and a
// fixed length would degrade into an unbounded retry loop on a large instance
// rather than simply producing a slightly longer code.
//
// The caller must still handle a unique-violation on insert. Between this
// check and the write, another request can take the same alias; this reduces
// the frequency of that race, it does not eliminate it.
func Generate(ctx context.Context, taken TakenFunc) (string, error) {
	return Policy{}.Generate(ctx, taken)
}

// Generate is the policy-aware form, starting from the configured length.
func (p Policy) Generate(ctx context.Context, taken TakenFunc) (string, error) {
	for length := p.generatedLength(); length <= MaxGeneratedLength; length++ {
		for attempt := 0; attempt < attemptsPerLength; attempt++ {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			candidate, err := Random(length)
			if err != nil {
				return "", err
			}
			inUse, err := taken(ctx, candidate)
			if err != nil {
				return "", fmt.Errorf("alias: check availability: %w", err)
			}
			if !inUse {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("alias: no free code found up to length %d", MaxGeneratedLength)
}
