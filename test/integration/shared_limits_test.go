//go:build integration

package integration

import (
	"net/netip"
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/ratelimit"
)

// The limitation this milestone discharges: two replicas must not each grant a
// full allowance.
//
// Before this, in-memory buckets meant N replicas allowed N times the
// configured rate — so an attacker spreading a credential-stuffing run across
// replicas got the limit multiplied by however many were behind the load
// balancer.
func TestCredentialLimitIsSharedAcrossReplicas(t *testing.T) {
	rdb := newRedisClient(t)

	// Two limiters on one Redis is what two replicas are. Three per minute, so
	// the budget is small enough to exhaust deterministically.
	replicaA := ratelimit.New(3, ratelimit.Options{
		Shared: ratelimit.NewShared(ratelimit.SharedConfig{Client: rdb, Name: "logintest"}),
	})
	replicaB := ratelimit.New(3, ratelimit.Options{
		Shared: ratelimit.NewShared(ratelimit.SharedConfig{Client: rdb, Name: "logintest"}),
	})

	addr := netip.MustParseAddr("203.0.113.9")

	allowed := 0
	// Alternate between replicas, which is what a load balancer does.
	for i := range 8 {
		lim := replicaA
		if i%2 == 1 {
			lim = replicaB
		}
		if ok, _ := lim.Allow(addr); ok {
			allowed++
		}
	}

	if allowed != 3 {
		t.Errorf("two replicas allowed %d of 8 requests against a limit of 3; "+
			"the budget is being multiplied by the replica count", allowed)
	}
}

// A different address must not be affected by the first one's spending. Obvious,
// and worth pinning: a shared backend keyed on the wrong thing would be one
// global bucket, which throttles the whole world the moment anyone misbehaves.
func TestSharedLimitIsPerAddress(t *testing.T) {
	rdb := newRedisClient(t)
	lim := ratelimit.New(2, ratelimit.Options{
		Shared: ratelimit.NewShared(ratelimit.SharedConfig{Client: rdb, Name: "peraddr"}),
	})

	spender := netip.MustParseAddr("203.0.113.10")
	for range 5 {
		lim.Allow(spender)
	}
	if ok, _ := lim.Allow(spender); ok {
		t.Fatal("the spending address was not throttled")
	}

	if ok, _ := lim.Allow(netip.MustParseAddr("203.0.113.11")); !ok {
		t.Error("a different address was throttled by somebody else's spending; " +
			"the shared bucket is not keyed per address")
	}
}

// Two limits must not share a bucket. `login` and `api` have very different
// rates, and one key space would make the tighter one govern both.
func TestSharedLimitsAreSeparatePerName(t *testing.T) {
	rdb := newRedisClient(t)
	addr := netip.MustParseAddr("203.0.113.12")

	login := ratelimit.New(2, ratelimit.Options{
		Shared: ratelimit.NewShared(ratelimit.SharedConfig{Client: rdb, Name: "sep-login"}),
	})
	api := ratelimit.New(2, ratelimit.Options{
		Shared: ratelimit.NewShared(ratelimit.SharedConfig{Client: rdb, Name: "sep-api"}),
	})

	for range 5 {
		login.Allow(addr)
	}
	if ok, _ := login.Allow(addr); ok {
		t.Fatal("the login limit did not throttle")
	}
	if ok, _ := api.Allow(addr); !ok {
		t.Error("exhausting the login limit also exhausted the API limit; the " +
			"two limits share a key space")
	}
}

// The milestone's stated risk, and the one that matters most.
//
// A limiter that starts refusing requests when Redis hiccups converts an
// optional dependency into an outage — the opposite of the accepted posture,
// which is that a limiter is abuse mitigation and not an authorization
// boundary. With Redis gone it must fall back to the per-instance bucket:
// still limiting, just per replica.
func TestSharedLimitFallsBackToLocalWhenRedisIsGone(t *testing.T) {
	proxy := newRedisProxy(t)
	backend := ratelimit.NewShared(ratelimit.SharedConfig{
		Client: proxy.client(t), Name: "falltest", Timeout: 50 * time.Millisecond,
	})
	lim := ratelimit.New(3, ratelimit.Options{Shared: backend})
	addr := netip.MustParseAddr("203.0.113.13")

	// Healthy first, so the fallback is a change of behaviour rather than the
	// only behaviour this ever had.
	if ok, _ := lim.Allow(addr); !ok {
		t.Fatal("the first request was throttled against a fresh limit")
	}
	if backend.Fallbacks() != 0 {
		t.Fatalf("fell back %d times while Redis was healthy", backend.Fallbacks())
	}

	// Redis stalls: connections accepted, never answered. This is the shape
	// that hangs a caller, and the reason the budget is enforced from outside
	// the client call.
	proxy.blackholed(true)
	proxy.cut()

	// Requests still get an answer, and get it quickly.
	start := time.Now()
	for range 3 {
		lim.Allow(addr)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("three throttled requests took %s with Redis stalled; a limiter "+
			"must not make the request path wait on an optional dependency", elapsed)
	}
	if backend.Fallbacks() == 0 {
		t.Error("no fallback was recorded while Redis was unreachable")
	}

	// And it is still limiting, locally. A fresh address gets the local budget
	// and then stops, rather than being allowed without limit.
	fresh := netip.MustParseAddr("203.0.113.14")
	allowed := 0
	for range 10 {
		if ok, _ := lim.Allow(fresh); ok {
			allowed++
		}
	}
	if allowed == 0 {
		t.Error("everything was refused with Redis down; a limiter must fail " +
			"open, not turn a cache outage into an outage")
	}
	if allowed > 4 {
		t.Errorf("%d of 10 requests were allowed with Redis down against a limit "+
			"of 3; the local bucket is not limiting either", allowed)
	}
}

// Once Redis is unreachable the breaker must stop paying the timeout on every
// request. Without it, an outage puts the full budget onto every login.
func TestSharedLimitStopsCallingARedisThatIsNotAnswering(t *testing.T) {
	proxy := newRedisProxy(t)
	proxy.blackholed(true)

	backend := ratelimit.NewShared(ratelimit.SharedConfig{
		Client: proxy.client(t), Name: "breaker", Timeout: 200 * time.Millisecond,
	})
	lim := ratelimit.New(100, ratelimit.Options{Shared: backend})
	addr := netip.MustParseAddr("203.0.113.15")

	// Enough requests to trip the breaker several times over. If every one of
	// them paid the 200ms timeout this would take four seconds.
	start := time.Now()
	for range 20 {
		lim.Allow(addr)
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("20 requests took %s against a stalled Redis with a 200ms "+
			"budget; the breaker is not opening, so every request pays the "+
			"timeout for the whole outage", elapsed)
	}
}

// With no Redis client at all — the cache-disabled deployment — the limiter must
// behave exactly as it did before this milestone.
func TestLimiterWithoutASharedBackendIsUnchanged(t *testing.T) {
	lim := ratelimit.New(3, ratelimit.Options{
		Shared: ratelimit.NewShared(ratelimit.SharedConfig{Client: nil, Name: "nope"}),
	})
	addr := netip.MustParseAddr("203.0.113.16")

	allowed := 0
	for range 10 {
		if ok, _ := lim.Allow(addr); ok {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("allowed %d of 10 against a local limit of 3", allowed)
	}
}
