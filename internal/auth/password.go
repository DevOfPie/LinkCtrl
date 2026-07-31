// Package auth handles passwords, sessions, API keys and permission checks.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the argon2 cost parameters.
//
// Stored in the hash string itself (PHC format), so changing these does not
// invalidate existing passwords: an old hash still verifies against its own
// recorded parameters, and NeedsRehash reports that it should be upgraded on
// the next successful login.
type Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultParams follows the RFC 9106 second recommendation: 64 MiB, t=3, p=2.
// config.Validate refuses anything below the 19 MiB floor.
var DefaultParams = Params{
	MemoryKiB:   64 * 1024,
	Iterations:  3,
	Parallelism: 2,
	SaltLength:  16,
	KeyLength:   32,
}

var (
	ErrMismatch      = errors.New("auth: password does not match")
	ErrInvalidHash   = errors.New("auth: hash is not in a recognised format")
	ErrUnsupportedID = errors.New("auth: unsupported password hash algorithm")
)

// MaxPasswordLength caps input before hashing.
//
// Argon2 has no practical input limit, so this is not about the algorithm: it
// is a denial-of-service guard. Hashing is deliberately expensive, and an
// unbounded body means an attacker can make the server do unbounded work.
const MaxPasswordLength = 4096

// MinPasswordLength is the floor for new passwords. Length is the only
// requirement — no composition rules, which push people toward predictable
// substitutions without adding real entropy (NIST SP 800-63B).
const MinPasswordLength = 12

// maxHashFieldLen bounds the decoded salt and key of a stored hash.
//
// This project writes 16-byte salts and 32-byte keys, so anything approaching
// this is a corrupt row rather than an old one. The bound exists so the lengths
// can be narrowed to the uint32 argon2 wants without the conversion being an
// act of faith.
const maxHashFieldLen = 1024

// Hasher hashes and verifies passwords.
//
// The semaphore is the reason this is a struct rather than free functions.
// Each hash allocates 64 MiB, so N concurrent logins allocate N x 64 MiB; a
// credential-stuffing burst would otherwise OOM the process. Limiting
// concurrent hashing bounds that at a fixed cost, and the login rate limiter
// keeps the queue behind it short.
type Hasher struct {
	params Params
	sem    chan struct{}
}

func NewHasher(p Params) *Hasher {
	if p.SaltLength == 0 {
		p.SaltLength = DefaultParams.SaltLength
	}
	if p.KeyLength == 0 {
		p.KeyLength = DefaultParams.KeyLength
	}
	limit := runtime.NumCPU()
	if limit > 4 {
		limit = 4
	}
	if limit < 1 {
		limit = 1
	}
	return &Hasher{params: p, sem: make(chan struct{}, limit)}
}

func (h *Hasher) acquire() { h.sem <- struct{}{} }
func (h *Hasher) release() { <-h.sem }

// Hash returns a PHC-encoded argon2id hash.
func (h *Hasher) Hash(password string) (string, error) {
	if len(password) < MinPasswordLength {
		return "", fmt.Errorf("auth: password must be at least %d characters", MinPasswordLength)
	}
	if len(password) > MaxPasswordLength {
		return "", fmt.Errorf("auth: password must be at most %d bytes", MaxPasswordLength)
	}

	salt := make([]byte, h.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}

	h.acquire()
	key := argon2.IDKey([]byte(password), salt,
		h.params.Iterations, h.params.MemoryKiB, h.params.Parallelism, h.params.KeyLength)
	h.release()

	return encode(h.params, salt, key), nil
}

// Verify checks a password against a stored hash.
//
// Returns ErrMismatch for a wrong password and a different error for a
// malformed hash, so a corrupt row is not silently reported to the user as
// "wrong password" while the real problem goes uninvestigated.
func (h *Hasher) Verify(password, encoded string) error {
	params, salt, want, err := decode(encoded)
	if err != nil {
		return err
	}
	if len(password) > MaxPasswordLength {
		return ErrMismatch
	}

	h.acquire()
	// Verification uses the parameters recorded in the hash, not the current
	// defaults, so raising the cost does not lock existing users out.
	// KeyLength rather than len(want): decode sets it from the same slice, and
	// it is already bounded and already a uint32.
	got := argon2.IDKey([]byte(password), salt,
		params.Iterations, params.MemoryKiB, params.Parallelism, params.KeyLength)
	h.release()

	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// NeedsRehash reports whether a stored hash was made with weaker parameters
// than the current policy. Callers rehash on the next successful login, which
// is the only moment the plaintext is available.
func (h *Hasher) NeedsRehash(encoded string) bool {
	params, _, _, err := decode(encoded)
	if err != nil {
		// Unparseable: replace it at the next opportunity.
		return true
	}
	return params.MemoryKiB < h.params.MemoryKiB ||
		params.Iterations < h.params.Iterations ||
		params.Parallelism < h.params.Parallelism ||
		params.KeyLength < h.params.KeyLength
}

// DummyVerify performs a hash with the same cost as a real verification and
// discards the result.
//
// Called when the account does not exist, so that login timing does not reveal
// whether an email is registered. Without it, "no such user" returns in
// microseconds while a real user costs ~50ms, which is a trivially measurable
// account-enumeration oracle.
func (h *Hasher) DummyVerify(password string) {
	h.acquire()
	defer h.release()
	_ = argon2.IDKey([]byte(password), dummySalt,
		h.params.Iterations, h.params.MemoryKiB, h.params.Parallelism, h.params.KeyLength)
}

// A fixed salt is fine here: the output is discarded and never stored. It
// exists only to make the work identical in cost to a real verification.
var dummySalt = []byte("linkctrl-timing-equalizer")

func encode(p Params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

// decode parses a PHC string. Written to accept any well-formed argon2id hash,
// not only ones this code produced, so passwords can be imported from another
// system without forcing a reset.
func decode(encoded string) (Params, []byte, []byte, error) {
	var p Params

	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, key
	if len(parts) != 6 || parts[0] != "" {
		return p, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return p, nil, nil, fmt.Errorf("%w: %q", ErrUnsupportedID, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return p, nil, nil, fmt.Errorf("auth: unsupported argon2 version %d", version)
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&p.MemoryKiB, &p.Iterations, &p.Parallelism); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if p.MemoryKiB == 0 || p.Iterations == 0 || p.Parallelism == 0 {
		return p, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 || len(salt) > maxHashFieldLen {
		return p, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 || len(key) > maxHashFieldLen {
		return p, nil, nil, ErrInvalidHash
	}

	// Both lengths are bounded by maxHashFieldLen immediately above, so
	// narrowing them cannot wrap. Every caller uses these rather than taking
	// len() again, which keeps that guarantee in one place.
	p.SaltLength = uint32(len(salt)) //nolint:gosec // G115: bounded by maxHashFieldLen above
	p.KeyLength = uint32(len(key))   //nolint:gosec // G115: bounded by maxHashFieldLen above
	return p, salt, key, nil
}
