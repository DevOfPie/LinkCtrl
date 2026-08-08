package auth

// The TOTP secret at rest (M53).
//
// **Encrypted, not hashed**, and that is forced rather than chosen: verifying a
// time-based code means recomputing it, which means having the secret back. It is
// the first thing in this product that is reversible on purpose — sessions,
// invitations, registrations, password resets, recovery codes and API keys are
// all one-way — so the key it depends on is worth being explicit about.
//
// # Its own variable, and why not the pepper
//
// `LINKCTRL_MFA_SECRET_KEY`, never `LINKCTRL_API_KEY_PEPPER`. Reuse is the
// tempting answer and m53.md refuses it by name: `docs/dev-notes/instances.md`
// already documents that rotating the pepper silently invalidates every issued
// API key, so sharing it would mean rotating an API-key secret also locks every
// account out of its second factor. Two credential lifecycles with nothing to do
// with each other would then have one lifetime, and an operator rotating the one
// they meant to rotate would discover the other by being telephoned about it.
//
// # Losing it
//
// Every enrolled account loses its second factor and nothing else — the bound
// m53.md states and the reason this is a survivable operator mistake rather than
// a data loss. The secret cannot be decrypted, so no TOTP code can be verified,
// so an enrolled account's sign-in refuses at the second step. What still works is
// the recovery code, because that is a SHA-256 hash and this key is not involved
// in it: the account signs in with a recovery code, disables the second factor
// with another one, and enrols again. An account with neither is the operator's,
// which is the same last resort a lost password has had since M51.
//
// That chain is why the key is *optional*. An instance that never sets it has no
// second factor available and is exactly the instance every deployment was before
// this milestone; an instance that loses it has enrolled accounts falling back
// down the chain above. Making it required would break every existing deployment
// on upgrade to buy a feature nobody had asked for yet.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// MFAKeyMinBytes is the shortest configured value accepted.
//
// Thirty-two, matching `API_KEY_PEPPER`'s floor, and for the operator's sake
// rather than the cipher's: the value is hashed to a 256-bit key whatever its
// length, so a short one is not weaker than its own entropy — it is weaker than it
// looks. Refusing below the floor is what stops `MFA_SECRET_KEY=changeme` from
// producing a working instance.
const MFAKeyMinBytes = 32

// mfaSecretScheme prefixes every stored ciphertext.
//
// A version marker, present from the first byte written. Nothing rotates keys
// today and this is what makes rotation possible later without guessing at the
// format of what is already in the column — a re-encrypting sweep reads the scheme
// and knows what it is looking at. One character of cost for the one thing that
// cannot be retrofitted.
const mfaSecretScheme = "1"

// ErrMFAKeyMissing is a second-factor operation attempted on an instance with no
// `MFA_SECRET_KEY`.
//
// Distinct from a decryption failure, because the two are different operator
// problems with different answers: this one is *set the variable*, and a
// decryption failure is *you set the wrong one, or you set it after accounts had
// enrolled under another*. Both refuse the same way to whoever is at the form.
var ErrMFAKeyMissing = errors.New("auth: this instance has no MFA_SECRET_KEY configured")

// ErrMFASecretUnreadable is a stored secret that will not decrypt under the
// configured key.
//
// Authenticated encryption means this is the only failure shape there is: a wrong
// key, a truncated value and a tampered one are one error, because GCM's tag
// check does not distinguish them and neither should a caller.
var ErrMFASecretUnreadable = errors.New("auth: the stored second-factor secret cannot be read")

// MFACipher encrypts and decrypts TOTP secrets.
//
// AES-256-GCM, from `crypto/aes` and `crypto/cipher`. Authenticated, so a
// tampered ciphertext is a decryption failure rather than a secret that decodes to
// something an attacker chose; nonce-per-message, so the same secret written twice
// produces different bytes and the column tells nobody which accounts share a
// configuration mistake.
type MFACipher struct {
	aead cipher.AEAD
}

// NewMFACipher derives the key and prepares the cipher.
//
// **The configured value is hashed to the key rather than used as one.** SHA-256
// of the raw bytes, which accepts whatever an operator generated — `openssl rand
// -base64 48` produces 64 characters, and a 64-byte string is not an AES key. The
// alternative is demanding an exactly-32-byte base64 blob, which is a
// documentation problem that produces a support problem; hashing costs one
// invocation at boot and makes every value that clears the length floor work.
func NewMFACipher(key string) (*MFACipher, error) {
	if key == "" {
		return nil, ErrMFAKeyMissing
	}
	if len(key) < MFAKeyMinBytes {
		return nil, fmt.Errorf("auth: MFA_SECRET_KEY must be at least %d bytes, got %d",
			MFAKeyMinBytes, len(key))
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("auth: mfa cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: mfa gcm: %w", err)
	}
	return &MFACipher{aead: aead}, nil
}

// Seal encrypts a base32 TOTP secret for storage.
//
// Output is `1.<base64url(nonce||ciphertext||tag)>`, ASCII, which is what goes in
// `users.mfa_secret` — a `text` column since `00200_identity.sql`, so the encoding
// is not an aesthetic choice.
//
// No additional authenticated data, and the omission is deliberate rather than an
// oversight. Binding the ciphertext to the account id would stop a row being moved
// between accounts by somebody with write access to the database — who can also
// simply write their own secret, having the key or not. It would also make the
// column unreadable after a restore that renumbered anything. The threat this
// encryption is for is a leaked dump, and AAD does nothing about that one.
func (c *MFACipher) Seal(secret string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("auth: read nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(secret), nil)
	return mfaSecretScheme + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open decrypts a stored secret.
//
// Every malformed input answers ErrMFASecretUnreadable — an unknown scheme, bad
// base64, a value shorter than a nonce, a failed tag check. One error for all of
// them because they are one operator problem, and because a caller that could tell
// them apart would be tempted to treat some of them as recoverable.
func (c *MFACipher) Open(stored string) (string, error) {
	scheme, body, found := strings.Cut(stored, ".")
	if !found || scheme != mfaSecretScheme {
		return "", ErrMFASecretUnreadable
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", ErrMFASecretUnreadable
	}
	n := c.aead.NonceSize()
	if len(raw) < n {
		return "", ErrMFASecretUnreadable
	}
	plain, err := c.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", ErrMFASecretUnreadable
	}
	return string(plain), nil
}
