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
	// The read timeout is the hot path's patience. 50ms is far above a healthy
	// round trip and below the 100ms uncached redirect budget, so a stalled
	// Redis costs bounded latency and the lookup falls through to Postgres.
	//
	// Measured for M26.6 rather than assumed, because the resolver's per-lookup
	// deadline is not what holds this: a stalled read runs to ReadTimeout
	// whatever context it was handed. Against a proxy that accepted the
	// connection and never answered, five consecutive lookups cost 51ms each,
	// and the same held for a connection established and then left to go quiet
	// mid-command. DialTimeout does not enter into it, because a server that
	// accepts is a server whose dial succeeded: raising it to 8s left those
	// lookups at 51ms. A dial, unlike a read, does honour the deadline anyway —
	// an unroutable address with a 2s dial timeout under a 50ms context cost
	// 50ms. The bound survives an operator retuning it, because the resolver's
	// RedisTimeout *is* this value (cmd/linkctrl/main.go).
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

	client := redis.NewClient(opt)

	pingCtx, cancel := context.WithTimeout(ctx, c.Redis.DialTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		return client, fmt.Errorf("ping: %w", err)
	}
	return client, nil
}

// PingTimeout is the budget for a readiness probe's cache check.
const PingTimeout = 750 * time.Millisecond
