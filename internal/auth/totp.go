package auth

// RFC 6238 (TOTP), RFC 4226 (HOTP), and the `otpauth://` URI an authenticator
// app scans — written here rather than imported (M53).
//
// **No module dependency, and the reason is D72's** (m53.md's second bullet). The
// algorithm is HMAC over a big-endian time counter, truncated: `crypto/hmac`,
// `crypto/sha1`, `encoding/base32`, `encoding/binary`, all standard library.
// D72 turned a QR library down for pulling `image/png` and `compress/zlib` in on
// a path nothing called, and taking a TOTP module for sixty lines of arithmetic
// would be the same trade in the other direction — a supply-chain edge on the
// login path in exchange for code this file makes auditable in one sitting.
// TestNoModuleDependencyJoinedTheSetForTheSecondFactor asserts the require block
// did not move.
//
// **SHA-1, deliberately, and it is not a defect.** RFC 6238 permits SHA-256 and
// SHA-512; every authenticator app in circulation assumes SHA-1, and several
// ignore the `algorithm` parameter in the URI outright. The construction is
// HMAC-SHA-1, whose security does not rest on collision resistance, and the
// output is truncated to six digits and lives for thirty seconds. Choosing the
// stronger hash here would buy nothing and would produce codes that do not match
// what the user's phone shows.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // G505: HMAC-SHA-1 is what RFC 6238 specifies and what every authenticator app implements. See the package note above.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTPPeriod is the length of one step. Thirty seconds is RFC 6238's default and
// is what every authenticator app assumes; it is not configurable for that
// reason, because a value the phone cannot be told about is a value that produces
// codes nobody can match.
const TOTPPeriod = 30 * time.Second

// TOTPDigits is the length of a code. Six, for the same reason.
const TOTPDigits = 6

// TOTPSkew is how many steps either side of the current one are accepted.
//
// **One, which is a tolerance of ninety seconds in total** — the current step
// plus the one before and the one after. m53.md asks for clock skew to be
// answered by accepting the adjacent windows and documenting the tolerance as a
// number, *and no more*, so this is the number: 30 seconds of drift in either
// direction, plus the up-to-30 seconds a person spends typing.
//
// Wider is the tempting mistake. Every extra step multiplies the guessing surface
// by three-in-a-million per attempt and extends how long an observed code stays
// usable, and it treats a broken clock as something to absorb rather than
// something to fix. NTP exists.
const TOTPSkew = 1

// TOTPSecretBytes is the entropy in a generated secret.
//
// Twenty bytes — 160 bits — which is what RFC 4226 §4 R6 requires as a minimum
// and what HMAC-SHA-1's block handling makes the natural size: a longer secret is
// hashed down to twenty bytes before use, so the extra entropy never reaches the
// computation. It encodes to thirty-two base32 characters with no padding, which
// is the shape every authenticator app expects to be handed.
const TOTPSecretBytes = 20

// totpBase32 is base32 without padding, upper case.
//
// Unpadded because the `secret` parameter of an `otpauth://` URI is base32 with
// the `=` stripped, and several apps refuse a padded one. Twenty bytes divides
// evenly into thirty-two characters, so nothing is lost by it here.
var totpBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh secret, base32-encoded as an authenticator app
// expects to receive it.
//
// The encoded form is what travels: it is what goes in the URI, what is shown
// beside the QR code for somebody enrolling on the device they are reading, and
// what is encrypted at rest. Keeping one representation means there is no place
// for an encode and a decode to disagree.
func NewTOTPSecret() (string, error) {
	buf := make([]byte, TOTPSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: read totp secret: %w", err)
	}
	return totpBase32.EncodeToString(buf), nil
}

// TOTPStep is the counter RFC 6238 derives from a moment in time.
//
// Exported because it is the replay guard's unit: `users.mfa_last_step` holds one
// of these, and the refusal is an integer comparison rather than a set of spent
// codes. Unix seconds divided by the period, which is the specification's T
// with T0 = 0.
func TOTPStep(t time.Time) int64 {
	return t.Unix() / int64(TOTPPeriod/time.Second)
}

