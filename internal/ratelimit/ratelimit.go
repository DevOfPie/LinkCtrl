// Package ratelimit is an in-memory, fixed-cost request limiter.
//
// Four properties shaped it, and each is a trade rather than an oversight.
//
// It is in-memory and per-instance **by default**, and that is the shape this
// package was built in. The surfaces being protected include the redirect path,
// whose entire budget is 20ms, so spending a network round trip to decide
// whether to allow a request would cost more than the limit saves. Redis is also
// optional at runtime by design, and a limiter that stops limiting when the
// cache goes away is worse than one whose numbers are per-instance. The
// consequence is stated rather than hidden: with N replicas the effective limit
// is N times the configured one.
//
// **M24 added a shared mode, and this paragraph said otherwise until 0.2.0**
// (F38). `shared.go` — in this package — backs the *credential* and *API*
// limiters with a Redis token bucket, so those two are shared across replicas
// and fall back to these in-memory buckets only when Redis does not answer. The
// 404-probe limiter is the one that stays plain, and `limits.go` says
// "deliberately not shared" beside it for the reason above: it guards the
// redirect path. So the per-instance multiplication described here is true of
// the 404 limiter, true of every limiter while Redis is unreachable, and not
// true of the credential and API limiters on a healthy instance.
//
// IPv6 is keyed by /64, not by address. A single host is routinely handed a
// whole /64, so a per-address key would let one machine present an effectively
// unlimited number of identities — defeating the limit and growing the table
// without bound while doing it. /64 is a floor rather than the whole answer: a
// site delegated a shorter prefix, /56 or /48, holds 256 or 65536 distinct /64s
// and gets a bucket for each. Keying coarser than /64 would close that at the
// price F57 ruled out on the v4 side, where a wider key lets one abusive host
// throttle its neighbours.
//
// It fails open. When the key table is full and a sweep cannot free room, the
// request is allowed and a counter increments. A limiter is abuse mitigation,
// not an authorization boundary; refusing real traffic because bookkeeping ran
// out of space would turn a memory ceiling into an outage.
//
// It is a token bucket with lazy refill, so there is no timer per key and no
// background goroutine. Sweeping is amortized across calls, which means a
// limiter cannot outlive the thing that created it or leak a goroutine into a
// test binary.
package ratelimit

import (
	"hash/maphash"
	"math"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// shardCount spreads the table across independent locks. The redirect path
	// consults a limiter on every miss, and a single mutex there would serialize
	// exactly the requests the split pool exists to keep parallel.
	shardCount = 32

	// defaultMaxKeys bounds the table across all shards. At roughly 100 bytes
	// per entry — a heap-allocated bucket, the key string and its bytes, and the
	// map's own per-slot overhead — a full table is about 10 MB. The cap is per
	// limiter, and the server runs up to three (login, api, redirect_404), so
	// the process-wide worst case is ~300k keys. linkctrl_rate_limit_tracked_keys
	// is the number to watch; it is enough to track a large botnet and small
	// enough that it cannot become the reason the process is killed.
	defaultMaxKeys = 100_000

	// sweepEvery is how many calls pass between amortized sweeps. One shard is
	// swept per trigger, so the whole table turns over every 32 triggers.
	sweepEvery = 4096
)

// invalidKey is the bucket for requests whose address could not be parsed. They
// share one bucket rather than being exempt: an unparseable RemoteAddr should
// not be a way around the limit.
const invalidKey = "?"

// Options tunes a Limiter. The zero value is valid.
type Options struct {
	// Burst is how many requests may arrive at once before throttling starts.
	// Defaults to the per-minute rate, which lets a client spend its whole
	// minute's allowance immediately — right for scripts that batch, and still
	// bounded by the refill rate over any longer window.
	Burst int

	// MaxKeys bounds tracked keys across all shards. Zero means defaultMaxKeys.
	MaxKeys int

	// Now overrides the clock, for tests.
	Now func() time.Time

	// Shared makes this limit apply across replicas. Nil keeps it per process,
	// which is what every limit was before M24 and what the 404-probe limiter
	// stays: sharing that one would put a network round trip on the redirect
	// path and make an optional dependency load-bearing.
	Shared *Shared
}

// bucket is one key's allowance. Refill is computed from `last` when the bucket
// is next touched, so an idle key costs nothing.
type bucket struct {
	tokens float64
	last   time.Time
}

type shard struct {
	mu sync.Mutex
	m  map[string]*bucket
}

// Limiter allows or throttles requests by client address.
//
// Every method is nil-safe, and a nil Limiter allows everything. That is what
// makes "0 disables this limit" a single check at construction rather than a
// branch at every call site.
type Limiter struct {
	perSecond float64
	burst     float64
	maxShard  int
	now       func() time.Time

	shared *Shared

	seed   maphash.Seed
	shards [shardCount]shard

	calls     atomic.Uint64
	overflows atomic.Int64
}

