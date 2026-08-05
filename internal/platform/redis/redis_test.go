package redis_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/platform/redis"
)

// stalledRedis is the failure mode every timeout in this package is written
// against: a server that completes the TCP handshake and then never speaks.
//
// It is not a refused connection and not a slow one. A refusal fails fast on its
// own and needs no defence; a stall is the shape that holds a caller for as long
// as the caller is willing to be held, which is the whole question here.
//
// Connections are kept rather than closed, because closing one would send a FIN
// and turn the stall into a read error — the opposite of what is being tested.
func stalledRedis(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
			close(done)
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return "redis://" + ln.Addr().String() + "/0"
}

// stalledConfig points a client at a stall, with a read timeout an order of
// magnitude above the deadline the tests hand their calls.
//
// 400ms and 50ms are the numbers F138 was measured at, kept so the assertions
// below and the row that produced them describe the same experiment.
func stalledConfig(t *testing.T) config.Config {
	t.Helper()
	var c config.Config
	c.Redis.URL = stalledRedis(t)
	c.Redis.DialTimeout = time.Second
	c.Redis.ReadTimeout = 400 * time.Millisecond
	c.Redis.PoolSize = 4
	return c
}

// TestAContextDeadlineBoundsARedisCall is F138.
//
// go-redis hands the socket deadline context.Background() unless
// Options.ContextTimeoutEnabled is set, so before M45 every deadline this tree
// passed to a Redis call was decoration and ReadTimeout was the only thing
// bounding a stall. Nothing at any call site said so, which is what made it a
// trap rather than a preference.
//
// The assertion is deliberately loose. What is being tested is which of two
// bounds applies — 50ms or 400ms — not how precisely either is honoured, and a
// tight bound here would be a test that fails on a loaded machine while the
// property it guards is intact.
func TestAContextDeadlineBoundsARedisCall(t *testing.T) {
	t.Parallel()
	cfg := stalledConfig(t)

	client, err := redis.Open(t.Context(), cfg)
	if err == nil {
		t.Fatal("Open reported a healthy connection to a server that never answers")
	}
	if client == nil {
		t.Fatal("Open returned no client, so the caller cannot carry on without a cache")
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := client.Get(ctx, "any-key").Err(); err == nil {
		t.Fatal("a stalled server answered a lookup")
	}
	elapsed := time.Since(start)

	// Halfway between the two bounds. Under the option the call is held by the
	// 50ms context; without it, by the 400ms read timeout.
	if elapsed > 225*time.Millisecond {
		t.Errorf("a call carrying a 50ms deadline took %v against a stalled server; "+
			"the deadline is inert and REDIS_READ_TIMEOUT (%v) is what bounded it",
			elapsed, cfg.Redis.ReadTimeout)
	}
}

// TestReadTimeoutStillCapsAGenerousDeadline is the other direction, and it is
// the half that keeps the change from being a widening.
//
// pool.Conn.deadline takes the minimum of now+ReadTimeout and the context's
// deadline, so enabling the option can only shorten a call. That matters because
// two call sites in this tree ask for far longer than ReadTimeout on purpose —
// the analytics returning-visitor pipeline's five seconds and the readiness
// probe's 750ms — and both are still meant to be held to the hot path's number.
func TestReadTimeoutStillCapsAGenerousDeadline(t *testing.T) {
	t.Parallel()
	cfg := stalledConfig(t)

	client, _ := redis.Open(t.Context(), cfg)
	if client == nil {
		t.Fatal("Open returned no client")
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	start := time.Now()
	if err := client.Get(ctx, "any-key").Err(); err == nil {
		t.Fatal("a stalled server answered a lookup")
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("a call carrying a 5s deadline took %v; REDIS_READ_TIMEOUT (%v) is "+
			"supposed to be the ceiling on every call whatever the caller asks for",
			elapsed, cfg.Redis.ReadTimeout)
	}
}