// TOTPCode computes the code for one step.
//
// RFC 4226 §5.3's dynamic truncation, verbatim: HMAC the counter, take the low
// four bits of the last byte as an offset, read four bytes from there, mask the
// sign bit, and reduce modulo ten to the power of the digit count. The mask is
// what stops the result depending on the platform's integer signedness, which is
// the one place a hand-written HOTP usually goes wrong.
func TOTPCode(secret string, step int64) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}

	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step)) //nolint:gosec // G115: the counter is the specification's 64-bit big-endian T, and a negative step (a pre-1970 clock) is meant to wrap rather than be rejected.

	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	truncated := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff

	mod := uint32(1)
	for i := 0; i < TOTPDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", TOTPDigits, truncated%mod), nil
}

// TOTPVerify checks a presented code against a secret, over the accepted window,
// and reports which step matched.
//
// **The step is returned rather than a bare boolean**, and that is the whole
// interface to the replay guard: the caller writes the matched step to
// `users.mfa_last_step` through `AcceptMFAStep`, which refuses anything not
// strictly greater. A verifier that answered yes-or-no would leave the caller
// guessing which of the three accepted steps to record, and recording the wrong
// one either lets the code work twice or refuses the next two windows.
//
// The comparison is constant-time. Six digits is a small space and the code is
// compared against three candidates on a route an attacker can drive, so a
// byte-wise early exit is a timing oracle on the first digits — cheap to remove
// and awkward to reason about if it is left in.
//
// Steps are tried nearest-first, so an ordinary in-window code costs one HMAC and
// the skew allowance costs two more only when it is needed.
func TOTPVerify(secret, code string, now time.Time) (step int64, ok bool) {
	code = strings.TrimSpace(code)
	if len(code) != TOTPDigits {
		// Length is checked before anything is computed. It is not a secret —
		// the digit count is in the URI the person scanned — and it saves three
		// HMACs against a field somebody pasted a password into.
		return 0, false
	}

	current := TOTPStep(now)
	// 0, -1, +1, -2, +2 … which for TOTPSkew = 1 is three candidates.
	for offset := 0; offset <= TOTPSkew; offset++ {
		for _, candidate := range stepsAt(current, offset) {
			want, err := TOTPCode(secret, candidate)
			if err != nil {
				return 0, false
			}
			if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
				return candidate, true
			}
		}
	}
	return 0, false
}

// stepsAt yields the steps at a given distance from the current one: the current
// step alone at distance zero, and the pair either side of it beyond that.
func stepsAt(current int64, offset int) []int64 {
	if offset == 0 {
		return []int64{current}
	}
	return []int64{current - int64(offset), current + int64(offset)}
}

// TOTPURI builds the `otpauth://` URI an authenticator app scans.
//
// The shape is Google's de-facto Key URI Format, which every app implements:
//
//	otpauth://totp/<issuer>:<account>?secret=…&issuer=…&algorithm=SHA1&digits=6&period=30
//
// The issuer appears twice — as a label prefix and as a query parameter — because
// older apps read one and newer ones read the other, and an app that reads neither
// files the entry under a bare address with no clue which service it belongs to.
//
// **Everything is escaped, and the label is escaped as a path segment.** The
// account name is an email address and the issuer is the instance's own hostname,
// neither of which is attacker-controlled here; escaping them anyway is what stops
// that from being a fact this function depends on. `url.URL.String` would encode
// the path for us, but it also re-encodes `:` inconsistently across the label
// separator, so the path is built with `url.PathEscape` and assigned to `Opaque`
// — which is what the RFC 3986 shape of these URIs actually is.
func TOTPURI(issuer, account, secret string) string {
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	// Named explicitly rather than left to the app's defaults. They *are* the
	// defaults, and an app that assumes different ones produces codes this
	// product refuses with no way for anybody to see why.
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprintf("%d", TOTPDigits))
	q.Set("period", fmt.Sprintf("%d", int(TOTPPeriod/time.Second)))

	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// decodeTOTPSecret turns the stored base32 back into key bytes.
//
// Padding is tolerated on the way in and never produced on the way out. A secret
// that came from NewTOTPSecret has none; one an operator pasted from somewhere
// else may, and refusing it would be this product being stricter than the format
// for no reason.
func decodeTOTPSecret(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.TrimSpace(secret))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.TrimRight(s, "=")
	key, err := totpBase32.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("auth: decode totp secret: %w", err)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("auth: totp secret is empty")
	}
	return key, nil
}
