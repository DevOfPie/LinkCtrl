package analytics

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

// The within-day returning-visitor test (M34, decision D2).
//
// **"Returning" means seen earlier today, and today ends at midnight UTC.** A
// visitor from yesterday is new again. That is not an approximation of a
// longer-lived answer; it is the whole answer, and it is what makes the feature
// possible without the thing this product refuses to build. A durable
// "returning visitor" needs a durable per-person identifier — a cookie, or a
// hash kept long enough to link two days together — and both are exactly what
// the analytics design deletes its salts to avoid. Every surface that shows
// this condition says so in those words, because a person configuring it will
// otherwise assume the other meaning.
//
// The mechanism is a Redis set per (link, day) holding truncated visitor
// hashes:
//
//   - The **redirect path reads** it, with SISMEMBER, and only for a link whose
//     rules actually ask. That is the one Redis round trip M34 adds, and it is
//     reached only by a request that has already satisfied every other condition
//     on the rule.
//   - The **ingest pipeline writes** it, in the batch goroutine, for the clicks
//     the redirect handler flagged. Nothing is written on the hot path.
//   - The set **expires with the day**, so nothing outlives the salt that keyed
//     it. Once that salt is purged the members are unlinkable to any address,
//     which is the same de-identification the click events themselves rely on.
//
// With no Redis the answer is always "not returning", degraded rather than
// broken, in line with the phase-wide rule that nothing correctness-critical
// depends on the cache. A rule written as "returning visitors go here" simply
// never fires; a rule written as "new visitors go here" fires for everybody.
// Both are stated in the dashboard and in docs/configuration.md.

// ReturningKeyPrefix is the Redis namespace. Separate from the redirect cache's
// `lc:a:` so an operator reading the keyspace can tell the two apart, and so a
// domain sweep cannot reach these.
const ReturningKeyPrefix = "lc:rv:"

// ReturningHashLength is how much of the visitor hash a set member keeps.
//
// Eight bytes out of the sixteen VisitorHash already truncated to. This is a
// membership test for one link on one day, so the collision that matters is
// between two visitors of the same link on the same day: at a million distinct
// visitors that is a chance of roughly one in forty million, and the cost of
// losing is one visitor being routed as returning on their first visit. Against
// that, halving the key halves what Redis holds for a popular link and takes
// another eight bytes off a value that exists only to be compared with itself.
const ReturningHashLength = 8

// ReturningSet is the returning-visitor set, read on the redirect path and
// written by the ingester.
//
// A nil *ReturningSet is valid and answers "not returning" to everything, which
// is what an instance with no Redis has.
type ReturningSet struct {
	redis *goredis.Client
	salts *SaltCache
	// timeout bounds each command. Deliberately the same REDIS_READ_TIMEOUT the
	// resolver's cache reads use: this runs on the same path, under the same
	// budget, and giving it a number of its own would mean an operator tuning
	// the redirect path had two to find.
	timeout time.Duration
	log     *slog.Logger
}

// NewReturningSet builds one. A nil client returns nil, so "no Redis" is a nil
// pointer rather than a flag every call site has to check.
func NewReturningSet(rdb *goredis.Client, salts *SaltCache, timeout time.Duration, log *slog.Logger) *ReturningSet {
	if rdb == nil || salts == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = 50 * time.Millisecond
	}
	if log == nil {
		log = slog.Default()
	}
	return &ReturningSet{redis: rdb, salts: salts, timeout: timeout, log: log}
}

// Enabled reports whether there is anything to ask.
func (s *ReturningSet) Enabled() bool { return s != nil && s.redis != nil }

// returningKey is the set for one link on one UTC day.
func returningKey(linkID uuid.UUID, day time.Time) string {
	return ReturningKeyPrefix + linkID.String() + ":" + day.Format("20060102")
}

// Member derives a set member from the same inputs the visitor hash uses.
//
// Hex rather than raw bytes, because these end up in a Redis set an operator
// may well look at with redis-cli, and a binary member there is unreadable
// without being any more private.
func (s *ReturningSet) Member(salt []byte, ip netip.Addr, userAgent string, workspaceID uuid.UUID) string {
	h := VisitorHash(salt, ip, userAgent, workspaceID)
	if len(h) > ReturningHashLength {
		h = h[:ReturningHashLength]
	}
	return hex.EncodeToString(h)
}