// New returns a limiter allowing perMinute requests per key per minute, or nil
// if perMinute is zero or negative.
//
// Returning nil for a disabled limit is deliberate: the caller stores it, every
// method tolerates it, and there is no second "enabled" flag to keep in sync
// with the number.
func New(perMinute int, opts Options) *Limiter {
	if perMinute <= 0 {
		return nil
	}
	if opts.Burst <= 0 {
		opts.Burst = perMinute
	}
	if opts.MaxKeys <= 0 {
		opts.MaxKeys = defaultMaxKeys
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	l := &Limiter{
		shared:    opts.Shared,
		perSecond: float64(perMinute) / 60,
		burst:     float64(opts.Burst),
		// Rounded up so a small MaxKeys still leaves every shard room for one.
		maxShard: (opts.MaxKeys + shardCount - 1) / shardCount,
		now:      opts.Now,
		seed:     maphash.MakeSeed(),
	}
	for i := range l.shards {
		l.shards[i].m = make(map[string]*bucket)
	}
	return l
}

// Allow consumes one token, reporting whether the request may proceed and, if
// not, how long until it could.
func (l *Limiter) Allow(addr netip.Addr) (bool, time.Duration) {
	return l.takeKey(Key(addr), true)
}

// AllowKey is Allow against something that is not an address.
//
// Added by M35 for the one limit that has to be keyed on the *resource* rather
// than on the client: guesses at a link's password, driven through many
// visitors' browsers, spread across as many addresses as there are visitors and
// slip under a per-address bucket entirely (D54). Keying the same limiter on the
// alias closes that, and the two limbs are checked together — an attacker has to
// stay under both.
//
// Deliberately the same buckets, the same sweep and the same Redis script the
// address-keyed limit uses. A second mechanism would be a second thing to get
// wrong, and the shared limiter M24 built already takes a string key: `Key` was
// only ever how an address became one.
//
// The caller is responsible for a key that cannot collide with an address —
// prefix it — because both live in one table.
func (l *Limiter) AllowKey(key string) (bool, time.Duration) {
	if key == "" {
		key = invalidKey
	}
	return l.takeKey(key, true)
}

// Check reports whether a token is available without consuming one.
//
// Paired with Charge by callers that only bill some outcomes — the redirect
// path checks before resolving an alias and charges only for a miss, so a
// working short link never spends a token.
func (l *Limiter) Check(addr netip.Addr) (bool, time.Duration) {
	return l.takeKey(Key(addr), false)
}

// RefundKey hands one token back to a keyed bucket.
//
// For a caller that has to spend before it knows whether it should have. The
// link-password gate is the one: both limbs are consumed before the form is
// parsed, deliberately, so that timing cannot say which limb refused — and that
// left a link with more than `burst` legitimate visitors in a burst throttling
// itself with no attacker present (F115). A correct password refunds the alias
// limb, which touches neither D53 nor D54: the per-alias keying that stops
// distributed guessing is unchanged, and what is given back is only ever a token
// spent by somebody who proved they had the password.
//
// The address limb is deliberately **not** refunded. A visitor typing the right
// password is still traffic from that address, and the per-address limb is what
// bounds one machine grinding a wordlist.
//
// Never above burst: the shared script clamps, and so does the local bucket.
func (l *Limiter) RefundKey(key string) {
	if l == nil {
		return
	}
	if key == "" {
		key = invalidKey
	}
	if l.shared != nil {
		ttl := time.Duration(l.burst/l.perSecond*float64(time.Second)) + time.Minute
		if _, _, answered := l.shared.take(l.perSecond, l.burst, -1, ttl, key); answered {
			return
		}
	}
	l.refundLocal(key)
}

// refundLocal gives a token back to this process's own bucket.
//
// Only a bucket that already exists. Creating one to refund into would be
// inventing credit nobody spent, and an absent bucket is a full one anyway.
func (l *Limiter) refundLocal(k string) {
	sh := &l.shards[l.shardFor(k)]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if b, ok := sh.m[k]; ok {
		b.tokens = math.Min(l.burst, b.tokens+1)
	}
}

// Charge consumes a token if one is available, ignoring the answer.
func (l *Limiter) Charge(addr netip.Addr) {
	l.takeKey(Key(addr), true)
}

// takeKey is the whole limiter. Everything above is a way of arriving at a key.
func (l *Limiter) takeKey(k string, consume bool) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}

	// The shared decision first, when there is one. It is authoritative because
	// it is the only one that sees the other replicas; the local bucket below
	// is what answers when Redis does not.
	//
	// Only a consumed take goes to Redis. Check-then-Charge exists for the
	// redirect path, which is deliberately not shared, so a non-consuming
	// shared read would be a round trip nothing asks for.
	//
	// A shared answer returns without touching the local bucket, so the local
	// one is full at the moment Redis fails and a client gets one fresh burst
	// on failover. That is the fail-open direction, and the alternative —
	// spending both on every request — would make the local bucket useless as a
	// fallback by keeping it permanently empty.
	if consume && l.shared != nil {
		ttl := time.Duration(l.burst/l.perSecond*float64(time.Second)) + time.Minute
		if ok, retry, answered := l.shared.take(l.perSecond, l.burst, 1, ttl, k); answered {
			return ok, retry
		}
	}

	now := l.now()
	sh := &l.shards[l.shardFor(k)]

	sh.mu.Lock()
	b, ok := sh.m[k]
	switch {
	case ok:
		// Lazy refill. A monotonic clock with coarse resolution — Windows, for
		// one — can report a zero interval here, which adds no tokens. That is
		// the correct answer for zero elapsed time, not a rounding loss.
		if elapsed := now.Sub(b.last); elapsed > 0 {
			b.tokens = math.Min(l.burst, b.tokens+elapsed.Seconds()*l.perSecond)
			b.last = now
		}
	case len(sh.m) >= l.maxShard:
		// Full. Sweep the shard for keys that have refilled to full — they carry
		// no pending penalty, so dropping them loses nothing.
		l.sweepLocked(sh, now)
		if len(sh.m) >= l.maxShard {
			sh.mu.Unlock()
			l.overflows.Add(1)
			return true, 0
		}
		b = &bucket{tokens: l.burst, last: now}
		sh.m[k] = b
	default:
		b = &bucket{tokens: l.burst, last: now}
		sh.m[k] = b
	}

	allowed := b.tokens >= 1
	var retry time.Duration
	switch {
	case !allowed:
		// Time for the bucket to reach one whole token. Reported to the client as
		// Retry-After, so it is a floor rather than a promise.
		retry = time.Duration((1 - b.tokens) / l.perSecond * float64(time.Second))
	case consume:
		b.tokens--
	}
	sh.mu.Unlock()

	// Amortized maintenance, one shard per trigger. Doing it here rather than in
	// a goroutine keeps the limiter's lifetime exactly its owner's.
	if n := l.calls.Add(1); n%sweepEvery == 0 {
		target := &l.shards[(n/sweepEvery)%shardCount]
		target.mu.Lock()
		l.sweepLocked(target, now)
		target.mu.Unlock()
	}

	return allowed, retry
}

