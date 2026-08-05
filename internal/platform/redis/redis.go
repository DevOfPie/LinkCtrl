// Package redis builds the cache client.
//
// The cache is strictly optional. Redis runs with no persistence and an LRU
// eviction policy, so any key may vanish at any moment; nothing
// correctness-critical may be stored here. Every call site must treat a Redis
// error as a cache miss and fall through to Postgres, and readiness reports a
// Redis outage as degraded rather than unready — failing readiness would take
// a working site out of rotation over a cache problem.
package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/DevOfPie/LinkCtrl/internal/config"
)

type Client = redis.Client

// Open creates the client and verifies connectivity.
//
// A failure to connect is returned so startup can log it, but the caller is
// expected to continue: the service is fully functional without a cache, only
// slower.
func Open(ctx context.Context, c config.Config) (*redis.Client, error) {
	opt, err := redis.ParseURL(c.Redis.URL)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}

	opt.DialTimeout = c.Redis.DialTimeout
	// The read timeout is the hot path's patience, and the ceiling on every call
	// in this tree. 50ms is far above a healthy round trip and below the 100ms
	// uncached redirect budget, so a stalled Redis costs bounded latency and the
	// lookup falls through to Postgres.
	//
	// Measured for M26.6 rather than assumed, against a proxy that accepted the
	// connection and never answered: five consecutive lookups cost 51ms each,
	// and the same held for a connection established and then left to go quiet
	// mid-command. DialTimeout does not enter into it, because a server that
	// accepts is a server whose dial succeeded: raising it to 8s left those
	// lookups at 51ms. The bound survives an operator retuning it, because the
	// resolver's RedisTimeout *is* this value (cmd/linkctrl/main.go).
	//
	// Bounded, not free, and a whole redirect pays it twice: a miss spends this
	// timeout on the failed lookup and again on the Set that repopulates the
	// cache, both on the request. A cold resolve against a stalled Redis
	// measured 108ms. Nothing here *compounds* — there is no retry loop, which
	// was the invalidation path's defect and is what M26.6 bounded — so the
	// second call is recorded as deferred finding F9 rather than fixed here.
	//
	// MaxRetries is deliberately left at go-redis's default. It multiplies a
	// dial that never completes — 1.906s to 7.764s at a 300ms dial timeout,
	// four times over, on top of the pool's own five attempts — but only for a
	// call carrying no deadline, and every call site in this tree carries one.
	// Setting it would change the client the redirect path uses for no effect
	// this could measure. D26.
	opt.ReadTimeout = c.Redis.ReadTimeout
	opt.WriteTimeout = c.Redis.ReadTimeout
	opt.PoolSize = c.Redis.PoolSize
	opt.MinIdleConns = 2
	// Without this, a deadline handed to a Redis call is decoration: go-redis's
	// baseClient.context hands the socket deadline context.Background() unless
	// it is set, so what actually bounded a stalled read was ReadTimeout and
	// nothing else. Every comment in this tree that says "the caller's deadline
	// does not bind a stalled read" was describing this one unset field (F138).
	//
	// Measured at the pinned v9.21.0, against a listener that accepts and never
	// answers: one command carrying a 50ms context cost 400.85ms with
	// ReadTimeout 400ms, and 50.32ms with this set. TestAContextDeadlineBounds
	// ARedisCall is that probe.
	//
	// It can only ever *shorten* a call, never lengthen one. pool.Conn.deadline
	// takes the minimum of now+ReadTimeout and the context's deadline, so the
	// ceiling above still applies to a caller who asks for longer — the
	// analytics pipeline's five seconds and the readiness probe's 750ms are
	// still 50ms on the wire, exactly as they were.
	//
	// What changes is the other direction, and only there: a caller with less
	// than ReadTimeout left now gets what it asked for. On the redirect path
	// that is the request context, so a lookup starting 240ms into a 250ms
	// budget is bounded by the 10ms remaining instead of spending 50ms it does
	// not have. The populate is unaffected — store() detaches with
	// context.WithoutCancel, which drops the deadline as well as the cancel, so
	// a cache write still gets its full RedisTimeout after the request is gone.
	//
	// It removes neither hand-built defence, and that was checked rather than
	// assumed. internal/ratelimit's timer still bounds what the *caller* waits
	// — the same probe measured Shared.take at 50.46ms without this and 50.28ms
	// with it, because the timer was already the binding constraint — and D26's
	// REDIS_INVALIDATE_BUDGET still bounds a three-attempt loop that this
	// bounds one attempt of. Both keep their reasons, restated where they live.
	//
	// The pub/sub path never needed it: PubSub.conn writes and reads through
	// cn.WithReader with the caller's context directly rather than through
	// baseClient.context, so the subscriber's ReceiveTimeout has always meant
	// what it says.
	opt.ContextTimeoutEnabled = true

	client := redis.NewClient(opt)

	pingCtx, cancel := context.WithTimeout(ctx, c.Redis.DialTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		return client, fmt.Errorf("ping: %w", err)
	}
	return client, nil
}

// PingTimeout is the budget for a readiness probe's cache check.
//
// Generous, and deliberately not the number that decides a stall: ReadTimeout
// caps every command under it, so a Redis that accepts and never answers is
// reported degraded in REDIS_READ_TIMEOUT rather than in this. What this bounds
// is the rest — acquiring a connection, dialling one, retrying — on a probe that
// must answer whatever the cache is doing.
const PingTimeout = 750 * time.Millisecond
