package ratelimit

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Shared makes a limit apply across replicas instead of per process.
//
// In-memory buckets mean N replicas allow roughly N times the configured rate,
// and a restart resets every bucket. That is fine for the 404-probe limiter,
// whose job is to make alias scanning tedious, and wrong for the credential
// limiter, whose job is to make credential stuffing across a leaked list
// expensive — an attacker who can reach any replica gets N times the budget.
//
// It is a backend for an existing Limiter rather than a replacement, because
// the fallback is the whole design: any Redis failure means the local bucket
// answers instead, so the limit degrades from "shared" to "per instance" rather
// than from "enforced" to "absent".
type Shared struct {
	client *goredis.Client
	// prefix separates one limit's keys from another's.
	prefix string
	// timeout bounds one Redis round trip.
	timeout time.Duration
	log     *slog.Logger

	breaker breaker

	// fallbacks counts requests answered locally because Redis did not.
	fallbacks atomic.Int64
}

// SharedConfig configures a Redis-backed limit.
type SharedConfig struct {
	Client *goredis.Client
	// Name distinguishes this limit's keys, e.g. "login".
	Name string
	// Timeout bounds one round trip. Zero uses defaultSharedTimeout.
	//
	// It is short on purpose. This runs on the request path, including the
	// login path, and a limiter that makes a request wait is worse than one
	// that under-counts: the whole posture is that a limiter is abuse
	// mitigation, not an availability dependency.
	Timeout time.Duration
	Logger  *slog.Logger
}

const (
	defaultSharedTimeout = 50 * time.Millisecond

	// breakerThreshold is how many consecutive failures open the breaker.
	breakerThreshold = 3
	// breakerCooldown is how long the breaker stays open before one request is
	// allowed to test Redis again.
	breakerCooldown = 5 * time.Second
)

// NewShared returns a Redis backend, or nil if there is no client.
//
// Nil is a valid backend and means "not shared", so an instance with the cache
// disabled keeps exactly the per-process limiter it had before.
func NewShared(cfg SharedConfig) *Shared {
	if cfg.Client == nil {
		return nil
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultSharedTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Shared{
		client:  cfg.Client,
		prefix:  "lc:rl:" + cfg.Name + ":",
		timeout: cfg.Timeout,
		log:     cfg.Logger,
	}
}

// takeScript is a token bucket, evaluated atomically on the server.
//
// Atomic because the read-modify-write is the whole point: two replicas doing
// GET, compute, SET would each see the same starting tokens and each allow a
// request the other had already spent.
//
// The clock is Redis's own, via TIME, not the caller's. Replicas do not agree
// on the time to better than a few hundred milliseconds in practice, and a
// bucket refilled against a fast client's clock refills faster than it should —
// which is a limit that quietly does not hold, discovered by whoever has the
// most skewed clock. Redis 7 replicates effects rather than commands, so a
// non-deterministic script is safe here.
//
// Returns {allowed, retry_after_ms}.
const takeScript = `
local key   = KEYS[1]
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local cost  = tonumber(ARGV[3])
local ttl   = tonumber(ARGV[4])

local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

local data  = redis.call('HMGET', key, 't', 'ts')
local tokens = tonumber(data[1])
local last   = tonumber(data[2])
if tokens == nil or last == nil then
  tokens = burst
  last = now
end

local elapsed = now - last
if elapsed < 0 then elapsed = 0 end
tokens = math.min(burst, tokens + (elapsed / 1000) * rate)

local allowed = 0
local retry = 0
if tokens >= cost then
  -- Clamped, because cost may be negative. A refund (cost -1) hands a token
  -- back to a bucket a caller spent and then decided it should not have, and
  -- without this it could push the bucket past its burst — a bucket holding
  -- more than burst is a limit that does not hold for the next burst-many
  -- requests. Every other write in this script keeps tokens <= burst; this is
  -- the one that could not, once the cost stopped being 1.
  tokens = math.min(burst, tokens - cost)
  allowed = 1
else
  retry = math.ceil(((cost - tokens) / rate) * 1000)
end

redis.call('HSET', key, 't', tokens, 'ts', now)
redis.call('PEXPIRE', key, ttl)
return {allowed, retry}
`

var takeSHA = goredis.NewScript(takeScript)

// take asks Redis for a decision, reporting whether it answered at all.
//
// The Redis call runs in its own goroutine and is abandoned on timeout rather
// than merely cancelled, and it stayed that way through F138 rather than
// surviving it by neglect.
//
// It was built because a stalled Redis — one that accepts a connection and
// never answers — used to run to the client's own ReadTimeout whatever context
// it was handed: go-redis applies a caller's deadline to the socket only when
// Options.ContextTimeoutEnabled is set, and internal/platform/redis did not set
// it. Since M45 it does, so the context below now shortens the call itself.
//
// What that changed is which goroutine waits, not how long this one does. The
// same probe measured Run against a stall at 400.85ms without the option and
// 50.32ms with it, and `take` at 50.46ms and 50.28ms — because the timer was
// already the binding constraint and still is. Keeping it is what makes the
// caller's bound this package's own number rather than a consequence of
// go-redis's retry count, backoff and pool behaviour, none of which the socket
// deadline covers; the option is what stops the abandoned goroutine holding a
// connection for eight times as long as anybody is waiting for it.
//
// The invalidation path multiplied that same bound by a retry loop until M26.6
// bounded the loop (F2); here it would be a login endpoint hanging on an
// optional dependency, so the deadline is enforced from outside the call.
//
// An abandoned call may still land and spend a token that the local fallback
// also spent. Over-counting by a token during a Redis stall is the harmless
// direction, and the alternative is a request waiting on a server that is not
// answering.
//
// The context is Background rather than the request's, which is why the call
// sites carry a contextcheck suppression. Deriving it from the request would
// let a client escape being charged by hanging up mid-request — abandoning the
// connection is free, and a limiter that can be dodged that way is not a
// limiter. The deadline that matters is enforced by the timer below anyway.
func (s *Shared) take(rate, burst, cost float64, ttl time.Duration, key string) (allowed bool, retry time.Duration, answered bool) {
	if s == nil || !s.breaker.allow() {
		return false, 0, false
	}

	type result struct {
		allowed bool
		retry   time.Duration
		err     error
	}
	ch := make(chan result, 1)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()

		vals, err := takeSHA.Run(ctx, s.client, []string{s.prefix + key},
			rate, burst, cost, ttl.Milliseconds()).Slice()
		if err != nil {
			ch <- result{err: err}
			return
		}
		if len(vals) != 2 {
			ch <- result{err: errors.New("ratelimit: unexpected script result")}
			return
		}
		ok, _ := vals[0].(int64)
		ms, _ := vals[1].(int64)
		ch <- result{allowed: ok == 1, retry: time.Duration(ms) * time.Millisecond}
	}()

	timer := time.NewTimer(s.timeout)
	defer timer.Stop()

	select {
	case r := <-ch:
		if r.err != nil {
			s.fail(r.err)
			return false, 0, false
		}
		s.breaker.succeed()
		return r.allowed, r.retry, true
	case <-timer.C:
		s.fail(errors.New("ratelimit: redis did not answer within the budget"))
		return false, 0, false
	}
}