// sweepLocked drops buckets that have refilled to full. The caller holds the
// shard lock.
func (l *Limiter) sweepLocked(sh *shard, now time.Time) {
	for k, b := range sh.m {
		if b.tokens+now.Sub(b.last).Seconds()*l.perSecond >= l.burst {
			delete(sh.m, k)
		}
	}
}

func (l *Limiter) shardFor(key string) uint64 {
	return maphash.String(l.seed, key) % shardCount
}

// Len reports tracked keys. For tests and the metrics collector.
func (l *Limiter) Len() int {
	if l == nil {
		return 0
	}
	n := 0
	for i := range l.shards {
		l.shards[i].mu.Lock()
		n += len(l.shards[i].m)
		l.shards[i].mu.Unlock()
	}
	return n
}

// Overflows reports how many requests were allowed because the table was full.
//
// Worth a metric rather than a log line: a nonzero and climbing value means the
// limiter is no longer limiting, which is exactly the moment an operator wants
// to know without having to grep.
func (l *Limiter) Overflows() int64 {
	if l == nil {
		return 0
	}
	return l.overflows.Load()
}

// Key folds an address to its rate-limiting identity: the full address for
// IPv4, the /64 prefix for IPv6.
//
// The IPv6 case is the one that matters. Handing out /64s to single hosts is
// normal, so per-address keying would let one machine rotate through more
// identities than the table could ever hold — the limit would silently stop
// applying to precisely the client working hardest to evade it. It does not
// follow that a /64 is one customer: a site delegated a /56 or a /48 keeps a
// key per /64 inside it, which the package comment explains is deliberate.
func Key(addr netip.Addr) string {
	if !addr.IsValid() {
		return invalidKey
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

// RetryAfterSeconds renders a wait as an HTTP Retry-After value.
//
// Rounded up, with a floor of 1: Retry-After: 0 invites an immediate retry that
// is certain to be throttled again, and a client honouring it politely would
// hammer the endpoint it was just asked to back off from.
func RetryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	s := int(math.Ceil(d.Seconds()))
	if s < 1 {
		return 1
	}
	return s
}
