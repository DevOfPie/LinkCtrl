package redirect

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// blackhole accepts connections and never answers them, which is the Redis
// failure worth testing: a refused connection fails fast and a stalled one holds
// the caller for the whole read timeout.
func blackhole(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Read and discard forever. Never write, so every command the
			// client sends waits out its own read timeout.
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	return ln.Addr().String()
}

// A stalled Redis is paid for once per resolve, not twice.
//
// F9 measured a cold resolve against a stalled Redis at 108ms with a 50ms read
// timeout: the failed lookup, the Postgres query, and then the Set that would
// have repopulated the cache — against a server that was not going to answer
// that either. The documented uncached target is 100ms, so the second timeout is
// the difference between meeting it during a cache outage and missing it.
//
// The timeout here is deliberately much larger than the shipped 50ms so the two
// outcomes are hundreds of milliseconds apart rather than tens. A timing
// assertion is only worth writing when the margin cannot be closed by an
// unlucky scheduler.
func TestAStalledRedisIsPaidForOncePerResolve(t *testing.T) {
	const timeout = 400 * time.Millisecond

	rdb := goredis.NewClient(&goredis.Options{
		Addr:         blackhole(t),
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
		DialTimeout:  timeout,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	r := &Resolver{
		redis: rdb,
		mem:   newMemCache(16),
		log:   slog.New(slog.DiscardHandler),
		opts: Options{
			RedisTimeout: timeout,
			DBTimeout:    time.Second,
			TTL:          time.Minute,
			NegativeTTL:  time.Minute,
		},
	}

	const k = "lc:a:v1:example:abc"
	snap := notFoundSnapshot()

	// The lookup, which is the first timeout. It also records that the server is
	// not answering.
	start := time.Now()
	if got := r.fromRedis(context.Background(), k, time.Now()); got != nil {
		t.Fatal("a blackholed Redis returned a snapshot")
	}
	lookup := time.Since(start)
	if lookup < timeout/2 {
		t.Fatalf("the lookup returned in %v, which is faster than the read timeout "+
			"(%v) — the harness is not stalling anything and the rest of this test "+
			"proves nothing", lookup, timeout)
	}

	// The write that used to follow it. It must not reach the server.
	start = time.Now()
	r.store(context.Background(), k, snap, time.Now())
	write := time.Since(start)

	if write > timeout/2 {
		t.Errorf("repopulating the cache after a failed lookup took %v, so the request "+
			"paid the read timeout (%v) a second time on a write nobody was going to "+
			"read — F9", write, timeout)
	}

	// The in-process tier is still populated, which is what makes skipping the
	// shared write free rather than a trade.
	if _, ok := r.mem.get(k, time.Now()); !ok {
		t.Error("skipping the Redis write also skipped the in-process tier, which " +
			"would make the next request on this replica pay for the query again")
	}
}

// The suppression lapses, so a Redis that comes back is written to again.
//
// Without this the fix would be a one-way door: one transient stall and the
// shared tier stops being populated for the life of the process, which is worse
// than the defect it replaced.
func TestTheStalledRedisSuppressionLapses(t *testing.T) {
	r := &Resolver{opts: Options{DBTimeout: 20 * time.Millisecond}}

	now := time.Now()
	r.markUnavailable(now)

	if !r.redisUnavailable(now.Add(time.Millisecond)) {
		t.Error("a failure recorded a moment ago is not suppressing the write it " +
			"was recorded to suppress")
	}
	if r.redisUnavailable(now.Add(50 * time.Millisecond)) {
		t.Error("the suppression outlived its window, so a Redis that recovered " +
			"would never be repopulated")
	}

	// And a resolver that has seen nothing suppresses nothing, so the zero value
	// is the healthy state rather than a stalled one.
	if (&Resolver{opts: Options{DBTimeout: time.Second}}).redisUnavailable(time.Now()) {
		t.Error("a resolver that has seen no failure reports Redis unavailable")
	}
}