func (s *Shared) fail(err error) {
	s.fallbacks.Add(1)
	// Logged only when the breaker actually opens, not per request: a Redis
	// outage would otherwise produce one line per request on the busiest
	// endpoints, which is how a log becomes useless exactly when it is needed.
	if s.breaker.recordFailure() {
		s.log.Warn("rate limiting fell back to per-instance buckets; the "+
			"configured limit now applies per replica rather than across them",
			slog.String("limit", s.prefix), slog.Any("error", err))
	}
}

// Fallbacks reports how many decisions were made locally because Redis did not
// answer. Nonzero means the limit is no longer shared.
func (s *Shared) Fallbacks() int64 {
	if s == nil {
		return 0
	}
	return s.fallbacks.Load()
}

// breaker stops calling a Redis that is not answering.
//
// Without it, every request during an outage pays the full timeout before
// falling back, which turns a cache problem into latency on every login. With
// it, an outage costs the timeout a few times and then nothing until the
// cooldown lets one request through to check.
type breaker struct {
	mu           sync.Mutex
	consecutive  int
	openUntil    time.Time
	nowOverride  func() time.Time
	openedNotify bool
}

func (b *breaker) now() time.Time {
	if b.nowOverride != nil {
		return b.nowOverride()
	}
	return time.Now()
}

// allow reports whether a Redis call should be attempted.
// allow reports whether this caller may attempt the shared limiter, and admits
// exactly one probe per cooldown once the breaker is open.
//
// **The re-arm happens when the probe is dispatched, not when it answers**, and
// that is the whole of the half-open state (F123). Reading `openUntil` and
// returning true without writing it let *every* concurrent request through the
// moment the cooldown lapsed — each paying the full timeout — which is the
// opposite of what the comment above promised, and what makes a retuned
// `REDIS_READ_TIMEOUT` expensive rather than merely wasteful. Deferring the
// re-arm to the probe's result would leave the probe's own timeout window open
// for the same herd.
//
// A probe that fails needs no special case: `openUntil` is already armed, so the
// breaker stays shut until the next cooldown without waiting to reach the
// failure threshold again. A probe that succeeds clears it in succeed().
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if b.now().After(b.openUntil) {
		b.openUntil = b.now().Add(breakerCooldown)
		return true
	}
	return false
}

// recordFailure counts a failure, reporting whether this one opened the breaker.
func (b *breaker) recordFailure() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive++
	if b.consecutive < breakerThreshold {
		return false
	}
	b.openUntil = b.now().Add(breakerCooldown)
	if b.openedNotify {
		return false
	}
	b.openedNotify = true
	return true
}

func (b *breaker) succeed() {
	b.mu.Lock()
	b.consecutive = 0
	b.openUntil = time.Time{}
	b.openedNotify = false
	b.mu.Unlock()
}