// Seen reports whether this visitor was already seen on this link today.
//
// **Nothing here can reach Postgres**, and that is the design rather than a
// happy accident. The salt is read from the cache's in-memory map with
// SaltCache.Cached, never with For — For would create or fetch the day's salt,
// which is a database query, and m34.md's claim is that rule evaluation adds no
// database query per request. A cache miss therefore answers "not returning".
//
// That answer is not a degradation in the case it actually happens. The salt is
// absent from a process's memory only before that process has handled the day
// at all — at boot, which cmd/linkctrl warms past by loading today's salt before
// it listens, and immediately after midnight UTC, when the day's set is empty
// and "not returning" is true of everybody. What it is not is a silent fallback
// on a busy instance.
//
// Every Redis failure — key absent, server down, timeout — is also "not
// returning", for the reason every other Redis failure on this path is a miss:
// the redirect must complete either way, and the honest degradation of a
// condition that cannot be evaluated is the condition not matching.
func (s *ReturningSet) Seen(
	ctx context.Context, linkID, workspaceID uuid.UUID,
	ip netip.Addr, userAgent string, at time.Time,
) bool {
	if !s.Enabled() {
		return false
	}
	day := SaltDay(at)
	salt, ok := s.salts.Cached(day)
	if !ok {
		return false
	}

	// Its own deadline, independent of the request's, exactly as the resolver's
	// cache read takes one. Since M45 it binds a Redis that accepts the
	// connection and then never answers as well as the ordinary case (F138);
	// before that the client's own ReadTimeout was the only thing that did, and
	// it still caps this from above — s.timeout is REDIS_READ_TIMEOUT, so the
	// two numbers are the same one and always have been.
	rctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	member := s.Member(salt, ip, userAgent, workspaceID)
	found, err := s.redis.SIsMember(rctx, returningKey(linkID, day), member).Result()
	if err != nil {
		s.log.Debug("returning-visitor lookup failed; treating the visitor as new",
			slog.String("link_id", linkID.String()), slog.Any("error", err))
		return false
	}
	return found
}

// returningMark is one visitor to add, as the ingester accumulates them.
type returningMark struct {
	linkID uuid.UUID
	day    time.Time
	member string
}

// mark adds a batch's visitors to their sets.
//
// Runs in the ingester's batching goroutine, after the click rows have
// committed, and never on the redirect path — which is the half of D2 that
// makes the round trip above affordable. One pipeline for the whole batch, so a
// flush of five hundred clicks is one network exchange rather than a thousand.
//
// The TTL is set on every SADD rather than only on the first. EXPIRE on a key
// that already has one just moves it, the cost is a command inside a pipeline
// that was already being sent, and the alternative — remembering which keys are
// new — is state that would have to survive a restart to be worth anything. A
// key that somehow missed its expiry would hold a day's hashes forever.
func (s *ReturningSet) mark(ctx context.Context, marks []returningMark, now time.Time) {
	if !s.Enabled() || len(marks) == 0 {
		return
	}
	// Detached from any request and bounded generously: this is the batch
	// goroutine, not the hot path, and a flush that gives up too eagerly loses
	// a whole batch's worth of returning-visitor state. Five seconds against the
	// thirty this goroutine already allows its Postgres write, so a stalled Redis
	// cannot become the thing that holds up a flush. It is the budget for the
	// whole exchange and not the wire: REDIS_READ_TIMEOUT caps each command
	// underneath it at 50ms whatever this says, before F138 and after it, so the
	// five seconds is headroom for a slow pipeline rather than patience with a
	// stalled one. What a stall costs is the
	// queue filling and the ingester dropping events, which is counted and
	// alertable and is what a full queue has always done — the redirect path is
	// never waited on either way.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	pipe := s.redis.Pipeline()
	for _, m := range marks {
		key := returningKey(m.linkID, m.day)
		pipe.SAdd(wctx, key, m.member)
		pipe.Expire(wctx, key, returningTTL(m.day, now))
	}
	if _, err := pipe.Exec(wctx); err != nil {
		// Logged and dropped. A visitor missing from the set is read as new on
		// their next visit, which is the same answer the whole feature gives
		// when Redis is absent — so this degrades along an axis that is already
		// documented rather than opening a new one.
		s.log.Debug("returning-visitor set was not updated for a batch",
			slog.Int("visitors", len(marks)), slog.Any("error", err))
	}
}

// returningTTL is how long a day's set lives.
//
// To the end of the UTC day it describes, plus an hour. The day is what the
// answer means, so the key has no reason to outlive it; the hour is slack for
// a batch that flushes just after midnight and is still writing yesterday's
// clicks, which would otherwise create a key for a day nobody will ever read
// and leave it with no expiry worth having.
//
// Floored at a minute so a clock skew or a very late batch cannot ask Redis for
// a zero or negative expiry, which go-redis sends as "no TTL".
func returningTTL(day, now time.Time) time.Duration {
	ttl := day.AddDate(0, 0, 1).Add(time.Hour).Sub(now)
	if ttl < time.Minute {
		return time.Minute
	}
	return ttl
}
