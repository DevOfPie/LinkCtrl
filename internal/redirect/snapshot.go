// Package redirect resolves an alias to a destination.
//
// Everything here runs inside a 20ms budget, so the design is shaped by what
// the hot path must NOT do: no joins, no session lookup, no template
// rendering, no synchronous write, and no dependency whose failure can take
// the path down.
package redirect

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// Snapshot is everything the redirect handler needs, in one cacheable value.
//
// Deliberately not the full link row. It carries only what a decision depends
// on, so the cached payload stays small and a schema change to columns the hot
// path ignores does not invalidate the cache.
//
// The Phase 2 fields are present now because they change what must be cached,
// not what is currently enforced: adding them later would mean a cache-key
// version bump and a cold cache on upgrade.
type Snapshot struct {
	LinkID      uuid.UUID  `json:"i"`
	WorkspaceID uuid.UUID  `json:"w"`
	URL         string     `json:"u"`
	Status      string     `json:"s"`
	ExpiresAt   *time.Time `json:"e,omitempty"`

	ForwardQuery bool   `json:"q,omitempty"`
	HasPassword  bool   `json:"p,omitempty"` // PHASE 2
	MaxClicks    *int64 `json:"m,omitempty"` // PHASE 2
	OneTime      bool   `json:"o,omitempty"` // PHASE 2

	// Deep-link path forwarding (M33). Added without bumping CacheKeyVersion,
	// and the reason is narrower than the one written below for bot blocking.
	//
	// It is not that nobody could have set the column yet. A rolling restart
	// runs migrations at boot and then serves from old and new containers at
	// once, so an old binary goes on writing entries without this field while
	// the feature is already switched on somewhere — that is F41, recorded
	// against the paragraph below, and it is a real sequence rather than a
	// hypothetical one.
	//
	// What makes the omitted bump safe here is which way the zero value falls.
	// An absent `fp` decodes as false, false means *do not forward*, and that is
	// exactly what this alias did before the milestone existed. A visitor whose
	// deep link lands on a stale entry gets the 404 they would have got
	// yesterday, for at most REDIRECT_TTL, and the next fetch fixes it. The
	// failure is a feature not yet working, not a control not being applied —
	// so there is nothing here a cold cache would buy. A field whose absence
	// meant "forward" would have needed the bump, because then the stale
	// reading would send somebody somewhere the owner never configured.
	//
	// This holds only while the cache key is v1 for this build and the previous
	// one. M34 bumps it to v2; that ordering is the claim, not a coincidence.
	ForwardPath bool `json:"fp,omitempty"`

	// Bot blocking (M32.5). Both halves of the precedence rule travel together,
	// because the whole point is that a cache hit answers the question without
	// asking anything: a link's setting alone cannot decide, and fetching the
	// domain's separately would be the round trip this design exists to avoid.
	//
	// Adding them did NOT bump CacheKeyVersion, and that is a claim worth being
	// explicit about. Both are omitempty, so an entry written by the previous
	// build decodes with the zero values — inherit and off — which is exactly
	// "no blocking". On any instance holding such an entry the columns did not
	// exist a moment ago, so nobody can have switched blocking on yet, and the
	// stale reading cannot differ from the true one. A field whose absence had
	// meant something else would have needed the bump and a cold cache.
	BotPolicy       domain.BotPolicy       `json:"bb,omitempty"`
	DomainBotPolicy domain.DomainBotPolicy `json:"db,omitempty"`

	// NotFound marks a negative cache entry. Storing misses matters: an
	// unknown alias is the single most common request a public shortener
	// receives, mostly from scanners, and without this every one of them is a
	// database query.
	NotFound bool `json:"n,omitempty"`
}

// Short JSON keys are not premature micro-optimisation: this value is
// serialized and parsed on every cache miss and every cache write, and the
// field names would otherwise be most of the payload.

// Outcome is what the handler should do with a snapshot.
type Outcome int

const (
	// OutcomeRedirect sends the visitor onward.
	OutcomeRedirect Outcome = iota
	// OutcomeNotFound covers unknown, archived and disabled links. They are
	// deliberately indistinguishable: telling a scanner that an alias exists
	// but is archived is information it has no use for.
	OutcomeNotFound
	// OutcomeGone is an expired link. Distinct from not-found because the
	// alias really did exist, and 410 tells crawlers to stop asking.
	OutcomeGone
)

// Decide reports what to do with a snapshot at a given time.
//
// Expiry is evaluated here rather than filtered in SQL so that an expired link
// yields 410 rather than 404, and so the decision is identical whether the
// snapshot came from cache or from the database.
func (s *Snapshot) Decide(now time.Time) Outcome {
	switch {
	case s == nil, s.NotFound:
		return OutcomeNotFound
	case s.ExpiresAt != nil && !now.Before(*s.ExpiresAt):
		return OutcomeGone
	case s.Status == "expired":
		return OutcomeGone
	case s.Status != "active":
		// archived, disabled
		return OutcomeNotFound
	default:
		return OutcomeRedirect
	}
}

// CacheTTL returns how long this snapshot may be cached.
//
// Clamped to the expiry: caching a link for 24h when it expires in 5 minutes
// would keep serving it for hours after it should have stopped. This is the
// kind of bug that only shows up in production, on the one link that mattered.
func (s *Snapshot) CacheTTL(now time.Time, base, negative time.Duration) time.Duration {
	if s == nil || s.NotFound {
		// The same one-second floor as a positive entry, and for a sharper
		// reason: go-redis treats a zero expiration as "no TTL", so a
		// REDIRECT_NEGATIVE_TTL of 0 — the natural way to write "do not cache
		// misses" — wrote a permanent key for every well-formed alias anyone
		// ever probed. A scanner spraying /abc123 paths would fill Redis with
		// keys that never expire.
		if negative < time.Second {
			return time.Second
		}
		return negative
	}
	ttl := base
	if s.ExpiresAt != nil {
		if remaining := s.ExpiresAt.Sub(now); remaining < ttl {
			ttl = remaining
		}
	}
	if ttl < time.Second {
		ttl = time.Second
	}
	return ttl
}

func (s *Snapshot) encode() ([]byte, error) { return json.Marshal(s) }

func decodeSnapshot(b []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// notFoundSnapshot is the negative cache entry.
func notFoundSnapshot() *Snapshot { return &Snapshot{NotFound: true} }
