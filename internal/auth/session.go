package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

type ctxKey int

const ctxClientIP ctxKey = iota

// WithClientIP carries the resolved client address down to the service layer.
//
// It lives here, beside AnonymizeIP and Identity, rather than in the HTTP layer
// where it is set. Services take an *Identity and no request, and an audit
// event has to record the network a change came from — so without a carrier,
// every service method that will ever write an audit event grows an address
// parameter, and every caller of those methods grows one too. Five later
// milestones write audit events; that is the retrofit M21 exists to avoid.
//
// A context value rather than a field on Identity because it is a property of
// the request, not of who is making it: the same identity acts from different
// networks, and Identity is also built outside a request entirely, by the CLI.
func WithClientIP(ctx context.Context, addr netip.Addr) context.Context {
	return context.WithValue(ctx, ctxClientIP, addr)
}

// ClientIPFrom returns the resolved client address, or the zero Addr when there
// is none — a CLI invocation, a background job, or a test that did not set one.
// AnonymizeIP maps that to an empty string, so an event written off a request
// records no network rather than a misleading one.
func ClientIPFrom(ctx context.Context) netip.Addr {
	addr, _ := ctx.Value(ctxClientIP).(netip.Addr)
	return addr
}

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

// IsSessionInvalid reports whether an Authenticate failure means the credential
// itself is finished, as opposed to the lookup having failed.
//
// The distinction decides whether a caller may destroy the cookie. Authenticate
// returns wrapped pgx errors for a dead pool, a cancelled context or a missing
// workspace row, and treating those as "this session is over" turns a ten-second
// database blip into a forced sign-out for every signed-in user at once —
// sessions that were, and remain, perfectly valid.
func IsSessionInvalid(err error) bool {
	return errors.Is(err, ErrSessionNotFound) ||
		errors.Is(err, ErrSessionExpired) ||
		errors.Is(err, ErrSessionRevoked) ||
		errors.Is(err, ErrAccountInactive)
}

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

// NewOpaqueToken returns a random bearer-shaped secret and its storage hash.
//
// Only the hash is ever persisted. A database leak therefore does not hand over
// live credentials, which is the same reasoning as never storing a raw
// password. SHA-256 rather than argon2 is correct here: the token is
// full-entropy random, so key-stretching adds nothing, and these are verified
// on paths where 64 MiB of work would be untenable.
//
// Generalized out of NewSessionToken when invitations needed the same
// construction (M27). One implementation rather than two, so "hashed like a
// session token" is a fact about the code and not a claim in a comment.
func NewOpaqueToken(n int) (token string, hash []byte, err error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("auth: read token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashOpaqueToken(token), nil
}

// HashOpaqueToken returns the storage hash for a token minted by
// NewOpaqueToken.
func HashOpaqueToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// NewSessionToken returns a random session token and its storage hash.
func NewSessionToken() (token string, hash []byte, err error) {
	return NewOpaqueToken(sessionTokenBytes)
}

// HashSessionToken returns the storage hash for a session token.
func HashSessionToken(token string) []byte { return HashOpaqueToken(token) }

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
	// **6to4 is the same bug in a different scheme** (F59). `2002::/16` carries
	// the client's IPv4 address in bytes 2 to 5, which is inside the /48 this
	// function keeps: `2002:cb00:712a::1` masks to `2002:cb00:712a::/48`, and
	// those bytes are `203.0.113.42` in full. Folded to the embedded address and
	// masked to /24 like any other IPv4.
	//
	// Every other v4-in-v6 scheme was checked rather than assumed, and none
	// needs this: Teredo, NAT64 and ISATAP all embed in the low bits that a /48
	// discards. 6to4 is the single blind spot, against a mechanism RFC 7526
	// deprecated in 2015 — which is why it is one branch and not a table.
	if v4, ok := sixToFour(addr); ok {
		addr = v4
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

// sixToFour extracts the IPv4 address a 6to4 address embeds.
//
// 2002:V4ADDR::/16 — the four bytes after the 2002 prefix are the client's IPv4
// address verbatim. Reported as not-6to4 for everything else, including
// `::ffff:` mapped addresses, which the caller has already unmapped.
func sixToFour(addr netip.Addr) (netip.Addr, bool) {
	if !addr.Is6() || addr.Is4In6() {
		return addr, false
	}
	b := addr.As16()
	if b[0] != 0x20 || b[1] != 0x02 {
		return addr, false
	}
	return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
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

// ThresholdParam and WindowSecondsParam narrow the policy for the SQL that
// applies it.
//
// Clamped, not converted. A configured value large enough to wrap would arrive
// in the query as a negative threshold, and `failed_login_count + 1 >= -3` is
// true on the first attempt — a nonsense setting would lock every account out
// on one typo instead of being ignored.
func (p LockoutPolicy) ThresholdParam() int32 {
	return clampInt32(p.Threshold)
}

func (p LockoutPolicy) WindowSecondsParam() int32 {
	return clampInt32(int(p.Window.Seconds()))
}

func clampInt32(n int) int32 {
	switch {
	case n < 0:
		return 0
	case n > math.MaxInt32:
		return math.MaxInt32
	default:
		return int32(n)
	}
}

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
