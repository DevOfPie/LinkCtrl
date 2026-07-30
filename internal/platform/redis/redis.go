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
	// round trip and far below the 100ms uncached redirect budget, so a stalled
	// Redis costs a little latency and then falls through to Postgres.
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
