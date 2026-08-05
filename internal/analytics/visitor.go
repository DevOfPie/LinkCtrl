// Package analytics records and reads click data.
//
// The privacy guarantee here is structural rather than procedural: no IP
// address is ever written to click_events, because the table has no column for
// one. Visitor identity is a keyed hash whose key is deleted on a schedule,
// and that deletion is what makes the day's hashes irreversible.
package analytics

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// VisitorHashLength is how much of the HMAC is kept.
//
// 16 bytes is far beyond collision range for per-day, per-link counting, and
// truncating reduces what a database holds without weakening the property that
// matters: the hash cannot be reversed once the salt is gone.
const VisitorHashLength = 16

// SaltLength is the size of a daily salt.
const SaltLength = 32

// NewSalt generates a day's salt.
func NewSalt() ([]byte, error) {
	b := make([]byte, SaltLength)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("analytics: generate salt: %w", err)
	}
	return b, nil
}

// VisitorHash derives a per-day, per-workspace visitor identifier.
//
// HMAC rather than a plain hash of salt||data: a plain concatenation is
// vulnerable to length extension, and HMAC is the construction actually
// designed for keying a hash.
//
// workspaceID is part of the message, not the key. That is what stops the same
// person being correlated across two workspaces on the same instance — the
// salt is shared per day, but the derived hashes differ, so one workspace's
// analytics cannot be joined against another's.
//
// The inputs are separated by a NUL byte so that ("ab", "c") and ("a", "bc")
// cannot produce the same hash. Without a separator, a crafted user agent
// could be made to collide with a different address.
func VisitorHash(salt []byte, ip netip.Addr, userAgent string, workspaceID uuid.UUID) []byte {
	mac := hmac.New(sha256.New, salt)

	if ip.IsValid() {
		// Fold IPv4-mapped IPv6 so the same client reaching the server over
		// either stack hashes identically; otherwise one person counts twice.
		if ip.Is4In6() {
			ip = ip.Unmap()
		}
		b, _ := ip.MarshalBinary()
		mac.Write(b)
	}
	mac.Write([]byte{0})
	mac.Write([]byte(userAgent))
	mac.Write([]byte{0})
	wsBytes := workspaceID
	mac.Write(wsBytes[:])

	return mac.Sum(nil)[:VisitorHashLength]
}

// SaltDay returns the UTC day a timestamp belongs to.
//
// UTC always, never local time. A local-time boundary would rotate the salt at
// a different instant per deployment and, worse, make the same visitor hash
// differently on either side of a daylight-saving change.
func SaltDay(at time.Time) time.Time {
	u := at.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// FormatHash renders a hash for logs and debugging. Never used for storage.
func FormatHash(h []byte) string { return hex.EncodeToString(h) }
