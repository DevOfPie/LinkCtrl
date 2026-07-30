package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// SessionCookieName uses the __Host- prefix, which browsers only accept when
// the cookie is Secure, has Path=/, and carries no Domain attribute. That
// makes it impossible for a subdomain — including one an attacker controls via
// a stale DNS record or a shared hosting neighbour — to set or overwrite the
// session cookie.
//
// The prefix requires HTTPS, so local HTTP development uses the unprefixed
// name. Config refuses SECURE_COOKIES=false in production, so the weaker form
// cannot reach a real deployment.
const (
	SessionCookieName         = "__Host-linkctrl_session"
	SessionCookieNameInsecure = "linkctrl_session"
)

// sessionTokenBytes is the raw entropy in a session token. 32 bytes is well
// beyond guessing range and keeps the cookie a manageable length.
const sessionTokenBytes = 32

var (
	ErrSessionNotFound = errors.New("auth: session not found")
	ErrSessionExpired  = errors.New("auth: session expired")
	ErrSessionRevoked  = errors.New("auth: session revoked")
)

// Session is a live login.
type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// CookieName returns the correct cookie name for the deployment.
func CookieName(secure bool) string {
	if secure {
		return SessionCookieName
	}
	return SessionCookieNameInsecure
}

// NewSessionToken returns a random token and its storage hash.
//
// Only the hash is persisted. A database leak therefore does not hand over
// live sessions, which is the same reasoning as never storing a raw password.
// SHA-256 rather than argon2 is correct here: the token is full-entropy
// random, so key-stretching adds nothing, and session validation happens on
// every request where 64 MiB of work would be untenable.
func NewSessionToken() (token string, hash []byte, err error) {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("auth: read session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashSessionToken(token), nil
}

// HashSessionToken returns the storage hash for a token.
func HashSessionToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// SessionTTL bundles the two expiry rules.
type SessionTTL struct {
	// Absolute is the hard deadline from creation. A session dies at this
	// point regardless of activity, which bounds how long a stolen token stays
	// useful.
	Absolute time.Duration
	// Idle is the maximum gap between requests. Enforced against last_seen_at
	// at read time rather than by rewriting expires_at, so changing the policy
	// takes effect immediately and needs no data migration.
	Idle time.Duration
}

// Valid checks a session against both expiry rules.
func (t SessionTTL) Valid(s Session, now time.Time) error {
	if now.After(s.ExpiresAt) {
		return ErrSessionExpired
	}
	if t.Idle > 0 && now.Sub(s.LastSeenAt) > t.Idle {
		return ErrSessionExpired
	}
	return nil
}

// AnonymizeIP reduces an address to the prefix kept for session and audit
// records: /24 for IPv4, /48 for IPv6.
//
// The same reasoning as analytics — enough to recognise "this session moved to
// a different network", not enough to identify a person. Analytics keeps no
// address at all; sessions keep a prefix because "where was this session used"
// is a question a user legitimately asks of their own account.
func AnonymizeIP(addr netip.Addr) string {
	if !addr.IsValid() {
		return ""
	}
	// An IPv4-mapped IPv6 address must be folded first, or it would be treated
	// as IPv6 and masked to /48, which for a mapped address keeps the entire
	// IPv4 address intact.
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	bits := 24
	if addr.Is6() {
		bits = 48
	}
	prefix, err := addr.Prefix(bits)
	if err != nil {
		return ""
	}
	return prefix.String()
}

// LockoutPolicy throttles repeated failed logins for one account.
//
// Per-account, complementing the per-IP rate limit. Neither alone is enough:
// per-IP misses a distributed attack on one account, and per-account lets an
// attacker lock a victim out by failing on purpose — which is why this uses a
// short expiring window rather than a lock an administrator must clear.
type LockoutPolicy struct {
	Threshold int
	Window    time.Duration
}

var DefaultLockout = LockoutPolicy{Threshold: 5, Window: 15 * time.Minute}

// LockedUntil returns when a lockout expires, or the zero time if the account
// is not locked.
func (p LockoutPolicy) LockedUntil(failedCount int, lastFailure time.Time, now time.Time) time.Time {
	if p.Threshold <= 0 || failedCount < p.Threshold {
		return time.Time{}
	}
	until := lastFailure.Add(p.Window)
	if now.After(until) {
		return time.Time{}
	}
	return until
}
